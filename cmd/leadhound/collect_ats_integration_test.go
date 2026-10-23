//go:build integration

package main

import (
	"strings"
	"testing"
)

func TestCollectATSCommandDryRun(t *testing.T) {
	commandDB(t)
	t.Setenv("LEADHOUND_DATA_DIR", "")
	captureNotes(t)
	code, out := runCmd(t, "collect", "ats", "--dry-run", "--fixtures", atsTestdata)
	if code != 0 {
		t.Fatalf("exit %d, stdout %q", code, out)
	}
	for _, name := range []string{"greenhouse", "lever", "ashby"} {
		if !strings.Contains(out, "collect ats: "+name+" run ") {
			t.Errorf("stdout %q, want a line for %s", out, name)
		}
	}
	if strings.Count(out, "stop none") != 3 {
		t.Errorf("stdout %q, want three clean runs", out)
	}
	if got := dbCount(t, `SELECT count(*) FROM job_observation WHERE apply_type = 'ats'`); got == 0 {
		t.Error("no ats observations")
	}
	if got := dbCount(t, `SELECT count(*) FROM job_observation o WHERE NOT EXISTS (SELECT 1 FROM gate_result g WHERE g.observation_id = o.id)`); got != 0 {
		t.Errorf("observations without gate rows = %d, want 0", got)
	}
	// A dry run never reads as an ATS's own data.
	for _, table := range []string{"run", "http_fetch", "job"} {
		if got := dbCount(t, `SELECT count(*) FROM `+table+` WHERE source NOT LIKE '%-fixture'`); got != 0 {
			t.Errorf("%s rows not under a -fixture source = %d, want 0", table, got)
		}
	}
}

// TestCollectATSLiveOffWritesNoRun: with live fetching off, collect ats writes no run row.
func TestCollectATSLiveOffWritesNoRun(t *testing.T) {
	commandDB(t)
	t.Setenv("LEADHOUND_DATA_DIR", "")
	captureNotes(t)
	code, out := runCmd(t, "collect", "ats", "--boards", demoBoardsFile(t))
	if code != 0 || !strings.Contains(out, "live fetching is off here") {
		t.Fatalf("exit %d, stdout %q", code, out)
	}
	if got := dbCount(t, `SELECT count(*) FROM run`); got != 0 {
		t.Errorf("runs = %d, want 0", got)
	}
}

// TestCollectLinkedInUnchanged: collect linkedin still runs as before beside collect ats.
func TestCollectLinkedInUnchanged(t *testing.T) {
	commandDB(t)
	t.Setenv("LEADHOUND_DATA_DIR", "")
	captureNotes(t)
	code, out := runCmd(t, "collect", "linkedin", "--dry-run", "--fixtures", linkedinTestdata)
	if code != 0 || !strings.Contains(out, "collect linkedin: run ") {
		t.Fatalf("exit %d, stdout %q", code, out)
	}
	if got := dbCount(t, `SELECT count(*) FROM run WHERE source <> 'linkedin-fixture'`); got != 0 {
		t.Errorf("non-linkedin runs = %d, want 0", got)
	}
}
