//go:build integration

package leadstore_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/campaign"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/leadstore/leadstoretest"
	"github.com/gremy/webbership-leadhound/internal/mailbox"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

// Migration 00028: lead_crm, lead_stage_event, mailbox_event.snippet, call_outcome.call_back_on.

const crmVersion = 28 // 00028_crm.sql

// crmNow is the fixed "now" of these tests.
var crmNow = time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)

func int64p(n int64) *int64 { return &n }

// selftest makes lead a selftest lead.
func (f outreachFixture) selftest(t *testing.T, lead int64) {
	t.Helper()
	f.ownerRun(t, `UPDATE lead SET kind = 'selftest', probe_id = NULL WHERE id = $1`, lead)
}

// replied puts lead in state replied at at.
func (f outreachFixture) replied(t *testing.T, lead int64, at time.Time) {
	t.Helper()
	f.ownerRun(t, `UPDATE lead SET state = 'replied', state_at = $2 WHERE id = $1`, lead, at)
}

// replyEvent records a matched reply to lead (its message msg, or none) seen at at with
// snippet, as the owner.
func (f outreachFixture) replyEvent(t *testing.T, lead int64, uid int, snippet *string, at time.Time) {
	t.Helper()
	f.ownerRun(t, `INSERT INTO mailbox_event (mailbox, uidvalidity, uid, kind, from_domain, lead_id, seen_at, mailbox_id, snippet)
		VALUES ('INBOX', 1, $1, 'reply', 'reply.localhost', $2, $3, $4, $5)`, uid, lead, at, f.mailbox(t), snippet)
}

// TestMigrationCRMDownUp: the checks exist; the app may insert and update lead_crm but never
// delete it, may only append to lead_stage_event; down drops both tables and both columns, up
// puts them back.
func TestMigrationCRMDownUp(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	owner := f.owner
	for _, c := range []string{"lead_crm_stage_check", "lead_crm_lost_reason_check", "lead_crm_note_check",
		"lead_crm_next_action_check", "lead_crm_deal_value_check", "lead_crm_currency_check", "lead_crm_tenant_url_check",
		"lead_stage_event_stage_check", "mailbox_event_snippet_check"} {
		if n := ownerInt(t, owner, `SELECT count(*) FROM pg_constraint WHERE conname = $1`, c); n != 1 {
			t.Errorf("constraint %s: %d, want 1", c, n)
		}
	}
	if !appHas(t, owner, "lead_crm", "INSERT") || !appHas(t, owner, "lead_crm", "UPDATE") {
		t.Error("lead_crm: the app wants INSERT and UPDATE (every column a pointer)")
	}
	if appHas(t, owner, "lead_stage_event", "UPDATE") || !appHas(t, owner, "lead_stage_event", "INSERT") {
		t.Error("lead_stage_event: want INSERT and no UPDATE for the app")
	}
	for _, table := range []string{"lead_crm", "lead_stage_event"} {
		if appHas(t, owner, table, "DELETE") || appHas(t, owner, table, "TRUNCATE") {
			t.Errorf("%s: the app may DELETE or TRUNCATE", table)
		}
	}
	lead := f.siteLead(t, "crm-migrate.localhost", nil, 7, "info@crm-migrate.localhost")
	if err := f.q.SetLeadStage(ctx, lead, leadstore.StageTalking, "", "", crmNow); err != nil {
		t.Fatal(err)
	}
	app := storetest.AppConn(t)
	if _, err := app.Exec(ctx, `UPDATE lead_stage_event SET stage = 'won'`); sqlState(err) != "42501" {
		t.Errorf("app rewrites a stage change: %v, want 42501", err)
	}
	if _, err := app.Exec(ctx, `DELETE FROM lead_crm`); sqlState(err) != "42501" {
		t.Errorf("app deletes a CRM row: %v, want 42501", err)
	}

	p := leadsProvider(t)
	t.Cleanup(func() { migrateAll(t) })
	if _, err := p.DownTo(ctx, crmVersion-1); err != nil {
		t.Fatalf("down to %d: %v", crmVersion-1, err)
	}
	for _, table := range []string{"lead_crm", "lead_stage_event"} {
		var reg *string
		if err := owner.QueryRow(ctx, `SELECT to_regclass('public.' || $1)::text`, table).Scan(&reg); err != nil || reg != nil {
			t.Errorf("%s after down: %v %v, want gone", table, reg, err)
		}
	}
	for _, col := range [][2]string{{"mailbox_event", "snippet"}, {"call_outcome", "call_back_on"}} {
		if n := ownerInt(t, owner, `SELECT count(*) FROM information_schema.columns WHERE table_schema = 'public'
			AND table_name = $1 AND column_name = $2`, col[0], col[1]); n != 0 {
			t.Errorf("%s.%s left after down", col[0], col[1])
		}
	}
	if _, err := p.UpTo(ctx, crmVersion); err != nil {
		t.Fatalf("up again: %v", err)
	}
	if c, err := f.q.GetLeadCRM(ctx, lead); err != nil || c.Stage != "" || c.UpdatedAt != nil {
		t.Errorf("after down and up: %+v %v, want no row", c, err)
	}
}

