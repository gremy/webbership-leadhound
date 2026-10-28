//go:build integration

package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/campaign"
	"github.com/gremy/webbership-leadhound/internal/compliance"
	"github.com/gremy/webbership-leadhound/internal/fetch"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/leadstore/leadstoretest"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

const placesRelease = "2026-10-21.0"

// cafesDataDir turns the leadsDB data dir into one with the example campaigns (drupal-eol
// and cafes-loyalty), leads.yaml policies for the café countries, a Romanian footer and
// opt-out line, and the own vendor's host from private config.
func cafesDataDir(t *testing.T, dir string) {
	t.Helper()
	if err := os.CopyFS(filepath.Join(dir, campaign.Dir), os.DirFS("../../examples/campaigns")); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "leads.yaml")
	raw, err := os.ReadFile(path) // #nosec G304 -- the test's own temp copy
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	for _, r := range [][2]string{
		{"  DE: {channel: form,", "  RO: {channel: email, lang: ro, tz: Europe/Bucharest, footer: footer-ro}\n" +
			"  US: {channel: email, lang: en, tz: America/Chicago, footer: footer-en}\n  DE: {channel: form,"},
		{"    en: \"Not the right time?", "    ro: \"Nu e momentul potrivit? Răspunde cu \\\"dezabonare\\\" și nu îți mai scriu.\"\n    en: \"Not the right time?"},
		{"  reprobe_after_days: 30\n", "  reprobe_after_days: 30\n  cafe_own_vendor_hosts: [loyalty-own.example]\n"},
	} {
		if !strings.Contains(s, r[0]) {
			t.Fatalf("leads.yaml has no %q", r[0])
		}
		s = strings.Replace(s, r[0], r[1], 1)
	}
	if err := os.WriteFile(path, []byte(s), 0o600); err != nil { // #nosec G703 -- a test temp dir
		t.Fatal(err)
	}
	footer, err := os.ReadFile(filepath.Join(dir, "leads-templates", "footers", "footer-en.txt")) // #nosec G304 -- the test's own temp copy
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "leads-templates", "footers", "footer-ro.txt"), footer, 0o600); err != nil { // #nosec G703 -- a test temp dir
		t.Fatal(err)
	}
	// leads.yaml's own (Drupal) templates are needed in every language an email country of
	// it uses, RO's included: the English ones stand in here.
	for _, kind := range []string{"agency_partner", "d10", "unsupported"} {
		if err := os.CopyFS(filepath.Join(dir, "leads-templates", kind, "ro"), os.DirFS(filepath.Join(dir, "leads-templates", kind, "en"))); err != nil {
			t.Fatal(err)
		}
	}
}

// cafeFixtures writes a fixture dir serving the demo cafés' homepages from internal/cafe's
// test pages: Harbour Lane and Bean House show nothing, Copper Kettle links the own vendor
// (a customer), the Romanian café shows online ordering.
func cafeFixtures(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	manifest := map[string]string{}
	for host, page := range map[string]string{
		"harbour-lane-coffee.example": "none.html",
		"copper-kettle.example":       "own_customer.html",
		"bean-house.example":          "none.html",
		"cafeneaua-exemplu.example":   "romanian.html",
	} {
		b, err := os.ReadFile(filepath.Join("../../internal/cafe/testdata", page)) // #nosec G304 -- the repository's own test pages
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, host+".html"), b, 0o600); err != nil { // #nosec G703 -- a test temp dir
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "robots.txt"), []byte("User-agent: *\nAllow: /\n"), 0o600); err != nil { // #nosec G703 -- a test temp dir
			t.Fatal(err)
		}
		manifest["https://"+host+"/"] = host + ".html"
		manifest["https://"+host+"/robots.txt"] = "robots.txt"
	}
	b, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, fetch.ManifestFile), b, 0o600); err != nil { // #nosec G703 -- a test temp dir
		t.Fatal(err)
	}
	return dir
}

// importDemoPlaces registers the campaigns and imports examples/places.demo.ndjson into
// cafes-loyalty, returning the import's output.
func importDemoPlaces(t *testing.T, dir string) string {
	t.Helper()
	mustRun(t, "leads", "campaigns", "register")
	return mustRun(t, "leads", "places", "import", "overture", "--campaign", "cafes-loyalty", "--release", placesRelease,
		filepath.Join(dir, "places.demo.ndjson"))
}

