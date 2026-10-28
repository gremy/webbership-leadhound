package cafe

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func detectFile(t *testing.T, name, pageURL string) Result {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", name)) //nolint:gosec // G304: tests read their own fixtures by name
	if err != nil {
		t.Fatal(err)
	}
	return Detect(nil, Page{URL: pageURL, Body: body})
}

func TestDefaultConfigParses(t *testing.T) {
	c, err := ParseConfig(defaultSignals)
	if err != nil {
		t.Fatal(err)
	}
	if c.Vendor("instagram") == nil || c.Vendor("stampme") == nil {
		t.Fatal("instagram and stampme must be in the default list")
	}
	for _, v := range c.Vendors {
		if v.Own {
			t.Errorf("the own vendor comes from private config, never the embedded list: %q", v.ID)
		}
	}
}

func TestParseConfigRefuses(t *testing.T) {
	for name, y := range map[string]string{
		"bad kind":      "vendors: [{id: a, kind: coupons, match: [a.example]}]",
		"repeated id":   "vendors: [{id: a, kind: loyalty, match: [a.example]}, {id: a, kind: loyalty, match: [b.example]}]",
		"no match":      "vendors: [{id: a, kind: loyalty}]",
		"not a host":    "vendors: [{id: a, kind: loyalty, match: [localhost]}]",
		"blank phrase":  "words: [{id: w, kind: loyalty, phrases: ['  ']}]",
		"words no kind": "words: [{id: w, phrases: [x]}]",
		"not yaml":      "vendors: [",
	} {
		if _, err := ParseConfig([]byte(y)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// TestDetectLoyaltyVendor: a known vendor script makes has_loyalty, with the script URL
// (query dropped) as evidence; Instagram and ordering links are found too, each once.
func TestDetectLoyaltyVendor(t *testing.T) {
	r := detectFile(t, "loyalty_vendor.html", "https://bean-house.example/")
	if !r.HasLoyalty || !r.HasInstagram || !r.HasOrdering || r.OwnCustomer {
		t.Fatalf("flags: %+v", r)
	}
	if !slices.Equal(r.Vendors, []string{"stampme"}) {
		t.Fatalf("vendors %v", r.Vendors)
	}
	want := []string{
		"loyalty:stampme:script:https://cdn.stampme.com/widget/v2.js",
		"instagram:instagram:link:https://www.instagram.com/bean.example/",
		"ordering:ubereats:link:https://www.ubereats.com/store/bean-example",
	}
	var got []string
	for _, s := range r.Signals {
		got = append(got, s.String())
	}
	if !slices.Equal(got, want) {
		t.Fatalf("signals\n got %v\nwant %v", got, want)
	}
	if r.Language != "en" || r.LanguageVia != LangViaHTML {
		t.Fatalf("language %q via %q", r.Language, r.LanguageVia)
	}
	if r.Evidence() != strings.Join(want, "|")+"|lang:en:html-lang:en" {
		t.Fatalf("evidence %q", r.Evidence())
	}
	if strings.Contains(r.Evidence(), "demo-123") {
		t.Fatal("a query string must not reach the evidence")
	}
}

func TestDetectInstagramOnly(t *testing.T) {
	r := detectFile(t, "instagram_only.html", "https://maple.example/")
	if r.HasLoyalty || r.HasOrdering || !r.HasInstagram {
		t.Fatalf("flags: %+v", r)
	}
	if s := r.SignalsOf(SigInstagram); len(s) != 1 || s[0].Match != "https://instagr.am/maple.example" {
		t.Fatalf("instagram evidence %+v", s)
	}
	if r.Language != "en" || r.LanguageNote != "en-GB" {
		t.Fatalf("language %+v", r)
	}
}

// TestDetectOrdering: an ordering platform link and an ordering phrase; a look-alike host
// ("notwolt.com.example") is not Wolt.
func TestDetectOrdering(t *testing.T) {
	r := detectFile(t, "ordering.html", "https://caffe.example/")
	if !r.HasOrdering || r.HasLoyalty || r.HasInstagram {
		t.Fatalf("flags: %+v", r)
	}
	var got []string
	for _, s := range r.SignalsOf(SigOrdering) {
		got = append(got, s.Vendor+":"+s.Via+":"+s.Match)
	}
	want := []string{"bolt-food:link:https://food.bolt.eu/en/1-city/p/1234-demo", "order-words:word:Ordina online"}
	if !slices.Equal(got, want) {
		t.Fatalf("ordering %v, want %v", got, want)
	}
	if r.Language != "it" {
		t.Fatalf("language %q", r.Language)
	}
}

// TestDetectNone: a paper card, a phrase hidden in CSS, an image on a vendor CDN and a path
// that only starts like the vendor's loyalty path are not digital loyalty.
func TestDetectNone(t *testing.T) {
	r := detectFile(t, "none.html", "https://faint-cafe.example/")
	if r.HasLoyalty || r.HasOrdering || r.HasInstagram || len(r.Signals) != 0 {
		t.Fatalf("expected nothing, got %+v", r)
	}
	if r.Language != "en" || r.LanguageVia != LangViaText || !strings.HasSuffix(r.LanguageNote, "%") {
		t.Fatalf("language %+v", r)
	}
	d := Classify(Input{OwnWebsite: "https://faint-cafe.example/", HomepageRead: true, Detection: r})
	if d.Kind != KindNoDigitalLoyalty {
		t.Fatalf("kind %+v", d)
	}
}

// TestDetectRomanian: no lang attribute; the text says Romanian.
func TestDetectRomanian(t *testing.T) {
	r := detectFile(t, "romanian.html", "https://cafeneaua-demo.example/")
	if r.Language != "ro" || r.LanguageVia != LangViaText {
		t.Fatalf("language %q via %q (%s)", r.Language, r.LanguageVia, r.LanguageNote)
	}
	if !r.HasOrdering || r.SignalsOf(SigOrdering)[0].Match != "Comandă online" {
		t.Fatalf("ordering phrase with diacritics: %+v", r.Signals)
	}
}

// TestDetectInlineWidget: a vendor loaded from an inline script, a wallet phrase across a
// non-breaking space, and a look-alike host inside the script that must not count.
func TestDetectInlineWidget(t *testing.T) {
	r := detectFile(t, "inline_widget.html", "https://cafe-quartier.example/")
	var got []string
	for _, s := range r.Signals {
		got = append(got, s.String())
	}
	want := []string{"loyalty:loopy:inline-script:loopyloyalty.com", "loyalty:wallet-words:word:Apple Wallet"}
	if !slices.Equal(got, want) {
		t.Fatalf("signals %v, want %v", got, want)
	}
	if r.HasOrdering || !slices.Equal(r.Vendors, []string{"loopy"}) || r.Language != "fr" {
		t.Fatalf("result %+v", r)
	}
}

func TestDetectOwnCustomer(t *testing.T) {
	if r := detectFile(t, "own_customer.html", "https://client.example/"); r.OwnCustomer || r.HasLoyalty {
		t.Fatalf("without the own vendor configured, the demo host is unknown: %+v", r)
	}
	cfg, err := DefaultConfig().WithOwnVendor("Webbership Loyalty", []string{"loyalty-own.example"})
	if err != nil {
		t.Fatal(err)
	}
	if DefaultConfig().Vendor(OwnVendorID) != nil {
		t.Fatal("WithOwnVendor changed the default config")
	}
	body, err := os.ReadFile(filepath.Join("testdata", "own_customer.html"))
	if err != nil {
		t.Fatal(err)
	}
	r := Detect(cfg, Page{URL: "https://client.example/", Body: body})
	if !r.OwnCustomer || !r.HasLoyalty || r.Language != "ro" || r.Vendors[0] != OwnVendorID {
		t.Fatalf("result %+v", r)
	}
	d := Classify(Input{OwnWebsite: "https://client.example/", HomepageRead: true, Detection: r})
	if !d.Skip || d.Kind != "" {
		t.Fatalf("an existing customer is never a lead: %+v", d)
	}
}

func TestDetectDeterministicAndRelative(t *testing.T) {
	body := []byte(`<html><body><a href="/go">x</a><a href="//instagram.com/a">i</a>` +
		`<a href="javascript:void(0)">j</a><a href="http://[::1">bad</a></body></html>`)
	a := Detect(nil, Page{URL: "https://cafe.example/", Body: body})
	b := Detect(nil, Page{URL: "https://cafe.example/", Body: body})
	if a.Evidence() != b.Evidence() || !a.HasInstagram || a.Signals[0].Match != "https://instagram.com/a" {
		t.Fatalf("%+v vs %+v", a, b)
	}
	if r := Detect(nil, Page{}); r.HasLoyalty || r.Language != "" || len(r.Signals) != 0 {
		t.Fatalf("empty page: %+v", r)
	}
}

func TestSignalsCappedPerKind(t *testing.T) {
	var b strings.Builder
	b.WriteString("<html><body>")
	for _, h := range []string{"ubereats.com", "glovoapp.com", "wolt.com", "tazz.ro", "doordash.com", "grubhub.com", "chownow.com"} {
		b.WriteString(`<a href="https://` + h + `/x">o</a>`)
	}
	b.WriteString("</body></html>")
	r := Detect(nil, Page{URL: "https://cafe.example/", Body: []byte(b.String())})
	if len(r.SignalsOf(SigOrdering)) != MaxSignalsPerKind || !r.HasOrdering {
		t.Fatalf("%d ordering signals", len(r.SignalsOf(SigOrdering)))
	}
}

func TestCustomConfig(t *testing.T) {
	cfg, err := ParseConfig([]byte(`
vendors:
  - {id: demo-loyal, name: Demo, kind: loyalty, match: [loyal.example/cards]}
words:
  - {kind: loyalty, id: demo-words, phrases: [stamp on your phone]}
`))
	if err != nil {
		t.Fatal(err)
	}
	r := Detect(cfg, Page{URL: "https://c.example/", Body: []byte(
		`<p>Stamp   on your PHONE.</p><a href="https://www.loyal.example/cards/1">c</a><a href="https://loyal.example/other">o</a>`)})
	var got []string
	for _, s := range r.Signals {
		got = append(got, s.String())
	}
	want := []string{"loyalty:demo-loyal:link:https://www.loyal.example/cards/1", "loyalty:demo-words:word:Stamp on your PHONE"}
	if !slices.Equal(got, want) {
		t.Fatalf("%v, want %v", got, want)
	}
}
