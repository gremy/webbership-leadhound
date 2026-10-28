package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/campaign"
	"github.com/gremy/webbership-leadhound/internal/enrich"

	"github.com/gremy/webbership-leadhound/internal/console"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
)

// TestCallableCardFromVerdict: the card shows the call rule's verdict as it is: callable or
// blocked with the reason, the screening status and the campaign-rules warning.
func TestCallableCardFromVerdict(t *testing.T) {
	due := time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)
	c := leadstore.CallRow{MessageID: 1, OrgDomain: "a.example", Phone: "+441632960001", DueAt: &due}
	card := callCard(c, callVerdict{Rule: ruleScreening, Reason: "Not screened against TPS", Screening: "TPS: not screened yet"})
	if card.Callable || card.BlockedReason != "Not screened against TPS" || card.Screening != "TPS: not screened yet" {
		t.Errorf("blocked card = %+v", card)
	}
	card = callCard(c, callVerdict{Callable: true, Screening: "not required", CampaignOff: callsOffCard("IE")})
	if !card.Callable || card.BlockedReason != "" || !strings.Contains(card.CallsOff, "Ireland") {
		t.Errorf("callable card = %+v", card)
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
	}, time.UTC, console.TodayView{}, nil)
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
		card := callCard(leadstore.CallRow{MessageID: 1, Phone: value, PhoneNormalized: norm}, callVerdict{})
		if card.Dial != c.want || card.Number != c.printed {
			t.Errorf("%q: dial %q, number %q; want dial %q and the printed number", c.printed, card.Dial, card.Number, c.want)
		}
	}
	// A number printed nationally is kept as national digits; the card dials it with the
	// lead's country code, and with no country it gives no link rather than a wrong one.
	value, norm, ok := enrich.NormalizePhone("01437 720202")
	if !ok {
		t.Fatal("01437 720202 does not normalise")
	}
	if card := callCard(leadstore.CallRow{MessageID: 3, Phone: value, PhoneNormalized: norm, Country: "GB"}, callVerdict{}); card.Dial != "+441437720202" {
		t.Errorf("GB national: dial %q, want +441437720202", card.Dial)
	}
	if card := callCard(leadstore.CallRow{MessageID: 4, Phone: value, PhoneNormalized: norm}, callVerdict{}); card.Dial != "" || card.Number != "01437 720202" {
		t.Errorf("no country: dial %q number %q, want no dial and the printed number", card.Dial, card.Number)
	}
	if card := callCard(leadstore.CallRow{MessageID: 2}, callVerdict{}); card.Dial != "" || card.Number != "" {
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
			"Saved: Got through. (shadow batch) The sequence has ended: the remaining emails are cancelled.", "waits on"},
		{campaign.OutcomeDoNotCall, steps(leadstore.MsgCancelled, leadstore.MsgCancelled, nil, nil), true,
			"Saved: Do not call. (shadow batch) The sequence has ended: the remaining emails are cancelled. Their number is on the do-not-call list now.", "waits on"},
		{campaign.OutcomeNoAnswer, steps(leadstore.MsgApproved, leadstore.MsgApproved, &due, nil), false,
			`Saved: No answer. (shadow batch) The "tried to reach you" email is due Thu 29 Oct 09:30.`, "final"},
		{campaign.OutcomeVoicemail, steps(leadstore.MsgApproved, leadstore.MsgApproved, &due, nil), false,
			`Saved: Left a voicemail. (shadow batch) The "tried to reach you" email is due Thu 29 Oct 09:30.`, "final"},
		{campaign.OutcomeWrongNumber, steps(leadstore.MsgCancelled, leadstore.MsgApproved, nil, &due), false,
			"Saved: Wrong number. (shadow batch) The final email is due Thu 29 Oct 09:30.", "ended"},
		{campaign.OutcomeSkipped, steps(leadstore.MsgCancelled, leadstore.MsgApproved, nil, &due), false,
			"Saved: Skipped. (shadow batch) The final email is due Thu 29 Oct 09:30.", "ended"},
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

