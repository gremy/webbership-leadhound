package campaign

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/leadconfig"
	"github.com/gremy/webbership-leadhound/internal/outreach"
)

// dayCal counts calendar days: enough to see which anchor and which count the engine used.
var dayCal = CalendarFunc(func(from time.Time, n int) time.Time { return from.AddDate(0, 0, n) })

var (
	t0 = time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC) // step 0 sent
	t1 = time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC) // step 1 sent
	tc = time.Date(2026, 11, 23, 11, 0, 0, 0, time.UTC) // the call's outcome
	t3 = time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC) // step 3 sent
)

func at(t time.Time) *time.Time { return &t }

// fiveSteps is the default mixed sequence of testdata/valid.yaml: email, email +3, call +2
// (has_callable_phone), email +1 (call_not_connected), email +5.
func fiveSteps(t *testing.T) []Step {
	t.Helper()
	return validSpec(t).Sequence
}

func sent(id int64, step int, when time.Time) Row {
	return Row{MessageID: id, Step: step, State: StateSent, SentAt: at(when)}
}

func pending(id int64, step int) Row { return Row{MessageID: id, Step: step, State: StateApproved} }

func checkDecision(t *testing.T, got Decision, due map[int64]time.Time, cancel map[int64]string) {
	t.Helper()
	if due == nil {
		due = map[int64]time.Time{}
	}
	if cancel == nil {
		cancel = map[int64]string{}
	}
	if !maps.EqualFunc(got.Due, due, time.Time.Equal) {
		t.Errorf("due = %v; want %v", got.Due, due)
	}
	if !maps.Equal(got.Cancel, cancel) {
		t.Errorf("cancel = %v; want %v", got.Cancel, cancel)
	}
}

func TestSequenceEmailPath(t *testing.T) {
	seq := fiveSteps(t)
	cases := []struct {
		name   string
		rows   []Row
		due    map[int64]time.Time
		cancel map[int64]string
	}{
		{"nothing sent: step 0's due is set at approval",
			[]Row{{MessageID: 10, Step: 0, State: StateApproved, DueAt: at(t0)}, pending(11, 1), pending(12, 2), pending(13, 3), pending(14, 4)}, nil, nil},
		{"step 0 sent: step 1 due after 3",
			[]Row{sent(10, 0, t0), pending(11, 1), pending(12, 2), pending(13, 3), pending(14, 4)},
			map[int64]time.Time{11: t0.AddDate(0, 0, 3)}, nil},
		{"step 0 shadow-sent counts as sent",
			[]Row{{MessageID: 10, Step: 0, State: StateShadowSent, SentAt: at(t0)}, pending(11, 1)},
			map[int64]time.Time{11: t0.AddDate(0, 0, 3)}, nil},
		{"step 1 already scheduled: nothing new",
			[]Row{sent(10, 0, t0), {MessageID: 11, Step: 1, State: StateApproved, DueAt: at(t1)}, pending(12, 2)}, nil, nil},
		{"step 1 sent: the call falls due after 2",
			[]Row{sent(10, 0, t0), sent(11, 1, t1), pending(12, 2), pending(13, 3), pending(14, 4)},
			map[int64]time.Time{12: t1.AddDate(0, 0, 2)}, nil},
		{"call not made yet: steps 3 and 4 wait",
			[]Row{sent(10, 0, t0), sent(11, 1, t1), {MessageID: 12, Step: 2, State: StateApproved, DueAt: at(t1)}, pending(13, 3), pending(14, 4)}, nil, nil},
		{"no phone (no call, no step 3 row): step 4 counts from step 1's send",
			[]Row{sent(10, 0, t0), sent(11, 1, t1), pending(14, 4)},
			map[int64]time.Time{14: t1.AddDate(0, 0, 5)}, nil},
		{"call cancelled by the engine: step 3's condition fails, step 4 counts from step 1",
			[]Row{sent(10, 0, t0), sent(11, 1, t1), {MessageID: 12, Step: 2, State: StateCancelled, CancelReason: CancelCondition}, pending(13, 3), pending(14, 4)},
			map[int64]time.Time{14: t1.AddDate(0, 0, 5)}, map[int64]string{13: CancelCondition}},
		{"step 1 skipped at approval: nothing after it is scheduled (as the legacy rule)",
			[]Row{sent(10, 0, t0), {MessageID: 11, Step: 1, State: "skipped", CancelReason: "skipped at approval"}, pending(12, 2)}, nil, nil},
		{"step 1 refused: nothing after it",
			[]Row{sent(10, 0, t0), {MessageID: 11, Step: 1, State: "refused"}, pending(12, 2)}, nil, nil},
		{"step 1 maybe_sent: nothing after it",
			[]Row{sent(10, 0, t0), {MessageID: 11, Step: 1, State: "maybe_sent"}, pending(12, 2)}, nil, nil},
		{"reply cancelled every pending row: nothing due, nothing to cancel",
			[]Row{sent(10, 0, t0), {MessageID: 11, Step: 1, State: StateCancelled, CancelReason: "replied"},
				{MessageID: 12, Step: 2, State: StateCancelled, CancelReason: "replied"}, {MessageID: 14, Step: 4, State: StateCancelled, CancelReason: "replied"}}, nil, nil},
		{"every step done: nothing",
			[]Row{sent(10, 0, t0), sent(11, 1, t1), sent(14, 4, t3)}, nil, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			checkDecision(t, Next(seq, tc.rows, nil, t3, dayCal), tc.due, tc.cancel)
		})
	}
}

