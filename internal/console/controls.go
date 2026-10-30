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
		Data: controlsView{Page: p, Message: msg, Error: isErr}})
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
