package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/gremy/webbership-leadhound/internal/console"
	"github.com/gremy/webbership-leadhound/internal/leadbrief"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/linkedin"
	"github.com/gremy/webbership-leadhound/internal/outreach"
	"github.com/gremy/webbership-leadhound/internal/preview"
	"github.com/gremy/webbership-leadhound/internal/store"
)

// The console asks and systemd runs: a Run now press writes the request time to
// <data>/run/<action>, and that action's .path watcher starts its one fixed unit
// (deploy/README.md, Run now). The state of each action is read back from the request
// time and the run table.

// runRequestDir is the folder of request files inside the data directory.
const runRequestDir = "run"

// The card's windows.
const (
	runNowLookback   = 48 * time.Hour   // runs read for the card; an older request is forgotten
	runNowStuckGrace = 10 * time.Minute // past an action's wait plus this, a request never started is stuck
	fetchCooldown    = 60 * time.Minute // a fetch waits this long after a finished LinkedIn collect
)

// The units' limits. Each Wait mirrors the lock wait the unit's ExecStart takes (or, for
// the fetch, the collect unit it is ordered after); each Run mirrors its TimeoutStartSec,
// past which systemd has killed it and the run row never gets a finished_at.
const (
	previewWait        = 30 * time.Minute             // leadhound-preview.service: flock --wait 1800
	previewRunLimit    = time.Hour                    // leadhound-preview.service: TimeoutStartSec=1h
	fetchWait          = 5*time.Hour + 30*time.Minute // leadhound-fetch.service waits behind leadhound-collect.service
	fetchRunLimit      = time.Hour                    // leadhound-fetch.service: TimeoutStartSec=1h
	briefWait          = 30 * time.Minute             // leadhound-brief.service: flock --wait 1800
	briefRunLimit      = time.Hour                    // leadhound-brief.service: TimeoutStartSec=1h
	atsWait            = 10 * time.Minute             // leadhound-ats.service takes no lock: it starts at once
	atsRunLimit        = 8*time.Hour + 15*time.Minute // leadhound-ats.service: TimeoutStartSec=8h15min
	collectUnitTimeout = 5*time.Hour + 30*time.Minute // leadhound-collect.service: TimeoutStartSec=5h30min
	// atsScoreWait mirrors leadhound-ats.service's score step, flock --wait 5400 on the
	// score lock: past it (and the grace) after the last ATS collect, the score never started.
	atsScoreWait = 5400 * time.Second
)

// runNoteDidNotFinish is the note preview.go and brief.go defer onto their run row and
// replace only on success: a row closed with it finished badly.
const runNoteDidNotFinish = "did not finish"

// runLimits is how long an action may wait to start, and how long its unit may run.
type runLimits struct{ Wait, Run time.Duration }

var runNowLimits = map[string]runLimits{
	console.RunPreview: {Wait: previewWait, Run: previewRunLimit},
	console.RunFetch:   {Wait: fetchWait, Run: fetchRunLimit},
	console.RunBrief:   {Wait: briefWait, Run: briefRunLimit},
	console.RunATS:     {Wait: atsWait, Run: atsRunLimit},
}

// The run identities each action's steps write (see the collectors and preview.go).
const (
	previewFetchSource = "fetch" // preview --after-fetch
)

func isLinkedInCollect(r store.Run) bool {
	return r.Command == linkedin.CommandCollect && (r.Source == linkedin.Source || r.Source == linkedin.SourceFixture)
}

// isATSCollect is one ATS's collect row: the ATS run writes one per ATS, under its name.
func isATSCollect(r store.Run) bool {
	return r.Command == linkedin.CommandCollect && !isLinkedInCollect(r)
}

func isScore(r store.Run) bool { return r.Command == "score" }

// finalRow is the row that ends a single-step action: the preview's own row, the fetch's
// own preview, the brief.
func finalRow(action string, r store.Run) bool {
	switch action {
	case console.RunPreview:
		return r.Command == "preview" && r.Source == ""
	case console.RunFetch:
		return r.Command == "preview" && r.Source == previewFetchSource
	case console.RunBrief:
		return r.Command == "brief"
	}
	return false
}

// ended is when r ended: its finished_at (badly when its note is still the deferred "did
// not finish"), or, for a run still open past limit, the moment its unit's timeout killed
// it (badly).
func ended(r store.Run, now time.Time, limit time.Duration) (at time.Time, ok, badly bool) {
	if r.FinishedAt != nil {
		return *r.FinishedAt, true, r.Note == runNoteDidNotFinish
	}
	if now.Sub(r.StartedAt) > limit {
		return r.StartedAt.Add(limit), true, true
	}
	return time.Time{}, false, false
}

// progress is how far one instance of an action got after its anchor.
type progress struct {
	started *time.Time    // the first step's start; nil when nothing started
	step    string        // what runs now, while not done
	done    *time.Time    // the final step's end
	badly   bool          // the final step, or the unit, never finished
	stuck   time.Duration // non-zero: the next step never started within this wait (plus the grace)
}

