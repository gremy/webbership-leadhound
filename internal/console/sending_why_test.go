package console

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// TestSendingWhyOnLeadsAndDashboard: the Outreach page and the dashboard's leads card lead
// with the sending sentence, amber when blocked and green when not, each linking to Controls.
func TestSendingWhyOnLeadsAndDashboard(t *testing.T) {
	for _, path := range []string{"/leads/outreach", "/"} {
		for _, blocked := range []bool{true, false} {
			b := richBackend("x")
			b.dash.Leads = &LeadsSummary{}
			b.sendingWhy = SendingWhy{Blocked: blocked, Sentence: "Sending is paused, so nothing goes out."}
			h := newHarness(t, b, false)
			w := h.do(get(path, h.session(t)))
			body := w.Body.String()
			want, not := "notice notice-good sending-why", "notice notice-warn sending-why"
			if blocked {
				want, not = not, want
			}
			line := `sending-why" role="status">Sending is paused, so nothing goes out. <a href="/controls">Controls</a></p>`
			if w.Code != http.StatusOK || !strings.Contains(body, want) || strings.Contains(body, not) || !strings.Contains(body, line) {
				t.Errorf("%s blocked=%v: status %d, want the sentence %q with the Controls link", path, blocked, w.Code, want)
			}
		}
	}
}

// TestSendingWhyErrorKeepsPage: a failed read of the sentence leaves both pages up without
// it, and is logged; leads not set up shows no sentence and no log.
func TestSendingWhyErrorKeepsPage(t *testing.T) {
	for _, path := range []string{"/leads/outreach", "/"} {
		for _, err := range []error{errors.New("db gone"), ErrLeadsNotSetUp} {
			b := richBackend("x")
			b.dash.Leads = &LeadsSummary{}
			b.sendingWhy = SendingWhy{Blocked: true, Sentence: "never shown"}
			b.whyErr = err
			h := newHarness(t, b, false)
			w := h.do(get(path, h.session(t)))
			body := w.Body.String()
			if w.Code != http.StatusOK || strings.Contains(body, "sending-why") || strings.Contains(body, "never shown") {
				t.Errorf("%s %v: status %d, want the page without the sentence", path, err, w.Code)
			}
			if logged := strings.Contains(h.logs.String(), "sending why"); logged != !errors.Is(err, ErrLeadsNotSetUp) {
				t.Errorf("%s %v: logged=%v", path, err, logged)
			}
		}
	}
}

// TestDashboardNoLeadsNoSentence: with leads not set up the dashboard reads no sentence.
func TestDashboardNoLeadsNoSentence(t *testing.T) {
	b := richBackend("x")
	b.dash.Leads = nil
	b.sendingWhy = SendingWhy{Sentence: "never shown"}
	h := newHarness(t, b, false)
	w := h.do(get("/", h.session(t)))
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), "never shown") || b.n("SendingWhy") != 0 {
		t.Errorf("status %d, SendingWhy read %d times", w.Code, b.n("SendingWhy"))
	}
}

// TestSendingBackToControls: back=/controls returns to Controls, on success and on a
// refusal; any other back is ignored and the default Outreach redirect stands.
func TestSendingBackToControls(t *testing.T) {
	for _, c := range []struct{ back, want string }{
		{"/controls", "/controls"},
		{"", "/leads/outreach"},
		{"https://evil.example", "/leads/outreach"},
		{"//evil.example/controls", "/leads/outreach"},
		{"/leads/x", "/leads/outreach"},
		{"/controls/", "/leads/outreach"},
	} {
		for _, do := range []string{"pause", "resume"} {
			h := newHarness(t, controlsBackend(), false)
			w := h.do(post("/leads/sending", url.Values{"do": {do}, "back": {c.back}}, h.session(t)))
			if w.Code != http.StatusSeeOther || w.Header().Get("Location") != c.want {
				t.Errorf("back %q do %s: %d to %q, want 303 to %q", c.back, do, w.Code, w.Header().Get("Location"), c.want)
			}
		}
	}
	// A refused action re-renders the page it came from at 422.
	for _, c := range []struct{ back, title string }{{"/controls", "<h1>Controls</h1>"}, {"https://evil.example", "<h1>Leads</h1>"}} {
		h := newHarness(t, controlsBackend(), false)
		w := h.do(post("/leads/sending", url.Values{"do": {"stop"}, "back": {c.back}}, h.session(t)))
		if body := w.Body.String(); w.Code != http.StatusUnprocessableEntity || !strings.Contains(body, c.title) || !strings.Contains(body, "Choose pause or resume.") {
			t.Errorf("back %q: %d, want 422 on the page it came from", c.back, w.Code)
		}
	}
}

// TestControlsSendingFormsGoBack: both Sending forms on Controls carry back=/controls, and
// the old hint is gone.
func TestControlsSendingFormsGoBack(t *testing.T) {
	for _, on := range []bool{true, false} {
		b := controlsBackend()
		b.controls.Sending.On = on
		h := newHarness(t, b, false)
		body := h.do(get("/controls", h.session(t))).Body.String()
		if !strings.Contains(body, `<input type="hidden" name="back" value="/controls">`) {
			t.Errorf("on=%v: the Sending form has no back field", on)
		}
		if strings.Contains(body, "takes you to the Leads page") {
			t.Errorf("on=%v: the old hint still shows", on)
		}
	}
}
