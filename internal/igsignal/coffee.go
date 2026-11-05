package igsignal

import (
	"strings"
	"unicode"

	"golang.org/x/text/runes"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"

	"github.com/gremy/webbership-leadhound/internal/cafe"
)

// CoffeeWordsVersion names the following-list prefilter: the words are signals.yaml's
// instagram.coffee_words; the skill's page snippet and its writer apply the same list with
// the same fold, so an account this keeps is one the skill kept.
const CoffeeWordsVersion = "coffee-words@1"

// dropMarks is the fold's first step: decompose, then drop every mark, as the snippet's
// NFD normalize and its \p{M} replace do.
var dropMarks = transform.Chain(norm.NFD, runes.Remove(runes.In(unicode.M)))

// fold is a handle or a name as the prefilter reads it: accents dropped, lower case.
func fold(s string) string {
	t, _, err := transform.String(dropMarks, s)
	if err != nil {
		t = s
	}
	return strings.ToLower(t)
}

// CoffeeName reports whether an account passes coffee-words@1: one of the coffee words
// appears anywhere inside its folded handle or its folded name.
func CoffeeName(handle, fullName string) bool {
	return coffeeName(cafe.DefaultConfig().Instagram.CoffeeWords, handle, fullName)
}

func coffeeName(words []string, handle, fullName string) bool {
	h, n := fold(handle), fold(fullName)
	for _, w := range words {
		if strings.Contains(h, w) || strings.Contains(n, w) {
			return true
		}
	}
	return false
}
