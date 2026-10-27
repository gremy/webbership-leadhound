// Package config reads leadhound's environment.
//
// Load returns every variable with its default and a list of problems with the values that
// are set; it never decides what a command needs. Each command names its own required
// variables through Require, so a command that never sends mail does not demand a mail key.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Secret is a value that must never be printed or logged: a key, a database URL that may
// carry a password, or a private mail address. fmt, slog and encoding/json all show "[redacted]"; code that needs the
// value calls Reveal (or converts it to a string).
type Secret string

const redacted = "[redacted]"

func (Secret) String() string               { return redacted }
func (Secret) GoString() string             { return redacted }
func (Secret) LogValue() slog.Value         { return slog.StringValue(redacted) }
func (Secret) MarshalJSON() ([]byte, error) { return []byte(`"` + redacted + `"`), nil }

// Reveal returns the value itself, for the one place that hands it to a client or a driver.
func (s Secret) Reveal() string { return string(s) }

// Live says which job sources a command may reach over the network. The zero value is off:
// without LEADHOUND_LIVE nothing fetches from a job source.
type Live int

const (
	LiveOff Live = iota // fixture transport only
	LiveATS             // LEADHOUND_LIVE=ats: only the public ATS API hosts
	LiveAll             // LEADHOUND_LIVE=1: every source
)

// atsHosts are the hosts LiveATS allows matched exactly: the public ATS APIs, and the Common
// Crawl index that boards harvest reads to find more boards. Only the families in
// atsHostFamilies are matched by suffix, and then narrowly.
var atsHosts = []string{
	"boards-api.greenhouse.io",
	"api.lever.co",
	"api.eu.lever.co",
	"api.ashbyhq.com",
	"api.smartrecruiters.com",
	"index.commoncrawl.org",
}

// atsHostFamilies are the ATSs that serve each company's board from a host of its own,
// <company>.<family>. LiveATS allows such a host only when it is exactly one DNS label (a-z,
// 0-9 and inner dashes, 1-63 long) followed by one of these suffixes: "acme.teamtailor.com"
// is allowed; "teamtailor.com", "a.b.teamtailor.com", "acmeteamtailor.com" and
// "acme.teamtailor.com.evil.example" are not. The host is compared as given: the fetch
// client lower-cases it and drops one trailing root dot before asking, for the first
// request and every redirect alike.
var atsHostFamilies = []string{
	".teamtailor.com",
	".jobs.personio.de",
	".jobs.personio.com",
}

// inATSHostFamily reports whether host is one DNS label in front of an atsHostFamilies suffix.
func inATSHostFamily(host string) bool {
	for _, suffix := range atsHostFamilies {
		if label, ok := strings.CutSuffix(host, suffix); ok && isDNSLabel(label) {
			return true
		}
	}
	return false
}

// isDNSLabel reports whether s is one lower-case DNS label: 1-63 of a-z, 0-9 and '-', with
// no '-' at either end.
func isDNSLabel(s string) bool {
	if len(s) == 0 || len(s) > 63 || s[0] == '-' || s[len(s)-1] == '-' {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
			return false
		}
	}
	return true
}

func (l Live) String() string {
	switch l {
	case LiveATS:
		return "ats"
	case LiveAll:
		return "all"
	default:
		return "off"
	}
}

// Allows reports whether a request to host (lower-case, no trailing dot) may go out under
// this setting.
func (l Live) Allows(host string) bool {
	switch l {
	case LiveAll:
		return true
	case LiveATS:
		return slices.Contains(atsHosts, host) || inATSHostFamily(host)
	default:
		return false
	}
}

// ParseLive reads LEADHOUND_LIVE: "" is off, "1" is everything, "ats" is the ATS APIs only.
// Anything else is an error and leaves live fetching off.
func ParseLive(v string) (Live, error) {
	switch v {
	case "":
		return LiveOff, nil
	case "1":
		return LiveAll, nil
	case "ats":
		return LiveATS, nil
	default:
		return LiveOff, fmt.Errorf(`LEADHOUND_LIVE must be unset, "1" or "ats", not %q`, v)
	}
}