// TestSequenceCallOutcomes walks every row of the CP3 call-outcome table.
func TestSequenceCallOutcomes(t *testing.T) {
	seq := fiveSteps(t)
	rows := func() []Row {
		return []Row{sent(10, 0, t0), sent(11, 1, t1), {MessageID: 12, Step: 2, State: StateDone}, pending(13, 3), pending(14, 4)}
	}
	cases := []struct {
		outcome string
		due     map[int64]time.Time
		cancel  map[int64]string
	}{
		{OutcomeConnected, nil, map[int64]string{13: CancelCallConnected, 14: CancelCallConnected}},
		{OutcomeNoAnswer, map[int64]time.Time{13: tc.AddDate(0, 0, 1)}, nil},
		{OutcomeVoicemail, map[int64]time.Time{13: tc.AddDate(0, 0, 1)}, nil},
		{OutcomeWrongNumber, map[int64]time.Time{14: tc.AddDate(0, 0, 5)}, map[int64]string{13: CancelWrongNumber}},
		{OutcomeDoNotCall, nil, map[int64]string{13: CancelDoNotCall, 14: CancelDoNotCall}},
		{OutcomeSkipped, map[int64]time.Time{14: tc.AddDate(0, 0, 5)}, map[int64]string{13: CancelCallSkipped}},
	}
	if len(cases) != len(Outcomes) {
		t.Fatalf("the table covers %d outcomes; there are %d", len(cases), len(Outcomes))
	}
	for _, c := range cases {
		t.Run(c.outcome, func(t *testing.T) {
			outs := []Outcome{{MessageID: 12, Outcome: c.outcome, At: tc}}
			got := Next(seq, rows(), outs, t3, dayCal)
			checkDecision(t, got, c.due, c.cancel)

			// Applying the decision and walking again changes nothing: the engine's own
			// cancels are passed over, and a scheduled row is left alone.
			applied := rows()
			for i := range applied {
				id := applied[i].MessageID
				if r, ok := got.Cancel[id]; ok {
					applied[i].State, applied[i].CancelReason = StateCancelled, r
				}
				if d, ok := got.Due[id]; ok {
					applied[i].DueAt = at(d)
				}
			}
			checkDecision(t, Next(seq, applied, outs, t3, dayCal), nil, nil)
		})
	}

	t.Run("no answer, then step 3 sent: step 4 after step 3", func(t *testing.T) {
		r := rows()
		r[3] = sent(13, 3, t3)
		checkDecision(t, Next(seq, r, []Outcome{{MessageID: 12, Outcome: OutcomeNoAnswer, At: tc}}, t3, dayCal),
			map[int64]time.Time{14: t3.AddDate(0, 0, 5)}, nil)
	})
	t.Run("the latest outcome wins", func(t *testing.T) {
		outs := []Outcome{{MessageID: 12, Outcome: OutcomeConnected, At: tc.Add(time.Hour)}, {MessageID: 12, Outcome: OutcomeNoAnswer, At: tc}}
		checkDecision(t, Next(seq, rows(), outs, t3, dayCal), nil, map[int64]string{13: CancelCallConnected, 14: CancelCallConnected})
	})
	t.Run("a call marked done with no outcome stops the walk", func(t *testing.T) {
		checkDecision(t, Next(seq, rows(), nil, t3, dayCal), nil, nil)
	})
	t.Run("an outcome with no time anchors at now", func(t *testing.T) {
		checkDecision(t, Next(seq, rows(), []Outcome{{MessageID: 12, Outcome: OutcomeNoAnswer}}, t3, dayCal),
			map[int64]time.Time{13: t3.AddDate(0, 0, 1)}, nil)
	})
	t.Run("no slot from the calendar: nothing due", func(t *testing.T) {
		none := CalendarFunc(func(time.Time, int) time.Time { return time.Time{} })
		checkDecision(t, Next(seq, rows(), []Outcome{{MessageID: 12, Outcome: OutcomeNoAnswer, At: tc}}, t3, none), nil, nil)
	})
}

