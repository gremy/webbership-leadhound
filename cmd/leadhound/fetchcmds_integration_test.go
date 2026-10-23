//go:build integration

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gremy/webbership-leadhound/internal/fetch"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

const linkedinTestdata = "../../internal/linkedin/testdata"

// commandDB empties the test database and points the commands at it as the app role,
// with live fetching off.
func commandDB(t *testing.T) {
	t.Helper()
	storetest.ResetDB(t)
	t.Setenv("LEADHOUND_DATABASE_URL", storetest.AppDSN())
	t.Setenv("LEADHOUND_LIVE", "")
}

// fixtureDir copies the LinkedIn fixtures into a temp dir, with overrides applied to the
// shipped manifest.
func fixtureDir(t *testing.T, overrides map[string]string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(linkedinTestdata, fetch.ManifestFile))
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]string{}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	for k, v := range overrides {
		m[k] = v
	}
	dir := t.TempDir()
	for _, file := range m {
		b, err := os.ReadFile(filepath.Join(linkedinTestdata, file))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, file), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if raw, err = json.Marshal(m); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, fetch.ManifestFile), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func dbCount(t *testing.T, q string) int {
	t.Helper()
	var n int
	if err := storetest.OwnerConn(t).QueryRow(context.Background(), q).Scan(&n); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return n
}

func runCmd(t *testing.T, args ...string) (int, string) {
	t.Helper()
	out := captureStdout(t)
	var stderr bytes.Buffer
	code := run(context.Background(), args, &stderr)
	if code != 0 {
		t.Logf("stderr: %s", stderr.String())
	}
	return code, out.String()
}

func TestCollectCommandDryRun(t *testing.T) {
	commandDB(t)
	code, out := runCmd(t, "collect", "linkedin", "--dry-run", "--fixtures", linkedinTestdata)
	if code != 0 || !strings.Contains(out, "stop none") {
		t.Fatalf("exit %d, stdout %q", code, out)
	}
	if got := dbCount(t, `SELECT count(*) FROM job_observation`); got != 7 {
		t.Errorf("observations = %d, want 7", got)
	}
	// A dry run never reads as LinkedIn's data.
	for _, table := range []string{"run", "http_fetch", "job"} {
		if got := dbCount(t, `SELECT count(*) FROM `+table+` WHERE source <> 'linkedin-fixture'`); got != 0 {
			t.Errorf("%s rows not under linkedin-fixture = %d, want 0", table, got)
		}
	}
}

// TestPausedCommandsRecordAndExitZero: once the source is paused today, canary and probe
// record a run with the pausing reason, request nothing and exit 0.
func TestPausedCommandsRecordAndExitZero(t *testing.T) {
	commandDB(t)
	conn := storetest.OwnerConn(t)
	if _, err := conn.Exec(context.Background(),
		`INSERT INTO run (command, source, finished_at, requests, stop_reason) VALUES ('collect', 'linkedin-fixture', now(), 2, 'throttled')`); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"canary", "linkedin", "--dry-run", "--fixtures", linkedinTestdata},
		{"probe", "company", "northwind-labs", "--out", filepath.Join(t.TempDir(), "c.html"), "--dry-run", "--fixtures", linkedinTestdata},
		{"collect", "linkedin", "--dry-run", "--fixtures", linkedinTestdata},
	} {
		code, out := runCmd(t, args...)
		if code != 0 || !strings.Contains(out, "paused today: throttled") {
			t.Errorf("%v: exit %d, stdout %q", args, code, out)
		}
	}
	if got := dbCount(t, `SELECT count(*) FROM run WHERE stop_reason = 'throttled' AND note = 'paused today: throttled' AND requests = 0`); got != 3 {
		t.Errorf("paused runs = %d, want 3", got)
	}
	if got := dbCount(t, `SELECT count(*) FROM http_fetch`); got != 0 {
		t.Errorf("fetch rows = %d, want 0", got)
	}
}