// Config is leadhound's environment. Every field that holds a key, a database URL or a mail
// address is a Secret and stays redacted however the struct is printed; the other fields are
// plain settings (a directory path, a model name, a time zone, limits and budgets) that carry
// no credential or personal data, so a whole Config is safe to log.
type Config struct {
	DatabaseURL          Secret        // LEADHOUND_DATABASE_URL: the app role
	MigrateDatabaseURL   Secret        // LEADHOUND_MIGRATE_DATABASE_URL: the owner role
	DataDir              string        // LEADHOUND_DATA_DIR: the private data directory
	TypesafeAPIKey       Secret        // TYPESAFE_API_KEY
	JevModel             string        // LEADHOUND_JEV_MODEL
	JevDailyCallCap      int           // LEADHOUND_JEV_DAILY_CALL_CAP
	JevLeadsDailyCallCap int           // LEADHOUND_JEV_LEADS_DAILY_CALL_CAP: the runaway guard for lead relevance rating
	ResendAPIKey         Secret        // RESEND_API_KEY
	MailFrom             Secret        // LEADHOUND_MAIL_FROM: an address on a private sending domain
	MailTo               Secret        // LEADHOUND_MAIL_TO: a personal address
	TZ                   string        // LEADHOUND_TZ, an IANA zone name
	LinkedInInterval     time.Duration // LEADHOUND_LINKEDIN_INTERVAL: the pause between requests
	LinkedInSearchBudget int           // LEADHOUND_LINKEDIN_SEARCH_BUDGET: search requests per run
	LinkedInDetailBudget int           // LEADHOUND_LINKEDIN_DETAIL_BUDGET: detail requests per run, recheck included
	ATSInterval          time.Duration // LEADHOUND_ATS_INTERVAL: the pause between two requests to one ATS's API
	ATSDetailBudget      int           // LEADHOUND_ATS_DETAIL_BUDGET: posting-detail requests per ATS run (SmartRecruiters)
	Live                 Live          // LEADHOUND_LIVE
	ConsoleAddr          string        // LEADHOUND_CONSOLE_ADDR: host:port the console listens on (loopback only)
	ConsoleEmail         Secret        // LEADHOUND_CONSOLE_EMAIL: the operator's sign-in address
	ConsolePasswordHash  Secret        // LEADHOUND_CONSOLE_PASSWORD_HASH: bcrypt hash of the operator's password
	ConsoleURL           string        // LEADHOUND_CONSOLE_URL: the console's public https URL, for the brief's footer
	DBMaxBytes           int64         // LEADHOUND_DB_MAX_BYTES: the database size at which collectors stop storing
	DiskMinFreePct       int           // LEADHOUND_DISK_MIN_FREE_PCT: the free-disk floor, in percent, under which they stop
}

// Defaults for the variables that have one.
const (
	DefaultJevModel             = "jev-1.13.0"
	DefaultJevDailyCallCap      = 300
	DefaultJevLeadsDailyCallCap = 10000
	DefaultTZ                   = "Europe/Bucharest"
	DefaultLinkedInInterval     = 4 * time.Second
	DefaultLinkedInSearchBudget = 60
	DefaultLinkedInDetailBudget = 40
	DefaultATSInterval          = time.Second
	DefaultATSDetailBudget      = 500
	DefaultConsoleAddr          = "127.0.0.1:8090"
	DefaultDBMaxBytes           = 8 << 30
	DefaultDiskMinFreePct       = 15
	// MinATSInterval is the shortest pause LEADHOUND_ATS_INTERVAL may set: below it, one
	// ATS's API would see a burst from a large board list.
	MinATSInterval = 200 * time.Millisecond
)

