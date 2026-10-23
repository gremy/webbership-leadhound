//go:build integration

package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func runCmdCode(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	out := captureStdout(t)
	var stderr bytes.Buffer
	code := run(context.Background(), args, &stderr)
	return code, out.String(), stderr.String()
}

func TestRulesCommand(t *testing.T) {
	commandDB(t)

	if code, out, _ := runCmdCode(t, "rules", "list"); code != 0 || out != "no rules\n" {
		t.Fatalf("empty list: exit %d, %q", code, out)
	}
	code, out, _ := runCmdCode(t, "rules", "add", "--kind", "skip", "--field", "company", "--pattern", `acme\s+staffing`, "--reason", "staffing agency")
	if code != 0 || out != "rule 1 added\n" {
		t.Fatalf("add: exit %d, %q", code, out)
	}
	if code, out, _ := runCmdCode(t, "rules", "add", "--kind", "flag", "--field", "title", "--pattern", "contract", "--reason", "contract roles"); code != 0 || out != "rule 2 added\n" {
		t.Fatalf("add 2: exit %d, %q", code, out)
	}

	// An invalid pattern, kind or missing reason is a usage error and adds nothing.
	for _, args := range [][]string{
		{"rules", "add", "--kind", "skip", "--field", "company", "--pattern", "acme(", "--reason", "x"},
		{"rules", "add", "--kind", "drop", "--field", "company", "--pattern", "acme", "--reason", "x"},
		{"rules", "add", "--kind", "skip", "--field", "company", "--pattern", "acme"},
		{"rules", "disable", "1"},
		{"rules", "disable", "x", "--reason", "y"},
		{"rules", "remove", "1"},
	} {
		if code, _, stderr := runCmdCode(t, args...); code != 2 {
			t.Errorf("%v: exit %d (%s), want 2", args, code, stderr)
		}
	}
	if code, _, stderr := runCmdCode(t, "rules", "add", "--kind", "skip", "--field", "company", "--pattern", "acme(", "--reason", "x"); code != 2 || !strings.Contains(stderr, "does not compile") {
		t.Errorf("invalid regex: exit %d, stderr %q", code, stderr)
	}

	if code, out, _ := runCmdCode(t, "rules", "disable", "1", "--reason", "fine after all"); code != 0 || out != "rule 1 disabled\n" {
		t.Fatalf("disable: exit %d, %q", code, out)
	}
	if code, _, stderr := runCmdCode(t, "rules", "disable", "1", "--reason", "again"); code != 2 || !strings.Contains(stderr, "already disabled") {
		t.Errorf("disable twice: exit %d, stderr %q", code, stderr)
	}
	if code, _, stderr := runCmdCode(t, "rules", "disable", "99", "--reason", "x"); code != 2 || !strings.Contains(stderr, "does not exist") {
		t.Errorf("disable unknown: exit %d, stderr %q", code, stderr)
	}

	// list shows active rules; --all shows the disabled one too, never deleted.
	_, out, _ = runCmdCode(t, "rules", "list")
	if lines := strings.Split(strings.TrimSpace(out), "\n"); len(lines) != 1 || !strings.HasPrefix(lines[0], "2\tflag\ttitle\t\"contract\"\tcontract roles\tcreated ") || !strings.HasSuffix(lines[0], "\tactive") {
		t.Errorf("list = %q", out)
	}
	_, out, _ = runCmdCode(t, "rules", "list", "--all")
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "1\tskip\tcompany\t\"acme\\\\s+staffing\"\tstaffing agency\tcreated ") ||
		!strings.Contains(lines[0], "\tdisabled ") || !strings.HasSuffix(lines[0], ": fine after all") {
		t.Errorf("list --all = %q", out)
	}
	if got := dbCount(t, `SELECT count(*) FROM rule`); got != 2 {
		t.Errorf("rules = %d, want 2", got)
	}
}
