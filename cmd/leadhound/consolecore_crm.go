package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/gremy/webbership-leadhound/internal/console"
	"github.com/gremy/webbership-leadhound/internal/dial"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
)

// The console's CRMBackend: a lead's stage, note, deal and snooze, its timeline, the replies
// inbox, the pipeline, the call queue and the campaign report, adapted from the store's rows
// into the console's words. LeadCallOutcomeOn sits with LeadCallOutcome (consolecore_calls.go).

var _ console.CRMBackend = (*backend)(nil)

// stageWeights is how much of a deal's value each stage expects, in percent: the pipeline's
// expected revenue is value × weight. A stage not listed weighs nothing.
var stageWeights = map[string]int64{
	leadstore.StageTalking:  20,
	leadstore.StageProposal: 50,
	leadstore.StageWon:      100,
	leadstore.StageLost:     0,
}

// expectedCents is a deal of value cents (nil: none) weighted by stage; 0 with no value.
func expectedCents(stage string, value *int64) int64 {
	if value == nil {
		return 0
	}
	return *value * stageWeights[stage] / 100
}

// crmRepliesLimit is how many leads the replies inbox lists (the store's most).
const crmRepliesLimit = 200

// timelineLimit is how many items a lead's timeline shows.
const timelineLimit = 200

// crmErr turns a store error from a CRM read or write into the console's: a missing lead is
// console.ErrNotFound, input the store refused a *console.CRMRefused; anything else as it is.
func crmErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, leadstore.ErrLeadNotFound):
		return console.ErrNotFound
	case errors.Is(err, leadstore.ErrBadCRM):
		return crmRefusal(err)
	}
	return err
}

// crmFields are the form fields a store refusal names, by the words the store uses for
// them, checked in order (the lost reason before the bare "reason").
var crmFields = []struct{ words, field string }{
	{"lost reason", "lost_reason"},
	{"needs a reason", "lost_reason"},
	{"tenant link", "tenant_url"},
	{"next action", "next_action"},
	{"note", "note"},
	{"deal value", "value"},
	{"currency", "currency"},
	{"stage", "stage"},
}

// crmRefusal is a leadstore.ErrBadCRM refusal as the console shows it: the field the store's
// sentence names ("" when it names none, as for a snooze) and that sentence, capitalised,
// saying nothing was saved.
func crmRefusal(err error) *console.CRMRefused {
	msg := err.Error()
	if i := strings.LastIndex(msg, leadstore.ErrBadCRM.Error()+": "); i >= 0 {
		msg = msg[i+len(leadstore.ErrBadCRM.Error())+2:]
	}
	field := ""
	for _, f := range crmFields {
		if strings.Contains(msg, f.words) {
			field = f.field
			break
		}
	}
	if msg != "" {
		msg = strings.ToUpper(msg[:1]) + msg[1:]
	}
	return &console.CRMRefused{Field: field, Sentence: msg + ". Nothing was saved."}
}

// dayIn is the stored date d (a date column, read as midnight UTC) as midnight of that day
// in loc; nil for nil.
func dayIn(d *time.Time, loc *time.Location) *time.Time {
	if d == nil {
		return nil
	}
	y, m, dd := d.UTC().Date()
	t := time.Date(y, m, dd, 0, 0, 0, 0, loc)
	return &t
}

// LeadCRM is lead leadID's stage, note, next action, deal and snooze.
func (b *backend) LeadCRM(ctx context.Context, leadID int64) (console.LeadCRM, error) {
	if _, err := b.leadsSetup(ctx, leadsForProbe); err != nil {
		return console.LeadCRM{}, err
	}
	c, err := b.lq().GetLeadCRM(ctx, leadID)
	if err != nil {
		return console.LeadCRM{}, crmErr(err)
	}
	return console.LeadCRM{
		Stage: c.Stage, EffectiveStage: c.EffectiveStage, LostReason: c.LostReason, Note: c.Note,
		NextAction: c.NextAction, NextActionOn: dayIn(c.NextActionOn, b.loc), DealValueCents: c.DealValueCents,
		Currency: c.Currency, TenantURL: c.TenantURL, SnoozedUntil: c.SnoozedUntil,
	}, nil
}

