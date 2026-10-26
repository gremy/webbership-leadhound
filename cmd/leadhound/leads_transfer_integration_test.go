//go:build integration

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/gremy/webbership-leadhound/internal/fetch"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/leadstore/leadstoretest"
	"github.com/gremy/webbership-leadhound/internal/store"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

// The transfer test runs on one database: the fixture is exported, the database emptied
// (and its sequences moved on, so no id can line up by chance), then the export imported
// into it. In it .example stands for real domains and .invalid for the reserved TLDs.

// batchedOrg is the org whose lead is batched in the fixture and must arrive new.
const batchedOrg = "agency.example"

// transferFixture writes the source data and returns the alpha homepage's key, which the
// target gets a non-transfer decoy of.
func transferFixture(t *testing.T) (homeURL string, homeAt time.Time, homeSHA []byte) {
	t.Helper()
	ctx := context.Background()
	conn := storetest.AppConn(t)
	sq, lq := store.NewQueries(conn), leadstore.NewQueries(conn)
	seedMailbox(t, lq, testUser)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	runID, err := sq.StartRun(ctx, "leads-probe", sourceLeads)
	must(err)
	fetchRow := func(kind fetch.Kind, url string, status int, body string) int64 {
		t.Helper()
		rec := fetch.Record{Kind: kind, URL: url, Status: status, Body: []byte(body), ContentType: "text/html", FetchedAt: time.Now()}
		if status == 0 {
			rec.Body, rec.Err = nil, "connection refused"
		}
		id, err := sq.InsertFetch(ctx, runID, sourceLeads, rec)
		must(err)
		return id
	}
	rank := 5
	_, err = lq.ImportSites(ctx, leadstore.OriginManual, []leadstore.SiteImport{
		{Domain: "alpha.example", Country: "GB", Rank: &rank}, {Domain: "beta.example"}, {Domain: "gamma.example"},
		{Domain: "delta.example"}, {Domain: "reserved.invalid"},
	})
	must(err)
	_, err = lq.ImportSites(ctx, leadstore.OriginAgency, []leadstore.SiteImport{{Domain: "agency.example"}})
	must(err)
	site := func(d string) int64 {
		id, err := lq.SiteIDByDomain(ctx, d)
		must(err)
		return id
	}
	probe := func(siteDomain string, p leadstore.ProbeRow) int64 {
		p.DerivedBy = "drupal@1"
		id, err := lq.RecordProbe(ctx, site(siteDomain), runID, p)
		must(err)
		return id
	}

	fA0 := fetchRow(fetch.KindSite, "https://alpha.example/", 200, "<html>old alpha page</html>")
	probe("alpha.example", leadstore.ProbeRow{Outcome: leadstore.OutcomeOK, FetchIDs: []int64{fA0}})
	fA1 := fetchRow(fetch.KindSite, "https://alpha.example/", 200, "<html>drupal seven alpha</html>")
	fA2 := fetchRow(fetch.KindRobots, "https://alpha.example/robots.txt", 200, "User-agent: *")
	must(lq.InsertFetchHeader(ctx, fA1, http.Header{"X-Generator": {"Drupal 7 (https://www.drupal.org)"}}))
	pAlpha := probe("alpha.example", leadstore.ProbeRow{
		Outcome: leadstore.OutcomeOK, FinalHost: "www.alpha.example", IsDrupal: true, Major: 7, ExactVersion: "7.98",
		Signals:  []leadstore.Signal{{Signal: "generator", Value: "Drupal 7", FetchID: fA1}, {Signal: "robots", Value: "core", FetchID: fA2}},
		FetchIDs: []int64{fA1, fA2}, SiteName: "Alpha Ltd",
	})
	fB := fetchRow(fetch.KindSite, "https://beta.example/", 200, "")
	probe("beta.example", leadstore.ProbeRow{Outcome: leadstore.OutcomeOK, FetchIDs: []int64{fB}})
	fG := fetchRow(fetch.KindSite, "https://gamma.example/", 0, "")
	probe("gamma.example", leadstore.ProbeRow{Outcome: leadstore.OutcomeTransportError, FetchIDs: []int64{fG}})
	fR := fetchRow(fetch.KindSite, "https://reserved.invalid/", 200, "<html>drupal ten reserved</html>")
	pRes := probe("reserved.invalid", leadstore.ProbeRow{Outcome: leadstore.OutcomeOK, IsDrupal: true, Major: 10,
		Signals: []leadstore.Signal{{Signal: "generator", Value: "Drupal 10", FetchID: fR}}, FetchIDs: []int64{fR}})
	fetchRow(fetch.KindSite, "https://unreferenced.example/", 200, "<html>never carried</html>")

	alphaOrg, _, err := lq.UpsertSiteOrg(ctx, pAlpha)
	must(err)
	resOrg, _, err := lq.UpsertSiteOrg(ctx, pRes)
	must(err)
	fAgHome := fetchRow(fetch.KindSite, "https://agency.example/", 200, "<html>agency home</html>")
	ag, err := lq.UpsertAgency(ctx, leadstore.AgencyImport{Name: "Agency Co", Website: "https://agency.example/", Country: "GB"}, &fAgHome)
	must(err)
	must(lq.OrgSetSite(ctx, ag.OrgID, site("agency.example")))

	fAc := fetchRow(fetch.KindSite, "https://alpha.example/contact", 200, "<html>write to info</html>")
	fAgP := fetchRow(fetch.KindSite, "https://agency.example/upgrades", 200, "<html>we upgrade drupal sites</html>")
	fRc := fetchRow(fetch.KindSite, "https://reserved.invalid/contact", 200, "<html>reserved contact</html>")
	route := func(org int64, kind, value string, fid int64) int64 {
		_, err := lq.InsertRoute(ctx, leadstore.RouteRow{OrgID: org, Kind: kind, Value: value, FetchID: fid, MXOK: true, DerivedBy: "contacts@1"})
		must(err)
		routes, err := lq.RoutesFor(ctx, org)
		must(err)
		for _, r := range routes {
			if r.Value == value {
				return r.ID
			}
		}
		t.Fatalf("route %s not found", value)
		return 0
	}
	rAlpha := route(alphaOrg, leadstore.RouteEmail, "info@alpha.example", fAc)
	route(alphaOrg, leadstore.RouteForm, "https://alpha.example/contact", fAc)
	rAg := route(ag.OrgID, leadstore.RouteEmail, "hello@agency.example", fAgP)
	rRes := route(resOrg, leadstore.RouteEmail, "info@reserved.invalid", fRc)
	finding, err := lq.InsertAgencyFinding(ctx, leadstore.AgencyFindingRow{OrgID: ag.OrgID, URL: "https://agency.example/upgrades",
		Phrase: "we upgrade drupal sites", FetchID: fAgP, DerivedBy: "agencies@1"})
	must(err)
	for _, o := range []int64{alphaOrg, ag.OrgID, resOrg} {
		must(lq.RecordContactAttempt(ctx, o, runID, 1))
	}

	_, _, err = lq.UpsertLead(ctx, leadstore.LeadRow{OrgID: alphaOrg, Kind: leadstore.LeadUnsupported, ProbeID: &pAlpha,
		Channel: leadstore.ChannelEmail, RouteID: &rAlpha})
	must(err)
	agLead, _, err := lq.UpsertLead(ctx, leadstore.LeadRow{OrgID: ag.OrgID, Kind: leadstore.LeadAgencyPartner,
		AgencyFindingID: &finding, Channel: leadstore.ChannelEmail, RouteID: &rAg})
	must(err)
	_, _, err = lq.UpsertLead(ctx, leadstore.LeadRow{OrgID: resOrg, Kind: leadstore.LeadD10, ProbeID: &pRes,
		Channel: leadstore.ChannelEmail, RouteID: &rRes})
	must(err)
	_, err = lq.MarkContacted(ctx, "epsilon.example", "met at an event")
	must(err)
	// A live batch holding the agency lead: the batch stays behind, the lead arrives new.
	_, _, err = lq.CreateBatch(ctx, false, []leadstore.DraftRow{{LeadID: agLead, Step: 0, Channel: leadstore.ChannelEmail,
		To: "hello@agency.example", Subject: "Drupal", Body: "Hello", MessageID: "<m1@sender.example>", Slots: json.RawMessage(`{}`)}}, nil)
	must(err)
	must(lq.SetLeadState(ctx, agLead, leadstore.LeadBatched, "batch 1", leadstore.ActorSystem))
	_, err = lq.CreateSelftest(ctx, leadstore.SelftestDraft{To: "operator@sender.example", Subject: "selftest", Body: "selftest",
		MessageID: "<s1@sender.example>", Due: time.Now()})
	must(err)
	for _, s := range [][3]string{
		{leadstore.SuppressDomain, "blocked.example", leadstore.ReasonManual},
		{leadstore.SuppressDomain, "blocked.example", leadstore.ReasonOptOut},          // one value, a second reason: both cross
		{leadstore.SuppressAddress, "operator@sender.example", leadstore.ReasonOptOut}, // the selftest's own address
		{leadstore.SuppressDomain, "spam.invalid", leadstore.ReasonManual},
	} {
		_, err := lq.Suppress(ctx, s[0], s[1], s[2], "cli")
		must(err)
	}
	must(conn.QueryRow(ctx, `SELECT url, fetched_at, body_sha256 FROM http_fetch WHERE id = $1`, fA1).Scan(&homeURL, &homeAt, &homeSHA))
	return homeURL, homeAt, homeSHA
}

