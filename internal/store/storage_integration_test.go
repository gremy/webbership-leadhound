//go:build integration

package store_test

import (
	"context"
	"errors"
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
	assertGridTrimmed(t, grid)

	if r, e, _ := trimAll(t, q, opt); r != 0 || e != 0 {
		t.Errorf("second pass trimmed rejected=%d expired=%d, want none", r, e)
	}
}

// assertGridTrimmed checks every observation of the grid against its expected cell (text
// blanked and marked, or whole and unmarked, the title kept either way), that every row is
// still there, and the invariant sweep: no protected job's observation is trimmed, whatever
// the shape.
func assertGridTrimmed(t *testing.T, grid []trimFixture) {
	t.Helper()
	ctx := context.Background()
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
}

// gridOpt is the trim's production ages with a small batch, so the grid takes several.
func gridOpt(now time.Time) store.TrimOptions {
	return store.TrimOptions{RejectedAfter: 7 * 24 * time.Hour, ExpiredAfter: 30 * 24 * time.Hour, Batch: 3, Now: now}
}

// TestTrimDescriptionsGrid: the loop runs the batches from the first observation to the
// last, pausing between them, trims exactly the grid's expected cells, and a second pass
// trims nothing and still scans every observation (a protected or untouched one is never
// marked, so it is scanned again; the partial index keeps that cheap).
func TestTrimDescriptionsGrid(t *testing.T) {
	ctx := context.Background()
	q := appStore(t).Q()
	now := time.Now()
	grid := seedTrimGrid(t, q, now)
	opt := gridOpt(now)

	pauses := 0
	got, err := q.TrimDescriptions(ctx, opt, func() { pauses++ })
	if err != nil {
		t.Fatalf("TrimDescriptions: %v", err)
	}
	wantBatches := (len(grid) + opt.Batch - 1) / opt.Batch
	if want := (store.TrimResult{Rejected: 4, Expired: 4, Batches: wantBatches}); got != want {
		t.Errorf("TrimDescriptions = %+v, want %+v", got, want)
	}
	// One pause after every batch that scanned something, before the call that finds the end.
	if pauses != wantBatches {
		t.Errorf("pauses = %d, want %d", pauses, wantBatches)
	}
	assertGridTrimmed(t, grid)

	again, err := q.TrimDescriptions(ctx, opt, nil)
	if err != nil {
		t.Fatalf("second TrimDescriptions: %v", err)
	}
	if again.Rejected != 0 || again.Expired != 0 {
		t.Errorf("second pass = %+v, want nothing trimmed", again)
	}
	if want := (len(grid) - 8 + opt.Batch - 1) / opt.Batch; again.Batches != want {
		t.Errorf("second pass batches = %d, want %d (the untrimmed observations only)", again.Batches, want)
	}
	assertGridTrimmed(t, grid)
}

// TestTrimDescriptionsColdStart: on an empty database the loop asks once, trims nothing,
// never pauses and returns no error.
func TestTrimDescriptionsColdStart(t *testing.T) {
	q := appStore(t).Q()
	pauses := 0
	got, err := q.TrimDescriptions(context.Background(), gridOpt(time.Now()), func() { pauses++ })
	if err != nil || got != (store.TrimResult{}) || pauses != 0 {
		t.Errorf("TrimDescriptions on an empty database = %+v, %v, %d pauses; want nothing, no error, no pause", got, err, pauses)
	}
}

