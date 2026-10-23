package postingguard

import (
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// The instruction patterns are upstream's INSTRUCTION_PATTERNS. Upstream matches them with
// Python's Unicode-aware \b, \w and \s and with lookaheads; RE2 has ASCII classes and no
// lookahead. matchForm maps every non-ASCII word rune left after NFKC to the placeholder 'q'
// (a letter no pattern spells) and every non-ASCII space to '\f', so ASCII \b, \w and \s
// behave as Python's do, and it keeps each form rune's offset in the original line. Each lookahead is a check on the
// text after a marker group.

// instructionPattern is one pattern. A group named nh<k> must not be followed by a word rune
// or '-'; notAfter, when set, must not match at the end of the match.
type instructionPattern struct {
	re       *regexp.Regexp
	notAfter *regexp.Regexp
}

var aiTitle = regexp.MustCompile(`^(?i)[\s/&]+(?:[\w&/-]+\s+){0,3}?` +
	`(?:engineer|developer|researcher|scientist|architect|specialist|lead|consultant|manager|analyst|intern|` +
	`trainer|expert|strategist|designer|tester|owner|programmer)s?\b`)

var instructionPatterns = []instructionPattern{
	{re: regexp.MustCompile(`(?i)\bignore (all|any|the|previous|prior|above|earlier)\b`)},
	// The same instruction with its other two verbs, with an explicit object, because
	// "forget the" and "disregard the" are ordinary prose.
	{re: regexp.MustCompile(`(?i)\b(disregard|forget)( all| any| the| your| my)? (previous|prior|above|earlier|preceding|initial|original|system) (instructions?|prompts?)\b`)},
	{re: regexp.MustCompile(`(?i)\byou are (?:an?|the) (?:ai(?P<nh1>)|ai (?:assistant|agent|model|language model)|assistant|llm|language model)\b`)},
	{re: regexp.MustCompile(`(?i)\bsystem prompt\b`)},
	// The verb straight onto "the user" (not "the user's"); kept narrow because "never
	// disclose user data" is everyday privacy prose.
	{re: regexp.MustCompile(`(?i)\b(do not|don['\x{2019}]?t|never) (tell|inform) the user\b`), notAfter: regexp.MustCompile(`^(?i)['\x{2019}]s`)},
	{re: regexp.MustCompile(`(?i)\b(do not|don['\x{2019}]?t|never) (mention|reveal|disclose) (this|these|that|it|any of this|the (above|following|previous))\b[^.\n]{0,30}\bto the user\b`)},
	{re: regexp.MustCompile(`(?i)\b(run|execute) the following (command|commands|script|code|shell|instruction|instructions|steps)\b`)},
	// "As an AI" flags unless a job-title noun follows within a few words ("As an AI
	// Engineer"), the way AI job ads introduce the role.
	{re: regexp.MustCompile(`(?i)\bas an ai(?P<nh1>)`), notAfter: aiTitle},
	{re: regexp.MustCompile(`(?i)^\s*(assistant|system|user)\s*:`)},
	{re: regexp.MustCompile(`<\|`)},
	{re: regexp.MustCompile(`(?i)\[INST\]`)},
	{re: regexp.MustCompile(`(?i)###\s*(system|instruction)`)},
	{re: regexp.MustCompile(`(?i)\b(curl|wget|ssh|scp)\b`)},
	{re: regexp.MustCompile(`(?i)\.env\b`)},
	{re: regexp.MustCompile(`(?i)settings\.json`)},
	{re: regexp.MustCompile(`(?i)allowed-tools`)},
	{re: regexp.MustCompile(`(?i)\btool_use\b|\bfunction_call\b`)},
}

// matchForm maps s into the form the patterns run on (see above) and returns, for each rune
// of that form, the rune offset in s it came from. Python's IGNORECASE also matches dotless
// and dotted i, long s and the Kelvin sign to their ASCII letters, so those map to the
// letter rather than the placeholder.
//
// Deliberate departure from upstream: every other non-ASCII rune is first replaced by its
// NFKC form, so fullwidth and mathematical letters ("ｉｇｎｏｒｅ", "𝐢𝐠𝐧𝐨𝐫𝐞") spell the
// same instruction to the patterns that they spell to a reader. Only the matching form is
// normalised; the visible text keeps the original runes. A rune whose NFKC form is several
// runes (a ligature) contributes all of them, each mapped back to that one rune.
func matchForm(s string) (string, []int) {
	var b strings.Builder
	b.Grow(len(s))
	orig := make([]int, 0, len(s))
	put := func(r rune, i int) {
		switch {
		case r == '\v' || (r >= 0x1C && r <= 0x1F):
			b.WriteByte('\f')
		case r < utf8.RuneSelf:
			b.WriteRune(r)
		case r == 0x0130 || r == 0x0131:
			b.WriteByte('i')
		case r == 0x017F:
			b.WriteByte('s')
		case r == 0x212A:
			b.WriteByte('k')
		case isWord(r):
			b.WriteByte('q')
		case isPySpace(r):
			b.WriteByte('\f')
		default:
			b.WriteRune(r)
		}
		orig = append(orig, i)
	}
	i := 0
	for _, r := range s {
		switch {
		case r < utf8.RuneSelf, r == 0x0130, r == 0x0131, r == 0x017F, r == 0x212A:
			put(r, i)
		default:
			for _, n := range norm.NFKC.String(string(r)) {
				put(n, i)
			}
		}
		i++
	}
	return b.String(), orig
}

// find returns the rune offset in the original line of the first match in m, the line's
// matching form, or -1.
func (p instructionPattern) find(m string, orig []int) int {
	names := p.re.SubexpNames()
	for _, loc := range p.re.FindAllStringSubmatchIndex(m, -1) {
		if p.accept(m, loc, names) {
			return orig[utf8.RuneCountInString(m[:loc[0]])]
		}
	}
	return -1
}

func (p instructionPattern) accept(m string, loc []int, names []string) bool {
	for i, name := range names {
		if !strings.HasPrefix(name, "nh") || loc[2*i] < 0 {
			continue
		}
		if at := loc[2*i]; at < len(m) {
			r, _ := utf8.DecodeRuneInString(m[at:])
			if r == '-' || isWord(r) {
				return false
			}
		}
	}
	return p.notAfter == nil || !p.notAfter.MatchString(m[loc[1]:])
}

// instructionAt returns the rune offset of the first instruction-shaped match in line, or -1.
func instructionAt(line string) int {
	m, orig := matchForm(line)
	for _, p := range instructionPatterns {
		if at := p.find(m, orig); at >= 0 {
			return at
		}
	}
	return -1
}

// urlPattern is upstream's URL with Python's Unicode \s spelled out.
var urlPattern = regexp.MustCompile(`https?://[^\t\n\v\f\r \x{1c}-\x{1f}\x{85}\p{Z}<>"')\]]+`)

func isLatin(r rune) bool      { return (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') }
func isConfusable(r rune) bool { return (r >= 0x0400 && r <= 0x04FF) || (r >= 0x0370 && r <= 0x03FF) }

// excerpt is up to lineMax runes of line, always including rune offset at.
func excerpt(line string, at int) string {
	rs := []rune(line)
	if len(rs) <= lineMax {
		return line
	}
	start := max(0, at-lineMax/3)
	end := min(len(rs), start+lineMax)
	start = max(0, end-lineMax)
	head, tail := "", ""
	if start > 0 {
		head = "\u2026"
	}
	if end < len(rs) {
		tail = "\u2026"
	}
	return head + string(rs[start:end]) + tail
}

// scan finds instruction-shaped lines, hidden URLs, the visible URL count and mixed-script
// words in the cleaned visible and hidden text.
func scan(visible, hidden string) Findings {
	var f Findings
	seen := map[string]bool{}
	for _, line := range append(splitLines(visible), splitLines(hidden)...) {
		s := strings.TrimFunc(line, isPySpace)
		if s == "" || seen[s] {
			continue
		}
		if at := instructionAt(s); at >= 0 {
			seen[s] = true
			f.InstructionLike = append(f.InstructionLike, excerpt(s, at))
		}
	}
	for _, u := range urlPattern.FindAllString(hidden, -1) {
		if !slices.Contains(f.HiddenURLs, u) {
			f.HiddenURLs = append(f.HiddenURLs, u)
		}
	}
	f.VisibleURLCount = len(urlPattern.FindAllString(visible, -1))
	for _, w := range words(visible + "\n" + hidden) {
		if strings.IndexFunc(w, isLatin) >= 0 && strings.IndexFunc(w, isConfusable) >= 0 && !slices.Contains(f.HomoglyphRuns, w) {
			f.HomoglyphRuns = append(f.HomoglyphRuns, w)
		}
	}
	return f
}

// words splits s into runs of word runes (Python's \w+).
func words(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool { return !isWord(r) })
}

// quoteLines prefixes every instruction-shaped line of the cleaned text with QuotePrefix.
func quoteLines(text string) string {
	lines := strings.Split(text, "\n")
	for i, l := range lines {
		if s := strings.TrimFunc(l, isPySpace); s != "" && instructionAt(s) >= 0 {
			lines[i] = QuotePrefix + l
		}
	}
	return strings.Join(lines, "\n")
}
