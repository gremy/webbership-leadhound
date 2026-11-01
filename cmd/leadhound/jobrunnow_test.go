package main

import (
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/console"
	"github.com/gremy/webbership-leadhound/internal/store"
)

// closedRun is r closed by its unit at fin with result.
func closedRun(r store.Run, fin time.Time, result string) store.Run {
	r.FinishedAt = &fin
	r.Note += noteEnded + result
	return r
}

// TestJobRunUnits: the Run now rows are exactly console.RunNowJobs, each on its guarded job's
// own units and request action, every step the guard names has its words, and none of the
// actions is one a switch-on writes (runRequestJobs), which would start a long job unasked.
func TestJobRunUnits(t *testing.T) {
	if got := slices.Sorted(maps.Keys(jobRunUnits)); !slices.Equal(got, slices.Sorted(slices.Values(console.RunNowJobs))) {
		t.Fatalf("Run now rows %v, want %v", got, console.RunNowJobs)
	}
	for job, u := range jobRunUnits {
		g, ok := guardJobs[u.guardKey]
		if !ok {
			t.Fatalf("%s: %q is not a guarded job", job, u.guardKey)
		}
		if u.service != guardService(u.guardKey) || u.path != strings.TrimSuffix(u.service, ".service")+".path" ||
			!slices.Equal(g.actions, []string{u.action}) || u.switchJob != job || u.confirm == "" {
			t.Errorf("%s: %+v", job, u)
		}
		for _, s := range g.steps {
			if u.steps[s] == "" {
				t.Errorf("%s: step %s has no words", job, s)
			}
		}
		if len(u.steps) != len(g.steps) {
			t.Errorf("%s: words for %v, the guard's steps are %v", job, slices.Sorted(maps.Keys(u.steps)), g.steps)
		}
		if _, ok := runRequestJobs[u.action]; ok {
			t.Errorf("%s: %s is in runRequestJobs, so switching the job on would start it", job, u.action)
		}
		if (u.holderLine != "") != (g.lockJob != "") {
			t.Errorf("%s: holder line %q for lock job %q", job, u.holderLine, g.lockJob)
		}
		for _, unit := range []string{u.service, u.path} {
			if !slices.Contains(controlUnits, unit) {
				t.Errorf("%s: Controls does not ask systemd about %s", job, unit)
			}
		}
	}
	var timers []string
	for key := range guardJobs {
		if k, ok := guardKeyOfTimer(guardTimer(key)); !ok || k != key {
			t.Errorf("guardKeyOfTimer(%s) = %q, %v", guardTimer(key), k, ok)
		}
		timers = append(timers, guardTimer(key))
	}
	for _, u := range []string{unitSync, unitSend, unitResearch, "leadhound-brief.timer"} {
		if _, ok := guardKeyOfTimer(u); ok {
			t.Errorf("%s is not guarded", u)
		}
	}
	for _, a := range console.RunActions {
		key, ok := guardKeyOfAction(a)
		if want := map[string]string{console.RunATS: "ats", console.RunFetch: "collect"}[a]; key != want || ok != (want != "") {
			t.Errorf("guardKeyOfAction(%s) = %q, %v; want %q", a, key, ok, want)
		}
	}
	if len(timers) != 5 {
		t.Errorf("%d guarded timers", len(timers))
	}
}

