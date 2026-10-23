package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
)

// TestMain replaces the default transport for the whole test binary, so no command under
// test can reach a live site: only loopback may go out.
func TestMain(m *testing.M) {
	http.DefaultTransport = loopbackOnly{next: http.DefaultTransport}
	os.Exit(m.Run())
}

type loopbackOnly struct{ next http.RoundTripper }

func (l loopbackOnly) RoundTrip(r *http.Request) (*http.Response, error) {
	if ip := net.ParseIP(r.URL.Hostname()); r.URL.Hostname() != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return nil, errors.New("test guard: a request to " + r.URL.Hostname() + " left the fixtures")
	}
	return l.next.RoundTrip(r)
}

// captureStdout points stdout at a buffer for the test.
func captureStdout(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	saved := stdout
	stdout = &buf
	t.Cleanup(func() { stdout = saved })
	return &buf
}

// TestLiveOffSkips: without LEADHOUND_LIVE, each LinkedIn command says so and exits 0
// before it touches the database (the DSN points at a closed port, so opening it would
// fail the command).
func TestLiveOffSkips(t *testing.T) {
	t.Setenv("LEADHOUND_DATABASE_URL", "postgres://nobody@127.0.0.1:1/none?sslmode=disable&connect_timeout=1")
	for _, live := range []string{"", "ats"} {
		t.Setenv("LEADHOUND_LIVE", live)
		for _, args := range [][]string{
			{"collect", "linkedin"},
			{"canary", "linkedin"},
			{"probe", "company", "northwind-labs", "--out", t.TempDir() + "/page.html"},
		} {
			out := captureStdout(t)
			var stderr bytes.Buffer
			if code := run(context.Background(), args, &stderr); code != 0 {
				t.Errorf("LEADHOUND_LIVE=%q %v: exit %d, want 0; stderr %s", live, args, code, stderr.String())
			}
			if !strings.Contains(out.String(), "live fetching is off here") {
				t.Errorf("LEADHOUND_LIVE=%q %v: stdout %q, want the live-off message", live, args, out.String())
			}
		}
	}
}

func TestFetchCommandUsage(t *testing.T) {
	t.Setenv("LEADHOUND_DATABASE_URL", "postgres://nobody@127.0.0.1:1/none?sslmode=disable")
	t.Setenv("LEADHOUND_LIVE", "")
	for _, args := range [][]string{
		{"collect"},
		{"collect", "indeed"},
		{"collect", "linkedin", "--dry-run"},
		{"collect", "linkedin", "--fixtures", "x"},
		{"collect", "linkedin", "--bogus"},
		{"canary"},
		{"canary", "linkedin", "extra"},
		{"probe", "company", "northwind-labs"},
		{"probe", "company", "../etc", "--out", "x"},
		{"probe", "person", "x", "--out", "x"},
	} {
		var stderr bytes.Buffer
		if code := run(context.Background(), args, &stderr); code != 2 {
			t.Errorf("%v: exit %d, want 2; stderr %s", args, code, stderr.String())
		}
	}
}

func TestParseArgsInterleaved(t *testing.T) {
	var ff fetchFlags
	fs := newTestFlagSet(&ff)
	pos, err := parseArgs(fs, []string{"company", "--dry-run", "slug", "--fixtures", "dir"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(pos, ",") != "company,slug" || !ff.dryRun || ff.fixtures != "dir" {
		t.Fatalf("pos %v, flags %+v", pos, ff)
	}
}

func newTestFlagSet(ff *fetchFlags) *flag.FlagSet {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	ff.register(fs)
	return fs
}
