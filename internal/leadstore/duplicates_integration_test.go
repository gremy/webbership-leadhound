//go:build integration

package leadstore_test

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/places"
)

// dupPlace is the place a duplicate fixture lead stands for.
type dupPlace struct {
	name, freeform, town string
}

// dupLead adds a café lead in campaign 1, with an email route when email is true, whose one
// place is p. It returns the lead id.
func (f triageFixture) dupLead(t *testing.T, p dupPlace, email bool) int64 {
	t.Helper()
	*f.seq++
	domain := fmt.Sprintf("dup-cafe-%d.localhost", *f.seq)
	addr := ""
	if email {
		addr = "hello@" + domain
	}
	id := f.cafeLead(t, domain, nil, addr)
	pl := places.Place{SourceID: domain, Name: p.name, Category: "coffee_shop", Confidence: 0.9,
		Websites: []string{"https://" + domain + "/"}, Address: places.Address{Freeform: p.freeform, Locality: p.town, Country: "RO"}}
	if _, err := f.q.ImportPlaces(context.Background(), leadstore.PlaceImport{CampaignID: 1, Release: "2026-10-21.0",
		FileName: "places.ndjson", Stats: places.Stats{Lines: 1, Kept: 1}}, []leadstore.PlaceRow{{Place: pl}}); err != nil {
		t.Fatalf("place of %s: %v", domain, err)
	}
	return id
}

func (f triageFixture) duplicates(t *testing.T) leadstore.Duplicates {
	t.Helper()
	d, err := f.q.DuplicateLeads(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// soim is one fictive café as its map listing spells it.
var soim = dupPlace{"Cafenea Fictivă Știucă", "Strada Exemplu 3", "Fictivești"}

// TestDuplicateLeadsNormalized: one place spelled with other diacritics, punctuation and case
// is the same place, while two different cafés at one mall address are not, and neither is
// the same name and address in another town.
func TestDuplicateLeadsNormalized(t *testing.T) {
	f := newTriageFixture(t)
	a := f.dupLead(t, soim, false)
	b := f.dupLead(t, dupPlace{"CAFENEA FICTIVA ŞTIUCA!", "strada exemplu, 3", "FICTIVEŞTI"}, false)
	f.dupLead(t, dupPlace{"Bean Mall Fictiv", "Mall Exemplu, Calea Fictiva 1", "Oraș Exemplu"}, false)
	f.dupLead(t, dupPlace{"Espresso Fictiv", "Mall Exemplu, Calea Fictiva 1", "Oraș Exemplu"}, false)
	f.dupLead(t, dupPlace{"Cafenea Fictiva Stiuca", "Strada Exemplu 3", "Sat Exemplu"}, false)
	f.dupLead(t, dupPlace{"Cafenea Fictiva Stiuca", "", "Fictivesti"}, false)
	d := f.duplicates(t)
	want := leadstore.Duplicates{Groups: 1, Leads: 2, Skips: []leadstore.DuplicateSkip{{LeadID: b, KeeperID: a}}}
	if !reflect.DeepEqual(d, want) {
		t.Errorf("duplicates %+v, want %+v", d, want)
	}
}

// TestDuplicateLeadsKeeperOrder: the keeper is a lead past new, else one called or starred
// by the operator, else one with an email route, else the lowest id; a lead the system
// skipped is never the keeper, while one the operator skipped is past new and keeps.
func TestDuplicateLeadsKeeperOrder(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		// setup adds the group's two leads and returns them as the keeper and the skip; a zero
		// skip means nothing is skipped.
		setup func(t *testing.T, f triageFixture) (keeper, skip int64)
	}{
		{"past new before email", func(t *testing.T, f triageFixture) (int64, int64) {
			first, second := f.dupLead(t, soim, true), f.dupLead(t, soim, false)
			f.set(t, second, leadstore.LeadContacted, leadstore.ActorSystem)
			return second, first
		}},
		{"called before email", func(t *testing.T, f triageFixture) (int64, int64) {
			first, second := f.dupLead(t, soim, true), f.dupLead(t, soim, false)
			f.call(t, second, "maybe")
			return second, first
		}},
		{"starred before email", func(t *testing.T, f triageFixture) (int64, int64) {
			first, second := f.dupLead(t, soim, true), f.dupLead(t, soim, false)
			f.star(t, second)
			return second, first
		}},
		{"email before none", func(t *testing.T, f triageFixture) (int64, int64) {
			first, second := f.dupLead(t, soim, false), f.dupLead(t, soim, true)
			return second, first
		}},
		{"lowest id", func(t *testing.T, f triageFixture) (int64, int64) {
			first, second := f.dupLead(t, soim, true), f.dupLead(t, soim, true)
			return first, second
		}},
		{"a system skip never keeps", func(t *testing.T, f triageFixture) (int64, int64) {
			first, second := f.dupLead(t, soim, true), f.dupLead(t, soim, false)
			if moved, err := f.q.SkipNewLead(ctx, first, "build: kind off"); err != nil || !moved {
				t.Fatalf("system skip: %v %v", moved, err)
			}
			return second, 0
		}},
		{"an operator skip keeps", func(t *testing.T, f triageFixture) (int64, int64) {
			first, second := f.dupLead(t, soim, true), f.dupLead(t, soim, true)
			f.skip(t, second, triageInput)
			return second, first
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newTriageFixture(t)
			keeper, skip := tc.setup(t, f)
			d := f.duplicates(t)
			if d.Groups != 1 || d.Leads != 2 || len(d.Collisions) != 0 {
				t.Fatalf("duplicates %+v, want one group of two and no collision", d)
			}
			var want []leadstore.DuplicateSkip
			if skip != 0 {
				want = []leadstore.DuplicateSkip{{LeadID: skip, KeeperID: keeper}}
			}
			if !reflect.DeepEqual(d.Skips, want) {
				t.Errorf("skips %+v, want %+v", d.Skips, want)
			}
		})
	}
}

// TestDuplicateLeadsCalledPairStays: two duplicates the operator called or starred both stay,
// and the group is reported as a collision; a third, plain duplicate is still skipped.
func TestDuplicateLeadsCalledPairStays(t *testing.T) {
	f := newTriageFixture(t)
	called, starred, plain := f.dupLead(t, soim, false), f.dupLead(t, soim, true), f.dupLead(t, soim, true)
	f.call(t, called, "likely")
	f.star(t, starred)
	d := f.duplicates(t)
	want := leadstore.Duplicates{Groups: 1, Leads: 3,
		Skips:      []leadstore.DuplicateSkip{{LeadID: plain, KeeperID: called}},
		Collisions: []leadstore.DuplicateCollision{{KeeperID: called, Stayed: []int64{starred}}}}
	if !reflect.DeepEqual(d, want) {
		t.Errorf("duplicates %+v, want %+v", d, want)
	}
}
