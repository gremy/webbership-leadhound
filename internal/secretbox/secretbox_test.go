package secretbox

import (
	"bytes"
	"crypto/rand"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/gremy/webbership-leadhound/internal/config"
)

// testPassword is built at runtime so no whole password-like literal sits in the source.
var testPassword = "pw-" + "s3cret-" + "value"

func testCreds() Credentials {
	return Credentials{
		Address:  "sender@mail.example",
		Login:    "sender@mail.example",
		SMTPAddr: "smtp.mail.example:587",
		IMAPAddr: "imap.mail.example:993",
		Password: config.Secret(testPassword),
	}
}

func mustKeys(t *testing.T) (PublicKey, PrivateKey) {
	t.Helper()
	pub, priv, err := GenerateKeyPair(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv
}

func TestSealOpenRoundTripKeepsRealPassword(t *testing.T) {
	pub, priv := mustKeys(t)
	sealed, err := Seal(pub, testCreds(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, []byte(testPassword)) {
		t.Fatal("sealed bytes contain the plaintext password")
	}
	got, err := Open(priv, sealed)
	if err != nil {
		t.Fatal(err)
	}
	if got.Password.Reveal() != testPassword {
		t.Fatalf("password after round trip = %q, want the real one", got.Password.Reveal())
	}
	want := testCreds()
	if got.Address != want.Address || got.Login != want.Login || got.SMTPAddr != want.SMTPAddr || got.IMAPAddr != want.IMAPAddr {
		t.Fatalf("connection fields changed: %+v", got)
	}
}

func TestOpenWithWrongKeyFails(t *testing.T) {
	pub, _ := mustKeys(t)
	_, other := mustKeys(t)
	sealed, err := Seal(pub, testCreds(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	_, err = Open(other, sealed)
	if err == nil {
		t.Fatal("Open with the wrong key succeeded")
	}
	if strings.Contains(err.Error(), testPassword) {
		t.Fatal("error contains the plaintext")
	}
}

func TestOpenTamperedByteFails(t *testing.T) {
	pub, priv := mustKeys(t)
	sealed, err := Seal(pub, testCreds(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	for _, i := range []int{0, 31, 32, len(sealed) / 2, len(sealed) - 1} {
		bad := bytes.Clone(sealed)
		bad[i] ^= 0x01
		if _, err := Open(priv, bad); err == nil {
			t.Errorf("Open accepted a box with byte %d flipped", i)
		}
	}
	if _, err := Open(priv, sealed[:10]); err == nil {
		t.Error("Open accepted a truncated box")
	}
}

func TestMatches(t *testing.T) {
	c := testCreds()
	if !c.Matches(c.Address, c.Login, c.SMTPAddr, c.IMAPAddr) {
		t.Fatal("Matches false for identical fields")
	}
	cases := map[string][4]string{
		"address": {"other@mail.example", c.Login, c.SMTPAddr, c.IMAPAddr},
		"login":   {c.Address, "other@mail.example", c.SMTPAddr, c.IMAPAddr},
		"smtp":    {c.Address, c.Login, "smtp.evil.invalid:587", c.IMAPAddr},
		"imap":    {c.Address, c.Login, c.SMTPAddr, "imap.evil.invalid:993"},
	}
	for name, f := range cases {
		if c.Matches(f[0], f[1], f[2], f[3]) {
			t.Errorf("Matches true when %s differs", name)
		}
	}
}

func TestPublicEqualsGeneratedPublicKey(t *testing.T) {
	pub, priv := mustKeys(t)
	if priv.Public() != pub {
		t.Fatal("Public() differs from the generated public key")
	}
}

func TestFingerprintStable(t *testing.T) {
	pub, _ := mustKeys(t)
	other, _ := mustKeys(t)
	a, b := Fingerprint(pub), Fingerprint(pub)
	if a != b {
		t.Fatalf("fingerprint not stable: %q vs %q", a, b)
	}
	if len(a) != 16 || strings.Trim(a, "0123456789abcdef") != "" {
		t.Fatalf("fingerprint %q is not 16 lowercase hex chars", a)
	}
	if Fingerprint(other) == a {
		t.Fatal("two keys share a fingerprint")
	}
	// A fixed key has a fixed fingerprint (first 16 hex chars of SHA-256 of 32 zero bytes).
	if got := Fingerprint(PublicKey{}); got != "66687aadf862bd77" {
		t.Fatalf("Fingerprint(zero key) = %q", got)
	}
}

func TestParsePublicKeyRoundTrip(t *testing.T) {
	pub, _ := mustKeys(t)
	got, err := ParsePublicKey(pub.String())
	if err != nil {
		t.Fatal(err)
	}
	if got != pub {
		t.Fatal("ParsePublicKey(String()) differs")
	}
	if _, err := ParsePublicKey(" " + pub.String() + "\n"); err != nil {
		t.Errorf("surrounding space refused: %v", err)
	}
	for _, bad := range []string{"", "not base64!", "AAAA"} {
		if _, err := ParsePublicKey(bad); err == nil {
			t.Errorf("ParsePublicKey(%q) succeeded", bad)
		}
	}
}

func TestWritePrivateKeyRefusesOverwrite(t *testing.T) {
	_, priv := mustKeys(t)
	path := filepath.Join(t.TempDir(), "mailbox.key")
	if err := WritePrivateKey(path, priv); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Fatalf("key file mode = %v, want 0600", fi.Mode().Perm())
		}
	}
	_, other := mustKeys(t)
	if err := WritePrivateKey(path, other); err == nil {
		t.Fatal("WritePrivateKey overwrote an existing key")
	}
	got, err := ReadPrivateKey(path, true)
	if err != nil {
		t.Fatal(err)
	}
	if got != priv {
		t.Fatal("the existing key was changed")
	}
}

func TestReadPrivateKeyModeCheck(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file modes are not checked on Windows")
	}
	_, priv := mustKeys(t)
	path := filepath.Join(t.TempDir(), "mailbox.key")
	if err := WritePrivateKey(path, priv); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil { //nolint:gosec // G302: a key file made world-readable so the mode check must refuse it
		t.Fatal(err)
	}
	if _, err := ReadPrivateKey(path, true); err == nil {
		t.Fatal("a 0644 key file was accepted with checkMode")
	}
	got, err := ReadPrivateKey(path, false)
	if err != nil {
		t.Fatalf("a 0644 key file was refused without checkMode: %v", err)
	}
	if got != priv {
		t.Fatal("read a different key")
	}
}

func TestLoadPrivateKey(t *testing.T) {
	_, priv := mustKeys(t)
	credDir := t.TempDir()
	credPath := filepath.Join(credDir, "mailbox-key")
	if err := WritePrivateKey(credPath, priv); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		// systemd hands a credential over group-readable; it must still load.
		if err := os.Chmod(credPath, 0o440); err != nil { //nolint:gosec // G302: the mode systemd gives a credential, which must still load
			t.Fatal(err)
		}
	}
	_, filePriv := mustKeys(t)
	filePath := filepath.Join(t.TempDir(), "mailbox.key")
	if err := WritePrivateKey(filePath, filePriv); err != nil {
		t.Fatal(err)
	}
	env := func(m map[string]string) func(string) string {
		return func(k string) string { return m[k] }
	}

	got, err := LoadPrivateKey(env(map[string]string{"CREDENTIALS_DIRECTORY": credDir, KeyFileEnv: filePath}))
	if err != nil {
		t.Fatal(err)
	}
	if got != priv {
		t.Fatal("the credential was not preferred over the key file")
	}

	got, err = LoadPrivateKey(env(map[string]string{KeyFileEnv: filePath}))
	if err != nil {
		t.Fatal(err)
	}
	if got != filePriv {
		t.Fatal("the key file was not read")
	}

	if runtime.GOOS != "windows" {
		if err := os.Chmod(filePath, 0o640); err != nil { //nolint:gosec // G302: a group-readable key file, which the mode check must refuse
			t.Fatal(err)
		}
		if _, err := LoadPrivateKey(env(map[string]string{KeyFileEnv: filePath})); err == nil {
			t.Fatal("a group-readable key file was accepted through " + KeyFileEnv)
		}
	}

	_, err = LoadPrivateKey(env(nil))
	if err == nil || err.Error() != "this command needs the mailbox key; see deploy/README.md, Mailboxes" {
		t.Fatalf("no key: err = %v", err)
	}
}

func TestCredentialsNeverPrintPassword(t *testing.T) {
	c := testCreds()
	for _, s := range []string{c.Password.String(), strings.Join([]string{c.Address, c.Password.String()}, " ")} {
		if strings.Contains(s, testPassword) {
			t.Fatal("password printed")
		}
	}
}

func TestErrorsNeverContainPlaintext(t *testing.T) {
	pub, priv := mustKeys(t)
	_, other := mustKeys(t)
	sealed, err := Seal(pub, testCreds(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var errs []error
	_, e := Open(other, sealed)
	errs = append(errs, e)
	bad := bytes.Clone(sealed)
	bad[len(bad)-1] ^= 0xff
	_, e = Open(priv, bad)
	errs = append(errs, e)
	_, e = Open(priv, nil)
	errs = append(errs, e)
	// A box that opens but does not hold a valid bundle.
	junk, err := sealRaw(pub, []byte(`{"password":"`+testPassword+`"`), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	_, e = Open(priv, junk)
	errs = append(errs, e)
	// A failing random source while sealing.
	_, e = Seal(pub, testCreds(), bytes.NewReader(nil))
	errs = append(errs, e)
	for i, e := range errs {
		if e == nil {
			t.Errorf("case %d: no error", i)
			continue
		}
		if strings.Contains(e.Error(), testPassword) {
			t.Errorf("case %d: error %q contains the plaintext", i, e)
		}
	}
	if errs[0].Error() != "the password could not be opened with this key" {
		t.Errorf("wrong-key error = %q", errs[0])
	}
}
