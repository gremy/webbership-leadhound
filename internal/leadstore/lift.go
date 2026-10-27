package leadstore

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/gremy/webbership-leadhound/internal/outreach"
)

// The sources a lift records (suppression_lift_source_check).
const (
	LiftFromCLI     = "cli"
	LiftFromConsole = "console"
)

var (
	// ErrNoSuppression is returned by SuppressionByID and LiftSuppression for an id with no row.
	ErrNoSuppression = errors.New("leadstore: no such suppression")
	// ErrAlreadyLifted is returned by LiftSuppression for a suppression lifted before.
	ErrAlreadyLifted = errors.New("leadstore: suppression already lifted")
	// ErrLiftDomainMismatch is returned by LiftSuppression when the domain typed to confirm
	// the lift is not the suppression's (a mistyped id).
	ErrLiftDomainMismatch = errors.New("leadstore: the domain does not match the suppression")
	// ErrLiftReason is returned by LiftSuppression for a reason outside
	// suppression_lift_reason_check.
	ErrLiftReason = errors.New("leadstore: a lift needs a reason of 3 to 300 characters on one line, with no address in it")
)

// SuppressionRow is one suppression with its lift, if any. Value is the stored value, unmasked:
// callers mask addresses before showing them.
type SuppressionRow struct {
	ID                          int64
	Kind, Value, Reason, Source string
	CreatedAt                   time.Time
	LiftedAt                    *time.Time
	LiftReason, LiftSource      string
}

// Domain is the domain a lift must be confirmed with: a domain suppression's value, an
// address suppression's part after the @.
func (r SuppressionRow) Domain() string {
	if r.Kind == SuppressAddress {
		if at := strings.LastIndexByte(r.Value, '@'); at >= 0 {
			return r.Value[at+1:]
		}
	}
	return r.Value
}

// suppressionRowSQL selects every suppression with its lift, lifted ones included: the base
// table on purpose, for the operator's list and the lift itself, never to decide whether
// something may be written to (that is suppression_active's).
const suppressionRowSQL = `SELECT s.id, s.kind, s.value, s.reason, s.source, s.created_at,
	  l.lifted_at, coalesce(l.reason, ''), coalesce(l.source, '')
	FROM suppression s -- reads lifted rows on purpose
	LEFT JOIN suppression_lift l ON l.suppression_id = s.id`

// scanSuppressionRow reads one row of suppressionRowSQL's columns, then any extra ones.
func scanSuppressionRow(row pgx.CollectableRow) (SuppressionRow, error) {
	var r SuppressionRow
	err := row.Scan(&r.ID, &r.Kind, &r.Value, &r.Reason, &r.Source, &r.CreatedAt, &r.LiftedAt, &r.LiftReason, &r.LiftSource)
	return r, err
}

// SuppressionByID returns one suppression with its lift; ErrNoSuppression when there is none.
func (q Queries) SuppressionByID(ctx context.Context, id int64) (SuppressionRow, error) {
	rows, err := collect(ctx, q.db, scanSuppressionRow, suppressionRowSQL+` WHERE s.id = $1`, id)
	if err != nil {
		return SuppressionRow{}, fmt.Errorf("leadstore: suppression %d: %w", id, err)
	}
	if len(rows) == 0 {
		return SuppressionRow{}, fmt.Errorf("leadstore: suppression %d: %w", id, ErrNoSuppression)
	}
	return rows[0], nil
}

// SuppressionRows returns every suppression, active ones first (newest first), then lifted ones
// (newest lift first). limit caps each group (0: no cap).
func (q Queries) SuppressionRows(ctx context.Context, limit int) (active, lifted []SuppressionRow, err error) {
	var lim *int
	if limit > 0 {
		lim = &limit
	}
	active, err = collect(ctx, q.db, scanSuppressionRow,
		suppressionRowSQL+` WHERE l.id IS NULL ORDER BY s.created_at DESC, s.id DESC LIMIT $1`, lim)
	if err != nil {
		return nil, nil, fmt.Errorf("leadstore: suppression rows: %w", err)
	}
	lifted, err = collect(ctx, q.db, scanSuppressionRow,
		suppressionRowSQL+` WHERE l.id IS NOT NULL ORDER BY l.lifted_at DESC, l.id DESC LIMIT $1`, lim)
	if err != nil {
		return nil, nil, fmt.Errorf("leadstore: suppression rows: %w", err)
	}
	return active, lifted, nil
}

// Lifted is what LiftSuppression did.
type Lifted struct {
	Row             SuppressionRow   // as it was, with its new lift
	LeadsSuppressed int              // leads in state suppressed or opted_out this suppression reaches; their states are unchanged
	StillBlockedBy  []SuppressionRow // other active rows that still block the same value (another reason, or a domain row over an address)
}

// LiftReasonOK reports whether reason, trimmed, may be a lift's reason
// (suppression_lift_reason_check): 3 to 300 characters, one line, no @.
func LiftReasonOK(reason string) bool {
	r := strings.TrimSpace(reason)
	n := utf8.RuneCountInString(r)
	return n >= 3 && n <= 300 && !strings.ContainsAny(r, "\r\n@")
}

