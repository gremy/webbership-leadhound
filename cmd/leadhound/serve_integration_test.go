//go:build integration

package main

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/brief"
	"github.com/gremy/webbership-leadhound/internal/config"
	"github.com/gremy/webbership-leadhound/internal/console"
	"github.com/gremy/webbership-leadhound/internal/leadconfig"
	"github.com/gremy/webbership-leadhound/internal/leadstore/leadstoretest"
	"github.com/gremy/webbership-leadhound/internal/secretbox"
	"github.com/gremy/webbership-leadhound/internal/store"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

// testBackend is the console backend over the test database, as serve builds it, with the
// real build behind a request that waits for it, so a dashboard read shows the full data.
// Its builds end with the test.
func testBackend(t *testing.T) *backend {
	t.Helper()
	ctx := context.Background()
	s, err := store.Open(ctx, storetest.AppDSN())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	prof, err := loadProfileDir(examplesDir)
	if err != nil {
		t.Fatal(err)
	}
	loc, err := time.LoadLocation(config.DefaultTZ)
	if err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.Load(func(string) string { return "" })
	b := newBackend(t.Context(), s, prof, loc, cfg)
	b.waitFor = time.Minute
	t.Cleanup(b.wait)
	return b
}

// trackRows is what one status change wrote: the job's latest status event and every rule.
type trackRows struct {
	status, reason, actor string
	hasRule               bool
	rules                 []string // kind field pattern reason from-job, one per rule
}

