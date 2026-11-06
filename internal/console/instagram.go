package console

import (
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
)

// The Instagram review list: the cafés Instagram showed that LeadHound holds no lead for, or
// matches only weakly, with the operator's three calls on each. The calls are `leads ig match`,
// `dismiss` and `want`, run by the backend through the same code as the CLI; nothing here makes
// a lead.

// leadsIGPath is the review list's page; its calls post to leadsIGPath + "/" + the call.
const leadsIGPath = "/leads/instagram"

// igTab is one status tab of the review list.
type igTab struct {
	Label, URL string
	Count      int
	Current    bool
}

// igReviewView is what leads_instagram renders: the list at one status and its tabs.
type igReviewView struct {
	IGReviewPage
	Tabs []igTab
}

// igStatusLabels are the tabs' words.
var igStatusLabels = map[string]string{
	IGReviewNew: "New", IGReviewWanted: "Wanted", IGReviewDismissed: "Dismissed", IGReviewMatched: "Matched",
}

// igStatusURL is the review list at status.
func igStatusURL(status string) string {
	if status == IGReviewNew {
		return leadsIGPath
	}
	return leadsIGPath + "?status=" + url.QueryEscape(status)
}

// leadsInstagram is GET /leads/instagram[?status=new|wanted|dismissed|matched].
func (s *server) leadsInstagram(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	if status == "" {
		status = IGReviewNew
	}
	if !slices.Contains(IGReviewStatuses, status) {
		http.NotFound(w, r)
		return
	}
	s.renderInstagram(w, r, http.StatusOK, status, s.takeFlash(w, r), false)
}

func (s *server) renderInstagram(w http.ResponseWriter, r *http.Request, code int, status, msg string, isErr bool) {
	p, err := s.b.InstagramReview(r.Context(), status)
	if s.leadsRead(w, r, code, err) {
		return
	}
	v := igReviewView{IGReviewPage: p}
	for _, st := range IGReviewStatuses {
		v.Tabs = append(v.Tabs, igTab{Label: igStatusLabels[st], URL: igStatusURL(st), Count: p.Counts[st], Current: st == p.Status})
	}
	s.render(w, r, code, "leads_instagram", page{Title: "Instagram", Active: leadsIGPath, SignedIn: true, Message: msg, Error: isErr, Data: v})
}

// instagramAct is POST /leads/instagram/{action}: match (handle and lead), dismiss or want
// (handle), from the tab named in status. Done, it goes back to that tab with what happened; a
// refusal shows the tab again (422) with the backend's words.
func (s *server) instagramAct(w http.ResponseWriter, r *http.Request) {
	do := r.PathValue("action")
	if do != IGMatch && do != IGDismiss && do != IGWant {
		http.NotFound(w, r)
		return
	}
	if !leadsForm(w, r) {
		return
	}
	logDo := "instagram " + do
	status := r.PostForm.Get("status")
	if !slices.Contains(IGReviewStatuses, status) {
		status = IGReviewNew
	}
	refuse := func(words string) {
		s.renderInstagram(w, r, http.StatusUnprocessableEntity, status, words, true)
	}
	a := IGAction{Do: do, Handle: strings.TrimSpace(r.PostForm.Get("handle"))}
	if a.Handle == "" || len(a.Handle) > maxLeadsText {
		s.leadsLog(logDo, "refused")
		refuse("Name the Instagram account; nothing was changed.")
		return
	}
	if do == IGMatch {
		id, err := strconv.ParseInt(strings.TrimSpace(r.PostForm.Get("lead")), 10, 64)
		if err != nil || id < 1 {
			s.leadsLog(logDo, "refused")
			refuse("Pick the lead it is; nothing was changed.")
			return
		}
		a.LeadID = id
	}
	msg, err := s.b.InstagramAct(r.Context(), a)
	switch {
	case errors.Is(err, ErrLeadsNotSetUp):
		s.leadsLog(logDo, "not set up")
		s.render(w, r, http.StatusUnprocessableEntity, "leads_off", page{Title: "Leads", Active: leadsPath, SignedIn: true})
		return
	case errors.Is(err, ErrUserFixable):
		s.leadsLog(logDo, "refused")
		refuse(refusalWords(err))
		return
	case err != nil:
		s.leadsLog(logDo, "failed")
		s.leadsFail(w, r, err)
		return
	}
	s.leadsLog(logDo, "ok")
	s.setFlash(w, msg)
	http.Redirect(w, r, igStatusURL(status), http.StatusSeeOther) //nolint:gosec // G710: status is one of IGReviewStatuses (checked above), so the target is one of four fixed Leads paths
}
