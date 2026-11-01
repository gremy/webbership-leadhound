package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gremy/webbership-leadhound/internal/config"
	"github.com/gremy/webbership-leadhound/internal/console"
	"github.com/gremy/webbership-leadhound/internal/store"
)

// The due-check: the first line of every guarded unit (ExecCondition=, deploy/README.md, Run
// now and the rolling schedule). Its timer ticks every 15 minutes; this check starts the job
// for a press at once, and otherwise only when the job is due: one interval after its last
// start, whoever started it (plannedNext). Every start it lets through is recorded, and that
// row is the next plan's anchor. When in doubt it waits: an error means "not now" and the
// next tick tries again, so a broken check shows as an overdue run, never as a job run every
// 15 minutes.

func init() { commands["timer-guard"] = cmdTimerGuard }

// The run commands the check writes. No other reader keys on them (timerguard_test.go sweeps),
// and the run lists leave them out (store.RecentRuns).
const (
	commandRunNow    = store.RunCommandRunNow    // source: the job; note: "asked at <RFC3339>"
	commandScheduled = store.RunCommandScheduled // source: the job; note: "planned <RFC3339>"
)

// The notes of a start row: how it began, then, once the unit has stopped, how it ended.
const (
	noteAskedAt        = "asked at "
	notePlanned        = "planned "
	noteEnded          = " · ended "
	noteUnrecorded     = "unrecorded"            // the run was over before its end was recorded
	noteStartNotStored = " · start not recorded" // a press that ran while the database could not be reached
)

// The schedule's grid: the guarded timers tick every tick, and a plan is rounded to it.
const tick = 15 * time.Minute

// pressFresh is how long a press waits to be served: a tick, systemd's one-minute accuracy and
// a margin, because a press that lands while a tick's check runs is merged into that start and
// only the next tick serves it.
const pressFresh = 20 * time.Minute

// pressAhead is how far ahead of now a request time may be and still count (systemd's timer
// accuracy); a time further ahead (clock skew, the mtime fallback) is ignored with a warning.
const pressAhead = time.Minute

// overdueAfter is how long past its plan a run may not have started before the console says so.
const overdueAfter = 30 * time.Minute

// lockStartGrace is how long the start row of the job sharing the lock counts as holding it
// before its fetch step has written a row: that step's row opens only after its flock, so two
// checks due on the same tick would otherwise both see the lock free.
const lockStartGrace = 10 * time.Minute

// The check's own budget, well inside the unit's `timeout 1m`.
const (
	guardBudget  = 40 * time.Second
	guardConnect = 10 * time.Second
)

// timerGuardLockKey is the transaction-level advisory lock the contacts and probe checks hold
// from their read to their write, so the two never decide at the same moment. It differs from
// every other key (the LH… keys, the score and scale keys, goose's), so a check never waits on
// a send, a settings save or a migrate.
const timerGuardLockKey int64 = 0x4c48_5449_4d52 // "LHTIMR"

// guardJob is one guarded unit: its plain name, the request actions that count as a press, its
// rhythm (days for a daily job, added as calendar days in the operator's zone; or every for a
// sub-daily one), the legacy slots its first run keeps (HH:MM), the commands of the job that
// shares its lock, with that job's run limit.
type guardJob struct {
	name      string
	actions   []string
	days      int
	every     time.Duration
	slots     []string
	lockedBy  []string
	lockLimit time.Duration
	// lockJob is the guarded job whose unit runs lockedBy, and lockWords says it in a sentence.
	lockJob, lockWords string
	// legacyCommand and legacy are the job's own run rows from before the due-check, used only
	// to anchor the very first plan.
	legacyCommand string
	legacy        func(store.Run) bool
	// steps are the job's step commands, used to name the running step, to date a run that
	// ended unrecorded and to tell whether it ran at all (workRows); none for ats and collect,
	// whose own work rows are their legacy collect rows (the Jobs home card keeps its own state).
	steps []string
	// limit is its unit's TimeoutStartSec: the longest a run can last.
	limit time.Duration
}

var guardJobs = map[string]guardJob{
	"leads-contacts": {name: "Contacts", actions: []string{"leads-contacts"}, days: 1, slots: []string{"10:00"},
		lockedBy: []string{commandLeadsSitesProbe}, lockLimit: 3*time.Hour + 5*time.Minute, // the probe's inner timeout 3h + kill 5m
		lockJob: "leads-probe", lockWords: "the site probe",
		legacyCommand: commandLeadsContacts, legacy: commandIs(commandLeadsContacts),
		steps: []string{commandLeadsContacts, commandLeadsEnrich, commandLeadsRate, commandLeadsBuild},
		limit: 10*time.Hour + time.Minute},
	"leads-probe": {name: "Site probe", actions: []string{"leads-probe"}, days: 1, slots: []string{"13:00"},
		lockedBy: []string{commandLeadsContacts, commandLeadsEnrich}, lockLimit: 2*time.Hour + 5*time.Minute, // the longer web step's timeout + kill
		lockJob: "leads-contacts", lockWords: "the contacts job",
		legacyCommand: commandLeadsSitesProbe, legacy: commandIs(commandLeadsSitesProbe),
		steps: []string{commandLeadsSitesProbe},
		limit: 390*time.Minute + time.Minute},
	"leads-batch": {name: "Morning batch", actions: []string{"leads-batch"}, days: 1, slots: []string{"06:15"},
		legacyCommand: commandLeadsBatch, legacy: commandIs(commandLeadsBatch),
		steps: []string{commandLeadsBuild, commandLeadsVerify, commandLeadsBatch},
		limit: 2*time.Hour + time.Minute},
	"ats": {name: "ATS collect", actions: []string{console.RunATS}, days: 1, slots: []string{"01:00"},
		legacyCommand: "collect", legacy: isATSCollect, limit: atsRunLimit + time.Minute},
	"collect": {name: "LinkedIn collect", actions: []string{console.RunFetch}, every: 12 * time.Hour, slots: []string{"06:30", "18:30"},
		legacyCommand: "collect", legacy: isLinkedInCollect, limit: collectUnitTimeout + time.Minute},
}