// TestJobRunNowState: the row's grid, read from the job's own start rows.
func TestJobRunNowState(t *testing.T) {
	u := jobRunUnits[console.JobContacts]
	now := bt(10, 27, 12, 0)
	p := func(t time.Time) *time.Time { return &t }
	open := startRun(1, commandScheduled, "leads-contacts", bt(10, 27, 10, 0), nil, bt(10, 27, 10, 0))
	pressRow := startRun(2, commandRunNow, "leads-contacts", bt(10, 27, 11, 2), nil, time.Time{})
	for _, tc := range []struct {
		name           string
		req            *time.Time
		starts, steps  []store.Run
		starting, read bool
		state, line    string
		busy           string
	}{
		{name: "systemd not read", read: false, starts: []store.Run{open}, starting: true, state: "idle",
			line: "Run now needs the server's systemd, which could not be asked just now."},
		{name: "never ran", read: true, state: "idle"},
		{name: "a passed-over tick: no row, the service momentarily starting", read: true, starting: true, state: "idle"},
		{name: "running, scheduled run, on a step", read: true, starting: true, starts: []store.Run{open},
			steps: []store.Run{stepRun(10, commandLeadsContacts, bt(10, 27, 10, 1), p(bt(10, 27, 11, 0))), stepRun(11, commandLeadsEnrich, bt(10, 27, 11, 1), nil)},
			state: "running", line: "Running since 10:00: reading the sites' own pages.", busy: "Already running since 10:00."},
		{name: "running, between two steps", read: true, starting: true, starts: []store.Run{open},
			steps: []store.Run{stepRun(10, commandLeadsContacts, bt(10, 27, 10, 1), p(bt(10, 27, 11, 0)))},
			state: "running", line: "Running since 10:00: between two steps.", busy: "Already running since 10:00."},
		{name: "running, a step row of an earlier run left open is not this run's", read: true, starting: true, starts: []store.Run{open},
			steps: []store.Run{stepRun(10, commandLeadsRate, bt(10, 26, 10, 1), nil)},
			state: "running", line: "Running since 10:00: between two steps.", busy: "Already running since 10:00."},
		{name: "running, merged press", read: true, starting: true, starts: []store.Run{open}, req: p(bt(10, 27, 10, 30)),
			state: "running", line: "Running since 10:00: between two steps. Asked at 10:30 while a run was already going; that run covers it.",
			busy: "Already running since 10:00."},
		{name: "a press merged into a tick's start: same second", read: true, starting: true, starts: []store.Run{open}, req: p(bt(10, 27, 10, 0)),
			state: "running", line: "Running since 10:00: between two steps.", busy: "Already running since 10:00."},
		{name: "open but not starting: did not finish", read: true, starts: []store.Run{open},
			state: "bad", line: "The last run did not finish (no end was recorded)."},
		{name: "open and dead, a press after it waits", read: true, starts: []store.Run{open}, req: p(bt(10, 27, 11, 50)),
			state: "waiting", line: "Asked at 11:50, waiting to start.", busy: "Already asked at 11:50."},
		{name: "waiting", read: true, req: p(bt(10, 27, 11, 45)), state: "waiting", line: "Asked at 11:45, waiting to start.", busy: "Already asked at 11:45."},
		{name: "stuck", read: true, req: p(bt(10, 27, 11, 30)), state: "stuck", line: u.stuckWords("11:30")},
		{name: "ahead of the clock", read: true, req: p(bt(10, 27, 12, 10)), state: "idle"},
		{name: "press, its run running", read: true, starting: true, starts: []store.Run{pressRow}, req: p(bt(10, 27, 11, 2)),
			state: "running", line: "Running since 11:02: between two steps.", busy: "Already running since 11:02."},
		{name: "press, its run ended", read: true, starts: []store.Run{closedRun(pressRow, bt(10, 27, 11, 40), "success")}, req: p(bt(10, 27, 11, 2)),
			state: "done", line: "The run asked at 11:02 ended at 11:40. Each step's result is in Last run above."},
		{name: "press served by a scheduled start after it", read: true,
			starts: []store.Run{closedRun(startRun(3, commandScheduled, "leads-contacts", bt(10, 27, 11, 15), nil, bt(10, 27, 10, 0)), bt(10, 27, 11, 40), "success")},
			req:    p(bt(10, 27, 11, 5)), state: "done", line: "The run asked at 11:05 ended at 11:40. Each step's result is in Last run above."},
		{name: "press, its run timed out", read: true, starts: []store.Run{closedRun(pressRow, bt(10, 27, 11, 40), "timeout")}, req: p(bt(10, 27, 11, 2)),
			state: "bad", line: "The run asked at 11:02 did not finish (timeout). See journalctl -u leadhound-leads-contacts.service."},
		{name: "press, its run closed unrecorded", read: true, starts: []store.Run{closedRun(pressRow, bt(10, 27, 11, 40), noteUnrecorded)}, req: p(bt(10, 27, 11, 2)),
			steps: []store.Run{stepRun(12, commandLeadsContacts, bt(10, 27, 11, 3), p(bt(10, 27, 11, 40)))},
			state: "bad", line: "The run asked at 11:02 did not finish (unrecorded). See journalctl -u leadhound-leads-contacts.service."},
		// Its start row committed but no step of it ran (the commit errored after landing, the
		// check was killed after it, the unit was stopped before its job): the press is not
		// served; within pressFresh it waits for the next check, which runs it, then the row
		// says the run did not start.
		{name: "press, its run started nothing, still fresh", read: true, req: p(bt(10, 27, 11, 50)),
			starts: []store.Run{closedRun(startRun(6, commandRunNow, "leads-contacts", bt(10, 27, 11, 50), nil, time.Time{}), bt(10, 27, 11, 50), noteUnrecorded)},
			steps:  []store.Run{stepRun(12, commandLeadsContacts, bt(10, 27, 10, 1), p(bt(10, 27, 11, 0)))},
			state:  "waiting", line: "Asked at 11:50, waiting to start.", busy: "Already asked at 11:50."},
		{name: "press, its run started nothing", read: true, starts: []store.Run{closedRun(pressRow, bt(10, 27, 11, 2), noteUnrecorded)}, req: p(bt(10, 27, 11, 2)),
			state: "bad", line: "The run asked at 11:02 did not start: its start was recorded, but none of its steps ran. See journalctl -u leadhound-leads-contacts.service."},
		// While its ExecStopPost= records the end the service is deactivating and the row still
		// open: running, and a press is refused (serviceBusy), or it would count as served by
		// the run that is ending.
		{name: "running, the service stopping", read: true, starting: serviceBusy(unitState{Stopping: true}), starts: []store.Run{open},
			state: "running", line: "Running since 10:00: between two steps.", busy: "Already running since 10:00."},
		{name: "press, a failed step in its run", read: true, starts: []store.Run{closedRun(pressRow, bt(10, 27, 11, 40), "success")}, req: p(bt(10, 27, 11, 2)),
			steps: []store.Run{{ID: 12, Command: commandLeadsEnrich, StartedAt: bt(10, 27, 11, 10), FinishedAt: p(bt(10, 27, 11, 20)), Note: "failed: the store went away"}},
			state: "bad", line: "The run asked at 11:02 ended at 11:40. Some steps failed; see Last run above."},
		{name: "press, a failed step of another run", read: true, starts: []store.Run{closedRun(pressRow, bt(10, 27, 11, 40), "success")}, req: p(bt(10, 27, 11, 2)),
			steps: []store.Run{{ID: 12, Command: commandLeadsEnrich, StartedAt: bt(10, 27, 9, 10), FinishedAt: p(bt(10, 27, 9, 20)), Note: "failed: the store went away"}},
			state: "done", line: "The run asked at 11:02 ended at 11:40. Each step's result is in Last run above."},
		{name: "press served by a run already going", read: true, starts: []store.Run{closedRun(open, bt(10, 27, 11, 40), "success")}, req: p(bt(10, 27, 10, 30)),
			state: "done", line: "Asked at 10:30 while a run was already going; it ended at 11:40."},
		{name: "press, its run, then a scheduled run", read: true, req: p(bt(10, 27, 1, 2)),
			starts: []store.Run{closedRun(startRun(4, commandRunNow, "leads-contacts", bt(10, 27, 1, 2), nil, time.Time{}), bt(10, 27, 2, 0), "success"),
				closedRun(startRun(5, commandScheduled, "leads-contacts", bt(10, 27, 10, 0), nil, bt(10, 27, 10, 0)), bt(10, 27, 11, 0), "success")},
			state: "idle"},
		{name: "a run that ended before the press does not serve it", read: true, req: p(bt(10, 27, 11, 50)),
			starts: []store.Run{closedRun(open, bt(10, 27, 11, 0), "success")},
			state:  "waiting", line: "Asked at 11:50, waiting to start.", busy: "Already asked at 11:50."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, busy := jobRunNowState(u, tc.req, tc.starts, tc.steps, tc.starting, tc.read, now)
			if got.State != tc.state || got.Line != tc.line || busy != tc.busy {
				t.Errorf("got %+v, busy %q\nwant %s %q, busy %q", got, busy, tc.state, tc.line, tc.busy)
			}
			if got.CanAsk != !got.Pending || got.Confirm != u.confirm || got.Pending != (got.State == "running" || got.State == "waiting") {
				t.Errorf("CanAsk %v, Pending %v, Confirm %q", got.CanAsk, got.Pending, got.Confirm)
			}
		})
	}
}

