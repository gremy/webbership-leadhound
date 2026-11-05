//go:build integration

package leadstore_test

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/leadstore"
)

// holdCase is one source combination of the hold fixture: the place-type rows written in
// order, and whether they hold the lead while roasteries are off.
type holdCase struct {
	name string
	rows []placeAnswer
	held bool
}

// holdCases holds every source combination the hold must read: each source alone, an unclear
// Jev answer over a name row, an operator café over a name roastery, and no row at all.
var holdCases = []holdCase{
	{"operator", []placeAnswer{{leadstore.PlaceRoastery, leadstore.PlaceFromOperator}}, true},
	{"jev", []placeAnswer{{leadstore.PlaceRoastery, leadstore.PlaceFromJev}}, true},
	{"name", []placeAnswer{{leadstore.PlaceRoastery, leadstore.PlaceFromName}}, true},
	{"unclear-over-name", []placeAnswer{{leadstore.PlaceRoastery, leadstore.PlaceFromName}, {leadstore.PlaceUnclear, leadstore.PlaceFromJev}}, true},
	{"operator-cafe-over-name", []placeAnswer{{leadstore.PlaceRoastery, leadstore.PlaceFromName}, {leadstore.PlaceCafe, leadstore.PlaceFromOperator}}, false},
	{"no-rows", nil, false},
}

// holdFixture is the hold fixture's leads: for each case a new café lead (a candidate) and a
// café lead whose call is due (a call-queue row), plus a Drupal lead carrying a stray
// roastery row.
type holdFixture struct {
	outreachFixture
	fresh, called map[string]int64
	drupal        int64
}

// writePlace appends rows to lead's place type, in order.
func (f outreachFixture) writePlace(t *testing.T, lead int64, rows []placeAnswer) {
	t.Helper()
	for _, r := range rows {
		var sha []byte
		if r.source == leadstore.PlaceFromJev {
			sha = placeSHA
		}
		if err := f.q.SetPlaceType(context.Background(), lead, r.placeType, r.source, "seen", sha, leadstore.ActorSystem); err != nil {
			t.Fatalf("set %v on %d: %v", r, lead, err)
		}
	}
}

func newHoldFixture(t *testing.T) holdFixture {
	t.Helper()
	h := holdFixture{outreachFixture: newOutreachFixture(t), fresh: map[string]int64{}, called: map[string]int64{}}
	for _, c := range holdCases {
		d := "fresh-" + c.name + ".localhost"
		h.fresh[c.name] = h.cafeLead(t, d, nil, "info@"+d)
		h.writePlace(t, h.fresh[c.name], c.rows)
		d = "called-" + c.name + ".localhost"
		s := h.seqBatch(t, false, h.cafeLead(t, d, nil, "info@"+d))
		h.send(t, s, false, 0, day(0))
		h.send(t, s, false, 1, day(3))
		h.called[c.name] = s.lead
		h.writePlace(t, s.lead, c.rows)
	}
	h.drupal = h.siteLead(t, "drupal-hold.localhost", nil, 10, "info@drupal-hold.localhost")
	h.ownerRun(t, `INSERT INTO lead_place_type (lead_id, place_type, source, actor) VALUES ($1, 'roastery', 'operator', 'gremy')`, h.drupal)
	return h
}

// all is every lead of the fixture.
func (h holdFixture) all() []int64 {
	out := []int64{h.drupal}
	for _, c := range holdCases {
		out = append(out, h.fresh[c.name], h.called[c.name])
	}
	return out
}

