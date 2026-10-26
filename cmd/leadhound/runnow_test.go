package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/console"
	"github.com/gremy/webbership-leadhound/internal/preview"
	"github.com/gremy/webbership-leadhound/internal/store"
)

// runAt builds a run row started at start, finished at end when end is not nil.
func runAt(command, source string, start time.Time, end *time.Time) store.Run {
	return store.Run{Command: command, Source: source, StartedAt: start, FinishedAt: end}
}

func tp(t time.Time) *time.Time { return &t }

// TestRunNowStateGrid drives runNowState over every action and state cell, plus the
// cross-talk cells where another unit's run must not end an action.
func TestRunNowStateGrid(t *testing.T) {
	loc := time.FixedZone("test", 2*60*60)
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, loc)
	ago := func(d time.Duration) time.Time { return now.Add(-d) }
	req := tp(ago(5 * time.Minute))
	old := tp(ago(3 * time.Hour)) // past every wait but the fetch's

	// final returns the action's final row(s) started at start, ended at end.
	final := func(action string, start time.Time, end *time.Time) []store.Run {
		switch action {
		case console.RunPreview:
			return []store.Run{runAt("preview", "", start, end)}
		case console.RunFetch:
			return []store.Run{runAt("preview", "fetch", start, end)}
		case console.RunBrief:
			return []store.Run{runAt("brief", "", start, end)}
		default: // ats: one collect row, then its score
			if end == nil {
				return []store.Run{runAt("collect", "greenhouse", start.Add(-time.Minute), tp(start)), runAt("score", "", start, nil)}
			}
			return []store.Run{runAt("collect", "greenhouse", start.Add(-time.Minute), tp(start)), runAt("score", "", start, end)}
		}
	}
	firstStep := func(action string, start time.Time) []store.Run {
		switch action {
		case console.RunFetch:
			return []store.Run{runAt("collect", "linkedin", start, nil)}
		case console.RunATS:
			return []store.Run{runAt("collect", "greenhouse", start, nil)}
		}
		return final(action, start, nil)
	}

	type cell struct {
		name      string
		requested *time.Time
		runs      []store.Run
		state     string
		line      string // a part of the line
	}
	for _, action := range console.RunActions {
		lim := runNowLimits[action]
		stuckReq := tp(ago(lim.Wait + runNowStuckGrace + time.Minute))
		cells := []cell{
			{"cold start, empty run table", nil, nil, "idle", "Not run"},
			{"no request, last run done", nil, final(action, ago(2*time.Hour), tp(ago(time.Hour+50*time.Minute))), "done", "Last done at"},
			{"request only", req, nil, "waiting", "Asked at 11:55, waiting for its turn"},
			{"request older than the limit", stuckReq, nil, "stuck", "systemctl reset-failed leadhound-" + action + ".path leadhound-" + action + ".service, then systemctl start leadhound-" + action + ".path"},
			{"step 1 running", req, firstStep(action, ago(2*time.Minute)), "running", "Running since 11:58"},
			{"final step running", req, final(action, ago(2*time.Minute), nil), "running", "Running since"},
			{"final step finished after the request", req, final(action, ago(3*time.Minute), tp(ago(time.Minute))), "done", "Last done at 11:59"},
			{"final step finished before the request", req, final(action, ago(time.Hour), tp(ago(50*time.Minute))), "waiting", "Asked at 11:55"},
			{"unfinished run past its timeout", old, final(action, ago(lim.Run+time.Hour), nil), "done", "did not finish"},
		}
		if action == console.RunFetch {
			// The fetch waits behind a whole collect unit: 3 h is no stuck yet.
			cells[8].requested = tp(ago(lim.Run + time.Hour + time.Minute))
		}
		if action == console.RunATS {
			// An ATS score past the limit, its collect rows ended.
			cells[8].runs = []store.Run{runAt("collect", "greenhouse", ago(atsRunLimit+2*time.Hour), tp(ago(atsRunLimit+90*time.Minute))),
				runAt("score", "", ago(atsRunLimit+time.Hour), nil)}
			cells[8].requested = tp(ago(atsRunLimit + 3*time.Hour))
		}
		for _, c := range cells {
			t.Run(action+"/"+c.name, func(t *testing.T) {
				got := runNowState(action, c.requested, c.runs, now, lim)
				if got.Action != action || got.State != c.state || !strings.Contains(got.Line, c.line) {
					t.Fatalf("got %+v, want state %s with %q", got, c.state, c.line)
				}
				if got.Pending != (c.state == "waiting" || c.state == "running") {
					t.Errorf("Pending = %v for %s", got.Pending, c.state)
				}
			})
		}
	}

	extra := []struct {
		name, action string
		requested    *time.Time
		runs         []store.Run
		state, line  string
	}{
		{"ats: a timed score ends while ATS rows are open", console.RunATS, req, []store.Run{
			runAt("collect", "greenhouse", ago(4*time.Minute), tp(ago(3*time.Minute))),
			runAt("collect", "lever", ago(3*time.Minute), nil),
			runAt("score", "", ago(2*time.Minute), tp(ago(time.Minute))),
		}, "running", "collecting from the job boards"},
		{"ats: a score from before the collects ended does not count", console.RunATS, req, []store.Run{
			runAt("score", "", ago(4*time.Minute), tp(ago(time.Minute))),
			runAt("collect", "greenhouse", ago(3*time.Minute), tp(ago(2*time.Minute))),
		}, "running", "waiting to score"},
		{"ats: a LinkedIn collect is not an ATS row", console.RunATS, req, []store.Run{
			runAt("collect", "linkedin", ago(4*time.Minute), tp(ago(3*time.Minute))),
			runAt("score", "", ago(2*time.Minute), tp(ago(time.Minute))),
		}, "waiting", "waiting for its turn"},
		{"fetch: a separate Show results now does not end it", console.RunFetch, req, []store.Run{
			runAt("preview", "", ago(3*time.Minute), tp(ago(time.Minute))),
		}, "waiting", "Asked at 11:55"},
		{"fetch: a separate preview after its collect does not end it", console.RunFetch, req, []store.Run{
			runAt("collect", "linkedin", ago(4*time.Minute), tp(ago(3*time.Minute))),
			runAt("preview", "", ago(2*time.Minute), tp(ago(time.Minute))),
		}, "running", "scoring, then the results"},
		{"fetch: the fetch's preview does not end a Show results now", console.RunPreview, req, []store.Run{
			runAt("preview", "fetch", ago(3*time.Minute), tp(ago(time.Minute))),
		}, "waiting", "Asked at 11:55"},
		{"preview waits for a running collect", console.RunPreview, req, []store.Run{
			runAt("collect", "linkedin", ago(10*time.Minute), nil),
		}, "waiting", "waiting for LinkedIn (a collect is running since 11:50)"},
		{"fetch waits behind a joined collect", console.RunFetch, req, []store.Run{
			runAt("collect", "linkedin-fixture", ago(10*time.Minute), nil),
		}, "waiting", "waiting for LinkedIn (a collect is running since 11:50)"},
		{"preview waits for the brief", console.RunPreview, req, []store.Run{
			runAt("brief", "", ago(7*time.Minute), nil),
		}, "waiting", "the brief is running since 11:53"},
		{"ats stuck names no LinkedIn", console.RunATS, tp(ago(atsWait + runNowStuckGrace + time.Minute)), nil, "stuck", "The watcher may not be running"},
		{"no request, the nightly ATS runs", console.RunATS, nil, []store.Run{
			runAt("collect", "greenhouse", ago(time.Hour), nil),
		}, "running", "Running since 11:00"},
		{"no request, a timed brief done", console.RunBrief, nil, []store.Run{
			runAt("brief", "", ago(4*time.Hour), tp(ago(4*time.Hour-time.Minute))),
		}, "done", "Last done at 08:01"},
		// The fetch's preview never started after its joined collect ended: stuck, not
		// running forever.
		{"fetch: no preview after its joined collect ended", console.RunFetch, tp(ago(2 * time.Hour)), []store.Run{
			runAt("collect", "linkedin", ago(110*time.Minute), tp(ago(previewWait+runNowStuckGrace+time.Minute))),
		}, "stuck", "or the watcher is not running: see deploy/README.md, Run now (systemctl reset-failed leadhound-fetch.path leadhound-fetch.service, then systemctl start leadhound-fetch.path)"},
		{"fetch: a timed collect after a dead watcher does not mask stuck", console.RunFetch, tp(ago(4 * time.Hour)), []store.Run{
			runAt("collect", "linkedin", ago(3*time.Hour), tp(ago(2*time.Hour))),
		}, "stuck", "Asked at 08:00 and not started"},
		{"fetch: within the preview's wait after its collect", console.RunFetch, tp(ago(2 * time.Hour)), []store.Run{
			runAt("collect", "linkedin", ago(110*time.Minute), tp(ago(previewWait+runNowStuckGrace-time.Minute))),
		}, "running", "scoring, then the results"},
		// The ATS score never started within its lock wait after the last collect.
		{"ats: no score within the score lock wait", console.RunATS, tp(ago(4 * time.Hour)), []store.Run{
			runAt("collect", "greenhouse", ago(3*time.Hour), tp(ago(atsScoreWait+runNowStuckGrace+time.Minute))),
		}, "done", "The last run did not finish."},
		{"ats: within the score lock wait", console.RunATS, tp(ago(4 * time.Hour)), []store.Run{
			runAt("collect", "greenhouse", ago(3*time.Hour), tp(ago(atsScoreWait+runNowStuckGrace-time.Minute))),
		}, "running", "waiting to score"},
		// A run closed by its deferred "did not finish" note finished badly.
		{"preview: a run noted did not finish", console.RunPreview, req, []store.Run{
			{Command: "preview", StartedAt: ago(3 * time.Minute), FinishedAt: tp(ago(time.Minute)), Note: runNoteDidNotFinish},
		}, "done", "The last run did not finish."},
		{"fetch: a preview noted did not finish", console.RunFetch, req, []store.Run{
			{Command: "preview", Source: "fetch", StartedAt: ago(3 * time.Minute), FinishedAt: tp(ago(time.Minute)), Note: runNoteDidNotFinish},
		}, "done", "The last run did not finish."},
		{"brief: a run noted did not finish", console.RunBrief, req, []store.Run{
			{Command: "brief", StartedAt: ago(3 * time.Minute), FinishedAt: tp(ago(time.Minute)), Note: runNoteDidNotFinish},
		}, "done", "The last run did not finish."},
		// Another unit's score held the lock and ended first; the ATS's own still runs.
		{"ats: another unit's score ends while the ATS score runs", console.RunATS, req, []store.Run{
			runAt("collect", "greenhouse", ago(4*time.Minute), tp(ago(3*time.Minute))),
			runAt("score", "", ago(3*time.Minute), tp(ago(2*time.Minute))),
			runAt("score", "", ago(2*time.Minute), nil),
		}, "running", "scoring"},
		{"ats: done once every later score finished", console.RunATS, req, []store.Run{
			runAt("collect", "greenhouse", ago(4*time.Minute), tp(ago(3*time.Minute))),
			runAt("score", "", ago(3*time.Minute), tp(ago(2*time.Minute))),
			runAt("score", "", ago(2*time.Minute), tp(ago(time.Minute))),
		}, "done", "Last done at 11:59"},
	}
	for _, c := range extra {
		t.Run(c.name, func(t *testing.T) {
			got := runNowState(c.action, c.requested, c.runs, now, runNowLimits[c.action])
			if got.State != c.state || !strings.Contains(got.Line, c.line) {
				t.Fatalf("got %+v, want state %s with %q", got, c.state, c.line)
			}
		})
	}
}

