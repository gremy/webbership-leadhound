//go:build integration

package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/ai"
	"github.com/gremy/webbership-leadhound/internal/fetch"
	"github.com/gremy/webbership-leadhound/internal/store"
)

// consoleSeed stores one job of source first seen at at, an observation with one gate result
// per verdict, and one decision per criterion, all created at at. It returns the job and
// observation ids.
func consoleSeed(t *testing.T, q store.Queries, source, sourceJobID string, at time.Time, verdicts ...string) (jobID, obsID int64) {
	t.Helper()
	ctx := context.Background()
	jobID, fetchID := seedSourced(t, q, source, sourceJobID, fetch.KindDetail)
	obsID, err := q.InsertObservation(ctx, store.ObservationInsert{JobID: jobID, FetchID: fetchID, DerivedBy: "test@1", Title: "Go Engineer"})
	if err != nil {
		t.Fatalf("InsertObservation: %v", err)
	}
	for _, v := range verdicts {
		if _, err := q.InsertGateResult(ctx, store.GateResultInsert{JobID: jobID, ObservationID: obsID, Gate: "language",
			Verdict: v, Reason: "test", DerivedBy: "gates@1"}); err != nil {
			t.Fatalf("InsertGateResult: %v", err)
		}
	}
	for _, d := range decisions(jobID, obsID, "pass", "h"+sourceJobID) {
		if _, err := q.InsertDecision(ctx, d); err != nil {
			t.Fatalf("InsertDecision: %v", err)
		}
	}
	ownerExec(t, "UPDATE job SET first_seen_at = $2 WHERE id = $1", jobID, at)
	ownerExec(t, "UPDATE gate_result SET created_at = $2 WHERE job_id = $1", jobID, at)
	ownerExec(t, "UPDATE decision SET created_at = $2 WHERE job_id = $1", jobID, at)
	return jobID, obsID
}

// TestConsoleColdStart: on an empty database the console's queries return zeros, an empty
// (non-nil) source map and no runs, never an error.
func TestConsoleColdStart(t *testing.T) {
	ctx := context.Background()
	q := appStore(t).Q()
	loc := bucharest(t)
	day := time.Date(2026, 10, 25, 0, 0, 0, 0, loc)

	f, err := q.FunnelOn(ctx, loc, day)
	if err != nil {
		t.Fatalf("FunnelOn: %v", err)
	}
	if f.NewBySource == nil || len(f.NewBySource) != 0 || f.Gated != 0 || f.GateFailed != 0 || f.Scored != 0 {
		t.Fatalf("empty FunnelOn = %+v, want zeros and an empty map", f)
	}
	runs, err := q.RecentRuns(ctx, 10)
	if err != nil {
		t.Fatalf("RecentRuns: %v", err)
	}
	if runs == nil || len(runs) != 0 {
		t.Fatalf("empty RecentRuns = %#v, want an empty slice", runs)
	}
	u, err := q.AIUsageOn(ctx, day)
	if err != nil {
		t.Fatalf("AIUsageOn: %v", err)
	}
	if u != (store.AIUsage{}) {
		t.Fatalf("empty AIUsageOn = %+v, want zeros", u)
	}
	if _, err := q.FunnelOn(ctx, nil, day); err == nil {
		t.Fatal("FunnelOn with a nil location succeeded; want an error")
	}
}

