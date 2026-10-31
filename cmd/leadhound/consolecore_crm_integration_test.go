//go:build integration

package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/campaign"
	"github.com/gremy/webbership-leadhound/internal/console"
	"github.com/gremy/webbership-leadhound/internal/leadstore/leadstoretest"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

// crmDay is midnight of the cp5 world's day, plus days, in the backend's zone: a date as the
// console hands one over.
func crmDay(w *cp5World, days int) time.Time {
	n := w.now.In(w.b.loc)
	return time.Date(n.Year(), n.Month(), n.Day()+days, 0, 0, 0, 0, w.b.loc)
}

// wantCRMRefused checks err is a *console.CRMRefused on field.
func wantCRMRefused(t *testing.T, what string, err error, field string) {
	t.Helper()
	var cr *console.CRMRefused
	if !errors.As(err, &cr) || !errors.Is(err, console.ErrUserFixable) {
		t.Fatalf("%s: %v, want a CRMRefused", what, err)
	}
	if cr.Field != field || !strings.HasSuffix(cr.Sentence, "Nothing was saved.") {
		t.Errorf("%s: field %q sentence %q, want field %q", what, cr.Field, cr.Sentence, field)
	}
}

// TestCRMBackendNotSetUp: without leads set up every CRM read and write says so.
func TestCRMBackendNotSetUp(t *testing.T) {
	ctx := context.Background()
	leadstoretest.Reset(t)
	b := testBackend(t)
	if _, err := b.LeadCRM(ctx, 1); !errors.Is(err, console.ErrLeadsNotSetUp) {
		t.Errorf("LeadCRM: %v", err)
	}
	if _, err := b.SetLeadStage(ctx, console.StageChange{LeadID: 1, Stage: console.StageTalking}); !errors.Is(err, console.ErrLeadsNotSetUp) {
		t.Errorf("SetLeadStage: %v", err)
	}
	if _, err := b.CampaignReport(ctx, time.Now().AddDate(0, 0, -30)); !errors.Is(err, console.ErrLeadsNotSetUp) {
		t.Errorf("CampaignReport: %v", err)
	}
	if _, err := b.LeadCallOutcomeOn(ctx, console.CallOutcomeRequest{MessageID: 1, Outcome: campaign.OutcomeNoAnswer}); !errors.Is(err, console.ErrLeadsNotSetUp) {
		t.Errorf("LeadCallOutcomeOn: %v", err)
	}
}

