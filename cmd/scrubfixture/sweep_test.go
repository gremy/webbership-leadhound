package main

import (
	"bytes"
	"context"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// probeEnv names the private directory of raw probe pages. The sweep skips without it.
const probeEnv = "LEADHOUND_PROBE_DIR"

// TestInvariantSweep scrubs every raw probe page and checks that no output holds the
// recruiter card class, a profile link, a denylist entry, or any original job ID, company
// name or company slug. Failures name the file and the rule, never the matched value.
func TestInvariantSweep(t *testing.T) {
	dir := os.Getenv(probeEnv)
	if dir == "" {
		t.Skip(probeEnv + " is not set; the sweep needs the private probe pages")
	}
	deny := loadDenylist(t)
	pages, err := filepath.Glob(filepath.Join(dir, "*.html"))
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(pages)
	s := NewScrubber(detectPhraseLang)
	// The search page goes first, as in generate, so cards claim the first IDs.
	sort.SliceStable(pages, func(i, j int) bool {
		return filepath.Base(pages[i]) == "search.html" && filepath.Base(pages[j]) != "search.html"
	})
	var detailCount, searchCount int
	for _, p := range pages {
		name := filepath.Base(p)
		if name == "view.html" {
			continue // the full logged-in-style page; not a guest endpoint response
		}
		raw, err := os.ReadFile(p) // #nosec G304 G703 -- test reads the operator's probe directory
		if err != nil {
			t.Fatal(err)
		}
		kind := KindDetail
		if name == "search.html" {
			kind = KindSearch
			searchCount++
		} else {
			detailCount++
		}
		out, err := s.Scrub(raw, Options{Kind: kind})
		if err != nil {
			t.Fatalf("%s: scrub failed: %v", name, err)
		}
		orig := CollectOriginals(raw)
		if kind == KindDetail && (len(orig.JobIDs) == 0 || len(orig.Slugs) == 0 || len(orig.Companies) == 0) {
			t.Errorf("%s: collector found %d job IDs, %d slugs, %d names; the sweep would prove nothing", name, len(orig.JobIDs), len(orig.Slugs), len(orig.Companies))
		}
		for _, rule := range Leaks(out, orig) {
			t.Errorf("%s: rule %s", name, rule)
		}
		if hit := denyHit(string(out), deny); hit {
			t.Errorf("%s: rule denylist", name)
		}
		closed, err := NewScrubber(detectPhraseLang).Scrub(raw, Options{Kind: kind, Closed: kind == KindDetail})
		if err != nil {
			t.Fatalf("%s: closed variant failed: %v", name, err)
		}
		for _, rule := range Leaks(closed, orig) {
			t.Errorf("%s (closed variant): rule %s", name, rule)
		}
	}
	if searchCount != 1 || detailCount != 10 {
		t.Errorf("swept %d search and %d detail pages, want 1 and 10", searchCount, detailCount)
	}
}

// rawNumberRe finds the long numbers in a raw page: job IDs, URNs, tracking numbers.
var rawNumberRe = regexp.MustCompile(`\d{7,}`)

// digitRunRe finds every whole digit run in a repository file.
var digitRunRe = regexp.MustCompile(`\d+`)

// skipDirs are the repository directories the repo sweep does not walk: git's own store
// and the ignored build and data output.
var skipDirs = map[string]bool{".git": true, "bin": true, "data": true, "node_modules": true}

// TestRepoSweep walks every text file in the repository and checks that none holds a
// long number, company name or company slug from any raw probe page, or a denylist
// entry. Failures name the file and the rule, never the matched value.
func TestRepoSweep(t *testing.T) {
	dir := os.Getenv(probeEnv)
	if dir == "" {
		t.Skip(probeEnv + " is not set; the sweep needs the private probe pages")
	}
	deny := loadDenylist(t)
	pages, err := filepath.Glob(filepath.Join(dir, "*.html"))
	if err != nil {
		t.Fatal(err)
	}
	nums := map[string]bool{}
	var orig Originals
	for _, p := range pages {
		raw, err := os.ReadFile(p) // #nosec G304 G703 -- test reads the operator's probe directory
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range rawNumberRe.FindAll(raw, -1) {
			nums[string(m)] = true
		}
		o := CollectOriginals(raw)
		orig.Slugs = append(orig.Slugs, o.Slugs...)
		orig.Companies = append(orig.Companies, o.Companies...)
	}
	if len(nums) == 0 || len(orig.Slugs) == 0 || len(orig.Companies) == 0 {
		t.Fatalf("collected %d numbers, %d slugs, %d names; the sweep would prove nothing", len(nums), len(orig.Slugs), len(orig.Companies))
	}
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("repository root not found from the test directory")
	}
	files := 0
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != root && (skipDirs[d.Name()] || (d.Name() == "raw" && filepath.Base(filepath.Dir(path)) == "testdata")) {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		data, err := os.ReadFile(path) // #nosec G304 G122 -- test walks its own repository
		if err != nil {
			return err
		}
		if bytes.IndexByte(data[:min(len(data), 8000)], 0) >= 0 {
			return nil // binary
		}
		files++
		rel, _ := filepath.Rel(root, path)
		if numberHit(data, nums) {
			t.Errorf("%s: rule original-number", rel)
		}
		if containsAny(data, orig.Slugs, true) {
			t.Errorf("%s: rule original-company-slug", rel)
		}
		if containsAny(data, orig.Companies, true) {
			t.Errorf("%s: rule original-company-name", rel)
		}
		if denyHit(string(data), deny) {
			t.Errorf("%s: rule denylist", rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if files < 20 {
		t.Errorf("swept only %d files; the walk missed the repository", files)
	}
}

// numberHit reports whether data holds any of nums as a whole digit run, whatever letters
// sit around it.
func numberHit(data []byte, nums map[string]bool) bool {
	for _, m := range digitRunRe.FindAll(data, -1) {
		if nums[string(m)] {
			return true
		}
	}
	return false
}

// TestGeneratedFixturesAreStable regenerates the fixture set into a temporary directory
// twice and checks the outputs match each other byte for byte.
func TestGeneratedFixturesAreStable(t *testing.T) {
	dir := os.Getenv(probeEnv)
	if dir == "" {
		t.Skip(probeEnv + " is not set")
	}
	a, b := t.TempDir(), t.TempDir()
	var sink strings.Builder
	if err := Generate(dir, a, &sink); err != nil {
		t.Fatal(err)
	}
	if err := Generate(dir, b, &sink); err != nil {
		t.Fatal(err)
	}
	files, _ := filepath.Glob(filepath.Join(a, "*"))
	if len(files) != 12 {
		t.Errorf("generate wrote %d files, want 12", len(files))
	}
	for _, f := range files {
		x, _ := os.ReadFile(f)                                  // #nosec G304 -- test temp dir
		y, _ := os.ReadFile(filepath.Join(b, filepath.Base(f))) // #nosec G304 -- test temp dir
		if string(x) != string(y) {
			t.Errorf("%s: two generations differ", filepath.Base(f))
		}
	}
}

// loadDenylist reads the private denylist the same way privacycheck finds it.
func loadDenylist(t *testing.T) []string {
	t.Helper()
	path := strings.TrimSpace(os.Getenv("LEADHOUND_DENYLIST"))
	if path == "" {
		out, err := exec.CommandContext(context.Background(), "git", "config", "--get", "leadhound.denylist").Output()
		if err == nil {
			path = strings.TrimSpace(string(out))
		}
	}
	if path == "" || path == "none" {
		t.Log("no denylist configured; the denylist rule is not checked")
		return nil
	}
	data, err := os.ReadFile(path) // #nosec G304 G703 -- the operator's own configuration
	if err != nil {
		t.Fatalf("denylist configured but unreadable")
	}
	var out []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") {
			if n := normalizeDeny(line); n != "" {
				out = append(out, n)
			}
		}
	}
	return out
}

func denyHit(s string, deny []string) bool {
	n := normalizeDeny(s)
	for _, d := range deny {
		if strings.Contains(n, d) {
			return true
		}
	}
	return false
}

// normalizeDeny matches privacycheck's normalisation: no diacritics, lower case, and every
// run of whitespace, dots, underscores and hyphens collapsed to one space.
func normalizeDeny(s string) string {
	var b strings.Builder
	sep := false
	for _, r := range norm.NFD.String(s) {
		if unicode.Is(unicode.Mn, r) {
			continue
		}
		if unicode.IsSpace(r) || r == '.' || r == '_' || r == '-' {
			sep = true
			continue
		}
		if sep && b.Len() > 0 {
			b.WriteByte(' ')
		}
		sep = false
		b.WriteRune(unicode.ToLower(r))
	}
	return b.String()
}
