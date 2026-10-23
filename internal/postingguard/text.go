package postingguard

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	blackFlag = '\U0001F3F4'
	cancelTag = '\U000E007F'
)

// rgiTagFlags are the tag letters of the England, Scotland and Wales flags (gbeng, gbsct,
// gbwls) after the black flag, before the cancel tag: the one legitimate use of tag
// characters. A payload inside a well-formed sequence, extra tags after a valid flag, or a
// non-RGI subdivision still count and alarm.
var rgiTagFlags = [][]rune{
	[]rune("\U000E0067\U000E0062\U000E0065\U000E006E\U000E0067"),
	[]rune("\U000E0067\U000E0062\U000E0073\U000E0063\U000E0074"),
	[]rune("\U000E0067\U000E0062\U000E0077\U000E006C\U000E0073"),
}

func isZeroWidth(r rune) bool {
	return r == 0x200B || r == 0x200C || r == 0x200D || (r >= 0x2060 && r <= 0x2064)
}

// isBidi: directional marks, embeddings, overrides and isolates. U+061C (Arabic letter
// mark) is added to upstream's set.
func isBidi(r rune) bool {
	return r == 0x061C || r == 0x200E || r == 0x200F || (r >= 0x202A && r <= 0x202E) || (r >= 0x2066 && r <= 0x2069)
}

// isBidiTamper: overrides and isolates, which reorder what the reader sees.
func isBidiTamper(r rune) bool {
	return r == 0x202D || r == 0x202E || (r >= 0x2066 && r <= 0x2069)
}

// isFormat: upstream's soft hyphen and line and paragraph separators, plus runes this port
// adds because a reader cannot see them either: the Mongolian vowel separator, the
// interlinear annotation controls, the Hangul fillers (which render as blank space) and
// the variation selectors supplement (which no job posting needs and which can carry
// smuggled bytes).
func isFormat(r rune) bool {
	switch r {
	case 0x00AD, 0x2028, 0x2029, 0x180E, 0xFFF9, 0xFFFA, 0xFFFB, 0x3164, 0xFFA0:
		return true
	}
	return r >= 0xE0100 && r <= 0xE01EF
}

func isTag(r rune) bool { return r >= 0xE0000 && r <= 0xE007F }

// isVariationSelector: VS1..VS16. Clean keeps one only directly after an emoji base, where
// it chooses text or emoji presentation; anywhere else it is counted as format and removed.
func isVariationSelector(r rune) bool { return r >= 0xFE00 && r <= 0xFE0F }

// isEmojiBase approximates the bases of emoji-variation-sequences.txt: every Other Symbol,
// plus the keycap bases and the few listed bases in other categories.
func isEmojiBase(r rune) bool {
	switch {
	case unicode.Is(unicode.So, r), r == '#', r == '*', r >= '0' && r <= '9',
		r == 0x203C, r == 0x2049, r == 0x2139, r >= 0x2194 && r <= 0x2199,
		r >= 0x25FB && r <= 0x25FE, r == 0x2934, r == 0x2935, r == 0x3030, r == 0x303D:
		return true
	}
	return false
}

// keptSelector reports whether rs[i], a variation selector, follows an emoji base directly.
func keptSelector(rs []rune, i int) bool {
	return i > 0 && isEmojiBase(rs[i-1])
}

// IsInvisible reports whether r is in the set Clean always removes from text: zero-width,
// bidi controls, the format runes (see isFormat), BOM and Unicode tag characters (tag
// characters survive only inside an RGI subdivision flag). Variation selectors depend on
// what precedes them, so they are not in this set; FirstInvisible checks them in context.
func IsInvisible(r rune) bool {
	return isZeroWidth(r) || isBidi(r) || isFormat(r) || r == 0xFEFF || isTag(r)
}

