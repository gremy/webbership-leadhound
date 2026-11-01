// Package leadrate asks the decider one question about a lead, how relevant it is to the
// offer its rating type stands for, and measures that answer against a hand-labelled golden
// set.
//
// Each rating type (leadstore.TypeOf the lead's kind) has its own question: relevance@1 for
// a Drupal lead (a freelance Drupal upgrade offer) and relevance-cafe@1 for a café lead (a
// digital loyalty card kept in the phone wallet). Each is one choice among the same five
// bands, strong, likely, maybe, unlikely and no, weighted 100, 75, 50, 25 and 0, each with
// its type's own description. A lead's relevance is the weighted sum
// of the band probabilities, rounded, so it runs 0 to 100. The state the decider reads is
// built deterministically from the lead's facts (State) and never carries an email address
// or a phone number. A lead is asked again only when InputHash changes.
package leadrate

import (
	"github.com/gremy/webbership-leadhound/internal/ai"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/scoring"
)

// Question names the Drupal question and its wording, as lead_relevance.question stores
// it. Changing the wording or the options means a new version here.
const Question = leadstore.QuestionDrupal

// questionName is the ai.Question name the decider answers under: a plain identifier like
// the job criteria's, since the provider keys its answers by it.
const questionName = "relevance"

// band is one option of the question: its name, weight and plain description.
type band struct {
	name        string
	weight      int
	description string
}

// rtype is one rating type's question: its stored name, its instructions and its bands.
type rtype struct {
	question     string
	instructions string
	bands        []band
}

// bands are the Drupal question's options from the best band to the worst; the order is
// the scale that within-one-band and the tie-break in Score use. Every type's bands carry
// these names and weights in this order.
var bands = []band{
	{leadstore.BandStrong, 100, "A real business or organisation that clearly relies on this Drupal site and would likely pay for an upgrade or a move to a supported version; for an agency, one that would likely hand such work to a contractor."},
	{leadstore.BandLikely, 75, "Probably a business or organisation that would pay for Drupal upgrade work, though something about it is unclear."},
	{leadstore.BandMaybe, 50, "Could go either way: too little is known about who runs the site or whether they would pay for the work."},
	{leadstore.BandUnlikely, 25, "Probably not worth contacting: a hobby, tiny, abandoned or placeholder site, or one with no clear way to reach its owner."},
	{leadstore.BandNo, 0, "Not a lead: nobody here could buy Drupal work, such as a test page, a directory listing or a spam site."},
}

// instructions is the Drupal question the decider is asked about the state.
const instructions = "Which description fits this website best as a lead for a freelance Drupal developer who upgrades sites off unsupported Drupal versions?"

// cafeBands are the café question's options, the same names and weights as bands.
var cafeBands = []band{
	{leadstore.BandStrong, 100, "An independent café that is clearly open and active, with no digital loyalty of its own, that would likely pay a small monthly fee."},
	{leadstore.BandLikely, 75, "Probably such a café, though something about it is unclear; a café already on a loyalty platform is at most likely."},
	{leadstore.BandMaybe, 50, "Could go either way: too little is known about the café or whether it is open."},
	{leadstore.BandUnlikely, 25, "Probably not worth contacting: it looks closed, seasonal or tiny, is part of a chain, or has no clear way to reach the owner."},
	{leadstore.BandNo, 0, "Not a café customer: a chain, a restaurant or bar listed as a café, a roaster or shop with no seating, or a duplicate listing."},
}

// cafeInstructions is the café question the decider is asked about the state.
const cafeInstructions = "Which description fits this café best as a customer for a digital loyalty card that its customers keep in their phone wallet?"

// rtypes are the questions by rating type. The drupal entry is the question every rating
// before café leads was made under; it shares bands, so its wording is the one above.
var rtypes = map[string]rtype{
	leadstore.TypeDrupal: {Question, instructions, bands},
	leadstore.TypeCafe:   {leadstore.QuestionCafe, cafeInstructions, cafeBands},
}

// typeQ is rating type t's question; anything that is not a known type is a Drupal lead,
// as leadstore.TypeOf says.
func typeQ(t string) rtype {
	if q, ok := rtypes[t]; ok {
		return q
	}
	return rtypes[leadstore.TypeDrupal]
}

// AIQuestion is rating type t's relevance question as the decider is asked it: one choice
// among the bands, each with its plain description.
func AIQuestion(t string) ai.Question {
	q := typeQ(t)
	opts := make([]ai.Option, len(q.bands))
	for i, b := range q.bands {
		opts[i] = ai.Option{Name: b.name, Description: b.description}
	}
	return ai.Question{Name: questionName, Instructions: q.instructions, Options: opts}
}

// QuestionFor is rating type t's question name and version, as lead_relevance.question
// stores it.
func QuestionFor(t string) string { return typeQ(t).question }

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

// InputHash is the hash of everything a rating depends on: the decider's name, the lead's
// type's question with its instructions and every option's name and description, the
// state, and the question's version (QuestionFor). It is scoring.InputHash's
// length-prefixed encoding, with the version in its keyword slot, so a changed lead,
// wording, type or decider asks again and an unchanged one does not. A Drupal lead's hash
// is exactly the one it had before café leads had their own question.
func InputHash(decider string, in Input) []byte {
	t := leadstore.TypeOf(in.Kind)
	return scoring.InputHash(decider, []ai.Question{AIQuestion(t)}, State(in), 0,
		map[string][]string{"question": {QuestionFor(t)}})
}

// Describe returns band name's plain description in rating type t's question, as the
// decider reads it; "" for a name that is not a band.
func Describe(t, name string) string {
	if i := bandIndex(name); i >= 0 {
		return typeQ(t).bands[i].description
	}
	return ""
}
