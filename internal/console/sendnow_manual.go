package console

import (
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
)

// The manual Send now's paths: the preview every Send now button posts to, and the confirm page of
// one manual batch (sendNowPath + id), with its confirm and check-email posts below it.
const (
	sendNowPath        = "/leads/send-now/"
	sendNowPreviewPath = sendNowPath + "preview"
)

// sendConfirmView is the confirm page: the plan, where Cancel and a sent batch go back to, and the
// line from the last action.
type sendConfirmView struct {
	Plan    SendPlan
	Back    string // a Leads page ("" when none was named: Cancel goes to the batch)
	Message string
	Error   bool
}

// Path is this page's own path, with its back kept.
func (v sendConfirmView) Path() string { return sendConfirmPath(v.Plan.BatchID, v.Back) }

// CancelTo is where Cancel goes: the page the send started from, or the batch.
func (v sendConfirmView) CancelTo() string {
	if v.Back != "" {
		return v.Back
	}
	return leadBatchPath + strconv.FormatInt(v.Plan.BatchID, 10)
}

// Counts are the tally: leads that go now, wait, and are left out by a hard stop.
func (v sendConfirmView) Counts() (sendNow, waiting, stopped int) {
	for _, l := range v.Plan.Leads {
		switch {
		case l.Stop != "":
			stopped++
		case l.Waiting != "":
			waiting++
		case l.OK:
			sendNow++
		}
	}
	return sendNow, waiting, stopped
}

// SendNow is how many leads go in this send.
func (v sendConfirmView) SendNow() int { n, _, _ := v.Counts(); return n }

// WaitingN is how many leads wait (not in this send).
func (v sendConfirmView) WaitingN() int { _, n, _ := v.Counts(); return n }

// Stopped is how many leads a hard stop leaves out.
func (v sendConfirmView) Stopped() int { _, _, n := v.Counts(); return n }

// Stops are the leads a hard stop leaves out.
func (v sendConfirmView) Stops() []SendPlanLead {
	var out []SendPlanLead
	for _, l := range v.Plan.Leads {
		if l.Stop != "" {
			out = append(out, l)
		}
	}
	return out
}

// ShowSend reports whether the page has a send button: the backend says it can send, nothing is
// waiting, and at least one lead goes.
func (v sendConfirmView) ShowSend() bool {
	return v.Plan.CanSend && v.Plan.Waiting == "" && v.SendNow() > 0
}

// sendConfirmPath is the confirm page of batch id, with back as its query when set.
func sendConfirmPath(id int64, back string) string {
	p := sendNowPath + strconv.FormatInt(id, 10)
	if back != "" {
		p += "?back=" + url.QueryEscape(back)
	}
	return p
}

// sendNowBack is a Send now form's or page's back: a Leads page (through leadsBack), or "" when
// none was named.
func sendNowBack(back string) string {
	if back == "" {
		return ""
	}
	return leadsBack(back)
}

// sendNowRefused answers a Send now refusal the operator can fix: the sentence as a flash, and back
// to next.
func (s *server) sendNowRefused(w http.ResponseWriter, r *http.Request, do, msg, next string) {
	s.leadsLog(do, "refused")
	s.setFlash(w, msg)
	http.Redirect(w, r, next, http.StatusSeeOther) //nolint:gosec // G710: the target is a Leads path rebuilt by leadsBack, or a fixed path with an id
}

// sendNowFailed answers a Send now backend error that is not a refusal: the not-set-up page or a
// 500. It reports whether err was one.
func (s *server) sendNowFailed(w http.ResponseWriter, r *http.Request, do string, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, ErrLeadsNotSetUp):
		s.leadsLog(do, "not set up")
		s.render(w, r, http.StatusUnprocessableEntity, "leads_off", page{Title: "Leads", Active: leadsPath, SignedIn: true})
	default:
		s.leadsLog(do, "failed")
		s.leadsFail(w, r, err)
	}
	return true
}

