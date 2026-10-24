//go:build integration

package leadstore_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/gremy/webbership-leadhound/internal/fetch"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

// robotsFetch writes one robots.txt fetch for the fixture's run and returns its id.
func (f siteFixture) robotsFetch(t *testing.T, url string) int64 {
	t.Helper()
	id, err := f.core.InsertFetch(context.Background(), f.runID, "leads", fetch.Record{
		Kind: fetch.KindRobots, URL: url, Status: 200, Body: []byte("User-agent: *\n"), ContentType: "text/plain",
	})
	if err != nil {
		t.Fatalf("insert robots fetch: %v", err)
	}
	return id
}

// drupalProbe adds site domain (country) and writes a Drupal probe of it whose homepage
// ended on finalHost. It returns the probe id and the homepage fetch id.
func (f siteFixture) drupalProbe(t *testing.T, domain, country, finalHost, siteName string, isDrupal bool) (probeID, homeID int64) {
	t.Helper()
	ctx := context.Background()
	if _, err := f.q.ImportSites(ctx, leadstore.OriginTranco, []leadstore.SiteImport{{Domain: domain, Country: country}}); err != nil {
		t.Fatalf("import %s: %v", domain, err)
	}
	robotsID := f.robotsFetch(t, "https://"+domain+"/robots.txt")
	homeID = f.fetchRow(t, "https://"+domain+"/")
	probeID, err := f.q.RecordProbe(ctx, siteID(t, domain), f.runID, leadstore.ProbeRow{
		Outcome: leadstore.OutcomeOK, FinalHost: finalHost, IsDrupal: isDrupal, Major: 7,
		FetchIDs: []int64{robotsID, homeID}, SiteName: siteName, DerivedBy: "drupal@1",
	})
	if err != nil {
		t.Fatalf("record probe of %s: %v", domain, err)
	}
	return probeID, homeID
}

func orgByDomain(t *testing.T, q leadstore.Queries, domain string) leadstore.Org {
	t.Helper()
	o, err := q.OrgByDomain(context.Background(), domain)
	if err != nil {
		t.Fatalf("org %s: %v", domain, err)
	}
	return o
}

func TestUpsertSiteOrgOnePerDomain(t *testing.T) {
	f := newSiteFixture(t)
	ctx := context.Background()
	p1, home1 := f.drupalProbe(t, "northwind.example", "de", "www.northwind.example", "Northwind Trading", true)
	// A second listed site whose homepage redirects to the same registrable domain.
	p2, _ := f.drupalProbe(t, "northwind-shop.example", "at", "shop.northwind.example", "Northwind Shop", true)

	id1, created, err := f.q.UpsertSiteOrg(ctx, p1)
	if err != nil || !created {
		t.Fatalf("first upsert = %d, %v, %v; want created", id1, created, err)
	}
	for _, p := range []int64{p1, p2} {
		id, created, err := f.q.UpsertSiteOrg(ctx, p)
		if err != nil || created || id != id1 {
			t.Fatalf("upsert probe %d = %d, %v, %v; want %d, not created", p, id, created, err, id1)
		}
	}

	o := orgByDomain(t, f.q, "northwind.example")
	sid := siteID(t, "northwind.example")
	want := leadstore.Org{
		ID: id1, Kind: leadstore.OrgSiteOwner, Domain: "northwind.example", Name: "Northwind Trading",
		Country: "DE", SiteID: &sid, Source: leadstore.OrgSourceProbe, SourceFetchID: &home1, CreatedAt: o.CreatedAt,
	}
	if !reflect.DeepEqual(o, want) {
		t.Errorf("org = %+v, want %+v", o, want)
	}

	// A probe with no site name gives an org with an empty name.
	p3, _ := f.drupalProbe(t, "contoso.example", "", "contoso.example", "", true)
	id3, created, err := f.q.UpsertSiteOrg(ctx, p3)
	if err != nil || !created || id3 == id1 {
		t.Fatalf("upsert contoso = %d, %v, %v", id3, created, err)
	}
	if got := orgByDomain(t, f.q, "contoso.example").Name; got != "" {
		t.Errorf("contoso name = %q, want empty", got)
	}

	pNo, _ := f.drupalProbe(t, "fabrikam.example", "", "fabrikam.example", "Fabrikam", false)
	if _, _, err := f.q.UpsertSiteOrg(ctx, pNo); !errors.Is(err, leadstore.ErrNotDrupalProbe) {
		t.Errorf("upsert non-Drupal probe: err = %v, want ErrNotDrupalProbe", err)
	}
	if _, _, err := f.q.UpsertSiteOrg(ctx, 999999); !errors.Is(err, leadstore.ErrProbeNotFound) {
		t.Errorf("upsert missing probe: err = %v, want ErrProbeNotFound", err)
	}
}

