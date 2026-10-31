package console

import (
	"context"
	"time"
)

// crmFake is the CRM part of fakeBackend: canned answers, and what each write asked.
type crmFake struct {
	crm           LeadCRM
	timeline      []TimelineItem
	replies       []ReplyRow
	pipeline      []PipelineCard
	callQueue     []CallQueueRow
	campReport    []CampaignReportRow
	crmErr        error // returned by every CRM write
	crmMsg        string
	lastStage     *StageChange
	lastNote      *NoteChange
	lastDeal      *DealChange
	lastSnooze    *time.Time
	snoozeCalls   int
	lastOutcomeOn *CallOutcomeRequest
	queueDay      time.Time
	reportSince   time.Time
}

func (f *fakeBackend) LeadCRM(_ context.Context, _ int64) (LeadCRM, error) {
	f.count("LeadCRM")
	if err := f.leadsRead(); err != nil {
		return LeadCRM{}, err
	}
	return f.crm, nil
}

func (f *fakeBackend) LeadTimeline(_ context.Context, _ int64) ([]TimelineItem, error) {
	f.count("LeadTimeline")
	return f.timeline, f.leadsRead()
}

func (f *fakeBackend) SetLeadStage(_ context.Context, c StageChange) (string, error) {
	f.count("SetLeadStage")
	f.mu.Lock()
	f.lastStage = &c
	f.mu.Unlock()
	return f.crmMsg, f.crmErr
}

func (f *fakeBackend) SetLeadNote(_ context.Context, c NoteChange) (string, error) {
	f.count("SetLeadNote")
	f.mu.Lock()
	f.lastNote = &c
	f.mu.Unlock()
	return f.crmMsg, f.crmErr
}

func (f *fakeBackend) SetLeadDeal(_ context.Context, c DealChange) (string, error) {
	f.count("SetLeadDeal")
	f.mu.Lock()
	f.lastDeal = &c
	f.mu.Unlock()
	return f.crmMsg, f.crmErr
}

func (f *fakeBackend) SnoozeLead(_ context.Context, _ int64, until *time.Time) (string, error) {
	f.count("SnoozeLead")
	f.mu.Lock()
	f.lastSnooze, f.snoozeCalls = until, f.snoozeCalls+1
	f.mu.Unlock()
	return f.crmMsg, f.crmErr
}

func (f *fakeBackend) RepliesInbox(context.Context) ([]ReplyRow, error) {
	f.count("RepliesInbox")
	return f.replies, f.leadsRead()
}

func (f *fakeBackend) Pipeline(context.Context) ([]PipelineCard, error) {
	f.count("Pipeline")
	return f.pipeline, f.leadsRead()
}

func (f *fakeBackend) CallQueue(_ context.Context, day time.Time) ([]CallQueueRow, error) {
	f.count("CallQueue")
	f.mu.Lock()
	f.queueDay = day
	f.mu.Unlock()
	return f.callQueue, f.leadsRead()
}

func (f *fakeBackend) CampaignReport(_ context.Context, since time.Time) ([]CampaignReportRow, error) {
	f.count("CampaignReport")
	f.mu.Lock()
	f.reportSince = since
	f.mu.Unlock()
	return f.campReport, f.leadsRead()
}

func (f *fakeBackend) LeadCallOutcomeOn(_ context.Context, req CallOutcomeRequest) (string, error) {
	f.count("LeadCallOutcomeOn")
	f.mu.Lock()
	f.lastOutcomeOn = &req
	f.lastOutcome = outcomeCall{ID: req.MessageID, Outcome: req.Outcome, Note: req.Note, Consent: req.Consent, ConsentAddress: req.ConsentAddress}
	f.mu.Unlock()
	return f.outcomeMsg, f.outcomeErr
}

// richCRM is a CRM with something on every page, every text field set to text: one lead in
// each stage, a reply, a call and a call-back, and two campaigns.
func richCRM(text string) crmFake {
	v := func(c int64) *int64 { return &c }
	at := statusNow.Add(-3 * time.Hour)
	next := time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)
	snooze := statusNow.Add(20 * time.Hour)
	return crmFake{
		crm: LeadCRM{Stage: StageProposal, EffectiveStage: StageProposal, Note: text, NextAction: text, NextActionOn: &next,
			DealValueCents: v(240000), Currency: "GBP", SnoozedUntil: &snooze},
		timeline: []TimelineItem{
			{At: at, Kind: TimelineReply, Line: "Reply received", Detail: text},
			{At: at.Add(-48 * time.Hour), Kind: TimelineEmail, Line: "Email sent · step 1", CostMicros: 20_000},
			{At: at.Add(-72 * time.Hour), Kind: TimelineCheck, Line: "Email checked · valid"},
		},
		replies: []ReplyRow{{LeadID: 5, Name: text, Domain: "willowlane.example", Campaign: "Cafe loyalty", Snippet: text, At: at, EffectiveStage: StageTalking}},
		pipeline: []PipelineCard{
			{LeadID: 5, Name: text, Stage: StageTalking, NextAction: text, NextActionOn: &next},
			{LeadID: 6, Name: "Halden & Rook", Stage: StageProposal, DealValueCents: v(240000), Currency: "GBP", ExpectedCents: 120000},
			{LeadID: 8, Name: "Saltmarsh Roasters", Stage: StageProposal, DealValueCents: v(95050), Currency: "EUR", ExpectedCents: 47525},
			{LeadID: 9, Name: "Copperleaf Books", Stage: StageWon, DealValueCents: v(120000), Currency: "RON", ExpectedCents: 120000, TenantURL: "https://copperleaf.example.com"},
			{LeadID: 10, Name: "Fernway Clinic", Stage: StageLost, LostReason: text},
		},
		callQueue: []CallQueueRow{
			{MessageID: 9, LeadID: 5, Name: text, Number: "+44 20 7946 0177", Dial: "+442079460177", Due: &at, Callable: true},
			{MessageID: 11, LeadID: 6, Name: "Halden & Rook", Number: "+44 20 7946 0123", CallBackOn: &next, Done: true, BlockedReason: "Outside calling hours now"},
		},
		campReport: []CampaignReportRow{
			{Campaign: text, Sent: 212, Contacted: 180, Replied: 19, Positive: 6, Won: 1, Revenue: []Money{{"GBP", 240000}},
				ClaudeMicros: 16_200_000, CostPerLeadMicros: 90_000, CostPerReplyMicros: 852_631, EmailChecks: 140},
			{Campaign: "Cafe loyalty", Sent: 0},
		},
	}
}
