package leadstore

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// The CRM after a reply (00028): a lead's pipeline stage, the operator's note and next action,
// its deal, snooze, its timeline, the replies inbox, the pipeline board and the call queue.
// Every write here is the operator's, through the console; the app role may write lead_crm
// (every column a pointer) and append to lead_stage_event.

// The pipeline stages (lead_crm_stage_check), in board order.
const (
	StageTalking  = "talking"
	StageProposal = "proposal"
	StageWon      = "won"
	StageLost     = "lost"
)

// Stages lists every pipeline stage, in board order.
var Stages = []string{StageTalking, StageProposal, StageWon, StageLost}

// Currencies lists the currencies a deal may be in (lead_crm_currency_check); sums are kept
// per currency and never converted.
var Currencies = []string{"GBP", "EUR", "RON", "USD"}

// DefaultCurrency is a deal's currency when none is given.
const DefaultCurrency = "GBP"

// The CRM text limits, in characters (the lead_crm checks).
const (
	MaxCRMNote    = 2000
	MaxNextAction = 200
	MaxLostReason = 200
	MaxTenantURL  = 500
)

// ErrBadCRM is returned, wrapped with a sentence saying why, for CRM input the table would
// refuse; nothing is written.
var ErrBadCRM = errors.New("bad CRM input")

// implicitTalkingSQL is true for lead l that counts as talking without a stored stage: it
// replied (state replied), or it is contacted and one of its call steps in a live batch has a
// connected outcome. NeedsMe's replied and connected rules read the same states.
const implicitTalkingSQL = `(l.state = 'replied' OR (l.state = 'contacted' AND EXISTS (
	  SELECT 1 FROM call_outcome co JOIN outreach_message cm ON cm.id = co.message_id
	  JOIN outreach_batch cb ON cb.id = cm.batch_id
	  WHERE cm.lead_id = l.id AND NOT cb.shadow AND co.outcome = 'connected')))`

// effectiveStageSQL is lead l's stage as the CRM shows it, with its lead_crm row as k (LEFT
// JOINed): the stored stage, else talking when implicitTalkingSQL holds, else ”.
const effectiveStageSQL = `coalesce(k.stage, CASE WHEN ` + implicitTalkingSQL + ` THEN 'talking' END, '')`

// leadNameSQL names lead l (org o, probe p LEFT JOINed) as the Leads list and NeedsMe do: the
// site's name, else the org's, else its domain.
const leadNameSQL = `coalesce(nullif(p.site_name, ''), nullif(o.name, ''), o.domain)`

// LeadCRM is a lead's CRM state. With no lead_crm row every stored field is its zero value
// (Currency DefaultCurrency) and UpdatedAt is nil.
type LeadCRM struct {
	LeadID int64
	// Stage is the stored stage; "" when the operator has set none.
	Stage string
	// EffectiveStage is the stage the CRM shows: Stage, else StageTalking when the lead replied
	// or is contacted with a connected call, else "".
	EffectiveStage string
	LostReason     string
	Note           string
	NextAction     string
	NextActionOn   *time.Time // the day the next action is due (midnight UTC of that date); nil when none
	DealValueCents *int64     // nil when no value is set
	Currency       string
	TenantURL      string     // the won lead's Webbership tenant; "" when none
	SnoozedUntil   *time.Time // nil when not snoozed
	UpdatedAt      *time.Time // nil when there is no row
}

// GetLeadCRM returns lead leadID's CRM state (the zero state with its EffectiveStage when it
// has no row). A missing lead is ErrLeadNotFound.
func (q Queries) GetLeadCRM(ctx context.Context, leadID int64) (LeadCRM, error) {
	c := LeadCRM{LeadID: leadID}
	var stage, tenant *string
	err := q.db.QueryRow(ctx,
		`SELECT k.stage, `+effectiveStageSQL+`, coalesce(k.lost_reason, ''), coalesce(k.note, ''),
		   coalesce(k.next_action, ''), k.next_action_on, k.deal_value_cents, coalesce(k.currency, $2),
		   k.tenant_url, k.snoozed_until, k.updated_at
		 FROM lead l LEFT JOIN lead_crm k ON k.lead_id = l.id WHERE l.id = $1`, leadID, DefaultCurrency).
		Scan(&stage, &c.EffectiveStage, &c.LostReason, &c.Note, &c.NextAction, &c.NextActionOn, &c.DealValueCents,
			&c.Currency, &tenant, &c.SnoozedUntil, &c.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return LeadCRM{}, fmt.Errorf("leadstore: lead crm %d: %w", leadID, ErrLeadNotFound)
	}
	if err != nil {
		return LeadCRM{}, fmt.Errorf("leadstore: lead crm %d: %w", leadID, err)
	}
	if stage != nil {
		c.Stage = *stage
	}
	if tenant != nil {
		c.TenantURL = *tenant
	}
	return c, nil
}