func readTrackRows(t *testing.T, jobID int64) trackRows {
	t.Helper()
	ctx := context.Background()
	conn := storetest.OwnerConn(t)
	var r trackRows
	err := conn.QueryRow(ctx,
		`SELECT status, reason, actor, rule_id IS NOT NULL FROM status_event WHERE job_id = $1 ORDER BY at DESC, id DESC LIMIT 1`, jobID).
		Scan(&r.status, &r.reason, &r.actor, &r.hasRule)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := conn.Query(ctx, `SELECT kind, field, pattern, reason, coalesce(created_from_job_id, 0) FROM rule ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var kind, field, pattern, reason string
		var from int64
		if err := rows.Scan(&kind, &field, &pattern, &reason, &from); err != nil {
			t.Fatal(err)
		}
		r.rules = append(r.rules, fmt.Sprintf("%s %s %s %q %d", kind, field, pattern, reason, from))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return r
}

// lastLine is the last non-empty line of s.
func lastLine(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	return lines[len(lines)-1]
}

// cliRefusal is the words of a track usage error on stderr, without the command's prefix.
func cliRefusal(stderr string) string {
	return strings.TrimSuffix(strings.TrimPrefix(stderr, "leadhound track: usage: "), "\n")
}

// consoleRefusal is the words of an ErrUserFixable error, without its prefix.
func consoleRefusal(t *testing.T, err error) string {
	t.Helper()
	if !errors.Is(err, console.ErrUserFixable) {
		t.Fatalf("err = %v, want ErrUserFixable", err)
	}
	return strings.TrimPrefix(err.Error(), console.ErrUserFixable.Error()+": ")
}

// TestTrackContract: the console's Track and the CLI's track, each on a fresh database with
// the same fixtures, write the same rows and say the same words.
func TestTrackContract(t *testing.T) {
	cases := []struct {
		name string
		cli  []string
		req  console.TrackRequest
	}{
		{"applied", []string{"track", "1", "applied"}, console.TrackRequest{Ref: "1", Action: "applied"}},
		{"skip reason", []string{"track", "2", "skip", "--reason", "French only"}, console.TrackRequest{Ref: "2", Action: "skip", Reason: "French only"}},
		{"skip rule", []string{"track", "3", "skip", "--reason", "agency", "--rule", "company=^tailspin"},
			console.TrackRequest{Ref: "3", Action: "skip", Reason: "agency", RuleField: "company", RulePattern: "^tailspin"}},
		{"skip rule no reason", []string{"track", "3", "skip", "--rule", "title=platform"},
			console.TrackRequest{Ref: "3", Action: "skip", RuleField: "title", RulePattern: "platform"}},
		{"maybe reason", []string{"track", "4", "maybe", "--reason", "unsure about the stack"},
			console.TrackRequest{Ref: "4", Action: "maybe", Reason: "unsure about the stack"}},
		{"job ref", nil, console.TrackRequest{Action: "interviewing"}}, // job:ID filled in below
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ids := sentBrief(t)
			cli, req := c.cli, c.req
			n, _ := strconv.Atoi(req.Ref) // the brief number; 0 for the job:ID case
			if cli == nil {
				ref := fmt.Sprintf("job:%d", ids[1])
				cli, req.Ref, n = []string{"track", ref, "interviewing"}, ref, 2
			}
			code, out, stderr := runCmdErr(t, cli...)
			if code != 0 {
				t.Fatalf("cli: exit %d, %q", code, stderr)
			}
			want := readTrackRows(t, ids[n-1])

			ids2 := sentBrief(t)
			if !slices.Equal(ids, ids2) {
				t.Fatalf("fixtures differ: %v vs %v", ids, ids2)
			}
			b := testBackend(t)
			got, err := b.Track(context.Background(), req)
			if err != nil {
				t.Fatalf("console: %v", err)
			}
			if rows := readTrackRows(t, ids[n-1]); !equalRows(rows, want) {
				t.Errorf("console rows = %+v, cli rows = %+v", rows, want)
			}
			if got.Message != lastLine(out) {
				t.Errorf("console message = %q, cli line = %q", got.Message, lastLine(out))
			}
			if got.Job.JobID != ids[n-1] || got.Job.Status != want.status {
				t.Errorf("outcome job = %+v, want job %d status %s", got.Job, ids[n-1], want.status)
			}
		})
	}
}

func equalRows(a, b trackRows) bool {
	return a.status == b.status && a.reason == b.reason && a.actor == b.actor && a.hasRule == b.hasRule && slices.Equal(a.rules, b.rules)
}

// TestTrackContractRefusals: what the CLI refuses the console refuses too, with the same
// words, and neither writes anything.
func TestTrackContractRefusals(t *testing.T) {
	cases := []struct {
		name  string
		setup []string // a CLI command run before, on both databases
		cli   []string
		req   console.TrackRequest
	}{
		{"skip after applied", []string{"track", "1", "applied"},
			[]string{"track", "1", "skip", "--reason", "changed my mind"}, console.TrackRequest{Ref: "1", Action: "skip", Reason: "changed my mind"}},
		{"unknown number", nil, []string{"track", "7", "applied"}, console.TrackRequest{Ref: "7", Action: "applied"}},
		{"unknown job", nil, []string{"track", "job:999", "applied"}, console.TrackRequest{Ref: "job:999", Action: "applied"}},
		{"bad job ref", nil, []string{"track", "job:abc", "applied"}, console.TrackRequest{Ref: "job:abc", Action: "applied"}},
		{"rule without skip", nil, []string{"track", "1", "applied", "--rule", "title=x"},
			console.TrackRequest{Ref: "1", Action: "applied", RuleField: "title", RulePattern: "x"}},
		{"rule bad field", nil, []string{"track", "1", "skip", "--rule", "salary=x"},
			console.TrackRequest{Ref: "1", Action: "skip", RuleField: "salary", RulePattern: "x"}},
		{"rule no pattern", nil, []string{"track", "1", "skip", "--rule", "title= "},
			console.TrackRequest{Ref: "1", Action: "skip", RuleField: "title", RulePattern: " "}},
		{"rule does not compile", nil, []string{"track", "1", "skip", "--rule", "title=(unclosed"},
			console.TrackRequest{Ref: "1", Action: "skip", RuleField: "title", RulePattern: "(unclosed"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sentBrief(t)
			if c.setup != nil {
				if code, _, stderr := runCmdErr(t, c.setup...); code != 0 {
					t.Fatalf("setup: exit %d, %q", code, stderr)
				}
			}
			before := events(t)
			code, _, stderr := runCmdErr(t, c.cli...)
			if code != 2 {
				t.Fatalf("cli: exit %d, %q", code, stderr)
			}
			want := cliRefusal(stderr)

			sentBrief(t)
			if c.setup != nil {
				if code, _, stderr := runCmdErr(t, c.setup...); code != 0 {
					t.Fatalf("setup: exit %d, %q", code, stderr)
				}
			}
			_, err := testBackend(t).Track(context.Background(), c.req)
			if got := consoleRefusal(t, err); got != want {
				t.Errorf("console says %q, cli says %q", got, want)
			}
			if events(t) != before || dbCount(t, `SELECT count(*) FROM rule`) != 0 {
				t.Error("a refusal wrote something")
			}
		})
	}
}

// TestTrackNoBriefYet: with no brief sent, a number is refused with the CLI's words, and
// an unknown action is refused too.
func TestTrackNoBriefYet(t *testing.T) {
	commandDB(t)
	code, _, stderr := runCmdErr(t, "track", "1", "applied")
	if code != 2 {
		t.Fatalf("cli: exit %d", code)
	}
	b := testBackend(t)
	_, err := b.Track(context.Background(), console.TrackRequest{Ref: "1", Action: "applied"})
	if got, want := consoleRefusal(t, err), cliRefusal(stderr); got != want {
		t.Errorf("console says %q, cli says %q", got, want)
	}
	_, err = b.Track(context.Background(), console.TrackRequest{Ref: "job:1", Action: "married"})
	if got := consoleRefusal(t, err); !strings.Contains(got, "applied, interviewing") {
		t.Errorf("unknown action: %q", got)
	}
}

// TestTrackDoubleSubmit: the second of two identical marks is a no-op in the CLI's words,
// as two tabs marking the same job would see.
func TestTrackDoubleSubmit(t *testing.T) {
	sentBrief(t)
	b := testBackend(t)
	req := console.TrackRequest{Ref: "1", Action: "applied"}
	if _, err := b.Track(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	before := events(t)
	got, err := b.Track(context.Background(), req)
	if err != nil || !got.NoOp || got.Message != "already applied; nothing changed" {
		t.Errorf("second mark = %+v, %v", got, err)
	}
	if events(t) != before {
		t.Error("the no-op wrote an event")
	}
}

// TestDashboardHealthParity: the dashboard's health lines are the brief's, built by the
// same code for the same day, and the brief written to the outbox carries every one.
func TestDashboardHealthParity(t *testing.T) {
	scoredFixtures(t)
	ob := filepath.Join(t.TempDir(), "outbox")
	if code, _, stderr := runCmdErr(t, "brief", "--outbox", ob); code != 0 {
		t.Fatalf("brief: exit %d, %q", code, stderr)
	}
	b := testBackend(t)
	ctx := context.Background()
	d, err := b.Dashboard(ctx)
	if err != nil {
		t.Fatal(err)
	}
	in, err := buildInput(ctx, b.s.Q(), b.prof, b.loc, time.Now().In(b.loc), false, true)
	if err != nil {
		t.Fatal(err)
	}
	want := brief.HealthLines(in)
	if !slices.Equal(d.Health, want) || len(want) == 0 {
		t.Errorf("dashboard health = %q, brief health = %q", d.Health, want)
	}
	text := outboxText(t, ob)
	for _, l := range want {
		if !strings.Contains(text, l+"\n") {
			t.Errorf("the brief lacks %q", l)
		}
	}
	if d.ApplyNow != len(in.Apply) || d.YourCallNow != len(in.YourCall) || d.Unscored != in.Unscored || d.Unreachable != in.Unreachable {
		t.Errorf("dashboard counts = %d %d %d %d, input = %d %d %d %d", d.ApplyNow, d.YourCallNow, d.Unscored, d.Unreachable,
			len(in.Apply), len(in.YourCall), in.Unscored, in.Unreachable)
	}
	if d.LastBrief != nil {
		t.Errorf("last brief = %+v, want none (nothing sent)", d.LastBrief)
	}
	if len(d.LastRuns) == 0 || d.Funnel.NewBySource["linkedin-fixture"] == 0 || d.Funnel.Scored == 0 || d.CallCap != config.DefaultJevDailyCallCap {
		t.Errorf("dashboard = %+v", d)
	}
}

// TestDashboardLastBrief: after brief --send, the dashboard shows the sent brief's own
// stored health lines, its day and how many it listed.
func TestDashboardLastBrief(t *testing.T) {
	ids := sentBrief(t)
	var body string
	if err := storetest.OwnerConn(t).QueryRow(context.Background(), `SELECT body_text FROM brief_log`).Scan(&body); err != nil {
		t.Fatal(err)
	}
	var want []string
	for l := range strings.Lines(body) {
		if l = strings.TrimSuffix(l, "\n"); strings.HasPrefix(l, "Health: ") {
			want = append(want, l)
		}
	}
	b := testBackend(t)
	d, err := b.Dashboard(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	lb := d.LastBrief
	if lb == nil {
		t.Fatal("no last brief")
	}
	if !slices.Equal(lb.Health, want) || len(want) == 0 {
		t.Errorf("last brief health = %q, stored = %q", lb.Health, want)
	}
	if lb.Listed != len(ids) || lb.Day.Format(time.DateOnly) != today() || lb.Day.Location() != b.loc || lb.SentAt.IsZero() {
		t.Errorf("last brief = %+v", lb)
	}

	jobs, err := b.BriefJobs(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs.Jobs) != len(ids) || jobs.SentAt == nil || jobs.Day.Format(time.DateOnly) != today() {
		t.Fatalf("brief jobs = %+v", jobs)
	}
	for i, j := range jobs.Jobs {
		if j.N != i+1 || j.JobID != ids[i] || j.Status != "shortlisted" || j.Title == "" || j.Company == "" {
			t.Errorf("job %d = %+v", i+1, j)
		}
	}
}

// TestDashboardCache: the dashboard serves its snapshot, built once, until it is
// dashboardFreshFor old; then it serves the old one as updating while the new one builds.
// A mark starts a rebuild at once, and until it lands the dashboard shows the old numbers
// as updating.
func TestDashboardCache(t *testing.T) {
	sentBrief(t)
	b := testBackend(t)
	var mu sync.Mutex
	// Noon of today: the clock moves on by minutes and must not cross into the next day,
	// which would rightly start a rebuild of its own.
	y, m, d := time.Now().In(b.loc).Date()
	now, builds := time.Date(y, m, d, 12, 0, 0, 0, b.loc), 0
	var hold chan struct{} // while non-nil, a build waits for it to close
	b.now = func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	at := func(d time.Duration) time.Time { mu.Lock(); defer mu.Unlock(); now = now.Add(d); return now }
	realBuild := b.build
	b.build = func(ctx context.Context, when time.Time) (snapshot, error) {
		mu.Lock()
		builds++
		h := hold
		mu.Unlock()
		if h != nil {
			<-h
		}
		return realBuild(ctx, when)
	}
	nBuilds := func() int { mu.Lock(); defer mu.Unlock(); return builds }
	ctx := context.Background()
	first, err := b.Dashboard(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !first.AsOf.Equal(b.now()) || first.Updating || first.Building || len(first.Health) == 0 || nBuilds() != 1 {
		t.Fatalf("first = %+v, builds %d", first, nBuilds())
	}
	at(dashboardFreshFor - time.Second)
	if second, _ := b.Dashboard(ctx); !second.AsOf.Equal(first.AsOf) || nBuilds() != 1 {
		t.Errorf("within %v: as of %v, builds %d", dashboardFreshFor, second.AsOf, nBuilds())
	}
	later := at(time.Second)
	if third, _ := b.Dashboard(ctx); !third.AsOf.Equal(first.AsOf) || !third.Updating {
		t.Errorf("after %v: as of %v, updating %v, builds %d; want the old one", dashboardFreshFor, third.AsOf, third.Updating, nBuilds())
	}
	b.wait()
	if fourth, _ := b.Dashboard(ctx); !fourth.AsOf.Equal(later) || fourth.Updating || nBuilds() != 2 {
		t.Errorf("after the build: as of %v, updating %v, builds %d; want %v", fourth.AsOf, fourth.Updating, nBuilds(), later)
	}

	mu.Lock()
	hold = make(chan struct{})
	mu.Unlock()
	marked := at(time.Second)
	out, err := b.Track(ctx, console.TrackRequest{Ref: "1", Action: "applied"})
	if err != nil || out.To != "applied" {
		t.Fatalf("mark = %+v, %v", out, err)
	}
	during, _ := b.Dashboard(ctx)
	if !during.Updating || !during.AsOf.Equal(later) || during.ApplyNow != first.ApplyNow || nBuilds() != 3 {
		t.Errorf("while the rebuild runs: updating %v, as of %v, builds %d; want the old numbers updating", during.Updating, during.AsOf, nBuilds())
	}
	mu.Lock()
	close(hold)
	hold = nil
	mu.Unlock()
	b.wait()
	after, _ := b.Dashboard(ctx)
	if after.Updating || !after.AsOf.Equal(marked) || nBuilds() != 3 {
		t.Errorf("after the rebuild: updating %v, as of %v, builds %d; want %v", after.Updating, after.AsOf, nBuilds(), marked)
	}
}

// TestConsoleColdStart: on an empty database every read answers without an error, the
// health lines are what the brief says for that empty day, and no brief is listed.
func TestConsoleColdStart(t *testing.T) {
	commandDB(t)
	t.Setenv("LEADHOUND_DATA_DIR", examplesDir)
	b := testBackend(t)
	ctx := context.Background()
	d, err := b.Dashboard(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ob := filepath.Join(t.TempDir(), "outbox")
	if code, _, stderr := runCmdErr(t, "brief", "--outbox", ob); code != 0 {
		t.Fatalf("brief: exit %d, %q", code, stderr)
	}
	text := outboxText(t, ob)
	if len(d.Health) == 0 {
		t.Error("no health lines on an empty day")
	}
	for _, l := range d.Health {
		if !strings.Contains(text, l+"\n") {
			t.Errorf("the empty day's brief lacks %q", l)
		}
	}
	if d.LastBrief != nil || d.ApplyNow != 0 || d.AICalls != 0 || len(d.Boards) != 0 || d.Funnel.Scored != 0 {
		t.Errorf("cold dashboard = %+v", d)
	}
	jobs, err := b.BriefJobs(ctx, "")
	if err != nil || len(jobs.Jobs) != 0 || jobs.SentAt != nil {
		t.Errorf("cold brief jobs = %+v, %v", jobs, err)
	}
	if tr, err := b.Tracked(ctx, ""); err != nil || len(tr.Rows) != 0 || tr.Total != 0 {
		t.Errorf("cold tracked = %v, %v", tr, err)
	}
	if rs, err := b.Rules(ctx); err != nil || len(rs) != 0 {
		t.Errorf("cold rules = %v, %v", rs, err)
	}
	if _, err := b.Tracked(ctx, "dreaming"); !errors.Is(err, console.ErrUserFixable) {
		t.Errorf("bad status filter: %v", err)
	}
}

// TestConsoleListsReadTheStore: Tracked, Runs and Rules show what the CLI wrote.
func TestConsoleListsReadTheStore(t *testing.T) {
	ids := sentBrief(t)
	if code, _, stderr := runCmdErr(t, "track", "3", "skip", "--reason", "agency", "--rule", "company=^tailspin"); code != 0 {
		t.Fatalf("track: exit %d, %q", code, stderr)
	}
	b := testBackend(t)
	ctx := context.Background()
	l, err := b.Tracked(ctx, "skipped")
	tr := l.Rows
	if err != nil || len(tr) != 1 || l.Total != 1 || tr[0].JobID != ids[2] || tr[0].Reason != "agency" || tr[0].Since.IsZero() {
		t.Errorf("tracked skipped = %+v, %v", l, err)
	}
	all, err := b.Tracked(ctx, "all")
	if want := dbCount(t, `SELECT count(DISTINCT job_id) FROM status_event`); err != nil || len(all.Rows) != want || all.Total != want {
		t.Errorf("tracked all = %d of %d, %v; want %d", len(all.Rows), all.Total, err, want)
	}
	rs, err := b.Rules(ctx)
	if err != nil || len(rs) != 1 || rs[0].Field != "company" || rs[0].FromJobID == nil || *rs[0].FromJobID != ids[2] {
		t.Errorf("rules = %+v, %v", rs, err)
	}
	runs, err := b.Runs(ctx, 2)
	if err != nil || len(runs) != 2 || runs[0].Command != "brief" {
		t.Errorf("runs = %+v, %v", runs, err)
	}
}

// outboxText is the one brief written to dir.
func outboxText(t *testing.T, dir string) string {
	t.Helper()
	es, err := os.ReadDir(dir)
	if err != nil || len(es) != 1 {
		t.Fatalf("outbox: %d files, %v", len(es), err)
	}
	b, err := os.ReadFile(filepath.Join(dir, es[0].Name())) // #nosec G304 -- a test temp dir
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestBriefFooterConsoleURL: a valid console URL puts the console line in the brief; none
// leaves the track commands; an unusable one leaves them too, with one notes line, and the
// brief still exits 0.
func TestBriefFooterConsoleURL(t *testing.T) {
	const cliLine = "Mark a job with leadhound track N applied"
	scoredFixtures(t)
	for _, c := range []struct {
		url, want, note string
	}{
		{"https://console.example", "Mark jobs in the console: https://console.example/jobs", ""},
		{"", cliLine, ""},
		{"http://bad.example", cliLine, "brief: LEADHOUND_CONSOLE_URL is not an https URL; the footer names the track commands"},
	} {
		t.Run(c.url, func(t *testing.T) {
			t.Setenv("LEADHOUND_CONSOLE_URL", c.url)
			nb := captureNotes(t)
			ob := filepath.Join(t.TempDir(), "outbox")
			if code, _, stderr := runCmdErr(t, "brief", "--allow-fake", "--outbox", ob); code != 0 {
				t.Fatalf("brief: exit %d, %q", code, stderr)
			}
			text := outboxText(t, ob)
			if !strings.Contains(text, c.want) {
				t.Errorf("brief lacks %q", c.want)
			}
			if c.want == cliLine && strings.Contains(text, "Mark jobs in the console") {
				t.Error("brief names the console")
			}
			if strings.Contains(text, "bad.example") {
				t.Error("brief names the unusable URL")
			}
			if got := nb.String(); c.note != "" && strings.Count(got, c.note) != 1 || c.note == "" && strings.Contains(got, "CONSOLE_URL") {
				t.Errorf("notes = %q, want %q once", got, c.note)
			}
		})
	}
}

// TestBadConsoleSettingsRunTimers: an unusable console address or URL never stops the
// timer commands, which run to exit 0 against a real database.
func TestBadConsoleSettingsRunTimers(t *testing.T) {
	commandDB(t)
	t.Setenv("LEADHOUND_CONSOLE_ADDR", "192.0.2.10:8090")
	t.Setenv("LEADHOUND_CONSOLE_URL", "nonsense")
	captureNotes(t)
	t.Setenv("LEADHOUND_DATA_DIR", "")
	if code, _, stderr := runCmdErr(t, "collect", "linkedin", "--dry-run", "--fixtures", linkedinTestdata); code != 0 {
		t.Errorf("collect: exit %d, %q", code, stderr)
	}
	t.Setenv("LEADHOUND_DATA_DIR", demoDataDir(t))
	for _, args := range [][]string{
		{"collect", "ats", "--dry-run", "--fixtures", atsTestdata},
		{"score", "--decider", "fake"},
		{"brief", "--outbox", filepath.Join(t.TempDir(), "o")},
	} {
		if code, _, stderr := runCmdErr(t, args...); code != 0 {
			t.Errorf("%v: exit %d, %q", args, code, stderr)
		}
	}
}

// freeAddr is a loopback address with a port nothing listens on right now.
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}

// TestServeRunsAndStops: serve answers HTTP on its address and, when its context ends (as
// on SIGTERM), shuts down and returns nil, so the command exits 0.
func TestServeRunsAndStops(t *testing.T) {
	commandDB(t)
	addr := freeAddr(t)
	t.Setenv("LEADHOUND_DATA_DIR", examplesDir)
	t.Setenv("LEADHOUND_CONSOLE_ADDR", addr)
	t.Setenv("LEADHOUND_CONSOLE_EMAIL", "operator@mail.example")
	t.Setenv("LEADHOUND_CONSOLE_PASSWORD_HASH", testHash(t))
	// deploy/env.example carries an empty LEADHOUND_MAILBOX_KEY_FILE= line: an env copied
	// from it starts the console.
	t.Setenv(secretbox.KeyFileEnv, "")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runServe(ctx, []string{"--dev"}) }()

	var resp *http.Response
	for range 100 {
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://"+addr+"/robots.txt", nil)
		r, err := http.DefaultClient.Do(req)
		if err == nil {
			resp = r
			break
		}
		select {
		case err := <-done:
			t.Fatalf("serve ended early: %v", err)
		case <-time.After(50 * time.Millisecond):
		}
	}
	if resp == nil {
		cancel()
		t.Fatal("serve never answered")
	}
	_ = resp.Body.Close()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("serve returned %v, want nil", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("serve did not stop")
	}
}

// jobsHandler is the console over b, as serve builds it in dev mode, and the Cookie header
// of a signed-in session.
func jobsHandler(t *testing.T, b *backend) (http.Handler, string) {
	t.Helper()
	a, err := console.NewAuth("operator@mail.example", testHash(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	token, err := a.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	return console.New(b, a, console.Options{Dev: true, Location: b.loc}), "lh_session=" + token
}

// hiddenField is one hidden input of a form, by name.
var hiddenField = regexp.MustCompile(`<input type="hidden" name="([a-z_]+)" value="([^"]*)">`)

