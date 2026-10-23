package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/time/rate"

	"github.com/gremy/webbership-leadhound/internal/ats"
	"github.com/gremy/webbership-leadhound/internal/config"
	"github.com/gremy/webbership-leadhound/internal/fetch"
	"github.com/gremy/webbership-leadhound/internal/store"
)

const atsTestdata = "../../internal/ats/testdata"

// runOut runs args and returns the exit code, stdout and stderr.
func runOut(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	out := captureStdout(t)
	captureNotes(t)
	var stderr bytes.Buffer
	code := run(context.Background(), args, &stderr)
	return code, out.String(), stderr.String()
}

// noDB points the database at a closed port, so a command that opened it would fail.
func noDB(t *testing.T) {
	t.Helper()
	t.Setenv("LEADHOUND_DATABASE_URL", "postgres://nobody@127.0.0.1:1/none?sslmode=disable&connect_timeout=1")
	t.Setenv("LEADHOUND_DATA_DIR", "")
}

// atsFixtureDir copies the day-one ATS fixtures into a temp dir and sets file bodies from
// bodies (a URL in bodies gets its own file with that body).
func atsFixtureDir(t *testing.T, bodies map[string]string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(atsTestdata, fetch.ManifestFile))
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]string{}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	for u, file := range m {
		b, err := os.ReadFile(filepath.Join(atsTestdata, file)) // #nosec G304 -- the test's own testdata
		if err != nil {
			t.Fatal(err)
		}
		if body, ok := bodies[u]; ok {
			b = []byte(body)
		}
		if err := os.WriteFile(filepath.Join(dir, file), b, 0o600); err != nil { // #nosec G703 -- a test temp dir
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, fetch.ManifestFile), raw, 0o600); err != nil { // #nosec G703 -- a test temp dir
		t.Fatal(err)
	}
	return dir
}

func demoBoard(t *testing.T, atsName string) ats.Board {
	t.Helper()
	for _, b := range ats.DemoBoards() {
		if b.ATS == atsName {
			return b
		}
	}
	t.Fatalf("no demo %s board", atsName)
	return ats.Board{}
}

func TestBoardsVerifyOutput(t *testing.T) {
	noDB(t)
	t.Setenv("LEADHOUND_LIVE", "")
	boards := ats.DemoBoards()

	t.Run("all ok", func(t *testing.T) {
		code, out, stderr := runOut(t, "boards", "verify", "--dry-run", "--fixtures", atsTestdata)
		if code != 0 {
			t.Fatalf("exit %d, stderr %s", code, stderr)
		}
		lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
		if len(lines) != len(boards) {
			t.Fatalf("lines = %q, want one per board", lines)
		}
		for i, b := range boards {
			if !strings.HasPrefix(lines[i], "ok  "+b.String()+"  ") || !strings.HasSuffix(lines[i], " postings") {
				t.Errorf("line %d = %q, want ok for %s", i, lines[i], b)
			}
		}
	})

	t.Run("404, broken and ok", func(t *testing.T) {
		gh, ashby := demoBoard(t, ats.Greenhouse), demoBoard(t, ats.Ashby)
		dir := atsFixtureDir(t, map[string]string{gh.APIURL(): "not json"})
		// Drop the Ashby board from the manifest: it answers 404.
		raw, err := os.ReadFile(filepath.Join(dir, fetch.ManifestFile)) // #nosec G304 -- a test temp dir
		if err != nil {
			t.Fatal(err)
		}
		m := map[string]string{}
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatal(err)
		}
		delete(m, ashby.APIURL())
		if raw, err = json.Marshal(m); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, fetch.ManifestFile), raw, 0o600); err != nil { // #nosec G703 -- a test temp dir
			t.Fatal(err)
		}

		code, out, _ := runOut(t, "boards", "verify", "--dry-run", "--fixtures", dir)
		if code != 1 {
			t.Errorf("exit %d, want 1", code)
		}
		for _, want := range []string{
			"error " + gh.String() + "  parser broke\n",
			"404 " + ashby.String() + "  not found\n",
			"ok  " + demoBoard(t, ats.Lever).String() + "  ",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("stdout %q, want %q", out, want)
			}
		}
	})

	t.Run("zero postings", func(t *testing.T) {
		gh := demoBoard(t, ats.Greenhouse)
		dir := atsFixtureDir(t, map[string]string{gh.APIURL(): `{"jobs": [], "meta": {"total": 0}}`})
		code, out, _ := runOut(t, "boards", "verify", "--dry-run", "--fixtures", dir)
		if code != 0 || !strings.Contains(out, "ok  "+gh.String()+"  0 postings (check the slug)\n") {
			t.Errorf("exit %d, stdout %q", code, out)
		}
	})

	t.Run("live off", func(t *testing.T) {
		code, out, _ := runOut(t, "boards", "verify", "--boards", demoBoardsFile(t))
		if code != 0 || !strings.Contains(out, "boards verify: live fetching is off here (LEADHOUND_LIVE is off); nothing requested") {
			t.Errorf("exit %d, stdout %q", code, out)
		}
	})

	t.Run("usage", func(t *testing.T) {
		for _, args := range [][]string{
			{"boards"},
			{"boards", "add"},
			{"boards", "verify", "extra"},
			{"boards", "verify", "--dry-run"},
			{"boards", "verify", "--fixtures", "x"},
			{"boards", "verify", "--boards", filepath.Join(t.TempDir(), "missing.yaml")},
		} {
			if code, _, stderr := runOut(t, args...); code != 2 {
				t.Errorf("%v: exit %d, want 2; stderr %s", args, code, stderr)
			}
		}
	})
}

