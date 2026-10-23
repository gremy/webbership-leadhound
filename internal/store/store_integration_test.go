//go:build integration

package store_test

import (
	"context"
	"errors"
	"os"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

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

func TestMigrateFromEmpty(t *testing.T) {
	ctx := context.Background()
	owner := storetest.OwnerConn(t)

	// A cold start: the schema goes, with every table and goose's own version table.
	for _, q := range []string{
		"DROP SCHEMA public CASCADE",
		"CREATE SCHEMA public",
		"GRANT USAGE ON SCHEMA public TO leadhound_app", // the per-database line in deploy/sql/grants.sql
	} {
		if _, err := owner.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if err := store.Migrate(ctx, storetest.MigrateDSN()); err != nil {
		t.Fatalf("migrate from empty: %v", err)
	}
	var version int64
	if err := owner.QueryRow(ctx, "SELECT max(version_id) FROM goose_db_version").Scan(&version); err != nil {
		t.Fatalf("read goose version: %v", err)
	}
	if version != 1 {
		t.Fatalf("goose version after migrate = %d, want 1", version)
	}

	// A second run finds nothing to do.
	if err := store.Migrate(ctx, storetest.MigrateDSN()); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	var applied int
	if err := owner.QueryRow(ctx, "SELECT count(*) FROM goose_db_version WHERE version_id > 0").Scan(&applied); err != nil {
		t.Fatalf("count goose rows: %v", err)
	}
	if applied != 1 {
		t.Fatalf("goose rows after second migrate = %d, want 1 (a no-op)", applied)
	}

	// The default privileges outlived the schema: the app role can write to the new tables.
	app := storetest.AppConn(t)
	if _, err := app.Exec(ctx, "INSERT INTO run (command) VALUES ('collect')"); err != nil {
		t.Fatalf("app insert after schema recreate: %v", err)
	}
	// Migrate took back what the default privileges gave the app on goose's table.
	assertGooseReadOnly(t, owner, "after migrate")

	// grants.sql run after migrate (the deploy order) re-grants every table and must still
	// leave goose's table read-only. The owner can run it: it owns the schema and its own
	// default privileges.
	grants, err := os.ReadFile("../../deploy/sql/grants.sql")
	if err != nil {
		t.Fatalf("read grants.sql: %v", err)
	}
	if _, err := owner.Exec(ctx, string(grants)); err != nil {
		t.Fatalf("run grants.sql after migrate: %v", err)
	}
	assertGooseReadOnly(t, owner, "after grants.sql")
	if err := store.Migrate(ctx, storetest.MigrateDSN()); err != nil {
		t.Fatalf("migrate after grants.sql: %v", err)
	}
	assertGooseReadOnly(t, owner, "after grants.sql then migrate")
	storetest.ResetDB(t)
}

// assertGooseReadOnly fails the test when the app role may write goose's version table.
func assertGooseReadOnly(t *testing.T, owner *pgx.Conn, when string) {
	t.Helper()
	for _, priv := range []string{"INSERT", "UPDATE", "DELETE"} {
		var ok bool
		if err := owner.QueryRow(context.Background(),
			"SELECT has_table_privilege('leadhound_app', 'public.goose_db_version', $1)", priv).Scan(&ok); err != nil {
			t.Fatalf("%s: has_table_privilege: %v", when, err)
		}
		if ok {
			t.Errorf("%s: leadhound_app has %s on goose_db_version, want none", when, priv)
		}
	}
}

func TestAppRoleCannotDeleteOrDrop(t *testing.T) {
	ctx := context.Background()
	storetest.ResetDB(t)
	app := storetest.AppConn(t)

	tests := []struct {
		name string
		sql  string
	}{
		{"delete", "DELETE FROM job"},
		{"drop", "DROP TABLE job"},
		{"truncate", "TRUNCATE job"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := app.Exec(ctx, tc.sql)
			if got := sqlState(err); got != "42501" {
				t.Fatalf("%s as app: err = %v (SQLSTATE %q), want 42501 insufficient_privilege", tc.sql, err, got)
			}
		})
	}
}

// TestAppRolePrivileges checks the app role's rights on every table the migrations made, so a
// later migration's table is covered without editing this test. goose's version table is
// read-only to the app: a forged version row would make goose skip a real migration.
func TestAppRolePrivileges(t *testing.T) {
	ctx := context.Background()
	storetest.Migrated(t)
	owner := storetest.OwnerConn(t)

	rows, err := owner.Query(ctx,
		"SELECT tablename FROM pg_tables WHERE schemaname = 'public' ORDER BY tablename")
	if err != nil {
		t.Fatalf("list tables: %v", err)
	}
	tables, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("list tables: %v", err)
	}
	if !slices.Contains(tables, "goose_db_version") || !slices.Contains(tables, "job") {
		t.Fatalf("tables = %v, want goose_db_version and job among them", tables)
	}

	has := func(table, priv string) bool {
		t.Helper()
		var ok bool
		q := "SELECT has_table_privilege('leadhound_app', format('public.%I', $1::text), $2)"
		if err := owner.QueryRow(ctx, q, table, priv).Scan(&ok); err != nil {
			t.Fatalf("has_table_privilege(%s, %s): %v", table, priv, err)
		}
		return ok
	}
	for _, table := range tables {
		t.Run(table, func(t *testing.T) {
			for _, priv := range []string{"DELETE", "TRUNCATE"} {
				if has(table, priv) {
					t.Errorf("leadhound_app has %s on %s, want none", priv, table)
				}
			}
			if table == "goose_db_version" {
				for _, priv := range []string{"INSERT", "UPDATE"} {
					if has(table, priv) {
						t.Errorf("leadhound_app has %s on goose_db_version, want none", priv)
					}
				}
				return
			}
			if !has(table, "INSERT") {
				t.Errorf("leadhound_app lacks INSERT on %s", table)
			}
		})
	}

	var create bool
	if err := owner.QueryRow(ctx, "SELECT has_schema_privilege('leadhound_app', 'public', 'CREATE')").Scan(&create); err != nil {
		t.Fatalf("has_schema_privilege: %v", err)
	}
	if create {
		t.Error("leadhound_app may CREATE in schema public, want not")
	}

	// And the real statement fails, not only the catalogue answer.
	app := storetest.AppConn(t)
	_, err = app.Exec(ctx, "INSERT INTO goose_db_version (version_id, is_applied) VALUES (999, true)")
	if got := sqlState(err); got != "42501" {
		t.Fatalf("forged goose row as app: err = %v (SQLSTATE %q), want 42501", err, got)
	}
}

