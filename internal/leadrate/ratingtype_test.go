package leadrate

// These values were recorded from the code before the café question existed and must
// never be edited. If this test fails, the change re-rates every Drupal lead: a Drupal
// lead's state or input hash must stay exactly what it was.

import (
	"encoding/hex"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/leadstore"
)

// pinDecider is the fixed decider name the pinned hashes were recorded under.
const pinDecider = "pin-decider-model"

// The pinned input hashes (hex) of the four pinned Drupal leads.
const (
	pinDrupalUnsupportedHash = "99a0b05cbaa124d0062a4604c2e36a8c4902f10252311f7acc62f5114b6f27c7"
	pinDrupalD10Hash         = "9433f57c8a3a9d882aafa56732674e4737a62219f00a9c50fa55b012ee0f5957"
	pinDrupalHandHash        = "7fc0a1c3d5b6fdc6ff4c69f51e7bc43e128a3cc3da60d43fc41a1b4496c06901"
	pinDrupalAgencyHash      = "a7a285b30d2f798a7e029fd53a3f51de2c4322bbdba8be617f0d30381a102ce7"
)

// The pinned states of the four pinned Drupal leads, as the decider reads them.
const (
	pinDrupalUnsupportedState = "Lead type: a site on an out-of-support Drupal version\n" +
		"Drupal major version: 7\n" +
		"Exact Drupal version: 7.98\n" +
		"End-of-life date of that major version: 5 January 2025\n" +
		"Country: GB\n" +
		"Top-sites list rank: 48210\n" +
		"Site name: Northwind Traders\n" +
		"Description: Wholesale fine foods at northwind.example.\n" +
		"Contact channels: email, form\n" +
		"Phone number on the site: yes\n" +
		"Social links: 3"
	pinDrupalD10State = "Lead type: a Drupal 10 site\n" +
		"Drupal major version: 10\n" +
		"Exact Drupal version: 10.3.6\n" +
		"End-of-life date of that major version: 9 December 2026\n" +
		"Country: DE\n" +
		"Top-sites list rank: not in the list\n" +
		"Site name: Contoso Verlag\n" +
		"Description: A publisher at contoso.invalid.\n" +
		"Contact channels: email\n" +
		"Phone number on the site: no\n" +
		"Social links: 1"
	pinDrupalHandState = "Lead type: a site added by hand\n" +
		"Drupal major version: unknown\n" +
		"Exact Drupal version: unknown\n" +
		"End-of-life date of that major version: unknown\n" +
		"Country: IE\n" +
		"Top-sites list rank: not in the list\n" +
		"Site name: Fabrikam Museum\n" +
		"Description: A small museum at fabrikam.example.\n" +
		"Contact channels: form\n" +
		"Phone number on the site: yes\n" +
		"Social links: 0"
	pinDrupalAgencyState = "Lead type: an agency that sells Drupal upgrades\n" +
		"Drupal major version: 10\n" +
		"Exact Drupal version: 10.2.0\n" +
		"End-of-life date of that major version: unknown\n" +
		"Country: NL\n" +
		"Top-sites list rank: 120345\n" +
		"Site name: Tailspin Digital\n" +
		"Description: A Drupal agency at tailspin.invalid.\n" +
		"Contact channels: email, form\n" +
		"Phone number on the site: no\n" +
		"Social links: 4"
)

func pinnedDrupalInputs() []struct {
	name, hash, state string
	in                Input
} {
	return []struct {
		name, hash, state string
		in                Input
	}{
		{"unsupported", pinDrupalUnsupportedHash, pinDrupalUnsupportedState, Input{Kind: leadstore.LeadUnsupported, Major: 7, Version: "7.98",
			EOL: time.Date(2025, time.January, 5, 0, 0, 0, 0, time.UTC), Country: "GB", ListRank: 48210,
			SiteName: "Northwind Traders", Description: "Wholesale fine foods at northwind.example.",
			Channels: []string{"form", "email"}, HasPhone: true, SocialLinks: 3}},
		{"d10", pinDrupalD10Hash, pinDrupalD10State, Input{Kind: leadstore.LeadD10, Major: 10, Version: "10.3.6",
			EOL: time.Date(2026, time.December, 9, 0, 0, 0, 0, time.UTC), Country: "DE",
			SiteName: "Contoso Verlag", Description: "A publisher at contoso.invalid.",
			Channels: []string{"email"}, SocialLinks: 1}},
		{"hand", pinDrupalHandHash, pinDrupalHandState, Input{Kind: leadstore.LeadHand, Country: "IE",
			SiteName: "Fabrikam Museum", Description: "A small museum at fabrikam.example.",
			Channels: []string{"form"}, HasPhone: true}},
		{"agency_partner", pinDrupalAgencyHash, pinDrupalAgencyState, Input{Kind: leadstore.LeadAgencyPartner, Major: 10,
			Version: "10.2.0", Country: "NL", ListRank: 120345, SiteName: "Tailspin Digital",
			Description: "A Drupal agency at tailspin.invalid.", Channels: []string{"email", "form"},
			SocialLinks: 4}},
	}
}

func TestDrupalHashPinned(t *testing.T) {
	for _, c := range pinnedDrupalInputs() {
		if got := State(c.in); got != c.state {
			t.Errorf("%s: State changed:\n got %q\nwant %q", c.name, got, c.state)
		}
		if got := hex.EncodeToString(InputHash(pinDecider, c.in)); got != c.hash {
			t.Errorf("%s: InputHash changed: got %s, want %s", c.name, got, c.hash)
		}
	}
}