// oneLine checks s (already trimmed) is at most max characters with no line break; what
// names it in the error.
func oneLine(s, what string, max int) error {
	switch {
	case strings.ContainsAny(s, "\r\n"):
		return fmt.Errorf("%w: the %s must be one line", ErrBadCRM, what)
	case utf8.RuneCountInString(s) > max:
		return fmt.Errorf("%w: the %s is over %d characters", ErrBadCRM, what, max)
	}
	return nil
}

// checkTenantURL checks u (trimmed, not empty) is an https URL with a host, at most
// MaxTenantURL characters.
func checkTenantURL(u string) error {
	p, err := url.Parse(u)
	switch {
	case utf8.RuneCountInString(u) > MaxTenantURL:
		return fmt.Errorf("%w: the tenant link is over %d characters", ErrBadCRM, MaxTenantURL)
	case err != nil || p.Scheme != "https" || p.Host == "" || p.User != nil || strings.ContainsAny(u, " \t\r\n") ||
		!strings.HasPrefix(u, "https://"):
		return fmt.Errorf("%w: the tenant link must be an https:// address", ErrBadCRM)
	}
	return nil
}

// notFound turns a foreign-key violation on lead_crm or lead_stage_event (the lead does not
// exist) into ErrLeadNotFound.
func notFound(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23503" {
		return ErrLeadNotFound
	}
	return err
}

// SetLeadStage moves lead leadID to stage (one of Stages) at now, in one transaction: the
// lead_crm row is written (created when missing) and, when the stage changes, a
// lead_stage_event is appended. Lost needs reason (one line, at most MaxLostReason
// characters), kept as the lost reason; for any other stage reason is ignored and the lost
// reason cleared. tenantURL (an https:// address, at most MaxTenantURL characters) goes only
// with won; "" keeps the one already stored while the lead stays won. Leaving won clears it.
// Bad input is ErrBadCRM and writes nothing; a missing lead is ErrLeadNotFound.
func (q Queries) SetLeadStage(ctx context.Context, leadID int64, stage, reason, tenantURL string, now time.Time) error {
	reason, tenantURL = strings.TrimSpace(reason), strings.TrimSpace(tenantURL)
	var err error
	switch {
	case !slices.Contains(Stages, stage):
		err = fmt.Errorf("%w: unknown stage %q", ErrBadCRM, stage)
	case stage == StageLost && reason == "":
		err = fmt.Errorf("%w: a lost lead needs a reason", ErrBadCRM)
	case stage == StageLost:
		err = oneLine(reason, "lost reason", MaxLostReason)
	case tenantURL != "" && stage != StageWon:
		err = fmt.Errorf("%w: a tenant link goes only with a won lead", ErrBadCRM)
	case tenantURL != "":
		err = checkTenantURL(tenantURL)
	}
	if err != nil {
		return fmt.Errorf("leadstore: set lead stage %d: %w", leadID, err)
	}
	if stage != StageLost {
		reason = ""
	}
	var tenant *string
	if tenantURL != "" {
		tenant = &tenantURL
	}
	err = q.inTx(ctx, func(tq Queries) error {
		var prev *string
		err := tq.db.QueryRow(ctx,
			`SELECT k.stage FROM lead l LEFT JOIN lead_crm k ON k.lead_id = l.id WHERE l.id = $1 FOR UPDATE OF l`, leadID).Scan(&prev)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrLeadNotFound
		}
		if err != nil {
			return err
		}
		if _, err := tq.db.Exec(ctx,
			`INSERT INTO lead_crm (lead_id, stage, lost_reason, tenant_url, updated_at) VALUES ($1, $2, $3, $4, $5)
			 ON CONFLICT (lead_id) DO UPDATE SET stage = EXCLUDED.stage, lost_reason = EXCLUDED.lost_reason,
			   tenant_url = CASE WHEN EXCLUDED.stage = 'won' THEN coalesce(EXCLUDED.tenant_url, lead_crm.tenant_url) END,
			   updated_at = EXCLUDED.updated_at`, leadID, stage, reason, tenant, now); err != nil {
			return err
		}
		if prev != nil && *prev == stage {
			return nil
		}
		_, err = tq.db.Exec(ctx, `INSERT INTO lead_stage_event (lead_id, stage, reason, at) VALUES ($1, $2, $3, $4)`,
			leadID, stage, reason, now)
		return err
	})
	if err != nil {
		return fmt.Errorf("leadstore: set lead stage %d: %w", leadID, notFound(err))
	}
	return nil
}

