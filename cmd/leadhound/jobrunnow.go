package main

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/gremy/webbership-leadhound/internal/console"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/store"
)

// Run now for the three daily leads jobs (Controls: Contacts, Site probe, Morning batch), and
// every guarded job's planned run (Controls, Jobs home, Status). A press writes the job's
// request file; its watcher starts the job's own unit, unchanged, and the unit's due-check
// (timerguard.go) opens the run's start row and its ExecStopPost= closes it. The row's state
// is read from those start rows, never from the service's Result or timestamps, which move on
// every tick the check passes over. The plan is the guard's own (readGuardRows, anchorOf,
// plannedNext, guardDecision, lockHeldBy): the console holds no second copy of the rule.

// jobRunUnit is one Controls row's Run now: its guarded job (guardJobs key), the service its
// watcher starts, the watcher, the request action, the switch that turns it off, its steps in
// words (by run command; the commands themselves are guardJobs' steps), the confirm box's
// sentence and, for a job sharing the sites' fetch lock, the refusal while the other holds it
// (one %s: since when).
type jobRunUnit struct {
	guardKey   string
	service    string
	path       string
	action     string
	switchJob  string
	steps      map[string]string
	confirm    string
	holderLine string
}

// jobRunUnits are the Run now rows, by console.RunNowJobs job.
var jobRunUnits = map[string]jobRunUnit{
	console.JobContacts: {guardKey: "leads-contacts", service: serviceContacts, path: watcherContacts, action: "leads-contacts",
		switchJob: leadstore.JobContacts,
		steps: map[string]string{
			commandLeadsContacts: "searching the sites for a way to reach them",
			commandLeadsEnrich:   "reading the sites' own pages",
			commandLeadsRate:     "rating the leads with Jev",
			commandLeadsBuild:    "rebuilding the lead list",
		},
		confirm:    "Runs the whole job now: the contact search (up to 300 sites), the enrichment (up to 300), rating the changed leads with Jev inside today's leads cap, then the lead list. It can take up to 10 hours. The next automatic run moves to 24 hours after this one.",
		holderLine: "The site probe is running since %s and holds the sites' fetch lock, so the contact search would skip. Press again once it has finished.",
	},
	console.JobProbe: {guardKey: "leads-probe", service: serviceProbe, path: watcherProbe, action: "leads-probe",
		switchJob:  leadstore.JobProbe,
		steps:      map[string]string{commandLeadsSitesProbe: "checking the sites' Drupal versions"},
		confirm:    "Checks the listed sites for the Drupal version they run (up to 10000 sites). It can take up to 3 hours. The next automatic run moves to 24 hours after this one.",
		holderLine: "The contacts job is running since %s and holds the sites' fetch lock. Press again once it has finished.",
	},
	console.JobBatch: {guardKey: "leads-batch", service: serviceBatch, path: watcherBatch, action: "leads-batch",
		switchJob: leadstore.JobBatch,
		steps: map[string]string{
			commandLeadsBuild:  "rebuilding the lead list",
			commandLeadsVerify: "checking the email addresses with Reoon",
			commandLeadsBatch:  "drafting the batch",
		},
		confirm: "Rebuilds the lead list, checks the addresses with Reoon (credits) and drafts the next batch with Claude for you to approve. Nothing is sent. Up to 2 hours. The next automatic run moves to 24 hours after this one.",
	},
}

// jobRunForget is how long a press is remembered on its row.
const jobRunForget = 24 * time.Hour

// guardService and guardTimer are a guarded job's units (deploy/leadhound-<job>.*).
func guardService(key string) string { return "leadhound-" + key + ".service" }
func guardTimer(key string) string   { return "leadhound-" + key + ".timer" }

// guardKeyOfTimer is the guarded job whose timer unit is, if any.
func guardKeyOfTimer(unit string) (string, bool) {
	for key := range guardJobs {
		if guardTimer(key) == unit {
			return key, true
		}
	}
	return "", false
}

// guardKeyOfAction is the guarded job a Jobs home Run now action presses, if any.
func guardKeyOfAction(action string) (string, bool) {
	for key, g := range guardJobs {
		if slices.Contains(g.actions, action) {
			return key, true
		}
	}
	return "", false
}