// TestCRMBackendColdStart: with no CRM row anywhere, a lead's CRM is the zero state, the
// inbox and the pipeline are empty, the report divides nothing by nothing (cost per lead and
// per reply 0), and a lead that does not exist is not found.
func TestCRMBackendColdStart(t *testing.T) {
	ctx := context.Background()
	w := newCP5World(t)
	b := w.b
	c, err := b.LeadCRM(ctx, w.gbLead)
	if err != nil {
		t.Fatal(err)
	}
	if c.Stage != "" || c.EffectiveStage != "" || c.NextActionOn != nil || c.DealValueCents != nil || c.Currency != "GBP" || c.SnoozedUntil != nil {
		t.Errorf("cold CRM %+v", c)
	}
	if rows, err := b.RepliesInbox(ctx); err != nil || len(rows) != 0 {
		t.Errorf("replies %v %+v", err, rows)
	}
	if cards, err := b.Pipeline(ctx); err != nil || len(cards) != 0 {
		t.Errorf("pipeline %v %+v", err, cards)
	}
	if _, err := b.LeadTimeline(ctx, w.gbLead); err != nil {
		t.Errorf("timeline: %v", err)
	}
	// Spend on a lead with nothing contacted: the cost per lead and per reply stay 0.
	ownerExec(t, `INSERT INTO ai_usage (provider, model, day, calls, input_tokens, purpose, cost_micros, created_at, account_id, lead_id)
		VALUES ('anthropic', 'claude-test', $1::date, 1, 10, 'drafting', 420000, $1, 1, $2)`, w.now.Add(-time.Hour), w.gbLead)
	rows, err := b.CampaignReport(ctx, w.now.AddDate(0, 0, -30))
	if err != nil {
		t.Fatal(err)
	}
	var spent int64
	for _, r := range rows {
		spent += r.ClaudeMicros
		if r.Contacted == 0 && r.CostPerLeadMicros != 0 || r.Replied == 0 && r.CostPerReplyMicros != 0 {
			t.Errorf("division by zero: %+v", r)
		}
		if r.EmailChecks != 0 {
			t.Errorf("email checks %+v", r)
		}
	}
	if spent != 420000 {
		t.Errorf("spend %d over %+v, want 420000", spent, rows)
	}
	// Two checks of the lead's address count on its campaign's row; one before since does not.
	var addr string
	if err := storetest.OwnerConn(t).QueryRow(ctx, `SELECT a FROM (
		  SELECT lower(r.value) AS a FROM lead l JOIN contact_route r ON r.id = l.route_id WHERE l.id = $1 AND r.kind = 'email'
		  UNION SELECT lower(m.to_address) FROM outreach_message m WHERE m.lead_id = $1 AND m.channel = 'email' AND m.to_address <> ''
		) x ORDER BY a LIMIT 1`, w.gbLead).Scan(&addr); err != nil {
		t.Fatalf("the lead's email address: %v", err)
	}
	for _, at := range []time.Time{w.now.Add(-time.Hour), w.now.Add(-2 * time.Hour), w.now.AddDate(0, 0, -31)} {
		ownerExec(t, `INSERT INTO email_check (account_id, address, provider, status, checked_at) VALUES (1, $1, 'reoon', 'safe', $2)`, addr, at)
	}
	rows, err = b.CampaignReport(ctx, w.now.AddDate(0, 0, -30))
	if err != nil {
		t.Fatal(err)
	}
	checks := 0
	for _, r := range rows {
		checks += r.EmailChecks
	}
	if checks != 2 {
		t.Errorf("email checks %d over %+v, want 2", checks, rows)
	}
	const nobody = 9_999_999
	if _, err := b.LeadCRM(ctx, nobody); !errors.Is(err, console.ErrNotFound) {
		t.Errorf("LeadCRM nobody: %v", err)
	}
	if _, err := b.LeadTimeline(ctx, nobody); !errors.Is(err, console.ErrNotFound) {
		t.Errorf("LeadTimeline nobody: %v", err)
	}
	if _, err := b.SetLeadStage(ctx, console.StageChange{LeadID: nobody, Stage: console.StageTalking}); !errors.Is(err, console.ErrNotFound) {
		t.Errorf("SetLeadStage nobody: %v", err)
	}
	if _, err := b.SetLeadNote(ctx, console.NoteChange{LeadID: nobody, Note: "x"}); !errors.Is(err, console.ErrNotFound) {
		t.Errorf("SetLeadNote nobody: %v", err)
	}
	if _, err := b.SnoozeLead(ctx, nobody, nil); !errors.Is(err, console.ErrNotFound) {
		t.Errorf("SnoozeLead nobody: %v", err)
	}
}

