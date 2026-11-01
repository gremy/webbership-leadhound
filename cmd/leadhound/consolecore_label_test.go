package main

import (
	"slices"
	"testing"

	"github.com/gremy/webbership-leadhound/internal/console"
	"github.com/gremy/webbership-leadhound/internal/leadrate"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
)

// TestCallBandsAreLeadrates: the bands the console offers and checks are leadrate's, in its
// order, each with leadrate's description as its help, and Clear is not a band.
func TestCallBandsAreLeadrates(t *testing.T) {
	if !slices.Equal(console.LabelBands, leadrate.Bands()) {
		t.Errorf("console offers %v, leadrate has %v", console.LabelBands, leadrate.Bands())
	}
	for _, typ := range leadstore.RatingTypes {
		for i, b := range callBands(typ) {
			if b.Value != leadrate.Bands()[i] || b.Help == "" || b.Help != leadrate.Describe(typ, b.Value) || b.Label == "" {
				t.Errorf("%s band %d: %+v", typ, i, b)
			}
		}
	}
	if d, c := callBands(leadstore.TypeDrupal), callBands(leadstore.TypeCafe); d[0].Help == c[0].Help {
		t.Errorf("a café band has the Drupal help: %q", c[0].Help)
	}
	if _, ok := leadrate.Weight(console.LabelClear); ok {
		t.Errorf("%q is a band", console.LabelClear)
	}
}
