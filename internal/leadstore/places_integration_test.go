//go:build integration

package leadstore_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/leadstore/leadstoretest"
	"github.com/gremy/webbership-leadhound/internal/places"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

func demoPlaces() []leadstore.PlaceRow {
	gb := places.Place{SourceID: "p-1", Name: "Harbour Example", Category: "coffee_shop", Confidence: 0.9,
		Websites: []string{"https://www.harbour.example/menu"}, Phones: []string{"+44 1632 960001"}, Emails: []string{"hi@harbour.example"},
		Address: places.Address{Locality: "Exampleton", Region: "ENG", Country: "GB"}}
	gbTwin := gb // a second branch on the same website: same org
	gbTwin.SourceID, gbTwin.Name = "p-2", "Harbour Example Two"
	us := places.Place{SourceID: "p-3", Name: "Bean Example", Category: "cafe", Confidence: 0.8,
		Websites: []string{"https://bean.example/"}, Address: places.Address{Region: "IL", Country: "US"}, Lon: -89.6, Lat: 39.8, HasPoint: true}
	fb := places.Place{SourceID: "P-4", Name: "Facebook Example", Category: "cafe", Confidence: 0.8,
		Websites: []string{"https://www.facebook.com/fb.example"}, Address: places.Address{Country: "GB"}}
	chain := places.Place{SourceID: "p-5", Name: "Chain Example", Category: "cafe", Confidence: 0.9,
		Websites: []string{"https://chain.example/"}, Brand: "Chain Example Co", Address: places.Address{Country: "GB"}}
	return []leadstore.PlaceRow{{Place: gb}, {Place: gbTwin}, {Place: us}, {Place: fb},
		{Place: chain, ChainRule: "brand", ChainDetail: `the map lists it under the brand "Chain Example Co"`}}
}

// ImportPlaces stores one release once: sites and orgs for the independent places (the US
// one with its ISO region, the one without a website on an org of its own with no site),
// none for a chain, and the import's counts; the same release again stores nothing, the
// next stores every place again over the same orgs. The Drupal build and the route search
// leave the place orgs out where they must, and forget empties a place's phones.
func TestImportPlacesStoreAndTwin(t *testing.T) {
	ctx := context.Background()
	leadstoretest.Reset(t)
	q := leadstoretest.Q(t)
	owner := storetest.OwnerConn(t)
	stats := places.Stats{Lines: 7, Kept: 5, Malformed: 1, NotCafe: 1, MalformedLines: []int{3}}
	imp := func(release string) leadstore.PlaceImported {
		t.Helper()
		res, err := q.ImportPlaces(ctx, leadstore.PlaceImport{CampaignID: 1, Release: release, FileName: "/tmp/x/places.ndjson", Stats: stats}, demoPlaces())
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	first := imp("2026-10-21.0")
	if want := (leadstore.PlaceImported{ImportID: 1, Stored: 5, Chains: 1, Orgs: 3, Sites: 2}); first != want {
		t.Errorf("first import %+v, want %+v", first, want)
	}
	if got := imp("2026-10-21.0"); got.Stored != 0 || got.Already != 5 || got.Orgs != 0 {
		t.Errorf("same release again %+v", got)
	}
	if got := imp("2026-11-18.0"); got.Stored != 5 || got.Already != 0 || got.Orgs != 0 || got.Sites != 0 || got.Chains != 1 {
		t.Errorf("next release %+v", got)
	}
	if _, err := q.ImportPlaces(ctx, leadstore.PlaceImport{CampaignID: 1, Release: "bad release"}, nil); !errors.Is(err, leadstore.ErrBadRelease) {
		t.Errorf("bad release: %v", err)
	}

	var region, file string
	if err := owner.QueryRow(ctx, `SELECT o.region, i.file_name FROM org o, place_import i WHERE o.domain = 'bean.example' AND i.id = 1`).Scan(&region, &file); err != nil || region != "US-IL" || file != "places.ndjson" {
		t.Errorf("region %q file %q (%v)", region, file, err)
	}
	var harbourOrgs int
	if err := owner.QueryRow(ctx, `SELECT count(DISTINCT org_id) FROM place WHERE name LIKE 'Harbour%'`).Scan(&harbourOrgs); err != nil || harbourOrgs != 1 {
		t.Errorf("the two Harbour places sit on %d orgs (%v), want 1", harbourOrgs, err)
	}
	var fbOrg int64
	if err := owner.QueryRow(ctx, `SELECT id FROM org WHERE domain = 'overture:p-4' AND site_id IS NULL AND source = 'place'`).Scan(&fbOrg); err != nil {
		t.Fatalf("no-website org: %v", err)
	}

	need, err := q.OrgsNeedingRoutes(ctx, 0, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range need {
		if o.ID == fbOrg {
			t.Error("the org of a place with no website is searched for routes")
		}
	}
	build, err := q.OrgsForBuild(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(build) != 0 {
		t.Errorf("the Drupal build reads %d place orgs", len(build))
	}
	cafes, err := q.CafeOrgsForBuild(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(cafes) != 3 {
		t.Errorf("café build orgs %d, want 3", len(cafes))
	}

	var harbour int64
	if err := owner.QueryRow(ctx, `SELECT id FROM org WHERE domain = 'harbour.example'`).Scan(&harbour); err != nil {
		t.Fatal(err)
	}
	f, err := leadstore.NewQueries(owner).ForgetOrg(ctx, harbour, "asked to be forgotten")
	if err != nil {
		t.Fatal(err)
	}
	if f.PlacesBlanked != 4 { // two places, two releases
		t.Errorf("places blanked %d, want 4", f.PlacesBlanked)
	}
	var left int
	if err := owner.QueryRow(ctx, `SELECT count(*) FROM place WHERE org_id = $1 AND (phones <> '[]' OR emails <> '[]')`, harbour).Scan(&left); err != nil || left != 0 {
		t.Errorf("%d places keep phones or emails (%v)", left, err)
	}
}
