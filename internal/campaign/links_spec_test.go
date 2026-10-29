package campaign

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gremy/webbership-leadhound/internal/leadconfig"
)

func TestFooterAndLinksAccepted(t *testing.T) {
	s := cafeSpec(t)
	for code, c := range s.Countries {
		if c.Email {
			c.Footer = "footer-cafe-en"
			s.Countries[code] = c
		}
	}
	s.Offer.Links = []string{"https://demo.example.test", "https://example.test/vs"}
	s.Offer.VendorLinks = map[string]string{
		"stampme":         "https://example.test/vs/stamp-me",
		VendorLinkDefault: "https://example.test/vs",
	}
	if err := s.Check(CheckOptions{KnownKinds: KnownKinds()}); err != nil {
		t.Fatalf("Check: %v", err)
	}
}

func TestFooterAndLinksRefused(t *testing.T) {
	cases := []struct {
		name  string
		drup  bool // edit the Drupal test spec (no cafe@1) instead of the café example
		edit  func(*Spec)
		wants []string
	}{
		{"footer not a plain name", false, func(s *Spec) {
			s.Countries["GB"] = Country{Email: true, Lang: "en", Footer: "../footer.txt"}
		}, []string{`countries.GB.footer: "../footer.txt" is not a plain file name`}},
		{"footer upper case", false, func(s *Spec) {
			s.Countries["GB"] = Country{Email: true, Lang: "en", Footer: "Footer"}
		}, []string{"is not a plain file name"}},
		{"footer without email", false, func(s *Spec) {
			s.Countries["GB"] = Country{Call: true, Lang: "en", Footer: "footer-en"}
		}, []string{"countries.GB.footer is only for a country with email on"}},
		{"link http", false, func(s *Spec) { s.Offer.Links = []string{"http://example.test"} }, []string{"not an absolute https:// URL"}},
		{"link relative", false, func(s *Spec) { s.Offer.Links = []string{"/vs/stamp-me"} }, []string{"not an absolute https:// URL"}},
		{"link without host", false, func(s *Spec) { s.Offer.Links = []string{"https:///vs"} }, []string{"not an absolute https:// URL"}},
		{"link with space", false, func(s *Spec) { s.Offer.Links = []string{"https://example.test/a b"} }, []string{"not an absolute https:// URL"}},
		{"link twice after normalising", false, func(s *Spec) {
			s.Offer.Links = []string{"https://Example.test/vs", "https://example.test/VS/"}
		}, []string{"listed twice"}},
		{"too many links", false, func(s *Spec) {
			for i := range MaxOfferLinks + 1 {
				s.Offer.Links = append(s.Offer.Links, "https://example.test/"+strings.Repeat("a", i+1))
			}
		}, []string{"at most 10"}},
		{"vendor unknown", false, func(s *Spec) {
			s.Offer.VendorLinks = map[string]string{"no-such-vendor-xyz": "https://example.test/vs"}
		}, []string{`offer.vendor_links: "no-such-vendor-xyz" is not a known loyalty vendor id`}},
		{"vendor link http", false, func(s *Spec) {
			s.Offer.VendorLinks = map[string]string{"stampme": "http://example.test/vs"}
		}, []string{"offer.vendor_links.stampme", "not an absolute https:// URL"}},
		{"vendor links without cafe@1", true, func(s *Spec) {
			s.Offer.VendorLinks = map[string]string{VendorLinkDefault: "https://example.test/vs"}
		}, []string{"offer.vendor_links is only for a campaign with the cafe@1 detector"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var s Spec
			if tc.drup {
				s = validSpec(t)
			} else {
				s = cafeSpec(t)
			}
			tc.edit(&s)
			err := s.Check(CheckOptions{KnownKinds: KnownKinds()})
			if err == nil {
				t.Fatal("Check passed; want a problem")
			}
			if !errors.Is(err, ErrInvalid) {
				t.Errorf("error does not wrap ErrInvalid: %v", err)
			}
			for _, w := range tc.wants {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q lacks %q", err, w)
				}
			}
		})
	}
}

func TestNewFieldsDecodeStrictly(t *testing.T) {
	base := validYAML(t)
	if _, err := Decode([]byte(base + "\nnot_a_field: 1\n")); err == nil {
		t.Error("an unknown key was accepted")
	}
	s, err := Decode([]byte(strings.Replace(base, "magnet: upgrade_risk_report}", "magnet: upgrade_risk_report, links: [\"https://example.test\"]}", 1)))
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Offer.Links) != 1 {
		t.Errorf("offer.links not decoded: %+v", s.Offer)
	}
}

// The new fields are omitempty, so the Drupal example's registered hash stays as it was
// before they were added (computed from the unchanged code on 29 Oct 2026).
func TestDrupalExampleHashUnchanged(t *testing.T) {
	s, err := Load(filepath.Join(examplesDir, "campaigns", "drupal-eol", FileName))
	if err != nil {
		t.Fatal(err)
	}
	j, h, err := Canonical(s)
	if err != nil {
		t.Fatal(err)
	}
	const want = "02b27c480a5a4e732d4ada528aa9868c0ff0ea833b5a8a6ab33dd60b80b846ba"
	if h != want {
		t.Errorf("drupal-eol hash = %s; want %s\n%s", h, want, j)
	}
	for _, key := range []string{`"footer"`, `"links"`, `"vendor_links"`} {
		if strings.Contains(string(j), key) {
			t.Errorf("canonical JSON carries %s though the spec leaves it out", key)
		}
	}
}

// footerNameRe mirrors leadconfig.FileNameRe, the rule leads.yaml's footer names follow, so a
// campaign footer name passes here exactly when leadconfig would load it.
func TestFooterNameRule(t *testing.T) {
	if got, want := footerNameRe.String(), leadconfig.FileNameRe.String(); got != want {
		t.Errorf("footerNameRe = %s; keep it equal to leadconfig.FileNameRe (%s)", got, want)
	}
}
