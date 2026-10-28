package deploy

import (
	"bufio"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/leadconfig"
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

// schedule is each timer's OnCalendar lines, whether it is persistent, and each service's
// ExecStart lines: the run order the deploy promises (README.md).
var schedule = map[string]struct {
	calendar   []string
	persistent bool
	exec       []string
}{
	"leadhound-ats": {
		[]string{"*-*-* 01:00:00 Europe/Bucharest"},
		true,
		[]string{
			"-/usr/bin/timeout --kill-after=5m 4h /opt/leadhound/bin/leadhound collect ats",
			scoreLock + "/usr/bin/timeout --kill-after=2m 140m /opt/leadhound/bin/leadhound score --max-duration 2h",
		},
	},
	"leadhound-collect": {
		[]string{"*-*-* 06:30:00 Europe/Bucharest", "*-*-* 14:30:00 Europe/Bucharest"},
		true,
		[]string{
			"-" + linkedinLock + "1800 --conflict-exit-code 75 " + linkedinLockPath +
				" /usr/bin/timeout --kill-after=5m 2h /opt/leadhound/bin/leadhound collect linkedin",
			scoreLock + "/usr/bin/timeout --kill-after=2m 1h /opt/leadhound/bin/leadhound score --max-duration 45m",
		},
	},
	"leadhound-prebrief": {
		[]string{"*-*-* 07:30:00 Europe/Bucharest"},
		true,
		[]string{linkedinLock + "2700 --conflict-exit-code 75 " + linkedinLockPath + " /opt/leadhound/bin/leadhound canary linkedin"},
	},
	"leadhound-brief": {
		[]string{"*-*-* 07:50:00 Europe/Bucharest"},
		true,
		[]string{linkedinLock + "1800 --conflict-exit-code 75 " + linkedinLockPath + " /opt/leadhound/bin/leadhound brief --send"},
	},
	"leadhound-backup": {
		[]string{"*-*-* 03:30:00 Europe/Bucharest"},
		true,
		[]string{"/opt/leadhound/bin/leadhound-backup"},
	},
	"leadhound-trim": {
		[]string{"*-*-* 15:00:00 Europe/Bucharest"},
		true,
		[]string{"/usr/bin/timeout --kill-after=2m 50m /opt/leadhound/bin/leadhound trim --max-duration 45m"},
	},
	"leadhound-leads-probe": {
		[]string{"*-*-* 13:00:00 Europe/Bucharest"},
		true,
		[]string{leadsProbeLock + "/usr/bin/timeout --kill-after=5m 3h /opt/leadhound/bin/leadhound leads sites probe --max 10000"},
	},
	"leadhound-leads-contacts": {
		[]string{"*-*-* 10:00:00 Europe/Bucharest"},
		true,
		[]string{
			"-" + leadsFetchLock + "/usr/bin/timeout --kill-after=5m 2h /opt/leadhound/bin/leadhound leads contacts --max 300",
			"-" + leadsFetchLock + "/usr/bin/timeout --kill-after=5m 60m /opt/leadhound/bin/leadhound leads enrich --max 300",
			"-/usr/bin/timeout --kill-after=5m 180m /opt/leadhound/bin/leadhound leads rate --max 10000",
			"/usr/bin/timeout --kill-after=5m 20m /opt/leadhound/bin/leadhound leads build",
		},
	},
	"leadhound-leads-batch": {
		[]string{"*-*-* 06:15:00 Europe/Bucharest"},
		true,
		[]string{
			"-/usr/bin/timeout --kill-after=5m 20m /opt/leadhound/bin/leadhound leads build",
			"/usr/bin/timeout --kill-after=5m 20m /opt/leadhound/bin/leadhound leads batch new",
		},
	},
	// Not persistent: a send missed while the server was down is never caught up outside the
	// recipients' business hours, and the next sync comes within 15 minutes anyway.
	"leadhound-leads-send": {
		[]string{"Mon..Fri *-*-* 10..19:00:00 Europe/Bucharest"},
		false,
		[]string{"/usr/bin/timeout --kill-after=5m 225m /opt/leadhound/bin/leadhound leads send --max 25"},
	},
	"leadhound-leads-sync": {
		[]string{"*-*-* *:00/15:00 Europe/Bucharest"},
		false,
		[]string{"/usr/bin/timeout --kill-after=5m 10m /opt/leadhound/bin/leadhound leads sync"},
	},
}

// leadsFinding and leadsSending are the leads timers install.sh enables only as
// /etc/leadhound/env switches them on: the finding ones with LEADHOUND_LEADS_TIMERS=on, the
// sending ones only with that, LEADHOUND_LEADS_SEND=on and live outreach (the mailboxes live
// in the database, so no env line names one).
var (
	leadsFinding = []string{"leadhound-leads-contacts.timer", "leadhound-leads-probe.timer"}
	leadsSending = []string{"leadhound-leads-batch.timer", "leadhound-leads-send.timer", "leadhound-leads-sync.timer"}
	// leadsTriggered are the leads services the console starts through a watcher, with no
	// timer: install.sh arms their watchers on every deploy, whatever the env switches say.
	leadsTriggered = []string{"leadhound-leads-research.service"}
)

// daemons are the services that run all the time rather than from a timer, with their
// ExecStart line.
var daemons = map[string]struct{ exec string }{
	"leadhound-console": {exec: "/opt/leadhound/bin/leadhound serve"},
}

// triggered are the services the console's Run now card starts through a same-named .path
// watcher rather than a timer: their one ExecStart line, and the Wants= and After= units they
// must name (the fetch pulls in the timed LinkedIn collect whole and follows it).
var triggered = map[string]struct {
	exec         string
	wants, after []string
}{
	"leadhound-preview": {
		exec:  linkedinLock + "1800 --conflict-exit-code 75 " + linkedinLockPath + " /opt/leadhound/bin/leadhound preview",
		after: []string{"network-online.target", "postgresql.service"},
	},
	"leadhound-fetch": {
		exec:  linkedinLock + "1800 --conflict-exit-code 75 " + linkedinLockPath + " /opt/leadhound/bin/leadhound preview --after-fetch",
		wants: []string{"leadhound-collect.service"},
		after: []string{"network-online.target", "postgresql.service", "leadhound-collect.service"},
	},
	// The console's Mailboxes page asks for a check here: the console never holds a mailbox
	// password, so the login test runs in this job, which gets the key.
	"leadhound-mailbox-check": {
		exec:  "/usr/bin/timeout --kill-after=1m 5m /opt/leadhound/bin/leadhound leads mailboxes check --requested",
		after: []string{"network-online.target", "postgresql.service"},
	},
	// Starring a lead with no usable route, or "Look again", asks for contact research here:
	// the console never holds the Anthropic key, so the Claude call runs in this job, which
	// gets it.
	"leadhound-leads-research": {
		exec:  "/usr/bin/timeout --kill-after=5m 45m /opt/leadhound/bin/leadhound leads research run --max 20",
		after: []string{"network-online.target", "postgresql.service"},
	},
}

// runActions are the console's requests (the Run now actions, the mailbox check and contact
// research): each has
// a request file /opt/leadhound/data/run/<action> and a watcher leadhound-<action>.path that
// starts leadhound-<action>.service.
var runActions = []string{"preview", "fetch", "brief", "ats", "mailbox-check", "leads-research"}

// runDir is where the console writes the Run now requests.
const runDir = "/opt/leadhound/data/run"

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
// linkedinLock is the flock call up to its wait in seconds. scoreLock is the flock call every
// score step starts with, up to the command it runs.
const (
	linkedinLockPath = "/opt/leadhound/data/.linkedin.lock"
	linkedinLock     = "/usr/bin/flock --wait "
	scoreLock        = "/usr/bin/flock --wait 5400 /opt/leadhound/data/.score.lock "
	// leadsFetchLockPath is the one lock every leads step that reads the public web holds.
	leadsFetchLockPath = "/opt/leadhound/data/.leads-fetch.lock"
	// leadsFetchLock is the flock call the contacts unit's web steps start with: no wait, and
	// exit 75 when the probe holds it.
	leadsFetchLock = "/usr/bin/flock --nonblock --conflict-exit-code 75 " + leadsFetchLockPath + " "
	// leadsProbeLock is the probe's flock call: it waits out the contacts unit's web steps
	// (TestProbeWaitsOutContacts) rather than lose its day, and fails when the wait runs out.
	leadsProbeLock = "/usr/bin/flock --wait 11700 --conflict-exit-code 75 " + leadsFetchLockPath + " "
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
		if got := u.one(t, path, "Timer", "Persistent"); got != strconv.FormatBool(want.persistent) {
			t.Errorf("%s: Persistent = %s, want %t", path, got, want.persistent)
		}
		if got := u.one(t, path, "Install", "WantedBy"); got != "timers.target" {
			t.Errorf("%s: WantedBy = %s, want timers.target", path, got)
		}
	}
}

