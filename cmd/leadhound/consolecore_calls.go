package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gremy/webbership-leadhound/internal/campaign"
	"github.com/gremy/webbership-leadhound/internal/console"
	"github.com/gremy/webbership-leadhound/internal/leadconfig"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
)

// The console's CallsBackend: the Today tab, the call card, its outcome and a lead's
// Sequence section, adapted from the store's rows into the console's words.

// LeadsToday is the day's work for campaign slug ("" or an unknown slug: every campaign).
func (b *backend) LeadsToday(ctx context.Context, slug string) (console.TodayView, error) {
	setup, err := b.leadsSetup(ctx, leadsForProbe)
	if err != nil {
		return console.TodayView{}, err
	}
	lq := b.lq()
	cs, err := lq.Campaigns(ctx, leadstore.DefaultAccountID)
	if err != nil {
		return console.TodayView{}, err
	}
	v := console.TodayView{CallsOff: callsOffWords}
	var campaignID int64
	for _, c := range cs {
		v.Campaigns = append(v.Campaigns, console.FilterOption{Value: c.Slug, Label: c.Name})
		if c.Slug == slug {
			campaignID, v.Campaign = c.ID, c.Slug
		}
	}
	now := b.now()
	t, err := lq.TodayView(ctx, leadstore.DefaultAccountID, campaignID, now.In(b.loc))
	if err != nil {
		return console.TodayView{}, err
	}
	pol, err := loadCallPolicies(ctx, lq)
	if err != nil {
		return console.TodayView{}, err
	}
	return todayFromStore(t, setup.leads, now, b.loc, v, pol), nil
}

// waitsOnCall words what call row c waits on: "the email before it", "the email at step 2".
func waitsOnCall(c leadstore.CallRow) string {
	if c.PrevStep < 0 {
		return "an earlier step"
	}
	typ := campaign.StepEmail
	if c.PrevChannel == leadstore.ChannelPhone {
		typ = campaign.StepCall
	}
	return console.WaitsOnWords(typ, c.PrevStep, c.Step)
}

// todayFromStore words the store's Today into the console's view v (its filter set). Each
// time shows once: a call's due time and a reply's time go in At, an email's in its line.
func todayFromStore(t leadstore.Today, cfg leadconfig.Config, now time.Time, loc *time.Location, v console.TodayView, pol callPolicies) console.TodayView {
	clock := func(at time.Time) string { return at.In(loc).Format("15:04") }
	call := func(c leadstore.CallRow, waiting bool) console.TodayItem {
		line := fmt.Sprintf("Step %d · call", c.Step+1)
		switch {
		case waiting && c.DueAt == nil:
			line += " · waits on " + waitsOnCall(c)
		case waiting:
			line += " · not due yet"
		case !inCallWindow(now, cfg.Policy(c.Country).Location()):
			line += " · outside calling hours now (09:30 to 17:00 their time, weekdays)"
		}
		if c.Outcome != "" {
			line += " · last time: " + strings.ReplaceAll(c.Outcome, "_", " ")
		}
		if pol.off(c.CampaignID, c.Country) {
			line += " · " + callsOffShort(c.Country)
		}
		if c.Shadow {
			line += " · shadow batch"
		}
		return console.TodayItem{LeadID: c.LeadID, MessageID: c.MessageID, OrgName: callOrgWords(c), Line: line, At: c.DueAt}
	}
	item := func(it leadstore.TodayItem) console.TodayItem {
		var line string
		switch it.Channel {
		case "email":
			line = fmt.Sprintf("Step %d · email · goes out by the send job · due %s", it.Step+1, clock(it.DueAt))
		case "form":
			line = "Contact form text · use it by hand, then mark it done"
		case "linkedin":
			line = "LinkedIn note · use it by hand, then mark it done"
		default:
			line = channelWords(it.Channel)
		}
		if it.Shadow {
			line += " · shadow batch"
		}
		out := console.TodayItem{LeadID: it.LeadID, MessageID: it.MessageID, OrgName: orgWords(it.OrgName, it.OrgDomain), Line: line}
		if it.Channel != "email" { // an email's time is in its line already
			at := it.DueAt
			out.At = &at
		}
		return out
	}
	for _, c := range t.CallsDueNow {
		v.CallsNow = append(v.CallsNow, call(c, false))
	}
	for _, c := range t.CallsOverdue {
		v.CallsOverdue = append(v.CallsOverdue, call(c, false))
	}
	for _, c := range t.CallsWaiting {
		v.CallsWaiting = append(v.CallsWaiting, call(c, true))
	}
	v.CallsWaitingTotal = t.CallsWaitingN
	for _, r := range t.RepliesWaiting {
		at := r.RepliedAt
		v.Replies = append(v.Replies, console.TodayItem{LeadID: r.LeadID, OrgName: orgWords(r.OrgName, r.OrgDomain),
			Line: "Replied · read it and answer from your mailbox", At: &at})
	}
	v.EmailsDue = t.EmailsDueCount
	for _, e := range t.EmailsDue {
		v.Emails = append(v.Emails, item(e))
	}
	for _, m := range t.Manual {
		v.Manual = append(v.Manual, item(m))
	}
	return v
}

// orgWords is "Org name (domain)", or the domain alone when the org has no other name.
func orgWords(name, domain string) string {
	if n := strings.TrimSpace(name); n != "" && n != domain {
		return n + " (" + domain + ")"
	}
	return domain
}

