package main

import (
	"bytes"
	"context"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/console"
	"github.com/gremy/webbership-leadhound/internal/linkedin"
	"github.com/gremy/webbership-leadhound/internal/store"
)

// bt is a wall-clock time in 2026 in the timers' zone.
func bt(m time.Month, d, h, mm int) time.Time {
	return time.Date(2026, m, d, h, mm, 0, 0, planZone)
}

// startRun is a start row of job: a press (note "asked at <started>") or a scheduled run
// planned at plan.
func startRun(id int64, command, job string, started time.Time, finished *time.Time, plan time.Time) store.Run {
	note := noteAskedAt + started.Format(time.RFC3339)
	if command == commandScheduled {
		note = notePlanned + plan.Format(time.RFC3339)
	}
	return store.Run{ID: id, Command: command, Source: job, StartedAt: started, FinishedAt: finished, Note: note}
}

func stepRun(id int64, command string, started time.Time, finished *time.Time) store.Run {
	return store.Run{ID: id, Command: command, StartedAt: started, FinishedAt: finished}
}

// captureGuardLog points the default logger at a buffer for the test.
func captureGuardLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	saved := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(saved) })
	return &buf
}

// TestTimerGuardJobs: every guarded job's slots read, sit on the tick grid, and its rhythm is
// one of daily or every; the two that share the fetch lock name each other.
func TestTimerGuardJobs(t *testing.T) {
	want := []string{"ats", "collect", "leads-batch", "leads-contacts", "leads-probe"}
	var got []string
	for key, g := range guardJobs {
		got = append(got, key)
		if (g.days > 0) == (g.every > 0) {
			t.Errorf("%s: days %d every %v, want exactly one", key, g.days, g.every)
		}
		for _, s := range g.slots {
			if at := slotOn(bt(11, 2, 0, 0), 0, s); !at.Equal(at.Round(tick)) {
				t.Errorf("%s: slot %s is off the %v grid", key, s, tick)
			}
		}
		if g.legacy == nil || g.legacyCommand == "" || g.limit <= 0 || g.name == "" || len(g.actions) == 0 {
			t.Errorf("%s: incomplete %+v", key, g)
		}
		if g.lockJob != "" {
			other := guardJobs[g.lockJob]
			if other.lockJob != key || len(g.lockedBy) == 0 {
				t.Errorf("%s and %s do not name each other", key, g.lockJob)
			}
			for _, c := range g.lockedBy {
				if !slices.Contains(other.steps, c) {
					t.Errorf("%s: lock step %s is not a step of %s", key, c, g.lockJob)
				}
			}
		}
	}
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Errorf("guarded jobs %v, want %v (deploy/units_test.go TestDueChecks lists the same)", got, want)
	}
	if timerGuardLockKey == settingsLockKey || timerGuardLockKey == rateLockKey || timerGuardLockKey == scoreLockKey {
		t.Error("timerGuardLockKey collides with another advisory lock key")
	}
}

