// Package leadstoretest gives integration tests a test database with both the core and the
// leads migrations applied, emptied between tests.
//
// It builds on storetest: the same two roles, DSNs and skip-or-fail rule.
package leadstoretest

import (
	"context"
	"sync"
	"testing"

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
// with storetest.ResetDB, so a test starts from an empty database with ids from 1. Every
// goose version table is left alone.
func Reset(t testing.TB) {
	t.Helper()
	storetest.ResetDB(t)
	Migrated(t)
	storetest.ResetDB(t)
}

// Q returns the lead query helpers over an app-role connection, closed when the test ends.
func Q(t testing.TB) leadstore.Queries {
	t.Helper()
	return leadstore.NewQueries(storetest.AppConn(t))
}
