package sitelist

import (
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// ErrMissingColumn is returned when a CSV header lacks a column a parser reads.
var ErrMissingColumn = errors.New("sitelist: missing column")

// ParseMajestic reads the Majestic Million CSV. Its header row names the columns, and
// GlobalRank and Domain are found by name, so their position does not matter; a header
// without either is an error. Rows are normalised, filtered and deduped as in
// ParseTranco; a malformed row is counted in Tally.Bad.
func ParseMajestic(r io.Reader, keep func(string) bool) ([]Entry, Tally, error) {
	cr, cols, err := openCSV(r, "GlobalRank", "Domain")
	if err != nil {
		return nil, Tally{}, fmt.Errorf("sitelist: majestic: %w", err)
	}
	rankCol, domainCol := cols[0], cols[1]
	c := newCollector(keep)
	err = eachRow(cr, &c.tally, func(rec []string) {
		if rankCol >= len(rec) || domainCol >= len(rec) {
			c.tally.Bad++
			return
		}
		rank, err := strconv.Atoi(strings.TrimSpace(rec[rankCol]))
		if err != nil || rank < 1 {
			c.tally.Bad++
			return
		}
		c.add(rec[domainCol], rank)
	})
	if err != nil {
		return nil, c.tally, fmt.Errorf("sitelist: majestic: %w", err)
	}
	return c.entries(), c.tally, nil
}

// openCSV reads the header row and returns the index of each named column (matched
// case-insensitively, a byte-order mark ignored).
func openCSV(r io.Reader, names ...string) (*csv.Reader, []int, error) {
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = -1
	cr.ReuseRecord = true
	header, err := cr.Read()
	if errors.Is(err, io.EOF) {
		return nil, nil, fmt.Errorf("%w: no header row", ErrMissingColumn)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("read header: %w", err)
	}
	cols := make([]int, len(names))
	for i, name := range names {
		cols[i] = -1
		for j, h := range header {
			if strings.EqualFold(strings.TrimSpace(strings.TrimPrefix(h, "\uFEFF")), name) {
				cols[i] = j
				break
			}
		}
		if cols[i] < 0 {
			return nil, nil, fmt.Errorf("%w: %s", ErrMissingColumn, name)
		}
	}
	return cr, cols, nil
}

// eachRow calls fn for every record after the header. A record the CSV reader cannot
// parse is counted bad and skipped; any other read error stops the walk.
func eachRow(cr *csv.Reader, tally *Tally, fn func([]string)) error {
	for {
		rec, err := cr.Read()
		if errors.Is(err, io.EOF) {
			return nil
		}
		var perr *csv.ParseError
		if errors.As(err, &perr) {
			tally.Bad++
			continue
		}
		if err != nil {
			return fmt.Errorf("read row: %w", err)
		}
		fn(rec)
	}
}