// sendNowPreview is POST /leads/send-now/preview: lead (repeatable) or batch, and back, the page the
// button sits on. It asks for the plan and goes to the confirm page; a refusal goes back to that
// page with its sentence.
func (s *server) sendNowPreview(w http.ResponseWriter, r *http.Request) {
	if !leadsForm(w, r) {
		return
	}
	const do = "send-now preview"
	f := r.PostForm
	back := sendNowBack(f.Get("back"))
	from := previewBack(back)
	var req ManualSendRequest
	for _, v := range f["lead"] {
		id, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		if err != nil || id < 1 {
			s.sendNowRefused(w, r, do, "One of the chosen leads is not a lead number; choose the leads again.", from)
			return
		}
		if !slices.Contains(req.LeadIDs, id) {
			req.LeadIDs = append(req.LeadIDs, id)
		}
	}
	if v := strings.TrimSpace(f.Get("batch")); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil || id < 1 {
			s.sendNowRefused(w, r, do, "That is not a batch number; open the batch again.", from)
			return
		}
		req.BatchID = id
	}
	switch {
	case req.BatchID > 0 && len(req.LeadIDs) > 0:
		s.sendNowRefused(w, r, do, "Send now takes chosen leads or a batch, not both; you sent a batch and "+plural(len(req.LeadIDs), "lead", "leads")+".", from)
		return
	case req.BatchID == 0 && len(req.LeadIDs) == 0:
		s.sendNowRefused(w, r, do, "Choose at least 1 lead to send now.", from)
		return
	case req.BatchID > 0 && len(f["skip"]) > 0:
		s.sendNowRefused(w, r, do, "Send now takes the whole batch; untick the "+plural(len(f["skip"]), "item", "items")+" left out, or use Approve.", from)
		return
	}
	plan, err := s.b.SendNowPreview(r.Context(), req)
	switch {
	case errors.Is(err, ErrUserFixable):
		s.sendNowRefused(w, r, do, refusalWords(err), from)
		return
	case errors.Is(err, ErrNotFound):
		s.sendNowRefused(w, r, do, "That batch or lead no longer exists; nothing was sent.", from)
		return
	case s.sendNowFailed(w, r, do, err):
		return
	}
	s.leadsLog(do, "ok")
	http.Redirect(w, r, sendConfirmPath(plan.BatchID, back), http.StatusSeeOther) //nolint:gosec // G710: the target is a Leads path rebuilt by leadsBack, or a fixed path with an id
}

// previewBack is where a refused preview goes: back, or the list when none was named.
func previewBack(back string) string {
	if back == "" {
		return leadsPath
	}
	return back
}

// plural is "1 lead" or "3 leads".
func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + many
}

// sendConfirm is GET /leads/send-now/{id}: the manual batch's plan. While drafts are still being
// written its waiting block polls this page with htmx (and shows a Refresh link without JS).
func (s *server) sendConfirm(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	back := sendNowBack(r.URL.Query().Get("back"))
	s.renderSendConfirm(w, r, http.StatusOK, id, back, s.takeFlash(w, r), false)
}

func (s *server) renderSendConfirm(w http.ResponseWriter, r *http.Request, code int, id int64, back, msg string, isErr bool) {
	plan, err := s.b.SendNowPlan(r.Context(), id)
	if s.leadsRead(w, r, code, err) {
		return
	}
	s.showSendConfirm(w, r, code, sendConfirmView{Plan: plan, Back: back, Message: msg, Error: isErr})
}

func (s *server) showSendConfirm(w http.ResponseWriter, r *http.Request, code int, v sendConfirmView) {
	s.render(w, r, code, "send_confirm", page{Title: "Send now", Active: leadsPath, SignedIn: true, Data: v})
}

