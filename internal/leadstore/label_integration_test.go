//go:build integration

package leadstore_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"

	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

// Migration 00015: the operator's own call on a lead.

// TestLeadLabelLatestWins: a call, a changed call and a cleared call are each a row, and
// the latest per lead wins in every read: the latest call per lead, the labelled set (only
// the leads whose latest call names a band, each with that call's input) and the count. A
// lead cleared then called again is labelled again; another lead is never touched.
func TestLeadLabelLatestWins(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	a := f.siteLead(t, "a.localhost", nil, 7, "")
	b := f.siteLead(t, "b.localhost", nil, 8, "")
	never := f.siteLead(t, "never.localhost", nil, 9, "")
	band := func(s string) *string { return &s }
	set := func(lead int64, b *string, input string) {
		t.Helper()
		var doc []byte
		if input != "" {
			doc = []byte(input)
		}
		if _, err := f.q.SetLeadLabel(ctx, lead, b, doc, leadstore.ActorOperator); err != nil {
			t.Fatalf("set %d: %v", lead, err)
		}
	}
	labelled := func() map[int64]string {
		t.Helper()
		ls, err := f.q.LabelledLeads(ctx)
		if err != nil {
			t.Fatal(err)
		}
		out := map[int64]string{}
		for _, l := range ls {
			out[l.LeadID] = *l.Band + " " + string(l.Input)
		}
		n, err := f.q.LabelCount(ctx)
		if err != nil || n != len(out) {
			t.Errorf("LabelCount = %d, %v; want %d", n, err, len(out))
		}
		ids, err := f.q.LabelledLeadIDs(ctx)
		if err != nil || len(ids) != len(out) {
			t.Errorf("LabelledLeadIDs = %v, %v; want %d ids", ids, err, len(out))
		}
		return out
	}
	if got := labelled(); len(got) != 0 {
		t.Fatalf("cold start: %v", got)
	}

	set(a, band(leadstore.BandStrong), `{"kind":"unsupported","major":7}`)
	set(b, band(leadstore.BandNo), `{"kind":"unsupported","major":8}`)
	set(a, band(leadstore.BandLikely), `{"kind":"unsupported","major":7,"country":"GB"}`)
	got := labelled()
	if got[a] != `likely {"kind": "unsupported", "major": 7, "country": "GB"}` || got[b] != `no {"kind": "unsupported", "major": 8}` || len(got) != 2 {
		t.Errorf("after a change: %v, want a likely (with its newer input) and b no", got)
	}
	latest, err := f.q.LatestLabels(ctx, []int64{a, b, never})
	if err != nil {
		t.Fatal(err)
	}
	if l := latest[a]; l.Band == nil || *l.Band != leadstore.BandLikely || l.Domain != "a.localhost" || l.Actor != leadstore.ActorOperator || l.CreatedAt.IsZero() {
		t.Errorf("latest of a: %+v", l)
	}
	if _, ok := latest[never]; ok || len(latest) != 2 {
		t.Errorf("latest: %v, want a and b only", latest)
	}

	set(a, nil, "")
	if got := labelled(); len(got) != 1 || got[b] == "" {
		t.Errorf("after clearing a: %v, want b only", got)
	}
	latest, err = f.q.LatestLabels(ctx, []int64{a})
	if err != nil || latest[a].Band != nil || latest[a].Input != nil {
		t.Errorf("latest of a after a clear: %+v %v, want a cleared call", latest[a], err)
	}
	set(a, band(leadstore.BandMaybe), `{"kind":"unsupported"}`)
	if got := labelled(); len(got) != 2 || got[a] != `maybe {"kind": "unsupported"}` {
		t.Errorf("after calling a again: %v", got)
	}
	if n := dbCountQ(t, f, `SELECT count(*) FROM lead_label`); n != 5 {
		t.Errorf("%d rows, want 5: every call and clear is its own row", n)
	}
}

