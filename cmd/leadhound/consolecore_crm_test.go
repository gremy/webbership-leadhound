package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/console"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
)

// TestStageWeights: every pipeline stage has its weight (talking 20%, proposal 50%, won
// 100%, lost 0), and the expected value is the deal's value times it, 0 with no value.
func TestStageWeights(t *testing.T) {
	want := map[string]int64{leadstore.StageTalking: 20, leadstore.StageProposal: 50, leadstore.StageWon: 100, leadstore.StageLost: 0}
	for _, st := range leadstore.Stages {
		w, ok := stageWeights[st]
		if !ok || w != want[st] {
			t.Errorf("stage %s weighs %d (listed %v), want %d", st, w, ok, want[st])
		}
	}
	if len(stageWeights) != len(leadstore.Stages) {
		t.Errorf("%d weights for %d stages", len(stageWeights), len(leadstore.Stages))
	}
	v := int64(240000)
	for st, exp := range map[string]int64{leadstore.StageTalking: 48000, leadstore.StageProposal: 120000, leadstore.StageWon: 240000, leadstore.StageLost: 0, "": 0} {
		if got := expectedCents(st, &v); got != exp {
			t.Errorf("%q: %d, want %d", st, got, exp)
		}
	}
	if got := expectedCents(leadstore.StageWon, nil); got != 0 {
		t.Errorf("no value: %d", got)
	}
}

// TestCRMErr: a missing lead is the console's not found; each store refusal names its form
// field and says nothing was saved, and stays a refusal the operator can fix.
func TestCRMErr(t *testing.T) {
	if !errors.Is(crmErr(fmt.Errorf("leadstore: set lead stage 4: %w", leadstore.ErrLeadNotFound)), console.ErrNotFound) {
		t.Error("missing lead")
	}
	if crmErr(nil) != nil {
		t.Error("nil")
	}
	other := errors.New("connection reset")
	if crmErr(other) != other {
		t.Error("other errors pass through")
	}
	for _, c := range []struct{ store, field, sentence string }{
		{"leadstore: set lead stage 4: bad CRM input: unknown stage \"maybe\"", "stage", "Unknown stage \"maybe\". Nothing was saved."},
		{"leadstore: set lead stage 4: bad CRM input: a lost lead needs a reason", "lost_reason", "A lost lead needs a reason. Nothing was saved."},
		{"leadstore: set lead stage 4: bad CRM input: the lost reason must be one line", "lost_reason", "The lost reason must be one line. Nothing was saved."},
		{"leadstore: set lead stage 4: bad CRM input: a tenant link goes only with a won lead", "tenant_url", "A tenant link goes only with a won lead. Nothing was saved."},
		{"leadstore: set lead note 4: bad CRM input: the note is over 2000 characters", "note", "The note is over 2000 characters. Nothing was saved."},
		{"leadstore: set lead note 4: bad CRM input: the next action must be one line", "next_action", "The next action must be one line. Nothing was saved."},
		{"leadstore: set lead deal 4: bad CRM input: the deal value cannot be below zero", "value", "The deal value cannot be below zero. Nothing was saved."},
		{"leadstore: set lead deal 4: bad CRM input: unknown currency \"XYZ\"", "currency", "Unknown currency \"XYZ\". Nothing was saved."},
		{"leadstore: snooze lead 4: bad CRM input: the snooze must end after now", "", "The snooze must end after now. Nothing was saved."},
	} {
		err := crmErr(&wrapped{msg: c.store, err: leadstore.ErrBadCRM})
		var cr *console.CRMRefused
		if !errors.As(err, &cr) || !errors.Is(err, console.ErrUserFixable) {
			t.Errorf("%s: %v", c.store, err)
			continue
		}
		if cr.Field != c.field || cr.Sentence != c.sentence {
			t.Errorf("%s: field %q sentence %q; want %q %q", c.store, cr.Field, cr.Sentence, c.field, c.sentence)
		}
	}
}

// wrapped is an error with the store's whole message that wraps err.
type wrapped struct {
	msg string
	err error
}

func (w *wrapped) Error() string { return w.msg }
func (w *wrapped) Unwrap() error { return w.err }