// errNotNow is the check's "do not start now" (not due, deferred, or in doubt): run() exits 3
// for it without the failure log line, and ExecCondition= passes over this start.
var errNotNow = errors.New("timer guard: not now")

// planZone is the timers' zone (OnCalendar=… Europe/Bucharest). Planning always uses it, never
// LEADHOUND_TZ, which can name another zone; time/tzdata (main.go) makes it load anywhere.
var planZone = mustZone("Europe/Bucharest")

func mustZone(name string) *time.Location {
	loc, err := time.LoadLocation(name)
	if err != nil {
		panic(err)
	}
	return loc
}

// guardClock is the check's clock; a test sets it.
var guardClock = time.Now

// interval is g's rhythm as a duration (a daily job's day taken as 24 hours, for windows only).
func (g guardJob) interval() time.Duration {
	if g.every > 0 {
		return g.every
	}
	return time.Duration(g.days) * 24 * time.Hour
}

// pressWindow is how old a press may be and still run: pressFresh, plus, for a job that waits
// for the one sharing its lock, the time that one may hold it.
func (g guardJob) pressWindow() time.Duration { return pressFresh + g.lockLimit }

// guardWindow is how far back the check reads: three intervals, so an anchor that is merely
// old is told apart from one that is missing, plus the lock and the run limits. The console's
// plannedRun reads the same window.
func guardWindow(g guardJob) time.Duration { return 3*g.interval() + g.lockLimit + g.limit }

// slotOn is the slot hm ("10:00") on the calendar day of day in planZone, plus addDays.
func slotOn(day time.Time, addDays int, hm string) time.Time {
	at, err := time.Parse("15:04", hm)
	if err != nil {
		panic(fmt.Sprintf("timer guard: slot %q: %v", hm, err)) // guardJobs is fixed; a test reads every slot
	}
	d := day.In(planZone)
	return time.Date(d.Year(), d.Month(), d.Day()+addDays, at.Hour(), at.Minute(), 0, 0, planZone)
}

// nextSlot is g's earliest legacy slot at or after t.
func nextSlot(g guardJob, t time.Time) time.Time {
	var best time.Time
	for d := 0; d <= 2 && best.IsZero(); d++ {
		for _, s := range g.slots {
			if at := slotOn(t, d, s); !at.Before(t) && (best.IsZero() || at.Before(best)) {
				best = at
			}
		}
	}
	return best
}

// lastSlot is g's latest legacy slot at or before t and within one interval of it.
func lastSlot(g guardJob, t time.Time) (time.Time, bool) {
	var best time.Time
	for d := 0; d >= -2; d-- {
		for _, s := range g.slots {
			if at := slotOn(t, d, s); !at.After(t) && t.Sub(at) <= g.interval() && at.After(best) {
				best = at
			}
		}
	}
	return best, !best.IsZero()
}

// plannedNext is when g's next automatic run is planned, counted from anchor (anchorOf). It is
// the one planning rule: the console calls this same function.
//   - No anchor, or one older than two intervals (a timer re-enabled after weeks off, a long
//     outage): the next legacy slot at or after now − tick/2, so a long-idle job resumes at its
//     usual time instead of starting a long, spending run at deploy time.
//   - Otherwise one interval after the anchor (a daily interval added as one calendar day in
//     planZone, so the local time holds across DST), rounded to the nearest tick: 15:02 plans
//     15:00, and a start a minute late never drifts the rhythm.
//
// The plan never only looks forward, so a deferred tick, a missed slot or a catch-up at boot is
// due at once and never lost.
func plannedNext(g guardJob, anchor *time.Time, now time.Time) time.Time {
	if anchorLapsed(g, anchor, now) {
		return nextSlot(g, now.Add(-tick/2))
	}
	var next time.Time
	if g.every > 0 {
		next = anchor.Add(g.every)
	} else {
		next = anchor.In(planZone).AddDate(0, 0, g.days)
	}
	// Round works on absolute time; Bucharest's offsets are whole hours, so the 15-minute grid
	// is the same in local time.
	return next.Round(tick).In(planZone)
}

// anchorLapsed is plannedNext's first case: no anchor, or one older than two intervals, so the
// plan is the next legacy slot (the console says "its usual time" for it).
func anchorLapsed(g guardJob, anchor *time.Time, now time.Time) bool {
	return anchor == nil || now.Sub(*anchor) > 2*g.interval()
}

// isStartRow is a start row the check wrote.
func isStartRow(r store.Run) bool { return r.Command == commandRunNow || r.Command == commandScheduled }

// notedTime is the time a start row's note gives after prefix ("planned …", "asked at …").
func notedTime(note, prefix string) (time.Time, bool) {
	rest, ok := strings.CutPrefix(note, prefix)
	if !ok {
		return time.Time{}, false
	}
	if i := strings.IndexByte(rest, ' '); i >= 0 {
		rest = rest[:i]
	}
	at, err := time.Parse(time.RFC3339, rest)
	return at, err == nil
}

