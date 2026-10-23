//go:build integration

package main

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

// scoredATSFixtures collects the ATS fixtures in a dry run, with no LinkedIn run, and
// scores them with the fake, as a dev dry run does.
func scoredATSFixtures(t *testing.T) {
	t.Helper()
	commandDB(t)
	t.Setenv("LEADHOUND_DATA_DIR", "")
	captureNotes(t)
	if code, out := runCmd(t, "collect", "ats", "--dry-run", "--fixtures", atsTestdata); code != 0 {
		t.Fatalf("collect ats: exit %d, %q", code, out)
	}
	t.Setenv("LEADHOUND_DATA_DIR", examplesDir)
	t.Setenv("TYPESAFE_API_KEY", "")
	t.Setenv("LEADHOUND_JEV_DAILY_CALL_CAP", "")
	for _, k := range []string{"RESEND_API_KEY", "LEADHOUND_MAIL_FROM", "LEADHOUND_MAIL_TO"} {
		t.Setenv(k, "")
	}
	if code, out := runCmd(t, "score", "--decider", "fake"); code != 0 {
		t.Fatalf("score: exit %d, %q", code, out)
	}
}

func briefDryRun(t *testing.T) string {
	t.Helper()
	code, out, stderr := runCmdErr(t, "brief", "--allow-fake", "--outbox", filepath.Join(t.TempDir(), "o"))
	if code != 0 {
		t.Fatalf("brief: exit %d, stderr %q", code, stderr)
	}
	return out
}

// healthLine is the brief's "Health: <name> …" line, or "".
func healthLine(out, name string) string {
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "Health: "+name+" ") {
			return l
		}
	}
	return ""
}

// TestBriefATSOnlyDay: a day only the ATS boards ran gives each its own health line with
// its new jobs and no canary, never "no source ran today", and lists its jobs.
func TestBriefATSOnlyDay(t *testing.T) {
	scoredATSFixtures(t)
	out := briefDryRun(t)
	for _, ats := range []string{"greenhouse", "lever", "ashby"} {
		name := strings.ToUpper(ats[:1]) + ats[1:] + " fixtures"
		n := dbCount(t, fmt.Sprintf(`SELECT count(*) FROM job WHERE source = '%s-fixture'`, ats))
		if n == 0 {
			t.Fatalf("no %s fixture jobs", ats)
		}
		if got, want := healthLine(out, name), fmt.Sprintf("Health: %s ok · %d new", name, n); got != want {
			t.Errorf("health line %q, want %q:\n%s", got, want, out)
		}
	}
	for _, bad := range []string{"no source ran today", "Greenhouse fixtures ok · 3 new · canary", "Lever fixtures did not run"} {
		if strings.Contains(out, bad) {
			t.Errorf("stdout has %q:\n%s", bad, out)
		}
	}
	if !strings.Contains(out, "Health: LinkedIn did not run") || !strings.Contains(out, "\nApply\n1. ") {
		t.Errorf("want LinkedIn's not-run line and listed ATS jobs:\n%s", out)
	}
}

