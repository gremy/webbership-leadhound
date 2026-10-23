package main

import (
	"bufio"
	"bytes"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// rawFlags list changed files NUL-separated with full blob ids, so a path is read exactly
// as git stores it (spaces, " and ", quotes and all) instead of parsed out of a patch header.
var rawFlags = []string{"--raw", "-z", "--no-renames", "--no-abbrev"}

// diffFlags give a zero-context patch of one text file with no external diff or textconv
// drivers. -a forces a text patch even when an attribute calls the file binary: the caller
// has already checked the content holds no NUL.
var diffFlags = []string{"-U0", "--no-color", "--no-ext-diff", "--no-textconv", "--no-renames", "-a"}

var hunkRe = regexp.MustCompile(`^@@+ [^@]*\+(\d+)`)

// change is one entry of `git diff --raw -z`.
type change struct {
	mode, blob, status, path string
}

// parseRaw reads ":oldmode newmode oldsha newsha status\0path\0" records.
func parseRaw(b []byte) []change {
	parts := strings.Split(string(b), "\x00")
	var out []change
	for i := 0; i+1 < len(parts); i += 2 {
		f := strings.Fields(strings.TrimPrefix(parts[i], ":"))
		if len(f) < 5 {
			continue
		}
		out = append(out, change{mode: f[1], blob: f[3], status: f[4][:1], path: parts[i+1]})
	}
	return out
}

// scanChanges checks each changed file: an added file's name, the extension of every file
// still present, and its content. An added text file is scanned whole; a modified one only
// through its added lines (patch returns its zero-context diff). Binary content, which git
// may also report for a text file through a -diff attribute, is judged by the blob itself.
func (r *reporter) scanChanges(prefix string, changes []change, patch func(path string) ([]byte, error)) error {
	for _, ch := range changes {
		if ch.status == "D" {
			continue
		}
		if ch.status == "A" {
			r.checkName(prefix, ch.path)
		}
		r.checkExt(prefix, ch.path)
		if ch.mode == "160000" || strings.Trim(ch.blob, "0") == "" { // submodule, or no blob
			continue
		}
		data, err := gitOutput("cat-file", "blob", ch.blob)
		if err != nil {
			return err
		}
		if ch.status == "A" || isBinary(data) {
			r.checkContent(prefix, ch.path, data)
			continue
		}
		p, err := patch(ch.path)
		if err != nil {
			return err
		}
		r.walkHunks(prefix, ch.path, p)
	}
	return nil
}

// walkHunks reports the added lines of a one-file patch. Header lines are skipped: the path
// comes from the raw listing, never from the patch.
func (r *reporter) walkHunks(prefix, path string, patch []byte) {
	inHunk, n := false, 0
	sc := bufio.NewScanner(bytes.NewReader(patch))
	sc.Buffer(make([]byte, 0, 64*1024), len(patch)+1)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "diff --git "):
			inHunk = false
		case strings.HasPrefix(line, "@@"):
			inHunk, n = true, hunkStart(line)
		case !inHunk:
		case strings.HasPrefix(line, "+"):
			r.checkLine(prefix, path, n, line[1:])
			n++
		case strings.HasPrefix(line, " "):
			n++
		}
	}
}

