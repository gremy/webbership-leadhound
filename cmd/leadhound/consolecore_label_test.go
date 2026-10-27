package main

import (
	"slices"
	"testing"

	"github.com/gremy/webbership-leadhound/internal/console"
	"github.com/gremy/webbership-leadhound/internal/leadrate"
)

// TestCallBandsAreLeadrates: the bands the console offers and checks are leadrate's, in its
// order, each with leadrate's description as its help, and Clear is not a band.
func TestCallBandsAreLeadrates(t *testing.T) {
	if !slices.Equal(console.LabelBands, leadrate.Bands()) {
		t.Errorf("console offers %v, leadrate has %v", console.LabelBands, leadrate.Bands())
	}
	for i, b := range callBands() {
		if b.Value != leadrate.Bands()[i] || b.Help == "" || b.Help != leadrate.Describe(b.Value) || b.Label == "" {
			t.Errorf("band %d: %+v", i, b)
		}
	}
	if _, ok := leadrate.Weight(console.LabelClear); ok {
		t.Errorf("%q is a band", console.LabelClear)
	}
}
