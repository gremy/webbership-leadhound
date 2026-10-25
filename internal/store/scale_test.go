//go:build scale

package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/ats"
	"github.com/gremy/webbership-leadhound/internal/linkedin"
	"github.com/gremy/webbership-leadhound/internal/store"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

// The scale tests run the hot queries on a database the size of production after a
// wide-net night (storetest.Seeded) as the app role, each against a time budget for a
// local M-series machine. They run with make test-scale.

// scaleStore returns the seeded scale database and an app-role Store on it, after checking
// that a plain app connection carries the role's 3-minute statement timeout: every budget
// below is measured under the fences the server has.
func scaleStore(t *testing.T) (*store.Store, storetest.ScaleInfo) {
	t.Helper()
	info := storetest.Seeded(t)
	s, err := store.Open(context.Background(), storetest.ScaleDSN())
	if err != nil {
		storetest.SkipOrFail(t, "skipping scale test: open app store: %v", err)
	}
	t.Cleanup(s.Close)
	if v, err := s.Q().Setting(context.Background(), "statement_timeout"); err != nil || v != "3min" {
		t.Fatalf("app role statement_timeout = %q, %v; want 3min", v, err)
	}
	return s, info
}

// within runs fn, logs how long it took and fails when that is over budget.
func within(t *testing.T, name string, budget time.Duration, fn func() error) {
	t.Helper()
	start := time.Now()
	err := fn()
	took := time.Since(start)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	t.Logf("%s: %v (budget %v)", name, took.Round(time.Millisecond), budget)
	if took > budget {
		t.Errorf("%s took %v, over its budget of %v", name, took.Round(time.Millisecond), budget)
	}
}

// TestScaleScoreQueue: the light first pass over every job worth scoring, run as the scorer
// runs it, under a 10-minute statement timeout; every open, current, new or shortlisted job
// is counted exactly once.
func TestScaleScoreQueue(t *testing.T) {
	s, _ := scaleStore(t)
	ctx := context.Background()
	var q store.ScoreQueue
	within(t, "ScoreQueue", 20*time.Second, func() error {
		return s.WithStatementTimeout(ctx, 10*time.Minute, func(qs store.Queries) error {
			var err error
			q, err = qs.ScoreQueue(ctx)
			return err
		})
	})
	var want int
	if err := s.Pool().QueryRow(ctx,
		`SELECT count(*) FROM job j
		 JOIN LATERAL (SELECT closed FROM job_observation WHERE job_id = j.id
		               ORDER BY observed_at DESC, id DESC LIMIT 1) o ON true
		 LEFT JOIN LATERAL (SELECT status FROM status_event WHERE job_id = j.id
		                    ORDER BY at DESC, id DESC LIMIT 1) s ON true
		 WHERE j.listing_status = 'open' AND NOT o.closed AND coalesce(s.status, 'new') IN ('new', 'shortlisted')`).Scan(&want); err != nil {
		t.Fatal(err)
	}
	if got := len(q.Entries) + q.GateFailed + q.AwaitingDetail; got != want || len(q.Entries) == 0 {
		t.Errorf("queue %d entries + %d gate failed + %d awaiting detail = %d; want %d", len(q.Entries), q.GateFailed, q.AwaitingDetail, got, want)
	}
}

