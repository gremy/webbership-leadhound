package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gremy/webbership-leadhound/internal/candidate"
	"github.com/gremy/webbership-leadhound/internal/linkedin"
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

// captureNotes points the notes writer at a buffer for the test.
func captureNotes(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	saved := notes
	notes = &buf
	t.Cleanup(func() { notes = saved })
	return &buf
}

// writeProfile writes the demo profile, with old replaced by new, as leadhound.yaml in a
// fresh data directory.
func writeProfile(t *testing.T, old, new string) string {
	t.Helper()
	b, err := os.ReadFile("../../examples/leadhound.demo.yaml")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, candidate.FileName), []byte(strings.Replace(string(b), old, new, 1)), 0o600); err != nil { // #nosec G703 -- a test temp dir
		t.Fatal(err)
	}
	return dir
}

// TestCandidateProfileWiring: without LEADHOUND_DATA_DIR each LinkedIn command prints one
// note and runs on the demo; with it, a broken profile is a usage error naming the problem,
// before anything else happens.
func TestCandidateProfileWiring(t *testing.T) {
	t.Setenv("LEADHOUND_DATABASE_URL", "postgres://nobody@127.0.0.1:1/none?sslmode=disable&connect_timeout=1")
	t.Setenv("LEADHOUND_LIVE", "")
	for _, cmd := range []string{"collect", "canary"} {
		t.Run(cmd+" unset", func(t *testing.T) {
			t.Setenv("LEADHOUND_DATA_DIR", "")
			captureStdout(t)
			n := captureNotes(t)
			var stderr bytes.Buffer
			if code := run(context.Background(), []string{cmd, "linkedin"}, &stderr); code != 0 {
				t.Fatalf("exit %d; stderr %s", code, stderr.String())
			}
			if got := n.String(); strings.Count(got, "\n") != 1 || !strings.Contains(got, "LEADHOUND_DATA_DIR is not set") {
				t.Errorf("notes %q, want one line about LEADHOUND_DATA_DIR", got)
			}
		})
		t.Run(cmd+" valid", func(t *testing.T) {
			t.Setenv("LEADHOUND_DATA_DIR", writeProfile(t, "", ""))
			out := captureStdout(t)
			n := captureNotes(t)
			var stderr bytes.Buffer
			if code := run(context.Background(), []string{cmd, "linkedin"}, &stderr); code != 0 {
				t.Fatalf("exit %d; stderr %s", code, stderr.String())
			}
			if n.Len() != 0 || !strings.Contains(out.String(), "live fetching is off here") {
				t.Errorf("notes %q, stdout %q", n.String(), out.String())
			}
		})
		t.Run(cmd+" broken", func(t *testing.T) {
			t.Setenv("LEADHOUND_DATA_DIR", writeProfile(t, "confidence_threshold: 0.7", "confidence_threshold: 3"))
			captureStdout(t)
			captureNotes(t)
			var stderr bytes.Buffer
			if code := run(context.Background(), []string{cmd, "linkedin"}, &stderr); code != 2 {
				t.Fatalf("exit %d, want 2", code)
			}
			if !strings.Contains(stderr.String(), "leadhound.yaml") || !strings.Contains(stderr.String(), "confidence_threshold") {
				t.Errorf("stderr %q, want the file and the problem", stderr.String())
			}
		})
	}
}

func TestCollectMatrix(t *testing.T) {
	prof, err := candidate.LoadFile("../../examples/leadhound.demo.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if m := collectMatrix(nil, nil, nil, 0); !slices.Equal(m.Keywords, linkedin.DemoMatrix().Keywords) {
		t.Errorf("no profile: %+v, want the demo matrix", m)
	}
	m := collectMatrix(&prof, nil, nil, 0)
	if !slices.Equal(m.Keywords, prof.LinkedIn.Keywords) || !slices.Equal(m.Locations, prof.LinkedIn.Locations) || m.MaxPages != prof.LinkedIn.MaxPages {
		t.Errorf("profile: %+v", m)
	}
	m = collectMatrix(&prof, []string{"k"}, []string{"l"}, 7)
	if !slices.Equal(m.Keywords, []string{"k"}) || !slices.Equal(m.Locations, []string{"l"}) || m.MaxPages != 7 {
		t.Errorf("flags: %+v, want the flags to win", m)
	}
}

func TestCanaryQueryChoice(t *testing.T) {
	prof, err := candidate.LoadFile("../../examples/leadhound.demo.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if k, l := canaryQuery(nil, "", ""); k != defaultCanaryKeywords || l != defaultCanaryLocation {
		t.Errorf("no profile: %q, %q", k, l)
	}
	pk, pl := prof.CanaryQuery()
	if k, l := canaryQuery(&prof, "", ""); k != pk || l != pl {
		t.Errorf("profile: %q, %q", k, l)
	}
	if k, l := canaryQuery(&prof, "k", "l"); k != "k" || l != "l" {
		t.Errorf("flags: %q, %q, want the flags to win", k, l)
	}
}
