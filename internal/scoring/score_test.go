package scoring

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/ai"
	"github.com/gremy/webbership-leadhound/internal/candidate"
	"github.com/gremy/webbership-leadhound/internal/store"
)

// testCriteria is a fictional candidate's criteria, in scorer order.
//
//nolint:gosec // G101: the Pass fields are criterion descriptions, not credentials
var testCriteria = candidate.Criteria{
	Title:      candidate.Criterion{Pass: "A backend engineering role.", Fail: "Not engineering.", Unclear: "A generic title."},
	Stack:      candidate.Criterion{Pass: "Rust is the main language.", Fail: "No Rust at all.", Unclear: "No languages named."},
	Seniority:  candidate.Criterion{Pass: "Mid-level or senior.", Fail: "Junior or principal.", Unclear: "No level given."},
	Location:   candidate.Criterion{Pass: "Fully remote from Exampleland.", Fail: "Office elsewhere.", Unclear: "Remote from where?"},
	WorkRights: candidate.Criterion{Pass: "Open to Exampleland citizens.", Fail: "Another citizenship.", Unclear: "Unnamed work rights."},
}.List()

var testKeywords = map[string][]string{"stack": {"rust", "postgres"}}

// memStore is an in-memory DecisionStore. Its queue is inputs as the database gives it:
// gate fails counted and left out, newest first by seen (zero when unset), then job id.
type memStore struct {
	inputs  []store.ScoreInput
	seen    map[int64]time.Time // first seen, by job
	written map[int64][]store.DecisionInsert
	failOn  int64     // InsertDecisions fails for this job
	loads   [][]int64 // every ScoreInputsFor batch
}

func (m *memStore) ScoreQueue(context.Context) (store.ScoreQueue, error) {
	var q store.ScoreQueue
	for _, in := range m.inputs {
		if GateFailed(in.Gates) {
			q.GateFailed++
			continue
		}
		q.Entries = append(q.Entries, store.QueueEntry{JobID: in.JobID, Title: in.Title,
			FirstSeenAt: m.seen[in.JobID], Decided: in.LastInputSHA256 != nil})
	}
	slices.SortStableFunc(q.Entries, func(a, b store.QueueEntry) int {
		if c := b.FirstSeenAt.Compare(a.FirstSeenAt); c != 0 {
			return c
		}
		return cmp.Compare(a.JobID, b.JobID)
	})
	return q, nil
}

func (m *memStore) ScoreInputsFor(_ context.Context, ids []int64) ([]store.ScoreInput, error) {
	m.loads = append(m.loads, slices.Clone(ids))
	var out []store.ScoreInput
	for _, id := range ids {
		for _, in := range m.inputs {
			if in.JobID == id {
				out = append(out, in)
			}
		}
	}
	return out, nil
}

func (m *memStore) InsertDecisions(_ context.Context, ds []store.DecisionInsert) error {
	if m.written == nil {
		m.written = map[int64][]store.DecisionInsert{}
	}
	if len(ds) > 0 && ds[0].JobID == m.failOn {
		return errors.New("disk full")
	}
	for _, d := range ds {
		m.written[d.JobID] = append(m.written[d.JobID], d)
	}
	return nil
}

func answer(choice string, conf float64) ai.Answer {
	probs := map[string]float64{Pass: 0.05, Fail: 0.05, Unclear: 0.05}
	probs[choice] = conf
	return ai.Answer{Choice: choice, Confidence: conf, Probabilities: probs}
}

// allPass answers every criterion pass at 0.9.
func allPass() map[string]ai.Answer {
	m := map[string]ai.Answer{}
	for _, c := range testCriteria {
		m[c.Name] = answer(Pass, 0.9)
	}
	return m
}

func job(id int64, desc string, gateVerdicts ...string) store.ScoreInput {
	in := store.ScoreInput{JobID: id, ObservationID: id * 10, Company: "Northwind Labs", Title: "Senior Rust Engineer",
		Location: "Exampleland", Seniority: "Mid-Senior level", EmploymentType: "Full-time", Description: desc}
	names := []string{"language", "location", "workplace", "work_rights"}
	for i, v := range gateVerdicts {
		in.Gates = append(in.Gates, store.GateResult{Gate: names[i], Verdict: v})
	}
	return in
}

