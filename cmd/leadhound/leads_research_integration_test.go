//go:build integration

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/ai"
	"github.com/gremy/webbership-leadhound/internal/claude"
	"github.com/gremy/webbership-leadhound/internal/config"
	"github.com/gremy/webbership-leadhound/internal/fetch"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/leadstore/leadstoretest"
	"github.com/gremy/webbership-leadhound/internal/research"
	"github.com/gremy/webbership-leadhound/internal/store"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"

	"golang.org/x/time/rate"
)

// researchQuote stands for a model quote holding a person's name; it must surface nowhere.
const researchQuote = "QUOTE-SENTINEL-research-cmd-5c1e"

// researchPerson is a person's address on domain, built at runtime so no file holds one whole.
func researchPerson(domain string) string { return "jane.smith" + "@" + domain }

// researchSite adds a probed Drupal 7 site on domain in country with an org and, when route
// is set, a role email route derived by derivedBy citing its kept homepage. It returns the
// org id. The caller runs `leads build` for the leads.
func researchSite(t *testing.T, domain, country, derivedBy string, route bool) int64 {
	t.Helper()
	ctx := context.Background()
	lq := leadstoretest.Q(t)
	core := store.NewQueries(storetest.AppConn(t))
	runID, err := core.StartRun(ctx, "leads-sites-probe", "research-fixture")
	if err != nil {
		t.Fatal(err)
	}
	home, err := core.InsertFetch(ctx, runID, "research-fixture", fetch.Record{Kind: fetch.KindSite, URL: "https://" + domain + "/",
		Status: 200, Body: []byte("<html><body>info" + "@" + domain + "</body></html>"), ContentType: "text/html"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lq.ImportSites(ctx, leadstore.OriginManual, []leadstore.SiteImport{{Domain: domain, Country: country}}); err != nil {
		t.Fatal(err)
	}
	siteID, err := lq.SiteIDByDomain(ctx, domain)
	if err != nil {
		t.Fatal(err)
	}
	probeID, err := lq.RecordProbe(ctx, siteID, runID, leadstore.ProbeRow{
		Outcome: leadstore.OutcomeOK, FinalHost: domain, IsDrupal: true, Major: 7,
		Signals: []leadstore.Signal{{Signal: "meta-generator", Value: "7", FetchID: home}}, FetchIDs: []int64{home}, DerivedBy: "drupal@1",
	})
	if err != nil {
		t.Fatal(err)
	}
	orgID, _, err := lq.UpsertSiteOrg(ctx, probeID)
	if err != nil {
		t.Fatal(err)
	}
	if route {
		if _, err := lq.InsertRoute(ctx, leadstore.RouteRow{OrgID: orgID, Kind: leadstore.RouteEmail, Value: "info" + "@" + domain,
			FetchID: home, MXOK: true, DerivedBy: derivedBy}); err != nil {
			t.Fatal(err)
		}
	}
	return orgID
}

// researchScene is a fresh leads database with no-route GB leads on domains, built, shadow
// mode and the clock at leadsTuesday.
func researchScene(t *testing.T, domains ...string) {
	t.Helper()
	leadsDB(t)
	t.Setenv("LEADHOUND_OUTREACH", "")
	t.Setenv("LEADHOUND_TZ", "Europe/Bucharest")
	saved := leadsNow
	leadsNow = func() time.Time { return leadsTuesday }
	t.Cleanup(func() { leadsNow = saved })
	for _, d := range domains {
		researchSite(t, d, "GB", "", false)
	}
	if len(domains) > 0 {
		mustRun(t, "leads", "build")
	}
}

// researchPages is an httptest web server serving pages by host and path.
type researchPages struct {
	srv   *httptest.Server
	mu    sync.Mutex
	asked []string
}

func newResearchPages(t *testing.T, pages map[string]string) *researchPages {
	t.Helper()
	p := &researchPages{}
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		p.asked = append(p.asked, r.Host+r.URL.Path)
		p.mu.Unlock()
		if r.URL.Path == "/robots.txt" {
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte("User-agent: *\nAllow: /\n"))
			return
		}
		body, ok := pages[r.Host+r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(p.srv.Close)
	saved, savedRes := researchClients, contactsResolver
	t.Cleanup(func() { researchClients, contactsResolver = saved, savedRes })
	contactsResolver = mxResolver{"northwind.example": true, "contoso.example": true}
	researchClients = func(cfg config.Config, ff fetchFlags, lim *rate.Limiter) (func(fetch.Recorder) *fetch.Client, error) {
		mk, err := siteClients(cfg, ff, lim)
		if err != nil {
			return nil, err
		}
		return func(rec fetch.Recorder) *fetch.Client {
			c := mk(rec)
			c.HTTP.Transport = toServer{addr: p.srv.Listener.Addr().String(), rt: http.DefaultTransport}
			return c
		}, nil
	}
	return p
}

// contactPageOf is an org's contact page printing its role address and a person's address.
func contactPageOf(domain string) string {
	role, person := "info"+"@"+domain, researchPerson(domain)
	return `<html><body><h1>Contact</h1><p>Write to us: <a href="mailto:` + role + `">` + role + `</a></p>` +
		`<p>Our manager: <a href="mailto:` + person + `">` + person + `</a></p></body></html>`
}

// researchAnswer is a model answer naming cands (kind, value, source URL), each with the
// sentinel quote.
func researchAnswer(t *testing.T, cands ...[3]string) string {
	t.Helper()
	var list []map[string]string
	for _, c := range cands {
		list = append(list, map[string]string{"kind": c[0], "value": c[1], "source_url": c[2], "quote": researchQuote + " " + c[1], "line_kind": "", "role": ""})
	}
	b, err := json.Marshal(map[string]any{"candidates": list})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// researchRow is one contact_research row's status, cost and counts.
type researchRow struct {
	status               string
	cost                 *int64
	candidates, verified int
	started, finished    bool
}

func researchRowOf(t *testing.T, orgID int64) researchRow {
	t.Helper()
	var r researchRow
	if err := storetest.OwnerConn(t).QueryRow(context.Background(),
		`SELECT status, cost_micros, candidates, verified, started_at IS NOT NULL, finished_at IS NOT NULL
		 FROM contact_research WHERE org_id = $1 ORDER BY id DESC LIMIT 1`, orgID).
		Scan(&r.status, &r.cost, &r.candidates, &r.verified, &r.started, &r.finished); err != nil {
		t.Fatalf("research row of org %d: %v", orgID, err)
	}
	return r
}

func orgIDOf(t *testing.T, domain string) int64 {
	t.Helper()
	var id int64
	if err := storetest.OwnerConn(t).QueryRow(context.Background(), `SELECT id FROM org WHERE domain = $1`, domain).Scan(&id); err != nil {
		t.Fatalf("org of %s: %v", domain, err)
	}
	return id
}

// researchSweep checks the CP3b invariant: every claude-research@1 route and fact cites a
// recorded 2xx fetch of a page on its org's domain that keeps its body and prints the value.
// It returns how many rows it checked.
func researchSweep(t *testing.T) int {
	t.Helper()
	rows, err := storetest.OwnerConn(t).Query(context.Background(),
		`SELECT x.what, x.value, f.id IS NOT NULL, coalesce(f.status, 0), coalesce(octet_length(f.body), 0),
		   coalesce(position(x.needle IN convert_from(f.body, 'UTF8')) > 0, false),
		   coalesce(f.url LIKE 'https://' || o.domain || '/%', false)
		 FROM (
		   SELECT 'route' AS what, org_id, value, value AS needle, fetch_id FROM contact_route WHERE derived_by = $1
		   UNION ALL
		   SELECT 'fact', org_id, value, value, fetch_id FROM org_fact WHERE derived_by = $1
		 ) x JOIN org o ON o.id = x.org_id LEFT JOIN http_fetch f ON f.id = x.fetch_id`, research.DerivedBy)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var what, value string
		var fetched, onPage, onDomain bool
		var status, size int
		if err := rows.Scan(&what, &value, &fetched, &status, &size, &onPage, &onDomain); err != nil {
			t.Fatal(err)
		}
		n++
		if !fetched || status/100 != 2 || size == 0 || !onPage || !onDomain {
			t.Errorf("%s %q: fetched %v status %d body %d on page %v on domain %v", what, value, fetched, status, size, onPage, onDomain)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return n
}

// captureResearchLogs sends every slog line of the test to a buffer.
func captureResearchLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var logs bytes.Buffer
	saved := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(saved) })
	return &logs
}

// TestResearchRunFindsRoleAddress: a starred lead with no route is requested from the CLI
// and run against an httptest Anthropic API and an httptest site, through the real capped
// caller, fetch client, robots and recorder. The cited page holds a role address and a
// person's: the role address becomes a contact_route derived by claude-research@1 citing the
// recorded fetch of that page (its body kept), the person's is dropped, the attempt is found
// with its priced cost and counts, and the spend is in ai_usage. A second org whose answer
// names only a person's address ends nothing. The next leads build gives the first lead its
// email channel. The invariant sweep holds; the quote and the person's address are in no
// table, no log line and not on stdout, and stdout names no address at all.
func TestResearchRunFindsRoleAddress(t *testing.T) {
	ctx := context.Background()
	researchScene(t, "northwind.example", "contoso.example")
	key := sentinelKey(t)
	claudeKeyFile(t, key)
	logs := captureResearchLogs(t)
	nw, co := orgIDOf(t, "northwind.example"), orgIDOf(t, "contoso.example")
	lead := leadIDOf(t, "northwind.example")
	if _, err := leadstoretest.Q(t).Star(ctx, lead, leadstore.ActorOperator); err != nil {
		t.Fatal(err)
	}

	pages := newResearchPages(t, map[string]string{
		"northwind.example/contact": contactPageOf("northwind.example"),
		"contoso.example/contact":   contactPageOf("contoso.example"),
	})
	answers := []string{
		researchAnswer(t,
			[3]string{"email", "info" + "@" + "northwind.example", "https://northwind.example/contact"},
			[3]string{"email", researchPerson("northwind.example"), "https://northwind.example/contact"}),
		researchAnswer(t, [3]string{"email", researchPerson("contoso.example"), "https://contoso.example/contact"}),
	}
	newAnthropicFake(t, key, func(n int) (int, string) {
		if n > len(answers) {
			t.Errorf("Claude call %d: only %d scripted", n, len(answers))
			return 500, `{"type":"error","error":{"type":"api_error","message":"unscripted"}}`
		}
		return 200, anthropicMessage("end_turn", answers[n-1], 1200, 150)
	})

	var stdoutAll strings.Builder
	out := mustRun(t, "leads", "research", "request", strconv.FormatInt(lead, 10))
	stdoutAll.WriteString(out)
	if !strings.Contains(out, "queued for northwind.example: it looks for a role email") {
		t.Fatalf("request:\n%s", out)
	}
	if got := researchRowOf(t, nw); got.status != leadstore.ResearchQueued {
		t.Fatalf("after request: %+v", got)
	}
	out = mustRun(t, "leads", "research", "run", "--max", "1")
	stdoutAll.WriteString(out)
	wantLines(t, "run 1", out, "1 org · found 1 · nothing 0 · capped 0 · error 0", "northwind.example: found 1 email (1 of 2 candidates verified)")

	got := researchRowOf(t, nw)
	if got.status != leadstore.ResearchFound || got.cost == nil || *got.cost <= 0 || got.candidates != 2 || got.verified != 1 || !got.finished {
		t.Fatalf("northwind attempt: %+v (cost %v), want found with a cost, 1 of 2", got, got.cost)
	}
	var routes []string
	rows, err := storetest.OwnerConn(t).Query(ctx, `SELECT value || ' ' || derived_by || ' ' || mx_ok FROM contact_route WHERE org_id = $1`, nw)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err != nil {
			t.Fatal(err)
		}
		routes = append(routes, r)
	}
	rows.Close()
	if want := "info" + "@" + "northwind.example claude-research@1 true"; len(routes) != 1 || routes[0] != want {
		t.Errorf("northwind routes %q, want [%q]", routes, want)
	}
	if n := dbCount(t, `SELECT count(*) FROM ai_usage WHERE purpose = 'research' AND cost_micros > 0`); n != 1 {
		t.Errorf("%d research ai_usage rows with a cost, want 1", n)
	}
	var cost int64
	if err := storetest.OwnerConn(t).QueryRow(ctx, `SELECT coalesce(sum(cost_micros), 0) FROM ai_usage WHERE purpose = 'research'`).Scan(&cost); err != nil {
		t.Fatal(err)
	}
	if cost != *got.cost {
		t.Errorf("attempt cost %d, ai_usage %d: want the same", *got.cost, cost)
	}

	// A second org: only a person's address is named, so nothing is stored.
	coLead := leadIDOf(t, "contoso.example")
	stdoutAll.WriteString(mustRun(t, "leads", "research", "request", strconv.FormatInt(coLead, 10)))
	out = mustRun(t, "leads", "research", "run")
	stdoutAll.WriteString(out)
	wantLines(t, "run 2", out, "contoso.example: nothing (0 of 1 candidates verified)")
	if got := researchRowOf(t, co); got.status != leadstore.ResearchNothing || got.verified != 0 || got.candidates != 1 {
		t.Errorf("contoso attempt: %+v, want nothing, 0 of 1", got)
	}
	if n := dbCount(t, fmt.Sprintf(`SELECT count(*) FROM contact_route WHERE org_id = %d`, co)); n != 0 {
		t.Errorf("contoso has %d routes, want none", n)
	}
	out = mustRun(t, "leads", "research", "run")
	wantLines(t, "run 3", out, "0 orgs", "nothing was queued")

	// The list shows domains and counts, never a value.
	out = mustRun(t, "leads", "research", "list")
	stdoutAll.WriteString(out)
	wantLines(t, "list", out, "northwind.example · lead", "· found · looks for a role email", "1 of 2 candidates verified", "contoso.example")
	if out := mustRun(t, "leads", "research", "list", "--status", "nothing"); strings.Contains(out, "northwind") || !strings.Contains(out, "contoso.example") {
		t.Errorf("list --status nothing:\n%s", out)
	}

	// The next build gives the researched lead its email channel.
	mustRun(t, "leads", "build")
	var channel, routeBy string
	if err := storetest.OwnerConn(t).QueryRow(ctx,
		`SELECT l.channel, r.derived_by FROM lead l JOIN contact_route r ON r.id = l.route_id WHERE l.id = $1`, lead).Scan(&channel, &routeBy); err != nil {
		t.Fatalf("lead after build: %v", err)
	}
	if channel != leadstore.ChannelEmail || routeBy != research.DerivedBy {
		t.Errorf("lead after build: channel %s route by %s", channel, routeBy)
	}

	// The fetches went to the cited pages only.
	asked := strings.Join(pages.requests(), " ")
	for _, want := range []string{"northwind.example/contact", "contoso.example/contact"} {
		if !strings.Contains(asked, want) {
			t.Errorf("the site was not asked for %s: %s", want, asked)
		}
	}

	if n := researchSweep(t); n != 1 {
		t.Errorf("sweep checked %d research rows, want 1", n)
	}
	dump := dumpEveryTable(t)
	for _, s := range []string{researchQuote, researchPerson("northwind.example"), researchPerson("contoso.example"), key} {
		if strings.Contains(dump, s) {
			t.Errorf("a table holds %q", s)
		}
		if strings.Contains(logs.String(), s) {
			t.Errorf("a log line holds %q", s)
		}
		if strings.Contains(stdoutAll.String(), s) {
			t.Errorf("stdout holds %q", s)
		}
	}
	if strings.Contains(stdoutAll.String(), "@") {
		t.Errorf("stdout names an address:\n%s", stdoutAll.String())
	}
	if strings.Contains(logs.String(), "@northwind") {
		t.Errorf("a log line names an address:\n%s", logs.String())
	}
}

