// Package igsignal reads Instagram capture files (ig-capture@1) and matches what they hold to
// café leads, deterministically: the parser, the matcher, the vendor rule and the coffee-name
// prefilter. The browser that writes the file (the /ig-signals skill and its helper
// ig_pace.py) lives outside this repository; this package never reaches Instagram.
//
// The package is pure: no store, no fetch. `leads ig import` (cmd/leadhound) wires it.
package igsignal

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gremy/webbership-leadhound/internal/enrich"
)

// CaptureVersion names the capture file's shape: one JSON record per line, "v": 1.
const CaptureVersion = "ig-capture@1"

// Record kinds.
const (
	KindProfile   = "profile"
	KindFollowing = "following"
)

// Profile outcomes. A private account shows its counts and nothing else; a missing handle no
// longer exists (not a wall: the run went on).
const (
	OutcomeOK      = "ok"
	OutcomeMissing = "missing"
	OutcomePrivate = "private"
)

// How a profile was reached (source.type).
const (
	SourceLead      = "lead"
	SourceFollowing = "following"
	SourceSearch    = "search"
	SourceManual    = "manual"
)

// The caps, as the skill's writer applies them: text longer than its cap is cut (in
// characters), captions past MaxCaptions are dropped, and a following record with more than
// MaxKept accounts is refused.
const (
	MaxFullName = 150
	MaxCategory = 100
	MaxBio      = 600
	MaxURL      = 500
	MaxWall     = 200
	MaxCaption  = 400
	MaxCaptions = 12
	MaxKept     = 5000
	// MaxLine is the longest line read; the writer refuses a record over 2,000,000 bytes.
	MaxLine = 4 << 20
)

// Source is how a profile was reached. Of is the list owner's handle for SourceFollowing,
// else empty. LeadID is the capture's lead hint (0 for none): for the report only, never a
// match, since lead ids differ between databases.
type Source struct {
	Type   string
	Of     string
	LeadID int64
}

// Profile is one profile capture. Text the capture did not give is empty, a count nil.
type Profile struct {
	Handle      string // lower case, without its @
	URL         string // the canonical https://www.instagram.com/<handle>/
	CapturedAt  time.Time
	Source      Source
	Outcome     string // OutcomeOK, OutcomeMissing or OutcomePrivate
	IGUserID    string
	FullName    string
	Category    string
	Bio         string
	ExternalURL string
	Followers   *int
	Following   *int
	Posts       *int
	IsPrivate   *bool
	Captions    []string
	// Wall is why the skill stopped on this read (a login form, a 429); empty for none. A
	// record with a wall is counted by the import and nothing of it is stored.
	Wall string
}

// Follow is one account kept from a following list.
type Follow struct {
	Handle   string
	FullName string
}

// Following is one capture of an account's following list, prefiltered by Filter.
type Following struct {
	Of         string // the list owner's handle
	CapturedAt time.Time
	Filter     string // CoffeeWordsVersion
	Seen       int    // accounts paged through
	Kept       []Follow
	Complete   bool   // the list ran to its end, with no wall and no cap
	Wall       string // as Profile.Wall
}

// Record is one valid line: exactly one of Profile and Following is set.
type Record struct {
	Line      int    // 1-based line number in the file
	Kind      string // KindProfile or KindFollowing
	SHA256    []byte // SHA-256 of the line's canonical JSON (keys sorted, no spaces): capture_sha256
	Profile   *Profile
	Following *Following
}

// Wall is the record's wall, empty when the read went through.
func (r Record) Wall() string {
	if r.Profile != nil {
		return r.Profile.Wall
	}
	if r.Following != nil {
		return r.Following.Wall
	}
	return ""
}

// LineError names a line Parse refused, by number only: Reason is fixed words, never the
// line's text, so a report can carry it.
type LineError struct {
	Line   int
	Reason string
}

func (e LineError) Error() string { return fmt.Sprintf("line %d: %s", e.Line, e.Reason) }

// errLine is a refusal of one line; its text is the LineError's Reason.
type errLine string

func (e errLine) Error() string { return string(e) }

var (
	handleRe = regexp.MustCompile(`^[a-z0-9._]{1,30}$`)
	userIDRe = regexp.MustCompile(`^[0-9]{1,25}$`)
	httpRe   = regexp.MustCompile(`(?i)^https?://`)
)