// appliedForm is the hidden fields of the Applied form of job n on the Jobs page.
func appliedForm(t *testing.T, page string, n int) url.Values {
	t.Helper()
	start := strings.Index(page, fmt.Sprintf(`id="job-%d"`, n))
	if start < 0 {
		t.Fatalf("no job %d on the page: %s", n, page)
	}
	form := page[start:]
	form = form[strings.Index(form, "<form"):]
	form = form[:strings.Index(form, "</form>")]
	v := url.Values{}
	for _, m := range hiddenField.FindAllStringSubmatch(form, -1) {
		v.Set(m[1], m[2])
	}
	return v
}

// TestJobsFormKeepsItsBrief: a Jobs page loaded before the next brief marks the job its
// number named in the brief it showed, not the job with that number in the new brief,
// and a refusal shows that same job's row.
func TestJobsFormKeepsItsBrief(t *testing.T) {
	ids := sentBrief(t)
	b := testBackend(t)
	h, c := jobsHandler(t, b)
	ctx := context.Background()

	r := httptest.NewRequest(http.MethodGet, "/jobs", nil)
	r.Header.Set("Cookie", c)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	form := appliedForm(t, w.Body.String(), 1)
	if form.Get("ref") != "1" || form.Get("day") != today() || form.Get("action") != "applied" {
		t.Fatalf("form = %v", form)
	}
	first, err := b.BriefJobs(ctx, today())
	if err != nil {
		t.Fatal(err)
	}

	// The next day's brief lists the same jobs the other way round.
	next := []int64{ids[3], ids[2], ids[1], ids[0]}
	if _, err := storetest.OwnerConn(t).Exec(ctx,
		`INSERT INTO brief_log (day, sent_at, subject, body_text, job_ids, send_state)
		 SELECT day + 1, now(), subject, body_text, $1, 'maybe_sent' FROM brief_log`, next); err != nil {
		t.Fatal(err)
	}
	latest, err := b.BriefJobs(ctx, "")
	if err != nil || latest.Jobs[0].JobID != ids[3] {
		t.Fatalf("latest brief = %+v, %v", latest, err)
	}
	// A row is told apart by its link: the fixtures share titles.
	oldLink, newLink := `href="`+first.Jobs[0].URL+`"`, `href="`+latest.Jobs[0].URL+`"`
	if oldLink == newLink {
		t.Fatalf("fixtures: jobs %d and %d share a link", ids[0], ids[3])
	}

	post := func(form url.Values) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/jobs/track", strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("HX-Request", "true")
		r.Header.Set("Cookie", c)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	w = post(form)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), oldLink) || !strings.Contains(w.Body.String(), `name="day" value="`+today()+`"`) {
		t.Errorf("mark: %d %s", w.Code, w.Body.String())
	}
	if st, _, _ := currentStatus(t, ids[0]); st != "applied" {
		t.Errorf("the job the old page showed is %s, want applied", st)
	}
	if st, _, _ := currentStatus(t, ids[3]); st == "applied" {
		t.Error("the job with that number in the new brief was marked")
	}

	form.Set("action", "skip")
	w = post(form)
	body := w.Body.String()
	if w.Code != http.StatusOK || !strings.Contains(body, oldLink) || strings.Contains(body, newLink) || !strings.Contains(body, "row-error") {
		t.Errorf("refusal: %d %s", w.Code, body)
	}
}

