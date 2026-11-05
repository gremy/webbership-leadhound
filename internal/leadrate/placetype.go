package leadrate

import (
	"github.com/gremy/webbership-leadhound/internal/ai"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/scoring"
)

// PlaceTypeQuestion is the place-type question's name and version, as a Jev
// lead_place_type row's detail stores it. Changing the wording or the options means a new
// version here.
const PlaceTypeQuestion = "place-type-cafe@1"

// placeTypeName is the ai.Question name the decider answers the place-type question under: a
// plain identifier, since the provider keys its answers by it.
const placeTypeName = "place_type"

// The place-type question's options, as the decider chooses among them.
const (
	PlaceOptionCoffeeShop = "coffee_shop"
	PlaceOptionRoastery   = "roastery"
	PlaceOptionUnclear    = "unclear"
)

// placeTypeInstructions is the place-type question the decider is asked about a café lead's
// state.
const placeTypeInstructions = "Does this place serve customers at a physical café, where they can sit and drink their coffee on the premises?"

// placeTypeOptions are the place-type question's options with their plain descriptions, and
// the store's place type each one is kept as.
var placeTypeOptions = []struct {
	name, description, placeType string
}{
	{PlaceOptionCoffeeShop, "A café or coffee shop that serves customers on the premises, with somewhere to sit.", leadstore.PlaceCafe},
	{PlaceOptionRoastery, "A roastery or online coffee store with no café to sit in.", leadstore.PlaceRoastery},
	{PlaceOptionUnclear, "Too little is known about the place to tell.", leadstore.PlaceUnclear},
}

// PlaceTypeAIQuestion is the place-type question as the decider is asked it: one choice
// among coffee_shop, roastery and unclear, each with its plain description.
func PlaceTypeAIQuestion() ai.Question {
	opts := make([]ai.Option, len(placeTypeOptions))
	for i, o := range placeTypeOptions {
		opts[i] = ai.Option{Name: o.name, Description: o.description}
	}
	return ai.Question{Name: placeTypeName, Instructions: placeTypeInstructions, Options: opts}
}

// PlaceTypeOf is the store's place type (leadstore.PlaceCafe, PlaceRoastery or PlaceUnclear)
// for the decider's choice; ok is false for a choice that is not an option.
func PlaceTypeOf(choice string) (placeType string, ok bool) {
	for _, o := range placeTypeOptions {
		if o.name == choice {
			return o.placeType, true
		}
	}
	return "", false
}

// PlaceTypeHash is the hash of everything a place-type answer depends on: the decider's
// name, the question with its instructions and every option's name and description, the
// café lead's state (State, the same input the café relevance question reads) and the
// question's version. It is scoring.InputHash's encoding, as InputHash uses it, so a changed
// lead, wording or decider asks again and an unchanged one does not.
func PlaceTypeHash(decider string, in Input) []byte {
	return scoring.InputHash(decider, []ai.Question{PlaceTypeAIQuestion()}, State(in), 0,
		map[string][]string{"question": {PlaceTypeQuestion}})
}