// runProgress walks action's steps in order from after: a step counts only if it started
// at or after the previous step ended, and the first at or after after.
func runProgress(action string, after time.Time, runs []store.Run, now time.Time, lim runLimits) progress {
	var p progress
	if action == console.RunATS {
		return atsProgress(after, runs, now, lim)
	}
	var last *store.Run
	for i := range runs {
		r := runs[i]
		if finalRow(action, r) && !r.StartedAt.Before(after) && (last == nil || !r.StartedAt.Before(last.StartedAt)) {
			last = &runs[i]
		}
	}
	if action == console.RunFetch {
		// The collect the fetch pulled in, when it started after the press.
		var c *store.Run
		for i := range runs {
			r := runs[i]
			if isLinkedInCollect(r) && !r.StartedAt.Before(after) && (last == nil || r.StartedAt.Before(last.StartedAt)) {
				if c == nil {
					c = &runs[i]
				}
				if p.started == nil {
					at := r.StartedAt
					p.started = &at
				}
			}
		}
		if last == nil && c != nil {
			if end, ok, _ := ended(*c, now, collectUnitTimeout); ok {
				// The fetch's preview starts once the collect ends, after at most its
				// LinkedIn lock wait; past that it never will. A timed collect that ran
				// after a request the watcher never took ends the same way.
				if now.Sub(end) > previewWait+runNowStuckGrace {
					p.stuck = previewWait
					return p
				}
				p.step = "scoring, then the results"
			} else {
				p.step = "reading LinkedIn"
			}
			return p
		}
	}
	if last == nil {
		return p
	}
	if p.started == nil {
		at := last.StartedAt
		p.started = &at
	}
	if at, ok, badly := ended(*last, now, lim.Run); ok {
		p.done, p.badly = &at, badly
		return p
	}
	switch action {
	case console.RunBrief:
		p.step = "composing and sending today's email"
	default:
		p.step = "checking and ranking the results"
	}
	return p
}

// atsProgress: every ATS collect row started after after, all ended; then a score started
// after the last of them ended. A score that began while ATS rows were still open is
// another unit's and never ends the ATS run.
func atsProgress(after time.Time, runs []store.Run, now time.Time, lim runLimits) progress {
	var p progress
	var lastEnd time.Time
	open, badly := false, false
	for _, r := range runs {
		if !isATSCollect(r) || r.StartedAt.Before(after) {
			continue
		}
		if p.started == nil {
			at := r.StartedAt
			p.started = &at
		}
		at, ok, b := ended(r, now, lim.Run)
		if !ok {
			open = true
			continue
		}
		badly = badly || b
		if at.After(lastEnd) {
			lastEnd = at
		}
	}
	if p.started == nil {
		return p
	}
	if open {
		p.step = "collecting from the job boards"
		return p
	}
	if badly {
		p.done, p.badly = &lastEnd, true
		return p
	}
	// Score rows carry no source, so every score that started after the last ATS collect
	// ended, within the ATS score's lock wait, is the closest proof: the run is done only
	// once all of them have finished. Another unit's score may hold the lock first, but the
	// ATS score's flock wakes the moment the earlier score frees it, so no gap shows.
	scoreBy := lastEnd.Add(atsScoreWait + runNowStuckGrace)
	var doneAt time.Time
	scored, scoredBadly := false, false
	for _, r := range runs {
		if !isScore(r) || r.StartedAt.Before(lastEnd) || r.StartedAt.After(scoreBy) {
			continue
		}
		at, ok, b := ended(r, now, lim.Run)
		if !ok {
			p.step = "scoring"
			return p
		}
		if !scored || at.After(doneAt) {
			doneAt, scoredBadly = at, b
		}
		scored = true
	}
	if scored {
		p.done, p.badly = &doneAt, scoredBadly
		return p
	}
	if now.After(scoreBy) {
		// The score never took the lock within its wait: the run did not finish.
		p.done, p.badly = &scoreBy, true
		return p
	}
	p.step = "waiting to score"
	return p
}

// runAnchor is where the latest instance of action began, for a card with no request: the
// latest final row, or for the ATS run its latest collect row.
func runAnchor(action string, runs []store.Run) (time.Time, bool) {
	var at time.Time
	found := false
	for _, r := range runs {
		match := finalRow(action, r)
		if action == console.RunATS {
			match = isATSCollect(r)
		}
		if match && (!found || !r.StartedAt.Before(at)) {
			at, found = r.StartedAt, true
		}
	}
	return at, found
}

// hhmm is t's wall clock in now's location.
func hhmm(t, now time.Time) string {
	t = t.In(now.Location())
	ty, tm, td := t.Date()
	ny, nm, nd := now.Date()
	if ty == ny && tm == nm && td == nd {
		return t.Format("15:04")
	}
	// a time from another day says which, or "21:30" reads as tonight
	if y := now.AddDate(0, 0, -1); y.Year() == ty && y.Month() == tm && y.Day() == td {
		return "yesterday " + t.Format("15:04")
	}
	return t.Format("Mon 2 Jan 15:04")
}

