package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestPrintBuildSummary(t *testing.T) {
	var b bytes.Buffer
	printBuildSummary(&b, buildSummary{
		Orgs: 6, Leads: 4, Created: 2, Refreshed: 1, Kept: 1, Skipped: 1, Reserved: 4,
		ByKind: map[string]map[string]int{
			"unsupported":    {"email": 1, "none": 1},
			"d10":            {"form": 1},
			"agency_partner": {"email": 1},
		},
		NoLead: map[string]int{"already current": 1, "public body": 1},
	})
	want := "leads build: 4 leads (2 new · 1 refreshed · 1 past new, kept) from 6 orgs · 1 new leads skipped (no longer a lead)" +
		" · fixture leads (reserved names, never in a real batch) 4\n" +
		"  agency_partner: email 1\n" +
		"  unsupported: email 1 · none 1\n" +
		"  d10: form 1\n" +
		"no lead: already current 1 · public body 1\n"
	if got := b.String(); got != want {
		t.Errorf("summary =\n%s\nwant\n%s", got, want)
	}
}

func TestLeadsBatchUsage(t *testing.T) {
	for _, args := range [][]string{
		{}, {"later"}, {"show"}, {"show", "x"}, {"approve", "1"}, {"approve", "1", "--hash", "abc", "--skip", "0"}, {"new", "extra"},
	} {
		if err := runLeadsBatch(t.Context(), args); err == nil || !strings.Contains(err.Error(), "usage") {
			t.Errorf("%q: err = %v, want a usage error", args, err)
		}
	}
	for _, args := range [][]string{{}, {"x"}, {"0"}} {
		if err := runLeadsDone(t.Context(), args); err == nil || !strings.Contains(err.Error(), "usage") {
			t.Errorf("done %q: err = %v", args, err)
		}
	}
	if err := runLeadsMarkContacted(t.Context(), nil); err == nil || !strings.Contains(err.Error(), "usage") {
		t.Errorf("mark-contacted: %v", err)
	}
}
