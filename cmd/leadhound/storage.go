package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/gremy/webbership-leadhound/internal/brief"
	"github.com/gremy/webbership-leadhound/internal/config"
	"github.com/gremy/webbership-leadhound/internal/diskfree"
	"github.com/gremy/webbership-leadhound/internal/store"
)

// StorageBudget is the limit past which the collectors stop storing: the database's size
// budget (LEADHOUND_DB_MAX_BYTES) and the free-disk floor (LEADHOUND_DISK_MIN_FREE_PCT) of
// the filesystem holding Path. Check reports whether either is passed, and why in a plain
// note; it is what the collectors and the leads commands ask.
type StorageBudget struct {
	MaxBytes   int64
	MinFreePct int
	Path       string
	Check      func(ctx context.Context) (over bool, why string, err error)

	size func(ctx context.Context) (int64, error)
}

// diskFreePercent reads a filesystem's free share; tests replace it.
var diskFreePercent = diskfree.FreePercent

// storagePath is the directory whose filesystem the free-disk floor is read on: the data
// directory, else the working directory (on the server both sit on the database's root
// filesystem).
func storagePath(cfg config.Config) string {
	if cfg.DataDir != "" {
		return cfg.DataDir
	}
	return "."
}

// newStorageBudget is the budget from cfg, reading the database's size through size.
func newStorageBudget(cfg config.Config, size func(context.Context) (int64, error)) StorageBudget {
	b := StorageBudget{MaxBytes: cfg.DBMaxBytes, MinFreePct: cfg.DiskMinFreePct, Path: storagePath(cfg), size: size}
	b.Check = func(ctx context.Context) (bool, string, error) {
		st, err := b.state(ctx)
		if err != nil {
			return false, "", err
		}
		if st.OverBudget() || st.UnderFloor() {
			return true, st.StopNote(), nil
		}
		return false, "", nil
	}
	return b
}

// state reads the database size and the free disk against the budget.
func (b StorageBudget) state(ctx context.Context) (*brief.StorageState, error) {
	n, err := b.size(ctx)
	if err != nil {
		return nil, err
	}
	pct, ok, err := diskFreePercent(b.Path)
	if err != nil {
		return nil, fmt.Errorf("storage: free disk: %w", err)
	}
	return &brief.StorageState{DBBytes: n, MaxBytes: b.MaxBytes, FreePct: pct, FreeKnown: ok, MinFreePct: b.MinFreePct}, nil
}

// storageCheck is the check a command hands its collector: cfg's budget over q's database.
// Tests replace it.
var storageCheck = func(cfg config.Config, q store.Queries) func(context.Context) (bool, string, error) {
	return newStorageBudget(cfg, q.DatabaseSize).Check
}

// The brief and the console read storage through readStorage (briefmap.go). It is wired
// once here, before any command runs, and reads the budget from the environment on each
// call, so it is never reassigned while the console builds in the background.
func init() { readStorage = readStorageFromEnv }

// readStorageFromEnv is readStorageWith over the environment's config and q's database.
func readStorageFromEnv(ctx context.Context, q store.Queries) (*brief.StorageState, error) {
	cfg, _ := config.Load(os.Getenv) // the command already reported any bad value
	return readStorageWith(ctx, cfg, q.DatabaseSize)
}

// readStorageWith is the storage state for the brief's line. A size or free disk that
// cannot be read is logged and gives no state and no error: the brief and the dashboard
// then show no storage line rather than fail.
func readStorageWith(ctx context.Context, cfg config.Config, size func(context.Context) (int64, error)) (*brief.StorageState, error) {
	st, err := newStorageBudget(cfg, size).state(ctx)
	if err != nil {
		slog.Warn("storage not read; no storage line", "err", err)
		return nil, nil
	}
	return st, nil
}
