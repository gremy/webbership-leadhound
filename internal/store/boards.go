package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// The origins a board row can have, from the strongest claim to the weakest.
const (
	OriginManual      = "manual"      // named by the operator; always collected, never retired
	OriginFound       = "found"       // found on an employer's own site
	OriginSeedList    = "seed-list"   // a public list of boards
	OriginCommonCrawl = "commoncrawl" // the Common Crawl harvest
)

// The states a board row can be in.
const (
	BoardUnverified = "unverified"
	BoardOK         = "ok"
	BoardNotFound   = "not_found"
	BoardError      = "error"
	BoardRetired    = "retired"
)

// RetireAfterMisses is how many misses, each on its own local day, retire a board that is
// not manual.
const RetireAfterMisses = 3

// Origins are the valid board origins; BoardStates the valid board states.
var (
	Origins     = []string{OriginManual, OriginFound, OriginSeedList, OriginCommonCrawl}
	BoardStates = []string{BoardUnverified, BoardOK, BoardNotFound, BoardError, BoardRetired}
)

// originRankSQL ranks an origin for the precedence manual > found > seed-list = commoncrawl.
// A re-import only ever moves a board's origin up this order.
const originRankSQL = `CASE %s WHEN 'manual' THEN 3 WHEN 'found' THEN 2 ELSE 1 END`

// BoardKey identifies one board: its ATS, its lower-case slug and its region (” or 'eu').
type BoardKey struct {
	ATS    string
	Slug   string
	Region string
}

// normalised is the key as the table stores it.
func (k BoardKey) normalised() BoardKey {
	return BoardKey{ATS: k.ATS, Slug: strings.ToLower(strings.TrimSpace(k.Slug)), Region: k.Region}
}

// BoardImport is one board to add to the registry.
type BoardImport struct {
	BoardKey
	Name   string
	Origin string
}

// Board is one registry row.
type Board struct {
	ID           int64      `db:"id"`
	ATS          string     `db:"ats"`
	Slug         string     `db:"slug"`
	Region       string     `db:"region"`
	Name         string     `db:"name"`
	Origin       string     `db:"origin"`
	State        string     `db:"state"`
	AddedAt      time.Time  `db:"added_at"`
	CheckedAt    *time.Time `db:"checked_at"`
	LastOKAt     *time.Time `db:"last_ok_at"`
	Misses       int        `db:"misses"`
	LastMissDay  *time.Time `db:"last_miss_day"`
	PostingsLast *int       `db:"postings_last"`
}

// Key is the row's BoardKey.
func (b Board) Key() BoardKey { return BoardKey{ATS: b.ATS, Slug: b.Slug, Region: b.Region} }

// importChunk is how many rows one INSERT carries.
const importChunk = 1000

// ImportTally is what one ImportBoardsTally did: rows added, rows already known (repeats in
// the input included), and, among the known, retired boards brought back by a manual import.
type ImportTally struct {
	Added   int
	Known   int
	Revived int
}

// ImportBoards is ImportBoardsTally returning only the added and known counts.
func (q Queries) ImportBoards(ctx context.Context, boards []BoardImport) (added, known int, err error) {
	t, err := q.ImportBoardsTally(ctx, boards)
	return t.Added, t.Known, err
}

