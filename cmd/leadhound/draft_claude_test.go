package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gremy/webbership-leadhound/internal/ai"
	"github.com/gremy/webbership-leadhound/internal/campaign"
	"github.com/gremy/webbership-leadhound/internal/claude"
	"github.com/gremy/webbership-leadhound/internal/leadconfig"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/outreach"
)

// draftStore is reportStore over one fictional lead.
type draftStore struct {
	lead leadstore.Lead
	org  leadstore.Org
	ev   leadstore.Evidence
	// The operator's sector for the org, the proofs ProofsFor answers with and what it was asked.
	sector string
	proofs []leadstore.Proof
	asked  *proofAsk
}

type proofAsk struct {
	limit  int
	sector string
}

func (s draftStore) LeadByID(context.Context, int64) (leadstore.Lead, error) { return s.lead, nil }
func (s draftStore) OrgByID(context.Context, int64) (leadstore.Org, error)   { return s.org, nil }
func (s draftStore) LeadEvidence(context.Context, []int64) (map[int64]leadstore.Evidence, error) {
	return map[int64]leadstore.Evidence{s.lead.ID: s.ev}, nil
}
func (s draftStore) Campaigns(context.Context, int64) ([]leadstore.Campaign, error) { return nil, nil }
func (s draftStore) OperatorSector(context.Context, int64) (string, error)          { return s.sector, nil }
func (s draftStore) ProofsFor(_ context.Context, _ int64, sector string, limit int) ([]leadstore.Proof, error) {
	if s.asked != nil {
		s.asked.limit, s.asked.sector = limit, sector
	}
	return s.proofs, nil
}

// draftWorld is the example campaign's plan and one d7 lead with its candidate and store.
type draftWorld struct {
	cfg  leadconfig.Config
	plan batchPlan
	rs   draftStore
	c    leadstore.Candidate
}

func newDraftWorld(t *testing.T, kind string, major int) draftWorld {
	t.Helper()
	cfg, err := leadconfig.Load(examplesDir)
	if err != nil {
		t.Fatal(err)
	}
	spec, err := campaign.Load(filepath.Join(examplesDir, campaign.Dir, "drupal-eol", campaign.FileName))
	if err != nil {
		t.Fatal(err)
	}
	plan, err := filePlan(examplesDir, spec, cfg)
	if err != nil {
		t.Fatal(err)
	}
	probe := int64(11)
	ev := leadstore.Evidence{LeadID: 42, ProbeID: &probe, FinalHost: "northfield.example", Major: major,
		Signals: []leadstore.EvidenceSignal{{Signal: leadstore.Signal{Signal: "meta-generator", Value: fmt.Sprint(major)},
			Fetch: leadstore.EvidenceFetch{ID: 5, URL: "https://northfield.example/"}}}}
	rs := draftStore{
		lead: leadstore.Lead{ID: 42, CampaignID: 1, OrgID: 7, Kind: kind, ProbeID: &probe, Channel: leadconfig.ChannelEmail},
		org:  leadstore.Org{ID: 7, Domain: "northfield.example", Name: "Northfield Museum Trust", Country: "GB"},
		ev:   ev,
	}
	c := leadstore.Candidate{LeadID: 42, OrgID: 7, Domain: "northfield.example", OrgName: "Northfield Museum Trust",
		Country: "GB", Kind: kind, Channel: leadconfig.ChannelEmail, RouteKind: leadstore.RouteEmail,
		Route: "info@northfield.example", ProbeID: &probe, Major: major}
	return draftWorld{cfg: cfg, plan: plan, rs: rs, c: c}
}

func (w draftWorld) input(t *testing.T, phone bool) outreach.WriteInput {
	t.Helper()
	in, err := draftCtx{cfg: w.cfg, plan: w.plan, campaignID: 1}.writeInput(context.Background(), w.rs, w.plan, w.c, w.rs.ev, phone)
	if err != nil {
		t.Fatal(err)
	}
	return in
}

