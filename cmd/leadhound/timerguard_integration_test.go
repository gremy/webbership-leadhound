//go:build integration

package main

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/store"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

// guardWorld is an empty test database and data directory for the due-check, with its clock
// set to now.
func guardWorld(t *testing.T, now time.Time) string {
	t.Helper()
	storetest.ResetDB(t)
	dir := t.TempDir()
	t.Setenv("LEADHOUND_DATABASE_URL", storetest.AppDSN())
	t.Setenv("LEADHOUND_DATA_DIR", dir)
	t.Setenv("INVOCATION_ID", "")
	t.Setenv("SERVICE_RESULT", "")
	t.Setenv("EXIT_STATUS", "")
	setGuardClock(t, now)
	return dir
}

func setGuardClock(t *testing.T, now time.Time) {
	t.Helper()
	saved := guardClock
	guardClock = func() time.Time { return now }
	t.Cleanup(func() { guardClock = saved })
}

// guardRun runs `leadhound timer-guard args…` and returns its exit and stdout.
func guardRun(t *testing.T, args ...string) (int, string) {
	t.Helper()
	captureGuardLog(t)
	out := captureStdout(t)
	code := run(context.Background(), append([]string{"timer-guard"}, args...), &bytes.Buffer{})
	return code, out.String()
}

// lastSlotBefore is g's latest slot at or before now (within a day).
func lastSlotBefore(g guardJob, now time.Time) time.Time {
	at, ok := lastSlot(g, now)
	if !ok {
		panic("no slot within a day")
	}
	return at
}

// insertRun stores a run row as the app role, with its start, end and note as given.
func insertRun(t *testing.T, command, source string, started time.Time, finished *time.Time, note string) {
	t.Helper()
	if _, err := storetest.AppConn(t).Exec(context.Background(),
		`INSERT INTO run (command, source, started_at, finished_at, note) VALUES ($1, $2, $3, $4, $5)`,
		command, source, started, finished, note); err != nil {
		t.Fatal(err)
	}
}