// TestCRMChecks: every lead_crm, lead_stage_event and snippet check refuses a bad row written
// straight to the table, and takes the edge that is allowed.
func TestCRMChecks(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	lead := f.siteLead(t, "crm-checks.localhost", nil, 7, "info@crm-checks.localhost")
	tx, err := f.owner.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	try := func(sql string, args ...any) error {
		t.Helper()
		if _, err := tx.Exec(ctx, "SAVEPOINT s"); err != nil {
			t.Fatal(err)
		}
		_, err := tx.Exec(ctx, sql, args...)
		if _, rerr := tx.Exec(ctx, "ROLLBACK TO SAVEPOINT s"); rerr != nil {
			t.Fatal(rerr)
		}
		return err
	}
	ins := `INSERT INTO lead_crm (lead_id, stage, lost_reason, note, next_action, deal_value_cents, currency, tenant_url)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`
	type row struct {
		stage, lost, note, next string
		deal                    *int64
		currency                string
		tenant                  *string
	}
	str := func(s string) *string { return &s }
	ok := row{currency: "GBP"}
	for _, c := range []struct {
		name string
		r    row
		good bool
	}{
		{"no stage", ok, true},
		{"unknown stage", row{stage: "open", currency: "GBP"}, false},
		{"lost with a reason", row{stage: "lost", lost: "went with another agency", currency: "GBP"}, true},
		{"lost without a reason", row{stage: "lost", currency: "GBP"}, false},
		{"lost with a blank reason", row{stage: "lost", lost: "   ", currency: "GBP"}, false},
		{"lost reason 200", row{stage: "lost", lost: strings.Repeat("r", 200), currency: "GBP"}, true},
		{"lost reason 201", row{stage: "lost", lost: strings.Repeat("r", 201), currency: "GBP"}, false},
		{"lost reason two lines", row{stage: "lost", lost: "one\ntwo", currency: "GBP"}, false},
		{"note 2000", row{note: strings.Repeat("n", 2000), currency: "GBP"}, true},
		{"note 2001", row{note: strings.Repeat("n", 2001), currency: "GBP"}, false},
		{"next action 200", row{next: strings.Repeat("a", 200), currency: "GBP"}, true},
		{"next action 201", row{next: strings.Repeat("a", 201), currency: "GBP"}, false},
		{"deal 0", row{deal: int64p(0), currency: "GBP"}, true},
		{"deal below 0", row{deal: int64p(-1), currency: "GBP"}, false},
		{"currency RON", row{currency: "RON"}, true},
		{"unknown currency", row{currency: "JPY"}, false},
		{"tenant on won", row{stage: "won", currency: "GBP", tenant: str("https://acme.example.localhost/")}, true},
		{"tenant on talking", row{stage: "talking", currency: "GBP", tenant: str("https://acme.example.localhost/")}, false},
		{"tenant with no stage", row{currency: "GBP", tenant: str("https://acme.example.localhost/")}, false},
		{"tenant over http", row{stage: "won", currency: "GBP", tenant: str("http://acme.example.localhost/")}, false},
		{"tenant with no host", row{stage: "won", currency: "GBP", tenant: str("https://")}, false},
	} {
		err := try(ins, lead, nullIfEmpty(c.r.stage), c.r.lost, c.r.note, c.r.next, c.r.deal, c.r.currency, c.r.tenant)
		switch {
		case c.good && err != nil:
			t.Errorf("%s: %v, want taken", c.name, err)
		case !c.good && sqlState(err) != "23514":
			t.Errorf("%s: %v, want a check violation", c.name, err)
		}
	}
	if err := try(`INSERT INTO lead_stage_event (lead_id, stage) VALUES ($1, 'open')`, lead); sqlState(err) != "23514" {
		t.Errorf("stage event with an unknown stage: %v", err)
	}
	ev := `INSERT INTO mailbox_event (mailbox, uidvalidity, uid, kind, lead_id, mailbox_id, snippet) VALUES ('INBOX', 1, 1, 'reply', $1, $2, $3)`
	if err := try(ev, lead, f.mailbox(t), strings.Repeat("é", 300)); err != nil {
		t.Errorf("snippet of 300 characters: %v", err)
	}
	if err := try(ev, lead, f.mailbox(t), strings.Repeat("é", 301)); sqlState(err) != "23514" {
		t.Errorf("snippet of 301 characters: %v", err)
	}
}

func nullIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// TestLeadCRMWrites: a cold start has no row (effective stage from the replied state or a
// connected call); each write refuses bad input with ErrBadCRM and writes nothing; stage
// changes append events (not a repeat of the same stage); leaving won clears the tenant link,
// leaving lost the reason; note, deal and snooze keep each other.
func TestLeadCRMWrites(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	q := f.q
	quiet := f.siteLead(t, "crm-quiet.localhost", nil, 7, "info@crm-quiet.localhost")
	talk := f.siteLead(t, "crm-talk.localhost", nil, 7, "info@crm-talk.localhost")
	called := f.siteLead(t, "crm-called.localhost", nil, 7, "info@crm-called.localhost")

	c, err := q.GetLeadCRM(ctx, quiet)
	if err != nil || c.Stage != "" || c.EffectiveStage != "" || c.UpdatedAt != nil || c.Currency != leadstore.DefaultCurrency {
		t.Fatalf("cold start: %+v %v", c, err)
	}
	f.replied(t, talk, crmNow)
	if c, err := q.GetLeadCRM(ctx, talk); err != nil || c.Stage != "" || c.EffectiveStage != leadstore.StageTalking || c.UpdatedAt != nil {
		t.Errorf("replied with no row: %+v %v, want talking without a stored stage", c, err)
	}
	// called: contacted with a connected call on a live batch.
	b, _ := f.batch(t, false, called)
	f.ownerRun(t, `UPDATE lead SET state = 'contacted' WHERE id = $1`, called)
	var call int64
	if err := f.owner.QueryRow(ctx, `INSERT INTO outreach_message (batch_id, lead_id, step, channel, body_text, slots, state, mailbox_id)
		VALUES ($1, $2, 2, 'phone', '', '{}', 'done', $3) RETURNING id`, b, called, f.mailbox(t)).Scan(&call); err != nil {
		t.Fatal(err)
	}
	f.ownerRun(t, `INSERT INTO call_outcome (message_id, outcome, actor, at) VALUES ($1, 'connected', 'operator', $2)`, call, crmNow)
	if c, err := q.GetLeadCRM(ctx, called); err != nil || c.EffectiveStage != leadstore.StageTalking {
		t.Errorf("connected call: %+v %v, want talking", c, err)
	}
	if _, err := q.GetLeadCRM(ctx, 999999); !errors.Is(err, leadstore.ErrLeadNotFound) {
		t.Errorf("missing lead: %v", err)
	}

	for _, c := range []struct {
		name                  string
		stage, reason, tenant string
		want                  string
	}{
		{"unknown stage", "open", "", "", "unknown stage"},
		{"lost without a reason", leadstore.StageLost, "  ", "", "a lost lead needs a reason"},
		{"lost reason two lines", leadstore.StageLost, "one\ntwo", "", "one line"},
		{"lost reason too long", leadstore.StageLost, strings.Repeat("r", leadstore.MaxLostReason+1), "", "over 200"},
		{"tenant on talking", leadstore.StageTalking, "", "https://acme.localhost/", "only with a won lead"},
		{"tenant over http", leadstore.StageWon, "", "http://acme.localhost/", "https://"},
		{"tenant with a space", leadstore.StageWon, "", "https://acme.localhost/a b", "https://"},
	} {
		err := q.SetLeadStage(ctx, quiet, c.stage, c.reason, c.tenant, crmNow)
		if !errors.Is(err, leadstore.ErrBadCRM) || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want ErrBadCRM saying %q", c.name, err, c.want)
		}
	}
	if c, _ := q.GetLeadCRM(ctx, quiet); c.UpdatedAt != nil {
		t.Errorf("a refused write wrote a row: %+v", c)
	}
	if err := q.SetLeadStage(ctx, 999999, leadstore.StageTalking, "", "", crmNow); !errors.Is(err, leadstore.ErrLeadNotFound) {
		t.Errorf("stage on a missing lead: %v", err)
	}

	events := func() int64 {
		return ownerInt(t, f.owner, `SELECT count(*) FROM lead_stage_event WHERE lead_id = $1`, talk)
	}
	steps := []struct {
		stage, reason, tenant string
		at                    time.Time
		wantTenant, wantLost  string
		wantEvents            int64
	}{
		{leadstore.StageProposal, "ignored", "", crmNow, "", "", 1},
		{leadstore.StageProposal, "", "", crmNow.Add(time.Minute), "", "", 1},
		{leadstore.StageWon, "", "https://acme.localhost/", crmNow.Add(2 * time.Minute), "https://acme.localhost/", "", 2},
		{leadstore.StageWon, "", "", crmNow.Add(3 * time.Minute), "https://acme.localhost/", "", 2},
		{leadstore.StageLost, "budget went elsewhere", "", crmNow.Add(4 * time.Minute), "", "budget went elsewhere", 3},
		{leadstore.StageTalking, "", "", crmNow.Add(5 * time.Minute), "", "", 4},
	}
	for i, s := range steps {
		if err := q.SetLeadStage(ctx, talk, s.stage, s.reason, s.tenant, s.at); err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
		c, err := q.GetLeadCRM(ctx, talk)
		if err != nil || c.Stage != s.stage || c.EffectiveStage != s.stage || c.TenantURL != s.wantTenant ||
			c.LostReason != s.wantLost || c.UpdatedAt == nil || !c.UpdatedAt.Equal(s.at) {
			t.Errorf("step %d (%s): %+v %v", i, s.stage, c, err)
		}
		if n := events(); n != s.wantEvents {
			t.Errorf("step %d: %d stage events, want %d", i, n, s.wantEvents)
		}
	}
	if n := ownerInt(t, f.owner, `SELECT count(*) FROM lead_stage_event WHERE lead_id = $1 AND stage = 'lost'
		AND reason = 'budget went elsewhere'`, talk); n != 1 {
		t.Errorf("the lost event keeps its reason: %d", n)
	}

	// Note and next action.
	due := time.Date(2026, 11, 12, 18, 0, 0, 0, time.UTC)
	if err := q.SetLeadNote(ctx, talk, "  Wants a quote for the D7 site.\nCall after the board meets.  ", " Send the quote ", &due, crmNow); err != nil {
		t.Fatal(err)
	}
	c, _ = q.GetLeadCRM(ctx, talk)
	if c.Note != "Wants a quote for the D7 site.\nCall after the board meets." || c.NextAction != "Send the quote" ||
		c.NextActionOn == nil || c.NextActionOn.Format(time.DateOnly) != "2026-11-12" || c.Stage != leadstore.StageTalking {
		t.Errorf("note: %+v", c)
	}
	for _, bad := range []struct {
		note, next, want string
	}{
		{strings.Repeat("n", leadstore.MaxCRMNote+1), "", "note is over"},
		{"", strings.Repeat("a", leadstore.MaxNextAction+1), "next action is over"},
		{"", "one\ntwo", "one line"},
	} {
		if err := q.SetLeadNote(ctx, talk, bad.note, bad.next, nil, crmNow); !errors.Is(err, leadstore.ErrBadCRM) || !strings.Contains(err.Error(), bad.want) {
			t.Errorf("note %q / %q: %v", bad.note[:min(len(bad.note), 10)], bad.next[:min(len(bad.next), 10)], err)
		}
	}
	if err := q.SetLeadNote(ctx, 999999, "", "", nil, crmNow); !errors.Is(err, leadstore.ErrLeadNotFound) {
		t.Errorf("note on a missing lead: %v", err)
	}

	// Deal.
	if err := q.SetLeadDeal(ctx, talk, int64p(-1), "GBP", crmNow); !errors.Is(err, leadstore.ErrBadCRM) {
		t.Errorf("negative deal: %v", err)
	}
	if err := q.SetLeadDeal(ctx, talk, int64p(100), "JPY", crmNow); !errors.Is(err, leadstore.ErrBadCRM) {
		t.Errorf("unknown currency: %v", err)
	}
	if err := q.SetLeadDeal(ctx, talk, int64p(450000), "eur", crmNow); err != nil {
		t.Fatal(err)
	}
	c, _ = q.GetLeadCRM(ctx, talk)
	if c.DealValueCents == nil || *c.DealValueCents != 450000 || c.Currency != "EUR" || c.NextAction != "Send the quote" {
		t.Errorf("deal: %+v", c)
	}
	if err := q.SetLeadDeal(ctx, talk, nil, "", crmNow); err != nil {
		t.Fatal(err)
	}
	if c, _ = q.GetLeadCRM(ctx, talk); c.DealValueCents != nil || c.Currency != leadstore.DefaultCurrency {
		t.Errorf("deal cleared: %+v", c)
	}

	// Snooze.
	if err := q.SnoozeLead(ctx, talk, crmNow, crmNow); !errors.Is(err, leadstore.ErrBadCRM) {
		t.Errorf("snooze to now: %v", err)
	}
	if err := q.SnoozeLead(ctx, talk, crmNow.Add(24*time.Hour), crmNow); err != nil {
		t.Fatal(err)
	}
	if c, _ = q.GetLeadCRM(ctx, talk); c.SnoozedUntil == nil || !c.SnoozedUntil.Equal(crmNow.Add(24*time.Hour)) || c.Stage != leadstore.StageTalking {
		t.Errorf("snoozed: %+v", c)
	}
	if err := q.ClearSnooze(ctx, talk, crmNow); err != nil {
		t.Fatal(err)
	}
	if c, _ = q.GetLeadCRM(ctx, talk); c.SnoozedUntil != nil {
		t.Errorf("snooze cleared: %+v", c)
	}
	if err := q.SnoozeLead(ctx, 999999, crmNow.Add(time.Hour), crmNow); !errors.Is(err, leadstore.ErrLeadNotFound) {
		t.Errorf("snooze a missing lead: %v", err)
	}
}

