package console

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const (
	testEmail    = "operator@mail.example"
	testPassword = "correct horse battery"
)

// fakeClock is a settable clock for the session and limiter windows.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// minCostHash hashes testPassword at bcrypt.MinCost, which NewAuth refuses;
// tests reach it through newAuth with the floor lowered.
func minCostHash(t *testing.T) string {
	t.Helper()
	h, err := bcrypt.GenerateFromPassword([]byte(testPassword), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	return string(h)
}

// testAuth builds an Auth over a cheap hash and a fake clock, and counts
// every bcrypt comparison it makes.
func testAuth(t *testing.T) (*Auth, *fakeClock, *int) {
	t.Helper()
	clock := newFakeClock()
	a, err := newAuth(testEmail, minCostHash(t), clock.now, bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	compare := a.compare
	a.compare = func(hash, password []byte) error {
		calls++
		return compare(hash, password)
	}
	return a, clock, &calls
}

func TestCheck(t *testing.T) {
	tests := []struct {
		name, email, password string
		want                  bool
	}{
		{"right email and password", testEmail, testPassword, true},
		{"wrong email", "someone@mail.example", testPassword, false},
		{"wrong password", testEmail, "correct horse battery!", false},
		{"both wrong", "someone@mail.example", "nope", false},
		{"empty email", "", testPassword, false},
		{"empty password", testEmail, "", false},
		{"email upper case", "OPERATOR@Mail.Example", testPassword, true},
		{"email with spaces", "  operator@mail.example\t", testPassword, true},
		{"password is case-sensitive", testEmail, "Correct horse battery", false},
		{"password is not trimmed", testEmail, " " + testPassword, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, _, calls := testAuth(t)
			if got := a.Check(tt.email, tt.password); got != tt.want {
				t.Errorf("Check(%q, %q) = %v, want %v", tt.email, tt.password, got, tt.want)
			}
			if *calls != 1 {
				t.Errorf("bcrypt ran %d times, want exactly 1 (it runs whatever the email)", *calls)
			}
		})
	}
}

func TestNewAuthEmailFoldedAtConstruction(t *testing.T) {
	clock := newFakeClock()
	a, err := newAuth("  Operator@MAIL.example ", minCostHash(t), clock.now, bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	if !a.Check(testEmail, testPassword) {
		t.Error("a configured email in mixed case with spaces should match its plain form")
	}
}

func TestNewAuthRefusals(t *testing.T) {
	cost11, err := bcrypt.GenerateFromPassword([]byte(testPassword), 11)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name, email, hash, want string
	}{
		{"empty email", "", minCostHash(t), "email"},
		{"blank email", "   ", minCostHash(t), "email"},
		{"empty hash", testEmail, "", "hash"},
		{"not a bcrypt hash", testEmail, "plain-text-password", "hash"},
		{"truncated hash", testEmail, "$2a$12$abc", "hash"},
		{"cost 4", testEmail, minCostHash(t), "cost"},
		{"cost 11", testEmail, string(cost11), "cost"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, err := NewAuth(tt.email, tt.hash, nil)
			if err == nil {
				t.Fatalf("NewAuth accepted it: %v", a)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q does not mention %q", err, tt.want)
			}
			if tt.hash != "" && strings.Contains(err.Error(), tt.hash) {
				t.Errorf("error %q repeats the hash", err)
			}
		})
	}
}

func TestNewAuthAcceptsCost12(t *testing.T) {
	t.Parallel() // cost 12 is slow under -race
	h, err := HashPassword([]byte(testPassword))
	if err != nil {
		t.Fatal(err)
	}
	a, err := NewAuth(testEmail, h, nil)
	if err != nil {
		t.Fatalf("NewAuth(cost 12) = %v", err)
	}
	if !a.Check(testEmail, testPassword) {
		t.Error("Check refused the right email and password")
	}
}