// cafeRoute gives the café on domain an email route with MX, read from its homepage fetch.
func cafeRoute(t *testing.T, domain string) {
	t.Helper()
	ctx := context.Background()
	owner := storetest.OwnerConn(t)
	var orgID, fetchID int64
	if err := owner.QueryRow(ctx, `SELECT o.id, (SELECT max(f.id) FROM http_fetch f) FROM org o WHERE o.domain = $1`, domain).Scan(&orgID, &fetchID); err != nil {
		t.Fatalf("org %s: %v", domain, err)
	}
	if _, err := leadstoretest.Q(t).InsertRoute(ctx, leadstore.RouteRow{OrgID: orgID, Kind: leadstore.RouteEmail,
		Value: "hello@" + domain, FetchID: fetchID, MXOK: true, DerivedBy: "contacts@1"}); err != nil {
		t.Fatal(err)
	}
}

// TestPlacesImportBuildEndToEnd: the demo export is read with the campaign's filters (one
// malformed line named by number, a pizzeria and a faint place skipped), six same-name
// places and one branded place are skipped as chains with their rule stored and no org;
// the independent cafés get orgs (the US one with its state), sites probed by cafe@1, and
// the build turns each probe into a lead or a reason: the own vendor's customer is
// skipped, the café with no website is a kind the campaign leaves off. A second import of
// the same release stores nothing; the next release stores the places again over the same
// orgs (the cold-start twin).
func TestPlacesImportBuildEndToEnd(t *testing.T) {
	ctx := context.Background()
	dir := leadsDB(t)
	cafesDataDir(t, dir)
	out := importDemoPlaces(t, dir)
	wantLines(t, "first import", out,
		"leads places import: campaign cafes-loyalty, release 2026-10-21.0, import 1",
		"  read 15 lines: 12 kept · skipped 3 (blank 0 · malformed 1 · not a café 1 · low confidence 1 · other country 0 · no name 0)",
		"  malformed lines: 7",
		"  stored 12 places (0 already in this release) · 7 chains skipped (no lead) · 5 orgs and 4 sites made",
	)
	owner := storetest.OwnerConn(t)
	var chains, chainOrgs int
	if err := owner.QueryRow(ctx, `SELECT count(*) FILTER (WHERE chain_rule <> ''), count(*) FILTER (WHERE chain_rule <> '' AND org_id IS NOT NULL) FROM place`).Scan(&chains, &chainOrgs); err != nil {
		t.Fatal(err)
	}
	if chains != 7 || chainOrgs != 0 {
		t.Errorf("chains %d (with an org %d), want 7 and 0", chains, chainOrgs)
	}
	var rule, detail string
	if err := owner.QueryRow(ctx, `SELECT chain_rule, chain_detail FROM place WHERE name = 'Branded Example Espresso'`).Scan(&rule, &detail); err != nil || rule != "brand" || !strings.Contains(detail, "Example Espresso Co") {
		t.Errorf("branded place: rule %q detail %q (%v)", rule, detail, err)
	}
	var region, source string
	if err := owner.QueryRow(ctx, `SELECT region, source FROM org WHERE domain = 'bean-house.example'`).Scan(&region, &source); err != nil || region != "US-IL" || source != leadstore.OrgSourcePlace {
		t.Errorf("US café org: region %q source %q (%v), want US-IL place", region, source, err)
	}
	var noSite int
	if err := owner.QueryRow(ctx, `SELECT count(*) FROM org WHERE domain = 'overture:demo-place-0005' AND site_id IS NULL`).Scan(&noSite); err != nil || noSite != 1 {
		t.Errorf("no-website café org: %d (%v), want 1 with no site", noSite, err)
	}
	var lines []int32
	if err := owner.QueryRow(ctx, `SELECT malformed_lines FROM place_import WHERE id = 1`).Scan(&lines); err != nil || len(lines) != 1 || lines[0] != 7 {
		t.Errorf("logged malformed lines %v (%v), want [7]", lines, err)
	}

	again := mustRun(t, "leads", "places", "import", "overture", "--campaign", "cafes-loyalty", "--release", placesRelease,
		filepath.Join(dir, "places.demo.ndjson"))
	wantLines(t, "same release again", again, "  stored 0 places (12 already in this release) · 0 chains skipped (no lead) · 0 orgs and 0 sites made")
	next := mustRun(t, "leads", "places", "import", "overture", "--campaign", "cafes-loyalty", "--release", "2026-11-18.0",
		filepath.Join(dir, "places.demo.ndjson"))
	wantLines(t, "next release", next, "  stored 12 places (0 already in this release) · 7 chains skipped (no lead) · 0 orgs and 0 sites made")

	// The probe reads place sites with cafe@1 (fixtures only, no network).
	probeOut := sitesOut(t, "leads", "sites", "probe", "--dry-run", "--fixtures", cafeFixtures(t))
	if !strings.Contains(probeOut, "· 4 sites probed") {
		t.Fatalf("probe:\n%s", probeOut)
	}
	var cafeProbes int
	if err := owner.QueryRow(ctx, `SELECT count(*) FROM site_probe WHERE derived_by = 'cafe@1' AND outcome = 'ok'`).Scan(&cafeProbes); err != nil || cafeProbes != 4 {
		t.Fatalf("cafe@1 probes %d (%v), want 4", cafeProbes, err)
	}

	build := mustRun(t, "leads", "build", "--campaign", "cafes-loyalty")
	wantLines(t, "café build", build,
		"leads build: 3 leads (3 new · 0 refreshed · 0 past new, kept) from 5 orgs · fixture leads (reserved names, never in a real batch) 3",
		"  cafe_no_digital_loyalty: none 3",
		"no lead: already on Webbership Loyalty 1 · kind off: cafe_no_website 1",
	)
	var reason, lang string
	if err := owner.QueryRow(ctx, `SELECT f.subject->>'reason', f.subject->>'language' FROM lead l JOIN finding f ON f.id = l.finding_id
		JOIN org o ON o.id = l.org_id WHERE o.domain = 'cafeneaua-exemplu.example'`).Scan(&reason, &lang); err != nil ||
		reason != "no digital loyalty on the homepage" || lang != "ro" {
		t.Errorf("RO café finding: reason %q language %q (%v)", reason, lang, err)
	}
	// The cold-start twin of the build: nothing changes on a second run.
	wantLines(t, "café build again", mustRun(t, "leads", "build", "--campaign", "cafes-loyalty"),
		"leads build: 3 leads (0 new · 0 refreshed · 0 past new, kept) from 5 orgs · fixture leads (reserved names, never in a real batch) 3")
}