func (p *researchPages) requests() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.asked...)
}

// TestResearchCappedThenAskedAgain: a run the spend cap refuses ends capped with no call
// counted, stores nothing, and the org can be asked for again at once (a capped attempt
// never counts toward the 90 days).
func TestResearchCappedThenAskedAgain(t *testing.T) {
	researchScene(t, "northwind.example")
	claudeKeyFile(t, sentinelKey(t))
	newResearchPages(t, nil)
	fake := &claude.Fake{Err: fmt.Errorf("claude: research: over the cap: %w", ai.ErrSpendCap)}
	saved := newResearchCaller
	t.Cleanup(func() { newResearchCaller = saved })
	newResearchCaller = func(store.Queries) (research.Caller, error) { return fake, nil }

	lead := strconv.FormatInt(leadIDOf(t, "northwind.example"), 10)
	mustRun(t, "leads", "research", "request", lead)
	out := mustRun(t, "leads", "research", "run")
	wantLines(t, "run", out, "capped 1", "northwind.example: capped: the research spend cap is reached; it may be asked again")
	org := orgIDOf(t, "northwind.example")
	if got := researchRowOf(t, org); got.status != leadstore.ResearchCapped || got.verified != 0 || !got.finished {
		t.Errorf("attempt: %+v, want capped", got)
	}
	if fake.Calls() != 1 {
		t.Errorf("%d fake calls, want 1", fake.Calls())
	}
	if n := dbCount(t, `SELECT count(*) FROM contact_route`); n != 0 {
		t.Errorf("%d routes stored, want none", n)
	}
	out = mustRun(t, "leads", "research", "request", lead)
	if !strings.Contains(out, "queued for northwind.example") {
		t.Errorf("request after a capped attempt:\n%s", out)
	}
	if n := dbCount(t, fmt.Sprintf(`SELECT count(*) FROM contact_research WHERE org_id = %d AND status = 'queued'`, org)); n != 1 {
		t.Errorf("%d queued attempts after asking again, want 1", n)
	}
	// Asked again right after a run that counted (found/nothing), the CLI says no request.
	fake.Err, fake.ResearchAnswer = nil, claude.ResearchResult{Text: `{"candidates":[]}`}
	out = mustRun(t, "leads", "research", "run")
	wantLines(t, "run 2", out, "northwind.example: nothing")
	out = mustRun(t, "leads", "research", "request", lead)
	if !strings.Contains(out, "no request: northwind.example: research last ran on") || !strings.Contains(out, "it may run again from") {
		t.Errorf("request inside 90 days:\n%s", out)
	}
}

