package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gremy/webbership-leadhound/internal/candidate"
)

// unitCommands are the command lines the deploy units run (deploy/leadhound-*.service),
// each as a dry run that reaches the database, so a test can point it at one that is not
// there. A recorded stop exiting 0 is proved against a real database in the integration
// tests (TestRecordedStopExitsZero, TestPausedCommandsRecordAndExitZero,
// TestScoreSpendCapExitsZero, TestBriefRecordedStopsExitZero, TestCollectATSRecordedStopExitsZero).
var unitCommands = [][]string{
	{"collect", "ats", "--dry-run", "--fixtures", atsTestdata},
	{"collect", "linkedin", "--dry-run", "--fixtures", "../../internal/linkedin/testdata"},
	{"canary", "linkedin", "--dry-run", "--fixtures", "../../internal/linkedin/testdata"},
	{"score", "--decider", "fake"},
	{"brief", "--outbox", ""}, // the outbox is set per test
}

// TestUnitCommandsCrashExitOne: a command that cannot reach its database is a crash, not a
// recorded outcome, so it exits 1 and the unit's next step still runs (ExecStart=-).
func TestUnitCommandsCrashExitOne(t *testing.T) {
	t.Setenv("LEADHOUND_DATABASE_URL", "postgres://nobody@127.0.0.1:1/none?sslmode=disable&connect_timeout=1")
	t.Setenv("LEADHOUND_DATA_DIR", demoDataDir(t))
	t.Setenv("LEADHOUND_LIVE", "")
	captureNotes(t)
	for _, args := range unitCommands {
		args = withOutbox(t, args)
		t.Run(strings.Join(args[:2], " "), func(t *testing.T) {
			captureStdout(t)
			var stderr bytes.Buffer
			if code := run(context.Background(), args, &stderr); code != 1 {
				t.Errorf("%v: exit %d, want 1; stderr %s", args, code, stderr.String())
			}
		})
	}
}

// TestUnitCommandsBadConfigExitTwo: a variable that cannot be used is bad config: exit 2,
// before anything is fetched or written.
func TestUnitCommandsBadConfigExitTwo(t *testing.T) {
	t.Setenv("LEADHOUND_DATABASE_URL", "postgres://nobody@127.0.0.1:1/none?sslmode=disable&connect_timeout=1")
	t.Setenv("LEADHOUND_DATA_DIR", demoDataDir(t))
	t.Setenv("LEADHOUND_LIVE", "")
	t.Setenv("LEADHOUND_TZ", "Not/AZone")
	captureNotes(t)
	cmds := slices.Concat(unitCommands, [][]string{{"brief", "--send"}})
	for _, args := range cmds {
		args = withOutbox(t, args)
		t.Run(strings.Join(args[:2], " "), func(t *testing.T) {
			captureStdout(t)
			var stderr bytes.Buffer
			if code := run(context.Background(), args, &stderr); code != 2 || !strings.Contains(stderr.String(), "LEADHOUND_TZ") {
				t.Errorf("%v: exit %d, want 2 naming LEADHOUND_TZ; stderr %s", args, code, stderr.String())
			}
		})
	}
}

// TestUnitCommandsIgnoreConsoleSettings: an unusable console address or URL is never bad
// config for a timer command, so each one gets past its config to the database (exit 1
// here, where the database is not there) instead of stopping with exit 2.
func TestUnitCommandsIgnoreConsoleSettings(t *testing.T) {
	t.Setenv("LEADHOUND_DATABASE_URL", "postgres://nobody@127.0.0.1:1/none?sslmode=disable&connect_timeout=1")
	t.Setenv("LEADHOUND_DATA_DIR", demoDataDir(t))
	t.Setenv("LEADHOUND_LIVE", "")
	t.Setenv("LEADHOUND_CONSOLE_ADDR", "192.0.2.10:8090")
	t.Setenv("LEADHOUND_CONSOLE_URL", "nonsense")
	t.Setenv("LEADHOUND_CONSOLE_PASSWORD_HASH", "not-a-hash")
	captureNotes(t)
	for _, args := range unitCommands {
		args = withOutbox(t, args)
		t.Run(strings.Join(args[:2], " "), func(t *testing.T) {
			captureStdout(t)
			var stderr bytes.Buffer
			if code := run(context.Background(), args, &stderr); code != 1 || strings.Contains(stderr.String(), "CONSOLE") {
				t.Errorf("%v: exit %d, want 1; stderr %s", args, code, stderr.String())
			}
		})
	}
}

// demoDataDir is a data directory holding the fictional demo profile as leadhound.yaml.
func demoDataDir(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("../../examples/" + demoProfileFile)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, candidate.FileName), b, 0o600); err != nil { // #nosec G703 -- a test temp dir
		t.Fatal(err)
	}
	return dir
}

// withOutbox fills an empty --outbox value with a temporary directory.
func withOutbox(t *testing.T, args []string) []string {
	t.Helper()
	out := append([]string(nil), args...)
	for i := range out {
		if out[i] == "--outbox" && i+1 < len(out) && out[i+1] == "" {
			out[i+1] = t.TempDir()
		}
	}
	return out
}
