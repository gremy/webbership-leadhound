// Package ats reads the public job boards that Greenhouse, Lever and Ashby host for a
// company: one request per board returns every posting it lists. The board list is
// ats-boards.yaml in LEADHOUND_DATA_DIR; the repository ships a fictional one in
// examples/ats-boards.demo.yaml, answered by the fixtures in testdata/.
package ats

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// The ATS names a board line can carry.
const (
	Greenhouse = "greenhouse"
	Lever      = "lever"
	Ashby      = "ashby"
)

// RegionEU is the only Lever region besides the default; it moves the API to api.eu.lever.co.
const RegionEU = "eu"

// BoardsFile is the board list's name inside the data directory.
const BoardsFile = "ats-boards.yaml"

// ErrParserBroke means the body was not the shape this ATS returns (bad JSON, missing the
// jobs array, a posting with no id or no title). The collector treats it like LinkedIn's
// parser_broke.
var ErrParserBroke = errors.New("ats: parser broke")

// slugRe is what a board slug may look like; it keeps the slug safe inside a URL path.
var slugRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)

// Board is one line of ats-boards.yaml: `- {ats: ashby, board: examplecorp}`.
type Board struct {
	ATS    string `yaml:"ats"`
	Board  string `yaml:"board"`            // the slug in the ATS's URL
	Name   string `yaml:"name,omitempty"`   // company name when the API gives none
	Region string `yaml:"region,omitempty"` // lever only: "eu" → api.eu.lever.co
}

// Validate checks the ATS is known, the slug is well formed and a region is set only for
// Lever, and only as "eu".
func (b Board) Validate() error {
	switch b.ATS {
	case Greenhouse, Lever, Ashby:
	case "":
		return fmt.Errorf("ats: board %q: no ats (want %s, %s or %s)", b.Board, Greenhouse, Lever, Ashby)
	default:
		return fmt.Errorf("ats: board %q: unknown ats %q (want %s, %s or %s)", b.Board, b.ATS, Greenhouse, Lever, Ashby)
	}
	if !slugRe.MatchString(b.Board) {
		return fmt.Errorf("ats: %s board %q: a slug is 1-100 letters, digits, dots, dashes or underscores, starting with a letter or digit", b.ATS, b.Board)
	}
	switch {
	case b.Region == "":
	case b.ATS != Lever:
		return fmt.Errorf("ats: %s: region is for lever boards only", b)
	case b.Region != RegionEU:
		return fmt.Errorf("ats: %s: unknown region %q (want %q or none)", b, b.Region, RegionEU)
	}
	return nil
}

// APIURL is the public endpoint that lists every posting on the board.
func (b Board) APIURL() string {
	slug := url.PathEscape(b.Board)
	switch b.ATS {
	case Greenhouse:
		return "https://boards-api.greenhouse.io/v1/boards/" + slug + "/jobs?content=true"
	case Lever:
		host := "api.lever.co"
		if b.Region == RegionEU {
			host = "api.eu.lever.co"
		}
		return "https://" + host + "/v0/postings/" + slug + "?mode=json"
	case Ashby:
		return "https://api.ashbyhq.com/posting-api/job-board/" + slug
	}
	return ""
}

// String is "<ats>/<board>".
func (b Board) String() string { return b.ATS + "/" + b.Board }

// key identifies a board for the duplicate check.
func (b Board) key() string { return strings.ToLower(b.ATS + "/" + b.Board) }

// company is the name a posting carries when the API gives none.
func (b Board) company() string {
	if n := strings.TrimSpace(b.Name); n != "" {
		return n
	}
	return b.Board
}

// LoadBoards reads <dataDir>/ats-boards.yaml strictly, validates each line, refuses
// duplicates (same ats and board, ignoring case) naming the line, and returns the boards
// in file order. A missing file keeps os.ErrNotExist in its chain.
func LoadBoards(dataDir string) ([]Board, error) {
	return LoadBoardsFile(filepath.Join(dataDir, BoardsFile))
}

// LoadBoardsFile is LoadBoards for an explicit path (used by --boards FILE).
func LoadBoardsFile(path string) ([]Board, error) {
	raw, err := os.ReadFile(path) // #nosec G304 -- the operator's own board list
	if err != nil {
		return nil, fmt.Errorf("ats: %w", err)
	}
	boards, err := parseBoards(raw)
	if err != nil {
		return nil, fmt.Errorf("ats: %s: %w", filepath.Base(path), err)
	}
	return boards, nil
}

// parseBoards decodes a board list: a YAML sequence of Board mappings, unknown keys refused.
func parseBoards(raw []byte) ([]Board, error) {
	var boards []Board
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&boards); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("the file is empty (write [] for no boards)")
		}
		return nil, err
	}
	// A second pass keeps each item's line number for the messages below.
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	var lines []int
	if len(doc.Content) == 1 && doc.Content[0].Kind == yaml.SequenceNode {
		for _, n := range doc.Content[0].Content {
			lines = append(lines, n.Line)
		}
	}
	lineOf := func(i int) int {
		if i < len(lines) {
			return lines[i]
		}
		return 0
	}

	seen := map[string]int{} // key → line of first sight
	for i, b := range boards {
		line := lineOf(i)
		if err := b.Validate(); err != nil {
			return nil, fmt.Errorf("line %d: %w", line, err)
		}
		if first, dup := seen[b.key()]; dup {
			return nil, fmt.Errorf("line %d: %s is listed twice (first on line %d)", line, b, first)
		}
		seen[b.key()] = line
	}
	if boards == nil {
		boards = []Board{}
	}
	return boards, nil
}

