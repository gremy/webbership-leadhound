package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/gremy/webbership-leadhound/internal/ai"
	"github.com/gremy/webbership-leadhound/internal/config"
)

// TestEvalDemo: the demo set runs with the fake and no database (the DSN points at a
// closed port, so any database use would fail the command).
func TestEvalDemo(t *testing.T) {
	t.Setenv("LEADHOUND_DATABASE_URL", "postgres://nobody@127.0.0.1:1/none?sslmode=disable&connect_timeout=1")
	t.Setenv("LEADHOUND_DATA_DIR", "")
	t.Setenv("TYPESAFE_API_KEY", "")
	out := captureStdout(t)
	var stderr bytes.Buffer
	if code := run(context.Background(), []string{"eval", "--golden", examplesDir, "--decider", "fake", "--runs", "2"}, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	for _, want := range []string{"eval: 5 golden jobs from golden.demo.jsonl · decider fake · 2 run(s)", "stack ", "disagreements between runs: none", "input tokens: 0"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("stdout lacks %q:\n%s", want, out.String())
		}
	}
}

func TestEvalUsageErrors(t *testing.T) {
	t.Setenv("LEADHOUND_DATA_DIR", "")
	t.Setenv("TYPESAFE_API_KEY", "")
	empty := t.TempDir()
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"eval"}, "usage: leadhound eval"},
		{[]string{"eval", "--golden", examplesDir, "--runs", "0"}, "--runs must be 1 to 5"},
		{[]string{"eval", "--golden", examplesDir, "--runs", "6"}, "--runs must be 1 to 5"},
		{[]string{"eval", "--golden", empty, "--config", examplesDir}, "no golden set"},
		{[]string{"eval", "--golden", empty}, "leadhound.yaml"},
		{[]string{"eval", "--golden", examplesDir, "--decider", "jev"}, "needs TYPESAFE_API_KEY"},
	} {
		captureStdout(t)
		var stderr bytes.Buffer
		if code := run(context.Background(), tc.args, &stderr); code != 2 || !strings.Contains(stderr.String(), tc.want) {
			t.Errorf("%v: exit %d, stderr %q, want 2 and %q", tc.args, code, stderr.String(), tc.want)
		}
	}
}

// TestEvalDefaultsToTheFakeEvenWithAKey: a key alone never makes eval live; it stays on
// the fake (the DSN and the API are never reached) and announces no paid calls.
func TestEvalDefaultsToTheFakeEvenWithAKey(t *testing.T) {
	t.Setenv("LEADHOUND_DATABASE_URL", "postgres://nobody@127.0.0.1:1/none?sslmode=disable&connect_timeout=1")
	t.Setenv("LEADHOUND_DATA_DIR", "")
	t.Setenv("TYPESAFE_API_KEY", "ts-"+strings.Repeat("x", 8)+"-test-only")
	n := captureNotes(t)
	out := captureStdout(t)
	var stderr bytes.Buffer
	if code := run(context.Background(), []string{"eval", "--golden", examplesDir, "--runs", "1"}, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if !strings.Contains(out.String(), "decider fake") {
		t.Errorf("stdout lacks the fake decider:\n%s", out.String())
	}
	if strings.Contains(n.String(), "calls") {
		t.Errorf("a fake eval announced calls: %q", n.String())
	}
}

// TestAnnounceEval: a live eval states its call count before calling; the fake says nothing.
func TestAnnounceEval(t *testing.T) {
	n := captureNotes(t)
	announceEval(ai.NewJev(config.Secret("ts-"+strings.Repeat("y", 8)), "jev-1.13.0"), 5, 3)
	if want := "eval: jev-1.13.0 will make 15 calls (5 jobs × 3 runs"; !strings.HasPrefix(n.String(), want) {
		t.Errorf("notes %q, want prefix %q", n.String(), want)
	}
	n.Reset()
	announceEval(fakeDecider(), 5, 3)
	if n.Len() != 0 {
		t.Errorf("fake announced %q", n.String())
	}
}
