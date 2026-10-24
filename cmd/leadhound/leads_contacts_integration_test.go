//go:build integration

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/gremy/webbership-leadhound/internal/contacts"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/postingguard"
	"github.com/gremy/webbership-leadhound/internal/sitelist"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

const (
	contactsFixtureDir = "../../internal/contacts/testdata/run"
	agenciesFixtureDir = "../../internal/agencies/testdata/run"
)

// fixturePersonLike are the person-like addresses the fixtures hold; none may be stored.
var fixturePersonLike = []string{
	"john.smith@northwind.example", "j.smith@northwind.example", "jane.doe@wingtip.example",
}

// mxResolver has an MX record for the listed domains only.
type mxResolver map[string]bool

func (r mxResolver) LookupMX(_ context.Context, name string) ([]*net.MX, error) {
	if r[name] {
		return []*net.MX{{Host: "mx." + name + ".", Pref: 10}}, nil
	}
	return nil, errors.New("no such host")
}

const testAgencies = `- name: Wingtip Digital
  website: https://www.wingtip.example/
  country: GB
  evidence_url: https://www.wingtip.example/services/drupal-upgrades
- name: Adatum Studio
  website: https://adatum.example/
  evidence_url: https://www.drupal.org/adatum-studio
- name: Northwind Agency
  website: https://www.northwind.example/
  evidence_url: https://northwind.example/upgrades
- name: Litware
  website: https://litware.example/
- name: Real-looking Agency
  website: https://example.com/
- name: Fabrikam Interactive
  website: ""
`

// runOut runs a command and returns its exit code, stdout and stderr.
func leadsRun(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	saved := stdout
	stdout = &out
	defer func() { stdout = saved }()
	code := run(context.Background(), args, &errOut)
	return code, out.String(), errOut.String()
}

func wantLines(t *testing.T, what, out string, lines ...string) {
	t.Helper()
	for _, l := range lines {
		if !strings.Contains(out, l) {
			t.Errorf("%s lacks %q:\n%s", what, l, out)
		}
	}
}