func TestHashPassword(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		password string
		ok       bool
	}{
		{"empty", "", false},
		{"11 bytes", strings.Repeat("a", MinPasswordLen-1), false},
		{"12 bytes", strings.Repeat("a", MinPasswordLen), true},
		{"72 bytes", strings.Repeat("b", 72), true},
		{"73 bytes", strings.Repeat("c", 73), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h, err := HashPassword([]byte(tt.password))
			if !tt.ok {
				if err == nil {
					t.Fatalf("HashPassword accepted %d bytes", len(tt.password))
				}
				return
			}
			if err != nil {
				t.Fatalf("HashPassword = %v", err)
			}
			cost, err := bcrypt.Cost([]byte(h))
			if err != nil {
				t.Fatal(err)
			}
			if cost != 12 {
				t.Errorf("cost = %d, want 12", cost)
			}
			if err := bcrypt.CompareHashAndPassword([]byte(h), []byte(tt.password)); err != nil {
				t.Errorf("round trip: %v", err)
			}
		})
	}
}

func TestSessions(t *testing.T) {
	a, clock, _ := testAuth(t)
	tok, err := a.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	if len(tok) != 64 {
		t.Errorf("token length = %d, want 64 hex characters", len(tok))
	}
	other, err := a.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	if other == tok {
		t.Fatal("two sessions share a token")
	}
	if !a.Valid(tok) {
		t.Fatal("a new session is not valid")
	}
	if a.Valid("") || a.Valid(strings.Repeat("0", 64)) || a.Valid(tok[:63]) {
		t.Error("an unknown token is valid")
	}

	// Using a session does not extend it: still valid one second before 12h, gone at 12h.
	clock.advance(6 * time.Hour)
	if !a.Valid(tok) {
		t.Fatal("session gone after 6h")
	}
	clock.advance(6*time.Hour - time.Second)
	if !a.Valid(tok) {
		t.Fatal("session gone before 12h")
	}
	clock.advance(time.Second)
	if a.Valid(tok) {
		t.Error("session still valid at 12h")
	}

	// Revoke ends a live session and leaves the others alone.
	s1, _ := a.NewSession()
	s2, _ := a.NewSession()
	a.Revoke(s1)
	if a.Valid(s1) {
		t.Error("revoked session still valid")
	}
	if !a.Valid(s2) {
		t.Error("revoking one session ended another")
	}
	a.Revoke("")
	a.Revoke("not-a-token")
}

func TestSessionsStoreDigestNotToken(t *testing.T) {
	a, _, _ := testAuth(t)
	tok, err := a.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for k := range a.sessions {
		if strings.Contains(fmt.Sprintf("%x", k), tok) || fmt.Sprint(k) == tok {
			t.Error("the session map holds the token itself")
		}
	}
}

func TestSessionsEvictExpiredOnWrite(t *testing.T) {
	a, clock, _ := testAuth(t)
	for range 3 {
		if _, err := a.NewSession(); err != nil {
			t.Fatal(err)
		}
	}
	clock.advance(12 * time.Hour)
	if _, err := a.NewSession(); err != nil {
		t.Fatal(err)
	}
	a.mu.Lock()
	n := len(a.sessions)
	a.mu.Unlock()
	if n != 1 {
		t.Errorf("%d sessions held after the others expired, want 1", n)
	}
}

func TestAllowLoginPerIP(t *testing.T) {
	a, clock, _ := testAuth(t)
	const ip = "192.0.2.10"
	for i := 1; i <= 5; i++ {
		if !a.AllowLoginFrom(ip, false) {
			t.Fatalf("attempt %d refused", i)
		}
		clock.advance(time.Second)
	}
	if a.AllowLoginFrom(ip, false) {
		t.Fatal("6th attempt in a minute allowed")
	}
	if !a.AllowLoginFrom("192.0.2.11", false) {
		t.Error("another IP was refused because of the first")
	}
	// The first attempt was at t0 and the clock is at t0+5s. At t0+1m it leaves the window.
	clock.advance(54 * time.Second)
	if a.AllowLoginFrom(ip, false) {
		t.Fatal("allowed before the window passed")
	}
	clock.advance(time.Second)
	if !a.AllowLoginFrom(ip, false) {
		t.Error("still refused once the oldest attempt left the window")
	}
}

