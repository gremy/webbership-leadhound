package main

import (
	"bufio"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gremy/webbership-leadhound/internal/ai"
	"github.com/gremy/webbership-leadhound/internal/campaign"
	"github.com/gremy/webbership-leadhound/internal/claude"
	"github.com/gremy/webbership-leadhound/internal/leadconfig"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/outreach"
	"github.com/gremy/webbership-leadhound/internal/store"
)

// Grounded Claude drafting (CP4) as the batch, `leads drafts sample` and `leads drafts eval`
// share it: one lead's WriteInput from its stored rows, every attempt recorded as a
// draft_generation, the trust gate read from the store, and the rows a batch stores for a
// lead Claude wrote.

// Why a lead of a writer: claude campaign was drafted from templates (NewBatchOutcome.Fallbacks).
const (
	fbCap         = "today's Claude cap reached"
	fbLeadCap     = "this lead's Claude cap reached"
	fbRefused     = "draft refused"
	fbUntrusted   = "Claude drafts not trusted yet"
	fbOff         = "Claude drafting off for this run"
	fbUnavailable = "Claude unavailable"
	fbNoInput     = "no fact sheet for Claude"
)

// failStop is the failure code a generation row carries for an attempt the model stopped
// (refusal or max_tokens) without a grounding failure: the row must name why it did not pass.
const failStop = "stop"

// greetings are the line above a Claude draft's first paragraph, by language; a language
// missing here gets none.
var greetings = map[string]string{outreach.LangEnglish: "Hello,"}

// newDraftCaller builds the spend-capped Claude caller for purpose (claude.PurposeDrafting or
// claude.PurposeDraftsEval). nil, nil means live calls are off (claudeCapped has said so).
// Tests replace it with a fake.
var newDraftCaller = func(cmd string, q store.Queries, purpose string) (outreach.Caller, error) {
	c, err := claudeCapped(cmd, q, purpose, claudeCampaignCaps{})
	if err != nil || c == nil {
		return nil, err
	}
	return c, nil
}

// generationRecorder appends draft_generation rows; leadstore.Queries is one. The batch
// records through a pool connection, outside its own transaction, so a billed attempt's row
// stays even when the batch is not written.
type generationRecorder interface {
	RecordGeneration(ctx context.Context, g leadstore.GenerationRow) (int64, error)
}

// claudeDrafting is what a batch may draft with Claude: the capped caller (nil: off, with
// off saying why) and where the attempts are recorded. A batchSetup without one (the
// console) drafts every lead from templates.
type claudeDrafting struct {
	caller outreach.Caller
	off    string
	rec    generationRecorder
}

