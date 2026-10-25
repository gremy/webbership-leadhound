//go:build integration

package store_test

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/store"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

func TestStorageDatabaseSize(t *testing.T) {
	q := appStore(t).Q()
	n, err := q.DatabaseSize(context.Background())
	if err != nil {
		t.Fatalf("DatabaseSize: %v", err)
	}
	if n <= 0 {
		t.Fatalf("DatabaseSize = %d, want > 0", n)
	}
}

// TestStorageSchema proves what 00007 adds: the trim column and its keyset index (valid), the
// disk_budget stop reason, and the trim function's fence: owned by the owner role, running as
// it with a fixed search_path, executable by the app role and by nobody through PUBLIC.
func TestStorageSchema(t *testing.T) {
	ctx := context.Background()
	q := appStore(t).Q()
	conn := storetest.AppConn(t)

	var valid bool
	if err := conn.QueryRow(ctx,
		`SELECT i.indisvalid FROM pg_index i JOIN pg_class c ON c.oid = i.indexrelid
		 WHERE c.relname = 'job_observation_untrimmed_id_idx'`).Scan(&valid); err != nil || !valid {
		t.Errorf("job_observation_untrimmed_id_idx valid = %v, %v; want true", valid, err)
	}

	runID, err := q.StartRun(ctx, "collect", "greenhouse")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	if err := q.FinishRun(ctx, runID, store.RunResult{StopReason: "disk_budget", Note: "over"}); err != nil {
		t.Errorf("FinishRun disk_budget: %v", err)
	}

	var owner string
	var secdef, appExec, publicExec bool
	var config []string
	if err := conn.QueryRow(ctx,
		`SELECT pg_get_userbyid(p.proowner), p.prosecdef, coalesce(p.proconfig, '{}'),
		        has_function_privilege('leadhound_app', p.oid, 'EXECUTE'),
		        EXISTS (SELECT 1 FROM aclexplode(coalesce(p.proacl, acldefault('f', p.proowner))) a
		                WHERE a.grantee = 0 AND a.privilege_type = 'EXECUTE')
		 FROM pg_proc p WHERE p.proname = 'leadhound_trim_batch'`).
		Scan(&owner, &secdef, &config, &appExec, &publicExec); err != nil {
		t.Fatalf("read leadhound_trim_batch: %v", err)
	}
	if owner != "leadhound_owner" || !secdef || !appExec || publicExec {
		t.Errorf("leadhound_trim_batch owner=%s secdef=%v app=%v public=%v; want leadhound_owner, true, true, false",
			owner, secdef, appExec, publicExec)
	}
	if !slices.Contains(config, "search_path=public, pg_temp") {
		t.Errorf("leadhound_trim_batch config = %v, want search_path=public, pg_temp", config)
	}
}

// trimFixture is one job of the trim grid: its state and the shape of its one observation.
type trimFixture struct {
	jobID, obsID int64
	protected    bool
	wantTrimmed  bool
}

