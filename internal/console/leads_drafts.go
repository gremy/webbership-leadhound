package console

import (
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// The Drafts tab (CP4): the sampled Claude drafts of a campaign, each with the facts it rests
// on, the check's verdict and the operator's review. The review is what the trust gate
// counts; the console never calls Claude.

const (
	// leadsDraftsPath is the Drafts tab.
	leadsDraftsPath = "/leads/drafts"
	// draftsLimit is how many sampled drafts the tab shows, newest first.
	draftsLimit = 50
)

// The review form's refusals, in plain words.
const (
	badVerdict     = "Choose Approve or Reject. Nothing was saved."
	badReason      = "That reason is not one the form offers. Nothing was saved."
	needReason     = "Tick at least one reason to reject it. Nothing was saved."
	needOtherNote  = "\"Other\" needs a note saying what is wrong. Nothing was saved."
	reviewNoteLong = "The note is longer than 500 characters; shorten it. Nothing was saved."
)

// reasonWords are the reject reasons in the form's words, keyed by ReviewReasons.
var reasonWords = map[string]string{
	"wrong_tone": "Wrong tone",
	"too_long":   "Too long",
	"unclear":    "Unclear",
	"factual":    "Factual problem",
	ReasonOther:  "Other",
}

// ReasonWords is a reject reason in the form's words; one the console does not know is shown
// with its underscores as spaces.
func ReasonWords(r string) string {
	if w, ok := reasonWords[r]; ok {
		return w
	}
	return strings.ReplaceAll(r, "_", " ")
}

// reviewForm is a review form as it was posted, kept when the page shows a refusal so the
// operator's ticks and note survive.
type reviewForm struct {
	ID      int64
	Reasons []string
	Note    string
}

// draftsView is what the Drafts tab renders: the backend's page, the line from the last
// action, the reasons in the form's order, the form a refusal came back to, and whether the
// list was cut at draftsLimit.
type draftsView struct {
	P       DraftsPage
	Message string
	Error   bool
	Reasons []string
	Form    reviewForm
	Capped  bool
	Sample  string // the command that makes samples of this campaign, for the empty state
}

// leadsDrafts is GET /leads/drafts?campaign=S: one campaign's sampled drafts. It never writes.
func (s *server) leadsDrafts(w http.ResponseWriter, r *http.Request) {
	s.renderDrafts(w, r, http.StatusOK, strings.TrimSpace(r.URL.Query().Get("campaign")), s.takeFlash(w, r), false, reviewForm{})
}

func (s *server) renderDrafts(w http.ResponseWriter, r *http.Request, code int, campaign, msg string, isErr bool, form reviewForm) {
	p, err := s.b.DraftSamples(r.Context(), campaign, draftsLimit)
	if errors.Is(err, ErrUserFixable) && campaign != "" {
		// An unknown campaign: say so over the account's first campaign.
		msg, isErr = refusalWords(err), true
		p, err = s.b.DraftSamples(r.Context(), "", draftsLimit)
	}
	if s.leadsRead(w, r, code, err) {
		return
	}
	s.render(w, r, code, "leads_drafts", page{Title: "Drafts", Active: leadsPath, SignedIn: true,
		Data: draftsView{P: p, Message: msg, Error: isErr, Reasons: ReviewReasons, Form: form,
			Capped: len(p.Samples) >= draftsLimit, Sample: sampleCommand(p.Campaign)}})
}

// sampleCommand is the command that drafts samples of campaign for review.
func sampleCommand(campaign string) string {
	if campaign == "" {
		campaign = "drupal-eol"
	}
	return "leads drafts sample --campaign " + campaign + " --n 20"
}

// draftAnchor is a draft's id on the Drafts tab.
func draftAnchor(id int64) string { return "draft-" + strconv.FormatInt(id, 10) }

// draftsBack is the Drafts tab of campaign, at draft id.
func draftsBack(campaign string, id int64) string {
	p := leadsDraftsPath
	if campaign != "" {
		p += "?" + url.Values{"campaign": {campaign}}.Encode()
	}
	return p + "#" + draftAnchor(id)
}

// reviewDraft is POST /leads/drafts/{id}/review: verdict (approve | reject), reason (each of
// ReviewReasons, for a reject) and note (at most 500 characters; needed with "other"), and
// campaign, the tab to come back to. A verdict or reason the form never offers is refused
// (400) before the backend; a reject without a reason, "other" without a note or a long note
// shows the tab again with the refusal (422), keeping the ticks and the note, as does a
// refusal of the backend's (a draft the check refused). Saved, it goes back (303) to the
// draft on the tab with its line.
func (s *server) reviewDraft(w http.ResponseWriter, r *http.Request) {
	const do = "draft-review"
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if !leadsForm(w, r) {
		return
	}
	f := r.PostForm
	campaign := strings.TrimSpace(f.Get("campaign"))
	verdict := f.Get("verdict")
	if verdict != VerdictApprove && verdict != VerdictReject {
		s.leadsLog(do, "refused")
		http.Error(w, badVerdict, http.StatusBadRequest)
		return
	}
	var reasons []string
	for _, x := range f["reason"] {
		if !slices.Contains(ReviewReasons, x) {
			s.leadsLog(do, "refused")
			http.Error(w, badReason, http.StatusBadRequest)
			return
		}
		if !slices.Contains(reasons, x) {
			reasons = append(reasons, x)
		}
	}
	note := strings.TrimSpace(f.Get("note"))
	form := reviewForm{ID: id, Reasons: reasons, Note: note}
	refuse := func(msg string) {
		s.leadsLog(do, "refused")
		s.renderDrafts(w, r, http.StatusUnprocessableEntity, campaign, msg, true, form)
	}
	switch {
	case utf8.RuneCountInString(note) > maxLeadsText:
		refuse(reviewNoteLong)
		return
	case verdict == VerdictReject && len(reasons) == 0:
		refuse(needReason)
		return
	case verdict == VerdictReject && slices.Contains(reasons, ReasonOther) && note == "":
		refuse(needOtherNote)
		return
	}
	if verdict == VerdictApprove {
		reasons = nil // an approve carries no reasons; ticks left over are ignored
	}
	err := s.b.ReviewDraft(r.Context(), id, verdict, reasons, note)
	switch {
	case errors.Is(err, ErrLeadsNotSetUp):
		s.leadsLog(do, "not set up")
		s.render(w, r, http.StatusUnprocessableEntity, "leads_off", page{Title: "Leads", Active: leadsPath, SignedIn: true})
		return
	case errors.Is(err, ErrUserFixable):
		refuse(refusalWords(err))
		return
	case err != nil:
		s.leadsLog(do, "failed")
		s.leadsFail(w, r, err)
		return
	}
	s.leadsLog(do, "ok")
	msg := "Approved draft " + strconv.FormatInt(id, 10) + "."
	if verdict == VerdictReject {
		words := make([]string, len(reasons))
		for i, x := range reasons {
			words[i] = strings.ToLower(ReasonWords(x))
		}
		msg = "Rejected draft " + strconv.FormatInt(id, 10) + ": " + strings.Join(words, ", ") + "."
	}
	s.setFlash(w, msg)
	http.Redirect(w, r, draftsBack(campaign, id), http.StatusSeeOther)
}

// failureWhere is where a check failure sits, in words: step counts from 0, -1 for the
// talking points or the whole draft; para counts from 0, -1 for the subject or the whole step.
func failureWhere(step, para int) string {
	switch {
	case step >= 0 && para >= 0:
		return "Email " + strconv.Itoa(step+1) + ", paragraph " + strconv.Itoa(para+1)
	case step >= 0:
		return "Email " + strconv.Itoa(step+1)
	case para >= 0:
		return "Talking point " + strconv.Itoa(para+1)
	}
	return "The whole draft"
}

// FailureWhere is failureWhere for the backend, which words each DraftFailure.
func FailureWhere(step, para int) string { return failureWhere(step, para) }

// sourceWords names the stored row a fact came from, in words: "the site check, row 12".
func sourceWords(table string, id int64) string {
	var w string
	switch table {
	case "site_probe":
		w = "the site check"
	case "org":
		w = "the organisation's record"
	case "org_fact":
		w = "a fact read on their site"
	case "lead":
		w = "the lead's record"
	case "fetch":
		w = "a page LeadHound read"
	case "config":
		w = "the leads settings"
	case "campaign":
		w = "the campaign"
	case "operator":
		w = "your own details"
	case "":
		w = "no stored row"
	default:
		w = strings.ReplaceAll(table, "_", " ")
	}
	if id > 0 {
		w += ", row " + strconv.FormatInt(id, 10)
	}
	return w
}

// groupWords is a fact group in words.
func groupWords(g string) string {
	switch g {
	case "lead":
		return "about them"
	case "sender":
		return "about you"
	case "offer":
		return "the offer"
	}
	return g
}
