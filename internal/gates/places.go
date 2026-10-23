package gates

import (
	"regexp"
	"slices"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// place is a country, a city resolved to its country, or a region of countries.
type place struct {
	name    string   // as said in a reason, e.g. "Bucharest (Romania)"
	country string   // set for a country or a city
	region  []string // set for a region
}

// eu lists the European Union's member states.
var eu = []string{
	"Austria", "Belgium", "Bulgaria", "Croatia", "Cyprus", "Czechia", "Denmark", "Estonia",
	"Finland", "France", "Germany", "Greece", "Hungary", "Ireland", "Italy", "Latvia",
	"Lithuania", "Luxembourg", "Malta", "Netherlands", "Poland", "Portugal", "Romania",
	"Slovakia", "Slovenia", "Spain", "Sweden",
}

var (
	eea    = append(slices.Clone(eu), "Iceland", "Liechtenstein", "Norway")
	europe = append(slices.Clone(eea), "Switzerland", "United Kingdom", "Serbia", "Ukraine",
		"Moldova", "Albania", "Bosnia and Herzegovina", "Montenegro", "North Macedonia")
)

// countryAliases maps a lower-case name to its canonical country.
var countryAliases = map[string]string{
	"united kingdom": "United Kingdom", "great britain": "United Kingdom", "britain": "United Kingdom", "england": "United Kingdom", "scotland": "United Kingdom",
	"united states": "United States", "united states of america": "United States",
	"the netherlands": "Netherlands", "holland": "Netherlands",
	"czech republic": "Czechia", "canada": "Canada", "india": "India", "israel": "Israel",
	"turkey": "Turkey", "türkiye": "Turkey", "românia": "Romania",
}

// upperAliases are matched case-sensitively, so "us" or "eu" in prose are not places.
var upperAliases = map[string]string{"US": "United States", "USA": "United States", "U.S.": "United States", "UK": "United Kingdom"}

// cityCountry maps a lower-case city to its country.
var cityCountry = map[string]string{
	"bucharest": "Romania", "bucurești": "Romania", "bucuresti": "Romania", "cluj": "Romania", "cluj-napoca": "Romania",
	"iași": "Romania", "iasi": "Romania", "brașov": "Romania", "brasov": "Romania",
	"vilnius": "Lithuania", "kaunas": "Lithuania", "riga": "Latvia", "tallinn": "Estonia",
	"berlin": "Germany", "munich": "Germany", "hamburg": "Germany", "frankfurt": "Germany", "cologne": "Germany",
	"paris": "France", "lyon": "France", "madrid": "Spain", "barcelona": "Spain", "valencia": "Spain", "seville": "Spain",
	"amsterdam": "Netherlands", "rotterdam": "Netherlands", "utrecht": "Netherlands", "eindhoven": "Netherlands", "the hague": "Netherlands",
	"london": "United Kingdom", "manchester": "United Kingdom", "edinburgh": "United Kingdom", "dublin": "Ireland",
	"lisbon": "Portugal", "porto": "Portugal", "warsaw": "Poland", "krakow": "Poland", "kraków": "Poland", "wroclaw": "Poland", "wrocław": "Poland",
	"milan": "Italy", "rome": "Italy", "turin": "Italy", "brussels": "Belgium", "antwerp": "Belgium", "vienna": "Austria",
	"prague": "Czechia", "budapest": "Hungary", "sofia": "Bulgaria", "athens": "Greece", "zagreb": "Croatia", "ljubljana": "Slovenia",
	"bratislava": "Slovakia", "stockholm": "Sweden", "copenhagen": "Denmark", "helsinki": "Finland", "oslo": "Norway",
	"zurich": "Switzerland", "zürich": "Switzerland", "geneva": "Switzerland", "luxembourg city": "Luxembourg",
	"new york": "United States", "san francisco": "United States", "toronto": "Canada", "belgrade": "Serbia", "chisinau": "Moldova",
}

// regionAliases maps a lower-case region to its countries.
var regionAliases = map[string][]string{
	"european union": eu, "europe": europe, "european economic area": eea,
	"emea": europe, "anywhere": nil,
}

// upperRegions are matched case-sensitively.
var upperRegions = map[string][]string{"EU": eu, "EEA": eea, "EMEA": europe, "CET": europe}

var placeRE, upperPlaceRE = buildPlaceREs()

func buildPlaceREs() (*regexp.Regexp, *regexp.Regexp) {
	var names []string
	for _, c := range append(slices.Clone(europe), "United States", "Canada", "Brazil", "Argentina", "Mexico", "Australia", "Singapore") {
		names = append(names, strings.ToLower(c))
	}
	for k := range countryAliases {
		names = append(names, k)
	}
	for k := range cityCountry {
		names = append(names, k)
	}
	for k := range regionAliases {
		names = append(names, k)
	}
	var upper []string
	for k := range upperAliases {
		upper = append(upper, k)
	}
	for k := range upperRegions {
		upper = append(upper, k)
	}
	return alternation(names, "(?i)"), alternation(upper, "")
}

// alternation builds `\b(?:a|b|…)\b` with the longest names first, so "the netherlands"
// wins over "netherlands" and "cluj-napoca" over "cluj".
func alternation(names []string, flags string) *regexp.Regexp {
	slices.Sort(names)
	names = slices.Compact(names)
	sort.SliceStable(names, func(i, j int) bool { return len(names[i]) > len(names[j]) })
	quoted := make([]string, len(names))
	for i, n := range names {
		quoted[i] = regexp.QuoteMeta(n)
	}
	// RE2 has no lookaround; `(?:^|[^\pL\pN])` and `(?:[^\pL\pN]|$)` bound the name in
	// Unicode terms (\b is ASCII-only and would split "Iași").
	return regexp.MustCompile(flags + `(?:^|[^\pL\pN])(` + strings.Join(quoted, "|") + `)(?:[^\pL\pN]|$)`)
}

// lookupPlace resolves a matched name.
func lookupPlace(name string) place {
	if c, ok := upperAliases[name]; ok {
		return place{name: name + " (" + c + ")", country: c}
	}
	if r, ok := upperRegions[name]; ok {
		return place{name: name, region: r}
	}
	l := strings.ToLower(name)
	if c, ok := countryAliases[l]; ok {
		return place{name: c, country: c}
	}
	if c, ok := cityCountry[l]; ok {
		return place{name: name + " (" + c + ")", country: c}
	}
	if r, ok := regionAliases[l]; ok {
		if r == nil {
			return place{name: name, region: []string{"*"}}
		}
		return place{name: name, region: r}
	}
	for _, c := range europe {
		if strings.EqualFold(c, l) {
			return place{name: c, country: c}
		}
	}
	return place{name: name, country: titleCaseWords(name)}
}

func titleCaseWords(s string) string {
	ws := strings.Fields(s)
	for i, w := range ws {
		ws[i] = titleCase(w)
	}
	return strings.Join(ws, " ")
}

// findPlaces returns every place named in s.
func findPlaces(s string) []place {
	var out []place
	for _, re := range []*regexp.Regexp{upperPlaceRE, placeRE} {
		// Each match consumes the separator after the name; the next search starts at
		// that separator, so "Spain/Romania" and "Spain,Romania" give both places.
		for pos := 0; pos < len(s); {
			m := re.FindStringSubmatchIndex(s[pos:])
			if m == nil {
				break
			}
			out = append(out, lookupPlace(s[pos+m[2]:pos+m[3]]))
			pos += m[3]
		}
	}
	return out
}

// allowed reports whether p is, or contains, one of the allowed countries.
func allowed(p place, allowedCountries []string) bool {
	for _, a := range allowedCountries {
		if strings.EqualFold(p.country, strings.TrimSpace(a)) {
			return true
		}
		for _, r := range p.region {
			if r == "*" || strings.EqualFold(r, strings.TrimSpace(a)) {
				return true
			}
		}
	}
	return false
}

// phraseAfter returns the words after text[end:], up to the end of the clause.
func phraseAfter(text string, end int) string {
	rest := text[end:]
	if i := strings.IndexAny(rest, ".;:!?()\n"); i >= 0 {
		// Keep "U.S." whole: a dot followed by a letter is not a clause end.
		for i >= 0 && rest[i] == '.' && i+1 < len(rest) && unicode.IsLetter(rune(rest[i+1])) {
			j := strings.IndexAny(rest[i+1:], ".;:!?()\n")
			if j < 0 {
				i = -1
				break
			}
			i += 1 + j
		}
		if i >= 0 {
			rest = rest[:i]
		}
	}
	if len(rest) > 80 {
		cut := 80
		for cut > 0 && !utf8.RuneStart(rest[cut]) {
			cut--
		}
		rest = rest[:cut]
	}
	return strings.TrimSpace(rest)
}

// properNoun returns the leading capitalised words of phrase ("the" skipped), e.g.
// "Fooland" from "Fooland or nearby"; "" when the phrase does not start with one.
func properNoun(phrase string) string {
	ws := strings.Fields(phrase)
	if len(ws) > 0 && strings.EqualFold(ws[0], "the") {
		ws = ws[1:]
	}
	var out []string
	for _, w := range ws {
		w = strings.TrimRight(w, ",")
		r, _ := utf8.DecodeRuneInString(w)
		if !unicode.IsUpper(r) || len(out) == 3 {
			break
		}
		out = append(out, w)
	}
	return strings.Join(out, " ")
}
