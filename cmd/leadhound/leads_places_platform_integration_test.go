//go:build integration

package main

import (
	"context"
	"strings"
	"testing"

	"github.com/gremy/webbership-leadhound/internal/places"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

// TestPlacesPlatformAndSharedWebsites: a café whose listed website is a link shortener's or
// a site builder's page has no website of its own, so it becomes a cafe_no_website lead
// with its listing's own role address (and an address on the platform's domain is never
// kept as its route); a brand's website and a domain several differently named places list
// are skipped like chains, counted in the import's summary, while the café that domain is
// named after keeps it; the same release again changes nothing.
func TestPlacesPlatformAndSharedWebsites(t *testing.T) {
	ctx := context.Background()
	dir := leadsDB(t)
	cafesDataDir(t, dir)
	cafeNoWebsiteOn(t, dir)
	saved := contactsResolver
	contactsResolver = mxResolver{"cafe-mira.example": true, "cafe-vela.example": true}
	t.Cleanup(func() { contactsResolver = saved })
	mustRun(t, "leads", "campaigns", "register")

	const rel = "2026-10-21.0"
	f := placesFile(t, "ro.ndjson",
		roPlaceLine(t, "pl-mira", "Cafe Mira Example", "https://bit.ly/mira-demo", []string{"contact@cafe-mira.example"}, []string{"+40 700 000 301"}),
		roPlaceLine(t, "pl-vela", "Vela Example Bistro", "https://vela-demo.wixsite.com/home",
			[]string{"office@cafe-vela.example", mailAt("office", "wixsite.com")}, nil),
		roPlaceLine(t, "pl-alfa", "Cafe Alfa", "https://menuhub.test/alfa", nil, nil),
		roPlaceLine(t, "pl-beta", "Bistro Beta", "https://menuhub.test/beta", nil, nil),
		roPlaceLine(t, "pl-gama", "Gama Coffee", "https://www.menuhub.test/gama", nil, nil),
		roPlaceLine(t, "pl-own", "Menuhub Cafe", "https://menuhub.test/", nil, nil),
		roPlaceLine(t, "pl-brivano", "Brivano Demo", "https://www.lavazza.test/", nil, nil),
	)
	out := importPlaces(t, rel, f)
	wantLines(t, "import", out,
		"  stored 7 places (0 already in this release) · 4 chains skipped (no lead) · 3 orgs and 1 sites made",
		"  of the chains, 4 places on a shared website: a brand's or supplier's, or one 3 or more differently named places list",
		"  from the map listings: 2 email routes and 1 phone number added")
	if n := dbCount(t, `SELECT count(*) FROM place WHERE source_id IN ('pl-alfa', 'pl-beta', 'pl-gama', 'pl-brivano') AND chain_rule = 'domain' AND org_id IS NULL`); n != 4 {
		t.Errorf("%d shared-website places stored as domain chains with no org, want 4", n)
	}
	if n := dbCount(t, `SELECT count(*) FROM org WHERE domain IN ('bit.ly', 'wixsite.com', 'lavazza.test')`); n != 0 {
		t.Errorf("%d orgs on a platform's or brand's domain, want 0", n)
	}
	if n := dbCount(t, `SELECT count(*) FROM org WHERE domain = 'menuhub.test'`); n != 1 {
		t.Errorf("the café named after menuhub.test has %d orgs there, want 1", n)
	}
	if got := orgRoutes(t, "overture:pl-vela"); len(got) != 1 || got[0].Value != "office@cafe-vela.example" {
		t.Errorf("Vela routes %+v, want only office@cafe-vela.example (never the builder's address)", got)
	}

	build := mustRun(t, "leads", "build", "--campaign", "cafes-loyalty")
	var kind, channel, route string
	if err := storetest.OwnerConn(t).QueryRow(ctx, `SELECT l.kind, l.channel, coalesce(r.value, '') FROM lead l
		JOIN org o ON o.id = l.org_id LEFT JOIN contact_route r ON r.id = l.route_id WHERE o.domain = 'overture:pl-mira'`).
		Scan(&kind, &channel, &route); err != nil {
		t.Fatalf("Cafe Mira lead: %v\nbuild said:\n%s", err, build)
	}
	if kind != "cafe_no_website" || channel != "email" || route != "contact@cafe-mira.example" {
		t.Errorf("Cafe Mira lead %s on %s via %q, want cafe_no_website by email\nbuild said:\n%s", kind, channel, route, build)
	}
	if got := orgRoutes(t, "overture:pl-mira"); len(got) != 1 || got[0].DerivedBy != places.DerivedBy {
		t.Errorf("Cafe Mira routes %+v", got)
	}

	before := dbCount(t, `SELECT count(*) FROM org`) + dbCount(t, `SELECT count(*) FROM contact_route`)
	again := importPlaces(t, rel, f)
	if !strings.Contains(again, "stored 0 places (7 already in this release)") || strings.Contains(again, "shared website") {
		t.Errorf("same release again:\n%s", again)
	}
	if after := dbCount(t, `SELECT count(*) FROM org`) + dbCount(t, `SELECT count(*) FROM contact_route`); after != before {
		t.Errorf("orgs and routes %d after the re-import, want %d", after, before)
	}
}
