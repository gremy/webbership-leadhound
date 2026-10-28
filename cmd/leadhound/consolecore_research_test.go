package main

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/campaign"
	"github.com/gremy/webbership-leadhound/internal/compliance"
	"github.com/gremy/webbership-leadhound/internal/console"
	"github.com/gremy/webbership-leadhound/internal/leadconfig"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
)

func researchTestConfig() leadconfig.Config {
	return leadconfig.Config{Countries: map[string]leadconfig.Country{
		"GB": {Channel: leadconfig.ChannelEmail},
		"BE": {Channel: leadconfig.ChannelEmail},
		"RO": {Channel: leadconfig.ChannelEmail},
		"DE": {Channel: leadconfig.ChannelForm},
	}}
}

// The research test campaigns (researchTestRules).
const (
	rcLegacy   = 1 // no registered spec: no country list, no calls
	rcCalls    = 2 // a call step; GB, RO (email and calls) and DE (calls, no written reason); IE off
	rcNoCalls  = 3 // every step an email; GB, RO and DE listed with calls on
	rcDEOff    = 4 // a call step; DE listed with its calls off
	rcDEReason = 5 // a call step; DE with calls on and the written reason the law table asks for
)

// researchTestRules are the research rules over researchTestConfig and the campaigns above.
func researchTestRules() researchRules {
	call := []campaign.Step{{Step: 0, Type: campaign.StepEmail}, {Step: 1, Type: campaign.StepCall, When: campaign.WhenHasCallablePhone}}
	mail := []campaign.Step{{Step: 0, Type: campaign.StepEmail}, {Step: 1, Type: campaign.StepEmail}}
	return researchRules{cfg: researchTestConfig(), table: compliance.Default(), now: time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC),
		camps: map[int64]campaignResearch{
			rcCalls: campaignResearchOf(map[string]campaign.Country{
				"GB": {Email: true, Call: true, Lang: "en"}, "RO": {Email: true, Call: true, Lang: "ro"},
				"DE": {Email: false, Call: true}, "IE": {},
			}, call, nil),
			rcNoCalls: campaignResearchOf(map[string]campaign.Country{
				"GB": {Email: true, Call: true, Lang: "en"}, "RO": {Email: true, Call: true, Lang: "ro"}, "DE": {Call: true},
			}, mail, nil),
			rcDEOff:    campaignResearchOf(map[string]campaign.Country{"GB": {Email: true, Call: true}, "DE": {Email: true}}, call, nil),
			rcDEReason: campaignResearchOf(map[string]campaign.Country{"DE": {Call: true, Acknowledge: "their tills run our software"}}, call, nil),
		}}
}