// ClearLeadStage takes lead leadID's stored stage away at now, so it reads as GetLeadCRM's
// effective stage again (talking after a reply or a connected call, else none): the stage,
// lost reason and tenant link go back to none and updated_at to now; the note, deal and snooze
// are kept. No lead_stage_event is written: that table takes only real stages (its check), so
// the timeline keeps the last stage set, while the funnel, which counts won by the stored
// stage, stops counting the lead. A lead with no stored stage is left as it is (clearing twice is clearing once); a
// missing lead is ErrLeadNotFound.
func (q Queries) ClearLeadStage(ctx context.Context, leadID int64, now time.Time) error {
	var exists bool
	err := q.db.QueryRow(ctx,
		`WITH cleared AS (
		   UPDATE lead_crm SET stage = NULL, lost_reason = '', tenant_url = NULL, updated_at = $2
		   WHERE lead_id = $1 AND stage IS NOT NULL RETURNING lead_id)
		 SELECT EXISTS (SELECT 1 FROM lead WHERE id = $1)`, leadID, now).Scan(&exists)
	if err != nil {
		return fmt.Errorf("leadstore: clear lead stage %d: %w", leadID, err)
	}
	if !exists {
		return fmt.Errorf("leadstore: clear lead stage %d: %w", leadID, ErrLeadNotFound)
	}
	return nil
}

// dateOnly is t's calendar date in its own location, as "YYYY-MM-DD"; nil for nil.
func dateOnly(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := t.Format(time.DateOnly)
	return &s
}

// SetLeadNote stores lead leadID's note (at most MaxCRMNote characters), next action (one line,
// at most MaxNextAction characters) and the day it is due (nextActionOn's calendar date in its
// own location; nil: none) at now, replacing all three; the stage, deal and snooze are kept.
// Both texts are trimmed. A past date is taken: the action is due at once. Bad input is
// ErrBadCRM; a missing lead is ErrLeadNotFound.
func (q Queries) SetLeadNote(ctx context.Context, leadID int64, note, nextAction string, nextActionOn *time.Time, now time.Time) error {
	note, nextAction = strings.TrimSpace(note), strings.TrimSpace(nextAction)
	if utf8.RuneCountInString(note) > MaxCRMNote {
		return fmt.Errorf("leadstore: set lead note %d: %w: the note is over %d characters", leadID, ErrBadCRM, MaxCRMNote)
	}
	if err := oneLine(nextAction, "next action", MaxNextAction); err != nil {
		return fmt.Errorf("leadstore: set lead note %d: %w", leadID, err)
	}
	if _, err := q.db.Exec(ctx,
		`INSERT INTO lead_crm (lead_id, note, next_action, next_action_on, updated_at) VALUES ($1, $2, $3, $4::date, $5)
		 ON CONFLICT (lead_id) DO UPDATE SET note = EXCLUDED.note, next_action = EXCLUDED.next_action,
		   next_action_on = EXCLUDED.next_action_on, updated_at = EXCLUDED.updated_at`,
		leadID, note, nextAction, dateOnly(nextActionOn), now); err != nil {
		return fmt.Errorf("leadstore: set lead note %d: %w", leadID, notFound(err))
	}
	return nil
}