// LeadTimeline is everything that happened to lead leadID, newest first, each item in plain
// words. The lead's whole Claude spend shows on its newest email sent (one read; there is no
// spend per item). console.ErrNotFound for no such lead.
func (b *backend) LeadTimeline(ctx context.Context, leadID int64) ([]console.TimelineItem, error) {
	if _, err := b.leadsSetup(ctx, leadsForProbe); err != nil {
		return nil, err
	}
	lq := b.lq()
	items, err := lq.LeadTimeline(ctx, leadID, timelineLimit)
	if err != nil {
		return nil, err
	}
	if len(items) == 0 {
		if _, err := lq.LeadByID(ctx, leadID); err != nil {
			return nil, crmErr(err)
		}
		return []console.TimelineItem{}, nil
	}
	out := make([]console.TimelineItem, 0, len(items))
	costAt := -1
	for _, it := range items {
		t := timelineItem(it)
		if costAt < 0 && it.Kind == leadstore.TimelineSent && !it.Shadow {
			costAt = len(out)
		}
		out = append(out, t)
	}
	if costAt >= 0 {
		spend, err := b.s.Q().ClaudeSpendForLead(ctx, leadID, "")
		if err != nil {
			return nil, err
		}
		out[costAt].CostMicros = spend
	}
	return out, nil
}

// timelineItem words one store timeline item: its kind as the console marks it, one plain
// line, and the snippet, note, reason or address as its detail.
func timelineItem(it leadstore.TimelineItem) console.TimelineItem {
	out := console.TimelineItem{At: it.At, Detail: it.Detail}
	shadow := func(line string) string {
		if it.Shadow {
			return line + " · shadow batch"
		}
		return line
	}
	switch it.Kind {
	case leadstore.TimelineEvent:
		out.Kind, out.Line = console.TimelineEvent, leadEventWords(it.State)
	case leadstore.TimelineSent:
		out.Kind, out.Line = console.TimelineEmail, shadow(sentWords(it.Channel, it.State, it.Step))
	case leadstore.TimelineMailbox:
		out.Kind, out.Line = console.TimelineReply, mailboxEventWords(it.State)
		if it.State == "bounce" {
			out.Kind = console.TimelineEvent
		}
	case leadstore.TimelineCall:
		line := "Call: " + console.OutcomeWords(it.State)
		if it.CallBackOn != nil {
			line += " · call back " + it.CallBackOn.UTC().Format("Mon 2 Jan")
		}
		out.Kind, out.Line = console.TimelineCall, shadow(line)
	case leadstore.TimelineEmailCheck:
		out.Kind, out.Line = console.TimelineCheck, "Email checked · "+emailCheckWords(it.State)
	case leadstore.TimelineStage:
		out.Kind, out.Line = console.TimelineStage, "Stage → "+console.StageWords(it.State)
	default:
		out.Kind, out.Line = console.TimelineEvent, strings.ReplaceAll(it.Kind, "_", " ")
	}
	return out
}

// leadEventWords is the state a lead entered, as a timeline line.
func leadEventWords(state string) string {
	switch state {
	case leadstore.LeadNew:
		return "Lead added"
	case leadstore.LeadBatched:
		return "Put in a batch"
	case leadstore.LeadOptedOut:
		return "Opted out"
	}
	w := strings.ReplaceAll(state, "_", " ")
	if w == "" {
		return "Lead changed"
	}
	return strings.ToUpper(w[:1]) + w[1:]
}

// sentWords is a message that went, as a timeline line: "Email sent · step 1", "Contact
// form used · step 2", "Email may have gone · step 1 (check it)".
func sentWords(channel, state string, step int) string {
	what := "Email"
	switch channel {
	case "form":
		what = "Contact form"
	case "linkedin":
		what = "LinkedIn note"
	}
	verb := " sent"
	switch {
	case state == leadstore.MsgMaybeSent:
		return fmt.Sprintf("%s may have gone · step %d (check it)", what, step+1)
	case channel == "form" || channel == "linkedin":
		verb = " used"
	}
	return fmt.Sprintf("%s%s · step %d", what, verb, step+1)
}

// mailboxEventWords is what the mailbox matched to the lead, as a timeline line.
func mailboxEventWords(kind string) string {
	switch kind {
	case "reply":
		return "Reply received"
	case "auto_reply":
		return "Auto-reply received"
	case "bounce":
		return "Email bounced"
	case "opt_out":
		return "Asked to stop"
	}
	return "Mail received"
}

// SetLeadStage sets lead c.LeadID's stage at the console's now. Stage "" clears the stored
// stage (leadstore.ClearLeadStage), so the lead follows its replies and calls again.
func (b *backend) SetLeadStage(ctx context.Context, c console.StageChange) (string, error) {
	if _, err := b.leadsSetup(ctx, leadsForProbe); err != nil {
		return "", err
	}
	if c.Stage == "" {
		return "", crmErr(b.lq().ClearLeadStage(ctx, c.LeadID, b.now()))
	}
	return "", crmErr(b.lq().SetLeadStage(ctx, c.LeadID, c.Stage, c.LostReason, c.TenantURL, b.now()))
}

