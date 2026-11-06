//go:build integration

package leadstore_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/gremy/webbership-leadhound/internal/cafe"
	"github.com/gremy/webbership-leadhound/internal/leadrate"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

// Migration 00032 and SyncIGPlaceTypes: Instagram as a source of a café lead's place type.
// Every name and handle here is invented (privacycheck: handles naming example or exemplu).

// igRealModel is the model of a real Jev answer in these tests: anything but the fake's.
const igRealModel = "jev-test-1"

// igPlaceScene builds Instagram state for the place-type tests on one fixture.
type igPlaceScene struct {
	outreachFixture
	ctx  context.Context
	next byte // the next capture's hash byte, so every capture is its own row
}

func newIGPlaceScene(t *testing.T) *igPlaceScene {
	t.Helper()
	return &igPlaceScene{outreachFixture: newOutreachFixture(t), ctx: context.Background(), next: 1}
}

// capture stores a new capture of handle and returns its profile id.
func (s *igPlaceScene) capture(t *testing.T, handle string) int64 {
	t.Helper()
	s.next++
	id, isNew, err := s.q.InsertIGProfile(s.ctx, igProfile(handle, s.next))
	if err != nil || !isNew {
		t.Fatalf("capture %s: %d %v %v", handle, id, isNew, err)
	}
	return id
}

// judge appends an ig_place answer of model (igRealModel or leadstore.IGFakeModel) on profile.
func (s *igPlaceScene) judge(t *testing.T, profile int64, choice, model string) {
	t.Helper()
	s.next++
	conf := 0.9
	if _, err := s.q.AddIGJudgment(s.ctx, leadstore.IGJudgment{ProfileID: profile, Question: leadstore.IGPlace,
		Choice: choice, RawChoice: choice, Confidence: &conf, Source: leadstore.IGFromJev, Model: model,
		InputSHA: igSHA(s.next)}); err != nil {
		t.Fatalf("judge profile %d %s: %v", profile, choice, err)
	}
}

// says captures handle and gives it real Jev's place choice.
func (s *igPlaceScene) says(t *testing.T, handle, choice string) {
	t.Helper()
	s.judge(t, s.capture(t, handle), choice, igRealModel)
}

// match appends handle's match to lead at tier (the operator's tier is his: actor gremy).
func (s *igPlaceScene) match(t *testing.T, handle string, lead int64, tier string) {
	t.Helper()
	actor := leadstore.ActorSystem
	if tier == leadstore.IGTierOperator {
		actor = leadstore.ActorGremy
	}
	if _, err := s.q.SetIGMatch(s.ctx, leadstore.IGMatch{Handle: handle, LeadID: lead, Tier: tier, Actor: actor}); err != nil {
		t.Fatalf("match %s to %d (%s): %v", handle, lead, tier, err)
	}
}

// place appends one place type row on lead (a Jev row gets its hash).
func (s *igPlaceScene) place(t *testing.T, lead int64, placeType, source string) {
	t.Helper()
	var sha []byte
	if source == leadstore.PlaceFromJev {
		sha = placeSHA
	}
	if err := s.q.SetPlaceType(s.ctx, lead, placeType, source, "seen", sha, leadstore.ActorSystem); err != nil {
		t.Fatalf("set %s from %s on %d: %v", placeType, source, lead, err)
	}
}

// sync runs SyncIGPlaceTypes.
func (s *igPlaceScene) sync(t *testing.T, dryRun bool) leadstore.IGPlaceSync {
	t.Helper()
	got, err := s.q.SyncIGPlaceTypes(s.ctx, dryRun)
	if err != nil {
		t.Fatalf("sync (dry run %v): %v", dryRun, err)
	}
	return got
}

