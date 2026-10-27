package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gremy/webbership-leadhound/internal/claude"
	"github.com/gremy/webbership-leadhound/internal/secretbox"
)

// TestServeRefusesKeyFile: serve exits 2 naming LEADHOUND_MAILBOX_KEY_FILE when it is set
// to anything, before the database is reached, since the console must never hold the
// private mailbox key; the empty line deploy/env.example carries is not set, so serve goes
// on to the database (exit 1 here, its port closed).
func TestServeRefusesKeyFile(t *testing.T) {
	for _, c := range []struct {
		name, value string
		code        int
	}{
		{"set", "/etc/leadhound/mailbox.key", 2},
		{"blank-looking", " ", 2},
		{"empty, as env.example", "", 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			serveEnv(t)
			t.Setenv(secretbox.KeyFileEnv, c.value)
			captureStdout(t)
			var stderr bytes.Buffer
			code := run(context.Background(), []string{"serve"}, &stderr)
			names := strings.Contains(stderr.String(), secretbox.KeyFileEnv+" is set; the console must never hold the mailbox key")
			if code != c.code || names != (c.code == 2) {
				t.Errorf("exit %d, stderr %q; want %d", code, stderr.String(), c.code)
			}
		})
	}
}

// TestConsolePublicKey: the console's public key is the one LEADHOUND_MAILBOX_PUBLIC_KEY
// holds; unset or broken, it is the zero key with the problem named, never the value.
func TestConsolePublicKey(t *testing.T) {
	pub, _, err := secretbox.GenerateKeyPair(nil)
	if err != nil {
		t.Fatal(err)
	}
	env := func(v string) func(string) string {
		return func(k string) string {
			if k == secretbox.PublicKeyEnv {
				return v
			}
			return ""
		}
	}
	if got, err := consolePublicKey(env(pub.String())); err != nil || got != pub {
		t.Errorf("good key: %v %v", got, err)
	}
	for _, v := range []string{"", "not-base64!", "c2hvcnQ="} {
		got, err := consolePublicKey(env(v))
		if err == nil || got != (secretbox.PublicKey{}) || !strings.Contains(err.Error(), secretbox.PublicKeyEnv) {
			t.Errorf("%q: key %v, err %v; want the zero key and a problem naming the variable", v, got, err)
		}
	}
}

// TestServeRefusesAnthropicKey: serve exits 2 before the database is reached when it can see
// the Anthropic key, through any of the three variables set to anything or an anthropic-key
// credential in $CREDENTIALS_DIRECTORY; with all of them empty, or a credentials directory
// holding only another credential, it goes on to the database (exit 1 here, its port closed).
// The message names the trigger, never a value.
func TestServeRefusesAnthropicKey(t *testing.T) {
	const sentinel = "sk-ant-sentinel-value"
	withCred := t.TempDir()
	if err := os.WriteFile(filepath.Join(withCred, claude.CredentialName), []byte(sentinel), 0o600); err != nil {
		t.Fatal(err)
	}
	otherCred := t.TempDir()
	if err := os.WriteFile(filepath.Join(otherCred, "mailbox-key"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name, key, value string
		code             int
		names            string
	}{
		{"ANTHROPIC_API_KEY", "ANTHROPIC_API_KEY", sentinel, 2, "ANTHROPIC_API_KEY is set; the console must never hold the Anthropic key"},
		{"ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_AUTH_TOKEN", sentinel, 2, "ANTHROPIC_AUTH_TOKEN is set; the console must never hold the Anthropic key"},
		{"key file", claude.KeyFileEnv, "/home/op/anthropic.key", 2, claude.KeyFileEnv + " is set; the console must never hold the Anthropic key"},
		{"key file blank-looking", claude.KeyFileEnv, " ", 2, claude.KeyFileEnv + " is set"},
		{"credential", "CREDENTIALS_DIRECTORY", withCred, 2, "$CREDENTIALS_DIRECTORY/anthropic-key exists; the console must never hold the Anthropic key"},
		{"another credential only", "CREDENTIALS_DIRECTORY", otherCred, 1, ""},
		{"credentials directory missing", "CREDENTIALS_DIRECTORY", filepath.Join(otherCred, "nope"), 1, ""},
		{"clean", claude.KeyFileEnv, "", 1, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			serveEnv(t)
			t.Setenv(c.key, c.value)
			captureStdout(t)
			var stderr bytes.Buffer
			code := run(context.Background(), []string{"serve"}, &stderr)
			if code != c.code {
				t.Errorf("exit %d, stderr %q; want %d", code, stderr.String(), c.code)
			}
			if c.names != "" && !strings.Contains(stderr.String(), c.names) {
				t.Errorf("stderr %q does not name %q", stderr.String(), c.names)
			}
			if c.code != 2 && strings.Contains(stderr.String(), "Anthropic key") {
				t.Errorf("refused a clean start: %q", stderr.String())
			}
			if strings.Contains(stderr.String(), sentinel) {
				t.Errorf("stderr carries the key value: %q", stderr.String())
			}
		})
	}
}

// TestServeAnthropicKeyRefusalNeverReads: the refusal only asks whether the credential is
// there; an empty environment asks nothing at all.
func TestServeAnthropicKeyRefusalNeverReads(t *testing.T) {
	asked := []string{}
	exists := func(p string) bool { asked = append(asked, p); return false }
	if err := serveAnthropicKeyRefusal(func(string) string { return "" }, exists); err != nil || len(asked) != 0 {
		t.Errorf("empty env: err %v, asked %q", err, asked)
	}
	env := func(k string) string {
		if k == "CREDENTIALS_DIRECTORY" {
			return "/run/credentials/leadhound-console.service"
		}
		return ""
	}
	if err := serveAnthropicKeyRefusal(env, exists); err != nil || !slices.Equal(asked, []string{"/run/credentials/leadhound-console.service/anthropic-key"}) {
		t.Errorf("credentials dir: err %v, asked %q", err, asked)
	}
}
