package outreach

import "testing"

func TestAgencyEvidenceLine(t *testing.T) {
	ok := map[string]string{
		"https://www.contoso.example/services/drupal-upgrades": "your page at contoso.example offers Drupal upgrades",
		"http://Contoso.Example/":                              "your page at contoso.example offers Drupal upgrades",
		"https://dev.contoso.example/en/migrations?x=1#top":    "your page at dev.contoso.example offers Drupal upgrades",
	}
	for in, want := range ok {
		got, err := AgencyEvidenceLine(in, "en")
		if err != nil || got != want {
			t.Errorf("AgencyEvidenceLine(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "contoso.example/services", "ftp://contoso.example/", "https:///path", "https://localhost/"} {
		if got, err := AgencyEvidenceLine(in, "en"); err == nil {
			t.Errorf("AgencyEvidenceLine(%q) = %q, want an error", in, got)
		}
	}
	const u = "https://www.contoso.example/services"
	if got, err := AgencyEvidenceLine(u, "ro"); err != nil || got != "pagina de pe contoso.example oferă actualizări Drupal" {
		t.Errorf("ro: %q, %v", got, err)
	}
	if got, err := AgencyEvidenceLine(u, "de"); err == nil {
		t.Errorf("de: %q, want an error for an unknown language", got)
	}
}
