package sitelist

import (
	"fmt"
	"io"
	"net/url"
	"strings"
)

// ParseHTTPArchive reads an HTTP Archive CSV export whose header names a page column
// holding page URLs, and returns each URL's registrable domain once, with Rank 0,
// sorted by domain. A header without page is an error; a row whose URL has no host or
// no registrable domain is counted in Tally.Bad.
func ParseHTTPArchive(r io.Reader) ([]Entry, Tally, error) {
	cr, cols, err := openCSV(r, "page")
	if err != nil {
		return nil, Tally{}, fmt.Errorf("sitelist: http archive: %w", err)
	}
	pageCol := cols[0]
	c := newCollector(nil)
	err = eachRow(cr, &c.tally, func(rec []string) {
		if pageCol >= len(rec) {
			c.tally.Bad++
			return
		}
		u, err := url.Parse(strings.TrimSpace(rec[pageCol]))
		if err != nil || u.Hostname() == "" {
			c.tally.Bad++
			return
		}
		c.add(u.Hostname(), 0)
	})
	if err != nil {
		return nil, c.tally, fmt.Errorf("sitelist: http archive: %w", err)
	}
	return c.entries(), c.tally, nil
}