// TestBriefLeavesOutATSJobNotSeen: an ATS job last seen on its board 3 days ago is not
// listed, while one seen today is; its listing stays open and its source's health line
// counts it, even on a day that source did not run.
func TestBriefLeavesOutATSJobNotSeen(t *testing.T) {
	scoredATSFixtures(t)
	out := briefDryRun(t)
	ctx := context.Background()
	conn := storetest.OwnerConn(t)
	rows, err := conn.Query(ctx, `SELECT id, source, url FROM job WHERE source LIKE '%-fixture' ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	type job struct {
		id          int64
		source, url string
	}
	var listed []job
	for rows.Next() {
		var j job
		if err := rows.Scan(&j.id, &j.source, &j.url); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(out, "Link: "+j.url+"\n") {
			listed = append(listed, j)
		}
	}
	rows.Close()
	if len(listed) < 2 {
		t.Fatalf("listed ATS jobs = %d, want at least 2:\n%s", len(listed), out)
	}
	stale, fresh := listed[0], listed[1]
	if _, err := conn.Exec(ctx, `UPDATE job SET last_seen_at = now() - interval '3 days' WHERE id = $1`, stale.id); err != nil {
		t.Fatal(err)
	}
	// The stale job's source did not run today: its other jobs are seen today all the same.
	if _, err := conn.Exec(ctx, `UPDATE run SET started_at = now() - interval '3 days', finished_at = now() - interval '3 days' WHERE source = $1`, stale.source); err != nil {
		t.Fatal(err)
	}

	out = briefDryRun(t)
	if strings.Contains(out, "Link: "+stale.url+"\n") {
		t.Errorf("the job not seen for 3 days is listed:\n%s", out)
	}
	if !strings.Contains(out, "Link: "+fresh.url+"\n") {
		t.Errorf("the job seen today is not listed:\n%s", out)
	}
	name := map[string]string{"greenhouse-fixture": "Greenhouse fixtures", "lever-fixture": "Lever fixtures", "ashby-fixture": "Ashby fixtures"}[stale.source]
	if got, want := healthLine(out, name), "Health: "+name+" did not run · 1 job left out, not seen on its board for 2 days"; got != want {
		t.Errorf("health line %q, want %q:\n%s", got, want, out)
	}
	if strings.Contains(out, "Nothing new worth applying") || strings.Contains(out, "nothing new") {
		t.Errorf("a day with a job left out reads as a quiet day:\n%s", out)
	}
	if got := dbCount(t, fmt.Sprintf(`SELECT count(*) FROM job WHERE id = %d AND listing_status = 'open'`, stale.id)); got != 1 {
		t.Errorf("the job left out is no longer open")
	}

	// Seen again on its board, it is listed again.
	if _, err := conn.Exec(ctx, `UPDATE job SET last_seen_at = now() WHERE id = $1`, stale.id); err != nil {
		t.Fatal(err)
	}
	if out = briefDryRun(t); !strings.Contains(out, "Link: "+stale.url+"\n") || strings.Contains(out, "not seen on") {
		t.Errorf("the job seen again is not listed:\n%s", out)
	}
}

// TestBriefBrokenATSRun: an ATS run that broke, was paused, failed or stopped gets a health
// line naming it, with no canary, and the day is never "nothing new".
func TestBriefBrokenATSRun(t *testing.T) {
	commandDB(t)
	t.Setenv("LEADHOUND_DATA_DIR", examplesDir)
	if _, err := storetest.OwnerConn(t).Exec(context.Background(),
		`INSERT INTO run (command, source, finished_at, stop_reason, note) VALUES
		   ('collect', 'linkedin', now(), '', '10 cards'),
		   ('canary', 'linkedin', now(), '', '10 cards'),
		   ('collect', 'greenhouse', now(), 'parser_broke', 'greenhouse: 2 boards (0 ok, 0 failed, 2 parser broke, 0 not found)'),
		   ('collect', 'lever', now(), 'throttled', 'lever: paused today: throttled'),
		   ('collect', 'ashby', now(), 'failure_rate', 'ashby: 4 boards (1 ok, 3 failed, 0 parser broke, 0 not found); stopped: failure_rate'),
		   ('collect', 'greenhouse-fixture', now(), '', 'greenhouse: failed: boom'),
		   ('collect', 'lever-fixture', now(), 'budget', 'lever: 6 boards; stopped: budget'),
		   ('score', '', now(), '', '')`); err != nil {
		t.Fatal(err)
	}
	code, out, stderr := runCmdErr(t, "brief", "--outbox", filepath.Join(t.TempDir(), "o"))
	if code != 0 {
		t.Fatalf("exit %d, %q", code, stderr)
	}
	for _, want := range []string{
		"Health: LinkedIn ok · 0 new · canary ok\n",
		"Health: Greenhouse parser broke: no Greenhouse jobs today until it is fixed\n",
		"Health: Lever paused today (throttled)\n",
		"Health: Ashby did not finish (failure rate)\n",
		"Health: Greenhouse fixtures did not finish (failed)\n",
		"Health: Lever fixtures did not finish (budget)\n",
		"nothing listed, see health",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Ashby fixtures") || strings.Contains(out, "Nothing new worth applying") {
		t.Errorf("a source that did not run is shown, or a broken board reads as a quiet day:\n%s", out)
	}
}
