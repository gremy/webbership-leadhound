package leadconfig

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

func envOf(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestLoadEnvDefaults(t *testing.T) {
	env, problems := LoadEnv(envOf(nil))
	if len(problems) != 0 {
		t.Fatalf("problems = %v", problems)
	}
	if env.ProbeConcurrency != DefaultProbeConcurrency || env.ProbeRate != DefaultProbeRate {
		t.Errorf("defaults = %d, %d", env.ProbeConcurrency, env.ProbeRate)
	}
	if DefaultProbeConcurrency != 16 || DefaultProbeRate != 12 {
		t.Errorf("default constants = %d, %d", DefaultProbeConcurrency, DefaultProbeRate)
	}
	if env.Outreach != "" || env.Live() {
		t.Errorf("outreach = %q, live = %v; want shadow", env.Outreach, env.Live())
	}
}

func TestLoadEnvValues(t *testing.T) {
	env, problems := LoadEnv(envOf(map[string]string{
		"LEADHOUND_OUTREACH":          "live",
		"LEADHOUND_PROBE_CONCURRENCY": "64",
		"LEADHOUND_PROBE_RATE":        "1",
	}))
	if len(problems) != 0 {
		t.Fatalf("problems = %v", problems)
	}
	if !env.Live() || env.ProbeConcurrency != 64 || env.ProbeRate != 1 || len(env.Retired) != 0 {
		t.Errorf("env = %+v", env)
	}
}

// TestLoadEnvRetired: the four single-mailbox variables are no longer read. Those still set
// are named in Retired, in RetiredEnv's order, never with their values, and never as a
// problem: a value that was once a problem (no port) is just retired now.
func TestLoadEnvRetired(t *testing.T) {
	want := []string{"LEADHOUND_OUTREACH_USER", "LEADHOUND_OUTREACH_PASSWORD", "LEADHOUND_SMTP_ADDR", "LEADHOUND_IMAP_ADDR"}
	if !slices.Equal(RetiredEnv, want) {
		t.Fatalf("RetiredEnv = %q, want %q", RetiredEnv, want)
	}
	pass := "pw-" + "leak"
	env, problems := LoadEnv(envOf(map[string]string{
		"LEADHOUND_IMAP_ADDR":         "imap.sender.example",
		"LEADHOUND_OUTREACH_PASSWORD": pass,
	}))
	if len(problems) != 0 {
		t.Errorf("problems = %q, want none", problems)
	}
	if got := []string{"LEADHOUND_OUTREACH_PASSWORD", "LEADHOUND_IMAP_ADDR"}; !slices.Equal(env.Retired, got) {
		t.Errorf("Retired = %q, want %q", env.Retired, got)
	}
	if out := fmt.Sprintf("%v %+v", env, env); strings.Contains(out, pass) || strings.Contains(out, "imap.sender.example") {
		t.Errorf("printing Env shows a retired value: %s", out)
	}
	if env, _ := LoadEnv(envOf(nil)); env.Retired != nil {
		t.Errorf("Retired with nothing set = %q", env.Retired)
	}
}

func TestLoadEnvProblems(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"outreach other than live", map[string]string{"LEADHOUND_OUTREACH": "yes"}, "LEADHOUND_OUTREACH"},
		{"concurrency zero", map[string]string{"LEADHOUND_PROBE_CONCURRENCY": "0"}, "LEADHOUND_PROBE_CONCURRENCY"},
		{"concurrency too high", map[string]string{"LEADHOUND_PROBE_CONCURRENCY": "65"}, "LEADHOUND_PROBE_CONCURRENCY"},
		{"concurrency not a number", map[string]string{"LEADHOUND_PROBE_CONCURRENCY": "many"}, "LEADHOUND_PROBE_CONCURRENCY"},
		{"rate zero", map[string]string{"LEADHOUND_PROBE_RATE": "0"}, "LEADHOUND_PROBE_RATE"},
		{"rate too high", map[string]string{"LEADHOUND_PROBE_RATE": "51"}, "LEADHOUND_PROBE_RATE"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env, problems := LoadEnv(envOf(tc.env))
			if len(problems) != 1 || !strings.Contains(problems[0], tc.want) {
				t.Fatalf("problems = %q, want one naming %s", problems, tc.want)
			}
			if env.ProbeConcurrency != DefaultProbeConcurrency || env.ProbeRate != DefaultProbeRate || env.Live() {
				t.Errorf("a bad value did not keep its default: %+v", env)
			}
		})
	}
}