// seedTrimGrid builds the status × observation grid at now: every job state against every
// observation shape, one job and one observation each.
func seedTrimGrid(t *testing.T, q store.Queries, now time.Time) []trimFixture {
	t.Helper()
	ctx := context.Background()
	states := []struct {
		name      string
		events    [][2]string // status, actor, oldest first
		briefed   bool
		protected bool
	}{
		{"new", [][2]string{{"new", "system"}}, false, false},
		{"shortlisted", [][2]string{{"new", "system"}, {"shortlisted", "system"}}, false, false},
		{"skipped-system", [][2]string{{"new", "system"}, {"skipped", "system"}}, false, false},
		{"applied-operator", [][2]string{{"new", "system"}, {"applied", "gremy"}}, false, true},
		{"skipped-operator", [][2]string{{"new", "system"}, {"skipped", "gremy"}}, false, true},
		// The current-status rule on its own: applied without the operator's hand.
		{"applied-system", [][2]string{{"new", "system"}, {"applied", "system"}}, false, true},
		// Applied once, skipped since: the current status is what counts.
		{"applied-then-skipped-system", [][2]string{{"applied", "system"}, {"skipped", "system"}}, false, false},
		{"briefed", [][2]string{{"new", "system"}}, true, true},
	}
	shapes := []struct {
		name     string
		age      time.Duration // observed (rejected) or checked (expired) this long ago
		rejected bool
		expired  bool
		trimmed  bool // for an unprotected job
	}{
		{"rejected-8d", 8 * 24 * time.Hour, true, false, true},
		{"rejected-6d", 6 * 24 * time.Hour, true, false, false},
		{"expired-31d", 31 * 24 * time.Hour, false, true, true},
		{"expired-29d", 29 * 24 * time.Hour, false, true, false},
		{"open-passing", 40 * 24 * time.Hour, false, false, false},
	}
	var out []trimFixture
	var briefed []int64
	for _, st := range states {
		for _, sh := range shapes {
			id := fmt.Sprintf("%s/%s", st.name, sh.name)
			jobID, fetchID := seedJob(t, q, id)
			obsID, err := q.InsertObservation(ctx, store.ObservationInsert{
				JobID: jobID, FetchID: fetchID, DerivedBy: "test@1", Title: "Go Engineer",
				DescriptionClean: "posting text " + id,
			})
			if err != nil {
				t.Fatalf("InsertObservation: %v", err)
			}
			verdict := "pass"
			if sh.rejected {
				verdict = "fail"
			}
			if _, err := q.InsertGateResult(ctx, store.GateResultInsert{
				JobID: jobID, ObservationID: obsID, Gate: "workplace", Verdict: verdict, Reason: "test", DerivedBy: "gates@1",
			}); err != nil {
				t.Fatalf("InsertGateResult: %v", err)
			}
			ownerExec(t, `UPDATE job_observation SET observed_at = $2 WHERE id = $1`, obsID, now.Add(-sh.age))
			if sh.expired {
				ownerExec(t, `UPDATE job SET listing_status = 'expired', status_checked_at = $2 WHERE id = $1`, jobID, now.Add(-sh.age))
			}
			for i, ev := range st.events {
				if _, err := q.InsertStatusEvent(ctx, jobID, ev[0], "", ev[1], nil); err != nil {
					t.Fatalf("InsertStatusEvent: %v", err)
				}
				// Spread the events in time so the latest one is unambiguous.
				ownerExec(t, `UPDATE status_event SET at = $2 WHERE job_id = $1 AND status = $3 AND actor = $4`,
					jobID, now.Add(-time.Duration(len(st.events)-i)*time.Hour), ev[0], ev[1])
			}
			if st.briefed {
				briefed = append(briefed, jobID)
			}
			out = append(out, trimFixture{
				jobID: jobID, obsID: obsID, protected: st.protected,
				wantTrimmed: !st.protected && sh.trimmed,
			})
		}
	}
	if _, err := q.UpsertBrief(ctx, "2026-01-02", store.BriefDraft{Subject: "s", Text: "t", JobIDs: briefed}); err != nil {
		t.Fatalf("UpsertBrief: %v", err)
	}
	if err := q.MarkBriefSent(ctx, "2026-01-02", "p"); err != nil {
		t.Fatalf("MarkBriefSent: %v", err)
	}
	return out
}

// trimAll runs TrimBatch from id 0 until a batch scans nothing, checking the keyset only
// moves forward, and returns the totals and the number of batches.
func trimAll(t *testing.T, q store.Queries, opt store.TrimOptions) (rejected, expired, batches int) {
	t.Helper()
	var after int64
	for {
		r, err := q.TrimBatch(context.Background(), opt, after)
		if err != nil {
			t.Fatalf("TrimBatch(after %d): %v", after, err)
		}
		if r.Done {
			if r.Rejected != 0 || r.Expired != 0 {
				t.Fatalf("TrimBatch done with counts %+v", r)
			}
			return rejected, expired, batches
		}
		if r.LastID <= after {
			t.Fatalf("TrimBatch(after %d) last id %d does not move forward", after, r.LastID)
		}
		after = r.LastID
		rejected += r.Rejected
		expired += r.Expired
		batches++
		if batches > 1000 {
			t.Fatal("TrimBatch never finished")
		}
	}
}