// TestTrackDayChecked: a day that is not YYYY-MM-DD is refused before anything is read,
// and a day with no brief is refused in the CLI's words.
func TestTrackDayChecked(t *testing.T) {
	sentBrief(t)
	b := testBackend(t)
	ctx := context.Background()
	before := events(t)
	_, err := b.Track(ctx, console.TrackRequest{Ref: "1", Day: "24/10/2026", Action: "applied"})
	if got := consoleRefusal(t, err); got != `the day must be YYYY-MM-DD, not "24/10/2026"` {
		t.Errorf("bad day: %q", got)
	}
	_, err = b.Track(ctx, console.TrackRequest{Ref: "1", Day: "2020-01-01", Action: "applied"})
	if got := consoleRefusal(t, err); got != "there is no brief for 2020-01-01" {
		t.Errorf("no brief: %q", got)
	}
	if jobs, err := b.BriefJobs(ctx, "2020-01-01"); err != nil || len(jobs.Jobs) != 0 {
		t.Errorf("brief jobs of a day with no brief = %+v, %v", jobs, err)
	}
	if _, err := b.BriefJobs(ctx, "nonsense"); !errors.Is(err, console.ErrUserFixable) {
		t.Errorf("brief jobs of a bad day: %v", err)
	}
	if events(t) != before {
		t.Error("a refusal wrote something")
	}
}

// TestTrackReasonCapped: a reason over 500 characters is refused, counted in characters,
// not bytes, and 500 is accepted and kept whole.
func TestTrackReasonCapped(t *testing.T) {
	ids := sentBrief(t)
	b := testBackend(t)
	ctx := context.Background()
	before := events(t)
	_, err := b.Track(ctx, console.TrackRequest{Ref: "1", Action: "skip", Reason: strings.Repeat("é", 501)})
	if got := consoleRefusal(t, err); got != "the reason is longer than 500 characters" {
		t.Errorf("long reason: %q", got)
	}
	if events(t) != before {
		t.Error("the refusal wrote something")
	}
	if _, err := b.Track(ctx, console.TrackRequest{Ref: "1", Action: "skip", Reason: strings.Repeat("é", 500)}); err != nil {
		t.Fatalf("500 characters: %v", err)
	}
	if _, reason, _ := currentStatus(t, ids[0]); reason != strings.Repeat("é", 500) {
		t.Errorf("reason kept as %d characters", len([]rune(reason)))
	}
}

