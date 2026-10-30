package console

import (
	"strings"
	"testing"
	"time"
)

// TestMailboxWarmupStartDate: with a fixed start date still ahead, the card and the list say
// the warm-up starts on that day, never that it starts with the mailbox's first email.
func TestMailboxWarmupStartDate(t *testing.T) {
	start := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)
	b := richBackend("x")
	b.mailbox = richMailbox("x")
	b.mailbox.Row.RampDay, b.mailbox.Row.RampStart = 0, &start
	b.mbSending = MailboxSendingView{MailboxID: 1, Why: SendingWhy{Blocked: true, Sentence: "The warm-up starts on Mon 16 Nov."}}
	b.mailboxes = richMailboxes("x")
	for i := range b.mailboxes.Groups {
		for j := range b.mailboxes.Groups[i].Rows {
			b.mailboxes.Groups[i].Rows[j].RampDay, b.mailboxes.Groups[i].Rows[j].RampStart = 0, &start
		}
	}
	h := newHarness(t, b, false)
	card := h.do(get("/mailboxes/1", h.session(t))).Body.String()
	if want := "<dt>Warm-up</dt><dd>Warm-up starts on Monday 16 November. Nothing has gone out yet because: The warm-up starts on Mon 16 Nov.</dd>"; !strings.Contains(card, want) {
		t.Errorf("the card lacks %q", want)
	}
	list := h.do(get("/mailboxes", h.session(t))).Body.String()
	if !strings.Contains(list, "warm-up starts on Monday 16 November") {
		t.Error("the list lacks the start date")
	}
	for _, body := range []string{card, list} {
		if strings.Contains(body, "first email") {
			t.Error("a page says the warm-up starts with the first email")
		}
	}
}
