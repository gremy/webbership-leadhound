//go:build integration

package main

import (
	"context"
	"testing"

	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/leadstore/leadstoretest"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

func switchRows(t *testing.T, table string) int {
	t.Helper()
	var n int
	if err := storetest.OwnerConn(t).QueryRow(context.Background(), `SELECT count(*) FROM `+table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func switchRun(t *testing.T, args ...string) string {
	t.Helper()
	code, out, errOut := leadsRun(t, args...)
	if code != 0 {
		t.Fatalf("%q exited %d:\n%s%s", args, code, out, errOut)
	}
	return out
}

// TestLeadsSwitchListSet: cold start everything on; set off and on, in plain words; an
// unchanged set writes nothing; bad arguments exit 2.
func TestLeadsSwitchListSet(t *testing.T) {
	leadsDB(t)
	out := switchRun(t, "leads", "switches", "list")
	wantLines(t, "cold list", out, "Sending: on · never switched", "Morning batch: on · never switched", "Inbox sync: on",
		"Site probe: on", "Contacts: on", "Research: on", "Email check: on")

	out = switchRun(t, "leads", "switches", "set", "probe", "off", "--reason", "too many fetches")
	wantLines(t, "off", out, "Site probe is switched off")
	out = switchRun(t, "leads", "switches", "list")
	wantLines(t, "list after off", out, "Site probe: off · since ", "· too many fetches", "Morning batch: on · never switched")

	out = switchRun(t, "leads", "switches", "set", "probe", "off")
	wantLines(t, "off again", out, "Site probe is already off; nothing changed.")
	if n := switchRows(t, "job_switch"); n != 1 {
		t.Errorf("job_switch rows %d after an unchanged set, want 1", n)
	}
	out = switchRun(t, "leads", "switches", "set", "probe", "on")
	wantLines(t, "on", out, "Site probe is switched on")
	s, err := leadstoretest.Q(t).JobSwitch(context.Background(), leadstore.DefaultAccountID, leadstore.JobProbe)
	if err != nil || !s.On || s.Actor != leadstore.ActorGremy {
		t.Errorf("probe after on: %+v %v", s, err)
	}

	for _, args := range [][]string{
		{"leads", "switches"},
		{"leads", "switches", "set", "send", "off"},
		{"leads", "switches", "set", "probe"},
		{"leads", "switches", "set", "probe", "maybe"},
		{"leads", "switches", "list", "extra"},
		{"leads", "switches", "convert-send-env"},
		{"leads", "switches", "nope"},
	} {
		if code, _, _ := leadsRun(t, args...); code != 2 {
			t.Errorf("%q exited %d, want 2", args, code)
		}
	}
	if n := switchRows(t, "job_switch"); n != 2 {
		t.Errorf("job_switch rows %d, want 2", n)
	}
}

// TestLeadsSwitchConvertSendEnv: the one-time conversion pauses sending as system when the
// old switch was anything but on (empty counts as off, quotes and blanks as env_value
// drops them), writes nothing when it was on, and writes no second row when run again.
func TestLeadsSwitchConvertSendEnv(t *testing.T) {
	for name, tc := range map[string]struct {
		was   string
		pause bool
	}{
		"on":        {"on", false},
		"quoted on": {` "on" `, false},
		"empty":     {"", true},
		"off":       {"off", true},
		"ON":        {"ON", true},
	} {
		t.Run(name, func(t *testing.T) {
			leadsDB(t)
			out := switchRun(t, "leads", "switches", "convert-send-env", "--was", tc.was)
			paused, reason, _, err := leadstoretest.Q(t).SendPaused(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			want := 0
			if tc.pause {
				want = 1
				wantLines(t, "convert", out, "sending is now paused")
				if !paused || reason != convertSendReason {
					t.Errorf("paused %v %q", paused, reason)
				}
				var actor string
				if err := storetest.OwnerConn(t).QueryRow(context.Background(), `SELECT actor FROM send_pause`).Scan(&actor); err != nil || actor != leadstore.ActorSystem {
					t.Errorf("actor %q (%v), want system", actor, err)
				}
			} else {
				wantLines(t, "convert", out, "Sending was on on the server")
				if paused {
					t.Error("was on, but sending is paused")
				}
			}
			out = switchRun(t, "leads", "switches", "convert-send-env", "--was", tc.was)
			if tc.pause {
				wantLines(t, "convert twice", out, "already paused: nothing changed")
			}
			if n := switchRows(t, "send_pause"); n != want {
				t.Errorf("send_pause rows %d after two runs, want %d", n, want)
			}
		})
	}
}