func scorer(m *memStore, d ai.Decider) *Scorer {
	return &Scorer{Store: m, Decider: d, Criteria: testCriteria, EvidenceKeywords: testKeywords, Threshold: 0.7}
}

func TestVerdictThreshold(t *testing.T) {
	for _, tc := range []struct {
		choice string
		conf   float64
		want   string
	}{
		{Pass, 0.9, Pass},
		{Pass, 0.7, Pass}, // at the threshold is enough
		{Pass, 0.69, Unclear},
		{Fail, 0.95, Fail},
		{Fail, 0.5, Unclear},
		{Unclear, 0.99, Unclear},
		{"other", 0.99, Unclear},
	} {
		if got := Verdict(answer(tc.choice, tc.conf), 0.7); got != tc.want {
			t.Errorf("Verdict(%s, %.2f) = %s, want %s", tc.choice, tc.conf, got, tc.want)
		}
	}
}

// TestRunThresholdMapping: with the Fake decider, answers below the threshold are written
// as unclear, the rest as chosen, with probabilities, the hash and the decider's name.
func TestRunThresholdMapping(t *testing.T) {
	ans := allPass()
	ans["stack"] = answer(Fail, 0.95)
	ans["seniority"] = answer(Pass, 0.55)
	m := &memStore{inputs: []store.ScoreInput{job(1, "We build services in Rust and Postgres.", "pass", "pass", "pass", "pass")}}
	fake := &ai.Fake{Answers: ans}
	res, err := scorer(m, fake).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Scored != 1 || res.Calls != 1 || fake.Calls() != 1 {
		t.Fatalf("result %+v, fake calls %d", res, fake.Calls())
	}
	got := map[string]store.DecisionInsert{}
	for _, d := range m.written[1] {
		got[d.Criterion] = d
	}
	if len(got) != 5 {
		t.Fatalf("wrote %d decisions, want 5", len(m.written[1]))
	}
	for crit, want := range map[string]string{"title": Pass, "stack": Fail, "seniority": Unclear, "location": Pass, "work_rights": Pass} {
		d := got[crit]
		if d.Verdict != want {
			t.Errorf("%s: verdict %s, want %s", crit, d.Verdict, want)
		}
		if d.Decider != "fake" || len(d.InputSHA256) != 32 || d.ObservationID != 10 || len(d.Probabilities) != 3 {
			t.Errorf("%s: decision %+v", crit, d)
		}
	}
	if !strings.Contains(got["seniority"].Reason, "below 0.70, so unclear") {
		t.Errorf("seniority reason does not say why it is unclear: %q", got["seniority"].Reason)
	}
}

// TestReasonHasEvidenceLine: the reason quotes the description's keyword line.
func TestReasonHasEvidenceLine(t *testing.T) {
	desc := "About us\nWe build payment services in Rust on Linux.\nWe value calm work."
	m := &memStore{inputs: []store.ScoreInput{job(1, desc, "pass", "pass", "pass", "pass")}}
	if _, err := scorer(m, &ai.Fake{Answers: allPass()}).Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, d := range m.written[1] {
		if d.Criterion != "stack" {
			continue
		}
		want := "stack: Rust is the main language.\n\"We build payment services in Rust on Linux.\"\np(pass)=0.90 p(fail)=0.05 p(unclear)=0.05"
		if d.Reason != want {
			t.Errorf("reason =\n%s\nwant\n%s", d.Reason, want)
		}
		return
	}
	t.Fatal("no stack decision")
}

// TestGateFailedNeverSent: a job with any gate fail is never sent to the decider.
func TestGateFailedNeverSent(t *testing.T) {
	m := &memStore{inputs: []store.ScoreInput{
		job(1, "Rust.", "fail"),
		job(2, "Rust.", "pass", "fail", "pass", "pass"),
		job(3, "Rust.", "pass", "pass", "flag", "fail"),
	}}
	fake := &ai.Fake{Answers: allPass()}
	res, err := scorer(m, fake).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if fake.Calls() != 0 || res.GateFailed != 3 || len(m.written) != 0 {
		t.Fatalf("fake calls %d, result %+v, written %d", fake.Calls(), res, len(m.written))
	}
}