// TestTrimBatchGrid: the trim blanks the text of old rejected and old expired postings of
// unprotected jobs only, in small keyset batches through the app role, keeps every row, and a
// second pass trims nothing.
func TestTrimBatchGrid(t *testing.T) {
	ctx := context.Background()
	s := appStore(t)
	q := s.Q()
	now := time.Now()
	grid := seedTrimGrid(t, q, now)
	opt := store.TrimOptions{RejectedAfter: 7 * 24 * time.Hour, ExpiredAfter: 30 * 24 * time.Hour, Batch: 3, Now: now}

	rejected, expired, batches := trimAll(t, q, opt)
	// (new | shortlisted | skipped by system | applied then skipped by system) × one shape each
	wantRejected, wantExpired := 4, 4
	if rejected != wantRejected || expired != wantExpired {
		t.Errorf("trimmed rejected=%d expired=%d, want %d and %d", rejected, expired, wantRejected, wantExpired)
	}
	if want := (len(grid) + opt.Batch - 1) / opt.Batch; batches != want {
		t.Errorf("batches = %d, want %d (every observation scanned once)", batches, want)
	}

	conn := storetest.AppConn(t)
	for _, f := range grid {
		var desc, title string
		var trimmedAt *time.Time
		if err := conn.QueryRow(ctx,
			`SELECT description_clean, title, description_trimmed_at FROM job_observation WHERE id = $1`, f.obsID).
			Scan(&desc, &title, &trimmedAt); err != nil {
			t.Fatalf("read observation %d: %v", f.obsID, err)
		}
		gotTrimmed := trimmedAt != nil
		if gotTrimmed != f.wantTrimmed || (desc == "") != f.wantTrimmed {
			t.Errorf("observation %d (job %d): trimmed=%v text=%q, want trimmed=%v", f.obsID, f.jobID, gotTrimmed, desc, f.wantTrimmed)
		}
		if title != "Go Engineer" {
			t.Errorf("observation %d title = %q, the trim touched more than the text", f.obsID, title)
		}
	}
	var rows int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM job_observation`).Scan(&rows); err != nil || rows != len(grid) {
		t.Errorf("observations = %d, %v; want %d", rows, err, len(grid))
	}

	// Invariant sweep: no protected job's observation is trimmed, whatever the shape.
	var bad int
	if err := conn.QueryRow(ctx,
		`SELECT count(*) FROM job_observation o
		 WHERE o.description_trimmed_at IS NOT NULL
		   AND (EXISTS (SELECT 1 FROM status_event s WHERE s.job_id = o.job_id AND s.actor = 'gremy')
		        OR (SELECT s.status FROM status_event s WHERE s.job_id = o.job_id ORDER BY s.at DESC, s.id DESC LIMIT 1)
		           IN ('applied', 'interviewing', 'offer')
		        OR EXISTS (SELECT 1 FROM brief_log b WHERE o.job_id = ANY (b.job_ids)))`).Scan(&bad); err != nil || bad != 0 {
		t.Errorf("protected observations trimmed = %d, %v; want 0", bad, err)
	}

	if r, e, _ := trimAll(t, q, opt); r != 0 || e != 0 {
		t.Errorf("second pass trimmed rejected=%d expired=%d, want none", r, e)
	}
}

// TestTrimBatchKeyset: a batch scans the next batch-size untrimmed observations after the
// given id, so a protected (never trimmed) one is passed over rather than scanned forever,
// and a start past the last observation is done at once.
func TestTrimBatchKeyset(t *testing.T) {
	ctx := context.Background()
	q := appStore(t).Q()
	now := time.Now()
	grid := seedTrimGrid(t, q, now)
	opt := store.TrimOptions{RejectedAfter: 7 * 24 * time.Hour, ExpiredAfter: 30 * 24 * time.Hour, Batch: 4, Now: now}

	first, err := q.TrimBatch(ctx, opt, 0)
	if err != nil {
		t.Fatalf("TrimBatch: %v", err)
	}
	if first.Done || first.LastID != grid[3].obsID {
		t.Errorf("first batch = %+v, want last id %d", first, grid[3].obsID)
	}
	// Starting from the middle scans only what is after it.
	mid := grid[len(grid)-2].obsID
	r, err := q.TrimBatch(ctx, opt, mid)
	if err != nil {
		t.Fatalf("TrimBatch(after %d): %v", mid, err)
	}
	if r.Done || r.LastID != grid[len(grid)-1].obsID {
		t.Errorf("batch after %d = %+v, want last id %d", mid, r, grid[len(grid)-1].obsID)
	}
	end, err := q.TrimBatch(ctx, opt, grid[len(grid)-1].obsID)
	if err != nil || !end.Done {
		t.Errorf("TrimBatch past the end = %+v, %v; want done", end, err)
	}
}

// TestTrimBatchColdStart: an empty database is done at once, with nothing trimmed.
func TestTrimBatchColdStart(t *testing.T) {
	q := appStore(t).Q()
	opt := store.TrimOptions{RejectedAfter: time.Hour, ExpiredAfter: time.Hour, Batch: 10}
	r, err := q.TrimBatch(context.Background(), opt, 0)
	if err != nil || !r.Done || r.Rejected != 0 || r.Expired != 0 {
		t.Errorf("TrimBatch on an empty database = %+v, %v; want done and nothing trimmed", r, err)
	}
}

// TestTrimBatchBadOptions: a batch size or an age that is not positive is refused before
// the database is asked.
func TestTrimBatchBadOptions(t *testing.T) {
	q := appStore(t).Q()
	for _, opt := range []store.TrimOptions{
		{RejectedAfter: time.Hour, ExpiredAfter: time.Hour, Batch: 0},
		{RejectedAfter: 0, ExpiredAfter: time.Hour, Batch: 10},
		{RejectedAfter: time.Hour, ExpiredAfter: -time.Hour, Batch: 10},
	} {
		if _, err := q.TrimBatch(context.Background(), opt, 0); err == nil {
			t.Errorf("TrimBatch(%+v) = nil error, want refused", opt)
		}
	}
}
