//go:build integration

package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gremy/webbership-leadhound/internal/config"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/store"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

// overBudget sets a database budget of one byte, which every database passes, so the
// commands' own storage check (the real database size) says stop.
func overBudget(t *testing.T) {
	t.Helper()
	t.Setenv("LEADHOUND_DB_MAX_BYTES", "1")
}

// TestCollectStorageOverAtStartCommands: over the budget, collect ats and collect linkedin
// request nothing, store nothing, record every run with stop reason disk_budget and exit 0.
func TestCollectStorageOverAtStartCommands(t *testing.T) {
	commandDB(t)
	t.Setenv("LEADHOUND_DATA_DIR", "")
	captureNotes(t)
	overBudget(t)
	code, out := runCmd(t, "collect", "ats", "--dry-run", "--fixtures", atsTestdata)
	if code != 0 || strings.Count(out, "stop disk_budget") == 0 || !strings.Contains(out, "over its 0 MB budget") {
		t.Errorf("collect ats: exit %d\n%s", code, out)
	}
	code, out = runCmd(t, "collect", "linkedin", "--dry-run", "--fixtures", linkedinTestdata)
	if code != 0 || !strings.Contains(out, "stop disk_budget") || !strings.Contains(out, "stopped storing: database") {
		t.Errorf("collect linkedin: exit %d\n%s", code, out)
	}
	if got := dbCount(t, `SELECT count(*) FROM run WHERE stop_reason <> 'disk_budget' OR finished_at IS NULL`); got != 0 {
		t.Errorf("%d runs not recorded as disk_budget", got)
	}
	for _, table := range []string{"http_fetch", "job"} {
		if got := dbCount(t, `SELECT count(*) FROM `+table); got != 0 {
			t.Errorf("%s rows = %d, want 0", table, got)
		}
	}
}

// TestBriefStorageOverSaysWhy: the brief after a collect stopped by the budget leads its
// storage line with the stop and names the stop reason in plain words.
func TestBriefStorageOverSaysWhy(t *testing.T) {
	scoredFixtures(t)
	overBudget(t)
	t.Setenv("LEADHOUND_DATA_DIR", "")
	if code, out := runCmd(t, "collect", "linkedin", "--dry-run", "--fixtures", linkedinTestdata); code != 0 || !strings.Contains(out, "stop disk_budget") {
		t.Fatalf("collect: exit %d\n%s", code, out)
	}
	t.Setenv("LEADHOUND_DATA_DIR", examplesDir)
	ob := filepath.Join(t.TempDir(), "outbox")
	if code, _, stderr := runCmdErr(t, "brief", "--outbox", ob); code != 0 {
		t.Fatalf("brief: exit %d, %q", code, stderr)
	}
	text := outboxText(t, ob)
	for _, want := range []string{"Health: collectors stopped storing: storage ", "storage budget reached"} {
		if !strings.Contains(text, want) {
			t.Errorf("the brief lacks %q:\n%s", want, text)
		}
	}
}

// TestStorageReaderUnderBudget: under the budget the brief's reader gives the state, which
// shows no line.
func TestStorageReaderUnderBudget(t *testing.T) {
	commandDB(t)
	ctx := context.Background()
	s, err := store.Open(ctx, storetest.AppDSN())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	st, err := readStorage(ctx, s.Q())
	if err != nil || st == nil || st.DBBytes <= 0 || st.MaxBytes != config.DefaultDBMaxBytes || st.OverBudget() {
		t.Errorf("state %+v, err %v", st, err)
	}
}

