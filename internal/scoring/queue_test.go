package scoring

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/ai"
	"github.com/gremy/webbership-leadhound/internal/candidate"
	"github.com/gremy/webbership-leadhound/internal/store"
)

// loaded is every job id the store was asked to load, in order.
func loaded(m *memStore) []int64 {
	var out []int64
	for _, l := range m.loads {
		out = append(out, l...)
	}
	return out
}

func titled(id int64, title string) store.ScoreInput {
	in := job(id, "Rust.", "pass")
	in.Title = title
	return in
}

// TestQueueNewestFirstKeepBeforeUnsure: kept titles go first, each group newest first.
func TestQueueNewestFirstKeepBeforeUnsure(t *testing.T) {
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	m := &memStore{
		inputs: []store.ScoreInput{titled(1, "Engineer"), titled(2, "Head of Coffee"), titled(3, "Engineer"),
			titled(4, "Head of Tea"), titled(5, "Engineer")},
		seen: map[int64]time.Time{1: base, 2: base.Add(4 * time.Hour), 3: base.Add(2 * time.Hour),
			4: base.Add(time.Hour), 5: base.Add(2 * time.Hour)},
	}
	check := func(title string) candidate.TitleVerdict {
		if strings.Contains(title, "Engineer") {
			return candidate.TitleKeep
		}
		return candidate.TitleUnsure
	}
	s := scorer(m, &ai.Fake{Answers: withTitleCheck(answer(Pass, 0.9))})
	s.TitleCheck = check
	if _, err := s.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Kept: 3 and 5 (same time, by id), then 1; unsure: 2, then 4.
	if got, want := loaded(m), []int64{3, 5, 1, 2, 4}; !slices.Equal(got, want) {
		t.Errorf("order %v, want %v", got, want)
	}
}

// TestGateFailedNeverLoaded: a gate-failed job is counted from the queue and its full
// input is never read.
func TestGateFailedNeverLoaded(t *testing.T) {
	m := &memStore{inputs: []store.ScoreInput{job(1, "Rust.", "fail"), job(2, "Rust.", "pass")}}
	res, err := scorer(m, &ai.Fake{Answers: allPass()}).Run(context.Background())
	if err != nil || res.GateFailed != 1 || res.Scored != 1 {
		t.Fatalf("%+v, %v", res, err)
	}
	if got := loaded(m); !slices.Equal(got, []int64{2}) {
		t.Errorf("loaded %v, want [2]", got)
	}
}

// TestBatchBoundedByTheCap: no batch is larger than Batch, and once the cap stops the run
// no further batch is loaded; what is left is the backlog.
func TestBatchBoundedByTheCap(t *testing.T) {
	m := &memStore{}
	for id := int64(1); id <= 5; id++ {
		m.inputs = append(m.inputs, job(id, "Rust.", "pass"))
	}
	capped := &ai.Capped{Decider: &ai.Fake{Answers: allPass()}, Store: &capStore{}, DailyCalls: 2, Loc: time.UTC}
	s := scorer(m, capped)
	s.Batch = 2
	res, err := s.Run(context.Background())
	if err != nil || res.StopReason != StopSpendCap || res.Scored != 2 {
		t.Fatalf("%+v, %v", res, err)
	}
	if len(m.loads) != 2 || len(m.loads[0]) != 2 || len(m.loads[1]) != 2 {
		t.Errorf("loads %v, want two batches of two", m.loads)
	}
	if res.Backlog != 3 || !strings.Contains(res.Note(), "backlog 3") {
		t.Errorf("backlog %d, note %q", res.Backlog, res.Note())
	}
}

// TestUnchangedBatchesDoNotStopTheRun: batches of unchanged jobs cost no call, so the run
// keeps loading until the queue ends, even with no calls left.
func TestUnchangedBatchesDoNotStopTheRun(t *testing.T) {
	m := &memStore{}
	for id := int64(1); id <= 4; id++ {
		m.inputs = append(m.inputs, job(id, "Rust.", "pass"))
	}
	if _, err := scorer(m, &ai.Fake{Answers: allPass()}).Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	for i := range m.inputs {
		m.inputs[i].LastInputSHA256 = m.written[m.inputs[i].JobID][0].InputSHA256
	}
	m.loads = nil
	capped := &ai.Capped{Decider: &ai.Fake{Answers: allPass()}, Store: &capStore{}, DailyCalls: 0, Loc: time.UTC}
	s := scorer(m, capped)
	s.Batch = 1
	res, err := s.Run(context.Background())
	if err != nil || res.Unchanged != 4 || res.StopReason != "" || len(m.loads) != 4 || res.Backlog != 0 {
		t.Fatalf("%+v, %v, loads %v", res, err, m.loads)
	}
}

func TestBatchFor(t *testing.T) {
	for remaining, want := range map[int]int{0: 10, 1: 11, 300: 340, 10000: 500, -5: 10} {
		if got := BatchFor(remaining); got != want {
			t.Errorf("BatchFor(%d) = %d, want %d", remaining, got, want)
		}
	}
}