func TestUpsertAgencyClashAndReimport(t *testing.T) {
	f := newSiteFixture(t)
	ctx := context.Background()
	p, _ := f.drupalProbe(t, "northwind.example", "de", "northwind.example", "Northwind Trading", true)
	siteOrg, _, err := f.q.UpsertSiteOrg(ctx, p)
	if err != nil {
		t.Fatalf("site org: %v", err)
	}
	before := orgByDomain(t, f.q, "northwind.example")

	// An agency on a domain a site owner already holds: reported, row untouched.
	fid := f.fetchRow(t, "https://www.northwind.example/services")
	got, err := f.q.UpsertAgency(ctx, leadstore.AgencyImport{
		Name: "Northwind Digital", Website: "https://www.northwind.example/", Country: "nl",
	}, &fid)
	if err != nil {
		t.Fatalf("clash upsert: %v", err)
	}
	if want := (leadstore.AgencyUpsert{OrgID: siteOrg, Clash: true, ExistingKind: leadstore.OrgSiteOwner}); got != want {
		t.Errorf("clash upsert = %+v, want %+v", got, want)
	}
	if after := orgByDomain(t, f.q, "northwind.example"); !reflect.DeepEqual(after, before) {
		t.Errorf("site_owner org changed by agency import: %+v, was %+v", after, before)
	}

	// A new agency, its domain read from the website; a re-import is a no-op.
	first, err := f.q.UpsertAgency(ctx, leadstore.AgencyImport{
		Name: "Contoso Web", Website: "https://www.contoso.example/en/", Country: "ro",
	}, nil)
	if err != nil || !first.Created || first.Clash {
		t.Fatalf("agency upsert = %+v, %v; want created", first, err)
	}
	again, err := f.q.UpsertAgency(ctx, leadstore.AgencyImport{
		Name: "Contoso Web Renamed", Domain: "Contoso.Example", Country: "ro",
	}, &fid)
	if want := (leadstore.AgencyUpsert{OrgID: first.OrgID}); err != nil || again != want {
		t.Fatalf("re-import = %+v, %v; want %+v", again, err, want)
	}
	o := orgByDomain(t, f.q, "contoso.example")
	if o.Kind != leadstore.OrgAgency || o.Source != leadstore.OrgSourceAgencies || o.Name != "Contoso Web" ||
		o.Country != "RO" || o.SiteID != nil || o.SourceFetchID != nil {
		t.Errorf("agency org = %+v", o)
	}

	if _, err := f.q.UpsertAgency(ctx, leadstore.AgencyImport{Name: "No Site"}, nil); !errors.Is(err, leadstore.ErrNoDomain) {
		t.Errorf("agency without a domain: err = %v, want ErrNoDomain", err)
	}
}

func TestOrgSetSite(t *testing.T) {
	f := newSiteFixture(t)
	ctx := context.Background()
	a, err := f.q.UpsertAgency(ctx, leadstore.AgencyImport{Name: "Contoso Web", Domain: "contoso.example"}, nil)
	if err != nil {
		t.Fatalf("agency: %v", err)
	}
	if _, err := f.q.ImportSites(ctx, leadstore.OriginAgency, []leadstore.SiteImport{{Domain: "contoso.example"}, {Domain: "fabrikam.example"}}); err != nil {
		t.Fatalf("import sites: %v", err)
	}
	sid := siteID(t, "contoso.example")
	for range 2 { // setting the same site again is a no-op
		if err := f.q.OrgSetSite(ctx, a.OrgID, sid); err != nil {
			t.Fatalf("set site: %v", err)
		}
	}
	if got := orgByDomain(t, f.q, "contoso.example").SiteID; got == nil || *got != sid {
		t.Errorf("site_id = %v, want %d", got, sid)
	}
	if err := f.q.OrgSetSite(ctx, a.OrgID, siteID(t, "fabrikam.example")); !errors.Is(err, leadstore.ErrOrgSiteSet) {
		t.Errorf("set another site: err = %v, want ErrOrgSiteSet", err)
	}
	if err := f.q.OrgSetSite(ctx, 999999, sid); !errors.Is(err, leadstore.ErrOrgNotFound) {
		t.Errorf("set site of missing org: err = %v, want ErrOrgNotFound", err)
	}
}

