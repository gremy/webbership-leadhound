package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver goose needs
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// Migrate applies every pending migration in migrations/ to the database at migrateDSN, which
// must be the owner role. A database already at the latest version is left as it is.
//
// A Postgres session advisory lock serialises concurrent runs, so two processes (two test
// packages, or a deploy racing a manual run) never apply the same migration twice.
func Migrate(ctx context.Context, migrateDSN string) (err error) {
	connCfg, err := pgx.ParseConfig(migrateDSN)
	if err != nil {
		// The DSN may carry a password, so it never goes into the error.
		return fmt.Errorf("store: migrate: parse dsn: %w", err)
	}
	db := stdlib.OpenDB(*connCfg)
	defer func() {
		if cerr := db.Close(); cerr != nil {
			err = errors.Join(err, fmt.Errorf("store: migrate: close: %w", cerr))
		}
	}()
	return migrate(ctx, db)
}

func migrate(ctx context.Context, db *sql.DB) error {
	sub, err := fs.Sub(migrationFS, "migrations")
	if err != nil {
		return fmt.Errorf("store: migrate: migrations dir: %w", err)
	}
	locker, err := lock.NewPostgresSessionLocker()
	if err != nil {
		return fmt.Errorf("store: migrate: locker: %w", err)
	}
	p, err := goose.NewProvider(goose.DialectPostgres, db, sub, goose.WithSessionLocker(locker))
	if err != nil {
		return fmt.Errorf("store: migrate: provider: %w", err)
	}
	if _, err := p.Up(ctx); err != nil {
		return fmt.Errorf("store: migrate: up: %w", err)
	}
	if _, err := db.ExecContext(ctx, revokeGooseSQL); err != nil {
		return fmt.Errorf("store: migrate: revoke app writes on goose table: %w", err)
	}
	return nil
}

// revokeGooseSQL takes write access to goose's version table away from the app role. The
// owner's default privileges give the app INSERT and UPDATE on every table the owner creates,
// goose's own included, and a version row the app could forge would make goose skip a real
// migration. It runs after every migrate, a no-op one too, so it holds on every database;
// deploy/sql/grants.sql does the same, so either may run last. It does nothing when the app
// role or the table does not exist.
const revokeGooseSQL = `DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'leadhound_app')
     AND to_regclass('public.goose_db_version') IS NOT NULL THEN
    REVOKE INSERT, UPDATE, DELETE, TRUNCATE ON public.goose_db_version FROM leadhound_app;
  END IF;
END
$$`
