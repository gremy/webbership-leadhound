//go:build integration

package leadstore_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/leadstore/leadstoretest"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

// placeAnswer is one lead_place_type row a case writes, in order.
type placeAnswer struct{ placeType, source string }

// placeSHA is a Jev row's input hash: only its length matters here.
var placeSHA = bytes.Repeat([]byte{0xab}, 32)

// TestPlaceTypeEffective: every combination of sources decides as operator, then Jev unless
// its latest row says unclear, then name, then café; the latest row of a source is the one
// that speaks, and the row returned names the source that decided.
func TestPlaceTypeEffective(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	const (
		cafe, roast, unclear = leadstore.PlaceCafe, leadstore.PlaceRoastery, leadstore.PlaceUnclear
		op, jev, name        = leadstore.PlaceFromOperator, leadstore.PlaceFromJev, leadstore.PlaceFromName
	)
	for i, c := range []struct {
		why        string
		rows       []placeAnswer
		want, from string
	}{
		{"no rows is a café by default", nil, cafe, ""},
		{"name alone", []placeAnswer{{roast, name}}, roast, name},
		{"jev alone", []placeAnswer{{roast, jev}}, roast, jev},
		{"operator alone", []placeAnswer{{roast, op}}, roast, op},
		{"unclear jev alone falls to the default", []placeAnswer{{unclear, jev}}, cafe, ""},
		{"jev beats name", []placeAnswer{{roast, name}, {cafe, jev}}, cafe, jev},
		{"jev beats name, whichever was written last", []placeAnswer{{cafe, jev}, {roast, name}}, cafe, jev},
		{"unclear jev falls through to name", []placeAnswer{{roast, name}, {unclear, jev}}, roast, name},
		{"operator beats jev", []placeAnswer{{roast, jev}, {cafe, op}}, cafe, op},
		{"operator beats name", []placeAnswer{{cafe, op}, {roast, name}}, cafe, op},
		{"operator beats an unclear jev", []placeAnswer{{unclear, jev}, {roast, op}}, roast, op},
		{"operator beats both", []placeAnswer{{cafe, name}, {cafe, jev}, {roast, op}}, roast, op},
		{"the latest operator row wins", []placeAnswer{{roast, op}, {cafe, op}}, cafe, op},
		{"the latest name row wins", []placeAnswer{{roast, name}, {cafe, name}}, cafe, name},
		{"the latest jev row wins", []placeAnswer{{cafe, jev}, {roast, jev}}, roast, jev},
		{"a later unclear hides an older jev answer", []placeAnswer{{roast, jev}, {unclear, jev}}, cafe, ""},
		{"a later unclear hides jev and leaves name", []placeAnswer{{cafe, name}, {roast, jev}, {unclear, jev}}, cafe, name},
		{"a later jev answer replaces unclear", []placeAnswer{{unclear, jev}, {roast, jev}}, roast, jev},
	} {
		t.Run(c.why, func(t *testing.T) {
			lead := f.cafeLead(t, fmt.Sprintf("place-%d.example", i), nil, "")
			for _, r := range c.rows {
				var sha []byte
				if r.source == jev {
					sha = placeSHA
				}
				if err := f.q.SetPlaceType(ctx, lead, r.placeType, r.source, "seen", sha, leadstore.ActorSystem); err != nil {
					t.Fatalf("set %v: %v", r, err)
				}
			}
			got, err := f.q.PlaceTypes(ctx, []int64{lead})
			if err != nil {
				t.Fatal(err)
			}
			if r := got[lead]; r.Type != c.want || r.Source != c.from {
				t.Errorf("got %s from %q, want %s from %q", r.Type, r.Source, c.want, c.from)
			}
		})
	}
}

