package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/console"
	"github.com/gremy/webbership-leadhound/internal/leadconfig"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/store"
)

// whyClock is the whyNot tests' now: Tuesday 27 Oct 2026, 10:00 UTC, inside 09:00-17:00.
var whyClock = time.Date(2026, 10, 27, 10, 0, 0, 0, time.UTC)

// whyScene is a live, unpaused account with the send timer on and one ready mailbox (1,
// outreach@sender.example, assigned to campaign 1 "Drupal EOL") holding two due emails to GB,
// at 10 a day from 1 Oct, sending 09:00-17:00 Monday to Friday, all in UTC.
func whyScene() sendFacts {
	cfg := leadconfig.Config{}
	cfg.Caps.StartDate = leadconfig.ParseDate("2026-10-01")
	cfg.Caps.Ramp = []leadconfig.Ramp{{FromDay: 0, PerDay: 10}}
	cfg.Schedule.BusinessHours = leadconfig.BusinessHours{Start: "09:00", End: "17:00", Days: []string{"Mon", "Tue", "Wed", "Thu", "Fri"}}
	cfg.Countries = map[string]leadconfig.Country{"GB": {Channel: "email"}}
	next := whyClock.Add(time.Hour)
	passed := &leadstore.MailboxCheck{Outcome: leadstore.CheckPassed}
	mb := leadstore.MailboxStatus{State: leadstore.MailboxActive, LastCheck: passed}
	mb.ID, mb.Address, mb.Domain = 1, "outreach@sender.example", "sender.example"
	due := whyClock.Add(-time.Hour)
	row := func(id int64, to string) leadstore.SendRow {
		r := leadstore.SendRow{CampaignID: 1, LeadKind: "drupal7", OrgCountry: "GB"}
		r.ID, r.BatchID, r.LeadID, r.Channel, r.ToAddress, r.MailboxID, r.DueAt = id, 1, id, "email", to, 1, &due
		return r
	}
	return sendFacts{
		live: true, sendTimer: true, sendNext: &next,
		rows:      []leadstore.SendRow{row(1, "info@a.example"), row(2, "info@b.example")},
		mailboxes: map[int64]leadstore.MailboxStatus{1: mb},
		assigned:  map[[2]int64]bool{{1, 1}: true},
		campaigns: map[int64]string{1: "Drupal EOL"},
		cfg:       cfg, loc: time.UTC, now: whyClock,
	}
}