// TestDraftsFakeDeciderGrounds: the fake decider's draft for the example campaign passes the
// whole grounding check and the render tail, for each kind and with or without a number; the
// steps asked for follow the number (no "tried to call" email without one).
func TestDraftsFakeDeciderGrounds(t *testing.T) {
	for _, tc := range []struct {
		kind  string
		major int
		phone bool
		steps string
	}{
		{"unsupported", 7, false, "0,1,4"},
		{"unsupported", 7, true, "0,1,3,4"},
		{"d10", 10, true, "0,1,3,4"},
	} {
		w := newDraftWorld(t, tc.kind, tc.major)
		in := w.input(t, tc.phone)
		if got := strings.Trim(strings.Join(strings.Fields(fmt.Sprint(in.Rules.EmailSteps)), ","), "[]"); got != tc.steps {
			t.Fatalf("%s phone %v: steps %s, want %s", tc.kind, tc.phone, got, tc.steps)
		}
		if in.Rules.CallStep != tc.phone || in.Render.Greeting != "Hello," || in.Sheet.WithLabel(outreach.LabelCallToAction) == nil {
			t.Fatalf("%s: call %v greeting %q, CTA fact %v", tc.kind, in.Rules.CallStep, in.Render.Greeting, in.Sheet.WithLabel(outreach.LabelCallToAction))
		}
		res, err := (&outreach.Writer{Caller: draftFake{}}).WriteLead(context.Background(), in)
		if err != nil {
			t.Fatal(err)
		}
		if res.Written == nil || len(res.Attempts) != 1 {
			t.Fatalf("%s phone %v: fallback %q attempts %+v", tc.kind, tc.phone, res.Fallback, res.Attempts)
		}
		if tc.phone != (res.CallCard != nil) {
			t.Fatalf("call card %v, want %v", res.CallCard != nil, tc.phone)
		}
	}
}

// TestDraftsParseUserTurnRoundTrip: the fake reads back exactly the sheet, pains and steps
// the user turn holds.
func TestDraftsParseUserTurnRoundTrip(t *testing.T) {
	in := newDraftWorld(t, "unsupported", 7).input(t, true)
	sheet, pains, steps, call, err := parseUserTurn(outreach.UserTurn(in) + "\n\nYour previous answer was refused.\n- G3 …")
	if err != nil {
		t.Fatal(err)
	}
	if len(sheet.Facts) != len(in.Sheet.Facts) || len(pains) != len(in.Rules.Pains) || !call || fmt.Sprint(steps) != fmt.Sprint(in.Rules.EmailSteps) {
		t.Fatalf("facts %d/%d pains %d/%d steps %v call %v", len(sheet.Facts), len(in.Sheet.Facts), len(pains), len(in.Rules.Pains), steps, call)
	}
	for i, f := range sheet.Facts {
		w := in.Sheet.Facts[i]
		if f.ID != w.ID || f.Value != w.Value || f.Label != w.Label || f.Group != w.Group || f.EchoGuard != w.EchoGuard {
			t.Fatalf("fact %d: %+v, want %+v", i, f, w)
		}
	}
}

// TestDraftsContractDriftBatchTail: at the batch level, a lead's Claude rows and its template
// rows share one render tail: the same greeting, footer, identity and opt-out lines below the
// call to action, the same threading and the same sequence shape.
func TestDraftsContractDriftBatchTail(t *testing.T) {
	w := newDraftWorld(t, "unsupported", 7)
	in := w.input(t, false)
	res, err := (&outreach.Writer{Caller: draftFake{}}).WriteLead(context.Background(), in)
	if err != nil || res.Written == nil {
		t.Fatalf("write: %v %q", err, res.Fallback)
	}
	rnd := bytes.NewReader(bytes.Repeat([]byte{7}, 4096))
	cl, err := claudeDraftRows(w.plan, w.c, in, res, 99, nil, "sender.example", rnd)
	if err != nil {
		t.Fatal(err)
	}
	tm, err := draftSequence(w.plan, w.c, w.rs.ev, w.cfg, nil, "sender.example", bytes.NewReader(bytes.Repeat([]byte{7}, 4096)))
	if err != nil {
		t.Fatal(err)
	}
	if len(cl) != len(tm) {
		t.Fatalf("claude %d rows, templates %d", len(cl), len(tm))
	}
	const cta = "Worth 10 minutes to find out?"
	for i := range cl {
		if cl[i].Step != tm[i].Step || cl[i].Channel != tm[i].Channel || cl[i].Condition != tm[i].Condition || cl[i].To != tm[i].To ||
			(cl[i].InReplyTo == "") != (tm[i].InReplyTo == "") {
			t.Fatalf("row %d shape: claude %+v templates %+v", i, cl[i], tm[i])
		}
		if !strings.HasPrefix(cl[i].Body, "Hello,\n\n") || !strings.HasPrefix(tm[i].Body, "Hello") {
			t.Fatalf("row %d greeting differs", i)
		}
		common := commonSuffix(cl[i].Body, tm[i].Body)
		if !strings.Contains(common, w.cfg.Sender.IdentityLine) || !strings.Contains(common, in.Render.Slots.OptOutLine.Value) {
			t.Fatalf("step %d: the shared tail lacks the identity or opt-out line:\n%s", cl[i].Step, common)
		}
		if i == 0 && !strings.HasPrefix(strings.TrimSpace(common), cta) {
			t.Fatalf("step 0 tails differ above the footer:\n%s", common)
		}
		var s claudeSlots
		if err := json.Unmarshal(cl[i].Slots, &s); err != nil || s.Writer != campaign.WriterClaude || s.GenerationID != 99 || len(s.Cites) == 0 {
			t.Fatalf("row %d slots %s (%v)", i, cl[i].Slots, err)
		}
	}
}