// recordAttempts stores every attempt of one lead's write as a draft_generation row, in
// order, and returns their ids.
func recordAttempts(ctx context.Context, rec generationRecorder, leadID, campaignID int64, sheet outreach.FactSheet,
	attempts []outreach.Attempt, purpose string) ([]int64, error) {
	ids := make([]int64, 0, len(attempts))
	for i, a := range attempts {
		fails := a.Failures
		if !a.OK() && len(fails) == 0 {
			fails = []outreach.Failure{{Code: failStop, Step: outreach.StepTalkingPoints, Para: outreach.ParaSubject,
				Token: a.StopReason, Msg: "the model stopped (" + a.StopReason + ") without a usable answer"}}
		}
		cost := a.CostMicros
		id, err := rec.RecordGeneration(ctx, leadstore.GenerationRow{
			LeadID: leadID, CampaignID: campaignID, Attempt: i + 1, Model: cmp.Or(a.Model, claude.FakeModel),
			Facts: sheet, Output: a.Output, CheckOK: a.OK(), Failures: fails, CostMicros: &cost, Purpose: purpose,
		})
		if err != nil {
			return ids, err
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// capReason is why a cap stopped a Claude call: the lead's own cap or the day's.
func capReason(err error) string {
	if strings.Contains(err.Error(), ": lead spent ") {
		return fbLeadCap
	}
	return fbCap
}

// --- the store side of the gate ---

// claudeReviews returns the latest review of every reviewed generation of campaignID that
// Claude wrote (a fake decider's sample never counts), newest review first, with each
// reviewed generation by id.
func claudeReviews(ctx context.Context, lq leadstore.Queries, campaignID int64) ([]leadstore.Review, map[int64]leadstore.Generation, error) {
	all, err := lq.ReviewsForCampaign(ctx, campaignID, 0)
	if err != nil {
		return nil, nil, err
	}
	gens := map[int64]leadstore.Generation{}
	var out []leadstore.Review
	for _, r := range all {
		g, err := lq.GenerationByID(ctx, r.GenerationID)
		if err != nil {
			return nil, nil, err
		}
		if g.Model == claude.FakeModel {
			continue
		}
		gens[g.ID] = g
		out = append(out, r)
	}
	return out, gens, nil
}

// gateStore is outreach.GateSource over the lead store.
type gateStore struct{ lq leadstore.Queries }

var _ outreach.GateSource = gateStore{}

func (s gateStore) LatestDraftEval(ctx context.Context, campaignID int64) (outreach.GateEval, bool, error) {
	e, ok, err := s.lq.LatestEval(ctx, campaignID)
	if err != nil || !ok {
		return outreach.GateEval{}, false, err
	}
	return outreach.GateEval{Reviewed: e.Reviewed, FirstTryPass: e.FirstTryPass, AfterRetryPass: e.AfterRetryPass, At: e.At}, true, nil
}

func (s gateStore) LastDraftReviews(ctx context.Context, campaignID int64, n int) (outreach.GateReviews, error) {
	rs, _, err := claudeReviews(ctx, s.lq, campaignID)
	if err != nil {
		return outreach.GateReviews{}, err
	}
	if n > 0 && len(rs) > n {
		rs = rs[:n]
	}
	sum := leadstore.SummarizeReviews(rs)
	return outreach.GateReviews{Reviewed: sum.Reviewed, Approved: sum.Approved}, nil
}

// draftGate is campaignID's trust gate as it stands.
func draftGate(ctx context.Context, lq leadstore.Queries, campaignID int64) (outreach.GateState, error) {
	return outreach.Gate(ctx, gateStore{lq}, campaignID)
}

// approvedExamples are at most outreach.MaxExamples approved Claude drafts of the campaign,
// newest review first, one per lead, and the ids of every approved generation.
func approvedExamples(ctx context.Context, lq leadstore.Queries, campaignID int64) ([]outreach.WrittenDraft, map[int64]bool, error) {
	rs, gens, err := claudeReviews(ctx, lq, campaignID)
	if err != nil {
		return nil, nil, err
	}
	approved := map[int64]bool{}
	var ex []outreach.WrittenDraft
	leads := map[int64]bool{}
	for _, r := range rs {
		g := gens[r.GenerationID]
		if r.Verdict != leadstore.VerdictApprove || !g.CheckOK || g.Output == nil {
			continue
		}
		approved[g.ID] = true
		if len(ex) >= outreach.MaxExamples || leads[g.LeadID] {
			continue
		}
		d, err := outreach.DecodeWrittenDraft(g.Output)
		if err != nil {
			continue // a stored answer that no longer decodes is no example
		}
		leads[g.LeadID] = true
		ex = append(ex, d)
	}
	return ex, approved, nil
}

// --- one lead's input ---

// draftCtx is what every lead of one campaign is written with. plan is the batch's plan;
// each lead writes under its own (batchPlan.forLead), which writeInput is given.
type draftCtx struct {
	cfg        leadconfig.Config
	plan       batchPlan
	campaignID int64
	examples   []outreach.WrittenDraft
}

// stepNeedsPhone reports whether st gets a row only when the lead has a callable number
// (draftSequence's rule).
func stepNeedsPhone(st campaign.Step) bool {
	return st.Type == campaign.StepCall || slices.Contains(
		[]string{campaign.WhenHasCallablePhone, campaign.WhenCallNotConnected, campaign.WhenEmailConsent}, st.When)
}

// writeInput builds lead c's WriteInput from its stored rows: the fact sheet from
// sheetInputForLead (the report's own builder) with the campaign's offer facts, the
// campaign's grounding rules for the lead's signals, only the email steps it will get a row
// for (so a lead without a number is not asked for the "I tried to call" email), and the
// template path's slots and footer for the rendering tail. plan is the lead's own plan
// (batchPlan.forLead): a call-first lead is written to the call_first sequence's steps.
func (d draftCtx) writeInput(ctx context.Context, rs reportStore, plan batchPlan, c leadstore.Candidate, ev leadstore.Evidence, phone bool) (outreach.WriteInput, error) {
	spec := plan.spec
	lead, err := rs.LeadByID(ctx, c.LeadID)
	if err != nil {
		return outreach.WriteInput{}, err
	}
	policy := d.cfg.Policy(c.Country)
	ref := outreach.Provenance{Table: "campaign", ID: d.campaignID}
	si, _, err := sheetInputForLead(ctx, rs, d.cfg, lead, spec, ref, policy.Lang)
	if err != nil {
		return outreach.WriteInput{}, err
	}
	if spec.Offer.Report != "" {
		si.Offer.Report, si.Offer.Source = spec.Offer.Report, ref
	}
	if si.Offer.Report != "" {
		si.Offer.Contents = slices.Clone(spec.Offer.ReportContains)
	}
	if spec.Offer.CallToAction != "" {
		si.Offer.CallToAction, si.Offer.Source = spec.Offer.CallToAction, ref
	}
	major := 0
	if si.Finding != nil {
		major = si.Finding.Major
	}
	rules, err := spec.GroundRules(campaign.Signals{Kind: lead.Kind, Major: major})
	if err != nil {
		return outreach.WriteInput{}, err
	}
	rules.StepRules = plan.rulesFor(lead.Kind)
	rules.EmailSteps = nil
	var calls []int
	afterCall := false // the first email goes only on their yes on a call (call_first)
	for _, st := range plan.seq {
		if stepNeedsPhone(st) && !phone {
			continue
		}
		switch st.Type {
		case campaign.StepEmail:
			if len(rules.EmailSteps) == 0 {
				afterCall = st.When == campaign.WhenEmailConsent
			}
			rules.EmailSteps = append(rules.EmailSteps, st.Step)
		case campaign.StepCall:
			calls = append(calls, st.Step)
		}
	}
	rules.ConfigBanned = slices.Clone(d.cfg.Copy.BannedWords)
	rules.CallStep = len(calls) > 0
	slots, err := leadSlots(c, ev, d.cfg, spec)
	if err != nil {
		return outreach.WriteInput{}, err
	}
	footer := plan.footerFor(c.Country, d.cfg)
	if footer == nil {
		return outreach.WriteInput{}, fmt.Errorf("no footer for country %q", c.Country)
	}
	return outreach.WriteInput{
		LeadID: c.LeadID, Campaign: spec.Name, Lang: policy.Lang,
		Sheet: outreach.BuildFactSheet(si), Rules: rules, CallSteps: calls, AfterCall: afterCall, Examples: d.examples,
		Render: outreach.WriteRender{Cfg: d.cfg, Footer: footer, Slots: slots, Greeting: greetings[policy.Lang]},
	}, nil
}

// leadCandidate is lead leadID as a Candidate, for a lead drafted outside a batch (eval).
func leadCandidate(ctx context.Context, lq leadstore.Queries, leadID int64) (leadstore.Candidate, error) {
	l, err := lq.LeadByID(ctx, leadID)
	if err != nil {
		return leadstore.Candidate{}, err
	}
	o, err := lq.OrgByID(ctx, l.OrgID)
	if err != nil {
		return leadstore.Candidate{}, err
	}
	return leadstore.Candidate{LeadID: l.ID, OrgID: o.ID, Domain: o.Domain, OrgName: o.Name, Country: o.Country, Region: o.Region,
		Kind: l.Kind, Channel: l.Channel, RouteID: l.RouteID, ProbeID: l.ProbeID, AgencyFindingID: l.AgencyFindingID}, nil
}

// leadDraftPhone is the number lead c's call steps dial under plan at now, by the batch's own rule
// (callPhone), reading the org's usable facts and the account's phone suppressions; nil with
// why when the call is left out. A plan with no call step gives nil, "".
func leadDraftPhone(ctx context.Context, lq leadstore.Queries, plan batchPlan, c leadstore.Candidate, now time.Time) (*leadstore.Fact, string, error) {
	if !plan.hasCall() {
		return nil, "", nil
	}
	sup, err := suppressedPhoneSet(ctx, lq)
	if err != nil {
		return nil, "", err
	}
	facts, err := lq.UsableOrgFacts(ctx, []int64{c.OrgID})
	if err != nil {
		return nil, "", err
	}
	return callPhone(ctx, lq, plan, c, facts, sup, now)
}

// suppressedPhoneSet is the account's active phone suppressions, as pickPhone reads them.
func suppressedPhoneSet(ctx context.Context, lq leadstore.Queries) (map[string]bool, error) {
	set, err := lq.SuppressionSet(ctx, leadstore.DefaultAccountID)
	if err != nil {
		return nil, err
	}
	sup := map[string]bool{}
	for _, v := range set.Phones {
		sup[v] = true
	}
	return sup, nil
}

// callPhone is the batch's rule for the number lead c's call steps dial, from its org's
// usable facts (UsableOrgFacts: never a number marked wrong): a phone fact that is not
// suppressed, whose call the call rules do not refuse for good (draftCallVerdict; the hours
// and the screening come round, so they never leave a call out). nil with why when the call
// is left out. A country whose call policy is off in the campaign still gets its call
// drafted; the operator decides.
func callPhone(ctx context.Context, lq leadstore.Queries, plan batchPlan, c leadstore.Candidate, facts []leadstore.Fact,
	suppressed map[string]bool, now time.Time) (*leadstore.Fact, string, error) {
	f, ok, why := pickPhone(facts, suppressed)
	if !ok {
		return nil, why, nil
	}
	v, err := draftCallVerdict(ctx, lq, plan, c, f, now)
	if err != nil {
		return nil, "", err
	}
	if v.lasting() {
		return nil, "calls not allowed: " + v.Reason, nil
	}
	return &f, "", nil
}

// --- the batch ---

// batchDrafter decides once per batch how its email leads are written, and counts every
// lead of a writer: claude campaign that went to templates, by why.
type batchDrafter struct {
	on        bool   // Claude writes this batch's email leads
	off       string // why not, when the campaign's writer is claude ("" for a template campaign)
	dc        draftCtx
	writer    *outreach.Writer
	rec       generationRecorder
	approved  map[int64]bool // approved sample generations a batch may reuse
	capped    bool           // today's cap stopped a call: no more calls this batch
	fallbacks map[string]int
	drafted   int // leads Claude's draft went into the batch for
}

// newBatchDrafter reads the campaign's writer and, for claude, its trust gate now: a
// campaign the gate does not trust drafts from templates, whatever its spec says.
func newBatchDrafter(ctx context.Context, lq leadstore.Queries, bs batchSetup, plan batchPlan, campaignID int64) (*batchDrafter, error) {
	d := &batchDrafter{fallbacks: map[string]int{}}
	if plan.spec.Writer != campaign.WriterClaude {
		return d, nil
	}
	st, err := draftGate(ctx, lq, campaignID)
	if err != nil {
		return nil, err
	}
	switch {
	case !st.Trusted:
		d.off = fbUntrusted
	case bs.claude == nil || bs.claude.caller == nil:
		d.off = fbOff
		if bs.claude != nil && bs.claude.off != "" {
			d.off = bs.claude.off
		}
	default:
		ex, approved, err := approvedExamples(ctx, lq, campaignID)
		if err != nil {
			return nil, err
		}
		d.on, d.approved = true, approved
		d.dc = draftCtx{cfg: bs.leads, plan: plan, campaignID: campaignID, examples: ex}
		d.writer = &outreach.Writer{Caller: bs.claude.caller, Log: slog.Default()}
		d.rec = bs.claude.rec
	}
	return d, nil
}

// draft is one lead's rows: Claude's for an email lead when the batch drafts with Claude,
// else (or when Claude gives none) the templates', counted by why. An error drops the lead,
// except a usage-store failure, which stops the batch (stopBatchError).
func (d *batchDrafter) draft(ctx context.Context, lq leadstore.Queries, plan batchPlan, c leadstore.Candidate,
	ev leadstore.Evidence, cfg leadconfig.Config, phone *leadstore.Fact, idDomain string, rnd io.Reader) ([]leadstore.DraftRow, error) {
	if d == nil || (!d.on && d.off == "") || c.Channel != leadconfig.ChannelEmail ||
		c.RouteKind != leadstore.RouteEmail || c.Route == "" {
		return draftSequence(plan, c, ev, cfg, phone, idDomain, rnd)
	}
	why, detail := d.off, ""
	if d.on {
		rows, reason, more, err := d.claudeRows(ctx, lq, plan, c, ev, phone, idDomain, rnd)
		if err != nil {
			return nil, err
		}
		if reason == "" {
			d.drafted++
			return rows, nil
		}
		why, detail = reason, more
	}
	rows, err := draftSequence(plan, c, ev, cfg, phone, idDomain, rnd)
	if err != nil {
		return nil, fmt.Errorf("%s; no template draft either: %w", cmp.Or(detail, why), err)
	}
	d.fallbacks[why]++
	return rows, nil
}

// stopBatchError is a failure the batch must stop on rather than drop one lead for.
type stopBatchError struct{ err error }

func (e *stopBatchError) Error() string { return e.err.Error() }
func (e *stopBatchError) Unwrap() error { return e.err }

// claudeRows writes lead c with Claude (or reuses a stored generation) and returns its rows.
// reason is set (with detail in plain words) when the lead falls back to templates; err is a
// stopBatchError.
func (d *batchDrafter) claudeRows(ctx context.Context, lq leadstore.Queries, plan batchPlan, c leadstore.Candidate,
	ev leadstore.Evidence, phone *leadstore.Fact, idDomain string, rnd io.Reader) (rows []leadstore.DraftRow, reason, detail string, err error) {
	in, err := d.dc.writeInput(ctx, lq, plan, c, ev, phone != nil)
	if err != nil {
		return nil, fbNoInput, fbNoInput + ": " + err.Error(), nil
	}
	genID, res, ok, err := d.reuse(ctx, lq, c.LeadID, in)
	if err != nil {
		return nil, "", "", &stopBatchError{err}
	}
	if !ok {
		if d.capped {
			return nil, fbCap, "", nil
		}
		var werr error
		res, werr = d.writer.WriteLead(ctx, in)
		ids, rerr := recordAttempts(ctx, d.rec, c.LeadID, d.dc.campaignID, in.Sheet, res.Attempts, leadstore.PurposeBatch)
		if rerr != nil {
			return nil, "", "", &stopBatchError{rerr}
		}
		switch {
		case werr != nil && errors.Is(werr, ai.ErrUsageStore):
			return nil, "", "", &stopBatchError{werr}
		case werr != nil && errors.Is(werr, ai.ErrSpendCap):
			r := capReason(werr)
			d.capped = d.capped || r == fbCap
			return nil, r, "", nil
		case werr != nil:
			return nil, fbUnavailable, fbUnavailable + ": " + werr.Error(), nil
		case res.Fallback != "":
			return nil, fbRefused, res.Fallback, nil
		}
		genID = ids[len(ids)-1]
	}
	rows, err = claudeDraftRows(plan, c, in, res, genID, phone, idDomain, rnd)
	if err != nil {
		return nil, fbRefused, "Draft refused: " + err.Error(), nil
	}
	return rows, "", "", nil
}

// reuse finds a stored generation for the lead on the same fact sheet that a batch may send
// instead of asking again: one drafted for a batch (a discarded batch's), or a sampled draft
// the operator approved; never a fake decider's, never a sample not approved. It is checked
// and rendered again under today's rules (a replay, no call); one that no longer passes is
// skipped.
func (d *batchDrafter) reuse(ctx context.Context, lq leadstore.Queries, leadID int64, in outreach.WriteInput) (int64, outreach.WriteResult, bool, error) {
	gens, err := lq.GenerationsForLead(ctx, leadID, d.dc.campaignID)
	if err != nil {
		return 0, outreach.WriteResult{}, false, err
	}
	sha := in.Sheet.SHA256()
	for _, g := range gens {
		if !g.CheckOK || g.Output == nil || g.FactSHA256 != sha || g.Model == claude.FakeModel {
			continue
		}
		if g.Purpose != leadstore.PurposeBatch && (g.Purpose != leadstore.PurposeSample || !d.approved[g.ID]) {
			continue
		}
		res, err := (&outreach.Writer{Caller: replayCaller{out: g.Output}}).WriteLead(ctx, in)
		if err == nil && res.Written != nil {
			return g.ID, res, true, nil
		}
	}
	return 0, outreach.WriteResult{}, false, nil
}

// replayCaller answers every Write with a stored answer, unbilled: a reuse or a fake eval
// runs the stored draft through the same check and rendering as a fresh one. A generation is
// stored as a WrittenDraft, so it is answered back in the wire shape the model answers in; one
// that no longer decodes is a bad answer (a failed attempt), never a crash.
type replayCaller struct{ out json.RawMessage }

func (r replayCaller) Write(context.Context, claude.WriteRequest) (claude.WriteResult, claude.Usage, error) {
	d, err := outreach.DecodeWrittenDraft(r.out)
	if err != nil {
		return claude.WriteResult{}, claude.Usage{Model: claude.FakeModel}, fmt.Errorf("replay: %w: %w", ai.ErrBadAnswer, err)
	}
	b, err := outreach.EncodeWireDraft(d)
	if err != nil {
		return claude.WriteResult{}, claude.Usage{Model: claude.FakeModel}, fmt.Errorf("replay: %w: %w", ai.ErrBadAnswer, err)
	}
	return claude.WriteResult{JSON: b}, claude.Usage{Model: claude.FakeModel}, nil
}

// claudeSlots is outreach_message.slots for a message Claude wrote: the generation it came
// from, every fact its paragraphs cite with the row each came from, and each paragraph's cites
// (talking points for a call row), so the batch page can show each claim with its source.
type claudeSlots struct {
	Writer       string               `json:"writer"`
	GenerationID int64                `json:"generation_id"`
	Cites        map[string]citedFact `json:"cites"`
	Paragraphs   []paragraphCites     `json:"paragraphs"`
}

type citedFact struct {
	Label string `json:"label"`
	Value string `json:"value"`
	Table string `json:"table"`
	ID    int64  `json:"id,omitempty"`
}

type paragraphCites struct {
	Cites  []string `json:"cites"`
	PainID string   `json:"pain_id,omitempty"`
}

func claudeSlotsJSON(genID int64, sheet outreach.FactSheet, paras []paragraphCites) (json.RawMessage, error) {
	byID := map[string]outreach.Fact{}
	for _, f := range sheet.Facts {
		byID[f.ID] = f
	}
	s := claudeSlots{Writer: campaign.WriterClaude, GenerationID: genID, Cites: map[string]citedFact{}, Paragraphs: paras}
	for _, p := range paras {
		for _, id := range p.Cites {
			if f, ok := byID[id]; ok {
				s.Cites[id] = citedFact{Label: f.Label, Value: f.Value, Table: f.Source.Table, ID: f.Source.ID}
			}
		}
	}
	b, err := json.Marshal(s)
	if err != nil {
		return nil, fmt.Errorf("leads: claude slots: %w", err)
	}
	return b, nil
}

// claudeDraftRows is a written lead's rows, in the plan's sequence order as draftSequence
// makes them: each email step the rendered, checked draft (threaded to the first email), the
// call step the talking points as the call card.
func claudeDraftRows(plan batchPlan, c leadstore.Candidate, in outreach.WriteInput, res outreach.WriteResult, genID int64,
	phone *leadstore.Fact, idDomain string, rnd io.Reader) ([]leadstore.DraftRow, error) {
	if res.Written == nil {
		return nil, errors.New("no grounded draft")
	}
	emails := map[int]outreach.Draft{}
	for _, e := range res.Emails {
		emails[e.Step] = e.Draft
	}
	written := map[int]outreach.WrittenStep{}
	for _, st := range res.Written.Steps {
		written[st.Step] = st
	}
	var out []leadstore.DraftRow
	first := ""
	for _, st := range plan.seq {
		if stepNeedsPhone(st) && phone == nil {
			continue
		}
		when := st.When
		if when == campaign.WhenAlways {
			when = ""
		}
		switch st.Type {
		case campaign.StepEmail:
			dr, ok := emails[st.Step]
			if !ok {
				return nil, fmt.Errorf("email step %d is missing", st.Step)
			}
			var paras []paragraphCites
			for _, p := range written[st.Step].Paragraphs {
				paras = append(paras, paragraphCites{Cites: slices.Clone(p.Cites), PainID: p.PainID})
			}
			slots, err := claudeSlotsJSON(genID, in.Sheet, paras)
			if err != nil {
				return nil, err
			}
			row := leadstore.DraftRow{LeadID: c.LeadID, Step: st.Step, Channel: "email", To: emailRecipient(when, c), Subject: dr.Subject,
				Body: dr.Body, Slots: slots, Condition: when}
			if row.MessageID, err = outreach.NewMessageID(idDomain, rnd); err != nil {
				return nil, err
			}
			if first == "" {
				first = row.MessageID
			} else {
				row.InReplyTo = first
			}
			out = append(out, row)
		case campaign.StepCall:
			if res.CallCard == nil {
				return nil, fmt.Errorf("call step %d has no talking points", st.Step)
			}
			if n := len(leadconfig.CallLines(res.CallCard.Body)); n != leadconfig.CallPoints {
				return nil, fmt.Errorf("call card step %d: %d talking points, want %d", st.Step, n, leadconfig.CallPoints)
			}
			var paras []paragraphCites
			for _, tp := range res.Written.TalkingPoints {
				paras = append(paras, paragraphCites{Cites: slices.Clone(tp.Cites)})
			}
			slots, err := claudeSlotsJSON(genID, in.Sheet, paras)
			if err != nil {
				return nil, err
			}
			out = append(out, leadstore.DraftRow{LeadID: c.LeadID, Step: st.Step, Channel: leadstore.ChannelPhone,
				Subject: "Call " + cmpOr(in.Render.Slots.OrgName.Value, c.Domain), Body: res.CallCard.Body, Slots: slots,
				Condition: when, PhoneFactID: phone.ID})
		}
	}
	return out, nil
}

// fallbackLine is the batch's word on its leads drafted from templates in a writer: claude
// campaign: "3 leads drafted from templates: today's Claude cap reached (2), draft refused
// (1)". "" when there were none.
func fallbackLine(fb map[string]int) string {
	total := 0
	type kv struct {
		k string
		n int
	}
	var list []kv
	for k, n := range fb {
		total += n
		list = append(list, kv{k, n})
	}
	if total == 0 {
		return ""
	}
	slices.SortFunc(list, func(a, b kv) int { return cmp.Or(cmp.Compare(b.n, a.n), strings.Compare(a.k, b.k)) })
	parts := make([]string, len(list))
	for i, x := range list {
		parts[i] = fmt.Sprintf("%s (%d)", x.k, x.n)
	}
	return fmt.Sprintf("%s drafted from templates: %s", plural(total, "lead", "leads"), strings.Join(parts, ", "))
}

// --- the fake decider ---

// draftFake is the fake decider's Caller: it reads the lead's facts, pains and steps back
// out of the user turn and answers with fakeDraft, unbilled. It lets the sample and the
// Drafts tab be walked without a key; it is never Claude, so its drafts never count toward
// the gate, never become examples and are never reused by a batch.
type draftFake struct{}

func (draftFake) Write(_ context.Context, r claude.WriteRequest) (claude.WriteResult, claude.Usage, error) {
	sheet, pains, steps, call, err := parseUserTurn(r.Prompt)
	if err != nil {
		return claude.WriteResult{}, claude.Usage{}, fmt.Errorf("claude fake: %w: %w", ai.ErrBadAnswer, err)
	}
	b, err := outreach.EncodeWireDraft(fakeDraft(sheet, pains, steps, call))
	if err != nil {
		return claude.WriteResult{}, claude.Usage{}, err
	}
	return claude.WriteResult{JSON: b}, claude.Usage{Model: claude.FakeModel}, nil
}

// parseUserTurn reads outreach.UserTurn back: the fact sheet, the allowed pains, the email
// steps and whether talking points are asked for. A retry's added text is ignored.
func parseUserTurn(p string) (outreach.FactSheet, []outreach.Pain, []int, bool, error) {
	var (
		sheet   outreach.FactSheet
		pains   []outreach.Pain
		steps   []int
		call    bool
		section string
	)
	sc := bufio.NewScanner(strings.NewReader(p))
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "Email steps to write: "):
			for _, s := range strings.Split(strings.TrimPrefix(line, "Email steps to write: "), ",") {
				n, err := strconv.Atoi(strings.TrimSpace(s))
				if err != nil {
					return sheet, nil, nil, false, fmt.Errorf("steps %q", line)
				}
				steps = append(steps, n)
			}
		case strings.HasPrefix(line, "Talking points: "):
			call = !strings.HasPrefix(line, "Talking points: none")
		case strings.HasPrefix(line, "Pains this lead's signals allow"):
			section = "pains"
		case strings.HasPrefix(line, "Fact sheet ("):
			section = "facts"
		case strings.TrimSpace(line) == "":
			if section == "facts" {
				return sheet, pains, steps, call, nil
			}
			section = ""
		case section == "pains" && strings.HasPrefix(line, "- "):
			id, text, ok := strings.Cut(strings.TrimPrefix(line, "- "), ": ")
			if ok {
				pains = append(pains, outreach.Pain{ID: id, Text: text})
			}
		case section == "facts":
			var f struct {
				ID, Label, Value string
				Group            outreach.FactGroup
				ContextOnly      bool `json:"context_only"`
			}
			if err := json.Unmarshal([]byte(line), &f); err != nil {
				return sheet, nil, nil, false, fmt.Errorf("fact line: %w", err)
			}
			sheet.Facts = append(sheet.Facts, outreach.Fact{ID: f.ID, Label: f.Label, Value: f.Value, Group: f.Group, EchoGuard: f.ContextOnly})
		}
	}
	return sheet, pains, steps, call, sc.Err()
}