// TestUnchangedNotSentAgain: a job whose request hashes to its last decision's hash costs
// nothing; a changed description is sent again.
func TestUnchangedNotSentAgain(t *testing.T) {
	in := job(1, "Rust.", "pass", "pass", "pass", "pass")
	m := &memStore{inputs: []store.ScoreInput{in}}
	fake := &ai.Fake{Answers: allPass()}
	s := scorer(m, fake)
	if _, err := s.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	in.LastInputSHA256 = m.written[1][0].InputSHA256
	m.inputs = []store.ScoreInput{in}
	res, err := s.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if fake.Calls() != 1 || res.Unchanged != 1 || res.Calls != 0 {
		t.Fatalf("second run: fake calls %d, result %+v", fake.Calls(), res)
	}
	in.Description = "Rust and Postgres."
	m.inputs = []store.ScoreInput{in}
	if res, err := s.Run(context.Background()); err != nil || res.Scored != 1 || fake.Calls() != 2 {
		t.Fatalf("changed run: %+v, %v, fake calls %d", res, err, fake.Calls())
	}
}

// capStore counts like ai_usage, for ai.Capped.
type capStore struct{ calls int }

func (c *capStore) AICallsOn(context.Context, string, time.Time) (int, error) { return c.calls, nil }
func (c *capStore) RecordAIUsage(context.Context, ai.UsageRow) error          { c.calls++; return nil }

// TestSpendCapStopsCleanly: at the cap the run stops with spend_cap and no error, keeping
// what it already wrote.
func TestSpendCapStopsCleanly(t *testing.T) {
	m := &memStore{inputs: []store.ScoreInput{
		job(1, "Rust.", "pass"), job(2, "Rust.", "pass"), job(3, "Rust.", "pass"),
	}}
	fake := &ai.Fake{Answers: allPass()}
	capped := &ai.Capped{Decider: fake, Store: &capStore{}, DailyCalls: 2, Loc: time.UTC}
	res, err := scorer(m, capped).Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.StopReason != StopSpendCap || res.Scored != 2 || res.Calls != 2 || fake.Calls() != 2 || len(m.written) != 2 {
		t.Fatalf("result %+v, fake calls %d, written %d", res, fake.Calls(), len(m.written))
	}
}

// scriptedDecider returns errs in order, then answers.
type scriptedDecider struct {
	errs   []error
	states []string
}

func (*scriptedDecider) Name() string { return "scripted" }

func (s *scriptedDecider) Decide(_ context.Context, state string, _ []ai.Question) (map[string]ai.Answer, ai.Usage, error) {
	s.states = append(s.states, state)
	if len(s.errs) > 0 {
		err := s.errs[0]
		s.errs = s.errs[1:]
		if err != nil {
			return nil, ai.Usage{}, err
		}
	}
	return allPass(), ai.Usage{}, nil
}

// TestTooLongRetriesOnceShorter: ErrTooLong retries once with the description cut to 12k
// characters, and the written hash is the shorter request's, so the next run does not
// send it again.
func TestTooLongRetriesOnceShorter(t *testing.T) {
	long := strings.Repeat("Rust services. ", 1500) // 22,500 characters
	in := job(1, long, "pass")
	m := &memStore{inputs: []store.ScoreInput{in}}
	d := &scriptedDecider{errs: []error{fmt.Errorf("jev: %w", ai.ErrTooLong)}}
	s := scorer(m, d)
	res, err := s.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Scored != 1 || res.Retried != 1 || res.Calls != 2 || len(d.states) != 2 {
		t.Fatalf("result %+v, states %d", res, len(d.states))
	}
	if n := len([]rune(d.states[1])); n >= len([]rune(d.states[0])) || !strings.HasSuffix(d.states[1], firstRunes(long, RetryDescription)) {
		t.Errorf("retry state is %d runes, first %d", n, len([]rune(d.states[0])))
	}
	in.LastInputSHA256 = m.written[1][0].InputSHA256
	m.inputs = []store.ScoreInput{in}
	if res, err := s.Run(context.Background()); err != nil || res.Unchanged != 1 || len(d.states) != 2 {
		t.Fatalf("rerun: %+v, %v, states %d", res, err, len(d.states))
	}

	// Too long twice is one failure, and the run goes on.
	m = &memStore{inputs: []store.ScoreInput{job(1, long, "pass"), job(2, "Rust.", "pass")}}
	d = &scriptedDecider{errs: []error{ai.ErrTooLong, ai.ErrTooLong}}
	res, err = scorer(m, d).Run(context.Background())
	if err != nil || res.Failures != 1 || res.Scored != 1 || res.StopReason != "" {
		t.Fatalf("too long twice: %+v, %v", res, err)
	}
}

