package leadrate

import (
	"bytes"
	"testing"

	"github.com/gremy/webbership-leadhound/internal/cafe"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
)

// placeTypeCafe is a café lead with no website of its own, as the place-type question reads
// it: only a name, a category, a town and a region.
func placeTypeCafe() Input {
	return Input{Kind: cafe.KindNoDigitalLoyalty, Country: "RO", SiteName: "Cafeneaua Exemplu",
		Cafe: &CafeFacts{Category: "coffee_shop", Locality: "Orașul Mic", Region: "Județul Fictiv"}}
}

// TestPlaceTypeQuestion: the question is asked under its plain name, its instructions ask
// about a physical café, and its options are coffee_shop, roastery and unclear in that order,
// the roastery one in the words the operator chose.
func TestPlaceTypeQuestion(t *testing.T) {
	if PlaceTypeQuestion != "place-type-cafe@1" {
		t.Errorf("PlaceTypeQuestion = %q", PlaceTypeQuestion)
	}
	q := PlaceTypeAIQuestion()
	if q.Name != "place_type" || q.Instructions != placeTypeInstructions {
		t.Errorf("question %q / %q", q.Name, q.Instructions)
	}
	want := []string{"coffee_shop", "roastery", "unclear"}
	if len(q.Options) != len(want) {
		t.Fatalf("%d options, want %d", len(q.Options), len(want))
	}
	for i, o := range q.Options {
		if o.Name != want[i] || o.Description == "" {
			t.Errorf("option %d = %+v, want %q with a description", i, o, want[i])
		}
	}
	if d := q.Options[1].Description; d != "A roastery or online coffee store with no café to sit in." {
		t.Errorf("roastery description = %q", d)
	}
}

// TestPlaceTypeOf: each option maps to the store's place type, coffee_shop to the store's
// cafe, and anything else is refused.
func TestPlaceTypeOf(t *testing.T) {
	for choice, want := range map[string]string{
		"coffee_shop": leadstore.PlaceCafe, "roastery": leadstore.PlaceRoastery, "unclear": leadstore.PlaceUnclear,
	} {
		if got, ok := PlaceTypeOf(choice); !ok || got != want {
			t.Errorf("PlaceTypeOf(%q) = %q, %v; want %q", choice, got, ok, want)
		}
	}
	for _, bad := range []string{"", "cafe", "maybe", "Roastery"} {
		if got, ok := PlaceTypeOf(bad); ok {
			t.Errorf("PlaceTypeOf(%q) = %q, want refused", bad, got)
		}
	}
}

// TestPlaceTypeHash: the hash is a stable SHA-256 of the same input; it changes with the
// café's facts and the decider, and it never equals the café relevance question's hash of
// the same input, so the two answers never stand in for each other.
func TestPlaceTypeHash(t *testing.T) {
	in := placeTypeCafe()
	h := PlaceTypeHash("decider-a", in)
	if len(h) != 32 {
		t.Fatalf("hash is %d bytes, want 32", len(h))
	}
	if !bytes.Equal(h, PlaceTypeHash("decider-a", placeTypeCafe())) {
		t.Error("the same input hashed differently")
	}
	if bytes.Equal(h, PlaceTypeHash("decider-b", in)) {
		t.Error("a different decider gave the same hash")
	}
	renamed := placeTypeCafe()
	renamed.SiteName = "Prăjitoria Exemplu"
	if bytes.Equal(h, PlaceTypeHash("decider-a", renamed)) {
		t.Error("a renamed café gave the same hash")
	}
	moved := placeTypeCafe()
	moved.Cafe.Locality = "Alt Oraș"
	if bytes.Equal(h, PlaceTypeHash("decider-a", moved)) {
		t.Error("a café in another town gave the same hash")
	}
	if bytes.Equal(h, InputHash("decider-a", in)) {
		t.Error("the place-type hash equals the relevance hash of the same input")
	}
}
