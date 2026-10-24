package main

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

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
	pages := jobPages(t, dir)
	s := NewScrubber(detectPhraseLang)
	// The search page goes first, as in generate, so cards claim the first IDs.
	sort.SliceStable(pages, func(i, j int) bool {
		return filepath.Base(pages[i]) == "search.html" && filepath.Base(pages[j]) != "search.html"
	})
	var detailCount, searchCount int
	for _, p := range pages {
		name := filepath.Base(p)
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
		orig := meaningful(CollectOriginals(raw))
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

// TestRepoSweep reads every text file git could commit and checks that none holds a long
// number, company name or company slug from any raw probe page, or a denylist entry.
// Ignored files (.env, build output, raw pages) can never be committed, so they are not
// read. Failures name the file and the rule, never the matched value.
func TestRepoSweep(t *testing.T) {
	dir := os.Getenv(probeEnv)
	if dir == "" {
		t.Skip(probeEnv + " is not set; the sweep needs the private probe pages")
	}
	deny := loadDenylist(t)
	pages := jobPages(t, dir)
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
		o := meaningful(CollectOriginals(raw))
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
	files, findings, err := sweepRepo(root, sweepRules{nums: nums, orig: orig, deny: deny})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range findings {
		t.Error(f)
	}
	if files < 20 {
		t.Errorf("swept only %d files; the walk missed the repository", files)
	}
}

// sweepRules are the original values the repo sweep looks for.
type sweepRules struct {
	nums map[string]bool
	orig Originals
	deny []string
}

// committableFiles lists, relative to root, the files git could commit: tracked files plus
// untracked files that are not ignored. Like privacycheck tree, it asks git rather than
// walking the folder, and it only reads.
func committableFiles(root string) ([]string, error) {
	cmd := exec.CommandContext(context.Background(), "git", "-C", root, "ls-files", "-z", "--cached", "--others", "--exclude-standard") // #nosec G204 -- fixed binary and arguments
	out, err := cmd.Output()
	if err != nil {
		return nil, errors.New("git ls-files failed")
	}
	set := map[string]bool{}
	for _, p := range strings.Split(string(out), "\x00") {
		if p != "" {
			set[p] = true
		}
	}
	return sorted(set), nil
}

// sweepRepo checks every committable regular text file under root against rules and
// returns how many files it read and one "<file>: rule <rule>" finding per hit. A tracked
// file missing from disk, a symlink and a binary file are skipped.
func sweepRepo(root string, rules sweepRules) (files int, findings []string, err error) {
	paths, err := committableFiles(root)
	if err != nil {
		return 0, nil, err
	}
	for _, rel := range paths {
		path := filepath.Join(root, filepath.FromSlash(rel))
		info, err := os.Lstat(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return files, findings, err
		}
		if !info.Mode().IsRegular() {
			continue
		}
		data, err := os.ReadFile(path) // #nosec G304 G122 -- paths come from git ls-files of the test's own repository
		if err != nil {
			return files, findings, err
		}
		if bytes.IndexByte(data[:min(len(data), 8000)], 0) >= 0 {
			continue // binary
		}
		files++
		if numberHit(data, rules.nums) {
			findings = append(findings, rel+": rule original-number")
		}
		if containsAny(data, rules.orig.Slugs, true) {
			findings = append(findings, rel+": rule original-company-slug")
		}
		if containsAny(data, rules.orig.Companies, true) {
			findings = append(findings, rel+": rule original-company-name")
		}
		if denyHit(string(data), rules.deny) {
			findings = append(findings, rel+": rule denylist")
		}
	}
	return files, findings, nil
}

// TestRepoSweepReadsOnlyCommittableFiles plants one original number in a tracked file, an
// untracked file that is not ignored, and an ignored .env in a temporary repository. The
// sweep must report the first two and never read the third.
func TestRepoSweepReadsOnlyCommittableFiles(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	root := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.CommandContext(context.Background(), "git", append([]string{"-C", root}, args...)...) // #nosec G204 -- fixed binary, test temp repository
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v: %s", args[0], err, out)
		}
	}
	git("init", "-q")
	planted := "40" + "91" + "736" // built at runtime, as CONTRIBUTING asks
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(".gitignore", ".env*\n")
	write("tracked.txt", "job "+planted+"\n")
	write(".env", "ID="+planted+"\n")
	git("add", ".gitignore", "tracked.txt")
	write("untracked.txt", "see "+planted+"\n")

	files, findings, err := sweepRepo(root, sweepRules{nums: map[string]bool{planted: true}})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"tracked.txt: rule original-number", "untracked.txt: rule original-number"}
	if strings.Join(findings, "|") != strings.Join(want, "|") {
		t.Errorf("findings = %q, want %q", findings, want)
	}
	if files != 3 {
		t.Errorf("swept %d files, want 3 (.gitignore, tracked.txt, untracked.txt)", files)
	}
}

// jobPageRe names the probe files that are guest job pages: the search page, the single
// detail page and the detail pages saved under their posting ID. Anything else in the probe
// directory (a company page kept for a later checkpoint, the logged-in view.html) is not a
// job page, so it is neither scrubbed as one nor mined for originals.
var jobPageRe = regexp.MustCompile(`^(search|detail|d\d+)\.html$`)

// jobPages returns the job pages the sweeps cover: every page sources.json feeds into the
// fixture set, plus every other file matching jobPageRe, sorted with the search page first
// (as in generate, so its cards claim the first IDs).
func jobPages(t *testing.T, dir string) []string {
	t.Helper()
	sources, err := loadSources(dir)
	if err != nil {
		t.Fatal(err)
	}
	set := map[string]bool{}
	for _, src := range sources {
		set[src] = true
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Type().IsRegular() && jobPageRe.MatchString(e.Name()) {
			set[e.Name()] = true
		}
	}
	names := sorted(set)
	sort.SliceStable(names, func(i, j int) bool {
		return names[i] == "search.html" && names[j] != "search.html"
	})
	pages := make([]string, len(names))
	for i, n := range names {
		pages[i] = filepath.Join(dir, n)
	}
	return pages
}

// minOriginalLen is the shortest company slug or name the substring rules look for. A
// value of one to three characters (a short slug such as an acronym) also occurs as an
// ordinary word or identifier in source, module paths and checksums, so it would raise
// false hits without saying anything about a leak. Job IDs and long numbers are not
// affected: they keep their own 7-or-more-digit rule.
const minOriginalLen = 4

// meaningful drops the slugs and company names shorter than minOriginalLen.
func meaningful(o Originals) Originals {
	keep := func(vals []string) []string {
		var out []string
		for _, v := range vals {
			if utf8.RuneCountInString(strings.TrimSpace(v)) >= minOriginalLen {
				out = append(out, v)
			}
		}
		return out
	}
	o.Slugs, o.Companies = keep(o.Slugs), keep(o.Companies)
	return o
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
