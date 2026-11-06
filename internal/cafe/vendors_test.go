package cafe

import (
	"slices"
	"testing"
)

// TestDetectAddedLoyaltyVendors: each competitor is found from a homepage linking its host,
// with the exact id the private campaign keys its links on.
func TestDetectAddedLoyaltyVendors(t *testing.T) {
	for _, tc := range []struct{ id, href string }{
		{"stampino", "https://stampino.ro/"},
		{"loyalzoo", "https://www.loyalzoo.com/"},
		{"loyalzoo", "https://start.mylty.co/?id=abc"},
		{"rewardly", "https://rewardly.me/"},
		{"rewardly", "https://rewardly.app/"},
		{"cardora", "https://cardora.ro/card/1"},
		{"tap2", "https://tap2.ai/"},
		{"tap2", "https://tap2.ch/"},
		{"tap2", "https://tap2.es/"},
		{"tap2", "https://tap2pay.ai/"},
		{"pick-coupons", "https://pick.coupons/"},
		{"pick-coupons", "https://www.instagram.com/pick.coupons/"},
	} {
		body := `<html><body><a href="` + tc.href + `">card</a></body></html>`
		r := Detect(nil, Page{URL: "https://cafe.example/", Body: []byte(body)})
		if !r.HasLoyalty || !slices.Equal(r.Vendors, []string{tc.id}) {
			t.Errorf("%s: loyalty %v vendors %v, want [%s]", tc.href, r.HasLoyalty, r.Vendors, tc.id)
		}
		if DefaultConfig().Vendor(tc.id) == nil {
			t.Errorf("%s: not in the default list", tc.id)
		}
	}
}

// TestDetectStampinoInstagram: a link to Stampino's Instagram is Stampino loyalty (every
// matching vendor fires on one link), and Instagram as well; another account is only Instagram.
func TestDetectStampinoInstagram(t *testing.T) {
	r := Detect(nil, Page{URL: "https://cafe.example/", Body: []byte(`<a href="https://www.instagram.com/stampino.app/">x</a>`)})
	if !r.HasLoyalty || !slices.Equal(r.Vendors, []string{"stampino"}) || !r.HasInstagram {
		t.Fatalf("stampino instagram: %+v", r)
	}
	r = Detect(nil, Page{URL: "https://cafe.example/", Body: []byte(`<a href="https://www.instagram.com/stampino.appfan/">x</a>`)})
	if r.HasLoyalty || !r.HasInstagram {
		t.Fatalf("look-alike account: %+v", r)
	}
}

// TestInstagramListsDecided pins D7's Instagram lists and vendor words as decided.
func TestInstagramListsDecided(t *testing.T) {
	ig := DefaultConfig().Instagram
	for _, c := range []struct {
		key       string
		got, want []string
	}{
		{"loyalty_cues", ig.LoyaltyCues, []string{"partener", "partner", "puncte", "points", "recompens", "reward", "fidelitate", "loyalty", "card", "aplicati", "app", "cupon", "coupon"}},
		{"prepaid_words", ig.PrepaidWords, []string{"abonament", "subscription", "prepaid", "preplatit", "bundle"}},
		{"question_cues", ig.QuestionCues, []string{"cat", "pret", "pretul", "price", "cost", "costa", "info", "detalii", "details", "dm", "cum", "how", "demo", "oferta"}},
		{"pick-coupons ig_words", DefaultConfig().Vendor("pick-coupons").IGWords, []string{"PICK"}},
	} {
		if !slices.Equal(c.got, c.want) {
			t.Errorf("%s = %q, want %q", c.key, c.got, c.want)
		}
	}
	if v := DefaultConfig().Vendor("tuanis"); v == nil || v.Name != "Tuanis" || v.Kind != SigLoyalty ||
		!slices.Equal(v.Match, []string{"tuanis.eu", "instagram.com/tuanis.eu"}) {
		t.Errorf("tuanis: %+v", v)
	}
	if !slices.Contains(DefaultConfig().Vendor("cardora").Match, "instagram.com/cardora.ro") {
		t.Error("cardora: no instagram.com/cardora.ro")
	}
}

// TestParseConfigRefusesInstagramWords: a cue that is not one lower-case ASCII word, a
// repeat, or an ig_words entry that is not one word, or sits on a non-loyalty vendor.
func TestParseConfigRefusesInstagramWords(t *testing.T) {
	for _, y := range []string{
		"instagram: {loyalty_cues: [Card]}",
		"instagram: {prepaid_words: [bundle, bundle]}",
		"instagram: {question_cues: [preț]}",
		"instagram: {question_cues: [how much]}",
		"vendors: [{id: a, kind: loyalty, match: [a.example], ig_words: [PI CK]}]",
		"vendors: [{id: a, kind: loyalty, match: [a.example], ig_words: [PICK, PICK]}]",
		"vendors: [{id: a, kind: ordering, match: [a.example], ig_words: [PICK]}]",
	} {
		if _, err := ParseConfig([]byte(y)); err == nil {
			t.Errorf("%q: accepted", y)
		}
	}
}
