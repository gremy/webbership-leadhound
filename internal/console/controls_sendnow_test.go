package console

import (
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestSendNowButtonOnlyBesideTheWindow (Task 4.3): Send now shows beside the sentence only
// when the backend offers it (the next window, or the next run), with what it skips and keeps,
// on Controls and on the Outreach list; the latest press's state shows either way.
func TestSendNowButtonOnlyBesideTheWindow(t *testing.T) {
	for _, offer := range []bool{true, false} {
		b := controlsBackend()
		why := SendingWhy{Blocked: true, Sentence: "3 approved emails go out in the next sending window, Wed 28 Oct 09:30.", SendNow: offer,
			SendNowLine: "Send now at 21:04 sent 2."}
		b.controls.Why, b.sendingWhy = why, why
		h := newHarness(t, b, false)
		for _, path := range []string{"/controls", "/leads/outreach"} {
			body := h.do(get(path, h.session(t))).Body.String()
			has := strings.Contains(body, `action="/controls/send-now"`)
			if has != offer {
				t.Errorf("%s offer %t: button shown %t", path, offer, has)
			}
			if offer && !strings.Contains(body, "Send now skips the sending window for one run only.") {
				t.Errorf("%s: the button does not say what it skips", path)
			}
			if !strings.Contains(body, "Send now at 21:04 sent 2.") {
				t.Errorf("%s: the press's state is missing", path)
			}
			back := strings.Contains(body, `name="back" value="/leads/outreach"`)
			if offer && back != (path == "/leads/outreach") {
				t.Errorf("%s: back field shown %t", path, back)
			}
		}
	}
}

func TestSendNowPosts(t *testing.T) {
	for _, c := range []struct{ back, want string }{
		{"", "/controls"}, {"/leads/outreach", "/leads/outreach"}, {"https://elsewhere.example/", "/controls"},
	} {
		b := controlsBackend()
		b.sendNowOut = ControlOutcome{Message: "Asked."}
		h := newHarness(t, b, false)
		w := h.do(post("/controls/send-now", url.Values{"back": {c.back}}, h.session(t)))
		if w.Code != http.StatusSeeOther || w.Header().Get("Location") != c.want {
			t.Errorf("back %q: %d to %q, want 303 to %s", c.back, w.Code, w.Header().Get("Location"), c.want)
		}
		if n := h.b.n("SendNow"); n != 1 {
			t.Errorf("back %q: SendNow called %d times", c.back, n)
		}
	}
}

func TestSendNowRefused(t *testing.T) {
	for _, back := range []string{"", "/leads/outreach"} {
		b := controlsBackend()
		b.sendNowErr = fmt.Errorf("%w: Today's warm-up limit is used (4 of 4); Send now only skips the sending window.", ErrUserFixable)
		h := newHarness(t, b, false)
		w := h.do(post("/controls/send-now", url.Values{"back": {back}}, h.session(t)))
		if w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("back %q: status %d, want 422", back, w.Code)
		}
		if body := w.Body.String(); !strings.Contains(body, "Today&#39;s warm-up limit is used (4 of 4)") || !strings.Contains(body, "notice-error") {
			t.Errorf("back %q: the refusal is not on the page", back)
		}
	}
}

func TestSendNowCrossOriginRefused(t *testing.T) {
	h := newHarness(t, controlsBackend(), false)
	for _, path := range []string{"/controls/send-now", "/controls/window"} {
		r := post(path, url.Values{"do": {"save"}}, h.session(t))
		r.Header.Set("Sec-Fetch-Site", "cross-site")
		if w := h.do(r); w.Code != http.StatusForbidden {
			t.Errorf("%s: status %d, want 403", path, w.Code)
		}
	}
	if n := h.b.total(); n != 0 {
		t.Errorf("backend called %d times", n)
	}
}

