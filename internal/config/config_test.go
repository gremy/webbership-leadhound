package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// env builds a getenv func from a map, as main passes os.Getenv.
func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestLoadDefaults(t *testing.T) {
	cfg, problems := Load(env(nil))
	if len(problems) != 0 {
		t.Fatalf("problems = %v, want none", problems)
	}
	want := Config{
		JevModel:             "jev-1.13.0",
		JevDailyCallCap:      300,
		TZ:                   "Europe/Bucharest",
		LinkedInInterval:     4 * time.Second,
		LinkedInSearchBudget: 60,
		LinkedInDetailBudget: 40,
		ATSInterval:          time.Second,
		ATSDetailBudget:      500,
		Live:                 LiveOff,
		DatabaseURL:          "",
		MigrateDatabaseURL:   "",
		DataDir:              "",
		TypesafeAPIKey:       "",
		ResendAPIKey:         "",
		MailFrom:             "",
		MailTo:               "",
		ConsoleAddr:          "127.0.0.1:8090",
		ConsoleEmail:         "",
		ConsolePasswordHash:  "",
		ConsoleURL:           "",
	}
	if !reflect.DeepEqual(cfg, want) {
		t.Fatalf("Load(empty) = %#v\nwant %#v", cfg, want)
	}
}

func TestLoadValues(t *testing.T) {
	cfg, problems := Load(env(map[string]string{
		"LEADHOUND_DATABASE_URL":           "postgres://app@db.example/leadhound",
		"LEADHOUND_MIGRATE_DATABASE_URL":   "postgres://owner@db.example/leadhound",
		"LEADHOUND_DATA_DIR":               "/srv/leadhound",
		"TYPESAFE_API_KEY":                 "tk-test",
		"LEADHOUND_JEV_MODEL":              "jev-2.0.0",
		"LEADHOUND_JEV_DAILY_CALL_CAP":     "12",
		"RESEND_API_KEY":                   "re-test",
		"LEADHOUND_MAIL_FROM":              "brief@mail.example",
		"LEADHOUND_MAIL_TO":                "inbox@mail.example",
		"LEADHOUND_TZ":                     "UTC",
		"LEADHOUND_LINKEDIN_INTERVAL":      "1500ms",
		"LEADHOUND_LINKEDIN_SEARCH_BUDGET": "5",
		"LEADHOUND_LINKEDIN_DETAIL_BUDGET": "0",
		"LEADHOUND_ATS_DETAIL_BUDGET":      "25",
		"LEADHOUND_LIVE":                   "ats",
		"LEADHOUND_CONSOLE_ADDR":           "localhost:9000",
		"LEADHOUND_CONSOLE_EMAIL":          "me@mail.example",
		"LEADHOUND_CONSOLE_PASSWORD_HASH":  "hash-test",
		"LEADHOUND_CONSOLE_URL":            "https://console.example",
	}))
	if len(problems) != 0 {
		t.Fatalf("problems = %v, want none", problems)
	}
	checks := []struct {
		name      string
		got, want any
	}{
		{"DatabaseURL", string(cfg.DatabaseURL), "postgres://app@db.example/leadhound"},
		{"MigrateDatabaseURL", string(cfg.MigrateDatabaseURL), "postgres://owner@db.example/leadhound"},
		{"DataDir", cfg.DataDir, "/srv/leadhound"},
		{"TypesafeAPIKey", string(cfg.TypesafeAPIKey), "tk-test"},
		{"JevModel", cfg.JevModel, "jev-2.0.0"},
		{"JevDailyCallCap", cfg.JevDailyCallCap, 12},
		{"ResendAPIKey", string(cfg.ResendAPIKey), "re-test"},
		{"MailFrom", string(cfg.MailFrom), "brief@mail.example"},
		{"MailTo", string(cfg.MailTo), "inbox@mail.example"},
		{"TZ", cfg.TZ, "UTC"},
		{"LinkedInInterval", cfg.LinkedInInterval, 1500 * time.Millisecond},
		{"LinkedInSearchBudget", cfg.LinkedInSearchBudget, 5},
		{"LinkedInDetailBudget", cfg.LinkedInDetailBudget, 0},
		{"ATSDetailBudget", cfg.ATSDetailBudget, 25},
		{"Live", cfg.Live, LiveATS},
		{"ConsoleAddr", cfg.ConsoleAddr, "localhost:9000"},
		{"ConsoleEmail", cfg.ConsoleEmail.Reveal(), "me@mail.example"},
		{"ConsolePasswordHash", cfg.ConsolePasswordHash.Reveal(), "hash-test"},
		{"ConsoleURL", cfg.ConsoleURL, "https://console.example"},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
}

func TestLoadATSInterval(t *testing.T) {
	for v, want := range map[string]time.Duration{"": time.Second, "200ms": 200 * time.Millisecond, "250ms": 250 * time.Millisecond, "2s": 2 * time.Second} {
		cfg, problems := Load(env(map[string]string{"LEADHOUND_ATS_INTERVAL": v}))
		if len(problems) != 0 || cfg.ATSInterval != want {
			t.Errorf("LEADHOUND_ATS_INTERVAL=%q: %v, problems %q; want %v", v, cfg.ATSInterval, problems, want)
		}
	}
	// Below the floor: reported, and the floor used.
	for _, v := range []string{"0s", "199ms"} {
		cfg, problems := Load(env(map[string]string{"LEADHOUND_ATS_INTERVAL": v}))
		if len(problems) != 1 || !strings.Contains(problems[0], "at least 200ms") || cfg.ATSInterval != MinATSInterval {
			t.Errorf("LEADHOUND_ATS_INTERVAL=%q: %v, problems %q; want the floor and one problem", v, cfg.ATSInterval, problems)
		}
	}
	// A bad value keeps the default.
	if cfg, _ := Load(env(map[string]string{"LEADHOUND_ATS_INTERVAL": "soon"})); cfg.ATSInterval != DefaultATSInterval {
		t.Errorf("bad value: ATSInterval = %v, want the default", cfg.ATSInterval)
	}
}

func TestLoadParseErrors(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		wantVar string // the variable the single problem must name
	}{
		{"cap not a number", map[string]string{"LEADHOUND_JEV_DAILY_CALL_CAP": "lots"}, "LEADHOUND_JEV_DAILY_CALL_CAP"},
		{"cap negative", map[string]string{"LEADHOUND_JEV_DAILY_CALL_CAP": "-1"}, "LEADHOUND_JEV_DAILY_CALL_CAP"},
		{"interval no unit", map[string]string{"LEADHOUND_LINKEDIN_INTERVAL": "4"}, "LEADHOUND_LINKEDIN_INTERVAL"},
		{"interval negative", map[string]string{"LEADHOUND_LINKEDIN_INTERVAL": "-2s"}, "LEADHOUND_LINKEDIN_INTERVAL"},
		{"ats interval no unit", map[string]string{"LEADHOUND_ATS_INTERVAL": "1"}, "LEADHOUND_ATS_INTERVAL"},
		{"ats interval negative", map[string]string{"LEADHOUND_ATS_INTERVAL": "-1s"}, "LEADHOUND_ATS_INTERVAL"},
		{"search budget", map[string]string{"LEADHOUND_LINKEDIN_SEARCH_BUDGET": "1.5"}, "LEADHOUND_LINKEDIN_SEARCH_BUDGET"},
		{"detail budget", map[string]string{"LEADHOUND_LINKEDIN_DETAIL_BUDGET": "x"}, "LEADHOUND_LINKEDIN_DETAIL_BUDGET"},
		{"ats detail budget", map[string]string{"LEADHOUND_ATS_DETAIL_BUDGET": "-3"}, "LEADHOUND_ATS_DETAIL_BUDGET"},
		{"ats detail budget text", map[string]string{"LEADHOUND_ATS_DETAIL_BUDGET": "many"}, "LEADHOUND_ATS_DETAIL_BUDGET"},
		{"timezone", map[string]string{"LEADHOUND_TZ": "Mars/Olympus"}, "LEADHOUND_TZ"},
		{"live", map[string]string{"LEADHOUND_LIVE": "yes"}, "LEADHOUND_LIVE"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, problems := Load(env(tt.env))
			if len(problems) != 1 || !strings.Contains(problems[0], tt.wantVar) {
				t.Fatalf("problems = %q, want one naming %s", problems, tt.wantVar)
			}
			// A bad value never silently turns live fetching on.
			if tt.wantVar == "LEADHOUND_LIVE" && cfg.Live != LiveOff {
				t.Errorf("Live = %v after a bad value, want off", cfg.Live)
			}
		})
	}
}

