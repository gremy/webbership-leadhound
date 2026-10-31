package console

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"
)

// richControls is a Controls page with every field set: sending blocked, one job off by the
// system, one never run, the timers not read from systemd.
func richControls(text string) ControlsPage {
	at := testSent
	next := testSent.Add(2 * time.Hour)
	done := testSent.Add(time.Minute)
	row := func(job, name string, on bool) ControlRow {
		return ControlRow{Job: job, Name: name, What: "What " + name + " does", On: on, Since: &at, Reason: text,
			Schedule: "Every morning at 07:00", Timer: TimerState{Unit: "leadhound-leads-" + job + ".timer", Enabled: true, Next: &next},
			LastRun: &ControlRun{StartedAt: at, FinishedAt: &done, Result: "Finished"}}
	}
	jobs := []ControlRow{
		row(JobBatch, "Morning batch", false),
		row(JobSync, "Inbox sync", true),
		row(JobProbe, "Site probe", true),
		row(JobContacts, "Contacts", true),
		row(JobResearch, "Contact research", true),
		row(JobVerify, "Email check", true),
	}
	jobs[0].BySystem = true
	jobs[2].LastRun = &ControlRun{StartedAt: at, Result: "Stopped: it was blocked", Failed: true}
	jobs[3].Timer = TimerState{Unit: "leadhound-leads-contacts.timer"}
	jobs[4].Timer, jobs[4].LastRun, jobs[4].Since = TimerState{}, nil, nil
	return ControlsPage{
		Why:        SendingWhy{Blocked: true, Sentence: "Shadow mode: LEADHOUND_OUTREACH is not live on the server, so nothing is sent for real."},
		Sending:    row("", "Sending", true),
		Jobs:       jobs,
		TimersNote: "The timers could not be read from the server, so each shows what the settings install.",
		Guards: []ServerGuard{
			{Name: "LEADHOUND_OUTREACH", State: "Shadow: nothing is sent for real.", Line: "LEADHOUND_OUTREACH=live"},
			{Name: "LEADHOUND_LIVE", Value: "1", State: "On.", Line: "LEADHOUND_LIVE="},
			{Name: "LEADHOUND_LEADS_TIMERS", Value: "on", State: "On.", Line: "LEADHOUND_LEADS_TIMERS=off"},
		},
		GuardsHow:  "Each is set in /etc/leadhound/env on the server.",
		RatingOn:   true,
		RatingPath: RatingSettingsPath,
	}
}

func controlsBackend() *fakeBackend {
	b := richBackend("x")
	b.controls = richControls("held for the holidays")
	b.ctlOut = ControlOutcome{Message: "Morning batch is on."}
	return b
}