// commonSuffix is the longest shared ending of a and b, from a line start.
func commonSuffix(a, b string) string {
	n := 0
	for n < len(a) && n < len(b) && a[len(a)-1-n] == b[len(b)-1-n] {
		n++
	}
	s := a[len(a)-n:]
	if i := strings.Index(s, "\n"); i >= 0 && n < len(a) && a[len(a)-n-1] != '\n' {
		s = s[i+1:]
	}
	return s
}

type recordedGen struct{ rows []leadstore.GenerationRow }

func (r *recordedGen) RecordGeneration(_ context.Context, g leadstore.GenerationRow) (int64, error) {
	r.rows = append(r.rows, g)
	return int64(len(r.rows)), nil
}

// TestDraftsRecordAttemptsStop: a stopped attempt (refusal) is stored as failed with a stop
// failure naming why, billed; a grounded one as passing.
func TestDraftsRecordAttemptsStop(t *testing.T) {
	rec := &recordedGen{}
	attempts := []outreach.Attempt{
		{StopReason: outreach.StopRefusal, Model: claude.ModelSonnet, CostMicros: 1200},
		{StopReason: outreach.StopEndTurn, Model: claude.ModelSonnet, Output: json.RawMessage(`{}`), CostMicros: 900},
	}
	ids, err := recordAttempts(context.Background(), rec, 4, 1, outreach.FactSheet{}, attempts, leadstore.PurposeBatch)
	if err != nil || len(ids) != 2 {
		t.Fatalf("ids %v err %v", ids, err)
	}
	a, b := rec.rows[0], rec.rows[1]
	if a.CheckOK || len(a.Failures) != 1 || a.Failures[0].Code != failStop || a.Failures[0].Token != outreach.StopRefusal || *a.CostMicros != 1200 || a.Attempt != 1 {
		t.Fatalf("refusal row %+v", a)
	}
	if !b.CheckOK || len(b.Failures) != 0 || b.Attempt != 2 || b.Purpose != leadstore.PurposeBatch {
		t.Fatalf("passing row %+v", b)
	}
}

func TestDraftsFallbackWords(t *testing.T) {
	if got := fallbackLine(map[string]int{fbCap: 3, fbRefused: 1}); got != "4 leads drafted from templates: today's Claude cap reached (3), draft refused (1)" {
		t.Fatalf("line %q", got)
	}
	if fallbackLine(nil) != "" {
		t.Fatal("no fallbacks, no line")
	}
	day := fmt.Errorf("claude: drafting: $2.9 spent of $3 on 2026-10-28, next call up to $0.1: %w", ai.ErrSpendCap)
	lead := fmt.Errorf("claude: drafting: lead spent $0.07 of $0.08, next call up to $0.04: %w", ai.ErrSpendCap)
	if capReason(day) != fbCap || capReason(lead) != fbLeadCap {
		t.Fatalf("cap reasons %q %q", capReason(day), capReason(lead))
	}
	if !errors.Is(&stopBatchError{ai.ErrUsageStore}, ai.ErrUsageStore) {
		t.Fatal("stopBatchError unwraps")
	}
}