// SetLeadDeal stores lead leadID's deal value in cents (nil: no value; never below 0) and its
// currency (one of Currencies; "" is DefaultCurrency) at now; everything else is kept. Bad
// input is ErrBadCRM; a missing lead is ErrLeadNotFound.
func (q Queries) SetLeadDeal(ctx context.Context, leadID int64, valueCents *int64, currency string, now time.Time) error {
	currency = strings.ToUpper(strings.TrimSpace(currency))
	if currency == "" {
		currency = DefaultCurrency
	}
	switch {
	case valueCents != nil && *valueCents < 0:
		return fmt.Errorf("leadstore: set lead deal %d: %w: the deal value cannot be below zero", leadID, ErrBadCRM)
	case !slices.Contains(Currencies, currency):
		return fmt.Errorf("leadstore: set lead deal %d: %w: unknown currency %q", leadID, ErrBadCRM, currency)
	}
	if _, err := q.db.Exec(ctx,
		`INSERT INTO lead_crm (lead_id, deal_value_cents, currency, updated_at) VALUES ($1, $2, $3, $4)
		 ON CONFLICT (lead_id) DO UPDATE SET deal_value_cents = EXCLUDED.deal_value_cents, currency = EXCLUDED.currency,
		   updated_at = EXCLUDED.updated_at`, leadID, valueCents, currency, now); err != nil {
		return fmt.Errorf("leadstore: set lead deal %d: %w", leadID, notFound(err))
	}
	return nil
}

// SnoozeLead hides lead leadID from the replies inbox and Needs me until until, which must be
// after now (else ErrBadCRM). A missing lead is ErrLeadNotFound.
func (q Queries) SnoozeLead(ctx context.Context, leadID int64, until, now time.Time) error {
	if !until.After(now) {
		return fmt.Errorf("leadstore: snooze lead %d: %w: the snooze must end after now", leadID, ErrBadCRM)
	}
	return q.setSnooze(ctx, leadID, &until, now)
}

// ClearSnooze shows lead leadID again at once. A lead never snoozed is left as it is; a
// missing lead is ErrLeadNotFound.
func (q Queries) ClearSnooze(ctx context.Context, leadID int64, now time.Time) error {
	return q.setSnooze(ctx, leadID, nil, now)
}

func (q Queries) setSnooze(ctx context.Context, leadID int64, until *time.Time, now time.Time) error {
	if _, err := q.db.Exec(ctx,
		`INSERT INTO lead_crm (lead_id, snoozed_until, updated_at) VALUES ($1, $2, $3)
		 ON CONFLICT (lead_id) DO UPDATE SET snoozed_until = EXCLUDED.snoozed_until, updated_at = EXCLUDED.updated_at`,
		leadID, until, now); err != nil {
		return fmt.Errorf("leadstore: snooze lead %d: %w", leadID, notFound(err))
	}
	return nil
}

// The kinds of a TimelineItem, one per source table.
const (
	TimelineEvent      = "event"       // lead_event: State and Detail (the reason)
	TimelineSent       = "sent"        // outreach_message sent: State, Step, Channel, Shadow
	TimelineMailbox    = "mailbox"     // mailbox_event: State (its kind), Detail (a reply's snippet)
	TimelineCall       = "call"        // call_outcome: State (the outcome), Detail (the note), CallBackOn
	TimelineEmailCheck = "email_check" // email_check: State (the status), Detail (the address checked)
	TimelineStage      = "stage"       // lead_stage_event: State (the stage), Detail (the lost reason)
)

// TimelineItem is one thing that happened to a lead.
type TimelineItem struct {
	Kind       string
	ID         int64 // the source row's id
	At         time.Time
	State      string
	Detail     string
	Step       int        // TimelineSent and TimelineCall: the 0-based step
	Channel    string     // TimelineSent: email | form | linkedin
	Shadow     bool       // TimelineSent and TimelineCall: from a shadow batch
	MessageID  int64      // TimelineSent and TimelineCall: the message; TimelineMailbox: the one it matched (0: none)
	CallBackOn *time.Time // TimelineCall: the call-back day; nil when none
}

// timelineLimitMax is the most items LeadTimeline returns.
const timelineLimitMax = 500

