//go:build integration

package leadstore_test

import (
	"context"
	"testing"

	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/leadstore/leadstoretest"
	"github.com/gremy/webbership-leadhound/internal/places"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

// LeadPlace reads the org's latest place within the campaign asked for, never another
// campaign's, and reports none for a campaign whose imports hold no place of the org.
func TestLeadPlaceByCampaign(t *testing.T) {
	ctx := context.Background()
	leadstoretest.Reset(t)
	q := leadstoretest.Q(t)
	owner := storetest.OwnerConn(t)
	if _, err := owner.Exec(ctx, `INSERT INTO campaign (id, account_id, slug, name) VALUES (2, 1, 'cafes', 'Cafes'), (3, 1, 'empty', 'Empty')`); err != nil {
		t.Fatal(err)
	}
	place := func(name, locality string) []leadstore.PlaceRow {
		return []leadstore.PlaceRow{{Place: places.Place{SourceID: "p-1", Name: name, Category: "coffee_shop", Confidence: 0.9,
			Websites: []string{"https://harbour.example/"}, Phones: []string{"+44 1632 960001"}, Emails: []string{"hi@harbour.example"},
			Address: places.Address{Locality: locality, Region: "ENG", Country: "GB", Postcode: "EX1 1AA", Freeform: "1 Example Row"}}}}
	}
	imp := func(campaign int64, release, name, locality string) {
		t.Helper()
		if _, err := q.ImportPlaces(ctx, leadstore.PlaceImport{CampaignID: campaign, Release: release, FileName: "places.ndjson",
			Stats: places.Stats{Lines: 1, Kept: 1}}, place(name, locality)); err != nil {
			t.Fatal(err)
		}
	}
	imp(1, "2026-10-21.0", "Harbour Old", "Oldton")
	imp(1, "2026-11-18.0", "Harbour Example", "Exampleton")
	imp(2, "2026-12-16.0", "Harbour Other Campaign", "Elsewhere")

	var org int64
	if err := owner.QueryRow(ctx, `SELECT id FROM org WHERE domain = 'harbour.example'`).Scan(&org); err != nil {
		t.Fatal(err)
	}
	got, ok, err := q.LeadPlace(ctx, org, 1)
	if err != nil || !ok {
		t.Fatalf("campaign 1: ok %v err %v", ok, err)
	}
	want := leadstore.LeadPlace{Name: "Harbour Example", Category: "coffee_shop", Country: "GB", Region: "ENG",
		Locality: "Exampleton", Postcode: "EX1 1AA", Freeform: "1 Example Row", Websites: []string{"https://harbour.example/"}}
	if got.Name != want.Name || got.Category != want.Category || got.Country != want.Country || got.Region != want.Region ||
		got.Locality != want.Locality || got.Postcode != want.Postcode || got.Freeform != want.Freeform ||
		len(got.Websites) != 1 || got.Websites[0] != want.Websites[0] {
		t.Errorf("campaign 1: %+v, want %+v", got, want)
	}
	if got, ok, err := q.LeadPlace(ctx, org, 2); err != nil || !ok || got.Name != "Harbour Other Campaign" {
		t.Errorf("campaign 2: %+v ok %v err %v", got, ok, err)
	}
	if got, ok, err := q.LeadPlace(ctx, org, 3); err != nil || ok || got.Name != "" {
		t.Errorf("campaign with no import: %+v ok %v err %v", got, ok, err)
	}
	if _, ok, err := q.LeadPlace(ctx, org+999, 1); err != nil || ok {
		t.Errorf("unknown org: ok %v err %v", ok, err)
	}
}