// TestDraftsSystemPromptOneCacheEntry: every lead of a campaign gets the same system prompt
// bytes (one prompt-cache entry), whatever its kind (d7, d9, d10, agency), its number and its
// country's sequence (a call-first country writes other steps with other word limits); what
// varies is the user turn's and the schema's.
func TestDraftsSystemPromptOneCacheEntry(t *testing.T) {
	callFirstTable(t)
	var first string
	turns := map[string]bool{}
	for _, tc := range []struct {
		kind    string
		major   int
		phone   bool
		country string
	}{
		{"unsupported", 7, false, "GB"},
		{"unsupported", 7, true, "GB"},
		{"unsupported", 9, true, "GB"},
		{"d10", 10, true, "GB"},
		{"d10", 10, false, "GB"},
		{"agency_partner", 7, true, "GB"},
		{"unsupported", 7, true, "RO"},
	} {
		w := newDraftWorld(t, tc.kind, tc.major)
		w.c.Country, w.rs.org.Country = tc.country, tc.country
		if _, ok := w.cfg.Countries[tc.country]; !ok {
			w.cfg.Countries[tc.country] = w.cfg.Countries["GB"]
		}
		plan, _ := w.plan.forLead(tc.country)
		in, err := draftCtx{cfg: w.cfg, plan: plan, campaignID: 1}.writeInput(context.Background(), w.rs, plan, w.c, w.rs.ev, tc.phone)
		if err != nil {
			t.Fatalf("%+v: %v", tc, err)
		}
		sys := outreach.SystemPrompt(in)
		if first == "" {
			first = sys
			t.Logf("system prompt: %d bytes", len(sys))
		}
		if sys != first {
			t.Fatalf("%+v: the system prompt differs from the first lead's", tc)
		}
		u := outreach.UserTurn(in)
		if (tc.country == "RO") != strings.Contains(u, "the first email after a call") {
			t.Fatalf("%+v: call-first role wrong:\n%s", tc, u)
		}
		turns[u] = true
	}
	if len(turns) < 5 {
		t.Fatalf("only %d distinct user turns: per-lead content is not in the user turn", len(turns))
	}
}

// TestDraftsReplayStoredGeneration: a stored generation (kept as a WrittenDraft) replays
// through the writer, which reads the answer in the wire shape: a batch can reuse it unbilled.
// A stored answer that no longer decodes is a failed attempt, not a crash.
func TestDraftsReplayStoredGeneration(t *testing.T) {
	in := newDraftWorld(t, "unsupported", 7).input(t, true)
	first, err := (&outreach.Writer{Caller: draftFake{}}).WriteLead(context.Background(), in)
	if err != nil || first.Written == nil {
		t.Fatalf("fake draft: %v %q", err, first.Fallback)
	}
	stored := first.Attempts[0].Output
	if _, err := outreach.DecodeWrittenDraft(stored); err != nil {
		t.Fatalf("the stored output is not a WrittenDraft: %v", err)
	}
	again, err := (&outreach.Writer{Caller: replayCaller{out: stored}}).WriteLead(context.Background(), in)
	if err != nil || again.Written == nil || len(again.Emails) != len(first.Emails) {
		t.Fatalf("replay: %v %q", err, again.Fallback)
	}
	bad, err := (&outreach.Writer{Caller: replayCaller{out: []byte(`{"steps":{}}`)}}).WriteLead(context.Background(), in)
	if err != nil || bad.Written != nil || bad.Fallback == "" {
		t.Fatalf("a stored answer that does not decode: %v %+v", err, bad)
	}
}

// proofWorld is a lead whose operator sector has two proofs: a public one with a page and a private
// one with a URL that must go nowhere.
func proofWorld(t *testing.T) draftWorld {
	t.Helper()
	w := newDraftWorld(t, "unsupported", 7)
	w.rs.sector, w.rs.asked = "education", &proofAsk{}
	w.rs.proofs = []leadstore.Proof{
		{ID: 2, Line: "I rebuilt a university site.", URL: "https://work.example/university", Public: true},
		{ID: 1, Line: "I moved a college to a current version.", URL: "https://secret.example/college"},
	}
	return w
}

// TestDraftsProofsPerLead: the sheet carries both proofs of the operator's sector; the public page
// is allowed for this lead alone, on a copy of the plan's links; the private URL goes nowhere; the
// store is asked for the sector and outreach.MaxProofFacts.
func TestDraftsProofsPerLead(t *testing.T) {
	w := proofWorld(t)
	shared := len(w.plan.rules.Links)
	w.plan.rules.Links = append(make([]string, 0, shared+4), w.plan.rules.Links...)
	in := w.input(t, false)
	if len(in.Sheet.WithLabel(outreach.LabelProofLine)) != 2 {
		t.Fatalf("sheet proofs %+v", in.Sheet.WithLabel(outreach.LabelProofLine))
	}
	if w.rs.asked.sector != "education" || w.rs.asked.limit != outreach.MaxProofFacts {
		t.Fatalf("asked %+v", *w.rs.asked)
	}
	if !slices.Contains(in.Rules.Links, "https://work.example/university") || slices.Contains(in.Rules.Links, "https://secret.example/college") {
		t.Fatalf("links %v", in.Rules.Links)
	}
	if got := w.plan.rules.Links[:cap(w.plan.rules.Links)]; slices.Contains(got, "https://work.example/university") {
		t.Fatalf("the proof page landed on the plan's shared links: %v", got)
	}
	for _, f := range in.Sheet.Facts {
		if strings.Contains(f.Value, "secret.example") {
			t.Fatalf("the private URL is on the sheet: %+v", f)
		}
	}
}

