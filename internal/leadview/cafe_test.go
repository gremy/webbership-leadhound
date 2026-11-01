package leadview

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/cafe"
	"github.com/gremy/webbership-leadhound/internal/leadconfig"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
)

func TestFindingLineCafe(t *testing.T) {
	s := leadstore.FindingSubject{Kind: "cafe_no_digital_loyalty", Detector: "cafe@1", Reason: "no digital loyalty on the homepage", Language: "ro", LanguageVia: "text"}
	if got, want := FindingLine(s, leadconfig.Facts{}, time.Now()), "Café: no digital loyalty on the homepage · language ro, from the page's text"; got != want {
		t.Errorf("FindingLine = %q, want %q", got, want)
	}
	s = leadstore.FindingSubject{Kind: "cafe_no_website", Detector: "cafe@1", Reason: "no website of its own on the map listing"}
	if got, want := FindingLine(s, leadconfig.Facts{}, time.Now()), "Café: no website of its own on the map listing"; got != want {
		t.Errorf("FindingLine = %q, want %q", got, want)
	}
}

func TestFindingLineCafeVendorNames(t *testing.T) {
	v := cafe.DefaultConfig().Vendor("stampme") // a real id/name pair from signals.yaml
	if v == nil {
		t.Fatal("signals.yaml has no stampme vendor")
	}
	cases := []struct{ reason, want string }{
		{"digital loyalty on the homepage: stampme", "Café: digital loyalty on the homepage: " + v.Name},
		{"digital loyalty on the homepage: stampme, fictional-loyalty-co", "Café: digital loyalty on the homepage: " + v.Name + ", fictional-loyalty-co"},
		{"digital loyalty on the homepage: fictional-loyalty-co", "Café: digital loyalty on the homepage: fictional-loyalty-co"},
		{`digital loyalty on the homepage: "card de fidelitate"`, `Café: digital loyalty on the homepage: "card de fidelitate"`},
		{"no digital loyalty on the homepage", "Café: no digital loyalty on the homepage"},
	}
	if v.Name != "Stamp Me" {
		t.Errorf("stampme name = %q, want Stamp Me", v.Name)
	}
	for _, tc := range cases {
		s := leadstore.FindingSubject{Kind: cafe.KindHasLoyalty, Detector: cafe.DerivedBy, Reason: tc.reason}
		if got := FindingLine(s, leadconfig.Facts{}, time.Now()); got != tc.want {
			t.Errorf("FindingLine(%q) = %q, want %q", tc.reason, got, tc.want)
		}
	}
}

func TestWhyLinesCafe(t *testing.T) {
	p := leadconfig.Profile{
		Kinds:     map[string]int{cafe.KindNoDigitalLoyalty: 25, cafe.KindHasLoyalty: 5, cafe.KindNoWebsite: 10},
		Countries: map[string]int{"RO": 10},
		Channels:  map[string]int{leadstore.ChannelEmail: 5},
	}
	cases := []struct {
		kind string
		want []string
	}{
		{cafe.KindNoDigitalLoyalty, []string{"Lead type, café with a website and no digital loyalty: +25", "Country you want, RO: +10", "Has an email address: +5"}},
		{cafe.KindHasLoyalty, []string{"Lead type, café already on a loyalty platform: +5", "Country you want, RO: +10", "Has an email address: +5"}},
		{cafe.KindNoWebsite, []string{"Lead type, café with no website of its own: +10", "Country you want, RO: +10", "Has an email address: +5"}},
	}
	for _, tc := range cases {
		got := WhyLines(leadstore.ScoreReasons(p, leadstore.Candidate{Kind: tc.kind, Country: "RO", Channel: leadstore.ChannelEmail}))
		if !slices.Equal(got, tc.want) {
			t.Errorf("WhyLines(%s) = %q, want %q", tc.kind, got, tc.want)
		}
		for _, line := range got {
			if strings.Contains(line, tc.kind) || strings.Contains(line, "_") || strings.Contains(line, "Drupal") {
				t.Errorf("WhyLines(%s) line %q carries a raw kind id or Drupal", tc.kind, line)
			}
		}
	}
}