// runNowState is one action's state on the card, from its request time (nil when none),
// the runs since a while before it, and the time now in the operator's location:
//   - no request: done ("Last done at HH:MM") or running from the latest run, idle when
//     none ran in the window;
//   - the final step ended after the request: done, or "did not finish" when its unit's
//     timeout killed it;
//   - a step started after the request: running, naming the step;
//   - nothing started: waiting (naming what holds LinkedIn, for preview and fetch), or
//     stuck past the wait plus a grace, naming both causes and the fix.
func runNowState(action string, requested *time.Time, runs []store.Run, now time.Time, lim runLimits) console.RunNowAction {
	a := console.RunNowAction{Action: action}
	var after time.Time
	if requested == nil {
		at, ok := runAnchor(action, runs)
		if !ok {
			a.State, a.Line = "idle", "Not run in the last two days."
			return a
		}
		after = at
	} else {
		after = *requested
	}
	p := runProgress(action, after, runs, now, lim)
	switch {
	case p.done != nil:
		a.State, a.Since = "done", p.done
		a.Line = "Last done at " + hhmm(*p.done, now) + "."
		if p.badly {
			a.Line = "The last run did not finish. It started at " + hhmm(*p.started, now) + "."
		}
		return a
	case p.stuck != 0:
		a.State, a.Since = "stuck", requested
		if requested == nil {
			a.Since = p.started
		}
		a.Line = stuckLine(action, hhmm(*a.Since, now), p.stuck)
		return a
	case p.started != nil:
		a.State, a.Since, a.Pending = "running", p.started, true
		a.Line = "Running since " + hhmm(*p.started, now) + ": " + p.step + "."
		return a
	}
	asked := hhmm(*requested, now)
	a.Since = requested
	if now.Sub(*requested) > lim.Wait+runNowStuckGrace {
		a.State, a.Line = "stuck", stuckLine(action, asked, lim.Wait)
		return a
	}
	a.State, a.Pending = "waiting", true
	a.Line = "Asked at " + asked + ", waiting for its turn."
	if action == console.RunPreview || action == console.RunFetch {
		if what, since, ok := linkedInHolder(action, runs, now); ok {
			a.Line = fmt.Sprintf("Asked at %s, waiting for LinkedIn (%s is running since %s).", asked, what, hhmm(since, now))
		}
	}
	return a
}

// stuckLine is the stuck sentence: both causes (LinkedIn busy past wait, or a watcher that
// is not running) and the fix.
func stuckLine(action, asked string, wait time.Duration) string {
	cause := fmt.Sprintf("LinkedIn may have stayed busy for %s (press again), or the watcher is not running", waitWords(wait))
	if action == console.RunATS {
		cause = "The watcher may not be running"
	}
	return fmt.Sprintf("Asked at %s and not started. %s: see deploy/README.md, Run now "+
		"(systemctl reset-failed leadhound-%s.path leadhound-%s.service, then systemctl start leadhound-%s.path).",
		asked, cause, action, action, action)
}

// waitWords is a lock wait in words: "30 minutes", "5h30".
func waitWords(d time.Duration) string {
	if d < time.Hour {
		return fmt.Sprintf("%d minutes", int(d.Minutes()))
	}
	return fmt.Sprintf("%dh%02d", int(d.Hours()), int(d.Minutes())%60)
}

// linkedInHolder is the latest open run that holds the LinkedIn lock a preview waits for:
// a LinkedIn collect, the brief, or the other kind of preview.
func linkedInHolder(action string, runs []store.Run, now time.Time) (string, time.Time, bool) {
	what, since, found := "", time.Time{}, false
	for _, r := range runs {
		var w string
		limit := collectUnitTimeout
		switch {
		case isLinkedInCollect(r):
			w = "a collect"
		case r.Command == "brief":
			w, limit = "the brief", briefRunLimit
		case r.Command == "preview" && !finalRow(action, r):
			w, limit = "another preview", previewRunLimit
		default:
			continue
		}
		if _, ok, _ := ended(r, now, limit); ok {
			continue
		}
		if !found || !r.StartedAt.Before(since) {
			what, since, found = w, r.StartedAt, true
		}
	}
	return what, since, found
}

// runNowFacts is what the refusals read besides the action's own state.
type runNowFacts struct {
	briefSentAt    *time.Time // today's brief went at
	collectEnded   *time.Time // the latest LinkedIn collect that finished
	collectRunning *time.Time // the start of a LinkedIn collect still open
	paused         bool       // LinkedIn is paused for the local day
	pausedReason   string     // throttled or blocked
	pausedAt       *time.Time // when, if the run is in the window
}

