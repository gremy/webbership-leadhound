package scoring

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/ai"
	"github.com/gremy/webbership-leadhound/internal/candidate"
	"github.com/gremy/webbership-leadhound/internal/store"
)

// screenAs is a title screen that gives every title v.
func screenAs(v candidate.TitleVerdict) func(string) candidate.TitleVerdict {
	return func(string) candidate.TitleVerdict { return v }
}

// withTitleCheck is allPass plus a title-only answer.
func withTitleCheck(a ai.Answer) map[string]ai.Answer {
	m := allPass()
	m[TitleCheckName] = a
	return m
}

func screened(m *memStore, d ai.Decider, v candidate.TitleVerdict) *Scorer {
	s := scorer(m, d)
	s.TitleCheck = screenAs(v)
	return s
}

// recorder records every request it passes on to its decider.
type recorder struct {
	ai.Decider
	states []string
	qs     [][]ai.Question
}

func (r *recorder) Decide(ctx context.Context, state string, qs []ai.Question) (map[string]ai.Answer, ai.Usage, error) {
	r.states = append(r.states, state)
	r.qs = append(r.qs, qs)
	return r.Decider.Decide(ctx, state, qs)
}

// TestUnsureTitleIsAskedFirst: an unsure title gets one title-only call (the title alone,
// the title criterion's options), then full scoring on a pass.
func TestUnsureTitleIsAskedFirst(t *testing.T) {
	m := &memStore{inputs: []store.ScoreInput{job(1, "Rust.", "pass")}}
	rec := &recorder{Decider: &ai.Fake{Answers: withTitleCheck(answer(Pass, 0.9))}}
	res, err := screened(m, rec, candidate.TitleUnsure).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.TitleAsked != 1 || res.TitlePassed != 1 || res.TitleFailed != 0 || res.Calls != 2 || res.Scored != 1 {
		t.Fatalf("result %+v", res)
	}
	if rec.states[0] != "Title: Senior Rust Engineer" || len(rec.qs[0]) != 1 {
		t.Fatalf("title-only request: state %q, %d questions", rec.states[0], len(rec.qs[0]))
	}
	q := rec.qs[0][0]
	title := testCriteria[0]
	if q.Name != TitleCheckName || q.Options[0].Description != title.Pass || q.Options[1].Description != title.Fail || q.Options[2].Description != title.Unclear {
		t.Errorf("title-only question %+v", q)
	}
	if len(rec.qs[1]) != 5 {
		t.Errorf("full scoring asked %d questions, want 5", len(rec.qs[1]))
	}
	if len(m.written[1]) != 5 {
		t.Errorf("wrote %d decisions, want 5", len(m.written[1]))
	}
	if !strings.Contains(res.Note(), "title check: 1 asked · 0 failed · 1 passed") {
		t.Errorf("note %q", res.Note())
	}
}

// TestKeptTitleIsNotAsked: a kept title, or no screen at all, goes straight to full
// scoring with the hash it had before the screen existed.
func TestKeptTitleIsNotAsked(t *testing.T) {
	for _, check := range []func(string) candidate.TitleVerdict{nil, screenAs(candidate.TitleKeep)} {
		m := &memStore{inputs: []store.ScoreInput{job(1, "Rust.", "pass")}}
		fake := &ai.Fake{Answers: allPass()} // no title_check script: asking it would fail
		s := scorer(m, fake)
		s.TitleCheck = check
		res, err := s.Run(context.Background())
		if err != nil || res.Calls != 1 || res.TitleAsked != 0 || res.Scored != 1 {
			t.Fatalf("%+v, %v", res, err)
		}
		want := InputHash("fake", Questions(testCriteria), State(Posting{Title: "Senior Rust Engineer", Company: "Northwind Labs",
			Location: "Exampleland", Seniority: "Mid-Senior level", EmploymentType: "Full-time", Description: "Rust."}, MaxDescription),
			0.7, testKeywords)
		if !bytes.Equal(m.written[1][0].InputSHA256, want) {
			t.Error("a kept title's hash changed")
		}
	}
}