// fx renders the fetch aliased f whole, its header included.
const fx = `concat_ws('~', f.url, f.fetched_at, f.status, f.kind, f.source,
	encode(f.body_sha256, 'hex'), f.body_size, f.error, f.content_type, f.final_url,
	encode(f.body, 'escape'), (SELECT headers::text FROM fetch_header h WHERE h.fetch_id = f.id))`

const notReserved = `!~ '\.invalid$'`

// canonical renders every carried row of each kind by natural key, never by id, so a source
// and a target compare line for line. The batched lead's state_at and the event that moved
// it back to new are left out here and checked on their own.
func canonical(t *testing.T) map[string][]string {
	t.Helper()
	queries := map[string]string{
		"sites": `SELECT concat_ws('|', s.domain, s.country, s.origin, s.list_rank, s.added_at, s.state, p.created_at)
			FROM site s LEFT JOIN site_probe p ON p.id = s.last_probe_id WHERE s.domain ` + notReserved,
		"probes": `SELECT concat_ws('|', s.domain, p.created_at, p.outcome, p.final_host, p.is_drupal, p.major, p.exact_version,
			p.ambiguous, p.site_name, p.derived_by,
			(SELECT string_agg(` + fx + `, ',' ORDER BY u.o) FROM unnest(p.fetch_ids) WITH ORDINALITY u(id, o) JOIN http_fetch f ON f.id = u.id),
			(SELECT string_agg(concat_ws('=', e->>'signal', e->>'value', f.url, f.fetched_at), ',' ORDER BY e->>'signal')
			 FROM jsonb_array_elements(p.signals) e LEFT JOIN http_fetch f ON f.id = (e->>'fetch_id')::bigint))
			FROM site_probe p JOIN site s ON s.id = p.site_id WHERE s.domain ` + notReserved,
		"orgs": `SELECT concat_ws('|', o.domain, o.kind, o.name, o.country, CASE WHEN s.domain ` + notReserved + ` THEN s.domain END,
			o.source, o.created_at, ` + fx + `)
			FROM org o LEFT JOIN site s ON s.id = o.site_id LEFT JOIN http_fetch f ON f.id = o.source_fetch_id
			WHERE o.kind <> 'selftest' AND o.domain ` + notReserved,
		"routes": `SELECT concat_ws('|', o.domain, r.kind, r.value, r.mx_ok, r.derived_by, r.found_at, ` + fx + `)
			FROM contact_route r JOIN org o ON o.id = r.org_id JOIN http_fetch f ON f.id = r.fetch_id
			WHERE o.kind <> 'selftest' AND o.domain ` + notReserved,
		"findings": `SELECT concat_ws('|', o.domain, a.url, a.phrase, a.derived_by, a.created_at, ` + fx + `)
			FROM agency_finding a JOIN org o ON o.id = a.org_id JOIN http_fetch f ON f.id = a.fetch_id
			WHERE o.kind <> 'selftest' AND o.domain ` + notReserved,
		"attempts": `SELECT concat_ws('|', o.domain, a.routes_found, a.attempted_at)
			FROM contact_attempt a JOIN org o ON o.id = a.org_id WHERE o.kind <> 'selftest' AND o.domain ` + notReserved,
		"leads": `SELECT concat_ws('|', o.domain, l.kind, ps.domain, p.created_at, fo.domain, f.url, f.created_at, l.channel,
			ro.domain, r.kind, r.value, CASE l.state WHEN 'batched' THEN 'new' ELSE l.state END,
			CASE WHEN o.domain <> '` + batchedOrg + `' THEN l.state_at END, l.created_at)
			FROM lead l JOIN org o ON o.id = l.org_id
			LEFT JOIN site_probe p ON p.id = l.probe_id LEFT JOIN site ps ON ps.id = p.site_id
			LEFT JOIN agency_finding f ON f.id = l.agency_finding_id LEFT JOIN org fo ON fo.id = f.org_id
			LEFT JOIN contact_route r ON r.id = l.route_id LEFT JOIN org ro ON ro.id = r.org_id
			WHERE l.kind <> 'selftest' AND o.domain ` + notReserved,
		"events": `SELECT concat_ws('|', o.domain, e.state, e.reason, e.actor, e.at)
			FROM lead_event e JOIN lead l ON l.id = e.lead_id JOIN org o ON o.id = l.org_id
			WHERE l.kind <> 'selftest' AND o.domain ` + notReserved + ` AND e.reason <> '` + transferBatchedReason + `'`,
		"suppressions": `SELECT concat_ws('|', kind, value, reason, source, created_at) FROM suppression
			WHERE value ` + notReserved + ` AND value <> 'operator@sender.example'`,
	}
	conn := storetest.AppConn(t)
	out := map[string][]string{}
	for kind, q := range queries {
		rows, err := conn.Query(context.Background(), q)
		if err != nil {
			t.Fatalf("canonical %s: %v", kind, err)
		}
		lines, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatalf("canonical %s: %v", kind, err)
		}
		slices.Sort(lines)
		out[kind] = lines
	}
	return out
}

