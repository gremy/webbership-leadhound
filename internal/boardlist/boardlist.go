// Package boardlist turns lists of public job boards into ats.Board values: the seed CSV
// (`name,slug,url`) and Common Crawl CDX JSON lines. It is pure: no network, no database.
// WriteCSV writes the same CSV shape ParseCSV reads, so a harvest's output is an import's
// input.
package boardlist

import (
	"bufio"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path"
	"sort"
	"strings"

	"github.com/gremy/webbership-leadhound/internal/ats"
)

// Entry is one board found in a list. Name is the company name the list gives ("" when it
// gives none, as in a CDX harvest); Board.Name carries the same value.
type Entry struct {
	Board ats.Board
	Name  string
}

// Refused is one input line that did not become an Entry. Reason is one of the fixed
// reason texts below, so refusals can be counted by reason; Text is the offending value.
type Refused struct {
	Line   int
	Text   string
	Reason string
}

// The reasons a URL or a line is refused. Each is a fixed text with no value in it.
var (
	ErrBadURL          = errors.New("not an http or https URL")
	ErrUnsupportedHost = errors.New("not a supported board host")
	ErrNoSlug          = errors.New("no board slug in the URL")
	ErrNotBoardPath    = errors.New("not a board page (embed, API or asset path)")
	ErrBadSlug         = errors.New("slug is not a valid board slug")
	ErrSlugMismatch    = errors.New("slug column disagrees with the URL")
	ErrBadCSVLine      = errors.New("not a readable CSV line")
	ErrBadCDXLine      = errors.New("not a CDX JSON line with a url")
)

// reasons lists every refusal reason, for reasonOf.
var reasons = []error{ErrBadURL, ErrUnsupportedHost, ErrNoSlug, ErrNotBoardPath, ErrBadSlug, ErrSlugMismatch, ErrBadCSVLine, ErrBadCDXLine}

// reasonOf is the fixed reason text an error carries.
func reasonOf(err error) string {
	for _, r := range reasons {
		if errors.Is(err, r) {
			return r.Error()
		}
	}
	return err.Error()
}

// host is a supported board host: the ATS, the Lever region, and the first path segments
// that are not boards.
type host struct {
	ats, region string
	reserved    map[string]bool
}

func set(words ...string) map[string]bool {
	m := map[string]bool{}
	for _, w := range words {
		m[w] = true
	}
	return m
}

var (
	greenhouseReserved = set("embed", "v1", "include", "assets", "static", "api", "job_app")
	leverReserved      = set("v0", "v1", "api", "assets", "static", "apply", "embed")
	ashbyReserved      = set("api", "assets", "static", "embed", "posting-api")
)

// hosts are the board hosts FromURL accepts, matched exactly after lower-casing.
var hosts = map[string]host{
	"boards.greenhouse.io":     {ats: ats.Greenhouse, reserved: greenhouseReserved},
	"job-boards.greenhouse.io": {ats: ats.Greenhouse, reserved: greenhouseReserved},
	"jobs.lever.co":            {ats: ats.Lever, reserved: leverReserved},
	"jobs.eu.lever.co":         {ats: ats.Lever, region: ats.RegionEU, reserved: leverReserved},
	"jobs.ashbyhq.com":         {ats: ats.Ashby, reserved: ashbyReserved},
}

// assetExt are file extensions that mark a first path segment as a file, not a board.
var assetExt = set(".ico", ".txt", ".xml", ".js", ".css", ".png", ".jpg", ".jpeg", ".gif", ".svg",
	".webp", ".json", ".map", ".woff", ".woff2", ".html", ".htm", ".php")

