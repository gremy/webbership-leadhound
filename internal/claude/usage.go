// Package claude is LeadHound's Anthropic (Claude) client: drafting and
// research calls, the price table, the spend cap and the key loader.
package claude

import (
	"context"
	"time"
)

// Purposes a Claude call is billed under, and the provider name stored with
// every usage row.
const (
	PurposeDrafting   = "drafting"
	PurposeResearch   = "research"
	PurposeDraftsEval = "drafts_eval"
	Provider          = "anthropic"
)

// UsageRow is one billed Claude call as the store records it.
type UsageRow struct {
	AccountID int64
	LeadID    int64 // 0 = no lead
	Purpose   string
	Model     string
	Day       time.Time // midnight of the local day in Capped.Loc

	InputTokens      int
	OutputTokens     int
	CacheReadTokens  int
	CacheWriteTokens int
	WebSearches      int

	CostMicros int64
	Batch      bool
}

// UsageStore is the slice of the store the cap needs. internal/store
// implements it; this package never imports the store.
type UsageStore interface {
	// ClaudeSpendOn returns micros spent by accountID on purpose on day;
	// purpose "" sums every Claude purpose.
	ClaudeSpendOn(ctx context.Context, accountID int64, purpose string, day time.Time) (int64, error)
	ClaudeSpendForLead(ctx context.Context, leadID int64, purpose string) (int64, error)
	RecordClaudeUsage(ctx context.Context, r UsageRow) error
}

// UsageLocker serialises the check-call-record sequence so two callers at the
// cap edge cannot both pass.
type UsageLocker interface {
	WithClaudeUsageLock(ctx context.Context, fn func(UsageStore) error) error
}
