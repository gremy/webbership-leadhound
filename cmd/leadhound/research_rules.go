package main

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/gremy/webbership-leadhound/internal/campaign"
	"github.com/gremy/webbership-leadhound/internal/compliance"
	"github.com/gremy/webbership-leadhound/internal/console"
	"github.com/gremy/webbership-leadhound/internal/leadconfig"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
)

// What contact research may look for (D5) is one decision, read by the star's note, the
// research a star queues, "Look again" and the CLI's research request alike, so a note never
// offers what the request then refuses. The lead's campaign must list its country (when the
// campaign lists countries) and leads.yaml's country rule must not be off; then research looks
// for the route the batch will use:
//
//   - an email country (channel email) whose leads the campaign's plan moves onto the
//     call_first sequence (compliance.CallFirst and a plan with a call step,
//     batchPlan.forLead) looks for a business phone, also for a lead that already has an
//     email, since that lead is called before it is written to;
//   - any other email country looks for a role email, as before;
//   - a contact-form country (channel form) looks for a phone only when the campaign calls
//     there (a call step in its default sequence, and the country's calls not turned off in
//     its list): a batch writes a form lead its form text, never a call, so a phone found for
//     a campaign that makes no calls would never be used.
//
// A phone is looked for only where the law table lets the campaign call, and never for an org
// that already has one the batch could dial (usablePhoneOrgs: pickPhone over UsableOrgFacts).

// The star notes D5 adds beside console.StarNoteEmail and console.StarNotePhone.
const (
	starNoteNotInCampaign = "Not in this campaign's countries"
	starNoteCallsBlocked  = "Calls aren't allowed there under the current rules"
	starNoteNoCalls       = "This campaign makes no calls there"
	// starNoteFindPhone offers a phone search for a contact-form country the campaign calls.
	starNoteFindPhone = "No phone yet: starring it looks for a business phone"
)

// researchRules are the campaigns' country lists and plans and the law table, read once per
// page or request, and, once read for a page (withPhones), which orgs already have a phone.
type researchRules struct {
	cfg   leadconfig.Config
	table *compliance.Table
	// camps holds what research reads of each campaign with a registered spec; a campaign
	// absent from it (the legacy campaign) lists no countries and makes no calls.
	camps map[int64]campaignResearch
	now   time.Time
	// phones are the orgs with a phone a call could dial (usablePhoneOrgs), for the orgs
	// withPhones read; an org not in it has none.
	phones map[int64]bool
}

// campaignResearch is what the research decision reads of one campaign's registered spec.
type campaignResearch struct {
	listed bool                         // the spec lists countries
	ov     compliance.CampaignOverrides // its country rules; the zero value when it lists none
	call   bool                         // its default sequence has a call step
	// callFirst says the plan moves a call-first country's email leads onto a call_first
	// sequence (its own, or campaign.DefaultCallFirst when the default has a call step):
	// filePlan's callFirst is not nil.
	callFirst bool
}

// loadResearchRules reads the account's campaigns' registered country rules and sequences.
func loadResearchRules(ctx context.Context, lq leadstore.Queries, cfg leadconfig.Config, now time.Time) (researchRules, error) {
	r := researchRules{cfg: cfg, table: compliance.Default(), camps: map[int64]campaignResearch{}, now: now}
	cs, err := lq.Campaigns(ctx, leadstore.DefaultAccountID)
	if err != nil {
		return researchRules{}, err
	}
	for _, c := range cs {
		if len(c.Spec) == 0 {
			continue
		}
		var spec struct {
			Countries map[string]campaign.Country `json:"countries"`
			Sequence  []campaign.Step             `json:"sequence"`
			Sequences map[string][]campaign.Step  `json:"sequences"`
		}
		if err := json.Unmarshal(c.Spec, &spec); err != nil {
			return researchRules{}, fmt.Errorf("campaign %s: %w", c.Slug, err)
		}
		r.camps[c.ID] = campaignResearchOf(spec.Countries, spec.Sequence, spec.Sequences)
	}
	return r, nil
}

