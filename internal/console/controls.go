package console

import (
	"errors"
	"net/http"
	"slices"
	"strings"
)

const controlsPath = "/controls"

// controlsView is the Controls page with the line from the last action.
type controlsView struct {
	Page    ControlsPage
	Message string
	Error   bool
	// Form is what the sending window's form holds: the window in force, or after a refusal
	// what was submitted.
	Form SendingWindowView
}

// sendNowView is the Send now button's data: the sending sentence it sits beside, and the
// page a press returns to ("" for Controls, or the Outreach list's path).
type sendNowView struct {
	Why  SendingWhy
	Back string
}

// controlNotes are the lines under a job's row that answer what its switch leaves running.
var controlNotes = map[string]string{
	JobBatch: "The New batch button on the Leads page still works while this is off.",
	JobSync:  "Sending reads replies itself before every run, so replies and opt-outs are never missed.",
}

// controlsLog writes one line for a Controls action: the job, what was asked and the outcome,
// never the reason typed.
func (s *server) controlsLog(job, do, outcome string) {
	s.log.Info("console controls", "job", job, "action", do, "outcome", outcome)
}

// controls is GET /controls. It reads only.
func (s *server) controls(w http.ResponseWriter, r *http.Request) {
	s.renderControls(w, r, http.StatusOK, s.takeFlash(w, r), false)
}

func (s *server) renderControls(w http.ResponseWriter, r *http.Request, code int, msg string, isErr bool) {
	p, err := s.b.Controls(r.Context())
	switch {
	case errors.Is(err, ErrLeadsNotSetUp):
		s.render(w, r, code, "leads_off", page{Title: "Controls", Active: controlsPath, SignedIn: true})
		return
	case err != nil:
		s.leadsFail(w, r, err)
		return
	}
	s.render(w, r, code, "controls", page{Title: "Controls", Active: controlsPath, SignedIn: true,
		Data: controlsView{Page: p, Message: msg, Error: isErr, Form: p.Window}})
}

// controlAct is POST /controls/{job}: do=on or do=off, with an optional reason. A job not in
// ControlJobs is no page at all, so nothing set on the server can be reached through it.
func (s *server) controlAct(w http.ResponseWriter, r *http.Request) {
	job := r.PathValue("job")
	if !slices.Contains(ControlJobs, job) {
		http.NotFound(w, r)
		return
	}
	if !leadsForm(w, r) {
		return
	}
	a := ControlAction{Job: job, Reason: strings.TrimSpace(r.PostForm.Get("reason"))}
	switch r.PostForm.Get("do") {
	case "on":
		a.Do = ControlSwitchOn
	case "off":
		a.Do = ControlSwitchOff
	default:
		s.controlsLog(job, "", "refused")
		s.renderControls(w, r, http.StatusUnprocessableEntity, "Choose on or off.", true)
		return
	}
	out, err := s.b.ControlAct(r.Context(), a)
	switch {
	case errors.Is(err, ErrLeadsNotSetUp):
		s.controlsLog(job, a.Do, "not set up")
		s.render(w, r, http.StatusUnprocessableEntity, "leads_off", page{Title: "Controls", Active: controlsPath, SignedIn: true})
		return
	case errors.Is(err, ErrUserFixable):
		s.controlsLog(job, a.Do, "refused")
		s.renderControls(w, r, http.StatusUnprocessableEntity, refusalWords(err), true)
		return
	case err != nil:
		s.controlsLog(job, a.Do, "failed")
		s.leadsFail(w, r, err)
		return
	}
	s.controlsLog(job, a.Do, "ok")
	s.setFlash(w, out.Message)
	http.Redirect(w, r, controlsPath, http.StatusSeeOther)
}

