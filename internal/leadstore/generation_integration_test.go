//go:build integration

package leadstore_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/leadstore/leadstoretest"
	"github.com/gremy/webbership-leadhound/internal/outreach"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

// Migration 00019: Claude drafts, their reviews and the drafting evals.

func sheetFor(major string) outreach.FactSheet {
	return outreach.FactSheet{Facts: []outreach.Fact{
		{ID: "F1", Label: outreach.LabelMajor, Value: "Drupal " + major, Group: outreach.GroupLead,
			Source: outreach.Provenance{Table: "site_probe", ID: 1}},
		{ID: "S1", Label: outreach.LabelProofLine, Value: "One true line.", Group: outreach.GroupSender,
			Source: outreach.ConfigSource},
	}}
}

const goodOutput = `{"steps":[{"step":0,"subject":"Hello","paragraphs":[{"text":"Hi.","cites":["S1"]}]}],"talking_points":[]}`

// generate records one attempt and fails the test on an error.
func (f outreachFixture) generate(t *testing.T, lead int64, sheet outreach.FactSheet, ok bool, purpose string) int64 {
	t.Helper()
	row := leadstore.GenerationRow{LeadID: lead, CampaignID: 1, Attempt: 1, Model: "fake", Facts: sheet,
		Output: []byte(goodOutput), CheckOK: ok, Purpose: purpose}
	if !ok {
		row.Attempt = 2
		row.Failures = []outreach.Failure{{Code: outreach.CodeClaim, Step: 0, Para: 0, Token: "2019", Msg: "not a stored fact"}}
	}
	id, err := f.q.RecordGeneration(context.Background(), row)
	if err != nil {
		t.Fatalf("record generation lead %d: %v", lead, err)
	}
	return id
}

