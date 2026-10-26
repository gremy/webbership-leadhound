package leadconfig

import (
	"fmt"
	"strconv"
)

// Env is the outreach part of the environment. It carries no credential: the sending
// mailboxes, their servers and their sealed passwords live in the database.
type Env struct {
	Outreach         string // LEADHOUND_OUTREACH: "" is shadow, "live" sends over SMTP
	ProbeConcurrency int    // LEADHOUND_PROBE_CONCURRENCY: sites probed at once
	ProbeRate        int    // LEADHOUND_PROBE_RATE: requests per second across a run
	// Retired names the variables of RetiredEnv that are still set, in its order; their
	// values are never read.
	Retired []string
}

// RetiredEnv are the variables of the old single outreach mailbox. They are no longer read:
// the operator adds each sending mailbox in the console (Mailboxes) instead.
var RetiredEnv = []string{"LEADHOUND_OUTREACH_USER", "LEADHOUND_OUTREACH_PASSWORD", "LEADHOUND_SMTP_ADDR", "LEADHOUND_IMAP_ADDR"}

// Defaults and ranges for the outreach variables.
const (
	OutreachLive            = "live"
	DefaultProbeConcurrency = 16
	MaxProbeConcurrency     = 64
	DefaultProbeRate        = 12
	MaxProbeRate            = 50
)

// Live reports whether outreach sends for real; anything else is shadow.
func (e Env) Live() bool { return e.Outreach == OutreachLive }

// LoadEnv reads the outreach variables through getenv, as config.Load does: every variable
// with its default, and one problem per value that cannot be used (which then keeps its
// default). A retired variable still set is named in Retired, never a problem.
func LoadEnv(getenv func(string) string) (Env, []string) {
	var problems []string
	report := func(p string) { problems = append(problems, p) }
	var e Env
	for _, k := range RetiredEnv {
		if getenv(k) != "" {
			e.Retired = append(e.Retired, k)
		}
	}
	switch v := getenv("LEADHOUND_OUTREACH"); v {
	case "", OutreachLive:
		e.Outreach = v
	default:
		report(fmt.Sprintf(`LEADHOUND_OUTREACH must be unset (shadow) or "live", not %q; staying in shadow`, v))
	}
	e.ProbeConcurrency = inRange(getenv, "LEADHOUND_PROBE_CONCURRENCY", DefaultProbeConcurrency, 1, MaxProbeConcurrency, report)
	e.ProbeRate = inRange(getenv, "LEADHOUND_PROBE_RATE", DefaultProbeRate, 1, MaxProbeRate, report)
	return e, problems
}

// inRange reads a whole number between lo and hi, keeping def when it is unset or unusable.
func inRange(getenv func(string) string, name string, def, lo, hi int, report func(string)) int {
	v := getenv(name)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < lo || n > hi {
		report(fmt.Sprintf("%s must be a whole number from %d to %d, not %q; using %d", name, lo, hi, v, def))
		return def
	}
	return n
}
