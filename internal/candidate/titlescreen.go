package candidate

import (
	"fmt"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// TitleRules is the optional title_screen block: a cheap pre-filter on a posting's title.
// Patterns are plain words or phrases (letters, digits, spaces and hyphens), matched
// case-insensitively on whole words: "go" matches "Go Engineer" but not "Google" or
// "cargo", and "full stack" matches "Full-Stack" too. A single-word include of
// fuzzyMinLetters or more letters also matches a title word of that length or more that is
// one Damerau-Levenshtein edit away ("engneer", "devloper"); this deliberately includes
// one-letter plurals and inflections ("engineers", "ingeniera"). Multi-word includes,
// short includes and every exclude match exactly.
type TitleRules struct {
	// Include: a title matching one of these is kept; a title matching none is unsure.
	// Empty keeps every title.
	Include []string `yaml:"include"`
	// Exclude: a title matching any of these is dropped, even when an include matches.
	Exclude []string `yaml:"exclude"`
}

// fuzzyMinLetters is the shortest single-word include, and the shortest title word, that
// the one-edit typo rule applies to.
const fuzzyMinLetters = 6

// TitleVerdict is the title screen's three-way answer.
type TitleVerdict int

const (
	// TitleKeep: an include matches and no exclude does (or there is no screen).
	TitleKeep TitleVerdict = iota
	// TitleDrop: an exclude matches; the posting is not stored.
	TitleDrop
	// TitleUnsure: neither matches; the posting is stored and the title is asked about.
	TitleUnsure
)

// String returns keep, drop or unsure.
func (v TitleVerdict) String() string {
	switch v {
	case TitleKeep:
		return "keep"
	case TitleDrop:
		return "drop"
	case TitleUnsure:
		return "unsure"
	}
	return fmt.Sprintf("TitleVerdict(%d)", int(v))
}

// TitleCheck returns the three-way screen built from title_screen. It never returns nil.
// Without a screen, or with an empty include list, every title is TitleKeep. Otherwise an
// exclude match is TitleDrop (exclude wins), an include match (exact, or the one-edit
// typo rule) is TitleKeep, and anything else is TitleUnsure. The patterns are compiled
// once here, so call it once and reuse the function; it is pure and safe for concurrent
// use.
func (cfg Config) TitleCheck() func(title string) TitleVerdict {
	s := compileScreen(cfg.TitleScreenRules)
	if len(s.include) == 0 {
		return func(string) TitleVerdict { return TitleKeep }
	}
	return s.check
}

// TitleScreen returns the collector's screen: it reports whether a posting with this title
// is stored, which is any verdict but TitleDrop (keep, or unsure for the scorer to ask
// about). It never returns nil; without a screen, or with an empty include list, it stores
// every title. Compile once and reuse, as with TitleCheck.
func (cfg Config) TitleScreen() func(title string) bool {
	check := cfg.TitleCheck()
	return func(title string) bool { return check(title) != TitleDrop }
}

// titleScreen is a compiled title_screen; read-only after compileScreen.
type titleScreen struct {
	include []string // normalised, space-padded
	exclude []string // normalised, space-padded
	fuzzy   [][]rune // single-word includes of fuzzyMinLetters or more, as runes
}

func compileScreen(r TitleRules) *titleScreen {
	s := &titleScreen{include: compilePatterns(r.Include), exclude: compilePatterns(r.Exclude)}
	for _, p := range s.include {
		w := strings.TrimSpace(p)
		if !strings.Contains(w, " ") && utf8.RuneCountInString(w) >= fuzzyMinLetters {
			s.fuzzy = append(s.fuzzy, []rune(w))
		}
	}
	return s
}

func (s *titleScreen) check(title string) TitleVerdict {
	t := normaliseTitle(title)
	for _, p := range s.exclude {
		if strings.Contains(t, p) {
			return TitleDrop
		}
	}
	for _, p := range s.include {
		if strings.Contains(t, p) {
			return TitleKeep
		}
	}
	if len(s.fuzzy) > 0 && s.fuzzyMatch(t) {
		return TitleKeep
	}
	return TitleUnsure
}

// fuzzyMatch reports whether a word of the normalised title t is one edit from a fuzzy
// include. Words are decoded into a stack buffer, so a normal title allocates nothing here.
func (s *titleScreen) fuzzyMatch(t string) bool {
	var buf [48]rune
	for word := range strings.SplitSeq(strings.TrimSpace(t), " ") {
		if utf8.RuneCountInString(word) < fuzzyMinLetters {
			continue
		}
		w := buf[:0]
		for _, r := range word {
			w = append(w, r)
		}
		for _, p := range s.fuzzy {
			if withinOneEdit(w, p) {
				return true
			}
		}
	}
	return false
}

// withinOneEdit reports whether a and b are at most one Damerau-Levenshtein edit apart:
// one insertion, deletion, substitution or swap of two adjacent letters.
func withinOneEdit(a, b []rune) bool {
	la, lb := len(a), len(b)
	if la-lb > 1 || lb-la > 1 {
		return false
	}
	i := 0
	for i < la && i < lb && a[i] == b[i] {
		i++
	}
	switch {
	case la == lb:
		if i == la || slices.Equal(a[i+1:], b[i+1:]) {
			return true
		}
		return i+1 < la && a[i] == b[i+1] && a[i+1] == b[i] && slices.Equal(a[i+2:], b[i+2:])
	case la > lb:
		return slices.Equal(a[i+1:], b[i:])
	default:
		return slices.Equal(a[i:], b[i+1:])
	}
}

// compilePatterns normalises each pattern the way titles are normalised, so a plain
// substring test on the space-padded forms is a whole-word (or whole-phrase) match.
func compilePatterns(list []string) []string {
	out := make([]string, 0, len(list))
	for _, p := range list {
		if n := normaliseTitle(p); n != " " {
			out = append(out, n)
		}
	}
	return out
}

// normaliseTitle applies NFKC, lower-cases, turns every run of characters that are not
// letters, marks or digits (spaces, hyphens, "/", ",", "(", ")", "|" and the rest) into
// one space, and pads the result with a space at each end. A blank input gives " ".
func normaliseTitle(s string) string {
	s = norm.NFKC.String(s)
	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte(' ')
	gap := true
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.IsMark(r) {
			b.WriteRune(unicode.ToLower(r))
			gap = false
		} else if !gap {
			b.WriteByte(' ')
			gap = true
		}
	}
	if !gap {
		b.WriteByte(' ')
	}
	return b.String()
}

