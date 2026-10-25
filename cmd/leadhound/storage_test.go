package main

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/gremy/webbership-leadhound/internal/brief"
	"github.com/gremy/webbership-leadhound/internal/config"
)

// The tests in this package run whatever the free disk of the machine they run on: the
// floor is set to 1% unless a test sets its own, so a nearly full laptop never stops a
// collector under test. The storage tests below give the free share themselves.
func init() {
	if os.Getenv("LEADHOUND_DISK_MIN_FREE_PCT") == "" {
		_ = os.Setenv("LEADHOUND_DISK_MIN_FREE_PCT", "1")
	}
}

// fakeDisk makes diskFreePercent answer pct, ok and err for the test, and records the path
// it was asked about.
func fakeDisk(t *testing.T, pct float64, ok bool, err error) *string {
	t.Helper()
	prev := diskFreePercent
	t.Cleanup(func() { diskFreePercent = prev })
	var asked string
	diskFreePercent = func(path string) (float64, bool, error) { asked = path; return pct, ok, err }
	return &asked
}

func sizeOf(n int64, err error) func(context.Context) (int64, error) {
	return func(context.Context) (int64, error) { return n, err }
}

// TestStorageBudgetCheck: the check stops exactly when the brief's line says the collectors
// stopped storing (the same two predicates), with the plan's plain note, and a size or
// free-disk error is an error.
func TestStorageBudgetCheck(t *testing.T) {
	const gb = int64(1) << 30
	cfg := config.Config{DBMaxBytes: 8 * gb, DiskMinFreePct: 15, DataDir: "/data"}
	tests := []struct {
		name     string
		size     int64
		pct      float64
		known    bool
		wantOver bool
		wantWhy  string
	}{
		{"comfortable", 2 * gb, 60, true, false, ""},
		{"at the budget", 8 * gb, 60, true, false, ""},
		{"over the budget", 8*gb + gb/5, 60, true, true, "database 8.2 GB over its 8 GB budget"},
		{"on the floor", 2 * gb, 15, true, false, ""},
		{"under the floor", 2 * gb, 13.7, true, true, "disk 13% free, under the 15% floor"},
		{"both", 9 * gb, 10, true, true, "database 9 GB over its 8 GB budget and disk 10% free, under the 15% floor"},
		{"free unknown", 2 * gb, 0, false, false, ""},
		{"free unknown, over", 9 * gb, 0, false, true, "database 9 GB over its 8 GB budget"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := fakeDisk(t, tt.pct, tt.known, nil)
			b := newStorageBudget(cfg, sizeOf(tt.size, nil))
			over, why, err := b.Check(context.Background())
			if err != nil || over != tt.wantOver || why != tt.wantWhy {
				t.Errorf("Check = %v, %q, %v; want %v, %q", over, why, err, tt.wantOver, tt.wantWhy)
			}
			if *path != "/data" {
				t.Errorf("free disk read at %q, want the data directory", *path)
			}
			st := &brief.StorageState{DBBytes: tt.size, MaxBytes: cfg.DBMaxBytes, FreePct: tt.pct, FreeKnown: tt.known, MinFreePct: 15}
			if (st.OverBudget() || st.UnderFloor()) != over {
				t.Errorf("the brief's predicates disagree with the check")
			}
		})
	}
}

func TestStorageBudgetCheckErrors(t *testing.T) {
	boom := errors.New("boom")
	cfg := config.Config{DBMaxBytes: 8 << 30, DiskMinFreePct: 15}

	fakeDisk(t, 50, true, nil)
	if _, _, err := newStorageBudget(cfg, sizeOf(0, boom)).Check(context.Background()); !errors.Is(err, boom) {
		t.Errorf("size error: err = %v, want boom", err)
	}
	path := fakeDisk(t, 0, false, boom)
	if _, _, err := newStorageBudget(cfg, sizeOf(1, nil)).Check(context.Background()); !errors.Is(err, boom) {
		t.Errorf("free disk error: err = %v, want boom", err)
	}
	if *path != "." {
		t.Errorf("free disk read at %q with no data directory, want the working directory", *path)
	}
}

// TestReadStorageState: the brief's reader returns the state, and on an unreadable size or
// disk no state and no error, so the brief and the dashboard never fail on it.
func TestReadStorageState(t *testing.T) {
	cfg := config.Config{DBMaxBytes: 8 << 30, DiskMinFreePct: 15}
	fakeDisk(t, 40, true, nil)
	st, err := readStorageWith(context.Background(), cfg, sizeOf(3<<30, nil))
	want := brief.StorageState{DBBytes: 3 << 30, MaxBytes: 8 << 30, FreePct: 40, FreeKnown: true, MinFreePct: 15}
	if err != nil || st == nil || *st != want {
		t.Errorf("state %+v, %v; want %+v", st, err, want)
	}
	st, err = readStorageWith(context.Background(), cfg, sizeOf(0, errors.New("size unreadable")))
	if st != nil || err != nil {
		t.Errorf("unreadable: state %+v, err %v; want neither", st, err)
	}
	if readStorage == nil {
		t.Errorf("readStorage is not wired")
	}
}