// startAnchor is where a start row counts from: a run-now at its start (the press moves the
// rhythm), a scheduled run at the time it was planned for (a deferral or a catch-up never
// drifts it; an unparseable note falls back to the start).
func startAnchor(r store.Run) time.Time {
	if r.Command == commandScheduled {
		if at, ok := notedTime(r.Note, notePlanned); ok {
			return at
		}
	}
	return r.StartedAt
}

// latestStart is the latest of starts (by started_at, then id), nil when there is none.
func latestStart(starts []store.Run) *store.Run {
	var latest *store.Run
	for i := range starts {
		r := &starts[i]
		if !isStartRow(*r) {
			continue
		}
		if latest == nil || r.StartedAt.After(latest.StartedAt) || (r.StartedAt.Equal(latest.StartedAt) && r.ID > latest.ID) {
			latest = r
		}
	}
	return latest
}

// anchorOf is what g's plan counts from: its latest start row (startAnchor); with none, the
// latest legacy slot at or before its latest legacy row, within one interval (else that row's
// start), so the old random delay and the batch's lead time never enter the rhythm; nil with
// no history at all.
func anchorOf(g guardJob, starts []store.Run, legacy *store.Run) *time.Time {
	if r := latestStart(starts); r != nil {
		at := startAnchor(*r)
		return &at
	}
	if legacy == nil {
		return nil
	}
	at := legacy.StartedAt
	if s, ok := lastSlot(g, legacy.StartedAt); ok {
		at = s
	}
	return &at
}

// pressServed is whether a press at r has had its run: a start row at or after r, or a run
// already going at r (a start row before r that finished after it). An open row never counts
// as going: the check closes a dead one first (closeDead), and the console shows a live one as
// running. The fetch button relies on this: it joins a collect already running, which must
// count as the press's, or the next tick would start a second collect.
func pressServed(r time.Time, starts []store.Run) bool {
	for _, s := range starts {
		if !isStartRow(s) {
			continue
		}
		if !s.StartedAt.Before(r) || (s.FinishedAt != nil && s.FinishedAt.After(r)) {
			return true
		}
	}
	return false
}

// pressWithin is the newest request in g's actions at most window old (and not more than
// pressAhead ahead of now) that served does not count as served; nil when there is none.
func pressWithin(g guardJob, reqs map[string]*time.Time, served func(time.Time) bool, now time.Time, window time.Duration) *time.Time {
	var newest *time.Time
	for _, a := range g.actions {
		r := reqs[a]
		if r == nil {
			continue
		}
		if age := now.Sub(*r); age < -pressAhead || age > window {
			continue
		}
		if newest == nil || r.After(*newest) {
			newest = r
		}
	}
	if newest == nil || served(*newest) {
		return nil
	}
	at := *newest
	return &at
}

// requestsAhead are g's request times further ahead of now than pressAhead: never a press,
// and worth one warning line.
func requestsAhead(g guardJob, reqs map[string]*time.Time, now time.Time) []time.Time {
	var out []time.Time
	for _, a := range g.actions {
		if r := reqs[a]; r != nil && now.Sub(*r) < -pressAhead {
			out = append(out, *r)
		}
	}
	return out
}

// lockHeldBy is the pure core of the lock-holder check, shared with the console: for a job
// that shares the sites' fetch lock (contacts and the probe), whether the other job holds it
// now and since when. It holds it while one of its fetch-step rows (lockedBy) is unfinished and
// younger than lockLimit (past that its timeout has killed it), or while its start row from
// the last lockStartGrace is open with no finished fetch-step row after it (the step's row
// opens only after its flock).
func lockHeldBy(g guardJob, lockStarts, lockSteps []store.Run, now time.Time) (time.Time, bool) {
	if len(g.lockedBy) == 0 {
		return time.Time{}, false
	}
	var since time.Time
	held := false
	hold := func(at time.Time) {
		if !held || at.After(since) {
			since, held = at, true
		}
	}
	for _, r := range lockSteps {
		if slices.Contains(g.lockedBy, r.Command) && r.FinishedAt == nil && now.Sub(r.StartedAt) < g.lockLimit {
			hold(r.StartedAt)
		}
	}
	for _, s := range lockStarts {
		if !isStartRow(s) || s.Source != g.lockJob || s.FinishedAt != nil || now.Sub(s.StartedAt) >= lockStartGrace {
			continue
		}
		stepped := slices.ContainsFunc(lockSteps, func(r store.Run) bool {
			return slices.Contains(g.lockedBy, r.Command) && r.FinishedAt != nil && !r.StartedAt.Before(s.StartedAt)
		})
		if !stepped {
			hold(s.StartedAt)
		}
	}
	return since, held
}

// The check's verdicts.
const (
	verdictPress    = "press"    // an unserved press: start now, as a run-now
	verdictDue      = "due"      // the plan has come: start now, as a scheduled run
	verdictDeferred = "deferred" // a press or the plan waits while the job sharing the lock runs
	verdictWait     = "wait"     // not due yet
)