// leadsBackend is the console backend over a leads scene (leadsScene, then any prep), read
// as serve reads its settings, on the leads clock.
func leadsBackend(t *testing.T) *backend {
	t.Helper()
	cfg := mustConfig(t)
	s, loc, err := openStoreAndZone(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	prof, err := loadProfileDir(examplesDir)
	if err != nil {
		t.Fatal(err)
	}
	b := newBackend(t.Context(), s, prof, loc, cfg)
	b.now = func() time.Time { return leadsNow() }
	b.waitFor = time.Minute
	t.Cleanup(b.wait)
	return b
}

// consoleContract is one write driven through its CLI and through the console backend with
// the same inputs.
type consoleContract struct {
	name    string
	prep    func(t *testing.T, dir string)
	cli     func(t *testing.T) []string
	exit    int
	console func(t *testing.T, b *backend) error
	refused bool // the console refuses it, and the CLI exits 2
	source  bool // a suppression: the console records its own source
}

// TestConsoleLeadsContract: every leads write and rule write the console makes writes what its
// CLI writes from the same start, refusals included (nothing), and nothing else.
func TestConsoleLeadsContract(t *testing.T) {
	ctx := context.Background()
	newShadow := func(t *testing.T, _ string) { mustRun(t, "leads", "batch", "new") }
	approved := func(t *testing.T, dir string) {
		newShadow(t, dir)
		mustRun(t, "leads", "batch", "approve", "1", "--hash", batchHash(t, 1))
	}
	live := func(t *testing.T, dir string) {
		goLive(t)
		setCaps(t, dir, "2026-10-01", "[{from_day: 0, per_day: 10}]")
	}
	formMsg := func(t *testing.T) int64 {
		return int64(batchCount(t, `SELECT min(id)::int FROM outreach_message WHERE channel = 'form'`))
	}
	maybe := func(t *testing.T, dir string) { liveMaybeSent(t, dir, false) }
	maybeMsg := func(t *testing.T) int64 {
		return int64(batchCount(t, `SELECT min(id)::int FROM outreach_message WHERE state = 'maybe_sent'`))
	}
	act := func(a func(t *testing.T) console.LeadAction) func(t *testing.T, b *backend) error {
		return func(t *testing.T, b *backend) error {
			_, err := b.LeadAct(ctx, a(t))
			return err
		}
	}
	fixed := func(a console.LeadAction) func(*testing.T) console.LeadAction {
		return func(*testing.T) console.LeadAction { return a }
	}
	// The mailbox writes: the CLI reads the password from stdin, the console from its form;
	// both seal with the test key, and the rows compare the sealed password by opening it.
	mailboxes := func(t *testing.T, _ string) { allowReservedNames(t); sealWithTestKey(t) }
	mbCLI := func(args ...string) func(t *testing.T) []string {
		return func(t *testing.T) []string {
			saved := mailboxPasswordInput
			t.Cleanup(func() { mailboxPasswordInput = saved })
			mailboxPasswordInput = stdinFile(t, testPassword+"\n")
			return append([]string{"leads", "mailboxes"}, args...)
		}
	}
	mbAct := func(a console.MailboxAction) func(t *testing.T, b *backend) error {
		return func(t *testing.T, b *backend) error {
			_, err := b.MailboxAct(ctx, a)
			return err
		}
	}
	mbIn := func(address string) console.MailboxInput {
		return console.MailboxInput{Address: address, SMTPAddr: "smtp.other.example:465", IMAPAddr: "imap.other.example:993", Selector: "s2",
			FromName: "Desk", Password: config.Secret(testPassword)}
	}
	mbAdd := func(address string) []string {
		return []string{"add", "--address", address, "--smtp", "smtp.other.example:465", "--imap", "imap.other.example:993", "--selector", "s2",
			"--from-name", "Desk"}
	}
	secondOnDomain := func(t *testing.T, dir string) {
		mailboxes(t, dir)
		seedMailbox(t, leadstoretest.Q(t), "second@"+testSenderDomain)
	}
	paused := func(t *testing.T, dir string) {
		mailboxes(t, dir)
		mustRun(t, "leads", "mailboxes", "pause", "1", "--reason", "holiday")
	}
	domainPaused := func(t *testing.T, dir string) {
		secondOnDomain(t, dir)
		mustRun(t, "leads", "mailboxes", "pause", "--domain", testSenderDomain, "--reason", "blocklisted")
	}
	cases := []consoleContract{
		{name: "mailbox add", prep: mailboxes, cli: mbCLI(mbAdd("Desk@Other.example")...),
			console: mbAct(console.MailboxAction{Do: console.MailboxAdd, Input: mbIn("Desk@Other.example")})},
		{name: "mailbox add bad address", prep: mailboxes, exit: 2, refused: true, cli: mbCLI(mbAdd("not an address")...),
			console: mbAct(console.MailboxAction{Do: console.MailboxAdd, Input: mbIn("not an address")})},
		{name: "mailbox add third on a domain", prep: secondOnDomain, exit: 2, refused: true, cli: mbCLI(mbAdd("third@" + testSenderDomain)...),
			console: mbAct(console.MailboxAction{Do: console.MailboxAdd, Input: mbIn("third@" + testSenderDomain)})},
		{name: "mailbox pause", prep: mailboxes, cli: mbCLI("pause", "1", "--reason", "holiday"),
			console: mbAct(console.MailboxAction{Do: console.MailboxPause, ID: 1, Reason: "holiday"})},
		{name: "mailbox resume", prep: paused, cli: mbCLI("resume", "1"),
			console: mbAct(console.MailboxAction{Do: console.MailboxResume, ID: 1})},
		{name: "mailbox retire unconfirmed", prep: mailboxes, exit: 2, refused: true, cli: mbCLI("retire", "1"),
			console: mbAct(console.MailboxAction{Do: console.MailboxRetire, ID: 1})},
		{name: "mailbox retire", prep: func(t *testing.T, dir string) { mailboxes(t, dir); approved(t, dir) }, cli: mbCLI("retire", "1", "--confirm"),
			console: mbAct(console.MailboxAction{Do: console.MailboxRetire, ID: 1, Confirm: true})},
		{name: "mailbox pause domain", prep: secondOnDomain, cli: mbCLI("pause", "--domain", testSenderDomain, "--reason", "blocklisted"),
			console: mbAct(console.MailboxAction{Do: console.MailboxPauseDomain, Domain: testSenderDomain, Reason: "blocklisted"})},
		{name: "mailbox resume domain", prep: domainPaused, cli: mbCLI("resume", "--domain", testSenderDomain),
			console: mbAct(console.MailboxAction{Do: console.MailboxResumeDomain, Domain: testSenderDomain})},
		{name: "approve with a skip", prep: newShadow,
			cli: func(t *testing.T) []string {
				return []string{"leads", "batch", "approve", "1", "--hash", batchHash(t, 1), "--skip", "2"}
			},
			console: act(func(t *testing.T) console.LeadAction {
				return console.LeadAction{Do: console.LeadApprove, ID: 1, Hash: batchHash(t, 1), Skip: []int{2}}
			})},
		{name: "approve wrong hash", prep: newShadow, exit: 2, refused: true,
			cli:     func(*testing.T) []string { return []string{"leads", "batch", "approve", "1", "--hash", "0123456789ab"} },
			console: act(fixed(console.LeadAction{Do: console.LeadApprove, ID: 1, Hash: "0123456789ab"}))},
		{name: "approve no hash", prep: newShadow, exit: 2, refused: true,
			cli:     func(*testing.T) []string { return []string{"leads", "batch", "approve", "1", "--hash", " "} },
			console: act(fixed(console.LeadAction{Do: console.LeadApprove, ID: 1}))},
		{name: "new batch live", prep: live, cli: func(*testing.T) []string { return []string{"leads", "batch", "new"} },
			console: act(fixed(console.LeadAction{Do: console.LeadNewBatch}))},
		{name: "new batch waits", prep: func(t *testing.T, dir string) { live(t, dir); mustRun(t, "leads", "batch", "new") }, refused: true,
			cli: func(*testing.T) []string { return []string{"leads", "batch", "new"} }, console: act(fixed(console.LeadAction{Do: console.LeadNewBatch}))},
		{name: "discard", prep: newShadow, cli: func(*testing.T) []string { return []string{"leads", "batch", "discard", "1"} },
			console: act(fixed(console.LeadAction{Do: console.LeadDiscard, ID: 1}))},
		{name: "discard approved", prep: approved, exit: 2, refused: true, cli: func(*testing.T) []string { return []string{"leads", "batch", "discard", "1"} },
			console: act(fixed(console.LeadAction{Do: console.LeadDiscard, ID: 1}))},
		{name: "done", prep: approved, cli: func(t *testing.T) []string { return []string{"leads", "done", fmt.Sprint(formMsg(t))} },
			console: act(func(t *testing.T) console.LeadAction { return console.LeadAction{Do: console.LeadDone, ID: formMsg(t)} })},
		{name: "suppress", source: true,
			cli: func(*testing.T) []string {
				return []string{"leads", "suppress", "add", "domain", "Litware.example", "--reason", "opt_out"}
			},
			console: act(fixed(console.LeadAction{Do: console.LeadSuppress, Kind: "domain", Value: "Litware.example", Reason: "opt_out"}))},
		{name: "suppress bad reason", exit: 2, refused: true,
			cli: func(*testing.T) []string {
				return []string{"leads", "suppress", "add", "domain", "litware.example", "--reason", "annoyed"}
			},
			console: act(fixed(console.LeadAction{Do: console.LeadSuppress, Kind: "domain", Value: "litware.example", Reason: "annoyed"}))},
		{name: "mark contacted", cli: func(*testing.T) []string {
			return []string{"leads", "mark-contacted", "northwind.example", "--note", "called them"}
		},
			console: act(fixed(console.LeadAction{Do: console.LeadContacted, Value: "northwind.example", Note: "called them"}))},
		{name: "resolve sent", prep: maybe, cli: func(t *testing.T) []string { return []string{"leads", "resolve", fmt.Sprint(maybeMsg(t)), "sent"} },
			console: act(func(t *testing.T) console.LeadAction {
				return console.LeadAction{Do: console.LeadResolve, ID: maybeMsg(t), Sent: true}
			})},
		{name: "resolve not sent", prep: maybe, cli: func(t *testing.T) []string { return []string{"leads", "resolve", fmt.Sprint(maybeMsg(t)), "not-sent"} },
			console: act(func(t *testing.T) console.LeadAction {
				return console.LeadAction{Do: console.LeadResolve, ID: maybeMsg(t)}
			})},
		{name: "rule add", cli: func(*testing.T) []string {
			return []string{"rules", "add", "--kind", "flag", "--field", "title", "--pattern", "contract", "--reason", "contract roles"}
		},
			console: func(t *testing.T, b *backend) error {
				_, err := b.AddRule(ctx, console.RuleRequest{Kind: "flag", Field: "title", Pattern: "contract", Reason: "contract roles"})
				return err
			}},
		{name: "rule add bad pattern", exit: 2, refused: true, cli: func(*testing.T) []string {
			return []string{"rules", "add", "--kind", "skip", "--field", "company", "--pattern", "acme(", "--reason", "x"}
		},
			console: func(t *testing.T, b *backend) error {
				_, err := b.AddRule(ctx, console.RuleRequest{Kind: "skip", Field: "company", Pattern: "acme(", Reason: "x"})
				return err
			}},
		{name: "rule disable", prep: func(t *testing.T, _ string) {
			mustRun(t, "rules", "add", "--kind", "skip", "--field", "company", "--pattern", "acme", "--reason", "agency")
		},
			cli: func(*testing.T) []string { return []string{"rules", "disable", "1", "--reason", "fine"} },
			console: func(t *testing.T, b *backend) error {
				_, err := b.DisableRule(ctx, 1, "fine")
				return err
			}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			run := func(viaCLI bool) (before, after []string) {
				dir := leadsScene(t)
				if c.prep != nil {
					c.prep(t, dir)
				}
				before = append(leadsRows(t), mailboxRows(t, dir)...)
				if viaCLI {
					if code, out, errOut := leadsRun(t, c.cli(t)...); code != c.exit {
						t.Fatalf("CLI exited %d, want %d: %s%s", code, c.exit, out, errOut)
					}
				} else {
					err := c.console(t, leadsBackend(t))
					if c.refused != errors.Is(err, console.ErrUserFixable) || (!c.refused && err != nil) {
						t.Fatalf("console: err = %v, want refused %v", err, c.refused)
					}
				}
				after = append(leadsRows(t), mailboxRows(t, dir)...)
				if !viaCLI && c.source {
					for i, r := range after {
						r = strings.Replace(r, " "+suppressFromConsole, " "+suppressFromCLI, 1)
						after[i] = strings.Replace(r, "("+suppressFromConsole+")", "("+suppressFromCLI+")", 1)
					}
				}
				return before, after
			}
			b1, viaCLI := run(true)
			b2, viaConsole := run(false)
			if !slices.Equal(b1, b2) {
				t.Fatalf("the two starts differ:\n%s", rowsDiff(b1, b2))
			}
			if !slices.Equal(viaCLI, viaConsole) {
				t.Errorf("CLI and console wrote different rows:\n%s", rowsDiff(viaCLI, viaConsole))
			}
			wrote := !slices.Equal(b2, viaConsole)
			if c.refused && wrote && c.name != "new batch waits" {
				t.Errorf("a refusal wrote:\n%s", rowsDiff(b2, viaConsole))
			}
			if !c.refused && !wrote {
				t.Error("the console wrote nothing")
			}
		})
	}
}

