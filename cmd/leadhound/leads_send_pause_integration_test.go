//go:build integration

package main

import (
	"context"
	"strings"
	"testing"

	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

// TestSendPauseBeforeRun: a pause set before a run sends nothing and marks nothing sending;
// a resume lets the next run send.
func TestSendPauseBeforeRun(t *testing.T) {
	w := newSendWorld(t, true, 20)
	w.batch(t, false, map[int64]string{w.lead(t, "alpha.example"): "alpha.example"})

	out := w.mustRun(t, "leads", "send", "pause", "--reason", "reading the copy again")
	wantLines(t, "pause", out, "Sending is paused")
	paused, reason, _, err := w.lq.SendPaused(context.Background())
	if err != nil || !paused || reason != "reading the copy again" {
		t.Fatalf("after pause: %v %q %v", paused, reason, err)
	}

	out = w.mustRun(t, "leads", "send")
	wantLines(t, "paused run", out, "Sending is paused by the operator")
	if strings.Contains(out, "Nothing due to send") {
		t.Errorf("a paused run says nothing is due:\n%s", out)
	}
	if w.smtp.count() != 0 {
		t.Fatal("a paused run sent")
	}
	if s := w.msgState(t, "alpha.example", 0); s != leadstore.MsgApproved {
		t.Errorf("alpha %s, want approved", s)
	}
	if note := w.lastNote(t, commandLeadsSend); !strings.Contains(note, "notes paused by the operator") {
		t.Errorf("run note %q", note)
	}

	// The selftest obeys the pause too, and makes no selftest rows.
	out = w.mustRun(t, "leads", "selftest", "--to", testOperator)
	wantLines(t, "paused selftest", out, "Sending is paused by the operator")
	if w.smtp.count() != 0 || w.count(t, `SELECT count(*) FROM lead WHERE kind = 'selftest'`) != 0 {
		t.Fatal("a paused selftest ran")
	}

	out = w.mustRun(t, "leads", "send", "resume")
	wantLines(t, "resume", out, "Sending is resumed")
	out = w.mustRun(t, "leads", "send")
	wantLines(t, "resumed run", out, "Sent 1: alpha.example")
	if s := w.msgState(t, "alpha.example", 0); s != leadstore.MsgSent {
		t.Errorf("alpha %s after resume, want sent", s)
	}
	w.sweep(t, "info@alpha.example", "reading the copy again")
}

// TestSendPauseMidRun: a pause set between two messages stops the run before the second,
// which stays approved (never sending) and goes on the next run after a resume.
func TestSendPauseMidRun(t *testing.T) {
	w := newSendWorld(t, true, 20)
	w.batch(t, false, map[int64]string{w.lead(t, "alpha.example"): "alpha.example", w.lead(t, "bravo.example"): "bravo.example"})
	q := w.lq
	w.smtp.onSend = func(n int, _ string) {
		if n == 0 {
			if err := q.SetSendPause(context.Background(), true, ""); err != nil {
				t.Error(err)
			}
		}
	}
	out := w.mustRun(t, "leads", "send")
	wantLines(t, "mid-run pause", out, "Sent 1: alpha.example", "Sending is paused by the operator")
	if w.smtp.count() != 1 {
		t.Fatalf("SMTP got %v, want alpha only", w.smtp.recipients())
	}
	if s := w.msgState(t, "bravo.example", 0); s != leadstore.MsgApproved {
		t.Errorf("bravo %s, want approved", s)
	}
	if n := w.count(t, `SELECT count(*) FROM outreach_message WHERE state = 'sending'`); n != 0 {
		t.Errorf("%d messages left sending", n)
	}
	w.smtp.onSend = nil

	// Still paused: the next run sends nothing.
	w.mustRun(t, "leads", "send")
	if w.smtp.count() != 1 {
		t.Fatal("a paused run sent")
	}
	w.mustRun(t, "leads", "send", "resume")
	out = w.mustRun(t, "leads", "send")
	wantLines(t, "after resume", out, "Sent 1: bravo.example")
	w.sweep(t, "info@alpha.example", "info@bravo.example")
}

func TestSendPauseUsage(t *testing.T) {
	w := newSendWorld(t, true, 20)
	for _, args := range [][]string{{"leads", "send", "pause", "extra"}, {"leads", "send", "resume", "--reason", "x"}, {"leads", "send", "stop"}} {
		if code, out := w.run(t, args...); code != 2 {
			t.Errorf("%q exited %d:\n%s", args, code, out)
		}
	}
	// Resume while not paused, and pause twice, are no-ops that say so.
	wantLines(t, "resume unpaused", w.mustRun(t, "leads", "send", "resume"), "Sending is not paused")
	w.mustRun(t, "leads", "send", "pause")
	wantLines(t, "pause twice", w.mustRun(t, "leads", "send", "pause"), "Sending is already paused")
	if n := w.count(t, `SELECT count(*) FROM send_pause`); n != 1 {
		t.Errorf("send_pause rows %d, want 1", n)
	}
}

// TestSendRampFromFirstSend: with caps.start_date first-send the ramp's day 0 is the first
// live send; with none yet it is today. A selftest never counts as the first send.
func TestSendRampFromFirstSend(t *testing.T) {
	w := newSendWorld(t, true, 1)
	w.editConfig(t, `start_date: 2026-10-01`, `start_date: first-send`)
	w.editConfig(t, `ramp: \[.*\]`, "ramp: [{from_day: 0, per_day: 1}, {from_day: 7, per_day: 3}]")
	owner := storetest.OwnerConn(t)
	ago := func(days string) {
		t.Helper()
		if _, err := owner.Exec(context.Background(),
			`UPDATE outreach_message SET sent_at = sent_at - $1::interval WHERE sent_at IS NOT NULL`, days+" days"); err != nil {
			t.Fatal(err)
		}
	}

	// A selftest eight days ago is not a first send: today is still day 0.
	w.mustRun(t, "leads", "selftest", "--to", testOperator)
	ago("8")
	leads := map[int64]string{}
	for _, d := range []string{"alpha.example", "bravo.example", "charlie.example"} {
		leads[w.lead(t, d)] = d
	}
	w.batch(t, false, leads)
	out := w.mustRun(t, "leads", "send")
	wantLines(t, "day 0", out, "Sent 1: ", "Held for a later run: today's cap 2.")

	// That send was eight days ago: week 2's cap of 3, none sent today, two left.
	ago("8")
	out = w.mustRun(t, "leads", "send")
	wantLines(t, "day 8", out, "Sent 2: ")
	w.sweep(t, "info@alpha.example", "info@bravo.example", "info@charlie.example")
}
