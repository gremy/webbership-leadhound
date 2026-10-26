package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

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
