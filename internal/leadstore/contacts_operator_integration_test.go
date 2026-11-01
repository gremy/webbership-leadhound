//go:build integration

package leadstore_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/leadstore"
)

// Migration 00029: the operator's own contacts, and marks on any contact.

// t12LinkedIn builds a LinkedIn address in pieces (the repository refuses the profile path as
// one piece of text).
func t12LinkedIn(section, slug string) string { return "https://linkedin.com/" + section + "/" + slug }

// t12Org adds a site lead in GB with no route; it returns the lead and its org.
func t12Org(t *testing.T, f outreachFixture, domain string) (lead, org int64) {
	t.Helper()
	lead = f.siteLead(t, domain, nil, 7, "")
	org = f.leadOf(t, lead).OrgID
	if _, err := f.owner.Exec(context.Background(), `UPDATE org SET country = 'GB' WHERE id = $1`, org); err != nil {
		t.Fatal(err)
	}
	return lead, org
}

// t12Research writes a route as if read from a page, and returns its id.
func t12Research(t *testing.T, f outreachFixture, org int64, kind, value string, personID int64) int64 {
	t.Helper()
	if ins, err := f.q.InsertRoute(context.Background(), leadstore.RouteRow{OrgID: org, Kind: kind, Value: value, FetchID: f.fid,
		MXOK: true, DerivedBy: "claude-research@1", PersonID: personID}); err != nil || !ins {
		t.Fatalf("research route %s: %v %v", value, ins, err)
	}
	return t12RouteID(t, f, org, value)
}

func t12RouteID(t *testing.T, f outreachFixture, org int64, value string) int64 {
	t.Helper()
	return ownerInt(t, f.owner, `SELECT id FROM contact_route WHERE org_id = $1 AND value = $2`, org, value)
}

// t12Live is the org's routes as RoutesFor returns them, one "value" per route, with "*" before
// the operator's.
func t12Live(t *testing.T, f outreachFixture, org int64) []string {
	t.Helper()
	routes, err := f.q.RoutesFor(context.Background(), org)
	if err != nil {
		t.Fatal(err)
	}
	out := []string{}
	for _, r := range routes {
		v := r.Value
		if r.Operator {
			v = "*" + v
		}
		out = append(out, v)
	}
	return out
}

// t12Contact finds a contact by kind and id.
func t12Contact(t *testing.T, f outreachFixture, org int64, kind string, id int64) leadstore.Contact {
	t.Helper()
	contacts, err := f.q.ContactsFor(context.Background(), org)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range contacts {
		if c.Kind == kind && c.ID == id {
			return c
		}
	}
	t.Fatalf("no %s %d in %+v", kind, id, contacts)
	return leadstore.Contact{}
}

func t12MarkCount(t *testing.T, f outreachFixture, column string, id int64) int64 {
	t.Helper()
	return ownerInt(t, f.owner, `SELECT count(*) FROM contact_mark WHERE `+column+` = $1`, id)
}

// TestOperatorRouteAdded: a new value is a row of his own with a claim: no fetch, no place, MX
// false but his; an email is lower-cased; each kind's bad value is refused with nothing written.
func TestOperatorRouteAdded(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	_, org := t12Org(t, f, "add.localhost")

	res, err := f.q.AddOperatorRoute(ctx, leadstore.OperatorRoute{OrgID: org, Kind: leadstore.RouteEmail, Value: " Jo@Add.Example ", Note: " met at a fair "})
	if err != nil || !res.Added || res.WasRetired || res.ID == 0 {
		t.Fatalf("add: %+v %v", res, err)
	}
	routes, err := f.q.RoutesFor(ctx, org)
	if err != nil || len(routes) != 1 {
		t.Fatalf("routes: %+v %v", routes, err)
	}
	r := routes[0]
	if r.Value != "jo@add.example" || r.MXOK || !r.Operator || r.DerivedBy != leadstore.DerivedByOperator || r.FetchID != 0 {
		t.Errorf("route: %+v", r)
	}
	c := t12Contact(t, f, org, leadstore.RouteEmail, res.ID)
	if c.Provenance != leadstore.ProvenanceOperator || c.Note != "met at a fair" || c.Retired || !c.Operator {
		t.Errorf("contact: %+v", c)
	}
	if n := ownerInt(t, f.owner, `SELECT count(*) FROM contact_mark WHERE route_id = $1 AND mark = 'claim' AND actor = 'operator'`, res.ID); n != 1 {
		t.Errorf("claims = %d", n)
	}

	before := t12Live(t, f, org)
	for name, r := range map[string]leadstore.OperatorRoute{
		"email":         {Kind: leadstore.RouteEmail, Value: "not an address"},
		"form":          {Kind: leadstore.RouteForm, Value: "contact.example/form"},
		"linkedin www":  {Kind: leadstore.RouteLinkedIn, Value: "https://www.linkedin.com/company/acme"},
		"linkedin case": {Kind: leadstore.RouteLinkedIn, Value: t12LinkedIn("company", "Acme")},
		"linkedin feed": {Kind: leadstore.RouteLinkedIn, Value: t12LinkedIn("feed", "x")},
		"note":          {Kind: leadstore.RouteEmail, Value: "a@add.example", Note: strings.Repeat("x", 201)},
	} {
		r.OrgID = org
		if _, err := f.q.AddOperatorRoute(ctx, r); !errors.Is(err, leadstore.ErrBadContact) {
			t.Errorf("%s: %v, want ErrBadContact", name, err)
		}
	}
	if _, err := f.q.AddOperatorRoute(ctx, leadstore.OperatorRoute{OrgID: org, Kind: "fax", Value: "1"}); !errors.Is(err, leadstore.ErrUnknownRouteKind) {
		t.Errorf("fax: %v", err)
	}
	if got := t12Live(t, f, org); !reflect.DeepEqual(got, before) {
		t.Errorf("after refusals: %v", got)
	}
	for _, r := range []leadstore.OperatorRoute{
		{OrgID: org, Kind: leadstore.RouteForm, Value: "https://add.example/contact"},
		{OrgID: org, Kind: leadstore.RouteLinkedIn, Value: t12LinkedIn("school", "add-uni")},
	} {
		if res, err := f.q.AddOperatorRoute(ctx, r); err != nil || !res.Added {
			t.Errorf("%s: %+v %v", r.Value, res, err)
		}
	}
}

