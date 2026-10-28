package main

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/claude"
	"github.com/gremy/webbership-leadhound/internal/console"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/outreach"
)

// TestDraftsConsoleContract: the console's review words are the store's: verdicts and
// reasons, in the form's order.
func TestDraftsConsoleContract(t *testing.T) {
	if !slices.Equal(console.ReviewReasons, leadstore.ReviewReasons) || console.ReasonOther != leadstore.ReasonOther ||
		console.VerdictApprove != leadstore.VerdictApprove || console.VerdictReject != leadstore.VerdictReject {
		t.Fatalf("console %v/%q, store %v/%q", console.ReviewReasons, console.ReasonOther, leadstore.ReviewReasons, leadstore.ReasonOther)
	}
}

func draftsSheet() outreach.FactSheet {
	return outreach.FactSheet{Facts: []outreach.Fact{
		{ID: "F2", Label: "Drupal version", Value: "7.98", Source: outreach.Provenance{Table: "site_probe", ID: 12}, Group: outreach.GroupLead},
		{ID: "F5", Label: "Their description", Value: "We bake bread", Source: outreach.Provenance{Table: "org_fact", ID: 3}, Group: outreach.GroupLead, EchoGuard: true},
		{ID: "S1", Label: "Sender", Value: "Demo Sender", Source: outreach.Provenance{Table: "campaign", ID: 1}, Group: outreach.GroupSender},
	}}
}

const draftsOutput = `{"steps":[{"step":0,"subject":"Your Drupal 7 site","paragraphs":[{"text":"I'm Demo Sender.","cites":["S1"]},` +
	`{"text":"Drupal 7.98 gets no fixes.","cites":["F2","F9"],"pain_id":"eol_no_security"}]},` +
	`{"step":1,"subject":"","paragraphs":[{"text":"Following up.","cites":["S1"]}]}],` +
	`"talking_points":[{"text":"Ask about 7.98","cites":["F2"]}]}`

// TestDraftSampleWords: a sampled generation becomes the console's sample: facts with their
// rows, steps with each cite resolved (a missing one marked), talking points, the review,
// failures worded with where they sit, and the fake decider marked.
func TestDraftSampleWords(t *testing.T) {
	at := time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC)
	g := leadstore.SampledGeneration{Generation: leadstore.Generation{ID: 41, LeadID: 5, Attempt: 1, Model: claude.ModelSonnet,
		Facts: draftsSheet(), Output: json.RawMessage(draftsOutput), CheckOK: true, At: at},
		Review: &leadstore.Review{Verdict: leadstore.VerdictReject, Reasons: []string{"too_long"}, Note: "n", At: at}}
	s := draftSample(g, "bakery.example")
	if s.GenerationID != 41 || s.Domain != "bakery.example" || s.Fake || !s.Reviewable || len(s.Facts) != 3 ||
		!s.Facts[1].ContextOnly || s.Facts[0].Table != "site_probe" || s.Facts[0].RowID != 12 || s.Facts[2].Group != "sender" {
		t.Fatalf("sample %+v", s)
	}
	if len(s.Steps) != 2 || len(s.Steps[0].Paragraphs) != 2 || len(s.TalkingPoints) != 1 {
		t.Fatalf("steps %+v talking points %+v", s.Steps, s.TalkingPoints)
	}
	p := s.Steps[0].Paragraphs[1]
	if p.Text != "Drupal 7.98 gets no fixes." || p.PainID != "eol_no_security" || len(p.Cites) != 2 ||
		p.Cites[0] != (console.DraftCite{ID: "F2", Label: "Drupal version", Value: "7.98", Table: "site_probe", RowID: 12}) ||
		!p.Cites[1].Missing {
		t.Fatalf("paragraph %+v", p)
	}
	if r := s.Review; r == nil || r.Verdict != leadstore.VerdictReject || !slices.Equal(r.Reasons, []string{"too_long"}) || !r.At.Equal(at) {
		t.Fatalf("review %+v", r)
	}

	g.Model, g.CheckOK, g.Output, g.Review = claude.FakeModel, false, nil, nil
	g.Failures = []outreach.Failure{{Code: outreach.CodeClaim, Step: 0, Para: 1, Token: "2019", Msg: "2019 is not in a cited fact"},
		{Code: outreach.CodeSchema, Step: outreach.StepTalkingPoints, Para: outreach.ParaSubject, Msg: "2 talking points; the call step needs 3"}}
	s = draftSample(g, "bakery.example")
	want := []console.DraftFailure{{Where: "Email 1, paragraph 2", Msg: "2019 is not in a cited fact", Code: "G3"},
		{Where: "The whole draft", Msg: "2 talking points; the call step needs 3", Code: "G1"}}
	if !s.Fake || s.Reviewable || s.Review != nil || len(s.Steps) != 0 || !slices.Equal(s.Failures, want) {
		t.Fatalf("refused fake sample %+v", s)
	}
}