// igRows is lead's instagram rows, oldest first, as "type @ detail".
func (s *igPlaceScene) igRows(t *testing.T, lead int64) []string {
	t.Helper()
	rows, err := s.owner.Query(s.ctx, `SELECT place_type || ' @ ' || detail FROM lead_place_type
		WHERE lead_id = $1 AND source = 'instagram' ORDER BY id`, lead)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// effective is lead's effective place type and its source.
func (s *igPlaceScene) effective(t *testing.T, lead int64) leadstore.PlaceTypeRow {
	t.Helper()
	got, err := s.q.PlaceTypes(s.ctx, []int64{lead})
	if err != nil {
		t.Fatal(err)
	}
	return got[lead]
}

// held reports whether lead is held out of café outreach.
func (s *igPlaceScene) held(t *testing.T, lead int64) bool {
	t.Helper()
	got, err := s.q.HeldLeads(s.ctx, []int64{lead})
	if err != nil {
		t.Fatal(err)
	}
	return got[lead]
}

// invariants: every instagram row has no hash, is the system's, and sits on a café lead.
func (s *igPlaceScene) invariants(t *testing.T) {
	t.Helper()
	kinds := []string{}
	for _, k := range cafe.Kinds {
		kinds = append(kinds, k.Kind)
	}
	if n := ownerInt(t, s.owner, `SELECT count(*) FROM lead_place_type t JOIN lead l ON l.id = t.lead_id
		WHERE t.source = 'instagram' AND (t.input_sha256 IS NOT NULL OR t.actor <> 'system' OR NOT l.kind = ANY($1::text[]))`,
		kinds); n != 0 {
		t.Errorf("%d instagram rows break the invariants (a hash, a non-system actor or a non-café lead)", n)
	}
}

// TestPlaceTypeInstagramRanking: Instagram ranks under the operator and over Jev and the name;
// a roastery from Instagram alone holds the lead; an unclear row from Instagram falls through.
func TestPlaceTypeInstagramRanking(t *testing.T) {
	const (
		cafe, roast, unclear = leadstore.PlaceCafe, leadstore.PlaceRoastery, leadstore.PlaceUnclear
		op, ig, jev, name    = leadstore.PlaceFromOperator, leadstore.PlaceFromInstagram, leadstore.PlaceFromJev, leadstore.PlaceFromName
	)
	s := newIGPlaceScene(t)
	for i, c := range []struct {
		why        string
		rows       []placeAnswer
		want, from string
		held       bool
	}{
		{"instagram café beats a roastery name", []placeAnswer{{roast, name}, {cafe, ig}}, cafe, ig, false},
		{"instagram café beats Jev's roastery", []placeAnswer{{roast, jev}, {cafe, ig}}, cafe, ig, false},
		{"instagram café beats Jev's roastery written after it", []placeAnswer{{cafe, ig}, {roast, jev}}, cafe, ig, false},
		{"the operator's café beats instagram's roastery", []placeAnswer{{roast, ig}, {cafe, op}}, cafe, op, false},
		{"instagram roastery alone holds the lead", []placeAnswer{{roast, ig}}, roast, ig, true},
		{"instagram unclear falls through to Jev", []placeAnswer{{roast, jev}, {unclear, ig}}, roast, jev, true},
		{"instagram unclear falls through to the name", []placeAnswer{{roast, name}, {unclear, ig}}, roast, name, true},
		{"a later instagram unclear hides its older roastery", []placeAnswer{{roast, ig}, {unclear, ig}}, cafe, "", false},
	} {
		t.Run(c.why, func(t *testing.T) {
			lead := s.cafeLead(t, fmt.Sprintf("ig-rank-%d.example", i), nil, "")
			for _, r := range c.rows {
				s.place(t, lead, r.placeType, r.source)
			}
			if got := s.effective(t, lead); got.Type != c.want || got.Source != c.from {
				t.Errorf("got %s from %q, want %s from %q", got.Type, got.Source, c.want, c.from)
			}
			if got := s.held(t, lead); got != c.held {
				t.Errorf("held %v, want %v", got, c.held)
			}
		})
	}
}

// TestPlaceTypeInstagramRefusals: an instagram row has no hash; unclear from instagram is
// allowed, from the name or the operator still refused; the table's checks agree.
func TestPlaceTypeInstagramRefusals(t *testing.T) {
	s := newIGPlaceScene(t)
	lead := s.cafeLead(t, "ig-refusals.example", nil, "")
	ig := leadstore.PlaceFromInstagram
	if err := s.q.SetPlaceType(s.ctx, lead, leadstore.PlaceRoastery, ig, "@a.example: roastery_or_online", nil, leadstore.ActorSystem); err != nil {
		t.Errorf("instagram with no hash: %v", err)
	}
	if err := s.q.SetPlaceType(s.ctx, lead, leadstore.PlaceUnclear, ig, igNoStrongMatchDetail, nil, leadstore.ActorSystem); err != nil {
		t.Errorf("unclear from instagram: %v", err)
	}
	for _, c := range []struct {
		why, placeType, source string
		sha                    []byte
		actor                  string
	}{
		{"instagram with a hash", leadstore.PlaceCafe, ig, placeSHA, leadstore.ActorSystem},
		{"unclear from name", leadstore.PlaceUnclear, leadstore.PlaceFromName, nil, leadstore.ActorSystem},
		{"unclear from operator", leadstore.PlaceUnclear, leadstore.PlaceFromOperator, nil, leadstore.ActorGremy},
	} {
		if err := s.q.SetPlaceType(s.ctx, lead, c.placeType, c.source, "", c.sha, c.actor); !errors.Is(err, leadstore.ErrBadPlaceType) {
			t.Errorf("%s: %v, want ErrBadPlaceType", c.why, err)
		}
	}
	if n := ownerInt(t, s.owner, `SELECT count(*) FROM lead_place_type`); n != 2 {
		t.Errorf("%d rows, want the 2 accepted", n)
	}
	for _, q := range []string{
		`INSERT INTO lead_place_type (lead_id, place_type, source, input_sha256, actor) VALUES ($1, 'cafe', 'instagram', decode(repeat('ab', 32), 'hex'), 'system')`,
		`INSERT INTO lead_place_type (lead_id, place_type, source, actor) VALUES ($1, 'unclear', 'operator', 'gremy')`,
	} {
		if _, err := s.owner.Exec(s.ctx, q, lead); sqlState(err) != "23514" {
			t.Errorf("%s: %v, want 23514 check_violation", q, err)
		}
	}
}

// igNoStrongMatchDetail is the detail Sync writes on a silent row with no strong match.
const igNoStrongMatchDetail = "no strong match"

// TestIGPlaceSyncColdStart: a strongly matched lead Instagram reads as a roastery, with no
// place rows, gets one instagram roastery row and is held; it is reported as Held.
func TestIGPlaceSyncColdStart(t *testing.T) {
	s := newIGPlaceScene(t)
	lead := s.cafeLead(t, "ig-cold.example", nil, "")
	s.says(t, "roastery.example", leadrate.IGRoasteryOrOnline)
	s.match(t, "roastery.example", lead, leadstore.IGTierHandle)
	got := s.sync(t, false)
	if got.Roastery != 1 || got.Cafe != 0 || got.Silent != 0 || len(got.Released) != 0 || len(got.Held) != 1 {
		t.Fatalf("sync: %+v, want one roastery, held", got)
	}
	h := got.Held[0]
	if h.LeadID != lead || h.Handle != "roastery.example" || h.Choice != leadrate.IGRoasteryOrOnline ||
		h.Before.Type != leadstore.PlaceCafe || h.Before.Source != "" ||
		h.After.Type != leadstore.PlaceRoastery || h.After.Source != leadstore.PlaceFromInstagram {
		t.Errorf("held: %+v", h)
	}
	if rows := s.igRows(t, lead); !reflect.DeepEqual(rows, []string{"roastery @ @roastery.example: roastery_or_online"}) {
		t.Errorf("rows %v", rows)
	}
	if !s.held(t, lead) {
		t.Error("the lead is not held")
	}
	s.invariants(t)
}

// TestIGPlaceSyncSteadyState: the cold start's twin; a second sync writes nothing.
func TestIGPlaceSyncSteadyState(t *testing.T) {
	s := newIGPlaceScene(t)
	a := s.cafeLead(t, "ig-steady-a.example", nil, "")
	b := s.cafeLead(t, "ig-steady-b.example", nil, "")
	s.says(t, "roastery.example", leadrate.IGRoasteryOrOnline)
	s.match(t, "roastery.example", a, leadstore.IGTierHandle)
	s.says(t, "seats.example", leadrate.IGCafeSeating)
	s.match(t, "seats.example", b, leadstore.IGTierDomain)
	if got := s.sync(t, false); got.Roastery != 1 || got.Cafe != 1 {
		t.Fatalf("first sync: %+v", got)
	}
	before := ownerInt(t, s.owner, `SELECT count(*) FROM lead_place_type`)
	if got := s.sync(t, false); !reflect.DeepEqual(got, leadstore.IGPlaceSync{}) {
		t.Errorf("second sync: %+v, want nothing", got)
	}
	if after := ownerInt(t, s.owner, `SELECT count(*) FROM lead_place_type`); after != before {
		t.Errorf("rows %d after the second sync, want %d", after, before)
	}
	s.invariants(t)
}

// TestIGPlaceSyncConcurrent: two syncs at once (an import and an operator's match) write each
// row once between them.
func TestIGPlaceSyncConcurrent(t *testing.T) {
	s := newIGPlaceScene(t)
	var leads []int64
	for i := range 5 {
		lead := s.cafeLead(t, fmt.Sprintf("ig-race-%d.example", i), nil, "")
		h := fmt.Sprintf("race%d.example", i)
		s.says(t, h, leadrate.IGRoasteryOrOnline)
		s.match(t, h, lead, leadstore.IGTierHandle)
		leads = append(leads, lead)
	}
	results := make(chan leadstore.IGPlaceSync, 2)
	errs := make(chan error, 2)
	for range 2 {
		q := leadstore.NewQueries(storetest.AppConn(t)) // a connection of its own, as two processes
		go func() {
			got, err := q.SyncIGPlaceTypes(s.ctx, false)
			results <- got
			errs <- err
		}()
	}
	total := 0
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
		total += (<-results).Roastery
	}
	if total != len(leads) {
		t.Errorf("the two syncs wrote %d roastery rows between them, want %d", total, len(leads))
	}
	if n := ownerInt(t, s.owner, `SELECT count(*) FROM lead_place_type WHERE source = 'instagram'`); n != int64(len(leads)) {
		t.Errorf("%d instagram rows, want one per lead (%d)", n, len(leads))
	}
}

