package dial

import (
	"os"
	"regexp"
	"testing"
)

// TestNumber: a national number dials with its country's code and its trunk 0 dropped; Italy
// keeps its 0; a country with no trunk prefix keeps every digit; NANP takes ten digits or one
// and ten; an international number passes through; anything unsafe gives no number.
func TestNumber(t *testing.T) {
	for _, c := range []struct{ stored, country, want string }{
		{"01437720202", "GB", "+441437720202"}, // the shadow walk's Pembrokeshire number
		{"01437720202", "gb", "+441437720202"},
		{"+441437720202", "GB", "+441437720202"},
		{"+441437720202", "", "+441437720202"},
		{"0612345678", "NL", "+31612345678"},
		{"0212345678", "RO", "+40212345678"},
		{"030123456", "DE", "+4930123456"},
		{"0612345678", "IT", "+390612345678"}, // Rome landline keeps its 0
		{"3123456789", "IT", "+393123456789"}, // Italian mobile
		{"912345678", "ES", "+34912345678"},
		{"33123456", "DK", "+4533123456"},
		{"012345678", "HU", ""}, // Hungary's trunk is 06, a lone 0 is not
		{"0612345678", "hu", "+3612345678"},
		{"06123456789", "HU", "+36123456789"},
		{"852123456", "LT", "+37052123456"},
		{"2125551234", "US", "+12125551234"},
		{"12125551234", "CA", "+12125551234"},
		{"0212555123", "US", ""},
		{"01437720202", "", ""},              // no country
		{"01437720202", "ZZ", ""},            // unknown country
		{"1437720202", "GB", ""},             // no trunk 0 where the country has one
		{"001437720202", "GB", ""},           // two zeros: an international prefix, never a trunk
		{"0912345678", "ES", ""},             // a 0 where the country has no trunk prefix
		{"", "GB", ""},                       // erased fact
		{"+", "GB", ""},                      //
		{"0143 772", "GB", ""},               // not digits
		{"+44 1437", "GB", ""},               //
		{"0" + "1234567890123456", "GB", ""}, // longer than E.164
	} {
		if got := Number(c.stored, c.country); got != c.want {
			t.Errorf("Number(%q, %q) = %q, want %q", c.stored, c.country, got, c.want)
		}
	}
}

// TestCallingCodeCoversCampaignCountries: every country the campaign country table holds, and
// Switzerland, has a calling code, so no lead of a campaign country loses its tel: link.
func TestCallingCodeCoversCampaignCountries(t *testing.T) {
	want := []string{"US", "CA", "GB", "IE", "FR", "BE", "SE", "FI", "EE", "AU", "NZ", "SK", "CY", "RO", "IT", "ES",
		"DE", "DK", "AT", "PL", "CZ", "BG", "PT", "SI", "LT", "HU", "LU", "MT", "HR", "GR", "NL", "LV", "CH"}
	if b, err := os.ReadFile("../compliance/countries.yaml"); err == nil {
		for _, m := range regexp.MustCompile(`(?m)^  ([A-Z]{2}):`).FindAllStringSubmatch(string(b), -1) {
			want = append(want, m[1])
		}
	}
	for _, c := range want {
		if CallingCode(c) == "" {
			t.Errorf("%s has no calling code", c)
		}
	}
	if got := CallingCode("IT"); got != "39" {
		t.Errorf("IT code %q", got)
	}
}