// answers are the lead queries' results by natural key: what CandidateLeads, DueSites,
// OrgHomepage and OrgsNeedingRoutes say.
type answers struct {
	Candidates []string
	Due        []string
	Homepages  []string
	NeedRoutes []string
}

func leadAnswers(t *testing.T, now time.Time) answers {
	t.Helper()
	ctx := context.Background()
	lq := leadstoretest.Q(t)
	var a answers
	cands, err := lq.CandidateLeads(ctx, leadstore.CandidateOptions{IncludeReserved: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cands {
		rank := -1
		if c.ListRank != nil {
			rank = *c.ListRank
		}
		a.Candidates = append(a.Candidates, fmt.Sprintf("%s|%s|%s|%s|%s|%s|%s|%d|%d|%s|%s|%s|%s",
			c.Domain, c.OrgName, c.Country, c.Kind, c.Channel, c.RouteKind, c.Route, rank, c.Major, c.ExactVersion,
			c.SiteName, c.Phrase, c.FindingURL))
	}
	for _, window := range []time.Duration{0, time.Hour} {
		due, err := lq.DueSites(ctx, 0, window, now)
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range due {
			a.Due = append(a.Due, fmt.Sprintf("%v %s %s", window, s.Domain, s.State))
		}
	}
	for _, d := range []string{"alpha.example", "agency.example", "epsilon.example"} {
		o, err := lq.OrgByDomain(ctx, d)
		if err != nil {
			t.Fatal(err)
		}
		h, err := lq.OrgHomepage(ctx, o.ID)
		if err != nil {
			t.Fatal(err)
		}
		a.Homepages = append(a.Homepages, fmt.Sprintf("%s|%s|%s|%s", d, h.URL, h.FinalURL, h.Body))
	}
	orgs, err := lq.OrgsNeedingRoutes(ctx, 0, now)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range orgs {
		a.NeedRoutes = append(a.NeedRoutes, o.Domain)
	}
	return a
}

func withoutReserved(lines []string) []string {
	var out []string
	for _, l := range lines {
		if !strings.Contains(l, ".invalid") && !strings.HasPrefix(l, "selftest-") {
			out = append(out, l)
		}
	}
	return out
}

var transferTables = []string{"run", "http_fetch", "fetch_header", "site", "site_probe", "org", "contact_route",
	"agency_finding", "contact_attempt", "lead", "lead_event", "suppression", "outreach_batch", "outreach_message"}

func tableCounts(t *testing.T) map[string]int {
	t.Helper()
	conn := storetest.AppConn(t)
	out := map[string]int{}
	for _, tbl := range transferTables {
		var n int
		if err := conn.QueryRow(context.Background(), "SELECT count(*) FROM "+tbl).Scan(&n); err != nil { // tbl is a test constant
			t.Fatal(err)
		}
		out[tbl] = n
	}
	return out
}

// fixtureValues are strings no transfer output may print.
var fixtureValues = []string{".example", ".invalid", "@", "http", "alpha", "agency", "Drupal 7", "selftest-"}

func noValues(t *testing.T, what, out string) {
	t.Helper()
	for _, v := range fixtureValues {
		if strings.Contains(out, v) {
			t.Errorf("%s prints a value (%q):\n%s", what, v, out)
		}
	}
}

func TestLeadsTransferRoundTrip(t *testing.T) {
	leadstoretest.Reset(t)
	t.Setenv("LEADHOUND_DATABASE_URL", storetest.AppDSN())
	saved := transferReservedTLDs
	transferReservedTLDs = []string{"invalid"}
	t.Cleanup(func() { transferReservedTLDs = saved })
	ctx := context.Background()

	homeURL, homeAt, homeSHA := transferFixture(t)
	dir := filepath.Join(t.TempDir(), "transfer")

	// A live message that may be going out stops the export before any file is written.
	ownerExecCmd(t, `UPDATE outreach_message m SET state = 'sending' FROM lead l WHERE l.id = m.lead_id AND l.kind <> 'selftest'`)
	code, out, errOut := leadsRun(t, "leads", "transfer", "export", dir)
	if code != 1 {
		t.Fatalf("export with a sending message: exit %d, stderr %q", code, errOut)
	}
	noValues(t, "refused export", out+errOut)
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("refused export left %s: %v", dir, err)
	}
	ownerExecCmd(t, `UPDATE outreach_message m SET state = 'draft' FROM lead l WHERE l.id = m.lead_id AND l.kind <> 'selftest'`)

	now := time.Now().Add(time.Minute)
	src := canonical(t)
	srcAnswers := leadAnswers(t, now)

	code, out, errOut = leadsRun(t, "leads", "transfer", "export", dir)
	if code != 0 {
		t.Fatalf("export exited %d: %s", code, errOut)
	}
	noValues(t, "export", out)
	wantLines(t, "export", out, "sites 5 ·", "fetches 8 (6 with a body)", "probes 4", "orgs 3", "routes 3",
		"findings 1", "attempts 2", "leads 3 (1 batched, carried as new)", "suppressions 2", "each mode 0600")
	var m transferManifest
	b, err := os.ReadFile(filepath.Join(dir, transferManifestName)) // #nosec G304 -- the test's temp dir
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &m); err != nil || len(m.Files) != len(transferFiles) {
		t.Fatalf("manifest: %v %+v", err, m)
	}
	for _, name := range append([]string{transferManifestName}, transferFiles...) {
		st, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm() != 0o600 {
			t.Errorf("%s mode %v, want 0600", name, st.Mode().Perm())
		}
	}

	// The target: empty lead tables, ids far from the source's, and a fetch of the server's
	// own (not a transfer's) with the alpha homepage's very key, which must not be reused.
	leadstoretest.Reset(t)
	for _, seq := range []string{"run", "http_fetch", "site", "site_probe", "org", "contact_route", "agency_finding",
		"contact_attempt", "lead", "lead_event", "suppression"} {
		ownerExecCmd(t, fmt.Sprintf("SELECT setval(pg_get_serial_sequence('%s', 'id'), 1000)", seq)) // a test constant
	}
	ownerExecCmd(t, `WITH r AS (INSERT INTO run (command) VALUES ('collect') RETURNING id)
		INSERT INTO http_fetch (run_id, source, kind, url, status, fetched_at, body_sha256)
		SELECT id, 'linkedin', 'site', $1, 200, $2, $3 FROM r`, homeURL, homeAt, homeSHA)
	before := tableCounts(t)

	code, out, errOut = leadsRun(t, "leads", "transfer", "import", dir)
	if code != 0 {
		t.Fatalf("import exited %d: %s", code, errOut)
	}
	noValues(t, "import", out)
	wantLines(t, "import", out, "(run 1002)", "sites: 5 added · 0 already there · 3 pointed at their last probe",
		"fetches: 8 added · 0 reused", "probes: 4 added", "suppressions: 2 added", "leads: 3 added", "events: ")

	dst := canonical(t)
	for kind, lines := range src {
		if !slices.Equal(lines, dst[kind]) {
			t.Errorf("%s differ:\nsource %q\ntarget %q", kind, lines, dst[kind])
		}
	}
	got := leadAnswers(t, now)
	want := srcAnswers
	want.Due = withoutReserved(want.Due)
	want.NeedRoutes = withoutReserved(want.NeedRoutes)
	want.Candidates = withoutReserved(want.Candidates)
	// The batched lead is the one difference: it arrives new, so a batch may draft it.
	if len(got.Candidates) != len(want.Candidates)+1 || !strings.HasPrefix(got.Candidates[len(got.Candidates)-1], batchedOrg+"|") {
		t.Errorf("candidates: target %q, source %q plus the batched lead", got.Candidates, want.Candidates)
	} else {
		got.Candidates = got.Candidates[:len(got.Candidates)-1]
	}
	for name, pair := range map[string][2][]string{
		"candidates": {want.Candidates, got.Candidates}, "due": {want.Due, got.Due},
		"homepages": {want.Homepages, got.Homepages}, "need routes": {want.NeedRoutes, got.NeedRoutes},
	} {
		if !slices.Equal(pair[0], pair[1]) {
			t.Errorf("%s: source %q, target %q", name, pair[0], pair[1])
		}
	}

	conn := storetest.AppConn(t)
	one := func(q string, args ...any) int {
		t.Helper()
		var n int
		if err := conn.QueryRow(ctx, q, args...).Scan(&n); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return n
	}
	checks := []struct {
		what string
		q    string
		want int
	}{
		{"batched lead arrives new", `SELECT count(*) FROM lead l JOIN org o ON o.id = l.org_id WHERE o.domain = '` + batchedOrg + `' AND l.state = 'new'`, 1},
		{"its move back to new", `SELECT count(*) FROM lead_event WHERE reason = '` + transferBatchedReason + `' AND state = 'new'`, 1},
		{"no batches or messages", `SELECT (SELECT count(*) FROM outreach_batch) + (SELECT count(*) FROM outreach_message)`, 0},
		{"no reserved or selftest row", `SELECT (SELECT count(*) FROM site WHERE domain ~ '\.invalid$')
			+ (SELECT count(*) FROM org WHERE kind = 'selftest' OR domain ~ '\.invalid$')
			+ (SELECT count(*) FROM lead WHERE kind = 'selftest')
			+ (SELECT count(*) FROM suppression WHERE value ~ '\.invalid$' OR value = 'operator@sender.example')
			+ (SELECT count(*) FROM http_fetch WHERE url LIKE '%.invalid/%')`, 0},
		{"no unreferenced fetch", `SELECT count(*) FROM http_fetch WHERE url LIKE '%unreferenced%'`, 0},
		{"the decoy kept beside the import", `SELECT count(*) FROM http_fetch WHERE url = $1 AND fetched_at = $2`, 2},
		{"alpha's homepage is the transfer's", `SELECT count(*) FROM org o JOIN http_fetch f ON f.id = o.source_fetch_id
			JOIN run r ON r.id = f.run_id WHERE o.domain = 'alpha.example' AND r.command = 'transfer'`, 1},
		{"probes, fetches and attempts hang from the transfer run", `SELECT (SELECT count(*) FROM site_probe WHERE run_id <> 1002)
			+ (SELECT count(*) FROM contact_attempt WHERE run_id <> 1002)
			+ (SELECT count(*) FROM http_fetch f JOIN run r ON r.id = f.run_id WHERE r.command = 'transfer' AND r.id <> 1002)`, 0},
		{"the transfer run finished", `SELECT count(*) FROM run WHERE command = 'transfer' AND finished_at IS NOT NULL`, 1},
	}
	for _, c := range checks {
		var args []any
		if strings.Contains(c.q, "$1") {
			args = []any{homeURL, homeAt}
		}
		if n := one(c.q, args...); n != c.want {
			t.Errorf("%s: %d, want %d", c.what, n, c.want)
		}
	}
	afterFirst := tableCounts(t)
	if afterFirst["run"] != before["run"]+1 {
		t.Errorf("runs: %d before, %d after the import, want one more", before["run"], afterFirst["run"])
	}

	// A second import changes no table's row count, run included.
	code, out, errOut = leadsRun(t, "leads", "transfer", "import", dir)
	if code != 0 {
		t.Fatalf("second import exited %d: %s", code, errOut)
	}
	noValues(t, "second import", out)
	wantLines(t, "second import", out, "(nothing new, no run row)", "sites: 0 added · 5 already there · 0 pointed",
		"leads: 0 added · 3 already there", "suppressions: 0 added · 2 already there")
	if again := tableCounts(t); !mapsEqual(again, afterFirst) {
		t.Errorf("second import changed row counts: %v, after the first %v", again, afterFirst)
	}

	// A manifest that does not match its files is refused before anything is written.
	tampered := filepath.Join(t.TempDir(), "tampered")
	if err := os.CopyFS(tampered, os.DirFS(dir)); err != nil {
		t.Fatal(err)
	}
	m.Files[0].Rows++
	b, _ = json.Marshal(m)
	if err := os.WriteFile(filepath.Join(tampered, transferManifestName), b, 0o600); err != nil { // #nosec G703 -- a test temp dir
		t.Fatal(err)
	}
	leadstoretest.Reset(t)
	code, out, errOut = leadsRun(t, "leads", "transfer", "import", tampered)
	if code != 2 || !strings.Contains(errOut, "does not match its manifest") {
		t.Errorf("tampered import: exit %d, stderr %q", code, errOut)
	}
	noValues(t, "tampered import", out+errOut)
	for tbl, n := range tableCounts(t) {
		if n != 0 {
			t.Errorf("tampered import wrote %d rows to %s", n, tbl)
		}
	}
}

