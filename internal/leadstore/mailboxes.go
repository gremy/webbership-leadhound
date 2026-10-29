package leadstore

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/gremy/webbership-leadhound/internal/mailbox"
	"github.com/gremy/webbership-leadhound/internal/sitelist"
)

// The sending mailboxes (migration 00007): one sender_mailbox row per sending address, its
// state history in mailbox_state (append-only, the latest row by id wins, no row means
// active) and its guided checks in mailbox_check. The password is only ever held sealed
// (PasswordSealed); nothing here sees it in the clear.

// ActorOperator is the operator, as the tables made from 00007 on name them (the older
// tables still say ActorGremy).
const ActorOperator = "operator"

// The states a mailbox can be in (mailbox_state_state_check).
const (
	MailboxActive  = "active"
	MailboxPaused  = "paused"
	MailboxRetired = "retired"
)

// The scopes of a mailbox_state row: the mailbox alone, or every mailbox of its domain.
const (
	ScopeMailbox = "mailbox"
	ScopeDomain  = "domain"
)

// The outcomes of a mailbox_check (mailbox_check_outcome_check).
const (
	CheckRequested = "requested"
	CheckPassed    = "passed"
	CheckFailed    = "failed"
	CheckDidNotRun = "did_not_run"
)

var (
	// ErrMailboxNotFound is returned when no sender_mailbox row has that id.
	ErrMailboxNotFound = errors.New("leadstore: mailbox not found")
	// ErrMailboxExists is returned by AddMailbox for an address already added.
	ErrMailboxExists = errors.New("leadstore: a mailbox with that address exists")
	// ErrMailboxRetired is returned by SetMailboxState for a retired mailbox: retired is final.
	ErrMailboxRetired = errors.New("leadstore: the mailbox is retired")
	// ErrSealRequired is returned by UpdateMailbox for a change of login or server without a
	// new sealed password: the seal binds the password to where it may be used.
	ErrSealRequired = errors.New("leadstore: a change of login or server needs the password again")
	// ErrBadMailbox is returned for a mailbox, edit or check result that cannot be stored.
	ErrBadMailbox = errors.New("leadstore: invalid mailbox")
	// ErrUnknownMailboxState is returned for a state or scope outside the table's checks.
	ErrUnknownMailboxState = errors.New("leadstore: unknown mailbox state")
	// ErrCheckNotRequested is returned by FinishCheck for a check already finished or missing.
	ErrCheckNotRequested = errors.New("leadstore: the check is not waiting to run")
)

// Mailbox is one sender_mailbox row.
type Mailbox struct {
	ID             int64     `db:"id"`
	Address        string    `db:"address"`
	Domain         string    `db:"domain"` // the registrable domain of Address
	Login          string    `db:"login"`
	SMTPAddr       string    `db:"smtp_addr"` // host:port
	IMAPAddr       string    `db:"imap_addr"` // host:port
	DKIMSelector   string    `db:"dkim_selector"`
	FromName       string    `db:"from_name"` // empty: leads.yaml sender.name
	PasswordSealed []byte    `db:"password_sealed"`
	SealedBy       string    `db:"sealed_by"` // fingerprint of the public key that sealed it
	CreatedAt      time.Time `db:"created_at"`
}

// Host is the address's own host (everything after the @, in lower case and punycode as
// stored): where its DNS records are checked (SPF, the DKIM selector, DMARC) and the key of
// its sender_check rows. Domain, the registrable domain, is for caps and domain pauses.
func (m Mailbox) Host() string {
	return MailboxHost(m.Address)
}

// MailboxHost is the host of a mailbox address, as Mailbox.Host.
func MailboxHost(address string) string {
	_, host, _ := strings.Cut(strings.ToLower(strings.TrimSpace(address)), "@")
	return host
}

// hostSQL is the SQL of Mailbox.Host over sender_mailbox mb (its address is stored in
// lower case).
const hostSQL = `split_part(mb.address, '@', 2)`

// MailboxEdit is a change to a mailbox; a nil field stays as it is. A change of Login,
// SMTPAddr or IMAPAddr needs PasswordSealed (and SealedBy) too.
type MailboxEdit struct {
	Login, SMTPAddr, IMAPAddr, DKIMSelector, FromName *string
	PasswordSealed                                    []byte // nil: keep the sealed password
	SealedBy                                          string // set with PasswordSealed
}