// TestConsoleSendPause: the console's pause and resume write the rows leads send pause and
// resume write, and setting the switch to what it is writes nothing.
func TestConsoleSendPause(t *testing.T) {
	ctx := context.Background()
	rows := func(t *testing.T) []string {
		t.Helper()
		var out []string
		r, err := storetest.OwnerConn(t).Query(ctx, `SELECT format('%s %s %s', paused, reason, actor) FROM send_pause ORDER BY id`)
		if err != nil {
			t.Fatal(err)
		}
		for r.Next() {
			var s string
			if err := r.Scan(&s); err != nil {
				t.Fatal(err)
			}
			out = append(out, s)
		}
		return out
	}
	leadsScene(t)
	mustRun(t, "leads", "send", "pause", "--reason", "holiday")
	mustRun(t, "leads", "send", "pause", "--reason", "again")
	mustRun(t, "leads", "send", "resume")
	viaCLI := rows(t)

	leadsScene(t)
	b := leadsBackend(t)
	for _, a := range []console.LeadAction{{Do: console.LeadPause, Reason: "holiday"}, {Do: console.LeadPause, Reason: "again"}, {Do: console.LeadResume}} {
		out, err := b.LeadAct(ctx, a)
		if err != nil {
			t.Fatal(err)
		}
		t.Log(out.Message)
	}
	if got := rows(t); !slices.Equal(got, viaCLI) || len(got) != 2 {
		t.Errorf("console wrote %q, CLI %q", got, viaCLI)
	}
	if _, err := b.LeadAct(ctx, console.LeadAction{Do: console.LeadPause, Reason: "storm"}); err != nil {
		t.Fatal(err)
	}
	p, err := b.Leads(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !p.Sending.Paused || p.Sending.PauseReason != "storm" {
		t.Errorf("leads page sending %+v, want paused for storm", p.Sending)
	}
	d, err := b.Dashboard(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if d.Leads == nil || d.Leads.Paused == nil {
		t.Errorf("dashboard leads card %+v, want paused", d.Leads)
	}
}

// TestConsoleBatchPage: the batch page through the real backend shows every message and the
// hash batch new stored; a row changed since then removes the approve form, and an approval
// from a page read before the change is refused with nothing approved.
func TestConsoleBatchPage(t *testing.T) {
	leadsScene(t)
	mustRun(t, "leads", "batch", "new")
	b := leadsBackend(t)
	h, c := jobsHandler(t, b)
	get := func(path string) (int, string) {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.Header.Set("Cookie", c)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code, w.Body.String()
	}
	hash := batchHash(t, 1)
	code, page := get("/leads/batches/1")
	if code != http.StatusOK || !strings.Contains(page, `name="hash" value="`+hash+`"`) || !strings.Contains(page, `action="/leads/batches/1/approve"`) {
		t.Fatalf("batch page %d without the approve form for %s:\n%s", code, hash, page)
	}
	msgs := batchCount(t, `SELECT count(*)::int FROM outreach_message WHERE batch_id = 1`)
	if n := strings.Count(page, `<section class="message">`); n != msgs {
		t.Errorf("%d messages shown, want %d", n, msgs)
	}
	if !strings.Contains(page, "Finding: ") {
		t.Error("no finding shown")
	}
	if _, err := storetest.OwnerConn(t).Exec(context.Background(),
		`UPDATE outreach_message SET body_text = body_text || ' ' WHERE id = (SELECT min(id) FROM outreach_message WHERE batch_id = 1)`); err != nil {
		t.Fatal(err)
	}
	code, page = get("/leads/batches/1")
	if code != http.StatusOK || strings.Contains(page, `action="/leads/batches/1/approve"`) || !strings.Contains(page, "changed since it was drafted") {
		t.Errorf("changed batch page %d:\n%s", code, page)
	}
	r := httptest.NewRequest(http.MethodPost, "/leads/batches/1/approve", strings.NewReader(url.Values{"hash": {hash}}.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Cookie", c)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), "changed since it was drafted, so it cannot be approved") {
		t.Errorf("approve of a changed batch: %d %s", w.Code, w.Body.String())
	}
	if n := batchCount(t, `SELECT count(*)::int FROM outreach_message WHERE state <> 'draft'`); n != 0 {
		t.Errorf("%d messages left draft", n)
	}
	if code, _ := get("/leads/batches/99"); code != http.StatusNotFound {
		t.Errorf("no such batch: %d", code)
	}
}

// TestConsoleLeadsPagesRead: the leads pages render from the real backend, the queue in the
// order a batch drafts, and the not-set-up line without a leads file.
func TestConsoleLeadsPagesRead(t *testing.T) {
	ctx := context.Background()
	dir := leadsScene(t)
	goLive(t)
	b := leadsBackend(t)
	q, err := b.LeadsNext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cands, err := leadstoretest.Q(t).NextUp(ctx, b.candidateOptions(mustLeads(t), leadsNow()), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(q.Leads) == 0 || len(q.Leads) != len(cands) {
		t.Fatalf("queue %d leads, NextUp %d", len(q.Leads), len(cands))
	}
	for i, c := range cands {
		if q.Leads[i].LeadID != c.LeadID || q.Leads[i].Score != c.Score {
			t.Errorf("queue %d is lead %d score %d, want %d score %d", i, q.Leads[i].LeadID, q.Leads[i].Score, c.LeadID, c.Score)
		}
	}
	if q.DeferUntil != "2026-11-10" {
		t.Errorf("default push-back %s", q.DeferUntil)
	}
	h, c := jobsHandler(t, b)
	for _, p := range []string{"/leads", "/leads/outreach", "/leads/priorities", "/"} {
		r := httptest.NewRequest(http.MethodGet, p, nil)
		r.Header.Set("Cookie", c)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusOK || strings.Contains(w.Body.String(), "not set up") {
			t.Errorf("%s: %d %s", p, w.Code, w.Body.String())
		}
	}
	// The old queue page is the list in the outreach order now.
	r := httptest.NewRequest(http.MethodGet, "/leads/next", nil)
	r.Header.Set("Cookie", c)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/leads?sort=order" {
		t.Errorf("/leads/next: %d to %q", w.Code, w.Header().Get("Location"))
	}
	if err := os.Remove(filepath.Join(dir, "leads.yaml")); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Leads(ctx); !errors.Is(err, console.ErrLeadsNotSetUp) {
		t.Errorf("without leads.yaml: %v", err)
	}
	d, err := b.Dashboard(ctx)
	if err != nil || d.Leads != nil {
		t.Errorf("dashboard without leads.yaml: %v %+v", err, d.Leads)
	}
}

func mustLeads(t *testing.T) leadconfig.Config {
	t.Helper()
	setup, err := leadsEnv(mustConfig(t), leadsForProbe)
	if err != nil {
		t.Fatal(err)
	}
	return setup.leads
}

// TestConsoleQueueSteering: star, unstar, up, down and pin to top write the operator's
// order the queue reads (starred first); a move of an unstarred lead is refused; push back
// (the default 14 days when no day is given), clear and skip write the priority rows; a
// preview of an edited profile writes nothing; save and reset add versions.
func TestConsoleQueueSteering(t *testing.T) {
	ctx := context.Background()
	leadsScene(t)
	goLive(t)
	b := leadsBackend(t)
	q, err := b.LeadsNext(ctx)
	if err != nil || len(q.Leads) < 3 {
		t.Fatalf("queue %v %v", q.Leads, err)
	}
	act := func(do string, id int64) (console.LeadOutcome, error) {
		t.Helper()
		return b.LeadAct(ctx, console.LeadAction{Do: do, ID: id})
	}
	head := func(n int) []int64 {
		t.Helper()
		q, err := b.LeadsNext(ctx)
		if err != nil || len(q.Leads) < n {
			t.Fatalf("queue %v %v", q.Leads, err)
		}
		ids := make([]int64, n)
		for i := range ids {
			ids[i] = q.Leads[i].LeadID
		}
		return ids
	}
	last, second := q.Leads[len(q.Leads)-1].LeadID, q.Leads[len(q.Leads)-2].LeadID
	for _, do := range []string{console.LeadUp, console.LeadDown, console.LeadTop} {
		if _, err := act(do, last); !errors.Is(err, console.ErrUserFixable) || !strings.Contains(err.Error(), "Star it first.") {
			t.Errorf("%s of an unstarred lead: %v", do, err)
		}
	}
	for _, id := range []int64{second, last} {
		if out, err := act(console.LeadStar, id); err != nil || !strings.Contains(out.Message, "starred") {
			t.Fatalf("star %d: %+v %v", id, out, err)
		}
	}
	if got := head(2); !slices.Equal(got, []int64{second, last}) {
		t.Errorf("starred order %v, want %v", got, []int64{second, last})
	}
	if out, err := act(console.LeadStar, last); err != nil || !strings.Contains(out.Message, "already starred") {
		t.Errorf("star again: %+v %v", out, err)
	}
	steps := []struct {
		do   string
		want []int64
	}{
		{console.LeadTop, []int64{last, second}},
		{console.LeadUp, []int64{last, second}}, // already first: nothing moves
		{console.LeadDown, []int64{second, last}},
		{console.LeadUp, []int64{last, second}},
	}
	for _, st := range steps {
		if _, err := act(st.do, last); err != nil {
			t.Fatalf("%s: %v", st.do, err)
		}
		if got := head(2); !slices.Equal(got, st.want) {
			t.Errorf("after %s: %v, want %v", st.do, got, st.want)
		}
	}
	if _, err := act(console.LeadUnstar, second); err != nil {
		t.Fatal(err)
	}
	if got := head(2); got[0] != last || got[1] == second {
		t.Errorf("after unstar: %v", got)
	}
	var operatorRows, otherRows int
	if err := storetest.OwnerConn(t).QueryRow(ctx,
		`SELECT count(*) FILTER (WHERE actor = 'operator')::int, count(*) FILTER (WHERE actor <> 'operator')::int FROM lead_star`).Scan(&operatorRows, &otherRows); err != nil {
		t.Fatal(err)
	}
	if operatorRows == 0 || otherRows != 0 {
		t.Errorf("lead_star rows: %d by the operator, %d by others", operatorRows, otherRows)
	}

	if _, err := b.LeadAct(ctx, console.LeadAction{Do: console.LeadDefer, ID: last, Until: "2026-10-20"}); !errors.Is(err, console.ErrUserFixable) {
		t.Errorf("a past day: %v", err)
	}
	if _, err := b.LeadAct(ctx, console.LeadAction{Do: console.LeadDefer, ID: last, Until: "nope"}); !errors.Is(err, console.ErrUserFixable) {
		t.Errorf("not a day: %v", err)
	}
	out, err := act(console.LeadDefer, last) // no day: the default push-back
	if err != nil || !strings.Contains(out.Message, "pushed back") {
		t.Fatalf("default push back: %+v %v", out, err)
	}
	var until string
	if err := storetest.OwnerConn(t).QueryRow(ctx,
		`SELECT until::text FROM lead_priority WHERE lead_id = $1 ORDER BY id DESC LIMIT 1`, last).Scan(&until); err != nil || until != "2026-11-10" {
		t.Errorf("default push back until %q (%v), want 2026-11-10", until, err)
	}
	q, _ = b.LeadsNext(ctx)
	for _, l := range q.Leads {
		if l.LeadID == last {
			t.Error("a pushed-back lead is still in the queue")
		}
	}
	if _, err := act(console.LeadClear, last); err != nil {
		t.Fatal(err)
	}
	first := q.Leads[0].LeadID
	if _, err := act(console.LeadSkip, first); err != nil {
		t.Fatal(err)
	}
	var state string
	if err := storetest.OwnerConn(t).QueryRow(ctx, `SELECT state FROM lead WHERE id = $1`, first).Scan(&state); err != nil || state != "skipped" {
		t.Errorf("skipped lead is %q (%v)", state, err)
	}
	for _, do := range []string{console.LeadStar, console.LeadUnstar, console.LeadUp, console.LeadDown, console.LeadTop, console.LeadSkip} {
		if _, err := act(do, 999); !errors.Is(err, console.ErrUserFixable) || !strings.Contains(err.Error(), "There is no lead 999.") {
			t.Errorf("%s of no such lead: %v", do, err)
		}
	}
	if _, err := act("pin", last); !errors.Is(err, console.ErrUserFixable) {
		t.Errorf("the retired pin: %v", err)
	}

	versions := func() int { return batchCount(t, `SELECT count(*)::int FROM priority_profile`) }
	page, err := b.LeadPriorities(ctx, nil)
	if err != nil || page.FromDB || len(page.Preview) == 0 {
		t.Fatalf("priorities %+v %v", page, err)
	}
	edit := page.Form
	edit.Countries = "gb 90"
	prev, err := b.LeadPriorities(ctx, &edit)
	if err != nil || len(prev.Problems) != 0 || !prev.Edited {
		t.Fatalf("preview %+v %v", prev, err)
	}
	if versions() != 0 {
		t.Error("a preview saved a version")
	}
	bad := page.Form
	bad.RankBands = "ten 5"
	if prev, _ := b.LeadPriorities(ctx, &bad); len(prev.Problems) == 0 || prev.Preview != nil {
		t.Errorf("a bad edit previewed: %+v", prev)
	}
	if _, err := b.LeadAct(ctx, console.LeadAction{Do: console.LeadSavePriority, Form: bad}); !errors.Is(err, console.ErrUserFixable) {
		t.Errorf("a bad save: %v", err)
	}
	if _, err := b.LeadAct(ctx, console.LeadAction{Do: console.LeadSavePriority, Form: edit}); err != nil {
		t.Fatal(err)
	}
	if page, _ = b.LeadPriorities(ctx, nil); !page.FromDB || page.Form.Countries != "GB 90" {
		t.Errorf("after save %+v", page.Form)
	}
	if _, err := b.LeadAct(ctx, console.LeadAction{Do: console.LeadResetPrio}); err != nil {
		t.Fatal(err)
	}
	if page, _ = b.LeadPriorities(ctx, nil); page.FromDB || versions() != 2 {
		t.Errorf("after reset FromDB %v, %d versions", page.FromDB, versions())
	}
}

// TestConsoleNewBatchNeverShadow: the console refuses to draft while sending is not set up,
// and a live batch it makes waits for approval.
func TestConsoleNewBatchNeverShadow(t *testing.T) {
	ctx := context.Background()
	dir := leadsScene(t)
	b := leadsBackend(t)
	if _, err := b.LeadAct(ctx, console.LeadAction{Do: console.LeadNewBatch}); !errors.Is(err, console.ErrUserFixable) ||
		!strings.Contains(err.Error(), "Sending isn't set up on this server yet.") {
		t.Fatalf("shadow mode: %v", err)
	}
	if n := batchCount(t, `SELECT count(*)::int FROM outreach_batch`); n != 0 {
		t.Fatalf("%d batches", n)
	}
	goLive(t)
	setCaps(t, dir, "first-send", "[{from_day: 0, per_day: 4}]")
	out, err := b.LeadAct(ctx, console.LeadAction{Do: console.LeadNewBatch})
	if err != nil || out.BatchID != 1 {
		t.Fatalf("live: %+v %v", out, err)
	}
	t.Log(out.Message)
	if n := batchCount(t, `SELECT count(*)::int FROM outreach_batch WHERE NOT shadow AND approved_at IS NULL`); n != 1 {
		t.Errorf("%d live batches waiting", n)
	}
	if n := batchCount(t, `SELECT count(*)::int FROM outreach_message WHERE state <> 'draft'`); n != 0 {
		t.Errorf("%d messages past draft", n)
	}
}

// TestConsoleJobPage: the job page through the real backend shows the job's description,
// its verdicts and its history, and a mark from it writes what track writes.
func TestConsoleJobPage(t *testing.T) {
	ids := sentBrief(t)
	b := testBackend(t)
	ctx := context.Background()
	j, err := b.Job(ctx, ids[0])
	if err != nil {
		t.Fatal(err)
	}
	if j.Description == "" || len(j.Criteria) == 0 || len(j.History) == 0 || j.Company == "" {
		t.Fatalf("job page %+v", j)
	}
	for _, cr := range j.Criteria {
		if cr.Verdict == "" {
			t.Errorf("criterion %s has no verdict", cr.Name)
		}
	}
	if _, err := b.Job(ctx, 999999); !errors.Is(err, console.ErrNotFound) {
		t.Errorf("no such job: %v", err)
	}
	h, c := jobsHandler(t, b)
	path := fmt.Sprintf("/jobs/%d", ids[0])
	r := httptest.NewRequest(http.MethodGet, path, nil)
	r.Header.Set("Cookie", c)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	page := w.Body.String()
	firstLine, _, _ := strings.Cut(j.Description, "\n")
	for _, want := range []string{fmt.Sprintf(`<article class="record" id="job-%d">`, ids[0]), "<dt>Apply link</dt>", "<dt>Status</dt>",
		template.HTMLEscapeString(firstLine)} {
		if !strings.Contains(page, want) {
			t.Errorf("job page lacks %q", want)
		}
	}
	r = httptest.NewRequest(http.MethodPost, path+"/track", strings.NewReader(url.Values{"action": {"applied"}}.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Cookie", c)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `id="outcome">`) {
		t.Fatalf("mark: %d %s", w.Code, w.Body.String())
	}
	if got := readTrackRows(t, ids[0]); got.status != "applied" || got.actor != "gremy" {
		t.Errorf("mark wrote %+v", got)
	}
	if !strings.Contains(w.Body.String(), `<span class="status status-applied">applied</span>`) {
		t.Error("the page after the mark does not show the new status")
	}
}

// TestConsoleRuleMarksStale: a rule added or disabled from the console makes the dashboard's
// numbers stale and starts their rebuild, as a mark does.
func TestConsoleRuleMarksStale(t *testing.T) {
	leadsScene(t)
	b := leadsBackend(t)
	ctx := context.Background()
	if _, err := b.AddRule(ctx, console.RuleRequest{Kind: "skip", Field: "company", Pattern: "acme", Reason: "agency"}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.DisableRule(ctx, 1, "fine"); err != nil {
		t.Fatal(err)
	}
	b.mu.Lock()
	marks := b.marks
	b.mu.Unlock()
	if marks != 2 {
		t.Errorf("%d marks, want 2", marks)
	}
	if _, err := b.DisableRule(ctx, 1, "again"); !errors.Is(err, console.ErrUserFixable) {
		t.Errorf("disabled twice: %v", err)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.marks != 2 {
		t.Error("a refusal marked the snapshot stale")
	}
}