// LeadTimeline returns what happened to lead leadID, newest first (then by kind and id), at
// most limit items (1..500; out of range is clamped), in one query: every lead_event (state
// and reason); every message of it that went (state sent, maybe_sent or shadow_sent, calls
// left to their outcomes), by when it went, shadow ones marked; every mailbox_event matched to
// it (kind, and a reply's snippet); every call outcome on its calls (outcome, note, call-back
// day); every email check of its addresses (its email route's and the addresses its emails
// went to) in its campaign's account; and every stage change. A missing lead has none.
func (q Queries) LeadTimeline(ctx context.Context, leadID int64, limit int) ([]TimelineItem, error) {
	limit = min(max(limit, 1), timelineLimitMax)
	rows, err := q.db.Query(ctx,
		`SELECT * FROM (
		   SELECT 'event' AS kind, e.id, e.at, e.state, e.reason AS detail, 0 AS step, '' AS channel, false AS shadow,
		     0::bigint AS message_id, NULL::date AS call_back_on
		   FROM lead_event e WHERE e.lead_id = $1
		   UNION ALL
		   SELECT 'sent', m.id, m.sent_at, m.state, '', m.step, m.channel, b.shadow, m.id, NULL
		   FROM outreach_message m JOIN outreach_batch b ON b.id = m.batch_id
		   WHERE m.lead_id = $1 AND m.channel <> 'phone' AND m.state IN ('sent', 'maybe_sent', 'shadow_sent')
		     AND m.sent_at IS NOT NULL
		   UNION ALL
		   SELECT 'mailbox', ev.id, ev.seen_at, ev.kind, coalesce(ev.snippet, ''), 0, '', false,
		     coalesce(ev.matched_message_id, 0), NULL
		   FROM mailbox_event ev WHERE ev.lead_id = $1
		   UNION ALL
		   SELECT 'call', co.id, co.at, co.outcome, co.note, m.step, m.channel, b.shadow, m.id, co.call_back_on
		   FROM call_outcome co JOIN outreach_message m ON m.id = co.message_id JOIN outreach_batch b ON b.id = m.batch_id
		   WHERE m.lead_id = $1
		   UNION ALL
		   SELECT 'email_check', ec.id, ec.checked_at, ec.status, ec.address, 0, '', false, 0, NULL
		   FROM email_check ec
		   WHERE ec.account_id = (SELECT c.account_id FROM lead l JOIN campaign c ON c.id = l.campaign_id WHERE l.id = $1)
		     AND ec.address IN (
		       SELECT lower(r.value) FROM lead l JOIN contact_route r ON r.id = l.route_id WHERE l.id = $1 AND r.kind = 'email'
		       UNION SELECT lower(m.to_address) FROM outreach_message m
		       WHERE m.lead_id = $1 AND m.channel = 'email' AND m.to_address <> '')
		   UNION ALL
		   SELECT 'stage', s.id, s.at, s.stage, s.reason, 0, '', false, 0, NULL
		   FROM lead_stage_event s WHERE s.lead_id = $1
		 ) t
		 ORDER BY t.at DESC, t.kind, t.id DESC
		 LIMIT $2`, leadID, limit)
	if err != nil {
		return nil, fmt.Errorf("leadstore: lead timeline %d: %w", leadID, err)
	}
	out := []TimelineItem{}
	var it TimelineItem
	if _, err := pgx.ForEachRow(rows, []any{&it.Kind, &it.ID, &it.At, &it.State, &it.Detail, &it.Step, &it.Channel,
		&it.Shadow, &it.MessageID, &it.CallBackOn}, func() error {
		out = append(out, it)
		return nil
	}); err != nil {
		return nil, fmt.Errorf("leadstore: lead timeline %d: %w", leadID, err)
	}
	return out, nil
}

// ReplyRow is one lead in the replies inbox.
type ReplyRow struct {
	LeadID         int64
	CampaignID     int64
	Name           string // as NeedsMe names it
	Domain         string
	LeadState      string
	EffectiveStage string // as GetLeadCRM reads it
	Snippet        string // the latest reply's snippet; "" when none was kept
	RepliedAt      time.Time
	Replies        int // matched replies in all
}

// repliesLimitMax is the most rows RepliesInbox returns.
const repliesLimitMax = 200

