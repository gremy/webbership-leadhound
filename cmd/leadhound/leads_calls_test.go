package main

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/campaign"
	"github.com/gremy/webbership-leadhound/internal/enrich"

	"github.com/gremy/webbership-leadhound/internal/console"
	"github.com/gremy/webbership-leadhound/internal/leadconfig"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
)

// TestCallsOffInCP3: until the call rules exist no call is callable anywhere: the card is
// blocked with the plain words, whatever its state, and the today view carries them over
// every call it lists.
func TestCallsOffInCP3(t *testing.T) {
	due := time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)
	for _, c := range []leadstore.CallRow{
		{MessageID: 1, OrgDomain: "a.example", Phone: "+441632960001", DueAt: &due},
		{MessageID: 2, OrgDomain: "b.example", Shadow: true, DueAt: &due, Outcome: "no_answer"},
		{MessageID: 3, OrgDomain: "c.example"},
	} {
		card := callCard(c)
		if card.Callable || card.BlockedReason != callsOffWords || !strings.Contains(card.Screening, "calls are off") {
			t.Errorf("call %d card = callable %v, blocked %q, screening %q", c.MessageID, card.Callable, card.BlockedReason, card.Screening)
		}
	}
	if callsOffWords != "Calls are off until the call rules are set up" {
		t.Errorf("callsOffWords = %q", callsOffWords)
	}
	v := todayFromStore(leadstore.Today{CallsDueNow: []leadstore.CallRow{{MessageID: 1, OrgDomain: "a.example", DueAt: &due}}},
		leadconfig.Config{}, due, time.UTC, console.TodayView{CallsOff: callsOffWords}, nil)
	// Calls are off, so a call due today is never "to make now": it is listed as due, with why.
	if v.CallsOff != callsOffWords || len(v.CallsNow) != 0 || len(v.CallsDue) != 1 || !strings.Contains(v.CallsDue[0].Line, "calls are off") {
		t.Errorf("today = %+v", v)
	}
}

// TestTodayTimeOnce: a Today row shows its time once: an email's due time in its line (At
// nil), a call's in At only (its line has no clock), never both.
func TestTodayTimeOnce(t *testing.T) {
	due := time.Date(2026, 10, 28, 11, 30, 0, 0, time.UTC)
	v := todayFromStore(leadstore.Today{
		CallsDueNow:    []leadstore.CallRow{{MessageID: 1, OrgDomain: "a.example", Step: 2, DueAt: &due, Shadow: true}},
		CallsOverdue:   []leadstore.CallRow{{MessageID: 2, OrgDomain: "b.example", Step: 2, DueAt: &due}},
		EmailsDueCount: 1,
		EmailsDue:      []leadstore.TodayItem{{MessageID: 3, OrgDomain: "c.example", Channel: "email", Step: 0, DueAt: due, Shadow: true}},
	}, leadconfig.Config{}, due, time.UTC, console.TodayView{}, nil)
	for _, it := range append(append(append(v.CallsNow, v.CallsDue...), v.CallsOverdue...), v.Emails...) {
		inLine := strings.Contains(it.Line, "11:30") || strings.Contains(it.Line, "due")
		if inLine && it.At != nil {
			t.Errorf("%s: the time shows twice: %q and At %v", it.OrgName, it.Line, it.At)
		}
		if !inLine && it.At == nil {
			t.Errorf("%s: no time at all: %q", it.OrgName, it.Line)
		}
	}
	if len(v.Emails) != 1 || !strings.Contains(v.Emails[0].Line, "Step 1 · email") {
		t.Errorf("email line: %+v", v.Emails)
	}
}

func intp(n int) *int { return &n }

