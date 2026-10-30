//go:build integration

package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/console"
	"github.com/gremy/webbership-leadhound/internal/leadbrief"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

// sendNowWorld is a live send world with three GB leads approved at 10:00 London on Tuesday
// 27 Oct 2026, a cap of capPerDay, and the clock moved to at.
func sendNowWorld(t *testing.T, capPerDay int, at time.Time) *sendWorld {
	t.Helper()
	w := newSendWorld(t, true, capPerDay)
	domains := []string{"alpha.example", "bravo.example", "charlie.example"}
	leads := map[int64]string{}
	for _, d := range domains {
		leads[w.lead(t, d)] = d
	}
	w.batch(t, false, leads)
	w.clock.set(at)
	return w
}

// TestSendNowSkipsOnlyTheWindow (Task 4.2): at 21:00 recipient-local, three emails due and
// room for two, the timer's run sends nothing (outside the window) and leads send --now sends
// two and holds one for the cap; the run is recorded as Send now. A follow-up the send just
// scheduled is not due, so a second --now sends nothing more.
func TestSendNowSkipsOnlyTheWindow(t *testing.T) {
	w := sendNowWorld(t, 2, time.Date(2026, 10, 27, 21, 0, 0, 0, time.UTC))
	out := w.mustRun(t, "leads", "send", "--max", "25")
	if n := w.smtp.count(); n != 0 {
		t.Fatalf("the timer's run sent %d at 21:00:\n%s", n, out)
	}
	wantLines(t, "timer run", out, "outside business hours 3")
	if strings.Contains(w.lastNote(t, commandLeadsSend), noteSendNow) {
		t.Error("a timer run is noted as send now")
	}

	out = w.mustRun(t, "leads", "send", "--now", "--max", "25")
	if n := w.smtp.count(); n != 2 {
		t.Fatalf("send now sent %d, want 2 (the cap):\n%s", n, out)
	}
	wantLines(t, "send now", out, "Send now: this run skips the sending window only", "Sent 2: ", "Held for a later run: today's cap 1.")
	if note := w.lastNote(t, commandLeadsSend); !leadbrief.NoteWords(note)[noteSendNow] {
		t.Errorf("run note %q does not say send now", note)
	}
	// The cap still holds under --now: the third waits.
	if n := w.count(t, `SELECT count(*) FROM outreach_message WHERE step = 0 AND state = 'approved'`); n != 1 {
		t.Errorf("%d first emails still approved, want 1", n)
	}
	// The follow-ups the two sends scheduled fall due days later: --now never makes them due.
	w.editConfig(t, `per_day: 2`, `per_day: 20`)
	w.clock.set(time.Date(2026, 10, 27, 21, 30, 0, 0, time.UTC))
	out = w.mustRun(t, "leads", "send", "--now")
	if n := w.count(t, `SELECT count(*) FROM outreach_message WHERE step > 0 AND state <> 'approved'`); n != 0 {
		t.Errorf("a follow-up not yet due was sent:\n%s", out)
	}
	if n := w.smtp.count(); n != 3 {
		t.Errorf("SMTP count %d after the cap was raised, want 3 (the third first email only):\n%s", n, out)
	}
	w.sweep(t)
}

// TestSendNowOnSaturday: a day outside the window's days is skipped too.
func TestSendNowOnSaturday(t *testing.T) {
	w := sendNowWorld(t, 20, time.Date(2026, 10, 31, 11, 0, 0, 0, time.UTC))
	w.mustRun(t, "leads", "send")
	if n := w.smtp.count(); n != 0 {
		t.Fatalf("the timer's run sent %d on a Saturday", n)
	}
	out := w.mustRun(t, "leads", "send", "--now")
	if n := w.smtp.count(); n != 3 {
		t.Errorf("send now on a Saturday sent %d, want 3:\n%s", n, out)
	}
}

