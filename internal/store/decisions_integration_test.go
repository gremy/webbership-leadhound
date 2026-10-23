//go:build integration

package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"

	"github.com/gremy/webbership-leadhound/internal/store"
)

// observeFor writes an observation of jobID with one gate result per verdict pair
// (gate, verdict) and returns its id.
func observeFor(t *testing.T, q store.Queries, jobID, fetchID int64, title string, closed bool, gv ...string) int64 {
	t.Helper()
	ctx := context.Background()
	obsID, err := q.InsertObservation(ctx, store.ObservationInsert{JobID: jobID, FetchID: fetchID, DerivedBy: "test@1",
		Title: title, DescriptionClean: "We write Go.", ApplyType: "offsite", Closed: closed})
	if err != nil {
		t.Fatalf("InsertObservation: %v", err)
	}
	for i := 0; i+1 < len(gv); i += 2 {
		if _, err := q.InsertGateResult(ctx, store.GateResultInsert{JobID: jobID, ObservationID: obsID, Gate: gv[i],
			Verdict: gv[i+1], Reason: "test", Snippet: gv[i] + " words", DerivedBy: "gates@1"}); err != nil {
			t.Fatalf("InsertGateResult: %v", err)
		}
	}
	return obsID
}

func decisions(jobID, obsID int64, verdict string, hash string) []store.DecisionInsert {
	var out []store.DecisionInsert
	for _, c := range []string{"title", "stack", "seniority", "location", "work_rights"} {
		out = append(out, store.DecisionInsert{JobID: jobID, ObservationID: obsID, Decider: "fake", Criterion: c,
			Verdict: verdict, Confidence: 0.9, Probabilities: map[string]float64{"pass": 0.9, "fail": 0.1},
			Reason: c + ": test", InputSHA256: []byte(hash)})
	}
	return out
}

func TestDecisionsLatestPerCriterion(t *testing.T) {
	ctx := context.Background()
	s := appStore(t)
	q := s.Q()
	jobID, fetchID := seedJob(t, q, "1000000001")
	obs1 := observeFor(t, q, jobID, fetchID, "Go Engineer", false, "language", "pass")

	if _, err := q.LatestInputSHA256(ctx, jobID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("no decision yet: %v", err)
	}
	if err := s.InsertDecisions(ctx, decisions(jobID, obs1, "pass", "hash-1")); err != nil {
		t.Fatalf("InsertDecisions: %v", err)
	}
	// A later observation does not hide the decisions made on the earlier one.
	obs2 := observeFor(t, q, jobID, fetchID, "Go Engineer", false, "language", "pass")
	got, err := q.LatestDecisions(ctx, jobID)
	if err != nil || len(got) != 5 || got[0].ObservationID != obs1 {
		t.Fatalf("LatestDecisions: %d, %v", len(got), err)
	}
	// A newer decision for one criterion replaces only that one.
	if _, err := q.InsertDecision(ctx, store.DecisionInsert{JobID: jobID, ObservationID: obs2, Decider: "fake",
		Criterion: "stack", Verdict: "fail", Confidence: 0.8, Reason: "stack: newer", InputSHA256: []byte("hash-2")}); err != nil {
		t.Fatal(err)
	}
	got, err = q.LatestDecisions(ctx, jobID)
	if err != nil || len(got) != 5 {
		t.Fatalf("LatestDecisions: %d, %v", len(got), err)
	}
	var names []string
	for _, d := range got {
		names = append(names, d.Criterion)
		if d.Criterion == "stack" && (d.Verdict != "fail" || d.ObservationID != obs2 || string(d.Probabilities) != "{}") {
			t.Errorf("stack decision %+v", d)
		}
		if d.Criterion == "title" {
			var p map[string]float64
			if err := json.Unmarshal(d.Probabilities, &p); err != nil || p["pass"] != 0.9 {
				t.Errorf("probabilities %s: %v", d.Probabilities, err)
			}
		}
	}
	if !slices.IsSorted(names) {
		t.Errorf("criteria not ordered: %v", names)
	}
	if h, err := q.LatestInputSHA256(ctx, jobID); err != nil || string(h) != "hash-2" {
		t.Errorf("LatestInputSHA256 = %q, %v", h, err)
	}
}

// TestInsertDecisionsAllOrNothing: one bad row rolls the whole job's decisions back.
func TestInsertDecisionsAllOrNothing(t *testing.T) {
	ctx := context.Background()
	s := appStore(t)
	q := s.Q()
	jobID, fetchID := seedJob(t, q, "1000000001")
	obs := observeFor(t, q, jobID, fetchID, "Go Engineer", false)
	ds := decisions(jobID, obs, "pass", "h")
	ds[4].Verdict = "maybe" // the CHECK refuses it
	if err := s.InsertDecisions(ctx, ds); err == nil {
		t.Fatal("want an error")
	}
	if got, _ := q.LatestDecisions(ctx, jobID); len(got) != 0 {
		t.Fatalf("%d decisions kept after a failed write", len(got))
	}
}

