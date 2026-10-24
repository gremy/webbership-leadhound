//go:build integration

package main

import (
	"context"
	"strings"
	"testing"

	"github.com/gremy/webbership-leadhound/internal/ats"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

func TestCollectATSCommandDryRun(t *testing.T) {
	commandDB(t)
	t.Setenv("LEADHOUND_DATA_DIR", "")
	captureNotes(t)
	code, out := runCmd(t, "collect", "ats", "--dry-run", "--fixtures", atsTestdata)
	if code != 0 {
		t.Fatalf("exit %d, stdout %q", code, out)
	}
	for _, name := range ats.Names {
		if !strings.Contains(out, "collect ats: "+name+" run ") {
			t.Errorf("stdout %q, want a line for %s", out, name)
		}
	}
	if strings.Count(out, "stop none") != len(ats.Names) {
		t.Errorf("stdout %q, want a clean run per ATS", out)
	}
	// The new ATSs' demo boards: every posting stored and observed under its fixture source,
	// the SmartRecruiters one with its detail where the fixtures hold one.
	for src, want := range map[string]int{"smartrecruiters-fixture": 5, "teamtailor-fixture": 5, "personio-fixture": 4} {
		if got := dbCount(t, `SELECT count(*) FROM job j WHERE j.source = '`+src+`' AND EXISTS (SELECT 1 FROM job_observation o WHERE o.job_id = j.id)`); got != want {
			t.Errorf("%s observed jobs = %d, want %d", src, got, want)
		}
	}
	if !strings.Contains(out, "details: 1 fetched, 4 failed, 0 left for a later run") {
		t.Errorf("stdout %q, want the SmartRecruiters detail line", out)
	}
	if got := dbCount(t, `SELECT count(*) FROM job_observation o JOIN http_fetch f ON f.id = o.fetch_id WHERE f.kind = 'detail' AND o.description_clean <> ''`); got != 1 {
		t.Errorf("observations from a detail = %d, want 1", got)
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

// TestCollectATSRecordedStopExitsZero: a stop collect ats records is an outcome, not a
// crash: the nightly unit's score step runs after it either way, but exit 0 keeps the unit
// green. Each ATS paused today (an earlier throttled run) records its pause and exits 0.
func TestCollectATSRecordedStopExitsZero(t *testing.T) {
	commandDB(t)
	t.Setenv("LEADHOUND_DATA_DIR", "")
	captureNotes(t)
	for _, name := range ats.Names {
		if _, err := storetest.OwnerConn(t).Exec(context.Background(),
			`INSERT INTO run (command, source, finished_at, requests, stop_reason) VALUES ('collect', $1, now(), 2, 'throttled')`,
			ats.SourceFixture(name)); err != nil {
			t.Fatal(err)
		}
	}
	code, out := runCmd(t, "collect", "ats", "--dry-run", "--fixtures", atsTestdata)
	if code != 0 {
		t.Fatalf("exit %d, stdout %q", code, out)
	}
	if got := strings.Count(out, "paused today: throttled"); got != len(ats.Names) {
		t.Errorf("stdout %q, want every ATS paused", out)
	}
	if got := dbCount(t, `SELECT count(*) FROM http_fetch`); got != 0 {
		t.Errorf("fetch rows = %d, want 0", got)
	}
}