// ImportBoardsTally adds boards to the registry. A board already there keeps its state and
// checks; its origin moves up (manual > found > seed-list = commoncrawl, never down) and an
// empty name is filled in. One exception to keeping the state: an import that raises a
// retired board's origin to manual revives it (state unverified, misses 0), since the
// operator has just named it. Boards repeated in the input count once, as already known.
func (q Queries) ImportBoardsTally(ctx context.Context, boards []BoardImport) (ImportTally, error) {
	var t ImportTally
	seen := map[BoardKey]bool{}
	var ats, slugs, regions, names, origins []string
	for _, b := range boards {
		k := b.normalised()
		if seen[k] {
			t.Known++
			continue
		}
		seen[k] = true
		ats, slugs, regions = append(ats, k.ATS), append(slugs, k.Slug), append(regions, k.Region)
		names, origins = append(names, strings.TrimSpace(b.Name)), append(origins, b.Origin)
	}
	for start := 0; start < len(ats); start += importChunk {
		end := min(start+importChunk, len(ats))
		n, revived, err := q.importChunk(ctx, ats[start:end], slugs[start:end], regions[start:end], names[start:end], origins[start:end])
		if err != nil {
			return t, err
		}
		t.Added += n
		t.Known += end - start - n
		t.Revived += revived
	}
	return t, nil
}

