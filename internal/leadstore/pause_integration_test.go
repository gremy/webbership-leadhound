//go:build integration

package leadstore_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/leadstore/leadstoretest"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

// TestSendPause: no row is not paused; the latest row wins; every switch stays as a row.
func TestSendPause(t *testing.T) {
	leadstoretest.Reset(t)
	ctx := context.Background()
	q := leadstoretest.Q(t)
	check := func(when string, wantPaused bool, wantReason string) {
		t.Helper()
		paused, reason, at, err := q.SendPaused(ctx)
		if err != nil {
			t.Fatalf("%s: %v", when, err)
		}
		if paused != wantPaused || reason != wantReason {
			t.Errorf("%s: paused %v %q, want %v %q", when, paused, reason, wantPaused, wantReason)
		}
		if when != "cold start" && at.IsZero() {
			t.Errorf("%s: no time", when)
		}
	}
	check("cold start", false, "")
	if err := q.SetSendPause(ctx, true, "checking the copy"); err != nil {
		t.Fatal(err)
	}
	check("paused", true, "checking the copy")
	if err := q.SetSendPause(ctx, false, ""); err != nil {
		t.Fatal(err)
	}
	check("resumed", false, "")
	if err := q.SetSendPause(ctx, true, "again"); err != nil {
		t.Fatal(err)
	}
	check("paused again", true, "again")
	var rows int
	if err := storetest.OwnerConn(t).QueryRow(ctx, `SELECT count(*) FROM send_pause WHERE actor = $1`, leadstore.ActorGremy).Scan(&rows); err != nil || rows != 3 {
		t.Errorf("send_pause rows %d (%v), want 3", rows, err)
	}
}

// TestSendPauseAppendOnly: the app role inserts and reads send_pause but never rewrites or
// deletes a row; it may set outreach_batch.discarded_at and no other new column.
func TestSendPauseAppendOnly(t *testing.T) {
	leadstoretest.Reset(t)
	ctx := context.Background()
	if err := leadstoretest.Q(t).SetSendPause(ctx, true, "x"); err != nil {
		t.Fatal(err)
	}
	app := storetest.AppConn(t)
	for _, q := range []string{"UPDATE send_pause SET paused = false", "DELETE FROM send_pause", "TRUNCATE send_pause"} {
		if _, err := app.Exec(ctx, q); sqlState(err) != "42501" {
			t.Errorf("%s as app: err = %v, want 42501", q, err)
		}
	}
	owner := storetest.OwnerConn(t)
	grants, err := os.ReadFile("../../deploy/sql/grants.sql")
	if err != nil {
		t.Fatal(err)
	}
	assert := func(when string) {
		t.Helper()
		if appHas(t, owner, "send_pause", "UPDATE") || !appHas(t, owner, "send_pause", "INSERT") || !appHas(t, owner, "send_pause", "SELECT") {
			t.Errorf("%s: send_pause wants INSERT and SELECT only", when)
		}
		if !appHasColumn(t, owner, "outreach_batch", "discarded_at", "UPDATE") {
			t.Errorf("%s: leadhound_app lacks UPDATE on outreach_batch.discarded_at", when)
		}
		assertLeadPrivileges(t, owner, when)
	}
	assert("after migrate")
	if _, err := owner.Exec(ctx, string(grants)); err != nil {
		t.Fatalf("grants.sql: %v", err)
	}
	assert("after grants.sql")
	if err := leadstore.Migrate(ctx, storetest.MigrateDSN()); err != nil {
		t.Fatal(err)
	}
	assert("after grants.sql then migrate")
}

