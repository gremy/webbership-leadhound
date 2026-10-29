package deploy

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestInstallSendSwitchConvert: install.sh carries the retired LEADHOUND_LEADS_SEND line over
// to the console's sending switch once, after the binary swap and before leads_timers can
// enable a sending timer. It runs install.sh's own send_switch_convert, then its leads_timers,
// with systemd-run, systemctl and chown replaced by recorders, over a sequence of deploys on
// one marker, which lives in root-owned /etc/leadhound and never in the leadhound-owned data
// dir, so the app user cannot delete it and a data-dir restore cannot lose it. The first
// deploy calls the installed binary as leadhound with the app env and the env's value (empty
// when no line sets it) and writes the marker 0640; a later one, or one with
// the line removed, calls nothing; a failed conversion fails the deploy, writes no marker and
// leaves the timers untouched.
func TestInstallSendSwitchConvert(t *testing.T) {
	steps := installSteps(t)
	swap := firstLine(steps, "mv -f", `"$bin/.leadhound.new"`, `"$bin/leadhound"`)
	call := firstLine(steps, "send_switch_convert /etc/leadhound/env /etc/leadhound/send-switch-converted")
	timers := firstLine(steps, "leads_timers /etc/leadhound/env")
	switch {
	case call < 0:
		t.Fatal("install.sh does not run send_switch_convert /etc/leadhound/env /etc/leadhound/send-switch-converted: the marker belongs in root-owned /etc/leadhound, where the app user cannot delete it")
	case call < swap:
		t.Errorf("install.sh converts the send switch (line %d) before the binary swap (line %d)", call, swap)
	case call > timers:
		t.Errorf("install.sh converts the send switch (line %d) after leads_timers (line %d)", call, timers)
	}
	if strings.Contains(installFunc(t, "leads_timers"), "LEADHOUND_LEADS_SEND") {
		t.Error("leads_timers still reads LEADHOUND_LEADS_SEND")
	}

	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "run.sh")
	// The same order as install.sh: convert, then the timers; set -e stops at a failed convert.
	body := "set -eu\n" + installFunc(t, "fail") + installFunc(t, "env_value") +
		installFunc(t, "leads_timers") + installFunc(t, "send_switch_convert") +
		"bin=/opt/leadhound/bin\nsend_switch_convert \"$1\" \"$2\"\nleads_timers \"$1\"\n"
	if err := os.WriteFile(script, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	// Each recorder writes its name and every argument in brackets, so an empty one shows.
	record := func(name string) string {
		return "#!/bin/sh\n{ printf '" + name + "'; printf ' [%s]' \"$@\"; echo; } >>\"$CALLS\"\n"
	}
	stubs := map[string]string{
		"systemd-run": record("systemd-run") + "[ -z \"${FAKE_CONVERT_FAIL:-}\" ] || exit 3\n",
		"systemctl":   record("systemctl"),
		"chown":       record("chown"),
	}
	for name, body := range stubs {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o700); err != nil { // #nosec G306 -- an executable stub in a test temp dir
			t.Fatal(err)
		}
	}

	const (
		finding = "LEADHOUND_LEADS_TIMERS=on\n"
		live    = "LEADHOUND_OUTREACH=live\n"
	)
	type deploy struct {
		env      string
		fail     bool
		wantWas  *string // the --was value convert gets; nil: convert is not called
		wantFail bool
	}
	str := func(s string) *string { return &s }
	for _, tc := range []struct {
		name    string
		deploys []deploy
	}{
		{"send on, then again", []deploy{
			{env: finding + "LEADHOUND_LEADS_SEND=on\n" + live, wantWas: str("on")},
			{env: finding + "LEADHOUND_LEADS_SEND=on\n" + live},
		}},
		{"send unset, then again", []deploy{
			{env: finding + live, wantWas: str("")},
			{env: finding + live},
		}},
		{"send off, then the line removed", []deploy{
			{env: finding + "LEADHOUND_LEADS_SEND=off\n" + live, wantWas: str("off")},
			{env: finding + live},
		}},
		{"send quoted on", []deploy{
			{env: finding + "LEADHOUND_LEADS_SEND='on'\n" + live, wantWas: str("on")},
		}},
		{"convert fails, then succeeds", []deploy{
			{env: finding + "LEADHOUND_LEADS_SEND=on\n" + live, fail: true, wantWas: str("on"), wantFail: true},
			{env: finding + "LEADHOUND_LEADS_SEND=on\n" + live, wantWas: str("on")},
			{env: finding + live},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tmp := t.TempDir()
			env, marker := filepath.Join(tmp, "env"), filepath.Join(tmp, "etc", "send-switch-converted")
			if err := os.Mkdir(filepath.Dir(marker), 0o700); err != nil {
				t.Fatal(err)
			}
			for i, d := range tc.deploys {
				if err := os.WriteFile(env, []byte(d.env), 0o600); err != nil {
					t.Fatal(err)
				}
				_, markerBefore := os.Stat(marker)
				calls := filepath.Join(tmp, "calls"+string(rune('0'+i)))
				cmd := exec.Command(sh, script, env, marker) // #nosec G204 -- the test's own script
				cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
					"CALLS="+calls, "FAKE_CONVERT_FAIL=")
				if d.fail {
					cmd.Env = append(cmd.Env, "FAKE_CONVERT_FAIL=1")
				}
				out, err := cmd.CombinedOutput()
				if (err != nil) != d.wantFail {
					t.Fatalf("deploy %d: err = %v, want failure %t\n%s", i, err, d.wantFail, out)
				}
				b, _ := os.ReadFile(calls) // #nosec G304 -- the test's own temp file
				var converts []string
				timersTouched := false
				for line := range strings.Lines(string(b)) {
					switch {
					case strings.HasPrefix(line, "systemd-run "):
						converts = append(converts, strings.TrimSpace(line))
					case strings.HasPrefix(line, "systemctl "):
						timersTouched = true
					}
				}
				if d.wantWas == nil {
					if len(converts) > 0 {
						t.Errorf("deploy %d: convert ran with the marker present: %q", i, converts)
					}
					if markerBefore != nil {
						t.Errorf("deploy %d: no conversion was expected, but there was no marker", i)
					}
				} else {
					want := "systemd-run [--quiet] [--wait] [--pipe] [--collect] [--uid=leadhound] [--gid=leadhound]" +
						" [-p] [EnvironmentFile=" + env + "] [/opt/leadhound/bin/leadhound] [leads] [switches]" +
						" [convert-send-env] [--was] [" + *d.wantWas + "]"
					if len(converts) != 1 || converts[0] != want {
						t.Errorf("deploy %d: convert calls %q, want exactly\n%s", i, converts, want)
					}
				}
				_, markerErr := os.Stat(marker)
				switch {
				case d.wantFail:
					if markerErr == nil {
						t.Errorf("deploy %d: a failed conversion left the marker", i)
					}
					if timersTouched {
						t.Errorf("deploy %d: a failed conversion went on to the leads timers:\n%s", i, b)
					}
					if !strings.Contains(string(out), "could not be carried over") {
						t.Errorf("deploy %d: the failure does not say what failed:\n%s", i, out)
					}
				case markerErr != nil:
					t.Errorf("deploy %d: no marker after the deploy: %v", i, markerErr)
				default:
					if !timersTouched {
						t.Errorf("deploy %d: leads_timers did not run after the conversion", i)
					}
				}
				if d.wantWas != nil && !d.wantFail {
					if fi, err := os.Stat(marker); err == nil && fi.Mode().Perm() != 0o640 {
						t.Errorf("deploy %d: marker mode %v, want 0640", i, fi.Mode().Perm())
					}
					if !strings.Contains(string(b), "chown [root:root] ["+marker+"]") {
						t.Errorf("deploy %d: the marker was not made root:root; calls:\n%s", i, b)
					}
				}
			}
		})
	}
}
