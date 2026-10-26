//go:build scale

package main

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/time/rate"

	"github.com/gremy/webbership-leadhound/internal/brief"
	"github.com/gremy/webbership-leadhound/internal/config"
	"github.com/gremy/webbership-leadhound/internal/console"
	"github.com/gremy/webbership-leadhound/internal/fetch"
	"github.com/gremy/webbership-leadhound/internal/leadconfig"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/leadstore/leadstoretest"
	"github.com/gremy/webbership-leadhound/internal/linkedin"
	"github.com/gremy/webbership-leadhound/internal/store"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

// The brief's scale tests run the brief's whole build and the score run's summary on the
// production-sized scale database (storetest.Seeded) as the app role, each against a time
// budget and a ceiling on the Go heap. They run with make test-scale.

// maxScaleHeap is the most Go heap the brief's build or the score summary may hold at once:
// memory follows the brief's caps, not the store.
const maxScaleHeap = 80 << 20

// scaleBriefStore opens the seeded scale database as the app role and removes, when the
// test ends, any run row the test wrote, so the reused seed stays as it was.
func scaleBriefStore(t *testing.T) (*store.Store, storetest.ScaleInfo) {
	t.Helper()
	info := storetest.Seeded(t)
	ctx := context.Background()
	s, err := store.Open(ctx, storetest.ScaleDSN())
	if err != nil {
		storetest.SkipOrFail(t, "skipping scale test: open app store: %v", err)
	}
	t.Cleanup(s.Close)
	var maxRun int64
	if err := s.Pool().QueryRow(ctx, `SELECT coalesce(max(id), 0) FROM run`).Scan(&maxRun); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		conn, err := pgx.Connect(context.Background(), storetest.ScaleMigrateDSN())
		if err != nil {
			t.Errorf("cleanup: %v", err)
			return
		}
		defer func() { _ = conn.Close(context.Background()) }()
		if _, err := conn.Exec(context.Background(), `DELETE FROM run WHERE id > $1`, maxRun); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	})
	return s, info
}

// measure runs fn while a goroutine samples the Go heap every 10 ms, after a GC baseline,
// and returns the highest HeapAlloc seen and how long fn took.
func measure(t *testing.T, fn func() error) (peak uint64, took time.Duration) {
	t.Helper()
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	stop, done := make(chan struct{}), make(chan uint64)
	go func() {
		high := ms.HeapAlloc
		tick := time.NewTicker(10 * time.Millisecond)
		defer tick.Stop()
		for {
			var m runtime.MemStats
			runtime.ReadMemStats(&m)
			high = max(high, m.HeapAlloc)
			select {
			case <-stop:
				done <- high
				return
			case <-tick.C:
			}
		}
	}()
	start := time.Now()
	err := fn()
	took = time.Since(start)
	close(stop)
	peak = <-done
	if err != nil {
		t.Fatal(err)
	}
	return peak, took
}

// budgets fails the test when the heap or the time is over its budget.
func budgets(t *testing.T, name string, peak uint64, took, maxTook time.Duration) {
	t.Helper()
	t.Logf("%s: %v (budget %v), peak heap %.1f MB (budget %d MB)", name, took.Round(time.Millisecond), maxTook,
		float64(peak)/(1<<20), maxScaleHeap>>20)
	if peak > maxScaleHeap {
		t.Errorf("%s: peak heap %.1f MB, over its budget of %d MB", name, float64(peak)/(1<<20), maxScaleHeap>>20)
	}
	if took > maxTook {
		t.Errorf("%s took %v, over its budget of %v", name, took.Round(time.Millisecond), maxTook)
	}
}

