package store

import (
	"context"
	"fmt"
	"time"

	"github.com/gremy/webbership-leadhound/internal/claude"
)

// ClaudeSpendBetween sums the cost in micro-dollars recorded for Claude (provider anthropic
// only, as ClaudeSpendOn) on every account and purpose by rows written in [from, to): the
// month to date, or the spend since the last top-up. Each row is one billed call, timed by
// when it was recorded (created_at), not by its local day. Jev rows never count; no rows is 0.
func (q Queries) ClaudeSpendBetween(ctx context.Context, from, to time.Time) (int64, error) {
	var n int64
	err := q.db.QueryRow(ctx,
		`SELECT COALESCE(sum(cost_micros), 0)::bigint FROM ai_usage
		 WHERE provider = $1 AND created_at >= $2 AND created_at < $3`,
		claude.Provider, from, to).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: claude spend between %s and %s: %w", from.Format(time.RFC3339), to.Format(time.RFC3339), err)
	}
	return n, nil
}
