//go:build integration

package store_test

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/gremy/webbership-leadhound/internal/store"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

// TestGateResultSnippet: the snippet a gate quotes is written by the app role, read back by
// LatestGateResults, and cut to 120 runes at the store even if a caller hands it more.
func TestGateResultSnippet(t *testing.T) {
	ctx := context.Background()
	q := appStore(t).Q()

	var ok bool
	if err := storetest.OwnerConn(t).QueryRow(ctx,
		"SELECT has_column_privilege('leadhound_app', 'public.gate_result', 'snippet', 'INSERT')").Scan(&ok); err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("leadhound_app lacks INSERT on gate_result.snippet")
	}

	jobID, fetchID := seedJob(t, q, "1000000002")
	obsID, err := q.InsertObservation(ctx, store.ObservationInsert{JobID: jobID, FetchID: fetchID, DerivedBy: "test@1", Title: "A"})
	if err != nil {
		t.Fatalf("InsertObservation: %v", err)
	}
	long := strings.Repeat("ă", 200) // two bytes a rune: a byte cut would split one
	for _, g := range []store.GateResultInsert{
		{JobID: jobID, ObservationID: obsID, Gate: "workplace", Verdict: "flag", Reason: "Posting says the job is hybrid.", Snippet: "hybrid, 2 days a week in the office", DerivedBy: "gates@1"},
		{JobID: jobID, ObservationID: obsID, Gate: "language", Verdict: "pass", Reason: "posting is in English", Snippet: long, DerivedBy: "gates@1"},
		{JobID: jobID, ObservationID: obsID, Gate: "location", Verdict: "pass", Reason: "none found", DerivedBy: "gates@1"},
	} {
		if _, err := q.InsertGateResult(ctx, g); err != nil {
			t.Fatalf("InsertGateResult as the app role: %v", err)
		}
	}
	got, err := q.LatestGateResults(ctx, jobID)
	if err != nil {
		t.Fatalf("LatestGateResults: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("LatestGateResults = %+v, want 3 rows", got)
	}
	if got[0].Snippet != "hybrid, 2 days a week in the office" {
		t.Errorf("workplace snippet = %q", got[0].Snippet)
	}
	if n := utf8.RuneCountInString(got[1].Snippet); n != 120 || got[1].Snippet != strings.Repeat("ă", 120) {
		t.Errorf("long snippet = %d runes (%q), want the first 120", n, got[1].Snippet)
	}
	if got[2].Snippet != "" {
		t.Errorf("unset snippet = %q, want the column default ''", got[2].Snippet)
	}
}
