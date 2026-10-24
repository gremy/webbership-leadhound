package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"github.com/gremy/webbership-leadhound/internal/console"
)

var (
	testHashOnce sync.Once
	testHashVal  string
	testHashErr  error
)

// testHash is a cost-12 hash of a test password, made once per test binary.
func testHash(t *testing.T) string {
	t.Helper()
	testHashOnce.Do(func() { testHashVal, testHashErr = console.HashPassword([]byte("a-test-password-only")) })
	if testHashErr != nil {
		t.Fatal(testHashErr)
	}
	return testHashVal
}

// serveEnv sets everything serve needs to start except the database, which points at a
// closed port: a refusal must come before the database is opened.
func serveEnv(t *testing.T) {
	t.Helper()
	t.Setenv("LEADHOUND_DATABASE_URL", "postgres://nobody@127.0.0.1:1/none?sslmode=disable&connect_timeout=1")
	t.Setenv("LEADHOUND_DATA_DIR", demoDataDir(t))
	t.Setenv("LEADHOUND_LIVE", "")
	t.Setenv("LEADHOUND_TZ", "")
	t.Setenv("LEADHOUND_CONSOLE_ADDR", "")
	t.Setenv("LEADHOUND_CONSOLE_URL", "")
	t.Setenv("LEADHOUND_CONSOLE_EMAIL", "operator@mail.example")
	t.Setenv("LEADHOUND_CONSOLE_PASSWORD_HASH", testHash(t))
}

// TestServeRefusals: serve exits 2, naming the problem, without the operator's email or
// hash, with a hash that is not bcrypt or is too cheap, in dev mode with live fetching
// on, on an address off loopback and with a console URL that is not https.
func TestServeRefusals(t *testing.T) {
	cheap, err := bcrypt.GenerateFromPassword([]byte("a-test-password-only"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name, key, value string
		args             []string
		want             string
	}{
		{"no email", "LEADHOUND_CONSOLE_EMAIL", "", nil, "LEADHOUND_CONSOLE_EMAIL is not set"},
		{"no hash", "LEADHOUND_CONSOLE_PASSWORD_HASH", "", nil, "LEADHOUND_CONSOLE_PASSWORD_HASH is not set"},
		{"bad hash", "LEADHOUND_CONSOLE_PASSWORD_HASH", "not-a-hash", nil, "not a bcrypt hash"},
		{"cheap hash", "LEADHOUND_CONSOLE_PASSWORD_HASH", string(cheap), nil, "below 12"},
		{"dev live on", "LEADHOUND_LIVE", "1", []string{"--dev"}, "--dev"},
		{"dev live ats", "LEADHOUND_LIVE", "ats", []string{"--dev"}, "--dev"},
		{"public addr", "LEADHOUND_CONSOLE_ADDR", "192.0.2.10:8090", nil, "loopback"},
		{"any addr", "LEADHOUND_CONSOLE_ADDR", ":8090", nil, "loopback"},
		{"http url", "LEADHOUND_CONSOLE_URL", "http://console.example", nil, "LEADHOUND_CONSOLE_URL must be an https URL"},
		{"stray argument", "", "", []string{"now"}, "usage: leadhound serve"},
	} {
		t.Run(c.name, func(t *testing.T) {
			serveEnv(t)
			if c.key != "" {
				t.Setenv(c.key, c.value)
			}
			captureStdout(t)
			var stderr bytes.Buffer
			code := run(context.Background(), append([]string{"serve"}, c.args...), &stderr)
			if code != 2 || !strings.Contains(stderr.String(), c.want) {
				t.Errorf("exit %d, stderr %q; want 2 naming %q", code, stderr.String(), c.want)
			}
			if strings.Contains(stderr.String(), "$2") {
				t.Error("stderr repeats the hash")
			}
		})
	}
}

// TestServeStartsPastRefusals: with every setting usable, serve gets past its checks to the
// database, whose failure is a crash (exit 1), not a usage error.
func TestServeStartsPastRefusals(t *testing.T) {
	serveEnv(t)
	t.Setenv("LEADHOUND_LIVE", "1")
	t.Setenv("LEADHOUND_CONSOLE_URL", "https://console.example")
	var stderr bytes.Buffer
	if code := run(context.Background(), []string{"serve"}, &stderr); code != 1 {
		t.Errorf("exit %d, want 1; stderr %q", code, stderr.String())
	}
}

// pipeStdin points the password input at a file holding s, as a pipe would.
func pipeStdin(t *testing.T, s string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "stdin")
	if err := os.WriteFile(path, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path) // #nosec G304 -- a test temp file
	if err != nil {
		t.Fatal(err)
	}
	saved := passwordInput
	passwordInput = f
	t.Cleanup(func() { passwordInput = saved; _ = f.Close() })
}