func hunkStart(line string) int {
	m := hunkRe.FindStringSubmatch(line)
	if m == nil {
		return 0
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

// history checks every commit that can be pushed (identities, message, added files and
// lines, against each parent for merges) and every tag (its name, and for an annotated tag
// its tagger and message).
func (r *reporter) history() error {
	// Branches, tags, remote-tracking refs and HEAD, rather than --all: --all also walks
	// refs/stash, and a stash is never pushed, so flagging it would only block a clean push.
	args := []string{"rev-list", "--branches", "--tags", "--remotes"}
	if head, err := gitOutput("rev-parse", "--verify", "-q", "HEAD^{commit}"); err == nil {
		args = append(args, strings.TrimSpace(string(head)))
	}
	out, err := gitOutput(args...)
	if err != nil {
		return err
	}
	for _, sha := range strings.Fields(string(out)) {
		if err := r.historyCommit(sha); err != nil {
			return err
		}
	}
	return r.tags()
}

func (r *reporter) historyCommit(sha string) error {
	prefix := fmt.Sprintf("commit %s ", sha[:7])
	raw, err := gitOutput("cat-file", "commit", sha)
	if err != nil {
		return err
	}
	parents := r.checkObject(prefix, string(raw), "author", "committer")
	if len(parents) == 0 {
		listing, err := gitOutput(append([]string{"diff-tree", "-r", "--root", "--no-commit-id"}, append(rawFlags, sha)...)...)
		if err != nil {
			return err
		}
		return r.scanChanges(prefix, parseRaw(listing), nil) // a root commit only adds files
	}
	for _, parent := range parents {
		listing, err := gitOutput(append([]string{"diff-tree", "-r"}, append(rawFlags, parent, sha)...)...)
		if err != nil {
			return err
		}
		err = r.scanChanges(prefix, parseRaw(listing), func(path string) ([]byte, error) {
			return gitOutput(append(append([]string{"--literal-pathspecs", "diff-tree", "-p", "-r"}, diffFlags...), parent, sha, "--", path)...)
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// tags checks every tag's name and, for annotated tags, the tag object itself.
func (r *reporter) tags() error {
	out, err := gitOutput("for-each-ref", "refs/tags", "--format=%(objecttype) %(objectname) %(refname:short)")
	if err != nil {
		return err
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.SplitN(line, " ", 3)
		if len(f) < 3 {
			continue
		}
		prefix := fmt.Sprintf("tag %s ", f[1][:7])
		for _, rule := range r.c.scanLine("", f[2]) {
			r.report(prefix+"tag-name", rule)
		}
		if f[0] != "tag" {
			continue // a lightweight tag: its commit is walked with the history
		}
		raw, err := gitOutput("cat-file", "tag", f[1])
		if err != nil {
			return err
		}
		r.checkObject(prefix, string(raw), "tagger")
	}
	return nil
}

// checkObject reads a raw commit or tag object (headers, a blank line, then the message),
// checks the identity headers of the given roles and every message line, and returns the
// parent ids.
func (r *reporter) checkObject(prefix, raw string, roles ...string) (parents []string) {
	headers, message, _ := strings.Cut(raw, "\n\n")
	for _, h := range strings.Split(headers, "\n") {
		if p, ok := strings.CutPrefix(h, "parent "); ok {
			parents = append(parents, strings.TrimSpace(p))
		}
		for _, role := range roles {
			if !strings.HasPrefix(h, role+" ") {
				continue
			}
			name, email := parseIdent(strings.TrimPrefix(h, role+" "))
			r.checkIdentityEmail(prefix, role, email)
			r.checkIdentityName(prefix, role, name)
		}
	}
	for n, line := range strings.Split(message, "\n") {
		for _, rule := range r.c.scanLine("", line) {
			r.report(fmt.Sprintf("%smessage:%d", prefix, n+1), rule)
		}
	}
	return parents
}

// parseIdent splits "Name <email> 1700000000 +0000".
func parseIdent(s string) (name, email string) {
	lt := strings.LastIndexByte(s, '<')
	gt := strings.LastIndexByte(s, '>')
	if lt < 0 || gt < lt {
		return strings.TrimSpace(s), ""
	}
	return strings.TrimSpace(s[:lt]), strings.ToLower(strings.TrimSpace(s[lt+1 : gt]))
}

// checkIdentityEmail: an author, committer or tagger address must be in allow.txt or a
// noreply address. Under LEADHOUND_DENYLIST=none the allowlist is empty, so only noreply
// addresses pass.
func (r *reporter) checkIdentityEmail(prefix, role, email string) {
	if r.c.allow[email] || isNoreply(email) {
		return
	}
	r.report(prefix+role+"-email", ruleNotAllowlisted)
}

// checkIdentityName checks an author, committer or tagger name against the denylist,
// unless allow.txt names it on a "name:" line (compared after normalisation).
func (r *reporter) checkIdentityName(prefix, role, name string) {
	if r.c.allowNames[normalize(name)] {
		return
	}
	if r.c.denyHit(name) {
		r.report(prefix+role+"-name", ruleDenylist)
	}
}