// TestCRMBackendStageDealNote: refused writes come back as CRMRefused with their field and
// save nothing; a stage and a deal show on the pipeline with the stage's expected value and
// on the timeline; a next action due today shows on the dashboard's Needs me in its words.
func TestCRMBackendStageDealNote(t *testing.T) {
	ctx := context.Background()
	w := newCP5World(t)
	b := w.b
	id := w.gbLead

	_, err := b.SetLeadStage(ctx, console.StageChange{LeadID: id, Stage: console.StageLost})
	wantCRMRefused(t, "lost with no reason", err, "lost_reason")
	_, err = b.SetLeadStage(ctx, console.StageChange{LeadID: id, Stage: console.StageProposal, TenantURL: "https://cafe.example.com"})
	wantCRMRefused(t, "tenant link off won", err, "tenant_url")
	neg := int64(-1)
	_, err = b.SetLeadDeal(ctx, console.DealChange{LeadID: id, ValueCents: &neg, Currency: "GBP"})
	wantCRMRefused(t, "negative deal", err, "value")
	_, err = b.SetLeadDeal(ctx, console.DealChange{LeadID: id, Currency: "XYZ"})
	wantCRMRefused(t, "unknown currency", err, "currency")
	_, err = b.SetLeadNote(ctx, console.NoteChange{LeadID: id, NextAction: "two\nlines"})
	wantCRMRefused(t, "two-line next action", err, "next_action")
	_, err = b.SnoozeLead(ctx, id, &w.now)
	wantCRMRefused(t, "snooze ending now", err, "")
	if n := countRows(t, `SELECT count(*) FROM lead_crm`); n != 0 {
		t.Fatalf("refused writes left %d lead_crm rows", n)
	}

	if _, err := b.SetLeadStage(ctx, console.StageChange{LeadID: id, Stage: console.StageProposal}); err != nil {
		t.Fatal(err)
	}
	v := int64(240000)
	if _, err := b.SetLeadDeal(ctx, console.DealChange{LeadID: id, ValueCents: &v, Currency: "EUR"}); err != nil {
		t.Fatal(err)
	}
	cards, err := b.Pipeline(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(cards) != 1 || cards[0].LeadID != id || cards[0].Stage != console.StageProposal || cards[0].Currency != "EUR" ||
		cards[0].DealValueCents == nil || *cards[0].DealValueCents != v || cards[0].ExpectedCents != 120000 || cards[0].Name == "" {
		t.Errorf("pipeline %+v", cards)
	}
	tl, err := b.LeadTimeline(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	staged := 0
	for _, it := range tl {
		if it.Kind == console.TimelineStage && it.Line == "Stage → Proposal" {
			staged++
		}
	}
	if staged != 1 {
		t.Errorf("stage changes on the timeline: %d in %+v", staged, tl)
	}
	// Cleared (Stage ""), the stored stage goes and the deal stays; nothing is appended to the
	// stage changes; set again, the lead is back on the pipeline.
	if _, err := b.SetLeadStage(ctx, console.StageChange{LeadID: id, Stage: ""}); err != nil {
		t.Fatalf("clear the stage: %v", err)
	}
	if c, err := b.LeadCRM(ctx, id); err != nil || c.Stage != "" || c.DealValueCents == nil || *c.DealValueCents != v {
		t.Errorf("cleared stage: %v %+v", err, c)
	}
	if cards, err := b.Pipeline(ctx); err != nil || len(cards) != 0 {
		t.Errorf("pipeline after clearing: %v %+v", err, cards)
	}
	if n := countRows(t, `SELECT count(*) FROM lead_stage_event`); n != 1 {
		t.Errorf("clearing wrote stage changes: %d", n)
	}
	if _, err := b.SetLeadStage(ctx, console.StageChange{LeadID: 9_999_999, Stage: ""}); !errors.Is(err, console.ErrNotFound) {
		t.Errorf("clear on a missing lead: %v", err)
	}
	if _, err := b.SetLeadStage(ctx, console.StageChange{LeadID: id, Stage: console.StageProposal}); err != nil {
		t.Fatal(err)
	}

	today := crmDay(w, 0)
	if _, err := b.SetLeadNote(ctx, console.NoteChange{LeadID: id, Note: "Owner wants a quote", NextAction: "Send the proposal", NextActionOn: &today}); err != nil {
		t.Fatal(err)
	}
	c, err := b.LeadCRM(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if c.Stage != console.StageProposal || c.Note != "Owner wants a quote" || c.NextAction != "Send the proposal" ||
		c.NextActionOn == nil || !c.NextActionOn.Equal(today) || c.Currency != "EUR" {
		t.Errorf("CRM %+v (next on %v, want %v)", c, c.NextActionOn, today)
	}
	var d console.Dashboard
	b.dashboardLeads(ctx, w.now, &d)
	if d.NeedsMeTotal != 1 || len(d.NeedsMe) != 1 || d.NeedsMe[0].LeadID != id || d.NeedsMe[0].What != "Next action due · Send the proposal" {
		t.Errorf("needs me %d %+v", d.NeedsMeTotal, d.NeedsMe)
	}
	// Snoozed, it drops off Needs me; cleared, it is back.
	until := w.now.Add(24 * time.Hour)
	if _, err := b.SnoozeLead(ctx, id, &until); err != nil {
		t.Fatal(err)
	}
	d = console.Dashboard{}
	b.dashboardLeads(ctx, w.now, &d)
	if d.NeedsMeTotal != 0 {
		t.Errorf("snoozed lead on Needs me: %+v", d.NeedsMe)
	}
	if _, err := b.SnoozeLead(ctx, id, nil); err != nil {
		t.Fatal(err)
	}
	if c, err := b.LeadCRM(ctx, id); err != nil || c.SnoozedUntil != nil {
		t.Errorf("cleared snooze: %v %+v", err, c.SnoozedUntil)
	}
}

// TestCRMBackendReplies: a matched reply shows in the inbox with its snippet, campaign and
// effective stage (talking, with no stage stored); snoozed, it leaves the inbox.
func TestCRMBackendReplies(t *testing.T) {
	ctx := context.Background()
	w := newCP5World(t)
	b := w.b
	id := w.gbLead
	var mailbox int64
	if err := storetest.OwnerConn(t).QueryRow(ctx, `SELECT id FROM sender_mailbox ORDER BY id LIMIT 1`).Scan(&mailbox); err != nil {
		mailbox = seedMailbox(t, leadstoretest.Q(t), "crm@sender.example").ID
	}
	ownerExec(t, `UPDATE lead SET state = 'replied', state_at = $2 WHERE id = $1`, id, w.now.Add(-time.Hour))
	ownerExec(t, `INSERT INTO mailbox_event (mailbox, uidvalidity, uid, kind, from_domain, lead_id, seen_at, mailbox_id, snippet)
		VALUES ('INBOX', 1, 41, 'reply', 'tailspin.example', $1, $2, $3, 'Thanks, call me on Thursday')`, id, w.now.Add(-time.Hour), mailbox)
	rows, err := b.RepliesInbox(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].LeadID != id || rows[0].Snippet != "Thanks, call me on Thursday" || rows[0].Campaign == "" ||
		rows[0].EffectiveStage != console.StageTalking || rows[0].Name == "" {
		t.Errorf("replies %+v", rows)
	}
	tl, err := b.LeadTimeline(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, it := range tl {
		if it.Kind == console.TimelineReply && it.Line == "Reply received" && it.Detail == "Thanks, call me on Thursday" {
			found = true
		}
	}
	if !found {
		t.Errorf("no reply on the timeline %+v", tl)
	}
	until := w.now.Add(7 * 24 * time.Hour)
	if _, err := b.SnoozeLead(ctx, id, &until); err != nil {
		t.Fatal(err)
	}
	if rows, err := b.RepliesInbox(ctx); err != nil || len(rows) != 0 {
		t.Errorf("snoozed reply still listed: %v %+v", err, rows)
	}
}

// TestCRMBackendCallQueue: the due call is queued as its card reads it; an outcome with a
// call-back date stores the date and answers as LeadCallOutcome does; the call-back is queued
// on its day, Done as the card is once the sequence moved on; a call-back in the past
// or with do-not-call is refused on its field; the report then counts the lead contacted and
// divides the spend by it.
func TestCRMBackendCallQueue(t *testing.T) {
	ctx := context.Background()
	w := newCP5World(t)
	b := w.b
	// The queue holds live calls only: the fixture's batch is a shadow one.
	ownerExec(t, `UPDATE outreach_batch SET shadow = false`)
	q, err := b.CallQueue(ctx, crmDay(w, 0))
	if err != nil {
		t.Fatal(err)
	}
	card, err := b.LeadCall(ctx, w.gbCall)
	if err != nil {
		t.Fatal(err)
	}
	var due *console.CallQueueRow
	for i := range q {
		if q[i].MessageID == w.gbCall {
			due = &q[i]
		}
	}
	if due == nil {
		t.Fatalf("the due call is not queued: %+v", q)
	}
	if due.LeadID != w.gbLead || due.Number != card.Number || due.Dial != card.Dial || due.Callable != card.Callable ||
		due.BlockedReason != card.BlockedReason || due.CallBackOn != nil || due.Done || due.Due == nil || due.Name == "" {
		t.Errorf("queued %+v; card %+v", *due, card)
	}

	_, err = b.LeadCallOutcomeOn(ctx, console.CallOutcomeRequest{MessageID: w.gbCall, Outcome: campaign.OutcomeNoAnswer, CallBackOn: new(crmDay(w, -1))})
	wantCRMRefused(t, "call-back yesterday", err, "call_back_on")
	_, err = b.LeadCallOutcomeOn(ctx, console.CallOutcomeRequest{MessageID: w.gbCall, Outcome: campaign.OutcomeDoNotCall, CallBackOn: new(crmDay(w, 2))})
	wantCRMRefused(t, "call-back with do not call", err, "call_back_on")
	if n := countRows(t, `SELECT count(*) FROM call_outcome`); n != 0 {
		t.Fatalf("refused outcomes wrote %d rows", n)
	}
	if _, err := b.LeadCallOutcomeOn(ctx, console.CallOutcomeRequest{MessageID: 9_999_999, Outcome: campaign.OutcomeNoAnswer}); !errors.Is(err, console.ErrNotFound) {
		t.Errorf("no such call: %v", err)
	}

	back := crmDay(w, 2)
	line, err := b.LeadCallOutcomeOn(ctx, console.CallOutcomeRequest{MessageID: w.gbCall, Outcome: campaign.OutcomeNoAnswer,
		Note: "rang twice", CallBackOn: &back})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(line, "Saved: "+console.OutcomeWords(campaign.OutcomeNoAnswer)) {
		t.Errorf("line %q", line)
	}
	var stored string
	if err := storetest.OwnerConn(t).QueryRow(ctx, `SELECT call_back_on::text FROM call_outcome WHERE message_id = $1`, w.gbCall).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != back.Format(time.DateOnly) {
		t.Errorf("call_back_on %s, want %s", stored, back.Format(time.DateOnly))
	}
	// The same outcome again: LeadCallOutcomeOn and LeadCallOutcome give the same answer.
	_, onErr := b.LeadCallOutcomeOn(ctx, console.CallOutcomeRequest{MessageID: w.gbCall, Outcome: campaign.OutcomeNoAnswer})
	_, plainErr := b.LeadCallOutcome(ctx, w.gbCall, campaign.OutcomeNoAnswer, "", false, "")
	if (onErr == nil) != (plainErr == nil) || onErr != nil && onErr.Error() != plainErr.Error() {
		t.Errorf("answers differ: %v / %v", onErr, plainErr)
	}

	card, err = b.LeadCall(ctx, w.gbCall)
	if err != nil {
		t.Fatal(err)
	}
	q, err = b.CallQueue(ctx, back)
	if err != nil {
		t.Fatal(err)
	}
	var cb *console.CallQueueRow
	for i := range q {
		if q[i].LeadID == w.gbLead {
			cb = &q[i]
		}
	}
	// The no-answer moved the sequence on, so the card says Done and the call-back row is done
	// too: listed on its day on the call it was set on, with no outcome buttons.
	n := 0
	for _, r := range q {
		if r.LeadID == w.gbLead {
			n++
		}
	}
	if cb == nil || n != 1 || cb.MessageID != w.gbCall || cb.CallBackOn == nil || !cb.CallBackOn.Equal(back) || !card.Done || cb.Done != card.Done {
		t.Errorf("call-back %+v listed %d times (want on %v, done as the card: %v)", cb, n, back, card.Done)
	}

	tl, err := b.LeadTimeline(ctx, w.gbLead)
	if err != nil {
		t.Fatal(err)
	}
	want := "Call: " + console.OutcomeWords(campaign.OutcomeNoAnswer) + " · call back " + back.Format("Mon 2 Jan")
	found := false
	for _, it := range tl {
		if it.Kind == console.TimelineCall && it.Line == want && it.Detail == "rang twice" {
			found = true
		}
	}
	if !found {
		t.Errorf("no %q on the timeline %+v", want, tl)
	}

	ownerExec(t, `INSERT INTO ai_usage (provider, model, day, calls, input_tokens, purpose, cost_micros, created_at, account_id, lead_id)
		VALUES ('anthropic', 'claude-test', $1::date, 1, 10, 'research', 300000, $1, 1, $2)`, w.now.Add(-time.Hour), w.gbLead)
	rows, err := b.CampaignReport(ctx, w.now.AddDate(0, 0, -30))
	if err != nil {
		t.Fatal(err)
	}
	var got *console.CampaignReportRow
	for i := range rows {
		if rows[i].ClaudeMicros > 0 {
			got = &rows[i]
		}
	}
	if got == nil || got.Contacted < 1 || got.CostPerLeadMicros != got.ClaudeMicros/int64(got.Contacted) || got.CostPerReplyMicros != 0 {
		t.Errorf("report %+v", rows)
	}
}
