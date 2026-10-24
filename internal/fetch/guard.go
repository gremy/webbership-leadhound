package fetch

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"sync"
	"syscall"
	"time"
)

// ErrNotPublic is returned when WithPublicOnly refuses a URL or a dial: a port other than
// 80 or 443, or an address that is not public. It is wrapped with the URL or address.
var ErrNotPublic = errors.New("fetch: not a public address on port 80 or 443")

// WithPublicOnly turns on the SSRF guard. checkURL then refuses, for the first request and
// every redirect hop, a port other than 80 or 443 and any literal IP that is not public; and
// the client dials through the guarded transport, whose dialer refuses a non-public address
// after DNS resolution, so a name that resolves to a private address is refused too.
//
// The guard composes with the live gate (both must allow). WithFixtures still bypasses the
// network, but checkURL's guard part applies to fixture URLs; WithTransport replaces the
// guarded transport, so tests can reach httptest's loopback server through checkURL alone.
func WithPublicOnly() Option { return func(c *Client) { c.publicOnly = true } }

// checkPublic is checkURL's guard part: the port, and the host when it is a literal IP.
func checkPublic(u *url.URL) error {
	if p := u.Port(); p != "" && p != "80" && p != "443" {
		return fmt.Errorf("%w: %s (port %s)", ErrNotPublic, u.Redacted(), p)
	}
	if ip, err := netip.ParseAddr(u.Hostname()); err == nil && !isPublic(ip) {
		return fmt.Errorf("%w: %s", ErrNotPublic, u.Redacted())
	}
	return nil
}

// refusedPrefixes are the ranges isPublic refuses on top of IsGlobalUnicast's own rule
// (which already refuses loopback, link-local, multicast and the unspecified address).
var refusedPrefixes = []netip.Prefix{
	prefix4(0, 0, 8),               // 0/8
	prefix4(10, 0, 8),              // 10/8
	prefix4(100, 64, 10),           // CGNAT 100.64/10
	prefix4(127, 0, 8),             // loopback 127/8
	prefix4(169, 254, 16),          // link-local 169.254/16
	prefix4(172, 16, 12),           // 172.16/12
	prefix4(192, 168, 16),          // 192.168/16
	prefix4(198, 18, 15),           // benchmarking 198.18/15
	prefix4(224, 0, 4),             // multicast 224/4
	prefix4(240, 0, 4),             // reserved 240/4
	netip.MustParsePrefix("::/96"), // IPv4-compatible, deprecated
	netip.MustParsePrefix("::1/128"),
	netip.MustParsePrefix("64:ff9b::/96"),
	netip.MustParsePrefix("64:ff9b:1::/48"), // local-use NAT64
	netip.MustParsePrefix("2002::/16"),
	netip.MustParsePrefix("fc00::/7"),
	netip.MustParsePrefix("fe80::/10"),
	netip.MustParsePrefix("ff00::/8"),
}

// prefix4 builds the IPv4 prefix a.b.0.0/bits (every refused range ends in two zero octets).
func prefix4(a, b byte, bits int) netip.Prefix {
	return netip.PrefixFrom(netip.AddrFrom4([4]byte{a, b, 0, 0}), bits)
}

// isPublic reports whether a may be dialled with the guard on: after Unmap it must be a
// global unicast address outside every refused range. An IPv4-mapped address is judged as
// the IPv4 address it maps; a zone is ignored.
func isPublic(a netip.Addr) bool {
	a = a.Unmap().WithZone("")
	if !a.IsValid() || !a.IsGlobalUnicast() {
		return false
	}
	for _, p := range refusedPrefixes {
		if p.Contains(a) {
			return false
		}
	}
	return true
}

// guardControl is the guarded dialer's Control: it runs after DNS resolution with the
// "ip:port" about to be dialled, and refuses an address that is not public.
func guardControl(_, address string, _ syscall.RawConn) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return fmt.Errorf("%w: %q: %w", ErrNotPublic, address, err)
	}
	if !isPublic(ap.Addr()) {
		return fmt.Errorf("%w: %s", ErrNotPublic, address)
	}
	return nil
}

var (
	guardedOnce sync.Once
	guarded     *http.Transport
)

// guardedTransport is the one guarded transport of the process, so every per-site client
// shares its connection pools.
func guardedTransport() *http.Transport {
	guardedOnce.Do(func() { guarded = newGuardedTransport(http.DefaultTransport) })
	return guarded
}

// newGuardedTransport builds a transport with base's settings when base is an
// *http.Transport, else with net/http's documented DefaultTransport settings (a test may
// have wrapped http.DefaultTransport). Either way it has no proxy (a proxy would dial in our
// place, out of the guard's sight) and a dialer that runs guardControl.
func newGuardedTransport(base http.RoundTripper) *http.Transport {
	var t *http.Transport
	if bt, ok := base.(*http.Transport); ok {
		t = bt.Clone()
	} else {
		t = &http.Transport{
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          100,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		}
	}
	t.Proxy = nil
	d := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second, Control: guardControl}
	t.DialContext = d.DialContext
	return t
}