// TestWhyNotSentences: each reason's own sentence, the first that applies winning, over facts
// with exactly that one thing in the way (and, for the order, two things).
func TestWhyNotSentences(t *testing.T) {
	setMailbox := func(f *sendFacts, edit func(*leadstore.MailboxStatus)) {
		m := f.mailboxes[1]
		edit(&m)
		f.mailboxes = map[int64]leadstore.MailboxStatus{1: m}
	}
	tomorrow9 := time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name    string
		edit    func(f *sendFacts)
		reason  int
		blocked bool
		want    string
		next    time.Time
	}{
		{"1 shadow", func(f *sendFacts) { f.live = false }, whyShadow, true,
			"Shadow mode: LEADHOUND_OUTREACH is not live on the server, so nothing is sent for real.", time.Time{}},
		{"1 shadow wins over a pause", func(f *sendFacts) {
			f.live, f.paused, f.pausedAt = false, true, whyClock.Add(-25*time.Hour)
		}, whyShadow, true, "Shadow mode: LEADHOUND_OUTREACH is not live on the server, so nothing is sent for real.", time.Time{}},
		{"2 sending off, with its reason", func(f *sendFacts) {
			f.paused, f.pausedAt, f.pauseReason = true, whyClock.Add(-25*time.Hour), "café mailbox first."
		}, whyPaused, true, "Sending is off since Mon 26 Oct 09:00: café mailbox first.", time.Time{}},
		{"2 sending off, no reason", func(f *sendFacts) { f.paused, f.pausedAt = true, whyClock.Add(-25*time.Hour) },
			whyPaused, true, "Sending is off since Mon 26 Oct 09:00.", time.Time{}},
		{"2 sending off wins over no timer and nothing waiting", func(f *sendFacts) {
			f.paused, f.pausedAt, f.sendTimer, f.rows = true, whyClock.Add(-25*time.Hour), false, nil
		}, whyPaused, true, "Sending is off since Mon 26 Oct 09:00.", time.Time{}},
		{"3 no send timer", func(f *sendFacts) { f.sendTimer = false }, whyNoTimer, true,
			"The send timer isn't enabled on the server.", time.Time{}},
		{"4 nothing approved", func(f *sendFacts) { f.rows = nil }, whyNothing, true,
			"Nothing approved is waiting to go out.", time.Time{}},
		{"4 nothing approved on this mailbox", func(f *sendFacts) { f.rows, f.mailboxScoped = nil, true }, whyNothing, true,
			"No approved email uses this mailbox.", time.Time{}},
		{"4 approved, none due yet", func(f *sendFacts) {
			later := whyClock.Add(26 * time.Hour)
			for i := range f.rows {
				f.rows[i].DueAt = &later
			}
		}, whyNotDue, true, "Nothing approved is due yet; the next email falls due Wed 28 Oct 12:00.", whyClock.Add(26 * time.Hour)},
		{"5 mailbox paused", func(f *sendFacts) {
			setMailbox(f, func(m *leadstore.MailboxStatus) { m.State, m.Reason = leadstore.MailboxPaused, "two bounces" })
		}, whyMailbox, true, "2 approved emails wait on outreach@sender.example: it is paused (two bounces).", time.Time{}},
		{"5 mailbox check failed", func(f *sendFacts) {
			setMailbox(f, func(m *leadstore.MailboxStatus) {
				m.LastCheck = &leadstore.MailboxCheck{Outcome: leadstore.CheckFailed}
			})
		}, whyMailbox, true, "2 approved emails wait on outreach@sender.example: its latest check has not passed.", time.Time{}},
		{"5 one email, no mailbox", func(f *sendFacts) { f.rows = f.rows[:1]; f.rows[0].MailboxID = 9 }, whyMailbox, true,
			"1 approved email waits with no mailbox to send from: they were drafted before mailboxes.", time.Time{}},
		{"6 not assigned", func(f *sendFacts) { f.assigned = map[[2]int64]bool{} }, whyUnassigned, true,
			"2 approved emails wait: outreach@sender.example is not assigned to Drupal EOL.", time.Time{}},
		{"6 wins over the cap", func(f *sendFacts) {
			f.assigned = map[[2]int64]bool{}
			setMailbox(f, func(m *leadstore.MailboxStatus) { m.SentToday = 10 })
		}, whyUnassigned, true, "2 approved emails wait: outreach@sender.example is not assigned to Drupal EOL.", time.Time{}},
		{"7 warm-up limit used", func(f *sendFacts) {
			setMailbox(f, func(m *leadstore.MailboxStatus) { m.SentToday = 10 })
		}, whyCap, true, "Today's warm-up limit is used (10 of 10); the next send is Wed 28 Oct 09:00.", tomorrow9},
		{"7 warm-up limit used, wins over the hours", func(f *sendFacts) {
			f.now = time.Date(2026, 10, 27, 18, 0, 0, 0, time.UTC)
			setMailbox(f, func(m *leadstore.MailboxStatus) { m.SentToday = 10 })
		}, whyCap, true, "Today's warm-up limit is used (10 of 10); the next send is Wed 28 Oct 09:00.", tomorrow9},
		{"7 on a Friday, the next send is Monday", func(f *sendFacts) {
			f.now = time.Date(2026, 10, 30, 10, 0, 0, 0, time.UTC)
			setMailbox(f, func(m *leadstore.MailboxStatus) { m.SentToday = 10 })
		}, whyCap, true, "Today's warm-up limit is used (10 of 10); the next send is Mon 2 Nov 09:00.", time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)},
		{"7 the domain's limit used, not the mailbox's", func(f *sendFacts) {
			f.cfg.Caps.DomainPerDay = 12
			setMailbox(f, func(m *leadstore.MailboxStatus) { m.SentToday = 5 })
			twin := f.mailboxes[1]
			twin.ID, twin.Address, twin.SentToday = 2, "desk@sender.example", 7
			f.mailboxes[2] = twin
		}, whyCap, true, "Today's sending limit for sender.example is used (12 of 12); the next send is Wed 28 Oct 09:00.", tomorrow9},
		{"7 the domain's limit used by a retired mailbox's sends", func(f *sendFacts) {
			f.cfg.Caps.DomainPerDay = 12
			setMailbox(f, func(m *leadstore.MailboxStatus) { m.SentToday = 5 })
			gone := f.mailboxes[1]
			gone.ID, gone.Address, gone.SentToday, gone.State = 2, "old@sender.example", 7, leadstore.MailboxRetired
			f.mailboxes[2] = gone
		}, whyCap, true, "Today's sending limit for sender.example is used (12 of 12); the next send is Wed 28 Oct 09:00.", tomorrow9},
		{"7 the warm-up starts on a later date", func(f *sendFacts) { f.cfg.Caps.StartDate = leadconfig.ParseDate("2026-11-16") }, whyCap, true,
			"The warm-up starts on Mon 16 Nov.", time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)},
		{"7 the warm-up starts today", func(f *sendFacts) { f.cfg.Caps.StartDate = leadconfig.ParseDate("2026-10-27") }, whyGoes, false,
			"2 approved emails go out at the next run, Tue 27 Oct 11:00.", whyClock.Add(time.Hour)},
		{"7 first-send, nothing sent yet", func(f *sendFacts) {
			f.cfg.Caps.StartDate, f.firstSend = leadconfig.ParseDate(leadconfig.FirstSend), true
		}, whyGoes, false, "2 approved emails go out at the next run, Tue 27 Oct 11:00.", whyClock.Add(time.Hour)},
		{"8 outside the sending window", func(f *sendFacts) { f.now = time.Date(2026, 10, 27, 18, 0, 0, 0, time.UTC) }, whyHours, true,
			"2 approved emails go out in the next sending window, Wed 28 Oct 09:00.", tomorrow9},
		{"9 not blocked", func(*sendFacts) {}, whyGoes, false,
			"2 approved emails go out at the next run, Tue 27 Oct 11:00.", whyClock.Add(time.Hour)},
		{"9 not blocked, two to one domain go one at a time", func(f *sendFacts) { f.rows[1].ToAddress = "sales@a.example" }, whyGoes, false,
			"1 approved email goes out at the next run, Tue 27 Oct 11:00.", whyClock.Add(time.Hour)},
		{"9 not blocked, next run unknown", func(f *sendFacts) { f.sendNext = nil }, whyGoes, false,
			"2 approved emails go out at the next send run.", time.Time{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := whyScene()
			tc.edit(&f)
			got := whyNot(f)
			if got.Reason != tc.reason || got.Blocked != tc.blocked || got.Sentence != tc.want || !got.Next.Equal(tc.next) {
				t.Errorf("whyNot = reason %d, blocked %v, %q, next %s\nwant reason %d, blocked %v, %q, next %s",
					got.Reason, got.Blocked, got.Sentence, got.Next, tc.reason, tc.blocked, tc.want, tc.next)
			}
			if v := got.view(); v.Sentence != got.Sentence || (v.Next == nil) != got.Next.IsZero() {
				t.Errorf("view %+v of %+v", v, got)
			}
		})
	}
}

