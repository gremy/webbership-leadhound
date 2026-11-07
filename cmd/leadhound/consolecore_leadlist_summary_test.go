package main

import (
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/console"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
)

// TestRowSummaryTown: a summary built from the place names its town once. When the address
// already holds the town, in any case, the town is left out of the line's head; when it does
// not, the town leads. A description always stands alone.
func TestRowSummaryTown(t *testing.T) {
	for _, c := range []struct {
		town, address, want string
	}{
		{"Fictiveton", "Fictiveton, 227150", "coffee shop · Fictiveton, 227150"},
		{"Fictiveton", "FICTIVETON 227150", "coffee shop · FICTIVETON 227150"},
		{"Fictiveton", "1 Fictive Road, AB1 2CD", "Fictiveton · coffee shop · 1 Fictive Road, AB1 2CD"},
		{"Fictiveton", "", "Fictiveton · coffee shop"},
		{"", "1 Fictive Road", "coffee shop · 1 Fictive Road"},
	} {
		row := console.LeadListRow{Town: c.town}
		if got := rowSummary(row, leadstore.RowExtras{Address: c.address, Category: "coffee_shop"}); got != c.want {
			t.Errorf("town %q, address %q: %q, want %q", c.town, c.address, got, c.want)
		}
	}
	if got := rowSummary(console.LeadListRow{Town: "Fictiveton"}, leadstore.RowExtras{Description: " A fictive  café. ", Address: "Fictiveton"}); got != "A fictive café." {
		t.Errorf("a description's summary %q", got)
	}
}

// TestLeadIntentViewWords: the page's intent line labels the class, keeps the reason, overlap
// and the operator's call, and dates it by the signal, else the sync, else not at all.
func TestLeadIntentViewWords(t *testing.T) {
	sig := time.Date(2026, 11, 6, 23, 30, 0, 0, time.UTC) // 7 Nov in Bucharest
	sync := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	loc, err := time.LoadLocation("Europe/Bucharest")
	if err != nil {
		t.Fatal(err)
	}
	v := leadIntentView(leadstore.IntentRow{Class: "paper", Reason: "Instagram shows a paper stamp card", Overlap: 2,
		Yours: true, SignalAt: &sig, SyncedAt: &sync}, loc)
	want := console.LeadIntentView{Class: "paper", Label: "Own scheme", Reason: "Instagram shows a paper stamp card", Date: "7 Nov", Overlap: 2, Yours: true}
	if *v != want {
		t.Errorf("view %+v, want %+v", *v, want)
	}
	if v := leadIntentView(leadstore.IntentRow{Class: "just_chose", SyncedAt: &sync}, loc); v.Date != "2 Nov" || v.Label != "Just chose" {
		t.Errorf("no signal: %+v, want the sync's date", *v)
	}
	if v := leadIntentView(leadstore.IntentRow{Class: "cold"}, loc); v.Date != "" || v.Label != "Cold" {
		t.Errorf("never synced: %+v", *v)
	}
}

// TestLeadListRowIntent: a café row carries its intent class; any other lead (which the store
// reads cold) carries none.
func TestLeadListRowIntent(t *testing.T) {
	now := time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)
	if r := leadListRow(leadstore.ListRow{LeadID: 1, PlaceType: "cafe", IntentClass: "shopping"}, nil, leadsWeighing{}, nil, now); r.IntentClass != "shopping" || r.IntentChip() != "Shopping" {
		t.Errorf("café row: %q / %q", r.IntentClass, r.IntentChip())
	}
	if r := leadListRow(leadstore.ListRow{LeadID: 2, IntentClass: "cold"}, nil, leadsWeighing{}, nil, now); r.IntentClass != "" {
		t.Errorf("Drupal row carries %q", r.IntentClass)
	}
}