// fakeGens is a generationReader over a map.
type fakeGens struct {
	m     map[int64]leadstore.Generation
	reads int
}

func (f *fakeGens) GenerationByID(_ context.Context, id int64) (leadstore.Generation, error) {
	f.reads++
	g, ok := f.m[id]
	if !ok {
		return leadstore.Generation{}, leadstore.ErrGenerationNotFound
	}
	return g, nil
}

// TestClaudeWriting: a Claude message's slots become its claims with the paragraph text of
// its own step (the talking points for a call), read once per generation; a template
// message, unreadable slots or a missing generation give no text or nothing.
func TestClaudeWriting(t *testing.T) {
	ctx := context.Background()
	sheet := draftsSheet()
	slots, err := claudeSlotsJSON(41, sheet, []paragraphCites{{Cites: []string{"S1"}}, {Cites: []string{"F2", "F9"}, PainID: "eol_no_security"}})
	if err != nil {
		t.Fatal(err)
	}
	q := &fakeGens{m: map[int64]leadstore.Generation{41: {ID: 41, Output: json.RawMessage(draftsOutput)}}}
	gens := map[int64]*outreach.WrittenDraft{}
	w, err := claudeWriting(ctx, q, slots, 0, false, gens)
	if err != nil || w == nil || w.GenerationID != 41 || len(w.Paragraphs) != 2 {
		t.Fatalf("writing %+v, %v", w, err)
	}
	p := w.Paragraphs[1]
	if p.Text != "Drupal 7.98 gets no fixes." || p.PainID != "eol_no_security" ||
		p.Cites[0] != (console.DraftCite{ID: "F2", Label: "Drupal version", Value: "7.98", Table: "site_probe", RowID: 12}) ||
		!p.Cites[1].Missing || w.Paragraphs[0].Text != "I'm Demo Sender." {
		t.Fatalf("paragraphs %+v", w.Paragraphs)
	}
	call, err := claudeSlotsJSON(41, sheet, []paragraphCites{{Cites: []string{"F2"}}})
	if err != nil {
		t.Fatal(err)
	}
	w, err = claudeWriting(ctx, q, call, 2, true, gens)
	if err != nil || len(w.Paragraphs) != 1 || w.Paragraphs[0].Text != "Ask about 7.98" {
		t.Fatalf("call %+v, %v", w, err)
	}
	if q.reads != 1 {
		t.Errorf("generation read %d times, want once", q.reads)
	}
	for _, s := range []string{"", "{}", `{"writer":"template"}`, "not json"} {
		if w, err := claudeWriting(ctx, q, json.RawMessage(s), 0, false, gens); w != nil || err != nil {
			t.Errorf("slots %q: %+v, %v", s, w, err)
		}
	}
	gone, err := claudeSlotsJSON(99, sheet, []paragraphCites{{Cites: []string{"F2"}}})
	if err != nil {
		t.Fatal(err)
	}
	w, err = claudeWriting(ctx, q, gone, 0, false, gens)
	if err != nil || len(w.Paragraphs) != 1 || w.Paragraphs[0].Text != "" || w.Paragraphs[0].Cites[0].Value != "7.98" {
		t.Fatalf("missing generation %+v, %v", w, err)
	}
}