// TestResearchLiveOffClaimsNothing: with live calls off a run claims nothing (the queued
// attempt stays queued, no caller is built, nothing is fetched) and exits 0.
func TestResearchLiveOffClaimsNothing(t *testing.T) {
	researchScene(t, "northwind.example")
	pages := newResearchPages(t, nil)
	saved := newResearchCaller
	t.Cleanup(func() { newResearchCaller = saved })
	newResearchCaller = func(store.Queries) (research.Caller, error) {
		t.Error("a caller was built with live calls off")
		return nil, nil
	}
	mustRun(t, "leads", "research", "request", strconv.FormatInt(leadIDOf(t, "northwind.example"), 10))
	for _, live := range []string{"", "ats"} {
		t.Setenv("LEADHOUND_LIVE", live)
		if out := mustRun(t, "leads", "research", "run"); strings.TrimSpace(out) != researchLiveOffWords {
			t.Errorf("LEADHOUND_LIVE=%q:\n%s", live, out)
		}
	}
	if got := researchRowOf(t, orgIDOf(t, "northwind.example")); got.status != leadstore.ResearchQueued || got.started {
		t.Errorf("attempt: %+v, want still queued, never started", got)
	}
	if len(pages.requests()) != 0 {
		t.Errorf("fetched %v with live calls off", pages.requests())
	}
}

