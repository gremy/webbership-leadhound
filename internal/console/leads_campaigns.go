package console

import (
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

// CampaignView is one campaign as the Campaigns tab shows it. The backend decodes the
// campaign's latest spec version into these fields; the console words them.
type CampaignView struct {
	Slug, Name string
	Retired    bool // the campaign is retired: it makes no new batches
	// The latest spec version: its SHA-256 in hex (the page shows the first 12 characters),
	// when it was registered, and where it came from ("file" | "console").
	SpecSHA    string
	SpecAt     time.Time
	SpecSource string
	Kinds      []string // the lead kinds, as labels ("Drupal 10")
	Countries  []CampaignCountry
	Steps      []CampaignStep
	Writer     string // "template" | "claude"
	// ClaudeSpendToday is the account's Claude spend today in words ("$0.84 of $5.00"),
	// every campaign together: spend is recorded per account and lead, not per campaign.
	// "" shows "not in use yet".
	ClaudeSpendToday string
}

// CampaignCountry is one country a campaign may reach: email and call on or off, and the
// language it writes in ("" when the spec names none).
type CampaignCountry struct {
	Code, Label string // "GB", "United Kingdom" (Label "" shows the code)
	Email, Call bool
	Language    string
}

// CampaignStep is one step of a campaign's sequence. Step counts from 0; Type is "email"
// or "call"; AfterBusinessDays is the wait after the step before it (0 for the first);
// When is the spec's condition ("" or "always" | "has_callable_phone" |
// "call_not_connected" | "email_consent").
type CampaignStep struct {
	Step              int
	Type              string
	AfterBusinessDays int
	When              string
}

// leadsCampaigns is GET /leads/campaigns: every campaign, read-only, and the mailboxes each
// one sends from, which the page can change.
func (s *server) leadsCampaigns(w http.ResponseWriter, r *http.Request) {
	s.renderCampaigns(w, r, http.StatusOK, s.takeFlash(w, r), false)
}

func (s *server) renderCampaigns(w http.ResponseWriter, r *http.Request, code int, msg string, isErr bool) {
	cs, err := s.b.Campaigns(r.Context())
	if s.leadsRead(w, r, code, err) {
		return
	}
	gates, err := s.b.DraftGates(r.Context())
	if s.leadsRead(w, r, code, err) {
		return
	}
	mbs, err := s.b.CampaignMailboxes(r.Context())
	if s.leadsRead(w, r, code, err) {
		return
	}
	bySlug := make(map[string]CampaignMailboxes, len(mbs))
	for _, m := range mbs {
		bySlug[m.Slug] = m
	}
	v := make([]campaignRow, len(cs))
	for i, c := range cs {
		v[i] = campaignRow{CampaignView: c}
		if g, ok := gates[c.Slug]; ok {
			v[i].Gate = &g
		}
		if m, ok := bySlug[c.Slug]; ok {
			v[i].Mailboxes = &m
		}
	}
	s.render(w, r, code, "leads_campaigns", page{Title: "Campaigns", Active: leadsPath, SignedIn: true,
		Message: msg, Error: isErr, Data: v})
}

// campaignRow is one campaign as the tab renders it: the backend's view, its Claude-draft
// gate (nil when the backend has none for it) and its mailboxes (nil when the backend has
// none for it).
type campaignRow struct {
	CampaignView
	Gate      *DraftGate
	Mailboxes *CampaignMailboxes
}

// campaignMailboxes is POST /leads/campaigns/{slug}/mailboxes: the form sends every mailbox it
// showed (shown) and the ticked ones (assigned). Each shown mailbox whose tick differs from its
// assignment now is assigned or unassigned, one at a time; a mailbox the form did not show is
// never touched. Done, it goes back to the campaign with what changed; a refusal shows the page
// again with its words (422), the changes before it kept.
func (s *server) campaignMailboxes(w http.ResponseWriter, r *http.Request) {
	slug := strings.TrimSpace(r.PathValue("slug"))
	if slug == "" {
		http.NotFound(w, r)
		return
	}
	if !leadsForm(w, r) {
		return
	}
	shown, ok := formIDs(r.PostForm["shown"])
	want, ok2 := formIDs(r.PostForm["assigned"])
	if !ok || !ok2 {
		http.Error(w, "The form could not be read.", http.StatusBadRequest)
		return
	}
	all, err := s.b.CampaignMailboxes(r.Context())
	if err != nil && !errors.Is(err, ErrLeadsNotSetUp) {
		s.leadsLog(ControlAssign, "failed")
		s.leadsFail(w, r, err)
		return
	}
	if errors.Is(err, ErrLeadsNotSetUp) {
		s.leadsLog(ControlAssign, "not set up")
		s.render(w, r, http.StatusUnprocessableEntity, "leads_off", page{Title: "Leads", Active: leadsPath, SignedIn: true})
		return
	}
	i := slices.IndexFunc(all, func(c CampaignMailboxes) bool { return c.Slug == slug })
	if i < 0 {
		http.NotFound(w, r)
		return
	}
	// The stored campaign's slug, never the request's, builds the redirect.
	next := campaignsPath + "#campaign-" + url.PathEscape(all[i].Slug)
	var done []string
	for _, row := range all[i].Rows {
		if !shown[row.MailboxID] || want[row.MailboxID] == row.Assigned {
			continue
		}
		a := ControlAction{Do: ControlUnassign, Campaign: slug, MailboxID: row.MailboxID}
		if want[row.MailboxID] {
			a.Do = ControlAssign
		}
		out, err := s.b.ControlAct(r.Context(), a)
		switch {
		case errors.Is(err, ErrLeadsNotSetUp):
			s.leadsLog(a.Do, "not set up")
			s.render(w, r, http.StatusUnprocessableEntity, "leads_off", page{Title: "Leads", Active: leadsPath, SignedIn: true})
			return
		case errors.Is(err, ErrUserFixable):
			s.leadsLog(a.Do, "refused")
			s.renderCampaigns(w, r, http.StatusUnprocessableEntity, strings.Join(append(done, refusalWords(err)), " "), true)
			return
		case err != nil:
			s.leadsLog(a.Do, "failed")
			s.leadsFail(w, r, err)
			return
		}
		s.leadsLog(a.Do, "ok")
		if out.Message != "" {
			done = append(done, out.Message)
		}
	}
	msg := strings.Join(done, " ")
	if msg == "" {
		msg = "Nothing changed: the ticks match the mailboxes already assigned."
	}
	s.setFlash(w, msg)
	http.Redirect(w, r, next, http.StatusSeeOther)
}

// campaignsPath is the Campaigns tab.
const campaignsPath = "/leads/campaigns"

// formIDs reads form values as a set of positive ids; false when one is not.
func formIDs(vs []string) (map[int64]bool, bool) {
	out := make(map[int64]bool, len(vs))
	for _, v := range vs {
		id, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		if err != nil || id <= 0 {
			return nil, false
		}
		out[id] = true
	}
	return out, true
}

// LandingWords is where the mailbox's domain sends a visitor, in words: "<host> → <url>",
// or why that is not known.
func (r CampaignMailboxRow) LandingWords() string {
	switch r.Landing.State {
	case LandingFound:
		return r.Host + " → " + r.Landing.URL
	case LandingNoAnswer:
		return "the domain's website didn't answer"
	}
	return "where its domain goes is not checked yet"
}

// OffSiteWords is the warning for a mailbox whose domain lands away from the campaign's site;
// "" when it does not.
func (r CampaignMailboxRow) OffSiteWords() string {
	if !r.Landing.OffSite || r.Landing.State != LandingFound {
		return ""
	}
	return r.Host + " sends visitors to " + r.Landing.URL + ", not to this campaign's site."
}

// stepWhen is a sequence step's condition in words; "" when it always runs.
func stepWhen(when string) string {
	switch when {
	case "", "always":
		return ""
	case "has_callable_phone":
		return "only when the lead has a phone number we may call"
	case "call_not_connected":
		return "only when the call before it did not get through"
	case "email_consent":
		return "only when the lead agreed to email"
	}
	return "only when: " + when
}

// writerWords is who drafts a campaign's messages, in words.
func writerWords(w string) string {
	switch w {
	case "claude":
		return "Claude"
	case "template", "":
		return "Templates"
	}
	return w
}

// shortSHA is a spec version's SHA cut to 12 characters.
func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
