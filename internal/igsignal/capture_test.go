package igsignal

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// marker sits in every refused line: a LineError must never carry it.
const marker = "MARKER_TEXT_NEVER_IN_A_REASON"

func profileRec(over map[string]any) map[string]any {
	r := map[string]any{
		"v": 1, "kind": "profile", "handle": "cafe.exemplu", "url": "https://www.instagram.com/cafe.exemplu/",
		"captured_at": "2026-11-05T21:00:00Z", "source": map[string]any{"type": "lead", "of": nil, "lead_id": 42},
		"outcome": "ok", "ig_user_id": "1234567", "full_name": "Cafe Exemplu " + marker, "category": "Coffee shop",
		"bio": "Specialty coffee", "external_url": "https://cafe-exemplu.example/", "followers": 1200, "following": 300,
		"posts": 88, "is_private": false, "captions": []any{"Latte art"}, "wall": nil,
	}
	maps.Copy(r, over)
	return r
}

func followingRec(over map[string]any) map[string]any {
	r := map[string]any{
		"v": 1, "kind": "following", "of": "vendor_example", "captured_at": "2026-11-05T21:30:00Z", "filter": "coffee-words@1",
		"seen": 120, "kept": []any{map[string]any{"handle": "cafe.exemplu", "full_name": "Cafe Exemplu " + marker}},
		"complete": true, "wall": nil,
	}
	maps.Copy(r, over)
	return r
}

