//go:build integration

package store_test

import (
	"context"
	"maps"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/ai"
	"github.com/gremy/webbership-leadhound/internal/claude"
)

// TestClaudeSpendByLead: Claude spend is summed per lead since a time (by created_at), for one
// purpose or all; rows with no lead, Jev rows and rows before since never count; the lead ids
// narrow it, and none reads every lead. Nothing is an empty map.
func TestClaudeSpendByLead(t *testing.T) {
	ctx := context.Background()
	q := appStore(t).Q()
	since := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)

	if got, err := q.ClaudeSpendByLead(ctx, nil, since, ""); err != nil || got == nil || len(got) != 0 {
		t.Fatalf("cold start: %v %v, want an empty map", got, err)
	}
	day := time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC)
	for _, r := range []claude.UsageRow{
		{AccountID: 1, LeadID: 7, Purpose: claude.PurposeDrafting, Model: "m", Day: day, CostMicros: 600_000},
		{AccountID: 1, LeadID: 7, Purpose: claude.PurposeResearch, Model: "m", Day: day, CostMicros: 200_000},
		{AccountID: 1, LeadID: 8, Purpose: claude.PurposeDrafting, Model: "m", Day: day, CostMicros: 5_000},
		{AccountID: 1, LeadID: 9, Purpose: claude.PurposeDrafting, Model: "m", Day: day, CostMicros: 70_000}, // old: back-dated below
		{AccountID: 1, Purpose: claude.PurposeDrafting, Model: "m", Day: day, CostMicros: 1_000},             // no lead
	} {
		if err := q.RecordClaudeUsage(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	if err := q.RecordAIUsage(ctx, ai.UsageRow{Provider: "typesafe", Model: "m", Purpose: ai.PurposeLeads, Day: day, Calls: 1, InputTokens: 9}); err != nil {
		t.Fatal(err)
	}
	ownerExec(t, `UPDATE ai_usage SET created_at = $1 WHERE lead_id = 9`, since.Add(-time.Hour))

	for _, c := range []struct {
		name    string
		ids     []int64
		purpose string
		want    map[int64]int64
	}{
		{"every lead, every purpose", nil, "", map[int64]int64{7: 800_000, 8: 5_000}},
		{"empty ids reads every lead", []int64{}, "", map[int64]int64{7: 800_000, 8: 5_000}},
		{"drafting", nil, claude.PurposeDrafting, map[int64]int64{7: 600_000, 8: 5_000}},
		{"lead 8 and an unknown lead", []int64{8, 99}, "", map[int64]int64{8: 5_000}},
		{"the old lead only", []int64{9}, "", map[int64]int64{}},
		{"Jev's purpose costs nothing", nil, ai.PurposeLeads, map[int64]int64{}},
	} {
		got, err := q.ClaudeSpendByLead(ctx, c.ids, since, c.purpose)
		if err != nil || !maps.Equal(got, c.want) {
			t.Errorf("%s: %v %v, want %v", c.name, got, err, c.want)
		}
	}
	if got, err := q.ClaudeSpendByLead(ctx, nil, since.Add(-2*time.Hour), ""); err != nil || got[9] != 70_000 {
		t.Errorf("an earlier since takes the old row: %v %v", got, err)
	}
}