// callFirst is the default call_first sequence of CP5: call, call again after 2 if not
// connected, email "as promised" (email_consent), follow-up email after 4 (email_consent).
var callFirst = []Step{
	{Step: 0, Type: StepCall},
	{Step: 1, Type: StepCall, AfterBusinessDays: 2, When: WhenCallNotConnected},
	{Step: 2, Type: StepEmail, When: WhenEmailConsent},
	{Step: 3, Type: StepEmail, AfterBusinessDays: 4, When: WhenEmailConsent},
}

func TestSequenceCallFirst(t *testing.T) {
	tc0, tc1 := tc, tc.AddDate(0, 0, 2)
	call := func(id int64, step int) Row { return Row{MessageID: id, Step: step, State: StateDone} }
	cases := []struct {
		name   string
		rows   []Row
		outs   []Outcome
		due    map[int64]time.Time
		cancel map[int64]string
	}{
		{"first call not made: nothing",
			[]Row{{MessageID: 20, Step: 0, State: StateApproved, DueAt: at(tc0)}, pending(21, 1), pending(22, 2), pending(23, 3)}, nil, nil, nil},
		{"no answer: call again after 2",
			[]Row{call(20, 0), pending(21, 1), pending(22, 2), pending(23, 3)},
			[]Outcome{{MessageID: 20, Outcome: OutcomeNoAnswer, At: tc0}},
			map[int64]time.Time{21: tc0.AddDate(0, 0, 2)}, nil},
		{"no answer twice: the sequence ends",
			[]Row{call(20, 0), call(21, 1), pending(22, 2), pending(23, 3)},
			[]Outcome{{MessageID: 20, Outcome: OutcomeNoAnswer, At: tc0}, {MessageID: 21, Outcome: OutcomeVoicemail, At: tc1}},
			nil, map[int64]string{22: CancelCondition, 23: CancelCondition}},
		{"connected without consent: the sequence ends",
			[]Row{call(20, 0), pending(21, 1), pending(22, 2), pending(23, 3)},
			[]Outcome{{MessageID: 20, Outcome: OutcomeConnected, At: tc0}},
			nil, map[int64]string{21: CancelCallConnected, 22: CancelNoConsent, 23: CancelNoConsent}},
		{"connected with consent: the email falls due",
			[]Row{call(20, 0), pending(21, 1), pending(22, 2), pending(23, 3)},
			[]Outcome{{MessageID: 20, Outcome: OutcomeConnected, EmailConsent: true, At: tc0}},
			map[int64]time.Time{22: tc0}, map[int64]string{21: CancelCallConnected}},
		{"no answer, then connected with consent on the second call",
			[]Row{call(20, 0), call(21, 1), pending(22, 2), pending(23, 3)},
			[]Outcome{{MessageID: 20, Outcome: OutcomeNoAnswer, At: tc0}, {MessageID: 21, Outcome: OutcomeConnected, EmailConsent: true, At: tc1}},
			map[int64]time.Time{22: tc1}, nil},
		{"consent, step 2 sent: the follow-up after 4",
			[]Row{call(20, 0), {MessageID: 21, Step: 1, State: StateCancelled, CancelReason: CancelCallConnected}, sent(22, 2, t3), pending(23, 3)},
			[]Outcome{{MessageID: 20, Outcome: OutcomeConnected, EmailConsent: true, At: tc0}},
			map[int64]time.Time{23: t3.AddDate(0, 0, 4)}, nil},
		{"consent, then opt-out (the store cancels every pending row): nothing due",
			[]Row{call(20, 0), {MessageID: 21, Step: 1, State: StateCancelled, CancelReason: CancelCallConnected},
				{MessageID: 22, Step: 2, State: StateCancelled, CancelReason: "opted out"}, {MessageID: 23, Step: 3, State: StateCancelled, CancelReason: "opted out"}},
			[]Outcome{{MessageID: 20, Outcome: OutcomeConnected, EmailConsent: true, At: tc0}}, nil, nil},
		{"wrong number: no second call, no consent",
			[]Row{call(20, 0), pending(21, 1), pending(22, 2), pending(23, 3)},
			[]Outcome{{MessageID: 20, Outcome: OutcomeWrongNumber, At: tc0}},
			nil, map[int64]string{21: CancelWrongNumber, 22: CancelCondition, 23: CancelCondition}},
		{"do not call: everything ends",
			[]Row{call(20, 0), pending(21, 1), pending(22, 2), pending(23, 3)},
			[]Outcome{{MessageID: 20, Outcome: OutcomeDoNotCall, At: tc0}},
			nil, map[int64]string{21: CancelDoNotCall, 22: CancelDoNotCall, 23: CancelDoNotCall}},
		{"skipped: no second call, no consent",
			[]Row{call(20, 0), pending(21, 1), pending(22, 2), pending(23, 3)},
			[]Outcome{{MessageID: 20, Outcome: OutcomeSkipped, At: tc0}},
			nil, map[int64]string{21: CancelCallSkipped, 22: CancelCondition, 23: CancelCondition}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			checkDecision(t, Next(callFirst, c.rows, c.outs, t3, dayCal), c.due, c.cancel)
		})
	}
}