// collectFacts reads the LinkedIn collect and pause times out of runs.
func collectFacts(runs []store.Run, now time.Time) (f runNowFacts) {
	for _, r := range runs {
		if isLinkedInCollect(r) {
			if r.FinishedAt != nil {
				if f.collectEnded == nil || r.FinishedAt.After(*f.collectEnded) {
					at := *r.FinishedAt
					f.collectEnded = &at
				}
			} else if _, ok, _ := ended(r, now, collectUnitTimeout); !ok {
				at := r.StartedAt
				f.collectRunning = &at
			}
		}
		if (r.Source == linkedin.Source || r.Source == linkedin.SourceFixture) &&
			(r.StopReason == "throttled" || r.StopReason == "blocked") && r.FinishedAt != nil {
			if f.pausedAt == nil || r.FinishedAt.After(*f.pausedAt) {
				at := *r.FinishedAt
				f.pausedAt = &at
			}
		}
	}
	return f
}

func refuse(format string, args ...any) error {
	return fmt.Errorf("%w: %s", console.ErrUserFixable, fmt.Sprintf(format, args...))
}

// runNowDecision is whether a press of req goes ahead, given the action's state and the
// facts, with the sentence the operator sees. Every refusal wraps console.ErrUserFixable.
func runNowDecision(req console.RunRequest, st console.RunNowAction, f runNowFacts, now time.Time) (string, error) {
	if !slices.Contains(console.RunActions, req.Action) {
		return "", refuse("%q is not a Run now action; use one of %s", req.Action, strings.Join(console.RunActions, ", "))
	}
	// systemd merges a start into a unit already starting or running, so a press now
	// would be swallowed: say so instead.
	switch st.State {
	case "waiting":
		return "", refuse("Already asked at %s.", hhmm(*st.Since, now))
	case "running":
		if req.Action == console.RunATS {
			return "", refuse("The ATS run is already running since %s.", hhmm(*st.Since, now))
		}
		return "", refuse("Already running since %s.", hhmm(*st.Since, now))
	}
	pause := "LinkedIn is paused today (" + f.pausedReason
	if f.pausedAt != nil {
		pause += " at " + hhmm(*f.pausedAt, now)
	}
	pause += ")"
	switch req.Action {
	case console.RunBrief:
		if f.briefSentAt != nil {
			return "", refuse("Today's email already went at %s.", hhmm(*f.briefSentAt, now))
		}
		return "Asked. Today's email is on its way.", nil
	case console.RunATS:
		if !req.Confirmed {
			return "", refuse("Tick the box to confirm: this run takes hours.")
		}
		return "Asked. The ATS run has started; it takes hours.", nil
	case console.RunFetch:
		if f.paused {
			return "", refuse("%s; a fetch would read nothing.", pause)
		}
		if f.collectRunning != nil {
			return fmt.Sprintf("Asked. Joining the collect running since %s; results follow it.", hhmm(*f.collectRunning, now)), nil
		}
		// Every extra collect is about 85 LinkedIn requests, and one throttled run
		// pauses LinkedIn for the whole local day.
		if f.collectEnded != nil && now.Sub(*f.collectEnded) < fetchCooldown {
			return "", refuse("LinkedIn was read at %s; fetch again after %s.",
				hhmm(*f.collectEnded, now), hhmm(f.collectEnded.Add(fetchCooldown), now))
		}
		return "Asked. Fetching from LinkedIn, then results; this takes a few minutes.", nil
	default: // preview
		if f.paused {
			return fmt.Sprintf("Asked. %s, so nothing is rechecked; the results show what is collected.", pause), nil
		}
		return "Asked. Results usually in a minute or two; longer if LinkedIn is busy.", nil
	}
}

// runRequestJobs are the request files whose unit runs a job with a switch (job_switch): a
// request for one while its job is switched off is refused before the file is written.
var runRequestJobs = map[string]string{researchAction: leadstore.JobResearch}

// switchedOffRequest refuses (console.ErrUserFixable) a request for action while the job its
// unit runs is switched off; nil for an action whose job is on or has no switch.
func switchedOffRequest(ctx context.Context, lq leadstore.Queries, action string) error {
	job, ok := runRequestJobs[action]
	if !ok {
		return nil
	}
	off, _, err := jobOff(ctx, lq, job)
	if err != nil || !off {
		return err
	}
	return refuse("%s is switched off on the Controls page.", jobName(job))
}

// writeRunRequest writes the request time into <dataDir>/run/<action>. A plain write is
// right: PathChanged= fires when the file is closed after the write, and a torn read of
// the time only falls back to the file's mtime.
func writeRunRequest(dataDir, action string, at time.Time) error {
	if dataDir == "" {
		return errors.New("run now: LEADHOUND_DATA_DIR is not set")
	}
	dir := filepath.Join(dataDir, runRequestDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("run now: %w", err)
	}
	path := filepath.Join(dir, action)
	if err := os.WriteFile(path, []byte(at.UTC().Format(time.RFC3339Nano)+"\n"), 0o600); err != nil {
		return fmt.Errorf("run now: %w", err)
	}
	return nil
}