// demoBoardsFile writes the fictional demo boards as a board list and returns its path.
func demoBoardsFile(t *testing.T) string {
	t.Helper()
	var b strings.Builder
	for _, d := range ats.DemoBoards() {
		b.WriteString("- {ats: " + d.ATS + ", board: " + d.Board)
		if d.Region != "" {
			b.WriteString(", region: " + d.Region)
		}
		b.WriteString("}\n")
	}
	return writeBoards(t, t.TempDir(), "boards.yaml", b.String())
}

// TestCollectATSLiveOff: without LEADHOUND_LIVE allowing the ATS hosts, collect ats says
// so and exits 0 before it touches the database (which points at a closed port). With no
// data directory its note names only the gates: collect ats has no search.
func TestCollectATSLiveOff(t *testing.T) {
	noDB(t)
	t.Setenv("LEADHOUND_LIVE", "")
	out := captureStdout(t)
	n := captureNotes(t)
	var stderr bytes.Buffer
	if code := run(context.Background(), []string{"collect", "ats", "--boards", demoBoardsFile(t)}, &stderr); code != 0 {
		t.Fatalf("exit %d, stderr %s", code, stderr.String())
	}
	if out.String() != "collect ats: live fetching is off here (LEADHOUND_LIVE is off); nothing requested\n" {
		t.Errorf("stdout %q", out.String())
	}
	if n.String() != "collect ats: LEADHOUND_DATA_DIR is not set; using neutral gates\n" {
		t.Errorf("notes %q", n.String())
	}
}

// TestCollectLinkedInNoteUnchanged: collect linkedin's data-dir note keeps its wording.
func TestCollectLinkedInNoteUnchanged(t *testing.T) {
	noDB(t)
	t.Setenv("LEADHOUND_LIVE", "")
	captureStdout(t)
	n := captureNotes(t)
	var stderr bytes.Buffer
	if code := run(context.Background(), []string{"collect", "linkedin"}, &stderr); code != 0 {
		t.Fatalf("exit %d, stderr %s", code, stderr.String())
	}
	if n.String() != "collect linkedin: LEADHOUND_DATA_DIR is not set; using the demo search and neutral gates\n" {
		t.Errorf("notes %q", n.String())
	}
}

// countingTransport counts the requests that reach it.
type countingTransport struct {
	next http.RoundTripper
	mu   sync.Mutex
	n    int
}

func (c *countingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	c.mu.Lock()
	c.n++
	c.mu.Unlock()
	return c.next.RoundTrip(r)
}

// TestLiveWithoutBoardListIsUsage: live, with neither --boards nor a list in the data
// directory, boards verify is a usage error naming both ways to give a list, and no request
// goes out. collect ats also reads the board registry, so its twin needs the database
// (TestCollectATSLiveNoBoardsIsUsage).
func TestLiveWithoutBoardListIsUsage(t *testing.T) {
	noDB(t)
	t.Setenv("LEADHOUND_LIVE", "ats")
	ct := &countingTransport{next: http.DefaultTransport}
	saved := http.DefaultTransport
	http.DefaultTransport = ct
	t.Cleanup(func() { http.DefaultTransport = saved })
	for _, dataDir := range []string{"", t.TempDir()} {
		t.Setenv("LEADHOUND_DATA_DIR", dataDir)
		for _, args := range [][]string{{"boards", "verify"}} {
			code, out, stderr := runOut(t, args...)
			if code != 2 || !strings.Contains(stderr, "--boards FILE") || !strings.Contains(stderr, ats.BoardsFile+" in LEADHOUND_DATA_DIR") {
				t.Errorf("data dir %q %v: exit %d, stdout %q, stderr %q", dataDir, args, code, out, stderr)
			}
		}
	}
	if ct.n != 0 {
		t.Errorf("requests = %d, want 0", ct.n)
	}
}