// FirstInvisible returns the first rune of s that Clean would have removed: a rune of the
// invisible set outside an RGI subdivision flag, or a variation selector that does not
// directly follow an emoji base. at is its byte offset. Cleaned text never has one.
func FirstInvisible(s string) (r rune, at int, found bool) {
	rs := []rune(s)
	offset := 0
	for i := 0; i < len(rs); i++ {
		if n := rgiFlagLen(rs, i); n > 0 {
			offset += len(string(rs[i : i+n]))
			i += n - 1
			continue
		}
		if IsInvisible(rs[i]) || (isVariationSelector(rs[i]) && !keptSelector(rs, i)) {
			return rs[i], offset, true
		}
		offset += utf8.RuneLen(rs[i])
	}
	return 0, 0, false
}

// rgiFlagLen returns the length in runes of an RGI subdivision flag starting at rs[i], or 0.
func rgiFlagLen(rs []rune, i int) int {
	if rs[i] != blackFlag || i+7 > len(rs) || rs[i+6] != cancelTag {
		return 0
	}
	for _, f := range rgiTagFlags {
		if string(rs[i+1:i+6]) == string(f) {
			return 7
		}
	}
	return 0
}

// cleanText removes invisible runes and folds whitespace. It returns the counts by category
// and the flagged count: a zero-width or BOM rune between two letters (splitting a word to
// dodge a scan) and every bidi override or isolate. A leading BOM, emoji joiners, RTL marks
// and soft hyphens are counted but not flagged.
func cleanText(s string) (string, InvisibleCounts, int) {
	rs := []rune(s)
	var c InvisibleCounts
	flagged := 0
	out := make([]rune, 0, len(rs))
	for i := 0; i < len(rs); i++ {
		if n := rgiFlagLen(rs, i); n > 0 {
			out = append(out, rs[i:i+n]...)
			i += n - 1
			continue
		}
		r := rs[i]
		switch {
		case isZeroWidth(r):
			c.ZeroWidth++
		case isBidi(r):
			c.Bidi++
		case isFormat(r), isVariationSelector(r) && !keptSelector(rs, i):
			c.Format++
		case r == 0xFEFF:
			c.BOM++
		case isTag(r):
			c.Tag++
		default:
			out = append(out, r)
			continue
		}
		if isBidiTamper(r) {
			flagged++
		} else if isZeroWidth(r) || r == 0xFEFF {
			if i > 0 && i+1 < len(rs) && unicode.IsLetter(rs[i-1]) && unicode.IsLetter(rs[i+1]) {
				flagged++
			}
		}
	}
	return fold(string(out)), c, flagged
}

var (
	foldSpaces   = regexp.MustCompile(`[ \t\x{a0}\f\v]+`)
	foldNewlines = regexp.MustCompile(` *\n *`)
	foldBlank    = regexp.MustCompile(`\n{3,}`)
)

// fold normalises line endings, collapses runs of spaces, trims spaces around newlines,
// keeps at most one blank line, and trims the ends.
func fold(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	s = foldSpaces.ReplaceAllString(s, " ")
	s = foldNewlines.ReplaceAllString(s, "\n")
	s = foldBlank.ReplaceAllString(s, "\n\n")
	return strings.TrimFunc(s, isPySpace)
}

// isPySpace is Python's str.isspace: Unicode white space plus the ASCII separators
// U+001C..U+001F.
func isPySpace(r rune) bool {
	return unicode.IsSpace(r) || (r >= 0x1C && r <= 0x1F)
}

// isWord is a Python \w rune: a letter, a number or an underscore.
func isWord(r rune) bool {
	return r == '_' || unicode.IsLetter(r) || unicode.IsNumber(r)
}

// splitLines is Python's str.splitlines.
func splitLines(s string) []string {
	var lines []string
	start := 0
	rs := []rune(s)
	for i := 0; i < len(rs); i++ {
		switch rs[i] {
		case '\n', '\v', '\f', 0x1C, 0x1D, 0x1E, 0x85, 0x2028, 0x2029:
			lines = append(lines, string(rs[start:i]))
			start = i + 1
		case '\r':
			lines = append(lines, string(rs[start:i]))
			if i+1 < len(rs) && rs[i+1] == '\n' {
				i++
			}
			start = i + 1
		}
	}
	if start < len(rs) {
		lines = append(lines, string(rs[start:]))
	}
	return lines
}
