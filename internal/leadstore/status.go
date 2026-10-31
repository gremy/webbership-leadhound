package leadstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

// What the console's Dashboard and Status page read: the paid services' last readings
// (service_status, 00026), the Claude top-ups (claude_topup, 00026), each mailbox's health,
// the day's email checks, the leads waiting on the operator and each campaign's funnel.

// The providers service_status holds a row for (service_status_provider_check).
const (
	ServiceReoon  = "reoon"
	ServiceClaude = "claude"
)

// MaxServiceError is the longest error RecordServiceError stores, in characters
// (service_status_error_check allows 500).
const MaxServiceError = 300

var (
	// ErrServiceNotFound is returned by ServiceStatus for a provider never recorded.
	ErrServiceNotFound = errors.New("leadstore: no service status")
	// ErrBadServiceStatus is returned for a reading that is not a JSON object.
	ErrBadServiceStatus = errors.New("leadstore: bad service status")
	// ErrTopupNotFound is returned by LatestClaudeTopup when no top-up is recorded.
	ErrTopupNotFound = errors.New("leadstore: no claude top-up")
)

// ServiceStatusRow is one service_status row. Data and FetchedAt are the last good reading
// (nil before the first); Error and ErrorAt the last failure ("" and nil when none ever). A
// failure is current when ErrorAt is after FetchedAt (or there is no reading).
type ServiceStatusRow struct {
	Provider  string          `db:"provider"`
	Data      json.RawMessage `db:"data"`
	FetchedAt *time.Time      `db:"fetched_at"`
	Error     string          `db:"error"`
	ErrorAt   *time.Time      `db:"error_at"`
}

// RecordServiceOK stores data (a JSON object) as provider's last good reading, taken at at.
// The last error is left as it is. A reading older than the one stored is ignored, so a late
// writer never puts an older reading back. A provider outside the closed set is refused by
// the table (a check violation); data that is not a JSON object is ErrBadServiceStatus.
func (q Queries) RecordServiceOK(ctx context.Context, provider string, data []byte, at time.Time) error {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(data, &obj); err != nil || obj == nil {
		return fmt.Errorf("leadstore: record service ok %s: %w: not a JSON object", provider, ErrBadServiceStatus)
	}
	if _, err := q.db.Exec(ctx,
		`INSERT INTO service_status (provider, data, fetched_at) VALUES ($1, $2::jsonb, $3)
		 ON CONFLICT (provider) DO UPDATE SET data = EXCLUDED.data, fetched_at = EXCLUDED.fetched_at
		 WHERE service_status.fetched_at IS NULL OR service_status.fetched_at <= EXCLUDED.fetched_at`,
		provider, string(data), at); err != nil {
		return fmt.Errorf("leadstore: record service ok %s: %w", provider, err)
	}
	return nil
}

// RecordServiceError stores msg, cut to its first line and MaxServiceError characters
// ("failed" when that leaves nothing), as provider's last failure, at at. The last good
// reading is left as it is. A failure older than the one stored is ignored.
func (q Queries) RecordServiceError(ctx context.Context, provider, msg string, at time.Time) error {
	if _, err := q.db.Exec(ctx,
		`INSERT INTO service_status (provider, error, error_at) VALUES ($1, $2, $3)
		 ON CONFLICT (provider) DO UPDATE SET error = EXCLUDED.error, error_at = EXCLUDED.error_at
		 WHERE service_status.error_at IS NULL OR service_status.error_at <= EXCLUDED.error_at`,
		provider, serviceErrorLine(msg), at); err != nil {
		return fmt.Errorf("leadstore: record service error %s: %w", provider, err)
	}
	return nil
}

// serviceErrorLine is msg's first non-blank line, trimmed and cut to MaxServiceError
// characters (valid UTF-8 kept whole); "failed" when msg has none.
func serviceErrorLine(msg string) string {
	line := ""
	for l := range strings.Lines(strings.ToValidUTF8(msg, "?")) {
		if l = strings.TrimSpace(l); l != "" {
			line = l
			break
		}
	}
	if line == "" {
		return "failed"
	}
	if utf8.RuneCountInString(line) > MaxServiceError {
		line = string([]rune(line)[:MaxServiceError-1]) + "…"
	}
	return line
}

