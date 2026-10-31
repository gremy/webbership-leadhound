//go:build integration

package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/claude"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

// TestClaudeSpendBetween: an empty table sums to 0; Claude rows of every account and purpose
// count by when they were recorded, from inclusive and to exclusive; a Jev row never counts.
func TestClaudeSpendBetween(t *testing.T) {
	ctx := context.Background()
	q := appStore(t).Q()
	from := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	to := from.AddDate(0, 1, 0)

	if n, err := q.ClaudeSpendBetween(ctx, from, to); err != nil || n != 0 {
		t.Fatalf("cold start: %d, %v; want 0", n, err)
	}

	day := from.AddDate(0, 0, 4)
	for _, r := range []claude.UsageRow{
		{AccountID: 1, Purpose: "research", Model: "m", Day: day, CostMicros: 100},
		{AccountID: 1, Purpose: "drafting", Model: "m", Day: day, CostMicros: 20},
		{AccountID: 1, Purpose: "drafts_eval", Model: "m", Day: day, CostMicros: 3},
	} {
		if err := q.RecordClaudeUsage(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	// Back-date each row: one at from (in), one a nanosecond before to (in), one at to (out).
	owner := storetest.OwnerConn(t)
	for cost, at := range map[int64]time.Time{100: from, 20: to.Add(-time.Microsecond), 3: to} {
		if _, err := owner.Exec(ctx, `UPDATE ai_usage SET created_at = $2 WHERE cost_micros = $1`, cost, at); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := owner.Exec(ctx, `INSERT INTO ai_usage (provider, model, day, calls, input_tokens, cost_micros, created_at)
		VALUES ('typesafe', 'm', '2026-10-05', 1, 5, 9000, $1)`, from.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if n, err := q.ClaudeSpendBetween(ctx, from, to); err != nil || n != 120 {
		t.Errorf("October: %d, %v; want 120", n, err)
	}
	if n, err := q.ClaudeSpendBetween(ctx, from.Add(time.Second), to.Add(time.Second)); err != nil || n != 23 {
		t.Errorf("shifted: %d, %v; want 23", n, err)
	}
}
