//go:build integration

package main

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/fetch"
	"github.com/gremy/webbership-leadhound/internal/store"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

// trimShape is one posting of the trim fixture: rejected or expired this long ago, or open
// and passing, on a job the operator applied to (protected) or not.
type trimShape struct {
	name      string
	age       time.Duration
	rejected  bool
	expired   bool
	protected bool
}

// seedTrimJobs stores one job and one observation per shape, aged against the current time
// (the command trims against now), and returns the observation ids by shape name.
func seedTrimJobs(t *testing.T, shapes []trimShape) map[string]int64 {
	t.Helper()
	ctx := context.Background()
	s, err := store.Open(ctx, storetest.AppDSN())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	q := s.Q()
	runID, err := q.StartRun(ctx, "collect", "linkedin")
	if err != nil {
		t.Fatal(err)
	}
	rec := store.NewFetchRecorder(q, runID, "linkedin")
	companyID, err := q.UpsertCompany(ctx, "Initech", "", "")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	obs := map[string]int64{}
	for _, sh := range shapes {
		url := "https://jobs.example/" + sh.name
		if err := rec.Record(ctx, fetch.Record{Kind: fetch.KindDetail, URL: url, Status: 200, Body: []byte("x")}); err != nil {
			t.Fatal(err)
		}
		jobID, err := q.UpsertJob(ctx, store.JobUpsert{Source: "linkedin", SourceJobID: sh.name, CompanyID: companyID, Title: "Go Engineer", URL: url})
		if err != nil {
			t.Fatal(err)
		}
		// Written by the owner, not through pipeline.Observe: the fixture sets its own verdict.
		var obsID int64
		if err := storetest.OwnerConn(t).QueryRow(ctx,
			`INSERT INTO job_observation (job_id, fetch_id, derived_by, title, description_clean)
			 VALUES ($1, $2, 'test@1', 'Go Engineer', $3) RETURNING id`,
			jobID, rec.LastID(), "posting text "+sh.name).Scan(&obsID); err != nil {
			t.Fatal(err)
		}
		verdict := "pass"
		if sh.rejected {
			verdict = "fail"
		}
		if _, err := q.InsertGateResult(ctx, store.GateResultInsert{JobID: jobID, ObservationID: obsID, Gate: "workplace",
			Verdict: verdict, Reason: "test", DerivedBy: "gates@1"}); err != nil {
			t.Fatal(err)
		}
		ownerExecCmd(t, `UPDATE job_observation SET observed_at = $2 WHERE id = $1`, obsID, now.Add(-sh.age))
		if sh.expired {
			ownerExecCmd(t, `UPDATE job SET listing_status = 'expired', status_checked_at = $2 WHERE id = $1`, jobID, now.Add(-sh.age))
		}
		if sh.protected {
			if _, err := q.InsertStatusEvent(ctx, jobID, "applied", "", "gremy", nil); err != nil {
				t.Fatal(err)
			}
		}
		obs[sh.name] = obsID
	}
	return obs
}

// trimmedObservations is how many observations have lost their text.
func trimmedObservations(t *testing.T) int {
	t.Helper()
	return dbCount(t, `SELECT count(*) FROM job_observation WHERE description_trimmed_at IS NOT NULL`)
}

// trimLine is the counts on the command's summary line.
var trimLine = regexp.MustCompile(`(?m)^trim: rejected (\d+) · expired (\d+) · batches (\d+) · \S+$`)

// parseTrimLine returns the summary line's rejected, expired and batches counts.
func parseTrimLine(t *testing.T, out string) (rejected, expired, batches int) {
	t.Helper()
	m := trimLine.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("no trim line in %q", out)
	}
	rejected, _ = strconv.Atoi(m[1])
	expired, _ = strconv.Atoi(m[2])
	batches, _ = strconv.Atoi(m[3])
	return rejected, expired, batches
}

