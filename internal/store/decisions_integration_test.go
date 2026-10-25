//go:build integration

package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/fetch"
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

// TestScoreQueue: the light pass returns only scoreable jobs without a failing gate,
// newest first, says which are decided on their current observation, and counts the gate
// fails; ScoreInputsFor loads one batch in the order asked, dropping what is no longer
// scoreable.
func TestScoreQueue(t *testing.T) {
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
	observeFor(t, q, shortlisted, f6, "Rust Developer", false, "language", "pass")
	if _, err := q.InsertStatusEvent(ctx, shortlisted, "shortlisted", "", "system", nil); err != nil {
		t.Fatal(err)
	}
	mk("1000000007") // no observation yet
	// The open job was first seen earlier than the shortlisted one.
	ownerExec(t, `UPDATE job SET first_seen_at = now() - interval '3 hours' WHERE id = $1`, open)
	ownerExec(t, `UPDATE job SET first_seen_at = now() - interval '1 hour' WHERE id = $1`, shortlisted)

	queue, err := q.ScoreQueue(ctx)
	if err != nil {
		t.Fatalf("ScoreQueue: %v", err)
	}
	if queue.GateFailed != 1 {
		t.Errorf("gate failed = %d, want 1", queue.GateFailed)
	}
	var gotIDs []int64
	for _, e := range queue.Entries {
		gotIDs = append(gotIDs, e.JobID)
	}
	if want := []int64{shortlisted, open}; !slices.Equal(gotIDs, want) {
		t.Fatalf("queue %v, want %v (newest first, gate fail left out)", gotIDs, want)
	}
	if e := queue.Entries[0]; e.Title != "Rust Developer" || e.Decided {
		t.Errorf("shortlisted entry %+v", e)
	}
	if e := queue.Entries[1]; e.Title != "Go Engineer" || !e.Decided || !e.FirstSeenAt.Before(queue.Entries[0].FirstSeenAt) {
		t.Errorf("open entry %+v", e)
	}

	got, err := q.ScoreInputsFor(ctx, []int64{shortlisted, open, expired})
	if err != nil {
		t.Fatalf("ScoreInputsFor: %v", err)
	}
	gotIDs = nil
	for _, in := range got {
		gotIDs = append(gotIDs, in.JobID)
	}
	if want := []int64{shortlisted, open}; !slices.Equal(gotIDs, want) {
		t.Fatalf("inputs %v, want %v", gotIDs, want)
	}
	o := got[1]
	if o.ObservationID != obsOpen || o.Title != "Go Engineer" || o.Company != "Initech" || o.Description != "We write Go." ||
		string(o.LastInputSHA256) != "hash-open" || len(o.Gates) != 2 || o.Gates[1].Snippet != "workplace words" {
		t.Errorf("open job input %+v", o)
	}
	if got[0].LastInputSHA256 != nil {
		t.Errorf("never decided job has a hash %q", got[0].LastInputSHA256)
	}
	if none, err := q.ScoreInputsFor(ctx, nil); err != nil || len(none) != 0 {
		t.Errorf("empty batch: %v, %v", none, err)
	}
}

// queryRankRows collects the streamed pass's rows as a caller holding only queries reads
// them (Queries.ForEachRankRow), for a small fixture only.
func queryRankRows(t *testing.T, q store.Queries, opt store.RankStreamOptions) []store.RankRow {
	t.Helper()
	var out []store.RankRow
	if err := q.ForEachRankRow(context.Background(), opt, func(r store.RankRow) error {
		out = append(out, r)
		return nil
	}); err != nil {
		t.Fatalf("ForEachRankRow: %v", err)
	}
	return out
}

