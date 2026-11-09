//go:build integration

package main

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/ai"
	"github.com/gremy/webbership-leadhound/internal/cafe"
	"github.com/gremy/webbership-leadhound/internal/leadconfig"
	"github.com/gremy/webbership-leadhound/internal/leadrate"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/leadstore/leadstoretest"
	"github.com/gremy/webbership-leadhound/internal/store"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

// placeTypeStub is a stand-in for Jev: relevance is the fake's "maybe", and the place-type
// question is answered roastery, except for the states in unclear, which are answered
// unclear. The business-kind question, an option of both rating types, is answered unclear.
func placeTypeStub(unclear ...string) *ai.Fake {
	answers := relevanceFake().Answers
	answers[leadrate.PlaceTypeAIQuestion().Name] = ai.Answer{Choice: leadrate.PlaceOptionRoastery, Confidence: 0.8}
	answers[leadrate.BusinessKindAIQuestion(leadstore.TypeDrupal).Name] = ai.Answer{Choice: leadrate.BusinessKindUnclear, Confidence: 0.8}
	f := &ai.Fake{Answers: answers, ByState: map[string]map[string]ai.Answer{}}
	for _, s := range unclear {
		f.ByState[s] = map[string]ai.Answer{leadrate.PlaceTypeAIQuestion().Name: {Choice: leadrate.PlaceOptionUnclear, Confidence: 0.4}}
	}
	return f
}

// placeTypeScene is the order scene's eight open Drupal leads with two of them turned into
// café leads: the roastery-to-be and the unclear-to-be, by their ids.
func placeTypeScene(t *testing.T) (roastery, unclear int64) {
	t.Helper()
	orderScene(t)
	roastery, unclear = setLeadKind(t, "lucerne.example", cafe.KindNoDigitalLoyalty), setLeadKind(t, "proseware.example", cafe.KindNoDigitalLoyalty)
	// Distinct names, so the two cafés' states differ as real ones do.
	ownerExec(t, `UPDATE org SET name = 'Prăjitoria Fictivă' WHERE domain = 'lucerne.example'`)
	ownerExec(t, `UPDATE org SET name = 'Cafeneaua Exemplu' WHERE domain = 'proseware.example'`)
	return roastery, unclear
}

// placeTypeOpenLeads is every open lead as leads rate reads it, in its order.
func placeTypeOpenLeads(t *testing.T) []leadstore.RateLead {
	t.Helper()
	leads, err := leadstoretest.Q(t).LeadsToRate(context.Background(), leadstore.ListContext{})
	if err != nil {
		t.Fatal(err)
	}
	return leads
}

// placeTypeState is the state the place-type question reads for lead id.
func placeTypeState(t *testing.T, id int64) string {
	t.Helper()
	for _, l := range placeTypeOpenLeads(t) {
		if l.LeadID == id {
			return leadrate.State(rateInput(l, leadconfig.Config{}))
		}
	}
	t.Fatalf("lead %d is not open", id)
	return ""
}

// placeTypeRun is one rating run, as leads rate makes it, with decider d named kind, at most
// maxCalls calls and a leads cap of dailyCap calls; it returns the first two passes' summaries.
func placeTypeRun(t *testing.T, d ai.Decider, kind string, maxCalls, dailyCap int) (rateSummary, placeSummary) {
	t.Helper()
	sum, ps, _ := rateRunAll(t, d, kind, maxCalls, dailyCap)
	return sum, ps
}

// rateRunAll is placeTypeRun with the summaries of all three passes.
func rateRunAll(t *testing.T, d ai.Decider, kind string, maxCalls, dailyCap int) (rateSummary, placeSummary, businessSummary) {
	t.Helper()
	ctx := context.Background()
	q := store.NewQueries(storetest.AppConn(t))
	runID, err := q.StartRun(ctx, commandLeadsRate, kind)
	if err != nil {
		t.Fatal(err)
	}
	loc, err := time.LoadLocation("Europe/Bucharest")
	if err != nil {
		t.Fatal(err)
	}
	rr := rateRun{
		lq:      leadstoretest.Q(t),
		decider: &ai.Capped{Decider: d, Store: q, DailyCalls: dailyCap, Loc: loc, Purpose: ai.PurposeLeads},
		kind:    kind, name: d.Name(), runID: runID, max: maxCalls,
	}
	sum, ps, ks, err := rr.rateAll(ctx, placeTypeOpenLeads(t))
	if err != nil {
		t.Fatal(err)
	}
	return sum, ps, ks
}

// placeTypeRows counts lead_place_type rows matching where.
func placeTypeRows(t *testing.T, where string) int {
	t.Helper()
	return dbCount(t, `SELECT count(*) FROM lead_place_type WHERE `+where)
}

