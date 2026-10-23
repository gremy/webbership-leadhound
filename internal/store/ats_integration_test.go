//go:build integration

package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/gremy/webbership-leadhound/internal/store"
)

// listingOf returns the listing_status of (source, id) and whether status_checked_at is set.
func listingOf(t *testing.T, q store.Queries, source, id string) (string, bool) {
	t.Helper()
	j, err := q.JobBySourceID(context.Background(), source, id)
	if err != nil {
		t.Fatalf("job %s/%s: %v", source, id, err)
	}
	return j.ListingStatus, j.StatusCheckedAt != nil
}

func TestExpireMissingAndReopen(t *testing.T) {
	ctx := context.Background()
	q := appStore(t).Q()
	companyID, err := q.UpsertCompany(ctx, "Northwind Labs", "", "")
	if err != nil {
		t.Fatal(err)
	}
	const src = "greenhouse"
	ids := []string{
		"acme/1", "acme/2", "acme/3", // the board under test
		"acme-labs/1", "acme-labs/9", // a board whose name starts with the same letters
		"acmex/1",         // and one that shares the prefix without a dash
		"ac%/1", "a_me/1", // LIKE wildcards in a board name are plain characters
		"other/1", // another board
	}
	for _, id := range ids {
		if _, err := q.UpsertJob(ctx, store.JobUpsert{Source: src, SourceJobID: id, CompanyID: companyID, Title: "Go Engineer", URL: "https://boards.example/" + id}); err != nil {
			t.Fatal(err)
		}
	}
	// The same board under another source is never touched.
	if _, err := q.UpsertJob(ctx, store.JobUpsert{Source: "lever", SourceJobID: "acme/2", CompanyID: companyID, Title: "Go Engineer", URL: "https://boards.example/l"}); err != nil {
		t.Fatal(err)
	}

	n, err := q.ExpireMissing(ctx, src, "acme", []string{"acme/1"})
	if err != nil || n != 2 {
		t.Fatalf("ExpireMissing = %d, %v; want 2", n, err)
	}
	for id, want := range map[string]string{
		"acme/1": "open", "acme/2": "expired", "acme/3": "expired",
		"acme-labs/1": "open", "acme-labs/9": "open", "acmex/1": "open", "ac%/1": "open", "a_me/1": "open", "other/1": "open",
	} {
		got, checked := listingOf(t, q, src, id)
		if got != want {
			t.Errorf("%s = %s, want %s", id, got, want)
		}
		if checked != (want == "expired") {
			t.Errorf("%s status_checked_at set = %t", id, checked)
		}
	}
	if got, _ := listingOf(t, q, "lever", "acme/2"); got != "open" {
		t.Errorf("lever acme/2 = %s, want open", got)
	}

	// A board name with a LIKE wildcard matches only itself.
	if n, err := q.ExpireMissing(ctx, src, "ac%", nil); err != nil || n != 1 {
		t.Fatalf("ExpireMissing ac%% = %d, %v; want 1", n, err)
	}
	if got, _ := listingOf(t, q, src, "acme-labs/1"); got != "open" {
		t.Errorf("acme-labs/1 = %s after ac%%, want open", got)
	}

	// Expiring again is idempotent: already expired jobs are not counted.
	if n, err := q.ExpireMissing(ctx, src, "acme", []string{"acme/1"}); err != nil || n != 0 {
		t.Fatalf("second ExpireMissing = %d, %v; want 0", n, err)
	}

	// An empty board name is refused, never a source-wide expiry.
	if _, err := q.ExpireMissing(ctx, src, "", nil); !errors.Is(err, store.ErrEmptyBoard) {
		t.Fatalf("empty board: err = %v, want ErrEmptyBoard", err)
	}

	// Reopen brings back only the expired jobs it names.
	n, err = q.Reopen(ctx, src, []string{"acme/2", "acme/1", "missing/7"})
	if err != nil || n != 1 {
		t.Fatalf("Reopen = %d, %v; want 1", n, err)
	}
	for id, want := range map[string]string{"acme/1": "open", "acme/2": "open", "acme/3": "expired"} {
		if got, _ := listingOf(t, q, src, id); got != want {
			t.Errorf("after reopen %s = %s, want %s", id, got, want)
		}
	}
	if n, err := q.Reopen(ctx, src, nil); err != nil || n != 0 {
		t.Fatalf("Reopen nil = %d, %v", n, err)
	}
}

