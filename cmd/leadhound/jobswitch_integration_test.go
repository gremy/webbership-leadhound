//go:build integration

package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/console"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/leadstore/leadstoretest"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

// switchTableCounts is the row count of every table in the database but run (the one a switched
// off job writes), read as the owner.
func switchTableCounts(t *testing.T) map[string]int {
	t.Helper()
	ctx := context.Background()
	conn := storetest.OwnerConn(t)
	rows, err := conn.Query(ctx, `SELECT table_name FROM information_schema.tables
		WHERE table_schema = 'public' AND table_type = 'BASE TABLE' AND table_name <> 'run' ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		names = append(names, n)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(names) < 30 {
		t.Fatalf("only %d tables found: %v", len(names), names)
	}
	out := map[string]int{}
	for _, n := range names {
		var c int
		if err := conn.QueryRow(ctx, `SELECT count(*) FROM "`+n+`"`).Scan(&c); err != nil {
			t.Fatal(err)
		}
		out[n] = c
	}
	return out
}

func runCount(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := storetest.AppConn(t).QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestJobOffEveryCommand: each gated command, its job switched off, exits 0 with the one
// line, records exactly one finished run of its own command with the note "switched off",
// and changes no other table.
func TestJobOffEveryCommand(t *testing.T) {
	cases := []struct {
		job, name, command string
		args               []string
		live               string
	}{
		{leadstore.JobBatch, "Morning batch", commandLeadsBatch, []string{"leads", "batch", "new"}, ""},
		{leadstore.JobSync, "Inbox sync", commandLeadsSync, []string{"leads", "sync"}, ""},
		{leadstore.JobProbe, "Site probe", commandLeadsSitesProbe, []string{"leads", "sites", "probe", "--dry-run", "--fixtures", sitesFixtureDir}, ""},
		{leadstore.JobContacts, "Contacts", commandLeadsContacts, []string{"leads", "contacts", "--dry-run", "--fixtures", sitesFixtureDir}, ""},
		{leadstore.JobContacts, "Contacts", commandLeadsEnrich, []string{"leads", "enrich", "--dry-run", "--fixtures", sitesFixtureDir}, ""},
		{leadstore.JobContacts, "Contacts", commandLeadsRate, []string{"leads", "rate", "--decider", "fake"}, ""},
		{leadstore.JobResearch, "Research", commandLeadsResearch, []string{"leads", "research", "run"}, "1"},
	}
	for _, c := range cases {
		t.Run(c.command, func(t *testing.T) {
			leadsDB(t)
			t.Setenv("LEADHOUND_LIVE", c.live)
			ctx := context.Background()
			lq := leadstoretest.Q(t)
			// Something for each job to do, were it on: sites to probe, a mailbox to sync.
			sitesOut(t, append([]string{"leads", "sites", "import", "manual"}, sitesFixtureDomains...)...)
			seedMailbox(t, lq, testUser)
			if _, err := lq.SetJobSwitch(ctx, 0, c.job, false, "testing", leadstore.ActorGremy); err != nil {
				t.Fatal(err)
			}
			runsBefore := runCount(t, `SELECT count(*) FROM run`)
			before := switchTableCounts(t)

			code, out, errOut := leadsRun(t, c.args...)
			if code != 0 {
				t.Fatalf("%q exited %d:\n%s%s", c.args, code, out, errOut)
			}
			want := c.name + " is switched off in the console (since "
			if !strings.HasPrefix(out, want) || !strings.HasSuffix(out, "): nothing ran.\n") || strings.Count(out, "\n") != 1 {
				t.Errorf("output %q, want the one line %q…", out, want)
			}
			if n := runCount(t, `SELECT count(*) FROM run`) - runsBefore; n != 1 {
				t.Errorf("%d new runs, want 1", n)
			}
			if n := runCount(t, `SELECT count(*) FROM run WHERE command = $1 AND note = 'switched off' AND finished_at IS NOT NULL`, c.command); n != 1 {
				t.Errorf("%d finished switched-off runs of %s, want 1", n, c.command)
			}
			after := switchTableCounts(t)
			for tbl, n := range before {
				if after[tbl] != n {
					t.Errorf("%s: %d rows → %d", tbl, n, after[tbl])
				}
			}
			if len(after) != len(before) {
				t.Errorf("tables %d → %d", len(before), len(after))
			}
		})
	}
}

// TestJobOffOtherJobsRun: switching one job off leaves the others on: the probe runs with
// the batch switched off, and a switch turned back on lets its job run.
func TestJobOffOtherJobsRun(t *testing.T) {
	leadsDB(t)
	ctx := context.Background()
	lq := leadstoretest.Q(t)
	sitesOut(t, append([]string{"leads", "sites", "import", "manual"}, sitesFixtureDomains...)...)
	if _, err := lq.SetJobSwitch(ctx, 0, leadstore.JobBatch, false, "", leadstore.ActorGremy); err != nil {
		t.Fatal(err)
	}
	if out := sitesOut(t, "leads", "sites", "probe", "--dry-run", "--fixtures", sitesFixtureDir); !strings.Contains(out, "6 sites probed") {
		t.Errorf("probe with the batch off:\n%s", out)
	}
	if _, err := lq.SetJobSwitch(ctx, 0, leadstore.JobProbe, false, "", leadstore.ActorGremy); err != nil {
		t.Fatal(err)
	}
	if out := sitesOut(t, "leads", "sites", "probe", "--dry-run", "--fixtures", sitesFixtureDir); !strings.Contains(out, "Site probe is switched off") {
		t.Errorf("probe off:\n%s", out)
	}
	if _, err := lq.SetJobSwitch(ctx, 0, leadstore.JobProbe, true, "", leadstore.ActorGremy); err != nil {
		t.Fatal(err)
	}
	if out := sitesOut(t, "leads", "sites", "probe", "--dry-run", "--fixtures", sitesFixtureDir); !strings.Contains(out, "· 0 sites probed") {
		t.Errorf("probe on again:\n%s", out)
	}
}

// TestSyncRecordsItsRun: leads sync, on, now records a finished run (the "last run" the
// console shows), with its outcome as the note.
func TestSyncRecordsItsRun(t *testing.T) {
	leadsDB(t)
	out := sitesOut(t, "leads", "sync")
	if !strings.Contains(out, "No mailbox to sync") {
		t.Fatalf("sync:\n%s", out)
	}
	if n := runCount(t, `SELECT count(*) FROM run WHERE command = $1 AND source = $2 AND note = 'no mailbox to sync' AND finished_at IS NOT NULL`,
		commandLeadsSync, sourceSync); n != 1 {
		t.Errorf("%d recorded sync runs, want 1", n)
	}
}

// TestSendSyncsWithSyncSwitchOff: the inbox sync switched off stops leads sync, never the
// reply sync leads send runs itself before sending, so no lock between them is needed: a
// reply that came in while the sync timer was off still stops the follow-up.
func TestSendSyncsWithSyncSwitchOff(t *testing.T) {
	w := newSendWorld(t, true, 20)
	a := "alpha.example"
	w.batch(t, false, map[int64]string{w.lead(t, a): a})
	wantLines(t, "first send", w.mustRun(t, "leads", "send"), "Sent 1: ")

	if _, err := w.lq.SetJobSwitch(context.Background(), 0, leadstore.JobSync, false, "", leadstore.ActorGremy); err != nil {
		t.Fatal(err)
	}
	w.clock.set(time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC))
	w.box.deliver("INBOX", replyHeader(a, w.firstMessageID(t, a), "Re: Your Drupal site"), "Yes, let us talk.")

	wantLines(t, "sync off", w.mustRun(t, "leads", "sync"), "Inbox sync is switched off in the console")
	if s := w.leadState(t, a); s != leadstore.LeadContacted {
		t.Fatalf("a switched-off sync read the reply: lead %s", s)
	}

	out := w.mustRun(t, "leads", "send")
	wantLines(t, "send with sync off", out, "1 reply to read in the outreach mailbox: "+a)
	if w.smtp.count() != 1 {
		t.Errorf("%d messages sent in all, want 1 (the follow-up stopped): %v", w.smtp.count(), w.smtp.recipients())
	}
	if s := w.leadState(t, a); s != leadstore.LeadReplied {
		t.Errorf("alpha %s, want replied", s)
	}
	if s := w.msgState(t, a, 1); s != leadstore.MsgCancelled {
		t.Errorf("alpha's follow-up %s, want cancelled", s)
	}
	w.sweep(t)
}

// TestRunNowSwitchedOffRequest: a request for research while it is switched off is refused
// in the console's words and writes no request file; on, it is allowed.
func TestRunNowSwitchedOffRequest(t *testing.T) {
	dir := leadsDB(t)
	ctx := context.Background()
	lq := leadstoretest.Q(t)
	if err := switchedOffRequest(ctx, lq, researchAction); err != nil {
		t.Fatalf("research on: %v", err)
	}
	if _, err := lq.SetJobSwitch(ctx, 0, leadstore.JobResearch, false, "", leadstore.ActorGremy); err != nil {
		t.Fatal(err)
	}
	err := switchedOffRequest(ctx, lq, researchAction)
	if !errors.Is(err, console.ErrUserFixable) || !strings.HasSuffix(err.Error(), ": Research is switched off on the Controls page.") {
		t.Fatalf("research off: %v", err)
	}
	for _, a := range console.RunActions { // no Run now card action runs a switched job
		if err := switchedOffRequest(ctx, lq, a); err != nil {
			t.Errorf("%s: %v", a, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, runRequestDir, researchAction)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a request file exists: %v", err)
	}
}

// TestResearchSwitchConsole: with research switched off, "Look again" is refused in the
// Controls words (422 through the console) and queues nothing; a star still stands and still
// queues its search, says research is off and writes no request file. Switched back on, the
// queued search is the one the research job claims, and star and Look again behave as before.
func TestResearchSwitchConsole(t *testing.T) {
	researchScene(t, "northwind.example", "contoso.example")
	ctx := context.Background()
	b := leadsBackend(t)
	lq := leadstoretest.Q(t)
	north, contoso := leadIDOf(t, "northwind.example"), leadIDOf(t, "contoso.example")
	file := researchRequestFile(t, b)
	_ = os.Remove(file)
	if _, err := lq.SetJobSwitch(ctx, 0, leadstore.JobResearch, false, "", leadstore.ActorGremy); err != nil {
		t.Fatal(err)
	}

	out, err := b.LeadAct(ctx, console.LeadAction{Do: console.LeadStar, ID: north})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(out.Message, "so a search for one is queued for the research job. "+researchOffStarWords) {
		t.Errorf("star with research off: %q", out.Message)
	}
	if n := dbCount(t, fmt.Sprintf(`SELECT count(*) FROM lead_star WHERE lead_id = %d AND action = 'star'`, north)); n != 1 {
		t.Errorf("%d stars stored, want 1", n)
	}
	if n := dbCount(t, fmt.Sprintf(`SELECT count(*) FROM contact_research WHERE lead_id = %d AND status = 'queued' AND trigger = 'star'`, north)); n != 1 {
		t.Errorf("%d queued star attempts, want 1", n)
	}
	if _, err := os.Stat(file); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a star with research off wrote the request file: %v", err)
	}

	_, err = b.LeadAct(ctx, console.LeadAction{Do: console.LeadLookAgain, ID: contoso})
	if !errors.Is(err, console.ErrUserFixable) || !strings.HasSuffix(err.Error(), ": Research is switched off on the Controls page.") {
		t.Errorf("look again with research off: %v", err)
	}
	c := newConsoleClient(t, b, nil)
	path := fmt.Sprintf("/leads/%d", contoso)
	w := c.post(path+"/look-again", url.Values{"back": {path + "#contact-research"}})
	if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), "Research is switched off on the Controls page.") {
		t.Errorf("look again through the console: %d\n%s", w.Code, w.Body.String())
	}
	if n := dbCount(t, fmt.Sprintf(`SELECT count(*) FROM contact_research WHERE lead_id = %d`, contoso)); n != 0 {
		t.Errorf("a refused look again queued %d attempts", n)
	}
	if _, err := os.Stat(file); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a refused look again wrote the request file: %v", err)
	}

	// Back on: the star's queued search is what the research job takes, and a new star
	// writes the request file with its usual words.
	if _, err := lq.SetJobSwitch(ctx, 0, leadstore.JobResearch, true, "", leadstore.ActorGremy); err != nil {
		t.Fatal(err)
	}
	out, err = b.LeadAct(ctx, console.LeadAction{Do: console.LeadStar, ID: contoso})
	if err != nil || strings.Contains(out.Message, "switched off") || !strings.HasSuffix(out.Message, "queued for the research job.") {
		t.Errorf("star with research on: %q %v", out.Message, err)
	}
	if _, err := os.Stat(file); err != nil {
		t.Errorf("no request file after a star with research on: %v", err)
	}
	rows, err := lq.ClaimResearch(ctx, 5)
	if err != nil || len(rows) != 2 || rows[0].LeadID != north {
		t.Fatalf("claim after the switch is on: %v, %+v", err, rows)
	}
	if out, err := b.LeadAct(ctx, console.LeadAction{Do: console.LeadLookAgain, ID: north}); err != nil || !strings.Contains(out.Message, "already waiting or running") {
		t.Errorf("look again with research on, while looking: %q %v", out.Message, err)
	}
}
