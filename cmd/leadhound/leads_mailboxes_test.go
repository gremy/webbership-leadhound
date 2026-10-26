package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/gremy/webbership-leadhound/internal/console"
	"github.com/gremy/webbership-leadhound/internal/secretbox"
)

// mailboxesRun runs `leadhound args...` and returns its exit code, stdout and stderr.
func mailboxesRun(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	saved := stdout
	stdout = &out
	defer func() { stdout = saved }()
	code := run(context.Background(), args, &errOut)
	return code, out.String(), errOut.String()
}

// noEnv clears every variable a leads command reads, so keygen and pubkey are shown to run
// with an empty environment and no data dir (as install.sh calls them, as root).
func noEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"LEADHOUND_DATA_DIR", "LEADHOUND_DATABASE_URL", secretbox.KeyFileEnv, secretbox.PublicKeyEnv,
		"CREDENTIALS_DIRECTORY", "LEADHOUND_OUTREACH"} {
		t.Setenv(k, "")
	}
}

// TestMailboxesKeygenPubkey: keygen writes a 0600 key, prints the public line and the
// fingerprint and never overwrites; pubkey prints the same public line from the file; both
// run with no environment and no data dir.
func TestMailboxesKeygenPubkey(t *testing.T) {
	noEnv(t)
	path := filepath.Join(t.TempDir(), "mailbox.key")

	code, out, errOut := mailboxesRun(t, "leads", "mailboxes", "keygen", "--out", path)
	if code != 0 {
		t.Fatalf("keygen exited %d: %s", code, errOut)
	}
	priv, err := secretbox.ReadPrivateKey(path, runtime.GOOS != "windows")
	if err != nil {
		t.Fatalf("the key file: %v", err)
	}
	if runtime.GOOS != "windows" {
		if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
			t.Errorf("key file mode = %v, %v; want 0600", fi.Mode().Perm(), err)
		}
	}
	line := secretbox.PublicKeyEnv + "=" + priv.Public().String()
	fp := secretbox.Fingerprint(priv.Public())
	if !strings.Contains(out, line+"\n") || !strings.Contains(out, fp) {
		t.Errorf("keygen printed %q, want %q and the fingerprint %s", out, line, fp)
	}
	if strings.Contains(out, base64Of(priv)) || strings.Contains(errOut, base64Of(priv)) {
		t.Error("keygen printed the private key")
	}

	before, err := os.ReadFile(path) // #nosec G304 -- the test's own temp file
	if err != nil {
		t.Fatal(err)
	}
	code, _, errOut = mailboxesRun(t, "leads", "mailboxes", "keygen", "--out", path)
	if code != 2 || !strings.Contains(errOut, "already exists") {
		t.Errorf("keygen over a key exited %d: %s; want 2, already exists", code, errOut)
	}
	if after, _ := os.ReadFile(path); !bytes.Equal(before, after) { // #nosec G304 -- the test's own temp file
		t.Error("keygen overwrote the key file")
	}

	code, out, errOut = mailboxesRun(t, "leads", "mailboxes", "pubkey", "--key", path)
	if code != 0 || out != line+"\n" {
		t.Errorf("pubkey = %d %q (%s), want 0 %q", code, out, errOut, line+"\n")
	}

	for _, args := range [][]string{
		{"leads", "mailboxes", "keygen"},
		{"leads", "mailboxes", "pubkey"},
		{"leads", "mailboxes", "pubkey", "--key", filepath.Join(t.TempDir(), "missing.key")},
	} {
		if code, _, _ := mailboxesRun(t, args...); code != 2 {
			t.Errorf("%q exited %d, want 2", args, code)
		}
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(path, 0o644); err != nil { //nolint:gosec // G302: the test makes the key readable on purpose
			t.Fatal(err)
		}
		if code, _, errOut := mailboxesRun(t, "leads", "mailboxes", "pubkey", "--key", path); code != 2 || !strings.Contains(errOut, "chmod 600") {
			t.Errorf("pubkey of a group-readable key = %d %s, want 2 and chmod 600", code, errOut)
		}
	}
}

func base64Of(k secretbox.PrivateKey) string {
	dir, _ := os.MkdirTemp("", "k")
	defer func() { _ = os.RemoveAll(dir) }()
	p := filepath.Join(dir, "k")
	if err := secretbox.WritePrivateKey(p, k); err != nil {
		return "unreadable"
	}
	b, _ := os.ReadFile(p) // #nosec G304 -- the test's own temp file
	return strings.TrimSpace(string(b))
}

// TestMailboxesUsage: a bad command line exits 2; a password is never taken from argv.
func TestMailboxesUsage(t *testing.T) {
	noEnv(t)
	for _, args := range [][]string{
		{"leads", "mailboxes"},
		{"leads", "mailboxes", "nonsense"},
		{"leads", "mailboxes", "add", "--address", "a@sender.example", "--password", "x"},
		{"leads", "mailboxes", "add", "--address", "a@sender.example", "extra"},
		{"leads", "mailboxes", "pause"},
		{"leads", "mailboxes", "pause", "x1"},
		{"leads", "mailboxes", "pause", "3", "--domain", "sender.example"},
		{"leads", "mailboxes", "resume"},
		{"leads", "mailboxes", "retire"},
		{"leads", "mailboxes", "check"},
		{"leads", "mailboxes", "check", "0"},
		{"leads", "mailboxes", "list", "extra"},
	} {
		if code, _, _ := mailboxesRun(t, args...); code != 2 {
			t.Errorf("%q exited %d, want 2", args, code)
		}
	}
}

