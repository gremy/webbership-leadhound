package sitelist

import "testing"

// fixtureLabel is an obviously fictional second-level label, joined to real public
// suffixes at runtime so no whole real-looking domain is written in source.
const fixtureLabel = "leadhound-fixture-" + "zq"

func TestRegistrable(t *testing.T) {
	tests := []struct {
		name, host, want string
		wantErr          bool
	}{
		{"reserved", "northwind.example", "northwind.example", false},
		{"www host", "www.northwind.example", "northwind.example", false},
		{"deep subdomain", "a.b.contoso.example", "contoso.example", false},
		{"upper case and trailing dot", "WWW.Northwind.Example.", "northwind.example", false},
		{"surrounding space", "  northwind.example ", "northwind.example", false},
		{"co.uk", "www." + fixtureLabel + ".co" + ".uk", fixtureLabel + ".co" + ".uk", false},
		{"org.uk", "shop." + fixtureLabel + ".org" + ".uk", fixtureLabel + ".org" + ".uk", false},
		{"ac.uk", "lib." + fixtureLabel + ".ac" + ".uk", fixtureLabel + ".ac" + ".uk", false},
		{"idn", "www.bücher.example", "xn--bcher-kva.example", false},
		{"empty", "", "", true},
		{"bare suffix", "co" + ".uk", "", true},
		{"single label", "localhost", "", true},
		{"ip address", "192.0.2.10", "", true},
		{"has path", "northwind.example/about", "", true},
		{"has port", "northwind.example:443", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Registrable(tt.host)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Registrable(%q) err = %v, wantErr %v", tt.host, err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("Registrable(%q) = %q, want %q", tt.host, got, tt.want)
			}
		})
	}
}

func TestCountryOf(t *testing.T) {
	tests := []struct{ domain, want string }{
		{fixtureLabel + ".co" + ".uk", "GB"},
		{fixtureLabel + ".ie", "IE"},
		{fixtureLabel + ".nl", "NL"},
		{fixtureLabel + ".ro", "RO"},
		{fixtureLabel + ".de", "DE"},
		{fixtureLabel + ".ch", "CH"},
		{"NORTHWIND.EXAMPLE", ""},
		{fixtureLabel + ".com", ""},
		{"", ""},
	}
	for _, tt := range tests {
		if got := CountryOf(tt.domain); got != tt.want {
			t.Errorf("CountryOf(%q) = %q, want %q", tt.domain, got, tt.want)
		}
	}
}

func TestKeepTLDs(t *testing.T) {
	keep := KeepTLDs([]string{"uk", ".IE", " example "})
	tests := []struct {
		domain string
		want   bool
	}{
		{fixtureLabel + ".co" + ".uk", true},
		{fixtureLabel + ".ie", true},
		{"northwind.example", true},
		{fixtureLabel + ".nl", false},
		{"contoso.invalid", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := keep(tt.domain); got != tt.want {
			t.Errorf("keep(%q) = %v, want %v", tt.domain, got, tt.want)
		}
	}
	if KeepTLDs(nil)("northwind.example") {
		t.Error("an empty list keeps nothing")
	}
}