// TestJobRunNowStateSweep: for every row, state and timing, CanAsk is !Pending, the confirm
// sentence is the row's, and a pending block always says something.
func TestJobRunNowStateSweep(t *testing.T) {
	now := bt(10, 27, 12, 0)
	for job, u := range jobRunUnits {
		for _, req := range []*time.Time{nil, new(now.Add(-time.Minute)), new(now.Add(-time.Hour)), new(now.Add(time.Hour))} {
			for _, starts := range [][]store.Run{nil,
				{startRun(1, commandRunNow, u.guardKey, now.Add(-2*time.Minute), nil, time.Time{})},
				{closedRun(startRun(1, commandScheduled, u.guardKey, now.Add(-3*time.Hour), nil, now.Add(-3*time.Hour)), now.Add(-time.Hour), "success")},
				{closedRun(startRun(1, commandRunNow, u.guardKey, now.Add(-30*time.Minute), nil, time.Time{}), now.Add(-5*time.Minute), "signal exit 9")},
			} {
				for _, starting := range []bool{false, true} {
					for _, read := range []bool{false, true} {
						got, busy := jobRunNowState(u, req, starts, nil, starting, read, now)
						if got.CanAsk != !got.Pending || got.Confirm != u.confirm || (got.Pending && got.Line == "") || (got.Pending != (busy != "")) {
							t.Errorf("%s req %v starts %v starting %v read %v: %+v busy %q", job, req, starts, starting, read, got, busy)
						}
					}
				}
			}
		}
	}
}