// TestConsoleFunnelAcrossDSTEnd: Bucharest's clocks go back an hour on 25 October 2026, so
// that day runs 25 hours. Rows at 23:59 and 00:01 local either side of each midnight land on
// the local day they belong to, the late one on the 25th included.
func TestConsoleFunnelAcrossDSTEnd(t *testing.T) {
	ctx := context.Background()
	q := appStore(t).Q()
	loc := bucharest(t)
	at := func(d, h, m int) time.Time { return time.Date(2026, 10, d, h, m, 0, 0, loc) }

	// 23:59 on the 25th is 21:59 UTC, past 24 hours after that day's midnight (21:00 UTC on the 24th).
	if got := at(25, 23, 59).Sub(at(25, 0, 0)); got <= 24*time.Hour {
		t.Fatalf("25 October in Bucharest: 23:59 is %v after midnight; the test needs the 25-hour day", got)
	}

	a, aObs := consoleSeed(t, q, "linkedin", "2000000001", at(24, 23, 59), "pass")
	consoleSeed(t, q, "linkedin", "2000000002", at(25, 0, 1), "pass", "fail", "fail")
	consoleSeed(t, q, "greenhouse", "2000000003", at(25, 23, 59), "pass", "flag")
	consoleSeed(t, q, "greenhouse", "2000000004", at(26, 0, 1), "fail")
	// Job a, first seen on the 24th, is gated again (and fails) just after midnight.
	id, err := q.InsertGateResult(ctx, store.GateResultInsert{JobID: a, ObservationID: aObs, Gate: "rule",
		Verdict: "fail", Reason: "test", DerivedBy: "gates@1"})
	if err != nil {
		t.Fatalf("InsertGateResult: %v", err)
	}
	ownerExec(t, "UPDATE gate_result SET created_at = $2 WHERE id = $1", id, at(25, 0, 1))

	cases := []struct {
		name string
		day  time.Time
		want store.Funnel
	}{
		{"the 24th", at(24, 12, 0), store.Funnel{NewBySource: map[string]int{"linkedin": 1}, Gated: 1, GateFailed: 0, Scored: 1}},
		{"the 25th, from midnight", at(25, 0, 0), store.Funnel{NewBySource: map[string]int{"linkedin": 1, "greenhouse": 1}, Gated: 3, GateFailed: 2, Scored: 2}},
		{"the 25th, late", at(25, 23, 30), store.Funnel{NewBySource: map[string]int{"linkedin": 1, "greenhouse": 1}, Gated: 3, GateFailed: 2, Scored: 2}},
		{"the 26th", at(26, 9, 0), store.Funnel{NewBySource: map[string]int{"greenhouse": 1}, Gated: 1, GateFailed: 1, Scored: 1}},
		{"the 27th", at(27, 9, 0), store.Funnel{NewBySource: map[string]int{}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := q.FunnelOn(ctx, loc, tc.day)
			if err != nil {
				t.Fatalf("FunnelOn: %v", err)
			}
			if got.Gated != tc.want.Gated || got.GateFailed != tc.want.GateFailed || got.Scored != tc.want.Scored ||
				len(got.NewBySource) != len(tc.want.NewBySource) {
				t.Fatalf("FunnelOn = %+v, want %+v", got, tc.want)
			}
			for src, n := range tc.want.NewBySource {
				if got.NewBySource[src] != n {
					t.Fatalf("FunnelOn = %+v, want %+v", got, tc.want)
				}
			}
			if got.NewBySource == nil {
				t.Fatal("FunnelOn NewBySource is nil, want an empty map")
			}
		})
	}
}