// ServiceStatus returns provider's row, or ErrServiceNotFound when nothing was ever recorded
// for it.
func (q Queries) ServiceStatus(ctx context.Context, provider string) (ServiceStatusRow, error) {
	rows, err := collect(ctx, q.db, pgx.RowToStructByName[ServiceStatusRow],
		`SELECT provider, data, fetched_at, error, error_at FROM service_status WHERE provider = $1`, provider)
	if err != nil {
		return ServiceStatusRow{}, fmt.Errorf("leadstore: service status %s: %w", provider, err)
	}
	if len(rows) == 0 {
		return ServiceStatusRow{}, fmt.Errorf("leadstore: service status %s: %w", provider, ErrServiceNotFound)
	}
	return rows[0], nil
}

// ClaudeTopup is one claude_topup row.
type ClaudeTopup struct {
	ID          int64     `db:"id"`
	AmountCents int64     `db:"amount_cents"`
	At          time.Time `db:"at"` // when the top-up was made
	CreatedAt   time.Time `db:"created_at"`
}

// AddClaudeTopup records a top-up of amountCents (above 0, else the table refuses it) made
// at at.
func (q Queries) AddClaudeTopup(ctx context.Context, amountCents int64, at time.Time) error {
	if _, err := q.db.Exec(ctx, `INSERT INTO claude_topup (amount_cents, at) VALUES ($1, $2)`, amountCents, at); err != nil {
		return fmt.Errorf("leadstore: add claude top-up: %w", err)
	}
	return nil
}

// LatestClaudeTopup returns the latest top-up by when it was made (then by id), or
// ErrTopupNotFound when none is recorded.
func (q Queries) LatestClaudeTopup(ctx context.Context) (ClaudeTopup, error) {
	rows, err := collect(ctx, q.db, pgx.RowToStructByName[ClaudeTopup],
		`SELECT id, amount_cents, at, created_at FROM claude_topup ORDER BY at DESC, id DESC LIMIT 1`)
	if err != nil {
		return ClaudeTopup{}, fmt.Errorf("leadstore: latest claude top-up: %w", err)
	}
	if len(rows) == 0 {
		return ClaudeTopup{}, fmt.Errorf("leadstore: latest claude top-up: %w", ErrTopupNotFound)
	}
	return rows[0], nil
}

// SenderCheckRow is the latest sender_check of a mailbox's host and DKIM selector.
type SenderCheckRow struct {
	SPF, DKIM, DMARC bool
	Note             string
	CheckedAt        time.Time
}

// MailboxHealthRow is one sending mailbox as the Status page shows it.
type MailboxHealthRow struct {
	ID       int64
	Address  string
	Domain   string     // the registrable domain of Address
	FromName string     // empty: leads.yaml sender.name
	State    string     // active or paused (the latest mailbox_state row; active when none)
	Reason   string     // the latest state row's reason ("" when none)
	StateAt  *time.Time // nil when no state row
	// LastCheck is the latest guided check, of any outcome (requested ones included); nil
	// when none.
	LastCheck *MailboxCheck
	// SenderCheck is the latest SPF/DKIM/DMARC check of its host with its selector; nil when
	// none.
	SenderCheck *SenderCheckRow
	SentToday   int        // live emails sent, maybe sent or sending since the day's start
	Sent7d      int        // the same, over the last seven local days (today included)
	Bounces7d   int        // hard bounces among its live sends of those seven days
	SyncedAt    *time.Time // the latest reply sync of any of its folders; nil when never
}