// TestApplyIncomingSnippet: a matched reply keeps its snippet, trimmed and cut to 300
// characters; an auto-reply, an unmatched reply and a blank snippet keep none.
func TestApplyIncomingSnippet(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	mb := leadstoretest.SeedMailbox(t, f.q, "out@snippet.localhost")
	_, ref := f.sentLead(t, "snippet.localhost", "info@snippet.localhost")
	long := "  " + strings.Repeat("ă", 310) + "  "
	for _, c := range []struct {
		uid     uint32
		kind    mailbox.MessageKind
		ref     *leadstore.SentRef
		snippet string
		want    *string
	}{
		{1, mailbox.KindReply, &ref, long, func() *string { s := strings.Repeat("ă", 300); return &s }()},
		{2, mailbox.KindReply, &ref, " Yes, call me on Monday. ", func() *string { s := "Yes, call me on Monday."; return &s }()},
		{3, mailbox.KindAutoReply, &ref, "Out of office", nil},
		{4, mailbox.KindReply, nil, "Who is this?", nil},
		{5, mailbox.KindReply, &ref, "   ", nil},
	} {
		if _, err := f.q.ApplyIncomingFor(ctx, mb.ID, leadstore.Incoming{Mailbox: "INBOX", UIDValidity: 1, UID: c.uid, Kind: c.kind,
			FromDomain: "snippet.localhost", Matched: c.ref, At: crmNow, Snippet: c.snippet}); err != nil {
			t.Fatalf("uid %d: %v", c.uid, err)
		}
		var got *string
		if err := f.owner.QueryRow(ctx, `SELECT snippet FROM mailbox_event WHERE mailbox_id = $1 AND uid = $2`, mb.ID, c.uid).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if (got == nil) != (c.want == nil) || (got != nil && *got != *c.want) {
			t.Errorf("uid %d: snippet %v, want %v", c.uid, got, c.want)
		}
	}
}

// TestLeadTimeline: every source shows, one kind each, newest first; shadow sends are marked;
// a call carries its call-back day; another lead's rows never show; the limit holds.
func TestLeadTimeline(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	q := f.q
	lead := f.siteLead(t, "timeline.localhost", nil, 7, "info@timeline.localhost")
	other := f.siteLead(t, "timeline-other.localhost", nil, 7, "info@timeline-other.localhost")
	if items, err := q.LeadTimeline(ctx, 999999, 50); err != nil || items == nil || len(items) != 0 {
		t.Fatalf("a missing lead: %+v %v", items, err)
	}
	// The fixture's own writes (the lead created, batched) are dated by the clock, before t0;
	// only what this test dates from t0 on is compared.
	t0 := crmNow.Add(-10 * time.Hour)
	at := func(h int) time.Time { return t0.Add(time.Duration(h) * time.Hour) }

	f.ownerRun(t, `INSERT INTO lead_event (lead_id, state, reason, actor, at) VALUES ($1, 'batched', 'batch 1', 'system', $2)`, lead, at(0))
	shadow, _ := f.batch(t, true, lead) // a shadow batch first: it leaves the lead new
	live, _ := f.batch(t, false, lead, other)
	f.sent(t, f.msgID(t, live, lead, 0), "sent", at(1))
	f.sent(t, f.msgID(t, shadow, lead, 0), "shadow_sent", at(2))
	f.sent(t, f.msgID(t, live, other, 0), "sent", at(2))
	snip := "Sounds good, send details."
	f.replyEvent(t, lead, 1, &snip, at(3))
	var call int64
	if err := f.owner.QueryRow(ctx, `INSERT INTO outreach_message (batch_id, lead_id, step, channel, body_text, slots, state, mailbox_id)
		VALUES ($1, $2, 2, 'phone', '', '{}', 'done', $3) RETURNING id`, live, lead, f.mailbox(t)).Scan(&call); err != nil {
		t.Fatal(err)
	}
	f.ownerRun(t, `INSERT INTO call_outcome (message_id, outcome, note, actor, at, call_back_on) VALUES ($1, 'no_answer', 'rang', 'operator', $2, '2026-11-12')`, call, at(4))
	if _, err := q.AddEmailCheck(ctx, leadstore.EmailCheck{Address: "INFO@timeline.localhost", Provider: "reoon", Status: "safe", CheckedAt: at(5)}); err != nil {
		t.Fatal(err)
	}
	if _, err := q.AddEmailCheck(ctx, leadstore.EmailCheck{Address: "info@timeline-other.localhost", Provider: "reoon", Status: "invalid", CheckedAt: at(5)}); err != nil {
		t.Fatal(err)
	}
	if err := q.SetLeadStage(ctx, lead, leadstore.StageLost, "no budget", "", at(6)); err != nil {
		t.Fatal(err)
	}

	all, err := q.LeadTimeline(ctx, lead, 50)
	if err != nil {
		t.Fatal(err)
	}
	var items []leadstore.TimelineItem
	for _, it := range all {
		if !it.At.Before(t0) {
			items = append(items, it)
		} else if it.Kind != leadstore.TimelineEvent {
			t.Errorf("an early item that is not the fixture's lead event: %+v", it)
		}
	}
	var kinds []string
	for _, it := range items {
		kinds = append(kinds, it.Kind)
	}
	want := []string{leadstore.TimelineStage, leadstore.TimelineEmailCheck, leadstore.TimelineCall, leadstore.TimelineMailbox,
		leadstore.TimelineSent, leadstore.TimelineSent, leadstore.TimelineEvent}
	if strings.Join(kinds, ",") != strings.Join(want, ",") {
		t.Fatalf("kinds %v, want %v (items %+v)", kinds, want, items)
	}
	if it := items[0]; it.State != leadstore.StageLost || it.Detail != "no budget" || !it.At.Equal(at(6)) {
		t.Errorf("stage: %+v", it)
	}
	if it := items[1]; it.State != "safe" || it.Detail != "info@timeline.localhost" {
		t.Errorf("email check: %+v", it)
	}
	if it := items[2]; it.State != "no_answer" || it.Detail != "rang" || it.MessageID != call || it.Step != 2 ||
		it.CallBackOn == nil || it.CallBackOn.Format(time.DateOnly) != "2026-11-12" {
		t.Errorf("call: %+v", it)
	}
	if it := items[3]; it.State != "reply" || it.Detail != snip {
		t.Errorf("mailbox: %+v", it)
	}
	if it := items[4]; !it.Shadow || it.State != "shadow_sent" || it.Channel != "email" || it.Step != 0 {
		t.Errorf("shadow send: %+v", it)
	}
	if it := items[5]; it.Shadow || it.State != "sent" || !it.At.Equal(at(1)) {
		t.Errorf("live send: %+v", it)
	}
	if it := items[6]; it.State != "batched" || it.Detail != "batch 1" {
		t.Errorf("event: %+v", it)
	}
	if items, err := q.LeadTimeline(ctx, lead, 2); err != nil || len(items) != 2 || items[0].Kind != leadstore.TimelineStage {
		t.Errorf("limit 2: %+v %v", items, err)
	}
}