// FromURL maps a public board URL to its Board: boards.greenhouse.io/<slug> and
// job-boards.greenhouse.io/<slug> are Greenhouse, jobs.lever.co/<slug> is Lever,
// jobs.eu.lever.co/<slug> is Lever in region eu, jobs.ashbyhq.com/<slug> is Ashby. The
// host matches exactly (any case), the scheme is http or https (none is read as https),
// and anything after the first path segment is ignored. The slug is that segment, in the
// case the URL spells it (Lever's API is case-sensitive: AIFund answers, aifund is a 404);
// it must pass ats.Board.Validate. Embed, API and asset paths are refused.
// Every error wraps one of the Err* reasons above.
func FromURL(raw string) (ats.Board, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ats.Board{}, ErrBadURL
	}
	if !strings.Contains(s, "://") {
		s = "https://" + s // CDX patterns and some lists drop the scheme
	}
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return ats.Board{}, fmt.Errorf("%w: %q", ErrBadURL, raw)
	}
	h, ok := hosts[strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")]
	if !ok {
		return ats.Board{}, fmt.Errorf("%w: %q", ErrUnsupportedHost, u.Hostname())
	}
	seg, _, _ := strings.Cut(strings.TrimPrefix(u.Path, "/"), "/")
	if seg == "" {
		return ats.Board{}, fmt.Errorf("%w: %q", ErrNoSlug, raw)
	}
	if low := strings.ToLower(seg); h.reserved[low] || assetExt[path.Ext(low)] {
		return ats.Board{}, fmt.Errorf("%w: %q", ErrNotBoardPath, raw)
	}
	b := ats.Board{ATS: h.ats, Board: seg, Region: h.region}
	if err := b.Validate(); err != nil {
		return ats.Board{}, fmt.Errorf("%w: %w", ErrBadSlug, err)
	}
	return b, nil
}

// BoardURL is the public board page for b, in the form WriteCSV writes and FromURL reads.
func BoardURL(b ats.Board) string {
	slug := url.PathEscape(b.Board)
	switch b.ATS {
	case ats.Greenhouse:
		return "https://job-boards.greenhouse.io/" + slug
	case ats.Lever:
		if b.Region == ats.RegionEU {
			return "https://jobs.eu.lever.co/" + slug
		}
		return "https://jobs.lever.co/" + slug
	case ats.Ashby:
		return "https://jobs.ashbyhq.com/" + slug
	}
	return ""
}

// ParseCSV reads a `name,slug,url` board list. The header line is required and names the
// three columns in any order (any case); other columns are ignored. The ATS, slug and
// region come from the URL (FromURL). A row whose slug column is set and disagrees with
// the URL's slug (ignoring case) is refused rather than guessed at. A line CSV cannot read
// is refused and reading goes on. Duplicates are kept; Dedupe drops them. The error is
// for a missing or incomplete header or a read failure.
func ParseCSV(r io.Reader) ([]Entry, []Refused, error) {
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = -1
	header, err := cr.Read()
	if errors.Is(err, io.EOF) {
		return nil, nil, errors.New("boardlist: csv: empty (want a name,slug,url header)")
	}
	if err != nil {
		return nil, nil, fmt.Errorf("boardlist: csv header: %w", err)
	}
	col := map[string]int{}
	for i, h := range header {
		h = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(h, "\ufeff")))
		if _, dup := col[h]; !dup {
			col[h] = i
		}
	}
	for _, want := range []string{"name", "slug", "url"} {
		if _, ok := col[want]; !ok {
			return nil, nil, fmt.Errorf("boardlist: csv header has no %q column (want name,slug,url)", want)
		}
	}
	field := func(rec []string, name string) string {
		if i := col[name]; i < len(rec) {
			return strings.TrimSpace(rec[i])
		}
		return ""
	}

	entries := []Entry{}
	var refused []Refused
	for {
		rec, err := cr.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		var pe *csv.ParseError
		if errors.As(err, &pe) {
			refused = append(refused, Refused{Line: pe.Line, Reason: ErrBadCSVLine.Error()})
			continue
		}
		if err != nil {
			return nil, nil, fmt.Errorf("boardlist: csv: %w", err)
		}
		line, _ := cr.FieldPos(0)
		raw := field(rec, "url")
		b, err := FromURL(raw)
		if err != nil {
			refused = append(refused, Refused{Line: line, Text: raw, Reason: reasonOf(err)})
			continue
		}
		if slug := field(rec, "slug"); slug != "" && !strings.EqualFold(slug, b.Board) {
			refused = append(refused, Refused{Line: line, Text: raw, Reason: ErrSlugMismatch.Error()})
			continue
		}
		name := field(rec, "name")
		b.Name = name
		entries = append(entries, Entry{Board: b, Name: name})
	}
	return entries, refused, nil
}

