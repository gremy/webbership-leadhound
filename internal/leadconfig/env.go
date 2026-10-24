package leadconfig

import (
	"fmt"
	"net"
	"strconv"

	"github.com/gremy/webbership-leadhound/internal/config"
)

// Env is the outreach part of the environment. User and Password are Secrets and stay
// redacted however the struct is printed; the other fields carry no credential.
type Env struct {
	SMTPAddr         string        // LEADHOUND_SMTP_ADDR: host:port
	IMAPAddr         string        // LEADHOUND_IMAP_ADDR: host:port
	User             config.Secret // LEADHOUND_OUTREACH_USER: the mailbox address
	Password         config.Secret // LEADHOUND_OUTREACH_PASSWORD
	Outreach         string        // LEADHOUND_OUTREACH: "" is shadow, "live" sends over SMTP
	ProbeConcurrency int           // LEADHOUND_PROBE_CONCURRENCY: sites probed at once
	ProbeRate        int           // LEADHOUND_PROBE_RATE: requests per second across a run
}

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
// default). Problems never repeat a secret value.
func LoadEnv(getenv func(string) string) (Env, []string) {
	var problems []string
	report := func(p string) { problems = append(problems, p) }
	e := Env{
		SMTPAddr: hostPort(getenv, "LEADHOUND_SMTP_ADDR", report),
		IMAPAddr: hostPort(getenv, "LEADHOUND_IMAP_ADDR", report),
		User:     config.Secret(getenv("LEADHOUND_OUTREACH_USER")),
		Password: config.Secret(getenv("LEADHOUND_OUTREACH_PASSWORD")),
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

// hostPort reads a host:port variable; an unusable value is reported and left empty.
func hostPort(getenv func(string) string, name string, report func(string)) string {
	v := getenv(name)
	if v == "" {
		return ""
	}
	if host, port, err := net.SplitHostPort(v); err != nil || host == "" || port == "" {
		report(fmt.Sprintf("%s must be host:port, such as mail.example:465, not %q", name, v))
		return ""
	}
	return v
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