// TestLeadsStorageOver: over the budget, the probe probes nothing and records disk_budget,
// sites import imports nothing and fails, and agencies import writes no org and records
// disk_budget.
func TestLeadsStorageOver(t *testing.T) {
	dir := leadsDB(t)
	sitesOut(t, append([]string{"leads", "sites", "import", "manual"}, sitesFixtureDomains...)...)
	overBudget(t)

	out := sitesOut(t, "leads", "sites", "probe", "--dry-run", "--fixtures", sitesFixtureDir)
	wantLines(t, "probe", out, "· 0 sites probed", "stop disk_budget (database ")
	if n := sitesCount(t, `SELECT count(*) FROM run WHERE command = 'leads-sites-probe' AND stop_reason = 'disk_budget' AND note LIKE '%stopped storing: database%'`); n != 1 {
		t.Errorf("%d probe runs recorded with disk_budget, want 1", n)
	}
	if n := sitesCount(t, `SELECT count(*) FROM http_fetch`); n != 0 {
		t.Errorf("%d fetches, want 0", n)
	}

	// The refusal is the command's error, which main logs; the exit says it failed.
	if code, out, _ := leadsRun(t, "leads", "sites", "import", "manual", "litware.example"); code != 1 || out != "" {
		t.Errorf("sites import over budget: exit %d, %q; want 1 and nothing printed", code, out)
	}
	if n := sitesCount(t, `SELECT count(*) FROM site WHERE domain = 'litware.example'`); n != 0 {
		t.Errorf("sites import wrote over budget")
	}

	if err := os.WriteFile(filepath.Join(dir, "agencies.yaml"), []byte(testAgencies), 0o600); err != nil { // #nosec G703 -- a test temp dir
		t.Fatal(err)
	}
	code, out, errOut := leadsRun(t, "leads", "agencies", "import", "--dry-run", "--fixtures", agenciesFixtureDir)
	if code != 0 {
		t.Fatalf("agencies import exited %d: %s", code, errOut)
	}
	wantLines(t, "agencies", out, "· created 0 ·", "stop disk_budget (database ")
	if n := sitesCount(t, `SELECT count(*) FROM org`); n != 0 {
		t.Errorf("%d orgs written over budget", n)
	}
	if n := sitesCount(t, `SELECT count(*) FROM run WHERE command = 'leads-agencies-import' AND stop_reason = 'disk_budget'`); n != 1 {
		t.Errorf("%d agency runs recorded with disk_budget, want 1", n)
	}
}

// TestContactsStorageEvery: the route search asks the budget before every
// contactsStorageEvery orgs; over at the second ask, it stops there and records disk_budget.
func TestContactsStorageEvery(t *testing.T) {
	leadsDB(t)
	ctx := context.Background()
	lq := leadstore.NewQueries(storetest.AppConn(t))
	for i := range contactsStorageEvery + 5 {
		if _, err := lq.UpsertAgency(ctx, leadstore.AgencyImport{Name: "Agency " + fmt.Sprint(i), Website: "https://agency" + fmt.Sprint(i) + ".example/"}, nil); err != nil {
			t.Fatal(err)
		}
	}
	var asked atomic.Int32
	prev := storageCheck
	t.Cleanup(func() { storageCheck = prev })
	storageCheck = func(config.Config, store.Queries) func(context.Context) (bool, string, error) {
		return func(context.Context) (bool, string, error) {
			if asked.Add(1) > 1 {
				return true, "disk 13% free, under the 15% floor", nil
			}
			return false, "", nil
		}
	}
	code, out, errOut := leadsRun(t, "leads", "contacts", "--dry-run", "--fixtures", contactsFixtureDir)
	if code != 0 {
		t.Fatalf("contacts exited %d: %s", code, errOut)
	}
	wantLines(t, "contacts", out, "· "+fmt.Sprint(contactsStorageEvery)+" orgs checked", "stop disk_budget (disk 13% free, under the 15% floor)")
	if asked.Load() != 2 {
		t.Errorf("asked %d times, want 2", asked.Load())
	}
	if n := sitesCount(t, `SELECT count(*) FROM run WHERE command = 'leads-contacts' AND stop_reason = 'disk_budget' AND note LIKE '%under the 15% floor%'`); n != 1 {
		t.Errorf("%d contacts runs recorded with disk_budget, want 1", n)
	}
}