// TestRecordedStopExitsZero: a run that recorded a stop is an outcome, not a crash.
func TestRecordedStopExitsZero(t *testing.T) {
	broken := map[string]string{"*seeMoreJobPostings/search?*&start=0*": "search_broken.html"}

	t.Run("collect parser_broke", func(t *testing.T) {
		commandDB(t)
		code, out := runCmd(t, "collect", "linkedin", "--dry-run", "--fixtures", fixtureDir(t, broken))
		if code != 0 || !strings.Contains(out, "stop parser_broke") {
			t.Fatalf("exit %d, stdout %q", code, out)
		}
		if got := dbCount(t, `SELECT count(*) FROM run WHERE command = 'collect' AND stop_reason = 'parser_broke'`); got != 1 {
			t.Errorf("parser_broke runs = %d, want 1", got)
		}
	})

	t.Run("collect budget", func(t *testing.T) {
		commandDB(t)
		t.Setenv("LEADHOUND_LINKEDIN_SEARCH_BUDGET", "0")
		code, out := runCmd(t, "collect", "linkedin", "--dry-run", "--fixtures", linkedinTestdata)
		if code != 0 || !strings.Contains(out, "stop budget") {
			t.Fatalf("exit %d, stdout %q", code, out)
		}
	})

	t.Run("canary parser_broke, then skip", func(t *testing.T) {
		commandDB(t)
		dir := fixtureDir(t, broken)
		code, out := runCmd(t, "canary", "linkedin", "--dry-run", "--fixtures", dir)
		if code != 0 || !strings.Contains(out, "stop parser_broke") {
			t.Fatalf("exit %d, stdout %q", code, out)
		}
		code, out = runCmd(t, "canary", "linkedin", "--dry-run", "--fixtures", dir)
		if code != 0 || !strings.Contains(out, "already ran today") {
			t.Fatalf("second canary: exit %d, stdout %q", code, out)
		}
	})

	t.Run("probe 404", func(t *testing.T) {
		commandDB(t)
		// No manifest entry for the company page: a 404 is recorded, the body saved.
		path := filepath.Join(t.TempDir(), "company.html")
		code, out := runCmd(t, "probe", "company", "northwind-labs", "--out", path, "--dry-run", "--fixtures", linkedinTestdata)
		if code != 0 || !strings.Contains(out, "status 404") {
			t.Fatalf("exit %d, stdout %q", code, out)
		}
	})
}

func TestProbeCompanyDryRun(t *testing.T) {
	commandDB(t)
	dir := fixtureDir(t, map[string]string{"*/company/northwind-labs": "search_empty.html"})
	path := filepath.Join(t.TempDir(), "company.html")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil { //nolint:gosec // proving the mode is tightened
		t.Fatal(err)
	}
	code, out := runCmd(t, "probe", "company", "northwind-labs", "--out", path, "--dry-run", "--fixtures", dir)
	if code != 0 || !strings.Contains(out, "status 200") {
		t.Fatalf("exit %d, stdout %q", code, out)
	}
	want, err := os.ReadFile(filepath.Join(linkedinTestdata, "search_empty.html"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("saved body differs from the response")
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v (%v), want 0600", fi.Mode().Perm(), err)
	}
	if got := dbCount(t, `SELECT count(*) FROM run WHERE command = 'probe' AND source = 'linkedin-fixture' AND requests = 1`); got != 1 {
		t.Errorf("probe runs = %d, want 1", got)
	}
	if got := dbCount(t, `SELECT count(*) FROM http_fetch WHERE kind = 'company' AND url = 'https://www.linkedin.com/company/northwind-labs'`); got != 1 {
		t.Errorf("company fetch rows = %d, want 1", got)
	}
	for _, table := range []string{"company", "job", "job_observation", "status_event"} {
		if got := dbCount(t, `SELECT count(*) FROM `+table); got != 0 {
			t.Errorf("%s rows = %d, want 0", table, got)
		}
	}
}
