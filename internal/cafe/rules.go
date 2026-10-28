package cafe

import (
	"fmt"
	"slices"
	"strings"
	"unicode"

	"golang.org/x/text/runes"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"

	"github.com/gremy/webbership-leadhound/internal/places"
)

// Kinds of a café lead.
const (
	KindNoDigitalLoyalty = "cafe_no_digital_loyalty" // the target: a site, and no digital loyalty on it
	KindHasLoyalty       = "cafe_has_loyalty"        // a competitor's customer; off by default
	KindNoWebsite        = "cafe_no_website"         // no own website; built from the place alone; off by default
)

// KindInfo describes one kind for the migration's kind rows and the console.
type KindInfo struct {
	Kind             string
	EnabledByDefault bool
	Label            string
}

// Kinds lists the café kinds in display order.
var Kinds = []KindInfo{
	{KindNoDigitalLoyalty, true, "Café with a website and no digital loyalty"},
	{KindHasLoyalty, false, "Café already on a loyalty platform"},
	{KindNoWebsite, false, "Café with no website of its own"},
}

// EnabledByDefault reports whether kind is on when a campaign does not say.
func EnabledByDefault(kind string) bool {
	i := slices.IndexFunc(Kinds, func(k KindInfo) bool { return k.Kind == kind })
	return i >= 0 && Kinds[i].EnabledByDefault
}

// Input is what Classify judges one café on.
type Input struct {
	// OwnWebsite is the café's own site (places.Place.OwnWebsite); empty when it lists
	// none, or only social or map pages.
	OwnWebsite string
	// HomepageRead: the homepage was fetched and Detect ran on it. False with a website
	// means the probe did not get through; the café gets no kind yet.
	HomepageRead bool
	Detection    Result
}

// Decision is Classify's verdict. Kind is "" when there is no lead (Skip says why).
type Decision struct {
	Kind     string
	Skip     bool   // no lead: already a Webbership customer, or the homepage was not read
	Reason   string // plain words for the lead page or the skip log
	Evidence string // Result.Evidence for the probe row; "" when there was no homepage
}

// Classify settles one café's kind. The table:
//
//	own website  homepage read  detection                 kind / skip
//	none         -              -                         cafe_no_website
//	yes          no             -                         skip: homepage not read
//	yes          yes            Webbership loyalty seen   skip: already a customer
//	yes          yes            any loyalty seen          cafe_has_loyalty
//	yes          yes            no loyalty seen           cafe_no_digital_loyalty
func Classify(in Input) Decision {
	if strings.TrimSpace(in.OwnWebsite) == "" {
		return Decision{Kind: KindNoWebsite, Reason: "no website of its own on the map listing"}
	}
	if !in.HomepageRead {
		return Decision{Skip: true, Reason: "homepage not read"}
	}
	r := in.Detection
	ev := r.Evidence()
	switch {
	case r.OwnCustomer:
		return Decision{Skip: true, Reason: "already on Webbership Loyalty", Evidence: ev}
	case r.HasLoyalty:
		return Decision{Kind: KindHasLoyalty, Reason: "digital loyalty on the homepage: " + loyaltyWhat(r), Evidence: ev}
	default:
		return Decision{Kind: KindNoDigitalLoyalty, Reason: "no digital loyalty on the homepage", Evidence: ev}
	}
}

// loyaltyWhat names the vendors seen, else the phrase.
func loyaltyWhat(r Result) string {
	if len(r.Vendors) > 0 {
		return strings.Join(r.Vendors, ", ")
	}
	if sig := r.SignalsOf(SigLoyalty); len(sig) > 0 {
		return fmt.Sprintf("%q", sig[0].Match)
	}
	return "seen"
}

// DefaultMaxLocations is the place count above which one name or one website is a chain.
const DefaultMaxLocations = 5

// ChainConfig is how chains are told apart from independent cafés.
type ChainConfig struct {
	// MaxLocations: a name or a website domain on more places than this, in one import,
	// is a chain. 0: DefaultMaxLocations.
	MaxLocations int
	// Names is the configured chain list; a café whose normalised name or brand is one of
	// them, or starts with one followed by a space, is a chain.
	Names []string
	// KeepBranded keeps places that carry an Overture brand; by default a brand is a chain.
	KeepBranded bool
}