func TestAllowLoginGlobalFailures(t *testing.T) {
	a, clock, _ := testAuth(t)
	for i := range 100 {
		ip := fmt.Sprintf("192.0.2.%d", i+1)
		if !a.AllowLoginFrom(ip, false) {
			t.Fatalf("attempt from %s refused before the bound", ip)
		}
		a.RecordFailure()
		clock.advance(time.Second)
	}
	if a.AllowLoginFrom("192.0.2.200", false) {
		t.Fatal("a new IP was allowed after 100 failures in 15 minutes")
	}
	// The first failure was at t0, 100s ago; it ages out at t0+15m.
	clock.advance(15*time.Minute - 101*time.Second)
	if a.AllowLoginFrom("192.0.2.200", false) {
		t.Fatal("allowed before the first failure aged out")
	}
	clock.advance(time.Second)
	if !a.AllowLoginFrom("192.0.2.200", false) {
		t.Error("still refused once a failure aged out")
	}
}

func TestAllowLoginSuccessesNeverTripGlobal(t *testing.T) {
	a, clock, _ := testAuth(t)
	for i := range 100 {
		ip := fmt.Sprintf("192.0.2.%d", i+1)
		if !a.AllowLoginFrom(ip, false) {
			t.Fatalf("login %d refused", i+1)
		}
		if !a.Check(testEmail, testPassword) {
			t.Fatal("Check refused the right password")
		}
		clock.advance(time.Second)
	}
	if !a.AllowLoginFrom("192.0.2.200", false) {
		t.Error("100 successful logins tripped the global bound")
	}
}

// At the cap, a new IP is refused rather than an old bucket dropped: dropping
// one would let a sender with many addresses wipe its own limit.
func TestAllowLoginIPCap(t *testing.T) {
	a, clock, _ := testAuth(t)
	const first = "192.0.2.1"
	for range 5 {
		a.AllowLoginFrom(first, false)
	}
	for i := 1; i < maxTrackedIPs; i++ {
		if !a.AllowLoginFrom(fmt.Sprintf("ip-%d", i), false) {
			t.Fatalf("IP %d refused below the cap", i)
		}
	}
	if a.AllowLoginFrom("192.0.2.2", false) {
		t.Fatal("a new IP was allowed with the map full of live buckets")
	}
	if a.AllowLoginFrom(first, false) {
		t.Fatal("a full bucket was reset by the flood")
	}
	a.mu.Lock()
	n := len(a.attempts)
	a.mu.Unlock()
	if n > maxTrackedIPs {
		t.Fatalf("tracking %d IPs, cap is %d", n, maxTrackedIPs)
	}
	// Once the window passes, the stale buckets are pruned and new IPs get in.
	clock.advance(time.Minute)
	if !a.AllowLoginFrom("192.0.2.2", false) {
		t.Error("a new IP is still refused after every bucket went stale")
	}
}

func TestAuthConcurrentUse(t *testing.T) {
	a, _, _ := testAuth(t)
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			ip := fmt.Sprintf("192.0.2.%d", i+1)
			for range 20 {
				a.AllowLoginFrom(ip, false)
				a.RecordFailure()
				tok, err := a.NewSession()
				if err != nil {
					t.Error(err)
					return
				}
				a.Valid(tok)
				a.Revoke(tok)
			}
		})
	}
	wg.Wait()
}

