//go:build integration

package main

import (
	"context"
	"testing"

	"github.com/gremy/webbership-leadhound/internal/console"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
)

// TestRelevanceDBGoldenIsInCallOrder: the eval's lines from the database come oldest call
// first, whatever the lead ids, and a changed call counts from when it was changed. That is
// what makes the cap's "newest no calls" true of the operator's calls.
func TestRelevanceDBGoldenIsInCallOrder(t *testing.T) {
	orderScene(t)
	b := leadsBackend(t)
	ctx := context.Background()
	domains := []string{"lucerne.example", "margie.example", "cpandl.example"}
	// The lead called first has the higher id, so call order is not lead id order.
	if leadIDOf(t, domains[0]) < leadIDOf(t, domains[2]) {
		domains[0], domains[2] = domains[2], domains[0]
	}
	first, middle, last := domains[0], domains[1], domains[2]
	for _, c := range []struct{ domain, band string }{
		{last, "no"}, {first, "maybe"}, {middle, "no"}, {last, "no"}, // the last call on last is the newest of all
	} {
		if _, err := b.LeadAct(ctx, console.LeadAction{Do: console.LeadLabel, ID: leadIDOf(t, c.domain), Value: c.band}); err != nil {
			t.Fatal(err)
		}
	}
	lines, _, err := dbGolden(ctx, mustConfig(t), leadstore.TypeDrupal)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 3 || lines[0].ID != first || lines[1].ID != middle || lines[2].ID != last {
		t.Fatalf("lines %+v; want %s, %s, %s", lines, first, middle, last)
	}
	// Room for two: the maybe stays, and of the two no calls the newest does.
	kept, leftOut := capEvalLines(lines, 2)
	if leftOut != 1 || len(kept) != 2 || kept[0].ID != first || kept[1].ID != last {
		t.Errorf("kept %+v, left out %d; want %s and %s, 1", kept, leftOut, first, last)
	}
}