// TestConsoleAIUsageOn: one day's calls are summed per purpose (jobs and leads have their
// own caps) and its input tokens over both, over every provider and model, by the local date
// ai_usage stores, the 25-hour day included. A row recorded without a purpose is jobs.
func TestConsoleAIUsageOn(t *testing.T) {
	ctx := context.Background()
	q := appStore(t).Q()
	loc := bucharest(t)
	day := func(d int) time.Time { return time.Date(2026, 10, d, 0, 0, 0, 0, loc) }

	for _, r := range []ai.UsageRow{
		{Provider: "typesafe", Model: "jev-1.13.0", Day: day(24), Calls: 1, InputTokens: 100},
		{Provider: "typesafe", Model: "jev-1.13.0", Day: day(25), Calls: 1, InputTokens: 300},
		{Provider: "typesafe", Model: "jev-1.14.0", Day: day(25), Calls: 2, InputTokens: 250, Purpose: ai.PurposeJobs},
		{Provider: "typesafe", Model: "jev-1.14.0", Day: day(25), Calls: 5, InputTokens: 40, Purpose: ai.PurposeLeads},
		{Provider: "fake", Model: "fake", Day: day(25), Calls: 1},
		{Provider: "typesafe", Model: "jev-1.13.0", Day: day(26), Calls: 1, InputTokens: 10},
		{Provider: "typesafe", Model: "jev-1.14.0", Day: day(26), Calls: 3, InputTokens: 7, Purpose: ai.PurposeLeads},
	} {
		if err := q.RecordAIUsage(ctx, r); err != nil {
			t.Fatalf("RecordAIUsage %+v: %v", r, err)
		}
	}
	cases := []struct {
		name string
		day  time.Time
		want store.AIUsage
	}{
		{"the 24th", day(24), store.AIUsage{JobsCalls: 1, InputTokens: 100}},
		{"the 25th", day(25), store.AIUsage{JobsCalls: 4, LeadsCalls: 5, InputTokens: 590}},
		{"the 25th, 23:59 local", time.Date(2026, 10, 25, 23, 59, 0, 0, loc), store.AIUsage{JobsCalls: 4, LeadsCalls: 5, InputTokens: 590}},
		{"the 26th", day(26), store.AIUsage{JobsCalls: 1, LeadsCalls: 3, InputTokens: 17}},
		{"the 27th", day(27), store.AIUsage{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := q.AIUsageOn(ctx, tc.day)
			if err != nil {
				t.Fatalf("AIUsageOn: %v", err)
			}
			if got != tc.want {
				t.Fatalf("AIUsageOn = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestConsoleRecentRuns: runs come newest first by started_at, then by id; the limit is
// clamped to 1–200.
func TestConsoleRecentRuns(t *testing.T) {
	ctx := context.Background()
	q := appStore(t).Q()
	base := time.Date(2026, 10, 24, 6, 0, 0, 0, time.UTC)

	var ids []int64
	for i, src := range []string{"linkedin", "greenhouse", "lever", "ashby"} {
		id, err := q.StartRun(ctx, "collect", src)
		if err != nil {
			t.Fatalf("StartRun: %v", err)
		}
		ids = append(ids, id)
		// Runs 2 and 3 start at the same moment; the higher id comes first.
		offset := time.Duration(i) * time.Hour
		if i == 2 {
			offset = time.Hour
		}
		ownerExec(t, "UPDATE run SET started_at = $2 WHERE id = $1", id, base.Add(offset))
	}
	if err := q.FinishRun(ctx, ids[0], store.RunResult{Requests: 3, Failures: 1, StopReason: "budget", Note: "spent"}); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}

	got, err := q.RecentRuns(ctx, 10)
	if err != nil {
		t.Fatalf("RecentRuns: %v", err)
	}
	want := []int64{ids[3], ids[2], ids[1], ids[0]}
	if len(got) != len(want) {
		t.Fatalf("RecentRuns = %d runs, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].ID != want[i] {
			t.Fatalf("RecentRuns order = %v, want %v", runIDs(got), want)
		}
	}
	last := got[3]
	if last.Source != "linkedin" || last.FinishedAt == nil || last.Requests != 3 || last.Failures != 1 ||
		last.StopReason != "budget" || last.Note != "spent" || !last.StartedAt.Equal(base) {
		t.Fatalf("oldest run = %+v", last)
	}

	if got, err := q.RecentRuns(ctx, 2); err != nil || len(got) != 2 || got[0].ID != ids[3] {
		t.Fatalf("RecentRuns(2) = %v, %v; want the two newest", runIDs(got), err)
	}
	for _, limit := range []int{0, -5} {
		if got, err := q.RecentRuns(ctx, limit); err != nil || len(got) != 1 || got[0].ID != ids[3] {
			t.Fatalf("RecentRuns(%d) = %v, %v; want the newest run only", limit, runIDs(got), err)
		}
	}

	ownerExec(t, "INSERT INTO run (command) SELECT 'collect' FROM generate_series(1, 205)")
	for _, limit := range []int{200, 201, 1000} {
		if got, err := q.RecentRuns(ctx, limit); err != nil || len(got) != 200 {
			t.Fatalf("RecentRuns(%d) = %d runs, %v; want 200", limit, len(got), err)
		}
	}
}

func runIDs(runs []store.Run) []int64 {
	out := make([]int64, len(runs))
	for i, r := range runs {
		out[i] = r.ID
	}
	return out
}

// TestConsoleLockJobUnknown: locking a job that does not exist is ErrNotFound.
func TestConsoleLockJobUnknown(t *testing.T) {
	ctx := context.Background()
	s := appStore(t)
	err := s.InTx(ctx, func(q store.Queries) error { return q.LockJob(ctx, 999) })
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("LockJob unknown = %v, want ErrNotFound", err)
	}
}

// TestConsoleLockJobWaits: a second transaction's LockJob on the same job waits until the
// first transaction commits.
func TestConsoleLockJobWaits(t *testing.T) {
	ctx := context.Background()
	s := appStore(t)
	jobID, _ := seedJob(t, s.Q(), "2000000010")

	locked := make(chan struct{})
	release := make(chan struct{})
	firstDone := make(chan error, 1)
	secondDone := make(chan error, 1)
	released := false
	defer func() {
		if !released {
			close(release)
		}
	}()

	go func() {
		firstDone <- s.InTx(ctx, func(q store.Queries) error {
			if err := q.LockJob(ctx, jobID); err != nil {
				close(locked)
				return err
			}
			close(locked)
			<-release
			return nil
		})
	}()
	<-locked

	go func() {
		secondDone <- s.InTx(ctx, func(q store.Queries) error { return q.LockJob(ctx, jobID) })
	}()

	select {
	case err := <-secondDone:
		t.Fatalf("second LockJob returned (%v) while the first transaction held the lock", err)
	case err := <-firstDone:
		t.Fatalf("first transaction ended early: %v", err)
	case <-time.After(300 * time.Millisecond):
	}

	close(release)
	released = true
	for name, ch := range map[string]chan error{"first": firstDone, "second": secondDone} {
		select {
		case err := <-ch:
			if err != nil {
				t.Fatalf("%s transaction: %v", name, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s transaction still waiting 5s after the first committed", name)
		}
	}
}