// TestWhyNotNamesTheMailboxWithMost: with emails held on two mailboxes, the sentence names the
// one holding more, and on a tie the lower id.
func TestWhyNotNamesTheMailboxWithMost(t *testing.T) {
	f := whyScene()
	second := f.mailboxes[1]
	second.ID, second.Address, second.State = 2, "hello@other.example", leadstore.MailboxPaused
	first := f.mailboxes[1]
	first.State = leadstore.MailboxPaused
	f.mailboxes = map[int64]leadstore.MailboxStatus{1: first, 2: second}
	extra := f.rows[0]
	extra.ID, extra.MailboxID, extra.ToAddress = 3, 2, "info@c.example"
	f.rows = append(f.rows[:1], extra)
	if got := whyNot(f).Sentence; got != "1 approved email waits on outreach@sender.example: it is paused." {
		t.Errorf("a tie: %q", got)
	}
	extra.ID, extra.ToAddress = 4, "info@d.example"
	f.rows = append(f.rows, extra)
	if got := whyNot(f).Sentence; got != "2 approved emails wait on hello@other.example: it is paused." {
		t.Errorf("two on mailbox 2: %q", got)
	}
}

// systemctlSample is systemctl show's output for controlUnits on a server whose finding
// timers are on and sending timers off (systemd 255, --timestamp=unix): each block lists the
// timer's own properties before the unit's, the research watcher has no next run, and a unit
// systemd does not know is not-found.
const systemctlSample = `NextElapseUSecRealtime=
Id=leadhound-leads-batch.timer
LoadState=loaded
ActiveState=inactive
UnitFileState=disabled

NextElapseUSecRealtime=
Id=leadhound-leads-sync.timer
LoadState=loaded
ActiveState=inactive
UnitFileState=disabled

NextElapseUSecRealtime=@1761901200
Id=leadhound-leads-probe.timer
LoadState=loaded
ActiveState=active
UnitFileState=enabled

NextElapseUSecRealtime=@1761984000
Id=leadhound-leads-contacts.timer
LoadState=loaded
ActiveState=active
UnitFileState=enabled

Id=leadhound-leads-send.timer
LoadState=not-found
ActiveState=inactive
UnitFileState=

Id=leadhound-leads-research.path
LoadState=loaded
ActiveState=active
UnitFileState=enabled
`