// TestUnavailableStops: a provider that stayed unavailable stops the run as throttled.
func TestUnavailableStops(t *testing.T) {
	m := &memStore{inputs: []store.ScoreInput{job(1, "Rust.", "pass"), job(2, "Rust.", "pass")}}
	d := &scriptedDecider{errs: []error{ai.ErrUnavailable}}
	res, err := scorer(m, d).Run(context.Background())
	if err != nil || res.StopReason != StopThrottled || len(d.states) != 1 || len(m.written) != 0 {
		t.Fatalf("%+v, %v, states %d", res, err, len(d.states))
	}
}

// TestStoreErrorIsAnError: a failed write is returned, not counted.
func TestStoreErrorIsAnError(t *testing.T) {
	m := &memStore{inputs: []store.ScoreInput{job(1, "Rust.", "pass")}, failOn: 1}
	if _, err := scorer(m, &ai.Fake{Answers: allPass()}).Run(context.Background()); err == nil {
		t.Fatal("want an error")
	}
}

func TestStateOrderAndCut(t *testing.T) {
	p := Posting{Title: "T", Company: "C", Location: "L", Seniority: "S", EmploymentType: "E", Description: "ąbcdef"}
	if got, want := State(p, 3), "Title: T\nCompany: C\nLocation: L\nSeniority: S\nEmployment type: E\n\nąbc"; got != want {
		t.Errorf("State = %q, want %q", got, want)
	}
}

func TestInputHashCoversEverythingADecisionDependsOn(t *testing.T) {
	qs := Questions(testCriteria)
	base := InputHash("fake", qs, "state", 0.7, testKeywords)
	other := Questions(testCriteria)
	other[1].Options[0].Description = "Go is the main language."
	for name, h := range map[string][]byte{
		"decider":         InputHash("jev-1.13.0", qs, "state", 0.7, testKeywords),
		"question":        InputHash("fake", other, "state", 0.7, testKeywords),
		"state":           InputHash("fake", qs, "state ", 0.7, testKeywords),
		"threshold":       InputHash("fake", qs, "state", 0.75, testKeywords),
		"keyword":         InputHash("fake", qs, "state", 0.7, map[string][]string{"stack": {"rust", "postgresql"}}),
		"keyword order":   InputHash("fake", qs, "state", 0.7, map[string][]string{"stack": {"postgres", "rust"}}),
		"keyword target":  InputHash("fake", qs, "state", 0.7, map[string][]string{"title": {"rust", "postgres"}}),
		"no keywords":     InputHash("fake", qs, "state", 0.7, nil),
		"keyword grouped": InputHash("fake", qs, "state", 0.7, map[string][]string{"stack": {"rust"}, "title": {"postgres"}}),
	} {
		if string(h) == string(base) {
			t.Errorf("changing the %s keeps the hash", name)
		}
	}
	kw := map[string][]string{"title": {"backend"}, "stack": {"rust", "postgres"}}
	a := InputHash("fake", Questions(testCriteria), "state", 0.7, kw)
	for range 20 { // map order must not matter
		if string(InputHash("fake", Questions(testCriteria), "state", 0.7, kw)) != string(a) {
			t.Fatal("the hash is not stable")
		}
	}
}

// TestThresholdOrKeywordChangeRescores: a job decided under one threshold or keyword list
// is asked again when either changes, and not when neither does.
func TestThresholdOrKeywordChangeRescores(t *testing.T) {
	in := job(1, "Rust.", "pass")
	m := &memStore{inputs: []store.ScoreInput{in}}
	fake := &ai.Fake{Answers: allPass()}
	s := scorer(m, fake)
	if _, err := s.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	in.LastInputSHA256 = m.written[1][0].InputSHA256
	m.inputs = []store.ScoreInput{in}
	if res, err := s.Run(context.Background()); err != nil || res.Unchanged != 1 || fake.Calls() != 1 {
		t.Fatalf("unchanged: %+v, %v, calls %d", res, err, fake.Calls())
	}
	s.Threshold = 0.8
	if res, err := s.Run(context.Background()); err != nil || res.Scored != 1 || fake.Calls() != 2 {
		t.Fatalf("threshold changed: %+v, %v, calls %d", res, err, fake.Calls())
	}
	s.Threshold = 0.7
	s.EvidenceKeywords = map[string][]string{"stack": {"rust"}}
	if res, err := s.Run(context.Background()); err != nil || res.Scored != 1 || fake.Calls() != 3 {
		t.Fatalf("keywords changed: %+v, %v, calls %d", res, err, fake.Calls())
	}
}

