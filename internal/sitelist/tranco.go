package sitelist

import (
	"bufio"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// maxLine bounds one list line; a longer line is a read error.
const maxLine = 1 << 20

// ParseTranco reads a headerless Tranco list of "rank,domain" lines. Each domain is
// normalised by Registrable and kept when keep (nil keeps all) says so; duplicates keep
// the best rank. A malformed line is counted in Tally.Bad and skipped; only a read error
// is returned as an error. The result is sorted by rank.
func ParseTranco(r io.Reader, keep func(string) bool) ([]Entry, Tally, error) {
	c := newCollector(keep)
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), maxLine)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		rankText, domain, ok := strings.Cut(line, ",")
		rank, err := strconv.Atoi(strings.TrimSpace(rankText))
		if !ok || err != nil || rank < 1 {
			c.tally.Bad++
			continue
		}
		c.add(domain, rank)
	}
	if err := sc.Err(); err != nil {
		return nil, c.tally, fmt.Errorf("sitelist: read tranco: %w", err)
	}
	return c.entries(), c.tally, nil
}