// legacyScheduleNext is the old rule, copied from cmd/leadhound/leads_send.go:1127-1138
// (sendRun.scheduleNext, applied by leadstore.ScheduleFollowup at
// internal/leadstore/mailbox.go:408, which sets due_at only when it is NULL): after step s is
// sent at sentAt, step s+1 falls due outreach.FollowupDue(sentAt, days[s], loc, bh), only for
// s < len(days) and s < 2; a zero due schedules nothing.
func legacyScheduleNext(days []int, step int, sentAt time.Time, loc *time.Location, bh leadconfig.BusinessHours) (int, time.Time, bool) {
	if step >= len(days) || step >= 2 {
		return 0, time.Time{}, false
	}
	due := outreach.FollowupDue(sentAt, days[step], loc, bh)
	if due.IsZero() {
		return 0, time.Time{}, false
	}
	return step + 1, due, true
}

// TestSequenceLegacyContractDrift: the legacy sequence stored on a new batch, run through
// Next on the real business-day calendar, gives exactly the old rule's due dates.
func TestSequenceLegacyContractDrift(t *testing.T) {
	cfg := demoConfig(t)
	days := cfg.Schedule.FollowupBusinessDays
	bh := cfg.Schedule.BusinessHours
	seq := LegacySequence(days)
	if got := Legacy(cfg).Sequence; len(got) != len(seq) || got[1] != seq[1] || got[2] != seq[2] {
		t.Fatalf("Legacy's sequence %+v is not LegacySequence %+v", got, seq)
	}
	for _, zone := range []string{"Europe/London", "Europe/Bucharest", "America/New_York"} {
		loc, err := time.LoadLocation(zone)
		if err != nil {
			t.Fatal(err)
		}
		cal := CalendarFunc(func(from time.Time, n int) time.Time { return outreach.FollowupDue(from, n, loc, bh) })
		sends := []time.Time{
			time.Date(2026, 11, 16, 10, 5, 0, 0, loc),  // Monday, inside the window
			time.Date(2026, 11, 20, 16, 29, 0, 0, loc), // Friday, just inside
			time.Date(2026, 11, 20, 18, 0, 0, 0, loc),  // Friday evening
			time.Date(2026, 11, 21, 11, 0, 0, 0, loc),  // Saturday (shadow or hand send)
			time.Date(2026, 12, 23, 9, 0, 0, 0, loc),   // before the window opens
			time.Date(2026, 3, 27, 15, 0, 0, 0, loc),   // the Friday before Europe's clock change
			time.Date(2026, 10, 30, 15, 0, 0, 0, loc),  // the Friday before the autumn change
		}
		for _, s := range sends {
			for step := range 3 {
				rows := []Row{pending(100, 0), pending(101, 1), pending(102, 2)}
				prev := s
				for i := 0; i <= step; i++ {
					rows[i] = sent(int64(100+i), i, prev)
					prev = prev.AddDate(0, 0, 9) // earlier steps' sends; only the last one counts
				}
				sentAt := *rows[step].SentAt
				want := map[int64]time.Time{}
				if next, due, ok := legacyScheduleNext(days, step, sentAt, loc, bh); ok {
					want[int64(100+next)] = due
				}
				got := Next(seq, rows, nil, sentAt, cal)
				if !maps.EqualFunc(got.Due, want, time.Time.Equal) || len(got.Cancel) != 0 {
					t.Errorf("%s step %d sent %s: engine %v cancel %v; legacy %v", zone, step, sentAt, got.Due, got.Cancel, want)
				}
			}
		}
	}
}