// MailboxHealth returns every mailbox that is not retired, of every account, by id: its
// state, its latest guided check and sender check, what it sent today (day, the start of
// the operator's local day, to now) and over the last seven local days (from the start of
// the day six days before day, in loc, to now), its hard bounces over the same seven days
// and when its replies were last synced. Retired mailboxes are left out: they send nothing
// and are shown on the Mailboxes page.
//
// Sent is SentSinceByMailbox's rule (state sending, sent or maybe_sent, live batches, by
// attempt time, selftests included, as the daily cap counts them); a hard bounce is
// HardBouncesInLastSends' rule (hard_bounce set on a sent, refused or maybe_sent message of a
// live batch, selftests left out), by attempt time. The password is never read.
func (q Queries) MailboxHealth(ctx context.Context, day time.Time, loc *time.Location, now time.Time) ([]MailboxHealthRow, error) {
	if loc == nil {
		loc = time.Local
	}
	d := day.In(loc)
	week := time.Date(d.Year(), d.Month(), d.Day()-6, 0, 0, 0, 0, loc)
	rows, err := q.db.Query(ctx,
		`SELECT mb.id, mb.address, mb.domain, mb.from_name,
		   coalesce(st.state, 'active'), coalesce(st.reason, ''), st.at,
		   ck.id, ck.requested_at, ck.actor, ck.checked_at, ck.smtp_ok, ck.imap_ok, ck.folders_ok,
		   ck.spf, ck.dkim, ck.dmarc, ck.fix, ck.outcome, ck.landing_url,
		   sc.spf, sc.dkim, sc.dmarc, sc.note, sc.checked_at,
		   sd.today, sd.week, sd.bounces, cur.synced
		 FROM sender_mailbox mb
		 `+latestStateSQL+`
		 LEFT JOIN LATERAL (
		   SELECT c.* FROM mailbox_check c WHERE c.mailbox_id = mb.id ORDER BY c.id DESC LIMIT 1
		 ) ck ON true
		 LEFT JOIN LATERAL (
		   SELECT s.spf, s.dkim, s.dmarc, s.note, s.checked_at FROM sender_check s
		   WHERE s.domain_sha256 = sha256(convert_to(`+hostSQL+`, 'UTF8')) AND s.selector = mb.dkim_selector
		     AND s.selector <> ''
		   ORDER BY s.id DESC LIMIT 1
		 ) sc ON true
		 LEFT JOIN LATERAL (
		   SELECT
		     count(*) FILTER (WHERE m.state IN `+liveSentStates+` AND m.sent_at >= $1) AS today,
		     count(*) FILTER (WHERE m.state IN `+liveSentStates+`) AS week,
		     count(*) FILTER (WHERE m.hard_bounce AND l.kind <> 'selftest'
		       AND m.state IN ('sent', 'refused', 'maybe_sent')) AS bounces
		   FROM outreach_message m JOIN outreach_batch b ON b.id = m.batch_id JOIN lead l ON l.id = m.lead_id
		   WHERE m.mailbox_id = mb.id AND NOT b.shadow AND m.sent_at >= $2 AND m.sent_at <= $3
		 ) sd ON true
		 LEFT JOIN LATERAL (
		   SELECT max(c.synced_at) AS synced FROM mailbox_cursor c WHERE c.mailbox_id = mb.id
		 ) cur ON true
		 WHERE coalesce(st.state, 'active') <> 'retired'
		 ORDER BY mb.id`, day, week, now)
	if err != nil {
		return nil, fmt.Errorf("leadstore: mailbox health: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (MailboxHealthRow, error) {
		var r MailboxHealthRow
		var c MailboxCheck
		var checkID *int64
		var requestedAt *time.Time
		var actor, fix, outcome *string
		var spf, dkim, dmarc *bool
		var note *string
		var scAt *time.Time
		err := row.Scan(&r.ID, &r.Address, &r.Domain, &r.FromName, &r.State, &r.Reason, &r.StateAt,
			&checkID, &requestedAt, &actor, &c.CheckedAt, &c.SMTPOK, &c.IMAPOK, &c.FoldersOK,
			&c.SPF, &c.DKIM, &c.DMARC, &fix, &outcome, &c.LandingURL,
			&spf, &dkim, &dmarc, &note, &scAt,
			&r.SentToday, &r.Sent7d, &r.Bounces7d, &r.SyncedAt)
		if err != nil {
			return MailboxHealthRow{}, err
		}
		if checkID != nil {
			c.ID, c.MailboxID, c.RequestedAt, c.Actor, c.Fix, c.Outcome = *checkID, r.ID, *requestedAt, *actor, *fix, *outcome
			r.LastCheck = &c
		}
		if scAt != nil {
			r.SenderCheck = &SenderCheckRow{SPF: *spf, DKIM: *dkim, DMARC: *dmarc, Note: *note, CheckedAt: *scAt}
		}
		return r, nil
	})
	if err != nil {
		return nil, fmt.Errorf("leadstore: mailbox health: %w", err)
	}
	return out, nil
}

// EmailCheckCounts are the email checks of a span by what they found. Valid, Invalid,
// CatchAll and Other add up to Total; ByStatus holds every stored status word as it is.
type EmailCheckCounts struct {
	Total    int
	Valid    int // safe, role_account (both send)
	Invalid  int // invalid, disabled, disposable, spamtrap, inbox_full (all block)
	CatchAll int // catch_all
	Other    int // unknown, and any word not named here
	ByStatus map[string]int
}