// campaignResearchOf is what research reads of a spec's countries and sequences, by the rule
// filePlan sets a plan's callFirst with.
func campaignResearchOf(countries map[string]campaign.Country, seq []campaign.Step, named map[string][]campaign.Step) campaignResearch {
	call := slices.ContainsFunc(seq, func(st campaign.Step) bool { return st.Type == campaign.StepCall })
	cr := campaignResearch{call: call, callFirst: len(named[campaign.SequenceCallFirst]) > 0 || call}
	if len(countries) > 0 {
		// A spec listing none has no campaign rules (nil Countries): an empty list would turn
		// every country off in the law table's verdict.
		cr.listed, cr.ov = true, specOverrides(countries)
	}
	return cr
}

// usablePhoneOrgs are the orgs among orgIDs with a phone a call could dial, by the batch's
// own pick: a phone fact not marked wrong (UsableOrgFacts) whose number no phone suppression
// of the account holds (pickPhone).
func usablePhoneOrgs(ctx context.Context, lq leadstore.Queries, orgIDs []int64) (map[int64]bool, error) {
	out := map[int64]bool{}
	if len(orgIDs) == 0 {
		return out, nil
	}
	facts, err := lq.UsableOrgFacts(ctx, orgIDs)
	if err != nil {
		return nil, err
	}
	byOrg := map[int64][]leadstore.Fact{}
	for _, f := range facts {
		if f.Kind == leadstore.FactPhone {
			byOrg[f.OrgID] = append(byOrg[f.OrgID], f)
		}
	}
	if len(byOrg) == 0 {
		return out, nil
	}
	sup, err := suppressedPhoneSet(ctx, lq)
	if err != nil {
		return nil, err
	}
	for org, fs := range byOrg {
		if _, ok, _ := pickPhone(fs, sup); ok {
			out[org] = true
		}
	}
	return out, nil
}

// withPhones is r with which of orgIDs already have a phone (usablePhoneOrgs), for a page
// that shows those orgs' leads.
func (r researchRules) withPhones(ctx context.Context, lq leadstore.Queries, orgIDs []int64) (researchRules, error) {
	phones, err := usablePhoneOrgs(ctx, lq, orgIDs)
	if err != nil {
		return researchRules{}, err
	}
	r.phones = phones
	return r, nil
}

// researchVerdict is what research may look for on one lead: Want, or none with Note (the
// star's note, "" when the star shows none) and NoRequest (why no request, in plain words).
// CallFirst says the phone is wanted because the lead is called before any email.
type researchVerdict struct {
	Want      []string
	Note      string
	NoRequest string
	CallFirst bool
}

// want decides for a lead of campaign campaignID in country, before what the lead already
// has: the route its campaign's plan and the country's rule would use.
func (r researchRules) want(campaignID int64, country string) researchVerdict {
	cr := r.camps[campaignID]
	var cc compliance.CampaignCountry
	if cr.listed {
		var ok bool
		cc, ok = cr.ov.Countries[countryCode(country)]
		if !ok || (!cc.Email && !cc.Call) {
			return researchVerdict{Note: starNoteNotInCampaign, NoRequest: "it is not in this campaign's countries"}
		}
	}
	var v researchVerdict
	switch r.cfg.Policy(country).Channel {
	case leadconfig.ChannelEmail:
		if !cr.callFirst || !r.table.CallFirst(country) {
			return researchVerdict{Want: []string{leadstore.WantEmail}}
		}
		v = researchVerdict{Want: []string{leadstore.WantPhone}, CallFirst: true}
	case leadconfig.ChannelForm:
		if !cr.call || (cr.listed && !cc.Call) {
			return researchVerdict{Note: starNoteNoCalls,
				NoRequest: countryWords(country) + " takes contact forms and this campaign makes no calls there, so there is no phone to look for"}
		}
		v = researchVerdict{Want: []string{leadstore.WantPhone}}
	default:
		return researchVerdict{NoRequest: countryWords(country) + " is off in leads.yaml, so there is nothing to look for"}
	}
	if !r.callsAllowed(cr.ov, country) {
		return researchVerdict{Note: starNoteCallsBlocked, NoRequest: "calls aren't allowed there under the current rules, so there is no phone to look for"}
	}
	return v
}

