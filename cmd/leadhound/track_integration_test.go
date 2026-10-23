//go:build integration

package main

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

// sentBrief scores the four-job fixtures, sends today's brief through a fake mailer and
// returns the job ids it listed, by number.
func sentBrief(t *testing.T) []int64 {
	t.Helper()
	scoredFixtures(t)
	useMailer(t, &fakeMailer{})
	if code, _, stderr := runCmdErr(t, "brief", "--send", "--allow-fake"); code != 0 {
		t.Fatalf("brief --send: exit %d, %q", code, stderr)
	}
	var ids []int64
	if err := storetest.OwnerConn(t).QueryRow(context.Background(), `SELECT job_ids FROM brief_log`).Scan(&ids); err != nil {
		t.Fatal(err)
	}
	if len(ids) != 4 {
		t.Fatalf("listed = %v, want 4", ids)
	}
	return ids
}

func currentStatus(t *testing.T, jobID int64) (status, reason, actor string) {
	t.Helper()
	err := storetest.OwnerConn(t).QueryRow(context.Background(),
		`SELECT status, reason, actor FROM status_event WHERE job_id = $1 ORDER BY at DESC, id DESC LIMIT 1`, jobID).
		Scan(&status, &reason, &actor)
	if err != nil {
		t.Fatal(err)
	}
	return status, reason, actor
}

func events(t *testing.T) int { return dbCount(t, `SELECT count(*) FROM status_event`) }

// TestTrackResolvesNumber: "applied 1" names the brief's first job before writing, and
// writes applied for it.
func TestTrackResolvesNumber(t *testing.T) {
	ids := sentBrief(t)
	code, out, stderr := runCmdErr(t, "track", "1", "applied", "--reason", "via their site")
	if code != 0 {
		t.Fatalf("exit %d, %q", code, stderr)
	}
	var company string
	if err := storetest.OwnerConn(t).QueryRow(context.Background(),
		`SELECT c.name FROM job j JOIN company c ON c.id = j.company_id WHERE j.id = $1`, ids[0]).Scan(&company); err != nil {
		t.Fatal(err)
	}
	if want := fmt.Sprintf("1 → Senior Platform Engineer 4 · %s (job:%d)\n", company, ids[0]); !strings.HasPrefix(out, want) {
		t.Errorf("stdout = %q, want it to start with %q", out, want)
	}
	if st, reason, actor := currentStatus(t, ids[0]); st != "applied" || reason != "via their site" || actor != "gremy" {
		t.Errorf("status = %s %q %s", st, reason, actor)
	}
	// job:ID and --day reach the same jobs.
	if code, out, _ := runCmdErr(t, "track", fmt.Sprintf("job:%d", ids[1]), "interviewing"); code != 0 || !strings.Contains(out, "shortlisted → interviewing") {
		t.Errorf("job:ID: exit %d, %q", code, out)
	}
	if code, out, _ := runCmdErr(t, "track", "3", "rejected", "--day", today()); code != 0 || !strings.Contains(out, fmt.Sprintf("(job:%d)", ids[2])) {
		t.Errorf("--day: exit %d, %q", code, out)
	}
}

// TestTrackErrors: a number past the brief, an unknown job, a day with no brief and no
// sent brief at all are clear usage errors that write nothing.
func TestTrackErrors(t *testing.T) {
	commandDB(t)
	if code, _, stderr := runCmdErr(t, "track", "1", "applied"); code != 2 || !strings.Contains(stderr, "no brief has been sent yet") {
		t.Fatalf("no brief: exit %d, %q", code, stderr)
	}
	sentBrief(t)
	before := events(t)
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"track", "7", "applied"}, "listed 4 jobs; there is no number 7"},
		{[]string{"track", "job:999", "applied"}, "job:999 does not exist"},
		{[]string{"track", "1", "applied", "--day", "2020-01-01"}, "there is no brief for 2020-01-01"},
		{[]string{"track", "1", "married"}, "usage: leadhound track"},
		{[]string{"track", "1", "applied", "--rule", "title=x"}, "--rule goes with skip only"},
		{[]string{"track", "1", "skip", "--rule", "salary=x"}, "--rule must be field=PATTERN"},
		{[]string{"track", "1", "skip", "--rule", "title=(unclosed"}, "does not compile"},
	} {
		code, _, stderr := runCmdErr(t, c.args...)
		if code != 2 || !strings.Contains(stderr, c.want) {
			t.Errorf("%v: exit %d, stderr %q, want %q", c.args, code, stderr, c.want)
		}
	}
	if got := events(t); got != before {
		t.Errorf("status events = %d, want %d", got, before)
	}
	if got := dbCount(t, `SELECT count(*) FROM rule`); got != 0 {
		t.Errorf("rules = %d, want 0", got)
	}
}

