package compliance

import "testing"

func TestEntityType(t *testing.T) {
	cases := []struct {
		name    string
		country string
		pages   []string
		want    string
	}{
		{"GB Ltd", "GB", []string{"© 2026 Harbour Lane Studio Ltd. All rights reserved."}, EntityCompany},
		{"GB Limited lower", "GB", []string{"harbour lane studio limited"}, EntityCompany},
		{"GB LLP", "GB", []string{"Smith & Jones LLP"}, EntityCompany},
		{"GB company number", "GB", []string{"Registered in England and Wales No. 01234567"}, EntityCompany},
		{"GB company number SC", "GB", []string{"Company number: SC123456"}, EntityCompany},
		{"GB sole trader", "GB", []string{"I'm a sole trader based in Leeds."}, EntitySoleTrader},
		{"GB nothing", "GB", []string{"We build websites."}, EntityUnknown},
		{"GB both is unknown", "GB", []string{"Jo Bloggs, sole trader", "clients include Acme Ltd"}, EntityUnknown},
		{"GB phone number is no company number", "GB", []string{"Call us on 01234 567890"}, EntityUnknown},
		{"DE GmbH", "DE", []string{"Muster GmbH, Berlin"}, EntityCompany},
		{"DE Einzelunternehmen", "DE", []string{"Einzelunternehmen Max Muster"}, EntitySoleTrader},
		{"RO SRL dotted", "RO", []string{"WEBBERSHIP S.R.L."}, EntityCompany},
		{"RO PFA", "RO", []string{"Ion Popescu PFA"}, EntitySoleTrader},
		{"SE AB exact case", "SE", []string{"Exempel AB"}, EntityCompany},
		{"SE ab prose is not AB", "SE", []string{"read more ab out us"}, EntityUnknown},
		{"FI Oy", "FI", []string{"Esimerkki Oy"}, EntityCompany},
		{"EE OÜ", "EE", []string{"Näide OÜ"}, EntityCompany},
		{"DK ApS", "DK", []string{"Eksempel ApS"}, EntityCompany},
		{"AU Pty Ltd", "AU", []string{"Example Pty  Ltd"}, EntityCompany},
		{"US Inc", "US", []string{"Example, Inc."}, EntityCompany},
		{"US LLC", "US", []string{"Example LLC"}, EntityCompany},
		{"FR SAS", "FR", []string{"Atelier Lune SAS"}, EntityCompany},
		{"NL BV", "NL", []string{"Voorbeeld B.V."}, EntityCompany},
		{"GmbH not a GB form", "GB", []string{"Muster GmbH"}, EntityUnknown},
		{"unlisted country uses every form", "NO", []string{"Muster GmbH"}, EntityCompany},
		{"CH AG", "CH", []string{"Beispiel AG, Zürich"}, EntityCompany},
		{"CH Sàrl", "CH", []string{"Muster Sàrl, Lausanne"}, EntityCompany},
		{"CH Sagl", "CH", []string{"Esempio Sagl, Lugano"}, EntityCompany},
		{"CH Einzelfirma", "CH", []string{"Einzelfirma Hans Muster"}, EntitySoleTrader},
		{"CH ag prose is not AG", "CH", []string{"Kontakt ag für alle"}, EntityUnknown},
		{"no pages", "GB", nil, EntityUnknown},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := EntityType(c.pages, c.country); got != c.want {
				t.Fatalf("EntityType(%q, %s) = %s, want %s", c.pages, c.country, got, c.want)
			}
		})
	}
}
