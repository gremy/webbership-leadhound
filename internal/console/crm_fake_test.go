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

// outreachFake is the outreach part of fakeBackend: canned answers, and what each write asked.
type outreachFake struct {
	draftRequest *LeadDraftRequest
	proofs       ProofPage
	outreachErr  error // returned by every outreach write
	outreachMsg  string
	lastContact  *NewContact
	lastEdit     *ContactChange
	lastRetire   *ContactRef
	lastDraft    *DraftEdit
	lastSent     *MarkSentRequest
	lastAsked    string // the channel the last RequestDraft asked for
	lastDiscard  int64
	lastSector   string
	lastProposal *NewProposal
	lastStatus   *ProposalStatusChange
	lastProof    *NewProof
	lastProofOut int64
	lastRetired  string // the note or reason of the last retire
}

// outreachWrite counts a write and stores what it asked.
func (f *fakeBackend) outreachWrite(name string, store func()) (string, error) {
	f.count(name)
	f.mu.Lock()
	store()
	f.mu.Unlock()
	return f.outreachMsg, f.outreachErr
}

func (f *fakeBackend) AddContact(_ context.Context, c NewContact) (string, error) {
	return f.outreachWrite("AddContact", func() { f.lastContact = &c })
}

func (f *fakeBackend) EditContact(_ context.Context, c ContactChange) (string, error) {
	return f.outreachWrite("EditContact", func() { f.lastEdit = &c })
}

func (f *fakeBackend) RetireContact(_ context.Context, _ int64, ref ContactRef, note string) (string, error) {
	return f.outreachWrite("RetireContact", func() { f.lastRetire, f.lastRetired = &ref, note })
}

func (f *fakeBackend) SaveDraft(_ context.Context, e DraftEdit) (string, error) {
	return f.outreachWrite("SaveDraft", func() { f.lastDraft = &e })
}

func (f *fakeBackend) MarkSent(_ context.Context, m MarkSentRequest) (string, error) {
	return f.outreachWrite("MarkSent", func() { f.lastSent = &m })
}

func (f *fakeBackend) RequestDraft(_ context.Context, _ int64, channel string) (string, error) {
	return f.outreachWrite("RequestDraft", func() { f.lastAsked = channel })
}

func (f *fakeBackend) DraftStatus(context.Context, int64) (*LeadDraftRequest, error) {
	f.count("DraftStatus")
	return f.draftRequest, f.leadsRead()
}

func (f *fakeBackend) DiscardDraft(_ context.Context, _, batchID int64) (string, error) {
	return f.outreachWrite("DiscardDraft", func() { f.lastDiscard = batchID })
}

func (f *fakeBackend) SetSector(_ context.Context, _ int64, sector string) (string, error) {
	return f.outreachWrite("SetSector", func() { f.lastSector = sector })
}

func (f *fakeBackend) AddProposal(_ context.Context, p NewProposal) (string, error) {
	return f.outreachWrite("AddProposal", func() { f.lastProposal = &p })
}

func (f *fakeBackend) SetProposalStatus(_ context.Context, c ProposalStatusChange) (string, error) {
	return f.outreachWrite("SetProposalStatus", func() { f.lastStatus = &c })
}

func (f *fakeBackend) Proofs(context.Context) (ProofPage, error) {
	f.count("Proofs")
	return f.proofs, f.leadsRead()
}

func (f *fakeBackend) AddProof(_ context.Context, p NewProof) (string, error) {
	return f.outreachWrite("AddProof", func() { f.lastProof = &p })
}

func (f *fakeBackend) RetireProof(_ context.Context, id int64, reason string) (string, error) {
	return f.outreachWrite("RetireProof", func() { f.lastProofOut, f.lastRetired = id, reason })
}

// richOutreach is a proof library with one line that has a public page and one that does not.
func richOutreach(text string) outreachFake {
	sector := []FilterOption{{"education", "Education"}, {"government", "Government"}}
	at := statusNow.Add(-72 * time.Hour)
	return outreachFake{proofs: ProofPage{
		Sectors: append(sector, FilterOption{"other", "Other"}),
		Proofs: []Proof{
			{ID: 3, Title: text, Line: text, URL: "https://harbourlane.example/work", Public: true, Sectors: sector, At: at},
			{ID: 2, Title: "Harbour Lane Studio", Line: "A line with no public page.", Sectors: sector[:1], At: at},
		},
	}}
}

