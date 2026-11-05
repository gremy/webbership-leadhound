//go:build integration

package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/gremy/webbership-leadhound/internal/cafe"
	"github.com/gremy/webbership-leadhound/internal/console"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/leadstore/leadstoretest"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

// triageScene is a fresh order scene with a backend and an act that runs one action through
// LeadAct and returns its message.
func triageScene(t *testing.T) func(do string, id int64) (string, error) {
	t.Helper()
	orderScene(t)
	b := leadsBackend(t)
	return func(do string, id int64) (string, error) {
		out, err := b.LeadAct(context.Background(), console.LeadAction{Do: do, ID: id})
		return out.Message, err
	}
}

// triageWords fails unless the action succeeded with a message holding want.
func triageWords(t *testing.T, step, msg string, err error, want string) {
	t.Helper()
	if err != nil || !strings.Contains(msg, want) {
		t.Errorf("%s: %q, %v, want %q", step, msg, err, want)
	}
}

// triageRefusal fails unless the action was refused with words holding want.
func triageRefusal(t *testing.T, step string, err error, want string) {
	t.Helper()
	if !errors.Is(err, console.ErrUserFixable) || !strings.Contains(err.Error(), want) {
		t.Errorf("%s: %v, want a refusal saying %q", step, err, want)
	}
}

