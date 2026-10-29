// Package places reads Overture Maps Places rows that the operator exported to NDJSON, and
// keeps the ones a campaign targets (cafés and coffee shops by default).
//
// LeadHound takes no parquet dependency: the operator exports a country extract with DuckDB
// (README), one JSON object per line. Two line shapes are read:
//
//   - a flat row, as DuckDB's COPY … (FORMAT json) writes it: {"id": …, "names": {…},
//     "categories": {…}, "addresses": [{…}], …}; geometry, when exported, is ignored unless
//     it is a GeoJSON Point object;
//   - a GeoJSON Feature per line (GeoJSON lines): {"type": "Feature", "id": …,
//     "geometry": {"type": "Point", "coordinates": [lon, lat]}, "properties": {…the row…}}.
//
// The package is pure: it reads an io.Reader and returns values. It stores nothing and
// fetches nothing; the import command (cmd/leadhound) does both.
package places

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/url"
	"slices"
	"strings"

	"golang.org/x/net/publicsuffix"
)

// Source is the place source name stored on each row.
const Source = "overture"

// DerivedBy names the routes and phone facts read from a place's own emails and phones
// (contact_route and org_fact rows that point at the place instead of a fetch).
const DerivedBy = "overture@1"

// Default categories: Overture's primary categories for cafés.
var DefaultCategories = []string{"coffee_shop", "cafe"}

// DefaultMinConfidence is the confidence below which a place is skipped (spec min_confidence).
const DefaultMinConfidence = 0.7

// MaxLineBytes caps one NDJSON line; a longer line is counted as malformed and skipped whole.
const MaxLineBytes = 1 << 20

// MaxMalformedLines caps how many malformed line numbers Stats keeps (the count is exact).
const MaxMalformedLines = 50

// Place is one Overture place kept by Read.
type Place struct {
	SourceID      string   // Overture's GERS id
	Name          string   // names.primary, whitespace folded
	Category      string   // the category that matched (categories.primary, else basic_category / taxonomy.primary)
	Alternates    []string // categories.alternate, as given
	Confidence    float64  // 0–1
	Websites      []string // as given, blanks dropped, duplicates dropped, order kept
	Phones        []string // as given (unverified: a map listing, not checked)
	Socials       []string // as given (Overture's socials are mostly Facebook pages)
	Emails        []string // as given
	Brand         string   // brand.names.primary; empty when Overture holds no brand
	BrandWikidata string   // brand.wikidata (Q…); empty when none
	Address       Address  // the first address
	Sources       []SourceRef
	Lon, Lat      float64 // from a GeoJSON Point; HasPoint says whether they were read
	HasPoint      bool
}

// Address is the first of a place's addresses. Region is Overture's subdivision code as
// given (for example "CA" in the US, "ENG" in the UK; some releases give "US-CA"); use
// RegionCode for the bare code and RegionISO for "US-CA".
type Address struct {
	Freeform string
	Locality string // the city or town
	Postcode string
	Region   string
	Country  string // ISO 3166-1 alpha-2, upper case
}

// SourceRef is one of Overture's source records behind a place.
type SourceRef struct {
	Dataset  string
	RecordID string
}

// RegionCode is the subdivision code without a country prefix, upper case ("CA" for both
// "CA" and "US-CA"); empty when Overture gave none.
func (a Address) RegionCode() string {
	r := strings.ToUpper(strings.TrimSpace(a.Region))
	if a.Country != "" {
		r = strings.TrimPrefix(r, a.Country+"-")
	}
	return r
}

// RegionISO is the ISO 3166-2 form "CC-RR"; empty when the country or the region is unknown.
func (a Address) RegionISO() string {
	r := a.RegionCode()
	if r == "" || a.Country == "" {
		return ""
	}
	return a.Country + "-" + r
}

