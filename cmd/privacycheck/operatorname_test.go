package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestNoOperatorNameInTrackedFiles keeps the operator's first name out of every tracked
// file: code and docs say "the operator" (CONTRIBUTING.md). The lowercase actor constant
// and the module path are stored values, not prose, so only the capitalised word is
// refused, and only as a whole word (ActorX-style identifiers stay). The word is built
// from parts so this file does not match itself.
func TestNoOperatorNameInTrackedFiles(t *testing.T) {
	word := regexp.MustCompile(`\b` + "Gre" + "my" + `\b`)
	top, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Skip("not in a git checkout")
	}
	root := strings.TrimSpace(string(top))
	ls := exec.Command("git", "ls-files", "-z")
	ls.Dir = root
	out, err := ls.Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	for _, p := range splitZ(out) {
		data, err := os.ReadFile(filepath.Join(root, p)) // #nosec G304 -- paths come from git ls-files
		if err != nil {
			if os.IsNotExist(err) {
				continue // deleted in the working tree, not yet staged
			}
			t.Fatalf("read %s: %v", p, err)
		}
		for i, line := range bytes.Split(data, []byte("\n")) {
			if word.Match(line) {
				t.Errorf("%s:%d: the operator's name; write \"the operator\"", p, i+1)
			}
		}
	}
}