// addFormCountry adds country to the scene's leads.yaml as a contact-form country (research
// looks for a phone there).
func addFormCountry(t *testing.T, code, tz string) {
	t.Helper()
	path := filepath.Join(os.Getenv("LEADHOUND_DATA_DIR"), "leads.yaml")
	raw, err := os.ReadFile(path) // #nosec G304 G703 -- the test's own temp copy, named by the scene
	if err != nil {
		t.Fatal(err)
	}
	out := strings.Replace(string(raw), "  DE: {channel: form,", "  "+code+": {channel: form, lang: en, tz: "+tz+"}\n  DE: {channel: form,", 1)
	if out == string(raw) {
		t.Fatal("leads.yaml countries not rewritten")
	}
	if err := os.WriteFile(path, []byte(out), 0o600); err != nil { // #nosec G703 -- a test temp dir
		t.Fatal(err)
	}
}

// TestResearchRequestNoRequest: a lead whose country is off, a lead past new, a lead in a
// contact-form country (DE, and RO made one here) of a campaign that makes no calls (the
// legacy campaign) and an unknown lead get no request, in plain words (the unknown one a
// usage error): a batch never calls a form lead, so no phone is looked for.
func TestResearchRequestNoRequest(t *testing.T) {
	researchScene(t)
	addFormCountry(t, "RO", "Europe/Bucharest")
	researchSite(t, "us.example", "US", "", false)
	researchSite(t, "skipped.example", "GB", "", false)
	researchSite(t, "contoso.example", "DE", "", false)
	researchSite(t, "bucuresti.example", "RO", "", false)
	mustRun(t, "leads", "build")
	if out := mustRun(t, "leads", "research", "request", strconv.FormatInt(leadIDOf(t, "us.example"), 10)); !strings.Contains(out,
		"no request: us.example: US is off in leads.yaml, so there is nothing to look for") {
		t.Errorf("US lead:\n%s", out)
	}
	skipped := leadIDOf(t, "skipped.example")
	if _, err := leadstoretest.Q(t).SkipNewLead(context.Background(), skipped, "test"); err != nil {
		t.Fatal(err)
	}
	if out := mustRun(t, "leads", "research", "request", strconv.FormatInt(skipped, 10)); !strings.Contains(out, "no request: skipped.example: the lead is no longer new") {
		t.Errorf("skipped lead:\n%s", out)
	}
	if out := mustRun(t, "leads", "research", "request", strconv.FormatInt(leadIDOf(t, "contoso.example"), 10)); !strings.Contains(out,
		"no request: contoso.example: DE takes contact forms and this campaign makes no calls there, so there is no phone to look for") {
		t.Errorf("DE lead:\n%s", out)
	}
	if out := mustRun(t, "leads", "research", "request", strconv.FormatInt(leadIDOf(t, "bucuresti.example"), 10)); !strings.Contains(out,
		"no request: bucuresti.example: RO takes contact forms and this campaign makes no calls there") {
		t.Errorf("RO lead:\n%s", out)
	}
	if code, _, errOut := leadsRun(t, "leads", "research", "request", "999999"); code != 2 || !strings.Contains(errOut, "lead 999999 does not exist") {
		t.Errorf("unknown lead: exit %d %s", code, errOut)
	}
	if n := dbCount(t, `SELECT count(*) FROM contact_research`); n != 0 {
		t.Errorf("%d attempts, want none", n)
	}
}