func TestLeadsAgenciesContactsExport(t *testing.T) {
	dir := leadsDB(t)
	cfgPath := filepath.Join(dir, "leads.yaml")
	cfg, err := os.ReadFile(cfgPath) // #nosec G304 -- the test's own temp copy
	if err != nil {
		t.Fatal(err)
	}
	cfg = regexp.MustCompile(`(?m)^public_body_suffixes: \[[^\]]*\]`).ReplaceAll(cfg, []byte("public_body_suffixes: [tailspin.example]"))
	if err := os.WriteFile(cfgPath, cfg, 0o600); err != nil { // #nosec G703 -- a test temp dir
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "agencies.yaml"), []byte(testAgencies), 0o600); err != nil {
		t.Fatal(err)
	}
	clock := &fakeClock{now: time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC)}
	savedClock, savedDry := agencyClock, contactsDryResolver
	agencyClock, contactsDryResolver = clock, mxResolver{"northwind.example": true}
	t.Cleanup(func() { agencyClock, contactsDryResolver = savedClock, savedDry })

	// Four Drupal sites, probed from the same fixtures.
	sitesOut(t, "leads", "sites", "import", "manual", "northwind.example", "contoso.example", "fabrikam.example", "tailspin.example")
	wantLines(t, "probe", sitesOut(t, "leads", "sites", "probe", "--dry-run", "--fixtures", contactsFixtureDir), "Drupal found: 4")

	// First route search: an org per Drupal site, the public body skipped.
	code, out, errOut := leadsRun(t, "leads", "contacts", "--dry-run", "--fixtures", contactsFixtureDir)
	if code != 0 {
		t.Fatalf("contacts exited %d: %s", code, errOut)
	}
	wantLines(t, "contacts 1", out,
		"(dry run, fixtures) · 3 orgs checked",
		"role emails found: 3 (with MX 2 · without 1)",
		"forms: 1",
		"no route: 1",
		"dropped: person-like 2 · protected (not decoded) 1 · foreign 1",
		"public bodies skipped: 1 · robots-disallowed pages: 1 · new orgs from probes: 4",
		"4 requests",
	)

	// Agency import: one host at a time, 10 s apart on the injected clock.
	code, out, errOut = leadsRun(t, "leads", "agencies", "import", "--dry-run", "--fixtures", agenciesFixtureDir)
	if code != 0 {
		t.Fatalf("agencies import exited %d: %s", code, errOut)
	}
	wantLines(t, "agencies", out, "· agencies 4 · created 3 · clashes 1 · findings 1 · no finding 2 · "+
		"skipped without website 1 · drupal.org pages skipped 1")
	// wingtip: robots, homepage, evidence (2 waits); adatum and litware: robots, homepage (1 each).
	if len(clock.sleeps) != 4 {
		t.Errorf("sleeps = %v, want four of 10 s", clock.sleeps)
	}
	for _, d := range clock.sleeps {
		if d < 10*time.Second {
			t.Errorf("a wait of %s between two requests to one host", d)
		}
	}
	// Running it again records no second finding.
	code, out, _ = leadsRun(t, "leads", "agencies", "import", "--dry-run", "--fixtures", agenciesFixtureDir)
	if code != 0 || !strings.Contains(out, "findings 0 (1 more already on file)") || !strings.Contains(out, "created 0") {
		t.Errorf("second import exited %d:\n%s", code, out)
	}

	// Second route search: the three agencies; litware's homepage gives no answer, so its
	// search is cut short and records nothing.
	code, out, errOut = leadsRun(t, "leads", "contacts", "--dry-run", "--fixtures", contactsFixtureDir)
	if code != 0 {
		t.Fatalf("contacts 2 exited %d: %s", code, errOut)
	}
	wantLines(t, "contacts 2", out,
		"· 3 orgs checked",
		"cut short (still due): 1",
		"role emails found: 1 (with MX 0 · without 1)",
		"forms: 1",
		"no route: 1",
		"dropped: person-like 1 · protected (not decoded) 0 · foreign 0",
	)
	// The cold-start twin: only the cut-short org is still due.
	if _, out, _ = leadsRun(t, "leads", "contacts", "--dry-run", "--fixtures", contactsFixtureDir); !strings.Contains(out, "· 1 orgs checked") ||
		!strings.Contains(out, "cut short (still due): 1") {
		t.Errorf("third search:\n%s", out)
	}
	if n := sitesCount(t, `SELECT count(*) FROM contact_attempt a JOIN org o ON o.id = a.org_id WHERE o.domain = 'litware.example'`); n != 0 {
		t.Errorf("a cut-short search recorded %d attempts", n)
	}
	// An org on drupal.org is never searched.
	dorg, err := leadstore.NewQueries(storetest.AppConn(t)).UpsertAgency(context.Background(),
		leadstore.AgencyImport{Name: "Directory", Website: "https://www.drupal.org/"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, out, _ = leadsRun(t, "leads", "contacts", "--dry-run", "--fixtures", contactsFixtureDir, "--org", fmt.Sprint(dorg.OrgID)); !strings.Contains(out, "· 0 orgs checked") ||
		!strings.Contains(out, "drupal.org orgs skipped: 1") {
		t.Errorf("drupal.org org search:\n%s", out)
	}

	conn := storetest.AppConn(t)
	ctx := context.Background()
	if n := sitesCount(t, `SELECT count(*) FROM run WHERE command = 'leads-contacts' AND source = 'contacts-fixture' AND finished_at IS NOT NULL`); n != 4 {
		t.Errorf("%d finished contacts runs, want 4", n)
	}
	if n := sitesCount(t, `SELECT count(*) FROM run WHERE command = 'leads-agencies-import' AND source = 'agencies-fixture' AND finished_at IS NOT NULL`); n != 2 {
		t.Errorf("%d finished agency runs, want 2", n)
	}
	if n := sitesCount(t, `SELECT count(*) FROM org o JOIN site s ON s.id = o.site_id
		WHERE o.kind = 'agency' AND s.origin = 'agency' AND o.domain IN ('wingtip.example', 'adatum.example')`); n != 2 {
		t.Errorf("%d agency orgs linked to their agency site rows, want 2", n)
	}
	if n := sitesCount(t, `SELECT count(*) FROM org WHERE domain = 'example.com'`); n != 0 {
		t.Errorf("a dry run imported an agency on a real TLD")
	}
	if n := sitesCount(t, `SELECT count(*) FROM agency_finding f JOIN org o ON o.id = f.org_id
		WHERE o.domain = 'wingtip.example' AND f.phrase LIKE '%Drupal 7 to Drupal 10%'
		AND f.url = 'https://www.wingtip.example/services/drupal-upgrades'`); n != 1 {
		t.Errorf("%d wingtip findings, want 1", n)
	}

	// The invariant sweep over every contact_route row.
	type routeRow struct {
		Kind, Value, OrgDomain, URL, FinalURL string
		Body                                  []byte
	}
	rows, err := conn.Query(ctx, `SELECT r.kind, r.value, o.domain, f.url, f.final_url, f.body
		FROM contact_route r JOIN org o ON o.id = r.org_id JOIN http_fetch f ON f.id = r.fetch_id ORDER BY r.id`)
	if err != nil {
		t.Fatal(err)
	}
	routes, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (routeRow, error) {
		var x routeRow
		err := r.Scan(&x.Kind, &x.Value, &x.OrgDomain, &x.URL, &x.FinalURL, &x.Body)
		return x, err
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 6 {
		t.Errorf("%d routes, want 6: %+v", len(routes), routes)
	}
	for _, r := range routes {
		switch r.Kind {
		case contacts.KindEmail:
			if !strings.Contains(strings.ToLower(postingguard.DecodePage(r.Body)), r.Value) {
				t.Errorf("email %s is not in the body of its fetch %s", r.Value, r.URL)
			}
			_, host, _ := strings.Cut(r.Value, "@")
			if d, err := sitelist.Registrable(host); err != nil || d != r.OrgDomain {
				t.Errorf("email %s is not on its org's domain %s", r.Value, r.OrgDomain)
			}
		case contacts.KindForm:
			if r.Value != r.URL && r.Value != r.FinalURL {
				t.Errorf("form %s is not its fetch's url %s / %s", r.Value, r.URL, r.FinalURL)
			}
			if u, err := url.Parse(r.Value); err != nil {
				t.Errorf("form %s: %v", r.Value, err)
			} else if d, err := sitelist.Registrable(u.Hostname()); err != nil || d != r.OrgDomain {
				t.Errorf("form %s is not on its org's domain %s", r.Value, r.OrgDomain)
			}
			if len(r.Body) == 0 {
				t.Errorf("form %s: its page keeps no body", r.Value)
			}
		default:
			t.Errorf("route kind %q", r.Kind)
		}
	}

	// A page fetched by these commands keeps its body only when it yielded a route or a finding.
	if n := sitesCount(t, `SELECT count(*) FROM http_fetch f
		WHERE f.source IN ('contacts-fixture', 'agencies-fixture') AND octet_length(f.body) > 0
		  AND NOT EXISTS (SELECT 1 FROM contact_route r WHERE r.fetch_id = f.id)
		  AND NOT EXISTS (SELECT 1 FROM agency_finding a WHERE a.fetch_id = f.id)`); n != 0 {
		t.Errorf("%d fetched pages keep a body without yielding anything", n)
	}
	// robots.txt kept contoso's contact page from being fetched; drupal.org was never read;
	// the clashing agency was never read.
	for _, sql := range []string{
		`SELECT count(*) FROM http_fetch WHERE url = 'https://contoso.example/contact'`,
		`SELECT count(*) FROM http_fetch WHERE url LIKE '%drupal.org%'`,
		`SELECT count(*) FROM http_fetch WHERE source = 'agencies-fixture' AND url LIKE '%northwind%'`,
	} {
		if n := sitesCount(t, sql); n != 0 {
			t.Errorf("%s = %d, want 0", sql, n)
		}
	}
	// At most 3 pages per org in one run (robots.txt not counted).
	pages, err := conn.Query(ctx, `SELECT run_id, url FROM http_fetch WHERE kind = 'site' AND source = 'contacts-fixture'`)
	if err != nil {
		t.Fatal(err)
	}
	perOrg := map[string]int{}
	for pages.Next() {
		var run int64
		var u string
		if err := pages.Scan(&run, &u); err != nil {
			t.Fatal(err)
		}
		host := strings.SplitN(strings.TrimPrefix(u, "https://"), "/", 2)[0]
		d, _ := sitelist.Registrable(host)
		perOrg[fmt.Sprintf("%s in run %d", d, run)]++
	}
	pages.Close()
	for k, n := range perOrg {
		if n > contacts.MaxPages {
			t.Errorf("%s: %d pages", k, n)
		}
	}

	// No person-like address from the fixtures in any text column of any lead table.
	cols, err := conn.Query(ctx, `SELECT table_name, column_name FROM information_schema.columns
		WHERE table_schema = 'public' AND table_name IN ('site', 'site_probe', 'fetch_header', 'org', 'contact_route', 'agency_finding', 'contact_attempt')
		  AND data_type IN ('text', 'character varying', 'jsonb', 'json', 'ARRAY')`)
	if err != nil {
		t.Fatal(err)
	}
	var targets [][2]string
	for cols.Next() {
		var tbl, col string
		if err := cols.Scan(&tbl, &col); err != nil {
			t.Fatal(err)
		}
		targets = append(targets, [2]string{tbl, col})
	}
	cols.Close()
	if len(targets) < 10 {
		t.Fatalf("only %d text columns found: %v", len(targets), targets)
	}
	for _, tc := range targets {
		for _, p := range fixturePersonLike {
			var n int
			sql := `SELECT count(*) FROM ` + pgx.Identifier{tc[0]}.Sanitize() + ` WHERE ` + pgx.Identifier{tc[1]}.Sanitize() + `::text ILIKE '%' || $1 || '%'`
			if err := conn.QueryRow(ctx, sql, p).Scan(&n); err != nil {
				t.Fatal(err)
			}
			if n != 0 {
				t.Errorf("%s.%s holds a person-like address %s", tc[0], tc[1], p)
			}
		}
	}

	// Export: reserved TLDs are left out, so only the one real-TLD org (RFC 2606) is written.
	q := leadstore.NewQueries(conn)
	ag, err := q.UpsertAgency(ctx, leadstore.AgencyImport{Name: "Documentation Agency", Website: "https://example.net/"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var fid int64
	if err := conn.QueryRow(ctx, `SELECT fetch_id FROM contact_route ORDER BY id LIMIT 1`).Scan(&fid); err != nil {
		t.Fatal(err)
	}
	if _, err := q.InsertRoute(ctx, leadstore.RouteRow{OrgID: ag.OrgID, Kind: leadstore.RouteForm, Value: "https://example.net/contact", FetchID: fid, DerivedBy: contacts.DerivedBy}); err != nil {
		t.Fatal(err)
	}
	code, out, errOut = leadsRun(t, "leads", "export", "routes")
	if code != 0 {
		t.Fatalf("export exited %d: %s", code, errOut)
	}
	path := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(out, "leads export routes: "), " · 1 rows\n"))
	if !strings.HasSuffix(out, " · 1 rows\n") || filepath.Dir(path) != filepath.Join(dir, "exports") {
		t.Fatalf("export said %q", out)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("export mode %v, want 0600", fi.Mode().Perm())
	}
	if di, err := os.Stat(filepath.Dir(path)); err != nil || di.Mode().Perm() != 0o700 {
		t.Errorf("exports dir: %v, %v", di, err)
	}
	body, err := os.ReadFile(path) // #nosec G304 -- the test's own temp dir
	if err != nil {
		t.Fatal(err)
	}
	want := "domain,kind,name,country,finding,finding_url,route_kind,route,mx_ok\n" +
		"example.net,agency,Documentation Agency,,,,form,https://example.net/contact,false\n"
	if string(body) != want {
		t.Errorf("export:\n%s\nwant:\n%s", body, want)
	}
	// The same day's export is never overwritten.
	if code, _, _ := leadsRun(t, "leads", "export", "routes"); code != 2 {
		t.Errorf("second export exited %d, want 2", code)
	}
}
