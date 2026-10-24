package leadconfig

import (
	"fmt"
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
	user := "outbox" + "@" + "sender.example"
	pass := "pw-" + "demo"
	env, problems := LoadEnv(envOf(map[string]string{
		"LEADHOUND_SMTP_ADDR":         "smtp.sender.example:465",
		"LEADHOUND_IMAP_ADDR":         "imap.sender.example:993",
		"LEADHOUND_OUTREACH_USER":     user,
		"LEADHOUND_OUTREACH_PASSWORD": pass,
		"LEADHOUND_OUTREACH":          "live",
		"LEADHOUND_PROBE_CONCURRENCY": "64",
		"LEADHOUND_PROBE_RATE":        "1",
	}))
	if len(problems) != 0 {
		t.Fatalf("problems = %v", problems)
	}
	if env.SMTPAddr != "smtp.sender.example:465" || env.IMAPAddr != "imap.sender.example:993" {
		t.Errorf("addrs = %q %q", env.SMTPAddr, env.IMAPAddr)
	}
	if env.User.Reveal() != user || env.Password.Reveal() != pass {
		t.Error("secrets not read")
	}
	if !env.Live() || env.ProbeConcurrency != 64 || env.ProbeRate != 1 {
		t.Errorf("env = %+v", env)
	}
	if out := fmt.Sprintf("%v %+v", env, env); strings.Contains(out, user) || strings.Contains(out, pass) {
		t.Errorf("printing Env shows a secret: %s", out)
	}
}

func TestLoadEnvProblems(t *testing.T) {
	pass := "pw-" + "leak"
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
		{"smtp without port", map[string]string{"LEADHOUND_SMTP_ADDR": "smtp.sender.example"}, "LEADHOUND_SMTP_ADDR"},
		{"imap without port", map[string]string{"LEADHOUND_IMAP_ADDR": "imap.sender.example"}, "LEADHOUND_IMAP_ADDR"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.env["LEADHOUND_OUTREACH_PASSWORD"] = pass
			env, problems := LoadEnv(envOf(tc.env))
			if len(problems) != 1 || !strings.Contains(problems[0], tc.want) {
				t.Fatalf("problems = %q, want one naming %s", problems, tc.want)
			}
			if strings.Contains(problems[0], pass) {
				t.Errorf("problem repeats a secret: %q", problems[0])
			}
			if env.ProbeConcurrency != DefaultProbeConcurrency || env.ProbeRate != DefaultProbeRate || env.Live() {
				t.Errorf("a bad value did not keep its default: %+v", env)
			}
		})
	}
}
