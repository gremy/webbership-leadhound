package fetch

import (
	"fmt"
	"net/netip"
	"net/url"
	"strings"

	"github.com/gremy/webbership-leadhound/internal/config"
)

// checkURL is the one place a URL is judged before it is requested, for the first request
// and for every redirect hop.
//
// It holds the dev-safety live gate only. CP6b's SSRF guard, WithPublicOnly, is a separate
// option with its own check (refusing private hosts, the opposite of this gate, which lets
// only loopback through when live fetching is off); the two are never merged into one flag.
func (c *Client) checkURL(u *url.URL) error {
	if (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return fmt.Errorf("%w: %q", ErrBadURL, u.Redacted())
	}
	if !c.fixtures {
		host := normalHost(u.Hostname())
		if !isLoopback(host) && !c.live.Allows(host) {
			return fmt.Errorf("%w: %s (LEADHOUND_LIVE is %s)", ErrLiveOff, host, c.live)
		}
	}
	return nil
}

// normalHost lower-cases a host and drops a trailing root dot.
func normalHost(h string) string {
	return strings.TrimSuffix(strings.ToLower(h), ".")
}

// isLoopback reports whether host is localhost or a loopback address (127.0.0.0/8, ::1,
// and their IPv4-mapped forms), so httptest servers work with live fetching off.
func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip, err := netip.ParseAddr(host)
	return err == nil && ip.Unmap().IsLoopback()
}

// WithLiveGate sets what the live gate allows, from LEADHOUND_LIVE: a host that is not
// loopback goes out only when live allows it, else Get returns ErrLiveOff and makes no
// request. Without this option the gate is LiveOff. It applies to every client but one
// built WithFixtures.
func WithLiveGate(live config.Live) Option { return func(c *Client) { c.live = live } }