// richOutreachDetail is d with something on every outreach card, every text field set to text: a
// draft of each channel and a contact of each kind, in the states the page tells apart.
func richOutreachDetail(d LeadDetailPage, text string) LeadDetailPage {
	at := statusNow.Add(-5 * time.Hour)
	sent := statusNow.Add(-2 * time.Hour)
	v := int64(240000)
	d.Reach, d.Channel = ReachManual, ChannelLinkedIn
	d.Drafts = []LeadDraft{
		{MessageID: 9, Channel: ChannelLinkedIn, Step: 0, Label: "Connection note", Note: true, TargetURL: "https://linkedin.com/company/harbourlane",
			Body: text, Edited: true, OriginalBody: text + " before", Editable: true, CanMarkSent: true, BatchID: 3, BatchHash: "0123456789ab",
			Origin: OriginDraftIt, Writer: WriterClaude, State: "Draft", DueNow: true},
		{MessageID: 10, Channel: ChannelLinkedIn, Step: 1, Label: "After they accept", TargetURL: "https://linkedin.com/company/harbourlane",
			Body: text, Editable: true, CanMarkSent: true, BatchID: 3, BatchHash: "0123456789ab", Origin: OriginDraftIt, Writer: WriterTemplate,
			Fallback: "Claude refused twice", State: "Draft"},
		{MessageID: 11, Channel: ChannelForm, Step: 0, Label: "Form message", TargetURL: "https://harbourlane.example/contact",
			Body: text, BatchID: 2, BatchHash: "ba9876543210", Origin: OriginBatch, Writer: WriterTemplate, State: "Marked sent", Sent: &sent},
		{MessageID: 12, Channel: ChannelEmail, Step: 0, Label: "First email", To: "hello@harbourlane.example", Subject: text,
			Body: text, Editable: true, BatchID: 4, BatchHash: "ab0123456789", Origin: OriginDraftIt, Writer: WriterClaude, State: "Draft", Due: &at},
	}
	d.Contacts = []LeadContact{
		{CID: "r12", Kind: ContactEmail, Value: "hello@harbourlane.example", Provenance: ProvenanceCrawl, FoundAt: at, Chosen: true,
			PageURL: "https://harbourlane.example/contact", Check: "Email check: safe, may be sent", CheckedAt: &at, MailServer: "Mail server found"},
		{CID: "r13", Kind: ContactLinkedIn, Value: "https://linkedin.com/company/harbourlane", Provenance: ProvenanceOperator, Operator: true, FoundAt: at, Note: text,
			Warning: "Its country is off in leads.yaml; this is your own contact, so it is still used."},
		{CID: "r14", Kind: ContactForm, Value: "https://harbourlane.example/old", Provenance: ProvenanceResearch, FoundAt: at, Retired: true,
			ReplacedBy: "https://harbourlane.example/contact"},
		{CID: "f7", Kind: ContactPhone, Value: "+44 20 7946 0177", Provenance: ProvenanceOverture, FoundAt: at},
		{CID: "p3", Kind: ContactPerson, Name: text, Role: "Director", Provenance: ProvenanceOperator, Operator: true, FoundAt: at, PersonID: 3},
	}
	d.DraftRequest = &LeadDraftRequest{Status: DraftQueued, RequestedAt: at}
	d.DraftItBlock = "Asked for at 09:12"
	d.DiscardBatchID = 3
	d.Proposals = []LeadProposal{
		{ID: 3, Title: text, URL: "https://harbourlane.example/proposal", ValueCents: &v, Currency: "GBP", At: at, Status: ProposalSent, StatusNote: text, StatusAt: at},
		{ID: 2, Title: "Harbour Lane Studio", Currency: "EUR", At: at, Status: ProposalDeclined, StatusAt: at},
	}
	d.Sector, d.SectorWords, d.SectorByRule = "education", "Education", true
	d.SectorOptions = []FilterOption{{"education", "Education"}, {"other", "Other"}}
	d.NoteMax = 200
	return d
}

// richReachList is p with a next action on every row and a number on every tab.
func richReachList(p LeadListPage) LeadListPage {
	reaches := []string{ReachManual, ReachDead, ReachEmail, ReachContacted}
	for i := range p.Rows {
		p.Rows[i].Reach = reaches[i%len(reaches)]
		if p.Rows[i].Reach == ReachDead {
			p.Rows[i].ReachReason = ReachNotSearched
		}
	}
	p.ReachCounts = ReachCounts{Email: 12, Manual: 7, Dead: 31, Contacted: 4, Other: 2}
	return p
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
