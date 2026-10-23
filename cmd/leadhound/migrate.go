package main

import (
	"context"
	"log/slog"

	"github.com/gremy/webbership-leadhound/internal/store"
)

func init() { commands["migrate"] = runMigrate }

// runMigrate applies the pending migrations as the owner role (LEADHOUND_MIGRATE_DATABASE_URL).
func runMigrate(ctx context.Context, args []string) error {
	if len(args) > 0 {
		return usageError("migrate takes no arguments")
	}
	cfg, err := loadConfig("LEADHOUND_MIGRATE_DATABASE_URL")
	if err != nil {
		return err
	}
	if err := store.Migrate(ctx, cfg.MigrateDatabaseURL.Reveal()); err != nil {
		return err
	}
	slog.Info("migrate: database is at the latest migration")
	return nil
}
