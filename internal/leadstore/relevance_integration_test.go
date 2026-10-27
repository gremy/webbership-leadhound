//go:build integration

package leadstore_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"math"
	"testing"

	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/leadstore/leadstoretest"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

// Migrations 00013 and 00014: relevance ratings, their evals, and the leads settings.

func TestRelevanceLatestWins(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	a := f.siteLead(t, "a.localhost", nil, 7, "")
	b := f.siteLead(t, "b.localhost", nil, 7, "")
	never := f.siteLead(t, "never.localhost", nil, 7, "")
	if got, err := f.q.LatestRelevance(ctx, []int64{a, b, never}); err != nil || len(got) != 0 {
		t.Fatalf("cold start: %v %v", got, err)
	}
	rate := func(lead int64, input, band string, score int) {
		t.Helper()
		sum := sha256.Sum256([]byte(input))
		if _, err := f.q.InsertRelevance(ctx, leadstore.RelevanceRow{LeadID: lead, Question: "relevance@1", Decider: "fake",
			Model: "fake-1", InputSHA256: sum[:], Band: band, Score: score,
			Probabilities: map[string]float64{band: 1}, InputTokens: 1900, RunID: f.runID}); err != nil {
			t.Fatalf("rate %d: %v", lead, err)
		}
	}
	rate(a, "first", leadstore.BandMaybe, 50)
	rate(b, "only", leadstore.BandNo, 0)
	rate(a, "second", leadstore.BandStrong, 92)
	got, err := f.q.LatestRelevance(ctx, []int64{a, b, never})
	if err != nil {
		t.Fatal(err)
	}
	second := sha256.Sum256([]byte("second"))
	if r := got[a]; r.Score != 92 || r.Band != leadstore.BandStrong || !bytes.Equal(r.InputSHA256, second[:]) ||
		r.Probabilities[leadstore.BandStrong] != 1 || r.InputTokens != 1900 || r.Model != "fake-1" {
		t.Errorf("lead a latest = %+v, want the second rating", r)
	}
	if r, ok := got[b]; !ok || r.Score != 0 {
		t.Errorf("lead b latest = %+v (%v)", r, ok)
	}
	if _, ok := got[never]; ok || len(got) != 2 {
		t.Errorf("a lead never rated has an entry: %v", got)
	}
	// The checks: a score out of range, an unknown band, a short hash.
	sum := sha256.Sum256([]byte("x"))
	for name, r := range map[string]leadstore.RelevanceRow{
		"score": {LeadID: a, Question: "q", Decider: "fake", InputSHA256: sum[:], Band: leadstore.BandNo, Score: 101, RunID: f.runID},
		"band":  {LeadID: a, Question: "q", Decider: "fake", InputSHA256: sum[:], Band: "great", Score: 1, RunID: f.runID},
		"hash":  {LeadID: a, Question: "q", Decider: "fake", InputSHA256: sum[:4], Band: leadstore.BandNo, Score: 1, RunID: f.runID},
	} {
		if _, err := f.q.InsertRelevance(ctx, r); sqlState(err) != "23514" {
			t.Errorf("%s: %v, want 23514", name, err)
		}
	}
	app := storetest.AppConn(t)
	for _, q := range []string{"UPDATE lead_relevance SET score = 0", "DELETE FROM lead_relevance"} {
		if _, err := app.Exec(ctx, q); sqlState(err) != "42501" {
			t.Errorf("%s as app: %v, want 42501", q, err)
		}
	}
}

