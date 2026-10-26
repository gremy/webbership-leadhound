package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/gremy/webbership-leadhound/internal/brief"
	"github.com/gremy/webbership-leadhound/internal/console"
	"github.com/gremy/webbership-leadhound/internal/leadconfig"
	"github.com/gremy/webbership-leadhound/internal/secretbox"
)

var (
	testHashOnce sync.Once
	testHashVal  string
	testHashErr  error
)

// testHash is a cost-12 hash of a test password, made once per test binary.
func testHash(t *testing.T) string {
	t.Helper()
	testHashOnce.Do(func() { testHashVal, testHashErr = console.HashPassword([]byte("a-test-password-only")) })
	if testHashErr != nil {
		t.Fatal(testHashErr)
	}
	return testHashVal
}

// serveEnv sets everything serve needs to start except the database, which points at a
// closed port: a refusal must come before the database is opened.
func serveEnv(t *testing.T) {
	t.Helper()
	t.Setenv("LEADHOUND_DATABASE_URL", "postgres://nobody@127.0.0.1:1/none?sslmode=disable&connect_timeout=1")
	t.Setenv("LEADHOUND_DATA_DIR", demoDataDir(t))
	t.Setenv("LEADHOUND_LIVE", "")
	t.Setenv("LEADHOUND_TZ", "")
	t.Setenv("LEADHOUND_CONSOLE_ADDR", "")
	t.Setenv("LEADHOUND_CONSOLE_URL", "")
	t.Setenv("LEADHOUND_CONSOLE_EMAIL", "operator@mail.example")
	t.Setenv("LEADHOUND_CONSOLE_PASSWORD_HASH", testHash(t))
	t.Setenv(secretbox.KeyFileEnv, "")
}

// TestServeRefusals: serve exits 2, naming the problem, without the operator's email or
// hash, with a hash that is not bcrypt or is too cheap, in dev mode with live fetching
// on, on an address off loopback and with a console URL that is not https.
func TestServeRefusals(t *testing.T) {
	cheap, err := bcrypt.GenerateFromPassword([]byte("a-test-password-only"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name, key, value string
		args             []string
		want             string
	}{
		{"no email", "LEADHOUND_CONSOLE_EMAIL", "", nil, "LEADHOUND_CONSOLE_EMAIL is not set"},
		{"no hash", "LEADHOUND_CONSOLE_PASSWORD_HASH", "", nil, "LEADHOUND_CONSOLE_PASSWORD_HASH is not set"},
		{"bad hash", "LEADHOUND_CONSOLE_PASSWORD_HASH", "not-a-hash", nil, "not a bcrypt hash"},
		{"cheap hash", "LEADHOUND_CONSOLE_PASSWORD_HASH", string(cheap), nil, "below 12"},
		{"dev live on", "LEADHOUND_LIVE", "1", []string{"--dev"}, "--dev"},
		{"dev live ats", "LEADHOUND_LIVE", "ats", []string{"--dev"}, "--dev"},
		{"public addr", "LEADHOUND_CONSOLE_ADDR", "192.0.2.10:8090", nil, "loopback"},
		{"any addr", "LEADHOUND_CONSOLE_ADDR", ":8090", nil, "loopback"},
		{"http url", "LEADHOUND_CONSOLE_URL", "http://console.example", nil, "LEADHOUND_CONSOLE_URL must be an https URL"},
		{"stray argument", "", "", []string{"now"}, "usage: leadhound serve"},
	} {
		t.Run(c.name, func(t *testing.T) {
			serveEnv(t)
			if c.key != "" {
				t.Setenv(c.key, c.value)
			}
			captureStdout(t)
			var stderr bytes.Buffer
			code := run(context.Background(), append([]string{"serve"}, c.args...), &stderr)
			if code != 2 || !strings.Contains(stderr.String(), c.want) {
				t.Errorf("exit %d, stderr %q; want 2 naming %q", code, stderr.String(), c.want)
			}
			if strings.Contains(stderr.String(), "$2") {
				t.Error("stderr repeats the hash")
			}
		})
	}
}

// TestServeStartsPastRefusals: with every setting usable, serve gets past its checks to the
// database, whose failure is a crash (exit 1), not a usage error.
func TestServeStartsPastRefusals(t *testing.T) {
	serveEnv(t)
	t.Setenv("LEADHOUND_LIVE", "1")
	t.Setenv("LEADHOUND_CONSOLE_URL", "https://console.example")
	var stderr bytes.Buffer
	if code := run(context.Background(), []string{"serve"}, &stderr); code != 1 {
		t.Errorf("exit %d, want 1; stderr %q", code, stderr.String())
	}
}

// pipeStdin points the password input at a file holding s, as a pipe would.
func pipeStdin(t *testing.T, s string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "stdin")
	if err := os.WriteFile(path, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path) // #nosec G304 -- a test temp file
	if err != nil {
		t.Fatal(err)
	}
	saved := passwordInput
	passwordInput = f
	t.Cleanup(func() { passwordInput = saved; _ = f.Close() })
}

