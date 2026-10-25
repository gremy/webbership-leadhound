//go:build linux || darwin

// Package diskfree reports how much of a filesystem is still free, for the storage floor that
// stops collectors before the shared disk fills up.
package diskfree

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// FreePercent returns the share of the filesystem holding path that an unprivileged process
// can still write, from 0 to 100. It counts the blocks available to non-root users (f_bavail)
// against all blocks (f_blocks): the root-reserved blocks are not free for Postgres or
// leadhound, which run as ordinary users, so the floor trips before they would see the disk as
// full. ok is true on every platform this file builds for; err says why the filesystem could
// not be read.
func FreePercent(path string) (pct float64, ok bool, err error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, true, fmt.Errorf("diskfree: statfs %s: %w", path, err)
	}
	pct, err = percent(st.Bavail, st.Blocks)
	if err != nil {
		return 0, true, fmt.Errorf("diskfree: %s: %w", path, err)
	}
	return pct, true, nil
}
