package main

import (
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/cafe"
	"github.com/gremy/webbership-leadhound/internal/console"
	"github.com/gremy/webbership-leadhound/internal/leadconfig"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
)

// TestRowSummaryCafeFacts: a café row's line facts come from the row and its extras. A café
// whose org carries the place stand-in domain has no website, even though its domain is not
// empty; a café whose finding rests on a read homepage was read, and its loyalty vendor is
// named as signals.yaml names it, from the finding's evidence.
func TestRowSummaryCafeFacts(t *testing.T) {
	var vendor cafe.Vendor
	for _, v := range cafe.DefaultConfig().Vendors {
		if v.Kind == cafe.SigLoyalty && !v.Own && v.Name != "" {
			vendor = v
			break
		}
	}
	if vendor.ID == "" {
		t.Fatal("signals.yaml names no loyalty vendor")
	}
	row := console.LeadListRow{RType: leadstore.TypeCafe, PlaceType: leadstore.PlaceCafe, Town: "Fictiveton", Country: "GB",
		Domain: leadstore.PlaceOrgPrefix + "fictive-1"}
	got := rowLineFacts(row, leadstore.FindingSubject{}, leadconfig.Facts{}, leadstore.RowExtras{Category: "coffee_shop", BusinessKind: "bakery_cafe"})
	want := console.LineFacts{RType: leadstore.TypeCafe, BusinessKind: "bakery_cafe", PlaceType: leadstore.PlaceCafe,
		Category: "coffee_shop", Town: "Fictiveton", Country: "GB"}
	if got != want {
		t.Errorf("a café with no website: %+v, want %+v", got, want)
	}

	row.Domain = "fictive.example"
	sig := cafe.Signal{Kind: cafe.SigLoyalty, Vendor: vendor.ID, Via: cafe.ViaScript, Match: "fictive.example"}
	got = rowLineFacts(row, leadstore.FindingSubject{}, leadconfig.Facts{}, leadstore.RowExtras{SiteRead: true,
		CafeWebsite: "fictive.example", Signals: []string{cafe.SigLoyalty + ":" + sig.String()}})
	if !got.HasWebsite || !got.SiteRead || got.Vendor != vendor.Name {
		t.Errorf("a read café on a vendor: %+v, want the vendor's name", got)
	}
	got = rowLineFacts(row, leadstore.FindingSubject{}, leadconfig.Facts{}, leadstore.RowExtras{SiteRead: true, CafeWebsite: "fictive.example"})
	if !got.HasWebsite || !got.SiteRead || got.Vendor != "" {
		t.Errorf("a read café with no loyalty: %+v", got)
	}
}

// TestRowSummaryDrupalFacts: a Drupal row's line facts hold its finding without the support
// clause, and the end-of-life date of its major on its own, only when the facts know it.
func TestRowSummaryDrupalFacts(t *testing.T) {
	facts := leadconfig.Facts{D10EOL: leadconfig.Date{Time: time.Date(2026, 12, 9, 0, 0, 0, 0, time.UTC)}}
	row := console.LeadListRow{RType: leadstore.TypeDrupal, Country: "GB", Domain: "fictive.example"}
	got := rowLineFacts(row, leadstore.FindingSubject{Kind: "drupal_eol", Major: 10}, facts, leadstore.RowExtras{BusinessKind: "education"})
	want := console.LineFacts{RType: leadstore.TypeDrupal, BusinessKind: "education", Country: "GB", HasWebsite: true,
		Finding: "Drupal 10", EOL: "9 December 2026"}
	if got != want {
		t.Errorf("a Drupal 10 row: %+v, want %+v", got, want)
	}
	if got := rowLineFacts(row, leadstore.FindingSubject{Kind: "drupal_eol", ExactVersion: "11.1.0"}, facts, leadstore.RowExtras{}); got.Finding != "Drupal 11.1.0" || got.EOL != "" {
		t.Errorf("a Drupal 11 row: finding %q, end of life %q", got.Finding, got.EOL)
	}
}

// TestRowSummaryExtras: rowExtras fills the full line, the table's short line, the site's
// description on one line as About, and the business kind in words as Kind.
func TestRowSummaryExtras(t *testing.T) {
	row := console.LeadListRow{RType: leadstore.TypeCafe, PlaceType: leadstore.PlaceCafe, Town: "Fictiveton", Domain: "fictive.example"}
	x := leadstore.RowExtras{Description: " A fictive  café\nby the river. ", BusinessKind: "bakery_cafe", SiteRead: true}
	rowExtras(&row, leadstore.ListRow{State: leadstore.LeadNew}, leadconfig.Facts{}, x)
	f := rowLineFacts(row, leadstore.FindingSubject{}, leadconfig.Facts{}, x)
	if row.Summary != console.LeadLine(f) || row.SummaryShort != console.LeadLineInTable(f) || row.Summary == "" {
		t.Errorf("lines %q / %q", row.Summary, row.SummaryShort)
	}
	if row.Details.About != "A fictive café by the river." || row.Details.Kind != "Bakery café" || row.BusinessKind != "bakery_cafe" {
		t.Errorf("about %q, kind %q / %q", row.Details.About, row.Details.Kind, row.BusinessKind)
	}
}

// TestRowSummaryTile: the tile takes the org's real name, else its real domain; a place org's
// stand-in domain is neither, and the row keeps the raw business kind.
func TestRowSummaryTile(t *testing.T) {
	now := time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)
	if r := leadListRow(leadstore.ListRow{LeadID: 1, Name: "șa fictive", Domain: "fictive.example"}, nil, leadsWeighing{}, nil, now); r.Tile != "Ș" {
		t.Errorf("named org's tile %q", r.Tile)
	}
	stand := leadstore.PlaceOrgPrefix + "fictive-2"
	if r := leadListRow(leadstore.ListRow{LeadID: 2, Name: stand, Domain: stand}, nil, leadsWeighing{}, nil, now); r.Tile != "#" || r.Name != "Lead 2" {
		t.Errorf("a place org's tile %q, name %q", r.Tile, r.Name)
	}
	if r := leadListRow(leadstore.ListRow{LeadID: 3, Domain: "9fictive.example"}, nil, leadsWeighing{}, nil, now); r.Tile != "9" {
		t.Errorf("an unnamed org's tile %q", r.Tile)
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