// TestRunNowDecision: each refusal rule, with a fake clock.
func TestRunNowDecision(t *testing.T) {
	loc := time.FixedZone("test", 0)
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, loc)
	ago := func(d time.Duration) *time.Time { return tp(now.Add(-d)) }
	idle := console.RunNowAction{State: "idle"}
	cases := []struct {
		name   string
		req    console.RunRequest
		st     console.RunNowAction
		f      runNowFacts
		refuse bool
		words  string
	}{
		{"unknown action", console.RunRequest{Action: "rm"}, idle, runNowFacts{}, true, `"rm" is not a Run now action`},
		{"press while waiting", console.RunRequest{Action: console.RunPreview}, console.RunNowAction{State: "waiting", Since: ago(3 * time.Minute)}, runNowFacts{}, true, "Already asked at 11:57"},
		{"press while running", console.RunRequest{Action: console.RunBrief}, console.RunNowAction{State: "running", Since: ago(3 * time.Minute)}, runNowFacts{}, true, "Already running since 11:57"},
		{"ats while the nightly runs", console.RunRequest{Action: console.RunATS, Confirmed: true}, console.RunNowAction{State: "running", Since: ago(time.Hour)}, runNowFacts{}, true, "The ATS run is already running since 11:00"},
		{"press when stuck goes ahead", console.RunRequest{Action: console.RunPreview}, console.RunNowAction{State: "stuck", Since: ago(time.Hour)}, runNowFacts{}, false, "Asked. Results usually"},
		{"brief already sent", console.RunRequest{Action: console.RunBrief}, idle, runNowFacts{briefSentAt: ago(4 * time.Hour)}, true, "Today's email already went at 08:00"},
		{"brief unsent", console.RunRequest{Action: console.RunBrief}, idle, runNowFacts{}, false, "Today's email is on its way"},
		{"ats unconfirmed", console.RunRequest{Action: console.RunATS}, idle, runNowFacts{}, true, "Tick the box to confirm"},
		{"ats confirmed", console.RunRequest{Action: console.RunATS, Confirmed: true}, idle, runNowFacts{}, false, "it takes hours"},
		{"fetch 59 min after a collect", console.RunRequest{Action: console.RunFetch}, idle, runNowFacts{collectEnded: ago(59 * time.Minute)}, true, "LinkedIn was read at 11:01; fetch again after 12:01"},
		{"fetch 61 min after a collect", console.RunRequest{Action: console.RunFetch}, idle, runNowFacts{collectEnded: ago(61 * time.Minute)}, false, "Fetching from LinkedIn"},
		{"fetch while a collect runs", console.RunRequest{Action: console.RunFetch}, idle, runNowFacts{collectEnded: ago(5 * time.Minute), collectRunning: ago(2 * time.Minute)}, false, "Joining the collect running since 11:58"},
		{"fetch paused today", console.RunRequest{Action: console.RunFetch}, idle, runNowFacts{paused: true, pausedReason: "throttled", pausedAt: ago(2 * time.Hour)}, true, "LinkedIn is paused today (throttled at 10:00); a fetch would read nothing"},
		{"preview paused today goes ahead", console.RunRequest{Action: console.RunPreview}, idle, runNowFacts{paused: true, pausedReason: "blocked"}, false, "LinkedIn is paused today (blocked), so nothing is rechecked"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			msg, err := runNowDecision(c.req, c.st, c.f, now)
			if c.refuse {
				if !errors.Is(err, console.ErrUserFixable) || !strings.Contains(err.Error(), c.words) {
					t.Fatalf("err = %v, want a refusal with %q", err, c.words)
				}
				return
			}
			if err != nil || !strings.Contains(msg, c.words) {
				t.Fatalf("msg %q, err %v; want %q", msg, err, c.words)
			}
		})
	}
}