func TestControlsPage(t *testing.T) {
	h := newHarness(t, controlsBackend(), false)
	w := h.do(get("/controls", h.session(t)))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		`href="/controls" aria-current="page">`,
		`notice notice-warn sending-why`, "Shadow mode: LEADHOUND_OUTREACH is not live",
		"Morning batch", "What Morning batch does", "Off since", "held for the holidays", "(set by the system)",
		"Every morning at 07:00", "timer enabled, next run", "timer not enabled", "no timer of its own", "Never switched", "Never run",
		"Last run", `<span class="bad">Stopped: it was blocked</span>`,
		"The New batch button on the Leads page still works while this is off.",
		"Sending reads replies itself before every run, so replies and opt-outs are never missed.",
		"The timers could not be read from the server",
		`action="/controls/batch"`, `name="do" value="on"`, `action="/controls/sync"`, `name="do" value="off"`,
		`action="/leads/sending"`, `name="do" value="pause"`,
		"Set on the server", "Each is set in /etc/leadhound/env on the server.", "LEADHOUND_OUTREACH=live", "LEADHOUND_LEADS_TIMERS=off",
		`href="/leads/settings#relevance"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	settingsAt, navAt := strings.Index(body, `href="/leads/settings">`), strings.Index(body, `href="/controls"`)
	if settingsAt < 0 || navAt < settingsAt || strings.Index(body, `href="/mailboxes"`) < navAt {
		t.Error("Controls is not right after Settings in the gear menu")
	}
	if n := h.b.n("ControlAct"); n != 0 {
		t.Errorf("GET called ControlAct %d times", n)
	}
}

func TestControlsNotBlockedIsGreen(t *testing.T) {
	b := controlsBackend()
	b.controls.Why = SendingWhy{Sentence: "3 approved emails go out at the next run, 09:00."}
	b.controls.Sending.On = false
	b.controls.TimersNote = ""
	h := newHarness(t, b, false)
	body := h.do(get("/controls", h.session(t))).Body.String()
	if !strings.Contains(body, `notice notice-good sending-why`) || strings.Contains(body, "notice-warn sending-why") {
		t.Error("an unblocked sentence is not green")
	}
	if !strings.Contains(body, `name="do" value="resume"`) {
		t.Error("sending off shows no resume")
	}
	if strings.Contains(body, `class="note"`) {
		t.Error("a timers note shows though the timers were read")
	}
}

func TestControlsNotSetUp(t *testing.T) {
	b := controlsBackend()
	b.leadsErr = ErrLeadsNotSetUp
	h := newHarness(t, b, false)
	w := h.do(get("/controls", h.session(t)))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), leadsNotSetUp) {
		t.Errorf("status %d, want the not-set-up page", w.Code)
	}
}

func TestControlSwitchPosts(t *testing.T) {
	for _, job := range ControlJobs {
		for _, do := range []string{"on", "off"} {
			t.Run(job+" "+do, func(t *testing.T) {
				h := newHarness(t, controlsBackend(), false)
				w := h.do(post("/controls/"+job, url.Values{"do": {do}, "reason": {"  quiet week  "}}, h.session(t)))
				if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/controls" {
					t.Fatalf("got %d to %q, want 303 to /controls", w.Code, w.Header().Get("Location"))
				}
				want := ControlAction{Do: ControlSwitchOn, Job: job, Reason: "quiet week"}
				if do == "off" {
					want.Do = ControlSwitchOff
				}
				if h.b.lastControl != want {
					t.Errorf("ControlAct got %+v, want %+v", h.b.lastControl, want)
				}
				if c := cookieNamed(w, "__Host-lh_flash"); c == nil || c.Value == "" {
					t.Error("no flash")
				}
			})
		}
	}
}

func TestControlRefused(t *testing.T) {
	h := newHarness(t, controlsBackend(), false)
	h.b.ctlErr = fmt.Errorf("%w: Sending is on, so the inbox sync stays on", ErrUserFixable)
	w := h.do(post("/controls/sync", url.Values{"do": {"off"}}, h.session(t)))
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status %d, want 422", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "Sending is on, so the inbox sync stays on") || !strings.Contains(body, "notice-error") {
		t.Error("the refusal is not on the page")
	}
	if !strings.Contains(body, "Set on the server") {
		t.Error("the page was not re-rendered")
	}

	h = newHarness(t, controlsBackend(), false)
	w = h.do(post("/controls/batch", url.Values{"do": {"maybe"}}, h.session(t)))
	if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), "Choose on or off.") {
		t.Errorf("a bad do: %d", w.Code)
	}
	if n := h.b.n("ControlAct"); n != 0 {
		t.Errorf("a bad do called ControlAct %d times", n)
	}

	h = newHarness(t, controlsBackend(), false)
	h.b.ctlErr = errors.New("database gone")
	if w := h.do(post("/controls/batch", url.Values{"do": {"on"}}, h.session(t))); w.Code != http.StatusInternalServerError {
		t.Errorf("a backend failure: %d, want 500", w.Code)
	}
}

// TestControlsNoServerGuardSwitch: no job name reaches anything set on the server.
func TestControlsUnknownJob(t *testing.T) {
	for _, job := range []string{"outreach", "live", "LEADHOUND_OUTREACH", "LEADHOUND_LIVE", "send", "timers"} {
		h := newHarness(t, controlsBackend(), false)
		w := h.do(post("/controls/"+job, url.Values{"do": {"on"}}, h.session(t)))
		if w.Code != http.StatusNotFound {
			t.Errorf("%s: status %d, want 404", job, w.Code)
		}
		if n := h.b.n("ControlAct"); n != 0 {
			t.Errorf("%s: ControlAct called %d times", job, n)
		}
	}
}

func TestControlsCrossOriginRefused(t *testing.T) {
	h := newHarness(t, controlsBackend(), false)
	r := post("/controls/batch", url.Values{"do": {"off"}}, h.session(t))
	r.Header.Set("Sec-Fetch-Site", "cross-site")
	if w := h.do(r); w.Code != http.StatusForbidden {
		t.Errorf("status %d, want 403", w.Code)
	}
	if n := h.b.total(); n != 0 {
		t.Errorf("backend called %d times", n)
	}
}

var formBlock = regexp.MustCompile(`(?s)<form\b.*?</form>`)

// TestNoGuardSwitchOnAnyPage sweeps every page the console renders: no form names
// LEADHOUND_OUTREACH or LEADHOUND_LIVE, so none can change them.
func TestNoGuardSwitchOnAnyPage(t *testing.T) {
	for _, rt := range routes {
		if !strings.HasPrefix(rt.pattern, "GET ") || rt.pattern == "GET /static/" {
			continue
		}
		h := newHarness(t, controlsBackend(), false)
		w := h.do(get(rt.sample, h.session(t)))
		for _, f := range formBlock.FindAllString(w.Body.String(), -1) {
			for _, guard := range []string{"LEADHOUND_OUTREACH", "LEADHOUND_LIVE", "OUTREACH=", "LIVE="} {
				if strings.Contains(f, guard) {
					t.Errorf("%s: a form names %s:\n%s", rt.pattern, guard, f)
				}
			}
		}
	}
	for _, rt := range routes {
		p := strings.ToLower(rt.pattern)
		if strings.HasPrefix(p, "post ") && (strings.Contains(p, "outreach") || strings.Contains(p, "/live")) {
			t.Errorf("a POST route %s looks like it sets a server guard", rt.pattern)
		}
	}
}