// LiftSuppression records that suppression id was wrong (FO6): one suppression_lift row, after
// checking that it exists and that domain (lower-cased, trimmed) equals its Domain(). The
// suppression row is never changed and never locked: SELECT … FOR UPDATE needs UPDATE on the
// table, which the app role does not hold. Two lifts of one id are settled by
// INSERT … ON CONFLICT (suppression_id) DO NOTHING RETURNING id: no row returned → ErrAlreadyLifted.
// The reason is checked here before the insert (the CHECK is the backstop) and stored trimmed.
// On ErrAlreadyLifted and ErrLiftDomainMismatch the returned Lifted.Row is the suppression as
// it stands, so the caller can say when and where it was lifted. Lead states are not changed.
// Run it in a transaction.
func (q Queries) LiftSuppression(ctx context.Context, id int64, domain, reason, source string) (Lifted, error) {
	if !LiftReasonOK(reason) {
		return Lifted{}, fmt.Errorf("leadstore: lift: %w", ErrLiftReason)
	}
	if source != LiftFromCLI && source != LiftFromConsole {
		return Lifted{}, fmt.Errorf("leadstore: lift: unknown source %q", source)
	}
	row, err := q.SuppressionByID(ctx, id)
	if err != nil {
		return Lifted{}, fmt.Errorf("leadstore: lift: %w", err)
	}
	// The domain first: a mistyped id says nothing about another row's lift.
	if strings.ToLower(strings.TrimSpace(domain)) != row.Domain() {
		return Lifted{Row: row}, fmt.Errorf("leadstore: lift: suppression %d: %w", id, ErrLiftDomainMismatch)
	}
	if row.LiftedAt != nil {
		return Lifted{Row: row}, fmt.Errorf("leadstore: lift: suppression %d: %w", id, ErrAlreadyLifted)
	}
	var liftID int64
	err = q.db.QueryRow(ctx,
		`INSERT INTO suppression_lift (suppression_id, reason, actor, source) VALUES ($1, $2, $3, $4)
		 ON CONFLICT (suppression_id) DO NOTHING RETURNING id`,
		id, strings.TrimSpace(reason), ActorOperator, source).Scan(&liftID)
	if errors.Is(err, pgx.ErrNoRows) {
		// Another lift of this id committed between the read and the insert.
		if cur, rerr := q.SuppressionByID(ctx, id); rerr == nil {
			row = cur
		}
		return Lifted{Row: row}, fmt.Errorf("leadstore: lift: suppression %d: %w", id, ErrAlreadyLifted)
	}
	if err != nil {
		return Lifted{}, fmt.Errorf("leadstore: lift: %w", err)
	}
	res := Lifted{}
	if res.Row, err = q.SuppressionByID(ctx, id); err != nil {
		return Lifted{}, fmt.Errorf("leadstore: lift: %w", err)
	}
	var accountID int64
	if err := q.db.QueryRow(ctx, `SELECT account_id FROM suppression WHERE id = $1 -- reads lifted rows on purpose: the row just lifted`, id).Scan(&accountID); err != nil {
		return Lifted{}, fmt.Errorf("leadstore: lift: account: %w", err)
	}
	if err := q.db.QueryRow(ctx,
		`SELECT count(*)::int FROM lead l JOIN org o ON o.id = l.org_id LEFT JOIN contact_route r ON r.id = l.route_id
		 WHERE l.state IN ('suppressed', 'opted_out') AND `+reachSQL(row.Kind)+` AND `+inAccountSQL,
		row.Value, accountID).Scan(&res.LeadsSuppressed); err != nil {
		return Lifted{}, fmt.Errorf("leadstore: lift: leads: %w", err)
	}
	if res.StillBlockedBy, err = q.stillBlocking(ctx, accountID, row); err != nil {
		return Lifted{}, fmt.Errorf("leadstore: lift: %w", err)
	}
	return res, nil
}

// stillBlocking returns the active rows that, after row's lift, still block its value: through
// outreach.Suppressed over SuppressionSet (the send-time check), each active row that matches
// on its own. Only the rows of the lifted row's account accountID count.
func (q Queries) stillBlocking(ctx context.Context, accountID int64, row SuppressionRow) ([]SuppressionRow, error) {
	addr, dom := "", row.Value
	if row.Kind == SuppressAddress {
		addr, dom = row.Value, ""
	}
	set, err := q.SuppressionSet(ctx, accountID)
	if err != nil {
		return nil, err
	}
	if !outreach.Suppressed(addr, dom, outreach.NewSuppressionSet(set.Addresses, set.Domains)) {
		return nil, nil
	}
	cands, err := collect(ctx, q.db, scanSuppressionRow,
		`SELECT s.id, s.kind, s.value, s.reason, s.source, s.created_at,
		   NULL::timestamptz, '', ''
		 FROM suppression_active s
		 WHERE s.account_id = $2 AND ((s.kind = 'address' AND s.value = $1) OR s.kind = 'domain')
		 ORDER BY s.id`, addr, accountID)
	if err != nil {
		return nil, err
	}
	var out []SuppressionRow
	for _, c := range cands {
		one := outreach.NewSuppressionSet(nil, nil)
		if c.Kind == SuppressAddress {
			one.AddAddress(c.Value)
		} else {
			one.AddDomain(c.Value)
		}
		if outreach.Suppressed(addr, dom, one) {
			out = append(out, c)
		}
	}
	return out, nil
}