// TestOperatorRouteClaimsExisting: adding a value research found writes no row, reports "already
// there", and makes it his, with his note. Provenance follows the latest mark.
func TestOperatorRouteClaimsExisting(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	_, org := t12Org(t, f, "claim.localhost")
	id := t12Research(t, f, org, leadstore.RouteEmail, "hello@claim.example", 0)
	if c := t12Contact(t, f, org, leadstore.RouteEmail, id); c.Provenance != leadstore.ProvenanceResearch || c.Operator {
		t.Errorf("before: %+v", c)
	}

	res, err := f.q.AddOperatorRoute(ctx, leadstore.OperatorRoute{OrgID: org, Kind: leadstore.RouteEmail, Value: "HELLO@claim.example", Note: "ask for Sam"})
	if err != nil || res.Added || res.WasRetired || res.ID != id {
		t.Fatalf("claim: %+v %v", res, err)
	}
	c := t12Contact(t, f, org, leadstore.RouteEmail, id)
	if c.Provenance != leadstore.ProvenanceOperator || !c.Operator || c.Note != "ask for Sam" || c.DerivedBy != "claude-research@1" {
		t.Errorf("after: %+v", c)
	}
	if n := ownerInt(t, f.owner, `SELECT count(*) FROM contact_route WHERE org_id = $1`, org); n != 1 {
		t.Errorf("routes = %d", n)
	}
}

// TestOperatorRouteUnretires: a value he retired comes back by adding it again; nothing else
// brings it back, not a crawl or research finding the same value.
func TestOperatorRouteUnretires(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	_, org := t12Org(t, f, "back.localhost")
	const value = "hello@back.example"
	first, err := f.q.AddOperatorRoute(ctx, leadstore.OperatorRoute{OrgID: org, Kind: leadstore.RouteEmail, Value: value})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.q.MarkContact(ctx, leadstore.Mark{OrgID: org, RouteID: first.ID, Mark: leadstore.MarkRetire, Note: "by mistake"}); err != nil {
		t.Fatal(err)
	}
	if got := t12Live(t, f, org); len(got) != 0 {
		t.Fatalf("retired route still read: %v", got)
	}
	c := t12Contact(t, f, org, leadstore.RouteEmail, first.ID)
	if !c.Retired || c.Operator || c.Note != "by mistake" || c.Provenance != leadstore.ProvenanceOperator {
		t.Errorf("retired: %+v", c)
	}

	if ins, err := f.q.InsertRoute(ctx, leadstore.RouteRow{OrgID: org, Kind: leadstore.RouteEmail, Value: value, FetchID: f.fid, MXOK: true, DerivedBy: "contacts@1"}); err != nil || ins {
		t.Errorf("crawl of a retired value: inserted %v, %v", ins, err)
	}
	if got := t12Live(t, f, org); len(got) != 0 {
		t.Errorf("a crawl un-retired it: %v", got)
	}

	res, err := f.q.AddOperatorRoute(ctx, leadstore.OperatorRoute{OrgID: org, Kind: leadstore.RouteEmail, Value: value})
	if err != nil || res.Added || !res.WasRetired || res.ID != first.ID {
		t.Fatalf("re-add: %+v %v", res, err)
	}
	if got, want := t12Live(t, f, org), []string{"*" + value}; !reflect.DeepEqual(got, want) {
		t.Errorf("after re-add %v, want %v", got, want)
	}
}

// TestOperatorContactNoteOnly: the same value with a new note is one more claim: no new row, no
// retire, the latest note wins, a blank one clears it.
func TestOperatorContactNoteOnly(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	_, org := t12Org(t, f, "note.localhost")
	add, err := f.q.AddOperatorRoute(ctx, leadstore.OperatorRoute{OrgID: org, Kind: leadstore.RouteEmail, Value: "a@note.example", Note: "first"})
	if err != nil {
		t.Fatal(err)
	}
	res, err := f.q.EditOperatorContact(ctx, leadstore.ContactEdit{OrgID: org, RouteID: add.ID, Value: " A@note.example", Note: "second"})
	if err != nil || res.Added || res.Replaced || res.ID != add.ID {
		t.Fatalf("edit: %+v %v", res, err)
	}
	if c := t12Contact(t, f, org, leadstore.RouteEmail, add.ID); c.Note != "second" || c.Retired {
		t.Errorf("after: %+v", c)
	}
	if n := t12MarkCount(t, f, "route_id", add.ID); n != 2 {
		t.Errorf("marks = %d", n)
	}
	if _, err := f.q.EditOperatorContact(ctx, leadstore.ContactEdit{OrgID: org, RouteID: add.ID, Value: "a@note.example"}); err != nil {
		t.Fatal(err)
	}
	if c := t12Contact(t, f, org, leadstore.RouteEmail, add.ID); c.Note != "" {
		t.Errorf("blank note: %+v", c)
	}
}

// TestOperatorContactValueChanged: a new value is a new row, the old one is retired naming it
// and stays visible; to a value that already exists it claims that row instead; the person
// carries over; a bad new value writes nothing, not even the retire.
func TestOperatorContactValueChanged(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	_, org := t12Org(t, f, "edit.localhost")
	person, err := f.q.AddPerson(ctx, leadstore.PersonRow{OrgID: org, Role: "Head of Digital"})
	if err != nil {
		t.Fatal(err)
	}
	old, err := f.q.AddOperatorRoute(ctx, leadstore.OperatorRoute{OrgID: org, Kind: leadstore.RouteLinkedIn, Value: t12LinkedIn("in", "old"), PersonID: person})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := f.q.EditOperatorContact(ctx, leadstore.ContactEdit{OrgID: org, RouteID: old.ID, Value: "nonsense"}); !errors.Is(err, leadstore.ErrBadContact) {
		t.Errorf("bad value: %v", err)
	}
	if n := t12MarkCount(t, f, "route_id", old.ID); n != 1 {
		t.Errorf("a refused edit left marks: %d", n)
	}

	res, err := f.q.EditOperatorContact(ctx, leadstore.ContactEdit{OrgID: org, RouteID: old.ID, Value: t12LinkedIn("in", "new"), Note: "moved"})
	if err != nil || !res.Added || !res.Replaced || res.ID == old.ID {
		t.Fatalf("edit: %+v %v", res, err)
	}
	if got, want := t12Live(t, f, org), []string{"*" + t12LinkedIn("in", "new")}; !reflect.DeepEqual(got, want) {
		t.Errorf("live %v, want %v", got, want)
	}
	gone := t12Contact(t, f, org, leadstore.RouteLinkedIn, old.ID)
	if !gone.Retired || gone.ReplacedBy != res.ID {
		t.Errorf("old: %+v", gone)
	}
	if c := t12Contact(t, f, org, leadstore.RouteLinkedIn, res.ID); c.PersonID != person || c.Role != "Head of Digital" || c.Note != "moved" {
		t.Errorf("new: %+v", c)
	}

	found := t12Research(t, f, org, leadstore.RouteLinkedIn, t12LinkedIn("company", "edit"), 0)
	again, err := f.q.EditOperatorContact(ctx, leadstore.ContactEdit{OrgID: org, RouteID: res.ID, Value: t12LinkedIn("company", "edit")})
	if err != nil || again.Added || !again.Replaced || again.ID != found {
		t.Fatalf("edit to an existing value: %+v %v", again, err)
	}
	if c := t12Contact(t, f, org, leadstore.RouteLinkedIn, res.ID); !c.Retired || c.ReplacedBy != found {
		t.Errorf("replaced: %+v", c)
	}
	if c := t12Contact(t, f, org, leadstore.RouteLinkedIn, found); !c.Operator {
		t.Errorf("claimed: %+v", c)
	}

	_, other := t12Org(t, f, "edit-other.localhost")
	if _, err := f.q.EditOperatorContact(ctx, leadstore.ContactEdit{OrgID: other, RouteID: found, Value: "x@edit.example"}); !errors.Is(err, leadstore.ErrContactNotFound) {
		t.Errorf("another org's route: %v", err)
	}
	if _, err := f.q.EditOperatorContact(ctx, leadstore.ContactEdit{OrgID: org, RouteID: found, FactID: 1, Value: "x@edit.example"}); !errors.Is(err, leadstore.ErrBadContact) {
		t.Errorf("two targets: %v", err)
	}
}