// rhythmWords is g's interval in words.
func rhythmWords(g guardJob) string {
	return fmt.Sprintf("%d hours", int(g.interval().Hours()))
}

// plannedLine is g's planned run as the console says it, from the rows the guard reads, with
// now in the operator's zone. It asks the guard's own decision (with no press), so the time it
// names is the one the check starts the job at:
//   - a run going (its start row open, younger than the unit's limit): running since, and the
//     next run counts from it;
//   - deferred behind the job sharing the lock: waiting, never overdue;
//   - more than overdueAfter past the plan with no start: overdue, naming the journal;
//   - else when, and after which run (or its usual time, with no recent run).
func plannedLine(key string, g guardJob, rows guardRows, now time.Time) console.PlannedRun {
	_, held := rows.lockHeld(g, now)
	anchor := rows.anchor(g)
	verdict, at := guardDecision(g, nil, rows.served, anchor, held, now)
	p := console.PlannedRun{At: at}
	latest := latestStart(rows.counted())
	switch {
	case latest != nil && latest.FinishedAt == nil && now.Sub(latest.StartedAt) < g.limit:
		p.Line = fmt.Sprintf("Running since %s; the next run is planned from this one.", hhmm(latest.StartedAt, now))
	case verdict == verdictDeferred:
		p.Line = fmt.Sprintf("Planned at %s; waiting while %s runs.", hhmm(at, now), g.lockWords)
	case now.Sub(at) > overdueAfter:
		p.Overdue = true
		p.Line = fmt.Sprintf("Overdue: planned at %s and not started. Check journalctl -u %s -g timer-guard.", hhmm(at, now), guardService(key))
	case anchorLapsed(g, anchor, now):
		p.Line = fmt.Sprintf("Next run planned at %s, its usual time; after that every %s from its last run.", hhmm(at, now), rhythmWords(g))
	default:
		how := "on schedule"
		if latest != nil && latest.Command == commandRunNow {
			how = "by hand"
		}
		p.Line = fmt.Sprintf("Next run planned at %s, %s after the run %s at %s.", hhmm(at, now), rhythmWords(g), how, hhmm(*anchor, now))
	}
	return p
}

// plannedRun is guarded job key's planned run (plannedLine), read as the guard reads it.
func (b *backend) plannedRun(ctx context.Context, key string, now time.Time) (*console.PlannedRun, error) {
	g, ok := guardJobs[key]
	if !ok {
		return nil, fmt.Errorf("console: %q is not a guarded job", key)
	}
	rows, err := readGuardRows(ctx, b.s.Pool(), key, g, now)
	if err != nil {
		return nil, err
	}
	p := plannedLine(key, g, rows, now.In(b.loc))
	return &p, nil
}

// endedWith is the result the unit recorded on a closed start row ("success", "timeout",
// "unrecorded"...), "" when the note holds none.
func endedWith(r store.Run) string {
	i := strings.LastIndex(r.Note, noteEnded)
	if i < 0 {
		return ""
	}
	return r.Note[i+len(noteEnded):]
}

// runStep is the step a run that began at since is on: the latest unfinished step row after
// it, in words; "between two steps" when none is open.
func (u jobRunUnit) runStep(steps []store.Run, since time.Time) string {
	var cur *store.Run
	for i := range steps {
		r := &steps[i]
		if _, ok := u.steps[r.Command]; !ok || r.FinishedAt != nil || r.StartedAt.Before(since) {
			continue
		}
		if cur == nil || !r.StartedAt.Before(cur.StartedAt) {
			cur = r
		}
	}
	if cur == nil {
		return "between two steps"
	}
	return u.steps[cur.Command]
}

// stepFailed is whether a step row of the run [from, to] failed (runView: leads notes, stop
// reasons and did-not-finish included).
func (u jobRunUnit) stepFailed(steps []store.Run, from, to, now time.Time) bool {
	for _, r := range steps {
		if _, ok := u.steps[r.Command]; ok && !r.StartedAt.Before(from) && !r.StartedAt.After(to) && runView(r, now).Failed {
			return true
		}
	}
	return false
}