// TestDraftsFakeWithTwoProofs: with two proof facts the fake still writes one, cited and verbatim,
// and it passes every check on each road. The manual texts come back with Claude's writer word.
func TestDraftsFakeWithTwoProofs(t *testing.T) {
	w := proofWorld(t)
	in := w.input(t, false)
	res, err := (&outreach.Writer{Caller: draftFake{}}).WriteLead(context.Background(), in)
	if err != nil || res.Written == nil {
		t.Fatalf("email: %v %q %+v", err, res.Fallback, res.Attempts)
	}
	proofs := 0
	for _, p := range res.Written.Steps[0].Paragraphs {
		if strings.HasPrefix(p.Text, "I rebuilt") || strings.HasPrefix(p.Text, "I moved") {
			proofs++
		}
	}
	if proofs != 1 {
		t.Fatalf("the first email holds %d proofs, want 1", proofs)
	}
	for _, channel := range []string{leadconfig.ChannelForm, leadconfig.ChannelLinkedIn} {
		m := in
		m.Examples = nil
		res, err := (&outreach.Writer{Caller: draftFake{}}).WriteManual(context.Background(), outreach.ManualInput{WriteInput: m, Channel: channel, NoteMax: w.cfg.Sender.LinkedInNoteMax})
		if err != nil || res.Written == nil {
			t.Fatalf("%s: %v %q %+v", channel, err, res.Fallback, res.Attempts)
		}
		for _, d := range res.Drafts {
			if d.Writer != outreach.ManualWriterClaude {
				t.Fatalf("%s writer %q", channel, d.Writer)
			}
		}
	}
}

// uncitedCaller answers with the fake's draft plus a follow-up paragraph that rests on nothing.
type uncitedCaller struct{}

func (uncitedCaller) Write(_ context.Context, r claude.WriteRequest) (claude.WriteResult, claude.Usage, error) {
	sheet, pains, steps, call, err := parseUserTurn(r.Prompt)
	if err != nil {
		return claude.WriteResult{}, claude.Usage{}, err
	}
	d := fakeDraft(sheet, pains, steps, call)
	last := &d.Steps[len(d.Steps)-1]
	last.Paragraphs = append(last.Paragraphs, outreach.Paragraph{Text: "A short plain line.", Cites: []string{}})
	b, err := outreach.EncodeWireDraft(d)
	return claude.WriteResult{JSON: b}, claude.Usage{Model: claude.FakeModel}, err
}

// TestDraftsUncitedOnlyWithVersions: an email with no operator versions in its prompt keeps today's
// behaviour, an uncited paragraph is not failed; with versions present the same answer fails, so
// copying the operator's plain text cannot pass.
func TestDraftsUncitedOnlyWithVersions(t *testing.T) {
	in := newDraftWorld(t, "unsupported", 7).input(t, false)
	res, err := (&outreach.Writer{Caller: uncitedCaller{}}).WriteLead(context.Background(), in)
	if err != nil || res.Written == nil {
		t.Fatalf("no versions: %v %q %+v", err, res.Fallback, res.Attempts)
	}
	in.OperatorVersions = []outreach.OperatorVersion{{Body: "My own words."}}
	res, err = (&outreach.Writer{Caller: uncitedCaller{}}).WriteLead(context.Background(), in)
	if err != nil || res.Written != nil || res.Fallback == "" {
		t.Fatalf("with versions: %v %+v", err, res)
	}
	found := false
	for _, f := range res.Attempts[0].Failures {
		found = found || f.Code == outreach.CodeCites
	}
	if !found {
		t.Fatalf("failures %+v, want the cites code", res.Attempts[0].Failures)
	}
}