// TestIGPlaceSyncSilence: an unclear or not-coffee call, a weak match, a tie, the fake
// decider's answer and a strong match with no call never write a row on a lead Instagram
// never typed.
func TestIGPlaceSyncSilence(t *testing.T) {
	for _, c := range []struct {
		why   string
		setup func(t *testing.T, s *igPlaceScene, lead, other int64)
	}{
		{"unclear", func(t *testing.T, s *igPlaceScene, lead, _ int64) {
			s.says(t, "quiet.example", leadrate.IGUnclear)
			s.match(t, "quiet.example", lead, leadstore.IGTierHandle)
		}},
		{"not coffee", func(t *testing.T, s *igPlaceScene, lead, _ int64) {
			s.says(t, "quiet.example", leadrate.IGNotCoffee)
			s.match(t, "quiet.example", lead, leadstore.IGTierHandle)
		}},
		{"a weak match", func(t *testing.T, s *igPlaceScene, lead, _ int64) {
			s.says(t, "quiet.example", leadrate.IGRoasteryOrOnline)
			s.match(t, "quiet.example", lead, leadstore.IGTierName)
		}},
		{"a tie", func(t *testing.T, s *igPlaceScene, lead, other int64) {
			s.says(t, "quiet.example", leadrate.IGRoasteryOrOnline)
			if _, err := s.q.SetIGMatch(s.ctx, leadstore.IGMatch{Handle: "quiet.example", Tier: leadstore.IGTierName,
				Candidates: []int64{lead, other}, Actor: leadstore.ActorSystem}); err != nil {
				t.Fatal(err)
			}
		}},
		{"a fake-model answer", func(t *testing.T, s *igPlaceScene, lead, _ int64) {
			s.judge(t, s.capture(t, "quiet.example"), leadrate.IGRoasteryOrOnline, leadstore.IGFakeModel)
			s.match(t, "quiet.example", lead, leadstore.IGTierHandle)
		}},
		{"no call at all", func(t *testing.T, s *igPlaceScene, lead, _ int64) {
			s.capture(t, "quiet.example")
			s.match(t, "quiet.example", lead, leadstore.IGTierHandle)
		}},
	} {
		t.Run(c.why, func(t *testing.T) {
			s := newIGPlaceScene(t)
			lead := s.cafeLead(t, "ig-quiet.example", nil, "")
			other := s.cafeLead(t, "ig-quiet-other.example", nil, "")
			c.setup(t, s, lead, other)
			if got := s.sync(t, false); !reflect.DeepEqual(got, leadstore.IGPlaceSync{}) {
				t.Errorf("sync: %+v, want nothing", got)
			}
			if n := ownerInt(t, s.owner, `SELECT count(*) FROM lead_place_type`); n != 0 {
				t.Errorf("%d place rows, want none", n)
			}
		})
	}
}