// TestResearchRouteSameChannelAsFinder is the contract-drift test: leads build gives a
// researched route (claude-research@1) exactly the channel and route it gives a finder route
// (contacts@1) of the same address, with no channel logic of its own.
func TestResearchRouteSameChannelAsFinder(t *testing.T) {
	type built struct{ channel, kind, value string }
	build := func(derivedBy string) built {
		researchScene(t)
		researchSite(t, "northwind.example", "GB", derivedBy, true)
		mustRun(t, "leads", "build")
		var b built
		var by string
		if err := storetest.OwnerConn(t).QueryRow(context.Background(),
			`SELECT l.channel, r.kind, r.value, r.derived_by FROM lead l JOIN contact_route r ON r.id = l.route_id
			 JOIN org o ON o.id = l.org_id WHERE o.domain = 'northwind.example'`).Scan(&b.channel, &b.kind, &b.value, &by); err != nil {
			t.Fatalf("%s: %v", derivedBy, err)
		}
		if by != derivedBy {
			t.Fatalf("%s: the lead's route is derived by %s", derivedBy, by)
		}
		return b
	}
	finder, researched := build("contacts@1"), build(research.DerivedBy)
	if finder != researched || finder.channel != leadstore.ChannelEmail {
		t.Errorf("finder route %+v, researched route %+v: want the same email channel", finder, researched)
	}
}