// TestPlacesLeaveDrupalBuildAlone is the contract drift test: the Drupal campaign's build
// prints the same and makes the same leads with a café import (and its cafe@1 probes) in
// the database as without.
func TestPlacesLeaveDrupalBuildAlone(t *testing.T) {
	type leadRow struct{ domain, kind, channel string }
	drupal := func(withPlaces bool) (string, []leadRow) {
		dir := leadsDB(t)
		cafesDataDir(t, dir)
		batchFixture(t)
		if withPlaces {
			importDemoPlaces(t, dir)
			sitesOut(t, "leads", "sites", "probe", "--dry-run", "--fixtures", cafeFixtures(t))
		} else {
			mustRun(t, "leads", "campaigns", "register")
		}
		out := mustRun(t, "leads", "build")
		rs, err := storetest.OwnerConn(t).Query(context.Background(),
			`SELECT o.domain, l.kind, l.channel FROM lead l JOIN org o ON o.id = l.org_id WHERE l.campaign_id = 1 ORDER BY o.domain`)
		if err != nil {
			t.Fatal(err)
		}
		defer rs.Close()
		var got []leadRow
		for rs.Next() {
			var r leadRow
			if err := rs.Scan(&r.domain, &r.kind, &r.channel); err != nil {
				t.Fatal(err)
			}
			got = append(got, r)
		}
		return out, got
	}
	before, beforeLeads := drupal(false)
	after, afterLeads := drupal(true)
	if before != after {
		t.Errorf("Drupal build output changed with places:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	if len(beforeLeads) == 0 || len(beforeLeads) != len(afterLeads) {
		t.Fatalf("Drupal leads before %v, after %v", beforeLeads, afterLeads)
	}
	for i := range beforeLeads {
		if beforeLeads[i] != afterLeads[i] {
			t.Errorf("Drupal lead %d: before %v, after %v", i, beforeLeads[i], afterLeads[i])
		}
	}
}

// TestCafeShadowBatchAndRegion: the example campaign checks, and a shadow batch drafts the
// café leads from its templates: the GB café gets the default sequence's first email with
// no link, the RO café the call-first sequence (its emails in Romanian, the first of them
// held to the no-link rule at step 2), and the US café's call is drafted because the
// place import stored its state (IL): a US café with no region would be "region unknown".
func TestCafeShadowBatchAndRegion(t *testing.T) {
	ctx := context.Background()
	dir := leadsDB(t)
	drawFromAll(t, dir)
	cafesDataDir(t, dir)
	t.Setenv("LEADHOUND_OUTREACH", "")
	now := time.Date(2026, 10, 27, 10, 0, 0, 0, time.UTC)
	saved := leadsNow
	leadsNow = func() time.Time { return now }
	t.Cleanup(func() { leadsNow = saved })
	seedMailbox(t, leadstoretest.Q(t), testUser, withDNSPass())

	if out := mustRun(t, "leads", "campaigns", "check"); !strings.Contains(out, "cafes-loyalty") {
		t.Errorf("campaigns check does not name cafes-loyalty:\n%s", out)
	}
	importDemoPlaces(t, dir)
	sitesOut(t, "leads", "sites", "probe", "--dry-run", "--fixtures", cafeFixtures(t))
	for _, d := range []string{"harbour-lane-coffee.example", "bean-house.example", "cafeneaua-exemplu.example"} {
		cafeRoute(t, d)
	}
	orgPhone(t, "bean-house.example", "+13125550103")
	orgPhone(t, "cafeneaua-exemplu.example", "+40700000104")
	mustRun(t, "leads", "build", "--campaign", "cafes-loyalty")

	// The call rule reads the stored region.
	lq := leadstoretest.Q(t)
	cands, err := lq.CandidateLeads(ctx, leadstore.CandidateOptions{IncludeReserved: true})
	if err != nil {
		t.Fatal(err)
	}
	var us *leadstore.Candidate
	for i := range cands {
		if cands[i].Domain == "bean-house.example" {
			us = &cands[i]
		}
	}
	if us == nil || us.Region != "US-IL" {
		t.Fatalf("US café candidate %+v, want region US-IL", us)
	}
	facts := callFacts{Country: "US", Number: "+13125550103", Org: compliance.OrgFacts{EntityType: compliance.EntityCompany, Region: us.Region}}
	if v := callVerdictFor(compliance.Default(), facts, now); v.Rule == compliance.RuleRegionUnknown {
		t.Errorf("US café with region IL: %+v", v)
	}
	facts.Org.Region = ""
	if v := callVerdictFor(compliance.Default(), facts, now); v.Rule != compliance.RuleRegionUnknown {
		t.Errorf("US café with no region: rule %q, want region_unknown", v.Rule)
	}

	out := mustRun(t, "leads", "batch", "new", "--campaign", "cafes-loyalty")
	type msg struct {
		step               int
		channel, seq, body string
	}
	rows := func(domain string) []msg {
		t.Helper()
		rs, err := storetest.OwnerConn(t).Query(ctx,
			`SELECT m.step, m.channel, coalesce(m.sequence_name, ''), m.body_text FROM outreach_message m
			 JOIN lead l ON l.id = m.lead_id JOIN org o ON o.id = l.org_id WHERE o.domain = $1 ORDER BY m.step`, domain)
		if err != nil {
			t.Fatal(err)
		}
		defer rs.Close()
		var got []msg
		for rs.Next() {
			var m msg
			if err := rs.Scan(&m.step, &m.channel, &m.seq, &m.body); err != nil {
				t.Fatal(err)
			}
			got = append(got, m)
		}
		return got
	}
	gb := rows("harbour-lane-coffee.example")
	if len(gb) == 0 || gb[0].step != 0 || gb[0].channel != "email" || strings.Contains(gb[0].body, "https://") ||
		!strings.Contains(gb[0].body, "Worth 10 minutes to see it on your phone?") {
		t.Fatalf("GB café rows %+v\nbatch new said:\n%s", gb, out)
	}
	ro := rows("cafeneaua-exemplu.example")
	shape := ""
	for _, m := range ro {
		shape += m.channel + ":" + m.seq + " "
	}
	if shape != "phone:call_first phone:call_first email:call_first email:call_first " {
		t.Fatalf("RO café rows %q\nbatch new said:\n%s", shape, out)
	}
	if !strings.Contains(ro[2].body, "Mulțumesc pentru discuție") || strings.Contains(ro[2].body, "https://") || strings.Contains(ro[2].body, "!") {
		t.Errorf("RO first email:\n%s", ro[2].body)
	}
	usCall := false
	for _, m := range rows("bean-house.example") {
		if m.channel == leadstore.ChannelPhone {
			usCall = true
		}
	}
	if !usCall {
		t.Errorf("US café (IL) got no call row\nbatch new said:\n%s", out)
	}
}
