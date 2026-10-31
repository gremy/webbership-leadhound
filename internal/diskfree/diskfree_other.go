//go:build !(linux || darwin)

// Package diskfree reports how much of a filesystem is still free, for the storage floor that
// stops collectors before the shared disk fills up.
package diskfree

// FreePercent reports nothing on this platform: ok is false, so the caller skips the free-disk
// floor and keeps only the database size budget.
func FreePercent(string) (pct float64, ok bool, err error) { return 0, false, nil }

// Space reports nothing on this platform: ok is false.
func Space(string) (free, total uint64, ok bool, err error) { return 0, 0, false, nil }
