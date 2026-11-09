package console

import (
	"reflect"
	"strings"
	"testing"

	"github.com/gremy/webbership-leadhound/internal/leadrate"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
)

func TestLeadLineCafe(t *testing.T) {
	cases := []struct {
		name      string
		f         LineFacts
		full, row string
	}{
		{
			"kind, town, vendor",
			LineFacts{RType: leadstore.TypeCafe, BusinessKind: leadrate.BusinessKindSpecialtyCoffee, Town: "Northtown", HasWebsite: true, SiteRead: true, Vendor: "Stampwise"},
			"Specialty coffee shop · Northtown · on Stampwise",
			"Specialty coffee shop · on Stampwise",
		},
		{
			"site read, no loyalty",
			LineFacts{RType: leadstore.TypeCafe, BusinessKind: leadrate.BusinessKindBakeryCafe, Town: "Northtown", HasWebsite: true, SiteRead: true},
			"Bakery café · Northtown · no loyalty seen",
			"Bakery café · no loyalty seen",
		},
		{
			"site read, loyalty seen with no named vendor",
			LineFacts{RType: leadstore.TypeCafe, BusinessKind: leadrate.BusinessKindBakeryCafe, Town: "Northtown", HasWebsite: true, SiteRead: true, LoyaltySeen: true},
			"Bakery café · Northtown · loyalty seen",
			"Bakery café · loyalty seen",
		},
		{
			"site not read says nothing of loyalty",
			LineFacts{RType: leadstore.TypeCafe, BusinessKind: leadrate.BusinessKindBarLounge, Town: "Northtown", HasWebsite: true},
			"Bar or lounge · Northtown",
			"Bar or lounge",
		},
		{
			"no website",
			LineFacts{RType: leadstore.TypeCafe, BusinessKind: leadrate.BusinessKindVenueCafe, Town: "Northtown"},
			"Café in a venue · Northtown · no website",
			"Café in a venue · no website",
		},
		{
			"no kind falls back to coffee_shop category",
			LineFacts{RType: leadstore.TypeCafe, Category: "coffee_shop", Town: "Northtown"},
			"Coffee shop · Northtown · no website",
			"Coffee shop · no website",
		},
		{
			"no kind falls back to cafe category",
			LineFacts{RType: leadstore.TypeCafe, Category: "cafe", Town: "Northtown", HasWebsite: true},
			"Café · Northtown",
			"Café",
		},
		{
			"unclear kind and other category give Café",
			LineFacts{RType: leadstore.TypeCafe, BusinessKind: leadrate.BusinessKindUnclear, Category: "bakery", HasWebsite: true},
			"Café",
			"Café",
		},
		{
			"nothing known gives Café",
			LineFacts{RType: leadstore.TypeCafe, HasWebsite: true},
			"Café",
			"Café",
		},
		{
			"roastery place type wins over a stored specialty_coffee",
			LineFacts{RType: leadstore.TypeCafe, BusinessKind: leadrate.BusinessKindSpecialtyCoffee, PlaceType: leadstore.PlaceRoastery, Town: "Northtown", HasWebsite: true},
			"Roastery or online store · Northtown",
			"Roastery or online store",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := LeadLine(c.f); got != c.full {
				t.Errorf("LeadLine = %q, want %q", got, c.full)
			}
			if got := LeadLineInTable(c.f); got != c.row {
				t.Errorf("LeadLineInTable = %q, want %q", got, c.row)
			}
		})
	}
}

func TestLeadLineDrupal(t *testing.T) {
	cases := []struct {
		name      string
		f         LineFacts
		full, row string
	}{
		{
			"kind, country, finding, date",
			LineFacts{RType: leadstore.TypeDrupal, BusinessKind: leadrate.BusinessKindEducation, Country: "Ireland", Finding: "Drupal 10", EOL: "9 Dec 2026"},
			"School or university · Ireland · Drupal 10 · end of life 9 Dec 2026",
			"School or university",
		},
		{
			"unclear kind gives no words",
			LineFacts{RType: leadstore.TypeDrupal, BusinessKind: leadrate.BusinessKindUnclear, Country: "Ireland", Finding: "Drupal 10"},
			"Ireland · Drupal 10",
			"",
		},
		{
			"no kind falls back to country and finding",
			LineFacts{RType: leadstore.TypeDrupal, Country: "Ireland", Finding: "Drupal 7", EOL: "5 Jan 2025"},
			"Ireland · Drupal 7 · end of life 5 Jan 2025",
			"",
		},
		{
			"kind only",
			LineFacts{RType: leadstore.TypeDrupal, BusinessKind: leadrate.BusinessKindAgency},
			"Agency",
			"Agency",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := LeadLine(c.f); got != c.full {
				t.Errorf("LeadLine = %q, want %q", got, c.full)
			}
			if got := LeadLineInTable(c.f); got != c.row {
				t.Errorf("LeadLineInTable = %q, want %q", got, c.row)
			}
		})
	}
}

