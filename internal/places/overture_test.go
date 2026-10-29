package places

import (
	"os"
	"slices"
	"strings"
	"testing"
)

func readFixture(t *testing.T, opts Options) ([]Place, Stats) {
	t.Helper()
	f, err := os.Open("testdata/places.ndjson")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	ps, st, err := Read(f, opts)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	return ps, st
}

func ids(ps []Place) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.SourceID
	}
	return out
}

// TestReadFixture: cafés kept in input order, non-cafés filtered, every line counted once.
func TestReadFixture(t *testing.T) {
	ps, st := readFixture(t, Options{})
	want := []string{"demo-0001", "demo-0002", "demo-0005", "demo-0006", "demo-0007", "demo-0014"}
	if got := ids(ps); !slices.Equal(got, want) {
		t.Fatalf("kept %v, want %v", got, want)
	}
	wantSt := Stats{Lines: 15, Kept: 6, Blank: 1, Malformed: 5, NotCafe: 1, LowConfidence: 1, NoName: 1,
		MalformedLines: []int{9, 10, 11, 13, 14}}
	if st.Lines != wantSt.Lines || st.Kept != wantSt.Kept || st.Blank != wantSt.Blank ||
		st.Malformed != wantSt.Malformed || st.NotCafe != wantSt.NotCafe ||
		st.LowConfidence != wantSt.LowConfidence || st.NoName != wantSt.NoName ||
		st.OtherCountry != 0 || !slices.Equal(st.MalformedLines, wantSt.MalformedLines) {
		t.Fatalf("stats %+v, want %+v", st, wantSt)
	}
	sum := st.Kept + st.Blank + st.Malformed + st.NotCafe + st.LowConfidence + st.OtherCountry + st.NoName
	if sum != st.Lines || st.Skipped() != st.Lines-st.Kept {
		t.Fatalf("counts do not add up: %+v", st)
	}
}

// TestReadFields: every field the store needs, region and country included.
func TestReadFields(t *testing.T) {
	ps, _ := readFixture(t, Options{})
	p := ps[0]
	if p.Name != "Cafeneaua Demo" || p.Category != "coffee_shop" || p.Confidence != 0.95 {
		t.Fatalf("name/category/confidence: %+v", p)
	}
	if !slices.Equal(p.Alternates, []string{"cafe", "bakery"}) ||
		!slices.Equal(p.Websites, []string{"https://www.cafeneaua-demo.example/"}) ||
		!slices.Equal(p.Phones, []string{"+40 700 000 001"}) ||
		!slices.Equal(p.Emails, []string{"salut@cafeneaua-demo.example"}) ||
		len(p.Socials) != 1 {
		t.Fatalf("lists: %+v", p)
	}
	a := p.Address
	if a.Locality != "Orasul Exemplu" || a.Postcode != "400000" || a.Country != "RO" || a.RegionCode() != "CJ" ||
		a.RegionISO() != "RO-CJ" || a.Freeform != "Strada Exemplu 1" {
		t.Fatalf("address: %+v", a)
	}
	if len(p.Sources) != 1 || p.Sources[0] != (SourceRef{Dataset: "meta", RecordID: "m-1"}) {
		t.Fatalf("sources: %+v", p.Sources)
	}
	if !p.HasPoint || p.Lon != 21.2 || p.Lat != 45.75 {
		t.Fatalf("point: %+v", p)
	}
	if p.Brand != "" || p.OwnWebsite() != "https://www.cafeneaua-demo.example/" {
		t.Fatalf("brand/own website: %+v", p)
	}

	us := ps[1]
	if us.Address.Region != "US-IL" || us.Address.RegionCode() != "IL" || us.Address.RegionISO() != "US-IL" || us.HasPoint {
		t.Fatalf("US region: %+v", us.Address)
	}

	feat := ps[2] // a GeoJSON Feature line
	if feat.Name != "Maple Example Coffee" || feat.Address.RegionISO() != "CA-ON" || !feat.HasPoint || feat.Lon != -79.4 {
		t.Fatalf("feature: %+v", feat)
	}
	if feat.OwnWebsite() != "" {
		t.Fatalf("a Facebook page is not the café's own website: %q", feat.OwnWebsite())
	}

	chain := ps[3]
	if chain.Brand != "Chain Example" || chain.BrandWikidata != "Q0" || chain.Address.RegionISO() != "GB-ENG" {
		t.Fatalf("brand: %+v", chain)
	}

	tax := ps[4]
	if tax.Category != "coffee_shop" {
		t.Fatalf("basic_category fallback: %+v", tax)
	}
}

