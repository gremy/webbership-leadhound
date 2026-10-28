package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gremy/webbership-leadhound/internal/campaign"
	"github.com/gremy/webbership-leadhound/internal/compliance"
	"github.com/gremy/webbership-leadhound/internal/console"
	"github.com/gremy/webbership-leadhound/internal/dial"
	"github.com/gremy/webbership-leadhound/internal/leadconfig"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/store"
)

// The console's CallsBackend: the Today tab, the call card, its outcome and a lead's
// Sequence section, adapted from the store's rows into the console's words.

// LeadsToday is the day's work for campaign slug ("" or an unknown slug: every campaign).
func (b *backend) LeadsToday(ctx context.Context, slug string) (console.TodayView, error) {
	if _, err := b.leadsSetup(ctx, leadsForProbe); err != nil {
		return console.TodayView{}, err
	}
	lq := b.lq()
	cs, err := lq.Campaigns(ctx, leadstore.DefaultAccountID)
	if err != nil {
		return console.TodayView{}, err
	}
	v := console.TodayView{}
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
	verdicts, err := callVerdicts(ctx, lq, now, t.CallsDueNow, t.CallsOverdue, t.CallsWaiting)
	if err != nil {
		return console.TodayView{}, err
	}
	v = todayFromStore(t, b.loc, v, verdicts)
	if len(v.Manual) == 0 {
		if v.ShadowManual, err = shadowManualCount(ctx, b.s.Pool(), campaignID); err != nil {
			return console.TodayView{}, err
		}
	}
	return v, nil
}

// shadowManualCount is how many contact forms and LinkedIn notes of approved shadow batches
// of campaign campaignID (0: every campaign) are approved and not yet done: shadow batches
// are practice, so Today does not list them and its empty state says they exist.
func shadowManualCount(ctx context.Context, db store.DBTX, campaignID int64) (int, error) {
	var n int
	err := db.QueryRow(ctx,
		`SELECT count(*) FROM outreach_message m JOIN outreach_batch b ON b.id = m.batch_id
		 WHERE b.shadow AND b.approved_at IS NOT NULL AND b.account_id = $1
		   AND ($2::bigint = 0 OR b.campaign_id = $2::bigint)
		   AND m.state = 'approved' AND m.channel IN ('form', 'linkedin')`,
		leadstore.DefaultAccountID, campaignID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("console: today: shadow forms and notes: %w", err)
	}
	return n, nil
}