// TestTimerParseSystemctlShow: the sample reads unit by unit; the next run is the @seconds
// time; a unit systemd does not know is neither loaded nor enabled; output that is not one
// block per unit is an error, never a guess.
func TestTimerParseSystemctlShow(t *testing.T) {
	got, err := parseSystemctlShow([]byte(systemctlSample), controlUnits)
	if err != nil {
		t.Fatal(err)
	}
	probeNext := time.Unix(1761901200, 0)
	if st := got[unitProbe]; !st.Loaded || !st.Enabled || !st.Active || st.Next == nil || !st.Next.Equal(probeNext) {
		t.Errorf("probe %+v, want loaded, enabled, active, next %s", st, probeNext)
	}
	if st := got[unitBatch]; !st.Loaded || st.Enabled || st.Active || st.Next != nil {
		t.Errorf("batch %+v, want loaded and nothing else", st)
	}
	if st := got[unitSend]; st.Loaded || st.Enabled || st.Next != nil {
		t.Errorf("send %+v, want not loaded", st)
	}
	if st := got[unitResearch]; !st.Enabled || st.Next != nil {
		t.Errorf("research %+v, want enabled with no next run", st)
	}
	for _, bad := range []string{"", "Id=leadhound-leads-batch.timer\n", strings.Replace(systemctlSample, "UnitFileState=enabled", "garbage", 1),
		strings.Replace(systemctlSample, "@1761901200", "@soon", 1), strings.Replace(systemctlSample, "leadhound-leads-sync.timer", "other.timer", 1)} {
		if _, err := parseSystemctlShow([]byte(bad), controlUnits); err == nil {
			t.Errorf("parsed %q", bad)
		}
	}
	for v, want := range map[string]*time.Time{
		"": nil, "n/a": nil, "0": nil, "infinity": nil,
		"@1761901200": &probeNext, "1761901200000000": &probeNext,
		"Fri 2025-10-31 09:00:00 UTC": new(time.Date(2025, 10, 31, 9, 0, 0, 0, time.UTC)),
	} {
		got, err := parseSystemdTime(v)
		if err != nil || (got == nil) != (want == nil) || (got != nil && !got.Equal(*want)) {
			t.Errorf("parseSystemdTime(%q) = %v, %v; want %v", v, got, err, want)
		}
	}
}