// TestHashPasswordPipe: a piped password prints one cost-12 bcrypt hash that matches it,
// and nothing else.
func TestHashPasswordPipe(t *testing.T) {
	const pw = "correct horse battery"
	pipeStdin(t, pw+"\n")
	out := captureStdout(t)
	var stderr bytes.Buffer
	if code := run(context.Background(), []string{"console", "hash-password"}, &stderr); code != 0 {
		t.Fatalf("exit %d, %q", code, stderr.String())
	}
	h := strings.TrimSuffix(out.String(), "\n")
	if !strings.HasPrefix(h, "$2") || strings.Contains(h, "\n") || strings.Contains(stderr.String(), pw) {
		t.Fatalf("stdout %q, stderr %q", out.String(), stderr.String())
	}
	if cost, err := bcrypt.Cost([]byte(h)); err != nil || cost != 12 {
		t.Errorf("cost %d, %v", cost, err)
	}
	if err := bcrypt.CompareHashAndPassword([]byte(h), []byte(pw)); err != nil {
		t.Errorf("the hash does not match the password: %v", err)
	}
}

// TestHashPasswordRefusals: a short or over-long password, an empty input and a wrong
// subcommand exit 2 and print no hash.
func TestHashPasswordRefusals(t *testing.T) {
	for _, c := range []struct {
		name, in string
		args     []string
		want     string
	}{
		{"short", "eleven-char\n", nil, "at least 12"},
		{"empty", "", nil, "at least 12"},
		{"too long", strings.Repeat("x", 73) + "\n", nil, "at most 72 bytes"},
		{"no subcommand", "a-long-enough-one\n", []string{"console"}, "usage: leadhound console hash-password"},
		{"unknown subcommand", "a-long-enough-one\n", []string{"console", "reset"}, "usage: leadhound console hash-password"},
	} {
		t.Run(c.name, func(t *testing.T) {
			pipeStdin(t, c.in)
			out := captureStdout(t)
			args := c.args
			if args == nil {
				args = []string{"console", "hash-password"}
			}
			var stderr bytes.Buffer
			code := run(context.Background(), args, &stderr)
			if code != 2 || !strings.Contains(stderr.String(), c.want) || out.Len() != 0 {
				t.Errorf("exit %d, stdout %q, stderr %q; want 2 naming %q", code, out.String(), stderr.String(), c.want)
			}
		})
	}
}

// TestReadPasswordLineKeepsSpaces: only the line ending is removed from a piped password.
func TestReadPasswordLineKeepsSpaces(t *testing.T) {
	for in, want := range map[string]string{
		" padded password \n":   " padded password ",
		"windows line ends\r\n": "windows line ends",
		"no line end":           "no line end",
		"first line\nsecond\n":  "first line",
	} {
		got, err := readPasswordLine(strings.NewReader(in))
		if err != nil || string(got) != want {
			t.Errorf("%q: got %q, %v; want %q", in, got, err, want)
		}
	}
}

// TestBriefLogHealth: only the lines that start with "Health: " are the stored health.
func TestBriefLogHealth(t *testing.T) {
	body := "LeadHound · Fri 23 Oct\n\nHealth: LinkedIn ok\nNot Health: x\nHealth: Scoring ok\r\n"
	got := storedHealth(body)
	if len(got) != 2 || got[0] != "Health: LinkedIn ok" || got[1] != "Health: Scoring ok" {
		t.Errorf("health = %q", got)
	}
	if storedHealth("") != nil {
		t.Error("an empty body has health lines")
	}
}

// testClock is a clock the test moves and the builds read.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// fakeBuild is a dashboard build that counts its calls, returns a snapshot whose Unscored
// is its call number, fails while fail is set, and, while hold is set, waits for a value on
// release (or its context's end) before it returns.
type fakeBuild struct {
	mu      sync.Mutex
	calls   int
	fail    bool
	hold    bool
	release chan struct{}
	ended   chan error // each build's end, its context's error when that ended it
}

func newFakeBuild() *fakeBuild {
	return &fakeBuild{release: make(chan struct{}), ended: make(chan error, 16)}
}

