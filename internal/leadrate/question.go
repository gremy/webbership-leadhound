// Package leadrate asks the decider one question about a lead, how relevant it is to a
// freelance Drupal upgrade offer, and measures that answer against a hand-labelled golden
// set.
//
// The question is relevance@1: one choice among five bands, strong, likely, maybe,
// unlikely and no, weighted 100, 75, 50, 25 and 0. A lead's relevance is the weighted sum
// of the band probabilities, rounded, so it runs 0 to 100. The state the decider reads is
// built deterministically from the lead's facts (State) and never carries an email address
// or a phone number. A lead is asked again only when InputHash changes.
package leadrate

import (
	"github.com/gremy/webbership-leadhound/internal/ai"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/scoring"
)

// Question names this question and its wording, as lead_relevance.question stores it.
// Changing the wording or the options means a new version here.
const Question = "relevance@1"

// questionName is the ai.Question name the decider answers under: a plain identifier like
// the job criteria's, since the provider keys its answers by it.
const questionName = "relevance"

// band is one option of the question: its name, weight and plain description.
type band struct {
	name        string
	weight      int
	description string
}

// bands are the options from the best band to the worst; the order is the scale that
// within-one-band and the tie-break in Score use.
var bands = []band{
	{leadstore.BandStrong, 100, "A real business or organisation that clearly relies on this Drupal site and would likely pay for an upgrade or a move to a supported version; for an agency, one that would likely hand such work to a contractor."},
	{leadstore.BandLikely, 75, "Probably a business or organisation that would pay for Drupal upgrade work, though something about it is unclear."},
	{leadstore.BandMaybe, 50, "Could go either way: too little is known about who runs the site or whether they would pay for the work."},
	{leadstore.BandUnlikely, 25, "Probably not worth contacting: a hobby, tiny, abandoned or placeholder site, or one with no clear way to reach its owner."},
	{leadstore.BandNo, 0, "Not a lead: nobody here could buy Drupal work, such as a test page, a directory listing or a spam site."},
}

// instructions is the question the decider is asked about the state.
const instructions = "Which description fits this website best as a lead for a freelance Drupal developer who upgrades sites off unsupported Drupal versions?"

// AIQuestion is the relevance question as the decider is asked it: one choice among the
// bands, each with its plain description.
func AIQuestion() ai.Question {
	opts := make([]ai.Option, len(bands))
	for i, b := range bands {
		opts[i] = ai.Option{Name: b.name, Description: b.description}
	}
	return ai.Question{Name: questionName, Instructions: instructions, Options: opts}
}

// Bands returns the band names from the best to the worst.
func Bands() []string {
	out := make([]string, len(bands))
	for i, b := range bands {
		out[i] = b.name
	}
	return out
}

// Weight returns band's weight (0 to 100); ok is false for a name that is not a band.
func Weight(name string) (weight int, ok bool) {
	i := bandIndex(name)
	if i < 0 {
		return 0, false
	}
	return bands[i].weight, true
}

// bandIndex is name's place on the scale, best first, or -1.
func bandIndex(name string) int {
	for i, b := range bands {
		if b.name == name {
			return i
		}
	}
	return -1
}

// InputHash is the hash of everything a rating depends on: the decider's name, the
// question with its instructions and every option's name and description, the state, and
// the question's version (Question). It is scoring.InputHash's length-prefixed encoding,
// with the version in its keyword slot, so a changed lead, wording or decider asks again
// and an unchanged one does not.
func InputHash(decider string, in Input) []byte {
	return scoring.InputHash(decider, []ai.Question{AIQuestion()}, State(in), 0,
		map[string][]string{"question": {Question}})
}

// Describe returns band name's plain description, as the decider reads it; "" for a name
// that is not a band.
func Describe(name string) string {
	if i := bandIndex(name); i >= 0 {
		return bands[i].description
	}
	return ""
}