// TestMailboxPresets: Spacemail's servers and selector, and Other with nothing filled;
// every preset port is one the check accepts. A preset is found by name in any case.
func TestMailboxPresets(t *testing.T) {
	want := []MailboxPreset{{Name: "Spacemail", SMTP: "mail.spacemail.com:465", IMAP: "mail.spacemail.com:993", Selector: "spacemail"}, {Name: "Other"}}
	if len(mailboxPresets) != len(want) {
		t.Fatalf("presets = %+v", mailboxPresets)
	}
	for i, p := range mailboxPresets {
		if p != want[i] {
			t.Errorf("preset %d = %+v, want %+v", i, p, want[i])
		}
		if p.SMTP != "" && checkServer("sending", p.SMTP, smtpPorts) != nil {
			t.Errorf("%s SMTP %s is refused", p.Name, p.SMTP)
		}
		if p.IMAP != "" && checkServer("reading", p.IMAP, imapPorts) != nil {
			t.Errorf("%s IMAP %s is refused", p.Name, p.IMAP)
		}
	}
	if p, ok := mailboxPreset("spaceMAIL"); !ok || p.Name != "Spacemail" {
		t.Errorf("preset lookup = %+v, %v", p, ok)
	}
	if _, ok := mailboxPreset("nope"); ok {
		t.Error("an unknown preset was found")
	}
}

// TestNormalizeMailboxAddress: lower case, IDN to punycode, the registrable domain; anything
// that is not one address is refused with plain words.
func TestNormalizeMailboxAddress(t *testing.T) {
	for _, c := range []struct{ in, addr, domain string }{
		{" Sales@Sender.Example ", "sales@sender.example", "sender.example"},
		{"info@mail.sender.example", "info@mail.sender.example", "sender.example"},
		{"info@bücher.example", "info@xn--bcher-kva.example", "xn--bcher-kva.example"},
	} {
		addr, domain, err := normalizeMailboxAddress(c.in)
		if err != nil || addr != c.addr || domain != c.domain {
			t.Errorf("%q = %q %q %v, want %q %q", c.in, addr, domain, err, c.addr, c.domain)
		}
	}
	for _, in := range []string{"", "nope", "@sender.example", "a@", "a@b@sender.example", "a b@sender.example",
		"<a@sender.example>", "a@sender.example\r\nBcc: x@sender.example", "a@192.0.2.1", "a@sender..example"} {
		if _, _, err := normalizeMailboxAddress(in); !errors.Is(err, console.ErrUserFixable) {
			t.Errorf("%q: %v, want a refusal", in, err)
		}
	}
}

// TestCheckServer: host:port with an accepted port only.
func TestCheckServer(t *testing.T) {
	for _, ok := range []string{"smtp.sender.example:465", "smtp.sender.example:587"} {
		if err := checkServer("sending", ok, smtpPorts); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "smtp.sender.example", "smtp.sender.example:25", ":465", "a b.example:465", "smtp.sender.example:993"} {
		err := checkServer("sending", bad, smtpPorts)
		if !errors.Is(err, console.ErrUserFixable) || !strings.Contains(err.Error(), "465 or 587") {
			t.Errorf("%q: %v", bad, err)
		}
	}
	if err := checkServer("reading", "imap.sender.example:143", imapPorts); err == nil || !strings.Contains(err.Error(), "port 993") {
		t.Errorf("imap 143: %v", err)
	}
}

// TestMailboxPasswordFromStdin: one line, its line ending dropped; never from argv.
func TestMailboxPasswordFromStdin(t *testing.T) {
	pw := "pw-" + "Lk20-not-a-real-one"
	for in, want := range map[string]string{pw + "\n": pw, pw + "\r\n": pw, pw: pw, "\n": "", "": ""} {
		f := stdinFile(t, in)
		got, err := readMailboxPassword(f)
		if err != nil || got.Reveal() != want {
			t.Errorf("%q = %q, %v; want %q", in, got.Reveal(), err, want)
		}
	}
}

// stdinFile is a file holding s, open for reading, standing in for stdin.
func stdinFile(t *testing.T, s string) *os.File {
	t.Helper()
	p := filepath.Join(t.TempDir(), "stdin")
	if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(p) // #nosec G304 -- the test's own temp file
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

// TestCredsFix: the reason mailboxCreds gives becomes the check's fix, as a sentence.
func TestCredsFix(t *testing.T) {
	err := errors.Join(errors.New("the mailbox is paused: mailbox 3: re-enter the password: it was saved under a different key"), errors.New("db down"))
	if got, want := credsFix(err), "Re-enter the password: it was saved under a different key."; got != want {
		t.Errorf("credsFix = %q, want %q", got, want)
	}
	if got := credsFix(errors.New("odd")); got != "Re-enter the password." {
		t.Errorf("credsFix of an odd error = %q", got)
	}
}