// ChainEntry is what the chain check reads of one place.
type ChainEntry struct {
	Name          string
	Website       string // the own website, if any
	Brand         string
	BrandWikidata string
}

// EntryOf is the chain entry of an Overture place.
func EntryOf(p places.Place) ChainEntry {
	return ChainEntry{Name: p.Name, Website: p.OwnWebsite(), Brand: p.Brand, BrandWikidata: p.BrandWikidata}
}

// Chain-skip rules.
const (
	ChainByList   = "list"   // a configured chain name
	ChainByBrand  = "brand"  // Overture holds a brand
	ChainByName   = "name"   // the same normalised name on more than MaxLocations places
	ChainByDomain = "domain" // the same website domain on more than MaxLocations places
)

// ChainVerdict is the chain check's answer; Rule is "" for an independent café.
type ChainVerdict struct {
	Chain  bool
	Rule   string
	Detail string // plain words: which name, brand or domain, and how many places
}

// ChainIndex counts names and domains over one import.
type ChainIndex struct {
	max     int
	list    []string
	branded bool // skip branded
	names   map[string]int
	domains map[string]int
}

// NewChainIndex counts entries (every place in the import, chains included).
func NewChainIndex(cfg ChainConfig, entries []ChainEntry) *ChainIndex {
	ix := &ChainIndex{max: cfg.MaxLocations, branded: !cfg.KeepBranded,
		names: map[string]int{}, domains: map[string]int{}}
	if ix.max <= 0 {
		ix.max = DefaultMaxLocations
	}
	for _, n := range cfg.Names {
		if k := NormalizeName(n); k != "" && !slices.Contains(ix.list, k) {
			ix.list = append(ix.list, k)
		}
	}
	for _, e := range entries {
		if k := NormalizeName(e.Name); k != "" {
			ix.names[k]++
		}
		if d := places.DomainKey(e.Website); d != "" {
			ix.domains[d]++
		}
	}
	return ix
}

// Check judges one entry. Rules run in order: list, brand, name count, domain count.
func (ix *ChainIndex) Check(e ChainEntry) ChainVerdict {
	name, brand := NormalizeName(e.Name), NormalizeName(e.Brand)
	for _, c := range ix.list {
		for _, v := range []string{name, brand} {
			if v != "" && (v == c || strings.HasPrefix(v, c+" ")) {
				return ChainVerdict{Chain: true, Rule: ChainByList, Detail: fmt.Sprintf("on the chain list as %q", c)}
			}
		}
	}
	if ix.branded && (brand != "" || strings.TrimSpace(e.BrandWikidata) != "") {
		b := strings.TrimSpace(e.Brand)
		if b == "" {
			b = strings.TrimSpace(e.BrandWikidata)
		}
		return ChainVerdict{Chain: true, Rule: ChainByBrand, Detail: fmt.Sprintf("the map lists it under the brand %q", b)}
	}
	if n := ix.names[name]; name != "" && n > ix.max {
		return ChainVerdict{Chain: true, Rule: ChainByName, Detail: fmt.Sprintf("the name %q is on %d places (more than %d)", name, n, ix.max)}
	}
	if d := places.DomainKey(e.Website); d != "" && ix.domains[d] > ix.max {
		return ChainVerdict{Chain: true, Rule: ChainByDomain, Detail: fmt.Sprintf("the website %s is on %d places (more than %d)", d, ix.domains[d], ix.max)}
	}
	return ChainVerdict{}
}

// foldMarks drops combining marks after decomposition ("Cafeneaua Şoim" → "Cafeneaua Soim").
var foldMarks = transform.Chain(norm.NFD, runes.Remove(runes.In(unicode.Mn)), norm.NFC)

// NormalizeName is a name for comparing: lower case, accents dropped, anything but letters
// and digits a space, spaces folded ("Café  Néro!" → "cafe nero").
func NormalizeName(s string) string {
	t, _, err := transform.String(foldMarks, s)
	if err != nil {
		t = s
	}
	t = strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return unicode.ToLower(r)
		}
		return ' '
	}, t)
	return strings.Join(strings.Fields(t), " ")
}