// TestScaleBriefBuild: the brief's whole build as runBrief runs it, without the network
// (the recheck before it, leaveOutStale, buildInput), with the seed's title and description
// rules active. The recheck runs under the fixture source, so nothing in the seed is due and
// the seed is left as it was.
func TestScaleBriefBuild(t *testing.T) {
	s, info := scaleBriefStore(t)
	prof, err := loadProfileDir(examplesDir)
	if err != nil {
		t.Fatal(err)
	}
	fx := t.TempDir()
	if err := os.WriteFile(filepath.Join(fx, fetch.ManifestFile), []byte(`{"*nothing-here*": "page.html"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fx, "page.html"), []byte("<html></html>"), 0o600); err != nil {
		t.Fatal(err)
	}
	ft, err := fetch.NewFixtureTransport(fx)
	if err != nil {
		t.Fatal(err)
	}
	newClient := func(rec fetch.Recorder) *fetch.Client {
		return fetch.New(fetch.WithFixtures(ft), fetch.WithRecorder(rec), fetch.WithProfile(fetch.LinkedInJobs),
			fetch.WithLimiter(rate.NewLimiter(rate.Inf, 1)), fetch.WithJitter(0))
	}
	ctx := context.Background()
	day := info.Day.Add(9 * time.Hour)
	var cfg config.Config
	var in brief.Input
	peak, took := measure(t, func() error {
		out := recheckListed(ctx, s, cfg, prof, info.Loc, day, false, newClient, linkedin.SourceFixture)
		if out.Err != nil {
			return out.Err
		}
		if _, err := leaveOutStale(ctx, s.Q(), prof, info.Loc, day, false, linkedin.SourceFixture); err != nil {
			return err
		}
		var err error
		in, err = buildInput(ctx, s.Q(), prof, info.Loc, day, false, false)
		return err
	})
	budgets(t, "brief build", peak, took, 45*time.Second)

	if len(in.Apply) == 0 {
		t.Error("the brief lists nothing in Apply")
	}
	if in.RunnerUp == nil || in.RunnerUp.ID != info.RunnerUpJobID {
		t.Errorf("runner-up = %+v, want job %d", in.RunnerUp, info.RunnerUpJobID)
	}
	ids := in.JobIDs()
	if slices.Contains(ids, info.NotSeenJobID) {
		t.Errorf("job %d, not seen on its board, is listed", info.NotSeenJobID)
	}
	// Every Apply job is decided pass on every criterion by the real decider.
	var allPass []int64
	rows, err := s.Pool().Query(ctx, `SELECT job_id FROM decision WHERE decider = $1 GROUP BY job_id
		HAVING bool_and(verdict = 'pass') AND count(*) = 5`, config.DefaultJevModel)
	if err != nil {
		t.Fatal(err)
	}
	if allPass, err = pgx.CollectRows(rows, pgx.RowTo[int64]); err != nil {
		t.Fatal(err)
	}
	for _, j := range in.Apply {
		if !slices.Contains(allPass, j.ID) {
			t.Errorf("Apply lists job %d, which is not all-pass", j.ID)
		}
	}
	notSeen := 0
	for _, src := range in.Sources {
		notSeen += src.NotSeen
	}
	if notSeen == 0 {
		t.Error("no source counts a job not seen on its board")
	}
}

// TestScaleScoreSummary: the score run's summary ranking over every job, every description
// read, so its skip split is exact.
func TestScaleScoreSummary(t *testing.T) {
	s, info := scaleBriefStore(t)
	prof, err := loadProfileDir(examplesDir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	var total int
	peak, took := measure(t, func() error {
		r, err := scoreSummary(ctx, s, prof, info.Loc, false)
		total = r.ApplyTotal + r.YourCallTotal
		for _, n := range r.Skipped {
			total += n
		}
		return err
	})
	budgets(t, "score summary", peak, took, 60*time.Second)
	if total != info.Jobs {
		t.Errorf("summary counts %d jobs, want %d", total, info.Jobs)
	}
}

// TestScaleConsoleDashboard: the console's snapshot build within the brief's budgets, and
// the dashboard within half a second in every state: cold (building, 0.6 s), fresh, and old
// while a new one builds. The builds end with the test.
func TestScaleConsoleDashboard(t *testing.T) {
	s, info := scaleBriefStore(t)
	prof, err := loadProfileDir(examplesDir)
	if err != nil {
		t.Fatal(err)
	}
	var cfg config.Config
	day := info.Day.Add(9 * time.Hour)
	base, stop := context.WithCancel(context.Background())
	b := newBackend(base, s, prof, info.Loc, cfg)
	t.Cleanup(func() { stop(); b.wait() })
	clk := &testClock{t: day}
	b.now = clk.now
	ctx := context.Background()

	var snap snapshot
	peak, took := measure(t, func() error {
		var err error
		snap, err = b.buildSnapshot(ctx, day)
		return err
	})
	budgets(t, "console build", peak, took, 45*time.Second)
	if len(snap.in.Apply) == 0 || len(snap.funnel.NewBySource) == 0 {
		t.Errorf("the snapshot lists nothing in Apply or no new jobs: %d, %v", len(snap.in.Apply), snap.funnel.NewBySource)
	}

	dash := func(state string, maxTook time.Duration) console.Dashboard {
		t.Helper()
		start := time.Now()
		d, err := b.Dashboard(ctx)
		if err != nil {
			t.Fatal(err)
		}
		took := time.Since(start)
		t.Logf("dashboard %s: %v (budget %v)", state, took.Round(time.Millisecond), maxTook)
		if took > maxTook {
			t.Errorf("dashboard %s took %v, over its budget of %v", state, took.Round(time.Millisecond), maxTook)
		}
		return d
	}
	if d := dash("cold", 600*time.Millisecond); !d.Building || !d.AsOf.IsZero() {
		t.Errorf("cold dashboard: building %v, as of %v", d.Building, d.AsOf)
	}
	b.wait()
	if d := dash("fresh", 500*time.Millisecond); d.Building || d.Updating || !d.AsOf.Equal(day) || d.ApplyNow != len(snap.in.Apply) {
		t.Errorf("fresh dashboard: %+v", d)
	}
	clk.add(dashboardFreshFor)
	if d := dash("updating", 500*time.Millisecond); !d.Updating || !d.AsOf.Equal(day) {
		t.Errorf("old dashboard: updating %v, as of %v", d.Updating, d.AsOf)
	}
}

// scaleLeadsConfig is the demo leads policy with the ramp starting at the first send, so a
// batch's sizing reads the first live send from the store as the server's does.
func scaleLeadsConfig(t *testing.T) leadconfig.Config {
	t.Helper()
	dir := t.TempDir()
	if err := os.CopyFS(filepath.Join(dir, "leads-templates"), os.DirFS(filepath.Join(examplesDir, "leads-templates"))); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(examplesDir, leadconfig.FileName))
	if err != nil {
		t.Fatal(err)
	}
	out := regexp.MustCompile(`(?m)^  start_date: .*$`).ReplaceAll(raw, []byte("  start_date: "+leadconfig.FirstSend))
	if err := os.WriteFile(filepath.Join(dir, leadconfig.FileName), out, 0o600); err != nil { // #nosec G703 -- a test temp dir
		t.Fatal(err)
	}
	cfg, err := leadconfig.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Caps.StartDate.IsFirstSend() {
		t.Fatal("caps.start_date not rewritten")
	}
	return cfg
}

// TestScaleLeadsBuildAndBatch: the whole lead build and the whole live batch new on the
// leads scale seed (leadstoretest.ScaleSeeded) as the app role, each in a transaction rolled
// back after, so the reused seed stays as it was. The seed lives on reserved names, which
// only a test lets through to a live batch.
func TestScaleLeadsBuildAndBatch(t *testing.T) {
	info := leadstoretest.ScaleSeeded(t)
	ctx := context.Background()
	s, err := store.Open(ctx, storetest.ScaleDSN())
	if err != nil {
		storetest.SkipOrFail(t, "skipping scale test: open app store: %v", err)
	}
	t.Cleanup(s.Close)
	cfg := scaleLeadsConfig(t)
	rolledBack := func(fn func(leadstore.Queries) error) error {
		tx, err := s.Pool().Begin(ctx)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback(ctx) }()
		return fn(leadstore.NewQueries(tx))
	}

	var sum buildSummary
	peak, took := measure(t, func() error {
		return rolledBack(func(lq leadstore.Queries) error {
			var err error
			sum, err = buildLeads(ctx, lq, cfg)
			return err
		})
	})
	budgets(t, "leads build", peak, took, 30*time.Second)
	if sum.Orgs != 9000 || sum.Leads == 0 || sum.Created == 0 {
		t.Errorf("build: %d orgs, %d leads, %d created", sum.Orgs, sum.Leads, sum.Created)
	}

	saved := leadsMail
	t.Cleanup(func() { leadsMail = saved })
	leadsMail.allowReserved = true
	bs := batchSetup{leads: cfg, loc: info.Loc} // the v3 scale seed holds ready mailboxes
	var out NewBatchOutcome
	peak, took = measure(t, func() error {
		return rolledBack(func(lq leadstore.Queries) error {
			var err error
			out, err = newBatch(ctx, lq, bs, info.Day.Add(10*time.Hour), NewBatchRequest{LiveOnly: true})
			return err
		})
	})
	budgets(t, "batch new", peak, took, 10*time.Second)
	if out.BatchID == 0 || out.Leads == 0 || out.Sizing == nil {
		t.Errorf("batch new: %+v", out)
	} else {
		t.Logf("batch new: %d leads, %s", out.Leads, out.Sizing.Words())
	}
}
