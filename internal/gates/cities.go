package gates

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/runes"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"
)

// foldPlace lower-cases s and drops its diacritics, so "Șample", "Şample" and "sample"
// compare equal.
func foldPlace(s string) string {
	t := transform.Chain(norm.NFD, runes.Remove(runes.In(unicode.Mn)), norm.NFC)
	out, _, err := transform.String(t, s)
	if err != nil {
		out = s
	}
	return strings.ToLower(out)
}

// allowedCity returns the first of cities that phrase names as a whole word, as the
// candidate wrote it, or "" when it names none.
func allowedCity(phrase string, cities []string) string {
	text := foldPlace(phrase)
	for _, c := range cities {
		name := foldPlace(strings.TrimSpace(c))
		if name == "" {
			continue
		}
		for from := 0; from < len(text); {
			i := strings.Index(text[from:], name)
			if i < 0 {
				break
			}
			start, end := from+i, from+i+len(name)
			if wordEdge(text, start, end) {
				return strings.TrimSpace(c)
			}
			from = start + 1
		}
	}
	return ""
}

// wordEdge reports whether text[start:end] is bounded by non-letters, non-digits or the
// text's ends.
func wordEdge(text string, start, end int) bool {
	if start > 0 {
		r, _ := utf8.DecodeLastRuneInString(text[:start])
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return false
		}
	}
	if end < len(text) {
		r, _ := utf8.DecodeRuneInString(text[end:])
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return false
		}
	}
	return true
}