func TestLoadReportsEveryProblem(t *testing.T) {
	_, problems := Load(env(map[string]string{
		"LEADHOUND_JEV_DAILY_CALL_CAP": "x",
		"LEADHOUND_LINKEDIN_INTERVAL":  "y",
		"LEADHOUND_LIVE":               "z",
	}))
	if len(problems) != 3 {
		t.Fatalf("problems = %q, want 3", problems)
	}
}

// TestLoadConsoleNeverReports: Load reports nothing about the console variables, however
// bad, so a console setting can never stop a timer command. serve and brief check them.
func TestLoadConsoleNeverReports(t *testing.T) {
	cfg, problems := Load(env(map[string]string{
		"LEADHOUND_CONSOLE_ADDR":          "192.0.2.10:8090",
		"LEADHOUND_CONSOLE_EMAIL":         "not an address",
		"LEADHOUND_CONSOLE_PASSWORD_HASH": "not a hash",
		"LEADHOUND_CONSOLE_URL":           "http://console.example/path?q=1",
	}))
	if len(problems) != 0 {
		t.Fatalf("problems = %q, want none", problems)
	}
	// The values are kept as given, for the callers to check.
	if cfg.ConsoleAddr != "192.0.2.10:8090" || cfg.ConsoleURL != "http://console.example/path?q=1" {
		t.Errorf("ConsoleAddr = %q, ConsoleURL = %q; want the values as set", cfg.ConsoleAddr, cfg.ConsoleURL)
	}
}