// TestJobRunNowDecision: the press's refusals in order, each with its sentence, and the
// sentence of a press that goes ahead.
func TestJobRunNowDecision(t *testing.T) {
	now := bt(10, 27, 12, 0)
	armed := unitState{Loaded: true, Enabled: true, Active: true}
	ok := jobRunFacts{read: true, path: armed}
	since := bt(10, 27, 11, 0)
	for _, tc := range []struct {
		name, job string
		confirmed bool
		busy      string
		f         jobRunFacts
		want      string
		refused   bool
	}{
		{"systemd not read", console.JobContacts, true, "Already running since 10:00.", jobRunFacts{off: true}, "The server's systemd could not be asked, so Run now cannot tell whether the job is already running. Try again in a moment.", true},
		{"already running", console.JobContacts, true, "Already running since 10:00.", jobRunFacts{read: true, off: true}, "Already running since 10:00.", true},
		{"already asked", console.JobProbe, false, "Already asked at 11:50.", ok, "Already asked at 11:50.", true},
		{"switched off", console.JobProbe, true, "", jobRunFacts{read: true, off: true}, "Site probe is switched off on this page; switch it on first.", true},
		{"watcher not armed", console.JobBatch, true, "", jobRunFacts{read: true, path: unitState{Loaded: true}}, "The Morning batch Run now watcher is not armed on the server. It goes with the Morning batch timer (deploy/README.md, Leads on the server).", true},
		{"watcher unknown", console.JobBatch, true, "", jobRunFacts{read: true}, "The Morning batch Run now watcher is not armed on the server. It goes with the Morning batch timer (deploy/README.md, Leads on the server).", true},
		{"watcher failed", console.JobContacts, true, "", jobRunFacts{read: true, path: unitState{Loaded: true, Enabled: true, Failed: true}},
			"The Contacts Run now watcher has stopped on the server. See deploy/README.md, Run now (systemctl reset-failed leadhound-leads-contacts.path leadhound-leads-contacts.service, then systemctl start leadhound-leads-contacts.path).", true},
		{"watcher stopped", console.JobContacts, true, "", jobRunFacts{read: true, path: unitState{Loaded: true, Enabled: true}},
			"The Contacts Run now watcher has stopped on the server. See deploy/README.md, Run now (systemctl reset-failed leadhound-leads-contacts.path leadhound-leads-contacts.service, then systemctl start leadhound-leads-contacts.path).", true},
		{"contacts while the probe holds the lock", console.JobContacts, true, "", jobRunFacts{read: true, path: armed, holder: "leads-probe", holderSince: &since},
			"The site probe is running since 11:00 and holds the sites' fetch lock, so the contact search would skip. Press again once it has finished.", true},
		{"the probe while contacts holds the lock", console.JobProbe, true, "", jobRunFacts{read: true, path: armed, holder: "leads-contacts", holderSince: &since},
			"The contacts job is running since 11:00 and holds the sites' fetch lock. Press again once it has finished.", true},
		{"batch waiting", console.JobBatch, true, "", jobRunFacts{read: true, path: armed, batchWaiting: true},
			"A batch is already waiting for you to approve, so the run would draft nothing. Approve or discard it first.", true},
		{"not confirmed", console.JobBatch, false, "", ok, "Tick the box to confirm: this run takes a while and spends credits.", true},
		{"asked: contacts", console.JobContacts, true, "", ok, "Asked. Contacts is starting; this row follows it, and the next automatic run moves to 24 hours after it.", false},
		{"asked: probe", console.JobProbe, true, "", ok, "Asked. Site probe is starting; this row follows it, and the next automatic run moves to 24 hours after it.", false},
		{"asked: batch", console.JobBatch, true, "", ok, "Asked. Morning batch is starting; this row follows it, and the next automatic run moves to 24 hours after it.", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			msg, err := jobRunNowDecision(tc.confirmed, tc.busy, tc.f, jobRunUnits[tc.job], now)
			switch {
			case tc.refused && !errors.Is(err, console.ErrUserFixable):
				t.Fatalf("got %q, %v; want refused", msg, err)
			case tc.refused && !strings.HasSuffix(err.Error(), ": "+tc.want):
				t.Errorf("refusal %q\nwant %q", err, tc.want)
			case !tc.refused && (err != nil || msg != tc.want):
				t.Errorf("got %q, %v; want %q", msg, err, tc.want)
			}
		})
	}
}

