package store

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestNormName(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Acme Widgets, Inc.", "acme widgets inc"},
		{"  ACME   widgets inc ", "acme widgets inc"},
		{"Acme-Widgets Inc", "acme widgets inc"},
		{"Acme.Widgets...Inc!!", "acme widgets inc"},
		{"Globex", "globex"},
		{"Ünïcode GmbH & Co. KG", "ünïcode gmbh co kg"},
		{"Initech (Remote)", "initech remote"},
		{"Tab\tand\nnewline", "tab and newline"},
		{"42 Labs", "42 labs"},
		{" -- ", ""},
		{"", ""},
	}
	for _, tc := range cases {
		if got := NormName(tc.in); got != tc.want {
			t.Errorf("NormName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestNoDeleteInQueries proves the store needs no DELETE: no hand-written query in this
// package deletes or truncates a row, which is what lets the app role go without both.
// Comment lines and the REVOKE that takes those rights away are not queries.
func TestNoDeleteInQueries(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`(?i)\b(DELETE|TRUNCATE)\b`)
	skip := regexp.MustCompile(`(?m)^\s*//.*$|^.*\bREVOKE\b.*$`)
	checked := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f) // #nosec G304 -- this package's own source files
		if err != nil {
			t.Fatal(err)
		}
		checked++
		b = skip.ReplaceAll(b, nil)
		if loc := re.FindIndex(b); loc != nil {
			t.Errorf("%s: %q: the app role has no DELETE or TRUNCATE", f, b[loc[0]:loc[1]])
		}
	}
	if checked == 0 {
		t.Fatal("no source files checked")
	}
}
