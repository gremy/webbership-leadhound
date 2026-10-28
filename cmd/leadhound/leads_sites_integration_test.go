//go:build integration

package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/gremy/webbership-leadhound/internal/campaign"
	"github.com/gremy/webbership-leadhound/internal/leadstore/leadstoretest"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

const sitesFixtureDir = "../../internal/drupal/testdata/sites"

var sitesFixtureDomains = []string{
	"northwind.example", "contoso.example", "fabrikam.example", "tailspin.example", "wingtip.example", "adatum.example",
}

// leadsDB empties the test database (leads tables included), points the commands at it and
// at a copy of examples/ as the data dir, with live fetching off. The copy leaves out the
// example campaigns/ folder, as a data dir from before campaign files has none: the legacy
// campaign leads.yaml gives is the only one, and a batch needs no registered version.
// campaignsDataDir is the data dir with the campaign files.
func leadsDB(t *testing.T) string {
	t.Helper()
	leadstoretest.Reset(t)
	dir := t.TempDir()
	if err := os.CopyFS(dir, os.DirFS("../../examples")); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(dir, campaign.Dir)); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LEADHOUND_DATABASE_URL", storetest.AppDSN())
	t.Setenv("LEADHOUND_DATA_DIR", dir)
	t.Setenv("LEADHOUND_LIVE", "")
	return dir
}

// sitesOut runs a command and returns its stdout; a non-zero exit fails the test.
func sitesOut(t *testing.T, args ...string) string {
	t.Helper()
	var out, errOut bytes.Buffer
	saved := stdout
	stdout = &out
	t.Cleanup(func() { stdout = saved })
	if code := run(context.Background(), args, &errOut); code != 0 {
		t.Fatalf("%q exited %d: %s", args, code, errOut.String())
	}
	stdout = saved
	return out.String()
}

func TestLeadsSitesImportProbeEval(t *testing.T) {
	dir := leadsDB(t)

	args := append([]string{"leads", "sites", "import", "manual"}, sitesFixtureDomains...)
	args = append(args, "www.northwind.example", "not a domain")
	if got, want := sitesOut(t, args...), "leads sites import manual: added 6 · known 1 · skipped 1 (bad lines)\n"; got != want {
		t.Errorf("import manual: %q, want %q", got, want)
	}

	out := sitesOut(t, "leads", "sites", "probe", "--dry-run", "--fixtures", sitesFixtureDir)
	for _, want := range []string{
		"(dry run, fixtures) · 6 sites probed",
		"Drupal found: 3 (D7 1 · D10 1 · D11 1)",
		"exact versions seen: 10.6.14 1",
		"bot checks: 1 · robots-disallowed: 1 · not public: 0",
		"errors: 0 (unreachable 0 · HTTP errors 0 · not HTML 0)",
		"stop none",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("probe summary lacks %q:\n%s", want, out)
		}
	}
	// The cold-start twin: nothing is due within 30 days.
	if out := sitesOut(t, "leads", "sites", "probe", "--dry-run", "--fixtures", sitesFixtureDir); !strings.Contains(out, "· 0 sites probed") {
		t.Errorf("second probe:\n%s", out)
	}
	if n := sitesCount(t, `SELECT count(*) FROM run WHERE command = 'leads-sites-probe' AND source = 'sites-fixture' AND finished_at IS NOT NULL`); n != 2 {
		t.Errorf("%d finished probe runs, want 2", n)
	}

	labels := filepath.Join(dir, "labels.csv")
	csv := "domain,final_url,major\n" +
		"northwind.example,https://northwind.example/,7\n" +
		"contoso.example,https://contoso.example/,10\n" +
		"fabrikam.example,https://fabrikam.example/,10\n" +
		"tailspin.example,https://tailspin.example/,none\n" +
		"wingtip.example,https://wingtip.example/,11\n" +
		"proseware.example,https://proseware.example/,9\n"
	if err := os.WriteFile(labels, []byte(csv), 0o600); err != nil {
		t.Fatal(err)
	}
	before := sitesCount(t, `SELECT count(*) FROM site_probe`)
	want := "leads sites eval: 6 labelled · 4 reachable in both runs · agreement 3/4 (75.0%)\n" +
		"  missed Drupal: 0 · false Drupal: 0 · wrong major: 1 · major unknown: 0\n" +
		"  unreachable in either run: 1 · not probed: 1\n" +
		"  disagreements (domain · label · found · signals):\n" +
		"    fabrikam.example · D10 · D11 · meta-generator:11|drupalSettings\n"
	if got := sitesOut(t, "leads", "sites", "eval", "--labels", labels); got != want {
		t.Errorf("eval:\n%s\nwant:\n%s", got, want)
	}
	if after := sitesCount(t, `SELECT count(*) FROM site_probe`); after != before {
		t.Errorf("eval wrote probes: %d → %d", before, after)
	}
}

func TestLeadsSitesImportRankedList(t *testing.T) {
	dir := leadsDB(t)
	// A test-only TLD list, so every row stays on reserved names.
	cfgPath := filepath.Join(dir, "leads.yaml")
	cfg, err := os.ReadFile(cfgPath) // #nosec G304 -- the test's own temp copy
	if err != nil {
		t.Fatal(err)
	}
	cfg = regexp.MustCompile(`(?m)^  tlds: \[[^\]]*\]`).ReplaceAll(cfg, []byte("  tlds: [example, test]"))
	if err := os.WriteFile(cfgPath, cfg, 0o600); err != nil { // #nosec G703 -- a test temp dir
		t.Fatal(err)
	}
	list := filepath.Join(dir, "majestic.csv")
	body := "GlobalRank,TldRank,Domain,TLD\n" +
		"1,1,www.northwind.example,example\n" +
		"2,1,contoso.test,test\n" +
		"3,1,fabrikam.invalid,invalid\n" +
		"x,1,broken.example,example\n"
	if err := os.WriteFile(list, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	want := "leads sites import majestic: added 2 · known 0 · skipped 1 (bad lines) · filtered 1 (other TLDs)\n"
	if got := sitesOut(t, "leads", "sites", "import", "majestic", list); got != want {
		t.Errorf("import majestic: %q, want %q", got, want)
	}
	if n := sitesCount(t, `SELECT count(*) FROM site WHERE origin = 'majestic' AND list_rank IS NOT NULL
		AND domain IN ('northwind.example', 'contoso.test')`); n != 2 {
		t.Errorf("%d ranked sites imported, want northwind.example and contoso.test", n)
	}
}

func sitesCount(t *testing.T, sql string) int {
	t.Helper()
	var n int
	if err := storetest.AppConn(t).QueryRow(context.Background(), sql).Scan(&n); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return n
}