// TestSendNowKeepsTheRest: under --now the sending switch, a suppression and an unassigned
// mailbox still stop a send.
func TestSendNowKeepsTheRest(t *testing.T) {
	ctx := context.Background()
	w := sendNowWorld(t, 20, time.Date(2026, 10, 27, 21, 0, 0, 0, time.UTC))

	w.mustRun(t, "leads", "send", "pause", "--reason", "testing")
	out := w.mustRun(t, "leads", "send", "--now")
	if n := w.smtp.count(); n != 0 {
		t.Fatalf("send now sent %d while sending is off:\n%s", n, out)
	}
	wantLines(t, "paused", out, "Sending is paused by the operator")
	w.mustRun(t, "leads", "send", "resume")

	if _, err := w.lq.Suppress(ctx, 0, leadstore.SuppressAddress, "info@alpha.example", leadstore.ReasonOptOut, "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := w.lq.SetCampaignMailbox(ctx, leadstore.DefaultCampaignID, w.mailbox.ID, false, leadstore.ActorGremy); err != nil {
		t.Fatal(err)
	}
	out = w.mustRun(t, "leads", "send", "--now")
	if n := w.smtp.count(); n != 0 {
		t.Fatalf("send now sent %d from an unassigned mailbox:\n%s", n, out)
	}
	wantLines(t, "unassigned", out, "mailbox not assigned to the campaign 2")

	if _, err := w.lq.SetCampaignMailbox(ctx, leadstore.DefaultCampaignID, w.mailbox.ID, true, leadstore.ActorGremy); err != nil {
		t.Fatal(err)
	}
	out = w.mustRun(t, "leads", "send", "--now")
	got := w.smtp.recipients()
	if len(got) != 2 {
		t.Errorf("send now sent %v, want bravo and charlie:\n%s", got, out)
	}
	for _, r := range got {
		if r == "info@alpha.example" {
			t.Errorf("send now sent to a suppressed address:\n%s", out)
		}
	}
	w.sweep(t, "info@alpha.example")
}

// TestSendWindowSaved (Task 4.1): with no saved window the file's applies; a window saved in
// the console (08:30-18:00) is the one the send keeps, also after leads.yaml's own window
// changes (saved wins, as for every other setting); a reset puts the file's back.
func TestSendWindowSaved(t *testing.T) {
	ctx := context.Background()
	w := sendNowWorld(t, 20, time.Date(2026, 10, 27, 17, 0, 0, 0, time.UTC))

	// The file's window ends at 16:30: at 17:00 London nothing goes.
	w.mustRun(t, "leads", "send")
	if n := w.smtp.count(); n != 0 {
		t.Fatalf("the file's window sent %d at 17:00", n)
	}
	out := w.mustRun(t, "leads", "settings", "show")
	wantLines(t, "file window", out, "schedule.business_hours (sending window, recipient's local time): 09:30 to 16:30 on Mon, Tue, Wed, Thu, Fri · file")

	if _, err := w.lq.SaveSettings(ctx, 0, []byte(`{"schedule":{"business_hours":{"start":"08:30","end":"18:00","days":["Mon","Tue","Wed","Thu","Fri"]}}}`),
		leadstore.ActorOperator); err != nil {
		t.Fatal(err)
	}
	// A deploy-data of another file window: the saved one still wins.
	w.editConfig(t, `business_hours: \{start: "09:30", end: "16:30"`, `business_hours: {start: "10:00", end: "12:00"`)
	out = w.mustRun(t, "leads", "settings", "show")
	wantLines(t, "saved window", out, "schedule.business_hours (sending window, recipient's local time): 08:30 to 18:00 on Mon, Tue, Wed, Thu, Fri · console ")

	w.editConfig(t, `spacing_minutes: \[3, 9\]`, `spacing_minutes: [70, 70]`)
	out = w.mustRun(t, "leads", "send")
	// 17:00 is inside 08:30-18:00: the first goes; after 70 minutes it is 18:10, past the end.
	if n := w.smtp.count(); n != 1 {
		t.Fatalf("the saved window sent %d at 17:00, want 1:\n%s", n, out)
	}
	wantLines(t, "saved window send", out, "outside business hours 2")

	w.clock.set(time.Date(2026, 10, 27, 18, 5, 0, 0, time.UTC))
	w.mustRun(t, "leads", "send")
	if n := w.smtp.count(); n != 1 {
		t.Errorf("sent at 18:05 with the window ending 18:00")
	}

	w.mustRun(t, "leads", "settings", "reset", "--section", "window")
	out = w.mustRun(t, "leads", "settings", "show")
	wantLines(t, "reset", out, "schedule.business_hours (sending window, recipient's local time): 10:00 to 12:00 on Mon, Tue, Wed, Thu, Fri · file")
}

