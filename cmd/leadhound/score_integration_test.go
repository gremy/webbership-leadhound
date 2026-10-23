//go:build integration

package main

import (
	"strings"
	"testing"
)

// collectFixtures fills the test database from the LinkedIn fixtures with the neutral
// gates, as a dev dry run does.
func collectFixtures(t *testing.T) {
	t.Helper()
	commandDB(t)
	t.Setenv("LEADHOUND_DATA_DIR", "")
	if code, out := runCmd(t, "collect", "linkedin", "--dry-run", "--fixtures", linkedinTestdata); code != 0 {
		t.Fatalf("collect: exit %d, %q", code, out)
	}
	t.Setenv("LEADHOUND_DATA_DIR", examplesDir)
	t.Setenv("TYPESAFE_API_KEY", "")
	t.Setenv("LEADHOUND_JEV_DAILY_CALL_CAP", "")
}

// TestScoreCommandFake: the fake decider scores every job whose gates did not fail, five
// decisions each, never sends a gate-failed job, and costs nothing on a second run.
func TestScoreCommandFake(t *testing.T) {
	collectFixtures(t)
	sendable := dbCount(t, `SELECT count(*) FROM job j WHERE NOT EXISTS (
		SELECT 1 FROM gate_result g WHERE g.observation_id = (
		  SELECT id FROM job_observation o WHERE o.job_id = j.id ORDER BY observed_at DESC, id DESC LIMIT 1)
		AND g.verdict = 'fail') AND EXISTS (SELECT 1 FROM job_observation o WHERE o.job_id = j.id AND NOT o.closed)`)
	if sendable == 0 {
		t.Fatal("the fixtures give no job to score; the test proves nothing")
	}

	code, out := runCmd(t, "score", "--decider", "fake")
	if code != 0 || !strings.Contains(out, "stop none") || !strings.Contains(out, "decider fake") {
		t.Fatalf("exit %d, stdout %q", code, out)
	}
	if got := dbCount(t, `SELECT count(*) FROM decision`); got != 5*sendable {
		t.Errorf("decisions = %d, want %d", got, 5*sendable)
	}
	if got := dbCount(t, `SELECT count(DISTINCT job_id) FROM decision d WHERE EXISTS (
		SELECT 1 FROM gate_result g WHERE g.job_id = d.job_id AND g.verdict = 'fail')`); got != 0 {
		t.Errorf("gate-failed jobs with decisions = %d, want 0", got)
	}
	if got := dbCount(t, `SELECT count(*) FROM ai_usage WHERE provider = 'fake'`); got != sendable {
		t.Errorf("usage rows = %d, want %d", got, sendable)
	}
	if !strings.Contains(out, "ranked: apply ") {
		t.Errorf("no ranking line: %q", out)
	}

	code, out = runCmd(t, "score", "--decider", "fake")
	if code != 0 || !strings.Contains(out, "scored 0 · unchanged ") || !strings.Contains(out, "calls 0") {
		t.Fatalf("second run: exit %d, stdout %q", code, out)
	}
	if got := dbCount(t, `SELECT count(*) FROM decision`); got != 5*sendable {
		t.Errorf("decisions after the second run = %d, want %d", got, 5*sendable)
	}
	if got := dbCount(t, `SELECT count(*) FROM run WHERE command = 'score' AND finished_at IS NOT NULL AND stop_reason = ''`); got != 2 {
		t.Errorf("finished score runs = %d, want 2", got)
	}
}

// TestScoreSpendCapExitsZero: at the daily cap the run is recorded as spend_cap and the
// command exits 0 without a call.
func TestScoreSpendCapExitsZero(t *testing.T) {
	collectFixtures(t)
	t.Setenv("LEADHOUND_JEV_DAILY_CALL_CAP", "0")
	code, out := runCmd(t, "score", "--decider", "fake")
	if code != 0 || !strings.Contains(out, "stop spend_cap") {
		t.Fatalf("exit %d, stdout %q", code, out)
	}
	if got := dbCount(t, `SELECT count(*) FROM run WHERE command = 'score' AND stop_reason = 'spend_cap' AND finished_at IS NOT NULL AND requests = 0`); got != 1 {
		t.Errorf("spend_cap runs = %d, want 1", got)
	}
	if got := dbCount(t, `SELECT count(*) FROM decision`); got != 0 {
		t.Errorf("decisions = %d, want 0", got)
	}
}
