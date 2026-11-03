//go:build integration

package main

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/gremy/webbership-leadhound/internal/store/storetest"
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

// TestRulesTestCommand: the dry run lists what one pattern would hide under its two labels, a
// rescued job never, and writes nothing; a bad field, pattern or day count is a usage error.
func TestRulesTestCommand(t *testing.T) {
	commandDB(t)
	b := testBackend(t)
	ctx := context.Background()
	briefed := bareJob(t, b, "noise-briefed")
	other := bareJob(t, b, "other")
	rescued := bareJob(t, b, "noise-rescued")
	future := bareJob(t, b, "noise-future")
	older := bareJob(t, b, "noise-older")
	if _, err := storetest.OwnerConn(t).Exec(ctx, `UPDATE job SET first_seen_at = now() - interval '20 days' WHERE id = $1`, older); err != nil {
		t.Fatal(err)
	}
	sendBriefOn(t, today(), briefed, other, rescued)
	if _, err := b.s.Q().InsertStatusEvent(ctx, rescued, "new", "maybe: worth a look", "gremy", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := b.s.Q().AddRule(ctx, "skip", "title", "other", "an active rule is not this test's", nil); err != nil {
		t.Fatal(err)
	}
	writes := func() int {
		return dbCount(t, `SELECT (SELECT count(*) FROM rule) + (SELECT count(*) FROM status_event) + (SELECT count(*) FROM job)`)
	}
	before := writes()

	code, out, stderr := runCmdCode(t, "rules", "test", "--field", "title", "--pattern", "NOISE")
	want := "would hide from the queue (briefs of the last 14 days): 1\n" +
		fmt.Sprintf("job:%d\tnoise-briefed Co\tnoise-briefed card title\n", briefed) +
		"reached no brief of the last 14 days, first seen in the last 30 days: 2\n" +
		fmt.Sprintf("job:%d\tnoise-future Co\tnoise-future card title\n", future) +
		fmt.Sprintf("job:%d\tnoise-older Co\tnoise-older card title\n", older)
	if code != 0 || out != want {
		t.Errorf("exit %d, %q, stderr %q; want %q", code, out, stderr, want)
	}
	_, out, _ = runCmdCode(t, "rules", "test", "--field", "title", "--pattern", "noise", "--days", "10")
	if !strings.Contains(out, "first seen in the last 10 days: 1\n") || strings.Contains(out, "noise-older") {
		t.Errorf("--days 10 = %q", out)
	}
	if _, out, _ := runCmdCode(t, "rules", "test", "--field", "company", "--pattern", "no match here"); !strings.Contains(out, "days: 0\n") {
		t.Errorf("no match = %q", out)
	}
	if got := writes(); got != before {
		t.Errorf("rows went from %d to %d: the dry run wrote", before, got)
	}
	for _, args := range [][]string{
		{"rules", "test", "--pattern", "x"},
		{"rules", "test", "--field", "salary", "--pattern", "x"},
		{"rules", "test", "--field", "title"},
		{"rules", "test", "--field", "title", "--pattern", "x("},
		{"rules", "test", "--field", "title", "--pattern", "x", "--days", "0"},
		{"rules", "test", "--field", "title", "--pattern", "x", "stray"},
	} {
		if code, _, stderr := runCmdCode(t, args...); code != 2 {
			t.Errorf("%v: exit %d (%s), want 2", args, code, stderr)
		}
	}
}

// TestRulesTestIgnoresStoredFails: the dry run asks what this one pattern would hide, so a job
// another rule's stored fail keeps from the brief is not listed, and one it also matches is.
func TestRulesTestIgnoresStoredFails(t *testing.T) {
	commandDB(t)
	b := testBackend(t)
	other := queueRule(t, b, "skip", "company", "elsewhere", "another rule")
	stored := bareJob(t, b, "stored")
	queueStoredFail(t, b, stored, observeText(t, b, stored, "stored", "text", false), other)
	both := bareJob(t, b, "noise")
	queueStoredFail(t, b, both, observeText(t, b, both, "noise", "text", false), other)

	code, out, stderr := runCmdCode(t, "rules", "test", "--field", "title", "--pattern", "noise")
	want := "would hide from the queue (briefs of the last 14 days): 0\n" +
		"reached no brief of the last 14 days, first seen in the last 30 days: 1\n" +
		fmt.Sprintf("job:%d\tnoise Co\tnoise\n", both)
	if code != 0 || out != want {
		t.Errorf("exit %d, %q, stderr %q; want %q", code, out, stderr, want)
	}
}
