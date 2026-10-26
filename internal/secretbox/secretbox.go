// Package secretbox seals mailbox passwords with a public key so that only the jobs that
// send, sync and check mail can open them.
//
// A password is sealed (NaCl anonymous box: X25519 + XSalsa20-Poly1305) together with the
// four connection fields it may be used with, so a worker can refuse a password whose
// mailbox settings were changed after it was saved. The console holds only the public key;
// the private key reaches the workers as a systemd credential, or locally through
// LEADHOUND_MAILBOX_KEY_FILE.
package secretbox

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"golang.org/x/crypto/curve25519"
	"golang.org/x/crypto/nacl/box"

	"github.com/gremy/webbership-leadhound/internal/config"
)

const (
	// PublicKeyEnv holds the base64 public key the console and the CLI seal with. It is
	// not secret.
	PublicKeyEnv = "LEADHOUND_MAILBOX_PUBLIC_KEY"
	// KeyFileEnv names the private key file for local runs, where no systemd credential
	// exists. It must never reach the console.
	KeyFileEnv = "LEADHOUND_MAILBOX_KEY_FILE"
	// credentialName is the systemd credential the worker units load the private key as.
	credentialName = "mailbox-key"
)

var (
	errOpen    = errors.New("the password could not be opened with this key")
	errNoKey   = errors.New("this command needs the mailbox key; see deploy/README.md, Mailboxes")
	errKeySize = errors.New("a mailbox key must be 32 bytes, base64-encoded")
)

// PublicKey is an X25519 public key.
type PublicKey [32]byte

// PrivateKey is an X25519 private key.
type PrivateKey [32]byte

// GenerateKeyPair makes a new key pair from rand (crypto/rand.Reader when nil).
func GenerateKeyPair(r io.Reader) (PublicKey, PrivateKey, error) {
	if r == nil {
		r = rand.Reader
	}
	pub, priv, err := box.GenerateKey(r)
	if err != nil {
		return PublicKey{}, PrivateKey{}, fmt.Errorf("generate mailbox key: %w", err)
	}
	return *pub, *priv, nil
}

// Public derives the public key from the private one.
func (k PrivateKey) Public() PublicKey {
	var pub PublicKey
	out, err := curve25519.X25519(k[:], curve25519.Basepoint)
	if err != nil {
		// X25519 with the base point never yields the all-zero output that is its only
		// error; a zero key here can open nothing.
		return pub
	}
	copy(pub[:], out)
	return pub
}

// String is the base64 form written to PublicKeyEnv.
func (k PublicKey) String() string { return base64.StdEncoding.EncodeToString(k[:]) }

// Fingerprint is the first 16 hex characters of the SHA-256 of the public key. Each sealed
// password records the fingerprint of the key that sealed it.
func Fingerprint(k PublicKey) string {
	sum := sha256.Sum256(k[:])
	return hex.EncodeToString(sum[:])[:16]
}

// ParsePublicKey reads the base64 form of a public key.
func ParsePublicKey(b64 string) (PublicKey, error) {
	var k PublicKey
	if err := decodeKey(b64, k[:]); err != nil {
		return PublicKey{}, err
	}
	return k, nil
}

func decodeKey(b64 string, dst []byte) error {
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64))
	if err != nil || len(b) != len(dst) {
		return errKeySize
	}
	copy(dst, b)
	return nil
}

// Credentials is what a sealed password holds: the password and the connection fields it
// may be used with.
type Credentials struct {
	Address, Login, SMTPAddr, IMAPAddr string
	Password                           config.Secret
}

// bundle is the sealed JSON. Password is a plain string here because config.Secret
// marshals as "[redacted]".
type bundle struct {
	Address  string `json:"address"`
	Login    string `json:"login"`
	SMTPAddr string `json:"smtp_addr"`
	IMAPAddr string `json:"imap_addr"`
	Password string `json:"password"`
}

// Matches says whether the credentials were sealed for exactly these connection fields.
// Callers check it before using the password.
func (c Credentials) Matches(address, login, smtp, imap string) bool {
	return c.Address == address && c.Login == login && c.SMTPAddr == smtp && c.IMAPAddr == imap
}