// TestIGPlaceSyncUnmatch: an operator's unmatch of a typed handle writes a silent row and the
// lead falls back to a café, reported as Released from Instagram's own roastery.
func TestIGPlaceSyncUnmatch(t *testing.T) {
	s := newIGPlaceScene(t)
	lead := s.cafeLead(t, "ig-unmatch.example", nil, "")
	s.says(t, "roastery.example", leadrate.IGRoasteryOrOnline)
	s.match(t, "roastery.example", lead, leadstore.IGTierHandle)
	s.sync(t, false)
	s.match(t, "roastery.example", 0, leadstore.IGTierOperator) // the unmatch
	got := s.sync(t, false)
	if got.Silent != 1 || got.Roastery != 0 || got.Cafe != 0 || len(got.Held) != 0 || len(got.Released) != 1 {
		t.Fatalf("sync after unmatch: %+v", got)
	}
	r := got.Released[0]
	if r.LeadID != lead || r.Handle != "" || r.Before.Source != leadstore.PlaceFromInstagram || r.After.Type != leadstore.PlaceCafe || r.After.Source != "" {
		t.Errorf("released: %+v", r)
	}
	want := []string{"roastery @ @roastery.example: roastery_or_online", "unclear @ " + igNoStrongMatchDetail}
	if rows := s.igRows(t, lead); !reflect.DeepEqual(rows, want) {
		t.Errorf("rows %v, want %v", rows, want)
	}
	if s.held(t, lead) {
		t.Error("still held after the unmatch")
	}
	if got := s.sync(t, false); !reflect.DeepEqual(got, leadstore.IGPlaceSync{}) {
		t.Errorf("a re-sync after the unmatch: %+v, want nothing", got)
	}
	s.invariants(t)
}

