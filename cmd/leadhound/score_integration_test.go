//go:build integration

package main

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/gremy/webbership-leadhound/internal/store/storetest"
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

// TestScoreTitleCheck: with the demo profile's title screen, a title the screen is unsure
// of gets one title-only call (the fake fails it) and no full scoring, a stored title the
// screen drops fails without a call, both rank as a failed criterion, and a second run
// asks nothing.
func TestScoreTitleCheck(t *testing.T) {
	collectFixtures(t)
	ctx := context.Background()
	conn := storetest.OwnerConn(t)
	// The fixtures leave most jobs language-failed; pass two more, so three can be scored.
	if _, err := conn.Exec(ctx, `UPDATE gate_result SET verdict = 'pass' WHERE job_id IN (
		SELECT job_id FROM job_observation WHERE NOT closed ORDER BY job_id LIMIT 3)`); err != nil {
		t.Fatal(err)
	}
	sendableSQL := `SELECT j.id FROM job j WHERE NOT EXISTS (
		SELECT 1 FROM gate_result g WHERE g.observation_id = (
		  SELECT id FROM job_observation o WHERE o.job_id = j.id ORDER BY observed_at DESC, id DESC LIMIT 1)
		AND g.verdict = 'fail') AND EXISTS (SELECT 1 FROM job_observation o WHERE o.job_id = j.id AND NOT o.closed)
		ORDER BY j.id`
	rows, err := conn.Query(ctx, sendableSQL)
	if err != nil {
		t.Fatal(err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) < 3 {
		t.Fatalf("want 3 sendable jobs, have %d", len(ids))
	}
	unsure, drop := ids[0], ids[1]
	for id, title := range map[int64]string{unsure: "Head of Coffee", drop: "Sales Recruiter"} {
		if _, err := conn.Exec(ctx, `UPDATE job_observation SET title = $2 WHERE job_id = $1`, id, title); err != nil {
			t.Fatal(err)
		}
	}
	sendable := len(ids)

	code, out := runCmd(t, "score", "--decider", "fake")
	if code != 0 || !strings.Contains(out, "title check: 1 asked · 2 failed · 0 passed") || !strings.Contains(out, "stop none") {
		t.Fatalf("exit %d, stdout %q", code, out)
	}
	// Full scoring for the kept titles, one title decision each for the other two.
	if got, want := dbCount(t, `SELECT count(*) FROM decision`), 5*(sendable-2)+2; got != want {
		t.Errorf("decisions = %d, want %d", got, want)
	}
	for _, id := range []int64{unsure, drop} {
		if got := dbCount(t, fmt.Sprintf(`SELECT count(*) FROM decision WHERE job_id = %d AND criterion = 'title' AND verdict = 'fail'`, id)); got != 1 {
			t.Errorf("job %d: title fails = %d, want 1", id, got)
		}
	}
	if got := dbCount(t, fmt.Sprintf(`SELECT count(*) FROM decision WHERE job_id = %d AND reason LIKE '%%title-only check%%'`, unsure)); got != 1 {
		t.Errorf("the unsure job's reason does not name the title-only check")
	}
	// One call per kept title, one title-only call, none for the dropped title.
	if got := dbCount(t, `SELECT coalesce(sum(calls), 0) FROM ai_usage WHERE provider = 'fake'`); got != sendable-1 {
		t.Errorf("usage calls = %d, want %d", got, sendable-1)
	}
	if !strings.Contains(out, "criterion 2") || strings.Contains(out, "unscored") {
		t.Errorf("ranking does not count the title fails as failed criteria: %q", out)
	}

	code, out = runCmd(t, "score", "--decider", "fake")
	if code != 0 || !strings.Contains(out, "calls 0") || !strings.Contains(out, "title check: 0 asked · 0 failed · 0 passed") {
		t.Fatalf("second run: exit %d, stdout %q", code, out)
	}
}