// OptOutEvent is one recorded opt-out with the suppressions its message wrote (matched by
// source mailbox:<mailbox_id>:<folder>:<uid>).
type OptOutEvent struct {
	EventID, MailboxID int64
	Folder             string
	UIDValidity, UID   uint32
	FromDomain         string
	SeenAt             time.Time
	LeadID             *int64
	Suppressions       []SuppressionRow
	Ambiguous          bool   // several events share this source (the source has no UIDVALIDITY) and their transactions did not tell them apart
	ToAddress          string // the address our matched message went to (unmasked; callers mask), "" when none
}

// OptOutEvents returns every mailbox_event of kind opt_out, oldest first, with its opt_out
// suppressions, lifted ones included: matched by source, and when several events share one
// source (the source has no UIDVALIDITY), also by transaction. seen_at is the sync's Go clock
// (leadsNow), not the insert's now(), so it never equals suppression.created_at; instead
// the event and its suppressions are written in the one ApplyIncomingFor transaction and
// share its xmin. A suppression of a shared source whose xmin is no event's (a database
// restored from a dump, a write outside a transaction) goes to every event of that source,
// and those events are Ambiguous. An event from before 00007 (a source with no mailbox id)
// matches no suppression.
func (q Queries) OptOutEvents(ctx context.Context) ([]OptOutEvent, error) {
	type evRow struct {
		ev   OptOutEvent
		xmin string
		src  string
	}
	evs, err := collect(ctx, q.db, func(row pgx.CollectableRow) (evRow, error) {
		var r evRow
		var validity, uid int64
		err := row.Scan(&r.ev.EventID, &r.ev.MailboxID, &r.ev.Folder, &validity, &uid, &r.ev.FromDomain,
			&r.ev.SeenAt, &r.ev.LeadID, &r.ev.ToAddress, &r.xmin, &r.src)
		r.ev.UIDValidity, r.ev.UID = uint32(validity), uint32(uid)
		return r, err
	}, `SELECT e.id, e.mailbox_id, e.mailbox, e.uidvalidity, e.uid, e.from_domain, e.seen_at, e.lead_id,
	     coalesce(m.to_address, ''), e.xmin::text,
	     'mailbox:' || e.mailbox_id || ':' || e.mailbox || ':' || e.uid
	   FROM mailbox_event e LEFT JOIN outreach_message m ON m.id = e.matched_message_id
	   WHERE e.kind = 'opt_out'
	   ORDER BY e.seen_at, e.id`)
	if err != nil {
		return nil, fmt.Errorf("leadstore: opt-out events: %w", err)
	}
	type supRow struct {
		row  SuppressionRow
		xmin string
	}
	sups, err := collect(ctx, q.db, func(row pgx.CollectableRow) (supRow, error) {
		var r supRow
		err := row.Scan(&r.row.ID, &r.row.Kind, &r.row.Value, &r.row.Reason, &r.row.Source, &r.row.CreatedAt,
			&r.row.LiftedAt, &r.row.LiftReason, &r.row.LiftSource, &r.xmin)
		return r, err
	}, `SELECT s.id, s.kind, s.value, s.reason, s.source, s.created_at,
	     l.lifted_at, coalesce(l.reason, ''), coalesce(l.source, ''), s.xmin::text
	   FROM suppression s -- reads lifted rows on purpose
	   LEFT JOIN suppression_lift l ON l.suppression_id = s.id
	   WHERE s.reason = 'opt_out' AND s.source IN (
	     SELECT 'mailbox:' || e.mailbox_id || ':' || e.mailbox || ':' || e.uid FROM mailbox_event e WHERE e.kind = 'opt_out')
	   ORDER BY s.id`)
	if err != nil {
		return nil, fmt.Errorf("leadstore: opt-out events: %w", err)
	}
	bySource := map[string][]int{} // source → indexes into evs
	for i, e := range evs {
		bySource[e.src] = append(bySource[e.src], i)
	}
	out := make([]OptOutEvent, len(evs))
	for i, e := range evs {
		out[i] = e.ev
	}
	for _, s := range sups {
		group := bySource[s.row.Source]
		if len(group) == 1 {
			out[group[0]].Suppressions = append(out[group[0]].Suppressions, s.row)
			continue
		}
		matched := false
		for _, i := range group {
			if evs[i].xmin == s.xmin {
				out[i].Suppressions = append(out[i].Suppressions, s.row)
				matched = true
			}
		}
		if !matched {
			for _, i := range group {
				out[i].Suppressions = append(out[i].Suppressions, s.row)
				out[i].Ambiguous = true
			}
		}
	}
	return out, nil
}