// TestIGPlaceSyncRematch: a handle re-matched from lead A to lead B moves the type with it:
// A goes silent (Released), B gets the roastery (Held).
func TestIGPlaceSyncRematch(t *testing.T) {
	s := newIGPlaceScene(t)
	a := s.cafeLead(t, "ig-rematch-a.example", nil, "")
	b := s.cafeLead(t, "ig-rematch-b.example", nil, "")
	s.says(t, "roastery.example", leadrate.IGRoasteryOrOnline)
	s.match(t, "roastery.example", a, leadstore.IGTierDomain)
	s.sync(t, false)
	s.match(t, "roastery.example", b, leadstore.IGTierOperator)
	got := s.sync(t, false)
	if got.Silent != 1 || got.Roastery != 1 || len(got.Released) != 1 || len(got.Held) != 1 ||
		got.Released[0].LeadID != a || got.Held[0].LeadID != b || got.Held[0].Handle != "roastery.example" {
		t.Fatalf("sync after re-match: %+v", got)
	}
	if ea, eb := s.effective(t, a), s.effective(t, b); ea.Type != leadstore.PlaceCafe || eb.Type != leadstore.PlaceRoastery ||
		eb.Source != leadstore.PlaceFromInstagram {
		t.Errorf("A %+v, B %+v", ea, eb)
	}
	s.invariants(t)
}

// TestIGPlaceSyncRealOverFake: the fake decider's café never types the lead; real Jev's
// roastery on a later capture does.
func TestIGPlaceSyncRealOverFake(t *testing.T) {
	s := newIGPlaceScene(t)
	lead := s.cafeLead(t, "ig-real.example", nil, "")
	s.judge(t, s.capture(t, "roastery.example"), leadrate.IGCafeSeating, leadstore.IGFakeModel)
	s.match(t, "roastery.example", lead, leadstore.IGTierHandle)
	if got := s.sync(t, false); !reflect.DeepEqual(got, leadstore.IGPlaceSync{}) {
		t.Fatalf("fake only: %+v, want nothing", got)
	}
	s.says(t, "roastery.example", leadrate.IGRoasteryOrOnline)
	if got := s.sync(t, false); got.Roastery != 1 || len(got.Held) != 1 {
		t.Fatalf("after a real answer: %+v", got)
	}
	if e := s.effective(t, lead); e.Type != leadstore.PlaceRoastery || e.Source != leadstore.PlaceFromInstagram {
		t.Errorf("effective %+v", e)
	}
}

// TestIGPlaceSyncOperatorWins: the operator's café stands over Instagram's roastery row.
func TestIGPlaceSyncOperatorWins(t *testing.T) {
	s := newIGPlaceScene(t)
	lead := s.cafeLead(t, "ig-operator.example", nil, "")
	s.place(t, lead, leadstore.PlaceCafe, leadstore.PlaceFromOperator)
	s.says(t, "roastery.example", leadrate.IGRoasteryOrOnline)
	s.match(t, "roastery.example", lead, leadstore.IGTierHandle)
	got := s.sync(t, false)
	if got.Roastery != 1 || len(got.Held) != 0 || len(got.Released) != 0 {
		t.Fatalf("sync: %+v, want the row written and no change of type", got)
	}
	if e := s.effective(t, lead); e.Type != leadstore.PlaceCafe || e.Source != leadstore.PlaceFromOperator {
		t.Errorf("effective %+v, want the operator's café", e)
	}
	if s.held(t, lead) {
		t.Error("held over the operator's call")
	}
}