// guardDecision is the check's decision for g at now, pure: an unserved fresh press runs at
// once (for contacts and the probe it waits while the other holds the lock, or contacts'
// fetch steps would exit 75 and lose the day's search); else the plan from anchor is due from
// half a tick before it, and waits while the lock is held. plan is the press time for a
// press, else the plan.
func guardDecision(g guardJob, reqs map[string]*time.Time, served func(time.Time) bool, anchor *time.Time, lockHeld bool, now time.Time) (verdict string, plan time.Time) {
	if r := pressWithin(g, reqs, served, now, g.pressWindow()); r != nil {
		if lockHeld && len(g.lockedBy) > 0 {
			return verdictDeferred, *r
		}
		return verdictPress, *r
	}
	plan = plannedNext(g, anchor, now)
	switch {
	case now.Before(plan.Add(-tick / 2)):
		return verdictWait, plan
	case lockHeld && len(g.lockedBy) > 0:
		return verdictDeferred, plan
	}
	return verdictDue, plan
}

// guardRows is what the check reads for one job over guardWindow; the console's plannedRun
// reads the same through readGuardRows.
type guardRows struct {
	starts     []store.Run // the job's start rows, oldest first
	legacy     *store.Run  // its latest legacy row, nil when none
	steps      []store.Run // its own work rows (workRows: g.steps' rows, else its legacy rows), oldest first
	lockStarts []store.Run // the start rows of the job sharing its lock (lockJob)
	lockSteps  []store.Run // the rows of that job's fetch steps (lockedBy)
}

// counted are the rows' start rows that count for the plan and for a press (countedStarts).
func (rows guardRows) counted() []store.Run { return countedStarts(rows.starts, rows.steps) }

// anchor is the rows' anchorOf, over the counted start rows.
func (rows guardRows) anchor(g guardJob) *time.Time { return anchorOf(g, rows.counted(), rows.legacy) }

// served is pressServed over the job's counted start rows.
func (rows guardRows) served(r time.Time) bool { return pressServed(r, rows.counted()) }

// startedNothing is a start row closed as unrecorded with nothing to show its job ran: no work
// row of the job (workRows) began inside [started_at, finished_at]. Its start was recorded,
// but the job itself may never have started (a commit that errored after it landed, a check
// killed after its commit, the unit stopped between its check and its start), so it neither
// anchors the plan nor serves a press: the slot stays due, the press stays pending.
func startedNothing(s store.Run, work []store.Run) bool {
	if !isStartRow(s) || s.FinishedAt == nil || endedWith(s) != noteUnrecorded {
		return false
	}
	return !slices.ContainsFunc(work, func(r store.Run) bool {
		return !r.StartedAt.Before(s.StartedAt) && !r.StartedAt.After(*s.FinishedAt)
	})
}

// countedStarts are starts without the ones that started nothing (startedNothing over work):
// what anchorOf and pressServed read, in the check and in the console alike.
func countedStarts(starts, work []store.Run) []store.Run {
	return slices.DeleteFunc(slices.Clone(starts), func(s store.Run) bool { return startedNothing(s, work) })
}

// lockHeld is lockHeldBy over the rows.
func (rows guardRows) lockHeld(g guardJob, now time.Time) (time.Time, bool) {
	return lockHeldBy(g, rows.lockStarts, rows.lockSteps, now)
}

const runColumns = `id, command, source, started_at, finished_at, requests, failures, stop_reason, note`

func queryRuns(ctx context.Context, db store.DBTX, what, sql string, args ...any) ([]store.Run, error) {
	rows, err := db.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("timer guard: %s: %w", what, err)
	}
	runs, err := pgx.CollectRows(rows, pgx.RowToStructByName[store.Run])
	if err != nil {
		return nil, fmt.Errorf("timer guard: %s: %w", what, err)
	}
	return runs, nil
}

// guardStartRows are job's start rows (run-now and scheduled, source job) started at or after
// since, oldest first.
func guardStartRows(ctx context.Context, db store.DBTX, job string, since time.Time) ([]store.Run, error) {
	return queryRuns(ctx, db, "start rows of "+job,
		`SELECT `+runColumns+` FROM run
		 WHERE command = ANY($1) AND source = $2 AND started_at >= $3
		 ORDER BY started_at, id`, []string{commandRunNow, commandScheduled}, job, since)
}

// guardLegacyRow is g's latest legacy row started at or after since, nil when there is none.
func guardLegacyRow(ctx context.Context, db store.DBTX, g guardJob, since time.Time) (*store.Run, error) {
	runs, err := queryRuns(ctx, db, "legacy rows of "+g.legacyCommand,
		`SELECT `+runColumns+` FROM run
		 WHERE command = $1 AND started_at >= $2
		 ORDER BY started_at DESC, id DESC
		 LIMIT $3`, g.legacyCommand, since, store.MaxRunsSince)
	if err != nil {
		return nil, err
	}
	for i := range runs {
		if g.legacy(runs[i]) {
			return &runs[i], nil
		}
	}
	return nil, nil
}

// guardStepRows are the rows of commands started at or after since, oldest first (none for no
// commands).
func guardStepRows(ctx context.Context, db store.DBTX, commands []string, since time.Time) ([]store.Run, error) {
	if len(commands) == 0 {
		return nil, nil
	}
	return queryRuns(ctx, db, "step rows",
		`SELECT `+runColumns+` FROM run
		 WHERE command = ANY($1) AND started_at >= $2
		 ORDER BY started_at, id`, commands, since)
}

// workRows are g's own work rows started at or after since, oldest first: its step rows
// (g.steps), or for a job without steps (ats, collect) its legacy rows (g.legacy), one per
// ATS or per collect. They date a run that ended unrecorded (deadEnd) and show whether it ran
// at all (startedNothing).
func workRows(ctx context.Context, db store.DBTX, g guardJob, since time.Time) ([]store.Run, error) {
	if len(g.steps) > 0 {
		return guardStepRows(ctx, db, g.steps, since)
	}
	runs, err := guardStepRows(ctx, db, []string{g.legacyCommand}, since)
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(runs, func(r store.Run) bool { return !g.legacy(r) }), nil
}