// TestPriorityTablesRevoked: the revokes for the priority tables (created by a later
// migration) take UPDATE away once the tables exist, from both the migrate and grants.sql.
// Before the tables exist each revoke is skipped (TestLeadsMigrateFromEmpty runs both on an
// empty database). When a migration already made them, the test only checks them.
func TestPriorityTablesRevoked(t *testing.T) {
	leadstoretest.Migrated(t)
	ctx := context.Background()
	owner := storetest.OwnerConn(t)
	grants, err := os.ReadFile("../../deploy/sql/grants.sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"priority_profile", "lead_priority"} {
		var exists bool
		if err := owner.QueryRow(ctx, `SELECT to_regclass('public.' || $1) IS NOT NULL`, table).Scan(&exists); err != nil {
			t.Fatal(err)
		}
		if !exists {
			// A stand-in made by the owner gets UPDATE through the default privileges.
			if _, err := owner.Exec(ctx, `CREATE TABLE `+table+` (id bigserial PRIMARY KEY)`); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if _, err := storetest.OwnerConn(t).Exec(context.Background(), `DROP TABLE `+table); err != nil {
					t.Errorf("drop %s: %v", table, err)
				}
			})
			if !appHas(t, owner, table, "UPDATE") {
				t.Fatalf("%s: the stand-in has no UPDATE to take away", table)
			}
		}
		if err := leadstore.Migrate(ctx, storetest.MigrateDSN()); err != nil {
			t.Fatal(err)
		}
		if appHas(t, owner, table, "UPDATE") || !appHas(t, owner, table, "INSERT") {
			t.Errorf("after migrate: %s wants INSERT without UPDATE", table)
		}
		if _, err := owner.Exec(ctx, `GRANT UPDATE ON `+table+` TO leadhound_app`); err != nil {
			t.Fatal(err)
		}
		if _, err := owner.Exec(ctx, string(grants)); err != nil {
			t.Fatalf("grants.sql: %v", err)
		}
		if appHas(t, owner, table, "UPDATE") {
			t.Errorf("after grants.sql: %s keeps UPDATE", table)
		}
	}
}

// TestFirstLiveSend: the earliest live sent message; shadow, selftest and maybe_sent rows
// never count, and none is nil.
func TestFirstLiveSend(t *testing.T) {
	leadstoretest.Reset(t)
	ctx := context.Background()
	owner := storetest.OwnerConn(t)
	q := leadstoretest.Q(t)
	id := func(sql string, args ...any) int64 {
		t.Helper()
		var v int64
		if err := owner.QueryRow(ctx, sql+" RETURNING id", args...).Scan(&v); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return v
	}
	lead := func(domain, kind string) int64 {
		okind, source := "site_owner", "probe"
		if kind == leadstore.LeadSelftest {
			okind, source = "selftest", "selftest"
		}
		org := id(`INSERT INTO org (kind, domain, source) VALUES ($1, $2, $3)`, okind, domain, source)
		return id(`INSERT INTO lead (org_id, kind, channel, state) VALUES ($1, $2, 'email', 'contacted')`, org, kind)
	}
	batch := func(shadow bool) int64 {
		return id(`INSERT INTO outreach_batch (shadow, content_sha256, templates_sha256) VALUES ($1, '\x00', '\x00')`, shadow)
	}
	msg := func(b, l int64, state string, sent time.Time) {
		id(`INSERT INTO outreach_message (batch_id, lead_id, step, channel, body_text, slots, state, sent_at)
		    VALUES ($1, $2, 0, 'email', 'body', '{}', $3, $4)`, b, l, state, sent)
	}
	if got, err := q.FirstLiveSend(ctx); err != nil || got != nil {
		t.Fatalf("cold start: %v, %v", got, err)
	}
	base := time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)
	live, shadow := batch(false), batch(true)
	msg(shadow, lead("shadow.localhost", "hand"), "shadow_sent", base)
	msg(live, lead("self.localhost", "selftest"), "sent", base.Add(time.Hour))
	msg(live, lead("maybe.localhost", "hand"), "maybe_sent", base.Add(2*time.Hour))
	if got, err := q.FirstLiveSend(ctx); err != nil || got != nil {
		t.Fatalf("shadow, selftest and maybe_sent counted: %v, %v", got, err)
	}
	msg(live, lead("later.localhost", "hand"), "sent", base.Add(48*time.Hour))
	msg(live, lead("first.localhost", "hand"), "sent", base.Add(24*time.Hour))
	got, err := q.FirstLiveSend(ctx)
	if err != nil || got == nil || !got.Equal(base.Add(24*time.Hour)) {
		t.Fatalf("first live send %v (%v), want %v", got, err, base.Add(24*time.Hour))
	}
}