// TestResearchDecision is D5 per lead, one decision for the star's note, the star's request,
// "Look again" and the CLI: a call-first country (RO: email needs a yes on a call) under a
// campaign with a call step looks for a phone, also on a lead that has an email, and for
// nothing once its org has a phone to dial; under a campaign with no call step, or none
// registered, RO looks for an email as before; an email country looks for an email as
// before; a contact-form country (DE) looks for a phone only when the campaign calls there
// (a call step, DE's calls on, the law's written reason given), never under a campaign that
// makes no calls there; a country the campaign does not list, or that is off, gets nothing.
func TestResearchDecision(t *testing.T) {
	rr := researchTestRules()
	phone, email := []string{leadstore.WantPhone}, []string{leadstore.WantEmail}
	for _, c := range []struct {
		name     string
		lead     researchLead
		want     []string // forLead's want, star and button alike unless button says otherwise
		button   []string // "Look again"'s want when it differs from the star's
		note     string   // the new lead's star note
		noReqHas string   // a word of the NoRequest when want is nil
	}{
		{"call-first, email, no phone", researchLead{leadstore.ChannelEmail, rcCalls, "RO", false}, phone, nil, console.StarNotePhone, ""},
		{"call-first, no route, no phone", researchLead{leadstore.ChannelNone, rcCalls, "RO", false}, phone, nil, console.StarNotePhone, ""},
		{"call-first, email, a phone", researchLead{leadstore.ChannelEmail, rcCalls, "RO", true}, nil, nil, "", "already has a phone"},
		{"call-first, no route, a phone", researchLead{leadstore.ChannelNone, rcCalls, "RO", true}, nil, nil, "", "already has a phone"},
		{"RO, no call step", researchLead{leadstore.ChannelNone, rcNoCalls, "RO", false}, email, nil, console.StarNoteEmail, ""},
		{"RO, no call step, email", researchLead{leadstore.ChannelEmail, rcNoCalls, "RO", false}, nil, nil, "", "already has an email"},
		{"RO, legacy", researchLead{leadstore.ChannelNone, rcLegacy, "RO", false}, email, nil, console.StarNoteEmail, ""},
		{"email country", researchLead{leadstore.ChannelNone, rcCalls, "GB", false}, email, nil, console.StarNoteEmail, ""},
		{"email country, legacy", researchLead{leadstore.ChannelNone, rcLegacy, "GB", true}, email, nil, console.StarNoteEmail, ""},
		{"email country, email", researchLead{leadstore.ChannelEmail, rcCalls, "GB", false}, nil, nil, "", "already has an email"},
		{"email country, form", researchLead{leadstore.ChannelForm, rcCalls, "GB", false}, nil, email, "", "way to reach it (form)"},
		{"form country, legacy (no calls)", researchLead{leadstore.ChannelNone, rcLegacy, "DE", false}, nil, nil, starNoteNoCalls, "makes no calls"},
		{"form country, no call step", researchLead{leadstore.ChannelForm, rcNoCalls, "DE", false}, nil, nil, "", "makes no calls"},
		{"form country, its calls off", researchLead{leadstore.ChannelNone, rcDEOff, "DE", false}, nil, nil, starNoteNoCalls, "makes no calls"},
		{"form country, calls on", researchLead{leadstore.ChannelNone, rcDEReason, "DE", false}, phone, nil, starNoteFindPhone, ""},
		{"form country, calls on, form", researchLead{leadstore.ChannelForm, rcDEReason, "DE", false}, phone, nil, starNoteFindPhone, ""},
		{"form country, calls on, a phone", researchLead{leadstore.ChannelForm, rcDEReason, "DE", true}, nil, nil, "", "already has a phone"},
		{"form country, calls the law blocks", researchLead{leadstore.ChannelNone, rcCalls, "DE", false}, nil, nil, starNoteCallsBlocked, "calls aren't allowed"},
		{"not in the campaign", researchLead{leadstore.ChannelNone, rcCalls, "BE", false}, nil, nil, starNoteNotInCampaign, "not in this campaign"},
		{"off in the campaign", researchLead{leadstore.ChannelNone, rcCalls, "IE", false}, nil, nil, starNoteNotInCampaign, "not in this campaign"},
		{"off in leads.yaml", researchLead{leadstore.ChannelNone, rcLegacy, "US", false}, nil, nil, "", "off in leads.yaml"},
	} {
		star := rr.forLead(c.lead, leadstore.ResearchStar)
		if !slices.Equal(star.Want, c.want) || (c.want == nil && !strings.Contains(star.NoRequest, c.noReqHas)) {
			t.Errorf("%s: star wants %v (%q), want %v", c.name, star.Want, star.NoRequest, c.want)
		}
		button := c.want
		if c.button != nil {
			button = c.button
		}
		for _, trig := range []string{leadstore.ResearchButton, leadstore.ResearchCLI} {
			if got := rr.forLead(c.lead, trig).Want; !slices.Equal(got, button) {
				t.Errorf("%s: %s wants %v, want %v", c.name, trig, got, button)
			}
		}
		if got := starNote(rr, leadstore.LeadNew, c.lead); got != c.note {
			t.Errorf("%s: star note %q, want %q", c.name, got, c.note)
		}
		// One decision: the note offers a search exactly when the star asks for one, and
		// "Look again" is offered exactly when it asks for one.
		offers := slices.Contains([]string{console.StarNoteEmail, console.StarNotePhone, starNoteFindPhone}, c.note)
		if offers != (star.Want != nil) {
			t.Errorf("%s: note %q but the star wants %v", c.name, c.note, star.Want)
		}
		if v := consoleResearch(leadstore.LeadResearch{}, rr, leadstore.LeadNew, c.lead); v.CanAsk != (button != nil) || !slices.Equal(v.Want, button) {
			t.Errorf("%s: Look again %+v, want %v", c.name, v, button)
		}
		if starNote(rr, leadstore.LeadBatched, c.lead) != "" {
			t.Errorf("%s: a lead past new has a star note", c.name)
		}
	}
	// The star's words say why a phone is looked for.
	if w := starQueuedWords(ResearchAsk{Want: phone, CallFirst: true}); !strings.HasPrefix(w, "Its country takes calls first") {
		t.Errorf("call-first star words %q", w)
	}
	if w := starQueuedWords(ResearchAsk{Want: phone}); !strings.HasPrefix(w, "It has no phone yet") {
		t.Errorf("form-country star words %q", w)
	}
	if w := starQueuedWords(ResearchAsk{Want: email}); !strings.HasPrefix(w, "It has no email yet") {
		t.Errorf("email star words %q", w)
	}
}