func TestMarkChecked(t *testing.T) {
	ctx := context.Background()
	q := appStore(t).Q()
	companyID, err := q.UpsertCompany(ctx, "Northwind Labs", "", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, j := range []struct{ src, id string }{{"lever", "acme/1"}, {"lever", "acme/2"}, {"ashby", "acme/1"}} {
		if _, err := q.UpsertJob(ctx, store.JobUpsert{Source: j.src, SourceJobID: j.id, CompanyID: companyID, Title: "Go Engineer", URL: "https://boards.example/x"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := q.ExpireMissing(ctx, "lever", "acme", []string{"acme/1"}); err != nil {
		t.Fatal(err)
	}
	ownerExec(t, `UPDATE job SET status_checked_at = NULL`)

	n, err := q.MarkChecked(ctx, "lever", []string{"acme/1", "missing/3"})
	if err != nil || n != 1 {
		t.Fatalf("MarkChecked = %d, %v; want 1", n, err)
	}
	for _, j := range []struct {
		src, id string
		status  string
		checked bool
	}{
		{"lever", "acme/1", "open", true},
		{"lever", "acme/2", "expired", false}, // not listed: untouched, status kept
		{"ashby", "acme/1", "open", false},    // another source
	} {
		got, checked := listingOf(t, q, j.src, j.id)
		if got != j.status || checked != j.checked {
			t.Errorf("%s %s = %s checked %t, want %s %t", j.src, j.id, got, checked, j.status, j.checked)
		}
	}
	if n, err := q.MarkChecked(ctx, "lever", nil); err != nil || n != 0 {
		t.Fatalf("MarkChecked nil = %d, %v", n, err)
	}
}

// TestExpireMissingBoardPart: the board is the part of source_job_id before the first '/',
// compared whole: Lever EU's "eu:<slug>" is its own board, distinct from "<slug>"; an id
// with no '/' belongs to no board; a prefix holding '/' reaches nothing.
func TestExpireMissingBoardPart(t *testing.T) {
	ctx := context.Background()
	q := appStore(t).Q()
	companyID, err := q.UpsertCompany(ctx, "Northwind Labs", "", "")
	if err != nil {
		t.Fatal(err)
	}
	const src = "lever"
	for _, id := range []string{"acme/1", "acme/2/x", "eu:acme/1", "eu:acme/2", "acme"} {
		if _, err := q.UpsertJob(ctx, store.JobUpsert{Source: src, SourceJobID: id, CompanyID: companyID, Title: "Go Engineer", URL: "https://boards.example/" + id}); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := q.ExpireMissing(ctx, src, "eu:acme", []string{"eu:acme/1"}); err != nil || n != 1 {
		t.Fatalf("ExpireMissing eu:acme = %d, %v; want 1", n, err)
	}
	if n, err := q.ExpireMissing(ctx, src, "acme/2", nil); err != nil || n != 0 {
		t.Fatalf("ExpireMissing acme/2 = %d, %v; want 0", n, err)
	}
	if n, err := q.ExpireMissing(ctx, src, "acme", nil); err != nil || n != 2 {
		t.Fatalf("ExpireMissing acme = %d, %v; want 2", n, err)
	}
	for id, want := range map[string]string{
		"acme/1": "expired", "acme/2/x": "expired", "eu:acme/1": "open", "eu:acme/2": "expired", "acme": "open",
	} {
		if got, _ := listingOf(t, q, src, id); got != want {
			t.Errorf("%s = %s, want %s", id, got, want)
		}
	}
}