func startRows(t *testing.T, job string) []store.Run {
	t.Helper()
	runs, err := guardStartRows(context.Background(), storetest.AppConn(t), job, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	return runs
}

// TestTimerGuardRecord: the unit's check (--record) on the test database: the first slot with
// no history starts once and records it, the same tick again waits, --ended closes the row, a
// press starts once, a bad job waits.
func TestTimerGuardRecord(t *testing.T) {
	g := guardJobs["leads-batch"]
	slot := lastSlotBefore(g, time.Now())
	dir := guardWorld(t, slot)

	if code, out := guardRun(t, "--record", "leads-batch"); code != 0 || out != "" {
		t.Fatalf("no history at the slot: exit %d, out %q; want 0", code, out)
	}
	rows := startRows(t, "leads-batch")
	if len(rows) != 1 || rows[0].Command != commandScheduled || rows[0].FinishedAt != nil ||
		rows[0].Note != notePlanned+slot.Format(time.RFC3339) {
		t.Fatalf("start rows %+v, want one open scheduled row planned at the slot", rows)
	}

	// The unit stops: --ended after a passed-over tick does nothing, after the run closes it.
	t.Setenv("SERVICE_RESULT", "exec-condition")
	if code, _ := guardRun(t, "--record", "--ended", "leads-batch"); code != 0 {
		t.Fatalf("--ended exec-condition: exit %d", code)
	}
	if r := startRows(t, "leads-batch"); r[0].FinishedAt != nil {
		t.Fatal("--ended after a passed-over tick closed the run")
	}
	t.Setenv("SERVICE_RESULT", "success")
	t.Setenv("EXIT_STATUS", "0")
	if code, _ := guardRun(t, "--record", "--ended", "leads-batch"); code != 0 {
		t.Fatalf("--ended: exit %d", code)
	}
	if r := startRows(t, "leads-batch"); r[0].FinishedAt == nil || !strings.HasSuffix(r[0].Note, " · ended success") {
		t.Fatalf("--ended: %+v", r[0])
	}
	t.Setenv("SERVICE_RESULT", "")
	t.Setenv("EXIT_STATUS", "")

	if code, _ := guardRun(t, "--record", "leads-batch"); code != 3 {
		t.Errorf("the same tick again: exit %d, want 3", code)
	}
	if n := len(startRows(t, "leads-batch")); n != 1 {
		t.Errorf("%d start rows after the same tick, want 1", n)
	}

	// A press now, after that run started (an earlier press would be served by it): one
	// run-now row; the same press again waits.
	press := time.Now()
	setGuardClock(t, press.Add(time.Second))
	if err := writeRunRequest(dir, "leads-batch", press); err != nil {
		t.Fatal(err)
	}
	if code, _ := guardRun(t, "--record", "leads-batch"); code != 0 {
		t.Fatalf("a press: exit %d, want 0", code)
	}
	rows = startRows(t, "leads-batch")
	if len(rows) != 2 || rows[1].Command != commandRunNow || !strings.HasPrefix(rows[1].Note, noteAskedAt) {
		t.Fatalf("start rows %+v, want a run-now row", rows)
	}
	if code, _ := guardRun(t, "--record", "leads-batch"); code != 3 {
		t.Errorf("the same press again: exit %d, want 3", code)
	}
	if n := runCount(t, `SELECT count(*) FROM run WHERE command = $1`, commandRunNow); n != 1 {
		t.Errorf("%d run-now rows, want 1", n)
	}
	// The run-now row was left open: the next check closes it as unrecorded before deciding.
	if r := startRows(t, "leads-batch"); r[1].FinishedAt == nil || !strings.HasSuffix(r[1].Note, " · ended unrecorded") {
		t.Errorf("a dead run left open: %+v", r[1])
	}

	if code, _ := guardRun(t, "--record", "no-such-job"); code != 3 {
		t.Errorf("bad job: exit %d, want 3", code)
	}
}

// TestTimerGuardExplain: without --record the check prints the verdict, the anchor and the plan,
// exits 0 and writes nothing, even when the job is due.
func TestTimerGuardExplain(t *testing.T) {
	g := guardJobs["ats"]
	guardWorld(t, lastSlotBefore(g, time.Now()))
	code, out := guardRun(t, "ats")
	if code != 0 || !strings.Contains(out, "ATS collect (ats): due now, planned at ") ||
		!strings.Contains(out, "No run recorded yet") || !strings.Contains(out, "Nothing was written") {
		t.Errorf("exit %d, out:\n%s", code, out)
	}
	if n := runCount(t, `SELECT count(*) FROM run`); n != 0 {
		t.Errorf("%d rows written by a check by hand, want 0", n)
	}
}

// TestTimerGuardLockDeferred: contacts due while the probe's fetch row is open waits (exit 3)
// with its line, and records nothing.
func TestTimerGuardLockDeferred(t *testing.T) {
	now := time.Now()
	guardWorld(t, now)
	plan := now.Round(tick)
	insertRun(t, commandScheduled, "leads-contacts", plan.AddDate(0, 0, -1), tp(plan.AddDate(0, 0, -1).Add(time.Hour)),
		notePlanned+plan.AddDate(0, 0, -1).In(planZone).Format(time.RFC3339)+noteEnded+"success")
	insertRun(t, commandLeadsSitesProbe, "", now.Add(-time.Hour), nil, "")
	code, out := guardRun(t, "--record", "leads-contacts")
	if code != 3 || !strings.Contains(out, "Contacts: planned at ") || !strings.HasSuffix(out, "; waiting while the site probe runs.\n") {
		t.Errorf("exit %d, out %q; want 3 and the deferred line", code, out)
	}
	if n := len(startRows(t, "leads-contacts")); n != 1 {
		t.Errorf("%d contacts start rows, want the seeded one only", n)
	}
}

// TestTimerGuardEndedRecordsLostPress: a press that ran while its start could not be recorded
// is written by --ended as a finished run-now row at the press, and the next tick starts
// nothing.
func TestTimerGuardEndedRecordsLostPress(t *testing.T) {
	now := time.Now()
	dir := guardWorld(t, now)
	r := now.Add(-3 * time.Minute)
	if err := writeRunRequest(dir, "leads-probe", r); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SERVICE_RESULT", "success")
	if code, _ := guardRun(t, "--record", "--ended", "leads-probe"); code != 0 {
		t.Fatalf("--ended: exit %d", code)
	}
	rows := startRows(t, "leads-probe")
	if len(rows) != 1 || rows[0].Command != commandRunNow || rows[0].FinishedAt == nil ||
		!rows[0].StartedAt.Equal(r.Truncate(time.Microsecond)) || !strings.Contains(rows[0].Note, "start not recorded · ended success") {
		t.Fatalf("rows %+v, want one finished run-now row at the press", rows)
	}
	t.Setenv("SERVICE_RESULT", "")
	if code, _ := guardRun(t, "--record", "leads-probe"); code != 3 {
		t.Errorf("the next tick: exit %d, want 3", code)
	}
	if n := len(startRows(t, "leads-probe")); n != 1 {
		t.Errorf("%d start rows, want 1", n)
	}
}

// TestTimerGuardDatabaseUnreachable: the unit's check waits when the database cannot be
// reached, and runs a fresh press anyway.
func TestTimerGuardDatabaseUnreachable(t *testing.T) {
	now := time.Now()
	dir := guardWorld(t, now)
	t.Setenv("LEADHOUND_DATABASE_URL", "postgres://nobody@127.0.0.1:1/none?sslmode=disable&connect_timeout=1")
	if code, _ := guardRun(t, "--record", "collect"); code != 3 {
		t.Errorf("no press: exit %d, want 3", code)
	}
	if err := writeRunRequest(dir, "fetch", now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if code, _ := guardRun(t, "--record", "collect"); code != 0 {
		t.Errorf("a fresh press: exit %d, want 0", code)
	}
}

// TestTimerGuardContactsProbeConcurrent: contacts and the probe due on the same tick, deciding
// at the same moment: at most one opens a scheduled row, and the other defers behind it.
func TestTimerGuardContactsProbeConcurrent(t *testing.T) {
	for i := range 5 {
		now := time.Now()
		guardWorld(t, now)
		plan := now.Round(tick)
		for _, job := range []string{"leads-contacts", "leads-probe"} {
			insertRun(t, commandScheduled, job, plan.AddDate(0, 0, -1), tp(plan.AddDate(0, 0, -1).Add(time.Hour)),
				notePlanned+plan.AddDate(0, 0, -1).In(planZone).Format(time.RFC3339)+noteEnded+"success")
		}
		ctx := context.Background()
		var wg sync.WaitGroup
		verdicts := map[string]string{}
		var mu sync.Mutex
		for _, job := range []string{"leads-contacts", "leads-probe"} {
			s, err := store.Open(ctx, storetest.AppDSN())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(s.Close)
			wg.Go(func() {
				var out bytes.Buffer
				v, _, err := guardRecord(ctx, s, job, guardJobs[job], nil, now, &out)
				if err != nil {
					t.Errorf("%s: %v", job, err)
				}
				mu.Lock()
				verdicts[job] = v
				mu.Unlock()
			})
		}
		wg.Wait()
		n := runCount(t, `SELECT count(*) FROM run WHERE command = $1 AND finished_at IS NULL`, commandScheduled)
		due, deferred := 0, 0
		for _, v := range verdicts {
			switch v {
			case verdictDue:
				due++
			case verdictDeferred:
				deferred++
			}
		}
		if n != 1 || due != 1 || deferred != 1 {
			t.Fatalf("round %d: %d new scheduled rows, verdicts %v; want one due and one deferred", i, n, verdicts)
		}
	}
}