// Options choose which places Read keeps. The zero value keeps DefaultCategories at
// DefaultMinConfidence in every country.
type Options struct {
	Categories    []string // primary categories to keep; empty: DefaultCategories
	Countries     []string // ISO alpha-2 codes to keep; empty: every country
	MinConfidence float64  // below it a place is skipped; 0: DefaultMinConfidence; negative: keep all
}

// Stats count what Read saw. Every line lands in exactly one count: Kept, Blank, Malformed,
// NotCafe, LowConfidence, OtherCountry or NoName.
type Stats struct {
	Lines         int
	Kept          int
	Blank         int
	Malformed     int
	NotCafe       int
	LowConfidence int
	OtherCountry  int
	NoName        int
	// MalformedLines are the first MaxMalformedLines malformed line numbers (1-based). Only
	// line numbers are kept, never the line's text: a row may hold a person's phone.
	MalformedLines []int
}

// Skipped is every line not kept.
func (s Stats) Skipped() int { return s.Lines - s.Kept }

// row is the subset of an Overture place row that is read.
type row struct {
	Type       string          `json:"type"` // "Feature" for a GeoJSON line
	ID         json.RawMessage `json:"id"`
	Geometry   json.RawMessage `json:"geometry"`
	Properties json.RawMessage `json:"properties"`
	Names      *struct {
		Primary string `json:"primary"`
	} `json:"names"`
	Categories *struct {
		Primary   string   `json:"primary"`
		Alternate []string `json:"alternate"`
	} `json:"categories"`
	BasicCategory string `json:"basic_category"`
	Taxonomy      *struct {
		Primary string `json:"primary"`
	} `json:"taxonomy"`
	Confidence *float64 `json:"confidence"`
	Websites   []string `json:"websites"`
	Socials    []string `json:"socials"`
	Emails     []string `json:"emails"`
	Phones     []string `json:"phones"`
	Brand      *struct {
		Wikidata string `json:"wikidata"`
		Names    *struct {
			Primary string `json:"primary"`
		} `json:"names"`
	} `json:"brand"`
	Addresses []struct {
		Freeform string `json:"freeform"`
		Locality string `json:"locality"`
		Postcode string `json:"postcode"`
		Region   string `json:"region"`
		Country  string `json:"country"`
	} `json:"addresses"`
	Sources []struct {
		Dataset  string `json:"dataset"`
		RecordID string `json:"record_id"`
	} `json:"sources"`
}

type point struct {
	Type        string    `json:"type"`
	Coordinates []float64 `json:"coordinates"`
}

// errMalformed marks a line that is JSON but not a usable place row.
var errMalformed = errors.New("not a place row")

// Read reads NDJSON place rows from r and returns the places opts keeps, in input order, with
// the counts. A malformed line (bad JSON, a non-object, no id, a line over MaxLineBytes) is
// counted and skipped, never guessed at; only a read error from r is returned as an error.
func Read(r io.Reader, opts Options) ([]Place, Stats, error) {
	cats := opts.Categories
	if len(cats) == 0 {
		cats = DefaultCategories
	}
	minConf := opts.MinConfidence
	if minConf == 0 {
		minConf = DefaultMinConfidence
	}
	countries := make([]string, 0, len(opts.Countries))
	for _, c := range opts.Countries {
		countries = append(countries, strings.ToUpper(strings.TrimSpace(c)))
	}

	var (
		out []Place
		st  Stats
	)
	br := bufio.NewReaderSize(r, 64<<10)
	for {
		line, tooLong, err := readLine(br)
		if err != nil && !errors.Is(err, io.EOF) {
			return out, st, fmt.Errorf("places: line %d: %w", st.Lines+1, err)
		}
		if line == nil && !tooLong && errors.Is(err, io.EOF) {
			return out, st, nil
		}
		st.Lines++
		switch {
		case tooLong:
			st.malformed()
		case len(bytes.TrimSpace(line)) == 0:
			st.Blank++
		default:
			p, perr := parse(line)
			switch {
			case perr != nil:
				st.malformed()
			case !slices.Contains(cats, p.Category) && !anyIn(cats, p.fallbackCats()):
				st.NotCafe++
			case p.Name == "":
				st.NoName++
			case minConf >= 0 && p.Confidence < minConf:
				st.LowConfidence++
			case len(countries) > 0 && !slices.Contains(countries, p.Address.Country):
				st.OtherCountry++
			default:
				p.pickCategory(cats)
				p.fallback = nil
				st.Kept++
				out = append(out, p.Place)
			}
		}
		if errors.Is(err, io.EOF) {
			return out, st, nil
		}
	}
}