func TestValidConsoleAddr(t *testing.T) {
	tests := []struct {
		addr string
		ok   bool
	}{
		{"127.0.0.1:8090", true},
		{"[::1]:8090", true},
		{"localhost:8090", true},
		{"127.0.0.2:8090", true},
		{"192.0.2.10:8090", false},
		{"0.0.0.0:8090", false},
		{"[::]:8090", false},
		{":8090", false},
		{"8090", false},
		{"", false},
		{"127.0.0.1", false},
		{"127.0.0.1:", false},
		{"127.0.0.1:0", false},
		{"127.0.0.1:65536", false},
		{"127.0.0.1:http", false},
		{"LOCALHOST:8090", false},
		{"localhost.example:8090", false},
		{"console.example:8090", false},
	}
	for _, tt := range tests {
		err := ValidConsoleAddr(tt.addr)
		if (err == nil) != tt.ok {
			t.Errorf("ValidConsoleAddr(%q) = %v, want ok=%v", tt.addr, err, tt.ok)
		}
		if err != nil && !strings.Contains(err.Error(), "LEADHOUND_CONSOLE_ADDR") {
			t.Errorf("ValidConsoleAddr(%q) = %v, want the variable named", tt.addr, err)
		}
	}
	if err := ValidConsoleAddr(DefaultConsoleAddr); err != nil {
		t.Errorf("the default %q is refused: %v", DefaultConsoleAddr, err)
	}
}

func TestValidConsoleURL(t *testing.T) {
	tests := []struct {
		url string
		ok  bool
	}{
		{"https://console.example", true},
		{"https://console.example/", true},
		{"https://console.example:8443", true},
		{"http://console.example", false},
		{"https://console.example/console", false},
		{"https://console.example/jobs/", false},
		{"https://console.example?x=1", false},
		{"https://console.example/?", false},
		{"https://console.example#top", false},
		{"https://console.example/#", false},
		{"https:///jobs", false},
		{"https://", false},
		{"https://:443", false},
		{"https://user" + "@" + "console.example", false},
		{"console.example", false},
		{"//console.example", false},
		{"https:console.example", false},
		{"", false},
		{" https://console.example", false},
	}
	for _, tt := range tests {
		err := ValidConsoleURL(tt.url)
		if (err == nil) != tt.ok {
			t.Errorf("ValidConsoleURL(%q) = %v, want ok=%v", tt.url, err, tt.ok)
		}
		if err != nil && !strings.Contains(err.Error(), "LEADHOUND_CONSOLE_URL") {
			t.Errorf("ValidConsoleURL(%q) = %v, want the variable named", tt.url, err)
		}
	}
}

