// Package postingguard is the integrity gate a fetched job posting passes before anything
// reads it. It is a Go port of upstream ai-job-search's posting_guard.py (MIT, see NOTICE).
//
// A posting is third-party text that is later read next to the candidate's own data, so the
// payload classes a reader cannot see are made visible deterministically:
//
//   - hidden HTML (display:none, visibility:hidden or collapse, opacity:0, font-size:0 or
//     at most 1px, a transparent or zero-alpha colour, clip, clip-path, scale(0), off-screen
//     positioning, a collapsed box with overflow:hidden, the hidden attribute,
//     aria-hidden=true, a <dialog> without open) is removed from the text and reported;
//     comments and script, style, noscript, svg, template, iframe, noembed, noframes and
//     title are removed and scanned but not listed;
//   - invisible characters (zero-width, bidi controls, soft hyphen, line and paragraph
//     separators and the other format runes of isFormat, BOM, Unicode tag characters, and
//     variation selectors not directly after an emoji) are removed and counted, keeping the
//     three RGI subdivision flag emoji intact;
//   - instruction-shaped lines are kept but marked with QuotePrefix; the patterns match an
//     NFKC form of the line, so fullwidth and mathematical letters cannot dodge them;
//   - URLs that only exist in hidden content, and words that mix Latin with Cyrillic or Greek
//     letters, are listed.
//
// It advises; it never blocks. Findings carry excerpts for the caller's memory only; what is
// stored is CountsJSON, which holds counts and never the removed text.
package postingguard

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"
)

// QuotePrefix marks an instruction-shaped line in the cleaned text: the line stays readable
// but is visibly a quotation of the posting, not something to act on.
const QuotePrefix = "> [quoted] "

const (
	excerptMax = 120 // runes kept of a hidden block
	lineMax    = 200 // runes kept of an instruction-shaped line
)

// InvisibleCounts counts the invisible runes removed, by category.
type InvisibleCounts struct {
	ZeroWidth int `json:"zero_width"`
	Bidi      int `json:"bidi"`
	Format    int `json:"format"` // soft hyphen, separators, fillers, stray variation selectors
	BOM       int `json:"bom"`
	Tag       int `json:"tag"` // Unicode tag characters outside the RGI subdivision flags
}

// Findings is what the guard found. The string slices are excerpts for the caller to show
// or log in memory; they are never stored (see CountsJSON).
type Findings struct {
	HiddenBlocks     []string // excerpts of removed hidden elements
	InstructionLike  []string // instruction-shaped lines, visible or hidden
	HiddenURLs       []string // URLs that appear only in hidden content
	HomoglyphRuns    []string // words mixing Latin with Cyrillic or Greek letters
	VisibleURLCount  int
	Invisible        InvisibleCounts
	InvisibleFlagged int // zero-width or BOM inside a word, and every bidi override or isolate
}

// Result is the cleaned, visible text and the findings.
type Result struct {
	Visible  string
	Findings Findings
}

// Clean runs the whole gate over a posting. With isHTML the input is markup and only what a
// browser would show is kept, with block elements on their own lines; otherwise it is plain
// text. Invalid UTF-8 is replaced with U+FFFD; use DecodePage first for raw page bytes.
func Clean(htmlOrText string, isHTML bool) Result {
	in := validUTF8(htmlOrText)
	text, hidden := in, ""
	var blocks []string
	if isHTML {
		text, hidden, blocks = cleanHTML(in)
	}
	text, counts, flagged := cleanText(text)
	hidden, _, _ = cleanText(hidden)
	f := scan(text, hidden)
	f.HiddenBlocks = blocks
	f.Invisible = counts
	f.InvisibleFlagged = flagged
	return Result{Visible: quoteLines(text), Findings: f}
}