// fakeDraft is a plain draft built only from the sheet and the allowed pains, shaped as the
// prompt asks: step 0 opens with the sender's name, then up to three pains as given, the
// proof line verbatim, the report and the call to action; each follow-up the report and the
// call to action; three talking points when the sequence has a call.
func fakeDraft(sheet outreach.FactSheet, pains []outreach.Pain, steps []int, call bool) outreach.WrittenDraft {
	fact := func(label string) *outreach.Fact {
		for i, f := range sheet.Facts {
			if f.Label == label && !f.EchoGuard {
				return &sheet.Facts[i]
			}
		}
		return nil
	}
	para := func(text string, f *outreach.Fact) outreach.Paragraph {
		return outreach.Paragraph{Text: text, Cites: []string{f.ID}}
	}
	var opener, offer, cta *outreach.Paragraph
	if f := fact(outreach.LabelSenderName); f != nil {
		p := para("I'm "+f.Value+".", f)
		opener = &p
	} else if f := fact(outreach.LabelIdentityLine); f != nil {
		p := para(f.Value, f)
		opener = &p
	}
	subject := "A short note"
	if f := fact(outreach.LabelReport); f != nil {
		p := para("I can write you a "+f.Value+". It is yours to keep.", f)
		offer, subject = &p, "A "+f.Value
	}
	if f := fact(outreach.LabelCallToAction); f != nil {
		p := para(f.Value, f)
		cta = &p
	}
	add := func(list []outreach.Paragraph, ps ...*outreach.Paragraph) []outreach.Paragraph {
		for _, p := range ps {
			if p != nil {
				list = append(list, *p)
			}
		}
		return list
	}
	var d outreach.WrittenDraft
	sorted := slices.Clone(steps)
	slices.Sort(sorted)
	for _, s := range sorted {
		st := outreach.WrittenStep{Step: s}
		if s == sorted[0] {
			st.Subject = subject
			st.Paragraphs = add(st.Paragraphs, opener)
			for _, p := range pains[:min(3, len(pains))] {
				st.Paragraphs = append(st.Paragraphs, outreach.Paragraph{Text: p.Text, Cites: []string{}, PainID: p.ID})
			}
			if f := fact(outreach.LabelProofLine); f != nil {
				st.Paragraphs = append(st.Paragraphs, para(f.Value, f))
			}
		}
		st.Paragraphs = add(st.Paragraphs, offer, cta)
		if len(st.Paragraphs) == 0 {
			st.Paragraphs = add(st.Paragraphs, opener)
		}
		d.Steps = append(d.Steps, st)
	}
	d.TalkingPoints = []outreach.TalkingPoint{}
	if call {
		var tps []outreach.TalkingPoint
		for _, p := range add(nil, opener, offer, cta) {
			tps = append(tps, outreach.TalkingPoint{Text: p.Text, Cites: p.Cites})
		}
		for len(tps) > 0 && len(tps) < outreach.TalkingPoints {
			tps = append(tps, tps[0])
		}
		d.TalkingPoints = append(d.TalkingPoints, tps...)
	}
	return d
}