// maxCDXLine is the longest CDX line read; a longer one is an error.
const maxCDXLine = 1 << 20

// ParseCDX reads Common Crawl CDX JSON lines (one `{"url": "...", ...}` object per line,
// as output=json gives), maps each url with FromURL and returns the boards deduplicated in
// first-seen order, with Name "". A URL on a host that is not a supported board host is
// counted in skipped, not refused: a crawl prefix holds plenty of those. Blank lines are
// ignored. The error is for a read failure only.
func ParseCDX(r io.Reader) (entries []Entry, refused []Refused, skipped int, err error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), maxCDXLine)
	entries = []Entry{}
	line := 0
	for sc.Scan() {
		line++
		text := strings.TrimSpace(sc.Text())
		if text == "" {
			continue
		}
		var rec struct {
			URL string `json:"url"`
		}
		if json.Unmarshal([]byte(text), &rec) != nil || strings.TrimSpace(rec.URL) == "" {
			refused = append(refused, Refused{Line: line, Text: text, Reason: ErrBadCDXLine.Error()})
			continue
		}
		b, err := FromURL(rec.URL)
		switch {
		case errors.Is(err, ErrUnsupportedHost):
			skipped++
		case err != nil:
			refused = append(refused, Refused{Line: line, Text: rec.URL, Reason: reasonOf(err)})
		default:
			entries = append(entries, Entry{Board: b})
		}
	}
	if err := sc.Err(); err != nil {
		return nil, nil, 0, fmt.Errorf("boardlist: cdx line %d: %w", line+1, err)
	}
	return Dedupe(entries), refused, skipped, nil
}

// key identifies a board for Dedupe.
func key(b ats.Board) string {
	return strings.ToLower(b.ATS + "/" + b.Board + "/" + b.Region)
}

// Dedupe keeps the first entry for each ats, slug and region, in input order.
func Dedupe(entries []Entry) []Entry {
	seen := map[string]bool{}
	out := make([]Entry, 0, len(entries))
	for _, e := range entries {
		k := key(e.Board)
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, e)
	}
	return out
}

// WriteCSV writes entries as a `name,slug,url` list, header first, sorted by ats, then
// slug, then region; ParseCSV reads it back to the same boards. The input is not changed.
func WriteCSV(w io.Writer, entries []Entry) error {
	sorted := make([]Entry, len(entries))
	copy(sorted, entries)
	sort.SliceStable(sorted, func(i, j int) bool {
		a, b := sorted[i].Board, sorted[j].Board
		if a.ATS != b.ATS {
			return a.ATS < b.ATS
		}
		if a.Board != b.Board {
			return a.Board < b.Board
		}
		return a.Region < b.Region
	})
	cw := csv.NewWriter(w)
	if err := cw.Write([]string{"name", "slug", "url"}); err != nil {
		return fmt.Errorf("boardlist: write csv: %w", err)
	}
	for _, e := range sorted {
		if err := cw.Write([]string{e.Name, e.Board.Board, BoardURL(e.Board)}); err != nil {
			return fmt.Errorf("boardlist: write csv: %w", err)
		}
	}
	cw.Flush()
	if err := cw.Error(); err != nil {
		return fmt.Errorf("boardlist: write csv: %w", err)
	}
	return nil
}