func TestScoreInputs(t *testing.T) {
	ctx := context.Background()
	s := appStore(t)
	q := s.Q()
	mk := func(id string) (int64, int64) { return seedJob(t, q, id) }

	open, f1 := mk("1000000001")
	observeFor(t, q, open, f1, "Old title", false, "language", "pass")
	obsOpen := observeFor(t, q, open, f1, "Go Engineer", false, "language", "pass", "workplace", "flag")
	if err := s.InsertDecisions(ctx, decisions(open, obsOpen, "pass", "hash-open")); err != nil {
		t.Fatal(err)
	}
	gateFail, f2 := mk("1000000002")
	observeFor(t, q, gateFail, f2, "Go Engineer", false, "language", "fail")
	expired, f3 := mk("1000000003")
	observeFor(t, q, expired, f3, "Go Engineer", false, "language", "pass")
	ownerExec(t, `UPDATE job SET listing_status = 'expired' WHERE id = $1`, expired)
	closedObs, f4 := mk("1000000004")
	observeFor(t, q, closedObs, f4, "Go Engineer", true, "language", "pass")
	applied, f5 := mk("1000000005")
	observeFor(t, q, applied, f5, "Go Engineer", false, "language", "pass")
	if _, err := q.InsertStatusEvent(ctx, applied, "new", "", "system", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := q.InsertStatusEvent(ctx, applied, "applied", "", "gremy", nil); err != nil {
		t.Fatal(err)
	}
	shortlisted, f6 := mk("1000000006")
	observeFor(t, q, shortlisted, f6, "Go Engineer", false, "language", "pass")
	if _, err := q.InsertStatusEvent(ctx, shortlisted, "shortlisted", "", "system", nil); err != nil {
		t.Fatal(err)
	}
	mk("1000000007") // no observation yet

	got, err := q.ScoreInputs(ctx)
	if err != nil {
		t.Fatalf("ScoreInputs: %v", err)
	}
	var gotIDs []int64
	for _, in := range got {
		gotIDs = append(gotIDs, in.JobID)
	}
	// The gate-failed job is returned: the scorer skips it, so the rule lives in one place.
	if want := []int64{open, gateFail, shortlisted}; !slices.Equal(gotIDs, want) {
		t.Fatalf("jobs %v, want %v", gotIDs, want)
	}
	first := got[0]
	if first.ObservationID != obsOpen || first.Title != "Go Engineer" || first.Company != "Initech" ||
		string(first.LastInputSHA256) != "hash-open" || len(first.Gates) != 2 || first.Gates[1].Snippet != "workplace words" {
		t.Errorf("open job input %+v", first)
	}
	if got[2].LastInputSHA256 != nil {
		t.Errorf("never decided job has a hash %q", got[2].LastInputSHA256)
	}
}

func TestRankInputs(t *testing.T) {
	ctx := context.Background()
	s := appStore(t)
	q := s.Q()
	a, f1 := seedJob(t, q, "1000000001")
	obsA := observeFor(t, q, a, f1, "Go Engineer", false, "language", "pass")
	if err := s.InsertDecisions(ctx, decisions(a, obsA, "pass", "h")); err != nil {
		t.Fatal(err)
	}
	// A later observation: its gates are the current ones; the decisions stay.
	observeFor(t, q, a, f1, "Go Engineer II", false, "language", "pass", "workplace", "flag")
	b, f2 := seedJob(t, q, "1000000002")
	observeFor(t, q, b, f2, "Go Engineer", true, "language", "fail")
	if _, err := q.InsertStatusEvent(ctx, b, "skipped", "", "gremy", nil); err != nil {
		t.Fatal(err)
	}
	seedJob(t, q, "1000000003") // no observation: not ranked

	got, err := q.RankInputs(ctx, store.RankOptions{AllowFake: true})
	if err != nil {
		t.Fatalf("RankInputs: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("%d inputs, want 2", len(got))
	}
	ra, rb := got[0], got[1]
	if ra.JobID != a || ra.Status != "new" || ra.ListingStatus != "open" || ra.Title != "Go Engineer II" ||
		ra.Company != "Initech" || ra.ApplyType != "offsite" || len(ra.Gates) != 2 || len(ra.Decisions) != 5 {
		t.Errorf("job a: %+v", ra)
	}
	if rb.JobID != b || rb.Status != "skipped" || !rb.Closed || len(rb.Gates) != 1 || len(rb.Decisions) != 0 {
		t.Errorf("job b: %+v", rb)
	}
}

// TestRankInputsIgnoresFakeDecisions: the fake decider's decisions are read only on an
// explicit AllowFake; otherwise the latest real decision per criterion is returned, or none.
func TestRankInputsIgnoresFakeDecisions(t *testing.T) {
	ctx := context.Background()
	s := appStore(t)
	q := s.Q()
	a, f1 := seedJob(t, q, "1000000001")
	obsA := observeFor(t, q, a, f1, "Go Engineer", false, "language", "pass")
	if err := s.InsertDecisions(ctx, decisions(a, obsA, "pass", "h")); err != nil { // decider fake
		t.Fatal(err)
	}
	read := func(allow bool) []store.Decision {
		t.Helper()
		got, err := q.RankInputs(ctx, store.RankOptions{AllowFake: allow})
		if err != nil || len(got) != 1 {
			t.Fatalf("RankInputs: %d, %v", len(got), err)
		}
		return got[0].Decisions
	}
	if got := read(false); len(got) != 0 {
		t.Fatalf("fake decisions read without AllowFake: %d", len(got))
	}
	if got := read(true); len(got) != 5 || got[0].Decider != "fake" {
		t.Fatalf("AllowFake: %d decisions", len(got))
	}

	// A real decision, then a newer fake one: without AllowFake the real one stands.
	real := decisions(a, obsA, "fail", "h2")
	for i := range real {
		real[i].Decider = "jev-1.13.0"
	}
	if err := s.InsertDecisions(ctx, real); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertDecisions(ctx, decisions(a, obsA, "pass", "h3")); err != nil {
		t.Fatal(err)
	}
	if got := read(false); len(got) != 5 || got[0].Decider != "jev-1.13.0" || got[0].Verdict != "fail" {
		t.Fatalf("without AllowFake: %+v", got)
	}
	if got := read(true); len(got) != 5 || got[0].Decider != "fake" {
		t.Fatalf("with AllowFake: %+v", got)
	}
}