// check asserts that HeldLeads, CandidateLeads, NextUp and CallQueue agree: the held leads are
// exactly want's, the candidates are the fresh leads not held plus the Drupal lead, and the
// call queue holds the called leads not held.
func (h holdFixture) check(t *testing.T, want map[string]bool) {
	t.Helper()
	ctx := context.Background()
	var wantHeld, wantCands, wantCalls []int64
	for _, c := range holdCases {
		if want[c.name] {
			wantHeld = append(wantHeld, h.fresh[c.name], h.called[c.name])
			continue
		}
		wantCands = append(wantCands, h.fresh[c.name])
		wantCalls = append(wantCalls, h.called[c.name])
	}
	wantCands = append(wantCands, h.drupal)

	held, err := h.q.HeldLeads(ctx, append(h.all(), 999_999))
	if err != nil {
		t.Fatal(err)
	}
	var gotHeld []int64
	for id, ok := range held {
		if !ok {
			t.Errorf("lead %d is in the map as false", id)
		}
		gotHeld = append(gotHeld, id)
	}
	if !slices.Equal(sortedIDs(gotHeld...), sortedIDs(wantHeld...)) {
		t.Errorf("held %v, want %v", sortedIDs(gotHeld...), sortedIDs(wantHeld...))
	}

	o := leadstore.CandidateOptions{IncludeReserved: true}
	cands, err := h.q.CandidateLeads(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	next, err := h.q.NextUp(ctx, o, 50)
	if err != nil {
		t.Fatal(err)
	}
	for name, got := range map[string][]leadstore.Candidate{"CandidateLeads": cands, "NextUp": next} {
		var ids []int64
		for _, c := range got {
			ids = append(ids, c.LeadID)
		}
		if !slices.Equal(sortedIDs(ids...), sortedIDs(wantCands...)) {
			t.Errorf("%s %v, want %v", name, sortedIDs(ids...), sortedIDs(wantCands...))
		}
	}

	rows, err := h.q.CallQueue(ctx, day(5).Add(3*time.Hour), time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	var calls []int64
	for _, r := range rows {
		calls = append(calls, r.LeadID)
	}
	if !slices.Equal(sortedIDs(calls...), sortedIDs(wantCalls...)) {
		t.Errorf("CallQueue %v, want %v", sortedIDs(calls...), sortedIDs(wantCalls...))
	}
}

// heldByDefault is each case's hold while roasteries are off.
func heldByDefault() map[string]bool {
	out := map[string]bool{}
	for _, c := range holdCases {
		out[c.name] = c.held
	}
	return out
}

// TestPlaceHoldSources: with roasteries off (no switch row), a roastery from any source is held
// out of the candidates, NextUp and the call queue, and HeldLeads names exactly those leads; an
// unclear Jev answer leaves a name roastery held, an operator café frees one, and a Drupal
// lead with a stray roastery row is never held.
func TestPlaceHoldSources(t *testing.T) {
	h := newHoldFixture(t)
	h.check(t, heldByDefault())
}

// TestPlaceHoldSwitch: switching roasteries on brings every held lead back on every road;
// switching them off again holds them again.
func TestPlaceHoldSwitch(t *testing.T) {
	h := newHoldFixture(t)
	ctx := context.Background()
	if _, err := h.q.SetRoasteryOutreach(ctx, 0, true, leadstore.ActorGremy); err != nil {
		t.Fatal(err)
	}
	h.check(t, map[string]bool{})
	if _, err := h.q.SetRoasteryOutreach(ctx, 0, false, leadstore.ActorGremy); err != nil {
		t.Fatal(err)
	}
	h.check(t, heldByDefault())
}

// TestPlaceHoldOperatorCorrection: an operator's correction back to café frees a lead held by
// any source, on every road.
func TestPlaceHoldOperatorCorrection(t *testing.T) {
	h := newHoldFixture(t)
	want := heldByDefault()
	for _, c := range holdCases {
		if !c.held {
			continue
		}
		h.operatorPlaceType(t, h.fresh[c.name], leadstore.PlaceCafe)
		h.operatorPlaceType(t, h.called[c.name], leadstore.PlaceCafe)
		want[c.name] = false
		t.Run(c.name, func(t *testing.T) { h.check(t, want) })
	}
}

// TestPlaceHoldStarred: a starred roastery held out of outreach is out of the starred status
// with the roastery words; a starred café is ready; switching roasteries on makes the roastery
// ready too.
func TestPlaceHoldStarred(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	roastery := f.cafeLead(t, "starred-roastery.localhost", nil, "info@starred-roastery.localhost")
	plain := f.cafeLead(t, "starred-cafe.localhost", nil, "info@starred-cafe.localhost")
	f.operatorPlaceType(t, roastery, leadstore.PlaceRoastery)
	f.star(t, roastery)
	f.star(t, plain)
	o := leadstore.StarredStatusOptions{CandidateOptions: leadstore.CandidateOptions{IncludeReserved: true}}
	reasons := func() map[int64]string {
		t.Helper()
		st, err := f.q.StarredStatus(ctx, o)
		if err != nil {
			t.Fatal(err)
		}
		out := map[int64]string{}
		for _, s := range st {
			out[s.LeadID] = fmt.Sprintf("%s|%s", s.Reason, s.Detail)
		}
		return out
	}
	got := reasons()
	if want := leadstore.StarredOther + "|" + leadstore.StarredRoasteryWords; got[roastery] != want {
		t.Errorf("roastery %q, want %q", got[roastery], want)
	}
	if got[plain] != leadstore.StarredReady+"|" {
		t.Errorf("café %q, want ready", got[plain])
	}
	if _, err := f.q.SetRoasteryOutreach(ctx, 0, true, leadstore.ActorGremy); err != nil {
		t.Fatal(err)
	}
	if got := reasons(); got[roastery] != leadstore.StarredReady+"|" {
		t.Errorf("roastery with roasteries on %q, want ready", got[roastery])
	}
}
