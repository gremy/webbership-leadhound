//go:build integration

package leadstore_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

// TestEmailCheckStore: a check is stored lower-cased; an address's fresh checks come newest
// first and older ones are left out; a day's checks are read for the cap; the table refuses an
// address not in lower case, another provider and an UPDATE by the app; a bad row is refused
// before any write.
func TestEmailCheckStore(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	q := f.q
	now := time.Now()
	score := 91
	yes := true
	add := func(addr, status string, at time.Time) int64 {
		t.Helper()
		id, err := q.AddEmailCheck(ctx, leadstore.EmailCheck{Address: addr, Provider: "reoon", Status: status, CheckedAt: at,
			Score: &score, Role: &yes})
		if err != nil {
			t.Fatalf("add %s %s: %v", addr, status, err)
		}
		return id
	}

	// Cold start: nothing for anyone.
	got, err := q.EmailChecks(ctx, 0, []string{"info@a.example"}, now.Add(-90*24*time.Hour))
	if err != nil || len(got) != 0 {
		t.Fatalf("cold start: %v %v", got, err)
	}

	add("Info@A.example", "unknown", now.Add(-48*time.Hour))
	add("info@a.example", "safe", now.Add(-time.Hour))
	add("info@a.example", "invalid", now.Add(-100*24*time.Hour)) // stale: id is newest, but older than the cutoff
	add("hello@b.example", "catch_all", now.Add(-time.Minute))

	got, err = q.EmailChecks(ctx, leadstore.DefaultAccountID, []string{"INFO@a.example", "hello@b.example", "none@c.example"}, now.Add(-90*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	a := got["info@a.example"]
	if len(a) != 2 || a[0].Status != "safe" || a[1].Status != "unknown" || a[0].Score == nil || *a[0].Score != 91 || a[0].Role == nil || !*a[0].Role || a[0].CatchAll != nil {
		t.Errorf("info@a: %+v", a)
	}
	if b := got["hello@b.example"]; len(b) != 1 || b[0].Status != "catch_all" || b[0].Address != "hello@b.example" {
		t.Errorf("hello@b: %+v", b)
	}
	if _, ok := got["none@c.example"]; ok || len(got) != 2 {
		t.Errorf("keys %v", got)
	}

	day, err := q.EmailChecksSince(ctx, 0, now.Add(-2*time.Hour))
	if err != nil || len(day) != 2 || day[0].Status != "safe" || day[1].Status != "catch_all" {
		t.Errorf("today's checks: %+v %v", day, err)
	}

	// Refused before any write.
	for _, c := range []leadstore.EmailCheck{{Address: " ", Provider: "reoon", Status: "safe"}, {Address: "x@y.example", Provider: "reoon"}} {
		if _, err := q.AddEmailCheck(ctx, c); !errors.Is(err, leadstore.ErrBadEmailCheck) {
			t.Errorf("%+v: %v, want ErrBadEmailCheck", c, err)
		}
	}
	// The table's own checks, and the app role's privileges.
	app := storetest.AppConn(t)
	for _, stmt := range []string{
		`INSERT INTO email_check (address, provider, status) VALUES ('Upper@x.example', 'reoon', 'safe')`,
		`INSERT INTO email_check (address, provider, status) VALUES ('a@x.example', 'zerobounce', 'safe')`,
		`INSERT INTO email_check (address, provider, status) VALUES ('a@x.example', 'reoon', 'Not A Status')`,
	} {
		if _, err := app.Exec(ctx, stmt); sqlState(err) != "23514" {
			t.Errorf("%s: %v, want a check violation", stmt, err)
		}
	}
	if _, err := app.Exec(ctx, `UPDATE email_check SET status = 'safe'`); sqlState(err) != "42501" {
		t.Errorf("UPDATE as app: %v, want insufficient_privilege", err)
	}
}

// TestEmailCheckLeadRoute: SetNewLeadRoute moves a new lead onto another route of its own org,
// never onto another org's route, never a lead past new, and reports no move when nothing
// changes.
func TestEmailCheckLeadRoute(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	q := f.q
	id := f.siteLead(t, "route.example", nil, 7, "info@route.example")
	other := f.siteLead(t, "other.example", nil, 7, "info@other.example")
	l, err := q.LeadByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.InsertRoute(ctx, leadstore.RouteRow{OrgID: l.OrgID, Kind: leadstore.RouteForm, Value: "https://route.example/contact",
		FetchID: f.fid, DerivedBy: "contacts@1"}); err != nil {
		t.Fatal(err)
	}
	routes, err := q.RoutesFor(ctx, l.OrgID)
	if err != nil {
		t.Fatal(err)
	}
	var form int64
	for _, r := range routes {
		if r.Kind == leadstore.RouteForm {
			form = r.ID
		}
	}
	ol, err := q.LeadByID(ctx, other)
	if err != nil {
		t.Fatal(err)
	}
	if moved, err := q.SetNewLeadRoute(ctx, id, leadstore.ChannelEmail, *ol.RouteID); err != nil || moved {
		t.Errorf("another org's route: moved %v, %v", moved, err)
	}
	if moved, err := q.SetNewLeadRoute(ctx, id, leadstore.ChannelForm, form); err != nil || !moved {
		t.Fatalf("form route: moved %v, %v", moved, err)
	}
	if l, _ = q.LeadByID(ctx, id); l.Channel != leadstore.ChannelForm || l.RouteID == nil || *l.RouteID != form {
		t.Errorf("after move: %+v", l)
	}
	if moved, err := q.SetNewLeadRoute(ctx, id, leadstore.ChannelForm, form); err != nil || moved {
		t.Errorf("same route again: moved %v, %v", moved, err)
	}
	if _, err := f.owner.Exec(ctx, `UPDATE lead SET state = 'skipped' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if moved, err := q.SetNewLeadRoute(ctx, id, leadstore.ChannelEmail, routes[0].ID); err != nil || moved {
		t.Errorf("lead past new: moved %v, %v", moved, err)
	}
}