// sendNowFile is the Send now request file's content, "" when none was written.
func sendNowFile(t *testing.T, dir string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, runRequestDir, sendNowAction)) // #nosec G304 -- the test's own temp dir
	if errors.Is(err, os.ErrNotExist) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestSendNowConsole (Tasks 4.2, 4.3): over the real store, outside the window the sentence
// offers Send now; a press writes the request file and nothing else, a second press while it
// waits is refused, and the run it starts (leads send --now) is shown with what it sent. Sending
// off and the warm-up limit used are refused before any file is written. A window saved on
// Controls changes the sentence and shows its source.
func TestSendNowConsole(t *testing.T) {
	ctx := context.Background()
	w, b := controlsWorld(t, 10)
	w.clock.set(time.Date(2026, 10, 27, 17, 0, 0, 0, time.UTC))
	p, err := b.Controls(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !p.Why.SendNow || p.Why.SendNowLine != "" || p.Window.Start != "09:30" || p.Window.Source.Console {
		t.Fatalf("before a press: %+v, window %+v", p.Why, p.Window)
	}
	out, err := b.SendNow(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out.Message, "Asked.") || sendNowFile(t, w.dir) == "" || w.smtp.count() != 0 {
		t.Fatalf("press: %q, file %q, sent %d", out.Message, sendNowFile(t, w.dir), w.smtp.count())
	}
	if _, err := b.SendNow(ctx); !errors.Is(err, console.ErrUserFixable) || !strings.Contains(err.Error(), "Send now was already asked at 17:00.") {
		t.Errorf("a second press: %v", err)
	}
	if v, _ := b.SendingWhy(ctx); v.SendNowLine != "Send now asked at 17:00, waiting to start." || !v.SendNowPending {
		t.Errorf("waiting: %+v", v)
	}

	w.mustRun(t, "leads", "send", "--now")
	p, err = b.Controls(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// The run row is stamped by the database clock, not the test clock: only its words count.
	if !strings.HasPrefix(p.Why.SendNowLine, "Send now at ") || !strings.HasSuffix(p.Why.SendNowLine, " sent 2.") || p.Why.SendNowPending {
		t.Errorf("after the run: %+v", p.Why)
	}
	if p.Sending.LastRun == nil || p.Sending.LastRun.Result != "Finished (Send now)" {
		t.Errorf("sending row's last run %+v", p.Sending.LastRun)
	}

	// Refused before any file: sending off, then today's warm-up limit used.
	w2, b2 := controlsWorld(t, 2)
	w2.clock.set(time.Date(2026, 10, 27, 17, 0, 0, 0, time.UTC))
	if _, err := setSendPause(ctx, w2.lq, true, "", leadstore.ActorGremy); err != nil {
		t.Fatal(err)
	}
	if _, err := b2.SendNow(ctx); !errors.Is(err, console.ErrUserFixable) || !strings.Contains(err.Error(), "Sending is switched off.") {
		t.Errorf("sending off: %v", err)
	}
	if _, err := setSendPause(ctx, w2.lq, false, "", leadstore.ActorGremy); err != nil {
		t.Fatal(err)
	}
	w2.clock.set(time.Date(2026, 10, 27, 10, 0, 0, 0, time.UTC))
	w2.mustRun(t, "leads", "send")
	w2.batch(t, false, map[int64]string{w2.lead(t, "gamma.example"): "gamma.example"})
	w2.clock.set(time.Date(2026, 10, 27, 17, 0, 0, 0, time.UTC))
	if _, err := b2.SendNow(ctx); !errors.Is(err, console.ErrUserFixable) ||
		!strings.Contains(err.Error(), "Today's warm-up limit is used (2 of 2); Send now only skips the sending window.") {
		t.Errorf("cap used: %v", err)
	}
	if f := sendNowFile(t, w2.dir); f != "" {
		t.Errorf("a refused press wrote the request file: %q", f)
	}

	// A window saved on Controls: at 17:00 the emails go at the next run, not the next window.
	w3, b3 := controlsWorld(t, 10)
	w3.clock.set(time.Date(2026, 10, 27, 17, 0, 0, 0, time.UTC))
	if err := b3.SaveLeadSettings(ctx, console.SettingsWindow, console.LeadSettingsValues{WindowStart: "08:30", WindowEnd: "18:00",
		WindowDays: []string{"Mon", "Tue", "Wed", "Thu", "Fri"}}, false); err != nil {
		t.Fatal(err)
	}
	if got := sentence(t, b3); got != "2 approved emails go out at the next run, Tue 27 Oct 11:00." {
		t.Errorf("sentence with the saved window: %q", got)
	}
	p, err = b3.Controls(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if p.Window.Start != "08:30" || p.Window.End != "18:00" || !p.Window.Source.Console {
		t.Errorf("saved window on Controls: %+v", p.Window)
	}
	var refused *console.SettingsRefused
	if err := b3.SaveLeadSettings(ctx, console.SettingsWindow, console.LeadSettingsValues{WindowStart: "18:00", WindowEnd: "08:30",
		WindowDays: []string{"Mon"}}, false); !errors.As(err, &refused) || refused.Problems[0] != "The end must come after the start." {
		t.Errorf("a broken window: %v", err)
	}
}

// TestSendLockHeld (fix 3): "a send is running" is the send lock itself, read from pg_locks: held
// while a send holds it, free the moment it is released, and a run row left open by a killed
// send blocks nothing.
func TestSendLockHeld(t *testing.T) {
	ctx := context.Background()
	w, b := controlsWorld(t, 10)
	w.clock.set(time.Date(2026, 10, 27, 17, 0, 0, 0, time.UTC))
	// A killed send: its run row never finished.
	if _, err := storetest.OwnerConn(t).Exec(ctx, `INSERT INTO run (command, source, started_at) VALUES ($1, 'outreach', now())`, commandLeadsSend); err != nil {
		t.Fatal(err)
	}
	held, err := sendLockHeld(ctx, b.s.Pool())
	if err != nil || held {
		t.Fatalf("no send running: held %t, %v", held, err)
	}
	if v, err := b.SendingWhy(ctx); err != nil || v.SendNowPending || !v.SendNow {
		t.Errorf("a killed run's row blocks Send now: %+v, %v", v, err)
	}
	lock, err := leadstore.TryLock(ctx, storetestPool(t), leadstore.SendLockKey)
	if err != nil {
		t.Fatal(err)
	}
	if held, err := sendLockHeld(ctx, b.s.Pool()); err != nil || !held {
		t.Errorf("a send holds the lock: held %t, %v", held, err)
	}
	if _, err := b.SendNow(ctx); !errors.Is(err, console.ErrUserFixable) || !strings.Contains(err.Error(), "A send is running since ") {
		t.Errorf("a press while a send runs: %v", err)
	}
	lock.Release()
	if held, err := sendLockHeld(ctx, b.s.Pool()); err != nil || held {
		t.Errorf("released: held %t, %v", held, err)
	}
}