// LeadCall is call messageID's card; console.ErrNotFound when it is no call of the account.
func (b *backend) LeadCall(ctx context.Context, messageID int64) (console.CallCard, error) {
	if _, err := b.leadsSetup(ctx, leadsForProbe); err != nil {
		return console.CallCard{}, err
	}
	lq := b.lq()
	c, err := lq.CallByID(ctx, leadstore.DefaultAccountID, messageID)
	if errors.Is(err, leadstore.ErrMessageNotFound) {
		return console.CallCard{}, console.ErrNotFound
	}
	if err != nil {
		return console.CallCard{}, err
	}
	pol, err := loadCallPolicies(ctx, lq)
	if err != nil {
		return console.CallCard{}, err
	}
	card := callCard(c)
	card.NotDue = notDueWords(c, b.now(), b.loc)
	if pol.off(c.CampaignID, c.Country) {
		card.CallsOff = callsOffCard(c.Country)
	}
	return card, nil
}

// notDueWords says why call c takes no outcome yet at now: it waits on an earlier step, or
// its due time is still to come. "" when it is due, or no longer waiting at all.
func notDueWords(c leadstore.CallRow, now time.Time, loc *time.Location) string {
	if c.State != leadstore.MsgApproved {
		return ""
	}
	if c.DueAt == nil {
		return "Not due yet: this call waits on " + waitsOnCall(c) + "."
	}
	if c.DueAt.After(now) {
		return "Not due yet: due " + c.DueAt.In(loc).Format("Mon 2 Jan 15:04") + "."
	}
	return ""
}

// callCard words one call row as its card. In CP3 no call is callable.
func callCard(c leadstore.CallRow) console.CallCard {
	card := console.CallCard{
		MessageID: c.MessageID, LeadID: c.LeadID, OrgName: orgWords(c.OrgName, c.OrgDomain),
		Number: c.Phone, SourceURL: c.SourceURL, SourceReadAt: c.FetchedAt,
		FoundByResearch: strings.HasPrefix(c.DerivedBy, "research"),
		TalkingPoints:   leadconfig.CallLines(c.Card),
		Callable:        false, BlockedReason: callsOffWords, Screening: callScreeningWords,
		Shadow: c.Shadow, Due: c.DueAt,
	}
	if c.Outcome != "" && c.OutcomeAt != nil {
		card.Latest = &console.CallLog{Outcome: c.Outcome, Note: c.Note, At: *c.OutcomeAt}
	}
	return card
}

// LeadCallOutcome records what happened on call messageID (recordCall) and returns its
// "Saved. …" line; a refusal wraps console.ErrUserFixable with its words.
func (b *backend) LeadCallOutcome(ctx context.Context, messageID int64, outcome, note string, consent bool, consentAddress string) (string, error) {
	setup, err := b.leadsSetup(ctx, leadsForOutreach)
	if err != nil {
		return "", err
	}
	line, err := recordCall(ctx, b.lq(), setup.leads, b.now(), messageID, outcome, note, consent, consentAddress)
	var r *callRefusalError
	switch {
	case errors.As(err, &r):
		return "", refused("%s", r.words)
	case errors.Is(err, errCallNotFound):
		return "", console.ErrNotFound
	}
	return line, err
}

// LeadSequence is lead leadID's steps in its latest batch; console.ErrNotFound for no such lead.
func (b *backend) LeadSequence(ctx context.Context, leadID int64) ([]console.SequenceStep, error) {
	if _, err := b.leadsSetup(ctx, leadsForProbe); err != nil {
		return nil, err
	}
	lq := b.lq()
	if _, err := lq.LeadByID(ctx, leadID); errors.Is(err, leadstore.ErrLeadNotFound) {
		return nil, console.ErrNotFound
	} else if err != nil {
		return nil, err
	}
	steps, err := lq.SequenceOf(ctx, leadID)
	if err != nil {
		return nil, err
	}
	out := sequenceSteps(steps)
	pol, err := loadCallPolicies(ctx, lq)
	if err != nil {
		return nil, err
	}
	for i, st := range steps {
		if st.Type != campaign.StepCall || st.MessageID == 0 {
			continue
		}
		c, err := lq.CallByID(ctx, leadstore.DefaultAccountID, st.MessageID)
		if err != nil {
			return nil, err
		}
		if pol.off(c.CampaignID, c.Country) {
			out[i].Note = callsOffShort(c.Country)
		}
	}
	return out, nil
}

// sequenceSteps maps the store's steps onto the console's states.
func sequenceSteps(steps []leadstore.SequenceStep) []console.SequenceStep {
	out := make([]console.SequenceStep, 0, len(steps))
	for _, st := range steps {
		s := console.SequenceStep{Step: st.Step, Type: st.Type, MessageID: st.MessageID, Reason: st.CancelReason, Outcome: st.Outcome}
		switch st.State {
		case leadstore.MsgDraft:
			s.State = console.SeqDraft
		case leadstore.MsgApproved:
			s.State, s.At = console.SeqWaiting, nil
			if st.DueAt != nil {
				s.State, s.At = console.SeqDue, st.DueAt
			}
		case leadstore.MsgSent, leadstore.MsgShadowSent:
			s.State, s.At, s.Shadow = console.SeqSent, st.SentAt, st.State == leadstore.MsgShadowSent
		case leadstore.MsgDone:
			s.State, s.At = console.SeqDone, st.OutcomeAt
			if s.At == nil {
				s.At = st.SentAt
			}
		case leadstore.MsgCancelled:
			s.State = console.SeqCancelled
		case leadstore.MsgMaybeSent, leadstore.MsgSending:
			s.State = console.SeqCheck
		default:
			s.State = st.State
			if st.MessageID == 0 {
				s.State = st.Skipped
				if st.Skipped == leadstore.NoNumber {
					s.State = console.SeqNoNumber
				}
			}
		}
		out = append(out, s)
	}
	return out
}
