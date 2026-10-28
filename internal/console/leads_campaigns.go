package console

import (
	"net/http"
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

// leadsCampaigns is GET /leads/campaigns: every campaign, read-only.
func (s *server) leadsCampaigns(w http.ResponseWriter, r *http.Request) {
	cs, err := s.b.Campaigns(r.Context())
	if s.leadsRead(w, r, http.StatusOK, err) {
		return
	}
	gates, err := s.b.DraftGates(r.Context())
	if s.leadsRead(w, r, http.StatusOK, err) {
		return
	}
	v := make([]campaignRow, len(cs))
	for i, c := range cs {
		v[i] = campaignRow{CampaignView: c}
		if g, ok := gates[c.Slug]; ok {
			v[i].Gate = &g
		}
	}
	s.render(w, r, http.StatusOK, "leads_campaigns", page{Title: "Campaigns", Active: leadsPath, SignedIn: true,
		Data: v})
}

// campaignRow is one campaign as the tab renders it: the backend's view and its Claude-draft
// gate (nil when the backend has none for it).
type campaignRow struct {
	CampaignView
	Gate *DraftGate
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
