//go:build integration

package linkedin_test

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/fetch"
	"github.com/gremy/webbership-leadhound/internal/gates"
	"github.com/gremy/webbership-leadhound/internal/linkedin"
	"github.com/gremy/webbership-leadhound/internal/store"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

// collected fills the database from the shipped fixtures and returns the store and the
// job id of each LinkedIn job ID asked for.
func collected(t *testing.T, sourceIDs ...string) (*store.Store, map[string]int64) {
	t.Helper()
	ctx := context.Background()
	s := openStore(t)
	var logs bytes.Buffer
	if _, err := collector(t, s, fixtures(t, testdata), time.Now, &logs).Run(ctx, demo); err != nil {
		t.Fatalf("collect: %v", err)
	}
	ids := map[string]int64{}
	for _, sid := range sourceIDs {
		j, err := s.Q().JobBySourceID(ctx, linkedin.Source, sid)
		if err != nil {
			t.Fatalf("job %s: %v", sid, err)
		}
		ids[sid] = j.ID
	}
	return s, ids
}

func rechecker(t *testing.T, s *store.Store, rt http.RoundTripper, now time.Time, extra ...fetch.Option) *linkedin.Rechecker {
	return &linkedin.Rechecker{
		Store:     s,
		NewClient: newClientFunc(t, rt, extra...),
		Now:       func() time.Time { return now },
		Loc:       bucharest,
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		Gates:     gates.CandidateGates{Languages: []string{"en"}},
	}
}

func listing(t *testing.T, jobID int64) string {
	t.Helper()
	var st string
	if err := storetest.OwnerConn(t).QueryRow(context.Background(),
		`SELECT listing_status FROM job WHERE id = $1`, jobID).Scan(&st); err != nil {
		t.Fatal(err)
	}
	return st
}

