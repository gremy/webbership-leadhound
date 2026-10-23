package pipeline

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestObserveIsTheOnlyObservationWriter: outside the store's tests, no Go file but this
// package's observe.go calls InsertObservation, so every observation goes through Observe
// and arrives with its gate results.
func TestObserveIsTheOnlyObservationWriter(t *testing.T) {
	root := filepath.Join("..", "..")
	needle := ".Insert" + "Observation(" // split, so this file does not match itself
	var callers []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); name == ".git" || name == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		body, err := os.ReadFile(path) //nolint:gosec // G304: walks this repository's own sources
		if err != nil {
			return err
		}
		if !strings.Contains(string(body), needle) {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		switch {
		case rel == "internal/pipeline/observe.go":
		case strings.HasPrefix(rel, "internal/store/") && strings.HasSuffix(rel, "_test.go"):
		default:
			callers = append(callers, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(callers) > 0 {
		t.Errorf("InsertObservation called outside pipeline.Observe: %v", callers)
	}
}
