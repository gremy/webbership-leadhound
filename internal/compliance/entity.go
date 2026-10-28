package compliance

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// gbCompanyNumber matches a UK company number stated as one: "Company No. 01234567",
// "Registered in England and Wales No: 12345678", "company number SC123456".
var gbCompanyNumber = regexp.MustCompile(`(?i)\b(company|registered|registration|crn)\b[^\n]{0,60}?\b((SC|NI|OC|SO|NC)?[0-9]{6,8})\b`)

// formPattern matches a legal-form word or phrase as a whole word. Forms of two letters or
// fewer (SA, AB, Oy, AS) match in their exact case, so "sa" or "as" in prose never count;
// longer forms match in any case.
func formPattern(form string) *regexp.Regexp {
	letters := 0
	for _, r := range form {
		if r != '.' && r != ' ' {
			letters++
		}
	}
	flags := "(?i)"
	if letters <= 2 {
		flags = ""
	}
	q := regexp.QuoteMeta(form)
	q = strings.ReplaceAll(q, `\ `, `\s+`)
	q = strings.ReplaceAll(q, " ", `\s+`)
	pre := `(?:^|[^\p{L}\p{N}])`
	post := `(?:$|[^\p{L}\p{N}])`
	// A form that ends in a dot ("Inc.") is already bounded on the right.
	if r, _ := utf8.DecodeLastRuneInString(form); r == '.' || r == ')' {
		post = ""
	}
	if r, _ := utf8.DecodeRuneInString(form); r == '(' {
		pre = ""
	}
	return regexp.MustCompile(flags + pre + q + post)
}

// EntityType reads an org's own page texts for its legal form: company when a legal-form
// word of the country (or, for a country the table doesn't list, any country) or a GB
// company number appears, sole_trader when only sole-trader words appear, unknown when
// neither or both do.
func (t *Table) EntityType(pageTexts []string, country string) string {
	cc := code(country)
	forms, ok := t.forms[cc]
	if !ok {
		forms = t.allForms
	}
	companyNumber := cc == "GB"
	if c, ok := t.Countries[cc]; ok {
		companyNumber = c.CompanyNumber
	}
	company, sole := false, false
	for _, text := range pageTexts {
		if !company {
			if companyNumber && gbCompanyNumber.MatchString(text) {
				company = true
			}
			for _, re := range forms {
				if re.MatchString(text) {
					company = true
					break
				}
			}
		}
		if !sole {
			for _, re := range t.soleTrader {
				if re.MatchString(text) {
					sole = true
					break
				}
			}
		}
	}
	switch {
	case company && !sole:
		return EntityCompany
	case sole && !company:
		return EntitySoleTrader
	default:
		return EntityUnknown
	}
}

// EntityType is Default().EntityType.
func EntityType(pageTexts []string, country string) string {
	return defaultTable.EntityType(pageTexts, country)
}
