package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gremy/webbership-leadhound/internal/scoring"
)

// examplesDir holds the fictional demo profile.
var examplesDir = filepath.Join("..", "..", "examples")

func TestScoreCheckConfig(t *testing.T) {
	t.Setenv("LEADHOUND_DATA_DIR", examplesDir)
	out := captureStdout(t)
	var stderr bytes.Buffer
	if code := run(context.Background(), []string{"score", "--check-config"}, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	want := "profile ok: criteria 5 · evidence keywords 13 (for 2 criteria) · search keywords 2 · locations 1\n"
	if out.String() != want {
		t.Errorf("stdout %q, want %q", out.String(), want)
	}
}

// TestScoreUsageErrors: each is exit 2, before any database is opened (the DSN points at a
// closed port).
func TestScoreUsageErrors(t *testing.T) {
	t.Setenv("LEADHOUND_DATABASE_URL", "postgres://nobody@127.0.0.1:1/none?sslmode=disable&connect_timeout=1")
	t.Setenv("TYPESAFE_API_KEY", "")
	for _, tc := range []struct {
		dataDir string
		args    []string
		want    string
	}{
		{"", []string{"score", "--check-config"}, "LEADHOUND_DATA_DIR is not set"},
		{"", []string{"score", "--decider", "fake"}, "LEADHOUND_DATA_DIR is not set"},
		{t.TempDir(), []string{"score", "--check-config"}, "leadhound.yaml"},
		{examplesDir, []string{"score", "--decider", "jev"}, "needs TYPESAFE_API_KEY"},
		// No key and no flag never falls back to the fake.
		{examplesDir, []string{"score"}, "set TYPESAFE_API_KEY or pass --decider fake"},
		{examplesDir, []string{"score", "--decider", "magic"}, "must be jev or fake"},
		{examplesDir, []string{"score", "extra"}, "usage: leadhound score"},
	} {
		t.Setenv("LEADHOUND_DATA_DIR", tc.dataDir)
		captureStdout(t)
		var stderr bytes.Buffer
		if code := run(context.Background(), tc.args, &stderr); code != 2 || !strings.Contains(stderr.String(), tc.want) {
			t.Errorf("%v (data dir %q): exit %d, stderr %q, want 2 and %q", tc.args, tc.dataDir, code, stderr.String(), tc.want)
		}
	}
}

func TestFakeDeciderPassesEveryCriterion(t *testing.T) {
	prof, err := loadProfileDir(examplesDir)
	if err != nil {
		t.Fatal(err)
	}
	f := fakeDecider()
	if f.Name() != "fake" {
		t.Errorf("name %q", f.Name())
	}
	for _, c := range prof.Candidate.Criteria.List() {
		if a := f.Answers[c.Name]; a.Choice != "pass" || a.Confidence != 0.9 {
			t.Errorf("%s: %+v", c.Name, a)
		}
	}
}

// TestFakeDeciderFailsTheTitleCheck: the fake answers the title-only question too, always
// fail at 0.9, so a dry run shows the title check at work.
func TestFakeDeciderFailsTheTitleCheck(t *testing.T) {
	if a := fakeDecider().Answers[scoring.TitleCheckName]; a.Choice != scoring.Fail || a.Confidence != 0.9 {
		t.Errorf("title check answer %+v", a)
	}
}