func (q Queries) importChunk(ctx context.Context, ats, slugs, regions, names, origins []string) (added, revived int, err error) {
	raises := fmt.Sprintf(originRankSQL, "EXCLUDED.origin") + ` > ` + fmt.Sprintf(originRankSQL, "board.origin")
	revive := `EXCLUDED.origin = 'manual' AND board.origin <> 'manual' AND board.state = 'retired'`
	// xmax = 0 marks a row this statement inserted; an existing row that changed has its
	// transaction id there. Rows that need no change are not touched and not returned. The
	// old CTE reads the rows as they were before the insert (one snapshot for the whole
	// statement), so a revived board is one that was retired and now is not.
	sql := `WITH input AS (
		  SELECT * FROM unnest($1::text[], $2::text[], $3::text[], $4::text[], $5::text[]) AS t(ats, slug, region, name, origin)
		), old AS (
		  SELECT b.id, b.state FROM board b JOIN input i ON (b.ats, b.slug, b.region) = (i.ats, i.slug, i.region)
		), ins AS (
		  INSERT INTO board (ats, slug, region, name, origin)
		  SELECT ats, slug, region, name, origin FROM input
		  ON CONFLICT (ats, slug, region) DO UPDATE SET
		    origin = CASE WHEN ` + raises + ` THEN EXCLUDED.origin ELSE board.origin END,
		    name = CASE WHEN board.name = '' THEN EXCLUDED.name ELSE board.name END,
		    state = CASE WHEN ` + revive + ` THEN 'unverified' ELSE board.state END,
		    misses = CASE WHEN ` + revive + ` THEN 0 ELSE board.misses END,
		    last_miss_day = CASE WHEN ` + revive + ` THEN NULL ELSE board.last_miss_day END
		  WHERE ` + raises + `
		     OR (board.name = '' AND EXCLUDED.name <> '')
		  RETURNING board.id, xmax = 0 AS inserted, board.state
		)
		SELECT ins.inserted, COALESCE(old.state = 'retired' AND ins.state <> 'retired', false)
		FROM ins LEFT JOIN old ON old.id = ins.id`
	rows, err := q.db.Query(ctx, sql, ats, slugs, regions, names, origins)
	if err != nil {
		return 0, 0, fmt.Errorf("store: import boards: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var ins, rev bool
		if err := rows.Scan(&ins, &rev); err != nil {
			return 0, 0, fmt.Errorf("store: import boards: %w", err)
		}
		if ins {
			added++
		}
		if rev {
			revived++
		}
	}
	if err := rows.Err(); err != nil {
		return 0, 0, fmt.Errorf("store: import boards: %w", err)
	}
	return added, revived, nil
}

// BoardFilter selects registry rows. Empty fields select everything.
type BoardFilter struct {
	State string
	ATS   string
	// NotRetired leaves retired boards out (ignored when State is set).
	NotRetired bool
	Limit      int    // 0: no limit
	Order      string // "" or "id": by id; "oldest": least recently checked first; "random"
}

// Board orders a BoardFilter can ask for.
const (
	BoardOrderOldest = "oldest"
	BoardOrderRandom = "random"
)

const boardColumns = `id, ats, slug, region, name, origin, state, added_at, checked_at, last_ok_at, misses, last_miss_day, postings_last`

// ListBoards returns the registry rows f selects.
func (q Queries) ListBoards(ctx context.Context, f BoardFilter) ([]Board, error) {
	order := "id"
	switch f.Order {
	case "", "id":
	case BoardOrderOldest:
		order = "checked_at NULLS FIRST, id"
	case BoardOrderRandom:
		order = "random()"
	default:
		return nil, fmt.Errorf("store: list boards: unknown order %q", f.Order)
	}
	limit := any(nil)
	if f.Limit > 0 {
		limit = f.Limit
	}
	rows, err := q.db.Query(ctx,
		`SELECT `+boardColumns+` FROM board
		 WHERE ($1 = '' OR state = $1) AND ($2 = '' OR ats = $2)
		   AND ($1 <> '' OR NOT $3 OR state <> 'retired')
		 ORDER BY `+order+` LIMIT $4`, f.State, f.ATS, f.NotRetired, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list boards: %w", err)
	}
	bs, err := pgx.CollectRows(rows, pgx.RowToStructByName[Board])
	if err != nil {
		return nil, fmt.Errorf("store: list boards: %w", err)
	}
	return bs, nil
}

// CollectBoards returns the boards a collect reads: every board in state ok, and every
// manual board in any state but retired, by id.
func (q Queries) CollectBoards(ctx context.Context) ([]Board, error) {
	rows, err := q.db.Query(ctx,
		`SELECT `+boardColumns+` FROM board
		 WHERE state = 'ok' OR (origin = 'manual' AND state <> 'retired')
		 ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("store: collect boards: %w", err)
	}
	bs, err := pgx.CollectRows(rows, pgx.RowToStructByName[Board])
	if err != nil {
		return nil, fmt.Errorf("store: collect boards: %w", err)
	}
	return bs, nil
}

// BoardCount is how many boards of one ATS are in one state.
type BoardCount struct {
	ATS   string `db:"ats"`
	State string `db:"state"`
	N     int    `db:"n"`
}

// CountBoards counts the registry by ATS and state, with the same ATS and state filters as
// ListBoards. Combinations with no board are left out.
func (q Queries) CountBoards(ctx context.Context, f BoardFilter) ([]BoardCount, error) {
	rows, err := q.db.Query(ctx,
		`SELECT ats, state, count(*)::int AS n FROM board
		 WHERE ($1 = '' OR state = $1) AND ($2 = '' OR ats = $2)
		 GROUP BY ats, state ORDER BY ats, state`, f.State, f.ATS)
	if err != nil {
		return nil, fmt.Errorf("store: count boards: %w", err)
	}
	cs, err := pgx.CollectRows(rows, pgx.RowToStructByName[BoardCount])
	if err != nil {
		return nil, fmt.Errorf("store: count boards: %w", err)
	}
	return cs, nil
}

// The outcomes of one board request, as RecordBoardCheck takes them.
const (
	CheckOK       = BoardOK       // answered and parsed
	CheckNotFound = BoardNotFound // answered 404
	CheckError    = BoardError    // an unexpected status, a network error or a body the parser could not read
	// CheckNoVerdict: the request said nothing about the board (a body over the size limit,
	// a timeout). Only checked_at is written: no miss, no state change, last_ok_at and
	// postings_last kept.
	CheckNoVerdict = "no_verdict"
)

// BoardChange is what one RecordBoardCheck did to a board's row.
type BoardChange struct {
	Found  bool   // the board is in the registry; false: nothing was written
	Old    string // state before
	New    string // state after
	Misses int
	Manual bool
	// Flagged: a manual board that has missed enough days to be retired, which manual boards
	// never are.
	Flagged bool
}

// Changed reports whether the board's state moved.
func (c BoardChange) Changed() bool { return c.Found && c.Old != c.New }

// RecordBoardCheck writes the outcome of one request to a board: checked_at always; on ok
// the state, last_ok_at and postings_last, and misses back to 0; on not_found or error the
// state and one more miss, but only one per local day (the day of now in loc), so a board
// that fails twice in a day is not retired early. A board that is not manual and reaches
// RetireAfterMisses becomes retired; a manual one keeps its outcome state and is Flagged. On
// CheckNoVerdict only checked_at is written. A board missing from the registry is left out
// (Found false): a check never inserts.
func (q Queries) RecordBoardCheck(ctx context.Context, k BoardKey, outcome string, postings int, loc *time.Location, now time.Time) (BoardChange, error) {
	switch outcome {
	case CheckOK, CheckNotFound, CheckError, CheckNoVerdict:
	default:
		return BoardChange{}, fmt.Errorf("store: record board check: unknown outcome %q", outcome)
	}
	if loc == nil {
		return BoardChange{}, errors.New("store: record board check: nil location")
	}
	k = k.normalised()
	if outcome == CheckNoVerdict {
		return q.recordNoVerdict(ctx, k, now)
	}
	local := now.In(loc)
	day := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, time.UTC)
	var c BoardChange
	var origin string
	err := q.db.QueryRow(ctx,
		`WITH old AS (
		   SELECT id, state FROM board WHERE ats = $1 AND slug = $2 AND region = $3 FOR UPDATE
		 ), miss AS (
		   SELECT b.id,
		          CASE WHEN $4 = 'ok' THEN 0
		               WHEN b.last_miss_day IS DISTINCT FROM $7::date THEN b.misses + 1
		               ELSE b.misses END AS misses
		   FROM board b JOIN old ON old.id = b.id
		 )
		 UPDATE board b SET
		   checked_at = $6,
		   last_ok_at = CASE WHEN $4 = 'ok' THEN $6 ELSE b.last_ok_at END,
		   postings_last = CASE WHEN $4 = 'ok' THEN $5 ELSE b.postings_last END,
		   misses = miss.misses,
		   last_miss_day = CASE WHEN $4 = 'ok' THEN NULL ELSE $7::date END,
		   state = CASE WHEN $4 <> 'ok' AND b.origin <> 'manual' AND miss.misses >= $8 THEN 'retired' ELSE $4 END
		 FROM old, miss
		 WHERE b.id = old.id AND miss.id = b.id
		 RETURNING old.state, b.state, b.misses, b.origin`,
		k.ATS, k.Slug, k.Region, outcome, postings, now, day, RetireAfterMisses,
	).Scan(&c.Old, &c.New, &c.Misses, &origin)
	if errors.Is(err, pgx.ErrNoRows) {
		return BoardChange{}, nil
	}
	if err != nil {
		return BoardChange{}, fmt.Errorf("store: record board check %s/%s: %w", k.ATS, k.Slug, err)
	}
	c.Found = true
	c.Manual = origin == OriginManual
	c.Flagged = c.Manual && outcome != CheckOK && c.Misses >= RetireAfterMisses
	return c, nil
}

// recordNoVerdict stamps checked_at on a board and changes nothing else.
func (q Queries) recordNoVerdict(ctx context.Context, k BoardKey, now time.Time) (BoardChange, error) {
	var c BoardChange
	var origin string
	err := q.db.QueryRow(ctx,
		`UPDATE board SET checked_at = $4 WHERE ats = $1 AND slug = $2 AND region = $3
		 RETURNING state, misses, origin`, k.ATS, k.Slug, k.Region, now).Scan(&c.New, &c.Misses, &origin)
	if errors.Is(err, pgx.ErrNoRows) {
		return BoardChange{}, nil
	}
	if err != nil {
		return BoardChange{}, fmt.Errorf("store: record board check %s/%s: %w", k.ATS, k.Slug, err)
	}
	c.Found, c.Old, c.Manual = true, c.New, origin == OriginManual
	return c, nil
}
