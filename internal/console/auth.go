package console

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// MinPasswordLen is the shortest password HashPassword accepts.
const MinPasswordLen = 12

const (
	// hashCost is the bcrypt cost HashPassword uses and the lowest NewAuth accepts.
	hashCost = 12
	// maxPasswordLen is bcrypt's limit: it reads only the first 72 bytes.
	maxPasswordLen = 72

	sessionTTL = 12 * time.Hour

	// deviceTTL is how long the trusted-device cookie lives. It is set again
	// on every successful sign-in.
	deviceTTL = 90 * 24 * time.Hour
	// deviceLabel separates the device key from any other use of the hash.
	deviceLabel = "leadhound console device v1\x00"
	deviceIDLen = 16

	ipAttempts = 5
	ipWindow   = time.Minute

	globalFailures = 100
	globalWindow   = 15 * time.Minute

	// maxTrackedIPs bounds the per-IP limiter map.
	maxTrackedIPs = 10000
)

// Auth checks the operator's sign-in, holds the sessions in memory and limits
// login attempts. A restart signs the operator out.
type Auth struct {
	email   [sha256.Size]byte // SHA-256 of the lower-cased, trimmed email
	hash    []byte
	now     func() time.Time
	compare func(hash, password []byte) error
	// deviceKey signs trusted-device cookies. It is derived from the password
	// hash, so a new password forgets every device.
	deviceKey [sha256.Size]byte

	mu       sync.Mutex
	sessions map[[sha256.Size]byte]time.Time // SHA-256 of the token → expiry
	attempts map[string][]time.Time          // IP → login attempts in the last ipWindow
	failures []time.Time                     // failed logins in the last globalWindow, oldest first
}

// NewAuth returns an Auth for one operator. It refuses an empty email, a hash
// bcrypt cannot read and a hash with a cost below 12. A nil now means time.Now.
func NewAuth(email, passwordHash string, now func() time.Time) (*Auth, error) {
	return newAuth(email, passwordHash, now, hashCost)
}

// newAuth is NewAuth with the cost floor as a parameter, so tests can use cheap hashes.
func newAuth(email, passwordHash string, now func() time.Time, minCost int) (*Auth, error) {
	email = foldEmail(email)
	if email == "" {
		return nil, errors.New("console: the operator email is empty")
	}
	cost, err := bcrypt.Cost([]byte(passwordHash))
	if err != nil {
		return nil, errors.New("console: the password hash is not a bcrypt hash")
	}
	if cost < minCost {
		return nil, fmt.Errorf("console: the password hash has cost %d, below %d", cost, minCost)
	}
	if now == nil {
		now = time.Now
	}
	return &Auth{
		email:     sha256.Sum256([]byte(email)),
		hash:      []byte(passwordHash),
		now:       now,
		compare:   bcrypt.CompareHashAndPassword,
		deviceKey: sha256.Sum256([]byte(deviceLabel + passwordHash)),
		sessions:  make(map[[sha256.Size]byte]time.Time),
		attempts:  make(map[string][]time.Time),
	}, nil
}

func foldEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

// Check reports whether email and password are the operator's. It compares
// fixed-size digests of the emails in constant time and always runs bcrypt,
// so a wrong email takes as long as a wrong password.
func (a *Auth) Check(email, password string) bool {
	got := sha256.Sum256([]byte(foldEmail(email)))
	emailOK := subtle.ConstantTimeCompare(a.email[:], got[:]) == 1
	passOK := a.compare(a.hash, []byte(password)) == nil
	// bcrypt ignores bytes past 72, so a longer password would match on its prefix.
	return emailOK && passOK && len(password) <= maxPasswordLen
}

// NewSession starts a session and returns its token: 32 random bytes in hex.
// Only the token's SHA-256 is kept. A session lasts 12 hours from now, however
// often it is used.
func (a *Auth) NewSession() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("console: new session: %w", err)
	}
	token := hex.EncodeToString(b)
	now := a.now()
	a.mu.Lock()
	defer a.mu.Unlock()
	for k, exp := range a.sessions {
		if !now.Before(exp) {
			delete(a.sessions, k)
		}
	}
	a.sessions[sha256.Sum256([]byte(token))] = now.Add(sessionTTL)
	return token, nil
}

// Valid reports whether token is a live session.
func (a *Auth) Valid(token string) bool {
	if token == "" {
		return false
	}
	key := sha256.Sum256([]byte(token))
	now := a.now()
	a.mu.Lock()
	defer a.mu.Unlock()
	exp, ok := a.sessions[key]
	if !ok {
		return false
	}
	if !now.Before(exp) {
		delete(a.sessions, key)
		return false
	}
	return true
}

// Revoke ends the session token, if there is one.
func (a *Auth) Revoke(token string) {
	if token == "" {
		return
	}
	key := sha256.Sum256([]byte(token))
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.sessions, key)
}

// NewDevice returns a trusted-device cookie value: 16 random bytes in hex, a
// dot, and the hex HMAC-SHA256 of those bytes under the device key. Nothing is
// stored; TrustedDevice checks the MAC.
func (a *Auth) NewDevice() (string, error) {
	id := make([]byte, deviceIDLen)
	if _, err := rand.Read(id); err != nil {
		return "", fmt.Errorf("console: new device: %w", err)
	}
	return hex.EncodeToString(id) + "." + hex.EncodeToString(a.deviceMAC(id)), nil
}