// TestPlaceTypeDrupalLead: a lead whose rating type is not café reads as an empty type even
// with a stray row, is refused a write, and counts as no place type; a missing lead is not in
// the map and is refused as not found.
func TestPlaceTypeDrupalLead(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	drupal := f.siteLead(t, "drupal-place.example", nil, 7, "")
	err := f.q.SetPlaceType(ctx, drupal, leadstore.PlaceRoastery, leadstore.PlaceFromOperator, "", nil, leadstore.ActorGremy)
	if !errors.Is(err, leadstore.ErrNotCafeLead) {
		t.Fatalf("set on a Drupal lead: %v, want ErrNotCafeLead", err)
	}
	if n := ownerInt(t, f.owner, `SELECT count(*) FROM lead_place_type`); n != 0 {
		t.Fatalf("%d rows after the refusal", n)
	}
	f.ownerRun(t, `INSERT INTO lead_place_type (lead_id, place_type, source, actor) VALUES ($1, 'roastery', 'operator', 'gremy')`, drupal)
	got, err := f.q.PlaceTypes(ctx, []int64{drupal, 999_999})
	if err != nil {
		t.Fatal(err)
	}
	r, ok := got[drupal]
	if !ok || r.Type != "" || r.Source != "" || r.Detail != "" {
		t.Errorf("Drupal lead: %+v (present %v), want an empty row", r, ok)
	}
	if _, ok := got[999_999]; ok {
		t.Error("a missing lead is in the map")
	}
	counts, err := f.q.PlaceTypeCounts(ctx, 0)
	if err != nil || counts != (leadstore.PlaceTypeCounts{}) {
		t.Errorf("counts %+v (%v), want none", counts, err)
	}
	err = f.q.SetPlaceType(ctx, 999_999, leadstore.PlaceRoastery, leadstore.PlaceFromOperator, "", nil, leadstore.ActorGremy)
	if !errors.Is(err, leadstore.ErrLeadNotFound) {
		t.Errorf("set on a missing lead: %v, want ErrLeadNotFound", err)
	}
}

// TestPlaceTypeRefusals: a type, source, actor or hash that does not fit is refused before any
// write, and the table's own checks refuse the same rows from a writer that skips Go.
func TestPlaceTypeRefusals(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	lead := f.cafeLead(t, "refused-place.example", nil, "")
	for _, c := range []struct {
		why, placeType, source string
		sha                    []byte
		actor                  string
		want                   error
	}{
		{"unknown type", "bar", leadstore.PlaceFromName, nil, leadstore.ActorSystem, leadstore.ErrBadPlaceType},
		{"unknown source", leadstore.PlaceCafe, "guess", nil, leadstore.ActorSystem, leadstore.ErrBadPlaceType},
		{"unclear from name", leadstore.PlaceUnclear, leadstore.PlaceFromName, nil, leadstore.ActorSystem, leadstore.ErrBadPlaceType},
		{"unclear from operator", leadstore.PlaceUnclear, leadstore.PlaceFromOperator, nil, leadstore.ActorGremy, leadstore.ErrBadPlaceType},
		{"jev with no hash", leadstore.PlaceCafe, leadstore.PlaceFromJev, nil, leadstore.ActorSystem, leadstore.ErrBadPlaceType},
		{"jev with a short hash", leadstore.PlaceCafe, leadstore.PlaceFromJev, []byte{1}, leadstore.ActorSystem, leadstore.ErrBadPlaceType},
		{"name with a hash", leadstore.PlaceCafe, leadstore.PlaceFromName, placeSHA, leadstore.ActorSystem, leadstore.ErrBadPlaceType},
		{"unknown actor", leadstore.PlaceCafe, leadstore.PlaceFromName, nil, "someone", leadstore.ErrUnknownActor},
	} {
		if err := f.q.SetPlaceType(ctx, lead, c.placeType, c.source, "", c.sha, c.actor); !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", c.why, err, c.want)
		}
	}
	if n := ownerInt(t, f.owner, `SELECT count(*) FROM lead_place_type`); n != 0 {
		t.Errorf("%d rows after the refusals", n)
	}
	for _, q := range []string{
		`INSERT INTO lead_place_type (lead_id, place_type, source, actor) VALUES ($1, 'bar', 'name', 'system')`,
		`INSERT INTO lead_place_type (lead_id, place_type, source, actor) VALUES ($1, 'cafe', 'guess', 'system')`,
		`INSERT INTO lead_place_type (lead_id, place_type, source, actor) VALUES ($1, 'unclear', 'name', 'system')`,
		`INSERT INTO lead_place_type (lead_id, place_type, source, actor) VALUES ($1, 'cafe', 'jev', 'system')`,
		`INSERT INTO lead_place_type (lead_id, place_type, source, input_sha256, actor) VALUES ($1, 'cafe', 'jev', '\x01', 'system')`,
		`INSERT INTO lead_place_type (lead_id, place_type, source, input_sha256, actor) VALUES ($1, 'cafe', 'name', decode(repeat('ab', 32), 'hex'), 'system')`,
		`INSERT INTO lead_place_type (lead_id, place_type, source, actor) VALUES ($1, 'cafe', 'name', 'someone')`,
		`INSERT INTO lead_place_type (lead_id, place_type, source, detail, actor) VALUES ($1, 'cafe', 'name', E'two\nlines', 'system')`,
	} {
		if _, err := f.owner.Exec(ctx, q, lead); sqlState(err) != "23514" {
			t.Errorf("%s: %v, want 23514 check_violation", q, err)
		}
	}
}