// TestBilledBadAnswerCountsAndRunGoesOn: a 200 the scorer cannot use counts against the
// cap and is one failure; the run goes on to the next job.
func TestBilledBadAnswerCountsAndRunGoesOn(t *testing.T) {
	m := &memStore{inputs: []store.ScoreInput{job(1, "Rust.", "pass"), job(2, "Rust.", "pass")}}
	d := &scriptedDecider{errs: []error{fmt.Errorf("jev: %w", ai.ErrBadAnswer)}}
	usage := &capStore{}
	capped := &ai.Capped{Decider: d, Store: usage, DailyCalls: 5, Loc: time.UTC}
	res, err := scorer(m, capped).Run(context.Background())
	if err != nil || res.Failures != 1 || res.Scored != 1 || res.Calls != 2 || usage.calls != 2 {
		t.Fatalf("%+v, %v, usage calls %d; want 1 failure, 1 scored, 2 calls counted", res, err, usage.calls)
	}
}

// failingUsage is a usage store whose insert fails.
type failingUsage struct{ capStore }

func (*failingUsage) RecordAIUsage(context.Context, ai.UsageRow) error { return errors.New("db down") }

// TestUsageStoreFailureStopsTheRun: a call that cannot be counted stops the run with an
// error, before any further paid call.
func TestUsageStoreFailureStopsTheRun(t *testing.T) {
	m := &memStore{inputs: []store.ScoreInput{job(1, "Rust.", "pass"), job(2, "Rust.", "pass")}}
	fake := &ai.Fake{Answers: allPass()}
	capped := &ai.Capped{Decider: fake, Store: &failingUsage{}, DailyCalls: 5, Loc: time.UTC}
	res, err := scorer(m, capped).Run(context.Background())
	if !errors.Is(err, ai.ErrUsageStore) {
		t.Fatalf("err = %v, want ErrUsageStore", err)
	}
	if fake.Calls() != 1 || len(m.written) != 0 || res.Scored != 0 {
		t.Fatalf("calls %d, written %d, result %+v; want 1 call and nothing written", fake.Calls(), len(m.written), res)
	}
}

// TestDeadlineStopsWithBudget: once the deadline has passed, Run stops before the next job
// with StopBudget, not an error, and the jobs it did not reach are its backlog. A zero
// deadline is no budget.
func TestDeadlineStopsWithBudget(t *testing.T) {
	inputs := []store.ScoreInput{job(1, "Rust.", "pass"), job(2, "Rust.", "pass"), job(3, "Rust.", "pass")}
	t0 := time.Date(2026, 3, 10, 6, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		deadline        time.Time
		scored, backlog int
		stop            string
	}{
		{time.Time{}, 3, 0, ""},
		{t0.Add(90 * time.Second), 2, 1, StopBudget}, // the clock reads a minute later per job
		{t0, 0, 3, StopBudget},
	} {
		m := &memStore{inputs: inputs}
		fake := &ai.Fake{Answers: allPass()}
		s := scorer(m, fake)
		s.Deadline = c.deadline
		ticks := 0
		s.now = func() time.Time { ticks++; return t0.Add(time.Duration(ticks-1) * time.Minute) }
		res, err := s.Run(context.Background())
		if err != nil {
			t.Fatalf("deadline %v: %v", c.deadline, err)
		}
		if res.Scored != c.scored || res.Backlog != c.backlog || res.StopReason != c.stop || fake.Calls() != c.scored {
			t.Errorf("deadline %v: %+v, calls %d; want scored %d, backlog %d, stop %q", c.deadline, res, fake.Calls(), c.scored, c.backlog, c.stop)
		}
	}
}