func TestCheckConstraints(t *testing.T) {
	ctx := context.Background()
	storetest.ResetDB(t)
	app := storetest.AppConn(t)

	// One valid row in each parent table, so only the CHECK under test can fail.
	var runID, fetchID, companyID, jobID, obsID int64
	scan := func(dest *int64, q string, args ...any) {
		t.Helper()
		if err := app.QueryRow(ctx, q, args...).Scan(dest); err != nil {
			t.Fatalf("setup %q: %v", q, err)
		}
	}
	scan(&runID, "INSERT INTO run (command) VALUES ('collect') RETURNING id")
	scan(&fetchID, "INSERT INTO http_fetch (run_id, source, kind, url, status) VALUES ($1, 'linkedin', 'detail', 'https://example.com/jobs/1', 200) RETURNING id", runID)
	scan(&companyID, "INSERT INTO company (name, norm_name) VALUES ('Example Corp', 'example corp') RETURNING id")
	scan(&jobID, "INSERT INTO job (source, source_job_id, company_id, title, url) VALUES ('linkedin', '1', $1, 'Go Engineer', 'https://example.com/jobs/1') RETURNING id", companyID)
	scan(&obsID, "INSERT INTO job_observation (job_id, fetch_id, derived_by, title) VALUES ($1, $2, 'test@1', 'Go Engineer') RETURNING id", jobID, fetchID)

	tests := []struct {
		name string
		sql  string
		args []any
	}{
		{"gate_result.verdict", "INSERT INTO gate_result (job_id, observation_id, gate, verdict, reason, derived_by) VALUES ($1, $2, 'language', 'maybe', 'r', 'gates@1')", []any{jobID, obsID}},
		{"gate_result.gate", "INSERT INTO gate_result (job_id, observation_id, gate, verdict, reason, derived_by) VALUES ($1, $2, 'salary', 'pass', 'r', 'gates@1')", []any{jobID, obsID}},
		{"status_event.status", "INSERT INTO status_event (job_id, status, actor) VALUES ($1, 'ghosted', 'gremy')", []any{jobID}},
		{"status_event.actor", "INSERT INTO status_event (job_id, status, actor) VALUES ($1, 'new', 'someone')", []any{jobID}},
		{"decision.criterion", "INSERT INTO decision (job_id, observation_id, decider, criterion, verdict, confidence, probabilities, reason, input_sha256) VALUES ($1, $2, 'fake', 'salary', 'pass', 0.9, '{}', 'r', '\\x00')", []any{jobID, obsID}},
		{"decision.verdict", "INSERT INTO decision (job_id, observation_id, decider, criterion, verdict, confidence, probabilities, reason, input_sha256) VALUES ($1, $2, 'fake', 'title', 'maybe', 0.9, '{}', 'r', '\\x00')", []any{jobID, obsID}},
		{"run.stop_reason", "INSERT INTO run (command, stop_reason) VALUES ('collect', 'bored')", nil},
		{"http_fetch.kind", "INSERT INTO http_fetch (run_id, source, kind, url, status) VALUES ($1, 'linkedin', 'other', 'https://example.com/', 200)", []any{runID}},
		{"job.listing_status", "INSERT INTO job (source, source_job_id, company_id, title, url, listing_status) VALUES ('linkedin', '2', $1, 't', 'https://example.com/jobs/2', 'gone')", []any{companyID}},
		{"rule.kind", "INSERT INTO rule (kind, field, pattern, reason) VALUES ('block', 'title', 'x', 'r')", nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := app.Exec(ctx, tc.sql, tc.args...)
			if got := sqlState(err); got != "23514" {
				t.Fatalf("err = %v (SQLSTATE %q), want 23514 check_violation", err, got)
			}
		})
	}

	// The valid values go in, so the rejections above are the CHECKs and not the setup.
	valid := []struct {
		sql  string
		args []any
	}{
		{"INSERT INTO gate_result (job_id, observation_id, gate, verdict, reason, derived_by) VALUES ($1, $2, 'language', 'pass', 'r', 'gates@1')", []any{jobID, obsID}},
		{"INSERT INTO status_event (job_id, status, actor) VALUES ($1, 'new', 'system')", []any{jobID}},
		{"INSERT INTO decision (job_id, observation_id, decider, criterion, verdict, confidence, probabilities, reason, input_sha256) VALUES ($1, $2, 'fake', 'title', 'pass', 0.9, '{}', 'r', '\\x00')", []any{jobID, obsID}},
	}
	for _, v := range valid {
		if _, err := app.Exec(ctx, v.sql, v.args...); err != nil {
			t.Fatalf("valid row %q: %v", v.sql, err)
		}
	}
}

func TestOpen(t *testing.T) {
	ctx := context.Background()
	storetest.Migrated(t)
	s, err := store.Open(ctx, storetest.AppDSN())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	var one int
	if err := s.Pool().QueryRow(ctx, "SELECT 1").Scan(&one); err != nil {
		t.Fatalf("query: %v", err)
	}
	if one != 1 {
		t.Fatalf("SELECT 1 = %d", one)
	}
}