// Load reads the environment through getenv (os.Getenv in main, a map in tests). It returns
// the config with defaults filled in and one problem per variable whose value cannot be used;
// a bad value keeps its default. Problems never repeat a value that may be secret.
//
// The four console variables are read as given and never reported here, so a console
// setting cannot stop a timer command: serve checks them with ValidConsoleAddr and
// Require, and brief uses the URL only when ValidConsoleURL accepts it.
func Load(getenv func(string) string) (Config, []string) {
	c := Config{
		DatabaseURL:        Secret(getenv("LEADHOUND_DATABASE_URL")),
		MigrateDatabaseURL: Secret(getenv("LEADHOUND_MIGRATE_DATABASE_URL")),
		DataDir:            getenv("LEADHOUND_DATA_DIR"),
		TypesafeAPIKey:     Secret(getenv("TYPESAFE_API_KEY")),
		JevModel:           or(getenv("LEADHOUND_JEV_MODEL"), DefaultJevModel),
		ResendAPIKey:       Secret(getenv("RESEND_API_KEY")),
		MailFrom:           Secret(getenv("LEADHOUND_MAIL_FROM")),
		MailTo:             Secret(getenv("LEADHOUND_MAIL_TO")),
		TZ:                 or(getenv("LEADHOUND_TZ"), DefaultTZ),

		ConsoleAddr:         or(getenv("LEADHOUND_CONSOLE_ADDR"), DefaultConsoleAddr),
		ConsoleEmail:        Secret(getenv("LEADHOUND_CONSOLE_EMAIL")),
		ConsolePasswordHash: Secret(getenv("LEADHOUND_CONSOLE_PASSWORD_HASH")),
		ConsoleURL:          getenv("LEADHOUND_CONSOLE_URL"),
	}
	var problems []string
	report := func(p string) { problems = append(problems, p) }

	c.JevDailyCallCap = count(getenv, "LEADHOUND_JEV_DAILY_CALL_CAP", DefaultJevDailyCallCap, report)
	c.JevLeadsDailyCallCap = count(getenv, "LEADHOUND_JEV_LEADS_DAILY_CALL_CAP", DefaultJevLeadsDailyCallCap, report)
	c.LinkedInSearchBudget = count(getenv, "LEADHOUND_LINKEDIN_SEARCH_BUDGET", DefaultLinkedInSearchBudget, report)
	c.LinkedInDetailBudget = count(getenv, "LEADHOUND_LINKEDIN_DETAIL_BUDGET", DefaultLinkedInDetailBudget, report)
	c.ATSDetailBudget = count(getenv, "LEADHOUND_ATS_DETAIL_BUDGET", DefaultATSDetailBudget, report)
	c.DBMaxBytes = between(getenv, "LEADHOUND_DB_MAX_BYTES", DefaultDBMaxBytes, 1, math.MaxInt64, report)
	c.DiskMinFreePct = int(between(getenv, "LEADHOUND_DISK_MIN_FREE_PCT", DefaultDiskMinFreePct, 1, 99, report))

	c.LinkedInInterval = duration(getenv, "LEADHOUND_LINKEDIN_INTERVAL", DefaultLinkedInInterval, "4s", report)
	c.ATSInterval = duration(getenv, "LEADHOUND_ATS_INTERVAL", DefaultATSInterval, "1s", report)
	if c.ATSInterval < MinATSInterval {
		report(fmt.Sprintf("LEADHOUND_ATS_INTERVAL must be at least %s, not %s; using %s", MinATSInterval, c.ATSInterval, MinATSInterval))
		c.ATSInterval = MinATSInterval
	}
	if _, err := time.LoadLocation(c.TZ); err != nil {
		report(fmt.Sprintf("LEADHOUND_TZ must be an IANA time zone such as %s, not %q", DefaultTZ, c.TZ))
		c.TZ = DefaultTZ
	}
	live, err := ParseLive(getenv("LEADHOUND_LIVE"))
	if err != nil {
		report(err.Error())
	}
	c.Live = live
	return c, problems
}