// RepliesInbox returns the leads with a reply the mailbox matched to them (a mailbox_event of
// kind reply with a lead), latest reply first (then by lead id), at most limit (1..200; out
// of range is clamped), and how many there are in all: each with its latest reply's snippet
// and time, how many replies it sent and its effective stage. A lead snoozed past now is left
// out, and so is every selftest lead.
func (q Queries) RepliesInbox(ctx context.Context, now time.Time, limit int) ([]ReplyRow, int, error) {
	limit = min(max(limit, 1), repliesLimitMax)
	rows, err := q.db.Query(ctx,
		`WITH r AS (
		   SELECT DISTINCT ON (ev.lead_id) ev.lead_id, coalesce(ev.snippet, '') AS snippet, ev.seen_at,
		     count(*) OVER (PARTITION BY ev.lead_id) AS n
		   FROM mailbox_event ev WHERE ev.kind = 'reply' AND ev.lead_id IS NOT NULL
		   ORDER BY ev.lead_id, ev.seen_at DESC, ev.id DESC
		 )
		 SELECT l.id, l.campaign_id, `+leadNameSQL+`, o.domain, l.state, `+effectiveStageSQL+`, r.snippet, r.seen_at, r.n,
		   count(*) OVER ()
		 FROM r JOIN lead l ON l.id = r.lead_id JOIN org o ON o.id = l.org_id
		 LEFT JOIN site_probe p ON p.id = l.probe_id
		 LEFT JOIN lead_crm k ON k.lead_id = l.id
		 WHERE l.kind <> 'selftest' AND (k.snoozed_until IS NULL OR k.snoozed_until <= $1)
		 ORDER BY r.seen_at DESC, l.id DESC
		 LIMIT $2`, now, limit)
	if err != nil {
		return nil, 0, fmt.Errorf("leadstore: replies inbox: %w", err)
	}
	out := []ReplyRow{}
	total := 0
	var r ReplyRow
	if _, err := pgx.ForEachRow(rows, []any{&r.LeadID, &r.CampaignID, &r.Name, &r.Domain, &r.LeadState, &r.EffectiveStage,
		&r.Snippet, &r.RepliedAt, &r.Replies, &total}, func() error {
		out = append(out, r)
		return nil
	}); err != nil {
		return nil, 0, fmt.Errorf("leadstore: replies inbox: %w", err)
	}
	return out, total, nil
}

// PipelineRow is one lead on the pipeline board.
type PipelineRow struct {
	LeadID         int64
	CampaignID     int64
	Name           string // as NeedsMe names it
	Domain         string
	Stage          string // the effective stage: one of Stages
	DealValueCents *int64 // nil when no value is set
	Currency       string
	NextAction     string
	NextActionOn   *time.Time
	LostReason     string
	TenantURL      string
	UpdatedAt      *time.Time // the lead_crm row's; nil for a lead talking without a row
}

// Pipeline returns every lead in an effective stage (a stored stage, or talking because it
// replied or is contacted with a connected call), selftests left out, in board order (talking,
// proposal, won, lost), then most recently updated first (a lead with no row last), then by
// lead id.
func (q Queries) Pipeline(ctx context.Context) ([]PipelineRow, error) {
	rows, err := q.db.Query(ctx,
		`WITH cand AS (
		   SELECT lead_id AS id FROM lead_crm WHERE stage IS NOT NULL
		   UNION SELECT id FROM lead WHERE state = 'replied'
		   UNION SELECT m.lead_id FROM call_outcome co JOIN outreach_message m ON m.id = co.message_id
		     JOIN outreach_batch b ON b.id = m.batch_id WHERE co.outcome = 'connected' AND NOT b.shadow
		 ), staged AS (
		   SELECT l.id, `+effectiveStageSQL+` AS stage FROM cand c JOIN lead l ON l.id = c.id
		   LEFT JOIN lead_crm k ON k.lead_id = l.id
		   WHERE l.kind <> 'selftest'
		 )
		 SELECT l.id, l.campaign_id, `+leadNameSQL+`, o.domain, s.stage, k.deal_value_cents, coalesce(k.currency, $1),
		   coalesce(k.next_action, ''), k.next_action_on, coalesce(k.lost_reason, ''), coalesce(k.tenant_url, ''), k.updated_at
		 FROM staged s JOIN lead l ON l.id = s.id JOIN org o ON o.id = l.org_id
		 LEFT JOIN site_probe p ON p.id = l.probe_id
		 LEFT JOIN lead_crm k ON k.lead_id = l.id
		 WHERE s.stage <> ''
		 ORDER BY array_position(ARRAY['talking', 'proposal', 'won', 'lost'], s.stage), k.updated_at DESC NULLS LAST, l.id`,
		DefaultCurrency)
	if err != nil {
		return nil, fmt.Errorf("leadstore: pipeline: %w", err)
	}
	out := []PipelineRow{}
	var r PipelineRow
	if _, err := pgx.ForEachRow(rows, []any{&r.LeadID, &r.CampaignID, &r.Name, &r.Domain, &r.Stage, &r.DealValueCents,
		&r.Currency, &r.NextAction, &r.NextActionOn, &r.LostReason, &r.TenantURL, &r.UpdatedAt}, func() error {
		out = append(out, r)
		return nil
	}); err != nil {
		return nil, fmt.Errorf("leadstore: pipeline: %w", err)
	}
	return out, nil
}