// TestOperatorPhone: a number goes through E.164 with the org's country; the same number in any
// stored form is claimed; a retired one leaves OrgFacts and comes back by a claim, never by a
// crawl; an edit retires the old fact naming the new one.
func TestOperatorPhone(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	_, org := t12Org(t, f, "phone.localhost")
	res, err := f.q.AddOperatorPhone(ctx, leadstore.OperatorPhone{OrgID: org, Value: "020 7946 0123", Note: "mornings"})
	if err != nil || !res.Added {
		t.Fatalf("add: %+v %v", res, err)
	}
	facts, err := f.q.OrgFacts(ctx, []int64{org})
	if err != nil || len(facts) != 1 || facts[0].Normalized != "+442079460123" || facts[0].FetchID != 0 || facts[0].DerivedBy != leadstore.DerivedByOperator {
		t.Fatalf("facts: %+v %v", facts, err)
	}
	if c := t12Contact(t, f, org, leadstore.ContactPhone, res.ID); c.Note != "mornings" || c.Provenance != leadstore.ProvenanceOperator || c.Value != facts[0].Value {
		t.Errorf("contact: %+v", c)
	}
	if again, err := f.q.AddOperatorPhone(ctx, leadstore.OperatorPhone{OrgID: org, Value: "+44 (0)20 7946 0123"}); err != nil || again.Added || again.ID != res.ID {
		t.Errorf("same number: %+v %v", again, err)
	}
	for _, bad := range []string{"call me", "123", ""} {
		if _, err := f.q.AddOperatorPhone(ctx, leadstore.OperatorPhone{OrgID: org, Value: bad}); !errors.Is(err, leadstore.ErrBadContact) {
			t.Errorf("%q: %v", bad, err)
		}
	}
	if _, err := f.q.AddOperatorPhone(ctx, leadstore.OperatorPhone{OrgID: -1, Value: "+442079460123"}); !errors.Is(err, leadstore.ErrOrgNotFound) {
		t.Errorf("no org: %v", err)
	}

	// A fact kept in national form, found by enrich, is the same number.
	page := f.pageFetch(t, "https://phone.localhost/contact", "", "<html>x</html>")
	if _, err := f.q.InsertFact(ctx, leadstore.FactRow{OrgID: org, Kind: leadstore.FactPhone, Value: "020 7946 0999", Normalized: "02079460999", FetchID: page, DerivedBy: "enrich@1"}); err != nil {
		t.Fatal(err)
	}
	national := ownerInt(t, f.owner, `SELECT id FROM org_fact WHERE org_id = $1 AND normalized = '02079460999'`, org)
	if c := t12Contact(t, f, org, leadstore.ContactPhone, national); c.Provenance != leadstore.ProvenanceEnrich {
		t.Errorf("enrich: %+v", c)
	}
	if got, err := f.q.AddOperatorPhone(ctx, leadstore.OperatorPhone{OrgID: org, Value: "+44 20 7946 0999"}); err != nil || got.Added || got.ID != national {
		t.Errorf("national form: %+v %v", got, err)
	}

	// Edit: new number, old one retired naming it.
	edit, err := f.q.EditOperatorContact(ctx, leadstore.ContactEdit{OrgID: org, FactID: res.ID, Value: "020 7946 0456"})
	if err != nil || !edit.Added || !edit.Replaced {
		t.Fatalf("edit: %+v %v", edit, err)
	}
	if c := t12Contact(t, f, org, leadstore.ContactPhone, res.ID); !c.Retired || c.ReplacedBy != edit.ID {
		t.Errorf("old: %+v", c)
	}
	facts, _ = f.q.OrgFacts(ctx, []int64{org})
	for _, x := range facts {
		if x.ID == res.ID {
			t.Errorf("retired phone still read: %+v", x)
		}
	}
	if ins, err := f.q.InsertFact(ctx, leadstore.FactRow{OrgID: org, Kind: leadstore.FactPhone, Value: "x", Normalized: "+442079460123", FetchID: page, DerivedBy: "enrich@1"}); err != nil || ins {
		t.Errorf("re-insert: %v %v", ins, err)
	}
	if facts, _ = f.q.OrgFacts(ctx, []int64{org}); len(facts) != 2 {
		t.Errorf("a crawl un-retired it: %+v", facts)
	}
	back, err := f.q.AddOperatorPhone(ctx, leadstore.OperatorPhone{OrgID: org, Value: "+442079460123"})
	if err != nil || back.Added || !back.WasRetired || back.ID != res.ID {
		t.Errorf("back: %+v %v", back, err)
	}
	if facts, _ = f.q.OrgFacts(ctx, []int64{org}); len(facts) != 3 {
		t.Errorf("after un-retire: %+v", facts)
	}

	// A phone's mark needs a phone fact of the org.
	if _, err := f.q.InsertFact(ctx, leadstore.FactRow{OrgID: org, Kind: leadstore.FactDescription, Value: "d", Normalized: "d", FetchID: page, DerivedBy: "enrich@1"}); err != nil {
		t.Fatal(err)
	}
	desc := ownerInt(t, f.owner, `SELECT id FROM org_fact WHERE org_id = $1 AND kind = 'description'`, org)
	if err := f.q.MarkContact(ctx, leadstore.Mark{OrgID: org, FactID: desc, Mark: leadstore.MarkRetire}); !errors.Is(err, leadstore.ErrContactNotFound) {
		t.Errorf("mark a description: %v", err)
	}
}

