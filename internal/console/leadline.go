package console

import (
	"strings"
	"unicode"

	"github.com/gremy/webbership-leadhound/internal/leadstore"
)

// LineFacts are the facts a lead's one-line summary is made from.
// It holds no email, phone or route, so the line can never show one.
type LineFacts struct {
	// RType is the lead's rating type, leadstore.TypeCafe or leadstore.TypeDrupal.
	RType string
	// BusinessKind is the stored business kind, or empty when none is stored.
	BusinessKind string
	// PlaceType is the café lead's effective place type, as leadstore names it.
	PlaceType string
	// Category is the place category the café was found under, such as coffee_shop.
	Category string
	Town     string
	Country  string
	// HasWebsite is true when the lead has a real website of its own.
	HasWebsite bool
	// SiteRead is true when that website was read for loyalty signals.
	SiteRead bool
	// LoyaltySeen is true when the site shows digital loyalty, a named vendor's or its own.
	LoyaltySeen bool
	// Vendor is the loyalty vendor seen on the site, or empty when none was seen.
	Vendor string
	// Finding is the Drupal finding, such as "Drupal 10".
	Finding string
	// EOL is the finding's end-of-life date, or empty when it is not known.
	EOL string
}

// lineSep joins the parts of a lead's line.
const lineSep = " · "

// roasteryWords describe a lead whose place type is roastery, whatever its stored kind.
const roasteryWords = "Roastery or online store"

// LeadLine is the lead's full one-line summary, for the narrow cards, the card examples and
// the table's title.
func LeadLine(f LineFacts) string {
	return leadLine(f, true)
}

// LeadLineInTable is the lead's line without what the table's Where and Finding columns
// already show: the town, the country, the Drupal finding and its end-of-life date.
func LeadLineInTable(f LineFacts) string {
	return leadLine(f, false)
}

// leadLine builds the line, with or without the parts the table shows in its own columns.
func leadLine(f LineFacts, full bool) string {
	var parts []string
	add := func(s string) {
		if s = strings.TrimSpace(s); s != "" {
			parts = append(parts, s)
		}
	}
	if f.RType == leadstore.TypeCafe {
		add(cafeWords(f))
		if full {
			add(f.Town)
		}
		if !f.HasWebsite {
			add("no website")
		}
		switch {
		case strings.TrimSpace(f.Vendor) != "":
			add("on " + strings.TrimSpace(f.Vendor))
		case f.LoyaltySeen:
			add("loyalty seen")
		case f.SiteRead:
			add("no loyalty seen")
		}
		return strings.Join(parts, lineSep)
	}
	add(BusinessKindWords(f.BusinessKind))
	if full {
		add(f.Country)
		add(f.Finding)
		if eol := strings.TrimSpace(f.EOL); eol != "" {
			add("end of life " + eol)
		}
	}
	return strings.Join(parts, lineSep)
}

// cafeWords names what kind of café the lead is: roastery first, then the stored kind,
// then the place category, else plain "Café".
func cafeWords(f LineFacts) string {
	if f.PlaceType == leadstore.PlaceRoastery {
		return roasteryWords
	}
	if w := BusinessKindWords(f.BusinessKind); w != "" {
		return w
	}
	if f.Category == "coffee_shop" {
		return "Coffee shop"
	}
	return "Café"
}

// businessKindWords are the plain words for each business kind. Unclear has none.
var businessKindWords = map[string]string{
	"specialty_coffee":   "Specialty coffee shop",
	"neighbourhood_cafe": "Neighbourhood café",
	"bakery_cafe":        "Bakery café",
	"restaurant_cafe":    "Café restaurant",
	"bar_lounge":         "Bar or lounge",
	"venue_cafe":         "Café in a venue",
	"roastery_shop":      "Roastery shop",
	"education":          "School or university",
	"government":         "Public body",
	"healthcare":         "Healthcare",
	"nonprofit":          "Nonprofit or association",
	"company":            "Company",
	"media":              "Media or publisher",
	"agency":             "Agency",
	"personal":           "Personal site",
}

// BusinessKindWords is the plain words for a stored business kind, or "" for unclear or
// an unknown kind.
func BusinessKindWords(kind string) string {
	return businessKindWords[kind]
}

// TileLetter is the letter shown on a lead's tile: the first letter or digit of the name,
// upper-cased, else of the domain, else "#". The caller passes the org's real name and its
// real domain, never a stand-in.
func TileLetter(name, domain string) string {
	for _, s := range []string{name, domain} {
		for _, r := range s {
			if unicode.IsLetter(r) || unicode.IsDigit(r) {
				return string(unicode.ToUpper(r))
			}
		}
	}
	return "#"
}