func mapsEqual(a, b map[string]int) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func TestLeadsCheckConfig(t *testing.T) {
	dir := t.TempDir()
	if err := os.CopyFS(dir, os.DirFS("../../examples")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LEADHOUND_DATABASE_URL", "")
	t.Setenv("LEADHOUND_DATA_DIR", dir)
	code, out, errOut := leadsRun(t, "leads", "check-config", "--agencies", filepath.Join(dir, "agencies.demo.yaml"))
	if code != 0 {
		t.Fatalf("check-config exited %d: %s", code, errOut)
	}
	if want := "leads check-config: leads.yaml ok · templates: 3 sets (9 email steps, 3 forms) · 1 footer · " +
		"agencies: 2 with a website, 1 without · TLDs: 8 · countries: 2 · operator addresses: 1\n"; out != want {
		t.Errorf("check-config:\n got %q\nwant %q", out, want)
	}
	noValues(t, "check-config", out)

	// The default agency list is <data dir>/agencies.yaml; without it, a usage error.
	if code, _, errOut := leadsRun(t, "leads", "check-config"); code != 2 || !strings.Contains(errOut, "agencies.yaml") {
		t.Errorf("no agencies.yaml: exit %d, %q", code, errOut)
	}
	// A template missing is a problem too.
	if err := os.Remove(filepath.Join(dir, "leads-templates", "d10", "en", "1.txt")); err != nil {
		t.Fatal(err)
	}
	if code, out, errOut := leadsRun(t, "leads", "check-config", "--agencies", filepath.Join(dir, "agencies.demo.yaml")); code != 2 || out != "" ||
		!strings.Contains(errOut, "template d10/en/1.txt is missing") {
		t.Errorf("missing template: exit %d, stdout %q, stderr %q", code, out, errOut)
	}

	// Problems name the place and the kind, never the value: a quoted setting, an agency's
	// name, an address.
	cfgPath := filepath.Join(dir, "leads.yaml")
	cfg, err := os.ReadFile(cfgPath) // #nosec G304 -- the test's own temp copy
	if err != nil {
		t.Fatal(err)
	}
	broken := strings.Replace(string(cfg), "operator_addresses: [operator@sender.example]", "operator_addresses: [secretvalue1]", 1)
	broken = strings.Replace(broken, "templates_dir: leads-templates", "templates_dir: ../secretvalue2", 1)
	if broken == string(cfg) {
		t.Fatal("the example leads.yaml changed shape; update this test")
	}
	if err := os.WriteFile(cfgPath, []byte(broken), 0o600); err != nil { // #nosec G703 -- a test temp dir
		t.Fatal(err)
	}
	agencyPath := filepath.Join(dir, "agencies.bad.yaml")
	if err := os.WriteFile(agencyPath, []byte("- name: Secretvalue3 Agency\n  website: https://secretvalue4.example/\n  country: secretvalue5\n"), 0o600); err != nil { // #nosec G703 -- a test temp dir
		t.Fatal(err)
	}
	code, out, errOut = leadsRun(t, "leads", "check-config", "--agencies", agencyPath)
	if code != 2 || out != "" {
		t.Errorf("broken config: exit %d, stdout %q", code, out)
	}
	wantLines(t, "broken config", errOut, "sender.operator_addresses[0] is not an email address",
		`templates_dir: "…" must be a path inside the data dir`, `agencies.yaml: row 1: country "…" is not a two-letter`)
	if strings.Contains(errOut, "secretvalue") || strings.Contains(errOut, "Secretvalue") {
		t.Errorf("broken config prints a value: %s", errOut)
	}
}