// DemoBoards is the built-in list of fictional boards that the testdata fixtures answer.
func DemoBoards() []Board {
	return []Board{
		{ATS: Greenhouse, Board: "examplecorp", Name: "Example Corp"},
		{ATS: Lever, Board: "samplesoft", Name: "SampleSoft"},
		{ATS: Lever, Board: "beispielwerk", Name: "Beispielwerk", Region: RegionEU},
		{ATS: Ashby, Board: "demolabs", Name: "Demo Labs"},
	}
}

// Posting is one job as a board listed it.
type Posting struct {
	ID              string // the ATS's own id, as text
	Title           string
	Company         string // from the API when it has one, else Board.Name, else the slug
	Location        string // as listed, plus the stated workplace (see withWorkplace)
	DescriptionHTML string // markup for pipeline.Observe to sanitise
	EmploymentType  string
	PostedAt        *time.Time // UTC; nil when the board gives no date
	ApplyURL        string
	URL             string // the public posting page (job.url)
}

// Parse reads one board's API response. It dispatches on b.ATS; unlisted postings (Ashby
// isListed=false) are dropped. A body of the wrong shape gives ErrParserBroke.
func Parse(b Board, body []byte) ([]Posting, error) {
	var (
		ps  []Posting
		err error
	)
	switch b.ATS {
	case Greenhouse:
		ps, err = parseGreenhouse(b, body)
	case Lever:
		ps, err = parseLever(b, body)
	case Ashby:
		ps, err = parseAshby(b, body)
	default:
		return nil, fmt.Errorf("ats: parse %s: unknown ats %q", b, b.ATS)
	}
	if err != nil {
		return nil, fmt.Errorf("ats: parse %s: %w", b, err)
	}
	return ps, nil
}

// SourceJobID is the lower-cased board, a slash and the posting's id, so ids from two
// boards never collide.
func SourceJobID(b Board, p Posting) string { return strings.ToLower(b.Board) + "/" + p.ID }

// DerivedBy is the provenance tag of rows derived from an ATS response: "<ats>-parser@1".
func DerivedBy(atsName string) string { return atsName + "-parser@1" }

// SourceFixture is the source name of rows from a dry run, "<ats>-fixture", so they never
// read as the ATS's own data.
func SourceFixture(atsName string) string { return atsName + "-fixture" }

// broke wraps a shape problem in ErrParserBroke.
func broke(format string, args ...any) error {
	return fmt.Errorf("%s: %w", fmt.Sprintf(format, args...), ErrParserBroke)
}

// idText turns a JSON id (a number or a string) into text; "" when absent or empty.
func idText(raw json.RawMessage) string {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return ""
	}
	if s[0] == '"' {
		var str string
		if err := json.Unmarshal(raw, &str); err != nil {
			return ""
		}
		return strings.TrimSpace(str)
	}
	var n json.Number
	if err := json.Unmarshal(raw, &n); err != nil {
		return ""
	}
	if i, err := n.Int64(); err == nil {
		return strconv.FormatInt(i, 10)
	}
	return n.String()
}

// parseTime reads an RFC 3339 date; nil when empty or not understood.
func parseTime(s string) *time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return nil
	}
	t = t.UTC()
	return &t
}

// joinNonEmpty joins the non-blank parts with ", ".
func joinNonEmpty(parts ...string) string {
	var out []string
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, ", ")
}

// The workplaces an employer can state.
const (
	workRemote = "remote"
	workHybrid = "hybrid"
	workOnsite = "onsite"
)

// workplaceKind normalises an ATS workplace value ("on-site", "OnSite", "Remote", ...) to
// one of the work* constants; "" for unknown or unspecified.
func workplaceKind(v string) string {
	k := strings.NewReplacer("-", "", "_", "", " ", "").Replace(strings.ToLower(strings.TrimSpace(v)))
	switch k {
	case workRemote, workHybrid, workOnsite:
		return k
	}
	return ""
}

// withWorkplace adds the employer's stated workplace to the location text, so the code
// gates and the brief see it: " (Remote)", " (Hybrid)" or " (On-site)", unless the text
// already says so. An empty location becomes the bare word.
func withWorkplace(location, kind string) string {
	var label string
	var words []string
	switch kind {
	case workRemote:
		label, words = "Remote", []string{"remote"}
	case workHybrid:
		label, words = "Hybrid", []string{"hybrid"}
	case workOnsite:
		label, words = "On-site", []string{"on-site", "onsite", "on site", "in office", "in-office"}
	default:
		return location
	}
	location = strings.TrimSpace(location)
	if location == "" {
		return label
	}
	low := strings.ToLower(location)
	for _, w := range words {
		if strings.Contains(low, w) {
			return location
		}
	}
	return location + " (" + label + ")"
}
