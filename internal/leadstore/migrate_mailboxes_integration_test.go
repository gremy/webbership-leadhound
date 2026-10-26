//go:build integration

package leadstore_test

import (
	"context"
	"database/sql"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/gremy/webbership-leadhound/internal/leadstore/leadstoretest"
	"github.com/gremy/webbership-leadhound/internal/store"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

// Migration 00008 (MB-R2): rows from before mailboxes are given one retired placeholder
// mailbox, then mailbox_id is required and the event and cursor keys carry the mailbox.

const (
	placeholderAddress = "legacy@legacy.invalid"
	mailboxesRequired  = 8 // 00008_mailboxes_required.sql
)

// leadsProvider is a goose provider over the leads migrations on the test database, as the
// owner, for stepping to a given version.
func leadsProvider(t *testing.T) *goose.Provider {
	t.Helper()
	cfg, err := pgx.ParseConfig(storetest.MigrateDSN())
	if err != nil {
		t.Fatal(err)
	}
	db := stdlib.OpenDB(*cfg)
	t.Cleanup(func() { _ = db.Close() })
	p, err := goose.NewProvider(goose.DialectPostgres, db, os.DirFS("migrations"),
		goose.WithTableName("goose_db_version_leads"))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// toVersionBefore08 empties the database and migrates it to 00007: core, then leads up to 7.
func toVersionBefore08(t *testing.T, owner *pgx.Conn) *goose.Provider {
	t.Helper()
	dropSchema(t, owner)
	t.Cleanup(func() {
		migrateAll(t)
		leadstoretest.Reset(t)
	})
	if err := store.Migrate(context.Background(), storetest.MigrateDSN()); err != nil {
		t.Fatalf("core migrate: %v", err)
	}
	p := leadsProvider(t)
	if _, err := p.UpTo(context.Background(), mailboxesRequired-1); err != nil {
		t.Fatalf("leads up to %d: %v", mailboxesRequired-1, err)
	}
	return p
}

func ownerInt(t *testing.T, owner *pgx.Conn, q string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := owner.QueryRow(context.Background(), q, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return n
}

func ownerRun(t *testing.T, owner *pgx.Conn, q string, args ...any) {
	t.Helper()
	if _, err := owner.Exec(context.Background(), q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

// columnNullable reports whether table.mailbox_id may be NULL.
func columnNullable(t *testing.T, owner *pgx.Conn, table string) bool {
	t.Helper()
	var yes string
	if err := owner.QueryRow(context.Background(),
		`SELECT is_nullable FROM information_schema.columns WHERE table_schema = 'public' AND table_name = $1 AND column_name = 'mailbox_id'`,
		table).Scan(&yes); err != nil {
		t.Fatalf("%s.mailbox_id: %v", table, err)
	}
	return yes == "YES"
}

// keyColumns returns the columns of constraint name, in key order ("" when it does not exist).
func keyColumns(t *testing.T, owner *pgx.Conn, name string) string {
	t.Helper()
	var cols sql.NullString
	if err := owner.QueryRow(context.Background(),
		`SELECT string_agg(a.attname, ',' ORDER BY k.ord)
		 FROM pg_constraint c
		 CROSS JOIN LATERAL unnest(c.conkey) WITH ORDINALITY AS k(attnum, ord)
		 JOIN pg_attribute a ON a.attrelid = c.conrelid AND a.attnum = k.attnum
		 WHERE c.conname = $1`, name).Scan(&cols); err != nil {
		t.Fatalf("constraint %s: %v", name, err)
	}
	return cols.String
}

var mailboxTables = []string{"outreach_message", "mailbox_event", "mailbox_cursor"}

func TestMigrationMailboxesRequiredBackfills(t *testing.T) {
	ctx := context.Background()
	owner := storetest.OwnerConn(t)
	p := toVersionBefore08(t, owner)

	// At 00007: a real mailbox with one message, and one row of each table with no mailbox.
	realMB := ownerInt(t, owner, `INSERT INTO sender_mailbox (address, domain, login, smtp_addr, imap_addr, dkim_selector, password_sealed, sealed_by)
		VALUES ('outreach@sender.example', 'sender.example', 'outreach@sender.example', 'smtp.sender.invalid:465', 'imap.sender.invalid:993', 'lh', '\x00', 'k')
		RETURNING id`)
	org := ownerInt(t, owner, `INSERT INTO org (kind, domain, source) VALUES ('site_owner', 'northwind.example', 'probe') RETURNING id`)
	org2 := ownerInt(t, owner, `INSERT INTO org (kind, domain, source) VALUES ('site_owner', 'southwind.example', 'probe') RETURNING id`)
	lead := ownerInt(t, owner, `INSERT INTO lead (org_id, kind, channel) VALUES ($1, 'hand', 'email') RETURNING id`, org)
	lead2 := ownerInt(t, owner, `INSERT INTO lead (org_id, kind, channel) VALUES ($1, 'hand', 'email') RETURNING id`, org2)
	batch := ownerInt(t, owner, `INSERT INTO outreach_batch (shadow, content_sha256, templates_sha256) VALUES (false, '\x00', '\x00') RETURNING id`)
	old := ownerInt(t, owner, `INSERT INTO outreach_message (batch_id, lead_id, step, channel, body_text, slots)
		VALUES ($1, $2, 0, 'email', 'hello', '{}') RETURNING id`, batch, lead)
	kept := ownerInt(t, owner, `INSERT INTO outreach_message (batch_id, lead_id, step, channel, body_text, slots, mailbox_id)
		VALUES ($1, $2, 0, 'email', 'hello', '{}', $3) RETURNING id`, batch, lead2, realMB)
	ownerRun(t, owner, `INSERT INTO mailbox_event (mailbox, uidvalidity, uid, kind) VALUES ('INBOX', 7, 1, 'unmatched')`)
	ownerRun(t, owner, `INSERT INTO mailbox_cursor (mailbox, uidvalidity, last_uid, synced_at) VALUES ('INBOX', 7, 1, now())`)

	if _, err := p.UpTo(ctx, mailboxesRequired); err != nil {
		t.Fatalf("up to %d: %v", mailboxesRequired, err)
	}
	if n := ownerInt(t, owner, `SELECT count(*) FROM sender_mailbox`); n != 2 {
		t.Fatalf("%d mailboxes after 00008, want the real one and one placeholder", n)
	}
	legacy := ownerInt(t, owner, `SELECT id FROM sender_mailbox WHERE address = $1 AND domain = 'legacy.invalid'
		AND login = $1 AND smtp_addr = 'legacy.invalid:465' AND imap_addr = 'legacy.invalid:993'
		AND password_sealed = ''::bytea AND sealed_by = ''`, placeholderAddress)
	var state, reason, actor string
	if err := owner.QueryRow(ctx, `SELECT state, reason, actor FROM mailbox_state WHERE mailbox_id = $1 ORDER BY id DESC LIMIT 1`,
		legacy).Scan(&state, &reason, &actor); err != nil {
		t.Fatalf("placeholder state: %v", err)
	}
	if state != "retired" || reason != "rows from before mailboxes" || actor != "system" {
		t.Errorf("placeholder state %q %q %q, want retired by system", state, reason, actor)
	}
	if n := ownerInt(t, owner, `SELECT count(*) FROM mailbox_state WHERE mailbox_id = $1`, legacy); n != 1 {
		t.Errorf("placeholder has %d state rows, want 1", n)
	}
	if n := ownerInt(t, owner, `SELECT count(*) FROM mailbox_check WHERE mailbox_id = $1`, legacy); n != 0 {
		t.Errorf("placeholder has %d checks, want none", n)
	}
	for id, want := range map[int64]int64{old: legacy, kept: realMB} {
		if got := ownerInt(t, owner, `SELECT mailbox_id FROM outreach_message WHERE id = $1`, id); got != want {
			t.Errorf("message %d mailbox %d, want %d", id, got, want)
		}
	}
	for _, tbl := range []string{"mailbox_event", "mailbox_cursor"} {
		if got := ownerInt(t, owner, `SELECT mailbox_id FROM `+tbl); got != legacy { // tbl is a test constant
			t.Errorf("%s mailbox %d, want the placeholder %d", tbl, got, legacy)
		}
	}
	for _, tbl := range mailboxTables {
		if columnNullable(t, owner, tbl) {
			t.Errorf("%s.mailbox_id is still nullable", tbl)
		}
	}
	if got := keyColumns(t, owner, "mailbox_event_mailbox_uid_key"); got != "mailbox_id,mailbox,uidvalidity,uid" {
		t.Errorf("mailbox_event key %q", got)
	}
	if got := keyColumns(t, owner, "mailbox_cursor_pkey"); got != "mailbox_id,mailbox" {
		t.Errorf("mailbox_cursor key %q", got)
	}
	// The same folder and UID under another mailbox is a row of its own.
	ownerRun(t, owner, `INSERT INTO mailbox_event (mailbox, uidvalidity, uid, kind, mailbox_id) VALUES ('INBOX', 7, 1, 'unmatched', $1)`, realMB)
	ownerRun(t, owner, `INSERT INTO mailbox_cursor (mailbox, uidvalidity, last_uid, synced_at, mailbox_id) VALUES ('INBOX', 7, 1, now(), $1)`, realMB)
	ownerRun(t, owner, `DELETE FROM mailbox_event WHERE mailbox_id = $1`, realMB)
	ownerRun(t, owner, `DELETE FROM mailbox_cursor WHERE mailbox_id = $1`, realMB)

	// Down reverses it: the placeholder goes, its rows have no mailbox again, the old keys return.
	if _, err := p.DownTo(ctx, mailboxesRequired-1); err != nil {
		t.Fatalf("down to %d: %v", mailboxesRequired-1, err)
	}
	if n := ownerInt(t, owner, `SELECT count(*) FROM sender_mailbox WHERE address = $1`, placeholderAddress); n != 0 {
		t.Errorf("placeholder left after down")
	}
	if n := ownerInt(t, owner, `SELECT count(*) FROM mailbox_state`); n != 0 {
		t.Errorf("%d state rows left after down", n)
	}
	if n := ownerInt(t, owner, `SELECT count(*) FROM outreach_message WHERE id = $1 AND mailbox_id IS NULL`, old); n != 1 {
		t.Error("the old message has a mailbox after down")
	}
	if got := ownerInt(t, owner, `SELECT mailbox_id FROM outreach_message WHERE id = $1`, kept); got != realMB {
		t.Errorf("the real mailbox's message moved to %d after down", got)
	}
	for _, tbl := range mailboxTables {
		if !columnNullable(t, owner, tbl) {
			t.Errorf("%s.mailbox_id is NOT NULL after down", tbl)
		}
	}
	for _, tbl := range []string{"mailbox_event", "mailbox_cursor"} {
		if n := ownerInt(t, owner, `SELECT count(*) FROM `+tbl+` WHERE mailbox_id IS NULL`); n != 1 { // tbl is a test constant
			t.Errorf("%s: %d rows without a mailbox after down, want 1", tbl, n)
		}
	}
	if got := keyColumns(t, owner, "mailbox_event_mailbox_uid_key"); got != "mailbox,uidvalidity,uid" {
		t.Errorf("mailbox_event key after down %q", got)
	}
	if got := keyColumns(t, owner, "mailbox_cursor_pkey"); got != "mailbox" {
		t.Errorf("mailbox_cursor key after down %q", got)
	}
}

func TestMigrationMailboxesRequiredEmpty(t *testing.T) {
	owner := storetest.OwnerConn(t)
	p := toVersionBefore08(t, owner)
	if _, err := p.UpTo(context.Background(), mailboxesRequired); err != nil {
		t.Fatalf("up to %d: %v", mailboxesRequired, err)
	}
	if n := ownerInt(t, owner, `SELECT count(*) FROM sender_mailbox`); n != 0 {
		t.Errorf("%d mailboxes on an empty database after 00008, want none", n)
	}
	if n := ownerInt(t, owner, `SELECT count(*) FROM mailbox_state`); n != 0 {
		t.Errorf("%d state rows on an empty database after 00008, want none", n)
	}
	for _, tbl := range mailboxTables {
		if columnNullable(t, owner, tbl) {
			t.Errorf("%s.mailbox_id is still nullable", tbl)
		}
	}
}
