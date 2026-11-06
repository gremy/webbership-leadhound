package igsignal

import (
	"testing"

	"github.com/gremy/webbership-leadhound/internal/cafe"
)

// TestVendorWord: a vendor's ig_words count case-sensitively, as a whole word, and only beside
// a loyalty cue in the same bio or caption (Toto Café's "PICK").
func TestVendorWord(t *testing.T) {
	for _, c := range []struct {
		name    string
		p       Profile
		id, via string
	}{
		{"Toto's case", Profile{Captions: []string{"New partner 🎉 collect points with PICK"}}, "pick-coupons", VendorViaWord},
		{"folded cue in the bio", Profile{Bio: "Recompense cu PICK."}, "pick-coupons", VendorViaWord},
		{"wrong case, no cue", Profile{Captions: []string{"pick up your coffee"}}, "", ""},
		{"wrong case beside a cue", Profile{Captions: []string{"pick up your loyalty card"}}, "", ""},
		{"no cue", Profile{Captions: []string{"PICK your coffee"}}, "", ""},
		{"not a whole word", Profile{Captions: []string{"PICKUP points"}}, "", ""},
		{"cue in another caption", Profile{Captions: []string{"PICK", "loyalty card"}}, "", ""},
		{"a tag outranks a word", Profile{Captions: []string{"partner PICK", "@stampino.app"}}, "stampino", VendorViaTag},
		{"Tuanis by its Instagram tag", Profile{Bio: "carduri cu @tuanis.eu"}, "tuanis", VendorViaTag},
		{"Cardora by its Instagram tag", Profile{Captions: []string{"via @cardora.ro"}}, "cardora", VendorViaTag},
	} {
		id, via, ok := Vendor(c.p, nil)
		if id != c.id || via != c.via || ok != (c.id != "") {
			t.Errorf("%s: %q %q %v, want %q %q", c.name, id, via, ok, c.id, c.via)
		}
	}
}

// TestVendorWordOwnWins: the own vendor seen by a word still outranks a rival tag.
func TestVendorWordOwnWins(t *testing.T) {
	cfg, err := cafe.ParseConfig([]byte(`
instagram: {loyalty_cues: [card]}
vendors:
  - {id: rival, name: Rival, kind: loyalty, match: [rival.example]}
  - {id: mine, name: Mine, kind: loyalty, own: true, match: [mine.example], ig_words: [MINE]}
`))
	if err != nil {
		t.Fatal(err)
	}
	if id, via, _ := Vendor(Profile{Bio: "@rival.example", Captions: []string{"card with MINE"}}, cfg); id != "mine" || via != VendorViaWord {
		t.Fatalf("own by word: %q %q", id, via)
	}
}

func TestPrepaid(t *testing.T) {
	for _, c := range []struct {
		name string
		p    Profile
		want bool
	}{
		{"bio", Profile{Bio: "Abonament lunar de cafea"}, true},
		{"caption, folded", Profile{Captions: []string{"latte", "Cafea PREPLĂTITĂ? nu, preplătit!"}}, true},
		{"not a whole word", Profile{Bio: "subscriptions open", Captions: []string{"bundles"}}, false},
		{"nothing", Profile{Bio: "Specialty coffee", Captions: []string{"flat white"}}, false},
	} {
		if got := Prepaid(c.p, nil); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

func TestHasWord(t *testing.T) {
	cues := cafe.DefaultConfig().Instagram.QuestionCues
	for _, c := range []struct {
		text string
		want bool
	}{
		{"Cât costă?", true},
		{"Preț pentru 50 de carduri", true},
		{"DM me", true},
		{"catalog nou", false},
		{"frumos", false},
	} {
		if got := HasWord(c.text, cues); got != c.want {
			t.Errorf("%q: %v, want %v", c.text, got, c.want)
		}
	}
}
