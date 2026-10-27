// Package storetest gives integration tests a migrated, emptied test database.
//
// It is an ordinary package (not a _test file) so every package's integration tests can import
// it. Two roles reach the test database: the app role (TEST_DB_DSN) for behaviour, and the
// owner role (TEST_MIGRATE_DSN) for migrations and for clearing data between tests.
package storetest

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/gremy/webbership-leadhound/internal/store"
)

const (
	defaultAppDSN     = "postgres://leadhound_app@localhost/leadhound_test?sslmode=disable"
	defaultMigrateDSN = "postgres://leadhound_owner@localhost/leadhound_test?sslmode=disable"
)

// AppDSN is the app role's DSN for the test database: TEST_DB_DSN, or the local default.
func AppDSN() string { return envOr("TEST_DB_DSN", defaultAppDSN) }

// MigrateDSN is the owner role's DSN for the test database: TEST_MIGRATE_DSN, or the local
// default.
func MigrateDSN() string { return envOr("TEST_MIGRATE_DSN", defaultMigrateDSN) }

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// SkipOrFail skips when the database is unavailable, so a plain run without Postgres still
// passes; under TEST_DB_REQUIRED=1 (make test-integration) it fails instead, because a skipped
// database test is no evidence at all.
func SkipOrFail(t testing.TB, format string, args ...any) {
	t.Helper()
	if os.Getenv("TEST_DB_REQUIRED") == "1" {
		t.Fatalf(format, args...)
	}
	t.Skipf(format, args...)
}

var (
	migrateOnce sync.Once
	migrateErr  error
)

// Migrated brings the test database up to the latest migration, once per test process. When
// the database is unavailable it skips, or fails under TEST_DB_REQUIRED=1.
func Migrated(t testing.TB) {
	t.Helper()
	migrateOnce.Do(func() {
		migrateErr = store.Migrate(context.Background(), MigrateDSN())
	})
	if migrateErr != nil {
		SkipOrFail(t, "skipping integration test: test database unavailable: %v", migrateErr)
	}
}

// KeptTables are the reference tables a reset never empties: their rows are seeded by a
// migration and change only by migration (leads 00010), and nothing references a test's
// rows from them. Every goose version table is kept too.
var KeptTables = []string{"lead_kind", "lead_channel"}

// resetTablesSQL lists the tables a reset empties: every table but the goose version tables
// and KeptTables ($1), each through quote_ident.
const resetTablesSQL = `SELECT quote_ident(tablename) FROM pg_tables
	 WHERE schemaname = 'public' AND tablename NOT LIKE 'goose\_db\_version%' AND tablename <> ALL($1::text[])
	 ORDER BY tablename`

// ResetDB migrates the test database if this process has not yet, then empties every table
// (TRUNCATE … RESTART IDENTITY CASCADE as the owner), so a test starts from an empty database
// with ids from 1. Every goose version table (goose_db_version, goose_db_version_leads) and
// every KeptTables reference table is left alone; CASCADE never reaches them, as they
// reference no emptied table. The leads seed rows (the default account and campaign) are
// put back after the truncate.
func ResetDB(t testing.TB) {
	t.Helper()
	Migrated(t)
	ctx := context.Background()
	conn := OwnerConn(t)
	rows, err := conn.Query(ctx, resetTablesSQL, KeptTables)
	if err != nil {
		t.Fatalf("storetest: list tables: %v", err)
	}
	tables, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("storetest: list tables: %v", err)
	}
	if len(tables) == 0 {
		return
	}
	// The names come from pg_tables through quote_ident, never from input.
	q := fmt.Sprintf("TRUNCATE %s RESTART IDENTITY CASCADE", strings.Join(tables, ", "))
	if _, err := conn.Exec(ctx, q); err != nil {
		t.Fatalf("storetest: truncate: %v", err)
	}
	// The leads tables, once migrated, hold seed rows a truncate empties: the default account
	// and campaign (leads 00016). They are put back rather than kept, so a test's own
	// accounts and campaigns never reach the next test.
	if _, err := conn.Exec(ctx, reseedSQL); err != nil {
		t.Fatalf("storetest: reseed: %v", err)
	}
}

// reseedSQL puts back leads migration 00016's seed rows when its tables exist: account 1
// (operator) and its campaign 1 (drupal-eol), the next ids from 2. It mirrors
// leadstoretest.SeedDefaults, which a plain ResetDB caller does not import.
const reseedSQL = `DO $$
BEGIN
  IF to_regclass('public.account') IS NOT NULL AND to_regclass('public.campaign') IS NOT NULL THEN
    INSERT INTO account (id, slug, name) VALUES (1, 'operator', 'Operator') ON CONFLICT DO NOTHING;
    INSERT INTO campaign (id, account_id, slug, name) VALUES (1, 1, 'drupal-eol', 'Drupal end-of-life') ON CONFLICT DO NOTHING;
    PERFORM setval(pg_get_serial_sequence('account', 'id'), (SELECT max(id) FROM account));
    PERFORM setval(pg_get_serial_sequence('campaign', 'id'), (SELECT max(id) FROM campaign));
  END IF;
END
$$`

// AppConn opens a connection to the test database as the app role, closed when the test ends.
func AppConn(t testing.TB) *pgx.Conn {
	t.Helper()
	return connect(t, AppDSN())
}

// OwnerConn opens a connection to the test database as the owner role, closed when the test
// ends.
func OwnerConn(t testing.TB) *pgx.Conn {
	t.Helper()
	return connect(t, MigrateDSN())
}

func connect(t testing.TB, dsn string) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		SkipOrFail(t, "skipping integration test: test database unavailable: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return conn
}
