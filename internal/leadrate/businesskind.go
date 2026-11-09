package leadrate

import (
	"slices"

	"github.com/gremy/webbership-leadhound/internal/ai"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/scoring"
)

// BusinessKindQuestion is the business-kind question's name and version, as
// lead_business_kind.question stores it. Changing the wording or the options means a new
// version here.
const BusinessKindQuestion = "business-kind@1"

// BusinessKindMinConfidence is the confidence below which a business-kind answer counts as
// unclear. It goes into BusinessKindHash's threshold slot, so changing it asks every lead
// again.
const BusinessKindMinConfidence = 0.6

// businessKindName is the ai.Question name the decider answers the business-kind question
// under: a plain identifier, since the provider keys its answers by it.
const businessKindName = "business_kind"

// The business-kind question's options, as the decider chooses among them and as
// lead_business_kind.kind stores them. BusinessKindUnclear is an option of both rating types.
const (
	BusinessKindSpecialtyCoffee   = "specialty_coffee"
	BusinessKindNeighbourhoodCafe = "neighbourhood_cafe"
	BusinessKindBakeryCafe        = "bakery_cafe"
	BusinessKindRestaurantCafe    = "restaurant_cafe"
	BusinessKindBarLounge         = "bar_lounge"
	BusinessKindVenueCafe         = "venue_cafe"
	BusinessKindRoasteryShop      = "roastery_shop"

	BusinessKindEducation  = "education"
	BusinessKindGovernment = "government"
	BusinessKindHealthcare = "healthcare"
	BusinessKindNonprofit  = "nonprofit"
	BusinessKindCompany    = "company"
	BusinessKindMedia      = "media"
	BusinessKindAgency     = "agency"
	BusinessKindPersonal   = "personal"

	BusinessKindUnclear = "unclear"
)

// businessKindUnclearWords is the unclear option's description in both rating types' lists.
const businessKindUnclearWords = "Too little is stated to tell. Choose this rather than guess from a name."

// businessKindOption is one option of the business-kind question with its plain description.
type businessKindOption struct {
	name, description string
}

// businessKindQuestions are the business-kind question's instructions and options by rating
// type. Every kind but unclear asks for a stated fact, so a lead known only by its name and
// town fits none of them and answers unclear.
var businessKindQuestions = map[string]struct {
	instructions string
	options      []businessKindOption
}{
	leadstore.TypeCafe: {
		"Which description fits this place best, judged only from what is stated about it?",
		[]businessKindOption{
			{BusinessKindSpecialtyCoffee, "Stated to be a café built around specialty coffee, such as single-origin beans or named roasters."},
			{BusinessKindNeighbourhoodCafe, "Stated to be a local café serving its neighbourhood; a place category of café or coffee shop alone does not show this."},
			{BusinessKindBakeryCafe, "Stated to be a bakery or pastry shop that also serves coffee."},
			{BusinessKindRestaurantCafe, "Stated to be a restaurant or bistro that also runs as a café."},
			{BusinessKindBarLounge, "Stated to be a bar, pub or lounge that also serves coffee."},
			{BusinessKindVenueCafe, "Stated to be a café inside another venue, such as a museum, bookshop, hotel or coworking space."},
			{BusinessKindRoasteryShop, "Stated to be a coffee roaster that runs its own shop or café."},
			{BusinessKindUnclear, businessKindUnclearWords},
		},
	},
	leadstore.TypeDrupal: {
		"Which description fits the organisation behind this website best, judged only from what is stated about it?",
		[]businessKindOption{
			{BusinessKindEducation, "Stated to be a school, university or other place of learning."},
			{BusinessKindGovernment, "Stated to be a public body, such as a ministry, council or public agency."},
			{BusinessKindHealthcare, "Stated to be a hospital, clinic or other healthcare provider."},
			{BusinessKindNonprofit, "Stated to be a nonprofit, charity, foundation or association."},
			{BusinessKindCompany, "Stated to be a business selling its own products or services, other than an agency or a publisher."},
			{BusinessKindMedia, "Stated to be a newspaper, magazine, broadcaster or other publisher."},
			{BusinessKindAgency, "Stated to be an agency that builds websites or runs marketing for clients."},
			{BusinessKindPersonal, "Stated to be one person's own site, such as a blog or portfolio."},
			{BusinessKindUnclear, businessKindUnclearWords},
		},
	},
}

// BusinessKindAIQuestion is rating type ratingType's business-kind question as the decider is
// asked it: one choice among that type's kinds and unclear, each with its plain description.
// Anything that is not a known type is a Drupal lead, as leadstore.TypeOf says.
func BusinessKindAIQuestion(ratingType string) ai.Question {
	q, ok := businessKindQuestions[ratingType]
	if !ok {
		q = businessKindQuestions[leadstore.TypeDrupal]
	}
	opts := make([]ai.Option, len(q.options))
	for i, o := range q.options {
		opts[i] = ai.Option{Name: o.name, Description: o.description}
	}
	return ai.Question{Name: businessKindName, Instructions: q.instructions, Options: opts}
}

// BusinessKinds returns the full closed list of kinds, each once: the café kinds, the Drupal
// kinds, then unclear. It is the list migration 00034's CHECK on lead_business_kind.kind holds.
func BusinessKinds() []string {
	var out []string
	for _, t := range []string{leadstore.TypeCafe, leadstore.TypeDrupal} {
		for _, o := range businessKindQuestions[t].options {
			if o.name != BusinessKindUnclear {
				out = append(out, o.name)
			}
		}
	}
	return append(out, BusinessKindUnclear)
}

// BusinessKindOf is the kind the decider's choice is kept as: the choice itself, or unclear
// when confidence is below BusinessKindMinConfidence. ok is false for a choice that is not a
// kind, at any confidence.
func BusinessKindOf(choice string, confidence float64) (kind string, ok bool) {
	if !slices.Contains(BusinessKinds(), choice) {
		return "", false
	}
	if confidence < BusinessKindMinConfidence {
		return BusinessKindUnclear, true
	}
	return choice, true
}

// BusinessKindHash is the hash of everything a business-kind answer depends on: the decider's
// name, the lead's rating type's question with its instructions and every option's name and
// description, the lead's state (State, the same input the relevance question reads),
// BusinessKindMinConfidence in the threshold slot and the question's version. It is
// scoring.InputHash's encoding, as PlaceTypeHash uses it, so a changed lead, wording, threshold
// or decider asks again and an unchanged one does not.
func BusinessKindHash(decider string, in Input) []byte {
	q := BusinessKindAIQuestion(leadstore.TypeOf(in.Kind))
	return scoring.InputHash(decider, []ai.Question{q}, State(in), BusinessKindMinConfidence,
		map[string][]string{"question": {BusinessKindQuestion}})
}