func (s *Stats) malformed() {
	s.Malformed++
	if len(s.MalformedLines) < MaxMalformedLines {
		s.MalformedLines = append(s.MalformedLines, s.Lines)
	}
}

// readLine returns the next line without its newline. A line longer than MaxLineBytes is
// drained and reported with tooLong; at the end of input err is io.EOF (with the last line,
// if it had no newline).
func readLine(br *bufio.Reader) (line []byte, tooLong bool, err error) {
	var buf []byte
	for {
		chunk, err := br.ReadSlice('\n')
		if !tooLong {
			if len(buf)+len(chunk) > MaxLineBytes+1 { // +1: the newline itself
				tooLong, buf = true, nil
			} else {
				buf = append(buf, chunk...)
			}
		}
		switch {
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case err != nil:
			if len(buf) == 0 && !tooLong {
				return nil, false, err
			}
			return bytes.TrimRight(buf, "\r\n"), tooLong, err
		default:
			return bytes.TrimRight(buf, "\r\n"), tooLong, nil
		}
	}
}

// parsed carries a Place with the categories only the filter reads.
type parsed struct {
	Place
	fallback []string // basic_category, taxonomy.primary: read when categories.primary is empty
}

func (p parsed) fallbackCats() []string {
	if p.Category != "" {
		return nil
	}
	return p.fallback
}

// pickCategory settles Category on the category that matched.
func (p *parsed) pickCategory(cats []string) {
	if slices.Contains(cats, p.Category) {
		return
	}
	for _, c := range p.fallback {
		if slices.Contains(cats, c) {
			p.Category = c
			return
		}
	}
}

func anyIn(set, vals []string) bool {
	for _, v := range vals {
		if slices.Contains(set, v) {
			return true
		}
	}
	return false
}

// parse reads one line into a place; a GeoJSON Feature's properties are the row.
func parse(line []byte) (parsed, error) {
	var top row
	if err := json.Unmarshal(line, &top); err != nil {
		return parsed{}, err
	}
	rw := top
	geom := top.Geometry
	if top.Type == "Feature" {
		if len(top.Properties) == 0 || top.Properties[0] != '{' {
			return parsed{}, errMalformed
		}
		rw = row{}
		if err := json.Unmarshal(top.Properties, &rw); err != nil {
			return parsed{}, err
		}
		if len(rw.ID) == 0 {
			rw.ID = top.ID
		}
	}
	id := rawString(rw.ID)
	if id == "" {
		return parsed{}, errMalformed
	}
	p := parsed{Place: Place{SourceID: id}}
	if rw.Names != nil {
		p.Name = fold(rw.Names.Primary)
	}
	if rw.Categories != nil {
		p.Category = strings.TrimSpace(rw.Categories.Primary)
		p.Alternates = clean(rw.Categories.Alternate)
	}
	if c := strings.TrimSpace(rw.BasicCategory); c != "" {
		p.fallback = append(p.fallback, c)
	}
	if rw.Taxonomy != nil {
		if c := strings.TrimSpace(rw.Taxonomy.Primary); c != "" {
			p.fallback = append(p.fallback, c)
		}
	}
	if rw.Confidence != nil {
		c := *rw.Confidence
		if math.IsNaN(c) || c < 0 || c > 1 {
			return parsed{}, errMalformed
		}
		p.Confidence = c
	}
	p.Websites = clean(rw.Websites)
	p.Socials = clean(rw.Socials)
	p.Emails = clean(rw.Emails)
	p.Phones = clean(rw.Phones)
	if rw.Brand != nil {
		p.BrandWikidata = strings.TrimSpace(rw.Brand.Wikidata)
		if rw.Brand.Names != nil {
			p.Brand = fold(rw.Brand.Names.Primary)
		}
	}
	if len(rw.Addresses) > 0 {
		a := rw.Addresses[0]
		p.Address = Address{
			Freeform: fold(a.Freeform),
			Locality: fold(a.Locality),
			Postcode: strings.TrimSpace(a.Postcode),
			Region:   strings.TrimSpace(a.Region),
			Country:  strings.ToUpper(strings.TrimSpace(a.Country)),
		}
	}
	for _, s := range rw.Sources {
		p.Sources = append(p.Sources, SourceRef{Dataset: strings.TrimSpace(s.Dataset), RecordID: strings.TrimSpace(s.RecordID)})
	}
	if len(geom) > 0 && geom[0] == '{' {
		var pt point
		if json.Unmarshal(geom, &pt) == nil && pt.Type == "Point" && len(pt.Coordinates) >= 2 {
			p.Lon, p.Lat, p.HasPoint = pt.Coordinates[0], pt.Coordinates[1], true
		}
	}
	return p, nil
}

