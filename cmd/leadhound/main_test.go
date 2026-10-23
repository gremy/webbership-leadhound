package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

func TestRunExitCodes(t *testing.T) {
	saved := commands
	t.Cleanup(func() { commands = saved })
	commands = map[string]command{
		"ok":    func(context.Context, []string) error { return nil },
		"fail":  func(context.Context, []string) error { return errors.New("boom") },
		"usage": func(context.Context, []string) error { return usageError("missing -x") },
	}
	tests := []struct {
		name       string
		args       []string
		want       int
		wantStderr string
	}{
		{"no command", nil, 2, "usage: leadhound"},
		{"unknown command", []string{"nope"}, 2, `unknown command "nope"`},
		{"ok", []string{"ok"}, 0, ""},
		{"failure", []string{"fail"}, 1, ""},
		{"usage error", []string{"usage", "-y"}, 2, "missing -x"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stderr bytes.Buffer
			if got := run(context.Background(), tt.args, &stderr); got != tt.want {
				t.Errorf("run(%q) = %d, want %d", tt.args, got, tt.want)
			}
			if !strings.Contains(stderr.String(), tt.wantStderr) {
				t.Errorf("stderr = %q, want it to contain %q", stderr.String(), tt.wantStderr)
			}
		})
	}
}

func TestLoadConfigReportsMissing(t *testing.T) {
	t.Setenv("LEADHOUND_DATABASE_URL", "")
	t.Setenv("LEADHOUND_LIVE", "bad")
	_, err := loadConfig("LEADHOUND_DATABASE_URL")
	if !errors.Is(err, errUsage) {
		t.Fatalf("err = %v, want a usage error", err)
	}
	for _, want := range []string{"LEADHOUND_DATABASE_URL is not set", "LEADHOUND_LIVE"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want it to mention %s", err, want)
		}
	}
}
