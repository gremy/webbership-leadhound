//go:build integration

package leadstore_test

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

// Migration 00011: stars and the operator's order.

const starVersion = 11 // 00011_lead_star.sql

// starOrder returns the starred lead ids in order.
func starOrder(t *testing.T, q leadstore.Queries) []int64 {
	t.Helper()
	order, err := q.StarOrder(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ids := []int64{}
	for _, s := range order {
		ids = append(ids, s.LeadID)
	}
	return ids
}

func TestStarOrderGrid(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	a := f.siteLead(t, "a.localhost", nil, 7, "")
	b := f.siteLead(t, "b.localhost", nil, 7, "")
	c := f.siteLead(t, "c.localhost", nil, 7, "")
	op := leadstore.ActorOperator
	step := func(what string, fn func() (bool, error), wantChange bool, want []int64) {
		t.Helper()
		changed, err := fn()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		if changed != wantChange {
			t.Errorf("%s: changed %v, want %v", what, changed, wantChange)
		}
		if got := starOrder(t, f.q); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: order %v, want %v", what, got, want)
		}
	}
	if got := starOrder(t, f.q); len(got) != 0 {
		t.Fatalf("cold start order = %v", got)
	}
	step("star a", func() (bool, error) { return f.q.Star(ctx, a, op) }, true, []int64{a})
	step("star b", func() (bool, error) { return f.q.Star(ctx, b, op) }, true, []int64{a, b})
	step("star c", func() (bool, error) { return f.q.Star(ctx, c, op) }, true, []int64{a, b, c})
	step("star b again", func() (bool, error) { return f.q.Star(ctx, b, op) }, false, []int64{a, b, c})
	step("down a", func() (bool, error) { return f.q.MoveDown(ctx, a, op) }, true, []int64{b, a, c})
	step("top c", func() (bool, error) { return f.q.MoveTop(ctx, c, op) }, true, []int64{c, b, a})
	step("top c again", func() (bool, error) { return f.q.MoveTop(ctx, c, op) }, false, []int64{c, b, a})
	step("up c (the first)", func() (bool, error) { return f.q.MoveUp(ctx, c, op) }, false, []int64{c, b, a})
	step("down a (the last)", func() (bool, error) { return f.q.MoveDown(ctx, a, op) }, false, []int64{c, b, a})
	step("unstar b", func() (bool, error) { return f.q.Unstar(ctx, b, op) }, true, []int64{c, a})
	step("unstar b again", func() (bool, error) { return f.q.Unstar(ctx, b, op) }, false, []int64{c, a})
	step("star b", func() (bool, error) { return f.q.Star(ctx, b, op) }, true, []int64{c, a, b})
	step("up b", func() (bool, error) { return f.q.MoveUp(ctx, b, op) }, true, []int64{c, b, a})

	d := f.siteLead(t, "d.localhost", nil, 7, "")
	for name, fn := range map[string]func(context.Context, int64, string) (bool, error){
		"up": f.q.MoveUp, "down": f.q.MoveDown, "top": f.q.MoveTop,
	} {
		if _, err := fn(ctx, d, op); !errors.Is(err, leadstore.ErrNotStarred) {
			t.Errorf("%s on an unstarred lead: %v, want ErrNotStarred", name, err)
		}
	}
	if _, err := f.q.Star(ctx, 99999, op); !errors.Is(err, leadstore.ErrLeadNotFound) {
		t.Errorf("star a missing lead: %v, want ErrLeadNotFound", err)
	}
	if _, err := f.q.Star(ctx, d, leadstore.ActorGremy); !errors.Is(err, leadstore.ErrUnknownActor) {
		t.Errorf("star as gremy: %v, want ErrUnknownActor", err)
	}
	// Append-only: every write was a row, and the app cannot rewrite one.
	app := storetest.AppConn(t)
	for _, q := range []string{"UPDATE lead_star SET position = 1", "DELETE FROM lead_star"} {
		if _, err := app.Exec(ctx, q); sqlState(err) != "42501" {
			t.Errorf("%s as app: %v, want 42501", q, err)
		}
	}
	if _, err := app.Exec(ctx, `INSERT INTO lead_star (lead_id, action, actor) VALUES ($1, 'star', 'operator')`, d); sqlState(err) != "23514" {
		t.Errorf("a star without a position: %v, want 23514", err)
	}
	if _, err := app.Exec(ctx, `INSERT INTO lead_star (lead_id, action, position, actor) VALUES ($1, 'unstar', 1, 'operator')`, d); sqlState(err) != "23514" {
		t.Errorf("an unstar with a position: %v, want 23514", err)
	}
}

