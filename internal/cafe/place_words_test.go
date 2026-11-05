package cafe

import (
	"slices"
	"testing"
)

// TestPlaceWordRoasteryMatches covers case, diacritics, whole words and punctuation.
func TestPlaceWordRoasteryMatches(t *testing.T) {
	cases := []struct {
		name, input, want string
	}{
		{"plain word", "Zorbix Roastery", "roastery"},
		{"upper case", "ZORBIX ROASTERS", "roasters"},
		{"multi-word entry", "Quillon Coffee Roaster", "coffee roaster"},
		{"multi-word entry across punctuation", "Quillon Coffee-Roaster", "coffee roaster"},
		{"diacritics as configured", "Prăjitoria Zorbix", "prăjitoria"},
		{"diacritics missing from the name", "Prajitoria Zorbix", "prăjitoria"},
		{"upper case with diacritics", "PRĂJITORIE QUILLON", "prăjitorie"},
		{"decomposed diacritics", "Pra\u0306jitorie Quillon", "prăjitorie"},
		{"compound word", "Microprăjitorie Zorbix", "microprăjitorie"},
		{"italian word", "Torrefazione Quillon", "torrefazione"},
		{"parentheses", "Zorbix (Roastery)", "roastery"},
		{"trailing punctuation", "Zorbix Roastery!", "roastery"},
		{"hyphen", "Zorbix-Roastery", "roastery"},
		{"comma", "Roasters, Quillon", "roasters"},
		{"a roastery that also has a shop", "Zorbix Coffee Roastery and Shop", "roastery"},

		{"past tense", "Roasted Bean", ""},
		{"word inside a longer word", "Roasterystreet", ""},
		{"word at the end of a longer word", "Microroastery Zorbix", ""},
		{"singular outside the multi-word entry", "Zorbix Roaster", ""},
		{"multi-word entry with its words apart", "Coffee Quillon Roaster", ""},
		{"shop alone", "Zorbix Coffee Shop", ""},
		{"magazin alone", "Magazin Quillon", ""},
		{"a café", "Cafeneaua Zorbix", ""},
		{"empty name", "", ""},
		{"blank name", "   ", ""},
		{"only punctuation", "!!! ---", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := RoasteryWord(c.input)
			if got != c.want || ok != (c.want != "") {
				t.Errorf("RoasteryWord(%q) = %q, %v; want %q, %v", c.input, got, ok, c.want, c.want != "")
			}
		})
	}
}

// TestPlaceWordList pins the embedded list, as the plan decided it, and that each word finds
// itself written back unchanged.
func TestPlaceWordList(t *testing.T) {
	want := []string{"roastery", "roasters", "coffee roaster", "prăjitorie", "prăjitoria", "microprăjitorie", "torrefazione"}
	var got []string
	for _, w := range roasteryWords() {
		got = append(got, w.written)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("place_words.roastery = %q, want %q", got, want)
	}
	for _, w := range want {
		if m, ok := RoasteryWord("Zorbix " + w); !ok || m != w {
			t.Errorf("RoasteryWord(%q) = %q, %v", "Zorbix "+w, m, ok)
		}
	}
}

// TestPlaceWordBadList covers a list that cannot load.
func TestPlaceWordBadList(t *testing.T) {
	for name, data := range map[string]string{
		"not yaml":   "place_words: [",
		"blank word": "place_words: {roastery: [\"--\"]}",
	} {
		if _, err := parseRoasteryWords([]byte(data)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}