// readRunRequest is the time in <dataDir>/run/<action>, nil when there is none: an RFC3339
// time, or the "at" of a manual Send now request's JSON line (sendNowRequest). Anything else,
// an empty file or JSON that does not parse included, falls back to the file's mtime; this
// time is for the console's state only, never what a send acts on (readSendNowRequest).
func readRunRequest(dataDir, action string) (*time.Time, error) {
	path := filepath.Join(dataDir, runRequestDir, action)
	b, err := os.ReadFile(path) // #nosec G304 -- action is one of console.RunActions
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("run now: %w", err)
	}
	text := strings.TrimSpace(string(b))
	at, err := time.Parse(time.RFC3339Nano, text)
	if err != nil && strings.HasPrefix(text, "{") {
		var req sendNowRequest
		if jerr := json.Unmarshal([]byte(text), &req); jerr == nil && !req.At.IsZero() {
			at, err = req.At, nil
		}
	}
	if err != nil {
		st, serr := os.Stat(path)
		if serr != nil {
			return nil, fmt.Errorf("run now: %w", serr)
		}
		at = st.ModTime()
	}
	return &at, nil
}

// sendNowRequest is a manual Send now request file's one JSON line, written by the console at
// confirm: the batch to send and when it was asked. The file is untrusted (the console and
// the send service run as one user), so it carries only the batch id; the override codes,
// the batch hash and the operator come from the send_request row the console wrote at
// confirm, which the send claims and checks again (leads send --from-request).
type sendNowRequest struct {
	BatchID int64     `json:"batch_id"`
	At      time.Time `json:"at"`
}

// The kinds of Send now request readSendNowRequest reads.
const (
	// sendRequestLegacy: an RFC3339 time or an empty file, as Controls' Send now writes it:
	// today's leads send --now.
	sendRequestLegacy = "legacy"
	// sendRequestManual: a JSON line naming a manual batch (sendNowRequest).
	sendRequestManual = "manual"
)

// maxSendNowRequest is the most a Send now request file may hold; anything longer is refused.
const maxSendNowRequest = 4096

// readSendNowRequest reads <dataDir>/run/leads-send-now for the send service: a JSON line with
// batch_id above 0 is a manual request for that batch; a time, an empty file or any other text
// not starting with "{" is a legacy request (the old service never read the file, so a torn
// write still means Send now). Text starting with "{" that does not parse as exactly
// sendNowRequest, or names no batch, is refused, as is a missing or oversized file.
func readSendNowRequest(dataDir string) (batchID int64, kind string, err error) {
	if dataDir == "" {
		return 0, "", errors.New("send now request: LEADHOUND_DATA_DIR is not set")
	}
	path := filepath.Join(dataDir, runRequestDir, sendNowAction)
	f, err := os.Open(path) // #nosec G304 -- a fixed name under the data directory
	if err != nil {
		return 0, "", fmt.Errorf("send now request: %w", err)
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, maxSendNowRequest+1))
	if err != nil {
		return 0, "", fmt.Errorf("send now request: %w", err)
	}
	if len(b) > maxSendNowRequest {
		return 0, "", fmt.Errorf("send now request: the file is over %d bytes", maxSendNowRequest)
	}
	text := strings.TrimSpace(string(b))
	if !strings.HasPrefix(text, "{") {
		return 0, sendRequestLegacy, nil
	}
	dec := json.NewDecoder(strings.NewReader(text))
	dec.DisallowUnknownFields()
	var req sendNowRequest
	if err := dec.Decode(&req); err != nil {
		return 0, "", fmt.Errorf("send now request: not a request line: %w", err)
	}
	if dec.More() {
		return 0, "", errors.New("send now request: more than one request in the file")
	}
	if req.BatchID <= 0 {
		return 0, "", fmt.Errorf("send now request: batch_id %d names no batch", req.BatchID)
	}
	return req.BatchID, sendRequestManual, nil
}

// runNowRead is the runs of the window and each action's request, a request older than
// the window forgotten. A request file that cannot be read is in errs.
func (b *backend) runNowRead(ctx context.Context, now time.Time) ([]store.Run, map[string]*time.Time, map[string]error, error) {
	since := now.Add(-runNowLookback)
	runs, err := b.s.Q().RunsSince(ctx, since)
	if err != nil {
		return nil, nil, nil, err
	}
	reqs, errs := map[string]*time.Time{}, map[string]error{}
	for _, a := range console.RunActions {
		at, err := readRunRequest(b.cfg.DataDir, a)
		switch {
		case err != nil:
			errs[a] = err
		case at != nil && !at.Before(since):
			reqs[a] = at
		}
	}
	return runs, reqs, errs, nil
}

// todaySentAt is when today's brief went, nil while unsent.
func (b *backend) todaySentAt(ctx context.Context, now time.Time) (*time.Time, error) {
	lb, err := b.s.Q().BriefByDay(ctx, now.In(b.loc).Format(time.DateOnly))
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return lb.SentAt, nil
}