// TestCallCardDial: the card dials the stored normalised number (enrich.NormalizePhone's
// output), not the printed text; an erased fact gives no number to dial.
func TestCallCardDial(t *testing.T) {
	for _, c := range []struct{ printed, want string }{
		{"+44 (0)20 7803 1100", "+442078031100"},
		{"0044 20 7803 1100", "+442078031100"},
	} {
		value, norm, ok := enrich.NormalizePhone(c.printed)
		if !ok {
			t.Fatalf("%q does not normalise", c.printed)
		}
		card := callCard(leadstore.CallRow{MessageID: 1, Phone: value, PhoneNormalized: norm})
		if card.Dial != c.want || card.Number != c.printed {
			t.Errorf("%q: dial %q, number %q; want dial %q and the printed number", c.printed, card.Dial, card.Number, c.want)
		}
	}
	if card := callCard(leadstore.CallRow{MessageID: 2}); card.Dial != "" || card.Number != "" {
		t.Errorf("erased fact: dial %q number %q, want none", card.Dial, card.Number)
	}
}

// TestNotDueWords: a call whose earlier step is unsent is not due whatever its due time, and
// the card says which step it waits on; a due call says nothing.
func TestNotDueWords(t *testing.T) {
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	past := now.Add(-time.Hour)
	waits := leadstore.CallRow{State: leadstore.MsgApproved, Step: 2, DueAt: &past, WaitsOnStep: intp(1), WaitsOnChannel: "email"}
	if got, want := notDueWords(waits, now, time.UTC), "This call isn't due yet: it waits on step 2 (email). Record what happened once it's due."; got != want {
		t.Errorf("waits: %q, want %q", got, want)
	}
	if got := notDueWords(leadstore.CallRow{State: leadstore.MsgApproved, Step: 2, DueAt: &past, PrevStep: 1}, now, time.UTC); got != "" {
		t.Errorf("due: %q, want none", got)
	}
}

// TestCallRefusalWordsNotDue: the store's not-due refusal reads in plain words with the step.
func TestCallRefusalWordsNotDue(t *testing.T) {
	err := errors.Join(errors.New("leadstore: record call outcome 3"), &leadstore.NotDueError{WaitsOnStep: 1, WaitsOnChannel: "email"})
	if got, want := callRefusalWords(err), "This call isn't due yet: it waits on step 2 (email). Record what happened once it's due. Nothing was saved."; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if got := callRefusalWords(&leadstore.NotDueError{WaitsOnStep: -1}); !strings.Contains(got, "waits on an earlier step") {
		t.Errorf("no step: %q", got)
	}
}

// TestOutcomeLine: the line after an outcome says what the sequence actually did, read from
// the steps as they stand: ended for connected and do-not-call, the "tried to reach you" email
// for no answer and voicemail, the final email for a wrong number or a skipped call.
func TestOutcomeLine(t *testing.T) {
	due := time.Date(2026, 10, 29, 9, 30, 0, 0, time.UTC)
	steps := func(s3, s4 string, d3, d4 *time.Time) []leadstore.SequenceStep {
		return []leadstore.SequenceStep{
			{Step: 0, Type: campaign.StepEmail, When: campaign.WhenAlways, MessageID: 1, State: leadstore.MsgShadowSent},
			{Step: 1, Type: campaign.StepEmail, When: campaign.WhenAlways, MessageID: 2, State: leadstore.MsgShadowSent},
			{Step: 2, Type: campaign.StepCall, When: campaign.WhenHasCallablePhone, MessageID: 3, State: leadstore.MsgDone},
			{Step: 3, Type: campaign.StepEmail, When: campaign.WhenCallNotConnected, MessageID: 4, State: s3, DueAt: d3},
			{Step: 4, Type: campaign.StepEmail, When: campaign.WhenAlways, MessageID: 5, State: s4, DueAt: d4},
		}
	}
	for _, c := range []struct {
		outcome    string
		steps      []leadstore.SequenceStep
		suppressed bool
		want, not  string
	}{
		{campaign.OutcomeConnected, steps(leadstore.MsgCancelled, leadstore.MsgCancelled, nil, nil), false,
			"Saved: connected. (shadow batch) The sequence has ended: the remaining emails are cancelled.", "waits on"},
		{campaign.OutcomeDoNotCall, steps(leadstore.MsgCancelled, leadstore.MsgCancelled, nil, nil), true,
			"Saved: do not call. (shadow batch) The sequence has ended: the remaining emails are cancelled. Their number is on the do-not-call list now.", "waits on"},
		{campaign.OutcomeNoAnswer, steps(leadstore.MsgApproved, leadstore.MsgApproved, &due, nil), false,
			`Saved: no answer. (shadow batch) The "tried to reach you" email is due Thu 29 Oct 09:30.`, "final"},
		{campaign.OutcomeVoicemail, steps(leadstore.MsgApproved, leadstore.MsgApproved, &due, nil), false,
			`Saved: voicemail. (shadow batch) The "tried to reach you" email is due Thu 29 Oct 09:30.`, "final"},
		{campaign.OutcomeWrongNumber, steps(leadstore.MsgCancelled, leadstore.MsgApproved, nil, &due), false,
			"Saved: wrong number. (shadow batch) The final email is due Thu 29 Oct 09:30.", "ended"},
		{campaign.OutcomeSkipped, steps(leadstore.MsgCancelled, leadstore.MsgApproved, nil, &due), false,
			"Saved: skipped. (shadow batch) The final email is due Thu 29 Oct 09:30.", "ended"},
	} {
		got := outcomeLine(leadstore.CallRecorded{Outcome: c.outcome, Shadow: true, Suppressed: c.suppressed}, c.steps, 2, time.UTC)
		if got != c.want {
			t.Errorf("%s:\n got %q\nwant %q", c.outcome, got, c.want)
		}
		if strings.Contains(got, c.not) || strings.Contains(got, "_") {
			t.Errorf("%s: %q says %q or carries a code", c.outcome, got, c.not)
		}
	}
}