// stubSystemctl makes systemctlShow return out and err for the test.
func stubSystemctl(t *testing.T, out string, err error) *[]string {
	t.Helper()
	saved := systemctlShow
	t.Cleanup(func() { systemctlShow = saved })
	var asked []string
	systemctlShow = func(_ context.Context, units []string) ([]byte, error) {
		asked = slices.Clone(units)
		return []byte(out), err
	}
	return &asked
}

// envOf is a getenv over m.
func envOf(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

// TestTimerReadFromSystemd: readTimers asks for every control unit in one call and says the
// state came from systemd.
func TestTimerReadFromSystemd(t *testing.T) {
	asked := stubSystemctl(t, systemctlSample, nil)
	r := readTimers(context.Background(), envOf(nil))
	if !r.fromSystemd || r.note != "" || !slices.Equal(*asked, controlUnits) {
		t.Fatalf("read %+v, asked %v", r, *asked)
	}
	if !r.units[unitContacts].Enabled || r.units[unitSync].Enabled {
		t.Errorf("units %+v", r.units)
	}
}

// TestTimerFailureLoggedOnce: a systemctl that fails is logged once per process, not on every
// page that reads the timers.
func TestTimerFailureLoggedOnce(t *testing.T) {
	stubSystemctl(t, "", errors.New("exec: systemctl: not found"))
	timersLogged.Store(false)
	t.Cleanup(func() { timersLogged.Store(false) })
	var buf bytes.Buffer
	saved := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(saved) })
	for range 3 {
		readTimers(context.Background(), envOf(nil))
	}
	if n := strings.Count(buf.String(), "systemctl show failed"); n != 1 {
		t.Errorf("logged %d times, want once:\n%s", n, buf.String())
	}
	if systemctlWait > 2*time.Second {
		t.Errorf("a page waits %s for systemd", systemctlWait)
	}
}

// TestTimerFallback: when systemctl fails, or prints what cannot be read, the timers are
// inferred from the environment as install.sh applies it, and the note says so.
func TestTimerFallback(t *testing.T) {
	for _, tc := range []struct {
		name, out string
		err       error
	}{
		{"systemctl fails", "", errors.New("exec: systemctl: not found")},
		{"output unreadable", "Id=leadhound-leads-batch.timer\n", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stubSystemctl(t, tc.out, tc.err)
			r := readTimers(context.Background(), envOf(map[string]string{"LEADHOUND_LEADS_TIMERS": "on"}))
			if r.fromSystemd || r.note != timersNotRead {
				t.Fatalf("read %+v, want the fallback with its note", r)
			}
			for unit, want := range map[string]bool{unitProbe: true, unitContacts: true, unitBatch: false, unitSend: false, unitSync: false, unitResearch: true} {
				if st := r.units[unit]; st.Enabled != want || st.Next != nil {
					t.Errorf("%s %+v, want enabled %v and no next run", unit, st, want)
				}
			}
		})
	}
}