// TestPlannedLine: the planned sentence for each case, from the rows the guard reads.
func TestPlannedLine(t *testing.T) {
	contacts, probe, collect := guardJobs["leads-contacts"], guardJobs["leads-probe"], guardJobs["collect"]
	for _, tc := range []struct {
		name    string
		key     string
		g       guardJob
		rows    guardRows
		now     time.Time
		at      time.Time
		line    string
		overdue bool
	}{
		{"no history", "leads-contacts", contacts, guardRows{}, bt(10, 27, 8, 0), bt(10, 27, 10, 0),
			"Next run planned at 10:00, its usual time; after that every 24 hours from its last run.", false},
		{"pressed at 15:02", "leads-contacts", contacts,
			guardRows{starts: []store.Run{closedRun(startRun(1, commandRunNow, "leads-contacts", bt(10, 26, 15, 2), nil, time.Time{}), bt(10, 26, 20, 0), "success")}},
			bt(10, 27, 12, 0), bt(10, 27, 15, 0), "Next run planned at 15:00, 24 hours after the run by hand at yesterday 15:02.", false},
		{"a scheduled run counts from its plan", "leads-probe", probe,
			guardRows{starts: []store.Run{closedRun(startRun(1, commandScheduled, "leads-probe", bt(10, 26, 15, 40), nil, bt(10, 26, 13, 0)), bt(10, 26, 17, 0), "success")}},
			bt(10, 27, 12, 0), bt(10, 27, 13, 0), "Next run planned at 13:00, 24 hours after the run on schedule at yesterday 13:00.", false},
		{"collect every 12 hours", "collect", collect,
			guardRows{starts: []store.Run{closedRun(startRun(1, commandScheduled, "collect", bt(10, 27, 6, 30), nil, bt(10, 27, 6, 30)), bt(10, 27, 6, 40), "success")}},
			bt(10, 27, 12, 0), bt(10, 27, 18, 30), "Next run planned at 18:30, 12 hours after the run on schedule at 06:30.", false},
		{"running", "leads-contacts", contacts,
			guardRows{starts: []store.Run{startRun(1, commandScheduled, "leads-contacts", bt(10, 27, 10, 0), nil, bt(10, 27, 10, 0))}},
			bt(10, 27, 12, 0), bt(10, 28, 10, 0), "Running since 10:00; the next run is planned from this one.", false},
		{"deferred past the plan: waiting, never overdue", "leads-probe", probe,
			guardRows{starts: []store.Run{closedRun(startRun(1, commandScheduled, "leads-probe", bt(10, 26, 13, 0), nil, bt(10, 26, 13, 0)), bt(10, 26, 14, 0), "success")},
				lockSteps: []store.Run{stepRun(9, commandLeadsEnrich, bt(10, 27, 12, 50), nil)}},
			bt(10, 27, 13, 45), bt(10, 27, 13, 0), "Planned at 13:00; waiting while the contacts job runs.", false},
		{"30 minutes past is not yet overdue", "leads-contacts", contacts,
			guardRows{starts: []store.Run{closedRun(startRun(1, commandScheduled, "leads-contacts", bt(10, 26, 10, 0), nil, bt(10, 26, 10, 0)), bt(10, 26, 18, 0), "success")}},
			bt(10, 27, 10, 30), bt(10, 27, 10, 0), "Next run planned at 10:00, 24 hours after the run on schedule at yesterday 10:00.", false},
		{"31 minutes past: overdue", "leads-contacts", contacts,
			guardRows{starts: []store.Run{closedRun(startRun(1, commandScheduled, "leads-contacts", bt(10, 26, 10, 0), nil, bt(10, 26, 10, 0)), bt(10, 26, 18, 0), "success")}},
			bt(10, 27, 10, 31), bt(10, 27, 10, 0), "Overdue: planned at 10:00 and not started. Check journalctl -u leadhound-leads-contacts.service -g timer-guard.", true},
		{"a long-idle job resumes at its usual time", "ats", guardJobs["ats"],
			guardRows{starts: []store.Run{closedRun(startRun(1, commandScheduled, "ats", bt(10, 1, 1, 0), nil, bt(10, 1, 1, 0)), bt(10, 1, 3, 0), "success")}},
			bt(10, 27, 12, 0), bt(10, 28, 1, 0), "Next run planned at Wed 28 Oct 01:00, its usual time; after that every 24 hours from its last run.", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := plannedLine(tc.key, tc.g, tc.rows, tc.now)
			if !got.At.Equal(tc.at) || got.Line != tc.line || got.Overdue != tc.overdue {
				t.Errorf("got %s %q overdue %v\nwant %s %q overdue %v", got.At, got.Line, got.Overdue, tc.at, tc.line, tc.overdue)
			}
		})
	}
}

