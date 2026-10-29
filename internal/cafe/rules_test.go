package cafe

import (
	"strings"
	"testing"

	"github.com/gremy/webbership-leadhound/internal/places"
)

func TestKinds(t *testing.T) {
	if !EnabledByDefault(KindNoDigitalLoyalty) || EnabledByDefault(KindHasLoyalty) ||
		EnabledByDefault(KindNoWebsite) || EnabledByDefault("unsupported") {
		t.Fatal("only cafe_no_digital_loyalty is on by default")
	}
}

// TestClassifyTable is the kind selection table in Classify's doc comment.
func TestClassifyTable(t *testing.T) {
	loyal := Result{HasLoyalty: true, Vendors: []string{"stampme"},
		Signals: []Signal{{Kind: SigLoyalty, Vendor: "stampme", Via: ViaScript, Match: "https://cdn.stampme.com/w.js"}}}
	wordOnly := Result{HasLoyalty: true,
		Signals: []Signal{{Kind: SigLoyalty, Vendor: "wallet-words", Via: ViaWord, Match: "Apple Wallet"}}}
	own := Result{HasLoyalty: true, OwnCustomer: true, Vendors: []string{"webbership"}}
	plain := Result{HasInstagram: true, HasOrdering: true}
	cases := []struct {
		name       string
		in         Input
		kind       string
		skip       bool
		reasonPart string
	}{
		{"no website", Input{}, KindNoWebsite, false, "no website"},
		{"blank website", Input{OwnWebsite: "  ", HomepageRead: true, Detection: loyal}, KindNoWebsite, false, "no website"},
		{"not read", Input{OwnWebsite: "https://c.example/"}, "", true, "not read"},
		{"own customer", Input{OwnWebsite: "https://c.example/", HomepageRead: true, Detection: own}, "", true, "Webbership"},
		{"vendor", Input{OwnWebsite: "https://c.example/", HomepageRead: true, Detection: loyal}, KindHasLoyalty, false, "stampme"},
		{"phrase only", Input{OwnWebsite: "https://c.example/", HomepageRead: true, Detection: wordOnly}, KindHasLoyalty, false, `"Apple Wallet"`},
		{"none", Input{OwnWebsite: "https://c.example/", HomepageRead: true, Detection: plain}, KindNoDigitalLoyalty, false, "no digital loyalty"},
	}
	for _, c := range cases {
		d := Classify(c.in)
		if d.Kind != c.kind || d.Skip != c.skip || !strings.Contains(d.Reason, c.reasonPart) {
			t.Errorf("%s: got %+v", c.name, d)
		}
		if d.Kind == KindHasLoyalty && d.Evidence == "" {
			t.Errorf("%s: no evidence", c.name)
		}
	}
}

func entry(name, site string) ChainEntry { return ChainEntry{Name: name, Website: site} }

func TestChains(t *testing.T) {
	var all []ChainEntry
	for i := range 6 { // six places, one name: more than 5
		all = append(all, entry("Bean  Example House", "https://bean"+string(rune('a'+i))+".example/"))
	}
	for i := range 6 { // six names, one domain (per-location subdomains)
		all = append(all, entry("Roast Example "+string(rune('A'+i)), "https://loc"+string(rune('a'+i))+".roast-chain.example/"))
	}
	for range 5 { // five places: exactly the limit, still independent
		all = append(all, entry("Five Example Cafe", "https://five.example/"))
	}
	for range 7 { // seven cafés whose site is a Facebook page: not one domain
		all = append(all, entry("Solo Example "+strings.Repeat("x", len(all)%3+1), "https://facebook.com/solo"))
	}
	branded := ChainEntry{Name: "Corner Example", Brand: "Corner Example Coffee", BrandWikidata: "Q0"}
	all = append(all, branded)
	ix := NewChainIndex(ChainConfig{Names: []string{"Café Lista"}}, all)

	cases := []struct {
		name string
		e    ChainEntry
		rule string
	}{
		{"name count", entry("bean example house!", "https://own.example/"), ChainByName},
		{"domain count", entry("Roast Example Z", "https://www.roast-chain.example/menu"), ChainByDomain},
		{"at the limit", entry("Five Example Cafe", "https://five.example/"), ""},
		{"shared platform", entry("Solo Example x", "https://facebook.com/solo"), ""},
		{"brand", branded, ChainByBrand},
		{"brand wikidata only", ChainEntry{Name: "Q Example", BrandWikidata: "Q1"}, ChainByBrand},
		{"list exact, accents", entry("CAFE LISTA", ""), ChainByList},
		{"list prefix", entry("Café Lista Centru", ""), ChainByList},
		{"list not a prefix word", entry("Café Listaria", ""), ""},
		{"list by brand", ChainEntry{Name: "Corner", Brand: "Cafe Lista"}, ChainByList},
		{"independent", entry("Cafeneaua Demo", "https://cafeneaua-demo.example/"), ""},
	}
	for _, c := range cases {
		v := ix.Check(c.e)
		if v.Rule != c.rule || v.Chain != (c.rule != "") {
			t.Errorf("%s: got %+v, want rule %q", c.name, v, c.rule)
		}
		if v.Chain && v.Detail == "" {
			t.Errorf("%s: no detail", c.name)
		}
	}

	keep := NewChainIndex(ChainConfig{KeepBranded: true, MaxLocations: 10}, all)
	if v := ix.Check(entry("Roast Example Z", "https://www.roast-chain.example/menu")); v.SharedDomain {
		t.Errorf("a domain count chain is not a shared website: %+v", v)
	}
	if v := keep.Check(branded); v.Chain {
		t.Errorf("KeepBranded: %+v", v)
	}
	if v := keep.Check(entry("Bean Example House", "")); v.Chain {
		t.Errorf("MaxLocations 10: six is not a chain: %+v", v)
	}
}