// rawString reads an id given as a JSON string; any other JSON value is no id.
func rawString(raw json.RawMessage) string {
	var s string
	if len(raw) == 0 || json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return strings.TrimSpace(s)
}

// fold trims and folds runs of whitespace to one space.
func fold(s string) string { return strings.Join(strings.Fields(s), " ") }

// clean drops blanks and duplicates, keeping order.
func clean(in []string) []string {
	var out []string
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s != "" && !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}

// sharedHosts are platforms many unrelated places list as their "website": a page there is
// not the café's own site, and its domain says nothing about a chain. A place whose only
// websites are here has no website of its own (a cafe_no_website lead, its listing's own
// email and phone kept). Every subdomain counts too, on purpose: a café's page on a site
// builder (mycafe.wixsite.com, mycafe.metro.bar) is one of thousands on a domain the café
// does not own, and taking its registrable domain as the org would merge unrelated cafés.
// Groups: social and messaging; maps, search and link shorteners; site builders and free
// hosting; menu, booking, directory and delivery platforms; forms, files and payment links.
var sharedHosts = []string{
	// social and messaging
	"facebook.com", "fb.com", "fb.me", "instagram.com", "instagr.am", "tiktok.com", "x.com",
	"twitter.com", "linktr.ee", "linkin.bio", "linkedin.com", "youtube.com", "whatsapp.com",
	"wa.me", "t.me", "tumblr.com", "pinterest.com",
	// maps, search, encyclopedias and link shorteners (Google on any suffix: shared)
	"goo.gl", "g.page", "g.co", "share.google", "business.site", "maps.app.goo.gl",
	"wikipedia.org", "wikimedia.org", "bit.ly", "tinyurl.com", "t.ly", "ow.ly", "is.gd",
	"cutt.ly", "rebrand.ly", "app.link", "tripadvisor.com", "yelp.com", "foursquare.com",
	// site builders and free hosting
	"wix.com", "wixsite.com", "squarespace.com", "square.site", "wordpress.com", "blogspot.com",
	"carrd.co", "weebly.com", "site123.me", "webnode.com", "jimdosite.com", "godaddysites.com",
	"placeweb.site", "infinityfreeapp.com", "metro.bar", "metro.rest", "metro.cafe",
	// menu, booking, directory and delivery platforms
	"ubereats.com", "glovoapp.com", "wolt.com", "deliveroo.co.uk", "just-eat.co.uk", "tazz.ro",
	"bolt.eu", "doordash.com", "grubhub.com", "foodpanda.com", "takeaway.com", "beefood.ro",
	"meniudigital.ro", "meniu-digital.com", "smart-menu.ro", "menubox.ro", "qubmenu.com",
	"eplus.menu", "digital-menu.app", "ialoc.ro", "turistinfo.ro", "tourismguide.ro",
	"findglocal.com",
	// forms, files and payment links
	"forms.gle", "jotform.com", "postimage.org", "pdfhost.io", "revolut.me",
}