// installFunction is the shell function name() { ... } of deploy/install.sh, through its
// closing brace.
func installFunction(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "deploy", "install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	in := false
	for line := range strings.Lines(string(b)) {
		in = in || strings.HasPrefix(line, name+"() {")
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

// TestTimerFallbackMatchesInstall: the fallback's rule and install.sh's own leads_timers,
// fed the same settings, enable exactly the same timers (the research watcher, which
// install.sh enables on every deploy, aside).
func TestTimerFallbackMatchesInstall(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "run.sh")
	body := "set -eu\n" + installFunction(t, "env_value") + installFunction(t, "leads_timers") + "leads_timers \"$1\"\n"
	if err := os.WriteFile(script, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "systemctl"), []byte("#!/bin/sh\necho \"$*\" >>\"$SYSTEMCTL_LOG\"\n"), 0o700); err != nil { // #nosec G306 -- an executable stub in a test temp dir
		t.Fatal(err)
	}
	for _, timers := range []string{"", "on", "off", "online"} {
		for _, outreachV := range []string{"", "live", "shadow"} {
			for _, send := range []string{"", "on", "off"} {
				env := map[string]string{"LEADHOUND_LEADS_TIMERS": timers, "LEADHOUND_OUTREACH": outreachV, "LEADHOUND_LEADS_SEND": send}
				var file strings.Builder
				for _, k := range slices.Sorted(func(yield func(string) bool) {
					for k := range env {
						if !yield(k) {
							return
						}
					}
				}) {
					file.WriteString(k + "=" + env[k] + "\n")
				}
				envPath := filepath.Join(t.TempDir(), "env")
				if err := os.WriteFile(envPath, []byte(file.String()), 0o600); err != nil {
					t.Fatal(err)
				}
				log := filepath.Join(t.TempDir(), "systemctl.log")
				cmd := exec.Command(sh, script, envPath) // #nosec G204 -- the test's own script
				cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "SYSTEMCTL_LOG="+log)
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("leads_timers: %v\n%s", err, out)
				}
				b, err := os.ReadFile(log) // #nosec G304 -- the test's own temp file
				if err != nil {
					t.Fatal(err)
				}
				installed := map[string]bool{}
				for line := range strings.Lines(string(b)) {
					f := strings.Fields(line)
					if len(f) > 2 && f[0] == "enable" {
						for _, u := range f[2:] {
							installed[u] = true
						}
					}
				}
				for unit, on := range inferredTimers(envOf(env)) {
					if unit == unitResearch {
						continue
					}
					if installed[unit] != on {
						t.Errorf("%v: %s inferred %v, install.sh enables it: %v", env, unit, on, installed[unit])
					}
				}
			}
		}
	}
}

// TestControlsServerGuards: the three guards set on the server, each with its value, what it
// means and the line that flips it; a value the server does not understand reads as off.
func TestControlsServerGuards(t *testing.T) {
	for _, tc := range []struct {
		env  map[string]string
		want []console.ServerGuard
	}{
		{nil, []console.ServerGuard{
			{Name: "LEADHOUND_OUTREACH", Value: "", State: "Shadow: nothing is sent for real.", Line: "LEADHOUND_OUTREACH=live"},
			{Name: "LEADHOUND_LIVE", Value: "", State: "Off: no outbound fetches and no paid Claude or Jev calls.", Line: "LEADHOUND_LIVE=1"},
			{Name: "LEADHOUND_LEADS_TIMERS", Value: "", State: "Off: the deploy installs no leads timer.", Line: "LEADHOUND_LEADS_TIMERS=on"},
		}},
		{map[string]string{"LEADHOUND_OUTREACH": "live", "LEADHOUND_LIVE": "1", "LEADHOUND_LEADS_TIMERS": "on"}, []console.ServerGuard{
			{Name: "LEADHOUND_OUTREACH", Value: "live", State: "Live: approved emails are sent for real.", Line: "LEADHOUND_OUTREACH="},
			{Name: "LEADHOUND_LIVE", Value: "1", State: "On: outbound fetches and paid Claude and Jev calls are allowed.", Line: "LEADHOUND_LIVE="},
			{Name: "LEADHOUND_LEADS_TIMERS", Value: "on", State: "On: the deploy installs the leads timers (batch, send and sync only with live sending).",
				Line: "LEADHOUND_LEADS_TIMERS=off"},
		}},
		{map[string]string{"LEADHOUND_LIVE": "yes"}, nil},
	} {
		got := serverGuards(envOf(tc.env))
		if tc.want == nil {
			if got[1].State != "Not understood, so off: no outbound fetches and no paid Claude or Jev calls." || got[1].Line != "LEADHOUND_LIVE=1" {
				t.Errorf("LEADHOUND_LIVE=yes: %+v", got[1])
			}
			continue
		}
		if !slices.Equal(got, tc.want) {
			t.Errorf("%v:\n got %+v\nwant %+v", tc.env, got, tc.want)
		}
	}
}