func TestSequenceLegacySequenceShape(t *testing.T) {
	seq := LegacySequence([]int{4, 7})
	want := []Step{{Step: 0, Type: StepEmail, When: WhenAlways}, {Step: 1, Type: StepEmail, AfterBusinessDays: 4, When: WhenAlways}, {Step: 2, Type: StepEmail, AfterBusinessDays: 7, When: WhenAlways}}
	if len(seq) != len(want) {
		t.Fatalf("got %+v", seq)
	}
	for i := range want {
		if seq[i] != want[i] {
			t.Errorf("step %d = %+v; want %+v", i, seq[i], want[i])
		}
	}
	s := validSpec(t)
	s.Sequence = seq
	s.Copy.MaxWords = nil
	if err := s.Check(known); err != nil {
		t.Errorf("the legacy sequence does not pass Check: %v", err)
	}
}

const callFirstYAML = `
sequences:
  call_first:
    - {step: 0, type: call}
    - {step: 1, type: call, after_business_days: 2, when: call_not_connected}
    - {step: 2, type: email, when: email_consent}
    - {step: 3, type: email, after_business_days: 4, when: email_consent}
`

func decodeWith(t *testing.T, edit func(string) string) (Spec, error) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "valid.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return Decode([]byte(edit(string(b))))
}

func TestSequenceNamedInSpec(t *testing.T) {
	s, err := decodeWith(t, func(b string) string { return b + callFirstYAML })
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Check(known); err != nil {
		t.Fatalf("Check: %v", err)
	}
	if got := s.SequenceFor(SequenceCallFirst); len(got) != 4 || got[0].Type != StepCall || got[3].When != WhenEmailConsent {
		t.Errorf("call_first = %+v", got)
	}
	if got := s.SequenceFor(""); len(got) != 5 || &got[0] != &s.Sequence[0] {
		t.Errorf("default = %+v", got)
	}
	if got := s.SequenceFor(SequenceDefault); len(got) != 5 {
		t.Errorf("default by name = %+v", got)
	}
	if s.SequenceFor("nope") != nil {
		t.Error("an unknown name gave a sequence")
	}
}