// RunNow asks for one Run now action: it refuses what the operator can fix (an unknown
// action, a press while the action waits or runs, today's email already sent, the ATS
// warning not confirmed, a fetch right after a collect or while LinkedIn is paused), and
// otherwise writes the request file its watcher starts the unit from.
func (b *backend) RunNow(ctx context.Context, req console.RunRequest) (console.RunOutcome, error) {
	if !slices.Contains(console.RunActions, req.Action) {
		_, err := runNowDecision(req, console.RunNowAction{}, runNowFacts{}, b.now())
		return console.RunOutcome{}, err
	}
	now := b.now()
	runs, reqs, errs, err := b.runNowRead(ctx, now)
	if err != nil {
		return console.RunOutcome{}, err
	}
	if err := errs[req.Action]; err != nil {
		return console.RunOutcome{}, err
	}
	local := now.In(b.loc)
	st := runNowState(req.Action, reqs[req.Action], runs, local, runNowLimits[req.Action])
	f := collectFacts(runs, local)
	if f.briefSentAt, err = b.todaySentAt(ctx, now); err != nil {
		return console.RunOutcome{}, err
	}
	if f.pausedReason, f.paused, err = b.s.Q().PausedToday(ctx, linkedin.Source, b.loc, now); err != nil {
		return console.RunOutcome{}, err
	}
	msg, err := runNowDecision(req, st, f, local)
	if err != nil {
		return console.RunOutcome{}, err
	}
	if err := switchedOffRequest(ctx, leadstore.NewQueries(b.s.Pool()), req.Action); err != nil {
		return console.RunOutcome{}, err
	}
	if err := writeRunRequest(b.cfg.DataDir, req.Action, now); err != nil {
		return console.RunOutcome{}, err
	}
	return console.RunOutcome{Message: msg}, nil
}

// RunNowCard is every action's state, when today's email went and when the shown results
// were made. A request file that cannot be read makes its action unavailable.
func (b *backend) RunNowCard(ctx context.Context) (console.RunNowCard, error) {
	now := b.now()
	runs, reqs, errs, err := b.runNowRead(ctx, now)
	if err != nil {
		return console.RunNowCard{}, err
	}
	local := now.In(b.loc)
	var card console.RunNowCard
	for _, a := range console.RunActions {
		if err := errs[a]; err != nil {
			card.Actions = append(card.Actions, console.RunNowAction{Action: a, State: "unavailable",
				Line: "The request file cannot be read; see the console's log."})
			slog.Default().Warn("console run now: request file", "action", a, "err", err)
			continue
		}
		card.Actions = append(card.Actions, runNowState(a, reqs[a], runs, local, runNowLimits[a]))
	}
	if card.BriefSentAt, err = b.todaySentAt(ctx, now); err != nil {
		return console.RunNowCard{}, err
	}
	if p, err := preview.Read(b.cfg.DataDir); err == nil {
		at := p.MadeAt
		card.ResultsAt = &at
	}
	return card, nil
}

// Results is the latest preview.json, each job with its live tracking and listing status.
// No preview yet is console.ErrNotFound; so is one that cannot be read (an older version,
// say), with one logged line.
func (b *backend) Results(ctx context.Context) (console.Results, error) {
	p, err := preview.Read(b.cfg.DataDir)
	if errors.Is(err, preview.ErrNone) {
		return console.Results{}, console.ErrNotFound
	}
	if err != nil {
		slog.Default().Warn("console results: preview unreadable", "err", err)
		return console.Results{}, fmt.Errorf("%w: %w", console.ErrNotFound, err)
	}
	out := console.Results{MadeAt: p.MadeAt, RecheckOff: p.RecheckOff, Health: p.Health,
		Unreachable: p.Unreachable, Unscored: p.Unscored}
	if p.Day != "" {
		if out.Day, err = time.ParseInLocation(time.DateOnly, p.Day, b.loc); err != nil {
			return console.Results{}, fmt.Errorf("console: preview day %q: %w", p.Day, err)
		}
	}
	var ids []int64
	for _, j := range slices.Concat(p.Apply, p.YourCall) {
		ids = append(ids, j.ID)
	}
	if p.RunnerUp != nil {
		ids = append(ids, p.RunnerUp.ID)
	}
	sums := map[int64]store.JobSummary{}
	if len(ids) > 0 {
		if sums, err = b.s.Q().JobSummaries(ctx, ids); err != nil {
			return console.Results{}, err
		}
	}
	for _, j := range p.Apply {
		out.Apply = append(out.Apply, resultJob(j, sums))
	}
	for _, j := range p.YourCall {
		out.YourCall = append(out.YourCall, resultJob(j, sums))
	}
	if p.RunnerUp != nil {
		r := resultJob(p.RunnerUp.Job, sums)
		out.RunnerUp = &r
		out.RunnerUpWhy = p.RunnerUp.FailedCriterion
		if p.RunnerUp.Note != "" {
			out.RunnerUpWhy += ": " + p.RunnerUp.Note
		}
	}
	return out, nil
}