// TestRepliesInbox: leads with a matched reply, latest reply first, with the latest snippet
// and the reply count; snoozed and selftest leads are left out; a snooze that ended shows again.
func TestRepliesInbox(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	q := f.q
	if rows, total, err := q.RepliesInbox(ctx, crmNow, 10); err != nil || len(rows) != 0 || total != 0 {
		t.Fatalf("cold start: %+v %d %v", rows, total, err)
	}
	a := f.siteLead(t, "inbox-a.localhost", nil, 7, "info@inbox-a.localhost")
	b := f.siteLead(t, "inbox-b.localhost", nil, 7, "info@inbox-b.localhost")
	snoozed := f.siteLead(t, "inbox-snoozed.localhost", nil, 7, "info@inbox-snoozed.localhost")
	self := f.siteLead(t, "inbox-self.localhost", nil, 7, "info@inbox-self.localhost")
	f.selftest(t, self)
	first, second := "First answer.", "Second answer."
	f.replyEvent(t, a, 1, &first, crmNow.Add(-5*time.Hour))
	f.replyEvent(t, a, 2, &second, crmNow.Add(-1*time.Hour))
	f.replyEvent(t, b, 3, nil, crmNow.Add(-2*time.Hour))
	f.replyEvent(t, snoozed, 4, &first, crmNow.Add(-30*time.Minute))
	f.replyEvent(t, self, 5, &first, crmNow)
	f.replied(t, a, crmNow.Add(-5*time.Hour))
	if err := q.SnoozeLead(ctx, snoozed, crmNow.Add(time.Hour), crmNow); err != nil {
		t.Fatal(err)
	}
	rows, total, err := q.RepliesInbox(ctx, crmNow, 10)
	if err != nil {
		t.Fatal(err)
	}
	if total != 2 || len(rows) != 2 {
		t.Fatalf("rows %+v total %d, want a then b", rows, total)
	}
	if r := rows[0]; r.LeadID != a || r.Snippet != second || r.Replies != 2 || !r.RepliedAt.Equal(crmNow.Add(-time.Hour)) ||
		r.EffectiveStage != leadstore.StageTalking || r.Name == "" || r.Domain != "inbox-a.localhost" {
		t.Errorf("a: %+v", r)
	}
	if r := rows[1]; r.LeadID != b || r.Snippet != "" || r.Replies != 1 {
		t.Errorf("b: %+v", r)
	}
	if rows, total, err := q.RepliesInbox(ctx, crmNow.Add(2*time.Hour), 1); err != nil || total != 3 || len(rows) != 1 || rows[0].LeadID != snoozed {
		t.Errorf("after the snooze ends (limit 1): %+v %d %v", rows, total, err)
	}
}

