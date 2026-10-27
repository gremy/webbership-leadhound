package console

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
)

// jobView is the job page: the job and the line from the mark made on it, if one was.
type jobView struct {
	Job     JobPage
	Message string
	Error   bool
}

// job is GET /jobs/{id}: one job's whole record and its mark form.
func (s *server) job(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	s.renderJob(w, r, http.StatusOK, id, "", false)
}

func (s *server) renderJob(w http.ResponseWriter, r *http.Request, code int, id int64, msg string, isErr bool) {
	j, err := s.b.Job(r.Context(), id)
	if errors.Is(err, ErrNotFound) {
		s.render(w, r, http.StatusNotFound, "job", page{Title: "Job", Active: "/jobs", SignedIn: true,
			Message: "There is no job " + strconv.FormatInt(id, 10) + ".", Error: true, Data: jobView{}})
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.render(w, r, code, "job", page{Title: j.Title, Active: "/jobs", SignedIn: true, Data: jobView{Job: j, Message: msg, Error: isErr}})
}

// trackJob is POST /jobs/{id}/track: the same mark as the Jobs page makes, answered with
// the job page showing what it did next to the form.
func (s *server) trackJob(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
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
	f := r.PostForm
	req := TrackRequest{
		Ref:         "job:" + strconv.FormatInt(id, 10),
		Action:      f.Get("action"),
		Reason:      f.Get("reason"),
		RuleField:   f.Get("rule_field"),
		RulePattern: f.Get("rule_pattern"),
	}
	out, err := s.b.Track(r.Context(), req)
	if err != nil && !errors.Is(err, ErrUserFixable) {
		s.fail(w, r, err)
		return
	}
	if err != nil {
		s.log.Info("console track refused", "job", id, "action", req.Action)
		s.renderJob(w, r, http.StatusUnprocessableEntity, id, refusalWords(err), true)
		return
	}
	s.log.Info("console track", "job", out.Job.JobID, "from", out.From, "to", out.To)
	s.renderJob(w, r, http.StatusOK, id, out.Message, false)
}

// addRule is POST /rules: kind, field, pattern and reason. The dashboard's numbers are
// worked out again, as after a mark.
func (s *server) addRule(w http.ResponseWriter, r *http.Request) {
	if !leadsForm(w, r) {
		return
	}
	f := r.PostForm
	msg, err := s.b.AddRule(r.Context(), RuleRequest{Kind: f.Get("kind"), Field: f.Get("field"),
		Pattern: f.Get("pattern"), Reason: strings.TrimSpace(f.Get("reason"))})
	s.ruleAnswer(w, r, "add", msg, err)
}

// disableRule is POST /rules/{id}/disable with a reason.
func (s *server) disableRule(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if !leadsForm(w, r) {
		return
	}
	msg, err := s.b.DisableRule(r.Context(), id, strings.TrimSpace(r.PostForm.Get("reason")))
	s.ruleAnswer(w, r, "disable", msg, err)
}

// ruleAnswer redirects to the Rules page with msg, or shows it a refusal (422).
func (s *server) ruleAnswer(w http.ResponseWriter, r *http.Request, do, msg string, err error) {
	switch {
	case errors.Is(err, ErrUserFixable):
		s.log.Info("console rule", "action", do, "outcome", "refused")
		s.renderRules(w, r, http.StatusUnprocessableEntity, refusalWords(err), true)
	case err != nil:
		s.fail(w, r, err)
	default:
		s.log.Info("console rule", "action", do, "outcome", "ok")
		s.setFlash(w, msg)
		http.Redirect(w, r, "/rules", http.StatusSeeOther)
	}
}