// TestTrimDescriptionsStopsOnContext: a context ended between batches stops the loop before
// the next one, returning what the batches so far did together with the context's error;
// what they trimmed stays trimmed, and a later run carries on from there. A context ended
// before the first batch trims nothing.
func TestTrimDescriptionsStopsOnContext(t *testing.T) {
	q := appStore(t).Q()
	now := time.Now()
	grid := seedTrimGrid(t, q, now)
	opt := gridOpt(now)

	ctx, cancel := context.WithCancel(context.Background())
	got, err := q.TrimDescriptions(ctx, opt, cancel)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("TrimDescriptions cancelled in its pause = %+v, %v; want context.Canceled", got, err)
	}
	if got.Batches != 1 {
		t.Errorf("batches before the stop = %d, want 1", got.Batches)
	}
	rest, err := q.TrimDescriptions(context.Background(), opt, nil)
	if err != nil {
		t.Fatalf("TrimDescriptions after the stop: %v", err)
	}
	if r, e := got.Rejected+rest.Rejected, got.Expired+rest.Expired; r != 4 || e != 4 {
		t.Errorf("trimmed over both runs rejected=%d expired=%d, want 4 and 4", r, e)
	}
	assertGridTrimmed(t, grid)

	done, cancelNow := context.WithCancel(context.Background())
	cancelNow()
	if got, err := q.TrimDescriptions(done, opt, nil); !errors.Is(err, context.Canceled) || got != (store.TrimResult{}) {
		t.Errorf("TrimDescriptions on an ended context = %+v, %v; want nothing and context.Canceled", got, err)
	}
}

// TestTrimDescriptionsBadOptions: options TrimBatch refuses are refused by the loop too,
// before anything is trimmed.
func TestTrimDescriptionsBadOptions(t *testing.T) {
	q := appStore(t).Q()
	if _, err := q.TrimDescriptions(context.Background(), store.TrimOptions{RejectedAfter: time.Hour, ExpiredAfter: time.Hour}, nil); err == nil {
		t.Error("TrimDescriptions with batch 0 = nil error, want refused")
	}
}

// gridJob returns the grid fixture whose job was seeded as "state/shape".
func gridJob(t *testing.T, q store.Queries, grid []trimFixture, sid string) trimFixture {
	t.Helper()
	j, err := q.JobBySourceID(context.Background(), "linkedin", sid)
	if err != nil {
		t.Fatalf("JobBySourceID %s: %v", sid, err)
	}
	for _, f := range grid {
		if f.jobID == j.ID {
			return f
		}
	}
	t.Fatalf("job %s not in the grid", sid)
	return trimFixture{}
}

// TestTrimLeavesScoreQueue: a job whose current observation was trimmed is never scored on
// its empty text. A trimmed rejected job still open stops being counted among the gate
// failures, and a trimmed expired job listed again (reopened before its board's new
// observation is written) is neither queued nor handed to the scorer. Once observed afresh
// it is queued again.
func TestTrimLeavesScoreQueue(t *testing.T) {
	ctx := context.Background()
	s := appStore(t)
	q := s.Q()
	now := time.Now()
	grid := seedTrimGrid(t, q, now)

	before, err := q.ScoreQueue(ctx)
	if err != nil {
		t.Fatalf("ScoreQueue: %v", err)
	}
	// Open and new or shortlisted: new, shortlisted and briefed; rejected 8 and 6 days ago
	// fail a gate, open-passing is queued.
	if before.GateFailed != 6 || len(before.Entries) != 3 {
		t.Fatalf("queue before the trim: %d entries, %d gate failed; want 3 and 6", len(before.Entries), before.GateFailed)
	}
	if _, err := q.TrimDescriptions(ctx, gridOpt(now), nil); err != nil {
		t.Fatalf("TrimDescriptions: %v", err)
	}
	after, err := q.ScoreQueue(ctx)
	if err != nil {
		t.Fatalf("ScoreQueue: %v", err)
	}
	// new and shortlisted × rejected 8 days ago are trimmed and leave the count.
	if after.GateFailed != 4 || !slices.Equal(entryIDs(after), entryIDs(before)) {
		t.Errorf("queue after the trim: %v, %d gate failed; want %v and 4", entryIDs(after), after.GateFailed, entryIDs(before))
	}

	reopened := gridJob(t, q, grid, "new/expired-31d")
	if !reopened.wantTrimmed {
		t.Fatal("new/expired-31d is not a trimmed cell")
	}
	if n, err := q.Reopen(ctx, "linkedin", []string{"new/expired-31d"}); err != nil || n != 1 {
		t.Fatalf("Reopen = %d, %v", n, err)
	}
	queue, err := q.ScoreQueue(ctx)
	if err != nil {
		t.Fatalf("ScoreQueue: %v", err)
	}
	if slices.Contains(entryIDs(queue), reopened.jobID) || queue.GateFailed != 4 || queue.AwaitingDetail != 0 {
		t.Errorf("reopened trimmed job queued: %v, %d gate failed, %d awaiting", entryIDs(queue), queue.GateFailed, queue.AwaitingDetail)
	}
	if in, err := q.ScoreInputsFor(ctx, []int64{reopened.jobID}); err != nil || len(in) != 0 {
		t.Errorf("ScoreInputsFor(reopened trimmed job) = %d inputs, %v; want none", len(in), err)
	}

	// Observed afresh, the job has whole text on its current observation and is queued.
	j, err := q.JobBySourceID(ctx, "linkedin", "new/expired-31d")
	if err != nil {
		t.Fatal(err)
	}
	_, fetchID := seedJob(t, q, "fresh-fetch-holder")
	if _, err := q.InsertObservation(ctx, store.ObservationInsert{
		JobID: j.ID, FetchID: fetchID, DerivedBy: "test@1", Title: "Go Engineer", DescriptionClean: "posting text again",
	}); err != nil {
		t.Fatalf("InsertObservation: %v", err)
	}
	queue, err = q.ScoreQueue(ctx)
	if err != nil {
		t.Fatalf("ScoreQueue: %v", err)
	}
	if !slices.Contains(entryIDs(queue), reopened.jobID) {
		t.Errorf("job observed afresh not queued: %v", entryIDs(queue))
	}
	if in, err := q.ScoreInputsFor(ctx, []int64{reopened.jobID}); err != nil || len(in) != 1 || in[0].Description != "posting text again" {
		t.Errorf("ScoreInputsFor(job observed afresh) = %+v, %v; want its new text", in, err)
	}
}