// callAdvanced reports whether the lead's sequence has moved on after call c: a later step of
// its batch is no longer waiting unscheduled. The store refuses a second outcome then
// (leadstore callRefusal, ErrCallAdvanced); this mirrors its test so the card can say so
// before the operator tries.
func callAdvanced(ctx context.Context, db store.DBTX, c leadstore.CallRow) (bool, error) {
	var advanced bool
	err := db.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM outreach_message WHERE lead_id = $1 AND batch_id = $2 AND step > $3
		   AND (state <> 'approved' OR due_at IS NOT NULL))`, c.LeadID, c.BatchID, c.Step).Scan(&advanced)
	if err != nil {
		return false, fmt.Errorf("console: call %d: %w", c.MessageID, err)
	}
	return advanced, nil
}

// waitsOnCall words what call row c waits on: "step 2 (email)" when an earlier step is still
// unsent, else "the email before it", "the email at step 2".
func waitsOnCall(c leadstore.CallRow) string {
	if c.WaitsOnStep != nil {
		return waitsOnStepWords(*c.WaitsOnStep, c.WaitsOnChannel)
	}
	if c.PrevStep < 0 {
		return "an earlier step"
	}
	typ := campaign.StepEmail
	if c.PrevChannel == leadstore.ChannelPhone {
		typ = campaign.StepCall
	}
	return console.WaitsOnWords(typ, c.PrevStep, c.Step)
}

// waitsOnStepWords names an unsent earlier step by its 1-based number and channel:
// "step 2 (email)".
func waitsOnStepWords(step int, channel string) string {
	return fmt.Sprintf("step %d (%s)", step+1, channelWords(channel))
}

// callVerdicts is the call rule's verdict at now for every call of rows, by message id.
func callVerdicts(ctx context.Context, lq leadstore.Queries, now time.Time, rows ...[]leadstore.CallRow) (map[int64]callVerdict, error) {
	rules, err := loadCallRules(ctx, lq)
	if err != nil {
		return nil, err
	}
	out := map[int64]callVerdict{}
	for _, rs := range rows {
		for _, c := range rs {
			v, err := rules.check(ctx, c, now)
			if err != nil {
				return nil, err
			}
			out[c.MessageID] = v
		}
	}
	return out, nil
}

// todayFromStore words the store's Today into the console's view v (its filter set). Each
// time shows once: a call's due time and a reply's time go in At, an email's in its line.
// A call due today goes under CallsNow only when the call rule says it is callable now
// (verdicts, by message id; a call with no verdict is not callable); else under CallsDue
// with the reason in its line. A call whose campaign rules have its country's calls off is
// callable with a warning in its line (the operator's call). A call whose earlier step is
// still unsent is not due, whatever its due time: it goes with the waiting calls.
func todayFromStore(t leadstore.Today, loc *time.Location, v console.TodayView, verdicts map[int64]callVerdict) console.TodayView {
	clock := func(at time.Time) string { return at.In(loc).Format("15:04") }
	callable := func(c leadstore.CallRow) bool {
		return c.WaitsOnStep == nil && verdicts[c.MessageID].Callable
	}
	call := func(c leadstore.CallRow, waiting bool) console.TodayItem {
		vd := verdicts[c.MessageID]
		line := fmt.Sprintf("Step %d · call", c.Step+1)
		switch {
		case c.WaitsOnStep != nil:
			line += " · waits on " + waitsOnCall(c)
		case waiting && c.DueAt == nil:
			line += " · waits on " + waitsOnCall(c)
		case waiting:
			line += " · not due yet"
		case !vd.Callable:
			line += " · not callable: " + console.AfterColon(cmp.Or(vd.Reason, "the call rules could not be read"))
		}
		// A waiting call the rules already refuse says why, as its card does; only the calling
		// hours are left out, since they come round before it is due.
		if (waiting || c.WaitsOnStep != nil) && !vd.Callable && vd.Rule != compliance.RuleWindow && vd.Reason != "" {
			line += " · not callable: " + console.AfterColon(vd.Reason)
		}
		if c.Outcome != "" {
			line += " · last time: " + console.OutcomeWords(c.Outcome)
		}
		if vd.CampaignOff != "" {
			line += " · " + callsOffShort(c.Country) + ": your call"
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
	var early []console.TodayItem // due by the clock, but an earlier step is unsent
	for _, c := range t.CallsDueNow {
		switch {
		case c.WaitsOnStep != nil:
			early = append(early, call(c, true))
		case callable(c):
			v.CallsNow = append(v.CallsNow, call(c, false))
		default:
			v.CallsDue = append(v.CallsDue, call(c, false))
		}
	}
	for _, c := range t.CallsOverdue {
		if c.WaitsOnStep != nil {
			early = append(early, call(c, true))
			continue
		}
		v.CallsOverdue = append(v.CallsOverdue, call(c, false))
	}
	for _, c := range t.CallsWaiting {
		v.CallsWaiting = append(v.CallsWaiting, call(c, true))
	}
	v.CallsWaiting = append(early, v.CallsWaiting...)
	v.CallsWaitingTotal = t.CallsWaitingN + len(early)
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
	rules, err := loadCallRules(ctx, lq)
	if err != nil {
		return console.CallCard{}, err
	}
	now := b.now()
	vd, err := rules.check(ctx, c, now)
	if err != nil {
		return console.CallCard{}, err
	}
	card := callCard(c, vd)
	card.NotDue = notDueWords(c, now, b.loc)
	if c.State == leadstore.MsgDone && c.Outcome != "" {
		if card.Done, err = callAdvanced(ctx, b.s.Pool(), c); err != nil {
			return console.CallCard{}, err
		}
	}
	return card, nil
}

// notDueWords says why call c takes no outcome yet at now: an earlier step is still unsent
// (whatever its due time), it waits on an earlier step, or its due time is still to come. ""
// when it is due, or no longer waiting at all.
func notDueWords(c leadstore.CallRow, now time.Time, loc *time.Location) string {
	if c.State != leadstore.MsgApproved {
		return ""
	}
	if c.WaitsOnStep != nil {
		return notDueWaitsWords(*c.WaitsOnStep, c.WaitsOnChannel)
	}
	if c.DueAt == nil {
		return "Not due yet: this call waits on " + waitsOnCall(c) + "."
	}
	if c.DueAt.After(now) {
		return "Not due yet: due " + c.DueAt.In(loc).Format("Mon 2 Jan 15:04") + "."
	}
	return ""
}

// notDueWaitsWords is the card's and the refusal's line for a call whose earlier step is
// still unsent.
func notDueWaitsWords(step int, channel string) string {
	return "This call isn't due yet: it waits on " + waitsOnStepWords(step, channel) + ". Record what happened once it's due."
}

// callCard words one call row as its card under the call rule's verdict vd: callable or
// not with the reason, the screening status, and the campaign-rules warning.
func callCard(c leadstore.CallRow, vd callVerdict) console.CallCard {
	card := console.CallCard{
		MessageID: c.MessageID, LeadID: c.LeadID, OrgName: orgWords(c.OrgName, c.OrgDomain),
		Number: c.Phone, Dial: dial.Number(c.PhoneNormalized, c.Country), SourceURL: c.SourceURL, SourceReadAt: c.FetchedAt,
		FoundByResearch: strings.HasPrefix(c.DerivedBy, "research"),
		TalkingPoints:   leadconfig.CallLines(c.Card),
		Callable:        vd.Callable, BlockedReason: vd.Reason, Screening: vd.Screening, CallsOff: vd.CampaignOff,
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
	line, err := recordCall(ctx, b.lq(), setup.leads, b.now(), b.loc, messageID, outcome, note, consent, consentAddress)
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
	rules, err := loadCallRules(ctx, lq)
	if err != nil {
		return nil, err
	}
	now := b.now()
	for i, st := range steps {
		if st.Type != campaign.StepCall || st.MessageID == 0 {
			continue
		}
		c, err := lq.CallByID(ctx, leadstore.DefaultAccountID, st.MessageID)
		if err != nil {
			return nil, err
		}
		if st.State != leadstore.MsgApproved {
			if rules.off(c.CampaignID, c.Country) {
				out[i].Note = callsOffShort(c.Country)
			}
			continue
		}
		vd, err := rules.check(ctx, c, now)
		if err != nil {
			return nil, err
		}
		switch {
		case vd.lasting():
			out[i].Note = "not callable: " + console.AfterColon(vd.Reason)
		case vd.CampaignOff != "":
			out[i].Note = callsOffShort(c.Country)
		}
	}
	return out, nil
}

// sequenceSteps maps the store's steps onto the console's states.
func sequenceSteps(steps []leadstore.SequenceStep) []console.SequenceStep {
	out := make([]console.SequenceStep, 0, len(steps))
	for _, st := range steps {
		s := console.SequenceStep{Step: st.Step, Type: st.Type, MessageID: st.MessageID, Reason: st.CancelReason, Outcome: st.Outcome, CallNote: st.Note}
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