// TestResearchRulesFromSpec: the plan facts research reads follow filePlan's: a call step in
// the default sequence makes calls and a call-first plan; a call_first sequence of its own
// makes a call-first plan with no call in the default; a spec listing no countries keeps no
// campaign rules (calls stay the law table's alone).
func TestResearchRulesFromSpec(t *testing.T) {
	call := []campaign.Step{{Step: 0, Type: campaign.StepEmail}, {Step: 1, Type: campaign.StepCall}}
	mail := []campaign.Step{{Step: 0, Type: campaign.StepEmail}}
	if cr := campaignResearchOf(nil, call, nil); !cr.call || !cr.callFirst || cr.listed || cr.ov.Countries != nil {
		t.Errorf("call step, no countries: %+v", cr)
	}
	if cr := campaignResearchOf(nil, mail, map[string][]campaign.Step{campaign.SequenceCallFirst: campaign.DefaultCallFirst()}); cr.call || !cr.callFirst {
		t.Errorf("own call_first: %+v", cr)
	}
	if cr := campaignResearchOf(map[string]campaign.Country{"GB": {Email: true}}, mail, nil); cr.call || cr.callFirst || !cr.listed {
		t.Errorf("all email, GB listed: %+v", cr)
	}
	// A campaign with a call step and no country list: an RO email lead looks for a phone.
	rr := researchRules{cfg: researchTestConfig(), table: compliance.Default(), now: time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC),
		camps: map[int64]campaignResearch{9: campaignResearchOf(nil, call, nil)}}
	if v := rr.forLead(researchLead{Channel: leadstore.ChannelEmail, CampaignID: 9, Country: "RO"}, leadstore.ResearchStar); !slices.Equal(v.Want, []string{leadstore.WantPhone}) {
		t.Errorf("unlisted call campaign, RO: %+v", v)
	}
}

// TestResearchConsoleView: the lead page's research takes its time from the status (asked,
// began, finished), keeps the attempt's own want, and offers Look again only to a new lead
// whose country gives something to look for.
func TestResearchConsoleView(t *testing.T) {
	rr := researchTestRules()
	asked := time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC)
	began, done := asked.Add(time.Minute), asked.Add(2*time.Minute)
	next := began.Add(leadstore.ResearchWindow)
	row := func(status string) *leadstore.Research {
		return &leadstore.Research{Status: status, Want: []string{leadstore.WantPhone}, Verified: 1,
			RequestedAt: asked, StartedAt: &began, FinishedAt: &done}
	}
	for _, c := range []struct {
		status string
		at     time.Time
	}{
		{leadstore.ResearchQueued, asked}, {leadstore.ResearchLooking, began}, {leadstore.ResearchFound, done},
	} {
		v := consoleResearch(leadstore.LeadResearch{Latest: row(c.status), NextAt: &next}, rr, leadstore.LeadNew, researchLead{leadstore.ChannelNone, rcLegacy, "GB", false})
		if v.Status != c.status || v.At == nil || !v.At.Equal(c.at) || !slices.Equal(v.Want, []string{leadstore.WantPhone}) ||
			v.Verified != 1 || v.NextAt != &next || !v.CanAsk {
			t.Errorf("%s: %+v", c.status, v)
		}
	}
	if v := consoleResearch(leadstore.LeadResearch{}, rr, leadstore.LeadNew, researchLead{leadstore.ChannelEmail, rcCalls, "RO", false}); v.Status != "" || !v.CanAsk ||
		!slices.Equal(v.Want, []string{leadstore.WantPhone}) {
		t.Errorf("never researched, RO: %+v", v)
	}
	if v := consoleResearch(leadstore.LeadResearch{}, rr, leadstore.LeadNew, researchLead{leadstore.ChannelNone, rcLegacy, "US", false}); v.CanAsk {
		t.Error("an off country offers Look again")
	}
	if v := consoleResearch(leadstore.LeadResearch{}, rr, leadstore.LeadBatched, researchLead{leadstore.ChannelNone, rcLegacy, "GB", false}); v.CanAsk {
		t.Error("a lead past new offers Look again")
	}
	if v := consoleResearch(leadstore.LeadResearch{}, rr, leadstore.LeadNew, researchLead{leadstore.ChannelEmail, rcLegacy, "GB", false}); v.CanAsk {
		t.Error("a lead that already has an email offers Look again")
	}
	if v := consoleResearch(leadstore.LeadResearch{}, rr, leadstore.LeadNew, researchLead{leadstore.ChannelNone, rcCalls, "BE", false}); v.CanAsk {
		t.Error("a lead outside its campaign's countries offers Look again")
	}
}
