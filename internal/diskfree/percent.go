package diskfree

import "errors"

// errNoBlocks is returned for a filesystem that reports a size of zero blocks, where a
// percentage means nothing.
var errNoBlocks = errors.New("filesystem reports no blocks")

// percent returns avail as a percentage of total. avail above total (a filesystem racing a
// resize) is capped at 100.
func percent(avail, total uint64) (float64, error) {
	if total == 0 {
		return 0, errNoBlocks
	}
	if avail > total {
		return 100, nil
	}
	return float64(avail) / float64(total) * 100, nil
}
