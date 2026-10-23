package gates

import (
	"regexp"
	"slices"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Place kinds.
const (
	kindCountry = iota
	kindRegion
	kindState // a US state or Canadian province: limited to its country
	kindCity
)

// place is a country, a state, a city resolved to its country, or a region of countries.
type place struct {
	name    string   // as said in a reason, e.g. "Bucharest (Romania)"
	country string   // set for a country, a state or a city
	region  []string // set for a region
	kind    int
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
	latam = []string{"Mexico", "Brazil", "Argentina", "Chile", "Colombia", "Peru", "Uruguay",
		"Costa Rica", "Ecuador", "Bolivia", "Paraguay", "Venezuela", "Guatemala", "Panama", "Dominican Republic"}
	apac = []string{"Australia", "New Zealand", "Singapore", "Japan", "South Korea", "India",
		"Indonesia", "Philippines", "Vietnam", "Thailand", "Malaysia", "China", "Hong Kong", "Taiwan",
		"Pakistan", "Bangladesh", "Sri Lanka"}
	northAmerica = []string{"United States", "Canada", "Mexico"}
	americas     = append(slices.Clone(northAmerica), latam[1:]...)
	// otherCountries are recognised by name beyond Europe and the regions above.
	otherCountries = []string{"Israel", "Turkey", "United Arab Emirates", "Saudi Arabia", "Qatar",
		"Egypt", "Nigeria", "Kenya", "South Africa", "Morocco", "Tunisia"}
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
var upperAliases = map[string]string{"US": "United States", "USA": "United States", "U.S.": "United States", "UK": "United Kingdom", "UAE": "United Arab Emirates"}

// regionAliases maps a lower-case region to its countries; nil means anywhere.
var regionAliases = map[string][]string{
	"european union": eu, "europe": europe, "european economic area": eea,
	"emea": europe, "anywhere": nil, "worldwide": nil,
	"latam": latam, "latin america": latam, "south america": latam,
	"apac": apac, "asia pacific": apac, "asia-pacific": apac,
	"north america": northAmerica, "americas": americas, "the americas": americas,
	"central european time": europe,
}

// upperRegions are matched case-sensitively.
var upperRegions = map[string][]string{"EU": eu, "EEA": eea, "EMEA": europe, "CET": europe, "CEST": europe, "LATAM": latam, "APAC": apac}

var placeRE, upperPlaceRE = buildPlaceREs()

// codeRE reads a state or province code right after a comma and before the end of the
// place ("Menlo Park, CA", "Austin, TX (Remote)", "Boston, MA; London"), so "CA" in a
// sentence ("experience with CA, PKI and HSMs" or ", CA licence") is not read.
var codeRE = func() *regexp.Regexp {
	codes := make([]string, 0, len(subdivisionCodes))
	for k := range subdivisionCodes {
		codes = append(codes, k)
	}
	slices.Sort(codes)
	return regexp.MustCompile(`,\s*(` + strings.Join(codes, "|") + `)(?:\s*[,;/|()]|\s+-|\.?\s*$)`)
}()

func buildPlaceREs() (*regexp.Regexp, *regexp.Regexp) {
	var names []string
	countries := slices.Concat(europe, latam, apac, northAmerica, otherCountries)
	for _, c := range countries {
		names = append(names, strings.ToLower(c))
	}
	for _, m := range []map[string]string{countryAliases, cityCountry, subdivisions} {
		for k := range m {
			names = append(names, k)
		}
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
		return place{name: name, region: r, kind: kindRegion}
	}
	l := strings.ToLower(name)
	if c, ok := countryAliases[l]; ok {
		return place{name: c, country: c}
	}
	if c, ok := cityCountry[l]; ok {
		return place{name: name + " (" + c + ")", country: c, kind: kindCity}
	}
	if c, ok := subdivisions[l]; ok {
		return place{name: name + " (" + c + ")", country: c, kind: kindState}
	}
	if r, ok := regionAliases[l]; ok {
		if r == nil {
			return place{name: name, region: []string{"*"}, kind: kindRegion}
		}
		return place{name: name, region: r, kind: kindRegion}
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

// findPlaces returns every place named in s, in the order s names them.
func findPlaces(s string) []place {
	found := findPlacesAt(s)
	out := make([]place, len(found))
	for i, f := range found {
		out[i] = f.place
	}
	return out
}

// placeAt is a place and where s names it.
type placeAt struct {
	place
	start, end int
	code       bool // a state or province code read after a comma
}

func findPlacesAt(s string) []placeAt {
	var out []placeAt
	for _, re := range []*regexp.Regexp{upperPlaceRE, placeRE} {
		// Each match consumes the separator after the name; the next search starts at
		// that separator, so "Spain/Romania" and "Spain,Romania" give both places.
		for pos := 0; pos < len(s); {
			m := re.FindStringSubmatchIndex(s[pos:])
			if m == nil {
				break
			}
			out = append(out, placeAt{place: lookupPlace(s[pos+m[2] : pos+m[3]]), start: pos + m[2], end: pos + m[3]})
			pos += m[3]
		}
	}
	var codes []placeAt
	for _, m := range codeRE.FindAllStringSubmatchIndex(s, -1) {
		code := s[m[2]:m[3]]
		c := subdivisionCodes[code]
		codes = append(codes, placeAt{place: place{name: code + " (" + c + ")", country: c, kind: kindState}, start: m[2], end: m[3], code: true})
	}
	out = append(out, codes...)
	// In order; of two names at one spot ("EMEA" read both ways) the longer stays.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].start != out[j].start {
			return out[i].start < out[j].start
		}
		return out[i].end > out[j].end
	})
	kept := out[:0]
	for _, p := range out {
		if len(kept) > 0 && p.start < kept[len(kept)-1].end {
			continue
		}
		kept = append(kept, p)
	}
	return joinCodes(s, kept)
}

// joinCodes reads a state or province code with the city before it: "Austin, TX" is one
// US city, "Cambridge, MA" is not the UK's Cambridge, and "Toronto, CA" or "Tel Aviv, IL"
// keep their own country, whose code it is. A code after an unknown name ("Menlo Park, CA")
// is still a city, named as written; a code alone is the state.
func joinCodes(s string, ps []placeAt) []placeAt {
	out := make([]placeAt, 0, len(ps))
	for _, p := range ps {
		if !p.code {
			out = append(out, p)
			continue
		}
		code := s[p.start:p.end]
		comma := strings.LastIndex(s[:p.start], ",")
		if n := len(out); n > 0 && out[n-1].end <= comma && strings.TrimSpace(s[out[n-1].end:comma]) == "" {
			prev := out[n-1]
			if prev.country != "" && countryCodes[prev.country] == code {
				continue
			}
			out[n-1] = placeAt{place: place{name: s[prev.start:p.end] + " (" + p.country + ")", country: p.country, kind: kindCity}, start: prev.start, end: p.end}
			continue
		}
		before := s[:comma]
		if i := strings.LastIndexAny(before, ",;/|()-\n"); i >= 0 {
			before = before[i+1:]
		}
		if name := strings.TrimSpace(before); name != "" && !workplaceOnlyRE.MatchString(name) {
			p.place = place{name: name + ", " + code + " (" + p.country + ")", country: p.country, kind: kindCity}
		}
		out = append(out, p)
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
