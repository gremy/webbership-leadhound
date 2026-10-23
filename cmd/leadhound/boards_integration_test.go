//go:build integration

package main

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/gremy/webbership-leadhound/internal/ats"
	"github.com/gremy/webbership-leadhound/internal/fetch"
	"github.com/gremy/webbership-leadhound/internal/store/storetest"
)

// stubNetwork answers every request a live client makes from the ATS fixtures (a URL not
// in them is a 404), counting them; nothing dials. Live clients have no transport of their
// own, so they use http.DefaultTransport.
func stubNetwork(t *testing.T) *countingTransport {
	t.Helper()
	ft, err := fetch.NewFixtureTransport(atsTestdata)
	if err != nil {
		t.Fatal(err)
	}
	ct := &countingTransport{next: lightGreenhouse{ft}}
	saved := http.DefaultTransport
	http.DefaultTransport = ct
	t.Cleanup(func() { http.DefaultTransport = saved })
	t.Setenv("LEADHOUND_ATS_INTERVAL", "200ms") // the floor
	t.Setenv("LEADHOUND_TZ", "UTC")
	return ct
}

// lightGreenhouse answers boards check's light Greenhouse URL (no content=true) from the
// fixture of the full one, which is the only one the fixtures hold.
type lightGreenhouse struct{ next http.RoundTripper }

func (l lightGreenhouse) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Hostname() == "boards-api.greenhouse.io" && r.URL.RawQuery == "" {
		r = r.Clone(r.Context())
		r.URL.RawQuery = "content=true"
	}
	return l.next.RoundTrip(r)
}