func TestClientIP(t *testing.T) {
	tests := []struct {
		name, remote string
		xff          []string
		want         string
	}{
		{"loopback peer uses the last entry", "127.0.0.1:5000", []string{"192.0.2.1, 192.0.2.9"}, "192.0.2.9"},
		{"loopback peer, one entry", "127.0.0.1:5000", []string{"192.0.2.7"}, "192.0.2.7"},
		{"loopback peer, header repeated", "127.0.0.1:5000", []string{"192.0.2.1", "192.0.2.3"}, "192.0.2.3"},
		{"IPv6 loopback peer", "[::1]:5000", []string{"192.0.2.4"}, "192.0.2.4"},
		{"loopback peer, IPv6 client", "127.0.0.1:5000", []string{"2001:db8::1"}, "2001:db8::1"},
		{"loopback peer, no header", "127.0.0.1:5000", nil, "127.0.0.1"},
		{"loopback peer, garbage header", "127.0.0.1:5000", []string{"not-an-address"}, "127.0.0.1"},
		{"loopback peer, trailing comma", "127.0.0.1:5000", []string{"192.0.2.1,"}, "127.0.0.1"},
		{"other peer ignores the header", "192.0.2.50:443", []string{"192.0.2.9"}, "192.0.2.50"},
		{"other IPv6 peer", "[2001:db8::5]:443", []string{"192.0.2.9"}, "2001:db8::5"},
		{"peer without a port", "192.0.2.51", nil, "192.0.2.51"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "/login", nil)
			r.RemoteAddr = tt.remote
			for _, v := range tt.xff {
				r.Header.Add("X-Forwarded-For", v)
			}
			if got := ClientIP(r); got != tt.want {
				t.Errorf("ClientIP = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDeviceRoundTrip(t *testing.T) {
	a, _, _ := testAuth(t)
	v, err := a.NewDevice()
	if err != nil {
		t.Fatal(err)
	}
	if !a.TrustedDevice(v) {
		t.Fatalf("TrustedDevice(%q) = false for a value NewDevice made", v)
	}
	w, err := a.NewDevice()
	if err != nil {
		t.Fatal(err)
	}
	if w == v {
		t.Fatal("two devices share a value")
	}
	// The value carries no secret: id and MAC are hex, joined by one dot.
	id, mac, ok := strings.Cut(v, ".")
	if !ok || len(id) != 32 || len(mac) != 64 {
		t.Errorf("value %q is not 32 hex, a dot and 64 hex", v)
	}
}

func TestDeviceSurvivesANewAuthWithTheSameHash(t *testing.T) {
	clock := newFakeClock()
	h := minCostHash(t)
	a, err := newAuth(testEmail, h, clock.now, bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	v, err := a.NewDevice()
	if err != nil {
		t.Fatal(err)
	}
	// A restart builds a new Auth from the same env file.
	b, err := newAuth(testEmail, h, clock.now, bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	if !b.TrustedDevice(v) {
		t.Error("a device was forgotten across a restart with the same password hash")
	}
}

func TestDeviceRefusals(t *testing.T) {
	a, _, _ := testAuth(t)
	v, err := a.NewDevice()
	if err != nil {
		t.Fatal(err)
	}
	id, mac, _ := strings.Cut(v, ".")

	// A new password is a new hash (a new salt even for the same password).
	other, _, _ := testAuth(t)
	fromOther, err := other.NewDevice()
	if err != nil {
		t.Fatal(err)
	}

	flip := func(s string) string {
		c := byte('0')
		if s[0] == '0' {
			c = '1'
		}
		return string(c) + s[1:]
	}
	tests := []struct{ name, value string }{
		{"tampered id", flip(id) + "." + mac},
		{"tampered MAC", id + "." + flip(mac)},
		{"made with a different password hash", fromOther},
		{"empty", ""},
		{"no dot", id + mac},
		{"two dots", id + "." + mac + "."},
		{"empty id", "." + mac},
		{"empty MAC", id + "."},
		{"non-hex id", strings.Repeat("z", 32) + "." + mac},
		{"non-hex MAC", id + "." + strings.Repeat("z", 64)},
		{"short MAC", id + "." + mac[:62]},
		{"short id", id[:30] + "." + mac},
		{"upper-case hex", strings.ToUpper(id) + "." + strings.ToUpper(mac)},
		{"session-shaped token", strings.Repeat("a", 64)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if a.TrustedDevice(tt.value) {
				t.Errorf("TrustedDevice(%q) = true", tt.value)
			}
		})
	}
}

// The global failure bound can be tripped on purpose; a trusted device still
// gets in, an untrusted one does not.
func TestAllowLoginFromTrustedSkipsGlobal(t *testing.T) {
	a, _, _ := testAuth(t)
	for i := range 100 {
		if !a.AllowLoginFrom(fmt.Sprintf("192.0.2.%d", i+1), false) {
			t.Fatalf("attempt %d refused before the bound", i+1)
		}
		a.RecordFailure()
	}
	if a.AllowLoginFrom("192.0.2.201", false) {
		t.Fatal("an untrusted device was allowed after 100 failures")
	}
	if !a.AllowLoginFrom("192.0.2.202", true) {
		t.Error("a trusted device from a fresh IP was refused by the global bound")
	}
}

func TestAllowLoginFromTrustedKeepsPerIPLimit(t *testing.T) {
	a, _, _ := testAuth(t)
	const ip = "192.0.2.10"
	for i := 1; i <= 5; i++ {
		if !a.AllowLoginFrom(ip, true) {
			t.Fatalf("attempt %d refused", i)
		}
	}
	if a.AllowLoginFrom(ip, true) {
		t.Error("a trusted device's 6th attempt in a minute was allowed")
	}
}

func TestAllowLoginFromTrustedKeepsIPCap(t *testing.T) {
	a, _, _ := testAuth(t)
	for i := range maxTrackedIPs {
		if !a.AllowLoginFrom(fmt.Sprintf("ip-%d", i), false) {
			t.Fatalf("IP %d refused below the cap", i)
		}
	}
	if a.AllowLoginFrom("192.0.2.2", true) {
		t.Error("a trusted device from a new IP was allowed with the map full of live buckets")
	}
}

func TestAllowLoginIPv6SharesSlash64(t *testing.T) {
	a, _, _ := testAuth(t)
	for i := 1; i <= 5; i++ {
		ip := fmt.Sprintf("2001:db8:0:1::%x", i)
		if !a.AllowLoginFrom(ip, false) {
			t.Fatalf("attempt %d from %s refused", i, ip)
		}
	}
	if a.AllowLoginFrom("2001:db8:0:1:ffff:ffff:ffff:ffff", false) {
		t.Error("a 6th attempt from the same /64 was allowed")
	}
	if !a.AllowLoginFrom("2001:db8:0:2::1", false) {
		t.Error("an address in another /64 was refused")
	}
}

func TestLimitKey(t *testing.T) {
	tests := []struct{ ip, want string }{
		{"192.0.2.1", "192.0.2.1"},
		{"192.0.2.2", "192.0.2.2"},
		{"2001:db8:0:1::1", "2001:db8:0:1::"},
		{"2001:db8:0:1:aaaa:bbbb:cccc:dddd", "2001:db8:0:1::"},
		{"2001:db8:0:2::1", "2001:db8:0:2::"},
		{"::ffff:192.0.2.3", "::ffff:192.0.2.3"},
		{"not-an-address", "not-an-address"},
		{"", ""},
	}
	for _, tt := range tests {
		if got := limitKey(tt.ip); got != tt.want {
			t.Errorf("limitKey(%q) = %q, want %q", tt.ip, got, tt.want)
		}
	}
}

func TestAllowLoginIPv4Unaffected(t *testing.T) {
	a, _, _ := testAuth(t)
	for range 5 {
		if !a.AllowLoginFrom("192.0.2.1", false) {
			t.Fatal("attempt refused below the limit")
		}
	}
	if a.AllowLoginFrom("192.0.2.1", false) {
		t.Error("6th attempt allowed")
	}
	if !a.AllowLoginFrom("192.0.2.2", false) {
		t.Error("a neighbouring IPv4 address shares the bucket")
	}
}

// The device key is SHA-256 of a fixed label and the password hash, so a
// cookie stays valid across restarts and dies with the hash.
func TestDeviceMACIsKeyedByTheHash(t *testing.T) {
	clock := newFakeClock()
	h := minCostHash(t)
	a, err := newAuth(testEmail, h, clock.now, bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	id := []byte("0123456789abcdef")
	key := sha256.Sum256([]byte("leadhound console device v1\x00" + h))
	m := hmac.New(sha256.New, key[:])
	m.Write(id)
	v := hex.EncodeToString(id) + "." + hex.EncodeToString(m.Sum(nil))
	if !a.TrustedDevice(v) {
		t.Error("a value built from the documented key was not trusted")
	}
}