// EmailChecksOn counts the email checks (every account, every provider: today only Reoon)
// made in [from, to), by status. No checks is all zeros and an empty, non-nil ByStatus.
func (q Queries) EmailChecksOn(ctx context.Context, from, to time.Time) (EmailCheckCounts, error) {
	out := EmailCheckCounts{ByStatus: map[string]int{}}
	rows, err := q.db.Query(ctx,
		`SELECT status, count(*) FROM email_check WHERE checked_at >= $1 AND checked_at < $2 GROUP BY status`, from, to)
	if err != nil {
		return EmailCheckCounts{}, fmt.Errorf("leadstore: email checks on: %w", err)
	}
	var status string
	var n int
	if _, err := pgx.ForEachRow(rows, []any{&status, &n}, func() error {
		out.ByStatus[status] = n
		out.Total += n
		switch status {
		case "safe", "role_account":
			out.Valid += n
		case "invalid", "disabled", "disposable", "spamtrap", "inbox_full":
			out.Invalid += n
		case "catch_all":
			out.CatchAll += n
		default:
			out.Other += n
		}
		return nil
	}); err != nil {
		return EmailCheckCounts{}, fmt.Errorf("leadstore: email checks on: %w", err)
	}
	return out, nil
}

// What a NeedsMeRow says happened.
const (
	NeedsReplied    = "replied"
	NeedsConnected  = "connected"
	NeedsNextAction = "next_action"
)

// NeedsMeRow is one lead waiting on the operator.
type NeedsMeRow struct {
	LeadID int64
	Name   string // as the Leads list names it: the site's name, else the org's, else its domain
	Domain string
	What   string // NeedsReplied, NeedsConnected or NeedsNextAction
	// At is when it happened: the reply or the connected call; for a next action, the start
	// of the local day it fell due.
	At         time.Time
	NextAction string // the operator's next action (lead_crm.next_action); "" when none
}

// needsMeLimitMax is the most rows NeedsMe returns.
const needsMeLimitMax = 100

// NeedsMe is NeedsMeAt at the clock's now in the machine's local zone. It is kept for the
// Dashboard until it moves to NeedsMeAt, which takes now and the operator's zone, so the day a
// next action falls due is the operator's.
func (q Queries) NeedsMe(ctx context.Context, limit int) ([]NeedsMeRow, int, error) {
	return q.NeedsMeAt(ctx, limit, time.Now(), time.Local)
}

