package main

import (
	"slices"
	"testing"

	"github.com/gremy/webbership-leadhound/internal/cafe"
	"github.com/gremy/webbership-leadhound/internal/campaign"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/probe"
)

// TestRelevanceSortLabel: the relevance sort says by score while no type is trusted, plain
// "Relevance" when every type is, and names the trusted types otherwise.
func TestRelevanceSortLabel(t *testing.T) {
	cases := []struct {
		trusted []string
		want    string
	}{
		{nil, "Relevance (by score for now)"},
		{[]string{leadstore.TypeDrupal}, "Relevance (Drupal leads; the rest by score)"},
		{[]string{leadstore.TypeCafe}, "Relevance (Café leads; the rest by score)"},
		{[]string{leadstore.TypeCafe, leadstore.TypeDrupal}, "Relevance"},
	}
	for _, c := range cases {
		if got := relevanceSortLabel(c.trusted); got != c.want {
			t.Errorf("%v: %q, want %q", c.trusted, got, c.want)
		}
	}
}

// TestHasReport: only the Drupal site kinds and a hand-added site build a report; a café or
// an agency lead never does.
func TestHasReport(t *testing.T) {
	for _, k := range []string{leadstore.LeadUnsupported, leadstore.LeadD10, leadstore.LeadHand} {
		if !hasReport(k) {
			t.Errorf("%s: no report", k)
		}
	}
	others := []string{leadstore.LeadAgencyPartner, leadstore.LeadSelftest}
	for _, k := range cafe.Kinds {
		others = append(others, k.Kind)
	}
	for _, k := range others {
		if hasReport(k) {
			t.Errorf("%s: has a report", k)
		}
	}
}

// TestOriginWords: every stored origin the lead page can show reads as words; an unknown one
// stays as stored.
func TestOriginWords(t *testing.T) {
	cases := map[string]string{
		leadstore.OriginPlace: "a map listing (Overture)", leadstore.OriginMajestic: "the top-sites list",
		leadstore.OriginTranco: "the top-sites list", leadstore.OriginHTTPArchive: "the top-sites list",
		leadstore.OriginManual: "added by hand", leadstore.OriginAgency: "the agencies list",
		leadstore.OrgSourceAgencies: "the agencies list", "something-new": "something-new",
	}
	for in, want := range cases {
		if got := originWords(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}

// TestCafeView: vendor ids read as signals.yaml names them, a phrase with no vendor reads as
// such, and the comparison link follows the batch rule: the vendor's link, else the
// campaign's default, else none.
func TestCafeView(t *testing.T) {
	sigs := []leadstore.Signal{
		{Signal: cafe.SigLoyalty, Value: "loyalty:stampme:script:https://stampme.com/w.js"},
		{Signal: cafe.SigOrdering, Value: "ordering:glovo:link:https://glovoapp.com/ro/x"},
		{Signal: cafe.SigOrdering, Value: "ordering:glovo:link:https://glovoapp.com/ro/y"},
		{Signal: cafe.SigInstagram, Value: "instagram:instagram:link:https://instagram.com/example"},
		{Signal: probe.SigCafeLang, Value: "ro:html-lang:ro-RO"},
	}
	links := map[string]string{"stampme": "https://example.test/vs-stampme", campaign.VendorLinkDefault: "https://example.test/vs"}
	v := cafeView(probe.CafeResult(sigs), "", links)
	if !slices.Equal(v.Loyalty, []string{"Stamp Me"}) || !slices.Equal(v.Ordering, []string{"Glovo"}) || !v.Instagram || v.Language != "Romanian" {
		t.Errorf("view %+v", v)
	}
	if v.VendorName != "Stamp Me" || v.VendorLink != "https://example.test/vs-stampme" {
		t.Errorf("vendor %q %q", v.VendorName, v.VendorLink)
	}

	none := cafeView(probe.CafeResult(nil), "en", links)
	if len(none.Loyalty) != 0 || len(none.Ordering) != 0 || none.VendorName != "" || none.VendorLink != "https://example.test/vs" || none.Language != "English" {
		t.Errorf("no vendor: %+v", none)
	}
	if bare := cafeView(probe.CafeResult(nil), "", nil); bare.VendorLink != "" || bare.Language != "" {
		t.Errorf("no links: %+v", bare)
	}

	word := cafeView(probe.CafeResult([]leadstore.Signal{{Signal: cafe.SigLoyalty, Value: "loyalty:card-phrase:word:card de fidelitate"}}), "", nil)
	if len(word.Loyalty) != 1 || word.Loyalty[0] == "card-phrase" {
		t.Errorf("phrase only: %+v", word)
	}
}

// TestPlaceAddress: the address is the street line, postcode and town, and the country by
// name, whichever the place has.
func TestPlaceAddress(t *testing.T) {
	cases := []struct {
		pl   leadstore.LeadPlace
		want string
	}{
		{leadstore.LeadPlace{Freeform: "Strada Exemplu 1", Postcode: "100001", Locality: "Oraș Exemplu", Country: "RO"}, "Strada Exemplu 1, 100001 Oraș Exemplu, Romania"},
		{leadstore.LeadPlace{Locality: "Sat Exemplu", Country: "RO"}, "Sat Exemplu, Romania"},
		{leadstore.LeadPlace{}, ""},
	}
	for _, c := range cases {
		if got := placeAddress(c.pl); got != c.want {
			t.Errorf("%+v: %q, want %q", c.pl, got, c.want)
		}
	}
}