func TestGenerationRoundTrip(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	lead := f.siteLead(t, "gen.localhost", nil, 7, "info@gen.localhost")
	sheet := sheetFor("7")
	cost := int64(1234)
	refused := f.generate(t, lead, sheet, false, leadstore.PurposeBatch)
	passed, err := f.q.RecordGeneration(ctx, leadstore.GenerationRow{LeadID: lead, CampaignID: 1, Attempt: 2,
		Model: "claude-sonnet-5-5", Facts: sheet, Output: []byte(goodOutput), CheckOK: true, CostMicros: &cost,
		Purpose: leadstore.PurposeBatch})
	if err != nil {
		t.Fatal(err)
	}
	// Not valid JSON: stored as NULL, refused.
	garbled, err := f.q.RecordGeneration(ctx, leadstore.GenerationRow{LeadID: lead, CampaignID: 1, Attempt: 1,
		Model: "fake", Facts: sheet, Output: []byte("not json"), CheckOK: false,
		Failures: []outreach.Failure{{Code: outreach.CodeSchema, Step: -1, Para: -1, Msg: "not JSON"}},
		Purpose:  leadstore.PurposeSample})
	if err != nil {
		t.Fatal(err)
	}

	all, err := f.q.GenerationsForLead(ctx, lead, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 || all[0].ID != garbled || all[1].ID != passed || all[2].ID != refused {
		t.Fatalf("generations = %+v, want %d %d %d newest first", all, garbled, passed, refused)
	}
	g := all[1]
	if g.FactSHA256 != sheet.SHA256() || !reflect.DeepEqual(g.Facts, sheet) || !g.CheckOK || len(g.Failures) != 0 ||
		g.CostMicros == nil || *g.CostMicros != cost || g.Model != "claude-sonnet-5-5" || g.Attempt != 2 {
		t.Errorf("passed generation read back as %+v", g)
	}
	var out, want any
	_ = json.Unmarshal(g.Output, &out)
	_ = json.Unmarshal([]byte(goodOutput), &want)
	if !reflect.DeepEqual(out, want) {
		t.Errorf("output = %s", g.Output)
	}
	if all[0].Output != nil || all[0].CheckOK || len(all[0].Failures) != 1 || all[0].Failures[0].Code != outreach.CodeSchema {
		t.Errorf("garbled generation = %+v, want NULL output and its G1 failure", all[0])
	}
	if all[2].Failures[0].Token != "2019" || all[2].CostMicros != nil {
		t.Errorf("refused generation = %+v", all[2])
	}
	if by, err := f.q.GenerationByID(ctx, passed); err != nil || by.ID != passed {
		t.Errorf("by id = %+v, %v", by, err)
	}
	if _, err := f.q.GenerationByID(ctx, 999); !errors.Is(err, leadstore.ErrGenerationNotFound) {
		t.Errorf("missing generation: %v", err)
	}
	if other, err := f.q.GenerationsForLead(ctx, lead, 2); err != nil || len(other) != 0 {
		t.Errorf("another campaign's generations = %v, %v", other, err)
	}
}

func TestGenerationReuseByFactHash(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	lead := f.siteLead(t, "reuse.localhost", nil, 7, "info@reuse.localhost")
	sheet := sheetFor("7")
	if _, ok, err := f.q.LatestGenerationForLead(ctx, lead, 1, sheet.SHA256()); err != nil || ok {
		t.Fatalf("cold start: ok %t, %v; want none", ok, err)
	}
	f.generate(t, lead, sheet, false, leadstore.PurposeBatch)
	if _, ok, _ := f.q.LatestGenerationForLead(ctx, lead, 1, sheet.SHA256()); ok {
		t.Fatal("a refused generation was offered for reuse")
	}
	first := f.generate(t, lead, sheet, true, leadstore.PurposeSample)
	second := f.generate(t, lead, sheet, true, leadstore.PurposeBatch)
	g, ok, err := f.q.LatestGenerationForLead(ctx, lead, 1, sheet.SHA256())
	if err != nil || !ok || g.ID != second {
		t.Fatalf("reuse = %d %t %v, want %d (newest passing; %d older)", g.ID, ok, err, second, first)
	}
	if _, ok, _ := f.q.LatestGenerationForLead(ctx, lead, 1, sheetFor("8").SHA256()); ok {
		t.Error("a generation was reused across changed facts")
	}
	if _, ok, _ := f.q.LatestGenerationForLead(ctx, lead, 2, sheet.SHA256()); ok {
		t.Error("a generation was reused across campaigns")
	}
}

func TestGenerationRefusesBadRows(t *testing.T) {
	f := newOutreachFixture(t)
	lead := f.siteLead(t, "badgen.localhost", nil, 7, "")
	neg := int64(-1)
	base := leadstore.GenerationRow{LeadID: lead, CampaignID: 1, Attempt: 1, Model: "fake", Facts: sheetFor("7"),
		Output: []byte(goodOutput), CheckOK: true, Purpose: leadstore.PurposeBatch}
	for name, edit := range map[string]func(*leadstore.GenerationRow){
		"attempt 0":           func(r *leadstore.GenerationRow) { r.Attempt = 0 },
		"attempt 3":           func(r *leadstore.GenerationRow) { r.Attempt = 3 },
		"no model":            func(r *leadstore.GenerationRow) { r.Model = "" },
		"model newline":       func(r *leadstore.GenerationRow) { r.Model = "a\nb" },
		"purpose":             func(r *leadstore.GenerationRow) { r.Purpose = "send" },
		"ok with failures":    func(r *leadstore.GenerationRow) { r.Failures = []outreach.Failure{{Code: "G3"}} },
		"refused, no failure": func(r *leadstore.GenerationRow) { r.CheckOK = false },
		"negative cost":       func(r *leadstore.GenerationRow) { r.CostMicros = &neg },
		"ok but not JSON":     func(r *leadstore.GenerationRow) { r.Output = []byte("{") },
	} {
		row := base
		edit(&row)
		if _, err := f.q.RecordGeneration(context.Background(), row); !errors.Is(err, leadstore.ErrBadGeneration) {
			t.Errorf("%s: %v, want ErrBadGeneration", name, err)
		}
	}
}

// TestReviewStatusGrid walks reviews through every status and operation: approve then
// reject (latest wins), a refused generation (refused), a missing one, bad input.
func TestReviewStatusGrid(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	lead := f.siteLead(t, "review.localhost", nil, 7, "info@review.localhost")
	sheet := sheetFor("7")
	good := f.generate(t, lead, sheet, true, leadstore.PurposeSample)
	refused := f.generate(t, lead, sheet, false, leadstore.PurposeSample)

	if got, err := f.q.ReviewsForCampaign(ctx, 1, 20); err != nil || len(got) != 0 {
		t.Fatalf("cold start reviews = %v, %v", got, err)
	}
	if _, err := f.q.RecordReview(ctx, good, leadstore.VerdictApprove, nil, "", op); err != nil {
		t.Fatalf("approve: %v", err)
	}
	reviews, err := f.q.ReviewsForCampaign(ctx, 1, 20)
	if err != nil || len(reviews) != 1 || reviews[0].Verdict != leadstore.VerdictApprove || reviews[0].LeadID != lead {
		t.Fatalf("after approve = %+v, %v", reviews, err)
	}
	if s := leadstore.SummarizeReviews(reviews); s.Reviewed != 1 || s.Approved != 1 || s.ApprovedShare() != 1 {
		t.Errorf("summary = %+v", s)
	}
	rej, err := f.q.RecordReview(ctx, good, leadstore.VerdictReject, []string{"other", "too_long"}, "too stiff", op)
	if err != nil {
		t.Fatalf("re-review reject: %v", err)
	}
	reviews, _ = f.q.ReviewsForCampaign(ctx, 1, 20)
	if len(reviews) != 1 || reviews[0].ID != rej || reviews[0].Verdict != leadstore.VerdictReject ||
		!reflect.DeepEqual(reviews[0].Reasons, []string{"too_long", "other"}) || reviews[0].Note != "too stiff" {
		t.Fatalf("latest review = %+v, want the reject (latest wins), reasons in the set's order", reviews)
	}
	if s := leadstore.SummarizeReviews(reviews); s.Reviewed != 1 || s.Approved != 0 {
		t.Errorf("summary after reject = %+v", s)
	}

	if _, err := f.q.RecordReview(ctx, refused, leadstore.VerdictApprove, nil, "", op); !errors.Is(err, leadstore.ErrReviewRefused) {
		t.Errorf("review of a refused generation: %v, want ErrReviewRefused", err)
	}
	if _, err := f.q.RecordReview(ctx, 999, leadstore.VerdictApprove, nil, "", op); !errors.Is(err, leadstore.ErrGenerationNotFound) {
		t.Errorf("review of a missing generation: %v, want ErrGenerationNotFound", err)
	}
	long := make([]byte, leadstore.MaxReviewNote+1)
	for i := range long {
		long[i] = 'a'
	}
	for name, c := range map[string]struct {
		verdict string
		reasons []string
		note    string
	}{
		"unknown verdict":     {"maybe", nil, ""},
		"reject no reason":    {leadstore.VerdictReject, nil, ""},
		"approve with reason": {leadstore.VerdictApprove, []string{"unclear"}, ""},
		"unknown reason":      {leadstore.VerdictReject, []string{"rude"}, ""},
		"other without note":  {leadstore.VerdictReject, []string{"other"}, "  "},
		"note too long":       {leadstore.VerdictReject, []string{"unclear"}, string(long)},
	} {
		if _, err := f.q.RecordReview(ctx, good, c.verdict, c.reasons, c.note, op); !errors.Is(err, leadstore.ErrBadReview) {
			t.Errorf("%s: %v, want ErrBadReview", name, err)
		}
	}
	if _, err := f.q.RecordReview(ctx, good, leadstore.VerdictApprove, nil, "", "bob"); !errors.Is(err, leadstore.ErrUnknownActor) {
		t.Errorf("unknown actor: %v", err)
	}
	var n int
	if err := f.owner.QueryRow(ctx, "SELECT count(*) FROM draft_review").Scan(&n); err != nil || n != 2 {
		t.Errorf("draft_review rows = %d (%v), want 2: refusals store nothing", n, err)
	}
	if leads, err := f.q.ReviewedLeads(ctx, 1); err != nil || !reflect.DeepEqual(leads, []int64{lead}) {
		t.Errorf("reviewed leads = %v, %v", leads, err)
	}
}

func TestReviewsLastN(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	lead := f.siteLead(t, "lastn.localhost", nil, 7, "")
	var gens []int64
	for range 4 {
		gens = append(gens, f.generate(t, lead, sheetFor("7"), true, leadstore.PurposeSample))
	}
	// Reviewed in order 0, 1, 2, 3, then 0 again: generation 0's latest review is now the newest.
	for i, g := range append(gens, gens[0]) {
		verdict, reasons := leadstore.VerdictApprove, []string(nil)
		if i%2 == 1 {
			verdict, reasons = leadstore.VerdictReject, []string{"unclear"}
		}
		if _, err := f.q.RecordReview(ctx, g, verdict, reasons, "", op); err != nil {
			t.Fatal(err)
		}
	}
	got, err := f.q.ReviewsForCampaign(ctx, 1, 2)
	if err != nil || len(got) != 2 || got[0].GenerationID != gens[0] || got[1].GenerationID != gens[3] {
		t.Fatalf("last 2 = %+v, %v; want generations %d then %d", got, err, gens[0], gens[3])
	}
	all, _ := f.q.ReviewsForCampaign(ctx, 1, 0)
	if s := leadstore.SummarizeReviews(all); s.Reviewed != 4 || s.Approved != 2 {
		t.Errorf("summary over all = %+v, want 4 reviewed, 2 approved (0 and 2 approve, 1 and 3 reject)", s)
	}
}

func TestSampleGenerationsWithReview(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	lead := f.siteLead(t, "sample.localhost", nil, 7, "")
	f.generate(t, lead, sheetFor("7"), true, leadstore.PurposeBatch) // not a sample
	a := f.generate(t, lead, sheetFor("7"), true, leadstore.PurposeSample)
	b := f.generate(t, lead, sheetFor("7"), false, leadstore.PurposeSample)
	if _, err := f.q.RecordReview(ctx, a, leadstore.VerdictApprove, nil, "", op); err != nil {
		t.Fatal(err)
	}
	rej, err := f.q.RecordReview(ctx, a, leadstore.VerdictReject, []string{"factual"}, "", op)
	if err != nil {
		t.Fatal(err)
	}
	got, err := f.q.SampleGenerations(ctx, 1, 0)
	if err != nil || len(got) != 2 || got[0].ID != b || got[1].ID != a {
		t.Fatalf("samples = %+v, %v; want %d, %d", got, err, b, a)
	}
	if got[0].Review != nil {
		t.Errorf("unreviewed sample has review %+v", got[0].Review)
	}
	if r := got[1].Review; r == nil || r.ID != rej || r.Verdict != leadstore.VerdictReject || r.LeadID != lead ||
		!reflect.DeepEqual(r.Reasons, []string{"factual"}) {
		t.Errorf("sample review = %+v, want the latest (reject)", r)
	}
	if one, _ := f.q.SampleGenerations(ctx, 1, 1); len(one) != 1 || one[0].ID != b {
		t.Errorf("limit 1 = %+v", one)
	}
}

func TestEvalAppendAndLatest(t *testing.T) {
	leadstoretest.Reset(t)
	q := leadstoretest.Q(t)
	ctx := context.Background()
	if _, ok, err := q.LatestEval(ctx, 1); err != nil || ok {
		t.Fatalf("cold start: ok %t, %v", ok, err)
	}
	e := leadstore.DraftEvalRow{CampaignID: 1, Runs: 2, Reviewed: 20, FirstTryPass: 0.9, AfterRetryPass: 0.98,
		Failures: map[string]int{"G3": 2}, MeanWords: map[string]float64{"0": 101.5}, Tokens: 5000, CostMicros: 700,
		Decider: leadstore.DeciderClaude}
	first, err := q.RecordEval(ctx, e)
	if err != nil {
		t.Fatal(err)
	}
	e.Reviewed, e.FirstTryPass = 25, 0.95
	second, err := q.RecordEval(ctx, e)
	if err != nil {
		t.Fatal(err)
	}
	got, ok, err := q.LatestEval(ctx, 1)
	if err != nil || !ok || got.ID != second || got.ID == first || got.Reviewed != 25 || got.FirstTryPass != 0.95 ||
		got.Failures["G3"] != 2 || got.MeanWords["0"] != 101.5 || got.Tokens != 5000 || got.Decider != "claude" {
		t.Fatalf("latest = %+v %t %v", got, ok, err)
	}
	if _, ok, _ := q.LatestEval(ctx, 2); ok {
		t.Error("another campaign has an eval")
	}

	few := e
	few.Reviewed = 19
	_, err = q.RecordEval(ctx, few)
	var tooFew *leadstore.EvalTooFewError
	if !errors.Is(err, leadstore.ErrEvalTooFew) || !errors.As(err, &tooFew) || tooFew.Reviewed != 19 {
		t.Errorf("19 reviewed: %v, want EvalTooFewError with the count", err)
	}
	for name, edit := range map[string]func(*leadstore.DraftEvalRow){
		"fake decider":        func(r *leadstore.DraftEvalRow) { r.Decider = leadstore.DeciderFake },
		"no runs":             func(r *leadstore.DraftEvalRow) { r.Runs = 0 },
		"share above 1":       func(r *leadstore.DraftEvalRow) { r.AfterRetryPass = 1.1 },
		"retry below first":   func(r *leadstore.DraftEvalRow) { r.AfterRetryPass = 0.5 },
		"negative tokens":     func(r *leadstore.DraftEvalRow) { r.Tokens = -1 },
		"negative failure n":  func(r *leadstore.DraftEvalRow) { r.Failures = map[string]int{"G1": -1} },
		"negative mean words": func(r *leadstore.DraftEvalRow) { r.MeanWords = map[string]float64{"0": -1} },
	} {
		row := e
		edit(&row)
		if _, err := q.RecordEval(ctx, row); !errors.Is(err, leadstore.ErrBadDraftEval) {
			t.Errorf("%s: %v, want ErrBadDraftEval", name, err)
		}
	}
	var n int
	if err := storetest.OwnerConn(t).QueryRow(ctx, "SELECT count(*) FROM draft_eval").Scan(&n); err != nil || n != 2 {
		t.Errorf("draft_eval rows = %d (%v), want 2 (append-only, refusals store nothing)", n, err)
	}
}