// SetLeadNote saves lead c.LeadID's note, next action and its day.
func (b *backend) SetLeadNote(ctx context.Context, c console.NoteChange) (string, error) {
	if _, err := b.leadsSetup(ctx, leadsForProbe); err != nil {
		return "", err
	}
	var on *time.Time
	if c.NextActionOn != nil {
		d := c.NextActionOn.In(b.loc) // the store keeps its calendar date in its own zone
		on = &d
	}
	return "", crmErr(b.lq().SetLeadNote(ctx, c.LeadID, c.Note, c.NextAction, on, b.now()))
}

// SetLeadDeal saves lead c.LeadID's deal value and currency.
func (b *backend) SetLeadDeal(ctx context.Context, c console.DealChange) (string, error) {
	if _, err := b.leadsSetup(ctx, leadsForProbe); err != nil {
		return "", err
	}
	return "", crmErr(b.lq().SetLeadDeal(ctx, c.LeadID, c.ValueCents, c.Currency, b.now()))
}

// SnoozeLead hides lead leadID until until; nil shows it again at once.
func (b *backend) SnoozeLead(ctx context.Context, leadID int64, until *time.Time) (string, error) {
	if _, err := b.leadsSetup(ctx, leadsForProbe); err != nil {
		return "", err
	}
	if until == nil {
		return "", crmErr(b.lq().ClearSnooze(ctx, leadID, b.now()))
	}
	return "", crmErr(b.lq().SnoozeLead(ctx, leadID, *until, b.now()))
}

// campaignNames is every campaign's name (its slug when it has none), by id.
func campaignNames(ctx context.Context, lq leadstore.Queries) (map[int64]string, error) {
	cs, err := lq.Campaigns(ctx, leadstore.DefaultAccountID)
	if err != nil {
		return nil, err
	}
	out := make(map[int64]string, len(cs))
	for _, c := range cs {
		out[c.ID] = c.Name
		if c.Name == "" {
			out[c.ID] = c.Slug
		}
	}
	return out, nil
}

// RepliesInbox is the leads that replied, newest reply first, snoozed ones left out.
func (b *backend) RepliesInbox(ctx context.Context) ([]console.ReplyRow, error) {
	if _, err := b.leadsSetup(ctx, leadsForProbe); err != nil {
		return nil, err
	}
	lq := b.lq()
	rows, _, err := lq.RepliesInbox(ctx, b.now(), crmRepliesLimit)
	if err != nil {
		return nil, err
	}
	names, err := campaignNames(ctx, lq)
	if err != nil {
		return nil, err
	}
	out := make([]console.ReplyRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, console.ReplyRow{LeadID: r.LeadID, Name: r.Name, Domain: r.Domain, Campaign: names[r.CampaignID],
			Snippet: r.Snippet, At: r.RepliedAt, EffectiveStage: r.EffectiveStage})
	}
	return out, nil
}

// Pipeline is every lead with an effective stage, in the store's board order, each with its
// expected value (stageWeights).
func (b *backend) Pipeline(ctx context.Context) ([]console.PipelineCard, error) {
	if _, err := b.leadsSetup(ctx, leadsForProbe); err != nil {
		return nil, err
	}
	rows, err := b.lq().Pipeline(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]console.PipelineCard, 0, len(rows))
	for _, r := range rows {
		out = append(out, console.PipelineCard{LeadID: r.LeadID, Name: r.Name, Domain: r.Domain, Stage: r.Stage,
			DealValueCents: r.DealValueCents, Currency: r.Currency, ExpectedCents: expectedCents(r.Stage, r.DealValueCents),
			NextAction: r.NextAction, NextActionOn: dayIn(r.NextActionOn, b.loc), LostReason: r.LostReason, TenantURL: r.TenantURL})
	}
	return out, nil
}

// CallQueue is day's calls and call-backs in the console's zone, each judged by the call
// rule as its card is (callable now, or why not). A call-back whose sequence has already
// moved on past its call is Done: the outcome route would refuse it (ErrCallAdvanced).
func (b *backend) CallQueue(ctx context.Context, day time.Time) ([]console.CallQueueRow, error) {
	if _, err := b.leadsSetup(ctx, leadsForProbe); err != nil {
		return nil, err
	}
	lq := b.lq()
	rows, err := lq.CallQueue(ctx, day, b.loc)
	if err != nil {
		return nil, err
	}
	calls := make([]leadstore.CallRow, len(rows))
	for i, r := range rows {
		calls[i] = r.CallRow
	}
	verdicts, err := callVerdicts(ctx, lq, b.now(), calls)
	if err != nil {
		return nil, err
	}
	out := make([]console.CallQueueRow, 0, len(rows))
	for _, r := range rows {
		vd := verdicts[r.MessageID]
		due := r.Due
		row := console.CallQueueRow{MessageID: r.MessageID, LeadID: r.LeadID, Name: r.Name, Number: r.Phone,
			Dial: dial.Number(r.PhoneNormalized, r.Country), Due: &due, Callable: vd.Callable, BlockedReason: vd.Reason, Shadow: r.Shadow}
		if r.WaitsOnStep != nil {
			row.Callable, row.BlockedReason = false, "it waits on "+waitsOnStepWords(*r.WaitsOnStep, r.WaitsOnChannel)
		}
		if r.Kind == leadstore.CallQueueBack {
			row.CallBackOn = dayIn(r.CallBackOn, b.loc)
			if r.State == leadstore.MsgDone && r.Outcome != "" {
				if row.Done, err = callAdvanced(ctx, b.s.Pool(), r.CallRow); err != nil {
					return nil, err
				}
			}
		}
		out = append(out, row)
	}
	return out, nil
}