// TestHashPasswordPipe: a piped password prints one cost-12 bcrypt hash that matches it,
// and nothing else.
func TestHashPasswordPipe(t *testing.T) {
	const pw = "correct horse battery"
	pipeStdin(t, pw+"\n")
	out := captureStdout(t)
	var stderr bytes.Buffer
	if code := run(context.Background(), []string{"console", "hash-password"}, &stderr); code != 0 {
		t.Fatalf("exit %d, %q", code, stderr.String())
	}
	h := strings.TrimSuffix(out.String(), "\n")
	if !strings.HasPrefix(h, "$2") || strings.Contains(h, "\n") || strings.Contains(stderr.String(), pw) {
		t.Fatalf("stdout %q, stderr %q", out.String(), stderr.String())
	}
	if cost, err := bcrypt.Cost([]byte(h)); err != nil || cost != 12 {
		t.Errorf("cost %d, %v", cost, err)
	}
	if err := bcrypt.CompareHashAndPassword([]byte(h), []byte(pw)); err != nil {
		t.Errorf("the hash does not match the password: %v", err)
	}
}

// TestHashPasswordRefusals: a short or over-long password, an empty input and a wrong
// subcommand exit 2 and print no hash.
func TestHashPasswordRefusals(t *testing.T) {
	for _, c := range []struct {
		name, in string
		args     []string
		want     string
	}{
		{"short", "eleven-char\n", nil, "at least 12"},
		{"empty", "", nil, "at least 12"},
		{"too long", strings.Repeat("x", 73) + "\n", nil, "at most 72 bytes"},
		{"no subcommand", "a-long-enough-one\n", []string{"console"}, "usage: leadhound console hash-password"},
		{"unknown subcommand", "a-long-enough-one\n", []string{"console", "reset"}, "usage: leadhound console hash-password"},
	} {
		t.Run(c.name, func(t *testing.T) {
			pipeStdin(t, c.in)
			out := captureStdout(t)
			args := c.args
			if args == nil {
				args = []string{"console", "hash-password"}
			}
			var stderr bytes.Buffer
			code := run(context.Background(), args, &stderr)
			if code != 2 || !strings.Contains(stderr.String(), c.want) || out.Len() != 0 {
				t.Errorf("exit %d, stdout %q, stderr %q; want 2 naming %q", code, out.String(), stderr.String(), c.want)
			}
		})
	}
}

// TestReadPasswordLineKeepsSpaces: only the line ending is removed from a piped password.
func TestReadPasswordLineKeepsSpaces(t *testing.T) {
	for in, want := range map[string]string{
		" padded password \n":   " padded password ",
		"windows line ends\r\n": "windows line ends",
		"no line end":           "no line end",
		"first line\nsecond\n":  "first line",
	} {
		got, err := readPasswordLine(strings.NewReader(in))
		if err != nil || string(got) != want {
			t.Errorf("%q: got %q, %v; want %q", in, got, err, want)
		}
	}
}

// TestBriefLogHealth: only the lines that start with "Health: " are the stored health.
func TestBriefLogHealth(t *testing.T) {
	body := "LeadHound · Fri 23 Oct\n\nHealth: LinkedIn ok\nNot Health: x\nHealth: Scoring ok\r\n"
	got := storedHealth(body)
	if len(got) != 2 || got[0] != "Health: LinkedIn ok" || got[1] != "Health: Scoring ok" {
		t.Errorf("health = %q", got)
	}
	if storedHealth("") != nil {
		t.Error("an empty body has health lines")
	}
}