func TestCollectATSUsage(t *testing.T) {
	noDB(t)
	t.Setenv("LEADHOUND_LIVE", "")
	for _, args := range [][]string{
		{"collect", "ats", "extra"},
		{"collect", "ats", "--dry-run"},
		{"collect", "ats", "--fixtures", "x"},
		{"collect", "ats", "--keyword", "go"},
		{"collect", "ats", "--boards", filepath.Join(t.TempDir(), "missing.yaml")},
	} {
		if code, _, stderr := runOut(t, args...); code != 2 {
			t.Errorf("%v: exit %d, want 2; stderr %s", args, code, stderr)
		}
	}
}

func writeBoards(t *testing.T, dir, file, body string) string {
	t.Helper()
	p := filepath.Join(dir, file)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil { // #nosec G703 -- a test temp dir
		t.Fatal(err)
	}
	return p
}

// TestBoardListPrecedence: --boards wins over LEADHOUND_DATA_DIR, which wins over the demo
// boards; the demo comes with one note.
func TestBoardListPrecedence(t *testing.T) {
	dataDir := t.TempDir()
	writeBoards(t, dataDir, ats.BoardsFile, "- {ats: lever, board: datadir-board}\n")
	flagFile := writeBoards(t, t.TempDir(), "mine.yaml", "- {ats: ashby, board: flag-board}\n")
	names := func(bs []ats.Board) []string {
		var out []string
		for _, b := range bs {
			out = append(out, b.String())
		}
		return out
	}

	for _, tt := range []struct {
		name    string
		dataDir string
		file    string
		dryRun  bool
		want    []string // nil: a usage error
		note    string
	}{
		{"flag wins", dataDir, flagFile, false, []string{"ashby/flag-board"}, ""},
		{"data dir", dataDir, "", false, []string{"lever/datadir-board"}, ""},
		{"flag wins in a dry run", dataDir, flagFile, true, []string{"ashby/flag-board"}, ""},
		{"no data dir, dry run", "", "", true, names(ats.DemoBoards()), "LEADHOUND_DATA_DIR is not set; using the demo boards"},
		{"data dir without a list, dry run", t.TempDir(), "", true, names(ats.DemoBoards()), "no " + ats.BoardsFile + " in LEADHOUND_DATA_DIR"},
		{"no data dir, live", "", "", false, nil, ""},
		{"data dir without a list, live", t.TempDir(), "", false, nil, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			n := captureNotes(t)
			got, err := boardList(config.Config{DataDir: tt.dataDir}, tt.file, "test", tt.dryRun)
			if tt.want == nil {
				if !errors.Is(err, errUsage) || got != nil || n.Len() != 0 {
					t.Errorf("boards %v, err %v, notes %q; want a usage error and no note", got, err, n.String())
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(names(got), tt.want) {
				t.Errorf("boards = %v, want %v", names(got), tt.want)
			}
			if tt.note == "" && n.Len() != 0 || tt.note != "" && (strings.Count(n.String(), "\n") != 1 || !strings.Contains(n.String(), tt.note)) {
				t.Errorf("notes %q, want %q", n.String(), tt.note)
			}
		})
	}

	t.Run("broken list", func(t *testing.T) {
		bad := t.TempDir()
		writeBoards(t, bad, ats.BoardsFile, "- {ats: workday, board: x}\n")
		_, err := boardList(config.Config{DataDir: bad}, "", "test", false)
		if !errors.Is(err, errUsage) || !strings.Contains(err.Error(), "workday") {
			t.Errorf("err = %v, want a usage error naming the problem", err)
		}
	})
}

// TestLiveAllowsBoards: LEADHOUND_LIVE=ats allows every ATS API host, the EU Lever one
// included; off allows none.
func TestLiveAllowsBoards(t *testing.T) {
	boards := ats.DemoBoards()
	if !liveAllowsBoards(config.LiveATS, boards) || !liveAllowsBoards(config.LiveAll, boards) {
		t.Error("ats or all refused a demo board host")
	}
	if liveAllowsBoards(config.LiveOff, boards) {
		t.Error("off allowed a board host")
	}
	if n := boardsPerATS(append(boards, boards[0])); n[ats.Lever] != 2 || n[ats.Greenhouse] != 1 || n[ats.Ashby] != 1 {
		t.Errorf("boardsPerATS = %v", n)
	}
}

// stubAnswer answers every request with fn's status and body; fn may block on the
// request's context to stand for a slow board.
type stubAnswer func(r *http.Request) (*http.Response, error)

func (f stubAnswer) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func answer(r *http.Request, status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{}, Request: r,
		Body: io.NopCloser(strings.NewReader(body)), ContentLength: int64(len(body))}
}

