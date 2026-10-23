package mail

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func fixedOutbox(dir string) *Outbox {
	loc := time.FixedZone("test", 3*3600)
	// 22:30 UTC is already the next day at UTC+3: the file takes the outbox's own day.
	return &Outbox{Dir: dir, Location: loc, Now: func() time.Time { return time.Date(2026, 10, 22, 22, 30, 0, 0, time.UTC) }}
}

func TestOutboxWritesNumberedFiles(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data", "outbox")
	var m Mailer = fixedOutbox(dir)
	if m.Provider() != "outbox" {
		t.Errorf("provider = %q", m.Provider())
	}
	msg := Message{To: "user@mail.example", Subject: "LeadHound · Fri 23 Oct", Text: "1. A job\n", HTML: "<ol><li>A job</li></ol>", IdempotencyKey: "brief-2026-10-23"}
	var ids []string
	for range 3 {
		id, err := m.Send(context.Background(), msg)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if strings.Join(ids, " ") != "2026-10-23-1.txt 2026-10-23-2.txt 2026-10-23-3.txt" {
		t.Errorf("ids = %q", ids)
	}
	b, err := os.ReadFile(filepath.Join(dir, "2026-10-23-1.txt")) // #nosec G304 -- the test's own temp dir
	if err != nil {
		t.Fatal(err)
	}
	want := "Subject: LeadHound · Fri 23 Oct\nIdempotency-Key: brief-2026-10-23\n\n1. A job\n\n" + htmlMarker + "\n<ol><li>A job</li></ol>\n"
	if string(b) != want {
		t.Errorf("file =\n%s\nwant\n%s", b, want)
	}
	if strings.Contains(string(b), msg.To) {
		t.Error("the recipient address is written to disk")
	}
}

// A file already there (an earlier run, or one deleted from the middle) is never overwritten.
func TestOutboxNeverOverwrites(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "2026-10-23-1.txt"), []byte("kept"), 0o600); err != nil {
		t.Fatal(err)
	}
	id, err := fixedOutbox(dir).Send(context.Background(), Message{To: "user@mail.example", Subject: "s", Text: "t"})
	if err != nil || id != "2026-10-23-2.txt" {
		t.Fatalf("id %q err %v", id, err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "2026-10-23-1.txt")); string(b) != "kept" { // #nosec G304 -- test temp dir
		t.Errorf("overwritten: %q", b)
	}
}

func TestOutboxPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits")
	}
	dir := filepath.Join(t.TempDir(), "outbox")
	id, err := fixedOutbox(dir).Send(context.Background(), Message{To: "user@mail.example", Subject: "s", Text: "t"})
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]os.FileMode{dir: 0o700, filepath.Join(dir, id): 0o600} {
		st, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := st.Mode().Perm(); got != want {
			t.Errorf("%s mode = %o, want %o", filepath.Base(path), got, want)
		}
	}
}

func TestOutboxRefusesBadRecipient(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "outbox")
	if _, err := fixedOutbox(dir).Send(context.Background(), Message{To: "nu"}); !errors.Is(err, ErrBadAddress) {
		t.Errorf("err = %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("outbox created for a refused message: %v", err)
	}
}

func TestOutboxNeedsADir(t *testing.T) {
	if _, err := (&Outbox{}).Send(context.Background(), Message{To: "user@mail.example"}); err == nil {
		t.Error("an outbox without a directory wrote somewhere")
	}
}