// resultJob is one preview job with its live status; a job the store no longer has keeps
// its preview facts and no status.
func resultJob(j preview.Job, sums map[int64]store.JobSummary) console.ResultJob {
	r := console.ResultJob{JobID: j.ID, Title: j.Title, Company: j.Company, Location: j.Location, URL: j.URL,
		ApplyType: j.ApplyType, ApplicantCount: j.ApplicantCount, PostedAt: j.PostedAt}
	if s, ok := sums[j.ID]; ok {
		r.Status, r.ListingStatus = s.Status, s.ListingStatus
	}
	for _, c := range j.Criteria {
		r.Criteria = append(r.Criteria, console.JobCriterion{Name: c.Name, Verdict: c.Verdict, Keywords: c.Note})
	}
	return r
}

// Send now: the Controls page (or the Leads page's Outreach list) asks for one send that
// skips the sending window, through the same request-file road as Run now: the console
// writes <data>/run/leads-send-now and leadhound-leads-send-now.path starts the fixed
// leadhound-leads-send-now.service (leads send --from-request). A time in the file is this
// Send now (leads send --now --max 25); a JSON line from a manual send's confirm
// (sendNowRequest) sends that batch alone with the overrides its send_request row holds. The
// console gains no privilege, and the run takes the same database send lock as the hourly
// send.

// sendNowAction is Send now's request file under runRequestDir, and its watcher's name.
const sendNowAction = "leads-send-now"

const (
	// sendNowWait is how long a Send now request may wait for its unit to start: the
	// watcher starts it at once, so past this (and the grace) it never will.
	sendNowWait = 2 * time.Minute
	// sendNowLookback is how far back the latest request and the send runs are read.
	sendNowLookback = 24 * time.Hour
)

// sentNote reads a send run note's count of emails sent ("sent 2").
var sentNote = regexp.MustCompile(`(?:^| · )sent (\d+)(?: ·|$)`)

// sendNowView is the state of Send now from its latest request (nil: none in the lookback),
// whether the send lock is held now (locked: a send runs, either kind, since the two share
// one lock) and the send runs since the lookback, oldest first: a send running, the request
// waiting for its unit or never started, or the request's own run (the first finished one
// after it whose note says "send now") and what it did. busy is the refusal while a send runs
// or a request waits.
type sendNowView struct {
	line    string
	pending bool
	busy    string
}

func sendNowState(requested *time.Time, locked bool, runs []store.Run, now time.Time) sendNowView {
	var v sendNowView
	var mine, open *store.Run
	for i := range runs {
		r := runs[i]
		if r.FinishedAt == nil {
			open = &runs[i]
		}
		if requested != nil && mine == nil && r.FinishedAt != nil && !r.StartedAt.Before(*requested) &&
			leadbrief.NoteWords(r.Note)[noteSendNow] {
			mine = &runs[i]
		}
	}
	if locked {
		since := ""
		if open != nil {
			since = " since " + hhmm(open.StartedAt, now)
		}
		v.line, v.pending = "A send is running"+since+".", true
		v.busy = "A send is running" + since + "; press again once it has finished."
		return v
	}
	if requested == nil {
		return v
	}
	asked := hhmm(*requested, now)
	switch {
	case mine == nil && now.Sub(*requested) <= sendNowWait+runNowStuckGrace:
		v.line, v.pending = "Send now asked at "+asked+", waiting to start.", true
		v.busy = "Send now was already asked at " + asked + "."
	case mine == nil && open != nil && !open.StartedAt.Before(*requested):
		v.line = "A send that started at " + hhmm(open.StartedAt, now) + ", after Send now was asked, did not finish."
	case mine == nil:
		v.line = "Send now asked at " + asked + " and not started: another send may have been running, or its watcher on the server is not running. Press again to ask once more."
	case leadbrief.NoteWords(mine.Note)[notePaused]:
		v.line = "Send now at " + hhmm(mine.StartedAt, now) + " sent nothing: sending was off."
	default:
		v.line = "Send now at " + hhmm(mine.StartedAt, now) + " finished."
		if m := sentNote.FindStringSubmatch(mine.Note); m != nil {
			v.line = fmt.Sprintf("Send now at %s sent %s.", hhmm(mine.StartedAt, now), m[1])
		}
	}
	return v
}