func TestRelevanceEvalLatest(t *testing.T) {
	leadstoretest.Reset(t)
	ctx := context.Background()
	q := leadstoretest.Q(t)
	if _, ok, err := q.LatestRelevanceEval(ctx, "relevance@1"); err != nil || ok {
		t.Fatalf("cold start: ok %v, %v", ok, err)
	}
	base := leadstore.RelevanceEval{Question: "relevance@1", Model: "m", N: 30, Exact: 0.5, WithinOne: 0.7, MAE: 18.5,
		Spearman: 0.41, Runs: 3, Trusted: false}
	if _, err := q.RecordRelevanceEval(ctx, base); err != nil {
		t.Fatal(err)
	}
	passed := base
	passed.WithinOne, passed.Spearman, passed.Trusted = 0.83, 0.62, true
	id, err := q.RecordRelevanceEval(ctx, passed)
	if err != nil {
		t.Fatal(err)
	}
	other := base
	other.Question = "relevance@2"
	if _, err := q.RecordRelevanceEval(ctx, other); err != nil {
		t.Fatal(err)
	}
	got, ok, err := q.LatestRelevanceEval(ctx, "relevance@1")
	if err != nil || !ok || got.ID != id || !got.Trusted || got.WithinOne != 0.83 || got.Spearman != 0.62 || got.N != 30 || got.Runs != 3 {
		t.Errorf("latest eval = %+v (%v, %v), want the passed one", got, ok, err)
	}
	for name, e := range map[string]leadstore.RelevanceEval{
		"share":   {Question: "q", Exact: 80, WithinOne: 0.5, Runs: 1},
		"nan rho": {Question: "q", Spearman: math.NaN(), Runs: 1},
		"no runs": {Question: "q"},
	} {
		if _, err := q.RecordRelevanceEval(ctx, e); !errors.Is(err, leadstore.ErrBadEval) {
			t.Errorf("%s: %v, want ErrBadEval", name, err)
		}
	}
	app := storetest.AppConn(t)
	for _, s := range []string{"UPDATE lead_relevance_eval SET trusted = true", "DELETE FROM lead_relevance_eval"} {
		if _, err := app.Exec(ctx, s); sqlState(err) != "42501" {
			t.Errorf("%s as app: %v, want 42501", s, err)
		}
	}
}

func TestSettingsLatestWins(t *testing.T) {
	leadstoretest.Reset(t)
	ctx := context.Background()
	q := leadstoretest.Q(t)
	raw, id, _, fromDB, err := q.LatestSettings(ctx, leadstore.DefaultAccountID)
	if err != nil || raw != nil || id != 0 || fromDB {
		t.Fatalf("cold start: %q %d %v %v", raw, id, fromDB, err)
	}
	first, err := q.SaveSettings(ctx, leadstore.DefaultAccountID, []byte(`{"skip_rank_over": 100000}`), leadstore.ActorOperator)
	if err != nil {
		t.Fatal(err)
	}
	second, err := q.SaveSettings(ctx, leadstore.DefaultAccountID, []byte(`{"batches_from": "starred_then_rest"}`), leadstore.ActorOperator)
	if err != nil {
		t.Fatal(err)
	}
	raw, id, at, fromDB, err := q.LatestSettings(ctx, leadstore.DefaultAccountID)
	var doc map[string]any
	if err != nil || id != second || !fromDB || at.IsZero() || json.Unmarshal(raw, &doc) != nil || doc["batches_from"] != "starred_then_rest" || len(doc) != 1 {
		t.Errorf("latest = %q %d %v %v, want the second save (id %d, first %d)", raw, id, fromDB, err, second, first)
	}
	reset, err := q.ResetSettings(ctx, leadstore.DefaultAccountID, leadstore.ActorOperator)
	if err != nil {
		t.Fatal(err)
	}
	raw, id, at, fromDB, err = q.LatestSettings(ctx, leadstore.DefaultAccountID)
	if err != nil || raw != nil || id != reset || fromDB || at.IsZero() {
		t.Errorf("after reset: %q %d %v %v, want no document, the reset's id %d", raw, id, fromDB, err, reset)
	}
	for name, doc := range map[string]string{"array": `[1]`, "invalid": `{"a":`, "string": `"x"`} {
		if _, err := q.SaveSettings(ctx, leadstore.DefaultAccountID, []byte(doc), leadstore.ActorOperator); !errors.Is(err, leadstore.ErrSettingsNotObject) {
			t.Errorf("%s: %v, want ErrSettingsNotObject", name, err)
		}
	}
	if _, err := q.SaveSettings(ctx, leadstore.DefaultAccountID, []byte(`{}`), leadstore.ActorGremy); !errors.Is(err, leadstore.ErrUnknownActor) {
		t.Errorf("as gremy: %v, want ErrUnknownActor", err)
	}
	app := storetest.AppConn(t)
	for _, s := range []string{"UPDATE lead_setting SET settings = NULL", "DELETE FROM lead_setting"} {
		if _, err := app.Exec(ctx, s); sqlState(err) != "42501" {
			t.Errorf("%s as app: %v, want 42501", s, err)
		}
	}
}