// TestDroppedTitleFailsWithoutACall: a stored title the screen drops records the title
// fail in code, never reaching the decider.
func TestDroppedTitleFailsWithoutACall(t *testing.T) {
	m := &memStore{inputs: []store.ScoreInput{job(1, "Rust.", "pass")}}
	fake := &ai.Fake{Answers: allPass()}
	res, err := screened(m, fake, candidate.TitleDrop).Run(context.Background())
	if err != nil || fake.Calls() != 0 || res.Calls != 0 || res.TitleFailed != 1 || res.TitleAsked != 0 {
		t.Fatalf("%+v, %v, calls %d", res, err, fake.Calls())
	}
	ds := m.written[1]
	if len(ds) != 1 || ds[0].Criterion != "title" || ds[0].Verdict != Fail || !strings.Contains(ds[0].Reason, "no call made") {
		t.Fatalf("decisions %+v", ds)
	}
	// Unchanged on the next run.
	m.inputs[0].LastInputSHA256 = ds[0].InputSHA256
	res, _ = screened(m, fake, candidate.TitleDrop).Run(context.Background())
	if res.Unchanged != 1 || len(m.written[1]) != 1 {
		t.Errorf("second run %+v", res)
	}
}

// TestTitleCheckFailSkipsFullScoring: a fail records one title decision that says where it
// came from, makes no full call, and ranks as a failed criterion, never as unscored.
func TestTitleCheckFailSkipsFullScoring(t *testing.T) {
	m := &memStore{inputs: []store.ScoreInput{job(1, "Rust.", "pass", "pass", "pass", "pass")}}
	fake := &ai.Fake{Answers: withTitleCheck(answer(Fail, 0.9))}
	res, err := screened(m, fake, candidate.TitleUnsure).Run(context.Background())
	if err != nil || fake.Calls() != 1 || res.TitleFailed != 1 || res.TitlePassed != 0 || res.Scored != 1 {
		t.Fatalf("%+v, %v, calls %d", res, err, fake.Calls())
	}
	ds := m.written[1]
	if len(ds) != 1 || ds[0].Criterion != "title" || ds[0].Verdict != Fail {
		t.Fatalf("decisions %+v", ds)
	}
	if !strings.HasPrefix(ds[0].Reason, "title: Not engineering. (title-only check, before full scoring)\n\"Senior Rust Engineer\"\n") {
		t.Errorf("reason %q", ds[0].Reason)
	}

	in := store.RankInput{JobID: 1, ListingStatus: "open", Status: "new", Title: "Senior Rust Engineer"}
	in.Gates = append(in.Gates, m.inputs[0].Gates...)
	in.Decisions = []store.Decision{{JobID: 1, Criterion: ds[0].Criterion, Verdict: ds[0].Verdict, Reason: ds[0].Reason}}
	r := Rank(CandidatesFrom([]store.RankInput{in}, nil), time.UTC, Caps{Apply: 5, YourCall: 5})
	if r.Skipped[SkipCriterion] != 1 || r.Skipped[SkipUnscored] != 0 || r.RunnerUp != nil || r.ApplyTotal+r.YourCallTotal != 0 {
		t.Errorf("ranking %+v", r)
	}
}

// TestTitleCheckUnclearGoesOn: unclear, or a fail below the threshold, goes on to full
// scoring, which asks the title again.
func TestTitleCheckUnclearGoesOn(t *testing.T) {
	for _, a := range []ai.Answer{answer(Unclear, 0.9), answer(Fail, 0.5)} {
		m := &memStore{inputs: []store.ScoreInput{job(1, "Rust.", "pass")}}
		fake := &ai.Fake{Answers: withTitleCheck(a)}
		res, err := screened(m, fake, candidate.TitleUnsure).Run(context.Background())
		if err != nil || res.TitlePassed != 1 || res.Calls != 2 || len(m.written[1]) != 5 {
			t.Fatalf("%+v: %+v, %v", a, res, err)
		}
		for _, d := range m.written[1] {
			if d.Criterion == "title" && d.Verdict != Pass {
				t.Errorf("title verdict %s, want full scoring's pass", d.Verdict)
			}
		}
	}
}

