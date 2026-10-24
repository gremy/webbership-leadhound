package leadstore

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

// versionTable is goose's version table for the leads set, separate from the core set's
// goose_db_version, so the two sets are numbered independently.
const versionTable = "goose_db_version_leads"

// ErrNoCoreSchema is returned by Migrate when the core tables the lead tables reference do
// not exist yet.
var ErrNoCoreSchema = errors.New("leadstore: migrate: core schema missing, run store.Migrate first")

// Migrate applies every pending leads migration to the database at migrateDSN, which must
// be the owner role and must already hold the core schema (store.Migrate). A database
// already at the latest leads version is left as it is.
//
// It takes the same Postgres session advisory lock as store.Migrate, so a leads migrate
// and a core migrate never run at the same time.
func Migrate(ctx context.Context, migrateDSN string) (err error) {
	connCfg, err := pgx.ParseConfig(migrateDSN)
	if err != nil {
		// The DSN may carry a password, so it never goes into the error.
		return fmt.Errorf("leadstore: migrate: parse dsn: %w", err)
	}
	db := stdlib.OpenDB(*connCfg)
	defer func() {
		if cerr := db.Close(); cerr != nil {
			err = errors.Join(err, fmt.Errorf("leadstore: migrate: close: %w", cerr))
		}
	}()
	return migrate(ctx, db)
}

func migrate(ctx context.Context, db *sql.DB) error {
	var core bool
	if err := db.QueryRowContext(ctx,
		"SELECT to_regclass('public.run') IS NOT NULL AND to_regclass('public.http_fetch') IS NOT NULL").Scan(&core); err != nil {
		return fmt.Errorf("leadstore: migrate: check core schema: %w", err)
	}
	if !core {
		return ErrNoCoreSchema
	}
	sub, err := fs.Sub(migrationFS, "migrations")
	if err != nil {
		return fmt.Errorf("leadstore: migrate: migrations dir: %w", err)
	}
	locker, err := lock.NewPostgresSessionLocker()
	if err != nil {
		return fmt.Errorf("leadstore: migrate: locker: %w", err)
	}
	p, err := goose.NewProvider(goose.DialectPostgres, db, sub,
		goose.WithTableName(versionTable), goose.WithSessionLocker(locker))
	if err != nil {
		return fmt.Errorf("leadstore: migrate: provider: %w", err)
	}
	if _, err := p.Up(ctx); err != nil {
		return fmt.Errorf("leadstore: migrate: up: %w", err)
	}
	if _, err := db.ExecContext(ctx, revokeLeadsSQL); err != nil {
		return fmt.Errorf("leadstore: migrate: revoke app writes: %w", err)
	}
	return nil
}

// revokeLeadsSQL takes from the app role what the owner's default privileges gave it and
// it must not have: every write on the leads version table, and UPDATE on the append-only
// lead tables. It runs after every migrate, a no-op one too; deploy/sql/grants.sql does the
// same, so either may run last. Each statement is skipped when the app role or its table
// does not exist.
const revokeLeadsSQL = `DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'leadhound_app') THEN
    RETURN;
  END IF;
  IF to_regclass('public.goose_db_version_leads') IS NOT NULL THEN
    REVOKE INSERT, UPDATE, DELETE, TRUNCATE ON public.goose_db_version_leads FROM leadhound_app;
  END IF;
  IF to_regclass('public.site_probe') IS NOT NULL THEN
    REVOKE UPDATE ON public.site_probe FROM leadhound_app;
  END IF;
  IF to_regclass('public.fetch_header') IS NOT NULL THEN
    REVOKE UPDATE ON public.fetch_header FROM leadhound_app;
  END IF;
END
$$`
