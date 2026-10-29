//go:build integration

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/campaign"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/leadstore/leadstoretest"
	"github.com/gremy/webbership-leadhound/internal/places"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

// roPlaceLine is one Overture NDJSON line of a Romanian café with its listing's emails and
// phones; website "" for none of its own (a Facebook page instead).
func roPlaceLine(t *testing.T, id, name, website string, emails, phones []string) string {
	t.Helper()
	p := map[string]any{
		"id": id, "names": map[string]any{"primary": name}, "categories": map[string]any{"primary": "cafe"},
		"confidence": 0.9, "addresses": []any{map[string]any{"locality": "Exampleni", "region": "TM", "country": "RO"}},
		"sources":  []any{map[string]any{"dataset": "demo", "record_id": id}},
		"websites": []string{"https://www.facebook.com/" + id + ".example"},
		"emails":   emails, "phones": phones,
	}
	if website != "" {
		p["websites"] = []string{website}
	}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// cafeNoWebsiteOn makes sure the example campaign lists the cafe_no_website kind, with the
// no-loyalty templates standing in when it has none of its own.
func cafeNoWebsiteOn(t *testing.T, dir string) {
	t.Helper()
	cdir := filepath.Join(dir, campaign.Dir, "cafes-loyalty")
	path := filepath.Join(cdir, "campaign.yaml")
	raw, err := os.ReadFile(path) // #nosec G304 -- the test's own temp copy
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	if !regexp.MustCompile(`(?m)^kinds: \[.*\bcafe_no_website\b`).MatchString(s) {
		const kinds = "kinds: [cafe_no_digital_loyalty"
		if !strings.Contains(s, kinds) {
			t.Fatalf("campaign.yaml has no %q", kinds)
		}
		s = strings.Replace(s, kinds, "kinds: [cafe_no_website, cafe_no_digital_loyalty", 1)
		if err := os.WriteFile(path, []byte(s), 0o600); err != nil { // #nosec G703 -- a test temp dir
			t.Fatal(err)
		}
	}
	tdir := filepath.Join(cdir, "templates", "cafe_no_website")
	if _, err := os.Stat(tdir); err == nil {
		return
	}
	if err := os.CopyFS(tdir, os.DirFS(filepath.Join(cdir, "templates", "cafe_no_digital_loyalty"))); err != nil {
		t.Fatal(err)
	}
}

type routeRow struct {
	Value, DerivedBy string
	MXOK, FromPlace  bool
	HasFetch         bool
}

func orgRoutes(t *testing.T, domain string) []routeRow {
	t.Helper()
	rs, err := storetest.OwnerConn(t).Query(context.Background(),
		`SELECT r.value, r.derived_by, r.mx_ok, r.place_id IS NOT NULL, r.fetch_id IS NOT NULL
		 FROM contact_route r JOIN org o ON o.id = r.org_id WHERE o.domain = $1 ORDER BY r.value`, domain)
	if err != nil {
		t.Fatal(err)
	}
	defer rs.Close()
	var out []routeRow
	for rs.Next() {
		var r routeRow
		if err := rs.Scan(&r.Value, &r.DerivedBy, &r.MXOK, &r.FromPlace, &r.HasFetch); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

func orgPhones(t *testing.T, domain string) string {
	t.Helper()
	var s string
	if err := storetest.OwnerConn(t).QueryRow(context.Background(),
		`SELECT coalesce(string_agg(f.normalized || '/' || f.derived_by, ' ' ORDER BY f.normalized), '')
		 FROM org_fact f JOIN org o ON o.id = f.org_id WHERE o.domain = $1 AND f.kind = 'phone'`, domain).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

// TestPlaceContactsBecomeRoutes is the status × operation grid of a place's own emails and
// phones: a café with no website and a role address becomes a cafe_no_website lead on the
// email channel with a callable phone; personal Gmail addresses go where the contact search
// sends them (dropped); a suppressed address and a suppressed number stay out; a route the
// contact search found keeps its source; the same release again and the next release add
// nothing twice (the cold-start twins); an org with only a map-listing route is still
// searched; and a café erased with `leads forget` gets nothing back from a later release.
func TestPlaceContactsBecomeRoutes(t *testing.T) {
	ctx := context.Background()
	dir := leadsDB(t)
	cafesDataDir(t, dir)
	cafeNoWebsiteOn(t, dir)
	saved := contactsResolver
	contactsResolver = mxResolver{"cafe-luna.example": true, "bistro-nord.example": true, "plain-cafe.example": true}
	t.Cleanup(func() { contactsResolver = saved })
	mustRun(t, "leads", "campaigns", "register")
	lq := leadstoretest.Q(t)
	if _, err := lq.Suppress(ctx, 0, leadstore.SuppressAddress, "contact@quiet-cafe.example", leadstore.ReasonOptOut, "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := lq.Suppress(ctx, 0, leadstore.SuppressPhone, leadstore.PhoneSuppressionValue("+40700000209"), leadstore.ReasonDoNotCall, "test"); err != nil {
		t.Fatal(err)
	}

	const r1, r2, r3 = "2026-10-21.0", "2026-11-18.0", "2026-12-16.0"
	luna := roPlaceLine(t, "rp-luna", "Cafe Luna Example", "",
		[]string{"Contact@Cafe-Luna.example", mailAt("maria.pop", "gmail.com"), mailAt("cafeluna", "gmail.com")}, []string{"+40 700 000 201", "0700 000 202"})
	quiet := roPlaceLine(t, "rp-quiet", "Quiet Example Cafe", "",
		[]string{"contact@quiet-cafe.example"}, []string{"+40 700 000 209"})
	a := placesFile(t, "a.ndjson", luna, quiet,
		roPlaceLine(t, "rp-nord", "Bistro Nord Example", "https://bistro-nord.example/", nil, nil),
		roPlaceLine(t, "rp-plain", "Plain Example Cafe", "https://plain-cafe.example/", []string{"contact@plain-cafe.example"}, nil),
	)
	out := importPlaces(t, r1, a)
	wantLines(t, "first import", out, "  from the map listings: 2 email routes and 2 phone numbers added")

	// The café with no website: its role address (lower case, MX found), not the Gmail ones.
	if got := orgRoutes(t, "overture:rp-luna"); len(got) != 1 ||
		got[0] != (routeRow{Value: "contact@cafe-luna.example", DerivedBy: places.DerivedBy, MXOK: true, FromPlace: true}) {
		t.Errorf("Cafe Luna routes %+v, want contact@cafe-luna.example from the place", got)
	}
	if got := orgPhones(t, "overture:rp-luna"); got != "+40700000201/overture@1 +40700000202/overture@1" {
		t.Errorf("Cafe Luna phones %q", got)
	}
	// Suppressed address and number: nothing written.
	if got := orgRoutes(t, "overture:rp-quiet"); len(got) != 0 {
		t.Errorf("suppressed address became routes %+v", got)
	}
	if got := orgPhones(t, "overture:rp-quiet"); got != "" {
		t.Errorf("suppressed number became a phone %q", got)
	}
	// An org whose only route is from its map listing is still searched on its own pages.
	orgs, err := lq.OrgsNeedingRoutes(ctx, 0, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	searched := false
	for _, o := range orgs {
		searched = searched || o.Domain == "plain-cafe.example"
	}
	if !searched {
		t.Error("plain-cafe.example, with only a map-listing route, is not due for the contact search")
	}

	// The build: Cafe Luna is a cafe_no_website lead on the email channel with its route.
	build := mustRun(t, "leads", "build", "--campaign", "cafes-loyalty")
	var kind, channel, routeValue string
	var orgID int64
	if err := storetest.OwnerConn(t).QueryRow(ctx, `SELECT l.kind, l.channel, coalesce(r.value, ''), o.id FROM lead l
		JOIN org o ON o.id = l.org_id LEFT JOIN contact_route r ON r.id = l.route_id WHERE o.domain = 'overture:rp-luna'`).
		Scan(&kind, &channel, &routeValue, &orgID); err != nil {
		t.Fatalf("Cafe Luna lead: %v\nbuild said:\n%s", err, build)
	}
	if kind != "cafe_no_website" || channel != "email" || routeValue != "contact@cafe-luna.example" {
		t.Errorf("Cafe Luna lead %s on %s via %q\nbuild said:\n%s", kind, channel, routeValue, build)
	}
	facts, err := lq.UsableOrgFacts(ctx, []int64{orgID})
	if err != nil {
		t.Fatal(err)
	}
	if f, ok, why := pickPhone(facts, nil); !ok || f.Normalized != "+40700000201" || f.FetchID != 0 {
		t.Errorf("Cafe Luna's callable phone %+v ok %v (%s), want +40700000201", f, ok, why)
	}

	// Cold-start twins: the same release again stores nothing; the next release adds no route twice.
	before := dbCount(t, `SELECT count(*) FROM contact_route`) + dbCount(t, `SELECT count(*) FROM org_fact`)
	if again := importPlaces(t, r1, a); strings.Contains(again, "from the map listings") {
		t.Errorf("same release again added contacts:\n%s", again)
	}
	if next := importPlaces(t, r2, a); strings.Contains(next, "from the map listings") || !strings.Contains(next, "stored 4 places") {
		t.Errorf("next release:\n%s", next)
	}
	if after := dbCount(t, `SELECT count(*) FROM contact_route`) + dbCount(t, `SELECT count(*) FROM org_fact`); after != before {
		t.Errorf("routes and facts %d after the re-imports, want %d", after, before)
	}

	// A route the contact search found keeps its source; the listing adds only what is new,
	// never another domain's address or a person's.
	var nordOrg, runID, fetchID int64
	owner := storetest.OwnerConn(t)
	if err := owner.QueryRow(ctx, `SELECT id FROM org WHERE domain = 'bistro-nord.example'`).Scan(&nordOrg); err != nil {
		t.Fatal(err)
	}
	if err := owner.QueryRow(ctx, `SELECT max(id) FROM run`).Scan(&runID); err != nil {
		t.Fatal(err)
	}
	if err := owner.QueryRow(ctx, `INSERT INTO http_fetch (run_id, source, kind, url, status) VALUES ($1, 'contacts', 'site', 'https://bistro-nord.example/contact', 200) RETURNING id`, runID).Scan(&fetchID); err != nil {
		t.Fatal(err)
	}
	if _, err := lq.InsertRoute(ctx, leadstore.RouteRow{OrgID: nordOrg, Kind: leadstore.RouteEmail, Value: "info@bistro-nord.example",
		FetchID: fetchID, MXOK: true, DerivedBy: "contacts@1"}); err != nil {
		t.Fatal(err)
	}
	b := placesFile(t, "b.ndjson", roPlaceLine(t, "rp-nord", "Bistro Nord Example", "https://bistro-nord.example/",
		[]string{"INFO@bistro-nord.example", "office@elsewhere.example", "hello@bistro-nord.example", "ana.ionescu@bistro-nord.example"}, nil))
	wantLines(t, "crawl route then listing", importPlaces(t, r3, b), "  from the map listings: 1 email route and 0 phone numbers added")
	want := []routeRow{
		{Value: "hello@bistro-nord.example", DerivedBy: places.DerivedBy, MXOK: true, FromPlace: true},
		{Value: "info@bistro-nord.example", DerivedBy: "contacts@1", MXOK: true, HasFetch: true},
	}
	if got := orgRoutes(t, "bistro-nord.example"); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("Bistro Nord routes %+v, want %+v", got, want)
	}

	// Erasure: forgetting the café with no website suppresses its org, so a later release
	// neither stores its place again nor brings back an email or a phone.
	t.Setenv("LEADHOUND_MIGRATE_DATABASE_URL", storetest.MigrateDSN()) // forget runs as the owner
	mustRun(t, "leads", "forget", "org", fmt.Sprint(orgID), "--reason", "asked to be forgotten")
	routesBefore, phonesBefore := len(orgRoutes(t, "overture:rp-luna")), orgPhones(t, "overture:rp-luna")
	c := placesFile(t, "c.ndjson", roPlaceLine(t, "rp-luna", "Cafe Luna Example", "",
		[]string{"contact@cafe-luna.example", "hello@cafe-luna.example"}, []string{"+40 700 000 201", "+40 700 000 299"}))
	wantLines(t, "after forget", importPlaces(t, "2027-01-20.0", c),
		"  stored 0 places (0 already in this release)",
		"  not stored: 1 place whose café is suppressed, by its website's domain or an earlier listing (forgotten or opted out), 1 of them with no website of their own")
	if got := len(orgRoutes(t, "overture:rp-luna")); got != routesBefore {
		t.Errorf("forgotten café routes %d, want %d", got, routesBefore)
	}
	if got := orgPhones(t, "overture:rp-luna"); got != phonesBefore || got != "" {
		t.Errorf("forgotten café phones %q (before the import %q), want none", got, phonesBefore)
	}
	if n := dbCount(t, `SELECT count(*) FROM place WHERE source_id = 'rp-luna' AND release = '2027-01-20.0'`); n != 0 {
		t.Errorf("forgotten café's place stored again: %d", n)
	}

	// Both directions of a listing that changes: the café forgotten with no website now lists
	// a website, and a café forgotten by its website now lists none. Neither comes back.
	var plainOrg int64
	if err := owner.QueryRow(ctx, `SELECT id FROM org WHERE domain = 'plain-cafe.example'`).Scan(&plainOrg); err != nil {
		t.Fatal(err)
	}
	mustRun(t, "leads", "forget", "org", fmt.Sprint(plainOrg), "--reason", "asked to be forgotten")
	plainRoutes := len(orgRoutes(t, "plain-cafe.example"))
	d := placesFile(t, "d.ndjson",
		roPlaceLine(t, "rp-luna", "Cafe Luna Example", "https://cafe-luna.example/", []string{"contact@cafe-luna.example"}, []string{"+40 700 000 201"}),
		roPlaceLine(t, "rp-plain", "Plain Example Cafe", "", []string{"contact@plain-cafe.example"}, []string{"+40 700 000 288"}),
	)
	wantLines(t, "listings changed after forget", importPlaces(t, "2027-02-17.0", d),
		"  stored 0 places (0 already in this release) · 0 chains skipped (no lead) · 0 orgs and 0 sites made",
		"  not stored: 2 places whose café is suppressed, by its website's domain or an earlier listing (forgotten or opted out), 1 of them with no website of their own")
	if n := dbCount(t, `SELECT count(*) FROM org WHERE domain IN ('cafe-luna.example', 'overture:rp-plain')`); n != 0 {
		t.Errorf("%d orgs made for the forgotten cafés' changed listings, want 0", n)
	}
	if got := len(orgRoutes(t, "plain-cafe.example")); got != plainRoutes {
		t.Errorf("forgotten website café routes %d, want %d", got, plainRoutes)
	}
	if n := dbCount(t, `SELECT count(*) FROM place WHERE release = '2027-02-17.0'`); n != 0 {
		t.Errorf("%d places of the forgotten cafés stored, want 0", n)
	}
}
