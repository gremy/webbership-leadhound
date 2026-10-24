//go:build integration

package leadstore_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/leadstore/leadstoretest"
	"github.com/gremy/webbership-leadhound/internal/store"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

// sqlState returns the SQLSTATE of a Postgres error, or "" for any other error.
func sqlState(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

// leadMigrationFiles returns the number of the newest file in migrations/ (the files the
// binary embeds) and how many there are.
func leadMigrationFiles(t *testing.T) (latest int64, count int64) {
	t.Helper()
	names, err := filepath.Glob(filepath.Join("migrations", "*.sql"))
	if err != nil || len(names) == 0 {
		t.Fatalf("migrations: %v (%d files)", err, len(names))
	}
	for _, n := range names {
		num, _, _ := strings.Cut(filepath.Base(n), "_")
		v, err := strconv.ParseInt(num, 10, 64)
		if err != nil {
			t.Fatalf("migration %s: no leading number: %v", n, err)
		}
		latest = max(latest, v)
	}
	return latest, int64(len(names))
}

// dropSchema empties the database as the owner: the schema goes with every table and both
// goose version tables.
func dropSchema(t *testing.T, owner *pgx.Conn) {
	t.Helper()
	for _, q := range []string{
		"DROP SCHEMA public CASCADE",
		"CREATE SCHEMA public",
		"GRANT USAGE ON SCHEMA public TO leadhound_app", // the per-database line in deploy/sql/grants.sql
	} {
		if _, err := owner.Exec(context.Background(), q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
}

// migrateAll runs the core migrations, then the leads migrations.
func migrateAll(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	if err := store.Migrate(ctx, storetest.MigrateDSN()); err != nil {
		t.Fatalf("core migrate: %v", err)
	}
	if err := leadstore.Migrate(ctx, storetest.MigrateDSN()); err != nil {
		t.Fatalf("leads migrate: %v", err)
	}
}

// appliedCount returns how many applied migrations a goose version table records.
func appliedCount(t *testing.T, owner *pgx.Conn, table string) int64 {
	t.Helper()
	var n int64
	q := "SELECT count(*) FROM " + pgx.Identifier{table}.Sanitize() + " WHERE version_id > 0"
	if err := owner.QueryRow(context.Background(), q).Scan(&n); err != nil {
		t.Fatalf("count %s rows: %v", table, err)
	}
	return n
}

// appHas reports whether the app role holds priv on table.
func appHas(t *testing.T, owner *pgx.Conn, table, priv string) bool {
	t.Helper()
	var ok bool
	q := "SELECT has_table_privilege('leadhound_app', format('public.%I', $1::text), $2)"
	if err := owner.QueryRow(context.Background(), q, table, priv).Scan(&ok); err != nil {
		t.Fatalf("has_table_privilege(%s, %s): %v", table, priv, err)
	}
	return ok
}

// appHasColumn reports whether the app role holds priv on column of table.
func appHasColumn(t *testing.T, owner *pgx.Conn, table, column, priv string) bool {
	t.Helper()
	var ok bool
	q := "SELECT has_column_privilege('leadhound_app', format('public.%I', $1::text), $2, $3)"
	if err := owner.QueryRow(context.Background(), q, table, column, priv).Scan(&ok); err != nil {
		t.Fatalf("has_column_privilege(%s.%s, %s): %v", table, column, priv, err)
	}
	return ok
}

// assertLeadPrivileges fails the test when the app role may write the leads version table,
// UPDATE an append-only lead table, or UPDATE an org column other than site_id.
func assertLeadPrivileges(t *testing.T, owner *pgx.Conn, when string) {
	t.Helper()
	for _, priv := range []string{"INSERT", "UPDATE", "DELETE", "TRUNCATE"} {
		if appHas(t, owner, "goose_db_version_leads", priv) {
			t.Errorf("%s: leadhound_app has %s on goose_db_version_leads, want none", when, priv)
		}
	}
	for _, table := range []string{"site_probe", "fetch_header", "contact_route", "agency_finding", "contact_attempt"} {
		if appHas(t, owner, table, "UPDATE") {
			t.Errorf("%s: leadhound_app has UPDATE on %s, want none", when, table)
		}
		if !appHas(t, owner, table, "INSERT") {
			t.Errorf("%s: leadhound_app lacks INSERT on %s", when, table)
		}
	}
	if !appHas(t, owner, "site", "UPDATE") {
		t.Errorf("%s: leadhound_app lacks UPDATE on site (its state pointers)", when)
	}
	if appHas(t, owner, "org", "UPDATE") {
		t.Errorf("%s: leadhound_app has table-wide UPDATE on org, want site_id only", when)
	}
	if !appHasColumn(t, owner, "org", "site_id", "UPDATE") {
		t.Errorf("%s: leadhound_app lacks UPDATE on org.site_id", when)
	}
	for _, col := range []string{"kind", "domain", "name", "country", "source", "source_fetch_id"} {
		if appHasColumn(t, owner, "org", col, "UPDATE") {
			t.Errorf("%s: leadhound_app has UPDATE on org.%s, want none", when, col)
		}
	}
	if !appHas(t, owner, "org", "INSERT") {
		t.Errorf("%s: leadhound_app lacks INSERT on org", when)
	}
}

func TestLeadsMigrateFromEmpty(t *testing.T) {
	ctx := context.Background()
	owner := storetest.OwnerConn(t)
	dropSchema(t, owner)
	grants, err := os.ReadFile("../../deploy/sql/grants.sql")
	if err != nil {
		t.Fatalf("read grants.sql: %v", err)
	}
	// grants.sql runs clean on an empty database: every revoke is guarded by its table.
	if _, err := owner.Exec(ctx, string(grants)); err != nil {
		t.Fatalf("run grants.sql on an empty database: %v", err)
	}
	migrateAll(t)

	var version int64
	if err := owner.QueryRow(ctx, "SELECT max(version_id) FROM goose_db_version_leads").Scan(&version); err != nil {
		t.Fatalf("read leads goose version: %v", err)
	}
	latest, count := leadMigrationFiles(t)
	if version != latest {
		t.Fatalf("leads goose version after migrate = %d, want %d", version, latest)
	}
	if got := appliedCount(t, owner, "goose_db_version_leads"); got != count {
		t.Fatalf("leads goose rows = %d, want %d", got, count)
	}

	// The app role can write the new tables through the owner's default privileges.
	app := storetest.AppConn(t)
	var runID, fetchID, siteID int64
	if err := app.QueryRow(ctx, "INSERT INTO run (command) VALUES ('leads-sites') RETURNING id").Scan(&runID); err != nil {
		t.Fatalf("insert run: %v", err)
	}
	if err := app.QueryRow(ctx,
		"INSERT INTO http_fetch (run_id, source, kind, url, status) VALUES ($1, 'site', 'site', 'https://northwind.example/', 200) RETURNING id", runID).Scan(&fetchID); err != nil {
		t.Fatalf("insert http_fetch: %v", err)
	}
	if err := app.QueryRow(ctx,
		"INSERT INTO site (domain, origin) VALUES ('northwind.example', 'manual') RETURNING id").Scan(&siteID); err != nil {
		t.Fatalf("insert site: %v", err)
	}
	var probeID int64
	if err := app.QueryRow(ctx,
		"INSERT INTO site_probe (site_id, run_id, outcome, fetch_ids, derived_by) VALUES ($1, $2, 'ok', ARRAY[$3::bigint], 'drupal@1') RETURNING id",
		siteID, runID, fetchID).Scan(&probeID); err != nil {
		t.Fatalf("insert site_probe: %v", err)
	}
	if _, err := app.Exec(ctx, "INSERT INTO fetch_header (fetch_id, headers) VALUES ($1, '{}')", fetchID); err != nil {
		t.Fatalf("insert fetch_header: %v", err)
	}
	if _, err := app.Exec(ctx, "UPDATE site SET state = 'probed', last_probe_id = $1 WHERE id = $2", probeID, siteID); err != nil {
		t.Fatalf("update site state pointers: %v", err)
	}
	assertLeadPrivileges(t, owner, "after migrate")

	// grants.sql after migrate (the deploy order) re-grants every table and must still leave
	// the leads revokes in place; a migrate after it keeps them too.
	if _, err := owner.Exec(ctx, string(grants)); err != nil {
		t.Fatalf("run grants.sql after migrate: %v", err)
	}
	assertLeadPrivileges(t, owner, "after grants.sql")
	migrateAll(t)
	assertLeadPrivileges(t, owner, "after grants.sql then migrate")
	leadstoretest.Reset(t)
}

func TestLeadsMigrateIdempotent(t *testing.T) {
	leadstoretest.Migrated(t)
	owner := storetest.OwnerConn(t)
	before := appliedCount(t, owner, "goose_db_version_leads")
	coreBefore := appliedCount(t, owner, "goose_db_version")
	for range 2 {
		if err := leadstore.Migrate(context.Background(), storetest.MigrateDSN()); err != nil {
			t.Fatalf("leads migrate again: %v", err)
		}
	}
	if got := appliedCount(t, owner, "goose_db_version_leads"); got != before {
		t.Fatalf("leads goose rows after re-migrate = %d, want %d (a no-op)", got, before)
	}
	if got := appliedCount(t, owner, "goose_db_version"); got != coreBefore {
		t.Fatalf("core goose rows after leads migrate = %d, want %d", got, coreBefore)
	}
}

func TestLeadsMigrateNeedsCoreSchema(t *testing.T) {
	ctx := context.Background()
	owner := storetest.OwnerConn(t)
	dropSchema(t, owner)
	t.Cleanup(func() { migrateAll(t) })

	err := leadstore.Migrate(ctx, storetest.MigrateDSN())
	if !errors.Is(err, leadstore.ErrNoCoreSchema) {
		t.Fatalf("leads migrate on an empty database: err = %v, want ErrNoCoreSchema", err)
	}
	// Nothing was created: not the version table, not a lead table.
	var made bool
	if err := owner.QueryRow(ctx,
		"SELECT to_regclass('public.goose_db_version_leads') IS NOT NULL OR to_regclass('public.site') IS NOT NULL").Scan(&made); err != nil {
		t.Fatalf("check tables: %v", err)
	}
	if made {
		t.Fatal("leads migrate without the core schema created tables")
	}
}

func TestLeadAppRoleCannotDelete(t *testing.T) {
	leadstoretest.Reset(t)
	app := storetest.AppConn(t)
	for _, q := range []string{
		"DELETE FROM site",
		"DELETE FROM site_probe",
		"DELETE FROM fetch_header",
		"DELETE FROM org",
		"DELETE FROM contact_route",
		"DELETE FROM agency_finding",
		"DELETE FROM contact_attempt",
		"TRUNCATE site",
		"DROP TABLE site",
	} {
		t.Run(q, func(t *testing.T) {
			_, err := app.Exec(context.Background(), q)
			if got := sqlState(err); got != "42501" {
				t.Fatalf("%s as app: err = %v (SQLSTATE %q), want 42501 insufficient_privilege", q, err, got)
			}
		})
	}
}

func TestLeadAppRoleCannotUpdateAppendOnly(t *testing.T) {
	leadstoretest.Reset(t)
	app := storetest.AppConn(t)
	for _, q := range []string{
		"UPDATE site_probe SET outcome = 'ok'",
		"UPDATE fetch_header SET headers = '{}'",
		"UPDATE contact_route SET mx_ok = true",
		"UPDATE agency_finding SET phrase = ''",
		"UPDATE contact_attempt SET routes_found = 0",
		"UPDATE org SET name = ''",
	} {
		t.Run(q, func(t *testing.T) {
			_, err := app.Exec(context.Background(), q)
			if got := sqlState(err); got != "42501" {
				t.Fatalf("%s as app: err = %v (SQLSTATE %q), want 42501 insufficient_privilege", q, err, got)
			}
		})
	}
}

func TestLeadAppRoleCannotWriteVersionTable(t *testing.T) {
	leadstoretest.Migrated(t)
	owner := storetest.OwnerConn(t)
	assertLeadPrivileges(t, owner, "after migrate")
	app := storetest.AppConn(t)
	for _, q := range []string{
		"INSERT INTO goose_db_version_leads (version_id, is_applied) VALUES (999, true)",
		"UPDATE goose_db_version_leads SET is_applied = false",
		"DELETE FROM goose_db_version_leads",
	} {
		t.Run(q, func(t *testing.T) {
			_, err := app.Exec(context.Background(), q)
			if got := sqlState(err); got != "42501" {
				t.Fatalf("%s as app: err = %v (SQLSTATE %q), want 42501 insufficient_privilege", q, err, got)
			}
		})
	}
}

func TestResetKeepsVersionTables(t *testing.T) {
	leadstoretest.Reset(t)
	owner := storetest.OwnerConn(t)
	_, leadCount := leadMigrationFiles(t)
	core := appliedCount(t, owner, "goose_db_version")
	if core == 0 {
		t.Fatal("goose_db_version is empty after Reset")
	}
	if got := appliedCount(t, owner, "goose_db_version_leads"); got != leadCount {
		t.Fatalf("goose_db_version_leads rows after Reset = %d, want %d", got, leadCount)
	}

	storetest.ResetDB(t)
	if err := leadstore.Migrate(context.Background(), storetest.MigrateDSN()); err != nil {
		t.Fatalf("leads migrate after ResetDB: %v", err)
	}
	if got := appliedCount(t, owner, "goose_db_version_leads"); got != leadCount {
		t.Fatalf("goose_db_version_leads rows after ResetDB and migrate = %d, want %d (a no-op)", got, leadCount)
	}
	if got := appliedCount(t, owner, "goose_db_version"); got != core {
		t.Fatalf("goose_db_version rows after ResetDB = %d, want %d", got, core)
	}
	// The lead tables were emptied with the rest.
	var sites int64
	if err := owner.QueryRow(context.Background(), "SELECT count(*) FROM site").Scan(&sites); err != nil {
		t.Fatalf("count sites: %v", err)
	}
	if sites != 0 {
		t.Fatalf("site rows after Reset = %d, want 0", sites)
	}
}
