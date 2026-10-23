package scoring

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/gremy/webbership-leadhound/internal/ai"
	"github.com/gremy/webbership-leadhound/internal/candidate"
	"github.com/gremy/webbership-leadhound/internal/gates"
	"github.com/gremy/webbership-leadhound/internal/store"
)

const (
	// maxEvidence is how many evidence lines a reason quotes.
	maxEvidence = 2
	// maxEvidenceRunes is the longest evidence line, ellipses included.
	maxEvidenceRunes = 120
	// NoEvidence stands in when no line of the posting backs the verdict.
	NoEvidence = "no evidence line found"
)

// criterionGates are the gates whose stored snippet backs a criterion that has no
// evidence keywords, in the order they are tried.
var criterionGates = map[string][]string{
	"location":    {gates.GateLocation, gates.GateWorkplace},
	"work_rights": {gates.GateWorkRights},
}

// Evidence is what a reason may quote: the cleaned posting and its gate snippets.
type Evidence struct {
	Title       string
	Description string // the cleaned description, one block per line
	Gates       []store.GateResult
	Keywords    map[string][]string // per criterion
}

// Reason builds a decision's reason in code, one part per line:
//
//	stack: <the chosen option's description>
//	"<an evidence line>"            (up to two, or "no evidence line found")
//	p(pass)=0.82 p(fail)=0.11 …     (highest first, then by name)
//
// When a low confidence turned the choice into unclear, the first line says so.
func Reason(c candidate.NamedCriterion, a ai.Answer, verdict string, threshold float64, ev Evidence) string {
	var lines []string
	head := fmt.Sprintf("%s: %s", c.Name, optionDescription(c, a.Choice))
	if verdict == Unclear && a.Choice != Unclear {
		head += fmt.Sprintf(" (confidence %.2f, below %.2f, so unclear)", a.Confidence, threshold)
	}
	lines = append(lines, head)
	found := EvidenceLines(c.Name, ev)
	if len(found) == 0 {
		lines = append(lines, NoEvidence)
	}
	for _, l := range found {
		lines = append(lines, `"`+l+`"`)
	}
	lines = append(lines, Probabilities(a.Probabilities))
	return strings.Join(lines, "\n")
}

// optionDescription is the criterion's description of the chosen option.
func optionDescription(c candidate.NamedCriterion, choice string) string {
	switch choice {
	case Pass:
		return c.Pass
	case Fail:
		return c.Fail
	case Unclear:
		return c.Unclear
	}
	return "no option chosen"
}

// Probabilities formats p(option)=0.00 for every option, highest first, ties by name.
func Probabilities(p map[string]float64) string {
	names := make([]string, 0, len(p))
	for n := range p {
		names = append(names, n)
	}
	slices.SortFunc(names, func(a, b string) int {
		switch {
		case p[a] > p[b]:
			return -1
		case p[a] < p[b]:
			return 1
		}
		return strings.Compare(a, b)
	})
	parts := make([]string, len(names))
	for i, n := range names {
		parts[i] = fmt.Sprintf("p(%s)=%.2f", n, p[n])
	}
	return strings.Join(parts, " ")
}

// EvidenceLines finds up to two lines that back criterion. A criterion with evidence
// keywords quotes the description's lines that name one (whole words, any case); one
// without quotes its gate's stored snippet (location, work rights) or the title (title).
// Every line is cut to 120 characters around what matched.
func EvidenceLines(criterion string, ev Evidence) []string {
	if kws := ev.Keywords[criterion]; len(kws) > 0 {
		return keywordLines(ev.Description, kws)
	}
	if criterion == "title" && strings.TrimSpace(ev.Title) != "" {
		return []string{firstRunes(ev.Title, maxEvidenceRunes)}
	}
	for _, gate := range criterionGates[criterion] {
		for _, g := range ev.Gates {
			if g.Gate == gate && strings.TrimSpace(g.Snippet) != "" {
				return []string{firstRunes(g.Snippet, maxEvidenceRunes)}
			}
		}
	}
	return nil
}

// keywordLines returns the first two distinct description lines naming any keyword.
func keywordLines(description string, keywords []string) []string {
	re := keywordRE(keywords)
	var out []string
	for _, line := range strings.Split(description, "\n") {
		line = strings.Join(strings.Fields(line), " ")
		m := re.FindStringSubmatchIndex(line)
		if m == nil {
			continue
		}
		cut := window(line, m[2], m[3], maxEvidenceRunes)
		if slices.Contains(out, cut) {
			continue
		}
		out = append(out, cut)
		if len(out) == maxEvidence {
			break
		}
	}
	return out
}

// keywordCache holds the compiled pattern per keyword list.
var keywordCache sync.Map // string → *regexp.Regexp

// keywordRE matches any keyword as a whole word, case-insensitively; group 1 is the
// keyword itself.
func keywordRE(keywords []string) *regexp.Regexp {
	key := strings.Join(keywords, "\x00")
	if v, ok := keywordCache.Load(key); ok {
		return v.(*regexp.Regexp)
	}
	alts := make([]string, len(keywords))
	for i, k := range keywords {
		alts[i] = regexp.QuoteMeta(strings.TrimSpace(k))
	}
	re := regexp.MustCompile(`(?i)(?:^|[^\pL\pN])(` + strings.Join(alts, "|") + `)(?:$|[^\pL\pN])`)
	keywordCache.Store(key, re)
	return re
}

// window cuts line to at most n runes around the byte range [start, end), marking each
// cut end with an ellipsis.
func window(line string, start, end, n int) string {
	if utf8.RuneCountInString(line) <= n {
		return line
	}
	runes := []rune(line)
	rs := utf8.RuneCountInString(line[:start])
	re := rs + utf8.RuneCountInString(line[start:end])
	budget := n - 2 // room for both ellipses
	from := rs - (budget-(re-rs))/2
	from = max(from, 0)
	to := min(from+budget, len(runes))
	from = max(to-budget, 0)
	s := string(runes[from:to])
	if from > 0 {
		s = "…" + s
	}
	if to < len(runes) {
		s += "…"
	}
	return s
}