func TestLive(t *testing.T) {
	tests := []struct {
		value   string
		want    Live
		wantErr bool
	}{
		{"", LiveOff, false},
		{"1", LiveAll, false},
		{"ats", LiveATS, false},
		{"0", LiveOff, true},
		{"true", LiveOff, true},
		{"ATS", LiveOff, true},
		{" 1", LiveOff, true},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%q", tt.value), func(t *testing.T) {
			got, err := ParseLive(tt.value)
			if (err != nil) != tt.wantErr || got != tt.want {
				t.Fatalf("ParseLive(%q) = %v, %v; want %v, err=%v", tt.value, got, err, tt.want, tt.wantErr)
			}
		})
	}
}

func TestLiveAllows(t *testing.T) {
	tests := []struct {
		live Live
		host string
		want bool
	}{
		{LiveOff, "boards-api.greenhouse.io", false},
		{LiveOff, "www.linkedin.com", false},
		{LiveATS, "boards-api.greenhouse.io", true},
		{LiveATS, "api.lever.co", true},
		{LiveATS, "api.eu.lever.co", true},
		{LiveATS, "api.ashbyhq.com", true},
		{LiveATS, "index.commoncrawl.org", true},
		{LiveATS, "data.commoncrawl.org", false},
		{LiveATS, "commoncrawl.org", false},
		{LiveOff, "index.commoncrawl.org", false},
		{LiveATS, "www.linkedin.com", false},
		{LiveATS, "careers.company.example", false},
		{LiveATS, "evil.api.lever.co", false},
		{LiveATS, "api.smartrecruiters.com", true},
		{LiveATS, "x.api.smartrecruiters.com", false},
		{LiveATS, "smartrecruiters.com", false},
		{LiveATS, "jobs.smartrecruiters.com", false},
		{LiveATS, "acme.teamtailor.com", true},
		{LiveATS, "acme-2.teamtailor.com", true},
		{LiveATS, "9.teamtailor.com", true},
		{LiveATS, "acme.jobs.personio.de", true},
		{LiveATS, "acme.jobs.personio.com", true},
		{LiveOff, "acme.teamtailor.com", false},
		{LiveATS, "teamtailor.com", false},
		{LiveATS, ".teamtailor.com", false},
		{LiveATS, "a.b.teamtailor.com", false},
		{LiveATS, "acmeteamtailor.com", false},
		{LiveATS, "teamtailor.com.evil.example", false},
		{LiveATS, "acme.teamtailor.com.evil.example", false},
		{LiveATS, "acme.teamtailor.com.", false}, // the client drops the root dot first
		{LiveATS, "ACME.teamtailor.com", false},  // the client lower-cases first
		{LiveATS, "acme.TEAMTAILOR.COM", false},
		{LiveATS, "-acme.teamtailor.com", false},
		{LiveATS, "acme-.teamtailor.com", false},
		{LiveATS, "ac_me.teamtailor.com", false},
		{LiveATS, "acme" + "@" + "x.teamtailor.com", false},
		{LiveATS, strings.Repeat("a", 63) + ".teamtailor.com", true},
		{LiveATS, strings.Repeat("a", 64) + ".teamtailor.com", false},
		{LiveATS, "jobs.personio.de", false},
		{LiveATS, "personio.de", false},
		{LiveATS, "acme.personio.de", false},
		{LiveATS, "a.b.jobs.personio.de", false},
		{LiveATS, "acme.jobs.personio.net", false},
		{LiveATS, "acme.jobs.personio.de.evil.example", false},
		{LiveATS, "acmejobs.personio.de", false},
		{LiveAll, "www.linkedin.com", true},
		{LiveAll, "careers.company.example", true},
	}
	for _, tt := range tests {
		if got := tt.live.Allows(tt.host); got != tt.want {
			t.Errorf("%v.Allows(%q) = %v, want %v", tt.live, tt.host, got, tt.want)
		}
	}
}