// TestSendingWindowRow (Task 4.3): the window in force, where it comes from, a save form and,
// once changed here, the reset.
func TestSendingWindowRow(t *testing.T) {
	b := controlsBackend()
	b.controls.Window = SendingWindowView{Start: "09:30", End: "16:30", Days: []string{"Mon", "Tue"}, AllDays: []string{"Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"}}
	h := newHarness(t, b, false)
	body := h.do(get("/controls", h.session(t))).Body.String()
	for _, want := range []string{`id="sending-window"`, "between 09:30 and 16:30 in each recipient's local time, on Mon, Tue",
		"From the leads settings file.", `action="/controls/window"`, `name="start" type="time" value="09:30"`,
		`name="day" value="Mon" checked`, `name="day" value="Wed">`} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if strings.Contains(body, "Use the file's window") {
		t.Error("a reset is offered for the file's own window")
	}
	b.controls.Window.Source = SettingSource{Console: true, At: time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)}
	body = h.do(get("/controls", h.session(t))).Body.String()
	if !strings.Contains(body, "Changed here on 30 October 2026.") || !strings.Contains(body, `name="do" value="reset" formnovalidate>Use the file's window`) {
		t.Error("a saved window does not say so or offer the reset")
	}
}

func TestSendingWindowSave(t *testing.T) {
	h := newHarness(t, controlsBackend(), false)
	w := h.do(post("/controls/window", url.Values{"do": {"save"}, "start": {" 08:30 "}, "end": {"18:00"}, "day": {"Mon", "Fri"}}, h.session(t)))
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/controls#sending-window" {
		t.Fatalf("%d to %q", w.Code, w.Header().Get("Location"))
	}
	s := h.b.lastSave()
	if s.Section != SettingsWindow || s.Reset || s.Values.WindowStart != "08:30" || s.Values.WindowEnd != "18:00" || !slices.Equal(s.Values.WindowDays, []string{"Mon", "Fri"}) {
		t.Errorf("saved %+v", s)
	}

	h = newHarness(t, controlsBackend(), false)
	h.do(post("/controls/window", url.Values{"do": {"reset"}, "start": {"01:00"}}, h.session(t)))
	if s := h.b.lastSave(); s.Section != SettingsWindow || !s.Reset || s.Values.WindowStart != "" {
		t.Errorf("reset %+v", s)
	}

	h = newHarness(t, controlsBackend(), false)
	h.b.controls.Window = SendingWindowView{Start: "09:30", End: "16:30", Days: []string{"Mon"}, AllDays: []string{"Mon", "Sat"}}
	h.b.saveErr = &SettingsRefused{Problems: []string{"The end must come after the start."}}
	w = h.do(post("/controls/window", url.Values{"do": {"save"}, "start": {"18:00"}, "end": {"08:30"}, "day": {"Sat"}}, h.session(t)))
	body := w.Body.String()
	if w.Code != http.StatusUnprocessableEntity || !strings.Contains(body, "The window was not saved:") ||
		!strings.Contains(body, "The end must come after the start.") || !strings.Contains(body, `name="start" type="time" value="18:00"`) ||
		!strings.Contains(body, `name="day" value="Sat" checked`) || !strings.Contains(body, "between 09:30 and 16:30") {
		t.Errorf("a refused window: %d\n%s", w.Code, body)
	}

	h = newHarness(t, controlsBackend(), false)
	if w := h.do(post("/controls/window", url.Values{"do": {"maybe"}}, h.session(t))); w.Code != http.StatusUnprocessableEntity || h.b.n("SaveLeadSettings") != 0 {
		t.Errorf("a bad do: %d", w.Code)
	}
}

// TestSendNowHiddenWhilePending (fix 5): while a press waits or a send runs, the button gives
// way to the pending line.
func TestSendNowHiddenWhilePending(t *testing.T) {
	b := controlsBackend()
	why := SendingWhy{Sentence: "2 approved emails go out at the next run.", SendNow: true, SendNowPending: true,
		SendNowLine: "Send now asked at 21:03, waiting to start."}
	b.controls.Why, b.sendingWhy = why, why
	h := newHarness(t, b, false)
	for _, path := range []string{"/controls", "/leads/outreach"} {
		body := h.do(get(path, h.session(t))).Body.String()
		if strings.Contains(body, `action="/controls/send-now"`) || !strings.Contains(body, "Send now asked at 21:03, waiting to start.") {
			t.Errorf("%s: the button shows while a press waits, or the line is missing", path)
		}
	}
}