// readGuardRows reads everything the check (and the console's plannedRun) decides job from.
func readGuardRows(ctx context.Context, db store.DBTX, job string, g guardJob, now time.Time) (guardRows, error) {
	var rows guardRows
	var err error
	since := now.Add(-guardWindow(g))
	if rows.starts, err = guardStartRows(ctx, db, job, since); err != nil {
		return guardRows{}, err
	}
	if rows.legacy, err = guardLegacyRow(ctx, db, g, since); err != nil {
		return guardRows{}, err
	}
	if rows.steps, err = workRows(ctx, db, g, since); err != nil {
		return guardRows{}, err
	}
	if g.lockJob != "" {
		if rows.lockStarts, err = guardStartRows(ctx, db, g.lockJob, now.Add(-lockStartGrace)); err != nil {
			return guardRows{}, err
		}
		if rows.lockSteps, err = guardStepRows(ctx, db, g.lockedBy, now.Add(-lockStartGrace-g.lockLimit)); err != nil {
			return guardRows{}, err
		}
	}
	return rows, nil
}

// deadEnd is when a start row closed as unrecorded counts as having ended: its last step row's
// end (or start, for a step left open) before the next start row, else its own start. It never
// counts as running just because no end is known.
func deadEnd(s store.Run, starts, steps []store.Run) time.Time {
	end, until := s.StartedAt, time.Time{}
	for _, n := range starts {
		if isStartRow(n) && n.StartedAt.After(s.StartedAt) && (until.IsZero() || n.StartedAt.Before(until)) {
			until = n.StartedAt
		}
	}
	for _, r := range steps {
		if r.StartedAt.Before(s.StartedAt) || (!until.IsZero() && !r.StartedAt.Before(until)) {
			continue
		}
		at := r.StartedAt
		if r.FinishedAt != nil {
			at = *r.FinishedAt
		}
		if at.After(end) {
			end = at
		}
	}
	return end
}

// deadRuns are the open start rows the check finds, closed: the check runs only while the unit
// is inactive (systemd merges a start into a running oneshot), so each belongs to a run that is
// over without recording its end (a reboot, or a failed --ended). Each gets deadEnd and its
// note "… · ended unrecorded".
func deadRuns(starts, steps []store.Run) []store.Run {
	var out []store.Run
	for _, s := range starts {
		if !isStartRow(s) || s.FinishedAt != nil {
			continue
		}
		end := deadEnd(s, starts, steps)
		s.FinishedAt = &end
		s.Note += noteEnded + noteUnrecorded
		out = append(out, s)
	}
	return out
}

// closeDead finishes rows.starts' dead runs (deadRuns) inside the check's transaction and
// returns the rows as they now stand.
func closeDead(ctx context.Context, db store.DBTX, rows guardRows) (guardRows, error) {
	dead := deadRuns(rows.starts, rows.steps)
	if len(dead) == 0 {
		return rows, nil
	}
	starts := slices.Clone(rows.starts)
	for _, d := range dead {
		if _, err := db.Exec(ctx, `UPDATE run SET finished_at = $2, note = $3 WHERE id = $1 AND finished_at IS NULL`,
			d.ID, *d.FinishedAt, d.Note); err != nil {
			return guardRows{}, fmt.Errorf("timer guard: close run %d: %w", d.ID, err)
		}
		for i := range starts {
			if starts[i].ID == d.ID {
				starts[i] = d
			}
		}
	}
	rows.starts = starts
	return rows, nil
}

// startGuardRun opens the run's start row in one statement, note included, so a failure
// between two writes can never leave an anchor without a run.
func startGuardRun(ctx context.Context, db store.DBTX, command, job, note string) error {
	if _, err := db.Exec(ctx, `INSERT INTO run (command, source, note) VALUES ($1, $2, $3)`, command, job, note); err != nil {
		return fmt.Errorf("timer guard: start run %s %s: %w", command, job, err)
	}
	return nil
}

// guardRecord is the check with --record: in one transaction (holding timerGuardLockKey for a
// job that shares the fetch lock) it closes dead runs, reads, decides and opens the start row
// of a press or a due run. It returns the verdict it reached ("" when an error came first) and
// the plan or press time; the caller turns them into the exit. A deferral prints its line.
func guardRecord(ctx context.Context, s *store.Store, job string, g guardJob, reqs map[string]*time.Time, now time.Time, out io.Writer) (string, time.Time, error) {
	tx, err := s.Pool().Begin(ctx)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("timer guard: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if len(g.lockedBy) > 0 {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, timerGuardLockKey); err != nil {
			return "", time.Time{}, fmt.Errorf("timer guard: lock: %w", err)
		}
	}
	rows, err := readGuardRows(ctx, tx, job, g, now)
	if err != nil {
		return "", time.Time{}, err
	}
	if rows, err = closeDead(ctx, tx, rows); err != nil {
		return "", time.Time{}, err
	}
	_, held := rows.lockHeld(g, now)
	verdict, at := guardDecision(g, reqs, rows.served, rows.anchor(g), held, now)
	switch verdict {
	case verdictPress:
		err = startGuardRun(ctx, tx, commandRunNow, job, noteAskedAt+at.In(planZone).Format(time.RFC3339))
	case verdictDue:
		err = startGuardRun(ctx, tx, commandScheduled, job, notePlanned+at.In(planZone).Format(time.RFC3339))
	case verdictDeferred:
		what := "planned at"
		if pressWithin(g, reqs, rows.served, now, g.pressWindow()) != nil {
			what = "asked at"
		}
		_, _ = fmt.Fprintf(out, "%s: %s %s; waiting while %s runs.\n", g.name, what, hhmm(at, now.In(planZone)), g.lockWords)
	}
	if err != nil {
		return verdict, at, err
	}
	// A deferral or a wait closed dead runs only; the commit keeps that too.
	if err := tx.Commit(ctx); err != nil {
		return verdict, at, fmt.Errorf("timer guard: commit: %w", err)
	}
	return verdict, at, nil
}