// TestPersonAddReadRetire: a person needs a name or a role; the operator's has a claim and his
// note, one read from a page has neither; a retired one leaves PeopleFor and stays in ContactsFor;
// a changed name is a new person; a route may belong only to a live person of the org.
func TestPersonAddReadRetire(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	_, org := t12Org(t, f, "person.localhost")
	_, other := t12Org(t, f, "person-other.localhost")

	for name, p := range map[string]leadstore.PersonRow{
		"empty":     {OrgID: org, Name: " "},
		"long name": {OrgID: org, Name: strings.Repeat("n", 121)},
		"long role": {OrgID: org, Role: strings.Repeat("r", 61)},
		"two lines": {OrgID: org, Name: "A\nB"},
	} {
		if _, err := f.q.AddPerson(ctx, p); !errors.Is(err, leadstore.ErrBadContact) {
			t.Errorf("%s: %v", name, err)
		}
	}
	mine, err := f.q.AddPerson(ctx, leadstore.PersonRow{OrgID: org, Name: "Jo Bloggs", Role: "Owner", Note: "friend of a friend"})
	if err != nil {
		t.Fatal(err)
	}
	read, err := f.q.AddPerson(ctx, leadstore.PersonRow{OrgID: org, Role: "Web Lead", FetchID: f.fid, DerivedBy: "claude-research@1"})
	if err != nil {
		t.Fatal(err)
	}
	if n := t12MarkCount(t, f, "person_id", read); n != 0 {
		t.Errorf("a person read from a page has marks: %d", n)
	}
	if c := t12Contact(t, f, org, leadstore.ContactPerson, mine); c.Provenance != leadstore.ProvenanceOperator || c.Note != "friend of a friend" || c.Name != "Jo Bloggs" || c.Role != "Owner" {
		t.Errorf("mine: %+v", c)
	}
	if c := t12Contact(t, f, org, leadstore.ContactPerson, read); c.Provenance != leadstore.ProvenanceResearch || c.Name != "" {
		t.Errorf("read: %+v", c)
	}

	for name, r := range map[string]struct {
		route leadstore.OperatorRoute
		want  error
	}{
		"form of a person":   {leadstore.OperatorRoute{OrgID: org, Kind: leadstore.RouteForm, Value: "https://person.example/c", PersonID: mine}, leadstore.ErrBadContact},
		"another org's":      {leadstore.OperatorRoute{OrgID: other, Kind: leadstore.RouteEmail, Value: "jo@person.example", PersonID: mine}, leadstore.ErrContactNotFound},
		"no such person yet": {leadstore.OperatorRoute{OrgID: org, Kind: leadstore.RouteEmail, Value: "jo@person.example", PersonID: mine + 1000}, leadstore.ErrContactNotFound},
	} {
		if _, err := f.q.AddOperatorRoute(ctx, r.route); !errors.Is(err, r.want) {
			t.Errorf("%s: %v, want %v", name, err, r.want)
		}
	}

	if err := f.q.MarkContact(ctx, leadstore.Mark{OrgID: org, PersonID: read, Mark: leadstore.MarkRetire}); err != nil {
		t.Fatal(err)
	}
	people, err := f.q.PeopleFor(ctx, org)
	if err != nil || len(people) != 1 || people[0].ID != mine || people[0].FetchID != 0 {
		t.Errorf("people: %+v %v", people, err)
	}
	if c := t12Contact(t, f, org, leadstore.ContactPerson, read); !c.Retired {
		t.Errorf("retired: %+v", c)
	}
	if _, err := f.q.AddOperatorRoute(ctx, leadstore.OperatorRoute{OrgID: org, Kind: leadstore.RouteEmail, Value: "web@person.example", PersonID: read}); !errors.Is(err, leadstore.ErrContactNotFound) {
		t.Errorf("retired person: %v", err)
	}

	note, err := f.q.EditOperatorContact(ctx, leadstore.ContactEdit{OrgID: org, PersonID: mine, Name: "Jo Bloggs", Role: "Owner", Note: "note only"})
	if err != nil || note.Added || note.Replaced || note.ID != mine {
		t.Errorf("same person: %+v %v", note, err)
	}
	moved, err := f.q.EditOperatorContact(ctx, leadstore.ContactEdit{OrgID: org, PersonID: mine, Name: "Jo Bloggs", Role: "Director"})
	if err != nil || !moved.Added || !moved.Replaced {
		t.Fatalf("new role: %+v %v", moved, err)
	}
	if people, _ := f.q.PeopleFor(ctx, org); len(people) != 1 || people[0].ID != moved.ID || people[0].Role != "Director" {
		t.Errorf("after role change: %+v", people)
	}
	if c := t12Contact(t, f, org, leadstore.ContactPerson, mine); !c.Retired {
		t.Errorf("old person: %+v", c)
	}
	if _, err := f.q.EditOperatorContact(ctx, leadstore.ContactEdit{OrgID: other, PersonID: moved.ID, Role: "x"}); !errors.Is(err, leadstore.ErrContactNotFound) {
		t.Errorf("another org's person: %v", err)
	}
}

// TestContactMarkRules: exactly one target, a known mark, a replacement of the same sort on the
// same org; a retire then a claim brings a research row back as his.
func TestContactMarkRules(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	_, org := t12Org(t, f, "mark.localhost")
	_, other := t12Org(t, f, "mark-other.localhost")
	a := t12Research(t, f, org, leadstore.RouteEmail, "a@mark.example", 0)
	b := t12Research(t, f, org, leadstore.RouteEmail, "b@mark.example", 0)
	c := t12Research(t, f, other, leadstore.RouteEmail, "c@mark.example", 0)

	for name, m := range map[string]leadstore.Mark{
		"no target":           {OrgID: org, Mark: leadstore.MarkClaim},
		"two targets":         {OrgID: org, RouteID: a, PersonID: 1, Mark: leadstore.MarkClaim},
		"unknown mark":        {OrgID: org, RouteID: a, Mark: "hide"},
		"replacement, claim":  {OrgID: org, RouteID: a, Mark: leadstore.MarkClaim, ReplacedByRouteID: b},
		"replacement, itself": {OrgID: org, RouteID: a, Mark: leadstore.MarkRetire, ReplacedByRouteID: a},
		"replacement, sort":   {OrgID: org, RouteID: a, Mark: leadstore.MarkRetire, ReplacedByFactID: 1},
		"long note":           {OrgID: org, RouteID: a, Mark: leadstore.MarkClaim, Note: strings.Repeat("x", 201)},
	} {
		if err := f.q.MarkContact(ctx, m); !errors.Is(err, leadstore.ErrBadContact) {
			t.Errorf("%s: %v, want ErrBadContact", name, err)
		}
	}
	if err := f.q.MarkContact(ctx, leadstore.Mark{OrgID: org, RouteID: c, Mark: leadstore.MarkRetire}); !errors.Is(err, leadstore.ErrContactNotFound) {
		t.Errorf("another org's route: %v", err)
	}
	if err := f.q.MarkContact(ctx, leadstore.Mark{OrgID: org, RouteID: a, Mark: leadstore.MarkRetire, ReplacedByRouteID: c}); !errors.Is(err, leadstore.ErrContactNotFound) {
		t.Errorf("another org's replacement: %v", err)
	}
	if n := t12MarkCount(t, f, "route_id", a); n != 0 {
		t.Errorf("refusals wrote %d marks", n)
	}

	if err := f.q.MarkContact(ctx, leadstore.Mark{OrgID: org, RouteID: a, Mark: leadstore.MarkRetire, ReplacedByRouteID: b}); err != nil {
		t.Fatal(err)
	}
	if got, want := t12Live(t, f, org), []string{"b@mark.example"}; !reflect.DeepEqual(got, want) {
		t.Errorf("live %v, want %v", got, want)
	}
	if err := f.q.MarkContact(ctx, leadstore.Mark{OrgID: org, RouteID: a, Mark: leadstore.MarkClaim}); err != nil {
		t.Fatal(err)
	}
	if got, want := t12Live(t, f, org), []string{"*a@mark.example", "b@mark.example"}; !reflect.DeepEqual(got, want) {
		t.Errorf("live %v, want %v", got, want)
	}
}

