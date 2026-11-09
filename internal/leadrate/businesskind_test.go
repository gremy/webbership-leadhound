package leadrate

import (
	"bytes"
	"slices"
	"strings"
	"testing"

	"github.com/gremy/webbership-leadhound/internal/ai"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/scoring"
)

// businessKindDrupalLead is a Drupal lead as the business-kind question reads it.
func businessKindDrupalLead() Input {
	return Input{Kind: "d10", Country: "GB", SiteName: "Example Organisation"}
}

// TestBusinessKindQuestion: each rating type is asked under the same plain name, in its own
// instructions, among its own options in the planned order, and unclear ends both lists with
// the same wording. A rating type that is not known is asked as a Drupal lead, as TypeOf says.
func TestBusinessKindQuestion(t *testing.T) {
	if BusinessKindQuestion != "business-kind@1" {
		t.Errorf("BusinessKindQuestion = %q", BusinessKindQuestion)
	}
	const unclearWords = "Too little is stated to tell. Choose this rather than guess from a name."
	for _, c := range []struct {
		ratingType, instructions string
		options                  []string
	}{
		{
			leadstore.TypeCafe, "Which description fits this place best, judged only from what is stated about it?",
			[]string{"specialty_coffee", "neighbourhood_cafe", "bakery_cafe", "restaurant_cafe", "bar_lounge", "venue_cafe", "roastery_shop", "unclear"},
		},
		{
			leadstore.TypeDrupal, "Which description fits the organisation behind this website best, judged only from what is stated about it?",
			[]string{"education", "government", "healthcare", "nonprofit", "company", "media", "agency", "personal", "unclear"},
		},
	} {
		q := BusinessKindAIQuestion(c.ratingType)
		if q.Name != "business_kind" || q.Instructions != c.instructions {
			t.Errorf("%s question %q / %q", c.ratingType, q.Name, q.Instructions)
		}
		if got := optionNames(q); !slices.Equal(got, c.options) {
			t.Errorf("%s options = %v, want %v", c.ratingType, got, c.options)
		}
		for _, o := range q.Options {
			if o.Name == BusinessKindUnclear {
				if o.Description != unclearWords {
					t.Errorf("%s unclear description = %q", c.ratingType, o.Description)
				}
				continue
			}
			// Every kind asks for a stated fact, so a bare name and town fit none but unclear.
			if !strings.HasPrefix(o.Description, "Stated to be ") {
				t.Errorf("%s option %q description = %q", c.ratingType, o.Name, o.Description)
			}
		}
	}
	unknown := BusinessKindAIQuestion("no-such-type")
	if !slices.Equal(optionNames(unknown), optionNames(BusinessKindAIQuestion(leadstore.TypeDrupal))) {
		t.Errorf("unknown rating type options = %v", optionNames(unknown))
	}
}

// TestBusinessKindsClosedList: the exported list is the migration's closed list, each kind
// once, and changing the returned slice does not change the list.
func TestBusinessKindsClosedList(t *testing.T) {
	want := []string{
		"specialty_coffee", "neighbourhood_cafe", "bakery_cafe", "restaurant_cafe", "bar_lounge", "venue_cafe", "roastery_shop",
		"education", "government", "healthcare", "nonprofit", "company", "media", "agency", "personal",
		"unclear",
	}
	got := BusinessKinds()
	if !slices.Equal(got, want) {
		t.Fatalf("BusinessKinds() = %v, want %v", got, want)
	}
	got[0] = "changed"
	if BusinessKinds()[0] != want[0] {
		t.Error("changing the returned slice changed the list")
	}
}

// TestBusinessKindOf: a known option at or above the threshold is kept, a known option below
// it becomes unclear, and an unknown choice is refused at any confidence, as PlaceTypeOf
// refuses one.
func TestBusinessKindOf(t *testing.T) {
	if BusinessKindMinConfidence != 0.6 {
		t.Errorf("BusinessKindMinConfidence = %v", BusinessKindMinConfidence)
	}
	for _, kind := range BusinessKinds() {
		if got, ok := BusinessKindOf(kind, 0.6); !ok || got != kind {
			t.Errorf("BusinessKindOf(%q, 0.6) = %q, %v", kind, got, ok)
		}
		if got, ok := BusinessKindOf(kind, 0.59); !ok || got != BusinessKindUnclear {
			t.Errorf("BusinessKindOf(%q, 0.59) = %q, %v; want unclear", kind, got, ok)
		}
	}
	for _, bad := range []string{"", "cafe", "maybe", "Company"} {
		for _, confidence := range []float64{0, 0.59, 1} {
			if got, ok := BusinessKindOf(bad, confidence); ok || got != "" {
				t.Errorf("BusinessKindOf(%q, %v) = %q, %v; want refused", bad, confidence, got, ok)
			}
		}
	}
}

// TestBusinessKindHash: the hash is a stable SHA-256 that changes with the decider and the
// input, carries the rating type's question, the threshold and the version, and never equals
// the place-type or relevance hash of the same input.
func TestBusinessKindHash(t *testing.T) {
	in := placeTypeCafe()
	h := BusinessKindHash("decider-a", in)
	if len(h) != 32 {
		t.Fatalf("hash is %d bytes, want 32", len(h))
	}
	if !bytes.Equal(h, BusinessKindHash("decider-a", placeTypeCafe())) {
		t.Error("the same input hashed differently")
	}
	if bytes.Equal(h, BusinessKindHash("decider-b", in)) {
		t.Error("a different decider gave the same hash")
	}
	renamed := placeTypeCafe()
	renamed.SiteName = "Prăjitoria Exemplu"
	if bytes.Equal(h, BusinessKindHash("decider-a", renamed)) {
		t.Error("a renamed café gave the same hash")
	}
	moved := placeTypeCafe()
	moved.Cafe.Locality = "Alt Oraș"
	if bytes.Equal(h, BusinessKindHash("decider-a", moved)) {
		t.Error("a café in another town gave the same hash")
	}
	if bytes.Equal(h, PlaceTypeHash("decider-a", in)) {
		t.Error("the business-kind hash equals the place-type hash of the same input")
	}
	if bytes.Equal(h, InputHash("decider-a", in)) {
		t.Error("the business-kind hash equals the relevance hash of the same input")
	}
	for _, c := range []struct {
		ratingType string
		in         Input
	}{{leadstore.TypeCafe, placeTypeCafe()}, {leadstore.TypeDrupal, businessKindDrupalLead()}} {
		want := scoring.InputHash("decider-a", []ai.Question{BusinessKindAIQuestion(c.ratingType)}, State(c.in),
			0.6, map[string][]string{"question": {"business-kind@1"}})
		if !bytes.Equal(BusinessKindHash("decider-a", c.in), want) {
			t.Errorf("%s hash does not carry its question, the threshold and the version", c.ratingType)
		}
	}
}