// CampaignReport is each campaign's funnel from since on (leadstore.CampaignFunnel) with the
// Claude spend on its leads since then (every purpose: a lead's Claude spend is drafting,
// research and drafts_eval), divided per lead contacted and per reply. A campaign that spent
// on Claude or ran email checks but did nothing else since is listed too, after the rest.
// EmailChecks counts the checks run since then on its leads' addresses
// (leadstore.EmailChecksByCampaign).
func (b *backend) CampaignReport(ctx context.Context, since time.Time) ([]console.CampaignReportRow, error) {
	if _, err := b.leadsSetup(ctx, leadsForProbe); err != nil {
		return nil, err
	}
	lq := b.lq()
	funnel, err := lq.CampaignFunnel(ctx, since)
	if err != nil {
		return nil, err
	}
	spend, err := b.s.Q().ClaudeSpendByLead(ctx, nil, since, "")
	if err != nil {
		return nil, err
	}
	ids := make([]int64, 0, len(spend))
	for id := range spend {
		ids = append(ids, id)
	}
	campaignOf, err := lq.LeadCampaigns(ctx, ids)
	if err != nil {
		return nil, err
	}
	byCampaign := map[int64]int64{}
	for id, micros := range spend {
		if c, ok := campaignOf[id]; ok {
			byCampaign[c] += micros
		}
	}
	checks, err := lq.EmailChecksByCampaign(ctx, since)
	if err != nil {
		return nil, err
	}
	out := make([]console.CampaignReportRow, 0, len(funnel))
	seen := map[int64]bool{}
	for _, f := range funnel {
		seen[f.CampaignID] = true
		name := f.Name
		if name == "" {
			name = f.Slug
		}
		row := reportRow(name, f, byCampaign[f.CampaignID])
		row.EmailChecks = checks[f.CampaignID]
		out = append(out, row)
	}
	var extra []int64
	for c := range byCampaign {
		if !seen[c] {
			seen[c] = true
			extra = append(extra, c)
		}
	}
	for c := range checks {
		if !seen[c] {
			seen[c] = true
			extra = append(extra, c)
		}
	}
	if len(extra) > 0 {
		slices.Sort(extra)
		names, err := campaignNames(ctx, lq)
		if err != nil {
			return nil, err
		}
		for _, c := range extra {
			row := reportRow(names[c], leadstore.CampaignFunnelRow{CampaignID: c}, byCampaign[c])
			row.EmailChecks = checks[c]
			out = append(out, row)
		}
	}
	return out, nil
}

// reportRow is one campaign's report row: funnel f, its revenue per currency in the order of
// leadstore.Currencies, and its Claude spend divided by leads contacted and by replies (0
// when there is nothing to divide by).
func reportRow(name string, f leadstore.CampaignFunnelRow, micros int64) console.CampaignReportRow {
	row := console.CampaignReportRow{Campaign: name, Sent: f.Sent, Contacted: f.Contacted, Replied: f.Replied,
		Positive: f.Positive, Won: f.Won, ClaudeMicros: micros,
		CostPerLeadMicros: perEach(micros, f.Contacted), CostPerReplyMicros: perEach(micros, f.Replied)}
	currencies := make([]string, 0, len(f.Revenue))
	for c := range f.Revenue {
		currencies = append(currencies, c)
	}
	rank := func(c string) int {
		if i := slices.Index(leadstore.Currencies, c); i >= 0 {
			return i
		}
		return len(leadstore.Currencies)
	}
	slices.SortFunc(currencies, func(a, b string) int {
		if d := rank(a) - rank(b); d != 0 {
			return d
		}
		return strings.Compare(a, b)
	})
	for _, c := range currencies {
		row.Revenue = append(row.Revenue, console.Money{Currency: c, Cents: f.Revenue[c]})
	}
	return row
}

// perEach is micros divided by n, rounded down; 0 when n is 0 or less.
func perEach(micros int64, n int) int64 {
	if n <= 0 {
		return 0
	}
	return micros / int64(n)
}