// TestLeadLineNoEmptyPart sweeps every combination of empty and filled facts and checks
// that no line holds an empty part or a doubled separator.
func TestLeadLineNoEmptyPart(t *testing.T) {
	for _, rtype := range []string{leadstore.TypeCafe, leadstore.TypeDrupal} {
		for mask := range 1 << 9 {
			pick := func(bit int, v string) string {
				if mask&(1<<bit) != 0 {
					return v
				}
				return ""
			}
			f := LineFacts{
				RType:        rtype,
				BusinessKind: pick(0, leadrate.BusinessKindUnclear),
				PlaceType:    pick(1, leadstore.PlaceRoastery),
				Category:     pick(2, "cafe"),
				Town:         pick(3, "Northtown"),
				Country:      pick(4, "Ireland"),
				Vendor:       pick(5, "Stampwise"),
				Finding:      pick(6, "Drupal 10"),
				EOL:          pick(7, "9 Dec 2026"),
				HasWebsite:   mask&(1<<8) != 0,
				SiteRead:     mask&(1<<8) != 0,
			}
			for _, line := range []string{LeadLine(f), LeadLineInTable(f)} {
				if strings.Contains(line, " ·  · ") || strings.HasPrefix(line, " · ") || strings.HasSuffix(line, " · ") {
					t.Fatalf("line %q for %+v has an empty part", line, f)
				}
				for part := range strings.SplitSeq(line, " · ") {
					if line != "" && strings.TrimSpace(part) == "" {
						t.Fatalf("line %q for %+v has an empty part", line, f)
					}
				}
			}
		}
	}
}

// TestLineFactsHoldNoContact checks by reflection that the line's facts have no field for
// an email, a phone or a route, so the line can never show one.
func TestLeadLineFactsHoldNoContact(t *testing.T) {
	typ := reflect.TypeFor[LineFacts]()
	for i := range typ.NumField() {
		name := strings.ToLower(typ.Field(i).Name)
		for _, banned := range []string{"email", "mail", "phone", "route", "address"} {
			if strings.Contains(name, banned) {
				t.Errorf("LineFacts has a contact field %q", typ.Field(i).Name)
			}
		}
	}
}

func TestBusinessKindWordsCoversEveryKind(t *testing.T) {
	for _, kind := range leadrate.BusinessKinds() {
		got := BusinessKindWords(kind)
		if kind == leadrate.BusinessKindUnclear {
			if got != "" {
				t.Errorf("BusinessKindWords(unclear) = %q, want empty", got)
			}
			continue
		}
		if got == "" {
			t.Errorf("BusinessKindWords(%q) is empty", kind)
		}
	}
	if got := BusinessKindWords("not_a_kind"); got != "" {
		t.Errorf("BusinessKindWords(unknown) = %q, want empty", got)
	}
}

func TestTileLetter(t *testing.T) {
	cases := []struct{ name, domain, want string }{
		{"șa", "", "Ș"},
		{"Northtown Coffee", "northtown.example", "N"},
		{"́\"(élan)", "", "É"},
		{"'Cafe", "", "C"},
		{"3 Beans", "", "3"},
		{"", "beans.example", "B"},
		{"  ", "-9lives.example", "9"},
		{"", "", "#"},
		{"--", "..", "#"},
	}
	for _, c := range cases {
		if got := TileLetter(c.name, c.domain); got != c.want {
			t.Errorf("TileLetter(%q, %q) = %q, want %q", c.name, c.domain, got, c.want)
		}
	}
}