func (f *fakeBuild) build(ctx context.Context, _ time.Time) (snapshot, error) {
	f.mu.Lock()
	f.calls++
	n, fail, hold := f.calls, f.fail, f.hold
	f.mu.Unlock()
	if hold {
		select {
		case <-f.release:
		case <-ctx.Done():
			f.ended <- ctx.Err()
			return snapshot{}, ctx.Err()
		}
	}
	f.ended <- nil
	if fail {
		return snapshot{}, errors.New("build failed")
	}
	return snapshot{in: brief.Input{Unscored: n}}, nil
}

func (f *fakeBuild) n() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeBuild) set(fail, hold bool) {
	f.mu.Lock()
	f.fail, f.hold = fail, hold
	f.mu.Unlock()
}

// waitEnded waits for one build to end and returns its context's error, if that ended it.
func (f *fakeBuild) waitEnded(t *testing.T) error {
	t.Helper()
	select {
	case err := <-f.ended:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("no build ended")
		return nil
	}
}

// started waits until n builds have started.
func (f *fakeBuild) started(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for f.n() < n {
		if time.Now().After(deadline) {
			t.Fatalf("%d builds started, want %d", f.n(), n)
		}
		time.Sleep(time.Millisecond)
	}
}

// endedOK waits for one build to end on its own.
func (f *fakeBuild) endedOK(t *testing.T) {
	t.Helper()
	if err := f.waitEnded(t); err != nil {
		t.Fatalf("a build ended with %v", err)
	}
}

// snapBackend is a backend with only its snapshot part: the fake build, the test clock at
// 09:00 UTC, and a base context the test ends when it is done, waiting for every build.
func snapBackend(t *testing.T, f *fakeBuild) (*backend, *testClock, context.CancelFunc) {
	t.Helper()
	clk := &testClock{t: time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC)}
	base, cancel := context.WithCancel(context.Background())
	b := &backend{loc: time.UTC, now: clk.now, base: base, build: f.build, waitFor: dashboardWaitFor}
	t.Cleanup(func() { cancel(); b.wait() })
	return b, clk, cancel
}

// running is whether a build runs.
func running(b *backend) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.building != nil
}

// settle waits until no build runs.
func settle(t *testing.T, b *backend) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		b.mu.Lock()
		running := b.building != nil
		b.mu.Unlock()
		if !running {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("a build is still running")
		}
		time.Sleep(time.Millisecond)
	}
}

// TestDashboardSnapshotColdStart: the first request, with a slow build running, answers
// within 600 ms that it is building, with nothing to show; a second request during that
// build starts none; once it lands it is served as it is, with no new build.
func TestDashboardSnapshotColdStart(t *testing.T) {
	f := newFakeBuild()
	f.set(false, true)
	b, clk, _ := snapBackend(t, f)
	start := time.Now()
	v := b.current(context.Background(), clk.now())
	if took := time.Since(start); took > 600*time.Millisecond {
		t.Errorf("cold start took %v", took)
	}
	if !v.updating || !v.at.IsZero() || v.failedAt != nil {
		t.Errorf("cold start = %+v, want building with nothing", v)
	}
	if v := b.current(context.Background(), clk.now()); !v.updating || !v.at.IsZero() || f.n() != 1 {
		t.Errorf("second request = %+v, builds %d; want still building, one build", v, f.n())
	}
	f.release <- struct{}{}
	f.endedOK(t)
	settle(t, b)
	clk.add(time.Minute)
	v = b.current(context.Background(), clk.now())
	if v.updating || v.snap.in.Unscored != 1 || !v.at.Equal(time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC)) || f.n() != 1 {
		t.Errorf("after the build = %+v, builds %d", v, f.n())
	}
}

// TestDashboardSnapshotQuickBuild: a build that ends within the wait is served by the
// request that started it.
func TestDashboardSnapshotQuickBuild(t *testing.T) {
	f := newFakeBuild()
	b, clk, _ := snapBackend(t, f)
	if v := b.current(context.Background(), clk.now()); v.updating || v.at.IsZero() || v.snap.in.Unscored != 1 {
		t.Errorf("quick build = %+v", v)
	}
}