// TestTimelineItemWords: each store item is one plain line with its kind's dot, no codes.
func TestTimelineItemWords(t *testing.T) {
	cb := time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)
	at := time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		in         leadstore.TimelineItem
		kind, line string
	}{
		{leadstore.TimelineItem{Kind: leadstore.TimelineSent, State: leadstore.MsgSent, Channel: "email", Step: 0}, console.TimelineEmail, "Email sent · step 1"},
		{leadstore.TimelineItem{Kind: leadstore.TimelineSent, State: leadstore.MsgShadowSent, Channel: "email", Step: 2, Shadow: true}, console.TimelineEmail, "Email sent · step 3 · shadow batch"},
		{leadstore.TimelineItem{Kind: leadstore.TimelineSent, State: leadstore.MsgMaybeSent, Channel: "email"}, console.TimelineEmail, "Email may have gone · step 1 (check it)"},
		{leadstore.TimelineItem{Kind: leadstore.TimelineSent, State: leadstore.MsgSent, Channel: "form", Step: 1}, console.TimelineEmail, "Contact form used · step 2"},
		{leadstore.TimelineItem{Kind: leadstore.TimelineMailbox, State: "reply", Detail: "Sounds good, call me"}, console.TimelineReply, "Reply received"},
		{leadstore.TimelineItem{Kind: leadstore.TimelineMailbox, State: "bounce"}, console.TimelineEvent, "Email bounced"},
		{leadstore.TimelineItem{Kind: leadstore.TimelineCall, State: "no_answer", CallBackOn: &cb}, console.TimelineCall, "Call: " + console.OutcomeWords("no_answer") + " · call back Tue 3 Nov"},
		{leadstore.TimelineItem{Kind: leadstore.TimelineEmailCheck, State: "safe"}, console.TimelineCheck, "Email checked · safe"},
		{leadstore.TimelineItem{Kind: leadstore.TimelineEmailCheck, State: "catch_all"}, console.TimelineCheck, "Email checked · catch-all"},
		{leadstore.TimelineItem{Kind: leadstore.TimelineStage, State: leadstore.StageProposal}, console.TimelineStage, "Stage → Proposal"},
		{leadstore.TimelineItem{Kind: leadstore.TimelineEvent, State: leadstore.LeadNew}, console.TimelineEvent, "Lead added"},
		{leadstore.TimelineItem{Kind: leadstore.TimelineEvent, State: "replied"}, console.TimelineEvent, "Replied"},
		{leadstore.TimelineItem{Kind: leadstore.TimelineEvent, State: leadstore.LeadOptedOut}, console.TimelineEvent, "Opted out"},
	} {
		c.in.At = at
		got := timelineItem(c.in)
		if got.Kind != c.kind || got.Line != c.line || got.Detail != c.in.Detail || !got.At.Equal(at) {
			t.Errorf("%+v: %+v; want kind %s line %q", c.in, got, c.kind, c.line)
		}
		if strings.Contains(got.Line, "_") {
			t.Errorf("%q carries a code", got.Line)
		}
	}
}

// TestReportRow: the cost per lead and per reply divide the spend by the leads contacted and
// the replies, 0 when there is nothing to divide by; revenue keeps one entry per currency in
// the currencies' order.
func TestReportRow(t *testing.T) {
	f := leadstore.CampaignFunnelRow{Sent: 12, Contacted: 10, Replied: 3, Positive: 1, Won: 2,
		Revenue: map[string]int64{"RON": 500000, "GBP": 240000, "EUR": 95050}}
	r := reportRow("Cafes", f, 1_000_000)
	if r.Campaign != "Cafes" || r.Sent != 12 || r.Contacted != 10 || r.Replied != 3 || r.Positive != 1 || r.Won != 2 || r.ClaudeMicros != 1_000_000 {
		t.Errorf("%+v", r)
	}
	if r.CostPerLeadMicros != 100_000 || r.CostPerReplyMicros != 333_333 {
		t.Errorf("per lead %d per reply %d", r.CostPerLeadMicros, r.CostPerReplyMicros)
	}
	want := []console.Money{{Currency: "GBP", Cents: 240000}, {Currency: "EUR", Cents: 95050}, {Currency: "RON", Cents: 500000}}
	if fmt.Sprint(r.Revenue) != fmt.Sprint(want) {
		t.Errorf("revenue %v, want %v", r.Revenue, want)
	}
	z := reportRow("Drupal", leadstore.CampaignFunnelRow{}, 420_000)
	if z.CostPerLeadMicros != 0 || z.CostPerReplyMicros != 0 || z.ClaudeMicros != 420_000 || z.Revenue != nil {
		t.Errorf("nothing contacted: %+v", z)
	}
}

// TestForgetWordsCRM: forget's line counts the CRM it erased, and says nothing of it when
// there was none.
func TestForgetWordsCRM(t *testing.T) {
	got := forgetWords(7, leadstore.Forgotten{PhoneFacts: 1, SnippetsBlanked: 2, CRMRows: 1, StageEvents: 3}, leadstore.ForgottenPages{})
	for _, want := range []string{"Org 7 forgotten:", "1 phone fact", "2 reply snippets emptied", "1 CRM record and 3 stage changes deleted"} {
		if !strings.Contains(got, want) {
			t.Errorf("%q lacks %q", got, want)
		}
	}
	plain := forgetWords(7, leadstore.Forgotten{PhoneFacts: 1}, leadstore.ForgottenPages{})
	if strings.Contains(plain, "CRM") || strings.Contains(plain, "snippet") {
		t.Errorf("no CRM: %q", plain)
	}
	if got := forgetWords(7, leadstore.Forgotten{CRMRows: 1}, leadstore.ForgottenPages{}); strings.Contains(got, "nothing left") {
		t.Errorf("a CRM row alone is something: %q", got)
	}
}
