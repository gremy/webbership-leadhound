package config

import "fmt"

// ClaudeCaps are one Claude purpose's spend caps in micro-dollars, as claude.Capped takes them.
type ClaudeCaps struct {
	DailyMicros int64 // the purpose's own daily cap
	TotalMicros int64 // the ceiling across every purpose (LEADHOUND_CLAUDE_DAILY_USD_CAP)
	LeadMicros  int64 // the most one lead may ever cost under the purpose
}

// Default Claude caps per purpose, in micro-dollars. The purpose names are claude.Purpose*
// (this package cannot import claude, which imports it). The drafting lead cap is about two
// worst-case attempts, so the one retry always fits; drafts_eval drafts one lead at a time
// like drafting and takes the same lead cap.
var defaultClaudeCaps = map[string]struct{ daily, lead int64 }{
	"drafting":    {3_000_000, 80_000},
	"research":    {1_500_000, 100_000},
	"drafts_eval": {2_000_000, 80_000},
}

// DefaultClaudeCaps returns purpose's default daily and per-lead caps in micro-dollars.
func DefaultClaudeCaps(purpose string) (daily, lead int64, ok bool) {
	d, ok := defaultClaudeCaps[purpose]
	return d.daily, d.lead, ok
}

// ClaudeCapsFor returns the caps a Claude call under purpose runs with. A campaign may set its
// own daily and per-lead caps (0 keeps the default); neither ever rises above
// LEADHOUND_CLAUDE_DAILY_USD_CAP, and the lead cap never rises above the purpose's daily cap.
// An unknown purpose or a negative cap is an error.
func (c Config) ClaudeCapsFor(purpose string, campaignDaily, campaignLead int64) (ClaudeCaps, error) {
	d, ok := defaultClaudeCaps[purpose]
	if !ok {
		return ClaudeCaps{}, fmt.Errorf("config: unknown Claude purpose %q", purpose)
	}
	if campaignDaily < 0 || campaignLead < 0 {
		return ClaudeCaps{}, fmt.Errorf("config: Claude %s caps must not be negative", purpose)
	}
	caps := ClaudeCaps{DailyMicros: d.daily, TotalMicros: c.ClaudeDailyCapMicros, LeadMicros: d.lead}
	if campaignDaily > 0 {
		caps.DailyMicros = campaignDaily
	}
	if campaignLead > 0 {
		caps.LeadMicros = campaignLead
	}
	caps.DailyMicros = min(caps.DailyMicros, caps.TotalMicros)
	caps.LeadMicros = min(caps.LeadMicros, caps.DailyMicros)
	return caps, nil
}
