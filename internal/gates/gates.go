// Package gates decides, from a posting's plain text, whether it can be ruled out before
// any model reads it: stored rules first, then language, location, workplace and work
// rights. Every result carries a plain-sentence reason and a snippet of the text that
// decided it, for the brief and as evidence for the scorer.
//
// The input is already-cleaned plain text (postingguard's output). Every pattern is RE2.
package gates

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// DerivedBy is written to gate_result.derived_by.
const DerivedBy = "gates@1"

// Gate names, as gate_result.gate stores them.
const (
	GateLanguage   = "language"
	GateLocation   = "location"
	GateWorkRights = "work_rights"
	GateWorkplace  = "workplace"
	GateRule       = "rule"
)

// Verdicts, as gate_result.verdict stores them.
const (
	Pass = "pass"
	Fail = "fail"
	Flag = "flag"
)

// Rule kinds.
const (
	KindSkip = "skip"
	KindFlag = "flag"
)

// maxSnippet is the longest snippet a result carries, in runes.
const maxSnippet = 120

// Observation is the plain text of one posting.
type Observation struct {
	Title       string
	Company     string
	Location    string
	Description string
	// Lang and LangConfidence carry a detection the caller already made with
	// DetectLanguage over the same text the language gate reads (the description, or the
	// title when the description is blank). When Lang is "", the gate detects it itself.
	Lang           string
	LangConfidence float64
	// ListedRemote is set when the source lists remote jobs only (LinkedIn's guest search
	// is filtered to remote), so a location field there names where the job is posted from,
	// not an office. It is not read as a remote signal: it only stops a bare location field
	// (places, no workplace) being read as an office's address.
	ListedRemote bool
}

// CandidateGates is the part of the candidate profile the gates read.
type CandidateGates struct {
	Languages        []string // ISO 639-1 codes, e.g. "en", "ro"
	AllowedCountries []string // country names, e.g. "Romania"
	// AllowedCities are cities the candidate can be based in or travel to, e.g. a home
	// city: a residence requirement naming one passes, and office days there never fail.
	AllowedCities []string
	RemoteOnly    bool
	// WorkRights is the candidate's own statement, e.g. "EU citizen". When it is set it
	// alone decides whether they hold EU work rights; when it is blank that is inferred
	// from an allowed EU country.
	WorkRights string
}

// Rule is one active stored rule.
type Rule struct {
	ID      int64
	Kind    string // KindSkip or KindFlag
	Field   string // company, title, description or location
	Pattern string // RE2, matched case-insensitively
	Reason  string
}

// Result is one gate verdict.
type Result struct {
	Gate    string
	Verdict string
	Reason  string
	Snippet string // at most 120 runes, from the text
	RuleID  *int64 // set when a stored rule decided it
}

// Evaluate runs the rules, then the language gate, then (unless the language failed) the
// location, workplace and work-rights gates. Each of those four gates gives exactly one
// result when it runs; each matching rule gives one result.
func Evaluate(obs Observation, c CandidateGates, rules []Rule) []Result {
	out := evalRules(obs, rules)
	lang := evalLanguage(obs, c)
	out = append(out, lang)
	if lang.Verdict == Fail {
		// The remaining patterns are English; on a posting in another language a pass
		// would only mean they could not read it.
		return out
	}
	return append(out,
		evalLocation(obs, c),
		evalWorkplace(obs, c),
		evalWorkRights(obs, c),
	)
}

// finding is a candidate result inside one gate; the most severe one wins.
type finding struct {
	verdict string
	reason  string
	snippet string
	pos     int
}

func severity(v string) int {
	switch v {
	case Fail:
		return 2
	case Flag:
		return 1
	}
	return 0
}

// worst picks the most severe finding, the earliest in the text among equals.
func worst(fs []finding) (finding, bool) {
	if len(fs) == 0 {
		return finding{}, false
	}
	best := fs[0]
	for _, f := range fs[1:] {
		if severity(f.verdict) > severity(best.verdict) || (severity(f.verdict) == severity(best.verdict) && f.pos < best.pos) {
			best = f
		}
	}
	return best, true
}

func result(gate string, f finding) Result {
	return Result{Gate: gate, Verdict: f.verdict, Reason: f.reason, Snippet: f.snippet}
}

// snippet returns the text around text[start:end], whitespace collapsed, at most
// maxSnippet runes, keeping the match itself whole where it fits.
func snippet(text string, start, end int) string {
	match := squash(text[start:end])
	n := utf8.RuneCountInString(match)
	if n >= maxSnippet {
		return truncate(match, maxSnippet)
	}
	room := maxSnippet - n
	before := lastRunes(squash(text[:start]), room/2)
	after := firstRunes(squash(text[end:]), room-utf8.RuneCountInString(before))
	s := match
	if before != "" && start > 0 && isSpace(text[start-1]) {
		s = before + " " + s
	} else {
		s = before + s
	}
	if after != "" && end < len(text) && isSpace(text[end]) {
		s += " " + after
	} else {
		s += after
	}
	return truncate(strings.TrimSpace(s), maxSnippet)
}

func isSpace(b byte) bool { return b == ' ' || b == '\n' || b == '\t' || b == '\r' }

func squash(s string) string { return strings.Join(strings.Fields(s), " ") }

func truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n])
}

func lastRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[len(r)-n:])
}

func firstRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	return truncate(s, n)
}

// opening is the snippet for a gate that matched nothing: the text's first runes.
func opening(text string) string {
	return truncate(squash(text), maxSnippet)
}

// negationWindow is how many words before a match a negation word may sit: the negation
// word itself and at most two words between, as in "not a fully remote role".
const negationWindow = 3

var negationWords = map[string]bool{"no": true, "not": true, "without": true, "non": true}

// negated reports whether a negation word sits just before text[pos:], in the same
// clause and within negationWindow words, e.g. "no security clearance", "not a fully
// remote role"; "No meetings culture, fully remote team" is not negated.
func negated(text string, pos int) bool {
	lo := pos - 48
	if lo < 0 {
		lo = 0
	}
	for lo > 0 && !utf8.RuneStart(text[lo]) {
		lo--
	}
	window := text[lo:pos]
	if i := strings.LastIndexAny(window, ",:;.!?\n"); i >= 0 {
		window = window[i+1:]
	} else if lo > 0 {
		// The window may start inside a word ("casi|no"); drop that partial word.
		r, _ := utf8.DecodeLastRuneInString(text[:lo])
		if unicode.IsLetter(r) {
			if j := strings.IndexFunc(window, func(r rune) bool { return !unicode.IsLetter(r) }); j >= 0 {
				window = window[j:]
			} else {
				window = ""
			}
		}
	}
	words := strings.FieldsFunc(window, func(r rune) bool { return !unicode.IsLetter(r) && r != '\'' })
	if len(words) > negationWindow {
		words = words[len(words)-negationWindow:]
	}
	for _, w := range words {
		if negationWords[strings.ToLower(w)] {
			return true
		}
	}
	return false
}

// notRequiredRE says a requirement just named is not one: "is not required", "not
// needed", "is not a requirement".
var notRequiredRE = regexp.MustCompile(`(?i)^\s*(?:is\s+|are\s+)?(?:not|n't|isn't|aren't)\s+(?:required|needed|necessary|mandatory|a\s+requirement)\b`)

// negatedAfter reports whether the words right after text[:end] say the requirement is
// not one.
func negatedAfter(text string, end int) bool {
	return notRequiredRE.MatchString(text[end:])
}