// batchClaude is what `leads batch new` drafts campaign c with: nil for a template campaign;
// for writer: claude the capped drafting caller, or, when live calls are off or the key or
// caps cannot be read, no caller and the batch drafts from templates (a batch on templates
// never needs the key), saying why once.
func batchClaude(c leadstore.Campaign, s *store.Store) *claudeDrafting {
	var spec campaign.Spec
	if len(c.Spec) == 0 || json.Unmarshal(c.Spec, &spec) != nil || spec.Writer != campaign.WriterClaude {
		return nil
	}
	rec := leadstore.NewQueries(s.Pool())
	caller, err := newDraftCaller("leads batch new", s.Q(), claude.PurposeDrafting)
	if err != nil {
		_, _ = fmt.Fprintf(stdout, "Claude drafting unavailable (%v): drafting from templates.\n", err)
		return &claudeDrafting{off: fbUnavailable, rec: rec}
	}
	return &claudeDrafting{caller: caller, rec: rec}
}

// campaignGates reads the draft gate for `campaigns check` and `register`, lazily: only a
// spec whose writer is claude asks, and each slug asked is remembered with its verdict, so a
// refusal can say why. A nil *campaignGates keeps every gate closed (no database).
type campaignGates struct {
	ctx context.Context
	lq  leadstore.Queries
	// open, when set, opens the store on the first ask (check needs no database otherwise);
	// close closes what it opened.
	open   func(context.Context) (*store.Store, error)
	opened *store.Store
	ready  bool
	asked  []string
	states map[string]outreach.GateState
	errs   map[string]error
}