// TestCollectFacts reads the collect and pause times out of the runs.
func TestCollectFacts(t *testing.T) {
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	f := collectFacts([]store.Run{
		runAt("collect", "linkedin", now.Add(-3*time.Hour), tp(now.Add(-2*time.Hour))),
		runAt("collect", "greenhouse", now.Add(-time.Hour), tp(now.Add(-30*time.Minute))),
		{Command: "recheck", Source: "linkedin", StartedAt: now.Add(-20 * time.Minute), FinishedAt: tp(now.Add(-19 * time.Minute)), StopReason: "throttled"},
		runAt("collect", "linkedin", now.Add(-10*time.Minute), nil),
		runAt("collect", "linkedin", now.Add(-10*time.Hour), nil), // killed long ago
	}, now)
	if f.collectEnded == nil || !f.collectEnded.Equal(now.Add(-2*time.Hour)) {
		t.Errorf("collectEnded = %v", f.collectEnded)
	}
	if f.collectRunning == nil || !f.collectRunning.Equal(now.Add(-10*time.Minute)) {
		t.Errorf("collectRunning = %v", f.collectRunning)
	}
	if f.pausedAt == nil || !f.pausedAt.Equal(now.Add(-19*time.Minute)) {
		t.Errorf("pausedAt = %v", f.pausedAt)
	}
}

