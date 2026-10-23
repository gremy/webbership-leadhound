// Command privacycheck keeps personal data out of this public repository.
//
// It checks content against a private denylist (kept outside the repo) and a set of
// generic patterns: email addresses, LinkedIn profile URLs, public IPv4 addresses and the
// recruiter card class in test fixtures. Denylist matching ignores case, diacritics and
// separators. Binary content is scanned through its readable text runs (UTF-8 and UTF-16),
// and document, archive, dump, mail and contact files are blocked by extension. A finding
// prints as "file:line: rule" ("file:binary: rule" for binary content) and never echoes
// the matched text, because hook output can land in logs. Errors never print paths or
// git's own error text either.
//
// Modes:
//
//	privacycheck staged        added lines and added file names in the index (pre-commit)
//	privacycheck msg <file>    every line of a commit message file (commit-msg)
//	privacycheck tree          every tracked file
//	privacycheck history       every commit's and annotated tag's added content, message and
//	                           identity (name and email)
//	privacycheck files <path>  the named files or directories, tracked or not
//
// The denylist path comes from LEADHOUND_DENYLIST, else from the repo-local git config key
// leadhound.denylist; without either it fails closed. LEADHOUND_DENYLIST=none runs the
// generic checks only, with an empty allowlist (for outside contributors). allow.txt,
// beside the denylist, lists what history mode accepts in identity headers: a plain line is
// an email accepted besides noreply, and a "name:<Full Name>" line exempts that exact name
// from the denylist check of author, committer and tagger names. allow.txt never exempts
// content (files or messages).
//
// Exit codes: 0 clean, 1 findings or failure, 2 usage.
package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const (
	envDenylist = "LEADHOUND_DENYLIST"
	gitKey      = "leadhound.denylist"
	hiddenName  = "(file name hidden)"
	usage       = "usage: privacycheck staged | msg <file> | tree | history | files <path>..."
	fixHint     = "set it once with: git config " + gitKey + " <absolute path to denylist.txt> (or " + envDenylist + "=none for the generic checks only)"
)

// ErrNoDenylist means no denylist is configured or the configured one is unreadable.
var ErrNoDenylist = errors.New("no denylist")

func main() {
	os.Exit(run(os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
}

func run(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprintln(stderr, usage)
		return 2
	}
	mode := args[0]
	switch {
	case mode == "msg" && len(args) == 2,
		mode == "files" && len(args) >= 2,
		(mode == "staged" || mode == "tree" || mode == "history") && len(args) == 1:
	default:
		_, _ = fmt.Fprintln(stderr, usage)
		return 2
	}

	c, err := loadChecker(getenv)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "privacycheck: %v\n", err)
		return 1
	}
	r := &reporter{c: c, w: stdout}
	switch mode {
	case "staged":
		err = r.staged()
	case "msg":
		err = r.msg(args[1])
	case "tree":
		err = r.tree()
	case "history":
		err = r.history()
	case "files":
		err = r.files(args[1:])
	}
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "privacycheck: %v\n", err)
		return 1
	}
	if r.count > 0 {
		_, _ = fmt.Fprintf(stderr, "privacycheck: %d finding(s); the matched text is never shown. Remove it and run again.\n", r.count)
		return 1
	}
	return 0
}

// loadChecker resolves the denylist: env, then repo-local git config, else fail closed.
func loadChecker(getenv func(string) string) (*checker, error) {
	path := strings.TrimSpace(getenv(envDenylist))
	if path == "none" {
		return newChecker(nil, nil), nil
	}
	if path == "" {
		// Exit status 1 just means the key is unset.
		out, _ := gitOutput("config", "--get", gitKey)
		path = strings.TrimSpace(string(out))
	}
	if path == "" {
		return nil, fmt.Errorf("%w configured; %s", ErrNoDenylist, fixHint)
	}
	deny, err := readList(path)
	if err != nil {
		// The OS error names the path, and the path must never be printed.
		return nil, fmt.Errorf("%w: configured denylist unreadable; %s", ErrNoDenylist, fixHint)
	}
	if len(deny) == 0 {
		return nil, fmt.Errorf("%w: the configured denylist has no entries; %s", ErrNoDenylist, fixHint)
	}
	allow := map[string]bool{}
	entries, err := readList(filepath.Join(filepath.Dir(path), "allow.txt"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, errors.New("allow.txt beside the configured denylist unreadable")
	}
	for _, a := range entries {
		allow[a] = true
	}
	return newChecker(deny, allow), nil
}

// readList returns the non-blank, non-comment lines of a list file.
func readList(path string) ([]string, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- the path is the operator's own configuration
	if err != nil {
		return nil, err
	}
	var out []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") {
			out = append(out, line)
		}
	}
	return out, nil
}

// gitOutput runs git and returns stdout. On failure the error names only the subcommand:
// git's own message can carry paths and file names.
func gitOutput(args ...string) ([]byte, error) {
	cmd := exec.CommandContext(context.Background(), "git", args...) // #nosec G204 -- fixed binary, arguments built here
	cmd.Stderr = io.Discard
	out, err := cmd.Output()
	if err != nil {
		return out, fmt.Errorf("git command failed (%s)", gitSubcommand(args))
	}
	return out, nil
}