func TestServices(t *testing.T) {
	services := units(t, "*.service")
	if want := len(schedule) + len(daemons) + len(triggered); len(services) != want {
		t.Errorf("%d services, want %d", len(services), want)
	}
	for path, u := range services {
		name := base(path)
		if _, ok := daemons[name]; ok {
			continue // TestConsoleService
		}
		var wantExec []string
		if tr, ok := triggered[name]; ok {
			// Started by the console's Run now card through its watcher, never by a timer.
			if _, err := os.Stat(name + ".path"); err != nil {
				t.Errorf("%s: no matching %s.path", path, name)
			}
			if _, err := os.Stat(name + ".timer"); err == nil {
				t.Errorf("%s: has a matching .timer; it runs only when the console asks", path)
			}
			for key, want := range map[string][]string{"Wants": tr.wants, "After": tr.after} {
				got := strings.Fields(strings.Join(u["Unit"][key], " "))
				for _, w := range want {
					if !slices.Contains(got, w) {
						t.Errorf("%s: %s = %q, want it to include %s", path, key, got, w)
					}
				}
			}
			wantExec = []string{tr.exec}
		} else {
			want, ok := schedule[name]
			if !ok {
				t.Errorf("%s: not in the schedule", path)
				continue
			}
			if _, err := os.Stat(name + ".timer"); err != nil {
				t.Errorf("%s: no matching %s.timer", path, name)
			}
			wantExec = want.exec
		}
		if got := u.one(t, path, "Service", "Type"); got != "oneshot" {
			t.Errorf("%s: Type = %s, want oneshot", path, got)
		}
		exec := u["Service"]["ExecStart"]
		if !slices.Equal(exec, wantExec) {
			t.Errorf("%s: ExecStart = %q, want %q", path, exec, wantExec)
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
		{"Service", "CPUQuota", "50%"},
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

// gnuDurationRE is the DURATION GNU coreutils timeout accepts (for the duration and for
// --kill-after): one number with at most one suffix, s, m, h or d, seconds when bare. A
// compound form Go's time.ParseDuration takes, such as "2h20m", makes timeout exit 125.
var gnuDurationRE = regexp.MustCompile(`^([0-9]+(?:\.[0-9]+)?)([smhd]?)$`)

// gnuDuration reads a /usr/bin/timeout DURATION, refusing what GNU timeout refuses, so no
// test reads a timeout value more leniently than the server does.
func gnuDuration(v string) (time.Duration, error) {
	m := gnuDurationRE.FindStringSubmatch(v)
	if m == nil {
		return 0, fmt.Errorf("%q is not a GNU timeout duration (one number, one optional s|m|h|d suffix)", v)
	}
	n, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0, err
	}
	unit := map[string]time.Duration{"": time.Second, "s": time.Second, "m": time.Minute, "h": time.Hour, "d": 24 * time.Hour}[m[2]]
	return time.Duration(n * float64(unit)), nil
}

// TestTimeoutDurationsAreGNU: every /usr/bin/timeout in every unit's ExecStart takes only
// options GNU timeout knows here (--kill-after=DURATION) and a DURATION in GNU's syntax. Go's
// time.ParseDuration accepts "2h20m"; GNU timeout does not, and the step never starts.
func TestTimeoutDurationsAreGNU(t *testing.T) {
	n := 0
	for path, u := range units(t, "*.service") {
		for _, e := range u["Service"]["ExecStart"] {
			fields := strings.Fields(e)
			for i, f := range fields {
				if strings.TrimPrefix(f, "-") != "/usr/bin/timeout" {
					continue
				}
				n++
				j := i + 1
				for ; j < len(fields) && strings.HasPrefix(fields[j], "-"); j++ {
					v, ok := strings.CutPrefix(fields[j], "--kill-after=")
					if !ok {
						t.Errorf("%s: %q: timeout option %q, want only --kill-after=DURATION", path, e, fields[j])
						continue
					}
					if !gnuDurationRE.MatchString(v) {
						t.Errorf("%s: %q: --kill-after=%s is not a GNU timeout duration (one number, one optional s|m|h|d suffix)", path, e, v)
					}
				}
				if j >= len(fields) {
					t.Errorf("%s: %q: timeout has no DURATION", path, e)
					continue
				}
				if !gnuDurationRE.MatchString(fields[j]) {
					t.Errorf("%s: %q: timeout DURATION %s is not GNU syntax (one number, one optional s|m|h|d suffix)", path, e, fields[j])
				}
			}
		}
	}
	if n == 0 {
		t.Fatal("no /usr/bin/timeout found in any unit")
	}
}

func TestGNUDuration(t *testing.T) {
	for v, want := range map[string]time.Duration{
		"140m": 140 * time.Minute, "225m": 225 * time.Minute, "4h": 4 * time.Hour,
		"2.5h": 150 * time.Minute, "90": 90 * time.Second, "1d": 24 * time.Hour,
	} {
		if got, err := gnuDuration(v); err != nil || got != want {
			t.Errorf("gnuDuration(%q) = %v, %v; want %v", v, got, err, want)
		}
	}
	for _, v := range []string{"2h20m", "3h45m", "1h30m0s", "10min", "", "h", "-5m", "5ms"} {
		if _, err := gnuDuration(v); err == nil {
			t.Errorf("gnuDuration(%q) accepted; GNU timeout refuses it", v)
		}
	}
}

// TestInstallEnablesEveryTimer: deploy/install.sh names exactly the timers in deploy/, enables
// every one but the leads timers on every deploy (TestInstallLeadsTimers decides those), and
// enables and restarts every daemon after the new binary is in place, so the running server
// picks it up. The console is skipped while the env file has no password hash, so a deploy
// before the console is configured still succeeds.
func TestInstallEnablesEveryTimer(t *testing.T) {
	b, err := os.ReadFile("install.sh")
	if err != nil {
		t.Fatal(err)
	}
	timerName := regexp.MustCompile(`leadhound-[a-z-]+\.timer`)
	named := timerName.FindAllString(string(b), -1)
	slices.Sort(named)
	named = slices.Compact(named)
	var timers, always []string
	for path := range units(t, "*.timer") {
		name := filepath.Base(path)
		timers = append(timers, name)
		if !slices.Contains(leadsFinding, name) && !slices.Contains(leadsSending, name) {
			always = append(always, name)
		}
	}
	slices.Sort(timers)
	slices.Sort(always)
	if !slices.Equal(named, timers) {
		t.Errorf("install.sh names %q, want %q", named, timers)
	}
	if want := len(leadsFinding) + len(leadsSending) + len(always); len(timers) != want {
		t.Errorf("%d timers, want %d: a leads timer in the test's sets has no file", len(timers), want)
	}

	steps := installSteps(t)
	// The unconditional enable: its line and the continuation after it, outside leads_timers.
	start := slices.IndexFunc(steps, func(l string) bool {
		return strings.HasPrefix(l, "systemctl enable --now leadhound-") && !strings.Contains(l, "leads")
	})
	if start < 0 {
		t.Fatal("install.sh has no systemctl enable --now for the timers")
	}
	line := steps[start]
	for i := start; strings.HasSuffix(steps[i], `\`) && i+1 < len(steps); i++ {
		line += " " + steps[i+1]
	}
	unconditional := timerName.FindAllString(line, -1)
	slices.Sort(unconditional)
	if !slices.Equal(unconditional, always) {
		t.Errorf("install.sh enables %q on every deploy, want %q", unconditional, always)
	}
	if call := firstLine(steps, "leads_timers /etc/leadhound/env"); call < 0 || call < firstLine(steps, "systemctl daemon-reload") {
		t.Error("install.sh does not run leads_timers /etc/leadhound/env after systemctl daemon-reload")
	}

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
	for _, src := range []string{"../internal/config/config.go", "../internal/store/store.go", "../internal/leadconfig/env.go", "../internal/secretbox/secretbox.go"} {
		b, err := os.ReadFile(src) // #nosec G304 -- the repository's own source
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range configVar.FindAllStringSubmatch(string(b), -1) {
			vars[m[1]] = true
		}
	}
	// The old mailbox lines are named in env.go only to say they are no longer read.
	for _, v := range leadconfig.RetiredEnv {
		delete(vars, v)
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
	for _, v := range leadconfig.RetiredEnv {
		if _, ok := env[v]; ok {
			t.Errorf("env.example sets %s, which is no longer read: mailboxes are added in the console", v)
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
	"csv", "lock", "new", "part", "txt", "json", "go", "path", "key",
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
		for _, f := range hostFindings(path, string(b)) {
			t.Error(f)
		}
	}
}

// TestHostFindingsSQL: in a .sql file a schema-qualified name such as public.org is not a
// host, and a real host in a comment still is.
func TestHostFindingsSQL(t *testing.T) {
	host := "contoso" + "." + "org" // built at runtime so the repo holds no host name
	for _, tc := range []struct {
		path, line string
		want       int
	}{
		{"sql/grants.sql", "REVOKE UPDATE ON public.org FROM leadhound_app;", 0},
		{"sql/grants.sql", "IF to_regclass('public.site') IS NOT NULL THEN", 0},
		{"sql/grants.sql", "-- see " + host, 1},
		{"sql/grants.sql", "-- see www." + host + " and public.org", 1},
		{"README.md", "public.org", 1}, // the schema rule is for .sql files only
	} {
		if got := hostFindings(tc.path, tc.line); len(got) != tc.want {
			t.Errorf("%s %q: findings %v, want %d", tc.path, tc.line, got, tc.want)
		}
	}
}

// hostFindings returns one message per IPv4 address, IPv6 address or non-reserved host name
// in text, the contents of path.
func hostFindings(path, text string) []string {
	var out []string
	isSQL := filepath.Ext(path) == ".sql"
	for n, line := range strings.Split(text, "\n") {
		if ipv4.MatchString(line) {
			out = append(out, fmt.Sprintf("%s:%d: an IPv4 address", path, n+1))
		}
		for _, m := range ipv6.FindAllString(line, -1) {
			if strings.Contains(m, "::") || strings.ContainsAny(strings.ToLower(m), "abcdef") {
				out = append(out, fmt.Sprintf("%s:%d: an IPv6 address", path, n+1))
			}
		}
		for _, m := range dotted.FindAllString(line, -1) {
			labels := strings.Split(strings.ToLower(m), ".")
			// A Go name such as store.Migrate: its last part starts with a capital.
			if last := m[strings.LastIndex(m, ".")+1:]; last[0] >= 'A' && last[0] <= 'Z' {
				continue
			}
			// A schema-qualified SQL name such as public.org.
			if isSQL && len(labels) == 2 && labels[0] == "public" {
				continue
			}
			if slices.Contains(fileSuffixes, labels[len(labels)-1]) ||
				slices.Contains(labels, "example") || slices.Contains(labels, "invalid") {
				continue
			}
			out = append(out, fmt.Sprintf("%s:%d: %q looks like a host name; use a reserved .example or .invalid one", path, n+1, m))
		}
	}
	return out
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

// TestInstallAppliesLimitsBeforeMigrate: install.sh applies sql/limits.sql as the postgres
// user, with root opening the file onto psql's stdin (the stage is 0700, so postgres cannot
// open a path inside it), and stops the deploy on a failure, before the migrate runs.
func TestInstallAppliesLimitsBeforeMigrate(t *testing.T) {
	steps := installSteps(t)
	limits := firstLine(steps, "runuser -u postgres --", "psql", "ON_ERROR_STOP=1", `<"$stage/limits.sql"`)
	migrate := firstLine(steps, `"$bin/.leadhound.new" migrate`)
	switch {
	case limits < 0:
		t.Fatal(`install.sh does not run psql as postgres with <"$stage/limits.sql" on stdin`)
	case migrate < 0:
		t.Fatal(`install.sh does not run migrate with "$bin/.leadhound.new"`)
	case limits > migrate:
		t.Errorf("install.sh applies the limits (line %d) after migrate (line %d)", limits, migrate)
	}
	if i := firstLine(steps, "-f", "limits.sql"); i >= 0 {
		t.Errorf("install.sh has psql open limits.sql itself, which postgres cannot read in the stage: %q", steps[i])
	}
	if limits >= 0 && !strings.Contains(steps[limits], "|| fail") && (limits+1 >= len(steps) || !strings.HasPrefix(steps[limits+1], "fail ")) {
		t.Errorf("install.sh does not stop when the limits fail to apply: %q", steps[limits])
	}
}

// TestDeployCopiesLimits: make deploy copies sql/limits.sql into the stage, where install.sh
// reads it.
func TestDeployCopiesLimits(t *testing.T) {
	b, err := os.ReadFile("../Makefile")
	if err != nil {
		t.Fatal(err)
	}
	var recipe []string
	in := false
	for line := range strings.Lines(string(b)) {
		switch {
		case strings.HasPrefix(line, "deploy:"):
			in = true
		case in && strings.HasPrefix(line, "\t"):
			recipe = append(recipe, line)
		case in:
			in = false
		}
	}
	if len(recipe) == 0 {
		t.Fatal("the Makefile has no deploy recipe")
	}
	for _, l := range recipe {
		if strings.Contains(l, "$(SCP)") && slices.Contains(strings.Fields(l), "deploy/sql/limits.sql") {
			return
		}
	}
	t.Error("make deploy does not copy deploy/sql/limits.sql to the stage")
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
			if !strings.Contains(e, " collect linkedin") && !strings.Contains(e, " canary linkedin") && !strings.Contains(e, " brief") && !strings.Contains(e, " preview") {
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
	if seen != 5 {
		t.Errorf("%d LinkedIn steps, want 5 (collect, canary, brief, preview, fetch's preview)", seen)
	}
	for name, want := range map[string][]string{
		"leadhound-brief":    {"leadhound-collect.service", "leadhound-prebrief.service"},
		"leadhound-prebrief": {"leadhound-collect.service"},
		"leadhound-fetch":    {"leadhound-collect.service"},
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
					v, err := gnuDuration(d)
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

// memory is each leadhound service's ceiling and its Go heap target: MemoryMax in systemd's
// units (M and G are 1024-based) and GOMEMLIMIT in Go's.
var memory = map[string]struct{ max, goLimit string }{
	"leadhound-console":        {"300M", "200MiB"},
	"leadhound-brief":          {"600M", "450MiB"},
	"leadhound-prebrief":       {"600M", "450MiB"},
	"leadhound-preview":        {"600M", "450MiB"},
	"leadhound-fetch":          {"600M", "450MiB"},
	"leadhound-collect":        {"1G", "850MiB"},
	"leadhound-ats":            {"1200M", "900MiB"},
	"leadhound-trim":           {"300M", "200MiB"},
	"leadhound-leads-probe":    {"600M", "450MiB"},
	"leadhound-leads-contacts": {"600M", "450MiB"},
	"leadhound-leads-batch":    {"600M", "450MiB"},
	"leadhound-leads-send":     {"600M", "450MiB"},
	"leadhound-leads-sync":     {"600M", "450MiB"},
	"leadhound-mailbox-check":  {"600M", "450MiB"},
	"leadhound-leads-research": {"600M", "450MiB"},
}

// TestMemoryCeiling: the server is shared, so every leadhound unit but the backup has a
// memory ceiling that kills (MemoryMax) and never one that throttles (MemoryHigh, which
// hangs a unit instead of failing it), a GOMEMLIMIT below that ceiling so Go collects before
// the kill, and the long collects take half the CPU share.
func TestMemoryCeiling(t *testing.T) {
	services := units(t, "*.service")
	if len(services) != len(memory)+1 {
		t.Errorf("%d services, want %d with a memory ceiling and the backup", len(services), len(memory))
	}
	for path, u := range services {
		name := base(path)
		for section, keys := range u {
			if _, ok := keys["MemoryHigh"]; ok {
				t.Errorf("%s: [%s] MemoryHigh is set; a throttled unit hangs, use MemoryMax alone", path, section)
			}
		}
		if name == "leadhound-backup" {
			continue
		}
		want, ok := memory[name]
		if !ok {
			t.Errorf("%s: no memory ceiling in the test's table", path)
			continue
		}
		maxV := u.one(t, path, "Service", "MemoryMax")
		if maxV != want.max {
			t.Errorf("%s: MemoryMax = %s, want %s", path, maxV, want.max)
		}
		var limits []string
		for _, e := range u["Service"]["Environment"] {
			if v, ok := strings.CutPrefix(e, "GOMEMLIMIT="); ok {
				limits = append(limits, v)
			}
		}
		if len(limits) != 1 {
			t.Errorf("%s: GOMEMLIMIT = %q, want exactly one Environment=GOMEMLIMIT=", path, limits)
			continue
		}
		if limits[0] != want.goLimit {
			t.Errorf("%s: GOMEMLIMIT = %s, want %s", path, limits[0], want.goLimit)
		}
		ceiling, err := memoryBytes(maxV, map[string]int64{"K": 1 << 10, "M": 1 << 20, "G": 1 << 30})
		if err != nil {
			t.Errorf("%s: MemoryMax %q: %v", path, maxV, err)
			continue
		}
		heap, err := memoryBytes(limits[0], map[string]int64{"KiB": 1 << 10, "MiB": 1 << 20, "GiB": 1 << 30})
		if err != nil {
			t.Errorf("%s: GOMEMLIMIT %q: %v", path, limits[0], err)
			continue
		}
		if heap >= ceiling {
			t.Errorf("%s: GOMEMLIMIT %s is not below MemoryMax %s", path, limits[0], maxV)
		}
		if name == "leadhound-ats" || name == "leadhound-collect" {
			if got := u.one(t, path, "Service", "CPUWeight"); got != "50" {
				t.Errorf("%s: CPUWeight = %s, want 50", path, got)
			}
		}
	}
}

// memoryBytes reads a size as a whole number followed by one of units' suffixes.
func memoryBytes(v string, units map[string]int64) (int64, error) {
	for suffix, mult := range units {
		if n, ok := strings.CutSuffix(v, suffix); ok {
			i, err := strconv.ParseInt(n, 10, 64)
			if err != nil || i <= 0 {
				return 0, fmt.Errorf("not a positive whole number of %s", suffix)
			}
			return i * mult, nil
		}
	}
	return 0, fmt.Errorf("no unit suffix")
}

// TestScoreHasBudget: every score step stops itself at a --max-duration budget and runs
// under a /usr/bin/timeout longer than that budget, which ends a score that hangs.
// TestTimeoutsCoverWaits counts the timeout and never the budget, so the budget being below
// the timeout means it is never counted twice.
func TestScoreHasBudget(t *testing.T) {
	score := regexp.MustCompile(`/usr/bin/timeout (?:--kill-after=\S+ )?(\S+) /opt/leadhound/bin/leadhound score --max-duration (\S+)$`)
	seen := 0
	for path, u := range units(t, "*.service") {
		for _, e := range u["Service"]["ExecStart"] {
			if !strings.Contains(e, "leadhound score") {
				continue
			}
			seen++
			m := score.FindStringSubmatch(e)
			if m == nil {
				t.Errorf("%s: %q: a score step runs as /usr/bin/timeout <D> ... leadhound score --max-duration <budget>", path, e)
				continue
			}
			timeout, terr := gnuDuration(m[1])
			budget, berr := time.ParseDuration(m[2])
			switch {
			case terr != nil || berr != nil:
				t.Errorf("%s: %q: timeout %v, budget %v", path, e, terr, berr)
			case budget <= 0:
				t.Errorf("%s: score --max-duration %s, want a budget above zero", path, m[2])
			case budget >= timeout:
				t.Errorf("%s: score --max-duration %s is not below its /usr/bin/timeout %s", path, m[2], m[1])
			}
		}
	}
	if seen != 2 {
		t.Errorf("%d score steps, want 2 (ats, collect)", seen)
	}
}

// TestBudgetBelowTimeout: every step that stops itself at a --max-duration budget (the scores
// and the trim) runs under a /usr/bin/timeout longer than that budget, which ends a step that
// hangs; TestTimeoutsCoverWaits then counts the timeout, which covers the budget.
func TestBudgetBelowTimeout(t *testing.T) {
	budgeted := regexp.MustCompile(`^-?(?:/usr/bin/flock .* )?/usr/bin/timeout (?:--kill-after=\S+ )?(\S+) /opt/leadhound/bin/leadhound [a-z ]+ --max-duration (\S+)$`)
	seen := 0
	for path, u := range units(t, "*.service") {
		for _, e := range u["Service"]["ExecStart"] {
			if !strings.Contains(e, "--max-duration") {
				continue
			}
			seen++
			m := budgeted.FindStringSubmatch(e)
			if m == nil {
				t.Errorf("%s: %q: a budgeted step runs as /usr/bin/timeout <D> ... leadhound <cmd> --max-duration <budget>", path, e)
				continue
			}
			timeout, terr := gnuDuration(m[1])
			budget, berr := time.ParseDuration(m[2])
			switch {
			case terr != nil || berr != nil:
				t.Errorf("%s: %q: timeout %v, budget %v", path, e, terr, berr)
			case budget <= 0:
				t.Errorf("%s: --max-duration %s, want a budget above zero", path, m[2])
			case budget >= timeout:
				t.Errorf("%s: --max-duration %s is not below its /usr/bin/timeout %s", path, m[2], m[1])
			}
		}
	}
	if seen != 3 {
		t.Errorf("%d budgeted steps, want 3 (ats score, collect score, trim)", seen)
	}
}

// TestTrimUnit: the trim runs in the daytime beside the server's other services, so it yields
// to everything (the lowest priority, idle I/O, a fifth of the CPU share), carries no
// environment of its own beyond GOMEMLIMIT, holds no lock, and is the only unit that runs it.
func TestTrimUnit(t *testing.T) {
	const path = "leadhound-trim.service"
	u := readUnit(t, path)
	for key, want := range map[string]string{"Nice": "19", "IOSchedulingClass": "idle", "CPUWeight": "20", "TimeoutStartSec": "1h"} {
		if got := u.one(t, path, "Service", key); got != want {
			t.Errorf("%s: %s = %s, want %s", path, key, got, want)
		}
	}
	for _, e := range u["Service"]["Environment"] {
		if !strings.HasPrefix(e, "GOMEMLIMIT=") {
			t.Errorf("%s: Environment=%s; the trim sets only GOMEMLIMIT", path, e)
		}
	}
	for p, s := range units(t, "*.service") {
		for _, e := range s["Service"]["ExecStart"] {
			if strings.Contains(e, "flock") && base(p) == "leadhound-trim" {
				t.Errorf("%s: %q takes a lock; the trim waits on nothing", p, e)
			}
			if strings.Contains(e, "leadhound trim") != (base(p) == "leadhound-trim") {
				t.Errorf("%s: %q: only leadhound-trim.service runs the trim", p, e)
			}
		}
	}
}

// TestLeadsUnits: every leads unit, the mailbox check included, yields to the server's other
// services (Nice, idle I/O, half the CPU share), runs each command under a /usr/bin/timeout,
// carries no environment of its own beyond GOMEMLIMIT (hours, caps and the pause stay in the
// commands). A contacts step that finds the fetch lock held counts the lock's exit 75 as
// done; the probe waits for the lock, so a wait that runs out fails its unit visibly.
func TestLeadsUnits(t *testing.T) {
	leads := units(t, "leadhound-leads-*.service")
	if want := len(leadsFinding) + len(leadsSending) + len(leadsTriggered); len(leads) != want {
		t.Errorf("%d leads services, want %d", len(leads), want)
	}
	leads["leadhound-mailbox-check.service"] = readUnit(t, "leadhound-mailbox-check.service")
	fetches := 0
	for path, u := range leads {
		for key, want := range map[string]string{"Nice": "10", "IOSchedulingClass": "idle", "CPUWeight": "50"} {
			if got := u.one(t, path, "Service", key); got != want {
				t.Errorf("%s: %s = %s, want %s", path, key, got, want)
			}
		}
		for _, e := range u["Service"]["Environment"] {
			if !strings.HasPrefix(e, "GOMEMLIMIT=") {
				t.Errorf("%s: Environment=%s; a leads unit sets only GOMEMLIMIT", path, e)
			}
		}
		locked, waits := false, false
		for _, e := range u["Service"]["ExecStart"] {
			cmd := strings.TrimPrefix(e, "-")
			if after, ok := strings.CutPrefix(cmd, leadsFetchLock); ok {
				cmd, locked = after, true
			}
			if after, ok := strings.CutPrefix(cmd, leadsProbeLock); ok {
				cmd, waits = after, true
			}
			if !strings.HasPrefix(cmd, "/usr/bin/timeout --kill-after=") || !strings.Contains(cmd, " /opt/leadhound/bin/leadhound leads ") {
				t.Errorf("%s: %q: a leads step runs as /usr/bin/timeout --kill-after=... <D> leadhound leads ...", path, e)
			}
		}
		success := u["Service"]["SuccessExitStatus"]
		switch {
		case locked && waits:
			t.Errorf("%s: both skips and waits for the fetch lock", path)
		case locked:
			fetches++
			if !slices.Equal(success, []string{"75"}) {
				t.Errorf("%s: SuccessExitStatus = %q, want 75: a run that finds the fetch lock held is done", path, success)
			}
		case waits:
			fetches++
			if len(success) > 0 {
				t.Errorf("%s: SuccessExitStatus = %q: a probe whose lock wait runs out must fail its unit", path, success)
			}
		case len(success) > 0:
			t.Errorf("%s: SuccessExitStatus = %q without the fetch lock", path, success)
		}
	}
	if fetches != len(leadsFinding) {
		t.Errorf("%d leads units take the fetch lock, want %d (probe, contacts)", fetches, len(leadsFinding))
	}
}

// TestProbeWaitsOutContacts: the contacts unit holds the fetch lock for its web steps, up to
// their timeouts and kill graces together (190 minutes), and its timer fires three hours
// before the probe's, so it can still hold the lock when the probe starts. The probe waits for
// the lock longer than the contacts unit can hold it, rather than skip with exit 75 and lose
// the day's probe without a word. TestTimeoutsCoverWaits checks its TimeoutStartSec covers
// the wait and the run.
func TestProbeWaitsOutContacts(t *testing.T) {
	var held time.Duration
	timeout := regexp.MustCompile(`/usr/bin/timeout --kill-after=(\S+) (\S+) `)
	for _, e := range readUnit(t, "leadhound-leads-contacts.service")["Service"]["ExecStart"] {
		if !strings.Contains(e, " "+leadsFetchLockPath+" ") {
			continue
		}
		m := timeout.FindStringSubmatch(e)
		if m == nil {
			t.Fatalf("contacts step %q holds the fetch lock without a timeout", e)
		}
		for _, d := range m[1:] {
			v, err := gnuDuration(d)
			if err != nil {
				t.Fatalf("contacts step %q: %v", e, err)
			}
			held += v
		}
	}
	if held == 0 {
		t.Fatal("no contacts step holds the fetch lock")
	}
	wait := regexp.MustCompile(`^/usr/bin/flock --wait (\d+) --conflict-exit-code \d+ (\S+) `)
	execs := readUnit(t, "leadhound-leads-probe.service")["Service"]["ExecStart"]
	if len(execs) != 1 {
		t.Fatalf("probe ExecStart = %q, want one step", execs)
	}
	m := wait.FindStringSubmatch(execs[0])
	if m == nil {
		t.Fatalf("probe %q does not wait for the fetch lock: it skips while the contacts unit holds it and loses its day", execs[0])
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		t.Fatal(err)
	}
	if got := time.Duration(n) * time.Second; got <= held || m[2] != leadsFetchLockPath {
		t.Errorf("probe waits %v for %s; want longer than the contacts unit's %v on %s", got, m[2], held, leadsFetchLockPath)
	}
}

// TestLeadsBuiltWhileFinding: the lead list is rebuilt by a finding unit, so the console's
// leads fill while only LEADHOUND_LEADS_TIMERS=on is set, never only once sending is on.
func TestLeadsBuiltWhileFinding(t *testing.T) {
	for _, timer := range leadsFinding {
		name := strings.TrimSuffix(timer, ".timer")
		for _, e := range schedule[name].exec {
			if strings.HasSuffix(e, " /opt/leadhound/bin/leadhound leads build") {
				return
			}
		}
	}
	t.Errorf("no finding unit (%q) runs leads build: leads would be built only once sending is on", leadsFinding)
}

// TestNoUnitLoadsOwnerEnv: no unit reads the owner role's environment; only install.sh's
// migrate does.
func TestNoUnitLoadsOwnerEnv(t *testing.T) {
	paths, err := filepath.Glob("leadhound-*.*")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		if ext := filepath.Ext(path); ext != ".service" && ext != ".timer" && ext != ".path" {
			continue
		}
		b, err := os.ReadFile(path) // #nosec G304 -- the test's own deploy files
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), "migrate.env") {
			t.Errorf("%s: names migrate.env; the units hold only the app role", path)
		}
	}
}

// installFunc is the shell function name() { ... } from install.sh, through its closing brace.
func installFunc(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("install.sh")
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	in := false
	for line := range strings.Lines(string(b)) {
		if strings.HasPrefix(line, name+"() {") {
			in = true
		}
		if in {
			out = append(out, line)
			if strings.TrimRight(line, "\n") == "}" {
				return strings.Join(out, "")
			}
		}
	}
	t.Fatalf("install.sh has no function %s", name)
	return ""
}

// TestInstallLeadsTimers runs install.sh's own leads_timers against env files, with systemctl
// replaced by a recorder, and checks exactly which leads timers end up enabled, and that every
// other one is disabled and stopped. The server env today has none of the switch lines, so a
// deploy leaves every leads timer off.
func TestInstallLeadsTimers(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "run.sh")
	body := "set -eu\n" + installFunc(t, "env_value") + installFunc(t, "leads_timers") + "leads_timers \"$1\"\n"
	if err := os.WriteFile(script, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	fake := "#!/bin/sh\necho \"$*\" >>\"$SYSTEMCTL_LOG\"\n"
	if err := os.WriteFile(filepath.Join(bin, "systemctl"), []byte(fake), 0o700); err != nil { // #nosec G306 -- an executable stub in a test temp dir
		t.Fatal(err)
	}
	const (
		finding = "LEADHOUND_LEADS_TIMERS=on\n"
		send    = "LEADHOUND_LEADS_SEND=on\n"
		live    = "LEADHOUND_OUTREACH=live\n"
		// The old mailbox lines no longer switch anything on or off.
		user = "LEADHOUND_OUTREACH_USER=outreach@mail.example\n"
		imap = "LEADHOUND_IMAP_ADDR=mail.example:993\n"
	)
	all := slices.Concat(leadsFinding, leadsSending)
	for _, tc := range []struct {
		name, env string
		want      []string
	}{
		{"empty env", "", nil},
		{"the env.example lines, empty", "LEADHOUND_LEADS_TIMERS=\nLEADHOUND_LEADS_SEND=\nLEADHOUND_OUTREACH=\nLEADHOUND_MAILBOX_PUBLIC_KEY=\nLEADHOUND_MAILBOX_KEY_FILE=\n", nil},
		{"finding on only", finding, leadsFinding},
		{"finding and send on, shadow", finding + send, leadsFinding},
		{"finding and send on, shadow, old mailbox lines", finding + send + user + imap, leadsFinding},
		{"everything", finding + send + live, all},
		{"everything, no old mailbox lines needed", finding + send + live + "LEADHOUND_OUTREACH_USER=\nLEADHOUND_IMAP_ADDR=\n", all},
		{"everything, old mailbox lines left in", finding + send + live + user + imap, all},
		{"everything, quoted as systemd allows", "LEADHOUND_LEADS_TIMERS=\"on\"\nLEADHOUND_LEADS_SEND='on'\nLEADHOUND_OUTREACH=\"live\"\n", all},
		{"send without finding", send + live, nil},
		{"finding switched off by a later line", finding + send + live + "LEADHOUND_LEADS_TIMERS=off\n", nil},
		{"send switched off by a later line", finding + send + live + "LEADHOUND_LEADS_SEND=\n", leadsFinding},
		{"not quite on", "LEADHOUND_LEADS_TIMERS=online\n" + send + live, nil},
		{"not quite live", finding + send + "LEADHOUND_OUTREACH=lively\n", leadsFinding},
		{"commented out", "#LEADHOUND_LEADS_TIMERS=on\n# " + send, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := filepath.Join(t.TempDir(), "env")
			if err := os.WriteFile(env, []byte(tc.env), 0o600); err != nil {
				t.Fatal(err)
			}
			log := filepath.Join(t.TempDir(), "systemctl.log")
			cmd := exec.Command(sh, script, env) // #nosec G204 -- the test's own script
			cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "SYSTEMCTL_LOG="+log)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("leads_timers: %v\n%s", err, out)
			}
			b, err := os.ReadFile(log) // #nosec G304 -- the test's own temp file
			if err != nil {
				t.Fatal(err)
			}
			var enabled, disabled []string
			for line := range strings.Lines(string(b)) {
				f := strings.Fields(line)
				switch {
				case len(f) > 2 && f[0] == "enable" && f[1] == "--now":
					enabled = append(enabled, f[2:]...)
				case len(f) > 2 && f[0] == "disable" && f[1] == "--now":
					disabled = append(disabled, f[2:]...)
				default:
					t.Errorf("systemctl %s: not enable --now or disable --now", strings.TrimSpace(line))
				}
			}
			slices.Sort(enabled)
			want := slices.Clone(tc.want)
			slices.Sort(want)
			if !slices.Equal(enabled, want) {
				t.Errorf("enabled %q, want %q\n%s", enabled, want, out)
			}
			// Every leads timer is decided on every deploy: enabled, or disabled and stopped.
			decided := slices.Concat(enabled, disabled)
			slices.Sort(decided)
			wantAll := slices.Clone(all)
			slices.Sort(wantAll)
			if !slices.Equal(decided, wantAll) {
				t.Errorf("enabled or disabled %q, want each of %q once", decided, wantAll)
			}
			if len(disabled) > 0 && !strings.Contains(string(out), " off (") {
				t.Errorf("leads_timers disabled %q without saying which are off and why:\n%s", disabled, out)
			}
		})
	}
}

// TestBackupUnitBudget: the unit bounds the kept dumps by count and by total size.
func TestBackupUnitBudget(t *testing.T) {
	u := readUnit(t, "leadhound-backup.service")
	env := map[string]string{}
	for _, e := range u["Service"]["Environment"] {
		k, v, _ := strings.Cut(e, "=")
		env[k] = v
	}
	for k, want := range map[string]string{
		"LEADHOUND_BACKUP_KEEP":      "14",
		"LEADHOUND_BACKUP_MAX_BYTES": "4294967296",
	} {
		if env[k] != want {
			t.Errorf("leadhound-backup.service %s = %q, want %q", k, env[k], want)
		}
	}
}

// TestBackupPrunes runs leadhound-backup.sh under sh against a temp dir of fake dumps, with
// pg_dump replaced by a stub that writes a dump of a set size or fails, and checks which dumps
// are left: the newest KEEP that fit the byte budget together, the newest always, and every
// old one after a failed dump or a refused setting.
func TestBackupPrunes(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	script, err := filepath.Abs("leadhound-backup.sh")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	stub := `#!/bin/sh
for a in "$@"; do
	case $a in --file=*) f=${a#--file=} ;; esac
done
head -c "$STUB_DUMP_BYTES" /dev/zero >"$f"
if [ -n "${STUB_DUMP_FAIL:-}" ]; then exit 1; fi
`
	if err := os.WriteFile(filepath.Join(bin, "pg_dump"), []byte(stub), 0o700); err != nil { // #nosec G306 -- an executable stub in a test temp dir
		t.Fatal(err)
	}
	// Five old dumps of 10 bytes each, oldest first; the new one sorts after all of them.
	old := []string{
		"leadhound-20010101T033000Z.dump",
		"leadhound-20010102T033000Z.dump",
		"leadhound-20010103T033000Z.dump",
		"leadhound-20010104T033000Z.dump",
		"leadhound-20010105T033000Z.dump",
	}
	const newDump = "new"
	for _, tc := range []struct {
		name    string
		env     []string
		fail    bool // the run exits non-zero
		want    []string
		wantOut string
	}{
		{"by count", []string{"LEADHOUND_BACKUP_KEEP=3", "STUB_DUMP_BYTES=10"}, false, []string{old[3], old[4], newDump}, ""},
		{"by size", []string{"LEADHOUND_BACKUP_MAX_BYTES=35", "STUB_DUMP_BYTES=10"}, false, []string{old[3], old[4], newDump}, ""},
		{"by size, exactly at the budget", []string{"LEADHOUND_BACKUP_MAX_BYTES=30", "STUB_DUMP_BYTES=10"}, false, []string{old[3], old[4], newDump}, ""},
		{"count and size, size binds", []string{"LEADHOUND_BACKUP_KEEP=4", "LEADHOUND_BACKUP_MAX_BYTES=45", "STUB_DUMP_BYTES=20"}, false, []string{old[3], old[4], newDump}, ""},
		{"count and size, count binds", []string{"LEADHOUND_BACKUP_KEEP=2", "LEADHOUND_BACKUP_MAX_BYTES=1000", "STUB_DUMP_BYTES=10"}, false, []string{old[4], newDump}, ""},
		{"the newest alone over the budget", []string{"LEADHOUND_BACKUP_MAX_BYTES=5", "STUB_DUMP_BYTES=10"}, false, []string{newDump}, "the newest dump alone is over"},
		{"everything fits", []string{"STUB_DUMP_BYTES=10"}, false, append(slices.Clone(old), newDump), ""},
		{"a failed dump", []string{"LEADHOUND_BACKUP_KEEP=1", "LEADHOUND_BACKUP_MAX_BYTES=1", "STUB_DUMP_BYTES=10", "STUB_DUMP_FAIL=1"}, true, old, ""},
		{"budget not a number", []string{"LEADHOUND_BACKUP_KEEP=1", "LEADHOUND_BACKUP_MAX_BYTES=4G", "STUB_DUMP_BYTES=10"}, true, old, "LEADHOUND_BACKUP_MAX_BYTES must be"},
		{"budget negative", []string{"LEADHOUND_BACKUP_KEEP=1", "LEADHOUND_BACKUP_MAX_BYTES=-1", "STUB_DUMP_BYTES=10"}, true, old, "LEADHOUND_BACKUP_MAX_BYTES must be"},
		{"budget zero", []string{"LEADHOUND_BACKUP_KEEP=1", "LEADHOUND_BACKUP_MAX_BYTES=0", "STUB_DUMP_BYTES=10"}, true, old, "LEADHOUND_BACKUP_MAX_BYTES must be at least 1"},
		{"budget too long", []string{"LEADHOUND_BACKUP_KEEP=1", "LEADHOUND_BACKUP_MAX_BYTES=9999999999999999999", "STUB_DUMP_BYTES=10"}, true, old, "LEADHOUND_BACKUP_MAX_BYTES must be"},
		{"keep zero", []string{"LEADHOUND_BACKUP_KEEP=0", "STUB_DUMP_BYTES=10"}, true, old, "LEADHOUND_BACKUP_KEEP must be at least 1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for _, name := range old {
				if err := os.WriteFile(filepath.Join(dir, name), make([]byte, 10), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			cmd := exec.Command(sh, script) // #nosec G204 -- the test's own script
			cmd.Env = append(os.Environ(),
				"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
				"LEADHOUND_BACKUP_DIR="+dir,
				"LEADHOUND_BACKUP_DB=leadhound_test",
				"LEADHOUND_BACKUP_KEEP=",
				"LEADHOUND_BACKUP_MAX_BYTES=",
				"STUB_DUMP_FAIL=",
			)
			cmd.Env = append(cmd.Env, tc.env...)
			out, err := cmd.CombinedOutput()
			if (err != nil) != tc.fail {
				t.Fatalf("leadhound-backup: err = %v, want failure %t\n%s", err, tc.fail, out)
			}
			if !strings.Contains(string(out), tc.wantOut) {
				t.Errorf("output lacks %q:\n%s", tc.wantOut, out)
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, e := range entries {
				name := e.Name()
				if !slices.Contains(old, name) && strings.HasPrefix(name, "leadhound-") && strings.HasSuffix(name, ".dump") {
					name = newDump
				}
				got = append(got, name)
			}
			slices.Sort(got)
			want := slices.Clone(tc.want)
			slices.Sort(want)
			if !slices.Equal(got, want) {
				t.Errorf("left %q, want %q\n%s", got, want, out)
			}
		})
	}
}

// TestPaths: the Run now watchers are exactly one per action, each watching only its own
// request file with PathChanged= (fires on a new write, never while an old file merely
// exists), starting only its same-named service (no Unit=), and armed at boot.
func TestPaths(t *testing.T) {
	paths := units(t, "*.path")
	var names, want []string
	for path := range paths {
		names = append(names, base(path))
	}
	for _, a := range runActions {
		want = append(want, "leadhound-"+a)
	}
	slices.Sort(names)
	slices.Sort(want)
	if !slices.Equal(names, want) {
		t.Errorf(".path units %q, want %q", names, want)
	}
	for path, u := range paths {
		name := base(path)
		action := strings.TrimPrefix(name, "leadhound-")
		if got := u.one(t, path, "Path", "PathChanged"); got != runDir+"/"+action {
			t.Errorf("%s: PathChanged = %s, want %s/%s", path, got, runDir, action)
		}
		for section, keys := range u {
			for key := range keys {
				if section == "Path" && key != "PathChanged" {
					t.Errorf("%s: [Path] %s is set; a watcher has PathChanged= alone (no Unit=: it starts its same-named service)", path, key)
				}
			}
		}
		if got := u.one(t, path, "Install", "WantedBy"); got != "paths.target" {
			t.Errorf("%s: WantedBy = %s, want paths.target", path, got)
		}
		if _, err := os.Stat(name + ".service"); err != nil {
			t.Errorf("%s: no matching %s.service", path, name)
		}
	}
}

// TestInstallArmsEveryPath: make deploy copies the watchers, install.sh installs them, creates
// the request directory for the console's user before arming them, and arms every one on
// every deploy.
func TestInstallArmsEveryPath(t *testing.T) {
	steps := installSteps(t)
	var watchers []string
	for path := range units(t, "*.path") {
		watchers = append(watchers, filepath.Base(path))
	}
	slices.Sort(watchers)
	pathName := regexp.MustCompile(`leadhound-[a-z-]+\.path`)
	arm := slices.IndexFunc(steps, func(l string) bool {
		return strings.HasPrefix(l, "systemctl enable --now ") && pathName.MatchString(l)
	})
	if arm < 0 {
		t.Fatal("install.sh has no systemctl enable --now for the .path watchers")
	}
	if strings.Contains(installFunc(t, "leads_timers"), ".path") {
		t.Error("install.sh arms a watcher inside leads_timers, not on every deploy")
	}
	armed := pathName.FindAllString(steps[arm], -1)
	slices.Sort(armed)
	if !slices.Equal(armed, watchers) {
		t.Errorf("install.sh arms %q, want %q", armed, watchers)
	}
	if reload := firstLine(steps, "systemctl daemon-reload"); reload < 0 || arm < reload {
		t.Error("install.sh arms the watchers before systemctl daemon-reload")
	}
	copyLoop := firstLine(steps, "for f in", `"$stage"/leadhound-*.path`)
	if copyLoop < 0 {
		t.Error(`install.sh does not install "$stage"/leadhound-*.path`)
	} else if copyLoop > arm {
		t.Error("install.sh arms the watchers before it installs them")
	}
	mkdir := slices.IndexFunc(steps, func(l string) bool {
		f := strings.Fields(l)
		return len(f) > 0 && f[0] == "install" && slices.Contains(f, "-d") && slices.Contains(f, `"$data/run"`)
	})
	switch {
	case mkdir < 0:
		t.Error(`install.sh does not create "$data/run"`)
	case !strings.Contains(steps[mkdir], "-o leadhound -g leadhound -m 0700"):
		t.Errorf(`install.sh creates "$data/run" as %q, want -o leadhound -g leadhound -m 0700`, steps[mkdir])
	case mkdir > arm:
		t.Error(`install.sh creates "$data/run" after arming the watchers`)
	}

	b, err := os.ReadFile("../Makefile")
	if err != nil {
		t.Fatal(err)
	}
	in, copies := false, false
	for line := range strings.Lines(string(b)) {
		switch {
		case strings.HasPrefix(line, "deploy:"):
			in = true
		case in && strings.HasPrefix(line, "\t"):
			copies = copies || (strings.Contains(line, "$(SCP)") && slices.Contains(strings.Fields(line), "deploy/leadhound-*.path"))
		case in:
			in = false
		}
	}
	if !copies {
		t.Error("make deploy does not copy deploy/leadhound-*.path to the stage")
	}
}

// mailboxWorkers are the only services that open a sealed mailbox password, so the only ones
// handed the mailboxes' private key, as a systemd credential.
var mailboxWorkers = []string{"leadhound-leads-send", "leadhound-leads-sync", "leadhound-mailbox-check"}

// claudeWorkers are the only services that call Claude, so the only ones handed the Anthropic
// key, as a systemd credential. The drafts unit joins them when it is added.
var claudeWorkers = []string{"leadhound-leads-batch", "leadhound-leads-research"}

const (
	mailboxCredential   = "mailbox-key:/etc/leadhound/mailbox.key"     //nolint:gosec // G101: a systemd credential name and path, not a credential
	anthropicCredential = "anthropic-key:/etc/leadhound/anthropic.key" //nolint:gosec // G101: a systemd credential name and path, not a credential
	noCredentials       = "-/run/credentials"                          //nolint:gosec // G101: a path the units hide, not a credential
)

// wantCredentials is the exact LoadCredential= list the service name gets, in file order.
func wantCredentials(name string) []string {
	var want []string
	if slices.Contains(mailboxWorkers, name) {
		want = append(want, mailboxCredential)
	}
	if slices.Contains(claudeWorkers, name) {
		want = append(want, anthropicCredential)
	}
	return want
}

// TestMailboxKeyOnlyForWorkers: the send, the sync and the mailbox check have the mailbox
// key's LoadCredential= line, the units that call Claude have the Anthropic key's, and nothing
// else has either or any other credential; every service with no credential, the console
// first, has InaccessiblePaths=-/run/credentials, so a compromised console or job can store a
// new sealed password but never read one back (MB-R4), and never holds the Anthropic key.
func TestMailboxKeyOnlyForWorkers(t *testing.T) {
	credKey := regexp.MustCompile(`^(Load|Set|Import)Credential`)
	all := units(t, "*.service")
	for _, pattern := range []string{"*.timer", "*.path"} {
		maps.Copy(all, units(t, pattern))
	}
	seen := map[string]bool{}
	for path, u := range all {
		name := base(path)
		var want []string
		if filepath.Ext(path) == ".service" {
			want = wantCredentials(name)
			if len(want) > 0 {
				seen[name] = true
			}
		}
		for section, keys := range u {
			for key, vals := range keys {
				if !credKey.MatchString(key) {
					continue
				}
				if len(want) == 0 || section != "Service" || key != "LoadCredential" || !slices.Equal(vals, want) {
					t.Errorf("%s: [%s] %s = %q; want exactly LoadCredential=%q (mailbox key only for %q, Anthropic key only for %q)",
						path, section, key, vals, want, mailboxWorkers, claudeWorkers)
				}
			}
		}
		if filepath.Ext(path) != ".service" {
			continue
		}
		inaccessible := strings.Fields(strings.Join(u["Service"]["InaccessiblePaths"], " "))
		switch {
		case len(want) > 0 && !slices.Equal(u["Service"]["LoadCredential"], want):
			t.Errorf("%s: LoadCredential = %q, want %q", path, u["Service"]["LoadCredential"], want)
		case len(want) > 0 && slices.ContainsFunc(inaccessible, func(p string) bool { return strings.Contains(p, "/run/credentials") }):
			t.Errorf("%s: InaccessiblePaths = %q hides its own credential", path, inaccessible)
		case len(want) == 0 && !slices.Contains(inaccessible, noCredentials):
			t.Errorf("%s: InaccessiblePaths = %q, want it to include %s", path, inaccessible, noCredentials)
		}
	}
	for _, name := range slices.Concat(mailboxWorkers, claudeWorkers) {
		if !seen[name] {
			t.Errorf("%s has no .service file", name)
		}
	}
}

// TestAnthropicKeyNeverOnConsole: the console unit gets no credential at all, hides
// /run/credentials, and nothing in it names the Anthropic key; no unit sets ANTHROPIC_ in its
// environment, and env.example sets no ANTHROPIC_ line (the env file reaches every unit, the
// console included).
func TestAnthropicKeyNeverOnConsole(t *testing.T) {
	const path = "leadhound-console.service"
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	u := readUnit(t, path)
	if got := u["Service"]["LoadCredential"]; len(got) != 0 {
		t.Errorf("%s: LoadCredential = %q, want none", path, got)
	}
	if strings.Contains(string(b), "anthropic") {
		t.Errorf("%s names the Anthropic key", path)
	}
	if inaccessible := strings.Fields(strings.Join(u["Service"]["InaccessiblePaths"], " ")); !slices.Contains(inaccessible, noCredentials) {
		t.Errorf("%s: InaccessiblePaths = %q, want it to include %s", path, inaccessible, noCredentials)
	}
	for p, u := range units(t, "*.service") {
		for _, e := range u["Service"]["Environment"] {
			if strings.Contains(e, "ANTHROPIC_") {
				t.Errorf("%s: Environment = %q; the Anthropic key is a credential, never an environment line", p, e)
			}
		}
	}
	env, err := os.ReadFile("env.example")
	if err != nil {
		t.Fatal(err)
	}
	for line := range strings.Lines(string(env)) {
		l := strings.TrimSpace(line)
		if !strings.HasPrefix(l, "#") && (strings.HasPrefix(l, "ANTHROPIC_") || strings.HasPrefix(l, "LEADHOUND_ANTHROPIC_KEY_FILE")) {
			t.Errorf("env.example sets %q; the env file reaches the console, which refuses to start with it", l)
		}
	}
}

// TestRemoteKeyOnlyForWorkers: make remote hands the key, as the same credential, only to the
// verbs that open a password; every other verb runs without it, and leads mailboxes list is
// free like the other lists.
func TestRemoteKeyOnlyForWorkers(t *testing.T) {
	mk, err := exec.LookPath("make")
	if err != nil {
		t.Skip("no make")
	}
	for _, tc := range []struct {
		args    string
		confirm bool
		key     bool
	}{
		{"leads selftest", true, true},
		{"leads send --max 1", true, true},
		{"leads sync", true, true},
		{"leads mailboxes check 3", true, true},
		{"leads mailboxes check --requested", true, true},
		{"leads mailboxes list", false, false},
		{"leads mailboxes add --address a@mail.example", true, false},
		{"leads mailboxes keygen --out x", true, false},
		{"leads batch show", false, false},
		{"leads sends", true, false},
		{"track list", false, false},
		{"serve", true, false},
	} {
		t.Run(tc.args, func(t *testing.T) {
			args := []string{"-n", "-C", "..", "remote", "HOST=h.invalid", "ARGS=" + tc.args}
			if tc.confirm {
				args = append(args, "CONFIRM=1")
			}
			cmd := exec.Command(mk, args...) // #nosec G204 -- the test's own Makefile
			cmd.Env = append(os.Environ(), "MAKEFLAGS=", "CONFIRM=")
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("make %q: %v\n%s", args, err, out)
			}
			if got := strings.Contains(string(out), "-p LoadCredential="+mailboxCredential); got != tc.key {
				t.Errorf("make remote ARGS=%q hands the key = %t, want %t:\n%s", tc.args, got, tc.key, out)
			}
		})
	}
}

// makeDry runs make -n in the repository root with args and returns its output and error;
// MAKEFLAGS and CONFIRM from the caller's environment never leak in.
func makeDry(t *testing.T, args ...string) (string, error) {
	t.Helper()
	mk, err := exec.LookPath("make")
	if err != nil {
		t.Skip("no make")
	}
	cmd := exec.Command(mk, append([]string{"-n", "-C", ".."}, args...)...) // #nosec G204 -- the test's own Makefile
	cmd.Env = append(os.Environ(), "MAKEFLAGS=", "CONFIRM=", "REMOTE_CLAUDE=", "ORG=", "REASON=")
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// TestRemoteClaudeKey: make remote hands the Anthropic key only with REMOTE_CLAUDE=1, as the
// same credential the Claude units get; it refuses the console, a verb that gets the mailbox
// key, a run without CONFIRM=1 and any value but 1, before anything runs.
func TestRemoteClaudeKey(t *testing.T) {
	const flag = "-p LoadCredential=" + anthropicCredential
	for _, tc := range []struct {
		args   []string
		key    bool
		refuse string
	}{
		{[]string{"ARGS=leads phones research --campaign drupal-eol --max 3", "CONFIRM=1"}, false, ""},
		{[]string{"ARGS=leads research run --max 3", "CONFIRM=1", "REMOTE_CLAUDE=1"}, true, ""},
		{[]string{"ARGS=leads drafts sample", "CONFIRM=1", "REMOTE_CLAUDE=1"}, true, ""},
		{[]string{"ARGS=serve", "CONFIRM=1", "REMOTE_CLAUDE=1"}, false, "console never holds"},
		{[]string{"ARGS=console hash-password", "CONFIRM=1", "REMOTE_CLAUDE=1"}, false, "console never holds"},
		{[]string{"ARGS=leads send --max 1", "CONFIRM=1", "REMOTE_CLAUDE=1"}, false, "no command holds both keys"},
		{[]string{"ARGS=leads batch show", "REMOTE_CLAUDE=1"}, false, "add CONFIRM=1"},
		{[]string{"ARGS=leads research run", "CONFIRM=1", "REMOTE_CLAUDE=yes"}, false, "must be 1 or unset"},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			out, err := makeDry(t, append([]string{"remote", "HOST=h.invalid"}, tc.args...)...)
			if tc.refuse != "" {
				if err == nil || !strings.Contains(out, tc.refuse) || strings.Contains(out, "systemd-run") {
					t.Fatalf("make remote %q = %v, want a refusal naming %q before anything runs:\n%s", tc.args, err, tc.refuse, out)
				}
				return
			}
			if err != nil {
				t.Fatalf("make remote %q: %v\n%s", tc.args, err, out)
			}
			if got := strings.Contains(out, flag); got != tc.key {
				t.Errorf("make remote %q hands the Anthropic key = %t, want %t:\n%s", tc.args, got, tc.key, out)
			}
			if strings.Contains(out, "-p LoadCredential="+mailboxCredential) && strings.Contains(out, flag) {
				t.Errorf("make remote %q hands both keys:\n%s", tc.args, out)
			}
		})
	}
}

// TestForgetTarget: make forget runs leads forget org as the owner role (migrate.env) on the
// server, the reason taken from the environment rather than spliced into the recipe; it
// refuses without HOST, ORG, REASON or CONFIRM=1, and an ORG that is not digits.
func TestForgetTarget(t *testing.T) {
	ok := []string{"forget", "HOST=h.invalid", "ORG=42", "REASON=asked to be removed", "CONFIRM=1"}
	out, err := makeDry(t, ok...)
	if err != nil {
		t.Fatalf("make %q: %v\n%s", ok, err, out)
	}
	for _, want := range []string{"-p EnvironmentFile=/etc/leadhound/migrate.env", "--uid=leadhound", "leads forget org 42 --reason '$r'", `"$FORGET_REASON"`} {
		if !strings.Contains(out, want) {
			t.Errorf("make forget does not contain %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "asked to be removed") {
		t.Errorf("make forget splices REASON into the recipe; it must reach the shell through the environment:\n%s", out)
	}
	for _, tc := range []struct {
		drop, add, refuse string
	}{
		{"HOST=h.invalid", "", "HOST is required"},
		{"ORG=42", "", "ORG is required"},
		{"ORG=42", "ORG=4a2", "digits only"},
		{"ORG=42", "ORG=1 2", "ORG is required"},
		{"REASON=asked to be removed", "", "REASON is required"},
		{"REASON=asked to be removed", "REASON= ", "REASON is required"},
		{"CONFIRM=1", "", "add CONFIRM=1"},
	} {
		t.Run(tc.refuse+" "+tc.add, func(t *testing.T) {
			args := slices.DeleteFunc(slices.Clone(ok), func(a string) bool { return a == tc.drop })
			if tc.add != "" {
				args = append(args, tc.add)
			}
			out, err := makeDry(t, args...)
			if err == nil || !strings.Contains(out, tc.refuse) || strings.Contains(out, "systemd-run") {
				t.Fatalf("make %q = %v, want a refusal naming %q before anything runs:\n%s", args, err, tc.refuse, out)
			}
		})
	}
}

// TestDeployDataCopiesCampaigns: make deploy-data checks the private campaigns/ with leads
// campaigns check, copies it with the same owner and modes as leads-templates/ (pruning a file
// no longer in the private tree), and reminds you to register; the folder is optional.
func TestDeployDataCopiesCampaigns(t *testing.T) {
	out, err := makeDry(t, "deploy-data", "HOST=h.invalid", "CONFIRM=1")
	if err != nil {
		t.Fatalf("make deploy-data: %v\n%s", err, out)
	}
	for _, want := range []string{
		"LEADHOUND_DATABASE_URL= go run ./cmd/leadhound leads campaigns check",
		"if test -d '../job-hunt/leadhound/campaigns'; then scp -r '../job-hunt/leadhound/campaigns'",
		"for t in leads-templates campaigns; do",
		`test -d \$t || continue;`,
		`find \$t -type d -exec install -d -o root -g leadhound -m 0750 /opt/leadhound/data/{}`,
		`find \$t -type f -exec install -o root -g leadhound -m 0640 {} /opt/leadhound/data/{}`,
		`find \$t -type f | while read -r f; do test -e $stage/\$f || rm -f \$f; done`,
		"ARGS='leads campaigns register'",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("make deploy-data does not contain %q:\n%s", want, out)
		}
	}
}

// TestInstallMailboxKey: install.sh generates the mailbox key after the new binary is in place
// and before the units are installed, only when the key file is missing, never overwrites it,
// always derives the public line from the key file (replacing one that differs, appending one
// that is missing), and prints the fingerprint but no key (MB-R15). It runs install.sh's own
// mailbox_key against a fake leadhound.
func TestInstallMailboxKey(t *testing.T) {
	steps := installSteps(t)
	swap := firstLine(steps, "mv -f", `"$bin/.leadhound.new"`, `"$bin/leadhound"`)
	call := firstLine(steps, "mailbox_key /etc/leadhound/mailbox.key /etc/leadhound/env")
	unitsAt := firstLine(steps, "/etc/systemd/system/")
	switch {
	case call < 0:
		t.Fatal("install.sh does not run mailbox_key /etc/leadhound/mailbox.key /etc/leadhound/env")
	case call < swap:
		t.Errorf("install.sh runs mailbox_key (line %d) before the binary swap (line %d)", call, swap)
	case call > unitsAt:
		t.Errorf("install.sh runs mailbox_key (line %d) after installing the units (line %d)", call, unitsAt)
	}
	fn := installFunc(t, "mailbox_key")
	if strings.Contains(fn, ".leadhound.new") || !strings.Contains(fn, `"$bin/leadhound" leads mailboxes keygen --out "$1"`) {
		t.Errorf("mailbox_key does not generate with the installed binary:\n%s", fn)
	}

	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "run.sh")
	body := "set -eu\n" + installFunc(t, "fail") + installFunc(t, "env_value") + fn + "bin=\"$3\"\nmailbox_key \"$1\" \"$2\"\n"
	if err := os.WriteFile(script, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	// The fake keygen refuses an existing file as the real one does; the fake pubkey derives
	// the public key from the private one, so a stale env line shows as a mismatch.
	fake := `#!/bin/sh
echo "$*" >>"$LEADHOUND_LOG"
case "$3" in
keygen)
	[ ! -e "$5" ] || { echo "$5 already exists" >&2; exit 2; }
	printf 'PRIVATE-%s\n' "$FAKE_KEY" >"$5"
	echo "LEADHOUND_MAILBOX_PUBLIC_KEY=pub-$FAKE_KEY"
	echo "Fingerprint: fp-$FAKE_KEY"
	;;
pubkey)
	[ -z "${FAKE_PUBKEY_FAIL:-}" ] || exit 2
	[ -n "$(find "$5" -perm 600)" ] || { echo "$5 is not 0600" >&2; exit 2; }
	k=$(cat "$5")
	echo "LEADHOUND_MAILBOX_PUBLIC_KEY=pub-${k#PRIVATE-}"
	;;
*) exit 9 ;;
esac
`
	for name, body := range map[string]string{"leadhound": fake, "chown": "#!/bin/sh\necho \"chown $*\" >>\"$LEADHOUND_LOG\"\n"} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o700); err != nil { // #nosec G306 -- an executable stub in a test temp dir
			t.Fatal(err)
		}
	}
	const other = "LEADHOUND_DATABASE_URL=postgres://app@localhost/leadhound\nLEADHOUND_TZ=Europe/Bucharest\n"
	for _, tc := range []struct {
		name       string
		key        string      // the key file's contents; empty is no file
		keyMode    os.FileMode // the existing key file's mode; 0 is 0600
		env        string
		pubFail    bool
		wantFail   bool
		wantKeygen bool
		wantEnv    string // empty: the env file is unchanged
		wantOut    string
	}{
		{name: "first deploy", env: other, wantKeygen: true, wantEnv: other + "LEADHOUND_MAILBOX_PUBLIC_KEY=pub-new\n", wantOut: "fingerprint fp-new"},
		{name: "first deploy, env.example's empty line", env: "LEADHOUND_MAILBOX_PUBLIC_KEY=\n" + other, wantKeygen: true, wantEnv: other + "LEADHOUND_MAILBOX_PUBLIC_KEY=pub-new\n", wantOut: "fingerprint fp-new"},
		{name: "key kept, line missing", key: "PRIVATE-old\n", env: other, wantEnv: other + "LEADHOUND_MAILBOX_PUBLIC_KEY=pub-old\n", wantOut: "wrote the mailbox public key"},
		{name: "key kept, line stale", key: "PRIVATE-old\n", env: "LEADHOUND_MAILBOX_PUBLIC_KEY=pub-stale\n" + other, wantEnv: other + "LEADHOUND_MAILBOX_PUBLIC_KEY=pub-old\n", wantOut: "wrote the mailbox public key"},
		{name: "key kept, two lines, the last right", key: "PRIVATE-old\n", env: "LEADHOUND_MAILBOX_PUBLIC_KEY=pub-stale\n" + other + "LEADHOUND_MAILBOX_PUBLIC_KEY=pub-old\n", wantEnv: other + "LEADHOUND_MAILBOX_PUBLIC_KEY=pub-old\n", wantOut: "wrote the mailbox public key"},
		{name: "key kept, line right", key: "PRIVATE-old\n", env: other + "LEADHOUND_MAILBOX_PUBLIC_KEY=pub-old\n", wantOut: "holds the mailbox public key"},
		{name: "key unreadable", key: "PRIVATE-old\n", env: other, pubFail: true, wantFail: true, wantOut: "could not be read"},
		{name: "key restored 0644", key: "PRIVATE-old\n", keyMode: 0o644, env: other, wantEnv: other + "LEADHOUND_MAILBOX_PUBLIC_KEY=pub-old\n", wantOut: "wrote the mailbox public key"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tmp := t.TempDir()
			key, env, log := filepath.Join(tmp, "mailbox.key"), filepath.Join(tmp, "env"), filepath.Join(tmp, "leadhound.log")
			if tc.key != "" {
				if err := os.WriteFile(key, []byte(tc.key), 0o600); err != nil {
					t.Fatal(err)
				}
				if tc.keyMode != 0 {
					if err := os.Chmod(key, tc.keyMode); err != nil {
						t.Fatal(err)
					}
				}
			}
			if err := os.WriteFile(env, []byte(tc.env), 0o600); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(sh, script, key, env, bin) // #nosec G204 -- the test's own script
			cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
				"LEADHOUND_LOG="+log, "FAKE_KEY=new", "FAKE_PUBKEY_FAIL=")
			if tc.pubFail {
				cmd.Env = append(cmd.Env, "FAKE_PUBKEY_FAIL=1")
			}
			out, err := cmd.CombinedOutput()
			if (err != nil) != tc.wantFail {
				t.Fatalf("mailbox_key: err = %v, want failure %t\n%s", err, tc.wantFail, out)
			}
			if !strings.Contains(string(out), tc.wantOut) {
				t.Errorf("output lacks %q:\n%s", tc.wantOut, out)
			}
			for _, secret := range []string{"PRIVATE", "pub-"} {
				if strings.Contains(string(out), secret) {
					t.Errorf("output shows key material %q:\n%s", secret, out)
				}
			}
			calls, _ := os.ReadFile(log) // #nosec G304 -- the test's own temp file
			if got := strings.Contains(string(calls), "keygen"); got != tc.wantKeygen {
				t.Errorf("keygen ran = %t, want %t; calls:\n%s", got, tc.wantKeygen, calls)
			}
			gotKey, err := os.ReadFile(key) // #nosec G304 -- the test's own temp file
			switch {
			case err != nil:
				t.Errorf("no key file after mailbox_key: %v", err)
			case tc.key != "" && string(gotKey) != tc.key:
				t.Errorf("the existing key was overwritten: %q, want %q", gotKey, tc.key)
			}
			wantEnv := tc.wantEnv
			if wantEnv == "" {
				wantEnv = tc.env
			}
			gotEnv, err := os.ReadFile(env) // #nosec G304 -- the test's own temp file
			if err != nil {
				t.Fatal(err)
			}
			if string(gotEnv) != wantEnv {
				t.Errorf("env file:\n%s\nwant:\n%s", gotEnv, wantEnv)
			}
			if fi, err := os.Stat(env); err != nil || fi.Mode().Perm() != 0o600 {
				t.Errorf("env file mode %v (%v), want 0600", fi.Mode().Perm(), err)
			}
			if fi, err := os.Stat(key); err != nil || fi.Mode().Perm() != 0o600 {
				t.Errorf("key file mode %v (%v), want 0600", fi.Mode().Perm(), err)
			}
			if chowned, _ := os.ReadFile(log); !strings.Contains(string(chowned), "chown root:root "+key) { // #nosec G304 -- the test's own temp file
				t.Errorf("the key was not made root:root; calls:\n%s", chowned)
			}
			if leftovers, _ := filepath.Glob(env + ".*"); len(leftovers) > 0 {
				t.Errorf("temporary files left: %q", leftovers)
			}
		})
	}
}

// TestInstallAnthropicKey: install.sh runs anthropic_key before the units are installed; the
// function never creates, reads or prints the key, makes an existing one root:root 0600 again,
// and on a missing one says the Claude units cannot start without failing the deploy.
func TestInstallAnthropicKey(t *testing.T) {
	steps := installSteps(t)
	call := firstLine(steps, "anthropic_key /etc/leadhound/anthropic.key")
	unitsAt := firstLine(steps, "/etc/systemd/system/")
	switch {
	case call < 0:
		t.Fatal("install.sh does not run anthropic_key /etc/leadhound/anthropic.key")
	case call > unitsAt:
		t.Errorf("install.sh runs anthropic_key (line %d) after installing the units (line %d)", call, unitsAt)
	}
	fn := installFunc(t, "anthropic_key")
	for _, never := range []string{"cat ", "<", ">", "leadhound ", "head", "tail", "sed", "grep", "read ", "touch", "install ", "cp ", "mv "} {
		if strings.Contains(fn, never) {
			t.Errorf("anthropic_key uses %q; it must never create, read or move the key:\n%s", never, fn)
		}
	}

	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "run.sh")
	if err := os.WriteFile(script, []byte("set -eu\n"+fn+"anthropic_key \"$1\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "chown"), []byte("#!/bin/sh\necho \"chown $*\" >>\"$LEADHOUND_LOG\"\n"), 0o700); err != nil { // #nosec G306 -- an executable stub in a test temp dir
		t.Fatal(err)
	}
	const secret = "sk-ant-sentinel-value\n" //nolint:gosec // G101: a sentinel the test looks for, not a credential
	for _, tc := range []struct {
		name    string
		mode    os.FileMode // 0: no key file
		wantOut string
	}{
		{"missing", 0, "cannot start until you place it"},
		{"kept 0600", 0o600, "is root:root 0600"},
		{"restored 0644", 0o644, "is root:root 0600"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tmp := t.TempDir()
			key, log := filepath.Join(tmp, "anthropic.key"), filepath.Join(tmp, "calls.log")
			if tc.mode != 0 {
				if err := os.WriteFile(key, []byte(secret), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(key, tc.mode); err != nil {
					t.Fatal(err)
				}
			}
			cmd := exec.Command(sh, script, key) // #nosec G204 -- the test's own script
			cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "LEADHOUND_LOG="+log)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("anthropic_key: %v\n%s", err, out)
			}
			if !strings.Contains(string(out), tc.wantOut) || strings.Contains(string(out), "sentinel") {
				t.Errorf("output %q, want %q and no key material", out, tc.wantOut)
			}
			calls, _ := os.ReadFile(log) // #nosec G304 -- the test's own temp file
			if tc.mode == 0 {
				if _, err := os.Lstat(key); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("a missing key was created (%v)", err)
				}
				if len(calls) != 0 {
					t.Errorf("chown ran on a missing key: %s", calls)
				}
				return
			}
			got, err := os.ReadFile(key) // #nosec G304 -- the test's own temp file
			if err != nil || string(got) != secret {
				t.Errorf("the key changed: %v", err)
			}
			if fi, err := os.Stat(key); err != nil || fi.Mode().Perm() != 0o600 {
				t.Errorf("key file mode %v (%v), want 0600", fi.Mode().Perm(), err)
			}
			if !strings.Contains(string(calls), "chown root:root "+key) {
				t.Errorf("the key was not made root:root; calls:\n%s", calls)
			}
		})
	}
}

// TestResearchUnit: contact research runs only when the console asks (its watcher, never a
// timer), holds the Anthropic key alone among the console-triggered units, and never the
// mailbox key; the console, which writes the request, holds no key at all.
func TestResearchUnit(t *testing.T) {
	const name = "leadhound-leads-research"
	u := readUnit(t, name+".service")
	if got := u["Service"]["LoadCredential"]; !slices.Equal(got, []string{anthropicCredential}) {
		t.Errorf("%s: LoadCredential = %q, want only %s", name, got, anthropicCredential)
	}
	if _, err := os.Stat(name + ".timer"); err == nil {
		t.Errorf("%s has a timer; it runs only when the console asks", name)
	}
	if got := readUnit(t, name+".path").one(t, name+".path", "Path", "PathChanged"); got != runDir+"/leads-research" {
		t.Errorf("%s.path watches %s", name, got)
	}
	console := readUnit(t, "leadhound-console.service")
	if got := console["Service"]["LoadCredential"]; len(got) != 0 {
		t.Errorf("the console gets %q; it must hold no key", got)
	}
	for _, other := range []string{"leadhound-mailbox-check", "leadhound-preview", "leadhound-fetch"} {
		if got := readUnit(t, other+".service")["Service"]["LoadCredential"]; slices.Contains(got, anthropicCredential) {
			t.Errorf("%s gets the Anthropic key", other)
		}
	}
}
