//go:build integration

package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"

	"golang.org/x/time/rate"

	"github.com/gremy/webbership-leadhound/internal/config"
	"github.com/gremy/webbership-leadhound/internal/fetch"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/leadstore/leadstoretest"
	"github.com/gremy/webbership-leadhound/internal/store"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

const enrichFixtureDir = "../../internal/enrich/testdata/run"

// enrichSite is one probed site with a lead: its kept homepage ("" keeps size and hash only)
// and, when contact is set, a contact page a form route was read from.
type enrichSite struct {
	domain, home, contact string
}

// enrichFixture writes, for each site, the rows the probe, the contact search and the build
// leave behind, and returns each org's id by domain.
func enrichFixture(t *testing.T, sites ...enrichSite) map[string]int64 {
	t.Helper()
	ctx := context.Background()
	lq := leadstoretest.Q(t)
	core := store.NewQueries(storetest.AppConn(t))
	runID, err := core.StartRun(ctx, "leads-sites-probe", "sites-fixture")
	if err != nil {
		t.Fatal(err)
	}
	page := func(u, body string) int64 {
		id, err := core.InsertFetch(ctx, runID, "sites-fixture", fetch.Record{Kind: fetch.KindSite, URL: u, Status: 200, Body: []byte(body), ContentType: "text/html"})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	orgs := map[string]int64{}
	for _, s := range sites {
		if _, err := lq.ImportSites(ctx, leadstore.OriginManual, []leadstore.SiteImport{{Domain: s.domain, Country: "GB"}}); err != nil {
			t.Fatal(err)
		}
		siteID, err := lq.SiteIDByDomain(ctx, s.domain)
		if err != nil {
			t.Fatal(err)
		}
		home := page("https://"+s.domain+"/", s.home)
		probeID, err := lq.RecordProbe(ctx, siteID, runID, leadstore.ProbeRow{
			Outcome: leadstore.OutcomeOK, FinalHost: s.domain, IsDrupal: true, Major: 7,
			Signals: []leadstore.Signal{{Signal: "Drupal.settings", FetchID: home}}, FetchIDs: []int64{home}, DerivedBy: "drupal@1",
		})
		if err != nil {
			t.Fatal(err)
		}
		orgID, _, err := lq.UpsertSiteOrg(ctx, probeID)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := lq.UpsertLead(ctx, leadstore.LeadRow{OrgID: orgID, Kind: leadstore.LeadUnsupported, ProbeID: &probeID, Channel: leadstore.ChannelNone}); err != nil {
			t.Fatal(err)
		}
		if s.contact != "" {
			u := "https://" + s.domain + "/contact"
			if _, err := lq.InsertRoute(ctx, leadstore.RouteRow{OrgID: orgID, Kind: leadstore.RouteForm, Value: u, FetchID: page(u, s.contact), DerivedBy: "contacts@1"}); err != nil {
				t.Fatal(err)
			}
		}
		orgs[s.domain] = orgID
	}
	return orgs
}

func enrichHome(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("../../internal/enrich/testdata", "home.html"))
	if err != nil {
		t.Fatal(err)
	}
	// A person's profile link, built at runtime so no file holds one.
	return strings.ReplaceAll(string(b), "PERSONPROFILE", "https://www.linkedin.com/"+"in"+"/jane-doe-northwind/")
}

