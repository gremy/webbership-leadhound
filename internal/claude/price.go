package claude

import "fmt"

// Price is one model's rates in micro-dollars per million tokens ($2.00/MTok = 2_000_000).
// Cache writes are the 5-minute (ephemeral) rate, the only cache LeadHound writes.
type Price struct {
	In, Out, CacheWrite, CacheRead int64
	// LongAbove, when set, is the prompt size (input + cache read + cache write tokens)
	// above which Long applies to the whole call.
	LongAbove int
	Long      *Price
}

// Per-call charges and discounts, in micro-dollars.
const (
	webSearchMicros = 10_000 // $10 per 1,000 searches; web fetch has no extra charge
	batchPercent    = 50     // Batch API price, in percent of the standard token rates
	perMTok         = 1_000_000
)

// prices are the Claude API list prices.
// Source: https://platform.claude.com/docs/en/about-claude/pricing (checked 2026-10-28).
var prices = map[string]Price{
	ModelSonnet: {In: 2_000_000, Out: 10_000_000, CacheWrite: 2_500_000, CacheRead: 100_000},
	ModelHaiku: {
		In: 100_000, Out: 500_000, CacheWrite: 125_000, CacheRead: 10_000,
		LongAbove: 100_000,
		Long:      &Price{In: 500_000, Out: 2_500_000, CacheWrite: 625_000, CacheRead: 50_000},
	},
}

// PriceOf returns the model's price; an unknown model is an error, never a zero price.
func PriceOf(model string) (Price, error) {
	if model == FakeModel {
		return Price{}, nil
	}
	p, ok := prices[model]
	if !ok {
		return Price{}, fmt.Errorf("claude: no price for model %q", model)
	}
	return p, nil
}

// Cost is what u was billed, in micro-dollars, rounded up so the ledger never under-counts.
func Cost(u Usage) (int64, error) {
	p, err := PriceOf(u.Model)
	if err != nil {
		return 0, err
	}
	if p.Long != nil && u.InputTokens+u.CacheReadTokens+u.CacheWriteTokens > p.LongAbove {
		p = *p.Long
	}
	// micro-dollars × 1e6, so one division at the end rounds once.
	tok := int64(u.InputTokens)*p.In + int64(u.OutputTokens)*p.Out +
		int64(u.CacheWriteTokens)*p.CacheWrite + int64(u.CacheReadTokens)*p.CacheRead
	if u.Batch {
		tok = (tok*batchPercent + 99) / 100
	}
	micros := (tok + perMTok - 1) / perMTok
	return micros + int64(u.WebSearches)*webSearchMicros, nil
}