// sendNowConfirm is POST /leads/send-now/{id}/confirm: hash, code (each warning the page showed),
// read (the "I have read" tick) and back. With warnings shown and no tick it refuses without asking
// the backend. A plan that changed shows the page again with the plan now.
func (s *server) sendNowConfirm(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if !leadsForm(w, r) {
		return
	}
	const do = "send-now confirm"
	f := r.PostForm
	back := sendNowBack(f.Get("back"))
	c := ManualConfirm{BatchID: id, HashPrefix: strings.TrimSpace(f.Get("hash")), Operator: s.opt.Operator}
	for _, code := range f["code"] {
		if !slices.Contains(SendWarningCodes, code) {
			s.leadsLog(do, "refused")
			s.renderSendConfirm(w, r, http.StatusUnprocessableEntity, id, back, "The send named a warning this console does not know; check the warnings again.", true)
			return
		}
		if !slices.Contains(c.Codes, code) {
			c.Codes = append(c.Codes, code)
		}
	}
	if c.HashPrefix == "" {
		s.leadsLog(do, "refused")
		s.renderSendConfirm(w, r, http.StatusUnprocessableEntity, id, back, "The send carried no batch hash; check the warnings again.", true)
		return
	}
	if len(c.Codes) > 0 && f.Get("read") == "" {
		s.leadsLog(do, "refused")
		s.renderSendConfirm(w, r, http.StatusUnprocessableEntity, id, back,
			"Tick that you have read the "+plural(len(c.Codes), "warning", "warnings")+" before you send past them; nothing was sent.", true)
		return
	}
	out, err := s.b.SendNowConfirm(r.Context(), c)
	var changed *PlanChangedError
	switch {
	case errors.As(err, &changed):
		s.leadsLog(do, "changed")
		s.showSendConfirm(w, r, http.StatusConflict, sendConfirmView{Plan: changed.Plan, Back: back, Message: changed.Error(), Error: true})
		return
	case errors.Is(err, ErrUserFixable):
		s.leadsLog(do, "refused")
		s.renderSendConfirm(w, r, http.StatusUnprocessableEntity, id, back, refusalWords(err), true)
		return
	case errors.Is(err, ErrNotFound):
		s.leadsLog(do, "refused")
		s.setFlash(w, "That batch no longer exists; nothing was sent.")
		http.Redirect(w, r, previewBack(back), http.StatusSeeOther) //nolint:gosec // G710: the target is a Leads path rebuilt by leadsBack, or a fixed path with an id
		return
	case s.sendNowFailed(w, r, do, err):
		return
	}
	s.leadsLog(do, "ok")
	s.setFlash(w, out.Message)
	next := back
	if next == "" {
		next = leadBatchPath + strconv.FormatInt(id, 10)
	}
	http.Redirect(w, r, next, http.StatusSeeOther) //nolint:gosec // G710: the target is a Leads path rebuilt by leadsBack, or a fixed path with an id
}

// sendNowCheckEmail is POST /leads/send-now/{id}/check-email: ask the email check to run now for the
// batch's unchecked addresses, then back to the confirm page with the outcome.
func (s *server) sendNowCheckEmail(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if !leadsForm(w, r) {
		return
	}
	const do = "send-now check email"
	next := sendConfirmPath(id, sendNowBack(r.PostForm.Get("back")))
	out, err := s.b.SendNowCheckEmail(r.Context(), id)
	switch {
	case errors.Is(err, ErrUserFixable):
		s.sendNowRefused(w, r, do, refusalWords(err), next)
		return
	case errors.Is(err, ErrNotFound):
		s.sendNowRefused(w, r, do, "That batch no longer exists; nothing was checked.", leadsPath)
		return
	case s.sendNowFailed(w, r, do, err):
		return
	}
	s.leadsLog(do, "ok")
	s.setFlash(w, out.Message)
	http.Redirect(w, r, next, http.StatusSeeOther) //nolint:gosec // G710: the target is a Leads path rebuilt by leadsBack, or a fixed path with an id
}