// CleanField cleans a one-line field such as a title, company or location: invalid UTF-8
// is replaced, invisible runes are removed and whitespace, line breaks included, folds to
// single spaces. Unlike Clean it never adds QuotePrefix, because a field is a value, not a
// line of prose; instruction quoting belongs to the description. A field is plain text, so
// there is no hidden markup to remove.
func CleanField(s string) string {
	text, _, _ := cleanText(strings.Join(strings.Fields(validUTF8(s)), " "))
	return strings.Join(strings.Fields(text), " ")
}

// validUTF8 replaces each byte that does not start a valid UTF-8 sequence with U+FFFD.
func validUTF8(s string) string {
	if utf8.ValidString(s) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		b.WriteRune(r) // ranging yields utf8.RuneError for each invalid byte
	}
	return b.String()
}

type counts struct {
	HiddenBlocks     int             `json:"hidden_blocks"`
	InstructionLike  int             `json:"instruction_like"`
	HiddenURLs       int             `json:"hidden_urls"`
	HomoglyphRuns    int             `json:"homoglyph_runs"`
	VisibleURLCount  int             `json:"visible_url_count"`
	InvisibleChars   InvisibleCounts `json:"invisible_chars"`
	InvisibleFlagged int             `json:"invisible_flagged"`
}

// CountsJSON is the stored form of the findings (the guard_findings column): counts only,
// in a fixed shape, never an excerpt.
func (f Findings) CountsJSON() json.RawMessage {
	b, err := json.Marshal(counts{
		HiddenBlocks:     len(f.HiddenBlocks),
		InstructionLike:  len(f.InstructionLike),
		HiddenURLs:       len(f.HiddenURLs),
		HomoglyphRuns:    len(f.HomoglyphRuns),
		VisibleURLCount:  f.VisibleURLCount,
		InvisibleChars:   f.Invisible,
		InvisibleFlagged: f.InvisibleFlagged,
	})
	if err != nil { // a struct of ints always marshals
		panic(fmt.Sprintf("postingguard: counts json: %v", err))
	}
	return b
}

// Alarm reports a signal that indicates targeting: an instruction-shaped line, a tampering
// character, a tag character or a mixed-script word. Hidden blocks and hidden URLs alone are
// context (navigation chrome is hidden on nearly every page) and do not alarm.
func (f Findings) Alarm() bool {
	return len(f.InstructionLike) > 0 || f.InvisibleFlagged > 0 || f.Invisible.Tag > 0 || len(f.HomoglyphRuns) > 0
}

// Verdict is upstream's one-line summary, word for word.
func (f Findings) Verdict() string {
	var alarms []string
	if n := len(f.InstructionLike); n > 0 {
		alarms = append(alarms, plural(n, "instruction-like line", "instruction-like lines"))
	}
	if n := f.InvisibleFlagged; n > 0 {
		alarms = append(alarms, plural(n, "invisible character inside words", "invisible characters inside words"))
	}
	if n := f.Invisible.Tag; n > 0 {
		alarms = append(alarms, plural(n, "tag character (text hidden from the reader)", "tag characters (text hidden from the reader)"))
	}
	if n := len(f.HomoglyphRuns); n > 0 {
		alarms = append(alarms, plural(n, "mixed-script word", "mixed-script words"))
	}
	var context []string
	if n := len(f.HiddenBlocks); n > 0 {
		context = append(context, plural(n, "hidden block", "hidden blocks"))
	}
	if n := len(f.HiddenURLs); n > 0 {
		context = append(context, plural(n, "hidden URL", "hidden URLs"))
	}
	removed := strings.Join(context, " and ")
	if len(alarms) > 0 {
		line := "posting_guard: \u26a0 " + strings.Join(alarms, ", ") + " - review before continuing"
		if removed != "" {
			return line + " (" + removed + " removed)"
		}
		return line
	}
	if removed != "" {
		return "posting_guard: clean (" + removed + " removed, none instruction-like)"
	}
	return "posting_guard: clean"
}

func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}