func status(t *testing.T, s *store.Store, jobID int64) string {
	t.Helper()
	st, err := s.Q().CurrentStatus(context.Background(), jobID)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// later is when the collected jobs are due a recheck.
func later() time.Time { return time.Now().Add(linkedin.RecheckAfter + time.Hour) }

// TestRecheckClosedExpires: the closed banner expires every listing; a new or shortlisted
// job is withdrawn, an applied one keeps its status (the grid's "recheck finds closed").
func TestRecheckClosedExpires(t *testing.T) {
	ctx := context.Background()
	s, ids := collected(t, "1000000004", "1000000005", "1000000007")
	newJob, shortlisted, applied := ids["1000000004"], ids["1000000005"], ids["1000000007"]
	q := s.Q()
	if _, err := q.InsertStatusEvent(ctx, shortlisted, "shortlisted", "", "system", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := q.InsertStatusEvent(ctx, applied, "applied", "", "gremy", nil); err != nil {
		t.Fatal(err)
	}
	obsBefore := count(t, `SELECT count(*) FROM job_observation`)

	closed := fixtures(t, fixtureDir(t, map[string]string{"*jobPosting/*": "detail_closed.html"}))
	res, err := rechecker(t, s, closed, later()).Run(ctx, []int64{newJob, shortlisted, applied})
	if err != nil {
		t.Fatal(err)
	}
	if res.Due != 3 || res.Expired != 3 || res.Withdrawn != 2 || res.Open != 0 || res.Unreachable != 0 || res.StopReason != "" {
		t.Fatalf("result = %+v", res)
	}
	if closed.Hits() != 3 || res.Requests != 3 {
		t.Errorf("requests = %d (hits %d), want 3", res.Requests, closed.Hits())
	}
	for _, id := range []int64{newJob, shortlisted, applied} {
		if got := listing(t, id); got != store.ListingExpired {
			t.Errorf("job %d listing = %s, want expired", id, got)
		}
	}
	for id, want := range map[int64]string{newJob: "withdrawn", shortlisted: "withdrawn", applied: "applied"} {
		if got := status(t, s, id); got != want {
			t.Errorf("job %d status = %s, want %s", id, got, want)
		}
	}
	if got := count(t, `SELECT count(*) FROM status_event WHERE status = 'withdrawn' AND actor = 'system' AND reason = $1`, linkedin.ClosedReason); got != 2 {
		t.Errorf("withdrawn events = %d, want 2", got)
	}
	// The closed page is still an observation, with its gate results.
	if got := count(t, `SELECT count(*) FROM job_observation`); got != obsBefore+3 {
		t.Errorf("observations = %d, want %d", got, obsBefore+3)
	}
	if got := count(t, `SELECT count(*) FROM job_observation o WHERE closed AND NOT EXISTS (SELECT 1 FROM gate_result g WHERE g.observation_id = o.id)`); got != 0 {
		t.Errorf("closed observations without gates = %d", got)
	}
	run := latestRun(t, linkedin.CommandRecheck)
	if run.Requests != 3 || run.StopReason != "" || run.Note != "3 due, 0 open, 3 expired, 0 unreachable" {
		t.Errorf("run = %+v", run)
	}

	// An expired listing is never due again.
	res, err = rechecker(t, s, closed, later()).Run(ctx, []int64{newJob, shortlisted, applied})
	if err != nil || res.Due != 0 || res.RunID != 0 {
		t.Fatalf("second run = %+v, %v", res, err)
	}
}

// TestRecheckTransportErrorThenOpen: no answer leaves the job unreachable (never closed,
// status unchanged); the next recheck tries it again however recent, and a readable open
// page makes it open with a new observation.
func TestRecheckTransportErrorThenOpen(t *testing.T) {
	ctx := context.Background()
	s, ids := collected(t, "1000000004")
	job := ids["1000000004"]
	obsBefore := count(t, `SELECT count(*) FROM job_observation WHERE job_id = $1`, job)

	res, err := rechecker(t, s, errTransport{}, later()).Run(ctx, []int64{job})
	if err != nil {
		t.Fatal(err)
	}
	if res.Unreachable != 1 || res.Expired != 0 || len(res.Observed) != 0 || res.Requests != 1 {
		t.Fatalf("result = %+v", res)
	}
	if got := listing(t, job); got != store.ListingUnreachable {
		t.Fatalf("listing = %s, want unreachable", got)
	}
	if got := status(t, s, job); got != "new" {
		t.Errorf("status = %s, want new", got)
	}
	if got := count(t, `SELECT count(*) FROM job_observation WHERE job_id = $1`, job); got != obsBefore {
		t.Errorf("observations = %d, want %d", got, obsBefore)
	}

	// Tomorrow (here: right away, since unreachable is always due) a 200 reopens it.
	ft := fixtures(t, testdata)
	res, err = rechecker(t, s, ft, time.Now()).Run(ctx, []int64{job})
	if err != nil {
		t.Fatal(err)
	}
	if res.Due != 1 || res.Open != 1 || !slices.Equal(res.Observed, []int64{job}) || ft.HitsFor("jobPosting/1000000004") != 1 {
		t.Fatalf("result = %+v", res)
	}
	if got := listing(t, job); got != store.ListingOpen {
		t.Fatalf("listing = %s, want open", got)
	}
	if got := count(t, `SELECT count(*) FROM job_observation WHERE job_id = $1`, job); got != obsBefore+1 {
		t.Errorf("observations = %d, want %d", got, obsBefore+1)
	}
	gs, err := s.Q().LatestGateResults(ctx, job)
	if err != nil || len(gs) == 0 {
		t.Fatalf("new observation has no gates: %v %v", gs, err)
	}
	// A recheck writes no second `new` event.
	if got := count(t, `SELECT count(*) FROM status_event WHERE job_id = $1`, job); got != 1 {
		t.Errorf("status events = %d, want 1", got)
	}
}

// TestRecheckNotDue: a listing checked within RecheckAfter is not fetched, and no run is
// recorded.
func TestRecheckNotDue(t *testing.T) {
	ctx := context.Background()
	s, ids := collected(t, "1000000004")
	ft := fixtures(t, testdata)
	res, err := rechecker(t, s, ft, time.Now()).Run(ctx, []int64{ids["1000000004"]})
	if err != nil || res.Due != 0 || res.RunID != 0 || ft.Hits() != 0 {
		t.Fatalf("result = %+v, %v, hits %d", res, err, ft.Hits())
	}
	if got := count(t, `SELECT count(*) FROM run WHERE command = 'recheck'`); got != 0 {
		t.Errorf("recheck runs = %d, want 0", got)
	}
}

// TestRecheckPausedToday: once LinkedIn throttled any command today, the recheck requests
// nothing; the due jobs are left out until the next check, never closed.
func TestRecheckPausedToday(t *testing.T) {
	ctx := context.Background()
	s, ids := collected(t, "1000000004")
	job := ids["1000000004"]
	fetchesBefore := count(t, `SELECT count(*) FROM http_fetch`)
	if _, err := storetest.OwnerConn(t).Exec(ctx,
		`INSERT INTO run (command, source, finished_at, stop_reason) VALUES ('collect', 'linkedin', $1, 'throttled')`, later()); err != nil {
		t.Fatal(err)
	}
	ft := fixtures(t, testdata)
	res, err := rechecker(t, s, ft, later()).Run(ctx, []int64{job})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Paused || res.StopReason != "throttled" || res.Unreachable != 1 || ft.Hits() != 0 {
		t.Fatalf("result = %+v, hits %d", res, ft.Hits())
	}
	if got := count(t, `SELECT count(*) FROM http_fetch`); got != fetchesBefore {
		t.Errorf("fetch rows = %d, want %d", got, fetchesBefore)
	}
	if got := listing(t, job); got != store.ListingUnreachable {
		t.Errorf("listing = %s, want unreachable", got)
	}
	run := latestRun(t, linkedin.CommandRecheck)
	if run.StopReason != "throttled" || run.Requests != 0 {
		t.Errorf("run = %+v", run)
	}
}

// TestRecheckBreakerOpen: once the detail budget is spent, the rest are unreachable, not
// closed.
func TestRecheckBreakerOpen(t *testing.T) {
	ctx := context.Background()
	s, ids := collected(t, "1000000004", "1000000005")
	a, b := ids["1000000004"], ids["1000000005"]
	ft := fixtures(t, testdata)
	res, err := rechecker(t, s, ft, later(), fetch.WithBreaker(fetch.NewBreaker(fetch.Budgets{fetch.KindDetail: 1}))).Run(ctx, []int64{a, b})
	if err != nil {
		t.Fatal(err)
	}
	if res.Open != 1 || res.Unreachable != 1 || res.StopReason != "budget" || ft.Hits() != 1 {
		t.Fatalf("result = %+v, hits %d", res, ft.Hits())
	}
	if listing(t, a) != store.ListingOpen || listing(t, b) != store.ListingUnreachable {
		t.Errorf("listings = %s, %s", listing(t, a), listing(t, b))
	}
}
