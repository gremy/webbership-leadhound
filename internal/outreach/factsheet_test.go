package outreach

import (
	"slices"
	"testing"
	"time"
)

// TestFactSheetStableIDs: groups in order, numbered from 1 within each, fixed facts first,
// empty values out, the org's own text marked echo_guard.
func TestFactSheetStableIDs(t *testing.T) {
	s := groundSheet(t, "d7")
	var got []string
	for _, f := range s.Facts {
		got = append(got, f.ID+" "+f.Label)
	}
	want := []string{
		"F1 org_name", "F2 org_country", "F3 org_domain", "F4 kind", "F5 finding_major", "F6 drupal_version",
		"F7 finding_signal", "F8 finding_evidence", "F9 finding_url", "F10 eol_date", "F11 org_text",
		"S1 sender_name", "S2 sender_role", "S3 identity_line", "S4 proof_line", "S5 sender_site", "S6 works_with",
		"O1 report", "O2 report_contains", "O3 report_contains", "O4 call_to_action",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("facts:\n got %q\nwant %q", got, want)
	}
	if f, _ := s.Fact("F5"); f.Value != "Drupal 7" || f.Group != GroupLead || f.Source.Table != "site_probe" {
		t.Fatalf("F5 = %+v", f)
	}
	if f, _ := s.Fact("F10"); f.Value != "5 January 2025" {
		t.Fatalf("F10 = %+v", f)
	}
	if f, _ := s.Fact("F11"); !f.EchoGuard {
		t.Fatal("org text is not echo_guard")
	}
	if f, _ := s.Fact("O2"); f.Value != "what an upgrade involves" {
		t.Fatalf("report contents not sorted: %+v", f)
	}
	if _, ok := s.Fact("F99"); ok {
		t.Fatal("F99 found")
	}

	empty := BuildFactSheet(SheetInput{Org: OrgFacts{Name: "  "}, Sender: SenderFacts{Name: "A"}, Offer: OfferFacts{Contents: []string{"", "x", "x"}}})
	if len(empty.Facts) != 2 || empty.Facts[0].ID != "S1" || empty.Facts[1].ID != "O1" {
		t.Fatalf("empty values or duplicates kept: %+v", empty.Facts)
	}
}

// TestFactSheetHash: the same facts in any input order give the same bytes and hash; a
// changed value changes it; provenance row ids do not.
func TestFactSheetHash(t *testing.T) {
	in := SheetInput{
		Org:      OrgFacts{Name: "Acme", Domain: "acme.example"},
		OrgTexts: []TextFact{{Value: "b text"}, {Value: "a text"}},
		Sender:   SenderFacts{Name: "Sam", Extra: []TextFact{{Label: "y", Value: "2"}, {Label: "x", Value: "1"}}},
		Offer:    OfferFacts{Report: "report", Contents: []string{"two", "one"}},
	}
	re := in
	re.OrgTexts = []TextFact{in.OrgTexts[1], in.OrgTexts[0]}
	re.Sender.Extra = []TextFact{in.Sender.Extra[1], in.Sender.Extra[0]}
	re.Offer.Contents = []string{"one", "two"}
	re.Org.Source = Provenance{Table: "org", ID: 99}

	a, b := BuildFactSheet(in), BuildFactSheet(re)
	if string(a.Canonical()) != string(b.Canonical()) || a.SHA256() != b.SHA256() {
		t.Fatalf("reordered input changed the sheet:\n%s\n%s", a.Canonical(), b.Canonical())
	}
	if len(a.SHA256()) != 64 {
		t.Fatalf("hash %q", a.SHA256())
	}
	ch := in
	ch.Offer.Contents = []string{"two", "one!"}
	if BuildFactSheet(ch).SHA256() == a.SHA256() {
		t.Fatal("a changed value kept the hash")
	}
	if BuildFactSheet(in).SHA256() != a.SHA256() {
		t.Fatal("the hash is not stable")
	}
}

// TestFactSheetDatesInTheLeadsLanguage: a date fact carries the human form in the lead's
// language, so drafts never write "2026-12-09"; G3 still compares dates as dates, so every
// form of the same date (and its month or year) passes against it, and another date does not.
func TestFactSheetDatesInTheLeadsLanguage(t *testing.T) {
	eol := time.Date(2026, 12, 9, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct{ lang, want string }{
		{"", "9 December 2026"}, {LangEnglish, "9 December 2026"}, {LangRomanian, "9 decembrie 2026"}, {"xx", "2026-12-09"},
	} {
		s := BuildFactSheet(SheetInput{Lang: tc.lang, EOL: eol, EOLSource: ConfigSource})
		fs := s.WithLabel(LabelEOLDate)
		if len(fs) != 1 || fs[0].Value != tc.want {
			t.Fatalf("lang %q: eol fact %+v, want %q", tc.lang, fs, tc.want)
		}
	}
	for _, src := range []string{"9 December 2026", "9 decembrie 2026"} {
		for _, text := range []string{"9 December 2026", "December 2026", "9 Dec 2026", "December 9, 2026", "2026-12-09", "in 2026", "9 decembrie 2026", "decembrie 2026"} {
			if got := uncoveredClaims("Support ends "+text+".", []string{src}); len(got) != 0 {
				t.Errorf("fact %q: %q refused: %v", src, text, got)
			}
		}
		for _, text := range []string{"10 December 2026", "November 2026", "2027", "9 ianuarie 2026"} {
			if got := uncoveredClaims("Support ends "+text+".", []string{src}); len(got) == 0 {
				t.Errorf("fact %q: %q passed", src, text)
			}
		}
	}
	if SheetDate(eol, LangRomanian) != "9 decembrie 2026" {
		t.Fatal("SheetDate ro")
	}
}