// TestControlsRows: every switched job has a row, in the console's order, named as the jobs
// name themselves, and every job that runs on a unit names one Controls asks systemd about;
// a run reads in plain words.
func TestControlsRows(t *testing.T) {
	var jobs []string
	for _, r := range controlJobs {
		jobs = append(jobs, r.job)
		if r.unit != "" && !slices.Contains(controlUnits, r.unit) {
			t.Errorf("%s: unit %s is not asked about", r.job, r.unit)
		}
		if r.what == "" || r.schedule == "" {
			t.Errorf("%s: no words", r.job)
		}
	}
	if !slices.Equal(jobs, console.ControlJobs) || !slices.Equal(jobs, leadstore.Jobs) {
		t.Errorf("rows %v, console %v, store %v", jobs, console.ControlJobs, leadstore.Jobs)
	}
	now := whyClock
	done := now.Add(-time.Minute)
	for _, tc := range []struct {
		run    func(r *store.Run)
		result string
		failed bool
	}{
		{func(r *store.Run) {}, "Finished", false},
		{func(r *store.Run) { r.Note = runNoteSwitchedOff }, "Switched off", false},
		{func(r *store.Run) { r.Note = notePaused }, "Sending was off", false},
		{func(r *store.Run) { r.StopReason = "spend_cap" }, "Stopped: today's spending limit was reached", true},
		{func(r *store.Run) { r.Failures = 2 }, "Finished with 2 failures", true},
		{func(r *store.Run) { r.FinishedAt = nil }, "Running", false},
		{func(r *store.Run) { r.FinishedAt, r.StartedAt = nil, now.Add(-25*time.Hour) }, "Did not finish", true},
	} {
		r := store.Run{StartedAt: now.Add(-time.Hour), FinishedAt: &done}
		tc.run(&r)
		if v := runView(r, now); v.Result != tc.result || v.Failed != tc.failed {
			t.Errorf("%+v: %q failed %v, want %q %v", r, v.Result, v.Failed, tc.result, tc.failed)
		}
	}
}

// TestControlsRampStart: the card's start date is caps.start_date only while it is ahead of
// the operator's today; first-send and a date reached give none.
func TestControlsRampStart(t *testing.T) {
	bucharest, err := time.LoadLocation("Europe/Bucharest")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		start string
		now   time.Time
		want  string
	}{
		{"2026-11-16", whyClock, "2026-11-16"},
		{"2026-10-28", time.Date(2026, 10, 27, 22, 30, 0, 0, time.UTC), ""}, // already the 28th in Bucharest
		{"2026-10-27", whyClock, ""},
		{"2026-10-01", whyClock, ""},
		{leadconfig.FirstSend, whyClock, ""},
	} {
		var cfg leadconfig.Config
		cfg.Caps.StartDate = leadconfig.ParseDate(tc.start)
		got := ""
		if s := rampStart(cfg, bucharest, tc.now); s != nil {
			got = s.Format(time.DateOnly)
		}
		if got != tc.want {
			t.Errorf("start %s at %s: %q, want %q", tc.start, tc.now, got, tc.want)
		}
	}
}
