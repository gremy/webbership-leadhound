package diskfree

import (
	"errors"
	"path/filepath"
	"runtime"
	"testing"
)

func TestPercent(t *testing.T) {
	tests := []struct {
		avail, total uint64
		want         float64
	}{
		{0, 100, 0},
		{15, 100, 15},
		{100, 100, 100},
		{150, 100, 100},
		{1, 8, 12.5},
		{1 << 40, 1 << 42, 25},
	}
	for _, tt := range tests {
		got, err := percent(tt.avail, tt.total)
		if err != nil || got != tt.want {
			t.Errorf("percent(%d, %d) = %v, %v; want %v", tt.avail, tt.total, got, err, tt.want)
		}
	}
	if _, err := percent(0, 0); !errors.Is(err, errNoBlocks) {
		t.Errorf("percent(0, 0) err = %v, want errNoBlocks", err)
	}
}

func TestFreePercent(t *testing.T) {
	pct, ok, err := FreePercent(t.TempDir())
	switch runtime.GOOS {
	case "linux", "darwin":
		if !ok || err != nil || pct < 0 || pct > 100 {
			t.Fatalf("FreePercent(temp dir) = %v, %v, %v; want 0-100, true, nil", pct, ok, err)
		}
	default:
		if ok || err != nil || pct != 0 {
			t.Fatalf("FreePercent(temp dir) = %v, %v, %v; want 0, false, nil", pct, ok, err)
		}
	}
}

func TestFreePercentMissingPath(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("no free-disk reading on " + runtime.GOOS)
	}
	_, ok, err := FreePercent(filepath.Join(t.TempDir(), "absent"))
	if !ok || err == nil {
		t.Fatalf("FreePercent(missing) ok = %v, err = %v; want true and an error", ok, err)
	}
}