// TestIGPlaceSyncNonCafeLead: a Drupal lead with a strong match is never selected.
func TestIGPlaceSyncNonCafeLead(t *testing.T) {
	s := newIGPlaceScene(t)
	drupal := s.siteLead(t, "ig-drupal.example", nil, 7, "")
	s.says(t, "roastery.example", leadrate.IGRoasteryOrOnline)
	s.match(t, "roastery.example", drupal, leadstore.IGTierHandle)
	if got := s.sync(t, false); !reflect.DeepEqual(got, leadstore.IGPlaceSync{}) {
		t.Errorf("sync: %+v, want nothing", got)
	}
	if n := ownerInt(t, s.owner, `SELECT count(*) FROM lead_place_type`); n != 0 {
		t.Errorf("%d place rows on a Drupal lead", n)
	}
}

// TestIGPlaceSyncSuppressedOrg: a lead whose org's domain is suppressed is never selected.
func TestIGPlaceSyncSuppressedOrg(t *testing.T) {
	s := newIGPlaceScene(t)
	lead := s.cafeLead(t, "ig-gone.example", nil, "")
	s.says(t, "roastery.example", leadrate.IGRoasteryOrOnline)
	s.match(t, "roastery.example", lead, leadstore.IGTierHandle)
	if _, err := s.q.Suppress(s.ctx, leadstore.DefaultAccountID, leadstore.SuppressDomain, "ig-gone.example",
		leadstore.ReasonManual, "forget: test"); err != nil {
		t.Fatal(err)
	}
	if got := s.sync(t, false); !reflect.DeepEqual(got, leadstore.IGPlaceSync{}) {
		t.Errorf("sync: %+v, want nothing", got)
	}
	if rows := s.igRows(t, lead); len(rows) != 0 {
		t.Errorf("rows %v on a suppressed org", rows)
	}
}

// TestIGPlaceSyncSameHandleAsCard: with a weak handle, a strong one, one re-matched away and
// then a newer strong one, Sync types the lead from the same handle its card shows
// (IGForLead), both reading igLeadHandleSQL.
func TestIGPlaceSyncSameHandleAsCard(t *testing.T) {
	s := newIGPlaceScene(t)
	lead := s.cafeLead(t, "ig-card.example", nil, "")
	other := s.cafeLead(t, "ig-card-other.example", nil, "")
	s.says(t, "first.example", leadrate.IGRoasteryOrOnline)
	s.match(t, "first.example", lead, leadstore.IGTierHandle)
	s.says(t, "moved.example", leadrate.IGCafeSeating)
	s.match(t, "moved.example", lead, leadstore.IGTierDomain)
	s.match(t, "moved.example", other, leadstore.IGTierOperator) // re-matched away
	s.says(t, "weak.example", leadrate.IGCafeSeating)
	s.match(t, "weak.example", lead, leadstore.IGTierName) // the latest, but weak
	check := func(wantHandle string) {
		t.Helper()
		card, err := s.q.IGForLead(s.ctx, lead)
		if err != nil || card == nil {
			t.Fatalf("card: %+v %v", card, err)
		}
		rows := s.igRows(t, lead)
		if card.Match.Handle != wantHandle || len(rows) == 0 || !strings.Contains(rows[len(rows)-1], "@"+card.Match.Handle+":") {
			t.Errorf("card names %s, Sync's latest row is %v; want both %s", card.Match.Handle, rows, wantHandle)
		}
	}
	s.sync(t, false)
	check("first.example")
	s.says(t, "second.example", leadrate.IGCafeSeating)
	s.match(t, "second.example", lead, leadstore.IGTierOperator) // the most recent strong match
	got := s.sync(t, false)
	check("second.example")
	if len(got.Released) != 1 || got.Released[0].Handle != "second.example" {
		t.Errorf("sync: %+v, want the lead released from second.example", got)
	}
}

