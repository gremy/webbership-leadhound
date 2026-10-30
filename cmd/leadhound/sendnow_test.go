package main

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/leadconfig"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/store"
)

// TestSendNowRefusals (Task 4.2): Send now is refused at the console, before any request file,
// in shadow mode, with sending off, with nothing due now (it never brings an email forward),
// with every due email held by its mailbox or campaign, and with today's limit used; it goes
// ahead outside the window and inside it alike.
func TestSendNowRefusals(t *testing.T) {
	evening := time.Date(2026, 10, 27, 21, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		edit func(f *sendFacts)
		want string
	}{
		{"outside the window", func(f *sendFacts) { f.now = evening }, ""},
		{"on a Saturday", func(f *sendFacts) { f.now = time.Date(2026, 10, 31, 11, 0, 0, 0, time.UTC) }, ""},
		{"inside the window", func(*sendFacts) {}, ""},
		{"no send timer: its watcher goes with it", func(f *sendFacts) { f.sendTimer = false }, "The send timer isn't enabled on the server."},
		{"shadow", func(f *sendFacts) { f.live = false }, "Shadow mode: LEADHOUND_OUTREACH is not live on the server, so Send now would send nothing for real."},
		{"sending off", func(f *sendFacts) { f.paused = true }, "Sending is switched off. Switch it on first; Send now only skips the sending window."},
		{"nothing approved", func(f *sendFacts) { f.rows = nil }, "Nothing approved is due now. Send now never brings an email forward; it only skips the sending window."},
		{"nothing due yet", func(f *sendFacts) {
			later := whyClock.Add(time.Hour)
			for i := range f.rows {
				f.rows[i].DueAt = &later
			}
		}, "Nothing approved is due now."},
		{"every due email's mailbox paused", func(f *sendFacts) {
			m := f.mailboxes[1]
			m.State = leadstore.MailboxPaused
			f.mailboxes = map[int64]leadstore.MailboxStatus{1: m}
		}, "2 approved emails wait on outreach@sender.example: it is paused."},
		{"unassigned", func(f *sendFacts) { f.assigned = map[[2]int64]bool{} }, "is not assigned to Drupal EOL."},
		{"warm-up limit used", func(f *sendFacts) {
			f.now = evening
			m := f.mailboxes[1]
			m.SentToday = 10
			f.mailboxes = map[int64]leadstore.MailboxStatus{1: m}
		}, "Today's warm-up limit is used (10 of 10); Send now only skips the sending window."},
		{"warm-up not started", func(f *sendFacts) { f.cfg.Caps.StartDate = leadconfig.ParseDate("2026-11-16") }, "The warm-up starts on Mon 16 Nov; Send now only skips the sending window."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := whyScene()
			tc.edit(&f)
			got := sendNowRefusal(f)
			if (tc.want == "") != (got == "") || !strings.HasPrefix(got, tc.want) && !strings.HasSuffix(got, tc.want) {
				t.Errorf("refusal %q, want %q", got, tc.want)
			}
		})
	}
}

// TestSendNowOffered: the button sits beside the sentence when the waiting emails go in the
// next window (8) or at the next run (9), never beside a block.
func TestSendNowOffered(t *testing.T) {
	for reason := whyShadow; reason <= whyNotDue; reason++ {
		want := reason == whyHours || reason == whyGoes
		if got := (sendWhy{Reason: reason}).view().SendNow; got != want {
			t.Errorf("reason %d: SendNow %t, want %t", reason, got, want)
		}
	}
}