// NeedsMeAt returns the leads waiting on the operator at now, at most limit (1..100; out of
// range is clamped), and how many there are in all. Today is now's calendar day in loc (nil:
// time.Local). The rule:
//
//   - next_action: the lead's next action date (lead_crm.next_action_on) is today or earlier,
//     whatever its state, carrying the next action's text;
//   - replied: the lead's state is replied (the mailbox matched a reply to it), at the time
//     it entered that state;
//   - connected: the lead's state is contacted and the latest outcome of one of its call
//     steps in a live batch is connected, at the latest such outcome;
//
// and for replied and connected, nothing is scheduled ahead for it (no message of a live
// batch that is not discarded is still draft, approved (an email or a call, whatever its due
// time) or sending) and its next action is not set for a later day: the operator has already
// said when they will come back to it. A lead is listed once, as next_action when it has one
// due. A lead snoozed past now (lead_crm.snoozed_until) is left out whatever it waits on, and
// selftest leads never count. A lead the operator has moved on (closed, skipped, opted out,
// ...) is no longer replied or contacted, so it drops off unless a next action is due.
//
// Order: the due next actions first, the longest overdue first; then the rest newest first
// (then by lead id).
func (q Queries) NeedsMeAt(ctx context.Context, limit int, now time.Time, loc *time.Location) ([]NeedsMeRow, int, error) {
	limit = min(max(limit, 1), needsMeLimitMax)
	if loc == nil {
		loc = time.Local
	}
	today := now.In(loc).Format(time.DateOnly)
	rows, err := q.db.Query(ctx,
		`WITH cand AS (
		   SELECT l.id, 'replied' AS what, l.state_at AS at FROM lead l
		   WHERE l.state = 'replied' AND l.kind <> 'selftest'
		   UNION ALL
		   SELECT l.id, 'connected', max(co.at) FROM lead l
		   JOIN outreach_message m ON m.lead_id = l.id AND m.channel = 'phone'
		   JOIN outreach_batch b ON b.id = m.batch_id AND NOT b.shadow
		   JOIN LATERAL (
		     SELECT c.outcome, c.at FROM call_outcome c WHERE c.message_id = m.id ORDER BY c.id DESC LIMIT 1
		   ) co ON co.outcome = 'connected'
		   WHERE l.state = 'contacted' AND l.kind <> 'selftest'
		   GROUP BY l.id
		 ), waiting AS (
		   SELECT c.id, c.what, c.at, 1 AS prio FROM cand c
		   WHERE NOT EXISTS (
		     SELECT 1 FROM outreach_message m JOIN outreach_batch b ON b.id = m.batch_id
		     WHERE m.lead_id = c.id AND NOT b.shadow AND b.discarded_at IS NULL
		       AND m.state IN ('draft', 'approved', 'sending'))
		   AND NOT EXISTS (SELECT 1 FROM lead_crm k WHERE k.lead_id = c.id AND k.next_action_on > $3::date)
		   UNION ALL
		   SELECT k.lead_id, 'next_action', NULL::timestamptz, 0 FROM lead_crm k JOIN lead l ON l.id = k.lead_id
		   WHERE k.next_action_on <= $3::date AND l.kind <> 'selftest'
		 ), one AS (
		   SELECT DISTINCT ON (w.id) w.id, w.what, w.at, w.prio FROM waiting w ORDER BY w.id, w.prio
		 )
		 SELECT c.id, coalesce(nullif(p.site_name, ''), nullif(o.name, ''), o.domain), o.domain, c.what, c.at,
		   coalesce(k.next_action, ''), k.next_action_on, count(*) OVER ()
		 FROM one c JOIN lead l ON l.id = c.id JOIN org o ON o.id = l.org_id
		 LEFT JOIN site_probe p ON p.id = l.probe_id
		 LEFT JOIN lead_crm k ON k.lead_id = c.id
		 WHERE k.snoozed_until IS NULL OR k.snoozed_until <= $2
		 ORDER BY c.prio, CASE WHEN c.prio = 0 THEN k.next_action_on END NULLS LAST, c.at DESC, c.id DESC
		 LIMIT $1`, limit, now, today)
	if err != nil {
		return nil, 0, fmt.Errorf("leadstore: needs me: %w", err)
	}
	var out []NeedsMeRow
	total := 0
	var r NeedsMeRow
	var at, on *time.Time
	if _, err := pgx.ForEachRow(rows, []any{&r.LeadID, &r.Name, &r.Domain, &r.What, &at, &r.NextAction, &on, &total}, func() error {
		row := r
		switch {
		case row.What == NeedsNextAction && on != nil:
			y, m, d := on.Date()
			row.At = time.Date(y, m, d, 0, 0, 0, 0, loc)
		case at != nil:
			row.At = *at
		}
		out = append(out, row)
		return nil
	}); err != nil {
		return nil, 0, fmt.Errorf("leadstore: needs me: %w", err)
	}
	return out, total, nil
}

// CampaignFunnelRow is one campaign's funnel since a time.
type CampaignFunnelRow struct {
	CampaignID int64
	Slug, Name string
	Sent       int // live emails sent, maybe sent or sending since (selftests left out)
	Replied    int // its leads that entered replied since
	Positive   int // orgs that agreed on a connected call to get its email since
	// Contacted counts its leads reached since: a live email sent (as Sent counts them) or a
	// call outcome recorded on a call step of a live batch, selftests left out. The divisor of
	// cost per lead.
	Contacted int
	Won       int // its leads moved to stage won since that are still won
	// Revenue sums the deal values of those won leads, in cents, per currency (never
	// converted); a won lead with no value adds nothing. Empty, never nil.
	Revenue map[string]int64
}

