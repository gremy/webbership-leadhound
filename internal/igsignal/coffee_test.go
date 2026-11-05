package igsignal

import (
	"slices"
	"testing"

	"golang.org/x/text/unicode/norm"

	"github.com/gremy/webbership-leadhound/internal/cafe"
)

// TestCoffeeName: coffee-words@1 as the skill's snippet applies it: any word inside the folded
// handle or name, accents dropped, case ignored.
func TestCoffeeName(t *testing.T) {
	for _, c := range []struct {
		handle, name string
		want         bool
	}{
		{"cafe.exemplu", "", true},
		{"x_example", "Prăjitorie Exemplu", true}, // ă folded: prajit
		{"x_example", "Café Exemplu", true},
		{"x_example", "CAFENEA EXEMPLU", true},
		{"kaffee_example", "", true},
		{"x_example", "Specialty Example", true},
		{"x_example", "Speciality Example", true},
		{"x_example", "Coffee Roasters", true},
		{"thebrewexample", "", true}, // inside a word counts
		{"x_example", "Ristretto bar", false},
		{"gym_example", "Gym Example", false},
		{"", "", false},
		{"torrefazione_example", "", false}, // a roastery word that is no coffee word: the list is the decided one
	} {
		if got := CoffeeName(c.handle, c.name); got != c.want {
			t.Errorf("CoffeeName(%q, %q) = %v, want %v", c.handle, c.name, got, c.want)
		}
	}
}

// TestCoffeeFold: the fold drops every mark, composed or not, as the snippet's NFD and
// \p{M} do; Romanian comma and cedilla letters fold alike.
func TestCoffeeFold(t *testing.T) {
	for in, want := range map[string]string{
		"Prăjitoria Șoim":          "prajitoria soim",
		"Prăjitoria Şoim":          "prajitoria soim",
		norm.NFD.String("Cafénea"): "cafenea",
		"CAFÉ":                     "cafe",
		"Kaffeë":                  "kaffee",
		"☕ coffee":                 "☕ coffee",
		norm.NFC.String("îndrăgostiți de cafea"): "indragostiti de cafea",
	} {
		if got := fold(in); got != want {
			t.Errorf("fold(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestCoffeeWordsFromSignals: the words are signals.yaml's, nowhere else; an empty list keeps
// nothing.
func TestCoffeeWordsFromSignals(t *testing.T) {
	words := cafe.DefaultConfig().CoffeeWords()
	if len(words) == 0 || !slices.Contains(words, "prajit") {
		t.Fatalf("signals.yaml coffee words: %q", words)
	}
	for _, w := range words {
		if !CoffeeName(w, "") || !CoffeeName("", w) {
			t.Errorf("%q does not keep itself", w)
		}
	}
	if coffeeName(nil, "cafe.exemplu", "Cafe") {
		t.Error("an empty list kept an account")
	}
	if CoffeeWordsVersion != "coffee-words@1" {
		t.Errorf("version %q", CoffeeWordsVersion)
	}
}