// TestPipeline: a replied lead with no row is talking; stored stages show in board order with
// their deal, next action, lost reason and tenant; a new lead and a selftest lead do not.
func TestPipeline(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	q := f.q
	if rows, err := q.Pipeline(ctx); err != nil || rows == nil || len(rows) != 0 {
		t.Fatalf("cold start: %+v %v", rows, err)
	}
	talk := f.siteLead(t, "pipe-talk.localhost", nil, 7, "info@pipe-talk.localhost")
	prop := f.siteLead(t, "pipe-prop.localhost", nil, 7, "info@pipe-prop.localhost")
	won := f.siteLead(t, "pipe-won.localhost", nil, 7, "info@pipe-won.localhost")
	lost := f.siteLead(t, "pipe-lost.localhost", nil, 7, "info@pipe-lost.localhost")
	_ = f.siteLead(t, "pipe-new.localhost", nil, 7, "info@pipe-new.localhost")
	self := f.siteLead(t, "pipe-self.localhost", nil, 7, "info@pipe-self.localhost")
	f.selftest(t, self)
	f.replied(t, talk, crmNow)
	f.replied(t, self, crmNow)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(q.SetLeadStage(ctx, prop, leadstore.StageProposal, "", "", crmNow))
	must(q.SetLeadNote(ctx, prop, "", "Chase the quote", &crmNow, crmNow))
	must(q.SetLeadStage(ctx, won, leadstore.StageWon, "", "https://pipe-won.example.localhost/", crmNow))
	must(q.SetLeadDeal(ctx, won, int64p(120000), "RON", crmNow))
	must(q.SetLeadStage(ctx, lost, leadstore.StageLost, "chose an in-house team", "", crmNow))
	must(q.SetLeadNote(ctx, self, "", "", nil, crmNow))

	rows, err := q.Pipeline(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 4 {
		t.Fatalf("rows %+v, want talk, prop, won, lost", rows)
	}
	if r := rows[0]; r.LeadID != talk || r.Stage != leadstore.StageTalking || r.UpdatedAt != nil || r.Currency != leadstore.DefaultCurrency {
		t.Errorf("talk: %+v", r)
	}
	if r := rows[1]; r.LeadID != prop || r.Stage != leadstore.StageProposal || r.NextAction != "Chase the quote" || r.NextActionOn == nil {
		t.Errorf("prop: %+v", r)
	}
	if r := rows[2]; r.LeadID != won || r.DealValueCents == nil || *r.DealValueCents != 120000 || r.Currency != "RON" ||
		r.TenantURL != "https://pipe-won.example.localhost/" || r.Name == "" {
		t.Errorf("won: %+v", r)
	}
	if r := rows[3]; r.LeadID != lost || r.LostReason != "chose an in-house team" {
		t.Errorf("lost: %+v", r)
	}
}

// TestNeedsMeCRM: a snoozed replied lead is hidden until the snooze ends; a future next action
// hides a replied lead; a due next action shows once, as next_action with its text, first,
// even on a lead nothing else would list; a selftest lead's next action never shows.
func TestNeedsMeCRM(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	q := f.q
	loc := time.FixedZone("UTC+2", 2*3600)
	snoozed := f.siteLead(t, "needs-snoozed.localhost", nil, 7, "info@needs-snoozed.localhost")
	later := f.siteLead(t, "needs-later.localhost", nil, 7, "info@needs-later.localhost")
	due := f.siteLead(t, "needs-due.localhost", nil, 7, "info@needs-due.localhost")
	quiet := f.siteLead(t, "needs-quiet.localhost", nil, 7, "info@needs-quiet.localhost")
	plain := f.siteLead(t, "needs-plain.localhost", nil, 7, "info@needs-plain.localhost")
	self := f.siteLead(t, "needs-self.localhost", nil, 7, "info@needs-self.localhost")
	f.selftest(t, self)
	for _, id := range []int64{snoozed, later, due, plain} {
		f.replied(t, id, crmNow.Add(-time.Hour))
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(q.SnoozeLead(ctx, snoozed, crmNow.Add(24*time.Hour), crmNow))
	tomorrow := crmNow.AddDate(0, 0, 1)
	must(q.SetLeadNote(ctx, later, "", "Ring back after the holiday", &tomorrow, crmNow))
	// crmNow is 14:00 in loc: today there is 2026-11-10.
	today := time.Date(2026, 11, 10, 0, 0, 0, 0, loc)
	yesterday := today.AddDate(0, 0, -1)
	must(q.SetLeadNote(ctx, due, "", "Send the proposal", &today, crmNow))
	must(q.SetLeadNote(ctx, quiet, "", "Check their new site", &yesterday, crmNow))
	must(q.SetLeadNote(ctx, self, "", "Test", &yesterday, crmNow))

	rows, total, err := q.NeedsMeAt(ctx, 10, crmNow, loc)
	if err != nil {
		t.Fatal(err)
	}
	if total != 3 || len(rows) != 3 {
		t.Fatalf("rows %+v total %d, want quiet, due (next actions) then plain", rows, total)
	}
	if r := rows[0]; r.LeadID != quiet || r.What != leadstore.NeedsNextAction || r.NextAction != "Check their new site" ||
		!r.At.Equal(yesterday) {
		t.Errorf("first: %+v", r)
	}
	if r := rows[1]; r.LeadID != due || r.What != leadstore.NeedsNextAction || r.NextAction != "Send the proposal" {
		t.Errorf("second: %+v", r)
	}
	if r := rows[2]; r.LeadID != plain || r.What != leadstore.NeedsReplied || r.NextAction != "" {
		t.Errorf("third: %+v", r)
	}
	// Two days on: the snooze has ended and later's next action is due.
	rows, total, err = q.NeedsMeAt(ctx, 10, crmNow.Add(48*time.Hour), loc)
	if err != nil || total != 5 {
		t.Fatalf("two days on: %+v %d %v, want 5", rows, total, err)
	}
	whats := map[int64]string{}
	for _, r := range rows {
		whats[r.LeadID] = r.What
	}
	if whats[snoozed] != leadstore.NeedsReplied || whats[later] != leadstore.NeedsNextAction {
		t.Errorf("two days on: %v", whats)
	}
	// The clock form still answers.
	if _, _, err := q.NeedsMe(ctx, 10); err != nil {
		t.Errorf("NeedsMe: %v", err)
	}
}

// TestCallQueue: the day's queue holds a call due that day with no outcome, and a call-back due
// that day or earlier on the call it was set on; an answered call, a future call-back, a shadow
// batch's call and a call-back on a lead that opted out do not show. A call-back date in the
// past, or with do_not_call, is refused and writes nothing.
func TestCallQueue(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	q := f.q
	if rows, err := q.CallQueue(ctx, day(5), time.UTC); err != nil || len(rows) != 0 {
		t.Fatalf("cold start: %+v %v", rows, err)
	}
	upToCall := func(domain string, shadow bool) seqLead {
		t.Helper()
		s := f.seqBatch(t, shadow, f.siteLead(t, domain, nil, 7, "info@"+domain))
		f.send(t, s, shadow, 0, day(0))
		f.send(t, s, shadow, 1, day(3))
		return s
	}
	due := upToCall("queue-due.localhost", false)
	answered := upToCall("queue-answered.localhost", false)
	back := upToCall("queue-back.localhost", false)
	future := upToCall("queue-future.localhost", false)
	gone := upToCall("queue-gone.localhost", false)
	_ = upToCall("queue-shadow.localhost", true)

	record := func(s seqLead, outcome string, backOn *time.Time) error {
		_, err := q.RecordCallOutcomeWithCallBack(ctx, 0, s.msg[2], outcome, "", nil, backOn, leadstore.ActorGremy, day(5), plainDays)
		return err
	}
	past, d6, d9 := day(4), day(6), day(9)
	if err := record(back, campaign.OutcomeNoAnswer, &past); !errors.Is(err, leadstore.ErrBadOutcome) || !strings.Contains(err.Error(), "in the past") {
		t.Errorf("call-back in the past: %v", err)
	}
	if err := record(back, campaign.OutcomeDoNotCall, &d6); !errors.Is(err, leadstore.ErrBadOutcome) {
		t.Errorf("call-back with do_not_call: %v", err)
	}
	if n := ownerInt(t, f.owner, `SELECT count(*) FROM call_outcome WHERE message_id = $1`, back.msg[2]); n != 0 {
		t.Fatalf("a refused call-back wrote %d outcomes", n)
	}
	today := day(5)
	if err := record(answered, campaign.OutcomeConnected, nil); err != nil {
		t.Fatal(err)
	}
	if err := record(back, campaign.OutcomeNoAnswer, &today); err != nil {
		t.Fatalf("call-back today: %v", err)
	}
	if err := record(future, campaign.OutcomeVoicemail, &d9); err != nil {
		t.Fatal(err)
	}
	if err := record(gone, campaign.OutcomeNoAnswer, &today); err != nil {
		t.Fatal(err)
	}
	f.ownerRun(t, `UPDATE lead SET state = 'opted_out' WHERE id = $1`, gone.lead)
	if n := ownerInt(t, f.owner, `SELECT count(*) FROM call_outcome WHERE message_id = $1 AND call_back_on = $2::date`,
		back.msg[2], "2026-11-07"); n != 1 {
		t.Errorf("call_back_on stored: %d", n)
	}

	rows, err := q.CallQueue(ctx, day(5).Add(3*time.Hour), time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	got := map[int64]leadstore.CallQueueRow{}
	for _, r := range rows {
		got[r.LeadID] = r
	}
	if len(rows) != 2 {
		t.Fatalf("rows %+v, want due and back", rows)
	}
	if r := got[due.lead]; r.Kind != leadstore.CallQueueDue || r.MessageID != due.msg[2] || !r.Due.Equal(day(5)) ||
		r.Phone != callNumber || r.Name == "" || r.CallBackOn != nil {
		t.Errorf("due: %+v", r)
	}
	if r := got[back.lead]; r.Kind != leadstore.CallQueueBack || r.MessageID != back.msg[2] || r.Outcome != campaign.OutcomeNoAnswer ||
		r.CallBackOn == nil || !r.Due.Equal(time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("call-back: %+v", r)
	}
	if !rows[0].Due.Before(rows[1].Due) && !rows[0].Due.Equal(rows[1].Due) {
		t.Errorf("not soonest first: %+v", rows)
	}
	// On the 11th the voicemail call-back is due too, and the first call-back is still waiting.
	rows, err = q.CallQueue(ctx, d9, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, r := range rows {
		if r.LeadID == future.lead && r.Kind == leadstore.CallQueueBack {
			n++
		}
	}
	if n != 1 {
		t.Errorf("on the 11th: %+v, want the voicemail call-back", rows)
	}
	_ = d6
}

// TestCampaignFunnelCRM: Contacted counts leads reached by a live email or a call outcome; Won
// counts leads moved to won since that are still won, and Revenue sums their values per
// currency; a lead won before since, or won then lost, does not count.
func TestCampaignFunnelCRM(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	q := f.q
	since := crmNow.AddDate(0, 0, -30)
	a := f.siteLead(t, "funnel-a.localhost", nil, 7, "info@funnel-a.localhost")
	b := f.siteLead(t, "funnel-b.localhost", nil, 7, "info@funnel-b.localhost")
	c := f.siteLead(t, "funnel-c.localhost", nil, 7, "info@funnel-c.localhost")
	d := f.siteLead(t, "funnel-d.localhost", nil, 7, "info@funnel-d.localhost")
	e := f.siteLead(t, "funnel-e.localhost", nil, 7, "info@funnel-e.localhost")
	live, _ := f.batch(t, false, a, b)
	f.sent(t, f.msgID(t, live, a, 0), "sent", since.Add(time.Hour))
	f.sent(t, f.msgID(t, live, a, 1), "sent", since.Add(2*time.Hour))
	f.sent(t, f.msgID(t, live, b, 0), "sent", since.Add(-time.Hour))
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(q.SetLeadStage(ctx, a, leadstore.StageWon, "", "", since.Add(time.Hour)))
	must(q.SetLeadDeal(ctx, a, int64p(300000), "GBP", crmNow))
	must(q.SetLeadStage(ctx, b, leadstore.StageWon, "", "", since.Add(time.Hour)))
	must(q.SetLeadDeal(ctx, b, int64p(50000), "EUR", crmNow))
	must(q.SetLeadStage(ctx, c, leadstore.StageWon, "", "", since.Add(time.Hour))) // no value
	must(q.SetLeadStage(ctx, d, leadstore.StageWon, "", "", since.Add(-time.Hour)))
	must(q.SetLeadDeal(ctx, d, int64p(999), "GBP", crmNow))
	must(q.SetLeadStage(ctx, e, leadstore.StageWon, "", "", since.Add(time.Hour)))
	must(q.SetLeadStage(ctx, e, leadstore.StageLost, "changed their mind", "", since.Add(2*time.Hour)))

	rows, err := q.CampaignFunnel(ctx, since)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows %+v, want campaign 1", rows)
	}
	r := rows[0]
	if r.Sent != 2 || r.Contacted != 1 || r.Won != 3 || len(r.Revenue) != 2 || r.Revenue["GBP"] != 300000 || r.Revenue["EUR"] != 50000 {
		t.Errorf("funnel: %+v", r)
	}
	if rows, err := q.CampaignFunnel(ctx, crmNow.Add(time.Hour)); err != nil || len(rows) != 0 {
		t.Errorf("nothing since: %+v %v", rows, err)
	}
}

// TestForgetOrgCRM: forget blanks the org's reply snippets and deletes its CRM row and stage
// changes, counting each; the other org's are kept; a second run counts zero.
func TestForgetOrgCRM(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	gone := f.siteLead(t, "forget-crm.localhost", nil, 7, "info@forget-crm.localhost")
	keep := f.siteLead(t, "keep-crm.localhost", nil, 7, "info@keep-crm.localhost")
	snip := "Please call Jo on the main line."
	for i, id := range []int64{gone, keep} {
		f.replyEvent(t, id, 10+i, &snip, crmNow)
		if err := f.q.SetLeadStage(ctx, id, leadstore.StageLost, "not now", "", crmNow); err != nil {
			t.Fatal(err)
		}
		if err := f.q.SetLeadNote(ctx, id, "Spoke to the owner.", "", nil, crmNow); err != nil {
			t.Fatal(err)
		}
	}
	owner := leadstore.NewQueries(f.owner)
	got, err := owner.ForgetOrg(ctx, f.orgOf(t, gone), "erasure request")
	if err != nil {
		t.Fatal(err)
	}
	if got.SnippetsBlanked != 1 || got.CRMRows != 1 || got.StageEvents != 1 {
		t.Errorf("forget = %+v, want one of each", got)
	}
	if n := ownerInt(t, f.owner, `SELECT count(*) FROM mailbox_event WHERE lead_id = $1 AND snippet IS NULL`, gone); n != 1 {
		t.Errorf("the forgotten lead's reply event: %d with no snippet, want 1 (the row stays)", n)
	}
	if c, err := f.q.GetLeadCRM(ctx, gone); err != nil || c.UpdatedAt != nil || c.Note != "" {
		t.Errorf("the forgotten lead's CRM: %+v %v", c, err)
	}
	if n := ownerInt(t, f.owner, `SELECT count(*) FROM lead_stage_event WHERE lead_id = $1`, gone); n != 0 {
		t.Errorf("stage events left: %d", n)
	}
	if c, err := f.q.GetLeadCRM(ctx, keep); err != nil || c.Note == "" || c.Stage != leadstore.StageLost {
		t.Errorf("the other lead's CRM went too: %+v %v", c, err)
	}
	if n := ownerInt(t, f.owner, `SELECT count(*) FROM mailbox_event WHERE lead_id = $1 AND snippet = $2`, keep, snip); n != 1 {
		t.Errorf("the other lead's snippet went too")
	}
	again, err := owner.ForgetOrg(ctx, f.orgOf(t, gone), "erasure request")
	if err != nil || again.SnippetsBlanked != 0 || again.CRMRows != 0 || again.StageEvents != 0 {
		t.Errorf("second forget: %+v %v", again, err)
	}
}

// TestLeadCampaigns: each existing lead maps to its campaign; unknown ids and none are empty.
func TestLeadCampaigns(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	a := f.siteLead(t, "camp-a.localhost", nil, 7, "info@camp-a.localhost")
	f.ownerRun(t, `INSERT INTO campaign (id, account_id, slug, name) VALUES (2, 1, 'cafes', 'Cafes')`)
	b := f.siteLead(t, "camp-b.localhost", nil, 7, "info@camp-b.localhost")
	f.ownerRun(t, `UPDATE lead SET campaign_id = 2 WHERE id = $1`, b)
	if got, err := f.q.LeadCampaigns(ctx, nil); err != nil || got == nil || len(got) != 0 {
		t.Errorf("none: %v %v", got, err)
	}
	got, err := f.q.LeadCampaigns(ctx, []int64{a, b, 999999})
	if err != nil || len(got) != 2 || got[a] != 1 || got[b] != 2 {
		t.Errorf("campaigns: %v %v", got, err)
	}
}

// TestClearLeadStage: a won lead with a tenant link, cleared, has no stored stage, lost reason
// or tenant link, its note and deal kept and its updated_at moved, and no stage change is
// written; a replied lead then reads as talking, a fresh one as none. Clearing again, or a lead
// never staged, changes nothing; a missing lead is ErrLeadNotFound.
func TestClearLeadStage(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	q := f.q
	talk := f.siteLead(t, "clear-talk.localhost", nil, 7, "info@clear-talk.localhost")
	fresh := f.siteLead(t, "clear-fresh.localhost", nil, 7, "info@clear-fresh.localhost")
	never := f.siteLead(t, "clear-never.localhost", nil, 7, "info@clear-never.localhost")
	f.replied(t, talk, crmNow.Add(-time.Hour))
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	events := func(lead int64) int64 {
		return ownerInt(t, f.owner, `SELECT count(*) FROM lead_stage_event WHERE lead_id = $1`, lead)
	}
	later := crmNow.Add(time.Hour)
	for _, c := range []struct {
		lead int64
		want string
	}{{talk, leadstore.StageTalking}, {fresh, ""}} {
		must(q.SetLeadStage(ctx, c.lead, leadstore.StageWon, "", "https://tenant.localhost/", crmNow))
		must(q.SetLeadNote(ctx, c.lead, "keep me", "", nil, crmNow))
		must(q.SetLeadDeal(ctx, c.lead, int64p(1000), "EUR", crmNow))
		before := events(c.lead)
		must(q.ClearLeadStage(ctx, c.lead, later))
		got, err := q.GetLeadCRM(ctx, c.lead)
		must(err)
		if got.Stage != "" || got.EffectiveStage != c.want || got.TenantURL != "" || got.LostReason != "" ||
			got.Note != "keep me" || got.DealValueCents == nil || *got.DealValueCents != 1000 || got.Currency != "EUR" ||
			got.UpdatedAt == nil || !got.UpdatedAt.Equal(later) {
			t.Errorf("cleared lead %d: %+v, want effective stage %q", c.lead, got, c.want)
		}
		if n := events(c.lead); n != before {
			t.Errorf("clearing lead %d wrote %d stage changes", c.lead, n-before)
		}
		// Again: nothing moves, not even updated_at.
		must(q.ClearLeadStage(ctx, c.lead, later.Add(time.Hour)))
		if again, err := q.GetLeadCRM(ctx, c.lead); err != nil || again.UpdatedAt == nil || !again.UpdatedAt.Equal(later) || again.Stage != "" {
			t.Errorf("cleared twice %d: %+v %v", c.lead, again, err)
		}
	}
	// A lost lead: its reason goes too.
	must(q.SetLeadStage(ctx, never, leadstore.StageLost, "no budget", "", crmNow))
	must(q.ClearLeadStage(ctx, never, later))
	if got, err := q.GetLeadCRM(ctx, never); err != nil || got.Stage != "" || got.LostReason != "" {
		t.Errorf("cleared lost lead: %+v %v", got, err)
	}
	// A lead with no row stays without one.
	none := f.siteLead(t, "clear-none.localhost", nil, 7, "info@clear-none.localhost")
	must(q.ClearLeadStage(ctx, none, later))
	if got, err := q.GetLeadCRM(ctx, none); err != nil || got.UpdatedAt != nil {
		t.Errorf("never staged: %+v %v", got, err)
	}
	if err := q.ClearLeadStage(ctx, 999999, later); !errors.Is(err, leadstore.ErrLeadNotFound) {
		t.Errorf("missing lead: %v", err)
	}
}

// TestEmailChecksByCampaign: checks since a time count once each on the campaign of the lead
// whose address they checked (its route's, or one its emails went to); a check before since,
// one of an address no lead has, and one in another account are not counted.
func TestEmailChecksByCampaign(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	q := f.q
	since := crmNow.AddDate(0, 0, -30)
	a := f.siteLead(t, "checks-a.localhost", nil, 7, "info@checks-a.localhost")
	f.ownerRun(t, `INSERT INTO campaign (id, account_id, slug, name) VALUES (2, 1, 'cafes', 'Cafes')`)
	b := f.siteLead(t, "checks-b.localhost", nil, 7, "info@checks-b.localhost")
	f.ownerRun(t, `UPDATE lead SET campaign_id = 2 WHERE id = $1`, b)
	// a's email went to a second address too.
	live, _ := f.batch(t, false, a)
	f.ownerRun(t, `UPDATE outreach_message SET to_address = 'Sales@checks-a.localhost' WHERE batch_id = $1 AND lead_id = $2 AND channel = 'email'`, live, a)

	if got, err := q.EmailChecksByCampaign(ctx, since); err != nil || got == nil || len(got) != 0 {
		t.Fatalf("none: %v %v", got, err)
	}
	add := func(addr string, at time.Time) {
		t.Helper()
		if _, err := q.AddEmailCheck(ctx, leadstore.EmailCheck{Address: addr, Provider: "reoon", Status: "safe", CheckedAt: at}); err != nil {
			t.Fatal(err)
		}
	}
	add("INFO@checks-a.localhost", since.Add(time.Hour))   // a's route
	add("info@checks-a.localhost", since.Add(2*time.Hour)) // a's route, again
	add("sales@checks-a.localhost", since.Add(time.Hour))  // where a's email went
	add("info@checks-a.localhost", since.Add(-time.Hour))  // before since
	add("info@checks-b.localhost", since.Add(time.Hour))   // b's route, campaign 2
	add("nobody@checks-x.localhost", since.Add(time.Hour)) // no lead has it
	f.ownerRun(t, `INSERT INTO account (id, slug, name) VALUES (2, 'other', 'Other') ON CONFLICT DO NOTHING`)
	f.ownerRun(t, `INSERT INTO email_check (account_id, address, provider, status, checked_at) VALUES (2, 'info@checks-b.localhost', 'reoon', 'safe', $1)`,
		since.Add(time.Hour)) // another account's check of b's address

	got, err := q.EmailChecksByCampaign(ctx, since)
	if err != nil || len(got) != 2 || got[1] != 3 || got[2] != 1 {
		t.Errorf("checks by campaign: %v %v, want map[1:3 2:1]", got, err)
	}
}