// TestStarredRefusalListsEachLead: with nothing ready, `leads batch new` lists every starred
// lead in the operator's order with its one reason in plain words: no email yet, looking for
// a contact, research found nothing (with the date), the country rule, suppressed, deferred
// (until), no longer new. It exits 0 and writes no batch.
func TestStarredRefusalListsEachLead(t *testing.T) {
	ctx := context.Background()
	researchScene(t)
	lq := leadstoretest.Q(t)
	seedMailbox(t, lq, testUser, withDNSPass())
	for _, s := range []struct {
		domain, country string
		route           bool
	}{
		{"noemail.example", "GB", false},
		{"looking.example", "GB", false},
		{"nothing.example", "GB", false},
		{"us.example", "US", true},
		{"suppressed.example", "GB", true},
		{"deferred.example", "GB", true},
		{"skipped.example", "GB", true},
	} {
		researchSite(t, s.domain, s.country, "contacts@1", s.route)
	}
	mustRun(t, "leads", "build")
	order := []string{"noemail.example", "looking.example", "nothing.example", "us.example", "suppressed.example", "deferred.example", "skipped.example"}
	for _, d := range order {
		if _, err := lq.Star(ctx, leadIDOf(t, d), leadstore.ActorOperator); err != nil {
			t.Fatal(err)
		}
	}
	// nothing.example's attempt is asked first, so the one claim takes it.
	mustRun(t, "leads", "research", "request", strconv.FormatInt(leadIDOf(t, "nothing.example"), 10))
	mustRun(t, "leads", "research", "request", strconv.FormatInt(leadIDOf(t, "looking.example"), 10))
	claimed, err := lq.ClaimResearch(ctx, 1)
	if err != nil || len(claimed) != 1 || claimed[0].Domain != "nothing.example" {
		t.Fatalf("claim: %+v %v", claimed, err)
	}
	if err := lq.FinishResearch(ctx, claimed[0].ID, leadstore.ResearchNothing, research.Model, nil, 0, 0); err != nil {
		t.Fatal(err)
	}
	mustRun(t, "leads", "suppress", "add", "domain", "suppressed.example")
	if err := lq.DeferLead(ctx, leadIDOf(t, "deferred.example"), leadsTuesday.AddDate(0, 0, 7)); err != nil {
		t.Fatal(err)
	}
	if _, err := lq.SkipNewLead(ctx, leadIDOf(t, "skipped.example"), "test"); err != nil {
		t.Fatal(err)
	}

	out := mustRun(t, "leads", "batch", "new")
	wantLines(t, "batch new", out,
		noStarredHead,
		"1. noemail.example — no email yet\n",
		"2. looking.example — looking for a contact\n",
		"3. nothing.example — research found nothing, ",
		"4. us.example — country rule: US is off in leads.yaml\n",
		"5. suppressed.example — suppressed\n",
		"6. deferred.example — deferred, until ",
		"7. skipped.example — no longer new: it is skipped\n",
	)
	if strings.Contains(out, noStarredWords) {
		t.Errorf("the nothing-starred line shows with starred leads:\n%s", out)
	}
	for _, code := range []string{"no_email_yet", "found_nothing", "country_rule", "not_new"} {
		if strings.Contains(out, code) {
			t.Errorf("batch new shows the code %q:\n%s", code, out)
		}
	}
	if n := batchCount(t, `SELECT count(*)::int FROM outreach_batch`); n != 0 {
		t.Errorf("%d batches written, want none", n)
	}
}