func ownerExecCmd(t *testing.T, q string, args ...any) {
	t.Helper()
	if _, err := storetest.OwnerConn(t).Exec(context.Background(), q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

func boardField(t *testing.T, field, atsName, slug string) string {
	t.Helper()
	var v string
	q := fmt.Sprintf(`SELECT coalesce(%s::text, '') FROM board WHERE ats = $1 AND slug = $2`, field) // field is a test constant
	if err := storetest.OwnerConn(t).QueryRow(context.Background(), q, atsName, slug).Scan(&v); err != nil {
		t.Fatalf("board %s/%s %s: %v", atsName, slug, field, err)
	}
	return v
}

// importDemoBoards puts the demo boards in the registry as seed-list boards in state.
func importDemoBoards(t *testing.T, state string) {
	t.Helper()
	if code, out := runCmd(t, "boards", "import", demoBoardsFile(t), "--origin", "seed-list"); code != 0 {
		t.Fatalf("import: exit %d, %q", code, out)
	}
	if state != "unverified" {
		ownerExecCmd(t, `UPDATE board SET state = $1`, state)
	}
}

// TestBoardsImportCommand: a CSV import reports added, already known and refused rows
// (capped at 20 lines); a second import adds nothing; a YAML import of a known board with
// a stronger origin raises it; --ats refuses the other ATSs' boards.
func TestBoardsImportCommand(t *testing.T) {
	commandDB(t)
	t.Setenv("LEADHOUND_DATA_DIR", "")
	captureNotes(t)
	var csv strings.Builder
	csv.WriteString("name,slug,url\n")
	csv.WriteString("Example Corp,examplecorp,https://boards.greenhouse.io/examplecorp\n")
	csv.WriteString("Beispielwerk,beispielwerk,https://jobs.eu.lever.co/beispielwerk\n")
	for i := range 25 {
		fmt.Fprintf(&csv, "Nobody %d,x%d,https://careers.example/jobs/%d\n", i, i, i)
	}
	path := writeBoards(t, t.TempDir(), "seed.csv", csv.String())

	code, out := runCmd(t, "boards", "import", path, "--origin", "seed-list")
	if code != 0 {
		t.Fatalf("exit %d, %q", code, out)
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if lines[0] != "boards import: added 2 · already known 0 · refused 25" {
		t.Errorf("first line %q", lines[0])
	}
	if len(lines) != 22 || lines[1] != "  line 4: not a supported board host" || lines[21] != "  and 5 more" {
		t.Errorf("refused lines (%d): %q", len(lines), lines)
	}

	code, out = runCmd(t, "boards", "import", path, "--origin", "commoncrawl")
	if code != 0 || !strings.HasPrefix(out, "boards import: added 0 · already known 2 · refused 25\n") {
		t.Errorf("second import: exit %d, %q", code, out)
	}
	if got := boardField(t, "origin", "lever", "beispielwerk"); got != "seed-list" {
		t.Errorf("origin after a weaker re-import = %s", got)
	}

	yaml := writeBoards(t, t.TempDir(), "mine.yaml", "- {ats: greenhouse, board: ExampleCorp}\n- {ats: ashby, board: demolabs, name: Demo Labs}\n")
	code, out = runCmd(t, "boards", "import", yaml, "--origin", "manual", "--ats", "greenhouse")
	if code != 0 || out != "boards import: added 0 · already known 1 · refused 1\n  ashby/demolabs: not a greenhouse board (--ats)\n" {
		t.Errorf("yaml import: exit %d, %q", code, out)
	}
	if got := boardField(t, "origin", "greenhouse", "examplecorp"); got != "manual" {
		t.Errorf("origin after manual import = %s", got)
	}
	if got := dbCount(t, `SELECT count(*) FROM board`); got != 2 {
		t.Errorf("boards = %d, want 2", got)
	}

	for _, args := range [][]string{
		{"boards", "import", path},
		{"boards", "import", path, "--origin", "guess"},
		{"boards", "import", "--origin", "manual"},
		{"boards", "import", path, "--origin", "manual", "--ats", "workday"},
		{"boards", "import", path + ".missing", "--origin", "manual"},
		{"boards", "import", writeBoards(t, t.TempDir(), "bad.yaml", "- {ats: workday, board: x}\n"), "--origin", "manual"},
	} {
		if code, _, _ := runCmdCode(t, args...); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}
}

// TestBoardsListCommand: the count table by ATS and state, filters, and --rows.
func TestBoardsListCommand(t *testing.T) {
	commandDB(t)
	importDemoBoards(t, "unverified")
	ownerExecCmd(t, `UPDATE board SET state = 'ok', postings_last = 4 WHERE ats = 'ashby'`)

	code, out := runCmd(t, "boards", "list")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	fields := map[string][]string{}
	for _, l := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		f := strings.Fields(l)
		fields[f[0]] = f[1:]
	}
	// Columns: unverified ok not_found error retired total.
	for name, want := range map[string]string{
		"ats":        "unverified ok not_found error retired total",
		"greenhouse": "1 0 0 0 0 1",
		"lever":      "2 0 0 0 0 2",
		"ashby":      "0 1 0 0 0 1",
		"total":      "3 1 0 0 0 4",
	} {
		if got := strings.Join(fields[name], " "); got != want {
			t.Errorf("%s row = %q, want %q\n%s", name, got, want, out)
		}
	}

	code, out = runCmd(t, "boards", "list", "--state", "ok", "--rows")
	if code != 0 || !strings.Contains(out, "demolabs") || strings.Contains(out, "examplecorp") {
		t.Errorf("--state ok --rows: exit %d\n%s", code, out)
	}
	if code, _, _ := runCmdCode(t, "boards", "list", "--state", "dead"); code != 2 {
		t.Errorf("bad --state: exit %d", code)
	}
}

// TestBoardsCheckCommand: each board's answer moves its row; a board that misses on a third
// separate day is retired, a manual one only flagged; each ATS writes a check run row and
// the output sums it up.
func TestBoardsCheckCommand(t *testing.T) {
	commandDB(t)
	importDemoBoards(t, "unverified")
	ct := stubNetwork(t)
	t.Setenv("LEADHOUND_LIVE", "ats")
	ghost := writeBoards(t, t.TempDir(), "ghost.yaml", "- {ats: lever, board: ghostboard}\n")
	ghostManual := writeBoards(t, t.TempDir(), "ghostm.yaml", "- {ats: ashby, board: ghostmanual}\n")
	for path, origin := range map[string]string{ghost: "seed-list", ghostManual: "manual"} {
		if code, out := runCmd(t, "boards", "import", path, "--origin", origin); code != 0 {
			t.Fatalf("import: %d %q", code, out)
		}
	}
	// Both ghosts already missed on two earlier days.
	ownerExecCmd(t, `UPDATE board SET state = 'not_found', misses = 2, last_miss_day = (now() AT TIME ZONE 'UTC')::date - 1 WHERE slug LIKE 'ghost%'`)

	code, out := runCmd(t, "boards", "check")
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if ct.n != 6 {
		t.Errorf("requests = %d, want 6", ct.n)
	}
	for _, slug := range []string{"examplecorp", "samplesoft", "beispielwerk", "demolabs"} {
		if st := dbCount(t, `SELECT count(*) FROM board WHERE slug = '`+slug+`' AND state = 'ok' AND checked_at IS NOT NULL AND last_ok_at IS NOT NULL AND postings_last > 0 AND misses = 0`); st != 1 {
			t.Errorf("%s not ok after check", slug)
		}
	}
	if got := boardField(t, "state", "lever", "ghostboard"); got != "retired" {
		t.Errorf("ghost board = %s, want retired", got)
	}
	if got := boardField(t, "state", "ashby", "ghostmanual"); got != "not_found" {
		t.Errorf("manual ghost board = %s, want not_found", got)
	}
	for _, want := range []string{
		"boards check: lever run ",
		"3 boards: 2 ok, 0 not_found, 0 error, 0 no verdict, 1 retired",
		"board states: 1 not_found→retired, 2 unverified→ok",
		"manual board ashby/ghostmanual has missed on 3+ days; kept",
		"boards check: 6 boards · 4 ok · 1 not_found · 0 error · 0 no verdict · 1 retired · 0 not checked · ",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if got := dbCount(t, `SELECT count(*) FROM run WHERE command = 'check' AND finished_at IS NOT NULL AND stop_reason = ''`); got != 3 {
		t.Errorf("check runs = %d, want 3", got)
	}
	if got := dbCount(t, `SELECT count(*) FROM http_fetch`); got != 0 {
		t.Errorf("fetch rows = %d, want 0 (check keeps no bodies)", got)
	}

	// Retired boards are left out unless asked for; --limit and --ats narrow the run.
	ct.n = 0
	code, out = runCmd(t, "boards", "check", "--ats", "lever", "--limit", "1", "--order", "random")
	if code != 0 || ct.n != 1 || !strings.Contains(out, "boards check: 1 boards") {
		t.Errorf("narrow check: exit %d, requests %d\n%s", code, ct.n, out)
	}
	ct.n = 0
	code, out = runCmd(t, "boards", "check", "--state", "error")
	if code != 0 || ct.n != 0 || !strings.Contains(out, "no boards match") {
		t.Errorf("no match: exit %d, requests %d\n%s", code, ct.n, out)
	}
	for _, args := range [][]string{
		{"boards", "check", "extra"},
		{"boards", "check", "--order", "newest"},
		{"boards", "check", "--limit", "-1"},
	} {
		if code, _, _ := runCmdCode(t, args...); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}
}

// TestBoardsCheckGates: live off requests nothing and says so; an empty registry is a
// usage error.
func TestBoardsCheckGates(t *testing.T) {
	commandDB(t)
	ct := stubNetwork(t)
	code, out := runCmd(t, "boards", "check")
	if code != 0 || !strings.Contains(out, "live fetching is off here") {
		t.Errorf("live off: exit %d %q", code, out)
	}
	t.Setenv("LEADHOUND_LIVE", "ats")
	code, _, stderr := runCmdCode(t, "boards", "check")
	if code != 2 || !strings.Contains(stderr, "boards import") {
		t.Errorf("empty registry: exit %d %q", code, stderr)
	}
	if ct.n != 0 {
		t.Errorf("requests = %d, want 0", ct.n)
	}
}

// fetchedURLs is every URL a board fetch row was written for, sorted.
func fetchedURLs(t *testing.T) []string {
	t.Helper()
	rows, err := storetest.OwnerConn(t).Query(context.Background(), `SELECT url FROM http_fetch WHERE kind = 'board'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var u string
		if err := rows.Scan(&u); err != nil {
			t.Fatal(err)
		}
		out = append(out, u)
	}
	sort.Strings(out)
	return out
}

// TestCollectATSReadsRegistryAndFile: a live collect (the network stubbed) reads the
// registry's ok boards and its manual boards that are not retired, plus the board file's
// boards (collected even though the registry lacks them), each once; a registry row that is
// not a valid board is skipped with a note.
func TestCollectATSReadsRegistryAndFile(t *testing.T) {
	commandDB(t)
	t.Setenv("LEADHOUND_DATA_DIR", "")
	n := captureNotes(t)
	importDemoBoards(t, "ok")
	// Passes the table's checks, fails ats.Board.Validate (a slug starts with a letter or digit).
	ownerExecCmd(t, `INSERT INTO board (ats, slug, origin, state) VALUES ('greenhouse', '-broken', 'seed-list', 'ok')`)
	demo := ats.DemoBoards()
	gh, lev, levEU, ashby := demo[0], demo[1], demo[2], demo[3]
	ownerExecCmd(t, `UPDATE board SET origin = 'manual', state = 'not_found' WHERE slug = $1 AND region = ''`, lev.Board)
	ownerExecCmd(t, `UPDATE board SET state = 'not_found' WHERE region = 'eu'`)
	ownerExecCmd(t, `UPDATE board SET state = 'retired', origin = 'manual' WHERE ats = 'ashby'`)
	// The file names the retired ashby board and the greenhouse one again: a file board is
	// collected whatever the registry says, and the greenhouse board only once.
	file := writeBoards(t, t.TempDir(), "boards.yaml", "- {ats: ashby, board: demolabs}\n- {ats: greenhouse, board: ExampleCorp}\n")

	stubNetwork(t)
	t.Setenv("LEADHOUND_LIVE", "ats")
	code, out := runCmd(t, "collect", "ats", "--boards", file)
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	want := []string{ashby.APIURL(), lev.APIURL(), gh.APIURL()}
	sort.Strings(want)
	if got := fetchedURLs(t); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("fetched %v\nwant %v (not %s)", got, want, levEU.APIURL())
	}
	if !strings.Contains(n.String(), "skipped") || !strings.Contains(n.String(), "registry board") {
		t.Errorf("notes %q, want the bad row named", n.String())
	}
}

// TestCollectATSDryRunIgnoresRegistry: a dry run reads only the board file or the demo
// boards, never the registry, and leaves the registry alone.
func TestCollectATSDryRunIgnoresRegistry(t *testing.T) {
	commandDB(t)
	t.Setenv("LEADHOUND_DATA_DIR", "")
	captureNotes(t)
	importDemoBoards(t, "ok")
	ghost := writeBoards(t, t.TempDir(), "ghost.yaml", "- {ats: lever, board: ghostboard}\n")
	if code, out := runCmd(t, "boards", "import", ghost, "--origin", "manual"); code != 0 {
		t.Fatalf("import: %d %q", code, out)
	}
	code, out := runCmd(t, "collect", "ats", "--dry-run", "--fixtures", atsTestdata)
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	var want []string
	for _, b := range ats.DemoBoards() {
		want = append(want, b.APIURL())
	}
	sort.Strings(want)
	if got := fetchedURLs(t); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("fetched %v\nwant the demo boards only %v", got, want)
	}
	file := writeBoards(t, t.TempDir(), "one.yaml", "- {ats: ashby, board: demolabs}\n")
	if code, out := runCmd(t, "collect", "ats", "--dry-run", "--fixtures", atsTestdata, "--boards", file); code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if got := dbCount(t, `SELECT count(*) FROM http_fetch WHERE kind = 'board'`); got != len(want)+1 {
		t.Errorf("board fetches = %d, want %d (the file's one board)", got, len(want)+1)
	}
	if got := dbCount(t, `SELECT count(*) FROM board WHERE checked_at IS NOT NULL`); got != 0 {
		t.Errorf("boards checked by a dry run = %d, want 0", got)
	}
}

// TestCollectATSLiveUpdatesRegistry: a live collect (the network stubbed) updates each
// fetched board's row, applies the profile's title screen and says how many it dropped.
func TestCollectATSLiveUpdatesRegistry(t *testing.T) {
	commandDB(t)
	captureNotes(t)
	// The demo profile, its screen also dropping "data" titles (the fixtures list one).
	t.Setenv("LEADHOUND_DATA_DIR", writeProfile(t, "exclude: [sales,", "exclude: [data, sales,"))
	importDemoBoards(t, "ok")
	stubNetwork(t)
	t.Setenv("LEADHOUND_LIVE", "ats")

	code, out := runCmd(t, "collect", "ats")
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if got := dbCount(t, `SELECT count(*) FROM board WHERE state = 'ok' AND checked_at IS NOT NULL AND postings_last > 0`); got != 4 {
		t.Errorf("boards updated = %d, want 4", got)
	}
	if got := dbCount(t, `SELECT count(*) FROM run WHERE command = 'collect' AND source IN ('greenhouse', 'lever', 'ashby')`); got != 3 {
		t.Errorf("live collect runs = %d, want 3", got)
	}
	// Every listed posting is either stored or screened out, and some were screened out.
	listedTotal := dbCount(t, `SELECT coalesce(sum(postings_last), 0)::int FROM board`)
	stored := dbCount(t, `SELECT count(*) FROM job`)
	screened := 0
	for _, m := range regexp.MustCompile(`(\d+) screened out`).FindAllStringSubmatch(out, -1) {
		n, _ := strconv.Atoi(m[1])
		screened += n
	}
	if stored == 0 || screened == 0 || stored+screened != listedTotal {
		t.Errorf("stored %d + screened %d, want %d listed\n%s", stored, screened, listedTotal, out)
	}
}

// TestCollectATSLiveNoBoardsIsUsage: live, with an empty registry and no board file,
// collect ats is a usage error naming the ways to give boards, and requests nothing.
func TestCollectATSLiveNoBoardsIsUsage(t *testing.T) {
	commandDB(t)
	t.Setenv("LEADHOUND_DATA_DIR", "")
	captureNotes(t)
	ct := stubNetwork(t)
	t.Setenv("LEADHOUND_LIVE", "ats")
	code, _, stderr := runCmdCode(t, "collect", "ats")
	if code != 2 || !strings.Contains(stderr, "boards import") || !strings.Contains(stderr, "--boards FILE") || !strings.Contains(stderr, ats.BoardsFile+" in LEADHOUND_DATA_DIR") {
		t.Errorf("exit %d, stderr %q", code, stderr)
	}
	if ct.n != 0 || dbCount(t, `SELECT count(*) FROM run`) != 0 {
		t.Errorf("requests %d, runs written", ct.n)
	}
}
