package leadstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gremy/webbership-leadhound/internal/campaign"
	"github.com/gremy/webbership-leadhound/internal/mailbox"
	"github.com/gremy/webbership-leadhound/internal/sitelist"
)

// The advisory lock keys of the outreach mailbox (RA3, RA26). They differ from goose's
// migration lock, so a migrate never waits on a send.
const (
	// SendLockKey is held by one send (or selftest) for its whole run.
	SendLockKey int64 = 0x4c48_5345_4e44 // "LHSEND"
	// SyncLockKey is held by one sync, standalone or inside a send.
	SyncLockKey int64 = 0x4c48_5359_4e43 // "LHSYNC"
)

// ErrLockHeld is returned by TryLock when another session holds the lock.
var ErrLockHeld = errors.New("leadstore: lock held by another run")

// Lock is one session-level advisory lock, held on its own pooled connection until Release.
type Lock struct {
	conn *pgxpool.Conn
	key  int64
}

// TryLock takes advisory lock key on a connection acquired from pool, without waiting:
// ErrLockHeld when another session has it.
func TryLock(ctx context.Context, pool *pgxpool.Pool, key int64) (*Lock, error) {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("leadstore: lock: %w", err)
	}
	var ok bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, key).Scan(&ok); err != nil {
		conn.Release()
		return nil, fmt.Errorf("leadstore: lock: %w", err)
	}
	if !ok {
		conn.Release()
		return nil, ErrLockHeld
	}
	return &Lock{conn: conn, key: key}, nil
}

// Release unlocks and returns the connection to the pool. When the unlock cannot run, the
// connection is closed instead, which ends the session and so frees the lock.
func (l *Lock) Release() {
	if l == nil || l.conn == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var ok bool
	if err := l.conn.QueryRow(ctx, `SELECT pg_advisory_unlock($1)`, l.key).Scan(&ok); err != nil || !ok {
		_ = l.conn.Conn().Close(ctx)
	}
	l.conn.Release()
	l.conn = nil
}

// SenderDomainSHA256 is how sender_check names the sending domain: sha256 of the domain in
// lower case, so the domain itself is never in a row.
func SenderDomainSHA256(domain string) []byte {
	sum := sha256.Sum256([]byte(strings.TrimSuffix(strings.ToLower(strings.TrimSpace(domain)), ".")))
	return sum[:]
}

// RecordSenderCheck writes one sender_check row for the domain whose hash is domainSHA.
func (q Queries) RecordSenderCheck(ctx context.Context, domainSHA []byte, r mailbox.DNSResult, at time.Time) error {
	if _, err := q.db.Exec(ctx,
		`INSERT INTO sender_check (domain_sha256, spf, dkim, dmarc, note, checked_at) VALUES ($1, $2, $3, $4, $5, $6)`,
		domainSHA, r.SPF, r.DKIM, r.DMARC, r.Note, at); err != nil {
		return fmt.Errorf("leadstore: record sender check: %w", err)
	}
	return nil
}