// stuckWords is the stuck press's sentence and the watcher's reset.
func (u jobRunUnit) stuckWords(asked string) string {
	return fmt.Sprintf("Asked at %s and not started: its watcher on the server may not be running. "+
		"See deploy/README.md, Run now (systemctl reset-failed %s %s, then systemctl start %s).", asked, u.path, u.service, u.path)
}

// jobRunNowState is a Run now row's block, pure, with busy the refusal a press would get for
// it ("" when none). It reads the job's own start rows (one per run, open while it runs and
// closed by the unit with "ended <result>"), never the service's Result or timestamps; steps
// (the job's step rows) name the step, show failed ones and tell a run that started nothing
// (countedStarts: it serves no press, as in the check). svcBusy is serviceBusy. In order:
//  1. systemd not read: idle, and the press is refused (jobRunNowDecision);
//  2. the latest run is open: running while its service starts or stops, else it did not finish;
//  3. a press served by a run that began at or after it: that run's outcome;
//  4. a press served by a run already going at it: when that run ended;
//  5. an unserved press: waiting within pressFresh (a tick and a margin, the guard's own), then
//     did not start when a run that started nothing began at or after it, else stuck: by then
//     the watcher and the next tick have both passed it by;
//  6. otherwise idle (Last run and the planned line cover the schedule's runs).
//
// Steps 3 and 4 hold only while that run is the latest; after a later run the row is idle.
func jobRunNowState(u jobRunUnit, requested *time.Time, starts, steps []store.Run, svcBusy, read bool, now time.Time) (console.JobRunNow, string) {
	st, busy := jobRunNowStep(u, requested, starts, steps, svcBusy, read, now)
	st.CanAsk, st.Confirm = !st.Pending, u.confirm
	return st, busy
}

// jobRunNowStep is jobRunNowState's block before CanAsk and Confirm are set.
func jobRunNowStep(u jobRunUnit, requested *time.Time, all, steps []store.Run, svcBusy, read bool, now time.Time) (console.JobRunNow, string) {
	if !read {
		return console.JobRunNow{State: "idle", Line: "Run now needs the server's systemd, which could not be asked just now."}, ""
	}
	starts := countedStarts(all, steps)
	latest := latestStart(starts)
	pressAfter := func(s store.Run) bool { return requested != nil && requested.After(s.StartedAt) }
	if latest != nil && latest.FinishedAt == nil {
		since := hhmm(latest.StartedAt, now)
		switch {
		case svcBusy:
			line := fmt.Sprintf("Running since %s: %s.", since, u.runStep(steps, latest.StartedAt))
			if pressAfter(*latest) {
				line += fmt.Sprintf(" Asked at %s while a run was already going; that run covers it.", hhmm(*requested, now))
			}
			return console.JobRunNow{State: "running", Pending: true, Line: line}, "Already running since " + since + "."
		case !pressAfter(*latest):
			return console.JobRunNow{State: "bad", Line: "The last run did not finish (no end was recorded)."}, ""
		}
		// A press after a run that is over without its end recorded waits for the next
		// check, which closes that run first: fall through to the press.
	}
	if requested == nil {
		return console.JobRunNow{State: "idle"}, ""
	}
	asked := hhmm(*requested, now)
	if pressServed(*requested, starts) {
		var by *store.Run // the run that began at or after the press, the earliest
		for i := range starts {
			s := &starts[i]
			if isStartRow(*s) && !s.StartedAt.Before(*requested) && (by == nil || s.StartedAt.Before(by.StartedAt)) {
				by = s
			}
		}
		if by == nil {
			// Served by the latest run, already going at the press (it began before it and
			// ended after it). Runs never overlap; anything else reads as idle.
			if latest.FinishedAt == nil || !latest.FinishedAt.After(*requested) {
				return console.JobRunNow{State: "idle"}, ""
			}
			return console.JobRunNow{State: "done",
				Line: fmt.Sprintf("Asked at %s while a run was already going; it ended at %s.", asked, hhmm(*latest.FinishedAt, now))}, ""
		}
		if by.ID != latest.ID || by.FinishedAt == nil {
			return console.JobRunNow{State: "idle"}, "" // a later run since (or an open one step 2 showed)
		}
		ended := hhmm(*by.FinishedAt, now)
		switch result := endedWith(*by); {
		case result == "":
			return console.JobRunNow{State: "bad", Line: fmt.Sprintf("The run asked at %s did not finish (no end recorded). See journalctl -u %s.",
				asked, u.service)}, ""
		case result != "success":
			return console.JobRunNow{State: "bad", Line: fmt.Sprintf("The run asked at %s did not finish (%s). See journalctl -u %s.",
				asked, result, u.service)}, ""
		case u.stepFailed(steps, by.StartedAt, *by.FinishedAt, now):
			return console.JobRunNow{State: "bad", Line: fmt.Sprintf("The run asked at %s ended at %s. Some steps failed; see Last run above.", asked, ended)}, ""
		}
		return console.JobRunNow{State: "done",
			Line: fmt.Sprintf("The run asked at %s ended at %s. Each step's result is in Last run above.", asked, ended)}, ""
	}
	if age := now.Sub(*requested); age <= pressFresh && age >= -pressAhead {
		return console.JobRunNow{State: "waiting", Pending: true, Line: "Asked at " + asked + ", waiting to start."}, "Already asked at " + asked + "."
	}
	if now.Sub(*requested) < -pressAhead {
		return console.JobRunNow{State: "idle"}, "" // ahead of the clock: the check ignores it too
	}
	if slices.ContainsFunc(all, func(s store.Run) bool { return !s.StartedAt.Before(*requested) && startedNothing(s, steps) }) {
		return console.JobRunNow{State: "bad", Line: fmt.Sprintf("The run asked at %s did not start: its start was recorded, but none of its steps ran. See journalctl -u %s.",
			asked, u.service)}, ""
	}
	return console.JobRunNow{State: "stuck", Line: u.stuckWords(asked)}, ""
}