// TestPlaceTypeColdStartAndRerun: the first run asks Jev about each open café lead once and
// stores roastery and unclear as Jev rows with the question and a 32-byte hash; an unclear
// answer leaves the effective type at the default. A rerun asks nothing and writes nothing;
// a changed input asks about that lead only. A Drupal lead is never asked or stored.
func TestPlaceTypeColdStartAndRerun(t *testing.T) {
	roasteryID, unclearID := placeTypeScene(t)
	stub := placeTypeStub(placeTypeState(t, unclearID))

	sum, ps := placeTypeRun(t, stub, rateDeciderJev, defaultRateMax, 1000)
	if sum.Rated != 8 || !ps.Ran || ps.Calls != 2 || ps.Roastery != 1 || ps.Unclear != 1 || ps.Cafe != 0 || ps.Due != 0 || ps.StopReason != "" {
		t.Fatalf("first run: relevance %+v, place type %+v", sum, ps)
	}
	if n := placeTypeRows(t, `source = 'jev' AND actor = 'system' AND detail = 'place-type-cafe@1' AND length(input_sha256) = 32`); n != 2 {
		t.Errorf("%d Jev rows as written, want 2", n)
	}
	if n := placeTypeRows(t, `lead_id NOT IN (`+strconv.FormatInt(roasteryID, 10)+`, `+strconv.FormatInt(unclearID, 10)+`)`); n != 0 {
		t.Errorf("%d rows on Drupal leads, want 0", n)
	}
	got, err := leadstoretest.Q(t).PlaceTypes(context.Background(), []int64{roasteryID, unclearID})
	if err != nil {
		t.Fatal(err)
	}
	if r := got[roasteryID]; r.Type != leadstore.PlaceRoastery || r.Source != leadstore.PlaceFromJev {
		t.Errorf("roastery lead's effective type = %+v, want roastery from jev", r)
	}
	if r := got[unclearID]; r.Type != leadstore.PlaceCafe || r.Source != "" {
		t.Errorf("unclear lead's effective type = %+v, want the default cafe with no source", r)
	}

	calls := stub.Calls()
	_, ps = placeTypeRun(t, stub, rateDeciderJev, defaultRateMax, 1000)
	if ps.Calls != 0 || ps.Unchanged != 2 || ps.Due != 0 || stub.Calls() != calls {
		t.Errorf("rerun: %+v and %d decider calls, want none asked and 2 unchanged", ps, stub.Calls()-calls)
	}
	if n := placeTypeRows(t, `true`); n != 2 {
		t.Errorf("%d rows after the rerun, want still 2", n)
	}

	setLeadKind(t, "lucerne.example", cafe.KindHasLoyalty) // the state's lead type line changes
	_, ps = placeTypeRun(t, stub, rateDeciderJev, defaultRateMax, 1000)
	if ps.Calls != 1 || ps.Roastery != 1 || ps.Unchanged != 1 {
		t.Errorf("after a changed input: %+v, want that lead asked again and the other unchanged", ps)
	}
	if n := placeTypeRows(t, `lead_id = `+strconv.FormatInt(roasteryID, 10)); n != 2 {
		t.Errorf("%d rows on the changed lead, want 2", n)
	}
}

// TestPlaceTypeSharesTheCap: relevance goes first and keeps its budget; the place-type pass
// uses only what is left of --max and the leads cap, and does not run at all when relevance
// stopped short.
func TestPlaceTypeSharesTheCap(t *testing.T) {
	placeTypeScene(t)
	stub := placeTypeStub()

	sum, ps := placeTypeRun(t, stub, rateDeciderJev, 3, 1000)
	if sum.Rated != 3 || sum.StopReason != rateStopMax || ps.Ran {
		t.Fatalf("--max 3: relevance %+v, place type %+v; want relevance stopped and no place-type pass", sum, ps)
	}
	if n := placeTypeRows(t, `true`); n != 0 {
		t.Errorf("%d place-type rows while relevance was short, want 0", n)
	}

	// Five leads still need relevance, so --max 6 leaves the place-type pass one call.
	sum, ps = placeTypeRun(t, stub, rateDeciderJev, 6, 1000)
	if sum.Rated != 5 || sum.StopReason != "" || ps.Calls != 1 || ps.Due != 1 || ps.StopReason != rateStopMax {
		t.Fatalf("--max 6: relevance %+v, place type %+v; want 5 rated, then 1 place-type call", sum, ps)
	}

	// The leads cap: 9 calls were made today, so a cap of 10 leaves the last café lead one.
	_, ps = placeTypeRun(t, stub, rateDeciderJev, defaultRateMax, 10)
	if ps.Calls != 1 || ps.Due != 0 || ps.Unchanged != 1 {
		t.Fatalf("cap 10: %+v, want the last café lead asked", ps)
	}
	_, ps = placeTypeRun(t, stub, rateDeciderJev, defaultRateMax, 10)
	if ps.Calls != 0 || ps.StopReason != "" || ps.Unchanged != 2 {
		t.Errorf("rerun at the cap: %+v, want nothing to ask", ps)
	}
	if n := placeTypeRows(t, `true`); n != 2 {
		t.Errorf("%d place-type rows, want 2", n)
	}
}

// TestPlaceTypeCapSpent: with the leads cap spent by relevance, the place-type pass makes no
// call and stops at the cap.
func TestPlaceTypeCapSpent(t *testing.T) {
	placeTypeScene(t)
	sum, ps := placeTypeRun(t, placeTypeStub(), rateDeciderJev, defaultRateMax, 8)
	if sum.Rated != 8 || ps.Calls != 0 || ps.StopReason != "spend_cap" || ps.Due != 2 {
		t.Errorf("relevance %+v, place type %+v; want the cap spent by relevance", sum, ps)
	}
	if n := placeTypeRows(t, `true`); n != 0 {
		t.Errorf("%d place-type rows, want 0", n)
	}
}

// TestPlaceTypeFakeStoresNothing: the fake decider, through the command and directly, rates
// relevance but asks and stores no place type, and the summary has no place-type line.
func TestPlaceTypeFakeStoresNothing(t *testing.T) {
	placeTypeScene(t)
	out := mustRun(t, "leads", "rate", "--decider", "fake")
	wantLines(t, "fake rate", out, "· decider fake · rated 8 · unchanged 0 · stop none")
	if strings.Contains(out, "place type:") {
		t.Errorf("a fake run printed a place-type line:\n%s", out)
	}
	if _, ps := placeTypeRun(t, placeTypeStub(), rateDeciderFake, defaultRateMax, 1000); ps.Ran {
		t.Errorf("a fake run ran the place-type pass: %+v", ps)
	}
	if n := placeTypeRows(t, `true`); n != 0 {
		t.Errorf("%d place-type rows from the fake decider, want 0", n)
	}
}