// TestTodayCallableSplit: "Calls to make now" holds only the calls the call rule says are
// callable; a call it refuses is under Calls due with its reason, in the store's order; a
// call whose earlier step is unsent goes with the waiting calls whatever its verdict; a
// campaign-rules warning keeps the call callable and says so.
func TestTodayCallableSplit(t *testing.T) {
	inHours := time.Date(2026, 10, 28, 11, 0, 0, 0, time.UTC)
	rows := []leadstore.CallRow{
		{MessageID: 1, OrgDomain: "a.example", Step: 2, DueAt: &inHours, Country: "IE"},
		{MessageID: 2, OrgDomain: "b.example", Step: 2, DueAt: &inHours, WaitsOnStep: intp(1), WaitsOnChannel: "email"},
		{MessageID: 3, OrgDomain: "c.example", Step: 2, DueAt: &inHours},
	}
	ok := map[int64]callVerdict{1: {Callable: true}, 2: {Callable: true}, 3: {Callable: true}}
	allowed := todayFromStore(leadstore.Today{CallsDueNow: rows}, time.UTC, console.TodayView{}, ok)
	if len(allowed.CallsNow) != 2 || allowed.CallsNow[0].MessageID != 1 || allowed.CallsNow[1].MessageID != 3 || len(allowed.CallsDue) != 0 {
		t.Errorf("all callable: now %+v due %+v", allowed.CallsNow, allowed.CallsDue)
	}
	if len(allowed.CallsWaiting) != 1 || allowed.CallsWaiting[0].MessageID != 2 || allowed.CallsWaitingTotal != 1 ||
		!strings.Contains(allowed.CallsWaiting[0].Line, "waits on step 2 (email)") {
		t.Errorf("waiting = %+v (total %d)", allowed.CallsWaiting, allowed.CallsWaitingTotal)
	}
	mixed := map[int64]callVerdict{
		1: {Callable: true, CampaignOff: callsOffCard("IE")},
		3: {Rule: "window", Reason: "Outside the calling hours for Ireland now"},
	}
	v := todayFromStore(leadstore.Today{CallsDueNow: rows}, time.UTC, console.TodayView{}, mixed)
	if len(v.CallsNow) != 1 || v.CallsNow[0].MessageID != 1 || !strings.Contains(v.CallsNow[0].Line, "calls off for IE in the campaign rules: your call") {
		t.Errorf("warned call: now %+v", v.CallsNow)
	}
	if len(v.CallsDue) != 1 || v.CallsDue[0].MessageID != 3 || !strings.Contains(v.CallsDue[0].Line, "not callable: outside the calling hours") {
		t.Errorf("refused call: due %+v", v.CallsDue)
	}
	// a waiting call the rules already refuse says why, as its card does (not the hours)
	waitRows := []leadstore.CallRow{{MessageID: 4, Step: 2, PrevStep: 1, PrevChannel: leadstore.ChannelEmail, WaitsOnStep: new(1)},
		{MessageID: 5, Step: 2, PrevStep: 1, PrevChannel: leadstore.ChannelEmail, WaitsOnStep: new(1)}}
	wv := todayFromStore(leadstore.Today{CallsWaiting: waitRows}, time.UTC, console.TodayView{}, map[int64]callVerdict{
		4: {Rule: ruleScreening, Reason: "Not screened against the TPS yet"},
		5: {Rule: "window", Reason: "Outside the calling hours for Ireland now"},
	})
	if len(wv.CallsWaiting) != 2 || !strings.Contains(wv.CallsWaiting[0].Line, " · not callable: not screened against the TPS yet") ||
		strings.Contains(wv.CallsWaiting[1].Line, "not callable") {
		t.Errorf("waiting calls: %+v", wv.CallsWaiting)
	}
	none := todayFromStore(leadstore.Today{CallsDueNow: rows[:1]}, time.UTC, console.TodayView{}, nil)
	if len(none.CallsNow) != 0 || len(none.CallsDue) != 1 {
		t.Errorf("a call with no verdict is not callable: %+v", none)
	}
}

// TestOutcomeLineButtonWordsAndOperatorZone: the saved line names the outcome in the
// buttons' own words, never its code, and gives the next step's time in the zone it is handed
// (the operator's), the zone every other console time uses.
func TestOutcomeLineButtonWordsAndOperatorZone(t *testing.T) {
	bucharest, err := time.LoadLocation("Europe/Bucharest")
	if err != nil {
		t.Skip("no tz data:", err)
	}
	due := time.Date(2026, 10, 28, 7, 50, 0, 0, time.UTC) // 07:50 London, 09:50 Bucharest
	steps := []leadstore.SequenceStep{
		{Step: 2, Type: campaign.StepCall, MessageID: 3, State: leadstore.MsgDone},
		{Step: 3, Type: campaign.StepEmail, When: campaign.WhenCallNotConnected, MessageID: 4, State: leadstore.MsgApproved, DueAt: &due},
	}
	got := outcomeLine(leadstore.CallRecorded{Outcome: campaign.OutcomeVoicemail}, steps, 2, bucharest)
	if want := `Saved: Left a voicemail. The "tried to reach you" email is due Wed 28 Oct 09:50.`; got != want {
		t.Errorf("got %q\nwant %q", got, want)
	}
	for _, o := range campaign.Outcomes {
		line := outcomeLine(leadstore.CallRecorded{Outcome: o}, nil, 2, time.UTC)
		if !strings.HasPrefix(line, "Saved: "+console.OutcomeWords(o)+".") || strings.Contains(line, "_") {
			t.Errorf("%s: %q", o, line)
		}
	}
	if got := callRefusalWords(fmt.Errorf("record: already recorded as no_answer: %w", leadstore.ErrCallAdvanced)); !strings.Contains(got, "Already recorded as No answer,") {
		t.Errorf("advanced refusal: %q", got)
	}
}

// TestTodayLastOutcomeWords: a call's last outcome on Today reads in the buttons' words.
func TestTodayLastOutcomeWords(t *testing.T) {
	due := time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC)
	v := todayFromStore(leadstore.Today{CallsOverdue: []leadstore.CallRow{{MessageID: 1, OrgDomain: "a.example", DueAt: &due, Outcome: campaign.OutcomeWrongNumber}}},
		time.UTC, console.TodayView{}, nil)
	if len(v.CallsOverdue) != 1 || !strings.Contains(v.CallsOverdue[0].Line, "last time: Wrong number") {
		t.Errorf("line: %+v", v.CallsOverdue)
	}
}

// TestSequenceStepsCarryCallNote: the lead page's steps carry the operator's call note.
func TestSequenceStepsCarryCallNote(t *testing.T) {
	at := time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC)
	out := sequenceSteps([]leadstore.SequenceStep{{Step: 2, Type: campaign.StepCall, MessageID: 3, State: leadstore.MsgDone,
		Outcome: campaign.OutcomeConnected, OutcomeAt: &at, Note: "spoke to Ana, send the report"}})
	if len(out) != 1 || out[0].CallNote != "spoke to Ana, send the report" || out[0].State != console.SeqDone {
		t.Errorf("steps: %+v", out)
	}
}
