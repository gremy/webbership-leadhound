package main

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	"github.com/gremy/webbership-leadhound/internal/enrich"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
)

func TestLeadsEnrichUsage(t *testing.T) {
	for _, args := range [][]string{
		{"leads", "enrich", "extra"},
		{"leads", "enrich", "--max", "0"},
		{"leads", "enrich", "--dry-run"},
		{"leads", "enrich", "--fixtures", "x"},
	} {
		if code := run(context.Background(), args, io.Discard); code != 2 {
			t.Errorf("%q exited %d, want 2", args, code)
		}
	}
}

// TestEnrichKindsAreFactKinds: the kinds enrich returns are the ones org_fact stores.
func TestEnrichKindsAreFactKinds(t *testing.T) {
	if enrich.KindPhone != leadstore.FactPhone || enrich.KindDescription != leadstore.FactDescription ||
		enrich.KindSocial != leadstore.FactSocial || enrich.MaxRunes != leadstore.MaxFactRunes {
		t.Error("enrich's kinds or length differ from leadstore's")
	}
}

func TestPrintEnrichSummary(t *testing.T) {
	var b bytes.Buffer
	printEnrichSummary(&b, 7, true, enrichSummary{Orgs: 3, Phones: 2, Descriptions: 1, Social: 4, Requests: 5})
	want := "leads enrich: run 7 (dry run, fixtures) · orgs 3 · phones 2 · descriptions 1 · social 4 · requests 5 · stop none\n"
	if !strings.HasPrefix(b.String(), want) {
		t.Errorf("summary:\n%s\nwant it to start\n%s", b.String(), want)
	}
	b.Reset()
	printEnrichSummary(&b, 8, false, enrichSummary{StopReason: stopDiskBudget, StopNote: "disk 13% free"})
	if !strings.Contains(b.String(), "· stop disk_budget\n") || !strings.Contains(b.String(), "stopped storing: disk 13% free") {
		t.Errorf("summary:\n%s", b.String())
	}
}