// TestPlaceTypeCounts: only open café leads of the campaign count, by effective type; an
// unclear answer on its own is a café, a lead past new and a Drupal lead are in neither, and
// campaign 0 counts every campaign's.
func TestPlaceTypeCounts(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	f.ownerRun(t, `INSERT INTO campaign (id, account_id, slug, name) VALUES (2, 1, 'second', 'Second')`)
	set := func(lead int64, placeType, source string, sha []byte) {
		t.Helper()
		if err := f.q.SetPlaceType(ctx, lead, placeType, source, "", sha, leadstore.ActorSystem); err != nil {
			t.Fatal(err)
		}
	}
	empty, err := f.q.PlaceTypeCounts(ctx, 1)
	if err != nil || empty != (leadstore.PlaceTypeCounts{}) {
		t.Errorf("cold start: %+v (%v)", empty, err)
	}
	f.cafeLead(t, "plain-place.example", nil, "")
	unclear := f.cafeLead(t, "unclear-place.example", nil, "")
	set(unclear, leadstore.PlaceUnclear, leadstore.PlaceFromJev, placeSHA)
	roast := f.cafeLead(t, "roast-place.example", nil, "")
	set(roast, leadstore.PlaceRoastery, leadstore.PlaceFromName, nil)
	skipped := f.cafeLead(t, "skipped-place.example", nil, "")
	set(skipped, leadstore.PlaceRoastery, leadstore.PlaceFromName, nil)
	f.ownerRun(t, `UPDATE lead SET state = 'skipped' WHERE id = $1`, skipped)
	f.siteLead(t, "drupal-counted.example", nil, 7, "")
	other := f.cafeLead(t, "other-campaign.example", nil, "")
	set(other, leadstore.PlaceRoastery, leadstore.PlaceFromOperator, nil)
	f.ownerRun(t, `UPDATE lead SET campaign_id = 2 WHERE id = $1`, other)

	for _, c := range []struct {
		campaign int64
		want     leadstore.PlaceTypeCounts
	}{
		{1, leadstore.PlaceTypeCounts{Cafe: 2, Roastery: 1}},
		{2, leadstore.PlaceTypeCounts{Roastery: 1}},
		{0, leadstore.PlaceTypeCounts{Cafe: 2, Roastery: 2}},
	} {
		got, err := f.q.PlaceTypeCounts(ctx, c.campaign)
		if err != nil || got != c.want {
			t.Errorf("campaign %d: %+v (%v), want %+v", c.campaign, got, err, c.want)
		}
	}
}

// TestPlaceTypeRoasterySwitch: cold start off; on and off again are rows; a write that would
// not change the value is skipped, off on a cold start included; an unknown actor is refused;
// each account has its own switch.
func TestPlaceTypeRoasterySwitch(t *testing.T) {
	leadstoretest.Reset(t)
	ctx := context.Background()
	q := leadstoretest.Q(t)
	owner := storetest.OwnerConn(t)
	want := func(account int64, on bool) {
		t.Helper()
		got, err := q.RoasteryOutreachOn(ctx, account)
		if err != nil || got != on {
			t.Errorf("account %d: on %v (%v), want %v", account, got, err, on)
		}
	}
	set := func(on, wantChanged bool) {
		t.Helper()
		changed, err := q.SetRoasteryOutreach(ctx, 0, on, leadstore.ActorGremy)
		if err != nil || changed != wantChanged {
			t.Errorf("set %v: changed %v (%v), want %v", on, changed, err, wantChanged)
		}
	}
	want(0, false)
	set(false, false)
	if n := ownerInt(t, owner, `SELECT count(*) FROM roastery_outreach_switch`); n != 0 {
		t.Errorf("%d rows after a no-op off", n)
	}
	set(true, true)
	want(0, true)
	set(true, false)
	set(false, true)
	want(0, false)
	if n := ownerInt(t, owner, `SELECT count(*) FROM roastery_outreach_switch`); n != 2 {
		t.Errorf("%d rows, want 2", n)
	}
	if _, err := q.SetRoasteryOutreach(ctx, 0, true, "someone"); !errors.Is(err, leadstore.ErrUnknownActor) {
		t.Errorf("unknown actor: %v", err)
	}
	ownerRun(t, owner, `INSERT INTO account (id, slug, name) VALUES (2, 'second', 'Second')`)
	if _, err := q.SetRoasteryOutreach(ctx, 2, true, leadstore.ActorGremy); err != nil {
		t.Fatal(err)
	}
	want(2, true)
	want(0, false)
}