// TestRankRowQueries: the pass over queries reads every job with an observation, its
// current observation's gates and its latest decisions, and leaves the connection's
// statement timeout as it was.
func TestRankRowQueries(t *testing.T) {
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

	got := queryRankRows(t, q, store.RankStreamOptions{AllowFake: true, DescriptionFor: store.DescAll})
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
	if ra.Description != "We write Go." {
		t.Errorf("job a description = %q", ra.Description)
	}

	// Inside a transaction (a savepoint) the pass works too, and its timeout ends with it.
	if err := s.InTx(ctx, func(tq store.Queries) error {
		if n := len(queryRankRows(t, tq, store.RankStreamOptions{})); n != 2 {
			t.Errorf("in a transaction: %d rows, want 2", n)
		}
		v, err := tq.Setting(ctx, "statement_timeout")
		if err != nil || v == "10min" {
			t.Errorf("statement_timeout after the pass = %q, %v", v, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// fn's error stops the pass and comes back as it is.
	stop := errors.New("stop")
	if err := q.ForEachRankRow(ctx, store.RankStreamOptions{}, func(store.RankRow) error { return stop }); !errors.Is(err, stop) {
		t.Errorf("ForEachRankRow = %v, want fn's error", err)
	}
}

// TestRankRowIgnoresFakeDecisions: the fake decider's decisions are read only on an
// explicit AllowFake; otherwise the latest real decision per criterion is returned, or none.
func TestRankRowIgnoresFakeDecisions(t *testing.T) {
	ctx := context.Background()
	s := appStore(t)
	q := s.Q()
	a, f1 := seedJob(t, q, "1000000001")
	obsA := observeFor(t, q, a, f1, "Go Engineer", false, "language", "pass")
	if err := s.InsertDecisions(ctx, decisions(a, obsA, "pass", "h")); err != nil { // decider fake
		t.Fatal(err)
	}
	read := func(allow bool) []store.DecisionVerdictRow {
		t.Helper()
		got := queryRankRows(t, q, store.RankStreamOptions{AllowFake: allow})
		if len(got) != 1 {
			t.Fatalf("ForEachRankRow: %d rows", len(got))
		}
		return got[0].Decisions
	}
	if got := read(false); len(got) != 0 {
		t.Fatalf("fake decisions read without AllowFake: %d", len(got))
	}
	if got := read(true); len(got) != 5 || got[0].Verdict != "pass" {
		t.Fatalf("AllowFake: %+v", got)
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
	if got := read(false); len(got) != 5 || got[0].Verdict != "fail" {
		t.Fatalf("without AllowFake: %+v", got)
	}
	if got := read(true); len(got) != 5 || got[0].Verdict != "pass" {
		t.Fatalf("with AllowFake: %+v", got)
	}
}

// TestRankRowSeenAt: SeenAt is when the job was last seen, first seen or observed
// again, so a job seen yesterday and observed again today reads as today's.
func TestRankRowSeenAt(t *testing.T) {
	q := appStore(t).Q()
	yesterday := time.Now().Add(-24 * time.Hour).Truncate(time.Second)
	a, f1 := seedJob(t, q, "1000000001")
	observeFor(t, q, a, f1, "Go Engineer", false, "language", "fail")
	b, f2 := seedJob(t, q, "1000000002")
	obsB := observeFor(t, q, b, f2, "Go Engineer", false, "language", "fail")
	ownerExec(t, "UPDATE job SET first_seen_at = $2 WHERE id = ANY($1)", []int64{a, b}, yesterday)
	ownerExec(t, "UPDATE job_observation SET observed_at = $2 WHERE job_id = ANY($1)", []int64{a, b}, yesterday)
	// b is observed again today.
	obsB2 := observeFor(t, q, b, f2, "Go Engineer", false, "language", "fail")
	if obsB2 == obsB {
		t.Fatal("no second observation")
	}

	got := queryRankRows(t, q, store.RankStreamOptions{})
	if len(got) != 2 {
		t.Fatalf("ForEachRankRow: %d rows", len(got))
	}
	if !got[0].SeenAt.Equal(yesterday) {
		t.Errorf("job a seen at %v, want %v", got[0].SeenAt, yesterday)
	}
	if d := time.Since(got[1].SeenAt); d < 0 || d > time.Hour {
		t.Errorf("job b seen at %v, want now", got[1].SeenAt)
	}
}

// seedSourced stores one job of source and a fetch row of kind for it, as seedJob does for
// linkedin, and returns the job and the fetch.
func seedSourced(t *testing.T, q store.Queries, source, sourceJobID string, kind fetch.Kind) (jobID, fetchID int64) {
	t.Helper()
	ctx := context.Background()
	runID, err := q.StartRun(ctx, "collect", source)
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	rec := store.NewFetchRecorder(q, runID, source)
	if err := rec.Record(ctx, fetch.Record{Kind: kind, URL: "https://api.example/" + source + "/" + sourceJobID, Status: 200, Body: []byte("x")}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	companyID, err := q.UpsertCompany(ctx, "Initech", "", "")
	if err != nil {
		t.Fatalf("UpsertCompany: %v", err)
	}
	jobID, err = q.UpsertJob(ctx, store.JobUpsert{Source: source, SourceJobID: sourceJobID, CompanyID: companyID,
		Title: "Go Engineer", URL: "https://jobs.example/" + sourceJobID})
	if err != nil {
		t.Fatalf("UpsertJob: %v", err)
	}
	return jobID, rec.LastID()
}

// TestScoreQueueHoldsJobsAwaitingDetail: a SmartRecruiters job (live or dry run) whose
// current observation came from a board fetch has no description yet: it is left out of
// the queue and the score inputs and counted as awaiting detail; once an observation from
// its detail fetch arrives it is queued. A Greenhouse job read from a board fetch is queued
// as before.
func TestScoreQueueHoldsJobsAwaitingDetail(t *testing.T) {
	ctx := context.Background()
	s := appStore(t)
	q := s.Q()

	live, liveBoard := seedSourced(t, q, "smartrecruiters", "examplerobotics/1", fetch.KindBoard)
	observeFor(t, q, live, liveBoard, "Go Engineer", false, "language", "pass")
	dry, dryBoard := seedSourced(t, q, "smartrecruiters-fixture", "examplerobotics/2", fetch.KindBoard)
	observeFor(t, q, dry, dryBoard, "Go Engineer", false, "language", "pass")
	gh, ghBoard := seedSourced(t, q, "greenhouse", "examplecorp/3", fetch.KindBoard)
	observeFor(t, q, gh, ghBoard, "Go Engineer", false, "language", "pass")

	ids := func(queue store.ScoreQueue) []int64 {
		var out []int64
		for _, e := range queue.Entries {
			out = append(out, e.JobID)
		}
		slices.Sort(out)
		return out
	}
	inputs := func() []int64 {
		got, err := q.ScoreInputsFor(ctx, []int64{live, dry, gh})
		if err != nil {
			t.Fatalf("ScoreInputsFor: %v", err)
		}
		var out []int64
		for _, in := range got {
			out = append(out, in.JobID)
		}
		slices.Sort(out)
		return out
	}

	queue, err := q.ScoreQueue(ctx)
	if err != nil {
		t.Fatalf("ScoreQueue: %v", err)
	}
	if got := ids(queue); !slices.Equal(got, []int64{gh}) || queue.AwaitingDetail != 2 || queue.GateFailed != 0 {
		t.Errorf("queue %v, awaiting %d, gate failed %d; want [%d], 2, 0", got, queue.AwaitingDetail, queue.GateFailed, gh)
	}
	if got := inputs(); !slices.Equal(got, []int64{gh}) {
		t.Errorf("inputs %v, want [%d]", got, gh)
	}

	// The live job's detail arrives: an observation cited to a detail fetch.
	_, detailFetch := seedSourced(t, q, "smartrecruiters", "examplerobotics/1", fetch.KindDetail)
	observeFor(t, q, live, detailFetch, "Go Engineer", false, "language", "pass")
	queue, err = q.ScoreQueue(ctx)
	if err != nil {
		t.Fatalf("ScoreQueue: %v", err)
	}
	if got, want := ids(queue), []int64{live, gh}; !slices.Equal(got, want) || queue.AwaitingDetail != 1 {
		t.Errorf("after the detail: queue %v, awaiting %d; want %v, 1", got, queue.AwaitingDetail, want)
	}
	if got, want := inputs(), []int64{live, gh}; !slices.Equal(got, want) {
		t.Errorf("after the detail: inputs %v, want %v", got, want)
	}
}

// TestScoreQueueListGateFailNotAwaitingDetail: a SmartRecruiters job whose current
// observation came from a board fetch and failed a gate other than the language gate is
// never asked for a detail, so it counts as gate-failed, not as awaiting its detail. One
// whose only fail is the language gate (read from the title while the description is
// empty) still awaits its detail.
func TestScoreQueueListGateFailNotAwaitingDetail(t *testing.T) {
	ctx := context.Background()
	s := appStore(t)
	q := s.Q()

	located, locatedBoard := seedSourced(t, q, "smartrecruiters", "examplerobotics/1", fetch.KindBoard)
	observeFor(t, q, located, locatedBoard, "Go Engineer", false, "language", "pass", "location", "fail")
	titled, titledBoard := seedSourced(t, q, "smartrecruiters-fixture", "examplerobotics/2", fetch.KindBoard)
	observeFor(t, q, titled, titledBoard, "Go Engineer", false, "language", "fail")
	waiting, waitingBoard := seedSourced(t, q, "smartrecruiters", "examplerobotics/3", fetch.KindBoard)
	observeFor(t, q, waiting, waitingBoard, "Go Engineer", false, "language", "pass", "workplace", "flag")

	queue, err := q.ScoreQueue(ctx)
	if err != nil {
		t.Fatalf("ScoreQueue: %v", err)
	}
	if len(queue.Entries) != 0 || queue.GateFailed != 1 || queue.AwaitingDetail != 2 {
		t.Errorf("queue: %d entries, %d gate failed, %d awaiting detail; want 0, 1, 2", len(queue.Entries), queue.GateFailed, queue.AwaitingDetail)
	}
}