// TestStarMovesConcurrent: twenty moves at once, each in its own transaction, leave every
// starred lead on a position of its own.
func TestStarMovesConcurrent(t *testing.T) {
	f := newOutreachFixture(t)
	ctx := context.Background()
	var leads []int64
	for _, d := range []string{"a", "b", "c", "d", "e"} {
		id := f.siteLead(t, d+".localhost", nil, 7, "")
		if _, err := f.q.Star(ctx, id, leadstore.ActorOperator); err != nil {
			t.Fatal(err)
		}
		leads = append(leads, id)
	}
	pool, err := pgxpool.New(ctx, storetest.AppDSN())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	q := leadstore.NewQueries(pool)
	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for i := range 20 {
		wg.Add(1)
		go func(lead int64) {
			defer wg.Done()
			if _, err := q.MoveDown(ctx, lead, leadstore.ActorOperator); err != nil {
				errs <- err
			}
		}(leads[i%len(leads)])
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent move down: %v", err)
	}
	order, err := f.q.StarOrder(ctx)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[int64]bool{}
	for _, s := range order {
		if seen[s.Position] {
			t.Errorf("position %d held twice: %+v", s.Position, order)
		}
		seen[s.Position] = true
	}
	if len(order) != len(leads) {
		t.Errorf("starred after the moves = %d, want %d", len(order), len(leads))
	}
}

// TestPinsBecomeStars: a database at 00010 with two pinned leads (and one cleared, one
// deferred) gets those two starred at 00011, the newest pin at the top, each with a system
// clear as its latest priority row; the defer stays.
func TestPinsBecomeStars(t *testing.T) {
	ctx := context.Background()
	owner := storetest.OwnerConn(t)
	p := migrateLeadsTo(t, owner, starVersion-1)
	q := leadstore.NewQueries(owner)
	lead := func(domain string) int64 {
		org := ownerInt(t, owner, `INSERT INTO org (kind, domain, source) VALUES ('site_owner', $1, 'probe') RETURNING id`, domain)
		// The schema is at 00010, before campaigns (00016): the lead is written by hand, as
		// UpsertLead writes a hand lead there.
		return ownerInt(t, owner, `INSERT INTO lead (org_id, kind, channel) VALUES ($1, 'hand', 'none') RETURNING id`, org)
	}
	older, newer, cleared, deferred := lead("older.localhost"), lead("newer.localhost"), lead("cleared.localhost"), lead("deferred.localhost")
	priority := func(id int64, action string) {
		if action == "defer" {
			ownerRun(t, owner, `INSERT INTO lead_priority (lead_id, action, until, actor) VALUES ($1, 'defer', '2026-12-01', 'gremy')`, id)
			return
		}
		ownerRun(t, owner, `INSERT INTO lead_priority (lead_id, action, actor) VALUES ($1, $2, 'gremy')`, id, action)
	}
	priority(older, "pin")
	priority(cleared, "pin")
	priority(newer, "pin")
	priority(cleared, "clear")
	priority(deferred, "defer")

	if _, err := p.UpTo(ctx, starVersion); err != nil {
		t.Fatalf("leads up to %d: %v", starVersion, err)
	}
	if got := starOrder(t, q); !reflect.DeepEqual(got, []int64{newer, older}) {
		t.Errorf("stars after 00011 = %v, want [newer %d, older %d]", got, newer, older)
	}
	latest := func(id int64) (action, actor string) {
		if err := owner.QueryRow(ctx, `SELECT action, actor FROM lead_priority WHERE lead_id = $1 ORDER BY id DESC LIMIT 1`, id).Scan(&action, &actor); err != nil {
			t.Fatal(err)
		}
		return action, actor
	}
	for _, id := range []int64{older, newer} {
		if action, actor := latest(id); action != leadstore.PriorityClear || actor != leadstore.ActorSystem {
			t.Errorf("lead %d latest priority = %s by %s, want clear by system", id, action, actor)
		}
	}
	if action, _ := latest(deferred); action != leadstore.PriorityDefer {
		t.Errorf("the deferred lead's latest priority = %s, want defer", action)
	}
	if n := ownerInt(t, owner, `SELECT count(*) FROM lead_star WHERE actor <> 'system'`); n != 0 {
		t.Errorf("%d converted stars not by the system", n)
	}

	// Down pins the starred leads again, the top star as the newest pin.
	if _, err := p.DownTo(ctx, starVersion-1); err != nil {
		t.Fatalf("down to %d: %v", starVersion-1, err)
	}
	var pins []int64
	rows, err := owner.Query(ctx, `SELECT lead_id FROM (SELECT DISTINCT ON (lead_id) lead_id, id, action FROM lead_priority ORDER BY lead_id, id DESC) p
		WHERE action = 'pin' ORDER BY id DESC`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		pins = append(pins, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(pins, []int64{newer, older}) {
		t.Errorf("pins after down = %v (newest first), want [%d %d]", pins, newer, older)
	}
}