// MailboxCheck is one mailbox_check row. Each result is nil until checked, or when its step
// never ran.
type MailboxCheck struct {
	ID          int64      `db:"id"`
	MailboxID   int64      `db:"mailbox_id"`
	RequestedAt time.Time  `db:"requested_at"`
	Actor       string     `db:"actor"`
	CheckedAt   *time.Time `db:"checked_at"`
	SMTPOK      *bool      `db:"smtp_ok"`
	IMAPOK      *bool      `db:"imap_ok"`
	FoldersOK   *bool      `db:"folders_ok"`
	SPF         *bool      `db:"spf"`
	DKIM        *bool      `db:"dkim"`
	DMARC       *bool      `db:"dmarc"`
	Fix         string     `db:"fix"`
	Outcome     string     `db:"outcome"`
	LandingURL  *string    `db:"landing_url"` // where https://<host>/ landed; nil when not checked or it did not answer
}

// MailboxCheckResult is what a check found, for FinishCheck. Outcome is passed, failed or
// did_not_run; a step that never ran stays nil.
type MailboxCheckResult struct {
	SMTPOK, IMAPOK, FoldersOK, SPF, DKIM, DMARC *bool
	Fix                                         string
	Outcome                                     string
	LandingURL                                  *string // the final URL of https://<host>/; nil when it did not answer
}

// MailboxStatus is a mailbox with what its readiness is judged by, for a day that starts
// at the since Mailboxes was given.
type MailboxStatus struct {
	Mailbox
	State, Reason, Scope string        // the latest mailbox_state row; active, "", mailbox when none
	StateAt              time.Time     // zero when no state row
	LastCheck            *MailboxCheck // the latest check, of any outcome; nil when none
	FirstLiveSend        *time.Time    // the first live (non-selftest) message sent from it
	SentToday            int           // live emails sent, maybe sent or sending since since
	DNSPassedToday       bool          // a full SPF/DKIM/DMARC pass for its Host and selector since since
}

func checkMailboxActor(actor string) error {
	if actor != ActorOperator && actor != ActorSystem {
		return fmt.Errorf("%w: %q", ErrUnknownActor, actor)
	}
	return nil
}

func checkFromName(name string) error {
	if strings.ContainsAny(name, "\r\n") {
		return fmt.Errorf("%w: the From name has a line break", ErrBadMailbox)
	}
	return nil
}

// isUniqueViolation reports a Postgres unique_violation.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// AddMailbox stores m for account accountID (0: DefaultAccountID; an address stays unique
// across accounts) (its Address trimmed and in lower case, its Domain the address's
// registrable domain) and requests its first check by actor, in one statement. It returns
// the new id. A blank login, server, selector, sealed password or fingerprint, an address
// that is not one local part and a domain, a Domain that is not the address's, or a From
// name with a line break is ErrBadMailbox; an address already added is ErrMailboxExists.
func (q Queries) AddMailbox(ctx context.Context, accountID int64, m Mailbox, actor string) (int64, error) {
	if err := checkMailboxActor(actor); err != nil {
		return 0, fmt.Errorf("leadstore: add mailbox: %w", err)
	}
	addr := strings.ToLower(strings.TrimSpace(m.Address))
	local, host, ok := strings.Cut(addr, "@")
	if !ok || local == "" || strings.Contains(host, "@") || strings.ContainsAny(addr, " \t\r\n<>,;\"") {
		return 0, fmt.Errorf("leadstore: add mailbox: %w: the address is not one mailbox address", ErrBadMailbox)
	}
	domain, err := sitelist.Registrable(host)
	if err != nil {
		return 0, fmt.Errorf("leadstore: add mailbox: %w: the address has no registrable domain", ErrBadMailbox)
	}
	if d := strings.ToLower(strings.TrimSpace(m.Domain)); d != "" && d != domain {
		return 0, fmt.Errorf("leadstore: add mailbox: %w: the domain is not the address's", ErrBadMailbox)
	}
	for _, f := range []struct{ name, v string }{
		{"login", m.Login}, {"smtp server", m.SMTPAddr}, {"imap server", m.IMAPAddr},
		{"dkim selector", m.DKIMSelector}, {"key fingerprint", m.SealedBy},
	} {
		if strings.TrimSpace(f.v) == "" {
			return 0, fmt.Errorf("leadstore: add mailbox: %w: no %s", ErrBadMailbox, f.name)
		}
	}
	if len(m.PasswordSealed) == 0 {
		return 0, fmt.Errorf("leadstore: add mailbox: %w: no sealed password", ErrBadMailbox)
	}
	if err := checkFromName(m.FromName); err != nil {
		return 0, fmt.Errorf("leadstore: add mailbox: %w", err)
	}
	var id int64
	err = q.db.QueryRow(ctx,
		`WITH mb AS (
		   INSERT INTO sender_mailbox (address, domain, login, smtp_addr, imap_addr, dkim_selector, from_name, password_sealed, sealed_by, account_id)
		   VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $11) RETURNING id
		 ), ck AS (
		   INSERT INTO mailbox_check (mailbox_id, actor) SELECT id, $10 FROM mb
		 )
		 SELECT id FROM mb`,
		addr, domain, strings.TrimSpace(m.Login), strings.TrimSpace(m.SMTPAddr), strings.TrimSpace(m.IMAPAddr),
		strings.TrimSpace(m.DKIMSelector), strings.TrimSpace(m.FromName), m.PasswordSealed, m.SealedBy, actor,
		orAccount(accountID)).Scan(&id)
	if isUniqueViolation(err) {
		return 0, fmt.Errorf("leadstore: add mailbox: %w", ErrMailboxExists)
	}
	if err != nil {
		return 0, fmt.Errorf("leadstore: add mailbox: %w", err)
	}
	return id, nil
}