// TrustedDevice reports whether v is a cookie value NewDevice made under the
// current password hash. It compares the MAC in constant time.
func (a *Auth) TrustedDevice(v string) bool {
	idHex, macHex, ok := strings.Cut(v, ".")
	// Lower-case hex only, as NewDevice writes it, so one device has one value.
	if !ok || len(idHex) != 2*deviceIDLen || len(macHex) != 2*sha256.Size ||
		strings.ToLower(v) != v {
		return false
	}
	id, err := hex.DecodeString(idHex)
	if err != nil {
		return false
	}
	mac, err := hex.DecodeString(macHex)
	if err != nil {
		return false
	}
	return hmac.Equal(mac, a.deviceMAC(id))
}

func (a *Auth) deviceMAC(id []byte) []byte {
	m := hmac.New(sha256.New, a.deviceKey[:])
	m.Write(id)
	return m.Sum(nil)
}

// AllowLoginFrom reports whether a login attempt from ip may go ahead, and
// counts it when it may. It refuses when ip already made 5 attempts in the
// last minute, or, unless the device is trusted, when 100 logins failed in the
// last 15 minutes from anywhere. A trusted device skips that bound so nobody
// can lock the operator out by tripping it on purpose. IPv6 addresses share
// one count per /64.
// When 10,000 IPs have attempts in the last minute, a new IP is refused: an
// existing bucket is never dropped to make room, so nobody can clear their own
// limit by sending from many addresses.
func (a *Auth) AllowLoginFrom(ip string, trusted bool) bool {
	now := a.now()
	ip = limitKey(ip)
	a.mu.Lock()
	defer a.mu.Unlock()

	a.failures = pruneBefore(a.failures, now.Add(-globalWindow))
	if !trusted && len(a.failures) >= globalFailures {
		return false
	}

	cutoff := now.Add(-ipWindow)
	hits, tracked := a.attempts[ip]
	if !tracked && len(a.attempts) >= maxTrackedIPs {
		for k, ts := range a.attempts {
			if len(pruneBefore(ts, cutoff)) == 0 {
				delete(a.attempts, k)
			}
		}
		if len(a.attempts) >= maxTrackedIPs {
			return false
		}
	}
	hits = pruneBefore(hits, cutoff)
	if len(hits) >= ipAttempts {
		a.attempts[ip] = hits
		return false
	}
	a.attempts[ip] = append(hits, now)
	return true
}

// RecordFailure counts one failed login toward the 15-minute bound.
func (a *Auth) RecordFailure() {
	now := a.now()
	a.mu.Lock()
	defer a.mu.Unlock()
	a.failures = append(pruneBefore(a.failures, now.Add(-globalWindow)), now)
	// Only the newest globalFailures matter: the bound lifts when the oldest of them ages out.
	if n := len(a.failures); n > globalFailures {
		a.failures = append(a.failures[:0], a.failures[n-globalFailures:]...)
	}
}

// limitKey is the per-IP limiter's key for ip: an IPv6 address becomes its
// /64, since one client usually holds the whole /64. IPv4 addresses, IPv4
// addresses written in IPv6 form and anything that does not parse are kept as
// they are.
func limitKey(ip string) string {
	parsed := net.ParseIP(ip)
	if parsed == nil || parsed.To4() != nil {
		return ip
	}
	return parsed.Mask(net.CIDRMask(64, 128)).String()
}

// pruneBefore drops the times at or before cutoff from ts, which is oldest first.
func pruneBefore(ts []time.Time, cutoff time.Time) []time.Time {
	i := 0
	for i < len(ts) && !ts[i].After(cutoff) {
		i++
	}
	return ts[i:]
}

// ClientIP returns the client's address. Behind the local proxy (a loopback
// peer) it is the last X-Forwarded-For entry, the one the proxy wrote; from any
// other peer the header is ignored and the peer's own address is used.
func ClientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	peer := net.ParseIP(host)
	if peer == nil || !peer.IsLoopback() {
		return host
	}
	values := r.Header.Values("X-Forwarded-For")
	if len(values) == 0 {
		return host
	}
	entries := strings.Split(values[len(values)-1], ",")
	last := strings.TrimSpace(entries[len(entries)-1])
	if ip := net.ParseIP(last); ip != nil {
		return ip.String()
	}
	return host
}

// HashPassword returns the bcrypt hash, at cost 12, of a password of 12 to 72 bytes.
func HashPassword(password []byte) (string, error) {
	if len(password) < MinPasswordLen {
		return "", fmt.Errorf("console: the password is shorter than %d bytes", MinPasswordLen)
	}
	if len(password) > maxPasswordLen {
		return "", fmt.Errorf("console: the password is longer than %d bytes", maxPasswordLen)
	}
	h, err := bcrypt.GenerateFromPassword(password, hashCost)
	if err != nil {
		return "", fmt.Errorf("console: hash the password: %w", err)
	}
	return string(h), nil
}