// TestTrackGrid: the grid's track-skip column, plus maybe and --rule.
func TestTrackGrid(t *testing.T) {
	ids := sentBrief(t)

	// skip on applied is refused.
	if code, _, _ := runCmdErr(t, "track", "1", "applied"); code != 0 {
		t.Fatal("applied failed")
	}
	before := events(t)
	if code, _, stderr := runCmdErr(t, "track", "1", "skip", "--reason", "changed my mind"); code != 2 || !strings.Contains(stderr, "already applied; use withdrawn") {
		t.Errorf("skip on applied: exit %d, %q", code, stderr)
	}
	if events(t) != before {
		t.Error("a refused skip wrote an event")
	}

	// skip, then skip again: the second is a no-op that says so.
	if code, out, _ := runCmdErr(t, "track", "2", "skip", "--reason", "French only"); code != 0 || !strings.Contains(out, "shortlisted → skipped") {
		t.Errorf("skip: exit %d, %q", code, out)
	}
	before = events(t)
	if code, out, _ := runCmdErr(t, "track", "2", "skip"); code != 0 || !strings.Contains(out, "already skipped; nothing changed") {
		t.Errorf("skip on skipped: exit %d, %q", code, out)
	}
	if events(t) != before {
		t.Error("a no-op skip wrote an event")
	}

	// skip on an expired listing is allowed; --rule adds a rule made from that job.
	if _, err := storetest.OwnerConn(t).Exec(context.Background(), `UPDATE job SET listing_status = 'expired' WHERE id = $1`, ids[2]); err != nil {
		t.Fatal(err)
	}
	code, out, stderr := runCmdErr(t, "track", "3", "skip", "--reason", "agency", "--rule", "company=^tailspin")
	if code != 0 || !strings.Contains(out, "shortlisted → skipped; rule 1 added") {
		t.Fatalf("skip with rule: exit %d, %q %q", code, out, stderr)
	}
	if got := dbCount(t, fmt.Sprintf(`SELECT count(*) FROM rule WHERE kind = 'skip' AND field = 'company' AND pattern = '^tailspin' AND reason = 'agency' AND created_from_job_id = %d`, ids[2])); got != 1 {
		t.Errorf("rule rows = %d, want 1", got)
	}
	if got := dbCount(t, fmt.Sprintf(`SELECT count(*) FROM status_event WHERE job_id = %d AND status = 'skipped' AND rule_id = 1`, ids[2])); got != 1 {
		t.Errorf("skip event with rule = %d, want 1", got)
	}

	// maybe keeps the status and writes the reason.
	if code, out, _ := runCmdErr(t, "track", "4", "maybe", "--reason", "unsure about the stack"); code != 0 || !strings.Contains(out, "status stays shortlisted") {
		t.Errorf("maybe: exit %d, %q", code, out)
	}
	if st, reason, _ := currentStatus(t, ids[3]); st != "shortlisted" || reason != "maybe: unsure about the stack" {
		t.Errorf("after maybe: %s %q", st, reason)
	}

	// The tracker lists them, latest first; --status filters.
	code, out, _ = runCmdErr(t, "track", "list")
	if code != 0 || strings.Count(out, "\n") != 4 || !strings.Contains(out, "\tskipped\t") || !strings.Contains(out, "\tapplied\t") {
		t.Errorf("track list: exit %d\n%s", code, out)
	}
	if code, out, _ := runCmdErr(t, "track", "list", "--status", "applied"); code != 0 || strings.Count(out, "\n") != 1 || !strings.Contains(out, fmt.Sprintf("job:%d\t", ids[0])) {
		t.Errorf("track list --status applied: exit %d\n%s", code, out)
	}
	if code, _, stderr := runCmdErr(t, "track", "list", "--status", "dreaming"); code != 2 || !strings.Contains(stderr, "--status must be") {
		t.Errorf("bad status filter: exit %d, %q", code, stderr)
	}
}
