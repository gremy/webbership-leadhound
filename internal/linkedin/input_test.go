package linkedin

import (
	"testing"

	"github.com/gremy/webbership-leadhound/internal/gates"
)

// TestObserveInputListedRemote: every observation the LinkedIn collector and recheck write
// says the source lists remote jobs only (the search is remote-filtered, f_WT=2), so the
// gates never read its unlabelled location field as an office.
func TestObserveInputListedRemote(t *testing.T) {
	g := gates.CandidateGates{Languages: []string{"en"}, RemoteOnly: true}
	d := Detail{Title: "Backend Engineer", Company: "Initech", Location: "Exampleville, Example Region", Description: "<p>Go.</p>"}
	in := observeInput(7, 9, "Card Co", d, g)
	if !in.ListedRemote {
		t.Error("ListedRemote = false, want true for a LinkedIn observation")
	}
	if in.JobID != 7 || in.FetchID != 9 || in.DerivedBy != DerivedBy || in.Location != d.Location || in.Company != "Initech" || !in.Gates.RemoteOnly {
		t.Errorf("input = %+v, want the detail's fields with the job, fetch and gates", in)
	}
	if got := observeInput(7, 9, "Card Co", Detail{}, g).Company; got != "Card Co" {
		t.Errorf("company = %q, want the card's when the page shows none", got)
	}
}
