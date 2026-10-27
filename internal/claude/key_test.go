package claude

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/gremy/webbership-leadhound/internal/ai"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func writeKey(t *testing.T, dir, name, body string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil { // umask may have narrowed it
		t.Fatal(err)
	}
	return p
}

func TestLoadKeyCredentialNoModeCheck(t *testing.T) {
	dir := t.TempDir()
	writeKey(t, dir, CredentialName, "cred-key\n", 0o640)
	file := writeKey(t, t.TempDir(), "k", "file-key", 0o600)
	k, err := LoadKey(env(map[string]string{"CREDENTIALS_DIRECTORY": dir, KeyFileEnv: file}))
	if err != nil {
		t.Fatal(err)
	}
	if k.Reveal() != "cred-key" {
		t.Errorf("key %q, want the credential, trimmed", k.Reveal())
	}
}

func TestLoadKeyFileNeeds0600(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no mode bits on Windows")
	}
	dir := t.TempDir()
	loose := writeKey(t, dir, "loose", "secret-loose", 0o644)
	_, err := LoadKey(env(map[string]string{KeyFileEnv: loose}))
	if err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Fatalf("err %v, want a mode refusal", err)
	}
	if strings.Contains(err.Error(), "secret-loose") {
		t.Error("error carries the key")
	}
	tight := writeKey(t, dir, "tight", "  file-key \n", 0o600)
	k, err := LoadKey(env(map[string]string{KeyFileEnv: tight, "CREDENTIALS_DIRECTORY": t.TempDir()}))
	if err != nil || k.Reveal() != "file-key" {
		t.Fatalf("key %q err %v", k.Reveal(), err)
	}
}

func TestLoadKeyIgnoresEnvKey(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sentinel-env")
	_, err := LoadKey(os.Getenv)
	if err == nil {
		t.Fatal("LoadKey took ANTHROPIC_API_KEY")
	}
	_, err = LoadKey(env(map[string]string{"ANTHROPIC_API_KEY": "sentinel-env"}))
	if err == nil {
		t.Fatal("LoadKey took ANTHROPIC_API_KEY")
	}
}

func TestLoadKeyEmptyFile(t *testing.T) {
	p := writeKey(t, t.TempDir(), "k", " \n", 0o600)
	if _, err := LoadKey(env(map[string]string{KeyFileEnv: p})); err == nil {
		t.Fatal("empty key file accepted")
	}
}

func TestFake(t *testing.T) {
	f := &Fake{
		WriteAnswer: WriteResult{JSON: json.RawMessage(`{"a":1}`)},
		ByLead:      map[int64]WriteResult{2: {JSON: json.RawMessage(`{"a":2}`)}},
	}
	res, u, err := f.Write(context.Background(), WriteRequest{LeadID: 2})
	if err != nil || string(res.JSON) != `{"a":2}` || u.Model != FakeModel {
		t.Fatalf("res %s usage %+v err %v", res.JSON, u, err)
	}
	if res, _, _ := f.Write(context.Background(), WriteRequest{LeadID: 3}); string(res.JSON) != `{"a":1}` {
		t.Errorf("fallback answer %s", res.JSON)
	}
	if _, _, err := (&Fake{}).Write(context.Background(), WriteRequest{}); !errors.Is(err, ai.ErrNotScripted) {
		t.Errorf("unscripted err %v", err)
	}
	if f.Calls() != 2 {
		t.Errorf("calls %d", f.Calls())
	}
	if c, _ := Cost(u); c != 0 {
		t.Errorf("fake cost %d", c)
	}
}