func TestRoutesAppendOnly(t *testing.T) {
	f := newSiteFixture(t)
	ctx := context.Background()
	a, err := f.q.UpsertAgency(ctx, leadstore.AgencyImport{Name: "Contoso Web", Domain: "contoso.example"}, nil)
	if err != nil {
		t.Fatalf("agency: %v", err)
	}
	fid := f.fetchRow(t, "https://contoso.example/contact")
	for i, tc := range []struct {
		row  leadstore.RouteRow
		want bool
	}{
		{leadstore.RouteRow{OrgID: a.OrgID, Kind: leadstore.RouteEmail, Value: " Info@Contoso.Example ", FetchID: fid, MXOK: true, DerivedBy: "contacts@1"}, true},
		{leadstore.RouteRow{OrgID: a.OrgID, Kind: leadstore.RouteEmail, Value: "info@contoso.example", FetchID: fid, DerivedBy: "contacts@1"}, false},
		{leadstore.RouteRow{OrgID: a.OrgID, Kind: leadstore.RouteForm, Value: "https://contoso.example/contact", FetchID: fid, DerivedBy: "contacts@1"}, true},
		{leadstore.RouteRow{OrgID: a.OrgID, Kind: leadstore.RouteForm, Value: "https://contoso.example/contact", FetchID: fid, DerivedBy: "contacts@1"}, false},
	} {
		got, err := f.q.InsertRoute(ctx, tc.row)
		if err != nil || got != tc.want {
			t.Errorf("insert %d = %v, %v; want %v", i, got, err, tc.want)
		}
	}
	routes, err := f.q.RoutesFor(ctx, a.OrgID)
	if err != nil {
		t.Fatalf("routes: %v", err)
	}
	if len(routes) != 2 || routes[0].Kind != leadstore.RouteEmail || routes[0].Value != "info@contoso.example" ||
		!routes[0].MXOK || routes[1].Kind != leadstore.RouteForm {
		t.Errorf("routes = %+v", routes)
	}
	if _, err := f.q.InsertRoute(ctx, leadstore.RouteRow{OrgID: a.OrgID, Kind: "phone", Value: "x", FetchID: fid}); !errors.Is(err, leadstore.ErrUnknownRouteKind) {
		t.Errorf("unknown kind: err = %v", err)
	}
	if _, err := f.q.InsertRoute(ctx, leadstore.RouteRow{OrgID: a.OrgID, Kind: leadstore.RouteEmail, Value: "  ", FetchID: fid}); !errors.Is(err, leadstore.ErrEmptyRoute) {
		t.Errorf("empty value: err = %v", err)
	}

	// The phrase bound counts runes, not bytes.
	ok := strings.Repeat("ä", leadstore.MaxPhraseRunes)
	if _, err := f.q.InsertAgencyFinding(ctx, leadstore.AgencyFindingRow{OrgID: a.OrgID, URL: "https://contoso.example/upgrades", Phrase: ok, FetchID: fid, DerivedBy: "agencies@1"}); err != nil {
		t.Errorf("120-rune phrase: %v", err)
	}
	if _, err := f.q.InsertAgencyFinding(ctx, leadstore.AgencyFindingRow{OrgID: a.OrgID, URL: "https://contoso.example/upgrades", Phrase: ok + "ä", FetchID: fid, DerivedBy: "agencies@1"}); !errors.Is(err, leadstore.ErrPhraseTooLong) {
		t.Errorf("121-rune phrase: err = %v, want ErrPhraseTooLong", err)
	}
	if err := f.q.RecordContactAttempt(ctx, a.OrgID, f.runID, 2); err != nil {
		t.Fatalf("record attempt: %v", err)
	}

	// The app role cannot rewrite a fact, and of org it may UPDATE site_id only.
	app := storetest.AppConn(t)
	for _, q := range []string{
		"UPDATE contact_route SET value = 'other@contoso.example'",
		"UPDATE agency_finding SET phrase = 'x'",
		"UPDATE contact_attempt SET routes_found = 0",
		"UPDATE org SET name = 'x'",
		"UPDATE org SET kind = 'agency', domain = 'x.example'",
	} {
		t.Run(q, func(t *testing.T) {
			_, err := app.Exec(ctx, q)
			if got := sqlState(err); got != "42501" {
				t.Fatalf("%s as app: err = %v (SQLSTATE %q), want 42501", q, err, got)
			}
		})
	}
	if _, err := app.Exec(ctx, "UPDATE org SET site_id = NULL WHERE id = $1", a.OrgID); err != nil {
		t.Errorf("UPDATE org.site_id as app: %v", err)
	}
}