// gitSubcommand skips global options ("-c key=value", "--literal-pathspecs").
func gitSubcommand(args []string) string {
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "-c":
			i++
		case strings.HasPrefix(args[i], "-"):
		default:
			return args[i]
		}
	}
	return "?"
}

// reporter prints findings as "<prefix><file>:<line>: <rule>" and counts them.
type reporter struct {
	c     *checker
	w     io.Writer
	count int
}

func (r *reporter) report(loc, rule string) {
	r.count++
	_, _ = fmt.Fprintf(r.w, "%s: %s\n", loc, rule)
}

// display returns the path, or a placeholder when the path itself trips a rule.
func (r *reporter) display(path string) string {
	if len(r.c.scanName(path)) > 0 {
		return hiddenName
	}
	return path
}

func (r *reporter) checkName(prefix, path string) {
	for _, rule := range r.c.scanName(path) {
		r.report(fmt.Sprintf("%s%s:0", prefix, hiddenName), rule)
	}
}

func (r *reporter) checkLine(prefix, path string, n int, line string) {
	for _, rule := range r.c.scanLine(path, line) {
		r.report(fmt.Sprintf("%s%s:%d", prefix, r.display(path), n), rule)
	}
}

// checkExt blocks document, archive, dump, mail and contact files by name alone.
func (r *reporter) checkExt(prefix, path string) {
	if hasBlockedExtension(path) {
		r.report(fmt.Sprintf("%s%s:0", prefix, r.display(path)), ruleBinaryDocument)
	}
}

// checkContent scans a whole blob line by line. Binary content (a NUL in the first 8000
// bytes, git's own test) is scanned through its readable text runs instead.
func (r *reporter) checkContent(prefix, path string, data []byte) {
	if isBinary(data) {
		r.checkBinary(prefix, path, data)
		return
	}
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64*1024), len(data)+1)
	for n := 1; sc.Scan(); n++ {
		r.checkLine(prefix, path, n, sc.Text())
	}
}

// checkBinary scans the readable text runs of binary content and reports each rule once.
func (r *reporter) checkBinary(prefix, path string, data []byte) {
	seen := map[string]bool{}
	for _, run := range textRuns(data) {
		for _, rule := range r.c.scanLine(path, run) {
			if !seen[rule] {
				seen[rule] = true
				r.report(fmt.Sprintf("%s%s:binary", prefix, r.display(path)), rule)
			}
		}
	}
}

func (r *reporter) staged() error {
	raw, err := gitOutput(append([]string{"diff", "--cached"}, rawFlags...)...)
	if err != nil {
		return err
	}
	return r.scanChanges("", parseRaw(raw), func(path string) ([]byte, error) {
		return gitOutput(append(append([]string{"--literal-pathspecs", "diff", "--cached"}, diffFlags...), "--", path)...)
	})
}

func (r *reporter) msg(file string) error {
	data, err := os.ReadFile(file) // #nosec G304 G703 -- git hands us the message file
	if err != nil {
		return errors.New("cannot read the commit message file")
	}
	// Every line, comments and the text below a scissors line included: a cleanup mode or
	// a hook can keep them, and a check that guesses wrong leaks.
	for n, line := range strings.Split(string(data), "\n") {
		for _, rule := range r.c.scanLine("", line) {
			r.report(fmt.Sprintf("commit message:%d", n+1), rule)
		}
	}
	return nil
}

func (r *reporter) tree() error {
	out, err := gitOutput("ls-files", "-z")
	if err != nil {
		return err
	}
	return r.scanPaths(splitZ(out))
}

func (r *reporter) files(paths []string) error {
	var all []string
	for _, p := range paths {
		// #nosec G703 -- the caller names what to scan on purpose
		err := filepath.WalkDir(p, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return errors.New("cannot walk a named path")
			}
			if d.IsDir() && d.Name() == ".git" {
				return filepath.SkipDir
			}
			if !d.IsDir() {
				all = append(all, path)
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	return r.scanPaths(all)
}

// scanPaths checks names and working-tree content; a symlink is checked by its target
// text, never followed, and a tracked file missing from disk is skipped.
func (r *reporter) scanPaths(paths []string) error {
	for _, p := range paths {
		r.checkName("", p)
		r.checkExt("", p)
		info, err := os.Lstat(p)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("cannot stat %s", r.display(p))
		}
		switch {
		case info.Mode()&fs.ModeSymlink != 0:
			target, err := os.Readlink(p)
			if err != nil {
				return fmt.Errorf("cannot read link %s", r.display(p))
			}
			r.checkLine("", p, 1, target)
		case info.Mode().IsRegular():
			data, err := os.ReadFile(p) // #nosec G304 -- paths come from git ls-files or the caller
			if err != nil {
				return fmt.Errorf("cannot read %s", r.display(p))
			}
			r.checkContent("", p, data)
		}
	}
	return nil
}

func splitZ(b []byte) []string {
	var out []string
	for _, s := range strings.Split(string(b), "\x00") {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}
