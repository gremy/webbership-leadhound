// Package ats reads the public job boards that Greenhouse, Lever, Ashby, SmartRecruiters,
// Teamtailor and Personio host for a company. Greenhouse, Lever and Ashby list every posting
// in one request; SmartRecruiters and Teamtailor are read page by page, Personio's feed may
// be asked twice (see Collector), and a SmartRecruiters posting's description takes one more
// request (see the detail step in collector.go). The board list is
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
	Greenhouse      = "greenhouse"
	Lever           = "lever"
	Ashby           = "ashby"
	SmartRecruiters = "smartrecruiters"
	Teamtailor      = "teamtailor"
	Personio        = "personio"
)

// Names are the ATS names, in the order the collector runs them.
var Names = []string{Greenhouse, Lever, Ashby, SmartRecruiters, Teamtailor, Personio}

// namesText is the ATS names for a message: "greenhouse, lever, ... or personio".
var namesText = strings.Join(Names[:len(Names)-1], ", ") + " or " + Names[len(Names)-1]

// RegionEU is the only Lever region besides the default; it moves the API to api.eu.lever.co.
const RegionEU = "eu"

// RegionCom is the only Personio region besides the default: it moves the board from
// <slug>.jobs.personio.de to <slug>.jobs.personio.com.
const RegionCom = "com"

// MaxBoardPages is the most list pages read from one paged board (SmartRecruiters,
// Teamtailor) in one run: 2,000 postings at 100 a page. A board with more is saved as far as
// it was read, and none of its jobs expire that run (see Collector).
const MaxBoardPages = 20

// MaxBoardRequests is the most list requests one board of atsName takes in a run, retries
// for a 429 not counted: MaxBoardPages for a paged ATS, two for Personio (the feed again
// without the language), one for the others.
func MaxBoardRequests(atsName string) int {
	switch atsName {
	case SmartRecruiters, Teamtailor:
		return MaxBoardPages
	case Personio:
		return 2
	}
	return 1
}

// BoardsFile is the board list's name inside the data directory.
const BoardsFile = "ats-boards.yaml"

// ErrParserBroke means the body was not the shape this ATS returns (bad JSON, missing the
// jobs array, a posting with no id or no title). The collector treats it like LinkedIn's
// parser_broke.
var ErrParserBroke = errors.New("ats: parser broke")

// slugRe is what a board slug may look like; it keeps the slug safe inside a URL path.
var slugRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)