// TestPlannedNext: the plan from no history, a legacy anchor, a press and a scheduled run, its
// rounding, its long-idle reset, DST, and its zone.
func TestPlannedNext(t *testing.T) {
	contacts, probe, collect := guardJobs["leads-contacts"], guardJobs["leads-probe"], guardJobs["collect"]
	day := func(h, m int) time.Time { return bt(11, 2, h, m) }
	cases := []struct {
		name   string
		g      guardJob
		anchor *time.Time
		now    time.Time
		want   time.Time
	}{
		{"no history before the slot", contacts, nil, day(9, 40), day(10, 0)},
		{"no history at the slot", contacts, nil, day(10, 0), day(10, 0)},
		{"no history just after the slot", contacts, nil, day(10, 5), day(10, 0)},
		{"no history past the slot", contacts, nil, day(10, 20), bt(11, 3, 10, 0)},
		{"pressed at 15:02", contacts, tp(day(15, 2)), day(16, 0), bt(11, 3, 15, 0)},
		{"collect from 12:10", collect, tp(day(12, 10)), day(13, 0), bt(11, 3, 0, 15)},
		{"an anchor three intervals old", contacts, tp(day(14, 0).AddDate(0, 0, -3)), day(14, 0), bt(11, 3, 10, 0)},
		{"no history, collect after its morning slot", collect, nil, day(7, 0), day(18, 30)},
		{"spring forward keeps 10:00", contacts, tp(bt(3, 28, 10, 0)), bt(3, 28, 12, 0), bt(3, 29, 10, 0)},
		{"fall back keeps 10:00", contacts, tp(bt(10, 24, 10, 0)), bt(10, 24, 12, 0), bt(10, 25, 10, 0)},
	}
	for _, c := range cases {
		if got := plannedNext(c.g, c.anchor, c.now); !got.Equal(c.want) {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}

	// A legacy contacts run yesterday at 10:04 anchors on 10:00, and the run is due today at
	// 10:00 and still due at 10:30 (a missed slot or a deferral is due at once, never tomorrow).
	legacy := stepRun(1, commandLeadsContacts, bt(11, 1, 10, 4), tp(bt(11, 1, 12, 0)))
	a := anchorOf(contacts, nil, &legacy)
	if a == nil || !a.Equal(bt(11, 1, 10, 0)) {
		t.Fatalf("legacy anchor %v, want yesterday 10:00", a)
	}
	for _, now := range []time.Time{day(9, 55), day(10, 0), day(10, 30), day(17, 0)} {
		if v, plan := guardDecision(contacts, nil, nil, a, false, now); v != verdictDue || !plan.Equal(day(10, 0)) {
			t.Errorf("legacy anchor at %v: %s %v, want due 10:00", now, v, plan)
		}
	}
	if v, _ := guardDecision(contacts, nil, nil, a, false, day(9, 50)); v != verdictWait {
		t.Errorf("legacy anchor at 09:50: %s, want wait", v)
	}

	// A scheduled probe planned at 13:00 that started at 15:40 after a deferral: 13:00 tomorrow.
	s := startRun(1, commandScheduled, "leads-probe", day(15, 40), tp(day(16, 0)), day(13, 0))
	if got := plannedNext(probe, anchorOf(probe, []store.Run{s}, nil), day(17, 0)); !got.Equal(bt(11, 3, 13, 0)) {
		t.Errorf("deferred scheduled run: %v, want 13:00 tomorrow", got)
	}

	// 30 days of scheduled starts, each 0-3 minutes late: every plan stays 10:00.
	var starts []store.Run
	plan := day(10, 0)
	for i := range 30 {
		started := plan.Add(time.Duration(i%4) * time.Minute)
		starts = append(starts, startRun(int64(i+1), commandScheduled, "leads-contacts", started, tp(started.Add(time.Hour)), plan))
		next := plannedNext(contacts, anchorOf(contacts, starts, nil), started.Add(2*time.Hour))
		if want := plan.AddDate(0, 0, 1); !next.Equal(want) || next.Hour() != 10 || next.Minute() != 0 {
			t.Fatalf("day %d: plan %v, want %v", i, next, want)
		}
		plan = next
	}

	// The zone is the timers', whatever LEADHOUND_TZ says.
	t.Setenv("LEADHOUND_TZ", "America/New_York")
	got := plannedNext(contacts, nil, day(9, 0))
	if got.Location().String() != "Europe/Bucharest" || got.Hour() != 10 || !got.Equal(day(10, 0)) {
		t.Errorf("plan %v in %s, want 10:00 Europe/Bucharest", got, got.Location())
	}
}

// TestAnchorOf: a press counts at its start, a scheduled run at its noted plan (a bad note at
// its start), no start row → the legacy row's slot, nothing → nil.
func TestAnchorOf(t *testing.T) {
	g := guardJobs["leads-contacts"]
	press := startRun(1, commandRunNow, "leads-contacts", bt(11, 2, 15, 2), nil, time.Time{})
	sched := startRun(2, commandScheduled, "leads-contacts", bt(11, 3, 15, 40), nil, bt(11, 3, 15, 0))
	bad := sched
	bad.ID, bad.Note = 3, "planned soon · ended success"
	ended := sched
	ended.Note += noteEnded + "success"
	legacy := stepRun(9, commandLeadsContacts, bt(11, 1, 10, 8), nil)
	cases := []struct {
		name   string
		starts []store.Run
		legacy *store.Run
		want   *time.Time
	}{
		{"a press at its start", []store.Run{press}, &legacy, tp(press.StartedAt)},
		{"a scheduled run at its plan", []store.Run{press, sched}, &legacy, tp(bt(11, 3, 15, 0))},
		{"an ended scheduled run at its plan", []store.Run{press, ended}, nil, tp(bt(11, 3, 15, 0))},
		{"a bad note at its start", []store.Run{bad}, nil, tp(bad.StartedAt)},
		{"no start row: the legacy row", nil, &legacy, tp(bt(11, 1, 10, 0))},
		{"nothing", nil, nil, nil},
	}
	for _, c := range cases {
		got := anchorOf(g, c.starts, c.legacy)
		if (got == nil) != (c.want == nil) || got != nil && !got.Equal(*c.want) {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

// TestAnchorOfLegacyEveryJob: each job's first plan counts from the old slot its last legacy
// run belongs to, never from the run's own (delayed) time.
func TestAnchorOfLegacyEveryJob(t *testing.T) {
	d := func(h, m int) time.Time { return bt(11, 1, h, m) }
	cases := []struct {
		job        string
		row        store.Run
		want, next time.Time
	}{
		{"leads-contacts", stepRun(1, commandLeadsContacts, d(10, 8), nil), d(10, 0), bt(11, 2, 10, 0)},
		{"leads-probe", stepRun(1, commandLeadsSitesProbe, d(13, 9), nil), d(13, 0), bt(11, 2, 13, 0)},
		{"leads-batch", stepRun(1, commandLeadsBatch, d(7, 5), nil), d(6, 15), bt(11, 2, 6, 15)},
		{"ats", store.Run{ID: 1, Command: linkedin.CommandCollect, Source: "greenhouse", StartedAt: d(1, 6)}, d(1, 0), bt(11, 2, 1, 0)},
		{"collect", store.Run{ID: 1, Command: linkedin.CommandCollect, Source: linkedin.Source, StartedAt: d(14, 35)}, d(6, 30), d(18, 30)},
	}
	for _, c := range cases {
		g := guardJobs[c.job]
		if !g.legacy(c.row) || g.legacyCommand != c.row.Command {
			t.Fatalf("%s: %+v is not its legacy row", c.job, c.row)
		}
		a := anchorOf(g, nil, &c.row)
		if a == nil || !a.Equal(c.want) {
			t.Errorf("%s: anchor %v, want %v", c.job, a, c.want)
			continue
		}
		if next := plannedNext(g, a, d(15, 0)); !next.Equal(c.next) {
			t.Errorf("%s: next %v, want %v", c.job, next, c.next)
		}
	}
}

// TestPressServed: a start after the press, or a run going at it, serves it; a run that
// ended before it, a run closed as unrecorded, or another unit's rows never do.
func TestPressServed(t *testing.T) {
	d := func(h, m int) time.Time { return bt(11, 2, h, m) }
	collect, contacts, ats := guardJobs["collect"], guardJobs["leads-contacts"], guardJobs["ats"]

	if !pressServed(d(10, 0), []store.Run{startRun(1, commandRunNow, "leads-contacts", d(10, 1), nil, time.Time{})}) {
		t.Error("a start after the press does not serve it")
	}
	if pressServed(d(10, 0), []store.Run{startRun(1, commandScheduled, "leads-contacts", d(6, 0), tp(d(9, 0)), d(6, 0))}) {
		t.Error("a run that ended before the press serves it")
	}

	// The fetch joining a collect: collect 06:30-06:38, fetch pressed 06:33; the 06:45 tick
	// starts nothing.
	col := []store.Run{startRun(1, commandScheduled, "collect", d(6, 30), tp(d(6, 38)), d(6, 30))}
	reqs := map[string]*time.Time{console.RunFetch: tp(d(6, 33))}
	if !pressServed(d(6, 33), col) {
		t.Error("the collect running at the press does not serve it")
	}
	if v, _ := guardDecision(collect, reqs, func(r time.Time) bool { return pressServed(r, col) }, anchorOf(collect, col, nil), false, d(6, 45)); v != verdictWait {
		t.Errorf("06:45 after a joined collect: %s, want wait (no second collect)", v)
	}

	// A collect running while the ATS unit's score rows come and go: only the collect's own
	// row serves a fetch press; another unit's open score row never does.
	score := store.Run{ID: 2, Command: "score", StartedAt: d(6, 50), FinishedAt: tp(d(7, 30))}
	if pressServed(d(7, 0), []store.Run{score}) {
		t.Error("another unit's score row serves the press")
	}
	if !pressServed(d(7, 0), []store.Run{startRun(1, commandScheduled, "collect", d(6, 30), tp(d(7, 20)), d(6, 30)), score}) {
		t.Error("the collect's own row does not serve the press")
	}

	// An ATS run closed as unrecorded (its score timed out with no row) and a press two hours
	// later: not served, so the press runs.
	open := []store.Run{startRun(1, commandScheduled, "ats", d(1, 0), nil, d(1, 0))}
	dead := deadRuns(open, nil)
	if len(dead) != 1 || !dead[0].FinishedAt.Equal(d(1, 0)) || !strings.HasSuffix(dead[0].Note, " · ended unrecorded") {
		t.Fatalf("dead ATS run %+v", dead)
	}
	atsReqs := map[string]*time.Time{console.RunATS: tp(d(3, 0))}
	if pressServed(d(3, 0), dead) {
		t.Error("an unrecorded run serves a later press")
	}
	if v, _ := guardDecision(ats, atsReqs, func(r time.Time) bool { return pressServed(r, dead) }, anchorOf(ats, dead, nil), false, d(3, 5)); v != verdictPress {
		t.Errorf("press after an unrecorded run: %s, want press", v)
	}

	// Contacts 10:00-18:00, a batch run's leads-build row at 17:30, a contacts press at 17:50:
	// the running contacts row serves it, and the 18:00 tick starts no second run.
	run := []store.Run{startRun(1, commandScheduled, "leads-contacts", d(10, 0), tp(d(18, 0)), d(10, 0)),
		stepRun(2, commandLeadsBuild, d(17, 30), tp(d(17, 35)))}
	if !pressServed(d(17, 50), run) {
		t.Error("the running contacts row does not serve the press")
	}
	creqs := map[string]*time.Time{"leads-contacts": tp(d(17, 50))}
	if v, _ := guardDecision(contacts, creqs, func(r time.Time) bool { return pressServed(r, run) }, anchorOf(contacts, run, nil), false, d(18, 0)); v != verdictWait {
		t.Errorf("18:00 after a served press: %s, want wait", v)
	}
}

// TestPressFreshness: a request time ahead of the clock is never a press and gives one warning
// line; one half a minute old is a press.
func TestPressFreshness(t *testing.T) {
	g := guardJobs["leads-batch"]
	now := bt(11, 2, 12, 0)
	never := func(time.Time) bool { return false }
	ahead := map[string]*time.Time{"leads-batch": tp(now.Add(10 * time.Minute))}
	if v, _ := guardDecision(g, ahead, never, tp(now.Add(-time.Hour)), false, now); v == verdictPress {
		t.Error("a request 10 minutes ahead is a press")
	}
	if got := requestsAhead(g, ahead, now); len(got) != 1 {
		t.Errorf("requestsAhead = %v, want one", got)
	}
	if v, _ := guardDecision(g, map[string]*time.Time{"leads-batch": tp(now.Add(-30 * time.Second))}, never, tp(now.Add(-time.Hour)), false, now); v != verdictPress {
		t.Errorf("a request 30 s old: %s, want press", v)
	}
	if v, _ := guardDecision(g, map[string]*time.Time{"leads-batch": tp(now.Add(-pressFresh - time.Minute))}, never, tp(now.Add(-time.Hour)), false, now); v != verdictWait {
		t.Errorf("a stale request: %s, want wait", v)
	}

	// Through the command: the database is not there, so the check waits, with exactly one
	// line about the request ahead of the clock.
	dir := t.TempDir()
	if err := writeRunRequest(dir, "leads-batch", now.Add(10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LEADHOUND_DATA_DIR", dir)
	t.Setenv("LEADHOUND_DATABASE_URL", "postgres://nobody@127.0.0.1:1/none?sslmode=disable&connect_timeout=1")
	t.Setenv("INVOCATION_ID", "")
	saved := guardClock
	guardClock = func() time.Time { return now }
	t.Cleanup(func() { guardClock = saved })
	logs := captureGuardLog(t)
	captureStdout(t)
	if code := run(context.Background(), []string{"timer-guard", "--record", "leads-batch"}, &bytes.Buffer{}); code != 3 {
		t.Errorf("exit %d, want 3", code)
	}
	if n := strings.Count(logs.String(), "ahead of the clock"); n != 1 {
		t.Errorf("%d lines about the request ahead, want 1:\n%s", n, logs)
	}
}

// TestTimerGuardDatabaseDown: with the database unreachable the check waits, except for a
// fresh press, which runs.
func TestTimerGuardDatabaseDown(t *testing.T) {
	dir := t.TempDir()
	now := bt(11, 2, 12, 0)
	t.Setenv("LEADHOUND_DATA_DIR", dir)
	t.Setenv("LEADHOUND_DATABASE_URL", "postgres://nobody@127.0.0.1:1/none?sslmode=disable&connect_timeout=1")
	t.Setenv("INVOCATION_ID", "")
	saved := guardClock
	guardClock = func() time.Time { return now }
	t.Cleanup(func() { guardClock = saved })
	captureGuardLog(t)
	captureStdout(t)
	if code := run(context.Background(), []string{"timer-guard", "--record", "leads-probe"}, &bytes.Buffer{}); code != 3 {
		t.Errorf("no press, database down: exit %d, want 3", code)
	}
	if err := writeRunRequest(dir, "leads-probe", now.Add(-2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if code := run(context.Background(), []string{"timer-guard", "--record", "leads-probe"}, &bytes.Buffer{}); code != 0 {
		t.Errorf("fresh press, database down: exit %d, want 0", code)
	}
	if code := run(context.Background(), []string{"timer-guard", "--record", "--ended", "leads-probe"}, &bytes.Buffer{}); code != 0 {
		t.Errorf("--ended, database down: exit %d, want 0", code)
	}
}

// TestTimerGuardEnded: --ended closes the latest open start row with systemd's result, does
// nothing after a passed-over tick or with no open row, and records a press that ran without
// its start row so the next tick does not run it again.
func TestTimerGuardEnded(t *testing.T) {
	g := guardJobs["leads-probe"]
	d := func(h, m int) time.Time { return bt(11, 2, h, m) }
	now := d(14, 0)
	open := guardRows{starts: []store.Run{
		startRun(1, commandScheduled, "leads-probe", d(13, 0).AddDate(0, 0, -1), tp(d(14, 0).AddDate(0, 0, -1)), d(13, 0).AddDate(0, 0, -1)),
		startRun(2, commandScheduled, "leads-probe", d(13, 0), nil, d(13, 0)),
	}}
	id, note, ins := endedAction(g, open, nil, "success", "success", now)
	if id != 2 || note != open.starts[1].Note+" · ended success" || ins != nil {
		t.Errorf("open row: close %d %q insert %v", id, note, ins)
	}
	env := map[string]string{"SERVICE_RESULT": "exit-code", "EXIT_STATUS": "1"}
	if got := endedResult(func(k string) string { return env[k] }); got != "exit-code exit 1" {
		t.Errorf("endedResult = %q", got)
	}
	env = map[string]string{"SERVICE_RESULT": "success", "EXIT_STATUS": "0"}
	if got := endedResult(func(k string) string { return env[k] }); got != "success" {
		t.Errorf("endedResult = %q", got)
	}
	if id, _, ins := endedAction(g, open, nil, "exec-condition", "exec-condition", now); id != 0 || ins != nil {
		t.Error("a passed-over tick closed or wrote something")
	}
	closed := guardRows{starts: open.starts[:1]}
	if id, _, ins := endedAction(g, closed, nil, "success", "success", now); id != 0 || ins != nil {
		t.Error("no open row, no press: something written")
	}

	// A press at 13:30 that ran while the database was down: a finished run-now row at the
	// press, and the next tick starts nothing.
	r := d(13, 30)
	reqs := map[string]*time.Time{"leads-probe": &r}
	if id, _, ins := endedAction(g, closed, reqs, "exec-condition", "exec-condition", now); id != 0 || ins != nil {
		t.Error("a passed-over tick recorded the press")
	}
	_, _, ins = endedAction(g, closed, reqs, "success", "success", now)
	if ins == nil || ins.Command != commandRunNow || !ins.StartedAt.Equal(r) || ins.FinishedAt == nil ||
		!strings.Contains(ins.Note, "start not recorded · ended success") {
		t.Fatalf("press without its start row: insert %+v", ins)
	}
	after := append(slices.Clone(closed.starts), *ins)
	after[len(after)-1].Source, after[len(after)-1].ID = "leads-probe", 3
	v, plan := guardDecision(g, reqs, func(x time.Time) bool { return pressServed(x, after) }, anchorOf(g, after, nil), false, d(14, 15))
	if v != verdictWait || !plan.Equal(bt(11, 3, 13, 30)) {
		t.Errorf("next tick after the recorded press: %s %v, want wait for 13:30 tomorrow", v, plan)
	}
	// A press already served by a start row is not recorded again.
	served := guardRows{starts: []store.Run{startRun(1, commandRunNow, "leads-probe", d(13, 31), tp(d(13, 50)), time.Time{})}}
	if _, _, ins := endedAction(g, served, reqs, "success", "success", now); ins != nil {
		t.Error("a served press was recorded again")
	}
}

// TestTimerGuardCloseDead: an open start row the check finds is closed as unrecorded, at its
// last step's end, before the decision; the decision then counts it as ended.
func TestTimerGuardCloseDead(t *testing.T) {
	g := guardJobs["leads-contacts"]
	d := func(h, m int) time.Time { return bt(11, 2, h, m) }
	starts := []store.Run{startRun(1, commandScheduled, "leads-contacts", d(10, 0), nil, d(10, 0))}
	steps := []store.Run{
		stepRun(2, commandLeadsContacts, d(10, 1), tp(d(11, 0))),
		stepRun(3, commandLeadsEnrich, d(11, 0), tp(d(11, 40))),
		stepRun(4, commandLeadsRate, d(11, 40), nil), // killed by the reboot
	}
	dead := deadRuns(starts, steps)
	if len(dead) != 1 || !dead[0].FinishedAt.Equal(d(11, 40)) || dead[0].Note != starts[0].Note+" · ended unrecorded" {
		t.Fatalf("deadRuns = %+v", dead)
	}
	if again := deadRuns(dead, steps); len(again) != 0 {
		t.Errorf("a closed row closed again: %+v", again)
	}
	// A press at 12:00 after the dead run is not served by it, so it runs.
	reqs := map[string]*time.Time{"leads-contacts": tp(d(12, 0))}
	if v, _ := guardDecision(g, reqs, func(r time.Time) bool { return pressServed(r, dead) }, anchorOf(g, dead, nil), false, d(12, 5)); v != verdictPress {
		t.Errorf("press after a dead run: %s, want press", v)
	}
	// A press at 11:00, while the dead run was still going, is served by it.
	if !pressServed(d(11, 0), dead) {
		t.Error("a press during the dead run is not served by it")
	}
}

// TestLockHeld: the probe holds contacts' lock while its fetch row is open and younger than
// its limit, or while its start row from the last 10 minutes has no finished fetch row yet.
func TestLockHeld(t *testing.T) {
	g := guardJobs["leads-contacts"]
	now := bt(11, 2, 13, 30)
	cases := []struct {
		name          string
		starts, steps []store.Run
		held          bool
	}{
		{"nothing", nil, nil, false},
		{"an open probe row", nil, []store.Run{stepRun(1, commandLeadsSitesProbe, now.Add(-time.Hour), nil)}, true},
		{"a finished probe row", nil, []store.Run{stepRun(1, commandLeadsSitesProbe, now.Add(-time.Hour), tp(now.Add(-time.Minute)))}, false},
		{"a probe row older than its limit", nil, []store.Run{stepRun(1, commandLeadsSitesProbe, now.Add(-g.lockLimit-time.Minute), nil)}, false},
		{"a probe start 3 minutes old, no step row yet",
			[]store.Run{startRun(1, commandScheduled, "leads-probe", now.Add(-3*time.Minute), nil, now)}, nil, true},
		{"a probe start 11 minutes old, no step row",
			[]store.Run{startRun(1, commandScheduled, "leads-probe", now.Add(-11*time.Minute), nil, now)}, nil, false},
		{"a probe start whose fetch already finished",
			[]store.Run{startRun(1, commandRunNow, "leads-probe", now.Add(-5*time.Minute), nil, time.Time{})},
			[]store.Run{stepRun(2, commandLeadsSitesProbe, now.Add(-4*time.Minute), tp(now.Add(-time.Minute)))}, false},
		{"a closed probe start", []store.Run{startRun(1, commandRunNow, "leads-probe", now.Add(-5*time.Minute), tp(now.Add(-4*time.Minute)), time.Time{})}, nil, false},
		{"another job's start", []store.Run{startRun(1, commandRunNow, "leads-batch", now.Add(-3*time.Minute), nil, time.Time{})}, nil, false},
		{"a contacts row (its own, not the lock holder's)", nil, []store.Run{stepRun(1, commandLeadsContacts, now.Add(-time.Hour), nil)}, false},
	}
	for _, c := range cases {
		if _, held := lockHeldBy(g, c.starts, c.steps, now); held != c.held {
			t.Errorf("%s: held %v, want %v", c.name, held, c.held)
		}
	}
	// The probe's view: an open contacts or enrich row holds it.
	p := guardJobs["leads-probe"]
	for _, c := range []string{commandLeadsContacts, commandLeadsEnrich} {
		if since, held := lockHeldBy(p, nil, []store.Run{stepRun(1, c, now.Add(-time.Hour), nil)}, now); !held || !since.Equal(now.Add(-time.Hour)) {
			t.Errorf("probe vs open %s: held %v since %v", c, held, since)
		}
	}
	if _, held := lockHeldBy(p, nil, []store.Run{stepRun(1, commandLeadsRate, now.Add(-time.Hour), nil)}, now); held {
		t.Error("contacts' rate step holds the fetch lock")
	}
	if _, held := lockHeldBy(guardJobs["collect"], nil, []store.Run{stepRun(1, commandLeadsSitesProbe, now, nil)}, now); held {
		t.Error("a job without a shared lock is held")
	}
}

// TestTimerGuardPressDeferredBehindLock: a contacts press while the probe holds the lock waits,
// and is still a press 40 minutes later once the lock is free.
func TestTimerGuardPressDeferredBehindLock(t *testing.T) {
	g := guardJobs["leads-contacts"]
	r := bt(11, 2, 13, 5)
	reqs := map[string]*time.Time{"leads-contacts": &r}
	anchor := tp(bt(11, 2, 10, 0))
	never := func(time.Time) bool { return false }
	if v, at := guardDecision(g, reqs, never, anchor, true, r.Add(time.Minute)); v != verdictDeferred || !at.Equal(r) {
		t.Errorf("press while the probe runs: %s %v, want deferred at the press", v, at)
	}
	if v, _ := guardDecision(g, reqs, never, anchor, false, r.Add(40*time.Minute)); v != verdictPress {
		t.Errorf("40 minutes later, lock free: %s, want press", v)
	}
	// A press older than the window is forgotten even so.
	if v, _ := guardDecision(g, reqs, never, anchor, false, r.Add(g.pressWindow()+time.Minute)); v != verdictWait {
		t.Errorf("past the window: %s, want wait", v)
	}
}

// TestGuardDecisionGrid: {no press, fresh unserved, served, stale} × {no start, last start
// under an interval ago, an interval ago} × {lock free, held} × {contacts, collect}.
func TestGuardDecisionGrid(t *testing.T) {
	for _, job := range []string{"leads-contacts", "collect"} {
		g := guardJobs[job]
		now := slotOn(bt(11, 2, 0, 0), 0, g.slots[0]) // on a slot, so "no start" is due
		for _, press := range []string{"none", "fresh", "served", "stale"} {
			for _, start := range []string{"none", "recent", "interval"} {
				for _, lock := range []bool{false, true} {
					reqs := map[string]*time.Time{}
					served := func(time.Time) bool { return press == "served" }
					switch press {
					case "fresh", "served":
						reqs[g.actions[0]] = tp(now.Add(-5 * time.Minute))
					case "stale":
						reqs[g.actions[0]] = tp(now.Add(-g.pressWindow() - time.Minute))
					}
					var anchor *time.Time
					switch start {
					case "recent":
						anchor = tp(now.Add(-g.interval() / 2))
					case "interval":
						anchor = tp(now.Add(-g.interval()))
					}
					held := lock && len(g.lockedBy) > 0
					want := verdictDue
					switch {
					case press == "fresh" && held:
						want = verdictDeferred
					case press == "fresh":
						want = verdictPress
					case start == "recent":
						want = verdictWait
					case held:
						want = verdictDeferred
					}
					if v, _ := guardDecision(g, reqs, served, anchor, lock, now); v != want {
						t.Errorf("%s press=%s start=%s lock=%v: %s, want %s", job, press, start, lock, v, want)
					}
				}
			}
		}
	}
}

// TestTimerGuardNoRecordUnderSystemd: a unit line that lost its --record (INVOCATION_ID set)
// waits (exit 3) before it reads anything.
func TestTimerGuardNoRecordUnderSystemd(t *testing.T) {
	t.Setenv("INVOCATION_ID", "0123456789abcdef")
	t.Setenv("LEADHOUND_DATABASE_URL", "postgres://nobody@127.0.0.1:1/none?sslmode=disable&connect_timeout=1")
	captureGuardLog(t)
	out := captureStdout(t)
	if code := run(context.Background(), []string{"timer-guard", "leads-probe"}, &bytes.Buffer{}); code != 3 {
		t.Errorf("exit %d, want 3", code)
	}
	if out.Len() != 0 {
		t.Errorf("printed %q", out)
	}
	if code := run(context.Background(), []string{"timer-guard", "--record", "nope"}, &bytes.Buffer{}); code != 3 {
		t.Errorf("bad job: exit %d, want 3", code)
	}
}

// simTick is one check of a simulated schedule.
type simTick struct {
	at   time.Time
	down bool
}

// simulate runs g's check at every tick from from for days, on rows it records itself: a
// started run lasts runFor (ticks while it runs are merged by systemd), presses add a check at
// their time (the watcher), a reboot drops the ticks while down and adds one at boot
// (Persistent=true), and lockHeld says when the job sharing the lock runs.
func simulate(g guardJob, job string, from time.Time, days int, presses []time.Time, lockHeld func(time.Time) bool,
	down func(time.Time) bool, boots []time.Time, runFor time.Duration) []store.Run {
	var checks []time.Time
	for at := from; at.Before(from.AddDate(0, 0, days)); at = at.Add(tick) {
		if !down(at) {
			checks = append(checks, at)
		}
	}
	checks = append(checks, presses...)
	checks = append(checks, boots...)
	slices.SortFunc(checks, func(a, b time.Time) int { return a.Compare(b) })
	var starts []store.Run
	for _, now := range checks {
		if last := latestStart(starts); last != nil && last.FinishedAt.After(now) {
			continue // the unit runs: systemd merges the start
		}
		reqs := map[string]*time.Time{}
		for _, p := range presses {
			if !p.After(now) {
				reqs[g.actions[0]] = tp(p)
			}
		}
		v, at := guardDecision(g, reqs, func(r time.Time) bool { return pressServed(r, starts) }, anchorOf(g, starts, nil), lockHeld(now), now)
		switch v {
		case verdictPress:
			starts = append(starts, startRun(int64(len(starts)+1), commandRunNow, job, now, tp(now.Add(runFor)), time.Time{}))
		case verdictDue:
			starts = append(starts, startRun(int64(len(starts)+1), commandScheduled, job, now, tp(now.Add(runFor)), at))
		}
	}
	return starts
}

// TestTimerGuardSweepColdDay: on a first day with no history, each job starts at exactly its
// legacy slots, and at no other tick.
func TestTimerGuardSweepColdDay(t *testing.T) {
	never := func(time.Time) bool { return false }
	from := bt(11, 2, 0, 0)
	for job, g := range guardJobs {
		starts := simulate(g, job, from, 1, nil, never, never, nil, 30*time.Minute)
		var got, want []time.Time
		for _, s := range starts {
			got = append(got, s.StartedAt)
			if s.Command != commandScheduled {
				t.Errorf("%s: a %s start on a cold day", job, s.Command)
			}
		}
		for _, s := range g.slots {
			want = append(want, slotOn(from, 0, s))
		}
		if !slices.EqualFunc(got, want, time.Time.Equal) {
			t.Errorf("%s: cold-day starts %v, want %v", job, got, want)
		}
	}
}

// TestTimerGuardSweepWeek: a week of contacts with a press on day 2 at 15:02, a deferral on
// day 4 and a reboot over the slot on day 5: one start per interval, none skipped, none
// repeated, and the rhythm moves only with the press.
func TestTimerGuardSweepWeek(t *testing.T) {
	g := guardJobs["leads-contacts"]
	day := func(n, h, m int) time.Time { return bt(11, 2+n, h, m) }
	press := day(2, 15, 2)
	lockHeld := func(t time.Time) bool { return !t.Before(day(4, 14, 50)) && t.Before(day(4, 17, 0)) }
	down := func(t time.Time) bool { return !t.Before(day(5, 14, 30)) && t.Before(day(5, 16, 20)) }
	starts := simulate(g, "leads-contacts", day(0, 0, 0), 7, []time.Time{press}, lockHeld, down, []time.Time{day(5, 16, 20)}, 30*time.Minute)

	want := []struct {
		command string
		at      time.Time
		anchor  time.Time
	}{
		{commandScheduled, day(0, 10, 0), day(0, 10, 0)},
		{commandScheduled, day(1, 10, 0), day(1, 10, 0)},
		{commandScheduled, day(2, 10, 0), day(2, 10, 0)},
		{commandRunNow, press, press},
		{commandScheduled, day(3, 15, 0), day(3, 15, 0)},
		{commandScheduled, day(4, 17, 0), day(4, 15, 0)},  // deferred behind the probe
		{commandScheduled, day(5, 16, 20), day(5, 15, 0)}, // caught up at boot
		{commandScheduled, day(6, 15, 0), day(6, 15, 0)},
	}
	if len(starts) != len(want) {
		t.Fatalf("%d starts, want %d: %v", len(starts), len(want), starts)
	}
	presses := 0
	for i, s := range starts {
		w := want[i]
		if s.Command != w.command || !s.StartedAt.Equal(w.at) || !startAnchor(s).Equal(w.anchor) {
			t.Errorf("start %d: %s at %v anchored %v, want %s at %v anchored %v",
				i, s.Command, s.StartedAt, startAnchor(s), w.command, w.at, w.anchor)
		}
		if s.Command == commandRunNow {
			presses++
			continue
		}
		if i == 0 {
			continue
		}
		gap := startAnchor(s).Sub(startAnchor(starts[i-1]))
		if gap < g.interval()-tick-time.Hour || gap > g.interval()+tick/2+time.Hour { // an hour either way only across DST
			t.Errorf("start %d: %v after the previous anchor", i, gap)
		}
	}
	if presses != 1 {
		t.Errorf("%d starts for one press, want 1", presses)
	}
}

// TestTimerGuardRunTableSweep: no reader of the run table takes a start row (run-now or
// scheduled) for one of its own: the Run now card, the LinkedIn facts and holder, the anchors,
// the failure note, and the commands the Controls and Status pages read.
func TestTimerGuardRunTableSweep(t *testing.T) {
	now := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	at := func(h, m int) time.Time { return time.Date(2026, 11, 2, h, m, 0, 0, time.UTC) }
	base := []store.Run{
		{ID: 1, Command: linkedin.CommandCollect, Source: linkedin.Source, StartedAt: at(4, 30), FinishedAt: tp(at(4, 40)), Requests: 80},
		{ID: 2, Command: "score", StartedAt: at(4, 41), FinishedAt: tp(at(4, 50))},
		{ID: 3, Command: "preview", Source: previewFetchSource, StartedAt: at(4, 51), FinishedAt: tp(at(4, 52))},
		{ID: 4, Command: linkedin.CommandCollect, Source: "greenhouse", StartedAt: at(0, 0), FinishedAt: tp(at(1, 0))},
		{ID: 5, Command: "brief", StartedAt: at(5, 50), FinishedAt: tp(at(5, 51))},
		{ID: 6, Command: "preview", StartedAt: at(6, 0), FinishedAt: tp(at(6, 1))},
		{ID: 7, Command: linkedin.CommandCollect, Source: linkedin.Source, StartedAt: at(8, 30)}, // running
	}
	extra := []store.Run{
		startRun(10, commandScheduled, "collect", at(8, 30), nil, at(8, 30)),
		startRun(11, commandRunNow, "ats", at(0, 0), tp(at(1, 30)), time.Time{}),
		startRun(12, commandScheduled, "leads-contacts", at(7, 0), tp(at(8, 0)), at(7, 0)),
		startRun(13, commandRunNow, "collect", at(8, 45), nil, time.Time{}),
	}
	extra[1].Note += noteEnded + "success"
	extra[2].Note += noteEnded + noteUnrecorded
	with := slices.Concat(base, extra)

	for _, a := range console.RunActions {
		for _, req := range []*time.Time{nil, tp(at(8, 40))} {
			if got, want := runNowState(a, req, with, now, runNowLimits[a]), runNowState(a, req, base, now, runNowLimits[a]); !reflect.DeepEqual(got, want) {
				t.Errorf("runNowState(%s, %v) = %+v with start rows, %+v without", a, req, got, want)
			}
		}
		gw, gok := runAnchor(a, with)
		bw, bok := runAnchor(a, base)
		if gok != bok || !gw.Equal(bw) {
			t.Errorf("runAnchor(%s) = %v %v with start rows, %v %v without", a, gw, gok, bw, bok)
		}
		w1, s1, ok1 := linkedInHolder(a, with, now)
		w2, s2, ok2 := linkedInHolder(a, base, now)
		if w1 != w2 || !s1.Equal(s2) || ok1 != ok2 {
			t.Errorf("linkedInHolder(%s) differs with start rows", a)
		}
	}
	if got, want := collectFacts(with, now), collectFacts(base, now); !reflect.DeepEqual(got, want) {
		t.Errorf("collectFacts = %+v with start rows, %+v without", got, want)
	}
	for _, r := range extra {
		if runNoteFailed(r) {
			t.Errorf("runNoteFailed(%q) is true", r.Note)
		}
		if isLinkedInCollect(r) || isATSCollect(r) || isScore(r) {
			t.Errorf("%s/%s reads as a collect or score row", r.Command, r.Source)
		}
		for _, u := range statusUnits {
			if u.runs != nil && u.runs(r) {
				t.Errorf("Status unit %s takes the %s row as its run", u.name, r.Command)
			}
		}
		if v := runView(r, now); v.Failed && r.FinishedAt != nil {
			t.Errorf("runView(%q) fails", r.Note)
		}
	}
	lists := [][]string{statusCommands, controlSending.commands}
	for _, j := range controlJobs {
		lists = append(lists, j.commands)
	}
	for _, l := range lists {
		if slices.Contains(l, commandRunNow) || slices.Contains(l, commandScheduled) {
			t.Errorf("a page reads the start rows' command: %v", l)
		}
	}
}