// TestSendNowState: the send lock held (either kind of send: one lock) refuses a press, with
// the open run's start time; a run killed without finishing holds no lock and blocks nothing.
// A request waits for its unit, then is said not started; only a finished run after it whose
// note says "send now" answers it, with what it sent or that sending was off.
func TestSendNowState(t *testing.T) {
	now := time.Date(2026, 10, 27, 21, 10, 0, 0, time.UTC)
	at := func(m int) *time.Time { t := time.Date(2026, 10, 27, 21, m, 0, 0, time.UTC); return &t }
	run := func(start int, end *time.Time, note string) store.Run {
		return store.Run{Command: commandLeadsSend, StartedAt: *at(start), FinishedAt: end, Note: note}
	}
	const sentTwo = "live · sent 2 · refused 0 · notes send now"
	for _, tc := range []struct {
		name       string
		req        *time.Time
		locked     bool
		runs       []store.Run
		line, busy string
		pending    bool
	}{
		{"nothing", nil, false, nil, "", "", false},
		{"a send holds the lock", nil, true, []store.Run{run(5, nil, "")}, "A send is running since 21:05.", "A send is running since 21:05; press again once it has finished.", true},
		{"the lock held, no open row yet", nil, true, nil, "A send is running.", "A send is running; press again once it has finished.", true},
		{"a killed run holds no lock and blocks nothing", nil, false, []store.Run{run(5, nil, "")}, "", "", false},
		{"asked, waiting", at(9), false, nil, "Send now asked at 21:09, waiting to start.", "Send now was already asked at 21:09.", true},
		{"asked, an earlier run does not answer it", at(9), false, []store.Run{run(1, at(3), sentTwo)}, "Send now asked at 21:09, waiting to start.", "Send now was already asked at 21:09.", true},
		{"asked long ago, never started", at(-10), false, nil, "Send now asked at 20:50 and not started", "", false},
		{"an hourly run after the request does not answer it", at(-10), false, []store.Run{run(-5, at(-2), "live · sent 3")}, "Send now asked at 20:50 and not started", "", false},
		{"its run sent two", at(1), false, []store.Run{run(1, at(4), sentTwo)}, "Send now at 21:01 sent 2.", "", false},
		{"its run after an hourly one", at(-10), false, []store.Run{run(-8, at(-6), "live · sent 3"), run(-5, at(-1), sentTwo)}, "Send now at 20:55 sent 2.", "", false},
		{"sending was off", at(1), false, []store.Run{run(1, at(1), "live · sent 0 · notes send now, paused by the operator")}, "Send now at 21:01 sent nothing: sending was off.", "", false},
		{"a run after it killed", at(-10), false, []store.Run{run(-5, nil, "")}, "A send that started at 20:55, after Send now was asked, did not finish.", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := sendNowState(tc.req, tc.locked, tc.runs, now)
			if !strings.HasPrefix(got.line, tc.line) || (tc.line == "") != (got.line == "") || got.busy != tc.busy || got.pending != tc.pending {
				t.Errorf("state %+v, want line %q busy %q pending %t", got, tc.line, tc.busy, tc.pending)
			}
			if strings.Contains(got.line, "README") || strings.Contains(got.line, "deploy/") {
				t.Errorf("a page line names a doc path: %q", got.line)
			}
		})
	}
}

// TestSendTimerSchedule: the schedule nextSendRun keeps is the send timer's own OnCalendar.
func TestSendTimerSchedule(t *testing.T) {
	b, err := os.ReadFile("../../deploy/leadhound-leads-send.timer")
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("OnCalendar=Mon..Fri *-*-* %d..%d:00:00 %s", sendTimerFirst, sendTimerLast, sendTimerZone)
	if !strings.Contains(string(b), want+"\n") || len(sendTimerDays) != 5 || sendTimerDays[0] != time.Monday || sendTimerDays[4] != time.Friday {
		t.Errorf("the send timer does not read %q", want)
	}
}

// TestNextSendRun (fix 2): the sentence's next time is a time the hourly send runs (Monday to
// Friday, 10:00 to 19:00 Bucharest) inside the recipient's window, never a window opening no run
// reaches; a window no run ever falls in says only Send now reaches the emails.
func TestNextSendRun(t *testing.T) {
	london, err := time.LoadLocation("Europe/London")
	if err != nil {
		t.Fatal(err)
	}
	week := []string{"Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"}
	bh := leadconfig.BusinessHours{Start: "08:30", End: "18:00", Days: week}
	tue21 := time.Date(2026, 10, 27, 21, 0, 0, 0, london)
	// 08:30 London opens before the first run (10:00 Bucharest is 08:00 London): 09:00.
	if got, want := nextSendRun(london, bh, tue21), time.Date(2026, 10, 28, 9, 0, 0, 0, london); !got.Equal(want) {
		t.Errorf("next run %s, want %s", got, want)
	}
	// Saturday is in the window, but no run comes before Monday.
	sat := time.Date(2026, 10, 31, 9, 0, 0, 0, london)
	if got, want := nextSendRun(london, bh, sat), time.Date(2026, 11, 2, 9, 0, 0, 0, london); !got.Equal(want) {
		t.Errorf("from Saturday: %s, want %s", got, want)
	}
	early := leadconfig.BusinessHours{Start: "05:00", End: "07:30", Days: week}
	if got := nextSendRun(london, early, tue21); !got.IsZero() {
		t.Errorf("a window no run reaches: %s", got)
	}
	f := whyScene()
	f.cfg.Schedule.BusinessHours = early
	f.now = time.Date(2026, 10, 27, 18, 0, 0, 0, time.UTC)
	if w := whyNot(f); w.Reason != whyHours || w.Sentence != "2 approved emails wait: the hourly send never runs inside the sending window, so only Send now reaches them." {
		t.Errorf("whyNot %+v", w)
	}
}
