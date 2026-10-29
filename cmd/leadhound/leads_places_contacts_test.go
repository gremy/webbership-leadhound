package main

import (
	"slices"
	"strings"
	"testing"

	"github.com/gremy/webbership-leadhound/internal/contacts"
)

var placeTestRoles = []string{"office", "contact", "info", "hello", "kontakt"}

// mailAt is local@domain built at runtime: a mailbox provider's address in a test is
// fictional all the same, and built this way no literal address sits in the tree (as
// internal/contacts' tests build theirs).
func mailAt(local, domain string) string { return local + "@" + domain }

// TestPlaceEmailsMatchContactSearch is the contract drift test: a place's listed addresses
// are kept exactly when the contact search keeps them on the org's own page (the same
// addresses printed as page text and as mailto links), so the two can never disagree on
// role local parts, other domains or people's addresses.
func TestPlaceEmailsMatchContactSearch(t *testing.T) {
	addrs := []string{
		"Info@Cafe-Nord.example", "hello@shop.cafe-nord.example", "contact-ro@cafe-nord.example",
		"ana.ionescu@cafe-nord.example", "office@elsewhere.example", mailAt("cafenord", "gmail.com"),
		mailAt("maria.pop", "gmail.com"), mailAt("info", "gmail.com"), "kontakt.bar@cafe-nord.example", "contactless@cafe-nord.example",
	}
	r := placeContactRules{roles: placeTestRoles}
	var got []string
	for _, e := range r.emails(addrs, "cafe-nord.example") {
		got = append(got, e.addr)
		if e.mxDomain != "cafe-nord.example" {
			t.Errorf("%s: MX domain %q, want the org's", e.addr, e.mxDomain)
		}
	}
	for _, page := range []string{
		"<p>" + strings.Join(addrs, " · ") + "</p>",
		`<a href="mailto:` + strings.Join(addrs, `"></a><a href="mailto:`) + `"></a>`,
	} {
		want := contacts.Extract("https://cafe-nord.example/contact", []byte(page), "cafe-nord.example", placeTestRoles).Emails
		if !slices.Equal(got, want) {
			t.Errorf("place emails %v, the contact search keeps %v from %q", got, want, page)
		}
	}
	if !slices.Contains(got, "info@cafe-nord.example") || slices.Contains(got, mailAt("cafenord", "gmail.com")) {
		t.Errorf("place emails %v", got)
	}
}

// TestPlaceEmailsNoWebsite: a café with no website of its own holds each address to its own
// domain, so a role address on the café's domain is kept; any address on a personal
// mailbox provider is dropped, a role local part included, as the contact
// search drops it as foreign; an address on a public body's domain is never kept.
func TestPlaceEmailsNoWebsite(t *testing.T) {
	r := placeContactRules{roles: placeTestRoles, publicSuffixes: []string{"gov.example"}}
	var got []string
	for _, e := range r.emails([]string{
		" Contact@Cafe-Luna.example ", mailAt("maria.pop", "gmail.com"), mailAt("cafeluna", "gmail.com"), "contact@cafe-luna.example",
		"info@primaria.gov.example", "office@mail.cafe-sud.example", mailAt("info", "gmail.com"), mailAt("office", "yahoo.ro"),
		mailAt("contact", "hotmail.co.uk"), mailAt("hello", "icloud.com"),
	}, "") {
		got = append(got, e.addr+">"+e.mxDomain)
	}
	want := []string{"contact@cafe-luna.example>cafe-luna.example", "office@mail.cafe-sud.example>cafe-sud.example"}
	if !slices.Equal(got, want) {
		t.Errorf("no-website emails %v, want %v", got, want)
	}
	if got := r.emails([]string{"info@cafe.gov.example"}, "cafe.gov.example"); len(got) != 0 {
		t.Errorf("public body org kept %v", got)
	}
}

// TestPlaceEmailsNoWebsitePlatform: a café whose listed website was a platform's has no
// domain of its own, so an address on a platform's or brand's domain is the platform's,
// never kept; the café's own address is.
func TestPlaceEmailsNoWebsitePlatform(t *testing.T) {
	r := placeContactRules{roles: placeTestRoles}
	var got []string
	for _, e := range r.emails([]string{
		mailAt("office", "meniudigital.ro"), mailAt("info", "demo.wixsite.com"), mailAt("contact", "lavazza.example"),
		mailAt("hello", "google.ro"), "contact@cafe-mira.example",
	}, "") {
		got = append(got, e.addr)
	}
	if want := []string{"contact@cafe-mira.example"}; !slices.Equal(got, want) {
		t.Errorf("no-website emails %v, want %v", got, want)
	}
}

// TestPlacePhone: a listed number is stored as the research and the screening registers
// read it (phones.E164): a national number gets the country's code, junk is no phone.
func TestPlacePhone(t *testing.T) {
	for _, c := range []struct {
		raw, country, value, norm string
		ok                        bool
	}{
		{"+40 700 000 201", "RO", "+40 700 000 201", "+40700000201", true},
		{"0700 000 202", "RO", "0700 000 202", "+40700000202", true},
		{"0040 700 000 203", "RO", "0040 700 000 203", "+40700000203", true},
		{"call us", "RO", "", "", false},
		{"12", "RO", "", "", false},
	} {
		v, n, ok := placePhone(c.raw, c.country)
		if v != c.value || n != c.norm || ok != c.ok {
			t.Errorf("placePhone(%q, %s) = %q %q %v, want %q %q %v", c.raw, c.country, v, n, ok, c.value, c.norm, c.ok)
		}
	}
}
