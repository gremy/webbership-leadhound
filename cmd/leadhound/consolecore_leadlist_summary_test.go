package main

import (
	"testing"

	"github.com/gremy/webbership-leadhound/internal/console"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
)

// TestRowSummaryTown: a summary built from the place names its town once. When the address
// already holds the town, in any case, the town is left out of the line's head; when it does
// not, the town leads. A description always stands alone.
func TestRowSummaryTown(t *testing.T) {
	for _, c := range []struct {
		town, address, want string
	}{
		{"Fictiveton", "Fictiveton, 227150", "coffee shop · Fictiveton, 227150"},
		{"Fictiveton", "FICTIVETON 227150", "coffee shop · FICTIVETON 227150"},
		{"Fictiveton", "1 Fictive Road, AB1 2CD", "Fictiveton · coffee shop · 1 Fictive Road, AB1 2CD"},
		{"Fictiveton", "", "Fictiveton · coffee shop"},
		{"", "1 Fictive Road", "coffee shop · 1 Fictive Road"},
	} {
		row := console.LeadListRow{Town: c.town}
		if got := rowSummary(row, leadstore.RowExtras{Address: c.address, Category: "coffee_shop"}); got != c.want {
			t.Errorf("town %q, address %q: %q, want %q", c.town, c.address, got, c.want)
		}
	}
	if got := rowSummary(console.LeadListRow{Town: "Fictiveton"}, leadstore.RowExtras{Description: " A fictive  café. ", Address: "Fictiveton"}); got != "A fictive café." {
		t.Errorf("a description's summary %q", got)
	}
}
