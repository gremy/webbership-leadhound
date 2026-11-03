package main

import (
	"slices"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/scoring"
	"github.com/gremy/webbership-leadhound/internal/store"
)

// queueRow is a queue row with this text, as a collector left it.
func queueRow(id int64, company, title, location, description string) store.NextJob {
	return store.NextJob{JobID: id, Company: company, Title: title, Location: location, Description: description,
		Status: "new", ListingStatus: store.ListingOpen, LatestActor: "system"}
}

func queueTestRules() []store.Rule {
	now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	return []store.Rule{
		{ID: 1, Kind: "skip", Field: "title", Pattern: `\bkeeper\b`, Reason: "another trade"},
		{ID: 2, Kind: "skip", Field: "company", Pattern: `^staffing`, Reason: "an agency"},
		{ID: 3, Kind: "skip", Field: "location", Pattern: `atlantis`, Reason: "too far"},
		{ID: 4, Kind: "skip", Field: "description", Pattern: `passport stamp`, Reason: "needs a stamp"},
		{ID: 5, Kind: "flag", Field: "title", Pattern: `contract`, Reason: "only a flag"},
		{ID: 6, Kind: "skip", Field: "title", Pattern: `off`, Reason: "disabled", DisabledAt: &now},
		{ID: 7, Kind: "skip", Field: "title", Pattern: `broken(`, Reason: "does not compile"},
		{ID: 8, Kind: "skip", Field: "title", Pattern: `engineer`, Reason: "never first"},
	}
}

func queueTestRows() []store.NextJob {
	return []store.NextJob{
		queueRow(1, "Northwind", "Go Developer", "Remote", "We write Go."),
		queueRow(2, "Northwind", "Lighthouse Keeper", "Remote", ""),
		queueRow(3, "Staffing Example", "Go Developer", "Remote", ""),
		queueRow(4, "Northwind", "Go Developer", "Atlantis", ""),
		queueRow(5, "Northwind", "Go Developer", "Remote", "Needs a PASSPORT STAMP."),
		queueRow(6, "Northwind", "Contract Go Developer", "Remote", ""),
		queueRow(7, "Northwind", "Off Season Developer", "Remote", ""),
		queueRow(8, "Northwind", "broken( Developer", "Remote", ""),
		queueRow(9, "Northwind", "Engineer and Keeper", "Remote", ""),
		queueRow(10, "Northwind", "Go Engineer", "Remote", ""),
	}
}

// TestQueueAgreesWithRanker: the contract between the two readers of the skip rules. Driven with
// the same rules and text, the ranker leaves out for a rule exactly what the queue hides, under the
// same first rule. A rule's stored gate result can outlive its disabling, so a disabled rule still
// keeps a job from the brief until a newer observation replaces it: that drift is accepted, and
// the plan's Checkpoint 2 tells the operator about it.
func TestQueueAgreesWithRanker(t *testing.T) {
	rules := queueTestRules()
	rows := queueTestRows()
	inputs := make([]store.RankInput, len(rows))
	for i, j := range rows {
		inputs[i] = store.RankInput{JobID: j.JobID, Title: j.Title, Company: j.Company, Location: j.Location, Description: j.Description,
			ListingStatus: j.ListingStatus, Status: j.Status}
	}
	candidates := scoring.CandidatesFrom(inputs, rules)
	_, hidden := splitQueue(rows, nil, newQueueRules(rules))
	var wantHidden []int64
	reasons := map[int64]string{}
	for _, r := range rules {
		reasons[r.ID] = r.Reason
	}
	wantReasons := map[int64]string{}
	for _, c := range candidates {
		if c.SkipRuleID != nil {
			wantHidden = append(wantHidden, c.JobID)
			wantReasons[c.JobID] = reasons[*c.SkipRuleID]
		}
	}
	var got []int64
	for _, h := range hidden {
		got = append(got, h.JobID)
		if h.Reason != wantReasons[h.JobID] {
			t.Errorf("job %d: reason %q, the ranker's rule says %q", h.JobID, h.Reason, wantReasons[h.JobID])
		}
	}
	if !slices.Equal(got, wantHidden) {
		t.Errorf("the queue hides %v, the ranker skips by rule %v", got, wantHidden)
	}
	if want := []int64{2, 3, 4, 5, 9, 10}; !slices.Equal(got, want) {
		t.Errorf("hidden = %v, want %v", got, want)
	}
	ranked := scoring.Rank(candidates, time.UTC, scoring.Caps{Apply: 50, YourCall: 50})
	if n := ranked.Skipped[scoring.SkipRule]; n != len(got) {
		t.Errorf("the ranker counts %d skipped by rule, the queue %d", n, len(got))
	}
}

// TestSplitQueueInvariant: every row of the briefed read lands in exactly one list, and of the
// never-briefed read only a rule-matched row lands at all, whichever way the actor falls.
func TestSplitQueueInvariant(t *testing.T) {
	rules := newQueueRules(queueTestRules())
	for _, actor := range []string{"system", "gremy"} {
		briefed, never := queueTestRows(), queueTestRows()
		for i := range briefed {
			briefed[i].LatestActor = actor
			never[i].LatestActor = actor
			never[i].JobID += 100
		}
		visible, hidden := splitQueue(briefed, never, rules)
		seen := map[int64]int{}
		for _, j := range visible {
			seen[j.JobID]++
		}
		for _, h := range hidden {
			seen[h.JobID]++
			if h.Briefed != (h.JobID < 100) {
				t.Errorf("actor %s: job %d briefed = %v", actor, h.JobID, h.Briefed)
			}
		}
		for _, j := range briefed {
			if seen[j.JobID] != 1 {
				t.Errorf("actor %s: briefed job %d in %d lists", actor, j.JobID, seen[j.JobID])
			}
		}
		for _, j := range never {
			want := 0
			if _, ok := rules.match(j); ok {
				want = 1
			}
			if seen[j.JobID] != want {
				t.Errorf("actor %s: never-briefed job %d in %d lists, want %d", actor, j.JobID, seen[j.JobID], want)
			}
		}
		if actor == "gremy" && len(hidden) != 0 {
			t.Errorf("rescued jobs hidden: %+v", hidden)
		}
		// A rescued never-briefed job follows the briefed ones.
		if actor == "gremy" && !slices.IsSortedFunc(visible, func(a, b store.NextJob) int { return int(a.JobID - b.JobID) }) {
			t.Errorf("order = %+v", visible)
		}
	}
}
