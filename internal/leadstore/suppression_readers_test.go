package leadstore_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// bareSuppressionRE finds a read of the bare suppression table: FROM or JOIN suppression,
// not suppression_active or suppression_lift.
var bareSuppressionRE = regexp.MustCompile(`(?i)\b(FROM|JOIN)\s+(public\.)?suppression(\s|\)|$)`)

// The markers a deliberate read of lifted rows carries on the same line.
const (
	liftedMarkerSQL = "-- reads lifted rows on purpose"
	liftedMarkerGo  = "// reads lifted rows on purpose"
	maxLiftedMarker = 4
)

// TestSuppressionReaders is the FO6 sweep: everything that decides whether an address or a
// domain may be written to reads suppression_active, never the bare table, which still holds
// the lifted rows. A read of the bare table must carry the marker on its line, and only a few
// may (SuppressionByID and SuppressionRows' shared select, OptOutEvents' join), so a future
// decision reader beside them is still caught. Migrations are left out: they define the view.
func TestSuppressionReaders(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	markers := 0
	files := 0
	for _, dir := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if d.Name() == "migrations" || d.Name() == "testdata" {
					return filepath.SkipDir
				}
				return nil
			}
			name := d.Name()
			if strings.HasSuffix(name, "_test.go") || (!strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, ".sql")) {
				return nil
			}
			files++
			b, err := os.ReadFile(path) // #nosec G304 G122 -- paths come from walking the repo's own internal/ and cmd/
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(root, path)
			text := string(b)
			lines := strings.Split(text, "\n")
			// Match over the whole file, so FROM and the table name on two lines are caught;
			// the line judged is the one holding the table name.
			for _, m := range bareSuppressionRE.FindAllStringSubmatchIndex(text, -1) {
				at := m[2] // start of FROM/JOIN
				name := strings.Index(strings.ToLower(text[at:m[1]]), "suppression") + at
				n := strings.Count(text[:name], "\n")
				line := lines[n]
				if strings.Contains(line, liftedMarkerSQL) || strings.Contains(line, liftedMarkerGo) {
					markers++
					continue
				}
				t.Errorf("%s:%d reads the bare suppression table, which still holds lifted rows; read suppression_active (FO6), or mark a deliberate read %q:\n\t%s",
					rel, n+1, liftedMarkerSQL, strings.TrimSpace(line))
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if files < 50 {
		t.Fatalf("swept %d files; the walk is not reaching the source tree", files)
	}
	if markers > maxLiftedMarker {
		t.Errorf("%d reads of the bare suppression table carry the marker, want at most %d: a decision reader must use suppression_active", markers, maxLiftedMarker)
	}
	if markers == 0 {
		t.Error("no marked read found: the sweep no longer sees lift.go")
	}
}