// TestTitleCheckCountsAgainstTheCap: the title-only call is a capped call recorded like
// any other, and a cap reached at either call stops the run cleanly.
func TestTitleCheckCountsAgainstTheCap(t *testing.T) {
	m := &memStore{inputs: []store.ScoreInput{job(1, "Rust.", "pass")}}
	usage := &capStore{}
	capped := &ai.Capped{Decider: &ai.Fake{Answers: withTitleCheck(answer(Pass, 0.9))}, Store: usage, DailyCalls: 1, Loc: time.UTC}
	res, err := screened(m, capped, candidate.TitleUnsure).Run(context.Background())
	if err != nil || res.StopReason != StopSpendCap || usage.calls != 1 || res.Calls != 1 || res.TitleAsked != 1 || len(m.written) != 0 {
		t.Fatalf("%+v, %v, usage %d", res, err, usage.calls)
	}

	usage = &capStore{}
	capped = &ai.Capped{Decider: &ai.Fake{Answers: withTitleCheck(answer(Pass, 0.9))}, Store: usage, DailyCalls: 0, Loc: time.UTC}
	res, err = screened(m, capped, candidate.TitleUnsure).Run(context.Background())
	if err != nil || res.StopReason != StopSpendCap || usage.calls != 0 || res.Calls != 0 || res.TitleAsked != 0 {
		t.Fatalf("cap at the title call: %+v, %v", res, err)
	}
}

// TestTitleCheckHash: a job the title check failed is not asked again on the same inputs,
// nor one it passed and full scoring decided; a changed title criterion asks again, and so
// does a title the screen now sorts differently.
func TestTitleCheckHash(t *testing.T) {
	m := &memStore{inputs: []store.ScoreInput{job(1, "Rust.", "pass")}}
	fake := &ai.Fake{Answers: withTitleCheck(answer(Fail, 0.9))}
	if _, err := screened(m, fake, candidate.TitleUnsure).Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	m.inputs[0].LastInputSHA256 = m.written[1][0].InputSHA256

	res, _ := screened(m, fake, candidate.TitleUnsure).Run(context.Background())
	if res.Unchanged != 1 || fake.Calls() != 1 {
		t.Fatalf("same inputs: %+v, calls %d", res, fake.Calls())
	}

	changed := scorer(m, fake)
	changed.TitleCheck = screenAs(candidate.TitleUnsure)
	changed.Criteria = append([]candidate.NamedCriterion(nil), testCriteria...)
	changed.Criteria[0].Fail = "Not a software role."
	res, _ = changed.Run(context.Background())
	if res.Unchanged != 0 || res.TitleAsked != 1 || fake.Calls() != 2 {
		t.Fatalf("changed criterion: %+v, calls %d", res, fake.Calls())
	}

	// Passed, then fully scored: the next run asks neither question.
	m2 := &memStore{inputs: []store.ScoreInput{job(2, "Rust.", "pass")}}
	pass := &ai.Fake{Answers: withTitleCheck(answer(Pass, 0.9))}
	if _, err := screened(m2, pass, candidate.TitleUnsure).Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	m2.inputs[0].LastInputSHA256 = m2.written[2][0].InputSHA256
	res, _ = screened(m2, pass, candidate.TitleUnsure).Run(context.Background())
	if res.Unchanged != 1 || pass.Calls() != 2 {
		t.Fatalf("passed and scored: %+v, calls %d", res, pass.Calls())
	}
	// The same job now kept by the screen is scored again (its screen input changed).
	res, _ = screened(m2, pass, candidate.TitleKeep).Run(context.Background())
	if res.Unchanged != 0 || res.TitleAsked != 0 || pass.Calls() != 3 {
		t.Fatalf("now kept: %+v, calls %d", res, pass.Calls())
	}
}
