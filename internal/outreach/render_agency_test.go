package outreach

import "testing"

func TestAgencyEvidenceLine(t *testing.T) {
	ok := map[string]string{
		"https://www.contoso.example/services/drupal-upgrades": "your page at contoso.example offers Drupal upgrades",
		"http://Contoso.Example/":                              "your page at contoso.example offers Drupal upgrades",
		"https://dev.contoso.example/en/migrations?x=1#top":    "your page at dev.contoso.example offers Drupal upgrades",
	}
	for in, want := range ok {
		got, err := AgencyEvidenceLine(in)
		if err != nil || got != want {
			t.Errorf("AgencyEvidenceLine(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "contoso.example/services", "ftp://contoso.example/", "https:///path", "https://localhost/"} {
		if got, err := AgencyEvidenceLine(in); err == nil {
			t.Errorf("AgencyEvidenceLine(%q) = %q, want an error", in, got)
		}
	}
}