// TestIGPlaceSyncReleased: Instagram's café on a lead held by its name or by Jev is reported
// as Released, with the source and detail it overrode.
func TestIGPlaceSyncReleased(t *testing.T) {
	s := newIGPlaceScene(t)
	byName := s.cafeLead(t, "ig-by-name.example", nil, "")
	byJev := s.cafeLead(t, "ig-by-jev.example", nil, "")
	s.place(t, byName, leadstore.PlaceRoastery, leadstore.PlaceFromName)
	s.place(t, byJev, leadstore.PlaceRoastery, leadstore.PlaceFromJev)
	s.says(t, "seats.example", leadrate.IGCafeSeating)
	s.match(t, "seats.example", byName, leadstore.IGTierHandle)
	s.says(t, "seats.exemplu", leadrate.IGCafeSeating)
	s.match(t, "seats.exemplu", byJev, leadstore.IGTierHandle)
	got := s.sync(t, false)
	if got.Cafe != 2 || len(got.Held) != 0 || len(got.Released) != 2 {
		t.Fatalf("sync: %+v", got)
	}
	from := map[int64]string{}
	for _, r := range got.Released {
		if r.Before.Type != leadstore.PlaceRoastery || r.Before.Detail != "seen" || r.After.Type != leadstore.PlaceCafe ||
			r.After.Source != leadstore.PlaceFromInstagram || r.Choice != leadrate.IGCafeSeating {
			t.Errorf("released: %+v", r)
		}
		from[r.LeadID] = r.Before.Source
	}
	if from[byName] != leadstore.PlaceFromName || from[byJev] != leadstore.PlaceFromJev {
		t.Errorf("released from %v", from)
	}
	if s.held(t, byName) || s.held(t, byJev) {
		t.Error("a released lead is still held")
	}
}

// TestIGPlaceSyncDryRun: a dry run writes no row and reports exactly what the real run then
// writes (a roastery, a café and a silent row in one run).
func TestIGPlaceSyncDryRun(t *testing.T) {
	s := newIGPlaceScene(t)
	roast := s.cafeLead(t, "ig-dry-roast.example", nil, "")
	seats := s.cafeLead(t, "ig-dry-seats.example", nil, "")
	gone := s.cafeLead(t, "ig-dry-gone.example", nil, "")
	s.place(t, seats, leadstore.PlaceRoastery, leadstore.PlaceFromName)
	s.place(t, gone, leadstore.PlaceRoastery, leadstore.PlaceFromInstagram) // typed once, no match now
	s.says(t, "roastery.example", leadrate.IGRoasteryOrOnline)
	s.match(t, "roastery.example", roast, leadstore.IGTierHandle)
	s.says(t, "seats.example", leadrate.IGCafeSeating)
	s.match(t, "seats.example", seats, leadstore.IGTierHandle)
	before := ownerInt(t, s.owner, `SELECT count(*) FROM lead_place_type`)
	dry := s.sync(t, true)
	if n := ownerInt(t, s.owner, `SELECT count(*) FROM lead_place_type`); n != before {
		t.Fatalf("a dry run wrote %d rows", n-before)
	}
	if dry.Roastery != 1 || dry.Cafe != 1 || dry.Silent != 1 || len(dry.Held) != 1 || len(dry.Released) != 2 {
		t.Fatalf("dry run: %+v", dry)
	}
	real := s.sync(t, false)
	if !reflect.DeepEqual(dry, real) {
		t.Errorf("dry run %+v, real run %+v", dry, real)
	}
	if n := ownerInt(t, s.owner, `SELECT count(*) FROM lead_place_type`); n != before+3 {
		t.Errorf("the real run wrote %d rows, want 3", n-before)
	}
	s.invariants(t)
}

// TestIGPlaceSyncGrid: the latest instagram row (none, roastery, café, unclear) × what
// Instagram says now (roastery, café, silent), one cell each: a row is written only where the
// two differ, none reading as silent.
func TestIGPlaceSyncGrid(t *testing.T) {
	const (
		cafe, roast, unclear = leadstore.PlaceCafe, leadstore.PlaceRoastery, leadstore.PlaceUnclear
	)
	choiceOf := map[string]string{roast: leadrate.IGRoasteryOrOnline, cafe: leadrate.IGCafeSeating, unclear: leadrate.IGUnclear}
	for _, c := range []struct {
		why         string
		latest, now string // latest "" is no instagram row
		write       bool
	}{
		{"none, roastery now: write", "", roast, true},
		{"none, café now: write", "", cafe, true},
		{"none, silent now: writes nothing", "", unclear, false},
		{"roastery, roastery now: nothing", roast, roast, false},
		{"roastery, café now: write", roast, cafe, true},
		{"roastery, silent now: write silent", roast, unclear, true},
		{"café, roastery now: write", cafe, roast, true},
		{"café, café now: nothing", cafe, cafe, false},
		{"café, silent now: write silent", cafe, unclear, true},
		{"silent, roastery now: write", unclear, roast, true},
		{"silent, café now: write", unclear, cafe, true},
		{"silent, silent now: writes nothing", unclear, unclear, false},
	} {
		t.Run(c.why, func(t *testing.T) {
			s := newIGPlaceScene(t)
			lead := s.cafeLead(t, "ig-grid.example", nil, "")
			if c.latest != "" {
				s.place(t, lead, c.latest, leadstore.PlaceFromInstagram)
			}
			s.says(t, "grid.example", choiceOf[c.now])
			s.match(t, "grid.example", lead, leadstore.IGTierHandle)
			before := s.igRows(t, lead)
			got := s.sync(t, false)
			rows := s.igRows(t, lead)
			n := got.Roastery + got.Cafe + got.Silent
			if !c.write {
				if n != 0 || len(rows) != len(before) {
					t.Errorf("sync %+v, rows %v, want nothing written", got, rows)
				}
				return
			}
			want := c.now + " @ @grid.example: " + choiceOf[c.now]
			if n != 1 || len(rows) != len(before)+1 || rows[len(rows)-1] != want {
				t.Errorf("sync %+v, rows %v, want one row %q", got, rows, want)
			}
			s.invariants(t)
		})
	}
}