// CampaignFunnel returns, per campaign of every account (by id), what it did since since;
// a campaign with nothing in any column is left out.
//
//   - Sent counts its email messages (the batch's campaign) of live batches in state sending,
//     sent or maybe_sent attempted at or after since, selftest leads left out.
//   - Replied counts its leads (the lead's campaign) with a lead_event into replied at or
//     after since.
//   - Positive counts the orgs with an email_consent for it given at or after since, on a
//     call of a live batch: the one thing the schema records as a lead saying yes. A reply's
//     tone is not recorded, so a reply alone never counts as positive.
//   - Contacted counts its leads (the batch's campaign) with a live email sent since or a call
//     outcome at or after since on a live batch's call step, selftests left out.
//   - Won counts its leads (the lead's campaign) with a lead_stage_event into won at or after
//     since whose stage is still won, selftests left out; Revenue sums their deal values per
//     currency.
func (q Queries) CampaignFunnel(ctx context.Context, since time.Time) ([]CampaignFunnelRow, error) {
	rows, err := q.db.Query(ctx,
		`WITH sent AS (
		   SELECT b.campaign_id, count(*) AS n FROM outreach_message m
		   JOIN outreach_batch b ON b.id = m.batch_id JOIN lead l ON l.id = m.lead_id
		   WHERE NOT b.shadow AND m.channel = 'email' AND m.state IN `+liveSentStates+`
		     AND m.sent_at >= $1 AND l.kind <> 'selftest'
		   GROUP BY b.campaign_id
		 ), replied AS (
		   SELECT l.campaign_id, count(DISTINCT l.id) AS n FROM lead_event e JOIN lead l ON l.id = e.lead_id
		   WHERE e.state = 'replied' AND e.at >= $1 AND l.kind <> 'selftest'
		   GROUP BY l.campaign_id
		 ), positive AS (
		   SELECT ec.campaign_id, count(DISTINCT ec.org_id) AS n FROM email_consent ec
		   JOIN call_outcome co ON co.id = ec.call_outcome_id
		   JOIN outreach_message m ON m.id = co.message_id JOIN outreach_batch b ON b.id = m.batch_id
		   WHERE ec.at >= $1 AND NOT b.shadow
		   GROUP BY ec.campaign_id
		 ), contacted AS (
		   SELECT x.campaign_id, count(DISTINCT x.lead_id) AS n FROM (
		     SELECT b.campaign_id, m.lead_id FROM outreach_message m
		     JOIN outreach_batch b ON b.id = m.batch_id
		     WHERE NOT b.shadow AND m.channel = 'email' AND m.state IN `+liveSentStates+` AND m.sent_at >= $1
		     UNION ALL
		     SELECT b.campaign_id, m.lead_id FROM call_outcome co
		     JOIN outreach_message m ON m.id = co.message_id JOIN outreach_batch b ON b.id = m.batch_id
		     WHERE NOT b.shadow AND co.at >= $1
		   ) x JOIN lead l ON l.id = x.lead_id
		   WHERE l.kind <> 'selftest'
		   GROUP BY x.campaign_id
		 ), wonl AS (
		   SELECT DISTINCT l.campaign_id, l.id, k.currency, k.deal_value_cents FROM lead_stage_event e
		   JOIN lead l ON l.id = e.lead_id JOIN lead_crm k ON k.lead_id = l.id
		   WHERE e.stage = 'won' AND e.at >= $1 AND k.stage = 'won' AND l.kind <> 'selftest'
		 ), won AS (
		   SELECT campaign_id, count(*) AS n FROM wonl GROUP BY campaign_id
		 ), revenue AS (
		   SELECT campaign_id, jsonb_object_agg(currency, cents) AS money FROM (
		     SELECT campaign_id, currency, sum(deal_value_cents)::bigint AS cents FROM wonl
		     WHERE deal_value_cents IS NOT NULL GROUP BY campaign_id, currency
		   ) y GROUP BY campaign_id
		 )
		 SELECT c.id, c.slug, c.name, coalesce(s.n, 0), coalesce(r.n, 0), coalesce(p.n, 0),
		   coalesce(ct.n, 0), coalesce(w.n, 0), coalesce(rv.money, '{}'::jsonb)
		 FROM campaign c
		 LEFT JOIN sent s ON s.campaign_id = c.id
		 LEFT JOIN replied r ON r.campaign_id = c.id
		 LEFT JOIN positive p ON p.campaign_id = c.id
		 LEFT JOIN contacted ct ON ct.campaign_id = c.id
		 LEFT JOIN won w ON w.campaign_id = c.id
		 LEFT JOIN revenue rv ON rv.campaign_id = c.id
		 WHERE s.n IS NOT NULL OR r.n IS NOT NULL OR p.n IS NOT NULL OR ct.n IS NOT NULL OR w.n IS NOT NULL
		 ORDER BY c.id`, since)
	if err != nil {
		return nil, fmt.Errorf("leadstore: campaign funnel: %w", err)
	}
	var out []CampaignFunnelRow
	var r CampaignFunnelRow
	var money map[string]int64
	if _, err := pgx.ForEachRow(rows, []any{&r.CampaignID, &r.Slug, &r.Name, &r.Sent, &r.Replied, &r.Positive,
		&r.Contacted, &r.Won, &money}, func() error {
		row := r
		row.Revenue = map[string]int64{}
		for k, v := range money {
			row.Revenue[k] = v
		}
		money = nil
		out = append(out, row)
		return nil
	}); err != nil {
		return nil, fmt.Errorf("leadstore: campaign funnel: %w", err)
	}
	return out, nil
}