// TestDraftsLinkedInTemplateRows: a LinkedIn lead's template rows are the note at step 0 and the
// message at step 1, each naming the target; a limit the note does not fit drops the lead.
func TestDraftsLinkedInTemplateRows(t *testing.T) {
	w := newDraftWorld(t, "unsupported", 7)
	c := w.c
	const target = "https://linkedin.com/company/northfield"
	c.Channel, c.RouteKind, c.Route = leadconfig.ChannelLinkedIn, leadconfig.ChannelLinkedIn, target
	rows, err := draftSequence(w.plan, c, w.rs.ev, w.cfg, nil, "", bytes.NewReader(nil))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Step != 0 || rows[1].Step != 1 || rows[0].To != target || rows[1].To != target || rows[0].Channel != "linkedin" {
		t.Fatalf("rows %+v", rows)
	}
	w.cfg.Sender.LinkedInNoteMax = 10
	if _, err := draftSequence(w.plan, c, w.rs.ev, w.cfg, nil, "", bytes.NewReader(nil)); err == nil || !strings.Contains(err.Error(), "at most 10") {
		t.Fatalf("over the limit: %v", err)
	}
}

// TestDraftsLinkedInNoteIsFirstRow: the note is the lead's first LinkedIn step wherever that falls.
func TestDraftsLinkedInNoteIsFirstRow(t *testing.T) {
	form := []leadstore.Message{{LeadID: 1, Step: 0, Channel: "form"}, {LeadID: 1, Step: 1, Channel: "linkedin"}, {LeadID: 1, Step: 2, Channel: "linkedin"}}
	if linkedInNote(form[0], form) || !linkedInNote(form[1], form) || linkedInNote(form[2], form) {
		t.Fatal("a form lead's LinkedIn steps")
	}
	li := []leadstore.Message{{LeadID: 2, Step: 0, Channel: "linkedin"}, {LeadID: 2, Step: 1, Channel: "linkedin"}}
	if !linkedInNote(li[0], li) || linkedInNote(li[1], li) {
		t.Fatal("a LinkedIn lead's steps")
	}
}

// TestDraftOriginWords: the words the lead page reads: Claude's slots, a template's reason, and a
// row from before this build, which says nothing and is the templates'.
func TestDraftOriginWords(t *testing.T) {
	raw, err := templateBecause(json.RawMessage(`{"Domain":{"value":"x.example","table":"org","id":1}}`), fbCap)
	if err != nil {
		t.Fatal(err)
	}
	if w, why := draftOrigin(raw); w != campaign.WriterTemplate || why != fbCap {
		t.Fatalf("template %q %q", w, why)
	}
	if !strings.Contains(string(raw), `"Domain"`) {
		t.Fatalf("the slots went: %s", raw)
	}
	if w, why := draftOrigin(json.RawMessage(`{"Domain":{"value":"x.example"}}`)); w != campaign.WriterTemplate || why != "" {
		t.Fatalf("old row %q %q", w, why)
	}
	if w, why := draftOrigin(json.RawMessage(`{"writer":"claude","generation_id":3}`)); w != campaign.WriterClaude || why != "" {
		t.Fatalf("claude %q %q", w, why)
	}
}

type oneGeneration struct{ g leadstore.Generation }

func (o oneGeneration) GenerationByID(context.Context, int64) (leadstore.Generation, error) {
	return o.g, nil
}

// TestClaudeWritingManual: a manual row's claims read its own part of a manual generation, which is
// never decoded as an email.
func TestClaudeWritingManual(t *testing.T) {
	out := json.RawMessage(`{"note":{"paragraphs":[{"text":"The note.","cites":["S1"]}]},"message":{"paragraphs":[{"text":"The message.","cites":["S1"]}]}}`)
	q := oneGeneration{leadstore.Generation{ID: 7, Channel: leadconfig.ChannelLinkedIn, Output: out}}
	for part, want := range map[string]string{outreach.PartNote: "The note.", outreach.PartMessage: "The message."} {
		slots, err := claudeSlotsFor(outreach.ManualWriterClaude, part, 7, outreach.FactSheet{}, []paragraphCites{{Cites: []string{"S1"}}})
		if err != nil {
			t.Fatal(err)
		}
		w, err := claudeWriting(context.Background(), q, slots, 2, false, map[int64]*outreach.WrittenDraft{})
		if err != nil || w == nil || len(w.Paragraphs) != 1 || w.Paragraphs[0].Text != want {
			t.Fatalf("%s: %+v %v", part, w, err)
		}
	}
}