// What a CallQueueRow is in the queue for.
const (
	CallQueueDue  = "due"       // a call step due that day (or overdue) with no outcome yet
	CallQueueBack = "call_back" // a call-back the operator set, due that day or earlier
)

// CallQueueRow is one call in the day's queue: the call step as the call card reads it
// (CallRow: the org, the number, the card, the latest outcome), its lead's name, why it is
// queued and when it is due.
type CallQueueRow struct {
	CallRow
	Name       string     `db:"name"` // as NeedsMe names the lead
	Kind       string     `db:"kind"` // CallQueueDue or CallQueueBack
	CallBackOn *time.Time `db:"call_back_on"`
	// Due is the call step's due time, or for a call-back the start of its day in the
	// queue's zone.
	Due time.Time `db:"-"`
}

// CallQueue returns day's calls (day's calendar date in loc; nil: time.Local), every account,
// soonest due first (then by message id):
//
//   - due: the approved call steps of approved, live, not discarded batches due before the
//     end of that day (overdue ones too) with no outcome yet;
//   - call_back: the leads whose latest call outcome on a live batch carries a call-back day of
//     that day or earlier, listed on the call that outcome was recorded on (MessageID is the
//     message the outcome route takes). A lead that has since opted out, bounced, been
//     suppressed or closed is left out, and so is one already queued with a due call.
//
// CallRow.Phone is read as the call page reads it (the phone fact the step dials).
func (q Queries) CallQueue(ctx context.Context, day time.Time, loc *time.Location) ([]CallQueueRow, error) {
	if loc == nil {
		loc = time.Local
	}
	d := day.In(loc)
	dayStart := time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, loc)
	dayEnd := dayStart.AddDate(0, 0, 1)
	wrap := func(inner, kind string) string {
		return `SELECT x.*, coalesce(nullif(p.site_name, ''), nullif(x.org_name, ''), x.org_domain) AS name,
		   '` + kind + `' AS kind, lc.call_back_on
		 FROM (` + inner + `) x JOIN lead l2 ON l2.id = x.lead_id
		 LEFT JOIN site_probe p ON p.id = l2.probe_id
		 LEFT JOIN lastco lc ON lc.message_id = x.message_id AND lc.lead_id = x.lead_id`
	}
	const lastco = `WITH lastco AS (
		   SELECT DISTINCT ON (m.lead_id) m.lead_id, co.message_id, co.call_back_on FROM call_outcome co
		   JOIN outreach_message m ON m.id = co.message_id JOIN outreach_batch b ON b.id = m.batch_id
		   WHERE NOT b.shadow ORDER BY m.lead_id, co.id DESC
		 ) `
	due, err := collect(ctx, q.db, pgx.RowToStructByName[CallQueueRow], lastco+wrap(callRowSQL+
		` AND m.state = 'approved' AND b.approved_at IS NOT NULL AND NOT b.shadow AND b.discarded_at IS NULL
		  AND m.due_at IS NOT NULL AND m.due_at < $1
		  AND NOT EXISTS (SELECT 1 FROM call_outcome c WHERE c.message_id = m.id)`, CallQueueDue), dayEnd)
	if err != nil {
		return nil, fmt.Errorf("leadstore: call queue: %w", err)
	}
	backs, err := collect(ctx, q.db, pgx.RowToStructByName[CallQueueRow], lastco+wrap(callRowSQL+
		` AND m.id IN (SELECT message_id FROM lastco WHERE call_back_on <= $1::date)
		  AND l.state NOT IN ('opted_out', 'bounced', 'suppressed', 'closed')`, CallQueueBack), d.Format(time.DateOnly))
	if err != nil {
		return nil, fmt.Errorf("leadstore: call queue: %w", err)
	}
	queued := map[int64]bool{}
	out := make([]CallQueueRow, 0, len(due)+len(backs))
	for _, r := range due {
		if r.DueAt != nil {
			r.Due = *r.DueAt
		}
		queued[r.LeadID] = true
		out = append(out, r)
	}
	for _, r := range backs {
		if queued[r.LeadID] || r.CallBackOn == nil {
			continue
		}
		y, m, dd := r.CallBackOn.Date()
		r.Due = time.Date(y, m, dd, 0, 0, 0, 0, loc)
		out = append(out, r)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].Due.Equal(out[j].Due) {
			return out[i].Due.Before(out[j].Due)
		}
		return out[i].MessageID < out[j].MessageID
	})
	return out, nil
}