func entryIDs(q store.ScoreQueue) []int64 {
	var ids []int64
	for _, e := range q.Entries {
		ids = append(ids, e.JobID)
	}
	return ids
}

// TestTrimJobsWithoutObservation: a trimmed posting still open and listed is not stored
// again (the trim never re-creates the rows it emptied), while a trimmed expired one is
// "without observation", so the board that lists it again observes it afresh before
// reopening it (the ATS collector reads fresh before Reopen). Untrimmed jobs, open or
// expired, keep their observation as before.
func TestTrimJobsWithoutObservation(t *testing.T) {
	ctx := context.Background()
	q := appStore(t).Q()
	now := time.Now()
	seedTrimGrid(t, q, now)
	if _, err := q.TrimDescriptions(ctx, gridOpt(now), nil); err != nil {
		t.Fatalf("TrimDescriptions: %v", err)
	}
	ids := []string{
		"new/rejected-8d",              // trimmed, open
		"skipped-system/expired-31d",   // trimmed, expired
		"new/rejected-6d",              // untrimmed, open
		"new/expired-29d",              // untrimmed, expired
		"applied-operator/expired-31d", // protected, expired, untrimmed
		"new/expired-31d",              // trimmed, expired
		"never-seen",                   // no row at all
	}
	got, err := q.JobsWithoutObservation(ctx, "linkedin", ids)
	if err != nil {
		t.Fatalf("JobsWithoutObservation: %v", err)
	}
	if want := []string{"skipped-system/expired-31d", "new/expired-31d", "never-seen"}; !slices.Equal(got, want) {
		t.Errorf("JobsWithoutObservation = %v, want %v", got, want)
	}

	// Reopened without a new observation (a title the screen now drops): open again, so it
	// is not stored again either; it simply stays out of the score queue.
	if _, err := q.Reopen(ctx, "linkedin", []string{"new/expired-31d"}); err != nil {
		t.Fatal(err)
	}
	got, err = q.JobsWithoutObservation(ctx, "linkedin", []string{"new/expired-31d"})
	if err != nil || len(got) != 0 {
		t.Errorf("JobsWithoutObservation(reopened trimmed) = %v, %v; want none", got, err)
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