// TestRunRequestFile: the request is written under <data>/run, mode 0600, holding the time,
// with the folder made on a cold start; text that is not a time falls back to the mtime.
func TestRunRequestFile(t *testing.T) {
	data := t.TempDir()
	if at, err := readRunRequest(data, console.RunPreview); err != nil || at != nil {
		t.Fatalf("cold start read = %v, %v", at, err)
	}
	at := time.Date(2026, 10, 26, 9, 30, 15, 123456789, time.UTC)
	if err := writeRunRequest(data, console.RunPreview, at); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(data, "run", "preview")
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", st.Mode().Perm())
	}
	dst, err := os.Stat(filepath.Join(data, "run"))
	if err != nil || dst.Mode().Perm() != 0o700 {
		t.Errorf("run dir = %v, %v; want 0700", dst, err)
	}
	b, _ := os.ReadFile(path) //nolint:gosec // G304: the test reads the file it just had written
	if strings.TrimSpace(string(b)) != "2026-10-26T09:30:15.123456789Z" {
		t.Errorf("content = %q", b)
	}
	got, err := readRunRequest(data, console.RunPreview)
	if err != nil || got == nil || !got.Equal(at) {
		t.Fatalf("read = %v, %v", got, err)
	}
	if err := os.WriteFile(path, []byte("2026-10-2"), 0o600); err != nil {
		t.Fatal(err)
	}
	mt := time.Date(2026, 10, 26, 10, 0, 0, 0, time.UTC)
	if err := os.Chtimes(path, mt, mt); err != nil {
		t.Fatal(err)
	}
	if got, err := readRunRequest(data, console.RunPreview); err != nil || got == nil || !got.Equal(mt) {
		t.Fatalf("torn read = %v, %v; want the mtime", got, err)
	}
	if err := writeRunRequest("", console.RunPreview, at); err == nil {
		t.Error("no data dir: want an error")
	}
}