func line(t *testing.T, rec map[string]any) string {
	t.Helper()
	b, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func parseString(s string) ([]Record, []LineError) { return Parse(strings.NewReader(s)) }

func readTestdata(t *testing.T, name string) ([]Record, []LineError) {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", name)) //nolint:gosec // G304: tests read their own fixtures by name
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	return Parse(f)
}

// TestCaptureValidTestdata: valid.jsonl, the lines ig_pace.py write also accepts (its own
// contract test feeds this file through it), parses whole, field by field.
func TestCaptureValidTestdata(t *testing.T) {
	recs, errs := readTestdata(t, "valid.jsonl")
	if len(errs) != 0 || len(recs) != 8 {
		t.Fatalf("valid.jsonl: %d records, errors %v", len(recs), errs)
	}
	p := recs[0].Profile
	if recs[0].Kind != KindProfile || p == nil || recs[0].Line != 1 {
		t.Fatalf("line 1: %+v", recs[0])
	}
	if p.Handle != "cafe.exemplu" || p.Source != (Source{Type: SourceLead, LeadID: 42}) || p.Outcome != OutcomeOK ||
		p.ExternalURL != "https://cafe-exemplu.example/" || *p.Followers != 1200 || len(p.Captions) != 2 ||
		!p.CapturedAt.Equal(time.Date(2026, 11, 5, 21, 0, 0, 0, time.UTC)) || p.IsPrivate == nil || *p.IsPrivate {
		t.Errorf("line 1 profile: %+v", p)
	}
	if got := recs[1].Profile; got.Source != (Source{Type: SourceFollowing, Of: "vendor_example"}) ||
		!strings.Contains(got.Bio, "\n") {
		t.Errorf("line 2: a bio keeps its lines, a following source its owner: %+v", got)
	}
	if got := recs[2].Profile; got.Outcome != OutcomePrivate || got.Captions != nil {
		t.Errorf("line 3 private: %+v", got)
	}
	if got := recs[3].Profile; got.Outcome != OutcomeMissing || got.Followers != nil || got.IsPrivate != nil ||
		got.Source.LeadID != 7 {
		t.Errorf("line 4 missing: %+v", got)
	}
	if recs[4].Wall() != "login form" {
		t.Errorf("line 5 wall: %q", recs[4].Wall())
	}
	f := recs[5].Following
	if recs[5].Kind != KindFollowing || f == nil || f.Of != "vendor_example" || f.Seen != 42 || !f.Complete ||
		len(f.Kept) != 2 || f.Kept[1] != (Follow{"bean_example", "Prăjitorie Exemplu"}) || f.Filter != CoffeeWordsVersion {
		t.Errorf("line 6 following: %+v", f)
	}
	if recs[6].Wall() != "http 429" || recs[6].Following.Complete || recs[6].Following.Kept[0].FullName != "" {
		t.Errorf("line 7 walled list: %+v", recs[6].Following)
	}
	if recs[7].Wall() != "" || recs[7].Following.Complete {
		t.Errorf("line 8 incomplete list: %+v", recs[7].Following)
	}
	for _, r := range recs {
		if len(r.SHA256) != 32 {
			t.Errorf("line %d: %d-byte hash", r.Line, len(r.SHA256))
		}
	}
}

// TestCaptureMixedTestdata: mixed.jsonl holds every record shape an import meets, all valid.
func TestCaptureMixedTestdata(t *testing.T) {
	recs, errs := readTestdata(t, "mixed.jsonl")
	if len(errs) != 0 {
		t.Fatalf("mixed.jsonl: %v", errs)
	}
	shapes := map[string]int{}
	for _, r := range recs {
		switch {
		case r.Wall() != "":
			shapes[r.Kind+" wall"]++
		case r.Profile != nil:
			shapes["profile "+r.Profile.Outcome]++
		case r.Following.Complete:
			shapes["following complete"]++
		default:
			shapes["following incomplete"]++
		}
	}
	want := map[string]int{"profile ok": 7, "profile private": 1, "profile missing": 1, "profile wall": 1,
		"following complete": 2, "following wall": 1}
	if !maps.Equal(shapes, want) {
		t.Errorf("shapes %v, want %v", shapes, want)
	}
}

// TestCaptureHashIsCanonical: the hash is of the record, not its spacing or key order; any
// change to a value (a new captured_at) is a new capture.
func TestCaptureHashIsCanonical(t *testing.T) {
	rec := profileRec(nil)
	a := line(t, rec)
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetIndent("", " ")
	if err := enc.Encode(rec); err != nil {
		t.Fatal(err)
	}
	spaced := strings.Join(strings.Fields(b.String()), " ") // other spacing, one line
	later := line(t, profileRec(map[string]any{"captured_at": "2026-11-06T09:00:00Z"}))
	recs, errs := parseString(a + "\n" + spaced + "\n" + later + "\n")
	if len(errs) != 0 || len(recs) != 3 {
		t.Fatalf("%d records, %v", len(recs), errs)
	}
	if !bytes.Equal(recs[0].SHA256, recs[1].SHA256) {
		t.Error("the same record with other spacing hashed differently")
	}
	if bytes.Equal(recs[0].SHA256, recs[2].SHA256) {
		t.Error("a recapture hashed the same as the first capture")
	}
	// Key order: a hand-written line with the keys reversed.
	reordered := `{"wall":null,"v":1,"url":"https://www.instagram.com/x.exemplu/","source":{"type":"manual"},"outcome":"missing","kind":"profile","handle":"x.exemplu","captured_at":"2026-11-05T21:00:00Z"}`
	sorted := `{"captured_at":"2026-11-05T21:00:00Z","handle":"x.exemplu","kind":"profile","outcome":"missing","source":{"type":"manual"},"url":"https://www.instagram.com/x.exemplu/","v":1,"wall":null}`
	recs, errs = parseString(reordered + "\n" + sorted)
	if len(errs) != 0 || len(recs) != 2 || !bytes.Equal(recs[0].SHA256, recs[1].SHA256) {
		t.Fatalf("key order changed the hash: %v", errs)
	}
}

// TestCaptureRefusals: every line the writer refuses is refused here, named by its number
// with fixed words; the rest of the file still parses.
func TestCaptureRefusals(t *testing.T) {
	src := func(over map[string]any) map[string]any { return profileRec(map[string]any{"source": over}) }
	kept := func(rows ...any) map[string]any { return followingRec(map[string]any{"kept": rows}) }
	many := make([]any, MaxKept+1)
	for i := range many {
		many[i] = map[string]any{"handle": fmt.Sprintf("cafe_%d", i), "full_name": nil}
	}
	cases := []struct {
		name string
		line string
		want string
	}{
		{"not json", "{not json " + marker, "not one JSON object"},
		{"an array", `["` + marker + `"]`, "not one JSON object"},
		{"two values", line(t, profileRec(nil)) + " {}", "not one JSON object"},
		{"v 2", line(t, profileRec(map[string]any{"v": 2})), "v must be 1"},
		{"v true", line(t, profileRec(map[string]any{"v": true})), "v must be 1"},
		{"no v", strings.Replace(line(t, profileRec(nil)), `"v":1,`, "", 1), "v must be 1"},
		{"kind story", line(t, profileRec(map[string]any{"kind": "story"})), "kind must be"},
		{"unknown key", line(t, profileRec(map[string]any{"likes" + marker: 3})), "unknown key in the profile"},
		{"key in another case", line(t, profileRec(map[string]any{"Handle": "cafe.exemplu"})), "unknown key"},
		{"bad handle", line(t, profileRec(map[string]any{"handle": "Bad Handle! " + marker})), "bad handle in handle"},
		{"instagram page", line(t, profileRec(map[string]any{"handle": "explore", "url": "https://www.instagram.com/explore/"})), "bad handle"},
		{"url not canonical", line(t, profileRec(map[string]any{"url": "https://instagram.com/cafe.exemplu"})), "canonical address"},
		{"no captured_at", line(t, profileRec(map[string]any{"captured_at": nil})), "captured_at must be a time"},
		{"bad captured_at", line(t, profileRec(map[string]any{"captured_at": "yesterday"})), "captured_at must be a time"},
		{"outcome maybe", line(t, profileRec(map[string]any{"outcome": "maybe"})), "outcome must be"},
		{"no outcome", line(t, profileRec(map[string]any{"outcome": nil})), "outcome must be"},
		{"followers text", line(t, profileRec(map[string]any{"followers": "1.2K"})), "followers must be"},
		{"followers bool", line(t, profileRec(map[string]any{"followers": true})), "followers must be"},
		{"followers fraction", line(t, profileRec(map[string]any{"followers": 1.5})), "followers must be"},
		{"posts negative", line(t, profileRec(map[string]any{"posts": -1})), "posts must be"},
		{"is_private text", line(t, profileRec(map[string]any{"is_private": "no"})), "is_private must be"},
		{"captions text", line(t, profileRec(map[string]any{"captions": "one"})), "captions must be"},
		{"captions numbers", line(t, profileRec(map[string]any{"captions": []any{1}})), "captions must be"},
		{"user id letters", line(t, profileRec(map[string]any{"ig_user_id": "12a"})), "ig_user_id must be"},
		{"user id number", line(t, profileRec(map[string]any{"ig_user_id": 12})), "ig_user_id must be"},
		{"script link", line(t, profileRec(map[string]any{"external_url": "javascript:x"})), "external_url must be"},
		{"name number", line(t, profileRec(map[string]any{"full_name": 5})), "full_name must be text"},
		{"private not private", line(t, profileRec(map[string]any{"outcome": "private", "captions": nil})), "is_private true"},
		{"missing with captions", line(t, profileRec(map[string]any{"outcome": "missing"})), "no captions"},
		{"blank wall", line(t, profileRec(map[string]any{"wall": "  "})), "wall must be"},
		{"no source", line(t, profileRec(map[string]any{"source": nil})), "source must be an object"},
		{"source extra", line(t, src(map[string]any{"type": "lead", "extra": 1})), "unknown key in the source"},
		{"source type", line(t, src(map[string]any{"type": "dm"})), "source type"},
		{"following no of", line(t, src(map[string]any{"type": "following"})), "source.of must be a handle"},
		{"following bad of", line(t, src(map[string]any{"type": "following", "of": "Not A Handle"})), "source.of must be a handle"},
		{"of on lead", line(t, src(map[string]any{"type": "lead", "of": "vendor_example"})), "only for a following list"},
		{"lead id on manual", line(t, src(map[string]any{"type": "manual", "lead_id": 4})), "lead_id"},
		{"lead id zero", line(t, src(map[string]any{"type": "lead", "lead_id": 0})), "lead_id"},
		{"lead id text", line(t, src(map[string]any{"type": "lead", "lead_id": "4"})), "lead_id"},
		{"list not coffee", line(t, kept(map[string]any{"handle": "gym_" + "x", "full_name": "Gym " + marker})), "fails coffee-words@1"},
		{"list twice", line(t, kept(map[string]any{"handle": "cafe.x"}, map[string]any{"handle": "cafe.x"})), "twice"},
		{"list extra key", line(t, kept(map[string]any{"handle": "cafe.x", "pk": "1"})), "only handle and full_name"},
		{"list bad handle", line(t, kept(map[string]any{"handle": "cafe x"})), "bad handle in kept"},
		{"list not a list", line(t, followingRec(map[string]any{"kept": "cafe.x"})), "kept must be a list"},
		{"list too long", line(t, kept(many...)), "at most 5000"},
		{"seen below kept", line(t, followingRec(map[string]any{"seen": 0})), "seen must be"},
		{"complete text", line(t, followingRec(map[string]any{"complete": "yes"})), "complete must be"},
		{"no complete", line(t, followingRec(map[string]any{"complete": nil})), "complete must be"},
		{"wall and complete", line(t, followingRec(map[string]any{"wall": "http 429"})), "not complete"},
		{"filter 2", line(t, followingRec(map[string]any{"filter": "coffee-words@2"})), "filter must be"},
		{"list extra", line(t, followingRec(map[string]any{"extra": 1})), "unknown key in the following record"},
		{"list bad owner", line(t, followingRec(map[string]any{"of": "Not A Handle"})), "bad handle in of"},
		{"not utf8", "{\"v\":1,\"kind\":\"profile\",\"bio\":\"\xff\"}", "not UTF-8"},
	}
	good := line(t, profileRec(nil))
	for _, c := range cases {
		recs, errs := parseString("\n" + c.line + "\n" + good)
		if len(errs) != 1 || errs[0].Line != 2 || !strings.Contains(errs[0].Reason, c.want) {
			t.Errorf("%s: errors %v, want line 2 %q", c.name, errs, c.want)
			continue
		}
		if strings.Contains(errs[0].Error(), marker) || strings.Contains(errs[0].Error(), "Not A Handle") {
			t.Errorf("%s: the reason carries the line's text: %q", c.name, errs[0].Error())
		}
		if len(recs) != 1 || recs[0].Line != 3 {
			t.Errorf("%s: the next line did not parse: %+v", c.name, recs)
		}
	}
}

// TestCaptureCaps: long text is cut as the writer cuts it, in characters; spaces are folded,
// a bio keeps its lines; blank captions go and at most 12 stay.
func TestCaptureCaps(t *testing.T) {
	caps := make([]any, 15)
	for i := range caps {
		caps[i] = fmt.Sprintf("c%d ", i) + strings.Repeat("ă", 500)
	}
	caps[0] = "   "
	rec := profileRec(map[string]any{
		"bio": "  " + strings.Repeat("b", 700), "full_name": "a   b\t" + strings.Repeat("n", 300),
		"category": strings.Repeat("c", 150), "external_url": "https://cafe-exemplu.example/" + strings.Repeat("p", 600),
		"captions": caps,
	})
	recs, errs := parseString(line(t, rec))
	if len(errs) != 0 {
		t.Fatal(errs)
	}
	p := recs[0].Profile
	for name, n := range map[string]int{"bio": utf8.RuneCountInString(p.Bio), "name": utf8.RuneCountInString(p.FullName),
		"category": len(p.Category), "url": len(p.ExternalURL), "captions": len(p.Captions)} {
		want := map[string]int{"bio": MaxBio, "name": MaxFullName, "category": MaxCategory, "url": MaxURL, "captions": MaxCaptions}[name]
		if n != want {
			t.Errorf("%s: %d, want %d", name, n, want)
		}
	}
	if !strings.HasPrefix(p.FullName, "a b ") || p.Bio[0] != 'b' || !strings.HasPrefix(p.Captions[0], "c1 ") {
		t.Errorf("folding: name %q, bio %q…, caption %q…", p.FullName[:6], p.Bio[:3], p.Captions[0][:4])
	}
	for _, c := range p.Captions {
		if utf8.RuneCountInString(c) != MaxCaption {
			t.Errorf("caption of %d characters", utf8.RuneCountInString(c))
		}
	}
	walled := followingRec(map[string]any{"wall": strings.Repeat("w ", 300), "complete": false})
	recs, errs = parseString(line(t, walled))
	if len(errs) != 0 || utf8.RuneCountInString(recs[0].Wall()) != MaxWall {
		t.Fatalf("wall cap: %v %d", errs, len(recs[0].Wall()))
	}
}

// TestCaptureLines: blank lines count but are no record; a line past MaxLine is refused and
// the next one read; a last line without its newline, or with \r\n, still parses.
func TestCaptureLines(t *testing.T) {
	good := line(t, profileRec(nil))
	long := `{"bio":"` + strings.Repeat("x", MaxLine) + `"}`
	recs, errs := parseString("\n  \n" + long + "\n" + good + "\r\n" + good)
	if len(errs) != 1 || errs[0].Line != 3 || !strings.Contains(errs[0].Reason, "longer than") {
		t.Fatalf("errors %v", errs)
	}
	if len(recs) != 2 || recs[0].Line != 4 || recs[1].Line != 5 {
		t.Fatalf("records %+v", recs)
	}
	if recs, errs := parseString(""); len(recs) != 0 || len(errs) != 0 {
		t.Fatalf("empty file: %v %v", recs, errs)
	}
}

// failingReader gives some bytes, then a read error.
type failingReader struct{ data []byte }

func (f *failingReader) Read(p []byte) (int, error) {
	if len(f.data) == 0 {
		return 0, fmt.Errorf("disk gone")
	}
	n := copy(p, f.data)
	f.data = f.data[n:]
	return n, nil
}

func TestCaptureReadError(t *testing.T) {
	good := line(t, profileRec(nil))
	recs, errs := Parse(&failingReader{data: []byte(good + "\n" + good[:20])})
	if len(recs) != 1 || len(errs) != 1 || errs[0].Line != 2 || strings.Contains(errs[0].Reason, "disk") {
		t.Fatalf("records %d, errors %v", len(recs), errs)
	}
}
