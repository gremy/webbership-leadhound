package claude

import "testing"

func TestEveryModelPriced(t *testing.T) {
	for _, m := range []string{ModelSonnet, ModelHaiku} {
		p, err := PriceOf(m)
		if err != nil {
			t.Fatalf("%s: %v", m, err)
		}
		if p.In <= 0 || p.Out <= 0 || p.CacheWrite <= 0 || p.CacheRead <= 0 {
			t.Errorf("%s has a zero rate: %+v", m, p)
		}
		if p.CacheWrite != p.In*125/100 {
			t.Errorf("%s cache write %d, want 1.25 × input", m, p.CacheWrite)
		}
	}
	for m := range prices {
		if m != ModelSonnet && m != ModelHaiku {
			t.Errorf("model %s priced but not covered here", m)
		}
	}
}

func TestUnknownModelIsAnError(t *testing.T) {
	for _, m := range []string{"", "claude-opus-5-5", "gpt-6"} {
		if _, err := Cost(Usage{Model: m, InputTokens: 1}); err == nil {
			t.Errorf("Cost(%q) = nil error, want refusal", m)
		}
	}
}

func TestCost(t *testing.T) {
	const M = 1_000_000
	cases := []struct {
		name string
		u    Usage
		want int64
	}{
		{"sonnet input", Usage{Model: ModelSonnet, InputTokens: M}, 2_000_000},
		{"sonnet output", Usage{Model: ModelSonnet, OutputTokens: M}, 10_000_000},
		{"sonnet cache read", Usage{Model: ModelSonnet, CacheReadTokens: M}, 100_000},
		{"sonnet cache write 5m", Usage{Model: ModelSonnet, CacheWriteTokens: M}, 2_500_000},
		{"haiku short", Usage{Model: ModelHaiku, InputTokens: 100_000, OutputTokens: 1000}, 10_000 + 500},
		{"haiku long tier", Usage{Model: ModelHaiku, InputTokens: 100_001}, 50_001}, // 100001 × $0.50/M, rounded up
		{"haiku long by cache", Usage{Model: ModelHaiku, InputTokens: 50_000, CacheReadTokens: 60_000}, 25_000 + 3_000},
		{"web search", Usage{Model: ModelSonnet, WebSearches: 3}, 30_000},
		{"web fetch free", Usage{Model: ModelSonnet, WebFetches: 5}, 0},
		{"batch half", Usage{Model: ModelSonnet, InputTokens: M, OutputTokens: M, Batch: true}, 6_000_000},
		{"batch stacks with cache", Usage{Model: ModelSonnet, CacheReadTokens: M, Batch: true}, 50_000},
		{"batch leaves search", Usage{Model: ModelSonnet, WebSearches: 1, Batch: true}, 10_000},
		{"rounds up", Usage{Model: ModelHaiku, CacheReadTokens: 1}, 1},
		{"fake is free", Usage{Model: FakeModel, InputTokens: M, OutputTokens: M}, 0},
	}
	for _, tc := range cases {
		got, err := Cost(tc.u)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got != tc.want {
			t.Errorf("%s: Cost = %d, want %d", tc.name, got, tc.want)
		}
	}
}