// SenderCheckPassedSince reports whether a check of the domain whose hash is domainSHA,
// made at or after since, found all three records.
func (q Queries) SenderCheckPassedSince(ctx context.Context, domainSHA []byte, since time.Time) (bool, error) {
	var ok bool
	if err := q.db.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM sender_check
		   WHERE domain_sha256 = $1 AND checked_at >= $2 AND spf AND dkim AND dmarc)`, domainSHA, since).Scan(&ok); err != nil {
		return false, fmt.Errorf("leadstore: sender check passed: %w", err)
	}
	return ok, nil
}

// errNoMailbox: a per-mailbox write was given no mailbox.
var errNoMailbox = errors.New("leadstore: no mailbox given")

// MailboxCursorFor returns how far folder name of sending mailbox mailboxID has been read;
// ok is false before that mailbox's first sync of it (a first sync rescans). The row's key
// is the mailbox and the folder (00008).
func (q Queries) MailboxCursorFor(ctx context.Context, mailboxID int64, name string) (c mailbox.Cursor, ok bool, err error) {
	var v, last int64
	err = q.db.QueryRow(ctx, `SELECT uidvalidity, last_uid FROM mailbox_cursor WHERE mailbox = $1 AND mailbox_id = $2`,
		name, mailboxID).Scan(&v, &last)
	if errors.Is(err, pgx.ErrNoRows) {
		return mailbox.Cursor{}, false, nil
	}
	if err != nil {
		return mailbox.Cursor{}, false, fmt.Errorf("leadstore: mailbox cursor: %w", err)
	}
	return mailbox.Cursor{UIDValidity: uint32(v), LastUID: uint32(last)}, true, nil // #nosec G115 -- written from uint32 values
}

// AdvanceCursorFor records that folder name of sending mailbox mailboxID has been read up to
// lastUID under uidValidity, at at. Under the same UIDVALIDITY the cursor only moves
// forward; a new UIDVALIDITY replaces it.
func (q Queries) AdvanceCursorFor(ctx context.Context, mailboxID int64, name string, uidValidity, lastUID uint32, at time.Time) error {
	if mailboxID == 0 {
		return fmt.Errorf("leadstore: advance cursor: %w", errNoMailbox)
	}
	if _, err := q.db.Exec(ctx,
		`INSERT INTO mailbox_cursor (mailbox, mailbox_id, uidvalidity, last_uid, synced_at) VALUES ($1, $2, $3, $4, $5)
		 ON CONFLICT (mailbox_id, mailbox) DO UPDATE SET
		   last_uid = CASE WHEN mailbox_cursor.uidvalidity = EXCLUDED.uidvalidity
		     THEN greatest(mailbox_cursor.last_uid, EXCLUDED.last_uid) ELSE EXCLUDED.last_uid END,
		   uidvalidity = EXCLUDED.uidvalidity,
		   synced_at = EXCLUDED.synced_at`,
		name, mailboxID, int64(uidValidity), int64(lastUID), at); err != nil {
		return fmt.Errorf("leadstore: advance cursor: %w", err)
	}
	return nil
}

// SentRef is one email we sent (or may have sent) from a live batch: what sync matches an
// incoming message against. ToAddress is where it went: an email_consent step's is the
// consented address (send refuses it under any other).
type SentRef struct {
	ID        int64  `db:"id"`         // outreach_message.id
	MessageID string `db:"message_id"` // the Message-ID header, without angle brackets
	LeadID    int64  `db:"lead_id"`
	ToAddress string `db:"to_address"`
	OrgDomain string `db:"org_domain"`
	LeadKind  string `db:"lead_kind"`
	MailboxID int64  `db:"mailbox_id"` // the sending mailbox it went from
}

// Selftest reports whether the message went to the operator's own address (RA24).
func (r SentRef) Selftest() bool { return r.LeadKind == LeadSelftest }

// SentRefs returns every email of a live batch in state sending, sent or maybe_sent, oldest
// first.
func (q Queries) SentRefs(ctx context.Context) ([]SentRef, error) {
	refs, err := collect(ctx, q.db, pgx.RowToStructByName[SentRef],
		`SELECT m.id, m.message_id, m.lead_id, lower(coalesce(nullif(m.to_address, ''), `+consentAddressSQL+`)) AS to_address,
		   o.domain AS org_domain, l.kind AS lead_kind,
		   m.mailbox_id
		 FROM outreach_message m
		 JOIN outreach_batch b ON b.id = m.batch_id
		 JOIN lead l ON l.id = m.lead_id
		 JOIN org o ON o.id = l.org_id
		 WHERE NOT b.shadow AND m.channel = 'email' AND m.state IN ('sending', 'sent', 'maybe_sent')
		 ORDER BY m.id`)
	if err != nil {
		return nil, fmt.Errorf("leadstore: sent refs: %w", err)
	}
	return refs, nil
}

// Incoming is one classified message of a sending mailbox, ready to be recorded.
type Incoming struct {
	Mailbox     string
	UIDValidity uint32
	UID         uint32
	Kind        mailbox.MessageKind
	FromDomain  string   // the registrable domain only
	Matched     *SentRef // the message of ours it answers or reports on; nil when none
	At          time.Time
	// Snippet is the reply's first plain-text lines (quoted history and signature already
	// stripped by the reader). It is stored only for a reply that matched a lead, cut to
	// MaxReplySnippet characters; for anything else it is dropped.
	Snippet string
}

// MaxReplySnippet is the longest reply snippet ApplyIncomingFor stores, in characters
// (mailbox_event_snippet_check).
const MaxReplySnippet = 300

// replySnippet is what in stores as its snippet: nil unless it is a reply that matched a lead
// and has text, else its trimmed text cut to MaxReplySnippet characters (valid UTF-8 kept whole).
func replySnippet(in Incoming) *string {
	if in.Kind != mailbox.KindReply || in.Matched == nil {
		return nil
	}
	s := strings.TrimSpace(strings.ToValidUTF8(in.Snippet, ""))
	if s == "" {
		return nil
	}
	if utf8.RuneCountInString(s) > MaxReplySnippet {
		s = strings.TrimSpace(string([]rune(s)[:MaxReplySnippet]))
	}
	return &s
}

// Applied is what ApplyIncomingFor did.
type Applied struct {
	Duplicate  bool   // the message was recorded by an earlier sync; nothing changed
	LeadState  string // the matched lead's state afterwards ("" when none matched)
	Moved      bool   // the lead changed state
	Suppressed int    // suppression rows written
	Cancelled  int64  // pending messages cancelled
}

// ApplyIncomingFor records in, read from sending mailbox mailboxID, applies what it means and
// moves that mailbox's cursor to its UID, all through q (run it in one transaction per
// message):
//   - reply: the lead moves to replied; its pending messages and its domain's are cancelled;
//   - bounce: the address we wrote to is suppressed (bounce), the lead moves to bounced, its
//     pending messages are cancelled;
//   - opt_out: the address we wrote to is suppressed, and the lead's domain (its org's and the
//     address's registrable domains) too unless the lead is a selftest (RA24); the lead moves
//     to opted_out; pending messages are cancelled;
//   - auto_reply and unmatched: recorded only.
//
// The event and the cursor record the mailbox, and a suppression's source is
// mailbox:<id>:<folder>:<uid>. Matching stays global: in.Matched may have gone from any
// mailbox. A message already recorded (the same mailbox, folder, UIDVALIDITY and UID) changes nothing
// but the cursor. mailbox_event keeps the registrable From domain only: no address, no body
// beyond a matched reply's snippet (replySnippet).
func (q Queries) ApplyIncomingFor(ctx context.Context, mailboxID int64, in Incoming) (Applied, error) {
	if mailboxID == 0 {
		return Applied{}, fmt.Errorf("leadstore: apply incoming: %w", errNoMailbox)
	}
	var res Applied
	var msgID, leadID *int64
	if in.Matched != nil {
		msgID, leadID = &in.Matched.ID, &in.Matched.LeadID
	}
	tag, err := q.db.Exec(ctx,
		`INSERT INTO mailbox_event (mailbox, uidvalidity, uid, kind, from_domain, matched_message_id, lead_id, seen_at, mailbox_id, snippet)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		 ON CONFLICT (mailbox_id, mailbox, uidvalidity, uid) DO NOTHING`,
		in.Mailbox, int64(in.UIDValidity), int64(in.UID), string(in.Kind), in.FromDomain, msgID, leadID, in.At, mailboxID, replySnippet(in))
	if err != nil {
		return Applied{}, fmt.Errorf("leadstore: apply incoming: %w", err)
	}
	if tag.RowsAffected() == 0 {
		res.Duplicate = true
	} else if in.Matched != nil {
		source := fmt.Sprintf("mailbox:%d:%s:%d", mailboxID, in.Mailbox, in.UID)
		if err := q.applyEffects(ctx, in, source, &res); err != nil {
			return Applied{}, fmt.Errorf("leadstore: apply incoming: %w", err)
		}
	}
	if err := q.AdvanceCursorFor(ctx, mailboxID, in.Mailbox, in.UIDValidity, in.UID, in.At); err != nil {
		return Applied{}, err
	}
	return res, nil
}

func (q Queries) applyEffects(ctx context.Context, in Incoming, source string, res *Applied) error {
	m := in.Matched
	var (
		to     string
		from   []string
		reason string
	)
	switch in.Kind {
	case mailbox.KindReply:
		to, from, reason = LeadReplied, []string{LeadBatched, LeadContacted}, "reply in "+in.Mailbox
	case mailbox.KindBounce:
		to, from, reason = LeadBounced, []string{LeadBatched, LeadContacted}, "bounce in "+in.Mailbox
	case mailbox.KindOptOut:
		to, from, reason = LeadOptedOut, []string{LeadBatched, LeadContacted, LeadReplied}, "opt-out in "+in.Mailbox
	default:
		return nil // auto_reply, unmatched: recorded only
	}
	moved, err := q.setLeadStateFrom(ctx, m.LeadID, from, to, reason, ActorSystem)
	if err != nil {
		return err
	}
	res.Moved = moved
	// A bounce or an opt-out is recorded for the matched lead's account: it then blocks every
	// campaign of that account.
	accountID, err := q.leadAccount(ctx, m.LeadID)
	if err != nil {
		return err
	}
	suppress := func(kind, value, why string) error {
		if value == "" {
			return nil
		}
		s, err := q.Suppress(ctx, accountID, kind, value, why, source)
		if err != nil {
			return err
		}
		if s.Recorded {
			res.Suppressed++
		}
		res.Cancelled += s.Cancelled
		return nil
	}
	switch in.Kind {
	case mailbox.KindBounce:
		if err := suppress(SuppressAddress, m.ToAddress, ReasonBounce); err != nil {
			return err
		}
	case mailbox.KindOptOut:
		if err := suppress(SuppressAddress, m.ToAddress, ReasonOptOut); err != nil {
			return err
		}
		if !m.Selftest() {
			// The lead's own domains, never the sender's: a thread answered from a webmail
			// address must not suppress the webmail provider.
			seen := map[string]bool{}
			_, toDomain, _ := strings.Cut(m.ToAddress, "@")
			for _, d := range []string{m.OrgDomain, toDomain} {
				r, err := sitelist.Registrable(d)
				if err != nil || seen[r] {
					continue
				}
				seen[r] = true
				if err := suppress(SuppressDomain, r, ReasonOptOut); err != nil {
					return err
				}
			}
		}
	}
	n, err := q.CancelPending(ctx, CancelTarget{LeadID: m.LeadID}, string(in.Kind))
	if err != nil {
		return err
	}
	res.Cancelled += n
	if in.Kind == mailbox.KindReply && !m.Selftest() && m.OrgDomain != "" {
		n, err := q.CancelPending(ctx, CancelTarget{Domain: m.OrgDomain}, string(in.Kind))
		if err != nil {
			return err
		}
		res.Cancelled += n
	}
	if err := q.db.QueryRow(ctx, `SELECT state FROM lead WHERE id = $1`, m.LeadID).Scan(&res.LeadState); err != nil {
		return err
	}
	return nil
}

// SendRow is one message as send checks it just before SMTP: the row, its batch's kind, and
// its lead's state, kind and org.
type SendRow struct {
	Message
	Shadow bool `db:"shadow"`
	// CampaignID is the campaign of the message's batch: a live send goes only from a mailbox
	// assigned to it.
	CampaignID int64  `db:"campaign_id"`
	LeadState  string `db:"lead_state"`
	LeadKind   string `db:"lead_kind"`
	OrgDomain  string `db:"org_domain"`
	OrgCountry string `db:"org_country"`
	// ConsentAddress is, for an email_consent step, the address the person gave when they
	// agreed on the call (the latest call outcome on the lead's calls of the batch, when it
	// came with consent); "" when there is none, and for every other step.
	ConsentAddress string `db:"consent_address"`
}

// Recipient is the address the message goes to: the consented address for an
// email_consent step, else the stored to_address. RecipientRefusal says when it may not go.
func (r SendRow) Recipient() string {
	if r.Condition == ConditionEmailConsent {
		return r.ConsentAddress
	}
	return r.ToAddress
}

// RecipientRefusal is why send must refuse r for its recipient: an email_consent step with
// no consent on record, or one whose stored recipient (a batch drafted before the
// recipient was left to the consent) is not the address they agreed to. "" when it may go.
func (r SendRow) RecipientRefusal() string {
	if r.Condition != ConditionEmailConsent {
		return ""
	}
	switch {
	case r.ConsentAddress == "":
		return "no email consent on record"
	case r.ToAddress != "" && !strings.EqualFold(strings.TrimSpace(r.ToAddress), r.ConsentAddress):
		return "the recipient is not the address they agreed to"
	}
	return ""
}

// consentAddressSQL is, for message m, the consent address its email_consent step goes to:
// the latest call outcome recorded on a call of m's lead in m's batch, when it came with
// email consent; ” when there is none, or when m is not an email_consent step.
const consentAddressSQL = `CASE WHEN m.condition = 'email_consent' THEN coalesce((
	  SELECT CASE WHEN co.email_consent THEN co.consent_address END
	  FROM call_outcome co JOIN outreach_message cm ON cm.id = co.message_id
	  WHERE cm.lead_id = m.lead_id AND cm.batch_id = m.batch_id
	  ORDER BY co.id DESC LIMIT 1), '') ELSE '' END`

const sendRowSQL = `SELECT ` + messageCols + `, b.shadow, b.campaign_id, l.state AS lead_state, l.kind AS lead_kind,
	  o.domain AS org_domain, o.country AS org_country, ` + consentAddressSQL + ` AS consent_address
	 FROM outreach_message m
	 JOIN outreach_batch b ON b.id = m.batch_id
	 JOIN lead l ON l.id = m.lead_id
	 JOIN org o ON o.id = l.org_id`

// SendRows returns the email messages of approved batches of the given kind that are
// approved and due at or before now, oldest due first.
func (q Queries) SendRows(ctx context.Context, now time.Time, shadow bool) ([]SendRow, error) {
	rows, err := collect(ctx, q.db, pgx.RowToStructByName[SendRow],
		sendRowSQL+` WHERE m.state = 'approved' AND m.channel = 'email' AND b.approved_at IS NOT NULL
		   AND b.shadow = $2 AND m.due_at IS NOT NULL AND m.due_at <= $1
		 ORDER BY m.due_at, m.id`, now, shadow)
	if err != nil {
		return nil, fmt.Errorf("leadstore: send rows: %w", err)
	}
	return rows, nil
}

// SendRowByID returns message id as it stands now, or ErrMessageNotFound.
func (q Queries) SendRowByID(ctx context.Context, id int64) (SendRow, error) {
	rows, err := collect(ctx, q.db, pgx.RowToStructByName[SendRow], sendRowSQL+` WHERE m.id = $1`, id)
	if err != nil {
		return SendRow{}, fmt.Errorf("leadstore: send row: %w", err)
	}
	if len(rows) == 0 {
		return SendRow{}, fmt.Errorf("leadstore: send row: message %d: %w", id, ErrMessageNotFound)
	}
	return rows[0], nil
}

// SentSince counts the live emails sent, maybe sent or left sending at or after since
// (their attempt time): what today's cap has used. A shadow_sent message never counts (L-R16).
func (q Queries) SentSince(ctx context.Context, since time.Time) (int, error) {
	var n int
	if err := q.db.QueryRow(ctx,
		`SELECT count(*) FROM outreach_message m JOIN outreach_batch b ON b.id = m.batch_id
		 WHERE NOT b.shadow AND m.state IN ('sending', 'sent', 'maybe_sent') AND m.sent_at >= $1`, since).Scan(&n); err != nil {
		return 0, fmt.Errorf("leadstore: sent since: %w", err)
	}
	return n, nil
}

// StampAttempt sets the time of a sending or maybe_sent message's attempt (its sent_at),
// unless it has one, so it counts toward that day's cap. Run it in the transaction of the
// MarkSending (or MarkMaybeSent) it follows.
func (q Queries) StampAttempt(ctx context.Context, id int64, at time.Time) error {
	tag, err := q.db.Exec(ctx,
		`UPDATE outreach_message SET sent_at = coalesce(sent_at, $2) WHERE id = $1 AND state IN ('sending', 'maybe_sent')`, id, at)
	return markResult("stamp attempt", id, tag.RowsAffected(), err)
}

// Requeue puts message id (sending) back to approved, due at due, with no attempt time:
// nothing was sent.
func (q Queries) Requeue(ctx context.Context, id int64, due time.Time, reply string) error {
	tag, err := q.db.Exec(ctx,
		`UPDATE outreach_message SET state = 'approved', due_at = $2, smtp_reply = $3, sent_at = NULL
		 WHERE id = $1 AND state = 'sending'`,
		id, due, reply)
	return markResult("requeue", id, tag.RowsAffected(), err)
}

// SendingIDs returns the live messages left in state sending, oldest first. Under the send
// lock none is in flight, so each was interrupted between SMTP and its outcome: it may
// have gone, and is for the operator to check by hand.
func (q Queries) SendingIDs(ctx context.Context) ([]int64, error) {
	ids, err := collect(ctx, q.db, pgx.RowTo[int64],
		`SELECT m.id FROM outreach_message m JOIN outreach_batch b ON b.id = m.batch_id
		 WHERE NOT b.shadow AND m.state = 'sending' ORDER BY m.id`)
	if err != nil {
		return nil, fmt.Errorf("leadstore: sending ids: %w", err)
	}
	return ids, nil
}

// ScheduleFollowup gives the approved email step of lead's sequence in batch its due time,
// when it has none yet. It reports whether a message was scheduled. The legacy (NULL
// sequence) send path's call; a sequence-driven batch uses ApplyDecision.
func (q Queries) ScheduleFollowup(ctx context.Context, batchID, leadID int64, step int, due time.Time) (bool, error) {
	tag, err := q.db.Exec(ctx,
		`UPDATE outreach_message SET due_at = $4
		 WHERE batch_id = $1 AND lead_id = $2 AND step = $3 AND channel = 'email' AND state = 'approved' AND due_at IS NULL`,
		batchID, leadID, step, due)
	if err != nil {
		return false, fmt.Errorf("leadstore: schedule follow-up: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// SetDueIfUnset gives approved message id its due time when it has none yet (due_at only moves
// from NULL: a time already set is never changed). It reports whether the row changed.
func (q Queries) SetDueIfUnset(ctx context.Context, id int64, due time.Time) (bool, error) {
	tag, err := q.db.Exec(ctx,
		`UPDATE outreach_message SET due_at = $2 WHERE id = $1 AND state = 'approved' AND due_at IS NULL`, id, due)
	if err != nil {
		return false, fmt.Errorf("leadstore: set due %d: %w", id, err)
	}
	return tag.RowsAffected() == 1, nil
}

// CancelMessage cancels message id with reason when it is still pending (approved). It
// reports whether the row changed.
func (q Queries) CancelMessage(ctx context.Context, id int64, reason string) (bool, error) {
	tag, err := q.db.Exec(ctx,
		`UPDATE outreach_message SET state = 'cancelled', cancel_reason = $2 WHERE id = $1 AND state = 'approved'`, id, reason)
	if err != nil {
		return false, fmt.Errorf("leadstore: cancel message %d: %w", id, err)
	}
	return tag.RowsAffected() == 1, nil
}

// DecisionApplied is what ApplyDecision changed.
type DecisionApplied struct {
	Scheduled int // rows given a due time
	Cancelled int // rows cancelled
}

// ApplyDecision applies one campaign.Next decision: cancels first (a row both cancelled and
// due cannot happen, but a cancel wins), then due times, each only on a row still approved
// and a due time only from NULL. Rows in ascending id order, so two writers lock alike. Run
// it in the transaction that read the rows the decision was made from.
func (q Queries) ApplyDecision(ctx context.Context, d campaign.Decision) (DecisionApplied, error) {
	var res DecisionApplied
	for _, id := range slices.Sorted(maps.Keys(d.Cancel)) {
		ok, err := q.CancelMessage(ctx, id, d.Cancel[id])
		if err != nil {
			return res, err
		}
		if ok {
			res.Cancelled++
		}
	}
	for _, id := range slices.Sorted(maps.Keys(d.Due)) {
		if _, cancelled := d.Cancel[id]; cancelled {
			continue
		}
		ok, err := q.SetDueIfUnset(ctx, id, d.Due[id])
		if err != nil {
			return res, err
		}
		if ok {
			res.Scheduled++
		}
	}
	return res, nil
}

// SelftestDraft is the one message a selftest sends.
type SelftestDraft struct {
	To        string // one of sender.operator_addresses
	Subject   string
	Body      string
	MessageID string
	Due       time.Time
	// MailboxID and FromName are the mailbox it goes from and the From name frozen with it.
	MailboxID int64
	FromName  string
}

// Selftest is what CreateSelftest wrote.
type Selftest struct {
	N         int // the selftest's number: its org is selftest-<N>
	OrgID     int64
	LeadID    int64
	BatchID   int64
	MessageID int64 // outreach_message.id
}

// CreateSelftest writes one selftest (RA14): an org selftest-<n> (kind and source selftest),
// its lead (kind selftest, channel email, no route), and a live one-message batch approved
// on the operator's word (the command line names their own address), the message due at d.Due. Run
// it in a transaction.
func (q Queries) CreateSelftest(ctx context.Context, d SelftestDraft) (Selftest, error) {
	var st Selftest
	if err := q.db.QueryRow(ctx, `SELECT count(*) + 1 FROM org WHERE kind = 'selftest'`).Scan(&st.N); err != nil {
		return Selftest{}, fmt.Errorf("leadstore: create selftest: %w", err)
	}
	id, _, created, err := q.insertOrg(ctx, Org{Kind: OrgSelftest, Domain: fmt.Sprintf("selftest-%d", st.N), Source: OrgSourceSelftest})
	if err != nil {
		return Selftest{}, fmt.Errorf("leadstore: create selftest: %w", err)
	}
	if !created {
		return Selftest{}, fmt.Errorf("leadstore: create selftest: org selftest-%d exists", st.N)
	}
	st.OrgID = id
	if st.LeadID, _, err = q.UpsertLead(ctx, LeadRow{OrgID: id, Kind: LeadSelftest, Channel: ChannelEmail}); err != nil {
		return Selftest{}, fmt.Errorf("leadstore: create selftest: %w", err)
	}
	batchID, sum, err := q.CreateBatch(ctx, false, []DraftRow{{
		LeadID: st.LeadID, Step: 0, Channel: ChannelEmail, To: strings.ToLower(strings.TrimSpace(d.To)),
		Subject: d.Subject, Body: d.Body, MessageID: d.MessageID, MailboxID: d.MailboxID, FromName: d.FromName,
	}}, nil)
	if err != nil {
		return Selftest{}, fmt.Errorf("leadstore: create selftest: %w", err)
	}
	st.BatchID = batchID
	a, err := q.ApproveBatch(ctx, batchID, hex.EncodeToString(sum), nil, func(Message) time.Time { return d.Due })
	if err != nil {
		return Selftest{}, fmt.Errorf("leadstore: create selftest: %w", err)
	}
	if a.Approved != 1 {
		return Selftest{}, fmt.Errorf("leadstore: create selftest: the message was not approved (%d skipped)", a.Skipped)
	}
	if _, err := q.setLeadStateFrom(ctx, st.LeadID, []string{LeadBatched}, LeadBatched,
		fmt.Sprintf("selftest batch %d approved on the command line", batchID), ActorGremy); err != nil {
		return Selftest{}, fmt.Errorf("leadstore: create selftest: %w", err)
	}
	if err := q.db.QueryRow(ctx, `SELECT id FROM outreach_message WHERE batch_id = $1`, batchID).Scan(&st.MessageID); err != nil {
		return Selftest{}, fmt.Errorf("leadstore: create selftest: %w", err)
	}
	return st, nil
}