func TestRequire(t *testing.T) {
	cfg, _ := Load(env(map[string]string{
		"LEADHOUND_DATABASE_URL": "postgres://app@db.example/x",
		"LEADHOUND_MAIL_TO":      "inbox@mail.example",
	}))
	got := Require(cfg, "LEADHOUND_DATABASE_URL", "RESEND_API_KEY", "LEADHOUND_MAIL_TO", "LEADHOUND_DATA_DIR")
	want := []string{"RESEND_API_KEY is not set", "LEADHOUND_DATA_DIR is not set"}
	if !slices.Equal(got, want) {
		t.Fatalf("Require = %q, want %q", got, want)
	}
	// The console's two secrets have no default, so Require can name them.
	got = Require(cfg, "LEADHOUND_CONSOLE_EMAIL", "LEADHOUND_CONSOLE_PASSWORD_HASH")
	want = []string{"LEADHOUND_CONSOLE_EMAIL is not set", "LEADHOUND_CONSOLE_PASSWORD_HASH is not set"}
	if !slices.Equal(got, want) {
		t.Fatalf("Require(console) = %q, want %q", got, want)
	}
	set, _ := Load(env(map[string]string{
		"LEADHOUND_CONSOLE_EMAIL":         "me@mail.example",
		"LEADHOUND_CONSOLE_PASSWORD_HASH": "hash-test",
	}))
	if got := Require(set, "LEADHOUND_CONSOLE_EMAIL", "LEADHOUND_CONSOLE_PASSWORD_HASH"); len(got) != 0 {
		t.Fatalf("Require(console set) = %q, want none", got)
	}
	if got := Require(cfg, "LEADHOUND_DATABASE_URL"); len(got) != 0 {
		t.Fatalf("Require(set) = %q, want none", got)
	}
	// A name Require does not know is a programming error, reported rather than ignored.
	if got := Require(cfg, "NOT_A_LEADHOUND_VAR"); len(got) != 1 || !strings.Contains(got[0], "unknown") {
		t.Fatalf("Require(unknown) = %q, want one unknown-variable problem", got)
	}
}

func TestSecretRedaction(t *testing.T) {
	const canary = "redaction-canary-value"
	cfg, _ := Load(env(map[string]string{
		"TYPESAFE_API_KEY":                canary,
		"RESEND_API_KEY":                  canary,
		"LEADHOUND_DATABASE_URL":          "postgres://app:" + canary + "@db.example/x",
		"LEADHOUND_MIGRATE_DATABASE_URL":  "postgres://owner:" + canary + "@db.example/x",
		"LEADHOUND_MAIL_FROM":             canary + "@mail.example",
		"LEADHOUND_MAIL_TO":               canary + "@inbox.example",
		"LEADHOUND_CONSOLE_EMAIL":         canary + "@console.example",
		"LEADHOUND_CONSOLE_PASSWORD_HASH": "$2a$12$" + canary,
	}))
	s := Secret(canary)

	var logged bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logged, nil))
	log.Info("cfg", "config", cfg, "key", s, "ptr", &cfg)
	logText := slog.New(slog.NewTextHandler(&logged, nil))
	logText.Info("cfg", "config", cfg, "key", s)

	asJSON, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}

	outputs := map[string]string{
		"%v secret":   fmt.Sprintf("%v", s),
		"%q secret":   fmt.Sprintf("%q", s),
		"%+v secret":  fmt.Sprintf("%+v", s),
		"%#v secret":  fmt.Sprintf("%#v", s),
		"%v config":   fmt.Sprintf("%v", cfg),
		"%+v config":  fmt.Sprintf("%+v", cfg),
		"%#v config":  fmt.Sprintf("%#v", cfg),
		"%v pointer":  fmt.Sprintf("%v", &cfg),
		"%+v pointer": fmt.Sprintf("%+v", &cfg),
		"Println":     fmt.Sprintln(s, cfg),
		"slog":        logged.String(),
		"json config": string(asJSON),
	}
	for name, out := range outputs {
		if strings.Contains(out, canary) {
			t.Errorf("%s leaks the secret: %s", name, out)
		}
	}
	if !strings.Contains(outputs["%v secret"], "[redacted]") || !strings.Contains(outputs["slog"], "[redacted]") ||
		!strings.Contains(outputs["json config"], `"ConsoleEmail":"[redacted]"`) ||
		!strings.Contains(outputs["json config"], `"ConsolePasswordHash":"[redacted]"`) {
		t.Errorf("redacted marker missing: %q / %q", outputs["%v secret"], outputs["slog"])
	}
	// The value stays reachable for the code that needs it.
	if s.Reveal() != canary || string(cfg.TypesafeAPIKey) != canary || cfg.MailTo.Reveal() != canary+"@inbox.example" ||
		cfg.ConsoleEmail.Reveal() != canary+"@console.example" || cfg.ConsolePasswordHash.Reveal() != "$2a$12$"+canary {
		t.Error("Reveal or conversion lost the value")
	}
	if b, err := s.MarshalJSON(); err != nil || strings.Contains(string(b), canary) {
		t.Errorf("MarshalJSON = %s, %v", b, err)
	}
}