// TestPlannedContractDrift: the console and the guard share one planning rule. At the tick the
// console's PlannedRun names the guard starts the job (due), one tick earlier it waits; and for
// the same press and rows the row and the guard agree whether the press is served, waiting
// (the console's waiting window is pressFresh itself) or past it.
func TestPlannedContractDrift(t *testing.T) {
	base := bt(10, 27, 0, 0)
	for key, g := range guardJobs {
		for _, rows := range []guardRows{
			{},
			{starts: []store.Run{closedRun(startRun(1, commandRunNow, key, base.Add(15*time.Hour+2*time.Minute), nil, time.Time{}), base.Add(16*time.Hour), "success")}},
			{starts: []store.Run{closedRun(startRun(1, commandScheduled, key, base.Add(15*time.Hour+40*time.Minute), nil, base.Add(13*time.Hour)), base.Add(16*time.Hour), "success")}},
		} {
			now := base.Add(17 * time.Hour)
			p := plannedLine(key, g, rows, now)
			if v, _ := guardDecision(g, nil, rows.served, rows.anchor(g), false, p.At); v != verdictDue {
				t.Errorf("%s: at the planned %s the guard says %s", key, p.At, v)
			}
			if p.At.Sub(now) > tick {
				if v, _ := guardDecision(g, nil, rows.served, rows.anchor(g), false, p.At.Add(-tick)); v != verdictWait {
					t.Errorf("%s: a tick before the planned %s the guard says %s", key, p.At, v)
				}
			}
		}
	}

	u := jobRunUnits[console.JobContacts]
	g := guardJobs[u.guardKey]
	now := bt(10, 27, 12, 0)
	run := closedRun(startRun(1, commandScheduled, u.guardKey, bt(10, 27, 10, 0), nil, bt(10, 27, 10, 0)), bt(10, 27, 11, 0), "success")
	for _, tc := range []struct {
		name   string
		r      time.Time
		starts []store.Run
		row    string // the row's state
		guard  string // the guard's verdict for the press
	}{
		{"served by a run already going", bt(10, 27, 10, 30), []store.Run{run}, "done", verdictWait},
		{"served by a start after it", bt(10, 27, 9, 50), []store.Run{run}, "done", verdictWait},
		{"waiting", now.Add(-pressFresh), []store.Run{run}, "waiting", verdictPress},
		{"just past the window", now.Add(-pressFresh - time.Minute), nil, "stuck", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows := guardRows{starts: tc.starts}
			st, _ := jobRunNowState(u, &tc.r, tc.starts, nil, false, true, now)
			if st.State != tc.row {
				t.Errorf("row %s, want %s", st.State, tc.row)
			}
			v, _ := guardDecision(g, map[string]*time.Time{u.action: &tc.r}, rows.served, rows.anchor(g), false, now)
			switch {
			case tc.guard != "" && v != tc.guard:
				t.Errorf("guard %s, want %s", v, tc.guard)
			case (tc.row == "waiting") != (v == verdictPress) && tc.row != "stuck":
				t.Errorf("row %s but guard %s", tc.row, v)
			}
			// Past pressFresh the row says stuck; the guard's press window for a job sharing
			// the lock is longer (it waits out the other's run), but its pure press rule is the
			// same pressWithin over pressFresh.
			if fresh := pressWithin(g, map[string]*time.Time{u.action: &tc.r}, rows.served, now, pressFresh) != nil; fresh != (tc.row == "waiting") {
				t.Errorf("pressWithin(pressFresh) %v, row %s", fresh, tc.row)
			}
		})
	}
}