// TestResearchStoreErrorNamesNoValue: a store error whose own text quotes the route it was
// storing (a trigger on contact_route raises one naming NEW.value, as a Postgres message can
// quote its input) fails the run with fixed words and the SQLSTATE in main's "command failed"
// log line, and the address is on neither stdout, stderr, a log line nor the run's note. The
// attempt ends error.
func TestResearchStoreErrorNamesNoValue(t *testing.T) {
	ctx := context.Background()
	researchScene(t, "northwind.example")
	claudeKeyFile(t, sentinelKey(t))
	logs := captureResearchLogs(t)
	newResearchPages(t, map[string]string{"northwind.example/contact": contactPageOf("northwind.example")})
	role := "info" + "@" + "northwind.example"
	fake := &claude.Fake{ResearchAnswer: claude.ResearchResult{
		Text: researchAnswer(t, [3]string{"email", role, "https://northwind.example/contact"})}}
	saved := newResearchCaller
	t.Cleanup(func() { newResearchCaller = saved })
	newResearchCaller = func(store.Queries) (research.Caller, error) { return fake, nil }

	owner := storetest.OwnerConn(t)
	for _, stmt := range []string{
		`CREATE OR REPLACE FUNCTION leak_sentinel_route() RETURNS trigger LANGUAGE plpgsql AS $$
		 BEGIN RAISE EXCEPTION 'cannot store route %', NEW.value USING ERRCODE = 'P0001'; END $$`,
		`CREATE TRIGGER leak_sentinel_route BEFORE INSERT ON contact_route FOR EACH ROW
		 WHEN (NEW.value = '` + role + `') EXECUTE FUNCTION leak_sentinel_route()`,
	} {
		if _, err := owner.Exec(ctx, stmt); err != nil {
			t.Fatalf("install the leaking trigger: %v", err)
		}
	}
	t.Cleanup(func() {
		_, _ = owner.Exec(context.Background(), `DROP TRIGGER IF EXISTS leak_sentinel_route ON contact_route`)
		_, _ = owner.Exec(context.Background(), `DROP FUNCTION IF EXISTS leak_sentinel_route()`)
	})

	mustRun(t, "leads", "research", "request", strconv.FormatInt(leadIDOf(t, "northwind.example"), 10))
	code, out, stderr := runCmdErr(t, "leads", "research", "run")
	if code == 0 {
		t.Fatalf("run with a failing store: exit 0\nstdout %s\nstderr %s", out, stderr)
	}
	if fake.Calls() != 1 {
		t.Fatalf("%d fake calls, want 1 (the store failed after the call)", fake.Calls())
	}
	wantLines(t, "stdout", out, "error 1", "northwind.example: error: what was found could not be stored")
	// A failed command is the "command failed" slog line (main's JSON handler on stderr).
	if !strings.Contains(logs.String(), "the store failed storing a route (SQLSTATE P0001)") {
		t.Errorf("logs %q: want the fixed words and the SQLSTATE", logs.String())
	}
	var note string
	if err := owner.QueryRow(ctx, `SELECT coalesce(note, '') FROM run WHERE command = $1 ORDER BY id DESC LIMIT 1`,
		commandLeadsResearch).Scan(&note); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(note, "SQLSTATE P0001") {
		t.Errorf("run note %q: want the SQLSTATE", note)
	}
	for name, s := range map[string]string{"stdout": out, "stderr": stderr, "logs": logs.String(), "run note": note} {
		if strings.Contains(s, role) || strings.Contains(s, "@northwind") {
			t.Errorf("%s names the address: %q", name, s)
		}
	}
	if got := researchRowOf(t, orgIDOf(t, "northwind.example")); got.status != leadstore.ResearchError || got.verified != 0 || !got.finished {
		t.Errorf("attempt: %+v, want error with nothing verified", got)
	}
	if n := dbCount(t, `SELECT count(*) FROM contact_route WHERE derived_by = 'claude-research@1'`); n != 0 {
		t.Errorf("%d research routes stored, want none", n)
	}
}