// serviceBusy is whether a guarded service is in a run: starting (its check or its job) or
// stopping (deactivating, while its ExecStopPost= records the end). A press while it stops
// would land before that end is written, count as served by the run that is ending, and never
// run.
func serviceBusy(st unitState) bool { return st.Starting || st.Stopping }

// jobRunFacts is what a press's refusals read besides the row's state: whether systemd was
// read, the job's switch, its watcher, the job holding the sites' fetch lock (holder, its
// guardJobs key, "" when none) and whether a batch already waits for approval.
type jobRunFacts struct {
	read, off    bool
	path         unitState
	holder       string
	holderSince  *time.Time
	batchWaiting bool
}

// jobRunNowDecision is whether a press of u goes ahead, pure, with the sentence the operator
// sees; every refusal wraps console.ErrUserFixable.
func jobRunNowDecision(confirmed bool, busy string, f jobRunFacts, u jobRunUnit, now time.Time) (string, error) {
	name := jobName(u.switchJob)
	switch {
	case !f.read:
		return "", refuse("The server's systemd could not be asked, so Run now cannot tell whether the job is already running. Try again in a moment.")
	case busy != "":
		return "", refuse("%s", busy)
	case f.off:
		return "", refuse("%s is switched off on this page; switch it on first.", name)
	case !f.path.Loaded || !f.path.Enabled:
		return "", refuse("The %s Run now watcher is not armed on the server. It goes with the %s timer (deploy/README.md, Leads on the server).", name, name)
	case f.path.Failed || !f.path.Active:
		return "", refuse("The %s Run now watcher has stopped on the server. See deploy/README.md, Run now (systemctl reset-failed %s %s, then systemctl start %s).",
			name, u.path, u.service, u.path)
	case f.holder != "" && u.holderLine != "":
		since := "a moment ago"
		if f.holderSince != nil {
			since = hhmm(*f.holderSince, now)
		}
		return "", refuse(u.holderLine, since)
	case f.batchWaiting:
		return "", refuse("A batch is already waiting for you to approve, so the run would draft nothing. Approve or discard it first.")
	case !confirmed:
		return "", refuse("Tick the box to confirm: this run takes a while and spends credits.")
	}
	return fmt.Sprintf("Asked. %s is starting; this row follows it, and the next automatic run moves to 24 hours after it.", name), nil
}