// endedResult is what --ended records: systemd's verdict ($SERVICE_RESULT) and, when it is not
// success, the exit status ($EXIT_STATUS).
func endedResult(getenv func(string) string) string {
	result := getenv("SERVICE_RESULT")
	if result == "" {
		result = "unknown"
	}
	if exit := getenv("EXIT_STATUS"); result != "success" && exit != "" {
		result += " exit " + exit
	}
	return result
}

// endedAction is what --ended writes, pure: nothing after a passed-over tick (exec-condition);
// else the latest open start row closed with its note and "· ended <result>" (close non-zero);
// else, for a press that ran while its start could not be recorded (fresh within its window
// plus the unit's limit, served by no start row), a finished run-now row at the press, so the
// next tick does not run it again (insert set); else nothing.
func endedAction(g guardJob, rows guardRows, reqs map[string]*time.Time, serviceResult, result string, now time.Time) (closeID int64, note string, insert *store.Run) {
	if serviceResult == "exec-condition" {
		return 0, "", nil
	}
	var open *store.Run
	for i := range rows.starts {
		r := &rows.starts[i]
		if isStartRow(*r) && r.FinishedAt == nil && (open == nil || r.StartedAt.After(open.StartedAt) ||
			(r.StartedAt.Equal(open.StartedAt) && r.ID > open.ID)) {
			open = r
		}
	}
	if open != nil {
		return open.ID, open.Note + noteEnded + result, nil
	}
	r := pressWithin(g, reqs, rows.served, now, g.pressWindow()+g.limit)
	if r == nil {
		return 0, "", nil
	}
	end := now
	return 0, "", &store.Run{Command: commandRunNow, StartedAt: *r, FinishedAt: &end,
		Note: noteAskedAt + r.In(planZone).Format(time.RFC3339) + noteStartNotStored + noteEnded + result}
}