func newCampaignGates(ctx context.Context, lq leadstore.Queries) *campaignGates {
	return &campaignGates{ctx: ctx, lq: lq, ready: true, states: map[string]outreach.GateState{}, errs: map[string]error{}}
}

// lazyCampaignGates opens the store with open only when a writer: claude spec asks.
func lazyCampaignGates(ctx context.Context, open func(context.Context) (*store.Store, error)) *campaignGates {
	return &campaignGates{ctx: ctx, open: open, states: map[string]outreach.GateState{}, errs: map[string]error{}}
}

// close closes the store the gates opened, if any.
func (g *campaignGates) close() {
	if g != nil && g.opened != nil {
		g.opened.Close()
	}
}

// trust is campaign.LoadOptions.DraftsTrusted; nil for a nil receiver.
func (g *campaignGates) trust() func(slug string) bool {
	if g == nil {
		return nil
	}
	return func(slug string) bool {
		if !slices.Contains(g.asked, slug) {
			g.asked = append(g.asked, slug)
		}
		if !g.ready {
			s, err := g.open(g.ctx)
			if err != nil {
				g.errs[slug] = fmt.Errorf("the draft gate could not be read: %w", err)
				return false
			}
			g.opened, g.lq, g.ready = s, leadstore.NewQueries(s.Pool()), true
		}
		c, err := g.lq.CampaignBySlug(g.ctx, leadstore.DefaultAccountID, slug)
		if errors.Is(err, leadstore.ErrCampaignNotFound) {
			g.errs[slug] = errors.New("the campaign is not registered yet, so it has no reviewed drafts or eval: register it with writer: template, sample and review drafts, then run the eval")
			return false
		}
		if err == nil {
			var st outreach.GateState
			if st, err = draftGate(g.ctx, g.lq, c.ID); err == nil {
				g.states[slug] = st
				return st.Trusted
			}
		}
		g.errs[slug] = fmt.Errorf("the draft gate could not be read: %w", err)
		return false
	}
}

// explain adds each asked, untrusted campaign's gate to err (a usage error naming the
// problems), one line each.
func (g *campaignGates) explain(err error) error {
	if g == nil {
		if !errors.Is(err, errUsage) || !strings.Contains(err.Error(), "until the draft gate passes") {
			return err
		}
		return usageError("%s\n  (no database: the draft gate counts as closed, so writer: claude is refused)", strings.TrimPrefix(err.Error(), errUsage.Error()+": "))
	}
	var lines []string
	for _, slug := range g.asked {
		if e, ok := g.errs[slug]; ok {
			lines = append(lines, fmt.Sprintf("  %s: Claude drafts: not trusted yet: %v", slug, e))
		} else if st, ok := g.states[slug]; ok && !st.Trusted {
			lines = append(lines, "  "+slug+": "+st.Summary())
		}
	}
	if len(lines) == 0 || !errors.Is(err, errUsage) {
		return err
	}
	return usageError("%s\n%s", strings.TrimPrefix(err.Error(), errUsage.Error()+": "), strings.Join(lines, "\n"))
}