// TestDashboardSnapshotFreshness: a snapshot is served until it is dashboardFreshFor old
// or the local day changes; then the old one is served at once, marked updating, while the
// new one builds.
func TestDashboardSnapshotFreshness(t *testing.T) {
	f := newFakeBuild()
	b, clk, _ := snapBackend(t, f)
	b.current(context.Background(), clk.now())
	clk.add(dashboardFreshFor - time.Second)
	if b.current(context.Background(), clk.now()); f.n() != 1 {
		t.Fatalf("within %v: %d builds", dashboardFreshFor, f.n())
	}
	f.set(false, true)
	clk.add(time.Second)
	start := time.Now()
	v := b.current(context.Background(), clk.now())
	if took := time.Since(start); took > 100*time.Millisecond {
		t.Errorf("the old snapshot took %v to serve", took)
	}
	if f.started(t, 2); f.n() != 2 || !v.updating || v.snap.in.Unscored != 1 {
		t.Errorf("at %v: %+v, builds %d; want the old one, updating", dashboardFreshFor, v, f.n())
	}
	f.release <- struct{}{}
	f.endedOK(t)
	f.endedOK(t)
	settle(t, b)

	// 23:59 to 00:01 is another day, two minutes on.
	f.set(false, false)
	clk.add(14*time.Hour + 49*time.Minute + 59*time.Second)
	b.current(context.Background(), clk.now())
	settle(t, b)
	n := f.n()
	clk.add(2 * time.Minute)
	b.current(context.Background(), clk.now())
	settle(t, b)
	if v := b.current(context.Background(), clk.now()); f.n() != n+1 || v.at.Day() != 25 {
		t.Errorf("the next day: %+v, builds %d; want a new build", v, f.n()-n)
	}
}

// TestDashboardSnapshotRequestEnds: a request that ends while it waits leaves the build
// running, and the build lands.
func TestDashboardSnapshotRequestEnds(t *testing.T) {
	f := newFakeBuild()
	f.set(false, true)
	b, clk, _ := snapBackend(t, f)
	b.waitFor = time.Minute
	ctx, cancel := context.WithCancel(context.Background())
	got := make(chan snapshotView)
	go func() { got <- b.current(ctx, clk.now()) }()
	cancel()
	if v := <-got; !v.updating {
		t.Errorf("the request saw %+v", v)
	}
	f.release <- struct{}{}
	if err := f.waitEnded(t); err != nil {
		t.Errorf("the build ended with %v", err)
	}
	settle(t, b)
	if v := b.current(context.Background(), clk.now()); v.at.IsZero() || f.n() != 1 {
		t.Errorf("after the build: %+v, builds %d", v, f.n())
	}
}

// TestDashboardSnapshotBackoff: a failed build keeps the old snapshot and says when it
// failed; no build starts for dashboardRetryAfter; then one does, and its success clears
// the failure.
func TestDashboardSnapshotBackoff(t *testing.T) {
	f := newFakeBuild()
	b, clk, _ := snapBackend(t, f)
	b.current(context.Background(), clk.now())
	f.set(true, false)
	clk.add(dashboardFreshFor)
	failedAt := clk.now()
	if v := b.current(context.Background(), clk.now()); !v.updating || v.failedAt != nil || v.snap.in.Unscored != 1 {
		t.Fatalf("an old snapshot is not served at once: %+v", v)
	}
	settle(t, b)
	v := b.current(context.Background(), clk.now())
	if f.n() != 2 || v.failedAt == nil || !v.failedAt.Equal(failedAt) || v.snap.in.Unscored != 1 || v.updating {
		t.Fatalf("after a failure: %+v, builds %d", v, f.n())
	}
	for range 3 {
		clk.add(dashboardRetryAfter/3 - time.Second)
		if v := b.current(context.Background(), clk.now()); f.n() != 2 || v.failedAt == nil || v.snap.in.Unscored != 1 {
			t.Fatalf("backing off: %+v, builds %d", v, f.n())
		}
	}
	if b.marked(); running(b) {
		t.Errorf("a mark started a build while backing off")
	}
	clk.add(3 * time.Second)
	f.set(false, false)
	b.current(context.Background(), clk.now())
	settle(t, b)
	if v := b.current(context.Background(), clk.now()); f.n() != 3 || v.failedAt != nil || v.snap.in.Unscored != 3 {
		t.Errorf("after %v: %+v, builds %d", dashboardRetryAfter, v, f.n())
	}

	// With no snapshot yet, a failure is all there is.
	f2 := newFakeBuild()
	f2.set(true, false)
	b2, clk2, _ := snapBackend(t, f2)
	if v := b2.current(context.Background(), clk2.now()); !v.at.IsZero() || v.failedAt == nil || v.updating {
		t.Errorf("first build failed: %+v", v)
	}
	if b2.current(context.Background(), clk2.now()); f2.n() != 1 {
		t.Errorf("first failure: %d builds, want 1", f2.n())
	}
}

