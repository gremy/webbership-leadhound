// Package leadstoretest gives integration tests a test database with both the core and the
// leads migrations applied, emptied between tests.
//
// It builds on storetest: the same two roles, DSNs and skip-or-fail rule.
package leadstoretest

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

var (
	migrateOnce sync.Once
	migrateErr  error
)

// Migrated brings the test database up to the latest core and leads migrations, the leads
// set once per test process. When the database is unavailable it skips, or fails under
// TEST_DB_REQUIRED=1.
func Migrated(t testing.TB) {
	t.Helper()
	storetest.Migrated(t)
	migrateOnce.Do(func() {
		migrateErr = leadstore.Migrate(context.Background(), storetest.MigrateDSN())
	})
	if migrateErr != nil {
		storetest.SkipOrFail(t, "skipping integration test: leads migrate: %v", migrateErr)
	}
}

// Reset migrates the test database if this process has not yet, then empties every table
// with storetest.ResetDB, so a test starts from an empty database with ids from 1, and puts
// back the default account and campaign leads migration 00016 seeds (SeedDefaults). Every
// goose version table is left alone.
func Reset(t testing.TB) {
	t.Helper()
	storetest.ResetDB(t)
	Migrated(t)
	storetest.ResetDB(t)
	conn := storetest.OwnerConn(t)
	if err := SeedDefaults(context.Background(), conn); err != nil {
		t.Fatalf("leadstoretest: %v", err)
	}
}

// seedDefaultsSQL puts back the rows leads migration 00016 seeds, which a TRUNCATE of every
// table empties: account 1 (operator) and its campaign 1 (drupal-eol), the next ids from 2.
const seedDefaultsSQL = `INSERT INTO account (id, slug, name) VALUES (1, 'operator', 'Operator') ON CONFLICT DO NOTHING;
INSERT INTO campaign (id, account_id, slug, name) VALUES (1, 1, 'drupal-eol', 'Drupal end-of-life') ON CONFLICT DO NOTHING;
SELECT setval(pg_get_serial_sequence('account', 'id'), (SELECT max(id) FROM account));
SELECT setval(pg_get_serial_sequence('campaign', 'id'), (SELECT max(id) FROM campaign));`

// SeedDefaults writes the default account and campaign (migration 00016's seed rows) over
// conn, the owner, when they are missing; a database that has them is left as it is.
func SeedDefaults(ctx context.Context, conn interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}) error {
	if _, err := conn.Exec(ctx, seedDefaultsSQL); err != nil {
		return fmt.Errorf("seed default account and campaign: %w", err)
	}
	return nil
}

// Q returns the lead query helpers over an app-role connection, closed when the test ends.
func Q(t testing.TB) leadstore.Queries {
	t.Helper()
	return leadstore.NewQueries(storetest.AppConn(t))
}