// TestTrimCommandGrid: trim blanks exactly the old rejected and old expired postings of
// unprotected jobs, prints its line and records one finished trim run; a second run trims
// nothing.
func TestTrimCommandGrid(t *testing.T) {
	commandDB(t)
	day := 24 * time.Hour
	obs := seedTrimJobs(t, []trimShape{
		{name: "rejected-8d", age: 8 * day, rejected: true},
		{name: "rejected-6d", age: 6 * day, rejected: true},
		{name: "expired-31d", age: 31 * day, expired: true},
		{name: "expired-29d", age: 29 * day, expired: true},
		{name: "open-passing", age: 40 * day},
		{name: "applied-rejected-8d", age: 8 * day, rejected: true, protected: true},
		{name: "applied-expired-31d", age: 31 * day, expired: true, protected: true},
	})

	code, out := runCmd(t, "trim", "--batch", "2")
	if code != 0 {
		t.Fatalf("trim: exit %d\n%s", code, out)
	}
	if r, e, b := parseTrimLine(t, out); r != 1 || e != 1 || b != 4 {
		t.Errorf("trim line rejected %d expired %d batches %d, want 1, 1, 4:\n%s", r, e, b, out)
	}
	for name, id := range obs {
		want := name == "rejected-8d" || name == "expired-31d"
		got := dbCount(t, fmt.Sprintf(`SELECT count(*) FROM job_observation WHERE id = %d AND description_trimmed_at IS NOT NULL AND description_clean = ''`, id))
		if (got == 1) != want {
			t.Errorf("%s: trimmed=%v, want %v", name, got == 1, want)
		}
	}
	if got := dbCount(t, `SELECT count(*) FROM run WHERE command = 'trim' AND finished_at IS NOT NULL AND stop_reason = ''
		AND note = 'rejected 1 · expired 1 · batches 4'`); got != 1 {
		t.Errorf("finished trim runs with the counts = %d, want 1", got)
	}

	code, out = runCmd(t, "trim")
	if code != 0 {
		t.Fatalf("second trim: exit %d\n%s", code, out)
	}
	if r, e, _ := parseTrimLine(t, out); r != 0 || e != 0 {
		t.Errorf("second trim rejected %d expired %d, want nothing:\n%s", r, e, out)
	}
	if got := trimmedObservations(t); got != 2 {
		t.Errorf("trimmed observations = %d, want 2", got)
	}
	if got := dbCount(t, `SELECT count(*) FROM run WHERE command = 'trim' AND finished_at IS NOT NULL`); got != 2 {
		t.Errorf("finished trim runs = %d, want 2", got)
	}
}

// TestTrimCommandBudget: a run that reaches --max-duration stops cleanly with what it got
// done (exit 0, stop reason budget, a note saying so), and the next run trims the rest.
func TestTrimCommandBudget(t *testing.T) {
	commandDB(t)
	var shapes []trimShape
	for i := range 12 {
		shapes = append(shapes, trimShape{name: fmt.Sprintf("rejected-%02d", i), age: 8 * 24 * time.Hour, rejected: true})
	}
	seedTrimJobs(t, shapes)

	// One observation a batch and 200 ms between batches: 500 ms gets through a few.
	code, out := runCmd(t, "trim", "--batch", "1", "--max-duration", "500ms")
	if code != 0 {
		t.Fatalf("trim: exit %d\n%s", code, out)
	}
	r, _, b := parseTrimLine(t, out)
	if r < 1 || r >= len(shapes) || b != r {
		t.Errorf("budget run rejected %d batches %d, want a part of %d:\n%s", r, b, len(shapes), out)
	}
	if !strings.Contains(out, "trim: stopped at the 500ms budget; the next run carries on") {
		t.Errorf("no budget line:\n%s", out)
	}
	if got := trimmedObservations(t); got != r {
		t.Errorf("trimmed observations = %d, want %d", got, r)
	}
	want := fmt.Sprintf(`SELECT count(*) FROM run WHERE command = 'trim' AND finished_at IS NOT NULL AND stop_reason = 'budget'
		AND note = 'stopped at the 500ms budget after rejected %d · expired 0 · batches %d; the next run carries on'`, r, b)
	if got := dbCount(t, want); got != 1 {
		t.Errorf("budget runs with the note = %d, want 1", got)
	}

	code, out = runCmd(t, "trim", "--max-duration", "0")
	if code != 0 {
		t.Fatalf("follow-up trim: exit %d\n%s", code, out)
	}
	if r2, _, _ := parseTrimLine(t, out); r+r2 != len(shapes) {
		t.Errorf("follow-up rejected %d after %d, want the rest of %d:\n%s", r2, r, len(shapes), out)
	}
	if got := trimmedObservations(t); got != len(shapes) {
		t.Errorf("trimmed observations = %d, want %d", got, len(shapes))
	}
}

// TestTrimCommandBadFlags: a bad flag or argument is a usage error (exit 2) that opens no
// run.
func TestTrimCommandBadFlags(t *testing.T) {
	commandDB(t)
	for _, args := range [][]string{
		{"--max-duration", "-1s"},
		{"--batch", "0"},
		{"--batch", "-5"},
		{"--rejected-after", "0"},
		{"--expired-after", "-1h"},
		{"--max-duration", "soon"},
		{"--no-such-flag"},
		{"extra"},
	} {
		code, _, stderr := runCmdErr(t, append([]string{"trim"}, args...)...)
		if code != 2 || !strings.HasPrefix(stderr, "leadhound trim: usage: ") {
			t.Errorf("trim %v: exit %d, stderr %q; want 2 and a usage error", args, code, stderr)
		}
	}
	if got := dbCount(t, `SELECT count(*) FROM run WHERE command = 'trim'`); got != 0 {
		t.Errorf("trim runs = %d, want 0", got)
	}
}
