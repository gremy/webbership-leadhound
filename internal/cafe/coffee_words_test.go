package cafe

import (
	"slices"
	"testing"
)

// TestCoffeeWordsDecided pins coffee-words@1 as decided: the embedded list, in its order. The
// /ig-signals skill's snippets and its helper hold the same list under their own contract test.
func TestCoffeeWordsDecided(t *testing.T) {
	want := []string{"cafe", "caffe", "coffee", "cafea", "cafenea", "kafe", "kaffee", "espresso", "roast", "prajit",
		"brew", "barista", "specialty", "speciality"}
	if got := DefaultConfig().CoffeeWords(); !slices.Equal(got, want) {
		t.Fatalf("instagram.coffee_words = %q, want %q", got, want)
	}
	got := DefaultConfig().CoffeeWords()
	got[0] = "changed"
	if DefaultConfig().CoffeeWords()[0] != "cafe" {
		t.Fatal("CoffeeWords handed out the config's own slice")
	}
}

// TestCoffeeWordsKeptByOwnVendor: the copy with the own vendor added keeps the list.
func TestCoffeeWordsKeptByOwnVendor(t *testing.T) {
	c, err := DefaultConfig().WithOwnVendor("own", []string{"loyalty-own.example"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(c.CoffeeWords(), DefaultConfig().CoffeeWords()) {
		t.Fatalf("WithOwnVendor dropped the coffee words: %q", c.CoffeeWords())
	}
}

func TestCoffeeWordsRefused(t *testing.T) {
	for name, y := range map[string]string{
		"upper case": "instagram: {coffee_words: [Cafe]}",
		"accent":     "instagram: {coffee_words: [cafeá]}",
		"two words":  "instagram: {coffee_words: ['coffee shop']}",
		"empty word": "instagram: {coffee_words: ['']}",
		"repeated":   "instagram: {coffee_words: [cafe, cafe]}",
	} {
		if _, err := ParseConfig([]byte(y)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	c, err := ParseConfig([]byte("vendors: [{id: a, kind: loyalty, match: [a.example]}]"))
	if err != nil || len(c.CoffeeWords()) != 0 {
		t.Fatalf("a list with no instagram block: %v, %q", err, c.CoffeeWords())
	}
}

// TestVendorsAt: the link test Detect uses, on its own. Every vendor a link is on comes back
// once (Stampino's Instagram account is Stampino and Instagram); a path entry needs its path.
func TestVendorsAt(t *testing.T) {
	ids := func(link string) []string {
		var out []string
		for _, v := range DefaultConfig().VendorsAt(link) {
			out = append(out, v.ID)
		}
		return out
	}
	for link, want := range map[string][]string{
		"https://stampino.ro/c/1":                      {"stampino"},
		"https://WWW.Instagram.com/stampino.app/":      {"stampino", "instagram"},
		"https://www.instagram.com/stampino.appfan/":   {"instagram"},
		"https://squareup.com/loyalty/x":               {"square-loyalty"},
		"https://squareup.com/other":                   nil,
		"https://app.glovoapp.com/ro/tm/cafe-exemplu/": {"glovo"},
		"stampino.ro":                   nil, // no scheme: not a link
		"ftp://stampino.ro/":            nil,
		"https://cafe-exemplu.example/": nil,
	} {
		if got := ids(link); !slices.Equal(got, want) {
			t.Errorf("VendorsAt(%q) = %q, want %q", link, got, want)
		}
	}
}