// labelRe is one DNS label: the slug of an ATS that puts it in the host name (Teamtailor,
// Personio), so <slug>.<ATS host> is a host with exactly one label more than the ATS's.
var labelRe = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?$`)

// Board is one line of ats-boards.yaml: `- {ats: ashby, board: examplecorp}`.
type Board struct {
	ATS    string `yaml:"ats"`
	Board  string `yaml:"board"`            // the slug in the ATS's URL
	Name   string `yaml:"name,omitempty"`   // company name when the API gives none
	Region string `yaml:"region,omitempty"` // lever: "eu" → api.eu.lever.co; personio: "com" → .jobs.personio.com
}

// regionFor is the one region each ATS may name besides none; "" for an ATS with no regions.
func regionFor(atsName string) string {
	switch atsName {
	case Lever:
		return RegionEU
	case Personio:
		return RegionCom
	}
	return ""
}

// Validate checks the ATS is known, the slug is well formed and a region is set only for
// Lever ("eu") or Personio ("com"). A Teamtailor or Personio slug becomes part of a host
// name, so it must be one DNS label (letters, digits and inner dashes, at most 63); the
// others go in a URL path and keep their case (SmartRecruiters' company id is case-sensitive).
func (b Board) Validate() error {
	switch b.ATS {
	case Greenhouse, Lever, Ashby, SmartRecruiters, Teamtailor, Personio:
	case "":
		return fmt.Errorf("ats: board %q: no ats (want %s)", b.Board, namesText)
	default:
		return fmt.Errorf("ats: board %q: unknown ats %q (want %s)", b.Board, b.ATS, namesText)
	}
	switch b.ATS {
	case Teamtailor, Personio:
		if !labelRe.MatchString(b.Board) {
			return fmt.Errorf("ats: %s board %q: a slug is one host-name label: 1-63 letters, digits or dashes, starting and ending with a letter or digit", b.ATS, b.Board)
		}
	default:
		if !slugRe.MatchString(b.Board) {
			return fmt.Errorf("ats: %s board %q: a slug is 1-100 letters, digits, dots, dashes or underscores, starting with a letter or digit", b.ATS, b.Board)
		}
	}
	region := regionFor(b.ATS)
	switch {
	case b.Region == "":
	case region == "":
		return fmt.Errorf("ats: %s: region is for %s (%q) and %s (%q) boards only", b, Lever, RegionEU, Personio, RegionCom)
	case b.Region != region:
		return fmt.Errorf("ats: %s: unknown region %q (want %q or none)", b, b.Region, region)
	}
	return nil
}

// APIURL is the public endpoint that lists the board's postings: all of them, or for a
// paged ATS the first page (see pageURL).
func (b Board) APIURL() string { return b.pageURL(0) }

// smartRecruitersAPI is the base of SmartRecruiters' public posting API.
const smartRecruitersAPI = "https://api.smartrecruiters.com/v1/companies/"

// smartRecruitersPageSize is the most postings one list request asks for (the API's limit).
const smartRecruitersPageSize = 100

// smartRecruitersListURL is one list page of a SmartRecruiters board.
func smartRecruitersListURL(b Board, limit, offset int) string {
	return smartRecruitersAPI + url.PathEscape(b.Board) + "/postings?limit=" + strconv.Itoa(limit) + "&offset=" + strconv.Itoa(max(offset, 0))
}

// DetailURL is the endpoint that gives one posting's description and apply URL, for an ATS
// whose list leaves them out (SmartRecruiters); "" for the others.
func (b Board) DetailURL(postingID string) string {
	if b.ATS != SmartRecruiters {
		return ""
	}
	return smartRecruitersAPI + url.PathEscape(b.Board) + "/postings/" + url.PathEscape(postingID)
}

// pageURL is the list request that starts at offset: the paged ATSs (SmartRecruiters,
// Teamtailor) take it as a query parameter; the others list everything at offset 0, and
// Personio's feed is asked in English (see the collector for the retry without).
func (b Board) pageURL(offset int) string {
	slug := url.PathEscape(b.Board)
	switch b.ATS {
	case SmartRecruiters:
		return smartRecruitersListURL(b, smartRecruitersPageSize, offset)
	case Teamtailor:
		return teamtailorPageURL(b, offset)
	case Personio:
		return personioFeedURL(b, personioLanguage)
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

// CheckURL is the lightest endpoint that still says whether the board exists and how many
// postings it lists (read it with CountPostings): Greenhouse's list without the
// descriptions, one SmartRecruiters posting (its totalFound counts them all), APIURL for
// the others. Teamtailor's first page counts at most teamtailorPageSize postings.
func (b Board) CheckURL() string {
	switch b.ATS {
	case Greenhouse:
		return "https://boards-api.greenhouse.io/v1/boards/" + url.PathEscape(b.Board) + "/jobs"
	case SmartRecruiters:
		return smartRecruitersListURL(b, 1, 0)
	}
	return b.APIURL()
}

// CountPostings reads how many postings a board lists from its answer to CheckURL or
// APIURL: SmartRecruiters' totalFound, else the postings Parse reads. A body of the wrong
// shape gives ErrParserBroke.
func CountPostings(b Board, body []byte) (int, error) {
	if b.ATS == SmartRecruiters {
		total, _, _, err := smartRecruitersPage(body)
		if err != nil {
			return 0, fmt.Errorf("ats: parse %s: %w", b, err)
		}
		return total, nil
	}
	ps, err := Parse(b, body)
	return len(ps), err
}

// parsePage reads one list page of b that was requested at offset. next is the offset of
// the page after it, and last says there is none: always for an ATS that lists everything
// in one answer; for SmartRecruiters when the postings read so far reach totalFound (or the
// page is empty); for Teamtailor when the page came back short.
func parsePage(b Board, offset int, body []byte) (ps []Posting, next int, last bool, err error) {
	ps, err = Parse(b, body)
	if err != nil {
		return nil, 0, false, err
	}
	next = offset + len(ps)
	switch b.ATS {
	case SmartRecruiters:
		total, _, _, err := smartRecruitersPage(body)
		if err != nil {
			return nil, 0, false, fmt.Errorf("ats: parse %s: %w", b, err)
		}
		return ps, next, len(ps) == 0 || next >= total, nil
	case Teamtailor:
		return ps, next, teamtailorLastPage(ps), nil
	}
	return ps, next, true, nil
}

// listTotal is how many postings a list page of b says the whole board holds, for an ATS
// whose pages say so (SmartRecruiters' totalFound); ok is false for the others or a page
// that does not say. The collector compares it across one board's pages: a board whose
// total changed while it was read may have shifted postings across a page edge.
func listTotal(b Board, body []byte) (total int, ok bool) {
	if b.ATS != SmartRecruiters {
		return 0, false
	}
	total, _, _, err := smartRecruitersPage(body)
	return total, err == nil
}

// regionPrefix is "<region>:" for a board with a region ("eu:", "com:") and "" otherwise. A
// slug never holds a colon, so the prefix keeps such a board apart from the same slug in
// the default region.
func (b Board) regionPrefix() string {
	if b.Region != "" {
		return b.Region + ":"
	}
	return ""
}

// String is "<ats>/<board>", or "<ats>/<region>:<board>" for a board with a region.
func (b Board) String() string { return b.ATS + "/" + b.regionPrefix() + b.Board }

// key identifies a board for the duplicate check: ats, region and slug, ignoring case.
func (b Board) key() string { return strings.ToLower(b.String()) }

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
		{ATS: SmartRecruiters, Board: "ExampleRobotics", Name: "Example Robotics"},
		{ATS: Teamtailor, Board: "mockworks", Name: "Mockworks"},
		{ATS: Personio, Board: "musterfabrik", Name: "Musterfabrik"},
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
	case SmartRecruiters:
		ps, err = parseSmartRecruiters(b, body)
	case Teamtailor:
		ps, err = parseTeamtailor(b, body)
	case Personio:
		ps, err = parsePersonio(b, body)
	default:
		return nil, fmt.Errorf("ats: parse %s: unknown ats %q", b, b.ATS)
	}
	if err != nil {
		return nil, fmt.Errorf("ats: parse %s: %w", b, err)
	}
	return ps, nil
}

// SourceJobID is the lower-cased board, a slash and the posting's id, so ids from two
// boards never collide; a board with a region starts with it ("eu:<board>/<id>",
// "com:<board>/<id>"), so the same slug in two regions stays two boards.
func SourceJobID(b Board, p Posting) string {
	return b.regionPrefix() + strings.ToLower(b.Board) + "/" + p.ID
}

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