// ValidConsoleAddr checks LEADHOUND_CONSOLE_ADDR: host:port, where the host is the literal
// "localhost" or a loopback IP and the port is a number from 1 to 65535. It resolves no
// name, so "localhost" is trusted to be the loopback name it is on every system.
func ValidConsoleAddr(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("LEADHOUND_CONSOLE_ADDR must be host:port, such as %s, not %q", DefaultConsoleAddr, addr)
	}
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return fmt.Errorf("LEADHOUND_CONSOLE_ADDR must listen on localhost or a loopback address, not %q", addr)
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("LEADHOUND_CONSOLE_ADDR must end in a port from 1 to 65535, not %q", addr)
	}
	return nil
}

// errConsoleURL is the one refusal ValidConsoleURL gives. It never repeats the value,
// which names a private host.
var errConsoleURL = errors.New("LEADHOUND_CONSOLE_URL must be an https URL with a host and nothing after it, such as https://console.example")

// ValidConsoleURL checks LEADHOUND_CONSOLE_URL: an absolute https URL with a host (and an
// optional port), no user, no path beyond "/", no query and no fragment.
func ValidConsoleURL(u string) error {
	p, err := url.Parse(u)
	if err != nil || p.Scheme != "https" || p.Opaque != "" || p.User != nil || p.Hostname() == "" ||
		(p.Path != "" && p.Path != "/") || p.RawQuery != "" || p.ForceQuery || strings.Contains(u, "#") {
		return errConsoleURL
	}
	return nil
}

// duration reads a non-negative duration variable, keeping def when it is unset or unusable.
func duration(getenv func(string) string, name string, def time.Duration, example string, report func(string)) time.Duration {
	v := getenv(name)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 0 {
		report(fmt.Sprintf("%s must be a duration such as %s, not %q", name, example, v))
		return def
	}
	return d
}

// count reads a non-negative integer variable, keeping def when it is unset or unusable.
func count(getenv func(string) string, name string, def int, report func(string)) int {
	v := getenv(name)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		report(fmt.Sprintf("%s must be a whole number of 0 or more, not %q", name, v))
		return def
	}
	return n
}

// between reads an integer variable that must lie from lo to hi, keeping def when it is unset
// or out of range.
func between(getenv func(string) string, name string, def, lo, hi int64, report func(string)) int64 {
	v := getenv(name)
	if v == "" {
		return def
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < lo || n > hi {
		if hi == math.MaxInt64 {
			report(fmt.Sprintf("%s must be a whole number of %d or more, not %q", name, lo, v))
		} else {
			report(fmt.Sprintf("%s must be a whole number from %d to %d, not %q", name, lo, hi, v))
		}
		return def
	}
	return n
}

// Require returns one problem for each named variable that is not set in cfg, in the order
// given. Commands call it with the variables they cannot run without.
func Require(cfg Config, names ...string) []string {
	var missing []string
	for _, name := range names {
		v, ok := cfg.value(name)
		switch {
		case !ok:
			missing = append(missing, fmt.Sprintf("%s is an unknown variable", name))
		case v == "":
			missing = append(missing, fmt.Sprintf("%s is not set", name))
		}
	}
	return missing
}

// value returns the raw string behind a required-capable variable. Variables with a default
// are always set, so only the ones without one are listed.
func (c Config) value(name string) (string, bool) {
	switch name {
	case "LEADHOUND_DATABASE_URL":
		return string(c.DatabaseURL), true
	case "LEADHOUND_MIGRATE_DATABASE_URL":
		return string(c.MigrateDatabaseURL), true
	case "LEADHOUND_DATA_DIR":
		return c.DataDir, true
	case "TYPESAFE_API_KEY":
		return string(c.TypesafeAPIKey), true
	case "RESEND_API_KEY":
		return string(c.ResendAPIKey), true
	case "LEADHOUND_MAIL_FROM":
		return string(c.MailFrom), true
	case "LEADHOUND_MAIL_TO":
		return string(c.MailTo), true
	case "LEADHOUND_CONSOLE_EMAIL":
		return string(c.ConsoleEmail), true
	case "LEADHOUND_CONSOLE_PASSWORD_HASH":
		return string(c.ConsolePasswordHash), true
	case "LEADHOUND_LIVE":
		if c.Live == LiveOff {
			return "", true
		}
		return c.Live.String(), true
	default:
		return "", false
	}
}

func or(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
