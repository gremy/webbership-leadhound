// Package dial turns a stored phone number into the number a tel: link dials from anywhere.
//
// The store keeps a number as enrich.NormalizePhone left it: "+" and digits when the page
// printed it internationally, national digits only when it did not ("01437 720202" is kept as
// "01437720202"). Dialled as stored, a national number reaches the wrong line from abroad (the
// operator calls from Romania), so the dial number is rebuilt here from the lead's country:
// its calling code, with the national trunk prefix dropped where the country has one. When the
// country or its code is not known, or the digits do not fit the country's rule, there is no
// dial number, so the card shows the number as text with no link rather than a wrong one.
package dial

import "strings"

// plan is how a country's national numbers become international ones: its calling code, and
// the trunk prefixes a national number starts with and that are dropped after the code. A
// country with no trunk prefix (Italy, Spain, ...) keeps every digit after its code; Italy's
// landlines start with 0 and keep it ("+39 06 ...").
type plan struct {
	code  string
	trunk []string
}

// plans are the countries a campaign may hold (ISO 3166-1 alpha-2), at least every country
// of the campaign country table.
var plans = map[string]plan{
	"US": {"1", nil}, // NANP: see nanp
	"CA": {"1", nil},
	"GB": {"44", []string{"0"}},
	"IE": {"353", []string{"0"}},
	"FR": {"33", []string{"0"}},
	"BE": {"32", []string{"0"}},
	"NL": {"31", []string{"0"}},
	"LU": {"352", nil},
	"DE": {"49", []string{"0"}},
	"AT": {"43", []string{"0"}},
	"CH": {"41", []string{"0"}},
	"IT": {"39", nil},
	"ES": {"34", nil},
	"PT": {"351", nil},
	"MT": {"356", nil},
	"GR": {"30", nil},
	"CY": {"357", nil},
	"DK": {"45", nil},
	"SE": {"46", []string{"0"}},
	"FI": {"358", []string{"0"}},
	"EE": {"372", nil},
	"LV": {"371", nil},
	"LT": {"370", []string{"8", "0"}},
	"PL": {"48", nil},
	"CZ": {"420", nil},
	"SK": {"421", []string{"0"}},
	"HU": {"36", []string{"06"}},
	"SI": {"386", []string{"0"}},
	"HR": {"385", []string{"0"}},
	"RO": {"40", []string{"0"}},
	"BG": {"359", []string{"0"}},
	"AU": {"61", []string{"0"}},
	"NZ": {"64", []string{"0"}},
}

// CallingCode is country's calling code without the "+" ("44" for GB); "" when unknown.
func CallingCode(country string) string {
	return plans[strings.ToUpper(strings.TrimSpace(country))].code
}

// Number is the international number ("+" and digits) a tel: link should dial for stored,
// the normalised number as kept, of a lead in country (ISO alpha-2). A stored number that
// already starts with "+" is returned as is. A national one gets the country's code, its
// trunk prefix dropped (one only: "0", or "06" for Hungary); Italy keeps its leading 0. It
// returns "" when the number cannot be dialled safely: no digits, a character that is not a
// digit, an unknown country, a national number that does not start with the country's
// trunk prefix where it has one (or does with a second 0, as "00..."), or an international
// number longer than E.164's fifteen digits.
func Number(stored, country string) string {
	if strings.HasPrefix(stored, "+") {
		if digitsOnly(stored[1:]) {
			return stored
		}
		return ""
	}
	if !digitsOnly(stored) {
		return ""
	}
	p, ok := plans[strings.ToUpper(strings.TrimSpace(country))]
	if !ok {
		return ""
	}
	var national string
	switch {
	case p.code == "1":
		national = nanp(stored)
	case len(p.trunk) == 0:
		// No trunk prefix: every digit is part of the number; one starting with 0 that is not
		// Italy's is an international prefix or a typo, never dialled.
		if strings.HasPrefix(stored, "0") && p.code != "39" {
			return ""
		}
		national = stored
	default:
		for _, t := range p.trunk {
			if rest, ok := strings.CutPrefix(stored, t); ok && !strings.HasPrefix(rest, "0") {
				national = rest
				break
			}
		}
	}
	if national == "" || len(p.code)+len(national) > 15 {
		return ""
	}
	return "+" + p.code + national
}

// nanp is a US or Canadian number's ten digits: ten as printed, or eleven with the leading 1;
// "" for anything else.
func nanp(digits string) string {
	switch {
	case len(digits) == 10 && digits[0] != '0' && digits[0] != '1':
		return digits
	case len(digits) == 11 && digits[0] == '1':
		return nanp(digits[1:])
	}
	return ""
}

// digitsOnly reports whether s is one or more ASCII digits.
func digitsOnly(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