// TestScaleScoreInputsFor: the full inputs of one scoring batch of 500 queued jobs.
func TestScaleScoreInputsFor(t *testing.T) {
	s, _ := scaleStore(t)
	ctx := context.Background()
	rows, err := s.Pool().Query(ctx,
		`SELECT j.id FROM job j JOIN job_observation o ON o.job_id = j.id
		 JOIN http_fetch f ON f.id = o.fetch_id
		 WHERE j.listing_status = 'open' AND NOT o.closed AND f.kind <> 'board'
		   AND NOT EXISTS (SELECT 1 FROM gate_result g WHERE g.observation_id = o.id AND g.verdict = 'fail')
		 ORDER BY j.first_seen_at DESC, j.id LIMIT 500`)
	if err != nil {
		t.Fatal(err)
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil || len(ids) != 500 {
		t.Fatalf("batch ids = %d, %v", len(ids), err)
	}
	var in []store.ScoreInput
	within(t, "ScoreInputsFor(500)", 2*time.Second, func() error {
		in, err = s.Q().ScoreInputsFor(ctx, ids)
		return err
	})
	if len(in) == 0 || in[0].Description == "" || len(in[0].Gates) == 0 {
		t.Errorf("score inputs = %d, first %+v", len(in), firstInput(in))
	}
}

func firstInput(in []store.ScoreInput) store.ScoreInput {
	if len(in) == 0 {
		return store.ScoreInput{}
	}
	in[0].Description = in[0].Description[:min(40, len(in[0].Description))]
	return in[0]
}

// TestScaleFunnelOn: the dashboard's funnel for the wide-net night and for today, under the
// role's default statement timeout and temp-file cap.
func TestScaleFunnelOn(t *testing.T) {
	s, info := scaleStore(t)
	ctx := context.Background()
	for _, day := range []struct {
		name string
		at   time.Time
	}{{"wide-net day", info.WideDay}, {"today", info.Day}} {
		var f store.Funnel
		within(t, "FunnelOn "+day.name, 5*time.Second, func() error {
			var err error
			f, err = s.Q().FunnelOn(ctx, info.Loc, day.at)
			return err
		})
		if len(f.NewBySource) == 0 || f.Gated == 0 {
			t.Errorf("funnel on the %s = %+v", day.name, f)
		}
	}
}

// TestScaleCountJobsFirstSeen: the brief's per-source count of the wide-net night's new jobs,
// for every source the brief lists.
func TestScaleCountJobsFirstSeen(t *testing.T) {
	s, info := scaleStore(t)
	ctx := context.Background()
	sources := []string{linkedin.Source, linkedin.SourceFixture}
	for _, a := range []string{ats.Greenhouse, ats.Lever, ats.Ashby, ats.SmartRecruiters, ats.Teamtailor, ats.Personio} {
		sources = append(sources, a, ats.SourceFixture(a))
	}
	total := 0
	within(t, "CountJobsFirstSeen x14", 2*time.Second, func() error {
		for _, src := range sources {
			n, err := s.Q().CountJobsFirstSeen(ctx, src, info.Loc, info.WideDay)
			if err != nil {
				return err
			}
			total += n
		}
		return nil
	})
	if total < info.Jobs/2 {
		t.Errorf("jobs first seen on the wide-net day = %d of %d", total, info.Jobs)
	}
}

// TestScaleTrackList: the console's Tracked page for its three filters, capped as the
// console caps them.
func TestScaleTrackList(t *testing.T) {
	s, info := scaleStore(t)
	ctx := context.Background()
	for _, tc := range []struct {
		name     string
		status   string
		limit    int
		minTotal int
	}{
		{"active", "", 0, 1},
		{"all", "all", 500, info.Jobs},
		{"new", "new", 500, info.Jobs / 2},
	} {
		var rows []store.TrackRow
		var total int
		within(t, "TrackList "+tc.name, time.Second, func() error {
			var err error
			rows, total, err = s.Q().TrackList(ctx, tc.status, tc.limit)
			return err
		})
		if total < tc.minTotal || len(rows) == 0 || (tc.limit > 0 && len(rows) != tc.limit) {
			t.Errorf("TrackList %s = %d rows of %d", tc.name, len(rows), total)
		}
	}
}

// errRollback makes InTx roll back a write a test only measures.
var errRollback = errors.New("roll back")

// TestScaleCollectorLookups: the ATS collector's per-board lookups on one source's ids, and
// the expiry of one board, rolled back.
func TestScaleCollectorLookups(t *testing.T) {
	s, _ := scaleStore(t)
	ctx := context.Background()
	src := ats.SmartRecruiters
	rows, err := s.Pool().Query(ctx,
		`SELECT source_job_id FROM job WHERE source = $1 ORDER BY id LIMIT 1000`, src)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil || len(ids) != 1000 {
		t.Fatalf("source ids = %d, %v", len(ids), err)
	}
	within(t, "JobsWithoutObservation(1000)", time.Second, func() error {
		_, err := s.Q().JobsWithoutObservation(ctx, src, ids)
		return err
	})
	var waiting []string
	within(t, "JobsAwaitingDetail(1000)", time.Second, func() error {
		var err error
		waiting, err = s.Q().JobsAwaitingDetail(ctx, src, ids)
		return err
	})
	if len(waiting) == 0 {
		t.Errorf("no %s job awaits its detail", src)
	}

	// The board with the most open jobs of the source, all but one of them listed.
	var board string
	var listed []string
	if err := s.Pool().QueryRow(ctx,
		`SELECT split_part(source_job_id, '/', 1) AS b, array_agg(source_job_id ORDER BY id)
		 FROM job WHERE source = $1 AND listing_status = 'open'
		 GROUP BY b ORDER BY count(*) DESC, b LIMIT 1`, src).Scan(&board, &listed); err != nil {
		t.Fatal(err)
	}
	var expired int64
	within(t, "ExpireMissing(one board)", time.Second, func() error {
		err := s.InTx(ctx, func(q store.Queries) error {
			var err error
			if expired, err = q.ExpireMissing(ctx, src, board, listed[1:]); err != nil {
				return err
			}
			return errRollback
		})
		if errors.Is(err, errRollback) {
			return nil
		}
		return err
	})
	if expired != 1 {
		t.Errorf("ExpireMissing on board %s expired %d, want 1", board, expired)
	}
}
