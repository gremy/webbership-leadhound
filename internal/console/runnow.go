package console

import (
	"errors"
	"net/http"
	"slices"
)

// The Run now card asks for one of four fixed actions; the backend writes a
// request file and systemd runs the matching unit. The console itself never
// runs a job.

// runNowView is the Run now card as the template draws it: the card and the line
// from the last press, if one was.
type runNowView struct {
	Card    RunNowCard
	Message string
	Error   bool
}

// Polling reports whether any action waits or runs, so the card asks for itself
// again every few seconds; with nothing pending it stays still.
func (v runNowView) Polling() bool {
	for _, a := range v.Card.Actions {
		if a.Pending {
			return true
		}
	}
	return false
}

// resultsView is the results page; Found is false when there is no preview yet.
type resultsView struct {
	Found   bool
	Results Results
}

// runNow is POST /run/{action}. An htmx press gets the card back in place, with the
// outcome or the refusal on it; a plain form gets a redirect to the dashboard with the
// outcome in the one-time cookie, or the dashboard at 422 with the refusal.
func (s *server) runNow(w http.ResponseWriter, r *http.Request) {
	action := r.PathValue("action")
	if !slices.Contains(RunActions, action) {
		http.NotFound(w, r)
		return
	}
	ok, tooBig := parseForm(w, r)
	if tooBig {
		return
	}
	if !ok {
		http.Error(w, "The form could not be read.", http.StatusBadRequest)
		return
	}
	req := RunRequest{Action: action, Confirmed: r.PostForm.Get("confirm") == "yes"}
	out, err := s.b.RunNow(r.Context(), req)
	switch {
	case errors.Is(err, ErrUserFixable):
		msg := refusalWords(err)
		if msg == "" {
			msg = out.Message
		}
		s.runLog(action, "refused")
		if isHX(r) {
			s.renderRunNow(w, r, msg, true)
			return
		}
		s.renderDashboard(w, r, http.StatusUnprocessableEntity, msg, true)
		return
	case err != nil:
		s.runLog(action, "failed")
		s.fail(w, r, err)
		return
	}
	s.runLog(action, "ok")
	if isHX(r) {
		s.renderRunNow(w, r, out.Message, false)
		return
	}
	s.setFlash(w, out.Message)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *server) runLog(action, outcome string) {
	s.log.Info("console run now", "action", action, "outcome", outcome)
}

// runStatus is GET /run/status: the Run now card alone, which the card polls while
// something is pending. A plain visit (a sign-in returning here) goes to the dashboard.
func (s *server) runStatus(w http.ResponseWriter, r *http.Request) {
	if !isHX(r) {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	s.renderRunNow(w, r, "", false)
}

// renderRunNow writes the card fragment, read fresh, with msg on it.
func (s *server) renderRunNow(w http.ResponseWriter, r *http.Request, msg string, isErr bool) {
	c, err := s.b.RunNowCard(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.write(w, r, http.StatusOK, s.pages["run_now"], "run_now", runNowView{Card: c, Message: msg, Error: isErr})
}

// results is GET /results: the latest preview, or a plain empty state before the first.
func (s *server) results(w http.ResponseWriter, r *http.Request) {
	s.renderResults(w, r, http.StatusOK, s.takeFlash(w, r), false)
}

func (s *server) renderResults(w http.ResponseWriter, r *http.Request, code int, msg string, isErr bool) {
	res, err := s.b.Results(r.Context())
	v := resultsView{Found: err == nil, Results: res}
	if err != nil && !errors.Is(err, ErrNotFound) {
		s.fail(w, r, err)
		return
	}
	s.render(w, r, code, "results", page{Title: "Results", Active: "/results", SignedIn: true,
		Message: msg, Error: isErr, Data: v})
}
