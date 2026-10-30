package console

import (
	"errors"
	"strings"
	"testing"
)

// TestDashboardPauseShownOnce: with sending paused, the dashboard's leads card says so once,
// in the sending sentence; the old pause notice shows only when the sentence could not be
// read.
func TestDashboardPauseShownOnce(t *testing.T) {
	for _, whyErr := range []error{nil, errors.New("db gone")} {
		b := richBackend("x")
		paused := testSent
		b.dash.Leads = &LeadsSummary{Paused: &paused}
		b.sendingWhy = SendingWhy{Blocked: true, Sentence: "Sending is off since Sat 24 Oct 04:50: holiday."}
		b.whyErr = whyErr
		h := newHarness(t, b, false)
		body := h.do(get("/", h.session(t))).Body.String()
		sentence, notice := strings.Count(body, "Sending is off since"), strings.Count(body, "Sending is paused (since")
		if whyErr == nil && (sentence != 1 || notice != 0) {
			t.Errorf("sentence read: %d sentences, %d old notices; want 1 and 0", sentence, notice)
		}
		if whyErr != nil && (sentence != 0 || notice != 1) {
			t.Errorf("sentence not read: %d sentences, %d old notices; want 0 and 1", sentence, notice)
		}
	}
}