func TestOrgsNeedingRoutesWindow(t *testing.T) {
	f := newSiteFixture(t)
	ctx := context.Background()
	add := func(domain string) int64 {
		t.Helper()
		r, err := f.q.UpsertAgency(ctx, leadstore.AgencyImport{Name: domain, Domain: domain}, nil)
		if err != nil {
			t.Fatalf("agency %s: %v", domain, err)
		}
		return r.OrgID
	}
	searched, found, fresh := add("contoso.example"), add("fabrikam.example"), add("northwind.example")
	if _, err := storetest.OwnerConn(t).Exec(ctx,
		"INSERT INTO org (kind, domain, source) VALUES ('selftest', 'selftest-1', 'selftest')"); err != nil {
		t.Fatalf("selftest org: %v", err)
	}
	// A search that found nothing still counts.
	if err := f.q.RecordContactAttempt(ctx, searched, f.runID, 0); err != nil {
		t.Fatalf("attempt: %v", err)
	}
	fid := f.fetchRow(t, "https://fabrikam.example/contact")
	if _, err := f.q.InsertRoute(ctx, leadstore.RouteRow{OrgID: found, Kind: leadstore.RouteEmail, Value: "info@fabrikam.example", FetchID: fid, DerivedBy: "contacts@1"}); err != nil {
		t.Fatalf("route: %v", err)
	}

	now := time.Now()
	ids := func(limit int, at time.Time) []int64 {
		t.Helper()
		orgs, err := f.q.OrgsNeedingRoutes(ctx, limit, at)
		if err != nil {
			t.Fatalf("orgs needing routes: %v", err)
		}
		out := []int64{}
		for _, o := range orgs {
			out = append(out, o.ID)
		}
		return out
	}
	for _, tc := range []struct {
		name  string
		limit int
		at    time.Time
		want  []int64
	}{
		{"now", 0, now, []int64{fresh}},
		{"29 days on", 0, now.Add(29 * 24 * time.Hour), []int64{fresh}},
		{"31 days on", 0, now.Add(31 * 24 * time.Hour), []int64{searched, found, fresh}},
		{"31 days on, limit 2", 2, now.Add(31 * 24 * time.Hour), []int64{searched, found}},
	} {
		if got := ids(tc.limit, tc.at); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: orgs = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// orgSnapshot is what a run of the org writes leaves in the database, ids included.
type orgSnapshot struct {
	Orgs   []string
	Routes []string
	OrgIDs []int64 // what the two upserts returned
}

// orgPass runs the org writes once on f and returns what the database then holds.
func orgPass(t *testing.T, f siteFixture, probeID int64, fid int64) orgSnapshot {
	t.Helper()
	ctx := context.Background()
	siteOrg, _, err := f.q.UpsertSiteOrg(ctx, probeID)
	if err != nil {
		t.Fatalf("site org: %v", err)
	}
	ag, err := f.q.UpsertAgency(ctx, leadstore.AgencyImport{Name: "Contoso Web", Website: "https://contoso.example/"}, &fid)
	if err != nil {
		t.Fatalf("agency: %v", err)
	}
	for _, r := range []leadstore.RouteRow{
		{OrgID: siteOrg, Kind: leadstore.RouteEmail, Value: "info@northwind.example", FetchID: fid, DerivedBy: "contacts@1"},
		{OrgID: ag.OrgID, Kind: leadstore.RouteForm, Value: "https://contoso.example/contact", FetchID: fid, DerivedBy: "contacts@1"},
	} {
		if _, err := f.q.InsertRoute(ctx, r); err != nil {
			t.Fatalf("route: %v", err)
		}
	}
	var s orgSnapshot
	rows, err := storetest.OwnerConn(t).Query(ctx,
		"SELECT id || ' ' || kind || ' ' || domain || ' ' || name || ' ' || source FROM org ORDER BY id")
	if err != nil {
		t.Fatalf("orgs: %v", err)
	}
	if s.Orgs, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
		t.Fatalf("scan orgs: %v", err)
	}
	for _, id := range []int64{siteOrg, ag.OrgID} {
		routes, err := f.q.RoutesFor(ctx, id)
		if err != nil {
			t.Fatalf("routes: %v", err)
		}
		for _, r := range routes {
			s.Routes = append(s.Routes, r.Kind+" "+r.Value)
		}
	}
	s.OrgIDs = []int64{siteOrg, ag.OrgID}
	return s
}

// TestOrgsColdStartTwin runs the same writes twice on one database, then once more on a
// fresh one: the repeat is a no-op and the fresh database ends the same.
func TestOrgsColdStartTwin(t *testing.T) {
	setup := func() (siteFixture, int64, int64) {
		f := newSiteFixture(t)
		p, _ := f.drupalProbe(t, "northwind.example", "de", "www.northwind.example", "Northwind Trading", true)
		return f, p, f.fetchRow(t, "https://contoso.example/contact")
	}
	f, p, fid := setup()
	first := orgPass(t, f, p, fid)
	if second := orgPass(t, f, p, fid); !reflect.DeepEqual(second, first) {
		t.Errorf("repeat changed the database:\n%+v\nwas\n%+v", second, first)
	}
	f2, p2, fid2 := setup()
	if twin := orgPass(t, f2, p2, fid2); !reflect.DeepEqual(twin, first) {
		t.Errorf("fresh database differs:\n%+v\nwant\n%+v", twin, first)
	}
	if len(first.Orgs) != 2 || len(first.Routes) != 2 {
		t.Errorf("snapshot = %+v, want 2 orgs and 2 routes", first)
	}
}