// TestCheckBoardOutcomes: a real client against stub answers. A 5xx is a miss; a body over
// the limit and a timeout say nothing (no verdict); a 404 is not found; Greenhouse is
// asked at its light URL.
func TestCheckBoardOutcomes(t *testing.T) {
	gh := ats.Board{ATS: ats.Greenhouse, Board: "examplecorp"}
	var asked []string
	var mu sync.Mutex
	for _, tc := range []struct {
		name    string
		fn      func(r *http.Request) (*http.Response, error)
		outcome string
	}{
		{"ok", func(r *http.Request) (*http.Response, error) {
			return answer(r, 200, `{"jobs": [{"id": 1, "title": "Go Engineer", "absolute_url": "https://jobs.example/1"}]}`), nil
		}, store.CheckOK},
		{"404", func(r *http.Request) (*http.Response, error) { return answer(r, 404, ""), nil }, store.CheckNotFound},
		{"5xx", func(r *http.Request) (*http.Response, error) { return answer(r, 502, "bad gateway"), nil }, store.CheckError},
		{"unreadable", func(r *http.Request) (*http.Response, error) { return answer(r, 200, "<html>"), nil }, store.CheckError},
		{"too large", func(r *http.Request) (*http.Response, error) {
			return answer(r, 200, `{"jobs": [`+strings.Repeat(" ", 2048)+`]}`), nil
		}, store.CheckNoVerdict},
		{"timeout", func(r *http.Request) (*http.Response, error) {
			<-r.Context().Done()
			return nil, r.Context().Err()
		}, store.CheckNoVerdict},
		{"network error", func(*http.Request) (*http.Response, error) { return nil, errors.New("connection reset") }, store.CheckNoVerdict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := fetch.New(
				fetch.WithTransport(stubAnswer(func(r *http.Request) (*http.Response, error) {
					mu.Lock()
					asked = append(asked, r.URL.String())
					mu.Unlock()
					return tc.fn(r)
				})),
				fetch.WithLiveGate(config.LiveATS),
				fetch.WithRecorder(verifyRecorder{}),
				fetch.WithMaxBody(1024),
				fetch.WithLimiter(rate.NewLimiter(rate.Inf, 1)),
				fetch.WithJitter(0),
			)
			c.HTTP.Timeout = 50 * time.Millisecond
			outcome, _, stop, err := checkBoard(context.Background(), c, gh)
			if err != nil || stop != "" || outcome != tc.outcome {
				t.Errorf("= %q, stop %q, err %v; want %q", outcome, stop, err, tc.outcome)
			}
		})
	}
	for _, u := range asked {
		if u != gh.CheckURL() {
			t.Errorf("asked %s, want the light URL %s", u, gh.CheckURL())
		}
	}
}

// TestRegistryBoardsSkipsBadRows: a registry row that is not a valid board is skipped with
// a note naming it, and the rest are kept.
func TestRegistryBoardsSkipsBadRows(t *testing.T) {
	n := captureNotes(t)
	got, skipped := registryBoards("collect ats", []store.Board{
		{ID: 1, ATS: ats.Greenhouse, Slug: "examplecorp"},
		{ID: 2, ATS: ats.Greenhouse, Slug: "-bad"},
		{ID: 3, ATS: ats.Greenhouse, Slug: "regional", Region: ats.RegionEU},
		{ID: 4, ATS: ats.Lever, Slug: "samplesoft", Region: ats.RegionEU, Name: "SampleSoft"},
	})
	if skipped != 2 || len(got) != 2 || got[1].Region != ats.RegionEU || got[1].Name != "SampleSoft" {
		t.Errorf("boards %v, skipped %d", got, skipped)
	}
	for _, id := range []string{"registry board 2 skipped", "registry board 3 skipped"} {
		if !strings.Contains(n.String(), id) {
			t.Errorf("notes %q lack %q", n.String(), id)
		}
	}
}

// TestMergeBoardsIgnoresCase: a file board that the registry holds in another spelling is
// the same board; the registry's spelling is the one collected.
func TestMergeBoardsIgnoresCase(t *testing.T) {
	reg := []ats.Board{{ATS: ats.Lever, Board: "MixedCase"}}
	file := []ats.Board{{ATS: ats.Lever, Board: "mixedcase", Name: "Mixed"}, {ATS: ats.Lever, Board: "mixedcase", Region: ats.RegionEU}}
	got := mergeBoards(reg, file)
	if len(got) != 2 || got[0].Board != "MixedCase" || got[0].Name != "Mixed" || got[1].Region != ats.RegionEU {
		t.Errorf("merged %+v", got)
	}
}