// Seal seals the credentials to pub, using rand (crypto/rand.Reader when nil).
func Seal(pub PublicKey, c Credentials, r io.Reader) ([]byte, error) {
	plain, err := json.Marshal(bundle{
		Address: c.Address, Login: c.Login, SMTPAddr: c.SMTPAddr, IMAPAddr: c.IMAPAddr,
		Password: c.Password.Reveal(),
	})
	if err != nil {
		return nil, errors.New("seal the password: encode failed")
	}
	return sealRaw(pub, plain, r)
}

func sealRaw(pub PublicKey, plain []byte, r io.Reader) ([]byte, error) {
	if r == nil {
		r = rand.Reader
	}
	k := [32]byte(pub)
	out, err := box.SealAnonymous(nil, plain, &k, r)
	clear(plain)
	if err != nil {
		return nil, fmt.Errorf("seal the password: %w", err)
	}
	return out, nil
}

// Open opens a sealed password with priv. Its errors never carry the contents.
func Open(priv PrivateKey, sealed []byte) (Credentials, error) {
	pub := [32]byte(priv.Public())
	k := [32]byte(priv)
	plain, ok := box.OpenAnonymous(nil, sealed, &pub, &k)
	if !ok {
		return Credentials{}, errOpen
	}
	defer clear(plain)
	var b bundle
	if err := json.Unmarshal(plain, &b); err != nil {
		return Credentials{}, errors.New("the opened password is not a valid mailbox bundle")
	}
	return Credentials{
		Address: b.Address, Login: b.Login, SMTPAddr: b.SMTPAddr, IMAPAddr: b.IMAPAddr,
		Password: config.Secret(b.Password),
	}, nil
}

// ReadPrivateKey reads a base64 private key file. With checkMode it refuses a file that
// group or other can read (the check is skipped on Windows, which has no such bits).
func ReadPrivateKey(path string, checkMode bool) (PrivateKey, error) {
	if checkMode && runtime.GOOS != "windows" {
		fi, err := os.Stat(path)
		if err != nil {
			return PrivateKey{}, fmt.Errorf("mailbox key: %w", err)
		}
		if fi.Mode().Perm()&0o077 != 0 {
			return PrivateKey{}, fmt.Errorf("mailbox key %s is readable by group or other (mode %v); run chmod 600 on it", path, fi.Mode().Perm())
		}
	}
	b, err := os.ReadFile(path) // #nosec G304 -- the operator names the key file
	if err != nil {
		return PrivateKey{}, fmt.Errorf("mailbox key: %w", err)
	}
	defer clear(b)
	var k PrivateKey
	if err := decodeKey(string(b), k[:]); err != nil {
		return PrivateKey{}, fmt.Errorf("mailbox key %s: %w", path, err)
	}
	return k, nil
}

// LoadPrivateKey finds the private key: the systemd credential first (read without a mode
// check, because systemd hands it over group-readable with an ACL for the service user),
// then KeyFileEnv with the mode check.
func LoadPrivateKey(getenv func(string) string) (PrivateKey, error) {
	if dir := getenv("CREDENTIALS_DIRECTORY"); dir != "" {
		path := filepath.Join(dir, credentialName)
		if _, err := os.Stat(path); err == nil {
			return ReadPrivateKey(path, false)
		}
	}
	if path := getenv(KeyFileEnv); path != "" {
		return ReadPrivateKey(path, true)
	}
	return PrivateKey{}, errNoKey
}

// WritePrivateKey writes a new private key file, mode 0600, and refuses to overwrite one.
func WritePrivateKey(path string, k PrivateKey) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) // #nosec G304 -- the operator names the key file
	if err != nil {
		return fmt.Errorf("write mailbox key: %w", err)
	}
	_, werr := f.WriteString(base64.StdEncoding.EncodeToString(k[:]) + "\n")
	cerr := f.Close()
	if err := errors.Join(werr, cerr); err != nil {
		return fmt.Errorf("write mailbox key: %w", err)
	}
	return nil
}