// LeadCampaigns returns the campaign of each of leadIDs that exists, by lead id (empty, never
// nil, for none). The CRM's cost per lead joins the Claude spend per lead (store, the core
// schema) to campaigns through it, so no query crosses the two migration sets.
func (q Queries) LeadCampaigns(ctx context.Context, leadIDs []int64) (map[int64]int64, error) {
	out := map[int64]int64{}
	if len(leadIDs) == 0 {
		return out, nil
	}
	rows, err := q.db.Query(ctx, `SELECT id, campaign_id FROM lead WHERE id = ANY($1)`, leadIDs)
	if err != nil {
		return nil, fmt.Errorf("leadstore: lead campaigns: %w", err)
	}
	var id, c int64
	if _, err := pgx.ForEachRow(rows, []any{&id, &c}, func() error {
		out[id] = c
		return nil
	}); err != nil {
		return nil, fmt.Errorf("leadstore: lead campaigns: %w", err)
	}
	return out, nil
}

// EmailChecksByCampaign counts the email checks run at or after since, by campaign id (empty,
// never nil, for none). email_check names no lead, so a check is joined to the leads whose
// addresses it checked as LeadTimeline joins it for one lead: an address of the lead's email
// route or one its emails went to, lower-cased, in its campaign's account. Each check counts
// once, against the campaign of the lowest-id lead it matched; a check matching no lead, or
// only selftest leads (the funnel leaves those out too), is not counted.
func (q Queries) EmailChecksByCampaign(ctx context.Context, since time.Time) (map[int64]int, error) {
	rows, err := q.db.Query(ctx,
		`WITH addr AS (
		   SELECT l.id AS lead_id, l.campaign_id, c.account_id, lower(r.value) AS address
		   FROM lead l JOIN campaign c ON c.id = l.campaign_id JOIN contact_route r ON r.id = l.route_id
		   WHERE r.kind = 'email' AND l.kind <> 'selftest'
		   UNION
		   SELECT l.id, l.campaign_id, c.account_id, lower(m.to_address)
		   FROM outreach_message m JOIN lead l ON l.id = m.lead_id JOIN campaign c ON c.id = l.campaign_id
		   WHERE m.channel = 'email' AND m.to_address <> '' AND l.kind <> 'selftest'
		 )
		 SELECT x.campaign_id, count(*) FROM (
		   SELECT DISTINCT ON (ec.id) a.campaign_id
		   FROM email_check ec JOIN addr a ON a.account_id = ec.account_id AND a.address = ec.address
		   WHERE ec.checked_at >= $1
		   ORDER BY ec.id, a.lead_id
		 ) x GROUP BY x.campaign_id`, since)
	if err != nil {
		return nil, fmt.Errorf("leadstore: email checks by campaign: %w", err)
	}
	out := map[int64]int{}
	var c int64
	var n int
	if _, err := pgx.ForEachRow(rows, []any{&c, &n}, func() error {
		out[c] = n
		return nil
	}); err != nil {
		return nil, fmt.Errorf("leadstore: email checks by campaign: %w", err)
	}
	return out, nil
}