// sendNow is POST /controls/send-now: one send that skips the sending window. An optional
// back of exactly the Outreach list's path returns there; any other value is ignored, so the
// form can never send the browser elsewhere.
func (s *server) sendNow(w http.ResponseWriter, r *http.Request) {
	if !leadsForm(w, r) {
		return
	}
	next, back := controlsPath, s.renderControls
	if r.PostForm.Get("back") == leadsOutPath {
		next, back = leadsOutPath, s.backToLeads
	}
	out, err := s.b.SendNow(r.Context())
	switch {
	case errors.Is(err, ErrLeadsNotSetUp):
		s.controlsLog("send-now", "send-now", "not set up")
		s.render(w, r, http.StatusUnprocessableEntity, "leads_off", page{Title: "Controls", Active: controlsPath, SignedIn: true})
		return
	case errors.Is(err, ErrUserFixable):
		s.controlsLog("send-now", "send-now", "refused")
		back(w, r, http.StatusUnprocessableEntity, refusalWords(err), true)
		return
	case err != nil:
		s.controlsLog("send-now", "send-now", "failed")
		s.leadsFail(w, r, err)
		return
	}
	s.controlsLog("send-now", "send-now", "ok")
	s.setFlash(w, out.Message)
	http.Redirect(w, r, next, http.StatusSeeOther)
}

// windowAnchor is the sending window's part of the Controls page.
const windowAnchor = "#sending-window"

// saveWindow is POST /controls/window: do=save with start, end and the days ticked, or
// do=reset to use the file's window again. A refusal shows the page with the problems and
// the form as submitted.
func (s *server) saveWindow(w http.ResponseWriter, r *http.Request) {
	if !leadsForm(w, r) {
		return
	}
	f := r.PostForm
	reset := f.Get("do") == "reset"
	var v LeadSettingsValues
	switch {
	case reset:
	case f.Get("do") == "save":
		v = LeadSettingsValues{WindowStart: strings.TrimSpace(f.Get("start")), WindowEnd: strings.TrimSpace(f.Get("end"))}
		days := f["day"]
		if len(days) > maxSettingsValues {
			days = days[:maxSettingsValues]
		}
		v.WindowDays = days
	default:
		s.controlsLog("window", "", "refused")
		s.renderControls(w, r, http.StatusUnprocessableEntity, "Choose save or reset; nothing was changed.", true)
		return
	}
	do := "save"
	if reset {
		do = "reset"
	}
	err := s.b.SaveLeadSettings(r.Context(), SettingsWindow, v, reset)
	switch {
	case errors.Is(err, ErrLeadsNotSetUp):
		s.controlsLog("window", do, "not set up")
		s.render(w, r, http.StatusUnprocessableEntity, "leads_off", page{Title: "Controls", Active: controlsPath, SignedIn: true})
		return
	case errors.Is(err, ErrUserFixable):
		s.controlsLog("window", do, "refused")
		s.renderControlsWindow(w, r, settingsProblems(err), v, reset)
		return
	case err != nil:
		s.controlsLog("window", do, "failed")
		s.leadsFail(w, r, err)
		return
	}
	s.controlsLog("window", do, "ok")
	msg := "Sending window saved: the next send run keeps it."
	if reset {
		msg = "The sending window is back to the leads settings file's."
	}
	s.setFlash(w, msg)
	http.Redirect(w, r, controlsPath+windowAnchor, http.StatusSeeOther)
}

// renderControlsWindow is the Controls page after a refused window: the problems, and the
// form holding what was submitted (not after a refused reset).
func (s *server) renderControlsWindow(w http.ResponseWriter, r *http.Request, problems []string, v LeadSettingsValues, reset bool) {
	p, err := s.b.Controls(r.Context())
	switch {
	case errors.Is(err, ErrLeadsNotSetUp):
		s.render(w, r, http.StatusUnprocessableEntity, "leads_off", page{Title: "Controls", Active: controlsPath, SignedIn: true})
		return
	case err != nil:
		s.leadsFail(w, r, err)
		return
	}
	p.WindowProblems = problems
	form := p.Window
	if !reset {
		form.Start, form.End, form.Days = v.WindowStart, v.WindowEnd, v.WindowDays
	}
	s.render(w, r, http.StatusUnprocessableEntity, "controls", page{Title: "Controls", Active: controlsPath, SignedIn: true,
		Data: controlsView{Page: p, Form: form}})
}