// sendLockHeld reports whether a send (the hourly one or Send now) holds the send lock now,
// read from pg_locks, so nothing is taken from the send that may be starting: a run killed
// without finishing its row holds nothing.
func sendLockHeld(ctx context.Context, db store.DBTX) (bool, error) {
	var held bool
	if err := db.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_locks
		   WHERE locktype = 'advisory' AND granted AND objsubid = 1
		     AND database = (SELECT oid FROM pg_database WHERE datname = current_database())
		     AND classid = ($1::bigint >> 32)::oid AND objid = ($1::bigint & 4294967295)::oid)`,
		leadstore.SendLockKey).Scan(&held); err != nil {
		return false, fmt.Errorf("console: send lock: %w", err)
	}
	return held, nil
}

// sendNowRead is the latest Send now request within the lookback (nil for none) and the
// send runs since then, oldest first. lenient leaves out a request file that cannot be read,
// with one logged line, so a page still shows; a press needs it read.
func (b *backend) sendNowRead(ctx context.Context, now time.Time, lenient bool) (*time.Time, []store.Run, error) {
	since := now.Add(-sendNowLookback)
	req, err := readRunRequest(b.cfg.DataDir, sendNowAction)
	switch {
	case err != nil && lenient:
		slog.Default().Warn("console send now: request file", "err", err)
		req = nil
	case err != nil:
		return nil, nil, err
	case req != nil && req.Before(since):
		req = nil
	}
	rows, err := b.s.Pool().Query(ctx,
		`SELECT id, command, source, started_at, finished_at, requests, failures, stop_reason, note
		 FROM run WHERE command = $1 AND started_at >= $2 ORDER BY started_at, id`, commandLeadsSend, since)
	if err != nil {
		return nil, nil, fmt.Errorf("console: send runs: %w", err)
	}
	runs, err := pgx.CollectRows(rows, pgx.RowToStructByName[store.Run])
	if err != nil {
		return nil, nil, fmt.Errorf("console: send runs: %w", err)
	}
	return req, runs, nil
}

// withSendNow is w with Send now's state read in.
func (b *backend) withSendNow(ctx context.Context, w console.SendingWhy) (console.SendingWhy, error) {
	now := b.now()
	req, runs, err := b.sendNowRead(ctx, now, true)
	if err != nil {
		return console.SendingWhy{}, err
	}
	locked, err := sendLockHeld(ctx, b.s.Pool())
	if err != nil {
		return console.SendingWhy{}, err
	}
	st := sendNowState(req, locked, runs, now.In(b.loc))
	w.SendNowLine, w.SendNowPending = st.line, st.pending
	return w, nil
}

// sendNowRefusal is why a Send now press must not go ahead over facts read once, in the
// words the page shows, or "": shadow mode, the sending switch off, nothing due now (or every
// due email held by its mailbox or its campaign), the send timer not enabled, and today's limit used on the mailboxes the
// due emails use.
func sendNowRefusal(f sendFacts) string {
	if !f.live {
		return "Shadow mode: LEADHOUND_OUTREACH is not live on the server, so Send now would send nothing for real."
	}
	if f.paused {
		return "Sending is switched off. Switch it on first; Send now only skips the sending window."
	}
	if !f.sendTimer {
		// Send now's watcher is armed with the send timer (install.sh's leads_timers): where
		// the hourly send does not run, Send now does not either.
		return whyNoTimerSentence
	}
	var due []leadstore.SendRow
	for _, r := range f.rows {
		if !r.DueAt.After(f.now) {
			due = append(due, r)
		}
	}
	if len(due) == 0 {
		return "Nothing approved is due now. Send now never brings an email forward; it only skips the sending window."
	}
	used := map[int64]bool{}
	for _, r := range due {
		if m, ok := f.mailboxes[r.MailboxID]; ok && sendReady(m) && f.assigned[[2]int64{r.CampaignID, r.MailboxID}] {
			used[r.MailboxID] = true
		}
	}
	if len(used) == 0 {
		return whyNot(f).Sentence
	}
	local := f.now.In(f.loc)
	loads := f.loads()
	room := outreach.RoomToday(f.cfg, loads, local)
	left := 0
	for id := range used {
		left += room[id]
	}
	if left == 0 {
		limit, _ := f.capLimit(used, loads, local)
		return limit + "; Send now only skips the sending window."
	}
	return ""
}

// SendNow asks for one Send now run (console.ControlsBackend).
func (b *backend) SendNow(ctx context.Context) (console.ControlOutcome, error) {
	setup, err := b.leadsSetup(ctx, leadsForProbe)
	if err != nil {
		return console.ControlOutcome{}, err
	}
	now := b.now()
	req, runs, err := b.sendNowRead(ctx, now, false)
	if err != nil {
		return console.ControlOutcome{}, err
	}
	locked, err := sendLockHeld(ctx, b.s.Pool())
	if err != nil {
		return console.ControlOutcome{}, err
	}
	if st := sendNowState(req, locked, runs, now.In(b.loc)); st.busy != "" {
		return console.ControlOutcome{}, refuse("%s", st.busy)
	}
	f, err := b.loadSendFacts(ctx, setup, readTimers(ctx, controlsGetenv), 0)
	if err != nil {
		return console.ControlOutcome{}, err
	}
	if why := sendNowRefusal(f); why != "" {
		return console.ControlOutcome{}, refuse("%s", why)
	}
	if err := writeRunRequest(setup.dataDir, sendNowAction, now); err != nil {
		return console.ControlOutcome{}, err
	}
	return console.ControlOutcome{Message: "Asked. The due emails go out now, within today's limits and spaced as usual."}, nil
}