// brandHosts are the registrable-domain labels (the part before the public suffix, any
// suffix: lavazza.com, lavazza.ro) of coffee suppliers, machine makers and hotel groups
// whose website a place lists instead of its own. A place on one is skipped like a chain
// (cafe.ChainIndex): its listing is the brand's (a hotel's bar, a supplier's corner) or its
// address book would point at the brand, so it is not an independent café to write to.
var brandHosts = []string{
	"lavazza", "illy", "segafredo", "dallmayr", "hausbrandt", "nespresso", "tchibo", "kimbo",
	"delonghi", "wega", "strauss-group", "hilton", "doubletree", "intercontinental", "ihg",
	"marriott", "accor",
}

// socialHosts are the hosts that make a link Instagram, or another social profile.
var socialHosts = []string{
	"facebook.com", "fb.com", "fb.me", "instagram.com", "instagr.am", "tiktok.com", "x.com",
	"twitter.com", "linktr.ee", "linkin.bio", "linkedin.com", "youtube.com",
}

// Host is a link's lower-case host without a leading "www."; empty when it does not parse.
// A link without a scheme ("cafe.example/menu") is read as http.
func Host(link string) string {
	link = strings.TrimSpace(link)
	if link == "" {
		return ""
	}
	if !strings.Contains(link, "://") {
		link = "http://" + link
	}
	u, err := url.Parse(link)
	if err != nil {
		return ""
	}
	return strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
}

// HostIs reports whether host is domain or a subdomain of it.
func HostIs(host, domain string) bool {
	return host == domain || strings.HasSuffix(host, "."+domain)
}

func hostIn(host string, list []string) bool {
	for _, d := range list {
		if HostIs(host, d) {
			return true
		}
	}
	return false
}

// shared reports whether host is on a shared platform: sharedHosts, or Google on any of its
// country domains (google.ro, maps.google.de, sites.google.com).
func shared(host string) bool { return hostIn(host, sharedHosts) || Label(host) == "google" }

// Label is the registrable domain of host without its public suffix ("lavazza" for
// shop.lavazza.co.uk); empty when host has no registrable domain.
func Label(host string) string {
	d, err := publicsuffix.EffectiveTLDPlusOne(host)
	if err != nil {
		return ""
	}
	suffix, _ := publicsuffix.PublicSuffix(d)
	return strings.TrimSuffix(d, "."+suffix)
}

// IsShared reports whether a link sits on a shared platform (social, a map listing or link
// shortener, a site builder's subdomain, a menu or delivery app) rather than on the café's
// own domain. A bare domain ("meniudigital.ro", an email's domain) is read as a link.
func IsShared(link string) bool { return shared(Host(link)) }

// IsBrandSite reports whether a link is a coffee supplier's, machine maker's or hotel
// group's own website (brandHosts), on any public suffix.
func IsBrandSite(link string) bool { return slices.Contains(brandHosts, Label(Host(link))) }

// IsSocial reports whether a link is a social profile.
func IsSocial(link string) bool { return hostIn(Host(link), socialHosts) }

// OwnWebsite is the place's first website on its own domain; empty when every website it
// lists is on a shared platform (a café whose "website" is its Facebook page has none).
func (p Place) OwnWebsite() string {
	for _, w := range p.Websites {
		if Host(w) != "" && !IsShared(w) {
			return w
		}
	}
	return ""
}

// DomainKey is the registrable domain of a link (eTLD+1, "www." and subdomains dropped),
// used to tell that two places share one website; empty for a shared platform or a link
// that does not parse.
func DomainKey(link string) string {
	h := Host(link)
	if h == "" || shared(h) {
		return ""
	}
	d, err := publicsuffix.EffectiveTLDPlusOne(h)
	if err != nil {
		return h
	}
	return d
}