// TestPlaceTypeInstagramConstants: leadrate's place choices are leadstore's strings, so the
// question and the sync can never disagree (they are defined from them).
func TestPlaceTypeInstagramConstants(t *testing.T) {
	if leadrate.IGCafeSeating != leadstore.IGPlaceCafeSeating || leadrate.IGRoasteryOrOnline != leadstore.IGPlaceRoasteryOrOnline {
		t.Error("leadrate's place choices drifted from leadstore's")
	}
}

// TestMigrationInstagramPlaceTypeDownUp: 00032 down refuses while an instagram row exists and
// moves nothing; with none, down restores the old checks (no instagram source, unclear from
// Jev only) and up allows them again.
func TestMigrationInstagramPlaceTypeDownUp(t *testing.T) {
	const version = 32 // 00032_instagram_place_type.sql
	s := newIGPlaceScene(t)
	owner := s.owner
	lead := s.cafeLead(t, "ig-down-up.example", nil, "")
	s.place(t, lead, leadstore.PlaceRoastery, leadstore.PlaceFromInstagram)

	p := leadsProvider(t)
	t.Cleanup(func() { migrateAll(t) })
	_, err := p.DownTo(s.ctx, version-1)
	if err == nil || !strings.Contains(err.Error(), "erase or move them first") {
		t.Fatalf("down with an instagram row: %v, want refused", err)
	}
	if n := ownerInt(t, owner, `SELECT count(*) FROM goose_db_version_leads WHERE version_id = $1`, version); n != 1 {
		t.Fatalf("a refused down moved the version (%d rows at %d)", n, version)
	}
	if n := ownerInt(t, owner, `SELECT count(*) FROM lead_place_type WHERE source = 'instagram'`); n != 1 {
		t.Fatal("a refused down moved the instagram row")
	}

	s.ownerRun(t, `DELETE FROM lead_place_type WHERE source = 'instagram'`)
	if _, err := p.DownTo(s.ctx, version-1); err != nil {
		t.Fatalf("down to %d: %v", version-1, err)
	}
	for _, q := range []string{
		`INSERT INTO lead_place_type (lead_id, place_type, source, actor) VALUES ($1, 'cafe', 'instagram', 'system')`,
		`INSERT INTO lead_place_type (lead_id, place_type, source, actor) VALUES ($1, 'unclear', 'instagram', 'system')`,
	} {
		if _, err := owner.Exec(s.ctx, q, lead); sqlState(err) != "23514" {
			t.Errorf("after down, %s: %v, want 23514", q, err)
		}
	}
	s.ownerRun(t, `INSERT INTO lead_place_type (lead_id, place_type, source, input_sha256, actor)
		VALUES ($1, 'unclear', 'jev', decode(repeat('ab', 32), 'hex'), 'system')`, lead)

	migrateAll(t)
	assertLeadPrivileges(t, owner, "after 00032 down and up")
	for _, q := range []string{
		`INSERT INTO lead_place_type (lead_id, place_type, source, actor) VALUES ($1, 'cafe', 'instagram', 'system')`,
		`INSERT INTO lead_place_type (lead_id, place_type, source, actor) VALUES ($1, 'unclear', 'instagram', 'system')`,
	} {
		if _, err := owner.Exec(s.ctx, q, lead); err != nil {
			t.Errorf("after up, %s: %v", q, err)
		}
	}
	if _, err := owner.Exec(s.ctx, `INSERT INTO lead_place_type (lead_id, place_type, source, actor) VALUES ($1, 'unclear', 'name', 'system')`, lead); sqlState(err) != "23514" {
		t.Errorf("after up, unclear from name: %v, want 23514", err)
	}
}
