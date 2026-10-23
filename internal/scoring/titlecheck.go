package scoring

import (
	"crypto/sha256"
	"fmt"
	"strings"

	"github.com/gremy/webbership-leadhound/internal/ai"
	"github.com/gremy/webbership-leadhound/internal/candidate"
	"github.com/gremy/webbership-leadhound/internal/store"
)

// TitleCheckName is the title-only question's name. It differs from the full title
// question's ("title") so a scripted decider, and anyone reading a request, can tell the
// two apart; its options are the title criterion's own.
const TitleCheckName = "title_check"

// Reason markers for a title decision made before full scoring.
const (
	titleCheckNote = " (title-only check, before full scoring)"
	titleDropNote  = " (dropped by the title screen in code, no call made)"
)

// titleCriterion returns the criterion named "title", if the scorer has one.
func titleCriterion(cs []candidate.NamedCriterion) (candidate.NamedCriterion, bool) {
	for _, c := range cs {
		if c.Name == "title" {
			return c, true
		}
	}
	return candidate.NamedCriterion{}, false
}

// TitleQuestion is the title-only question: the title criterion's pass, fail and unclear
// descriptions, asked about the title alone.
func TitleQuestion(c candidate.NamedCriterion) ai.Question {
	return ai.Question{
		Name:         TitleCheckName,
		Instructions: "Which description fits this job title best?",
		Options: []ai.Option{
			{Name: Pass, Description: c.Pass},
			{Name: Fail, Description: c.Fail},
			{Name: Unclear, Description: c.Unclear},
		},
	}
}

// TitleState is the title-only call's whole state: the title and nothing else, so the
// call is the cheapest one the decider takes.
func TitleState(title string) string {
	return "Title: " + title
}

// ScreenHash folds the title screen's verdict and the title-only question into a decision
// hash, so a changed screen or a changed title criterion asks again. A kept title's hash is
// left as InputHash gives it.
func ScreenHash(h []byte, screen candidate.TitleVerdict, tq ai.Question) []byte {
	sum := sha256.New()
	field := func(s string) {
		_, _ = fmt.Fprintf(sum, "%d:%s", len(s), s)
	}
	field(string(h))
	field("title_screen")
	field(screen.String())
	field(tq.Name)
	field(tq.Instructions)
	for _, o := range tq.Options {
		field(o.Name)
		field(o.Description)
	}
	return sum.Sum(nil)
}

// TitleCheckDecision is the title decision a failed title-only check records: the title
// criterion failing, with a reason that says where it came from.
func TitleCheckDecision(in store.ScoreInput, c candidate.NamedCriterion, a ai.Answer, threshold float64, decider string, hash []byte) store.DecisionInsert {
	reason := Reason(c, a, Fail, threshold, Evidence{Title: in.Title})
	head, rest, _ := strings.Cut(reason, "\n")
	return store.DecisionInsert{
		JobID: in.JobID, ObservationID: in.ObservationID, Decider: decider,
		Criterion: c.Name, Verdict: Fail, Confidence: a.Confidence, Probabilities: a.Probabilities,
		Reason: head + titleCheckNote + "\n" + rest, InputSHA256: hash,
	}
}

// DropDecision is the title decision for a stored job whose title the screen drops. The
// collector never stores one, but if it is there it fails on the title without a call.
func DropDecision(in store.ScoreInput, c candidate.NamedCriterion, decider string, hash []byte) store.DecisionInsert {
	lines := []string{fmt.Sprintf("%s: %s%s", c.Name, c.Fail, titleDropNote)}
	if t := strings.TrimSpace(in.Title); t != "" {
		lines = append(lines, `"`+firstRunes(t, maxEvidenceRunes)+`"`)
	}
	return store.DecisionInsert{
		JobID: in.JobID, ObservationID: in.ObservationID, Decider: decider,
		Criterion: c.Name, Verdict: Fail, Confidence: 1, Probabilities: map[string]float64{},
		Reason: strings.Join(lines, "\n"), InputSHA256: hash,
	}
}