// TestLeadLabelRefuses: a band that is not one, a call without input or with one that is not
// an object, a clear with input, an unknown actor and a lead that is not there write nothing;
// the table itself refuses a bad band or a call without input, and the app role can neither
// change nor delete a call.
func TestLeadLabelRefuses(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	a := f.siteLead(t, "a.localhost", nil, 7, "")
	band := func(s string) *string { return &s }
	obj := []byte(`{"kind":"unsupported"}`)
	for name, c := range map[string]struct {
		lead  int64
		band  *string
		input []byte
		actor string
		want  error
	}{
		"not a band":         {a, band("great"), obj, leadstore.ActorOperator, leadstore.ErrBadLabel},
		"upper case":         {a, band("Strong"), obj, leadstore.ActorOperator, leadstore.ErrBadLabel},
		"empty band":         {a, band(""), obj, leadstore.ActorOperator, leadstore.ErrBadLabel},
		"no input":           {a, band("strong"), nil, leadstore.ActorOperator, leadstore.ErrBadLabelInput},
		"array input":        {a, band("strong"), []byte(`[1]`), leadstore.ActorOperator, leadstore.ErrBadLabelInput},
		"broken input":       {a, band("strong"), []byte(`{"kind":`), leadstore.ActorOperator, leadstore.ErrBadLabelInput},
		"clear with input":   {a, nil, obj, leadstore.ActorOperator, leadstore.ErrBadLabelInput},
		"unknown actor":      {a, band("strong"), obj, leadstore.ActorGremy, leadstore.ErrUnknownActor},
		"no such lead":       {a + 1000, band("strong"), obj, leadstore.ActorOperator, leadstore.ErrLeadNotFound},
		"no such lead clear": {a + 1000, nil, nil, leadstore.ActorOperator, leadstore.ErrLeadNotFound},
	} {
		if _, err := f.q.SetLeadLabel(ctx, c.lead, c.band, c.input, c.actor); !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", name, err, c.want)
		}
	}
	if n := dbCountQ(t, f, `SELECT count(*) FROM lead_label`); n != 0 {
		t.Errorf("%d rows written by refused calls", n)
	}
	for _, s := range []string{
		`INSERT INTO lead_label (lead_id, band, input, actor) VALUES ($1, 'great', '{}', 'operator')`,
		`INSERT INTO lead_label (lead_id, band, input, actor) VALUES ($1, 'strong', NULL, 'operator')`,
		`INSERT INTO lead_label (lead_id, band, input, actor) VALUES ($1, NULL, '{}', 'operator')`,
		`INSERT INTO lead_label (lead_id, band, input, actor) VALUES ($1, 'strong', '[1]', 'operator')`,
		`INSERT INTO lead_label (lead_id, band, input, actor) VALUES ($1, 'strong', '{}', 'gremy')`,
	} {
		if _, err := f.owner.Exec(ctx, s, a); sqlState(err) != "23514" {
			t.Errorf("%s as owner: %v, want 23514 check_violation", s, err)
		}
	}
	if _, err := f.q.SetLeadLabel(ctx, a, band("strong"), obj, leadstore.ActorOperator); err != nil {
		t.Fatal(err)
	}
	app := storetest.AppConn(t)
	for _, s := range []string{"UPDATE lead_label SET band = 'no'", "DELETE FROM lead_label"} {
		if _, err := app.Exec(ctx, s); sqlState(err) != "42501" {
			t.Errorf("%s as app: %v, want 42501", s, err)
		}
	}
}

// TestRateLeadByIDMatchesLeadsToRate: one lead read by id is exactly the row LeadsToRate
// gives it, so a call snapshots what the relevance question reads; a missing lead is
// ErrLeadNotFound.
func TestRateLeadByIDMatchesLeadsToRate(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	a := f.siteLead(t, "a.localhost", nil, 7, "info@a.localhost")
	f.siteLead(t, "b.localhost", nil, 10, "")
	all, err := f.q.LeadsToRate(ctx, leadstore.ListContext{})
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(all, func(l leadstore.RateLead) bool { return l.LeadID == a })
	if i < 0 {
		t.Fatalf("lead %d not in LeadsToRate: %+v", a, all)
	}
	one, err := f.q.RateLeadByID(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := json.Marshal(all[i])
	got, _ := json.Marshal(one)
	if string(got) != string(want) {
		t.Errorf("RateLeadByID = %s\nLeadsToRate = %s", got, want)
	}
	if _, err := f.q.RateLeadByID(ctx, a+1000); !errors.Is(err, leadstore.ErrLeadNotFound) {
		t.Errorf("no such lead: %v", err)
	}
}

// dbCountQ is the single integer q returns, read as the owner.
func dbCountQ(t *testing.T, f outreachFixture, q string) int {
	t.Helper()
	var n int
	if err := f.owner.QueryRow(context.Background(), q).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
