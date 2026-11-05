//go:build integration

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/leadstore/leadstoretest"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

// addressLine is one Overture NDJSON line of a Romanian café with no website, at freeform
// address addr in town.
func addressLine(t *testing.T, id, name, addr, town string) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"id": id, "names": map[string]any{"primary": name}, "categories": map[string]any{"primary": "cafe"},
		"confidence": 0.9, "addresses": []any{map[string]any{"freeform": addr, "locality": town, "country": "RO"}},
		"sources": []any{map[string]any{"dataset": "demo", "record_id": id}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestPlacesBuildSkipsDuplicates: one café listed twice, spelled with other diacritics,
// punctuation and case, gets one lead; the other is skipped by the system with the reason
// "duplicate of lead N", and the summary counts it. A different café at the same address
// keeps its lead. A second build skips nothing new and counts 0, and the operator's Undo of
// the duplicate's skip is refused, because the next build would skip it again.
func TestPlacesBuildSkipsDuplicates(t *testing.T) {
	ctx := context.Background()
	dir := leadsDB(t)
	cafesDataDir(t, dir)
	mustRun(t, "leads", "campaigns", "register")
	file := placesFile(t, "dups.ndjson",
		addressLine(t, "dup-1", "Cafeneaua Fictivă", "Strada Exemplu 7", "Fictivești"),
		addressLine(t, "dup-2", "CAFENEAUA FICTIVA!", "strada exemplu, 7", "FICTIVESTI"),
		addressLine(t, "dup-3", "Alt Espresso Fictiv", "Strada Exemplu 7", "Fictivești"),
	)
	importPlaces(t, placesRelease, file)

	build := mustRun(t, "leads", "build", "--campaign", "cafes-loyalty")
	wantLines(t, "first build", build, "leads build: 2 leads (3 new ·", "  cafe_no_website: none 2\n", "no lead: duplicate of another lead 1\n")
	owner := storetest.OwnerConn(t)
	var keeper, dup int64
	if err := owner.QueryRow(ctx, `SELECT l.id FROM lead l JOIN org o ON o.id = l.org_id WHERE o.domain = 'overture:dup-1'`).Scan(&keeper); err != nil {
		t.Fatal(err)
	}
	if err := owner.QueryRow(ctx, `SELECT l.id FROM lead l JOIN org o ON o.id = l.org_id WHERE o.domain = 'overture:dup-2' AND l.state = 'skipped'`).Scan(&dup); err != nil {
		t.Fatalf("the duplicate is not skipped: %v", err)
	}
	wantReason := fmt.Sprintf("duplicate of lead %d", keeper)
	if n := dbCount(t, fmt.Sprintf(`SELECT count(*) FROM lead_event WHERE lead_id = %d AND state = 'skipped' AND actor = 'system' AND reason = '%s'`, dup, wantReason)); n != 1 {
		t.Errorf("%d system skips of lead %d with %q, want 1", n, dup, wantReason)
	}

	again := mustRun(t, "leads", "build", "--campaign", "cafes-loyalty")
	wantLines(t, "second build", again, "no lead: duplicate of another lead 0\n")
	if strings.Contains(again, "new leads skipped") {
		t.Errorf("second build skipped a lead:\n%s", again)
	}
	if n := dbCount(t, fmt.Sprintf(`SELECT count(*) FROM lead_event WHERE lead_id = %d AND state = 'skipped'`, dup)); n != 1 {
		t.Errorf("%d skips of the duplicate after two builds, want 1", n)
	}
	if n := dbCount(t, `SELECT count(*) FROM lead WHERE state = 'new'`); n != 2 {
		t.Errorf("%d new leads, want 2 (the keeper and the other café)", n)
	}

	if _, err := leadstoretest.Q(t).TriageUndoSkip(ctx, dup); !errors.Is(err, leadstore.ErrSystemSkip) {
		t.Errorf("undo of the duplicate's skip: %v, want ErrSystemSkip", err)
	}
}