// isPlainPattern reports whether p holds only letters, digits, spaces and hyphens and at
// least one letter or digit: a word or phrase, never a regular expression.
func isPlainPattern(p string) bool {
	word := false
	for _, r := range p {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			word = true
		case r == ' ' || r == '-' || unicode.IsMark(r):
		default:
			return false
		}
	}
	return word
}

// checkTitleRules reports empty, non-plain and duplicate patterns (duplicates compared
// after normalisation, so "Full Stack" and "full-stack" are one pattern), a pattern in
// both lists, and an exclude list with no include list (which would screen nothing).
func checkTitleRules(r TitleRules) []string {
	var p []string
	seen := map[string]string{} // normalised pattern -> list it first appeared in
	for _, list := range []struct {
		name     string
		patterns []string
	}{{"include", r.Include}, {"exclude", r.Exclude}} {
		field := "title_screen." + list.name
		for _, pat := range list.patterns {
			if strings.TrimSpace(pat) == "" {
				p = append(p, field+" has an empty pattern")
				continue
			}
			if !isPlainPattern(pat) {
				p = append(p, fmt.Sprintf("%s: %q is not a plain word or phrase (letters, digits, spaces and hyphens only)", field, pat))
				continue
			}
			n := normaliseTitle(pat)
			switch first, dup := seen[n]; {
			case dup && first == list.name:
				p = append(p, fmt.Sprintf("%s: %q is listed twice", field, pat))
			case dup:
				p = append(p, fmt.Sprintf("%s: %q is also in title_screen.%s", field, pat, first))
			default:
				seen[n] = list.name
			}
		}
	}
	if len(r.Exclude) > 0 && len(r.Include) == 0 {
		p = append(p, "title_screen.exclude needs an include list (without one the screen keeps every title)")
	}
	return p
}