// TestResultsRead: a missing and a corrupt preview.json are console.ErrNotFound; a good one
// maps to the results.
func TestResultsRead(t *testing.T) {
	data := t.TempDir()
	b := &backend{loc: time.UTC}
	b.cfg.DataDir = data
	if _, err := b.Results(context.Background()); !errors.Is(err, console.ErrNotFound) {
		t.Fatalf("missing: err = %v", err)
	}
	if err := os.WriteFile(filepath.Join(data, preview.FileName), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Results(context.Background()); !errors.Is(err, console.ErrNotFound) {
		t.Fatalf("corrupt: err = %v", err)
	}
	made := time.Date(2026, 10, 26, 9, 0, 0, 0, time.UTC)
	if err := preview.Write(data, preview.Preview{Version: preview.Version, MadeAt: made, Day: "2026-10-26",
		RecheckOff: true, Health: []string{"Health: recheck off (not live)"}, Unreachable: 2, Unscored: 3}); err != nil {
		t.Fatal(err)
	}
	r, err := b.Results(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !r.MadeAt.Equal(made) || r.Day.Format(time.DateOnly) != "2026-10-26" || !r.RecheckOff ||
		len(r.Health) != 1 || r.Unreachable != 2 || r.Unscored != 3 || len(r.Apply) != 0 || r.RunnerUp != nil {
		t.Fatalf("results = %+v", r)
	}
}

// TestResultJob maps one preview job, with and without a live summary.
func TestResultJob(t *testing.T) {
	n := 12
	j := preview.Job{ID: 7, Title: "Go engineer", Company: "Acme", URL: "https://example.invalid/7", ApplicantCount: &n,
		Criteria: []preview.Criterion{{Name: "remote", Verdict: "pass", Note: "remote"}}}
	r := resultJob(j, map[int64]store.JobSummary{7: {JobID: 7, Status: "shortlisted", ListingStatus: "listed"}})
	if r.JobID != 7 || r.Status != "shortlisted" || r.ListingStatus != "listed" || *r.ApplicantCount != 12 ||
		len(r.Criteria) != 1 || r.Criteria[0].Keywords != "remote" {
		t.Fatalf("result = %+v", r)
	}
	if r := resultJob(j, nil); r.Status != "" || r.Title != "Go engineer" {
		t.Fatalf("gone job = %+v", r)
	}
}

// TestHHMMNamesTheDay: a time from an earlier day says which, so a run from last night
// never reads as one from today.
func TestHHMMNamesTheDay(t *testing.T) {
	loc := time.FixedZone("test", 2*3600)
	now := time.Date(2026, 10, 26, 5, 30, 0, 0, loc)
	for _, c := range []struct {
		at   time.Time
		want string
	}{
		{time.Date(2026, 10, 26, 5, 2, 0, 0, loc), "05:02"},
		{time.Date(2026, 10, 25, 21, 30, 0, 0, loc), "yesterday 21:30"},
		{time.Date(2026, 10, 24, 11, 8, 0, 0, loc), "Sat 24 Oct 11:08"},
		{time.Date(2026, 10, 25, 22, 30, 0, 0, time.UTC), "00:30"}, // 22:30 UTC is already today here
	} {
		if got := hhmm(c.at, now); got != c.want {
			t.Errorf("hhmm(%v) = %q, want %q", c.at, got, c.want)
		}
	}
}
