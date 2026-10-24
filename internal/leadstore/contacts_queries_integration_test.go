//go:build integration

package leadstore_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/gremy/webbership-leadhound/internal/leadstore"
)

func TestDrupalProbesWithoutOrg(t *testing.T) {
	f := newSiteFixture(t)
	ctx := context.Background()
	pNorth, _ := f.drupalProbe(t, "northwind.example", "GB", "www.northwind.example", "Northwind", true)
	pContoso, _ := f.drupalProbe(t, "contoso.example", "", "", "", true)
	f.drupalProbe(t, "tailspin.example", "", "tailspin.example", "", false) // not Drupal
	// A Drupal site whose final host sits on a domain an agency already holds.
	if _, err := f.q.UpsertAgency(ctx, leadstore.AgencyImport{Name: "Fabrikam", Website: "https://fabrikam.example/"}, nil); err != nil {
		t.Fatal(err)
	}
	f.drupalProbe(t, "fabrikam-shop.example", "", "shop.fabrikam.example", "", true)

	got, err := f.q.DrupalProbesWithoutOrg(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []leadstore.ProbeForOrg{
		{ProbeID: pNorth, Domain: "northwind.example"},
		{ProbeID: pContoso, Domain: "contoso.example"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	if _, _, err := f.q.UpsertSiteOrg(ctx, pNorth); err != nil {
		t.Fatal(err)
	}
	got, err = f.q.DrupalProbesWithoutOrg(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ProbeID != pContoso {
		t.Errorf("after the org: %+v, want only contoso", got)
	}
}

func TestOrgHomepageAndByID(t *testing.T) {
	f := newSiteFixture(t)
	ctx := context.Background()
	p, home := f.drupalProbe(t, "northwind.example", "GB", "northwind.example", "", true)
	body := []byte("<html></html>")
	if err := f.core.AttachFetchBody(ctx, home, body); err != nil {
		t.Fatal(err)
	}
	orgID, _, err := f.q.UpsertSiteOrg(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	h, err := f.q.OrgHomepage(ctx, orgID)
	if err != nil {
		t.Fatal(err)
	}
	if h.FetchID != home || h.URL != "https://northwind.example/" || string(h.Body) != string(body) {
		t.Errorf("homepage = %+v", h)
	}
	o, err := f.q.OrgByID(ctx, orgID)
	if err != nil || o.Domain != "northwind.example" {
		t.Errorf("org by id = %+v, %v", o, err)
	}
	if _, err := f.q.OrgByID(ctx, 999); !errors.Is(err, leadstore.ErrOrgNotFound) {
		t.Errorf("missing org: %v, want ErrOrgNotFound", err)
	}

	// An agency without a source fetch has no homepage; a size-only one has no body.
	a, err := f.q.UpsertAgency(ctx, leadstore.AgencyImport{Name: "Contoso", Website: "https://contoso.example/"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if h, err := f.q.OrgHomepage(ctx, a.OrgID); err != nil || h.FetchID != 0 || len(h.Body) != 0 {
		t.Errorf("agency homepage = %+v, %v; want none", h, err)
	}
	if _, err := f.q.OrgHomepage(ctx, 999); !errors.Is(err, leadstore.ErrOrgNotFound) {
		t.Errorf("missing org: %v, want ErrOrgNotFound", err)
	}
}

func TestSiteIDByDomain(t *testing.T) {
	f := newSiteFixture(t)
	ctx := context.Background()
	if _, err := f.q.ImportSites(ctx, leadstore.OriginAgency, []leadstore.SiteImport{{Domain: "northwind.example"}}); err != nil {
		t.Fatal(err)
	}
	id, err := f.q.SiteIDByDomain(ctx, "Northwind.example")
	if err != nil || id != siteID(t, "northwind.example") {
		t.Errorf("site id = %d, %v", id, err)
	}
	if _, err := f.q.SiteIDByDomain(ctx, "contoso.example"); !errors.Is(err, leadstore.ErrSiteNotFound) {
		t.Errorf("missing site: %v, want ErrSiteNotFound", err)
	}
}

func TestHasAgencyFinding(t *testing.T) {
	f := newSiteFixture(t)
	ctx := context.Background()
	a, err := f.q.UpsertAgency(ctx, leadstore.AgencyImport{Name: "Contoso", Website: "https://contoso.example/"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := f.q.HasAgencyFinding(ctx, a.OrgID); err != nil || ok {
		t.Fatalf("before = %v, %v", ok, err)
	}
	fid := f.fetchRow(t, "https://contoso.example/upgrades")
	if _, err := f.q.InsertAgencyFinding(ctx, leadstore.AgencyFindingRow{
		OrgID: a.OrgID, URL: "https://contoso.example/upgrades", Phrase: "Drupal 7 upgrades", FetchID: fid, DerivedBy: "agencies@1",
	}); err != nil {
		t.Fatal(err)
	}
	if ok, err := f.q.HasAgencyFinding(ctx, a.OrgID); err != nil || !ok {
		t.Errorf("after = %v, %v", ok, err)
	}
}

func TestExportRoutes(t *testing.T) {
	f := newSiteFixture(t)
	ctx := context.Background()
	// example.net is an RFC 2606 name on a real TLD, so the export keeps it.
	rank := func(n int) *int { return &n }
	if _, err := f.q.ImportSites(ctx, leadstore.OriginTranco, []leadstore.SiteImport{
		{Domain: "example.net", Country: "GB", Rank: rank(5)},
		{Domain: "example.org", Country: "NL", Rank: rank(2)},
	}); err != nil {
		t.Fatal(err)
	}
	probe := func(domain string, major int, exact string) int64 {
		home := f.fetchRow(t, "https://"+domain+"/")
		id, err := f.q.RecordProbe(ctx, siteID(t, domain), f.runID, leadstore.ProbeRow{
			Outcome: leadstore.OutcomeOK, FinalHost: domain, IsDrupal: true, Major: major, ExactVersion: exact,
			FetchIDs: []int64{home}, SiteName: "Site " + domain, DerivedBy: "drupal@1",
		})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	pNet := probe("example.net", 7, "")
	pOrg := probe("example.org", 10, "10.2.7")
	pRes, _ := f.drupalProbe(t, "northwind.example", "GB", "northwind.example", "", true) // reserved: left out
	org := func(p int64) int64 {
		id, _, err := f.q.UpsertSiteOrg(ctx, p)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	oNet, oOrg, oRes := org(pNet), org(pOrg), org(pRes)
	agency, err := f.q.UpsertAgency(ctx, leadstore.AgencyImport{Name: "Agency", Website: "https://example.com/", Country: "DE"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	route := func(orgID int64, kind, value string, mx bool) {
		fid := f.fetchRow(t, "https://route.example/"+value)
		if _, err := f.q.InsertRoute(ctx, leadstore.RouteRow{OrgID: orgID, Kind: kind, Value: value, FetchID: fid, MXOK: mx, DerivedBy: "contacts@1"}); err != nil {
			t.Fatal(err)
		}
	}
	// example.net: a form, an email without MX, and an email with MX, which wins.
	route(oNet, leadstore.RouteForm, "https://example.net/contact", false)
	route(oNet, leadstore.RouteEmail, "office@example.net", false)
	route(oNet, leadstore.RouteEmail, "info@example.net", true)
	// example.org: an email without MX and a form, which wins.
	route(oOrg, leadstore.RouteEmail, "info@example.org", false)
	route(oOrg, leadstore.RouteForm, "https://example.org/contact", false)
	route(oRes, leadstore.RouteEmail, "info@northwind.example", true)
	route(agency.OrgID, leadstore.RouteEmail, "hello@example.com", true)
	fid := f.fetchRow(t, "https://example.com/upgrades")
	if _, err := f.q.InsertAgencyFinding(ctx, leadstore.AgencyFindingRow{
		OrgID: agency.OrgID, URL: "https://example.com/upgrades", Phrase: "we upgrade Drupal 7", FetchID: fid, DerivedBy: "agencies@1",
	}); err != nil {
		t.Fatal(err)
	}
	// An org with no route is not exported.
	if _, err := f.q.UpsertAgency(ctx, leadstore.AgencyImport{Name: "Quiet", Website: "https://quiet.example/"}, nil); err != nil {
		t.Fatal(err)
	}

	rows, err := f.q.ExportRoutes(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	want := []leadstore.ExportRow{
		{Domain: "example.org", Kind: leadstore.OrgSiteOwner, Name: "Site example.org", Country: "NL", Major: 10, ExactVersion: "10.2.7",
			RouteKind: leadstore.RouteForm, Route: "https://example.org/contact"},
		{Domain: "example.net", Kind: leadstore.OrgSiteOwner, Name: "Site example.net", Country: "GB", Major: 7,
			RouteKind: leadstore.RouteEmail, Route: "info@example.net", MXOK: true},
		{Domain: "example.com", Kind: leadstore.OrgAgency, Name: "Agency", Country: "DE",
			Phrase: "we upgrade Drupal 7", FindingURL: "https://example.com/upgrades",
			RouteKind: leadstore.RouteEmail, Route: "hello@example.com", MXOK: true},
	}
	if !reflect.DeepEqual(rows, want) {
		t.Fatalf("export:\n got %+v\nwant %+v", rows, want)
	}
	rows, err = f.q.ExportRoutes(ctx, 1)
	if err != nil || len(rows) != 1 || rows[0].Domain != "example.org" {
		t.Errorf("limit 1 = %+v, %v", rows, err)
	}
}