// rememberedPress is at when it is a press the row still shows (at most jobRunForget old).
func rememberedPress(at *time.Time, now time.Time) *time.Time {
	if at == nil || now.Sub(*at) > jobRunForget {
		return nil
	}
	return at
}

// jobRunNowView is job's Run now block for the Controls page. A request file that cannot be
// read is no press (one warning line), so the page still shows.
func (b *backend) jobRunNowView(ctx context.Context, job string, timers timerRead, now time.Time) (*console.JobRunNow, error) {
	u, ok := jobRunUnits[job]
	if !ok {
		return nil, nil
	}
	g := guardJobs[u.guardKey]
	req, err := readRunRequest(b.cfg.DataDir, u.action)
	if err != nil {
		slog.Default().Warn("console run now: request file unreadable, no press shown", "job", job, "err", err)
		req = nil
	}
	rows, err := readGuardRows(ctx, b.s.Pool(), u.guardKey, g, now)
	if err != nil {
		return nil, err
	}
	st, _ := jobRunNowState(u, rememberedPress(req, now), rows.starts, rows.steps, serviceBusy(timers.units[u.service]), timers.fromSystemd, now.In(b.loc))
	return &st, nil
}

// JobRunNow asks for one run of job now (console.ControlsBackend): it refuses what the
// operator can fix (jobRunNowDecision) and otherwise writes the request file the job's watcher
// starts its unit from. The holder is the guard's own lock rule over the rows, not the other
// service's Starting, which stays true for contacts' whole run though it holds the lock for
// its fetch steps only; the waiting batch is newBatch's own refusal (UnapprovedBatch, of the
// kind newBatchSetup makes).
func (b *backend) JobRunNow(ctx context.Context, job string, confirmed bool) (console.ControlOutcome, error) {
	u, ok := jobRunUnits[job]
	if !ok {
		return console.ControlOutcome{}, refuse("%q has no Run now; use one of %s.", job, strings.Join(console.RunNowJobs, ", "))
	}
	setup, err := b.leadsSetup(ctx, leadsForProbe)
	if err != nil {
		return console.ControlOutcome{}, err
	}
	g := guardJobs[u.guardKey]
	now := b.now()
	local := now.In(b.loc)
	timers := readTimers(ctx, controlsGetenv)
	req, err := readRunRequest(setup.dataDir, u.action)
	if err != nil {
		return console.ControlOutcome{}, err
	}
	rows, err := readGuardRows(ctx, b.s.Pool(), u.guardKey, g, now)
	if err != nil {
		return console.ControlOutcome{}, err
	}
	_, busy := jobRunNowState(u, rememberedPress(req, now), rows.starts, rows.steps, serviceBusy(timers.units[u.service]), timers.fromSystemd, local)
	f := jobRunFacts{read: timers.fromSystemd, path: timers.units[u.path]}
	lq := b.lq()
	if f.off, _, err = jobOff(ctx, lq, u.switchJob); err != nil {
		return console.ControlOutcome{}, err
	}
	if since, held := rows.lockHeld(g, now); held {
		f.holder, f.holderSince = g.lockJob, &since
	}
	if job == console.JobBatch {
		if _, f.batchWaiting, err = lq.UnapprovedBatch(ctx, leadstore.DefaultAccountID, newBatchSetup(setup, b.loc).shadow); err != nil {
			return console.ControlOutcome{}, err
		}
	}
	msg, err := jobRunNowDecision(confirmed, busy, f, u, local)
	if err != nil {
		return console.ControlOutcome{}, err
	}
	// The press is timed after systemd and the rows were read: a service that was no longer
	// stopping then has its run's end written, so that end is before the press, and the run
	// that ended cannot count as serving it.
	if err := writeRunRequest(setup.dataDir, u.action, b.now()); err != nil {
		return console.ControlOutcome{}, err
	}
	return console.ControlOutcome{Message: msg}, nil
}