// TestSequenceDefaultSpellings: sequences.default is the same spec as sequence:, hash and all.
func TestSequenceDefaultSpellings(t *testing.T) {
	a := validSpec(t)
	b, err := decodeWith(t, func(y string) string {
		return strings.Replace(y, "sequence:\n", "sequences:\n  default:\n", 1)
	})
	if err != nil {
		t.Fatal(err)
	}
	if b.Sequences != nil || len(b.Sequence) != 5 {
		t.Fatalf("sequences.default not moved into sequence: %+v / %+v", b.Sequence, b.Sequences)
	}
	_, ha, _ := Canonical(a)
	_, hb, _ := Canonical(b)
	if ha != hb {
		t.Error("the two spellings hash differently")
	}
}

// TestSequenceCanonicalUnchanged: a spec without named sequences canonicalises with no
// sequences key, so every hash registered before CP3 stays the same.
func TestSequenceCanonicalUnchanged(t *testing.T) {
	j, _, err := Canonical(validSpec(t))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(j, &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m["sequences"]; ok {
		t.Errorf("canonical JSON gained a sequences key: %s", j)
	}
	if !strings.HasSuffix(string(j), `"templates_dir":"templates"}`) {
		t.Errorf("canonical JSON no longer ends with templates_dir: %s", j)
	}
}

func TestSequenceCheckRules(t *testing.T) {
	cases := []struct {
		name  string
		edit  func(*Spec)
		wants []string
	}{
		{"both default spellings", func(s *Spec) { s.Sequences = map[string][]Step{SequenceDefault: s.Sequence} },
			[]string{"either sequence or sequences.default"}},
		{"call_first starts with an email", func(s *Spec) {
			s.Sequences = map[string][]Step{SequenceCallFirst: {{Step: 0, Type: StepEmail}}}
		}, []string{"sequences.call_first must start with a call"}},
		{"call_first empty", func(s *Spec) { s.Sequences = map[string][]Step{SequenceCallFirst: nil} },
			[]string{"sequences.call_first is empty"}},
		{"call_first gap", func(s *Spec) {
			cf := append([]Step(nil), callFirst...)
			cf[2].Step = 5
			s.Sequences = map[string][]Step{SequenceCallFirst: cf}
		}, []string{"sequences.call_first[2]: step is 5"}},
		{"unknown sequence name", func(s *Spec) { s.Sequences = map[string][]Step{"weekend": callFirst} },
			[]string{`"weekend" is not a known sequence`}},
		{"email_consent before a call", func(s *Spec) { s.Sequence[1].When = WhenEmailConsent },
			[]string{`when "email_consent" needs a call step before it`}},
		{"default starts with a call", func(s *Spec) { s.Sequence[0].Type = StepCall },
			[]string{"sequence must start with an email"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := validSpec(t)
			tc.edit(&s)
			err := s.Check(known)
			if err == nil {
				t.Fatal("Check passed; want a problem")
			}
			for _, w := range tc.wants {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q lacks %q", err, w)
				}
			}
		})
	}

	t.Run("max_words may name a step of the longest sequence", func(t *testing.T) {
		s := validSpec(t)
		long := append([]Step(nil), callFirst...)
		for i := 4; i < 7; i++ {
			long = append(long, Step{Step: i, Type: StepEmail, AfterBusinessDays: 3})
		}
		s.Sequences = map[string][]Step{SequenceCallFirst: long}
		s.Copy.MaxWords[6] = 50
		if err := s.Check(known); err != nil {
			t.Errorf("Check: %v", err)
		}
	})
}

// TestDefaultCallFirstPassesCheck: the default call_first sequence is one a spec could hold,
// and the engine runs it as the call-first grid expects.
func TestDefaultCallFirstPassesCheck(t *testing.T) {
	var errs []string
	checkSteps(func(f string, a ...any) { errs = append(errs, fmt.Sprintf(f, a...)) }, "sequences.call_first", DefaultCallFirst(), StepCall)
	if len(errs) > 0 {
		t.Fatalf("DefaultCallFirst fails its check: %v", errs)
	}
	if got := DefaultCallFirst(); len(got) != 4 || got[2].When != WhenEmailConsent || got[1].When != WhenCallNotConnected {
		t.Errorf("DefaultCallFirst = %+v", got)
	}
}