// TestTodayCallableSplit: "Calls to make now" holds only calls that can be made now; a call
// due but outside its calling hours, or with calls off, is under Calls due with its reason,
// in the store's order; a call whose earlier step is unsent goes with the waiting calls.
func TestTodayCallableSplit(t *testing.T) {
	inHours := time.Date(2026, 10, 28, 11, 0, 0, 0, time.UTC) // a Wednesday
	night := time.Date(2026, 10, 28, 22, 0, 0, 0, time.UTC)
	rows := []leadstore.CallRow{
		{MessageID: 1, OrgDomain: "a.example", Step: 2, DueAt: &inHours},
		{MessageID: 2, OrgDomain: "b.example", Step: 2, DueAt: &inHours, WaitsOnStep: intp(1), WaitsOnChannel: "email"},
		{MessageID: 3, OrgDomain: "c.example", Step: 2, DueAt: &inHours},
	}
	allowed := todayFromStore(leadstore.Today{CallsDueNow: rows}, leadconfig.Config{}, inHours, time.UTC, console.TodayView{}, nil)
	if len(allowed.CallsNow) != 2 || allowed.CallsNow[0].MessageID != 1 || allowed.CallsNow[1].MessageID != 3 || len(allowed.CallsDue) != 0 {
		t.Errorf("in hours, calls on: now %+v due %+v", allowed.CallsNow, allowed.CallsDue)
	}
	if len(allowed.CallsWaiting) != 1 || allowed.CallsWaiting[0].MessageID != 2 || allowed.CallsWaitingTotal != 1 ||
		!strings.Contains(allowed.CallsWaiting[0].Line, "waits on step 2 (email)") {
		t.Errorf("waiting = %+v (total %d)", allowed.CallsWaiting, allowed.CallsWaitingTotal)
	}
	late := todayFromStore(leadstore.Today{CallsDueNow: rows}, leadconfig.Config{}, night, time.UTC, console.TodayView{}, nil)
	if len(late.CallsNow) != 0 || len(late.CallsDue) != 2 || late.CallsDue[0].MessageID != 1 ||
		!strings.Contains(late.CallsDue[0].Line, "outside calling hours now") {
		t.Errorf("outside hours: now %+v due %+v", late.CallsNow, late.CallsDue)
	}
	off := todayFromStore(leadstore.Today{CallsDueNow: rows}, leadconfig.Config{}, inHours, time.UTC, console.TodayView{CallsOff: callsOffWords}, nil)
	if len(off.CallsNow) != 0 || len(off.CallsDue) != 2 || !strings.Contains(off.CallsDue[1].Line, "calls are off") {
		t.Errorf("calls off: now %+v due %+v", off.CallsNow, off.CallsDue)
	}
}
