package leadrate

import (
	"math"

	"github.com/gremy/webbership-leadhound/internal/ai"
)

// Score turns an answer into the lead's relevance and its top band.
//
// The relevance is round(Σ p(band) × weight(band)) over the five bands. The probabilities
// are taken over the bands only and normalised to sum to one, so a provider whose numbers
// do not quite add up still gives 0 to 100 (with probabilities that sum to one this is the
// plain weighted sum). The top band is the band with the highest probability; a tie goes
// to the chosen option, then to the better band. An answer with no usable probabilities
// scores its chosen band's weight; one whose choice is not a band either is (0, "").
func Score(a ai.Answer) (int, string) {
	var sum, total float64
	top, topP := "", -1.0
	for _, b := range bands {
		p := a.Probabilities[b.name]
		if math.IsNaN(p) || math.IsInf(p, 0) || p < 0 {
			p = 0
		}
		sum += p * float64(b.weight)
		total += p
		if p > topP || (p == topP && b.name == a.Choice) {
			top, topP = b.name, p
		}
	}
	if total <= 0 {
		w, ok := Weight(a.Choice)
		if !ok {
			return 0, ""
		}
		return w, a.Choice
	}
	s := int(math.Round(sum / total))
	return min(max(s, 0), 100), top
}