var (
	profileKeys = []string{"v", "kind", "handle", "url", "captured_at", "source", "outcome", "ig_user_id", "full_name",
		"category", "bio", "external_url", "followers", "following", "posts", "is_private", "captions", "wall"}
	followingKeys = []string{"v", "kind", "of", "captured_at", "filter", "seen", "kept", "complete", "wall"}
	sourceKeys    = []string{"type", "of", "lead_id"}
	followKeys    = []string{"handle", "full_name"}
)

// Parse reads a capture file: every valid line as a Record, in file order, and every other
// line as a LineError. A blank line is neither. Parse checks what the skill's writer checks,
// so a file it wrote parses whole; a line it would refuse is refused here.
func Parse(r io.Reader) ([]Record, []LineError) {
	var recs []Record
	var errs []LineError
	br := bufio.NewReaderSize(r, 64<<10)
	for n := 1; ; n++ {
		line, long, err := readLine(br)
		if err != nil && !errors.Is(err, io.EOF) {
			errs = append(errs, LineError{Line: n, Reason: "the file could not be read past here"})
			break
		}
		switch {
		case long:
			errs = append(errs, LineError{Line: n, Reason: fmt.Sprintf("longer than %d bytes", MaxLine)})
		case len(bytes.TrimSpace(line)) > 0:
			rec, perr := parseLine(line)
			if perr != nil {
				errs = append(errs, LineError{Line: n, Reason: perr.Error()})
			} else {
				rec.Line = n
				recs = append(recs, rec)
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
	}
	return recs, errs
}

// readLine reads one line without its newline. A line over MaxLine is read to its end and
// dropped (long true), so memory stays bounded and the next line is read as usual.
func readLine(br *bufio.Reader) (line []byte, long bool, err error) {
	for {
		chunk, err := br.ReadSlice('\n')
		if !long {
			if len(line)+len(chunk) > MaxLine+1 {
				long, line = true, nil
			} else {
				line = append(line, chunk...)
			}
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return bytes.TrimSuffix(bytes.TrimSuffix(line, []byte("\n")), []byte("\r")), long, err
	}
}

// fields is one JSON object's members, by exact key.
type fields map[string]json.RawMessage

func parseLine(line []byte) (Record, error) {
	if !utf8.Valid(line) {
		return Record{}, errLine("not UTF-8")
	}
	var f fields
	if err := decodeOne(line, &f); err != nil || f == nil {
		return Record{}, errLine("not one JSON object")
	}
	sha, err := canonicalSHA(line)
	if err != nil {
		return Record{}, errLine("not one JSON object")
	}
	var v int
	if raw, ok := f["v"]; !ok || json.Unmarshal(raw, &v) != nil || v != 1 {
		return Record{}, errLine("v must be 1")
	}
	var kind string
	if raw, ok := f["kind"]; ok {
		_ = json.Unmarshal(raw, &kind)
	}
	rec := Record{Kind: kind, SHA256: sha}
	switch kind {
	case KindProfile:
		rec.Profile, err = parseProfile(f)
	case KindFollowing:
		rec.Following, err = parseFollowing(f)
	default:
		return Record{}, errLine("kind must be profile or following")
	}
	if err != nil {
		return Record{}, err
	}
	return rec, nil
}

// decodeOne decodes data as exactly one JSON value into v, numbers kept as written.
func decodeOne(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errLine("more than one JSON value")
	}
	return nil
}

// canonicalSHA is the SHA-256 of line re-encoded with sorted keys, no spaces and no HTML
// escaping: the same capture written with other spacing or key order hashes the same.
func canonicalSHA(line []byte) ([]byte, error) {
	var v any
	if err := decodeOne(line, &v); err != nil {
		return nil, err
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	sum := sha256.Sum256(bytes.TrimSuffix(b.Bytes(), []byte("\n")))
	return sum[:], nil
}

func (f fields) only(keys []string, what string) error {
	for k := range f {
		if !slices.Contains(keys, k) {
			return errLine("unknown key in the " + what)
		}
	}
	return nil
}

func (f fields) null(key string) bool {
	raw, ok := f[key]
	return !ok || string(bytes.TrimSpace(raw)) == "null"
}

// text is key as text, "" when absent or null; spaces folded (only trimmed for keepLines),
// then cut to limit characters.
func (f fields) text(key string, limit int, keepLines bool) (string, error) {
	if f.null(key) {
		return "", nil
	}
	var s string
	if json.Unmarshal(f[key], &s) != nil {
		return "", errLine(key + " must be text or null")
	}
	if keepLines {
		s = strings.TrimSpace(s)
	} else {
		s = strings.Join(strings.Fields(s), " ")
	}
	return cut(s, limit), nil
}

// count is key as a whole number of zero or more, nil when absent or null.
func (f fields) count(key string) (*int, error) {
	if f.null(key) {
		return nil, nil
	}
	var n int
	if json.Unmarshal(f[key], &n) != nil || n < 0 {
		return nil, errLine(key + " must be a whole number or null")
	}
	return &n, nil
}

func (f fields) time(key string) (time.Time, error) {
	var s string
	if f.null(key) || json.Unmarshal(f[key], &s) != nil {
		return time.Time{}, errLine(key + " must be a time")
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, errLine(key + " must be a time")
	}
	return t.UTC(), nil
}

// handle is key as a handle, lower case without its @; errLine when it is not one.
func (f fields) handle(key string) (string, error) {
	var s string
	if f.null(key) || json.Unmarshal(f[key], &s) != nil {
		return "", errLine(key + " must be a handle")
	}
	return handleOf(s, key)
}

func handleOf(s, key string) (string, error) {
	h := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(s)), "@")
	if !handleRe.MatchString(h) {
		return "", errLine("bad handle in " + key)
	}
	if got, ok := enrich.InstagramHandle(h); !ok || got != h {
		return "", errLine("bad handle in " + key) // an Instagram page of its own (p, explore), or only dots
	}
	return h, nil
}

// wall is the record's wall: empty for none, else a reason, spaces folded and cut.
func (f fields) wall() (string, error) {
	if f.null("wall") {
		return "", nil
	}
	var s string
	if json.Unmarshal(f["wall"], &s) != nil || strings.TrimSpace(s) == "" {
		return "", errLine("wall must be a reason or null")
	}
	return cut(strings.Join(strings.Fields(s), " "), MaxWall), nil
}

func cut(s string, limit int) string {
	if utf8.RuneCountInString(s) <= limit {
		return s
	}
	return string([]rune(s)[:limit])
}

func parseProfile(f fields) (*Profile, error) {
	if err := f.only(profileKeys, "profile"); err != nil {
		return nil, err
	}
	var p Profile
	var err error
	if p.Handle, err = f.handle("handle"); err != nil {
		return nil, err
	}
	var u string
	if f.null("url") || json.Unmarshal(f["url"], &u) != nil || u != enrich.InstagramURL(p.Handle) {
		return nil, errLine("url must be the handle's canonical address")
	}
	p.URL = u
	if p.CapturedAt, err = f.time("captured_at"); err != nil {
		return nil, err
	}
	if p.Source, err = parseSource(f); err != nil {
		return nil, err
	}
	if !f.null("outcome") {
		_ = json.Unmarshal(f["outcome"], &p.Outcome)
	}
	if p.Outcome != OutcomeOK && p.Outcome != OutcomeMissing && p.Outcome != OutcomePrivate {
		return nil, errLine("outcome must be ok, missing or private")
	}
	if p.Wall, err = f.wall(); err != nil {
		return nil, err
	}
	if !f.null("ig_user_id") {
		if json.Unmarshal(f["ig_user_id"], &p.IGUserID) != nil || !userIDRe.MatchString(p.IGUserID) {
			return nil, errLine("ig_user_id must be digits or null")
		}
	}
	for _, t := range []struct {
		key   string
		limit int
		to    *string
	}{{"full_name", MaxFullName, &p.FullName}, {"category", MaxCategory, &p.Category}, {"bio", MaxBio, &p.Bio},
		{"external_url", MaxURL, &p.ExternalURL}} {
		if *t.to, err = f.text(t.key, t.limit, t.key == "bio"); err != nil {
			return nil, err
		}
	}
	if p.ExternalURL != "" && !httpRe.MatchString(p.ExternalURL) {
		return nil, errLine("external_url must be an http(s) link or null")
	}
	for _, c := range []struct {
		key string
		to  **int
	}{{"followers", &p.Followers}, {"following", &p.Following}, {"posts", &p.Posts}} {
		if *c.to, err = f.count(c.key); err != nil {
			return nil, err
		}
	}
	if !f.null("is_private") {
		var b bool
		if json.Unmarshal(f["is_private"], &b) != nil {
			return nil, errLine("is_private must be true, false or null")
		}
		p.IsPrivate = &b
	}
	if !f.null("captions") {
		var cs []string
		if json.Unmarshal(f["captions"], &cs) != nil {
			return nil, errLine("captions must be a list of text")
		}
		for _, c := range cs {
			if c = strings.Join(strings.Fields(c), " "); c != "" && len(p.Captions) < MaxCaptions {
				p.Captions = append(p.Captions, cut(c, MaxCaption))
			}
		}
	}
	switch {
	case p.Outcome == OutcomePrivate && (p.IsPrivate == nil || !*p.IsPrivate):
		return nil, errLine("outcome private needs is_private true")
	case p.Outcome != OutcomeOK && len(p.Captions) > 0:
		return nil, errLine("a " + p.Outcome + " profile carries no captions")
	}
	return &p, nil
}

func parseSource(f fields) (Source, error) {
	var s fields
	if f.null("source") || json.Unmarshal(f["source"], &s) != nil || s == nil {
		return Source{}, errLine("source must be an object")
	}
	if err := s.only(sourceKeys, "source"); err != nil {
		return Source{}, err
	}
	var out Source
	if !s.null("type") {
		_ = json.Unmarshal(s["type"], &out.Type)
	}
	if !slices.Contains([]string{SourceLead, SourceFollowing, SourceSearch, SourceManual}, out.Type) {
		return Source{}, errLine("source type must be lead, following, search or manual")
	}
	var err error
	switch {
	case out.Type == SourceFollowing:
		if out.Of, err = s.handle("of"); err != nil {
			return Source{}, errLine("source.of must be a handle")
		}
	case !s.null("of"):
		return Source{}, errLine("source.of is only for a following list")
	}
	if !s.null("lead_id") {
		if json.Unmarshal(s["lead_id"], &out.LeadID) != nil || out.LeadID <= 0 ||
			(out.Type != SourceLead && out.Type != SourceSearch) {
			return Source{}, errLine("source.lead_id is a positive number, only for source lead or search")
		}
	}
	return out, nil
}

func parseFollowing(f fields) (*Following, error) {
	if err := f.only(followingKeys, "following record"); err != nil {
		return nil, err
	}
	var out Following
	var err error
	if out.Of, err = f.handle("of"); err != nil {
		return nil, err
	}
	if out.CapturedAt, err = f.time("captured_at"); err != nil {
		return nil, err
	}
	if !f.null("filter") {
		_ = json.Unmarshal(f["filter"], &out.Filter)
	}
	if out.Filter != CoffeeWordsVersion {
		return nil, errLine("filter must be " + CoffeeWordsVersion)
	}
	if out.Wall, err = f.wall(); err != nil {
		return nil, err
	}
	var kept []fields
	if f.null("kept") || json.Unmarshal(f["kept"], &kept) != nil {
		return nil, errLine("kept must be a list of accounts")
	}
	if len(kept) > MaxKept {
		return nil, errLine(fmt.Sprintf("kept must be at most %d accounts", MaxKept))
	}
	seen := map[string]bool{}
	for _, k := range kept {
		if k == nil || k.only(followKeys, "kept account") != nil {
			return nil, errLine("each kept account holds only handle and full_name")
		}
		h, err := k.handle("handle")
		if err != nil {
			return nil, errLine("bad handle in kept")
		}
		if seen[h] {
			return nil, errLine("a kept account appears twice")
		}
		seen[h] = true
		name, err := k.text("full_name", MaxFullName, false)
		if err != nil {
			return nil, errLine("kept full_name must be text or null")
		}
		if !CoffeeName(h, name) {
			return nil, errLine("a kept account fails " + CoffeeWordsVersion)
		}
		out.Kept = append(out.Kept, Follow{Handle: h, FullName: name})
	}
	if f.null("seen") || json.Unmarshal(f["seen"], &out.Seen) != nil || out.Seen < len(out.Kept) {
		return nil, errLine("seen must be a whole number, at least the kept count")
	}
	if f.null("complete") || json.Unmarshal(f["complete"], &out.Complete) != nil {
		return nil, errLine("complete must be true or false")
	}
	if out.Wall != "" && out.Complete {
		return nil, errLine("a list stopped by a wall is not complete")
	}
	return &out, nil
}
