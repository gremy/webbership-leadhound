package mailbox

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func fixedOutbox(dir string) *Outbox {
	loc := time.FixedZone("test", 3*3600)
	// 22:30 UTC is already the next day at UTC+3: the file takes the outbox's own day.
	return &Outbox{Dir: dir, Location: loc, Now: func() time.Time { return time.Date(2026, 10, 22, 22, 30, 0, 0, time.UTC) }}
}

func TestOutboxWritesNumberedEmlFiles(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data", "outreach-outbox")
	raw, err := Build(fixtureMessage())
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for range 2 {
		n, err := fixedOutbox(dir).Write(raw)
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, n)
	}
	if names[0] != "2026-10-23-1.eml" || names[1] != "2026-10-23-2.eml" {
		t.Errorf("names = %v", names)
	}
	got, err := os.ReadFile(filepath.Join(dir, names[0])) // #nosec G304 -- the test's own temp dir
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(raw) {
		t.Error("the file is not the built message byte for byte")
	}
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(filepath.Join(dir, names[0]))
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("file mode %v, want 0600", fi.Mode().Perm())
		}
		di, err := os.Stat(dir)
		if err != nil {
			t.Fatal(err)
		}
		if di.Mode().Perm() != 0o700 {
			t.Errorf("dir mode %v, want 0700", di.Mode().Perm())
		}
	}
}

func TestOutboxNeverOverwrites(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "2026-10-23-1.eml"), []byte("kept"), 0o600); err != nil {
		t.Fatal(err)
	}
	n, err := fixedOutbox(dir).Write([]byte("x\r\n"))
	if err != nil || n != "2026-10-23-2.eml" {
		t.Fatalf("name %q err %v", n, err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "2026-10-23-1.eml")) // #nosec G304 -- the test's own temp dir
	if err != nil || string(b) != "kept" {
		t.Errorf("existing file changed: %q %v", b, err)
	}
}

func TestOutboxRefusesNoDirOrEmpty(t *testing.T) {
	if _, err := (&Outbox{}).Write([]byte("x")); err == nil {
		t.Error("no dir: want an error")
	}
	if _, err := fixedOutbox(t.TempDir()).Write(nil); err == nil {
		t.Error("empty message: want an error")
	}
}

func TestOutboxNarrowsAnExistingDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX modes only")
	}
	dir := filepath.Join(t.TempDir(), "outreach-outbox")
	if err := os.Mkdir(dir, 0o755); err != nil { // #nosec G301 -- the test makes a too-wide dir on purpose
		t.Fatal(err)
	}
	if _, err := fixedOutbox(dir).Write([]byte("x\r\n")); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o700 {
		t.Errorf("dir mode %v, want 0700", fi.Mode().Perm())
	}
}
