package main

// These values were recorded from the code before the café question existed and must
// never be edited. If this test fails, the change re-rates every Drupal lead: the input
// rateInput builds for a Drupal lead must hash exactly as it did.

import (
	"encoding/hex"
	"testing"

	"github.com/gremy/webbership-leadhound/internal/leadconfig"
	"github.com/gremy/webbership-leadhound/internal/leadrate"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
)

// pinRateDecider is the fixed decider name the pinned hashes were recorded under.
const pinRateDecider = "pin-decider-model"

// The pinned input hashes (hex) of rateInput for the four pinned Drupal leads.
const (
	pinRateUnsupportedHash = "48edd5475d81f4fffb051505882100ff4e700e2a0209de8c920fd92e917e43aa"
	pinRateD10Hash         = "5ad1e0c717fe59692e245b244bd254095792adff0f938c9859a40ab5bf02af6a"
	pinRateHandHash        = "cdd45535c48c6ca6dfc54e9e1f5a8664420d93c47a52ed40ed011eb4ff8f294b"
	pinRateAgencyHash      = "2e243920fac395006630e5d49d028ae6a01e9c4fe3ed7726ac8746994f4992b3"
)

// pinRateConfig is the config the pinned hashes were recorded under: fixed EOL facts.
func pinRateConfig() leadconfig.Config {
	var cfg leadconfig.Config
	cfg.Facts = leadconfig.Facts{
		D6EOL: leadconfig.ParseDate("2022-01-05"), D7EOL: leadconfig.ParseDate("2025-01-05"),
		D8EOL: leadconfig.ParseDate("2021-11-02"), D9EOL: leadconfig.ParseDate("2023-11-01"),
		D10EOL: leadconfig.ParseDate("2026-12-09"),
	}
	return cfg
}

// pinRateLeads are the four pinned Drupal leads with the hash each must give.
func pinRateLeads() []struct {
	name, hash string
	lead       leadstore.RateLead
} {
	rank := func(n int) *int { return &n }
	return []struct {
		name, hash string
		lead       leadstore.RateLead
	}{
		{"unsupported", pinRateUnsupportedHash, leadstore.RateLead{LeadID: 1, OrgID: 1, Domain: "northwind.example",
			Kind: leadstore.LeadUnsupported, Country: "GB", Major: 7, ExactVersion: "7.98", ListRank: rank(48210),
			SiteName: "Northwind Traders", Description: "Wholesale fine foods.", HasEmail: true, HasForm: true,
			HasPhone: true, SocialLinks: 3}},
		{"d10", pinRateD10Hash, leadstore.RateLead{LeadID: 2, OrgID: 2, Domain: "contoso.invalid",
			Kind: leadstore.LeadD10, Country: "DE", Major: 10, ExactVersion: "10.3.6",
			SiteName: "Contoso Verlag", Description: "A publisher.", HasEmail: true, SocialLinks: 1}},
		{"hand", pinRateHandHash, leadstore.RateLead{LeadID: 3, OrgID: 3, Domain: "fabrikam.example",
			Kind: leadstore.LeadHand, Country: "IE", SiteName: "Fabrikam Museum",
			Description: "A small museum.", HasForm: true, HasPhone: true}},
		{"agency_partner", pinRateAgencyHash, leadstore.RateLead{LeadID: 4, OrgID: 4, Domain: "tailspin.invalid",
			Kind: leadstore.LeadAgencyPartner, Country: "NL", Major: 10, ExactVersion: "10.2.0",
			ListRank: rank(120345), SiteName: "Tailspin Digital", Description: "A Drupal agency.",
			HasEmail: true, HasForm: true, SocialLinks: 4}},
	}
}

func TestRateInputDrupalPinned(t *testing.T) {
	cfg := pinRateConfig()
	for _, c := range pinRateLeads() {
		in := rateInput(c.lead, cfg)
		if got := hex.EncodeToString(leadrate.InputHash(pinRateDecider, in)); got != c.hash {
			t.Errorf("%s: rateInput hash changed: got %s, want %s", c.name, got, c.hash)
		}
	}
}