// UpdateMailbox applies e to mailbox id. A change of login, SMTP or IMAP server (to a value
// other than the current one) without a new sealed password is ErrSealRequired, and nothing
// changes; a missing mailbox is ErrMailboxNotFound.
func (q Queries) UpdateMailbox(ctx context.Context, id int64, e MailboxEdit) error {
	if e.FromName != nil {
		if err := checkFromName(*e.FromName); err != nil {
			return fmt.Errorf("leadstore: update mailbox: %w", err)
		}
	}
	if (len(e.PasswordSealed) == 0) != (strings.TrimSpace(e.SealedBy) == "") {
		return fmt.Errorf("leadstore: update mailbox: %w: a sealed password and its key fingerprint go together", ErrBadMailbox)
	}
	trim := func(p *string) *string {
		if p == nil {
			return nil
		}
		v := strings.TrimSpace(*p)
		return &v
	}
	for _, p := range []*string{e.Login, e.SMTPAddr, e.IMAPAddr, e.DKIMSelector} {
		if p != nil && strings.TrimSpace(*p) == "" {
			return fmt.Errorf("leadstore: update mailbox: %w: a login, server or selector cannot be blank", ErrBadMailbox)
		}
	}
	var sealed []byte
	var sealedBy *string
	if len(e.PasswordSealed) > 0 {
		sealed, sealedBy = e.PasswordSealed, &e.SealedBy
	}
	tag, err := q.db.Exec(ctx,
		`UPDATE sender_mailbox SET
		   login = coalesce($2, login), smtp_addr = coalesce($3, smtp_addr), imap_addr = coalesce($4, imap_addr),
		   dkim_selector = coalesce($5, dkim_selector), from_name = coalesce($6, from_name),
		   password_sealed = coalesce($7, password_sealed), sealed_by = coalesce($8, sealed_by)
		 WHERE id = $1 AND ($7::bytea IS NOT NULL OR
		   (coalesce($2, login) = login AND coalesce($3, smtp_addr) = smtp_addr AND coalesce($4, imap_addr) = imap_addr))`,
		id, trim(e.Login), trim(e.SMTPAddr), trim(e.IMAPAddr), trim(e.DKIMSelector), trim(e.FromName), sealed, sealedBy)
	if err != nil {
		return fmt.Errorf("leadstore: update mailbox: %w", err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	if _, err := q.MailboxByID(ctx, id); err != nil {
		return fmt.Errorf("leadstore: update mailbox: %w", err)
	}
	return fmt.Errorf("leadstore: update mailbox %d: %w", id, ErrSealRequired)
}

func checkStateScope(state, scope string) error {
	switch state {
	case MailboxActive, MailboxPaused, MailboxRetired:
	default:
		return fmt.Errorf("%w: %q", ErrUnknownMailboxState, state)
	}
	if scope != ScopeMailbox && scope != ScopeDomain {
		return fmt.Errorf("%w: scope %q", ErrUnknownMailboxState, scope)
	}
	return nil
}

// SetMailboxState appends a state row for mailbox id. A retired mailbox takes no new state
// (ErrMailboxRetired); a missing one is ErrMailboxNotFound. The mailbox row is locked for
// the statement, so two writers of one mailbox take turns.
func (q Queries) SetMailboxState(ctx context.Context, id int64, state, reason, scope, actor string) error {
	if err := checkStateScope(state, scope); err != nil {
		return fmt.Errorf("leadstore: set mailbox state: %w", err)
	}
	if err := checkMailboxActor(actor); err != nil {
		return fmt.Errorf("leadstore: set mailbox state: %w", err)
	}
	var found, retired bool
	var inserted *int64
	err := q.db.QueryRow(ctx,
		`WITH mb AS (SELECT id FROM sender_mailbox WHERE id = $1 FOR UPDATE),
		 cur AS (SELECT s.state FROM mailbox_state s WHERE s.mailbox_id = $1 ORDER BY s.id DESC LIMIT 1),
		 ins AS (
		   INSERT INTO mailbox_state (mailbox_id, state, reason, scope, actor)
		   SELECT id, $2, $3, $4, $5 FROM mb WHERE NOT EXISTS (SELECT 1 FROM cur WHERE state = 'retired')
		   RETURNING id
		 )
		 SELECT EXISTS (SELECT 1 FROM mb), EXISTS (SELECT 1 FROM cur WHERE state = 'retired'), (SELECT id FROM ins)`,
		id, state, reason, scope, actor).Scan(&found, &retired, &inserted)
	switch {
	case err != nil:
		return fmt.Errorf("leadstore: set mailbox state: %w", err)
	case !found:
		return fmt.Errorf("leadstore: set mailbox state: mailbox %d: %w", id, ErrMailboxNotFound)
	case retired || inserted == nil:
		return fmt.Errorf("leadstore: set mailbox state: mailbox %d: %w", id, ErrMailboxRetired)
	}
	return nil
}

// PauseMailboxIfActive pauses mailbox id alone (scope mailbox) with reason by actor, only
// when its latest state is active, and reports whether it wrote the row. A mailbox already
// paused (by the operator, its domain or the system) or retired keeps its latest row, so a
// check repeated on a paused mailbox never stacks rows or overwrites the operator's reason
// and scope. A missing mailbox is ErrMailboxNotFound.
func (q Queries) PauseMailboxIfActive(ctx context.Context, id int64, reason, actor string) (bool, error) {
	if err := checkMailboxActor(actor); err != nil {
		return false, fmt.Errorf("leadstore: pause mailbox: %w", err)
	}
	var found, wrote bool
	err := q.db.QueryRow(ctx,
		`WITH mb AS (SELECT id FROM sender_mailbox WHERE id = $1 FOR UPDATE),
		 cur AS (SELECT s.state FROM mailbox_state s WHERE s.mailbox_id = $1 ORDER BY s.id DESC LIMIT 1),
		 ins AS (
		   INSERT INTO mailbox_state (mailbox_id, state, reason, scope, actor)
		   SELECT id, 'paused', $2, 'mailbox', $3 FROM mb WHERE coalesce((SELECT state FROM cur), 'active') = 'active'
		   RETURNING id
		 )
		 SELECT EXISTS (SELECT 1 FROM mb), EXISTS (SELECT 1 FROM ins)`,
		id, reason, actor).Scan(&found, &wrote)
	switch {
	case err != nil:
		return false, fmt.Errorf("leadstore: pause mailbox: %w", err)
	case !found:
		return false, fmt.Errorf("leadstore: pause mailbox %d: %w", id, ErrMailboxNotFound)
	}
	return wrote, nil
}

// latestStateSQL is the latest mailbox_state row of each mailbox, as a lateral subquery
// over sender_mailbox mb.
const latestStateSQL = `LEFT JOIN LATERAL (
	  SELECT s.state, s.reason, s.scope, s.at FROM mailbox_state s WHERE s.mailbox_id = mb.id ORDER BY s.id DESC LIMIT 1
	) st ON true`

// SetDomainState appends one state row (scope domain) for every mailbox on domain whose
// latest state is active, and returns how many it wrote. A mailbox already paused (on its
// own, say by its bounces) gets no row, so a later resume of the domain, which lifts only
// domain-scope pauses, never lifts its own pause; a retired one never changes.
func (q Queries) SetDomainState(ctx context.Context, domain, state, reason, actor string) (int, error) {
	if err := checkStateScope(state, ScopeDomain); err != nil {
		return 0, fmt.Errorf("leadstore: set domain state: %w", err)
	}
	if err := checkMailboxActor(actor); err != nil {
		return 0, fmt.Errorf("leadstore: set domain state: %w", err)
	}
	tag, err := q.db.Exec(ctx,
		`WITH mbs AS (
		   SELECT mb.id FROM sender_mailbox mb `+latestStateSQL+`
		   WHERE mb.domain = $1 AND coalesce(st.state, 'active') = 'active'
		   ORDER BY mb.id FOR UPDATE OF mb
		 )
		 INSERT INTO mailbox_state (mailbox_id, state, reason, scope, actor)
		 SELECT id, $2, $3, 'domain', $4 FROM mbs ORDER BY id`,
		strings.ToLower(strings.TrimSpace(domain)), state, reason, actor)
	if err != nil {
		return 0, fmt.Errorf("leadstore: set domain state: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// RequestCheck requests a check of mailbox id by actor and returns its id. When one is
// already waiting to run, that one's id is returned and nothing is added.
func (q Queries) RequestCheck(ctx context.Context, mailboxID int64, actor string) (int64, error) {
	if err := checkMailboxActor(actor); err != nil {
		return 0, fmt.Errorf("leadstore: request check: %w", err)
	}
	var id *int64
	var found bool
	err := q.db.QueryRow(ctx,
		`WITH mb AS (SELECT id FROM sender_mailbox WHERE id = $1 FOR UPDATE),
		 waiting AS (SELECT c.id FROM mailbox_check c WHERE c.mailbox_id = $1 AND c.outcome = 'requested' ORDER BY c.id LIMIT 1),
		 ins AS (
		   INSERT INTO mailbox_check (mailbox_id, actor)
		   SELECT id, $2 FROM mb WHERE NOT EXISTS (SELECT 1 FROM waiting) RETURNING id
		 )
		 SELECT EXISTS (SELECT 1 FROM mb), coalesce((SELECT id FROM waiting), (SELECT id FROM ins))`,
		mailboxID, actor).Scan(&found, &id)
	switch {
	case err != nil:
		return 0, fmt.Errorf("leadstore: request check: %w", err)
	case !found || id == nil:
		return 0, fmt.Errorf("leadstore: request check: mailbox %d: %w", mailboxID, ErrMailboxNotFound)
	}
	return *id, nil
}

// FinishCheck records what check id found, at at. Only a check still requested is
// finished (else ErrCheckNotRequested); an outcome other than passed, failed or did_not_run
// is ErrBadMailbox.
func (q Queries) FinishCheck(ctx context.Context, checkID int64, r MailboxCheckResult, at time.Time) error {
	switch r.Outcome {
	case CheckPassed, CheckFailed, CheckDidNotRun:
	default:
		return fmt.Errorf("leadstore: finish check: %w: outcome %q", ErrBadMailbox, r.Outcome)
	}
	tag, err := q.db.Exec(ctx,
		`UPDATE mailbox_check SET checked_at = $2, smtp_ok = $3, imap_ok = $4, folders_ok = $5,
		   spf = $6, dkim = $7, dmarc = $8, fix = $9, outcome = $10, landing_url = $11
		 WHERE id = $1 AND outcome = 'requested'`,
		checkID, at, r.SMTPOK, r.IMAPOK, r.FoldersOK, r.SPF, r.DKIM, r.DMARC, r.Fix, r.Outcome, r.LandingURL)
	if err != nil {
		return fmt.Errorf("leadstore: finish check: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("leadstore: finish check %d: %w", checkID, ErrCheckNotRequested)
	}
	return nil
}

const checkCols = `c.id, c.mailbox_id, c.requested_at, c.actor, c.checked_at, c.smtp_ok, c.imap_ok, c.folders_ok,
	c.spf, c.dkim, c.dmarc, c.fix, c.outcome, c.landing_url`

// RequestedChecks returns every check still waiting to run, oldest first.
func (q Queries) RequestedChecks(ctx context.Context) ([]MailboxCheck, error) {
	cs, err := collect(ctx, q.db, pgx.RowToStructByName[MailboxCheck],
		`SELECT `+checkCols+` FROM mailbox_check c WHERE c.outcome = 'requested' ORDER BY c.id`)
	if err != nil {
		return nil, fmt.Errorf("leadstore: requested checks: %w", err)
	}
	return cs, nil
}

// MarkHardBounce records that message id hard-bounced; a missing message is
// ErrMessageNotFound.
func (q Queries) MarkHardBounce(ctx context.Context, messageID int64) error {
	tag, err := q.db.Exec(ctx, `UPDATE outreach_message SET hard_bounce = true WHERE id = $1`, messageID)
	if err != nil {
		return fmt.Errorf("leadstore: mark hard bounce: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("leadstore: mark hard bounce: message %d: %w", messageID, ErrMessageNotFound)
	}
	return nil
}

// CancelMailboxPending cancels mailbox id's pending (draft or approved) messages with
// reason, as CancelPending does for a lead, and returns how many it cancelled.
func (q Queries) CancelMailboxPending(ctx context.Context, mailboxID int64, reason string) (int, error) {
	tag, err := q.db.Exec(ctx,
		`UPDATE outreach_message SET state = 'cancelled', cancel_reason = $2
		 WHERE mailbox_id = $1 AND state IN ('draft', 'approved')`, mailboxID, reason)
	if err != nil {
		return 0, fmt.Errorf("leadstore: cancel mailbox pending: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// RecordSenderCheckSelector writes one sender_check row for the domain whose hash is
// domainSHA, checked with DKIM selector selector.
func (q Queries) RecordSenderCheckSelector(ctx context.Context, domainSHA []byte, selector string, r mailbox.DNSResult, at time.Time) error {
	if _, err := q.db.Exec(ctx,
		`INSERT INTO sender_check (domain_sha256, selector, spf, dkim, dmarc, note, checked_at) VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		domainSHA, strings.TrimSpace(selector), r.SPF, r.DKIM, r.DMARC, r.Note, at); err != nil {
		return fmt.Errorf("leadstore: record sender check: %w", err)
	}
	return nil
}

// SenderCheckPassedSinceSelector reports whether a check of the domain whose hash is
// domainSHA with DKIM selector selector, made at or after since, found all three records.
// A row with no selector (the single-mailbox check) never counts.
func (q Queries) SenderCheckPassedSinceSelector(ctx context.Context, domainSHA []byte, selector string, since time.Time) (bool, error) {
	var ok bool
	if err := q.db.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM sender_check
		   WHERE domain_sha256 = $1 AND selector = $2 AND selector <> '' AND checked_at >= $3 AND spf AND dkim AND dmarc)`,
		domainSHA, strings.TrimSpace(selector), since).Scan(&ok); err != nil {
		return false, fmt.Errorf("leadstore: sender check passed: %w", err)
	}
	return ok, nil
}

const mailboxCols = `mb.id, mb.address, mb.domain, mb.login, mb.smtp_addr, mb.imap_addr, mb.dkim_selector,
	mb.from_name, mb.password_sealed, mb.sealed_by, mb.created_at`

// MailboxByID returns mailbox id, or ErrMailboxNotFound.
func (q Queries) MailboxByID(ctx context.Context, id int64) (Mailbox, error) {
	ms, err := collect(ctx, q.db, pgx.RowToStructByName[Mailbox], `SELECT `+mailboxCols+` FROM sender_mailbox mb WHERE mb.id = $1`, id)
	if err != nil {
		return Mailbox{}, fmt.Errorf("leadstore: mailbox: %w", err)
	}
	if len(ms) == 0 {
		return Mailbox{}, fmt.Errorf("leadstore: mailbox %d: %w", id, ErrMailboxNotFound)
	}
	return ms[0], nil
}

// MailboxesOnDomain counts the mailboxes on domain that are not retired.
func (q Queries) MailboxesOnDomain(ctx context.Context, domain string) (int, error) {
	var n int
	if err := q.db.QueryRow(ctx,
		`SELECT count(*) FROM sender_mailbox mb `+latestStateSQL+`
		 WHERE mb.domain = $1 AND coalesce(st.state, 'active') <> 'retired'`,
		strings.ToLower(strings.TrimSpace(domain))).Scan(&n); err != nil {
		return 0, fmt.Errorf("leadstore: mailboxes on domain: %w", err)
	}
	return n, nil
}

// liveSentStates are the states SentSince counts toward a day.
const liveSentStates = `('sending', 'sent', 'maybe_sent')`

// Mailboxes returns every mailbox of account accountID (0: DefaultAccountID), by id, with its latest state and check, its first live
// send, what it sent since since and whether its host and selector passed a DNS check
// since since: one query. since is the start of the operator's day.
func (q Queries) Mailboxes(ctx context.Context, accountID int64, since time.Time) ([]MailboxStatus, error) {
	rows, err := q.db.Query(ctx,
		`SELECT `+mailboxCols+`,
		   coalesce(st.state, 'active'), coalesce(st.reason, ''), coalesce(st.scope, 'mailbox'), st.at,
		   ck.id, ck.requested_at, ck.actor, ck.checked_at, ck.smtp_ok, ck.imap_ok, ck.folders_ok,
		   ck.spf, ck.dkim, ck.dmarc, ck.fix, ck.outcome, ck.landing_url,
		   fl.first, sd.n,
		   EXISTS (SELECT 1 FROM sender_check sc
		     WHERE sc.domain_sha256 = sha256(convert_to(`+hostSQL+`, 'UTF8')) AND sc.selector = mb.dkim_selector
		       AND sc.selector <> '' AND sc.checked_at >= $1 AND sc.spf AND sc.dkim AND sc.dmarc)
		 FROM sender_mailbox mb
		 `+latestStateSQL+`
		 LEFT JOIN LATERAL (
		   SELECT c.* FROM mailbox_check c WHERE c.mailbox_id = mb.id ORDER BY c.id DESC LIMIT 1
		 ) ck ON true
		 LEFT JOIN LATERAL (
		   SELECT min(m.sent_at) AS first FROM outreach_message m
		   JOIN outreach_batch b ON b.id = m.batch_id JOIN lead l ON l.id = m.lead_id
		   WHERE m.mailbox_id = mb.id AND m.state = 'sent' AND NOT b.shadow AND l.kind <> 'selftest'
		 ) fl ON true
		 LEFT JOIN LATERAL (
		   SELECT count(*) AS n FROM outreach_message m JOIN outreach_batch b ON b.id = m.batch_id
		   WHERE m.mailbox_id = mb.id AND m.state IN `+liveSentStates+` AND m.sent_at >= $1 AND NOT b.shadow
		 ) sd ON true
		 WHERE mb.account_id = $2
		 ORDER BY mb.id`, since, orAccount(accountID))
	if err != nil {
		return nil, fmt.Errorf("leadstore: mailboxes: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (MailboxStatus, error) {
		var s MailboxStatus
		var stateAt *time.Time
		var c MailboxCheck
		var checkID *int64
		var requestedAt *time.Time
		var actor, fix, outcome *string
		err := row.Scan(&s.ID, &s.Address, &s.Domain, &s.Login, &s.SMTPAddr, &s.IMAPAddr, &s.DKIMSelector,
			&s.FromName, &s.PasswordSealed, &s.SealedBy, &s.CreatedAt,
			&s.State, &s.Reason, &s.Scope, &stateAt,
			&checkID, &requestedAt, &actor, &c.CheckedAt, &c.SMTPOK, &c.IMAPOK, &c.FoldersOK,
			&c.SPF, &c.DKIM, &c.DMARC, &fix, &outcome, &c.LandingURL,
			&s.FirstLiveSend, &s.SentToday, &s.DNSPassedToday)
		if err != nil {
			return MailboxStatus{}, err
		}
		if stateAt != nil {
			s.StateAt = *stateAt
		}
		if checkID != nil {
			c.ID, c.MailboxID, c.RequestedAt, c.Actor, c.Fix, c.Outcome = *checkID, s.ID, *requestedAt, *actor, *fix, *outcome
			s.LastCheck = &c
		}
		return s, nil
	})
	if err != nil {
		return nil, fmt.Errorf("leadstore: mailboxes: %w", err)
	}
	return out, nil
}

// countByMailbox runs a query of (mailbox_id, count) rows into a map.
func (q Queries) countByMailbox(ctx context.Context, op, sql string, args ...any) (map[int64]int, error) {
	rows, err := q.db.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("leadstore: %s: %w", op, err)
	}
	out := map[int64]int{}
	var id int64
	var n int
	if _, err := pgx.ForEachRow(rows, []any{&id, &n}, func() error { out[id] = n; return nil }); err != nil {
		return nil, fmt.Errorf("leadstore: %s: %w", op, err)
	}
	return out, nil
}

// SentSinceByMailbox counts, per mailbox, the live emails sent, maybe sent or left sending
// at or after since, as SentSince does for all of them. A mailbox with none is absent.
func (q Queries) SentSinceByMailbox(ctx context.Context, since time.Time) (map[int64]int, error) {
	return q.countByMailbox(ctx, "sent since by mailbox",
		`SELECT m.mailbox_id, count(*) FROM outreach_message m JOIN outreach_batch b ON b.id = m.batch_id
		 WHERE m.mailbox_id IS NOT NULL AND NOT b.shadow AND m.state IN `+liveSentStates+` AND m.sent_at >= $1
		 GROUP BY m.mailbox_id`, since)
}

// WaitingByMailbox counts, per mailbox, the live emails that may still go, as WaitingEmails
// does for all of them: approved, plus maybe sent or left sending. A mailbox with none is
// absent.
func (q Queries) WaitingByMailbox(ctx context.Context) (map[int64]int, error) {
	return q.countByMailbox(ctx, "waiting by mailbox",
		`SELECT m.mailbox_id, count(*) FROM outreach_message m
		 JOIN outreach_batch b ON b.id = m.batch_id JOIN lead l ON l.id = m.lead_id
		 WHERE m.mailbox_id IS NOT NULL AND NOT b.shadow AND b.approved_at IS NOT NULL AND m.channel = 'email'
		   AND l.kind <> 'selftest' AND m.state IN ('approved', 'sending', 'maybe_sent')
		 GROUP BY m.mailbox_id`)
}

// FirstLiveSendByMailbox returns, per mailbox, when its first live message recorded as sent
// went out, as FirstLiveSend does for all of them. A mailbox that has sent none is absent.
func (q Queries) FirstLiveSendByMailbox(ctx context.Context) (map[int64]time.Time, error) {
	rows, err := q.db.Query(ctx,
		`SELECT m.mailbox_id, min(m.sent_at) FROM outreach_message m
		 JOIN outreach_batch b ON b.id = m.batch_id JOIN lead l ON l.id = m.lead_id
		 WHERE m.mailbox_id IS NOT NULL AND NOT b.shadow AND l.kind <> 'selftest' AND m.state = 'sent' AND m.sent_at IS NOT NULL
		 GROUP BY m.mailbox_id`)
	if err != nil {
		return nil, fmt.Errorf("leadstore: first live send by mailbox: %w", err)
	}
	out := map[int64]time.Time{}
	var id int64
	var at time.Time
	if _, err := pgx.ForEachRow(rows, []any{&id, &at}, func() error { out[id] = at; return nil }); err != nil {
		return nil, fmt.Errorf("leadstore: first live send by mailbox: %w", err)
	}
	return out, nil
}

// HardBouncesInLastSends counts the hard bounces among mailbox id's last n live sends: its
// messages in state sent, refused or maybe_sent of live batches, selftests left out, newest
// first by attempt time, then by id.
func (q Queries) HardBouncesInLastSends(ctx context.Context, mailboxID int64, n int) (int, error) {
	var c int
	if err := q.db.QueryRow(ctx,
		`SELECT count(*) FILTER (WHERE hard_bounce) FROM (
		   SELECT m.hard_bounce FROM outreach_message m
		   JOIN outreach_batch b ON b.id = m.batch_id JOIN lead l ON l.id = m.lead_id
		   WHERE m.mailbox_id = $1 AND NOT b.shadow AND l.kind <> 'selftest' AND m.state IN ('sent', 'refused', 'maybe_sent')
		   ORDER BY m.sent_at DESC NULLS LAST, m.id DESC
		   LIMIT $2
		 ) last`, mailboxID, max(n, 0)).Scan(&c); err != nil {
		return 0, fmt.Errorf("leadstore: hard bounces: %w", err)
	}
	return c, nil
}