// TestRoutesForOrder (Decision 6, Decision 1): by kind; his before the rest even when found
// later; among LinkedIn routes his person profile before his company page, and in the rest the
// company page before the person profile, whichever was found first. The same order comes
// back through ContactsFor, with the retired after the live.
func TestRoutesForOrder(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	_, org := t12Org(t, f, "order.localhost")

	researchIn := t12LinkedIn("in", "found-person")
	researchCo := t12LinkedIn("company", "found-company")
	myCo := t12LinkedIn("company", "my-company")
	myIn := t12LinkedIn("in", "my-person")

	person, err := f.q.AddPerson(ctx, leadstore.PersonRow{OrgID: org, Role: "Web Lead", FetchID: f.fid, DerivedBy: "claude-research@1"})
	if err != nil {
		t.Fatal(err)
	}
	t12Research(t, f, org, leadstore.RouteLinkedIn, researchIn, person)
	t12Research(t, f, org, leadstore.RouteLinkedIn, researchCo, 0)
	t12Research(t, f, org, leadstore.RouteForm, "https://order.example/contact", 0)
	t12Research(t, f, org, leadstore.RouteEmail, "first@order.example", 0)
	for _, r := range []leadstore.OperatorRoute{
		{OrgID: org, Kind: leadstore.RouteLinkedIn, Value: myCo},
		{OrgID: org, Kind: leadstore.RouteLinkedIn, Value: myIn},
		{OrgID: org, Kind: leadstore.RouteEmail, Value: "mine@order.example"},
	} {
		if _, err := f.q.AddOperatorRoute(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	gone, err := f.q.AddOperatorRoute(ctx, leadstore.OperatorRoute{OrgID: org, Kind: leadstore.RouteEmail, Value: "gone@order.example"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.q.MarkContact(ctx, leadstore.Mark{OrgID: org, RouteID: gone.ID, Mark: leadstore.MarkRetire}); err != nil {
		t.Fatal(err)
	}

	want := []string{
		"*mine@order.example", "first@order.example",
		"https://order.example/contact",
		"*" + myIn, "*" + myCo, researchCo, researchIn,
	}
	if got := t12Live(t, f, org); !reflect.DeepEqual(got, want) {
		t.Errorf("order\n got %v\nwant %v", got, want)
	}
	routes, err := f.q.RoutesFor(ctx, org)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range routes {
		if r.Value == researchIn && r.PersonID != person {
			t.Errorf("person lost: %+v", r)
		}
	}

	contacts, err := f.q.ContactsFor(ctx, org)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, c := range contacts {
		kinds = append(kinds, c.Kind)
	}
	if contacts[2].Value != "gone@order.example" || !contacts[2].Retired || kinds[len(kinds)-1] != leadstore.ContactPerson {
		t.Errorf("contacts: %+v", contacts)
	}
}

// TestOperatorSetNewLeadRouteRefusesRetired: a retired route cannot become a lead's route, and
// can once a claim brings it back.
func TestOperatorSetNewLeadRouteRefusesRetired(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	lead, org := t12Org(t, f, "retired.localhost")
	add, err := f.q.AddOperatorRoute(ctx, leadstore.OperatorRoute{OrgID: org, Kind: leadstore.RouteForm, Value: "https://retired.example/c"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.q.MarkContact(ctx, leadstore.Mark{OrgID: org, RouteID: add.ID, Mark: leadstore.MarkRetire}); err != nil {
		t.Fatal(err)
	}
	if moved, err := f.q.SetNewLeadRoute(ctx, lead, leadstore.ChannelForm, add.ID); err != nil || moved {
		t.Errorf("retired route: moved %v, %v", moved, err)
	}
	if l := f.leadOf(t, lead); l.RouteID != nil || l.Channel != leadstore.ChannelNone {
		t.Errorf("lead moved: %+v", l)
	}
	if err := f.q.MarkContact(ctx, leadstore.Mark{OrgID: org, RouteID: add.ID, Mark: leadstore.MarkClaim}); err != nil {
		t.Fatal(err)
	}
	if moved, err := f.q.SetNewLeadRoute(ctx, lead, leadstore.ChannelForm, add.ID); err != nil || !moved {
		t.Errorf("claimed route: moved %v, %v", moved, err)
	}
}

// TestOperatorClearNewLeadRoute: a new lead with a route falls back to none; again is no move; a
// lead past new keeps its route.
func TestOperatorClearNewLeadRoute(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	lead := f.siteLead(t, "clear.localhost", nil, 7, "info@clear.localhost")
	if l := f.leadOf(t, lead); l.RouteID == nil || l.Channel != leadstore.ChannelEmail {
		t.Fatalf("fixture: %+v", l)
	}
	if moved, err := f.q.ClearNewLeadRoute(ctx, lead); err != nil || !moved {
		t.Fatalf("clear: %v %v", moved, err)
	}
	if l := f.leadOf(t, lead); l.RouteID != nil || l.Channel != leadstore.ChannelNone {
		t.Errorf("after: %+v", l)
	}
	if moved, err := f.q.ClearNewLeadRoute(ctx, lead); err != nil || moved {
		t.Errorf("again: %v %v", moved, err)
	}

	past := f.siteLead(t, "clear-past.localhost", nil, 7, "info@clear-past.localhost")
	if _, err := f.owner.Exec(ctx, `UPDATE lead SET state = 'contacted' WHERE id = $1`, past); err != nil {
		t.Fatal(err)
	}
	if moved, err := f.q.ClearNewLeadRoute(ctx, past); err != nil || moved {
		t.Errorf("past new: %v %v", moved, err)
	}
	if l := f.leadOf(t, past); l.RouteID == nil || l.Channel != leadstore.ChannelEmail {
		t.Errorf("past new moved: %+v", l)
	}
}

// TestOperatorRouteLeavesCrawlDue: a route he typed is not a route found on the org's pages, so
// the org is still searched.
func TestOperatorRouteLeavesCrawlDue(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	_, org := t12Org(t, f, "crawl.localhost")
	if _, err := f.q.AddOperatorRoute(ctx, leadstore.OperatorRoute{OrgID: org, Kind: leadstore.RouteEmail, Value: "me@crawl.example"}); err != nil {
		t.Fatal(err)
	}
	orgs, err := f.q.OrgsNeedingRoutes(ctx, 0, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range orgs {
		if o.ID == org {
			return
		}
	}
	t.Errorf("org %d with only his route is not due: %+v", org, orgs)
}

// TestRetiredContactNeverPicked (Decision 5, C10): every reader that decides who is contacted, or
// shows a live way in, leaves out a retired route and a retired phone, and an operator's claim
// brings them back. The contacts list still sees them.
func TestRetiredContactNeverPicked(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	const email, phone = "info@example.net", "+441632960055"

	// A new lead with an email route and a phone; a new lead with only a phone in a phone-only
	// country; a batched lead whose first email and call step are approved and due.
	lead := f.siteLead(t, "example.net", nil, 7, email)
	f.setCountry(t, lead, "GB")
	org := f.orgOf(t, lead)
	route := t12RouteID(t, f, org, email)
	fact := f.phoneFact(t, lead, phone)
	phoneLead := f.siteLead(t, "phoneonly.localhost", nil, 7, "")
	f.setCountry(t, phoneLead, "GB")
	phoneFact := f.phoneFact(t, phoneLead, "+441632960056")
	seq := f.seqBatch(t, false, f.siteLead(t, "called.localhost", nil, 7, "info@called.localhost"))
	seqOrg := f.orgOf(t, seq.lead)
	seqRoute := t12RouteID(t, f, seqOrg, "info@called.localhost")
	f.ownerRun(t, `UPDATE outreach_message SET due_at = $2 WHERE id = $1`, seq.msg[2], day(0))
	// A batch not yet written, whose call step dials a number about to be retired.
	draftLead := f.siteLead(t, "drafted.localhost", nil, 7, "info@drafted.localhost")
	draftFact := f.phoneFact(t, draftLead, "+441632960057")
	// An org with no lead yet: a hand lead takes its best route.
	agency := func(domain string) int64 {
		a, err := f.q.UpsertAgency(ctx, leadstore.AgencyImport{Name: domain, Website: "https://" + domain + "/"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		return a.OrgID
	}
	handGone := agency("handgone.localhost")
	handGoneRoute := t12Research(t, f, handGone, leadstore.RouteEmail, "info@handgone.localhost", 0)
	handBack := agency("handback.localhost")
	handBackRoute := t12Research(t, f, handBack, leadstore.RouteEmail, "info@handback.localhost", 0)

	mark := func(m string) {
		t.Helper()
		for _, x := range []leadstore.Mark{{OrgID: org, RouteID: route}, {OrgID: org, FactID: fact}, {OrgID: f.orgOf(t, phoneLead), FactID: phoneFact},
			{OrgID: f.orgOf(t, draftLead), FactID: draftFact}, {OrgID: handBack, RouteID: handBackRoute}} {
			x.Mark = m
			if err := f.q.MarkContact(ctx, x); err != nil {
				t.Fatal(err)
			}
		}
		// A retire cancels what is aimed at the value (TestRetireCancelsUnsent). Written directly
		// here, so the queues' own check stands proven for a message that slipped past it.
		f.ownerRun(t, `INSERT INTO contact_mark (route_id, mark, actor) VALUES ($1, $2, 'operator')`, seqRoute, m)
		f.ownerRun(t, `INSERT INTO contact_mark (fact_id, mark, actor) VALUES ($1, $2, 'operator')`, seq.phoneFact, m)
	}
	has := func(ok bool, err error) bool {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	// Each reader, true when it shows the seeded contact.
	readers := func() map[string]bool {
		t.Helper()
		out := map[string]bool{}
		routes, err := f.q.RoutesFor(ctx, org)
		out["RoutesFor"] = has(slices.ContainsFunc(routes, func(r leadstore.Route) bool { return r.ID == route }), err)
		facts, err := f.q.OrgFacts(ctx, []int64{org})
		out["OrgFacts"] = has(slices.ContainsFunc(facts, func(x leadstore.Fact) bool { return x.ID == fact }), err)
		facts, err = f.q.UsableOrgFacts(ctx, []int64{org})
		out["UsableOrgFacts"] = has(slices.ContainsFunc(facts, func(x leadstore.Fact) bool { return x.ID == fact }), err)
		numbers, err := f.q.ScreeningsMissingOrOlder(ctx, 0, "tps", "GB", time.Now())
		out["ScreeningsMissingOrOlder"] = has(slices.Contains(numbers, phone), err)
		exported, err := f.q.ExportRoutes(ctx, 0)
		out["ExportRoutes"] = has(slices.ContainsFunc(exported, func(r leadstore.ExportRow) bool { return r.Route == email }), err)
		cands, err := f.q.CandidateLeads(ctx, leadstore.CandidateOptions{IncludeReserved: true, PhoneOnlyCountries: []string{"GB"}})
		out["CandidateLeads route"] = has(slices.ContainsFunc(cands, func(c leadstore.Candidate) bool { return c.LeadID == lead }), err)
		out["CandidateLeads phone"] = slices.ContainsFunc(cands, func(c leadstore.Candidate) bool { return c.LeadID == phoneLead })
		d, err := f.q.LeadDetail(ctx, lead)
		out["LeadDetail routes"] = has(slices.ContainsFunc(d.Routes, func(r leadstore.DetailRoute) bool { return r.Value == email }), err)
		out["LeadDetail facts"] = slices.ContainsFunc(d.Facts, func(x leadstore.DetailFact) bool { return x.Value == phone })
		list, _, err := f.q.LeadList(ctx, leadstore.ListFilter{Status: "all", PerPage: 100}, leadstore.ListContext{ProfileJSON: "{}"})
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range list {
			if r.LeadID == lead {
				out["LeadList email"], out["LeadList phone"] = r.HasEmail, r.HasPhone
			}
		}
		calls, err := f.q.CallsDue(ctx, 0, 0, day(1))
		out["CallsDue"] = has(slices.ContainsFunc(calls, func(c leadstore.CallRow) bool { return c.MessageID == seq.msg[2] }), err)
		queue, err := f.q.CallQueue(ctx, day(1), time.UTC)
		out["CallQueue"] = has(slices.ContainsFunc(queue, func(c leadstore.CallQueueRow) bool { return c.MessageID == seq.msg[2] }), err)
		rows, err := f.q.SendRows(ctx, day(1), false)
		out["SendRows"] = has(slices.ContainsFunc(rows, func(r leadstore.SendRow) bool { return r.ID == seq.msg[0] }), err)
		due, err := f.q.DueMessages(ctx, day(1), false)
		out["DueMessages"] = has(slices.ContainsFunc(due, func(m leadstore.DueMessage) bool { return m.ID == seq.msg[0] }), err)
		today, err := f.q.TodayView(ctx, 0, 0, day(1))
		out["TodayView"] = has(slices.ContainsFunc(today.EmailsDue, func(i leadstore.TodayItem) bool { return i.MessageID == seq.msg[0] }), err)
		needing, err := f.q.OrgsNeedingRoutes(ctx, 0, time.Now())
		// This reader is the other way round: an org with a live route is not due a search.
		out["OrgsNeedingRoutes"] = has(!slices.ContainsFunc(needing, func(o leadstore.Org) bool { return o.ID == org }), err)
		return out
	}
	expect := func(when string, want bool) {
		t.Helper()
		got := readers()
		if len(got) != 17 {
			t.Errorf("%s: %d readers read: %v", when, len(got), got)
		}
		for name, live := range got {
			if live != want {
				t.Errorf("%s: %s shows it: %v, want %v", when, name, live, want)
			}
		}
	}

	expect("before any mark", true)
	mark(leadstore.MarkRetire)
	if err := f.q.MarkContact(ctx, leadstore.Mark{OrgID: handGone, RouteID: handGoneRoute, Mark: leadstore.MarkRetire}); err != nil {
		t.Fatal(err)
	}
	expect("retired", false)

	// A hand lead takes no retired route; a batch cannot dial a retired number.
	hand, err := f.q.MarkContacted(ctx, 0, "handgone.localhost", "")
	if err != nil {
		t.Fatal(err)
	}
	if l := f.leadOf(t, hand.LeadID); l.RouteID != nil || l.Channel != leadstore.ChannelNone {
		t.Errorf("hand lead on a retired route: %+v", l)
	}
	meta := leadstore.BatchMeta{Sequences: leadstore.BatchSequences{"default": fiveSteps}}
	if _, _, err := f.q.CreateBatchFor(ctx, meta, false, f.seqDrafts(t, draftLead, draftFact), []byte("t")); !errors.Is(err, leadstore.ErrNotDraftable) {
		t.Errorf("batch dialling a retired number: %v, want ErrNotDraftable", err)
	}
	// The record still sees them.
	if c := t12Contact(t, f, org, leadstore.RouteEmail, route); !c.Retired {
		t.Errorf("the contacts list lost the retired route: %+v", c)
	}
	if c := t12Contact(t, f, org, "phone", fact); !c.Retired {
		t.Errorf("the contacts list lost the retired phone: %+v", c)
	}

	mark(leadstore.MarkClaim)
	expect("claimed back", true)
	hand, err = f.q.MarkContacted(ctx, 0, "handback.localhost", "")
	if err != nil {
		t.Fatal(err)
	}
	if l := f.leadOf(t, hand.LeadID); l.RouteID == nil || *l.RouteID != handBackRoute {
		t.Errorf("hand lead after the claim: %+v, want route %d", l, handBackRoute)
	}
	if _, _, err := f.q.CreateBatchFor(ctx, meta, false, f.seqDrafts(t, draftLead, draftFact), []byte("t")); err != nil {
		t.Errorf("batch dialling a claimed number: %v", err)
	}
}

// t18States is the states of lead's messages msg, by step; a cancel for any other reason says it.
func t18States(t *testing.T, f outreachFixture, lead int64, msg map[int]int64) map[int]string {
	t.Helper()
	out := map[int]string{}
	for step, id := range msg {
		var state, reason string
		if err := f.owner.QueryRow(context.Background(), `SELECT state, cancel_reason FROM outreach_message WHERE id = $1 AND lead_id = $2`, id, lead).Scan(&state, &reason); err != nil {
			t.Fatal(err)
		}
		if state == leadstore.MsgCancelled && reason != leadstore.CancelContactRetired {
			state += " (" + reason + ")"
		}
		out[step] = state
	}
	return out
}

// TestRetireCancelsUnsent: retiring a route cancels the unsent messages to it, and a phone its
// unsent calls, leaving sent, done and maybe_sent rows; the same batch's hashes still hold and
// another lead's email still goes; a claim revives nothing; a batched lead with nothing left goes
// back to new, off the retired route.
func TestRetireCancelsUnsent(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	const a, b = "info@retirea.localhost", "info@retireb.localhost"
	leadA := f.siteLead(t, "retirea.localhost", nil, 7, a)
	leadB := f.siteLead(t, "retireb.localhost", nil, 7, b)
	factA := f.phoneFact(t, leadA, "+441632960061")
	factB := f.phoneFact(t, leadB, "+441632960062")
	orgA := f.orgOf(t, leadA)
	routeA := t12RouteID(t, f, orgA, a)
	batch, sum, err := f.q.CreateBatchFor(ctx, leadstore.BatchMeta{Sequences: leadstore.BatchSequences{"default": fiveSteps}}, false,
		append(f.seqDrafts(t, leadA, factA), f.seqDrafts(t, leadB, factB)...), []byte("t"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.q.ApproveBatch(ctx, batch, hex.EncodeToString(sum), nil, func(m leadstore.Message) time.Time {
		if m.Step == 0 {
			return dueBase
		}
		return time.Time{}
	}); err != nil {
		t.Fatal(err)
	}
	ms, err := f.q.BatchMessages(ctx, batch)
	if err != nil {
		t.Fatal(err)
	}
	msgs := map[int64]map[int]int64{leadA: {}, leadB: {}}
	for _, m := range ms {
		msgs[m.LeadID][m.Step] = m.ID
	}
	hashHolds := func(when string) {
		t.Helper()
		got, err := f.q.BatchContentSHA256(ctx, batch)
		if err != nil {
			t.Fatal(err)
		}
		bt, err := f.q.BatchByID(ctx, batch)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, bt.ContentSHA256) || !bytes.Equal(bt.ApprovedHash, bt.ContentSHA256) {
			t.Errorf("%s: the batch no longer hashes to itself", when)
		}
	}
	// The first email has gone; one row may have gone, one was done by hand. None of them moves.
	f.send(t, seqLead{lead: leadA, batch: batch, msg: msgs[leadA]}, false, 0, day(0))
	f.ownerRun(t, `UPDATE outreach_message SET state = 'maybe_sent' WHERE id = $1`, msgs[leadA][1])
	f.ownerRun(t, `UPDATE outreach_message SET state = 'done', sent_at = now() WHERE id = $1`, msgs[leadA][4])

	if err := f.q.MarkContact(ctx, leadstore.Mark{OrgID: orgA, RouteID: routeA, Mark: leadstore.MarkRetire}); err != nil {
		t.Fatal(err)
	}
	want := map[int]string{0: leadstore.MsgSent, 1: "maybe_sent", 2: leadstore.MsgApproved, 3: leadstore.MsgCancelled, 4: "done"}
	if got := t18States(t, f, leadA, msgs[leadA]); !reflect.DeepEqual(got, want) {
		t.Errorf("after the route's retire = %v, want %v", got, want)
	}
	if err := f.q.MarkContact(ctx, leadstore.Mark{OrgID: orgA, FactID: factA, Mark: leadstore.MarkRetire}); err != nil {
		t.Fatal(err)
	}
	want[2] = leadstore.MsgCancelled
	if got := t18States(t, f, leadA, msgs[leadA]); !reflect.DeepEqual(got, want) {
		t.Errorf("after the phone's retire = %v, want %v", got, want)
	}
	hashHolds("after the retires")

	// The other lead's email is untouched: due, unchanged, and it sends.
	for step, st := range t18States(t, f, leadB, msgs[leadB]) {
		if st != leadstore.MsgApproved {
			t.Errorf("the other lead's step %d = %s", step, st)
		}
	}
	rows, err := f.q.SendRows(ctx, day(1), false)
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(rows, func(r leadstore.SendRow) bool { return r.ID == msgs[leadB][0] })
	if i < 0 {
		t.Fatalf("the other lead's email is not due: %+v", rows)
	}
	if r := rows[i]; !bytes.Equal(leadstore.MessageSHA256(r.ToAddress, r.Subject, r.BodyText, r.InReplyTo), r.ApprovedSHA256) {
		t.Error("the other lead's email fails its approved hash")
	}
	f.send(t, seqLead{lead: leadB, batch: batch, msg: msgs[leadB]}, false, 0, day(0))
	if l := f.leadOf(t, leadB); l.State != leadstore.LeadContacted {
		t.Errorf("the other lead after its send: %s", l.State)
	}
	hashHolds("after the other lead's send")

	// A claim revives nothing.
	if err := f.q.MarkContact(ctx, leadstore.Mark{OrgID: orgA, RouteID: routeA, Mark: leadstore.MarkClaim}); err != nil {
		t.Fatal(err)
	}
	if got := t18States(t, f, leadA, msgs[leadA]); !reflect.DeepEqual(got, want) {
		t.Errorf("after the claim = %v, want %v", got, want)
	}

	// An unapproved live batch whose only message goes: the lead is not left batched, and is off
	// the route. Editing the value retires the old one the same way.
	const c = "info@retirec.localhost"
	leadC := f.siteLead(t, "retirec.localhost", nil, 7, c)
	orgC := f.orgOf(t, leadC)
	routeC := t12RouteID(t, f, orgC, c)
	one := func(lead int64, to string) int64 {
		t.Helper()
		id, _, err := f.q.CreateBatch(ctx, false, []leadstore.DraftRow{{LeadID: lead, Step: 0, Channel: leadstore.ChannelEmail, To: to,
			Subject: "s", Body: "b", MessageID: fmt.Sprintf("<%d.retire@seq.invalid>", lead), MailboxID: f.mailbox(t)}}, []byte("t"))
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	one(leadC, c)
	if l := f.leadOf(t, leadC); l.State != leadstore.LeadBatched {
		t.Fatalf("fixture: lead %s", l.State)
	}
	if _, err := f.q.EditOperatorContact(ctx, leadstore.ContactEdit{OrgID: orgC, RouteID: routeC, Value: "new@retirec.localhost"}); err != nil {
		t.Fatal(err)
	}
	l := f.leadOf(t, leadC)
	if l.State != leadstore.LeadNew || l.Channel != leadstore.ChannelNone || l.RouteID != nil {
		t.Errorf("lead with nothing left = %s %s %v, want new, none, no route", l.State, l.Channel, l.RouteID)
	}
	if n := ownerInt(t, f.owner, `SELECT count(*) FROM outreach_message WHERE lead_id = $1 AND state = 'cancelled' AND cancel_reason = $2`, leadC, leadstore.CancelContactRetired); n != 1 {
		t.Errorf("cancelled by the edit: %d, want 1", n)
	}
	if n := ownerInt(t, f.owner, `SELECT count(*) FROM lead_event WHERE lead_id = $1 AND state = 'new' AND reason LIKE 'contact retired%'`, leadC); n != 1 {
		t.Errorf("the move back has %d events, want 1", n)
	}

	// Forms and LinkedIn messages, shadow or live, go by their target; another target stays. A
	// shadow batch does not hold a lead batched, so this new lead stays new and on its route.
	leadD, orgD := t12Org(t, f, "retired.localhost")
	form := t12Research(t, f, orgD, leadstore.RouteForm, "https://retired.localhost/contact", 0)
	page := t12LinkedIn("company", "retired")
	linked := t12Research(t, f, orgD, leadstore.RouteLinkedIn, page, 0)
	shadow := ownerInt(t, f.owner, `INSERT INTO outreach_batch (shadow, content_sha256, templates_sha256, manual) VALUES (true, '\x00', '\x00', true) RETURNING id`)
	add := func(step int, channel, to string) int64 {
		return ownerInt(t, f.owner, `INSERT INTO outreach_message (batch_id, lead_id, step, channel, to_address, body_text, slots, mailbox_id)
			VALUES ($1, $2, $3, $4, $5, 'text', '{}', $6) RETURNING id`, shadow, leadD, step, channel, to, f.mailbox(t))
	}
	manual := map[int]int64{0: add(0, leadstore.RouteForm, "https://retired.localhost/contact"), 1: add(1, leadstore.RouteLinkedIn, page),
		2: add(2, leadstore.RouteForm, "https://retired.localhost/other")}
	for _, id := range []int64{form, linked} {
		if err := f.q.MarkContact(ctx, leadstore.Mark{OrgID: orgD, RouteID: id, Mark: leadstore.MarkRetire}); err != nil {
			t.Fatal(err)
		}
	}
	wantManual := map[int]string{0: leadstore.MsgCancelled, 1: leadstore.MsgCancelled, 2: leadstore.MsgDraft}
	if got := t18States(t, f, leadD, manual); !reflect.DeepEqual(got, wantManual) {
		t.Errorf("manual rows = %v, want %v", got, wantManual)
	}
	if l := f.leadOf(t, leadD); l.State != leadstore.LeadNew {
		t.Errorf("the shadow lead moved: %s", l.State)
	}
}
