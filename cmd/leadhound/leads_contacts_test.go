package main

import (
	"bytes"
	"context"
	"io"
	"testing"

	"github.com/gremy/webbership-leadhound/internal/leadstore"
)

func TestIsPublicBody(t *testing.T) {
	suffixes := []string{"gov.example", ".ac.example ", ""}
	for d, want := range map[string]bool{
		"gov.example":         true,
		"town.gov.example":    true,
		"uni.ac.example":      true,
		"northwind.example":   false,
		"notgov.example":      false,
		"gov.example.invalid": false,
		"Council.GOV.example": true,
	} {
		if got := isPublicBody(d, suffixes); got != want {
			t.Errorf("isPublicBody(%q) = %v, want %v", d, got, want)
		}
	}
}

func TestWriteRoutesCSV(t *testing.T) {
	rows := []leadstore.ExportRow{
		{Domain: "example.org", Kind: "site_owner", Name: "=cmd", Country: "NL", Major: 10, ExactVersion: "10.2.7",
			RouteKind: "form", Route: "https://example.org/contact"},
		{Domain: "example.net", Kind: "site_owner", Name: "Net", Country: "GB", Major: 7,
			RouteKind: "email", Route: "info@example.net", MXOK: true},
		{Domain: "example.com", Kind: "agency", Name: "Agency", Phrase: "we upgrade Drupal 7",
			FindingURL: "https://example.com/upgrades", RouteKind: "email", Route: "hello@example.com", MXOK: true},
	}
	var b bytes.Buffer
	if err := writeRoutesCSV(&b, rows); err != nil {
		t.Fatal(err)
	}
	want := "domain,kind,name,country,finding,finding_url,route_kind,route,mx_ok\n" +
		"example.org,site_owner,'=cmd,NL,D10 10.2.7,,form,https://example.org/contact,false\n" +
		"example.net,site_owner,Net,GB,D7,,email,info@example.net,true\n" +
		"example.com,agency,Agency,,we upgrade Drupal 7,https://example.com/upgrades,email,hello@example.com,true\n"
	if b.String() != want {
		t.Errorf("csv:\n%s\nwant:\n%s", b.String(), want)
	}
}

func TestLeadsContactsAndExportUsage(t *testing.T) {
	for _, args := range [][]string{
		{"leads", "contacts", "extra"},
		{"leads", "contacts", "--max", "0"},
		{"leads", "contacts", "--org", "x"},
		{"leads", "contacts", "--dry-run"},
		{"leads", "export"},
		{"leads", "export", "leads"},
		{"leads", "export", "routes", "--limit", "0"},
	} {
		if code := run(context.Background(), args, io.Discard); code != 2 {
			t.Errorf("%q exited %d, want 2", args, code)
		}
	}
}