// researchLead is what the decision reads of one lead.
type researchLead struct {
	Channel    string
	CampaignID int64
	Country    string
	Phone      bool // its org has a phone a call could dial (usablePhoneOrgs)
}

// forLead decides for lead l asked by trigger (star, button or cli): want's answer, unless
// the lead already has what it would look for. An email is not looked for on a lead that has
// one, and a star looks for one only on a lead with no route at all (a form lead keeps its
// form; "Look again" may still ask). A phone is looked for on any lead with no phone a call
// could dial, an email lead in a call-first country included.
func (r researchRules) forLead(l researchLead, trigger string) researchVerdict {
	v := r.want(l.CampaignID, l.Country)
	switch {
	case v.Want == nil:
		return v
	case slices.Contains(v.Want, leadstore.WantPhone):
		if l.Phone {
			return researchVerdict{NoRequest: "it already has a phone to call"}
		}
	case l.Channel == leadstore.ChannelEmail:
		return researchVerdict{NoRequest: "it already has an email to write to"}
	case trigger == leadstore.ResearchStar && l.Channel != leadstore.ChannelNone:
		return researchVerdict{NoRequest: "it already has a way to reach it (" + l.Channel + ")"}
	}
	return v
}

// callsAllowed reports whether the law table lets the campaign call country at all: a call
// that is off, on hold or needs the campaign's written reason (and has none) is not. Rules
// that depend on the org or the hour (individuals, calling windows) are judged per call.
func (r researchRules) callsAllowed(ov compliance.CampaignOverrides, country string) bool {
	ok, hard, rule, _ := r.table.Verdict(country, compliance.ChannelCall, compliance.OrgFacts{EntityType: "company"}, ov, r.now)
	if ok || !hard {
		return true
	}
	switch rule {
	case compliance.RuleOff, compliance.RuleHold, compliance.RuleAcknowledge, compliance.RuleUnlisted:
		return false
	}
	return true
}

// starNoteFor is the star control's note for what research would look for; "" for nothing.
func starNoteFor(v researchVerdict) string {
	switch {
	case slices.Contains(v.Want, leadstore.WantEmail):
		return console.StarNoteEmail
	case slices.Contains(v.Want, leadstore.WantPhone) && v.CallFirst:
		return console.StarNotePhone
	case slices.Contains(v.Want, leadstore.WantPhone):
		return starNoteFindPhone
	}
	return ""
}

// starNote is a new lead's star note: what starring it looks for (forLead with the star's
// trigger, the rule the star's request applies), or, for a lead with no route at all, why
// its campaign or the law gives nothing to look for. A lead past new has none.
func starNote(r researchRules, state string, l researchLead) string {
	if state != leadstore.LeadNew {
		return ""
	}
	v := r.forLead(l, leadstore.ResearchStar)
	if v.Want != nil {
		return starNoteFor(v)
	}
	if l.Channel != leadstore.ChannelNone {
		return ""
	}
	return v.Note
}

// listLead is list row lr as the research decision reads it, its phone from r.phones.
func (r researchRules) listLead(lr leadstore.ListRow) researchLead {
	return researchLead{Channel: lr.Channel, CampaignID: lr.CampaignID, Country: lr.Country, Phone: r.phones[lr.OrgID]}
}

// newLeadOrgs are the orgs of rows' new leads: the only ones a star note or "Look again"
// reads a phone for.
func newLeadOrgs(rows ...leadstore.ListRow) []int64 {
	var out []int64
	for _, lr := range rows {
		if lr.State == leadstore.LeadNew && !slices.Contains(out, lr.OrgID) {
			out = append(out, lr.OrgID)
		}
	}
	return out
}