// sweepFacts checks every org_fact row: no email-shaped value, no person's profile path,
// every row derived by enrich@1 and citing a fetch that keeps its body.
func sweepFacts(t *testing.T) {
	t.Helper()
	bad := regexp.MustCompile(`(?i)[a-z0-9._%+\-]+@[a-z0-9\-]+(?:\.[a-z0-9\-]+)*\.[a-z]{2,}|/in/`)
	rows, err := storetest.AppConn(t).Query(context.Background(),
		`SELECT x.kind, x.value, x.normalized, x.derived_by, octet_length(f.body) FROM org_fact x JOIN http_fetch f ON f.id = x.fetch_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var kind, value, norm, by string
		var size int
		if err := rows.Scan(&kind, &value, &norm, &by, &size); err != nil {
			t.Fatal(err)
		}
		if bad.MatchString(value) || bad.MatchString(norm) {
			t.Errorf("org_fact %s %q / %q is an address or a person", kind, value, norm)
		}
		if by != "enrich@1" {
			t.Errorf("org_fact %s %q derived by %q", kind, value, by)
		}
		if size == 0 {
			t.Errorf("org_fact %s %q cites a fetch without a body", kind, value)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
}

func factValues(t *testing.T, orgID int64, kind string) []string {
	t.Helper()
	facts, err := leadstoretest.Q(t).OrgFacts(context.Background(), []int64{orgID})
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, f := range facts {
		if f.Kind == kind {
			out = append(out, f.Normalized)
		}
	}
	return out
}

func TestLeadsEnrichFromStoredBodies(t *testing.T) {
	leadsDB(t)
	orgs := enrichFixture(t,
		enrichSite{domain: "northwind.example", home: enrichHome(t), contact: "<p>Tel: +44 20 7946 0150</p>"},
		enrichSite{domain: "example.com", home: enrichHome(t)}, // a real TLD: never touched by a dry run
	)
	out := mustRun(t, "leads", "enrich", "--dry-run", "--fixtures", enrichFixtureDir)
	wantLines(t, "enrich", out, "(dry run, fixtures) · orgs 1 · phones 3 · descriptions 1 · social 6 · requests 0 · stop none")
	if got, want := factValues(t, orgs["northwind.example"], leadstore.FactPhone), []string{"+442079460123", "+442079460456", "+442079460150"}; !slices.Equal(got, want) {
		t.Errorf("phones = %q, want %q", got, want)
	}
	if n := sitesCount(t, `SELECT count(*) FROM http_fetch WHERE source = 'enrich-fixture'`); n != 0 {
		t.Errorf("%d fetches recorded, want none: every page was kept", n)
	}
	if n := sitesCount(t, `SELECT count(*) FROM enrich_attempt a JOIN org o ON o.id = a.org_id WHERE o.domain = 'northwind.example' AND a.facts_found = 10`); n != 1 {
		t.Errorf("%d northwind attempts with 10 facts, want 1", n)
	}
	if n := sitesCount(t, `SELECT count(*) FROM enrich_attempt a JOIN org o ON o.id = a.org_id WHERE o.domain = 'example.com'`); n != 0 {
		t.Errorf("a dry run enriched a real-TLD org")
	}
	if n := sitesCount(t, `SELECT count(*) FROM run WHERE command = 'leads-enrich' AND source = 'enrich-fixture' AND finished_at IS NOT NULL AND requests = 0`); n != 1 {
		t.Errorf("%d finished enrich runs, want 1", n)
	}
	// The twin: nothing is due within 30 days.
	if out := mustRun(t, "leads", "enrich", "--dry-run", "--fixtures", enrichFixtureDir); !strings.Contains(out, "· orgs 0 · ") {
		t.Errorf("second run:\n%s", out)
	}
	sweepFacts(t)
}

func TestLeadsEnrichColdStart(t *testing.T) {
	leadsDB(t)
	enrichFixture(t, enrichSite{domain: "fabrikam.example"}) // a homepage kept as size and hash only
	out := mustRun(t, "leads", "enrich", "--dry-run", "--fixtures", enrichFixtureDir)
	wantLines(t, "enrich", out, "· orgs 1 · phones 0 · descriptions 0 · social 0 · requests 0 · stop none")
	if n := sitesCount(t, `SELECT count(*) FROM enrich_attempt WHERE facts_found = 0`); n != 1 {
		t.Errorf("%d empty attempts, want 1", n)
	}
	if n := sitesCount(t, `SELECT count(*) FROM org_fact`); n != 0 {
		t.Errorf("%d facts from no page", n)
	}
	if out := mustRun(t, "leads", "enrich", "--dry-run", "--fixtures", enrichFixtureDir); !strings.Contains(out, "· orgs 0 · ") {
		t.Errorf("second run:\n%s", out)
	}
	sweepFacts(t)
}

// siteServer serves pages by path for every host and logs each request as host+path.
type siteServer struct {
	srv   *httptest.Server
	mu    sync.Mutex
	asked []string
}

func newSiteServer(t *testing.T, pages map[string]string) *siteServer {
	t.Helper()
	s := &siteServer{}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.asked = append(s.asked, r.Host+r.URL.Path)
		s.mu.Unlock()
		body, ok := pages[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		if strings.HasSuffix(r.URL.Path, ".txt") {
			w.Header().Set("Content-Type", "text/plain")
		} else {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *siteServer) requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.asked)
}

// toServer sends every request to the test server, keeping the URL the client asked for, so
// the client sees the site's own address and nothing leaves the machine.
type toServer struct {
	addr string
	rt   http.RoundTripper
}

func (t toServer) RoundTrip(r *http.Request) (*http.Response, error) {
	out := r.Clone(r.Context())
	out.URL = &url.URL{Scheme: "http", Host: t.addr, Path: r.URL.Path, RawQuery: r.URL.RawQuery}
	out.Host = r.URL.Host
	resp, err := t.rt.RoundTrip(out)
	if resp != nil {
		resp.Request = r
	}
	return resp, err
}

// liveToServer runs live enrichment against s: the real client, its live gate, robots and
// budgets, with only the transport replaced.
func liveToServer(t *testing.T, s *siteServer) {
	t.Helper()
	t.Setenv("LEADHOUND_LIVE", "1")
	saved := enrichClients
	t.Cleanup(func() { enrichClients = saved })
	enrichClients = func(cfg config.Config, ff fetchFlags, lim *rate.Limiter) (func(fetch.Recorder) *fetch.Client, error) {
		mk, err := siteClients(cfg, ff, lim)
		if err != nil {
			return nil, err
		}
		return func(rec fetch.Recorder) *fetch.Client {
			c := mk(rec)
			c.HTTP.Transport = toServer{addr: s.srv.Listener.Addr().String(), rt: http.DefaultTransport}
			return c
		}, nil
	}
}

func TestLeadsEnrichFetchesAtMostTwo(t *testing.T) {
	leadsDB(t)
	home := `<meta name="description" content="Contoso builds boats.">
		<a href="/contact">Contact</a><a href="/impressum">Impressum</a><a href="/about">About us</a>`
	orgs := enrichFixture(t, enrichSite{domain: "contoso.example", home: home})
	s := newSiteServer(t, map[string]string{
		"/robots.txt": "User-agent: *\nDisallow: /impressum\n",
		"/contact":    "<p>Tel: +44 20 7946 0160</p>",
		"/impressum":  "<p>Tel: +44 20 7946 0161</p>",
		"/about":      "<p>Tel: +44 20 7946 0162</p>",
	})
	liveToServer(t, s)
	code, out, errOut := leadsRun(t, "leads", "enrich")
	if code != 0 {
		t.Fatalf("enrich exited %d: %s", code, errOut)
	}
	wantLines(t, "enrich", out,
		"· orgs 1 · phones 1 · descriptions 1 · social 0 · requests 2 · stop none",
		"contact pages fetched: 1 · robots-disallowed pages: 1 · cut short (still due): 0")
	if got, want := s.requests(), []string{"contoso.example/robots.txt", "contoso.example/contact"}; !slices.Equal(got, want) {
		t.Errorf("requests = %q, want %q: the impressum is disallowed and the about page is a third link", got, want)
	}
	if got := factValues(t, orgs["contoso.example"], leadstore.FactPhone); !slices.Equal(got, []string{"+442079460160"}) {
		t.Errorf("phones = %q", got)
	}
	sweepFacts(t)
}

func TestLeadsEnrichKeepsOnlyUsefulBodies(t *testing.T) {
	leadsDB(t)
	home := `<a href="/contact">Contact</a><a href="/kontakt">Kontakt</a>`
	enrichFixture(t,
		enrichSite{domain: "litware.example", home: home},
		// Its contact page repeats the phone it already has, so that page keeps no body.
		enrichSite{domain: "tailspin.example", home: `<a href="tel:+442079460170">Call</a><a href="/contact">Contact</a>`},
	)
	s := newSiteServer(t, map[string]string{
		"/robots.txt": "User-agent: *\nDisallow:\n",
		"/contact":    "<p>Tel: +44 20 7946 0170</p>",
		"/kontakt":    "<p>Nothing to read here.</p>",
	})
	liveToServer(t, s)
	code, out, errOut := leadsRun(t, "leads", "enrich")
	if code != 0 {
		t.Fatalf("enrich exited %d: %s", code, errOut)
	}
	wantLines(t, "enrich", out, "· orgs 2 · phones 2 · ", "contact pages fetched: 3 ·")
	if n := sitesCount(t, `SELECT count(*) FROM http_fetch WHERE source = 'enrich' AND kind = 'site'`); n != 3 {
		t.Errorf("%d pages fetched, want 3", n)
	}
	if n := sitesCount(t, `SELECT count(*) FROM http_fetch WHERE source = 'enrich' AND octet_length(body) > 0`); n != 1 {
		t.Errorf("%d fetched pages keep a body, want 1 (litware's contact page)", n)
	}
	if n := sitesCount(t, `SELECT count(*) FROM http_fetch f WHERE f.source = 'enrich' AND octet_length(f.body) > 0
		AND NOT EXISTS (SELECT 1 FROM org_fact x WHERE x.fetch_id = f.id)`); n != 0 {
		t.Errorf("%d fetched pages keep a body without giving a fact", n)
	}
	sweepFacts(t)
}