func TestReadCountries(t *testing.T) {
	ps, st := readFixture(t, Options{Countries: []string{"ro", " CA "}})
	if got := ids(ps); !slices.Equal(got, []string{"demo-0001", "demo-0005"}) {
		t.Fatalf("kept %v", got)
	}
	if st.OtherCountry != 4 {
		t.Fatalf("other country %d, want 4", st.OtherCountry)
	}
}

func TestReadConfidenceAndCategories(t *testing.T) {
	ps, st := readFixture(t, Options{MinConfidence: -1, Categories: []string{"cafe"}})
	// cafe only: 0002, 0004 (low confidence kept), 0014.
	if got := ids(ps); !slices.Equal(got, []string{"demo-0002", "demo-0004", "demo-0014"}) {
		t.Fatalf("kept %v", got)
	}
	if st.LowConfidence != 0 {
		t.Fatalf("negative MinConfidence keeps all: %+v", st)
	}
	ps, _ = readFixture(t, Options{MinConfidence: 0.9})
	if got := ids(ps); !slices.Equal(got, []string{"demo-0001", "demo-0006", "demo-0014"}) {
		t.Fatalf("min 0.9 kept %v", got)
	}
}

// TestReadLongLineAndNoTrailingNewline: an over-long line is one malformed line; the reader
// carries on, and a last line without a newline is still read.
func TestReadLongLineAndNoTrailingNewline(t *testing.T) {
	long := `{"id":"x","names":{"primary":"` + strings.Repeat("a", MaxLineBytes) + `"}}`
	in := long + "\n" + `{"id":"demo-1","names":{"primary":"Tail Example"},"categories":{"primary":"cafe"},"confidence":0.9}`
	ps, st, err := Read(strings.NewReader(in), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if st.Lines != 2 || st.Malformed != 1 || !slices.Equal(st.MalformedLines, []int{1}) || len(ps) != 1 || ps[0].Name != "Tail Example" {
		t.Fatalf("got %v %+v", ids(ps), st)
	}
}

func TestReadEmpty(t *testing.T) {
	ps, st, err := Read(strings.NewReader(""), Options{})
	if err != nil || len(ps) != 0 || st.Lines != 0 {
		t.Fatalf("%v %+v %v", ps, st, err)
	}
}

func TestMalformedLinesCapped(t *testing.T) {
	in := strings.Repeat("not json\n", MaxMalformedLines+10)
	_, st, err := Read(strings.NewReader(in), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if st.Malformed != MaxMalformedLines+10 || len(st.MalformedLines) != MaxMalformedLines {
		t.Fatalf("%d malformed, %d kept numbers", st.Malformed, len(st.MalformedLines))
	}
}

func TestIsBrandSite(t *testing.T) {
	for _, c := range []struct {
		link string
		want bool
	}{
		{"https://www.lavazza.example/it", true},
		{"https://shop.lavazza.co.uk/", true},
		{"lavazza.ro", true},
		{"https://www3.hilton.com/hotel", true},
		{"https://lavazzacafe.example/", false},
		{"https://cafe.example/lavazza", false},
		{"", false},
	} {
		if got := IsBrandSite(c.link); got != c.want {
			t.Errorf("IsBrandSite(%q) = %v, want %v", c.link, got, c.want)
		}
	}
}

// TestPlatformWebsiteIsNoWebsite: a place whose only website is a platform's page has none of
// its own; a real site listed after the platform's is still found.
func TestPlatformWebsiteIsNoWebsite(t *testing.T) {
	for _, c := range []struct {
		sites []string
		want  string
	}{
		{[]string{"https://bit.ly/x"}, ""},
		{[]string{"https://demo.wixsite.com/cafe", "https://share.google/y"}, ""},
		{[]string{"https://tinyurl.com/z", "https://cafe-demo.example/"}, "https://cafe-demo.example/"},
	} {
		if got := (Place{Websites: c.sites}).OwnWebsite(); got != c.want {
			t.Errorf("OwnWebsite(%v) = %q, want %q", c.sites, got, c.want)
		}
	}
}

func TestLinks(t *testing.T) {
	cases := []struct {
		link, host, key string
		shared, social  bool
	}{
		{"https://www.cafe.example/menu", "cafe.example", "cafe.example", false, false},
		{"cafe.example", "cafe.example", "cafe.example", false, false},
		{"https://london.chain.example.co.uk/x", "london.chain.example.co.uk", "example.co.uk", false, false},
		{"https://www.instagram.com/cafe.example", "instagram.com", "", true, true},
		{"https://m.facebook.com/x", "m.facebook.com", "", true, true},
		{"https://cafe.wixsite.com/home", "cafe.wixsite.com", "", true, false},
		{"https://www.ubereats.com/store/x", "ubereats.com", "", true, false},
		// Shorteners, maps and search on any Google suffix, builders on their own domain or a
		// café's subdomain, encyclopedias, messaging, menu platforms: never a café's own site.
		{"https://bit.ly/cafe-demo", "bit.ly", "", true, false},
		{"https://share.google/abc", "share.google", "", true, false},
		{"https://maps.app.goo.gl/x", "maps.app.goo.gl", "", true, false},
		{"https://www.google.ro/maps/place/x", "google.ro", "", true, false},
		{"https://sites.google.com/view/cafe", "sites.google.com", "", true, false},
		{"https://maps.google.co.uk/?cid=1", "maps.google.co.uk", "", true, false},
		{"https://ro.wikipedia.org/wiki/Cafe", "ro.wikipedia.org", "", true, false},
		{"https://www.wix.com/demo", "wix.com", "", true, false},
		{"https://demo.wix.com/cafe", "demo.wix.com", "", true, false},
		{"https://demo-cafe.metro.bar/", "demo-cafe.metro.bar", "", true, false},
		{"https://chat.whatsapp.com/x", "chat.whatsapp.com", "", true, false},
		{"https://meniudigital.ro/demo", "meniudigital.ro", "", true, false},
		{"meniudigital.ro", "meniudigital.ro", "", true, false},
		// Not platforms: a name merely holding a platform's word, and a brand's site (skipped
		// as a chain by the chain check, not "no website").
		{"https://googlecafe.example/", "googlecafe.example", "googlecafe.example", false, false},
		{"https://cafe-bit.ly.example/", "cafe-bit.ly.example", "ly.example", false, false},
		{"https://www.lavazza.example/", "lavazza.example", "lavazza.example", false, false},
		{"", "", "", false, false},
	}
	for _, c := range cases {
		if got := Host(c.link); got != c.host {
			t.Errorf("Host(%q) = %q, want %q", c.link, got, c.host)
		}
		if got := DomainKey(c.link); got != c.key {
			t.Errorf("DomainKey(%q) = %q, want %q", c.link, got, c.key)
		}
		if IsShared(c.link) != c.shared || IsSocial(c.link) != c.social {
			t.Errorf("%q: shared %v social %v", c.link, IsShared(c.link), IsSocial(c.link))
		}
	}
}
