package config

import (
	"strings"
	"testing"
)

// TestClaudeDailyUSDCap: LEADHOUND_CLAUDE_DAILY_USD_CAP parses to exact micro-dollars; an
// unusable value is a problem naming the variable and keeps the $5.00 default.
func TestClaudeDailyUSDCap(t *testing.T) {
	for _, c := range []struct {
		v    string
		want int64
		bad  bool
	}{
		{"", 5_000_000, false},
		{"5.00", 5_000_000, false},
		{"0", 0, false},
		{"0.07", 70_000, false},
		{"12", 12_000_000, false},
		{"1.123456", 1_123_456, false},
		{"100000", 100_000_000_000, false},
		{"100000.000001", 5_000_000, true},
		{"-1", 5_000_000, true},
		{"$5", 5_000_000, true},
		{"5.", 5_000_000, true},
		{".5", 5_000_000, true},
		{"1e3", 5_000_000, true},
		{"0.1234567", 5_000_000, true},
		{"five", 5_000_000, true},
		{" 5", 5_000_000, true},
	} {
		cfg, problems := Load(env(map[string]string{"LEADHOUND_CLAUDE_DAILY_USD_CAP": c.v}))
		if cfg.ClaudeDailyCapMicros != c.want {
			t.Errorf("%q: cap = %d, want %d", c.v, cfg.ClaudeDailyCapMicros, c.want)
		}
		if got := len(problems) == 1 && strings.Contains(problems[0], "LEADHOUND_CLAUDE_DAILY_USD_CAP"); got != c.bad {
			t.Errorf("%q: problems = %v, want a problem: %v", c.v, problems, c.bad)
		}
	}
}

// TestClaudeCapsFor: defaults per purpose; a campaign may lower either cap and raise the
// daily one only up to the env ceiling; the lead cap never passes the daily cap.
func TestClaudeCapsFor(t *testing.T) {
	cfg := Config{ClaudeDailyCapMicros: 5_000_000}
	for _, c := range []struct {
		name          string
		cfg           Config
		purpose       string
		daily, lead   int64
		want          ClaudeCaps
		wantErrSubstr string
	}{
		{"drafting defaults", cfg, "drafting", 0, 0, ClaudeCaps{3_000_000, 5_000_000, 200_000}, ""},
		{"research defaults", cfg, "research", 0, 0, ClaudeCaps{1_500_000, 5_000_000, 100_000}, ""},
		{"drafts_eval defaults", cfg, "drafts_eval", 0, 0, ClaudeCaps{2_000_000, 5_000_000, 200_000}, ""},
		{"campaign lowers", cfg, "drafting", 1_000_000, 40_000, ClaudeCaps{1_000_000, 5_000_000, 40_000}, ""},
		{"campaign over ceiling", cfg, "drafting", 9_000_000, 9_000_000, ClaudeCaps{5_000_000, 5_000_000, 5_000_000}, ""},
		{"low ceiling clamps default", Config{ClaudeDailyCapMicros: 1_000_000}, "research", 0, 0, ClaudeCaps{1_000_000, 1_000_000, 100_000}, ""},
		{"zero ceiling", Config{}, "drafting", 0, 0, ClaudeCaps{0, 0, 0}, ""},
		{"unknown purpose", cfg, "leads", 0, 0, ClaudeCaps{}, "unknown Claude purpose"},
		{"negative", cfg, "drafting", -1, 0, ClaudeCaps{}, "negative"},
	} {
		got, err := c.cfg.ClaudeCapsFor(c.purpose, c.daily, c.lead)
		if c.wantErrSubstr != "" {
			if err == nil || !strings.Contains(err.Error(), c.wantErrSubstr) {
				t.Errorf("%s: err = %v, want %q", c.name, err, c.wantErrSubstr)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("%s: = %+v, %v; want %+v", c.name, got, err, c.want)
		}
	}
}