// TestSharedDomains: a brand's website is skipped whoever lists it; a domain three or more
// differently named places list without carrying its name is shared, and skips those places
// but not the café named after it; two such names, or one café's several branch names,
// are still one café.
func TestSharedDomains(t *testing.T) {
	all := []ChainEntry{
		entry("Cafe Alfa", "https://menuhub.example/a"), entry("Bistro Beta", "https://menuhub.example/b"),
		entry("Gama Coffee", "https://www.menuhub.example/g"), entry("Menuhub Cafe", "https://menuhub.example/"),
		entry("Delta Cafe", "https://pair.example/"), entry("Epsilon Bistro", "https://pair.example/"),
		entry("Zorbix Cafe Mall", "https://zorbixcafe.example/"), entry("Zorbix's Cafe Centru", "https://zorbixcafe.example/"),
		entry("Zorbix Cafe Gara", "https://zorbixcafe.example/"), entry("Quillon Cafe Nord", "https://laquillon.example/"),
		entry("Quillon Cafe", "https://laquillon.example/"), entry("Quillon Cafe Sud", "https://laquillon.example/x"),
		entry("Coffee Plimo", "https://coffee.example/1"), entry("Cafe Vantrel", "https://coffee.example/2"),
		entry("Coffee Oskalo", "https://www.coffee.example/3"),
		entry("Plimo Coffee", "https://g.co/kgs/1"), entry("Vantrel Demo", "https://g.co/kgs/2"), entry("Oskalo", "https://g.co/kgs/3"),
	}
	ix := NewChainIndex(ChainConfig{}, all)
	for _, c := range []struct {
		name   string
		e      ChainEntry
		shared bool
	}{
		{"unrelated name on a shared domain", entry("Cafe Alfa", "https://menuhub.example/a"), true},
		{"the domain's own café", entry("Menuhub Cafe", "https://menuhub.example/"), false},
		{"two unrelated names", entry("Delta Cafe", "https://pair.example/"), false},
		{"branches of one café", entry("Zorbix Cafe Gara", "https://zorbixcafe.example/"), false},
		{"a word of the name in the domain", entry("Quillon Cafe Sud", "https://laquillon.example/x"), false},
		{"a brand's site, alone", entry("Brivano Demo", "https://www.lavazza.example/"), true},
		{"a brand's site, named after it", entry("Lavazza", "https://lavazza.example/"), true},
		{"a directory on a generic label", entry("Coffee Plimo", "https://coffee.example/1"), true},
		{"a platform: no website, not shared", entry("Vantrel Demo", "https://g.co/kgs/2"), false},
		{"independent", entry("Cafeneaua Demo", "https://cafeneaua-demo.example/"), false},
	} {
		v := ix.Check(c.e)
		if v.SharedDomain != c.shared || v.Chain != c.shared || (c.shared && (v.Rule != ChainByDomain || v.Detail == "")) {
			t.Errorf("%s: got %+v, want shared %v", c.name, v, c.shared)
		}
	}
}

func TestEntryOf(t *testing.T) {
	p := places.Place{Name: "Maple Example", Websites: []string{"https://facebook.com/m", "https://maple.example/"},
		Brand: "Maple", BrandWikidata: "Q2"}
	e := EntryOf(p)
	if e.Website != "https://maple.example/" || e.Brand != "Maple" || e.BrandWikidata != "Q2" || e.Name != "Maple Example" {
		t.Fatalf("%+v", e)
	}
}

func TestNormalizeName(t *testing.T) {
	for in, want := range map[string]string{
		"Café  Néro!":         "cafe nero",
		"Cafeneaua Şoim & Co": "cafeneaua soim co",
		"  ":                  "",
		"Kaffee-Haus 24":      "kaffee haus 24",
	} {
		if got := NormalizeName(in); got != want {
			t.Errorf("NormalizeName(%q) = %q, want %q", in, got, want)
		}
	}
}
