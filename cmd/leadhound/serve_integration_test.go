//go:build integration

package main

import (
	"context"
	"errors"
	"fmt"
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
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/brief"
	"github.com/gremy/webbership-leadhound/internal/config"
	"github.com/gremy/webbership-leadhound/internal/console"
	"github.com/gremy/webbership-leadhound/internal/store"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

// testBackend is the console backend over the test database, as serve builds it.
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
	return newBackend(s, prof, loc, cfg)
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

// TestDashboardCache: the dashboard's brief input is kept for a minute on the same local
// day, and a mark through Track clears it.
func TestDashboardCache(t *testing.T) {
	sentBrief(t)
	b := testBackend(t)
	now := time.Now()
	b.now = func() time.Time { return now }
	ctx := context.Background()
	first, err := b.Dashboard(ctx)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(30 * time.Second)
	second, _ := b.Dashboard(ctx)
	if !second.AsOf.Equal(first.AsOf) {
		t.Errorf("within a minute: as of %v, then %v", first.AsOf, second.AsOf)
	}
	now = now.Add(31 * time.Second)
	third, _ := b.Dashboard(ctx)
	if !third.AsOf.Equal(now) {
		t.Errorf("after a minute: as of %v, want %v", third.AsOf, now)
	}
	now = now.Add(time.Second)
	if _, err := b.Track(ctx, console.TrackRequest{Ref: "1", Action: "applied"}); err != nil {
		t.Fatal(err)
	}
	fourth, _ := b.Dashboard(ctx)
	if !fourth.AsOf.Equal(now) {
		t.Errorf("after a mark: as of %v, want %v", fourth.AsOf, now)
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
	if tr, err := b.Tracked(ctx, ""); err != nil || len(tr) != 0 {
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
	tr, err := b.Tracked(ctx, "skipped")
	if err != nil || len(tr) != 1 || tr[0].JobID != ids[2] || tr[0].Reason != "agency" || tr[0].Since.IsZero() {
		t.Errorf("tracked skipped = %+v, %v", tr, err)
	}
	all, err := b.Tracked(ctx, "all")
	if want := dbCount(t, `SELECT count(DISTINCT job_id) FROM status_event`); err != nil || len(all) != want {
		t.Errorf("tracked all = %d, %v; want %d", len(all), err, want)
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