// TestDashboardSnapshotMark: a mark makes the snapshot stale and starts a rebuild at once,
// the old numbers shown as updating until it lands; a mark during a build starts another
// once that one lands, since what it read is from before the mark; a mark during it does
// not start a second at the same time.
func TestDashboardSnapshotMark(t *testing.T) {
	f := newFakeBuild()
	b, clk, _ := snapBackend(t, f)
	b.current(context.Background(), clk.now())
	f.set(false, true)
	clk.add(time.Second)
	if b.marked(); !running(b) {
		t.Fatal("a mark started no build")
	}
	f.started(t, 2)
	if v := b.current(context.Background(), clk.now()); !v.updating || v.snap.in.Unscored != 1 {
		t.Errorf("after the mark: %+v, builds %d", v, f.n())
	}
	b.marked() // during the build: the build running is still the one the first mark started
	b.mu.Lock()
	if b.marks != 2 {
		t.Errorf("marks = %d", b.marks)
	}
	b.mu.Unlock()
	f.set(false, false)
	f.release <- struct{}{}
	f.endedOK(t)
	f.endedOK(t) // the rebuild for the second mark
	settle(t, b)
	if v := b.current(context.Background(), clk.now()); v.updating || v.snap.in.Unscored != 3 || f.n() != 3 {
		t.Errorf("after both marks: %+v, builds %d", v, f.n())
	}
}

// TestDashboardSnapshotShutdown: ending base cancels a running build, wait returns once it
// has ended, and no build starts after.
func TestDashboardSnapshotShutdown(t *testing.T) {
	f := newFakeBuild()
	f.set(false, true)
	b, clk, cancel := snapBackend(t, f)
	b.current(context.Background(), clk.now())
	cancel()
	if err := f.waitEnded(t); !errors.Is(err, context.Canceled) {
		t.Errorf("the build ended with %v, want its context cancelled", err)
	}
	done := make(chan struct{})
	go func() { b.wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("wait did not return")
	}
	clk.add(dashboardRetryAfter + dashboardFreshFor)
	if b.current(context.Background(), clk.now()); f.n() != 1 {
		t.Errorf("a build started after shutdown")
	}
	if b.marked(); running(b) {
		t.Errorf("a mark started a build after shutdown")
	}
}

// TestPriorityFormRoundTrip: a profile shown as the priorities form reads back as the same
// profile, and a form with problems names each of them.
func TestPriorityFormRoundTrip(t *testing.T) {
	p := leadconfig.Profile{
		Kinds: map[string]int{"unsupported": 20, "d10": 10}, Majors: map[int]int{7: 30, 10: -5}, Channels: map[string]int{"email": 10, "form": 0},
		Countries: map[string]int{"GB": 20, "DE": 10}, RankBands: []leadconfig.RankBand{{Under: 10000, Points: 15}, {Under: 100000, Points: 8}},
		ExactVersion: 5, ExcludeCountries: []string{"FR", "IT"},
	}
	f := profileForm(p)
	if f.Countries != "DE 10, GB 20" || f.RankBands != "10000 15, 100000 8" || f.Exclude != "FR, IT" || f.ExactVersion != "5" {
		t.Errorf("form %+v", f)
	}
	got, problems := parseProfileForm(f)
	if len(problems) != 0 || !reflect.DeepEqual(got, p) {
		t.Errorf("read back %+v (%v), want %+v", got, problems, p)
	}
	bad := f
	bad.Points = append(slices.Clone(f.Points), console.PointField{Key: "major.12", Value: "5"}, console.PointField{Key: "kind.hand", Value: "x"})
	bad.Countries, bad.RankBands, bad.ExactVersion = "gb", "ten 5", "200"
	_, problems = parseProfileForm(bad)
	for _, want := range []string{`"kind.hand" is not a priority rule`, `"major.12" is not a priority rule`, `Countries: "gb" is not two values`,
		`Site rank bands: "ten" is not a whole number`, "priority.exact_version: 200 points is outside -100 to 100"} {
		if !slices.Contains(problems, want) {
			t.Errorf("problems %q lack %q", problems, want)
		}
	}
	if n := normalForm(console.PriorityForm{Points: []console.PointField{{Key: "major.7", Value: "x"}}}); len(n.Points) != len(pointFields()) || n.Points[slices.IndexFunc(n.Points, func(p console.PointField) bool { return p.Key == "major.7" })].Value != "x" {
		t.Errorf("normal form %+v", n)
	}
}