// triageState is lead id's state as the owner reads it.
func triageState(t *testing.T, id int64) string {
	t.Helper()
	var s string
	if err := storetest.OwnerConn(t).QueryRow(context.Background(), `SELECT state FROM lead WHERE id = $1`, id).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

// triageCalls is how many lead_label rows lead id has.
func triageCalls(t *testing.T, id int64) int {
	t.Helper()
	var n int
	if err := storetest.OwnerConn(t).QueryRow(context.Background(), `SELECT count(*)::int FROM lead_label WHERE lead_id = $1`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestLeadActTriageSkipCafe: a skip of a café lead saves the call "no" with the lead's facts,
// a second skip is refused and changes nothing, and the undo clears the call the skip saved.
func TestLeadActTriageSkipCafe(t *testing.T) {
	act := triageScene(t)
	id := setLeadKind(t, "lucerne.example", cafe.KindNoDigitalLoyalty)

	msg, err := act(console.LeadSkip, id)
	triageWords(t, "skip", msg, err, "Skipped lucerne.example, and saved your call on it: no.")
	band, in, ok := latestCall(t, id)
	if !ok || band != leadstore.BandNo || in.Kind != cafe.KindNoDigitalLoyalty {
		t.Errorf("after the skip the call is %q %v with kind %q", band, ok, in.Kind)
	}
	_, err = act(console.LeadSkip, id)
	triageRefusal(t, "a second skip", err, "It is already skipped.")
	if n := triageCalls(t, id); n != 1 {
		t.Errorf("a second skip left %d calls", n)
	}

	msg, err = act(console.LeadUnskip, id)
	triageWords(t, "undo", msg, err, "lucerne.example is back as a new lead, and the call of no that the skip saved is cleared.")
	if s := triageState(t, id); s != leadstore.LeadNew {
		t.Errorf("after the undo the lead is %s", s)
	}
	if band, _, ok := latestCall(t, id); !ok || band != "" {
		t.Errorf("after the undo the call is %q %v, want cleared", band, ok)
	}
	_, err = act(console.LeadUnskip, id)
	triageRefusal(t, "an undo of a lead not skipped", err, "lucerne.example is not skipped")
}

// TestLeadActTriageSkipKeepsCall: a skip of a lead with a call keeps that call, and the undo
// keeps a call the operator made since the skip.
func TestLeadActTriageSkipKeepsCall(t *testing.T) {
	act := triageScene(t)
	ctx := context.Background()
	id := setLeadKind(t, "lucerne.example", cafe.KindNoDigitalLoyalty)
	strong := leadstore.BandStrong
	if _, err := leadstoretest.Q(t).SetLeadLabel(ctx, id, &strong, []byte(`{"kind":"x"}`), leadstore.ActorOperator); err != nil {
		t.Fatal(err)
	}
	msg, err := act(console.LeadSkip, id)
	triageWords(t, "skip", msg, err, "your call on it stays as it was")
	if band, _, _ := latestCall(t, id); band != leadstore.BandStrong {
		t.Errorf("the skip changed the call to %q", band)
	}
	msg, err = act(console.LeadUnskip, id)
	triageWords(t, "undo", msg, err, "no call was cleared")
	if band, _, _ := latestCall(t, id); band != leadstore.BandStrong {
		t.Errorf("the undo changed the call to %q", band)
	}
}

// TestLeadActTriageSkipBatched: a batched lead stays batched with no call, and the refusal
// says why; a later state is refused by name, and Good still saves a call on either.
func TestLeadActTriageSkipBatched(t *testing.T) {
	act := triageScene(t)
	id := setLeadKind(t, "lucerne.example", cafe.KindNoDigitalLoyalty)
	ownerExec(t, `UPDATE lead SET state = 'batched' WHERE id = $1`, id)
	_, err := act(console.LeadSkip, id)
	triageRefusal(t, "skip a batched lead", err, "lucerne.example was not skipped; nothing changed. It is already in a batch")
	if s := triageState(t, id); s != leadstore.LeadBatched {
		t.Errorf("the refused skip moved the lead to %s", s)
	}
	if n := triageCalls(t, id); n != 0 {
		t.Errorf("the refused skip wrote %d calls", n)
	}
	msg, err := act(console.LeadGood, id)
	triageWords(t, "good on a batched lead", msg, err, "Saved your call on lucerne.example: likely.")
	if s := triageState(t, id); s != leadstore.LeadBatched {
		t.Errorf("Good moved the lead to %s", s)
	}

	ownerExec(t, `UPDATE lead SET state = 'contacted' WHERE id = $1`, id)
	_, err = act(console.LeadSkip, id)
	triageRefusal(t, "skip a contacted lead", err, "Only a new lead can be skipped")
}

// TestLeadActTriageSystemSkip: a lead the system skipped is not the operator's to undo, and it
// stays skipped with no call written.
func TestLeadActTriageSystemSkip(t *testing.T) {
	act := triageScene(t)
	id := setLeadKind(t, "lucerne.example", cafe.KindNoDigitalLoyalty)
	if moved, err := leadstoretest.Q(t).SkipNewLead(context.Background(), id, "build: fixture"); err != nil || !moved {
		t.Fatalf("system skip: %v %v", moved, err)
	}
	_, err := act(console.LeadUnskip, id)
	triageRefusal(t, "undo a system skip", err, "skipped by the system")
	if s := triageState(t, id); s != leadstore.LeadSkipped {
		t.Errorf("the refused undo moved the lead to %s", s)
	}
}

// TestLeadActTriageGood: Good saves "likely", says so when it already stands, and a strong stays.
func TestLeadActTriageGood(t *testing.T) {
	act := triageScene(t)
	id := setLeadKind(t, "lucerne.example", cafe.KindNoDigitalLoyalty)
	msg, err := act(console.LeadGood, id)
	triageWords(t, "good", msg, err, "Saved your call on lucerne.example: likely.")
	msg, err = act(console.LeadGood, id)
	triageWords(t, "good again", msg, err, "already likely")
	if n := triageCalls(t, id); n != 1 {
		t.Errorf("two Goods left %d calls", n)
	}
	strong := leadstore.BandStrong
	if _, err := leadstoretest.Q(t).SetLeadLabel(context.Background(), id, &strong, []byte(`{"kind":"x"}`), leadstore.ActorOperator); err != nil {
		t.Fatal(err)
	}
	msg, err = act(console.LeadGood, id)
	triageWords(t, "good on a strong", msg, err, "stays strong")
	if band, _, _ := latestCall(t, id); band != leadstore.BandStrong {
		t.Errorf("Good changed a strong to %q", band)
	}
}

// TestLeadActTriageDrupal: a Drupal lead's skip and undo write no call, and Good and the place
// type corrections are refused with nothing written.
func TestLeadActTriageDrupal(t *testing.T) {
	act := triageScene(t)
	id := leadIDOf(t, "lucerne.example")
	msg, err := act(console.LeadSkip, id)
	triageWords(t, "skip", msg, err, "Skipped lucerne.example; no call was saved.")
	msg, err = act(console.LeadUnskip, id)
	triageWords(t, "undo", msg, err, "lucerne.example is back as a new lead; no call was cleared.")
	_, err = act(console.LeadGood, id)
	triageRefusal(t, "good", err, "lucerne.example is not a café lead")
	for _, do := range []string{console.LeadRoastery, console.LeadCafe} {
		_, err = act(do, id)
		triageRefusal(t, do, err, "lucerne.example is not a café lead")
	}
	if n := triageCalls(t, id); n != 0 {
		t.Errorf("a Drupal lead has %d calls", n)
	}
	var rows int
	if err := storetest.OwnerConn(t).QueryRow(context.Background(), `SELECT count(*)::int FROM lead_place_type WHERE lead_id = $1`, id).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Errorf("a Drupal lead has %d place type rows", rows)
	}
}

// TestLeadActTriagePlaceType: the operator's correction writes a row and no call, leaves the
// state alone in any state, and outranks a Jev row and a name row both ways.
func TestLeadActTriagePlaceType(t *testing.T) {
	act := triageScene(t)
	ctx := context.Background()
	lq := leadstoretest.Q(t)
	id := setLeadKind(t, "lucerne.example", cafe.KindNoDigitalLoyalty)
	effective := func() leadstore.PlaceTypeRow {
		t.Helper()
		rows, err := lq.PlaceTypes(ctx, []int64{id})
		if err != nil {
			t.Fatal(err)
		}
		return rows[id]
	}
	sha := make([]byte, 32)
	if err := lq.SetPlaceType(ctx, id, leadstore.PlaceCafe, leadstore.PlaceFromJev, "place-type-cafe@1", sha, leadstore.ActorSystem); err != nil {
		t.Fatal(err)
	}
	if err := lq.SetPlaceType(ctx, id, leadstore.PlaceCafe, leadstore.PlaceFromName, "cafenea", nil, leadstore.ActorSystem); err != nil {
		t.Fatal(err)
	}
	ownerExec(t, `UPDATE lead SET state = 'batched' WHERE id = $1`, id)

	msg, err := act(console.LeadRoastery, id)
	triageWords(t, "roastery", msg, err, "Marked lucerne.example as a roastery or online store")
	if got := effective(); got.Type != leadstore.PlaceRoastery || got.Source != leadstore.PlaceFromOperator {
		t.Errorf("after the operator's roastery: %+v", got)
	}
	if s := triageState(t, id); s != leadstore.LeadBatched {
		t.Errorf("the correction moved the lead to %s", s)
	}
	if n := triageCalls(t, id); n != 0 {
		t.Errorf("the correction wrote %d calls", n)
	}

	// Later rows of Jev and the name do not outrank the operator's.
	if err := lq.SetPlaceType(ctx, id, leadstore.PlaceRoastery, leadstore.PlaceFromName, "roastery", nil, leadstore.ActorSystem); err != nil {
		t.Fatal(err)
	}
	msg, err = act(console.LeadCafe, id)
	triageWords(t, "café", msg, err, "Marked lucerne.example as a coffee shop")
	if got := effective(); got.Type != leadstore.PlaceCafe || got.Source != leadstore.PlaceFromOperator {
		t.Errorf("after the operator's café: %+v", got)
	}
	if err := lq.SetPlaceType(ctx, id, leadstore.PlaceRoastery, leadstore.PlaceFromJev, "place-type-cafe@1", sha, leadstore.ActorSystem); err != nil {
		t.Fatal(err)
	}
	if got := effective(); got.Type != leadstore.PlaceCafe {
		t.Errorf("a later Jev roastery beat the operator's café: %+v", got)
	}
	if n := triageCalls(t, id); n != 0 {
		t.Errorf("the corrections wrote %d calls", n)
	}
}

// TestLeadActTriageMissingLead: every triage action refuses a lead that is not there.
func TestLeadActTriageMissingLead(t *testing.T) {
	act := triageScene(t)
	for _, do := range []string{console.LeadSkip, console.LeadUnskip, console.LeadGood, console.LeadRoastery, console.LeadCafe} {
		_, err := act(do, 999999)
		triageRefusal(t, do, err, "There is no lead 999999.")
	}
}