// guardEnded is --ended with --record: endedAction, in one transaction.
func guardEnded(ctx context.Context, s *store.Store, job string, g guardJob, reqs map[string]*time.Time, getenv func(string) string, now time.Time) error {
	if getenv("SERVICE_RESULT") == "exec-condition" {
		return nil // a passed-over tick: nothing to read or close
	}
	tx, err := s.Pool().Begin(ctx)
	if err != nil {
		return fmt.Errorf("timer guard: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	rows, err := readGuardRows(ctx, tx, job, g, now)
	if err != nil {
		return err
	}
	id, note, insert := endedAction(g, rows, reqs, getenv("SERVICE_RESULT"), endedResult(getenv), now)
	switch {
	case id != 0:
		if err := store.NewQueries(tx).FinishRun(ctx, id, store.RunResult{Note: note}); err != nil {
			return err
		}
	case insert != nil:
		if _, err := tx.Exec(ctx, `INSERT INTO run (command, source, started_at, finished_at, note) VALUES ($1, $2, $3, now(), $4)`,
			insert.Command, job, insert.StartedAt, insert.Note); err != nil {
			return fmt.Errorf("timer guard: record the press: %w", err)
		}
	default:
		return nil
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("timer guard: commit: %w", err)
	}
	return nil
}

// guardRequests reads g's request files, each at most guardWindow old; one that cannot be read
// is no press (one warning line).
func guardRequests(dataDir, job string, g guardJob, now time.Time) map[string]*time.Time {
	reqs := map[string]*time.Time{}
	if dataDir == "" {
		return reqs
	}
	for _, a := range g.actions {
		at, err := readRunRequest(dataDir, a)
		switch {
		case err != nil:
			slog.Warn("timer-guard: request file unreadable, no press", "job", job, "action", a, "err", err)
		case at != nil && now.Sub(*at) <= guardWindow(g):
			reqs[a] = at
		}
	}
	return reqs
}

// planWords is t in the timers' zone, in full, for the operator's check.
func planWords(t time.Time) string { return t.In(planZone).Format("Mon 2 Jan 15:04 MST") }

// guardExplain prints the verdict, the anchor and the plan for an operator's check by hand,
// and writes nothing.
func guardExplain(out io.Writer, job string, g guardJob, rows guardRows, reqs map[string]*time.Time, now time.Time) {
	since, held := rows.lockHeld(g, now)
	anchor := rows.anchor(g)
	verdict, at := guardDecision(g, reqs, rows.served, anchor, held, now)
	if r := latestStart(rows.starts); r != nil && r.FinishedAt == nil {
		_, _ = fmt.Fprintf(out, "%s (%s): a run is open since %s; it is running, or it ended without recording that (the next check closes it).\n",
			g.name, job, planWords(r.StartedAt))
	}
	switch verdict {
	case verdictPress:
		_, _ = fmt.Fprintf(out, "%s (%s): a press asked at %s is waiting; the next check starts it.\n", g.name, job, planWords(at))
	case verdictDue:
		_, _ = fmt.Fprintf(out, "%s (%s): due now, planned at %s; the next check starts it.\n", g.name, job, planWords(at))
	case verdictDeferred:
		_, _ = fmt.Fprintf(out, "%s (%s): planned at %s; waiting while %s runs (since %s).\n", g.name, job, planWords(at), g.lockWords, planWords(since))
	default:
		_, _ = fmt.Fprintf(out, "%s (%s): not due; next run planned at %s.\n", g.name, job, planWords(at))
	}
	switch r := latestStart(rows.counted()); {
	case r != nil && r.Command == commandRunNow:
		_, _ = fmt.Fprintf(out, "Counted from the run by hand at %s.\n", planWords(*anchor))
	case r != nil:
		_, _ = fmt.Fprintf(out, "Counted from the run on schedule planned at %s (started %s).\n", planWords(*anchor), planWords(r.StartedAt))
	case anchor != nil:
		_, _ = fmt.Fprintf(out, "Counted from its old schedule's slot at %s (its last run before the due-check).\n", planWords(*anchor))
	default:
		_, _ = fmt.Fprintln(out, "No run recorded yet, so the next run is at its usual time.")
	}
	for _, r := range requestsAhead(g, reqs, now) {
		_, _ = fmt.Fprintf(out, "A request time %s is ahead of the clock and is ignored.\n", planWords(r))
	}
	_, _ = fmt.Fprintln(out, "Nothing was written: only the unit's own check (--record) writes.")
}

// guardOpen opens the store for the check, with a short connect timeout.
func guardOpen(ctx context.Context, dsn string) (*store.Store, error) {
	return store.Open(ctx, dsn, appName(), func(c *pgxpool.Config) {
		c.ConnConfig.ConnectTimeout = guardConnect
		c.MaxConns = 2
	})
}

// cmdTimerGuard is `leadhound timer-guard [--record] [--ended] [--explain] <job>`.
//
// Without --record it prints the verdict, the anchor and the plan, writes nothing and exits 0:
// the operator's check, so it never opens a scheduled row that starts nothing and moves the
// plan. Under systemd without --record (INVOCATION_ID set: a unit line that lost it) it is
// errNotNow, so a mis-edited unit waits instead of running the job at every tick.
//
// With --record (only the unit passes it) it decides and writes (guardRecord): nil starts the
// job, errNotNow (exit 3) passes over this tick. Any error is errNotNow with one warning line,
// except a fresh press, which runs even when the database cannot be read (the job then reports
// the database, and --ended records the press) unless the check already let it through or
// found it served: every press it deals with is marked beside its request (servedMarker). With --ended it closes the run's row instead
// (guardEnded) and always exits 0: a failed close is healed by the next check's closeDead.
func cmdTimerGuard(ctx context.Context, args []string) (err error) {
	var job string
	defer func() {
		if r := recover(); r != nil {
			slog.Warn("timer-guard: crashed, not now", "job", job, "panic", fmt.Sprint(r))
			err = errNotNow
		}
	}()
	fs := flag.NewFlagSet("timer-guard", flag.ContinueOnError)
	record := fs.Bool("record", false, "decide and write (only the unit passes it)")
	endedFlag := fs.Bool("ended", false, "close the run's row with systemd's result")
	explain := fs.Bool("explain", false, "print the verdict, the anchor and the plan")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageError("usage: leadhound timer-guard [--record] [--ended] [--explain] <job>")
	}
	job = pos[0]
	g, ok := guardJobs[job]
	if !ok {
		slog.Warn("timer-guard: unknown job, not now", "job", job)
		return errNotNow
	}
	if !*record && os.Getenv("INVOCATION_ID") != "" {
		slog.Warn("timer-guard: run by systemd without --record, not now", "job", job)
		return errNotNow
	}
	ctx, cancel := context.WithTimeout(ctx, guardBudget)
	defer cancel()
	now := guardClock()
	cfg, cfgErr := loadConfig("LEADHOUND_DATABASE_URL")
	reqs := guardRequests(cfg.DataDir, job, g, now)
	if ahead := requestsAhead(g, reqs, now); len(ahead) > 0 {
		slog.Warn("timer-guard: request time ahead of the clock, ignored", "job", job, "at", ahead[0].Format(time.RFC3339))
	}

	if *endedFlag {
		if !*record {
			_, _ = fmt.Fprintln(stdout, "--ended writes only with --record; nothing was written.")
			return nil
		}
		if err := guardEndedOpen(ctx, cfg, cfgErr, job, g, reqs, now); err != nil {
			slog.Warn("timer-guard: run end not recorded", "job", job, "err", err)
		}
		return nil
	}

	if !*record {
		if cfgErr != nil {
			return cfgErr
		}
		s, err := guardOpen(ctx, cfg.DatabaseURL.Reveal())
		if err != nil {
			return err
		}
		defer s.Close()
		rows, err := readGuardRows(ctx, s.Pool(), job, g, now)
		if err != nil {
			return err
		}
		guardExplain(stdout, job, g, rows, reqs, now)
		return nil
	}

	// A press fresh enough to run even when the database cannot tell whether it was served,
	// unless the check already let it through or found it served (its marker, markedServed).
	freshPress := pressWithin(g, reqs, func(r time.Time) bool { return markedServed(cfg.DataDir, g, reqs, r) }, now, pressFresh)
	verdict, at, err := guardRecordOpen(ctx, cfg, cfgErr, job, g, reqs, now)
	if *explain && verdict != "" {
		_, _ = fmt.Fprintf(stdout, "%s (%s): %s, %s.\n", g.name, job, verdict, planWords(at))
	}
	switch {
	case err != nil && verdict == verdictPress:
		slog.Warn("timer-guard: press start not recorded, running it", "job", job, "err", err)
		markServedOrWarn(cfg.DataDir, job, g, reqs, at)
		return nil
	case err != nil && verdict == "" && freshPress != nil:
		// The marker is what keeps this press from running again at the next tick while the
		// database stays out of reach: unwritten, the check waits.
		if merr := markServed(cfg.DataDir, g, reqs, *freshPress); merr != nil {
			slog.Warn("timer-guard: database unreadable and the press could not be marked served, not now", "job", job, "err", err, "marker", merr)
			return errNotNow
		}
		slog.Warn("timer-guard: database unreadable, running the fresh press", "job", job, "err", err)
		return nil
	case err != nil:
		slog.Warn("timer-guard: in doubt, not now", "job", job, "err", err)
		return errNotNow
	}
	if r := pressDealt(g, reqs, verdict, at, now); r != nil {
		markServedOrWarn(cfg.DataDir, job, g, reqs, *r)
	}
	if verdict == verdictPress || verdict == verdictDue {
		return nil
	}
	return errNotNow
}

// pressDealt is the press the check's recorded decision dealt with: the one it let through
// (verdictPress), or the newest fresh press it found already served by a run (any verdict but
// a press waiting behind the lock); nil when there is none.
func pressDealt(g guardJob, reqs map[string]*time.Time, verdict string, at, now time.Time) *time.Time {
	if verdict == verdictPress {
		return &at
	}
	r := pressWithin(g, reqs, func(time.Time) bool { return false }, now, g.pressWindow())
	if r == nil || (verdict == verdictDeferred && at.Equal(*r)) {
		return nil
	}
	return r
}

// servedMarker is the file beside action's request file (in the data directory's run/ folder,
// the unit's own, 0700) that holds the time of the last press the check let through or found
// served. Only the database-down exception reads it (markedServed): a press that has had its
// run then never runs again while the database cannot say so. Request files are never deleted.
func servedMarker(dataDir, action string) string {
	return filepath.Join(dataDir, runRequestDir, "."+action+".served")
}

// pressActions are g's actions whose request is at r.
func pressActions(g guardJob, reqs map[string]*time.Time, r time.Time) []string {
	var out []string
	for _, a := range g.actions {
		if at := reqs[a]; at != nil && at.Equal(r) {
			out = append(out, a)
		}
	}
	return out
}

// markServed writes r into the marker of each of g's actions whose request is r: to a temp
// file beside it (0600), then renamed into place, so a reader never sees half a time.
func markServed(dataDir string, g guardJob, reqs map[string]*time.Time, r time.Time) error {
	if dataDir == "" {
		return errors.New("timer guard: LEADHOUND_DATA_DIR is not set")
	}
	for _, a := range pressActions(g, reqs, r) {
		path := servedMarker(dataDir, a)
		f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
		if err != nil {
			return fmt.Errorf("timer guard: mark served: %w", err)
		}
		tmp := f.Name()
		_, werr := f.WriteString(r.UTC().Format(time.RFC3339Nano) + "\n")
		if cerr := f.Close(); werr == nil {
			werr = cerr
		}
		if werr == nil {
			werr = os.Rename(tmp, path)
		}
		if werr != nil {
			_ = os.Remove(tmp)
			return fmt.Errorf("timer guard: mark served: %w", werr)
		}
	}
	return nil
}

// markServedOrWarn is markServed on the database's road, where the database already holds the
// truth: a marker that cannot be written is one warning line and never stops the job.
func markServedOrWarn(dataDir, job string, g guardJob, reqs map[string]*time.Time, r time.Time) {
	if err := markServed(dataDir, g, reqs, r); err != nil {
		slog.Warn("timer-guard: press not marked served", "job", job, "err", err)
	}
}

// markedServed is whether the marker of one of g's actions whose request is r holds r. A
// marker that cannot be read, or holds no time, counts as served: when in doubt, wait.
func markedServed(dataDir string, g guardJob, reqs map[string]*time.Time, r time.Time) bool {
	for _, a := range pressActions(g, reqs, r) {
		b, err := os.ReadFile(servedMarker(dataDir, a)) // #nosec G304 -- action is one of the fixed guardJobs actions
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return true
		}
		if at, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(string(b))); err != nil || at.Equal(r) {
			return true
		}
	}
	return false
}

// guardRecordOpen opens the store and runs guardRecord.
func guardRecordOpen(ctx context.Context, cfg config.Config, cfgErr error, job string, g guardJob, reqs map[string]*time.Time, now time.Time) (string, time.Time, error) {
	if cfgErr != nil {
		return "", time.Time{}, cfgErr
	}
	s, err := guardOpen(ctx, cfg.DatabaseURL.Reveal())
	if err != nil {
		return "", time.Time{}, err
	}
	defer s.Close()
	return guardRecord(ctx, s, job, g, reqs, now, stdout)
}

// guardEndedOpen opens the store and runs guardEnded.
func guardEndedOpen(ctx context.Context, cfg config.Config, cfgErr error, job string, g guardJob, reqs map[string]*time.Time, now time.Time) error {
	if cfgErr != nil {
		return cfgErr
	}
	s, err := guardOpen(ctx, cfg.DatabaseURL.Reveal())
	if err != nil {
		return err
	}
	defer s.Close()
	return guardEnded(ctx, s, job, g, reqs, os.Getenv, now)
}