// TestPlaceTypeSkipLabelColumn: the app role writes lead_event.skip_label_id at INSERT and
// never after, and the column holds only a real call.
func TestPlaceTypeSkipLabelColumn(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	lead := f.cafeLead(t, "skip-label.example", nil, "")
	band := leadstore.BandNo
	label, err := f.q.SetLeadLabel(ctx, lead, &band, []byte(`{}`), leadstore.ActorOperator)
	if err != nil {
		t.Fatal(err)
	}
	app := storetest.AppConn(t)
	const insert = `INSERT INTO lead_event (lead_id, state, actor, skip_label_id) VALUES ($1, 'skipped', 'gremy', $2)`
	if _, err := app.Exec(ctx, insert, lead, label); err != nil {
		t.Errorf("insert with a real call: %v", err)
	}
	if _, err := app.Exec(ctx, insert, lead, 999_999); sqlState(err) != "23503" {
		t.Errorf("insert with no such call: %v, want 23503 foreign_key_violation", err)
	}
	if _, err := app.Exec(ctx, `UPDATE lead_event SET skip_label_id = NULL`); sqlState(err) != "42501" {
		t.Errorf("update as app: %v, want 42501", err)
	}
	if appHasColumn(t, f.owner, "lead_event", "skip_label_id", "UPDATE") {
		t.Error("the app role may UPDATE lead_event.skip_label_id")
	}
}

// TestMigrationPlaceTypeDownUp: 00030 goes down with a row in each new table and a skip that
// names its call, dropping both tables and the column and keeping the lead_event row; up again
// restores them with the app role still unable to rewrite either table.
func TestMigrationPlaceTypeDownUp(t *testing.T) {
	const triageVersion = 30
	f := newOutreachFixture(t)
	ctx := context.Background()
	owner := f.owner
	lead := f.cafeLead(t, "down-up.example", nil, "")
	if err := f.q.SetPlaceType(ctx, lead, leadstore.PlaceRoastery, leadstore.PlaceFromName, "roastery", nil, leadstore.ActorSystem); err != nil {
		t.Fatal(err)
	}
	if _, err := f.q.SetRoasteryOutreach(ctx, 0, true, leadstore.ActorGremy); err != nil {
		t.Fatal(err)
	}
	band := leadstore.BandNo
	label, err := f.q.SetLeadLabel(ctx, lead, &band, []byte(`{}`), leadstore.ActorOperator)
	if err != nil {
		t.Fatal(err)
	}
	event := ownerInt(t, owner, `INSERT INTO lead_event (lead_id, state, actor, skip_label_id)
		VALUES ($1, 'skipped', 'gremy', $2) RETURNING id`, lead, label)

	p := leadsProvider(t)
	t.Cleanup(func() { migrateAll(t) })
	if _, err := p.DownTo(ctx, triageVersion-1); err != nil {
		t.Fatalf("down to %d: %v", triageVersion-1, err)
	}
	for _, table := range []string{"lead_place_type", "roastery_outreach_switch"} {
		var reg *string
		if err := owner.QueryRow(ctx, `SELECT to_regclass('public.' || $1)::text`, table).Scan(&reg); err != nil || reg != nil {
			t.Errorf("%s after down: %v %v, want gone", table, reg, err)
		}
	}
	if n := ownerInt(t, owner, `SELECT count(*) FROM information_schema.columns WHERE table_schema = 'public'
		AND table_name = 'lead_event' AND column_name = 'skip_label_id'`); n != 0 {
		t.Error("lead_event.skip_label_id left after down")
	}
	if n := ownerInt(t, owner, `SELECT count(*) FROM lead_event WHERE id = $1`, event); n != 1 {
		t.Error("the lead_event row went with the column")
	}

	migrateAll(t)
	assertLeadPrivileges(t, owner, "after 00030 down and up")
	got, err := f.q.PlaceTypes(ctx, []int64{lead})
	if err != nil || got[lead].Type != leadstore.PlaceCafe || got[lead].Source != "" {
		t.Errorf("after up: %+v (%v), want the café default", got[lead], err)
	}
	on, err := f.q.RoasteryOutreachOn(ctx, 0)
	if err != nil || on {
		t.Errorf("switch after up: %v (%v), want off", on, err)
	}
}
