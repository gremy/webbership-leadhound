package deploy

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// unit is one systemd unit file: each key's values, in file order, by section.
type unit map[string]map[string][]string

// readUnit parses the INI shape systemd uses: [Section] headers, Key=Value lines, # and ;
// comments. Line continuations are not used in these files and are refused.
func readUnit(t *testing.T, path string) unit {
	t.Helper()
	f, err := os.Open(path) // #nosec G304 -- the test's own deploy files
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	u := unit{}
	section := ""
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		switch {
		case line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";"):
			continue
		case strings.HasSuffix(line, `\`):
			t.Fatalf("%s:%d: line continuation", path, n)
		case strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]"):
			section = line[1 : len(line)-1]
			if u[section] == nil {
				u[section] = map[string][]string{}
			}
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok || section == "" {
			t.Fatalf("%s:%d: not a Key=Value line in a section", path, n)
		}
		u[section][strings.TrimSpace(k)] = append(u[section][strings.TrimSpace(k)], strings.TrimSpace(v))
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return u
}

// one is the single value of key in section, failing the test when there is not exactly one.
func (u unit) one(t *testing.T, name, section, key string) string {
	t.Helper()
	v := u[section][key]
	if len(v) != 1 {
		t.Fatalf("%s: [%s] %s = %q, want exactly one", name, section, key, v)
	}
	return v[0]
}

func units(t *testing.T, pattern string) map[string]unit {
	t.Helper()
	paths, err := filepath.Glob(pattern)
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) == 0 {
		t.Fatalf("no %s files", pattern)
	}
	out := map[string]unit{}
	for _, p := range paths {
		out[p] = readUnit(t, p)
	}
	return out
}

// schedule is each timer's OnCalendar lines and each service's ExecStart lines: the run
// order the deploy promises (README.md).
var schedule = map[string]struct {
	calendar []string
	exec     []string
}{
	"leadhound-ats": {
		[]string{"*-*-* 01:00:00 Europe/Bucharest"},
		[]string{
			"-/usr/bin/timeout --kill-after=5m 5h /opt/leadhound/bin/leadhound collect ats",
			"/usr/bin/flock --wait 5400 /opt/leadhound/data/.score.lock /opt/leadhound/bin/leadhound score",
		},
	},
	"leadhound-collect": {
		[]string{"*-*-* 06:30:00 Europe/Bucharest", "*-*-* 14:30:00 Europe/Bucharest"},
		[]string{
			"-" + linkedinLock + "1800 --conflict-exit-code 75 " + linkedinLockPath +
				" /usr/bin/timeout --kill-after=5m 2h /opt/leadhound/bin/leadhound collect linkedin",
			"/usr/bin/flock --wait 5400 /opt/leadhound/data/.score.lock /opt/leadhound/bin/leadhound score",
		},
	},
	"leadhound-prebrief": {
		[]string{"*-*-* 07:30:00 Europe/Bucharest"},
		[]string{linkedinLock + "2700 --conflict-exit-code 75 " + linkedinLockPath + " /opt/leadhound/bin/leadhound canary linkedin"},
	},
	"leadhound-brief": {
		[]string{"*-*-* 07:50:00 Europe/Bucharest"},
		[]string{linkedinLock + "1800 --conflict-exit-code 75 " + linkedinLockPath + " /opt/leadhound/bin/leadhound brief --send"},
	},
	"leadhound-backup": {
		[]string{"*-*-* 03:30:00 Europe/Bucharest"},
		[]string{"/opt/leadhound/bin/leadhound-backup"},
	},
}

// daemons are the services that run all the time rather than from a timer, with their
// ExecStart line.
var daemons = map[string]struct{ exec string }{
	"leadhound-console": {exec: "/opt/leadhound/bin/leadhound serve"},
}

// hardening is the sandbox every leadhound service but the backup shares.
var hardening = map[string]string{
	"User":            "leadhound",
	"Group":           "leadhound",
	"EnvironmentFile": "/etc/leadhound/env",
	"ProtectSystem":   "strict",
	"ReadWritePaths":  "/opt/leadhound/data",
	"ProtectHome":     "yes",
	"PrivateTmp":      "yes",
}

// linkedinLockPath is the one lock every LinkedIn step on the server holds, and
// linkedinLock is the flock call up to its wait in seconds.
const (
	linkedinLockPath = "/opt/leadhound/data/.linkedin.lock"
	linkedinLock     = "/usr/bin/flock --wait "
)

func base(path string) string {
	return strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
}

func TestTimers(t *testing.T) {
	timers := units(t, "*.timer")
	if len(timers) != len(schedule) {
		t.Errorf("%d timers, want %d", len(timers), len(schedule))
	}
	for path, u := range timers {
		name := base(path)
		want, ok := schedule[name]
		if !ok {
			t.Errorf("%s: not in the schedule", path)
			continue
		}
		if _, err := os.Stat(name + ".service"); err != nil {
			t.Errorf("%s: no matching %s.service", path, name)
		}
		cal := u["Timer"]["OnCalendar"]
		if !slices.Equal(cal, want.calendar) {
			t.Errorf("%s: OnCalendar = %q, want %q", path, cal, want.calendar)
		}
		for _, c := range cal {
			if !strings.HasSuffix(c, " Europe/Bucharest") {
				t.Errorf("%s: OnCalendar %q is not in Europe/Bucharest", path, c)
			}
		}
		if got := u.one(t, path, "Timer", "RandomizedDelaySec"); got != "600" {
			t.Errorf("%s: RandomizedDelaySec = %s, want 600", path, got)
		}
		if got := u.one(t, path, "Timer", "Persistent"); got != "true" {
			t.Errorf("%s: Persistent = %s, want true", path, got)
		}
		if got := u.one(t, path, "Install", "WantedBy"); got != "timers.target" {
			t.Errorf("%s: WantedBy = %s, want timers.target", path, got)
		}
	}
}

func TestServices(t *testing.T) {
	services := units(t, "*.service")
	if len(services) != len(schedule)+len(daemons) {
		t.Errorf("%d services, want %d", len(services), len(schedule)+len(daemons))
	}
	for path, u := range services {
		name := base(path)
		if _, ok := daemons[name]; ok {
			continue // TestConsoleService
		}
		want, ok := schedule[name]
		if !ok {
			t.Errorf("%s: not in the schedule", path)
			continue
		}
		if _, err := os.Stat(name + ".timer"); err != nil {
			t.Errorf("%s: no matching %s.timer", path, name)
		}
		if got := u.one(t, path, "Service", "Type"); got != "oneshot" {
			t.Errorf("%s: Type = %s, want oneshot", path, got)
		}
		exec := u["Service"]["ExecStart"]
		if !slices.Equal(exec, want.exec) {
			t.Errorf("%s: ExecStart = %q, want %q", path, exec, want.exec)
		}
		// A failed step never stops a later one: every step but the last ignores its exit
		// status, and the last one's is the unit's.
		for i, e := range exec {
			last := i == len(exec)-1
			if strings.HasPrefix(e, "-") == last {
				t.Errorf("%s: ExecStart %q: every step but the last starts with -", path, e)
			}
		}
		if got := u.one(t, path, "Service", "NoNewPrivileges"); got != "yes" {
			t.Errorf("%s: NoNewPrivileges = %s, want yes", path, got)
		}
		if name == "leadhound-backup" {
			continue
		}
		for key, want := range hardening {
			if got := u.one(t, path, "Service", key); got != want {
				t.Errorf("%s: %s = %s, want %s", path, key, got, want)
			}
		}
	}
}

// TestConsoleService: the console runs all the time, restarts after a crash but never after
// bad config (exit 2), shares the timers' sandbox and reaches only this machine.
func TestConsoleService(t *testing.T) {
	const path = "leadhound-console.service"
	u := readUnit(t, path)
	if _, err := os.Stat("leadhound-console.timer"); err == nil {
		t.Errorf("%s: has a matching .timer; the console is not timed", path)
	}
	for _, kv := range [][3]string{
		{"Service", "Type", "exec"},
		{"Service", "ExecStart", daemons["leadhound-console"].exec},
		{"Service", "Restart", "on-failure"},
		{"Service", "RestartPreventExitStatus", "2"},
		{"Service", "NoNewPrivileges", "yes"},
		{"Service", "IPAddressDeny", "any"},
		{"Service", "IPAddressAllow", "localhost"},
		{"Install", "WantedBy", "multi-user.target"},
	} {
		if got := u.one(t, path, kv[0], kv[1]); got != kv[2] {
			t.Errorf("%s: [%s] %s = %s, want %s", path, kv[0], kv[1], got, kv[2])
		}
	}
	for key, want := range hardening {
		if got := u.one(t, path, "Service", key); got != want {
			t.Errorf("%s: %s = %s, want %s", path, key, got, want)
		}
	}
}

// TestBriefRetriesAlone: only the brief retries, in its own unit, with a start limit that
// lets all three tries happen before it trips.
func TestBriefRetriesAlone(t *testing.T) {
	for path, u := range units(t, "*.service") {
		if _, ok := daemons[base(path)]; ok {
			continue // a daemon restarts after a crash: TestConsoleService
		}
		brief := base(path) == "leadhound-brief"
		sends := false
		for _, e := range u["Service"]["ExecStart"] {
			sends = sends || strings.Contains(e, " brief")
		}
		if sends != brief {
			t.Errorf("%s: runs brief = %t; only leadhound-brief.service may", path, sends)
		}
		if _, restarts := u["Service"]["Restart"]; restarts != brief {
			t.Errorf("%s: Restart set = %t; only leadhound-brief.service retries", path, restarts)
		}
		if !brief {
			continue
		}
		for key, want := range map[string]string{"Restart": "on-failure", "RestartSec": "10min", "RestartPreventExitStatus": "2"} {
			if got := u.one(t, path, "Service", key); got != want {
				t.Errorf("%s: %s = %s, want %s", path, key, got, want)
			}
		}
		if got := u.one(t, path, "Unit", "StartLimitBurst"); got != "3" {
			t.Errorf("%s: StartLimitBurst = %s, want 3", path, got)
		}
		interval, err := systemdDuration(u.one(t, path, "Unit", "StartLimitIntervalSec"))
		retry, rerr := systemdDuration(u.one(t, path, "Service", "RestartSec"))
		run, terr := systemdDuration(u.one(t, path, "Service", "TimeoutStartSec"))
		if err != nil || rerr != nil || terr != nil || interval <= 3*run+2*retry {
			t.Errorf("%s: StartLimitIntervalSec = %v (%v), want longer than three tries of %v, %v apart", path, interval, err, run, retry)
		}
	}
}

// systemdDuration reads a systemd time span in the forms these files use: a bare number of
// seconds, or Go-style units with "min" for minutes ("10min", "2h").
func systemdDuration(v string) (time.Duration, error) {
	if _, err := strconv.Atoi(v); err == nil {
		v += "s"
	}
	return time.ParseDuration(strings.ReplaceAll(v, "min", "m"))
}

// TestInstallEnablesEveryTimer: deploy/install.sh enables exactly the timers in deploy/, and
// enables and restarts every daemon after the new binary is in place, so the running server
// picks it up. The console is skipped while the env file has no password hash, so a deploy
// before the console is configured still succeeds.
func TestInstallEnablesEveryTimer(t *testing.T) {
	b, err := os.ReadFile("install.sh")
	if err != nil {
		t.Fatal(err)
	}
	enabled := regexp.MustCompile(`leadhound-[a-z]+\.timer`).FindAllString(string(b), -1)
	slices.Sort(enabled)
	enabled = slices.Compact(enabled)
	var timers []string
	for path := range units(t, "*.timer") {
		timers = append(timers, filepath.Base(path))
	}
	slices.Sort(timers)
	if !slices.Equal(enabled, timers) {
		t.Errorf("install.sh enables %q, want %q", enabled, timers)
	}

	steps := installSteps(t)
	swap := firstLine(steps, "mv -f", `"$bin/.leadhound.new"`, `"$bin/leadhound"`)
	for name := range daemons {
		enable := firstLine(steps, "systemctl enable "+name+".service")
		restart := firstLine(steps, "systemctl restart "+name+".service")
		switch {
		case enable < 0 || restart < 0:
			t.Errorf("install.sh does not enable and restart %s.service", name)
		case enable < swap || restart < swap:
			t.Errorf("install.sh enables or restarts %s.service before the binary swap (line %d)", name, swap)
		}
	}
	guard := firstLine(steps, "grep", "LEADHOUND_CONSOLE_PASSWORD_HASH=", "/etc/leadhound/env")
	enable := firstLine(steps, "systemctl enable leadhound-console.service")
	switch {
	case guard < 0:
		t.Error("install.sh does not check /etc/leadhound/env for LEADHOUND_CONSOLE_PASSWORD_HASH= before starting the console")
	case enable >= 0 && guard > enable:
		t.Errorf("install.sh checks for the password hash (line %d) after enabling the console (line %d)", guard, enable)
	}
	if healthz := firstLine(steps, "curl", "/healthz"); healthz < 0 || healthz < firstLine(steps, "systemctl restart leadhound-console.service") {
		t.Error("install.sh does not wait for the console's /healthz after restarting it")
	}
	if firstLine(steps, "journalctl -u leadhound-console.service") < 0 {
		t.Error("install.sh does not show the console's log when it does not answer")
	}
}

// configVar matches the environment variables the program reads.
var configVar = regexp.MustCompile(`"((?:LEADHOUND|TYPESAFE|RESEND)_[A-Z0-9_]+)"`)

// TestEnvExamplesCoverConfig: every variable the program reads has a line in env.example,
// except the owner role's URL, which only migrate.env.example carries.
func TestEnvExamplesCoverConfig(t *testing.T) {
	vars := map[string]bool{}
	for _, src := range []string{"../internal/config/config.go", "../internal/store/store.go"} {
		b, err := os.ReadFile(src) // #nosec G304 -- the repository's own source
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range configVar.FindAllStringSubmatch(string(b), -1) {
			vars[m[1]] = true
		}
	}
	if len(vars) < 10 {
		t.Fatalf("found only %d variables in the config source", len(vars))
	}
	set := func(file string) map[string]string {
		b, err := os.ReadFile(file) // #nosec G304 -- the test's own deploy files
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]string{}
		for line := range strings.Lines(string(b)) {
			if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok && !strings.HasPrefix(k, "#") {
				out[k] = v
			}
		}
		return out
	}
	env, migrate := set("env.example"), set("migrate.env.example")
	const ownerURL = "LEADHOUND_MIGRATE_DATABASE_URL"
	for v := range vars {
		switch _, inEnv := env[v]; {
		case v == ownerURL && inEnv:
			t.Errorf("env.example sets %s: the timers must never hold the owner role", v)
		case v == ownerURL:
		case !inEnv:
			t.Errorf("env.example has no line for %s", v)
		}
	}
	if _, ok := migrate[ownerURL]; !ok || len(migrate) != 1 {
		t.Errorf("migrate.env.example sets %v, want only %s", migrate, ownerURL)
	}
	for k, want := range map[string]string{
		"LEADHOUND_LIVE":               "1",
		"LEADHOUND_JEV_DAILY_CALL_CAP": "10000",
		"LEADHOUND_TZ":                 "Europe/Bucharest",
		"LEADHOUND_DATA_DIR":           "/opt/leadhound/data",
	} {
		if env[k] != want {
			t.Errorf("env.example %s = %q, want %q", k, env[k], want)
		}
	}
}

var (
	ipv4 = regexp.MustCompile(`\b\d{1,3}(?:\.\d{1,3}){3}\b`)
	// An IPv6 literal: hex groups and colons with a "::" or a hex letter, so a time such as
	// 01:00:00 is not one.
	ipv6 = regexp.MustCompile(`(?i)\b[0-9a-f]{0,4}(?::[0-9a-f]{0,4}){2,7}\b`)
	// A dotted name ending in a letters-only label: a host name or a file name.
	dotted = regexp.MustCompile(`(?i)\b[a-z0-9-]+(?:\.[a-z0-9-]+)*\.[a-z]{2,}\b`)
)

// fileSuffixes are the last labels of the file and unit names these files mention.
var fileSuffixes = []string{
	"yaml", "yml", "sql", "sh", "service", "timer", "target", "example", "env", "md", "dump",
	"csv", "lock", "new", "part", "txt", "json", "go",
}

// TestNoHostsOrAddresses: nothing in deploy/ names a real host or address. The only host
// names allowed are reserved ones (example, invalid, localhost).
func TestNoHostsOrAddresses(t *testing.T) {
	paths, err := filepath.Glob("*")
	if err != nil {
		t.Fatal(err)
	}
	sql, _ := filepath.Glob("sql/*")
	for _, path := range append(paths, sql...) {
		if fi, err := os.Stat(path); err != nil || fi.IsDir() || filepath.Ext(path) == ".go" {
			continue
		}
		b, err := os.ReadFile(path) // #nosec G304 -- the test's own deploy files
		if err != nil {
			t.Fatal(err)
		}
		for n, line := range strings.Split(string(b), "\n") {
			if ipv4.MatchString(line) {
				t.Errorf("%s:%d: an IPv4 address", path, n+1)
			}
			for _, m := range ipv6.FindAllString(line, -1) {
				if strings.Contains(m, "::") || strings.ContainsAny(strings.ToLower(m), "abcdef") {
					t.Errorf("%s:%d: an IPv6 address", path, n+1)
				}
			}
			for _, m := range dotted.FindAllString(line, -1) {
				labels := strings.Split(strings.ToLower(m), ".")
				// A Go name such as store.Migrate: its last part starts with a capital.
				if last := m[strings.LastIndex(m, ".")+1:]; last[0] >= 'A' && last[0] <= 'Z' {
					continue
				}
				if slices.Contains(fileSuffixes, labels[len(labels)-1]) ||
					slices.Contains(labels, "example") || slices.Contains(labels, "invalid") {
					continue
				}
				t.Errorf("%s:%d: %q looks like a host name; use a reserved .example or .invalid one", path, n+1, m)
			}
		}
	}
}

// installSteps is deploy/install.sh as its lines, comments left out.
func installSteps(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile("install.sh")
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for line := range strings.Lines(string(b)) {
		if l := strings.TrimSpace(line); l != "" && !strings.HasPrefix(l, "#") {
			out = append(out, l)
		}
	}
	return out
}

// firstLine is the index of the first of lines containing every one of parts, or -1.
func firstLine(lines []string, parts ...string) int {
	for i, l := range lines {
		ok := true
		for _, p := range parts {
			ok = ok && strings.Contains(l, p)
		}
		if ok {
			return i
		}
	}
	return -1
}

// TestInstallMigratesBeforeSwap: the new binary runs migrate before it replaces the old one,
// and the units change only after it succeeds, so a failed migrate leaves the old release.
func TestInstallMigratesBeforeSwap(t *testing.T) {
	steps := installSteps(t)
	migrate := firstLine(steps, `"$bin/.leadhound.new" migrate`)
	swap := firstLine(steps, "mv -f", `"$bin/.leadhound.new"`, `"$bin/leadhound"`)
	units := firstLine(steps, "/etc/systemd/system/")
	switch {
	case migrate < 0:
		t.Fatal(`install.sh does not run migrate with "$bin/.leadhound.new"`)
	case swap < 0:
		t.Fatal(`install.sh does not mv "$bin/.leadhound.new" into "$bin/leadhound"`)
	case swap < migrate:
		t.Errorf("install.sh swaps the binary (line %d) before migrate (line %d)", swap, migrate)
	case units < migrate:
		t.Errorf("install.sh installs the units (line %d) before migrate (line %d)", units, migrate)
	}
	if i := firstLine(steps, `"$bin/leadhound" migrate`); i >= 0 {
		t.Errorf("install.sh migrates with the installed binary: %q", steps[i])
	}
}

// TestInstallLocksEnvFiles: install.sh makes both env files root:root 0600 before anything
// reads them.
func TestInstallLocksEnvFiles(t *testing.T) {
	steps := installSteps(t)
	migrate := firstLine(steps, `"$bin/.leadhound.new" migrate`)
	for _, f := range []string{"/etc/leadhound/env", "/etc/leadhound/migrate.env"} {
		for _, cmd := range []string{"chown root:root", "chmod 0600"} {
			i := slices.IndexFunc(steps, func(l string) bool {
				return strings.HasPrefix(l, cmd+" ") && slices.Contains(strings.Fields(l), f)
			})
			switch {
			case i < 0:
				t.Errorf("install.sh has no %s for %s", cmd, f)
			case migrate >= 0 && i > migrate:
				t.Errorf("install.sh runs %s %s after migrate", cmd, f)
			}
		}
	}
}

// TestLinkedInLock: every step that reaches LinkedIn (a LinkedIn collect, the canary, the
// brief with its recheck) holds the same lock, and a lock timeout exits with a code the
// brief's retry does not treat as bad config. The brief and the canary are ordered after
// the steps they read, for a catch-up after downtime.
func TestLinkedInLock(t *testing.T) {
	lock := regexp.MustCompile(`^-?/usr/bin/flock --wait (\d+) --conflict-exit-code (\d+) (\S+) `)
	seen := 0
	for path, u := range units(t, "*.service") {
		for _, e := range u["Service"]["ExecStart"] {
			if !strings.Contains(e, " collect linkedin") && !strings.Contains(e, " canary linkedin") && !strings.Contains(e, " brief") {
				continue
			}
			seen++
			m := lock.FindStringSubmatch(e)
			if m == nil {
				t.Errorf("%s: %q does not hold the LinkedIn lock", path, e)
				continue
			}
			if m[3] != linkedinLockPath {
				t.Errorf("%s: LinkedIn lock %s, want %s", path, m[3], linkedinLockPath)
			}
			if prevent := u["Service"]["RestartPreventExitStatus"]; slices.Contains(prevent, m[2]) || m[2] == "0" {
				t.Errorf("%s: lock timeout exits %s, which the unit treats as success or as final", path, m[2])
			}
		}
	}
	if seen != 3 {
		t.Errorf("%d LinkedIn steps, want 3 (collect, canary, brief)", seen)
	}
	for name, want := range map[string][]string{
		"leadhound-brief":    {"leadhound-collect.service", "leadhound-prebrief.service"},
		"leadhound-prebrief": {"leadhound-collect.service"},
	} {
		after := strings.Fields(strings.Join(readUnit(t, name+".service")["Unit"]["After"], " "))
		for _, w := range want {
			if !slices.Contains(after, w) {
				t.Errorf("%s.service: After = %q, want it to include %s", name, after, w)
			}
		}
	}
}

// TestTimeoutsCoverWaits: each unit's TimeoutStartSec is longer than every lock wait and
// command timeout in its steps together, so systemd never cuts the last step short.
func TestTimeoutsCoverWaits(t *testing.T) {
	wait := regexp.MustCompile(`--wait (\d+) `)
	timeout := regexp.MustCompile(`/usr/bin/timeout (?:--kill-after=(\S+) )?(\S+) `)
	for path, u := range units(t, "*.service") {
		var sum time.Duration
		for _, e := range u["Service"]["ExecStart"] {
			if m := wait.FindStringSubmatch(e); m != nil {
				n, _ := strconv.Atoi(m[1])
				sum += time.Duration(n) * time.Second
			}
			if m := timeout.FindStringSubmatch(e); m != nil {
				for _, d := range m[1:] {
					if d == "" {
						continue
					}
					v, err := time.ParseDuration(d)
					if err != nil {
						t.Fatalf("%s: timeout %q: %v", path, d, err)
					}
					sum += v
				}
			}
		}
		got, err := systemdDuration(u.one(t, path, "Service", "TimeoutStartSec"))
		if err != nil || got <= sum {
			t.Errorf("%s: TimeoutStartSec = %v (%v), want longer than the waits and timeouts' %v", path, got, err, sum)
		}
	}
}

// TestMemoryCeiling: the server is shared, so every leadhound unit but the backup has a
// memory ceiling, and the long collects take half the CPU share.
func TestMemoryCeiling(t *testing.T) {
	for path, u := range units(t, "*.service") {
		name := base(path)
		if name == "leadhound-backup" {
			continue
		}
		if got := u.one(t, path, "Service", "MemoryMax"); got != "1200M" {
			t.Errorf("%s: MemoryMax = %s, want 1200M", path, got)
		}
		if got := u.one(t, path, "Service", "MemoryHigh"); got != "900M" {
			t.Errorf("%s: MemoryHigh = %s, want 900M", path, got)
		}
		if name == "leadhound-ats" || name == "leadhound-collect" {
			if got := u.one(t, path, "Service", "CPUWeight"); got != "50" {
				t.Errorf("%s: CPUWeight = %s, want 50", path, got)
			}
		}
	}
}
