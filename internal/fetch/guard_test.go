package fetch

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/config"
)

// v4 builds an IPv4 address from its octets, and m4 its IPv4-mapped IPv6 form; ip6 parses
// an IPv6 literal.
func v4(a, b, c, d byte) netip.Addr { return netip.AddrFrom4([4]byte{a, b, c, d}) }
func m4(a, b, c, d byte) netip.Addr { return netip.AddrFrom16(v4(a, b, c, d).As16()) }
func ip6(s string) netip.Addr       { return netip.MustParseAddr(s) }

// The public addresses in this table are only built, never dialled. Every other test uses
// the documentation ranges (192.0.2.0/24, 2001:db8::/32) or reserved host names.
func TestIsPublic(t *testing.T) {
	tests := []struct {
		addr netip.Addr
		want bool
	}{
		// One address per refused range.
		{v4(0, 0, 0, 0), false},         // 0/8
		{v4(0, 1, 2, 3), false},         // 0/8
		{v4(127, 0, 0, 1), false},       // loopback 127/8
		{v4(127, 255, 255, 254), false}, // loopback 127/8
		{v4(10, 0, 0, 1), false},        // RFC1918 10/8
		{v4(172, 16, 0, 1), false},      // RFC1918 172.16/12
		{v4(172, 31, 255, 254), false},  // RFC1918 172.16/12
		{v4(192, 168, 1, 1), false},     // RFC1918 192.168/16
		{v4(169, 254, 169, 254), false}, // link-local 169.254/16, cloud metadata
		{v4(169, 254, 0, 1), false},     // link-local 169.254/16
		{v4(100, 64, 0, 1), false},      // CGNAT 100.64/10
		{v4(100, 127, 255, 254), false}, // CGNAT 100.64/10
		{v4(224, 0, 0, 1), false},       // multicast 224/4
		{v4(239, 255, 255, 250), false}, // multicast 224/4
		{v4(198, 18, 0, 1), false},      // benchmarking 198.18/15
		{v4(198, 19, 255, 254), false},  // benchmarking 198.18/15
		{v4(240, 0, 0, 1), false},       // reserved 240/4
		{v4(255, 255, 255, 255), false}, // broadcast, inside 240/4
		{ip6("::"), false},              // unspecified
		{ip6("::1"), false},             // loopback
		{ip6("fc00::1"), false},         // ULA fc00::/7
		{ip6("fd12:3456::1"), false},    // ULA fc00::/7
		{ip6("fe80::1"), false},         // link-local fe80::/10
		{ip6("fe80::1%en0"), false},     // link-local with a zone
		{ip6("febf::1"), false},         // link-local fe80::/10
		{ip6("ff02::1"), false},         // multicast
		{ip6("64:ff9b::7f00:1"), false}, // NAT64 64:ff9b::/96
		{ip6("64:ff9b::808:808"), false},
		{ip6("64:ff9b:1::7f00:1"), false}, // local-use NAT64 64:ff9b:1::/48
		{ip6("::7f00:1"), false},          // IPv4-compatible ::/96
		{ip6("::808:808"), false},         // IPv4-compatible ::/96
		{ip6("2002:7f00:1::1"), false},    // 6to4 2002::/16
		{ip6("2002:808:808::1"), false},

		// IPv4-mapped forms of every IPv4 range.
		{m4(0, 0, 0, 0), false},
		{m4(127, 0, 0, 1), false},
		{m4(10, 0, 0, 1), false},
		{m4(172, 16, 0, 1), false},
		{m4(192, 168, 1, 1), false},
		{m4(169, 254, 169, 254), false},
		{m4(100, 64, 0, 1), false},
		{m4(224, 0, 0, 1), false},
		{m4(198, 18, 0, 1), false},
		{m4(240, 0, 0, 1), false},

		// Public controls, including the edges of the refused ranges.
		{v4(8, 8, 8, 8), true},
		{v4(1, 1, 1, 1), true},
		{m4(8, 8, 8, 8), true},
		{ip6("2606:4700:4700::1111"), true},
		{ip6("2a00:1450:4001::1"), true},
		{v4(192, 0, 2, 10), true},      // documentation range: public by the rule, used by fixtures
		{ip6("2001:db8::10"), true},    // documentation range
		{v4(100, 63, 255, 255), true},  // just below CGNAT
		{v4(100, 128, 0, 0), true},     // just above CGNAT
		{v4(172, 32, 0, 1), true},      // just above 172.16/12
		{v4(198, 17, 255, 255), true},  // just below 198.18/15
		{v4(198, 20, 0, 0), true},      // just above 198.18/15
		{v4(223, 255, 255, 254), true}, // just below multicast
	}
	for _, tt := range tests {
		got := isPublic(tt.addr)
		if got != tt.want {
			t.Errorf("isPublic(%s) = %v, want %v", tt.addr, got, tt.want)
		}
	}
	if isPublic(netip.Addr{}) {
		t.Errorf("the zero Addr is public")
	}
}

func TestGuardControl(t *testing.T) {
	tests := []struct {
		address string
		want    error
	}{
		{"192.0.2.10:443", nil},
		{"[2001:db8::10]:80", nil},
		{"127.0.0.1:80", ErrNotPublic},
		{"[::1]:443", ErrNotPublic},
		{netip.AddrPortFrom(v4(169, 254, 169, 254), 80).String(), ErrNotPublic},
		{netip.AddrPortFrom(v4(10, 0, 0, 1), 443).String(), ErrNotPublic},
		{netip.AddrPortFrom(m4(10, 0, 0, 1), 443).String(), ErrNotPublic},
		{"[fe80::1%en0]:443", ErrNotPublic},
		{"not-an-address", ErrNotPublic},
	}
	for _, tt := range tests {
		err := guardControl("tcp", tt.address, nil)
		if !errors.Is(err, tt.want) || (tt.want == nil && err != nil) {
			t.Errorf("guardControl(%s) = %v, want %v", tt.address, err, tt.want)
		}
	}
}

func TestGuardedTransportBuiltOnce(t *testing.T) {
	a, b := guardedTransport(), guardedTransport()
	if a != b {
		t.Fatalf("guardedTransport built twice")
	}
	if a.Proxy != nil {
		t.Errorf("guarded transport has a proxy")
	}
	if a == http.DefaultTransport {
		t.Errorf("guarded transport is http.DefaultTransport itself")
	}
	if New(WithPublicOnly()).HTTP.Transport != a {
		t.Errorf("WithPublicOnly client does not use the guarded transport")
	}
	if New().HTTP.Transport != nil {
		t.Errorf("a client without WithPublicOnly uses a transport")
	}
	st := &stubTransport{}
	if New(WithPublicOnly(), WithTransport(st)).HTTP.Transport != st {
		t.Errorf("WithTransport did not replace the guarded transport")
	}
}

// localhost is a name, so only the dial can judge it: the guarded transport refuses it once
// DNS has answered with a loopback address.
func TestGuardRefusesAfterDNS(t *testing.T) {
	assertGuardedDial(t, guardedTransport())
}

// A wrapped http.DefaultTransport (as cmd tests install) must not panic, and still gives a
// transport with no proxy and the guarded dialer.
func TestGuardedTransportFromWrappedDefault(t *testing.T) {
	orig := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = orig })
	http.DefaultTransport = &stubTransport{}

	tr := newGuardedTransport(http.DefaultTransport)
	if tr.Proxy != nil || tr.DialContext == nil || !tr.ForceAttemptHTTP2 || tr.MaxIdleConns != 100 {
		t.Errorf("fallback transport settings wrong: %+v", tr)
	}
	assertGuardedDial(t, tr)
}

func assertGuardedDial(t *testing.T, rt http.RoundTripper) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { hits.Add(1) }))
	t.Cleanup(srv.Close)
	byName := "http://localhost:" + mustParse(t, srv.URL).Port() + "/"

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, byName, nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := rt.RoundTrip(req)
	if err == nil {
		_ = res.Body.Close()
	}
	if !errors.Is(err, ErrNotPublic) {
		t.Fatalf("RoundTrip(%s) err = %v, want ErrNotPublic from the dial", byName, err)
	}
	if hits.Load() != 0 {
		t.Errorf("the loopback server was reached")
	}
}

func TestCheckURLPublicOnly(t *testing.T) {
	c := New(WithPublicOnly(), WithLiveGate(config.LiveAll))
	tests := []struct {
		url  string
		want error
	}{
		{"http://www.northwind.example/", nil},
		{"https://www.northwind.example/", nil},
		{"http://www.northwind.example:80/", nil},
		{"https://www.northwind.example:443/", nil},
		{"http://192.0.2.10/", nil},
		{"https://[2001:db8::10]/", nil},
		{"https://www.northwind.example:8443/", ErrNotPublic},
		{"http://192.0.2.10:8080/", ErrNotPublic},
		{"http://127.0.0.1/", ErrNotPublic},
		{"http://[::1]/", ErrNotPublic},
		{"http://[::ffff:127.0.0.1]/", ErrNotPublic},
		{"http://" + v4(169, 254, 169, 254).String() + "/latest/", ErrNotPublic},
		{"http://" + v4(10, 0, 0, 1).String() + "/", ErrNotPublic},
		{"http://[fe80::1%25en0]/", ErrNotPublic},
		{"file:///etc/hosts", ErrBadURL},
		{"mailto:office@northwind.example", ErrBadURL},
		{"javascript:alert(1)", ErrBadURL},
		{"data:text/html,x", ErrBadURL},
	}
	for _, tt := range tests {
		err := c.checkURL(mustParse(t, tt.url))
		if !errors.Is(err, tt.want) || (tt.want == nil && err != nil) {
			t.Errorf("checkURL(%s) = %v, want %v", tt.url, err, tt.want)
		}
	}
}

func TestPublicOnlyRefusesBeforeAnyDial(t *testing.T) {
	clk, rec := newFakeClock(), &memRecorder{}
	c := New(append(testOptions(clk, rec), WithPublicOnly(), WithLiveGate(config.LiveAll), WithTransport(failTransport{t}))...)
	for _, u := range []string{"file:///etc/hosts", "http://www.northwind.example:8080/", "http://127.0.0.1/"} {
		if _, err := c.Get(context.Background(), KindSite, u); err == nil {
			t.Errorf("Get(%s) succeeded", u)
		}
	}
	if len(rec.all()) != 0 || c.Breaker.Requests() != 0 {
		t.Errorf("a refused request was recorded or counted")
	}
}

func TestPublicOnlyRefusesRedirectToLoopback(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { hits.Add(1) }))
	t.Cleanup(srv.Close)
	target := srv.URL + "/admin"

	// The redirect target itself: allowed without the guard, refused with it.
	if err := New(WithLiveGate(config.LiveAll)).checkURL(mustParse(t, target)); err != nil {
		t.Fatalf("unguarded checkURL(%s) = %v", target, err)
	}
	if err := New(WithPublicOnly(), WithLiveGate(config.LiveAll)).checkURL(mustParse(t, target)); !errors.Is(err, ErrNotPublic) {
		t.Fatalf("guarded checkURL(%s) = %v, want ErrNotPublic", target, err)
	}

	// Through Get: a public host answers with a redirect to the loopback server.
	st := &stubTransport{fn: func(r *http.Request) (*http.Response, error) {
		resp := okResponse(r, http.StatusFound, "")
		resp.Header.Set("Location", target)
		return resp, nil
	}}
	clk, rec := newFakeClock(), &memRecorder{}
	c := New(append(testOptions(clk, rec), WithPublicOnly(), WithLiveGate(config.LiveAll), WithTransport(st))...)
	_, err := c.Get(context.Background(), KindSite, "https://www.northwind.example/")
	if !errors.Is(err, ErrNotPublic) {
		t.Fatalf("err = %v, want ErrNotPublic on the redirect", err)
	}
	if st.calls.Load() != 1 || hits.Load() != 0 {
		t.Errorf("calls = %d, server hits = %d; the redirect target must never be requested", st.calls.Load(), hits.Load())
	}
	if recs := rec.all(); len(recs) != 1 || recs[0].Status != http.StatusFound {
		t.Errorf("the refused 302 was not recorded: %+v", recs)
	}
}

// Both checks must allow: the live gate lets loopback through, the guard does not; the
// guard lets a public host through, the live gate (off) does not.
func TestPublicOnlyComposesWithLiveGate(t *testing.T) {
	c := New(WithPublicOnly(), WithLiveGate(config.LiveOff))
	if err := c.checkURL(mustParse(t, "http://127.0.0.1:80/")); !errors.Is(err, ErrNotPublic) {
		t.Errorf("loopback: err = %v, want ErrNotPublic", err)
	}
	if err := c.checkURL(mustParse(t, "https://www.northwind.example/")); !errors.Is(err, ErrLiveOff) {
		t.Errorf("public host with live off: err = %v, want ErrLiveOff", err)
	}
}

func TestPublicOnlyAppliesToFixtureURLs(t *testing.T) {
	private := "http://" + v4(10, 0, 0, 1).String() + "/"
	dir := fixtureDir(t, `{"https://www.northwind.example/": "home.html", "`+private+`": "home.html"}`,
		map[string]string{"home.html": "<html></html>"})
	ft, err := NewFixtureTransport(dir)
	if err != nil {
		t.Fatal(err)
	}
	clk, rec := newFakeClock(), &memRecorder{}
	c := New(append(testOptions(clk, rec), WithPublicOnly(), WithFixtures(ft))...)
	ctx := context.Background()
	if _, err := c.Get(ctx, KindSite, "https://www.northwind.example/"); err != nil {
		t.Errorf("fixture host: %v", err)
	}
	for _, u := range []string{private, "https://www.northwind.example:8443/"} {
		if _, err := c.Get(ctx, KindSite, u); !errors.Is(err, ErrNotPublic) {
			t.Errorf("Get(%s) err = %v, want ErrNotPublic", u, err)
		}
	}
	if len(rec.all()) != 1 {
		t.Errorf("records = %d, want 1", len(rec.all()))
	}
}

func TestHeaderAllowlist(t *testing.T) {
	st := &stubTransport{fn: func(r *http.Request) (*http.Response, error) {
		resp := okResponse(r, 200, "ok")
		for k, v := range map[string]string{
			"X-Generator":            "Drupal 10 (https://www.drupal.org)",
			"X-Drupal-Cache":         "HIT",
			"X-Drupal-Dynamic-Cache": "MISS",
			"Expires":                "Sun, 19 Nov 1978 05:00:00 GMT",
			"Server":                 "nginx",
			"Cf-Mitigated":           "challenge",
			"Retry-After":            "5",
			"Set-Cookie":             "session=abc",
			"X-Powered-By":           "PHP",
			"Link":                   "</node/1>; rel=shortlink",
		} {
			resp.Header.Set(k, v)
		}
		return resp, nil
	}}
	clk, rec := newFakeClock(), &memRecorder{}
	c := New(append(testOptions(clk, rec), WithTransport(st), WithLiveGate(config.LiveAll))...)
	resp, err := c.Get(context.Background(), KindSite, "https://www.northwind.example/")
	if err != nil {
		t.Fatal(err)
	}
	recs := rec.all()
	if len(recs) != 1 {
		t.Fatalf("records = %d, want 1", len(recs))
	}
	want := []string{"Cf-Mitigated", "Content-Type", "Expires", "Retry-After", "Server", "X-Drupal-Cache", "X-Drupal-Dynamic-Cache", "X-Generator"}
	for name, h := range map[string]http.Header{"Response": resp.Header, "Record": recs[0].Header} {
		if len(h) != len(want) {
			t.Errorf("%s.Header has %d keys, want %d: %v", name, len(h), len(want), h)
		}
		for _, k := range want {
			if h.Get(k) == "" {
				t.Errorf("%s.Header lacks %s", name, k)
			}
		}
		for _, k := range []string{"Set-Cookie", "X-Powered-By", "Link"} {
			if _, ok := h[k]; ok {
				t.Errorf("%s.Header carries %s, which is not allowlisted", name, k)
			}
		}
	}
	if got := resp.Header.Get("X-Generator"); !strings.HasPrefix(got, "Drupal 10") {
		t.Errorf("X-Generator = %q", got)
	}
}

func TestWithTimeout(t *testing.T) {
	if got := New().HTTP.Timeout; got != DefaultTimeout {
		t.Errorf("default timeout = %v, want %v", got, DefaultTimeout)
	}
	if got := New(WithTimeout(15 * time.Second)).HTTP.Timeout; got != 15*time.Second {
		t.Errorf("timeout = %v, want 15s", got)
	}
}

func TestWithMaxRedirects(t *testing.T) {
	// n is the most requests one Get makes, as with net/http's own default of 10.
	for _, tt := range []struct {
		opts  []Option
		calls int32
	}{
		{nil, maxRedirects},
		{[]Option{WithMaxRedirects(3)}, 3},
	} {
		st := &stubTransport{}
		st.fn = func(r *http.Request) (*http.Response, error) {
			resp := okResponse(r, http.StatusFound, "")
			next := *r.URL
			next.Path += "x"
			resp.Header.Set("Location", (&url.URL{Path: next.Path}).String())
			return resp, nil
		}
		clk, rec := newFakeClock(), &memRecorder{}
		opts := append(testOptions(clk, rec), WithTransport(st), WithLiveGate(config.LiveAll))
		c := New(append(opts, tt.opts...)...)
		if _, err := c.Get(context.Background(), KindSite, "https://www.northwind.example/"); err == nil {
			t.Errorf("an endless redirect returned no error")
		}
		if st.calls.Load() != tt.calls {
			t.Errorf("calls = %d, want %d", st.calls.Load(), tt.calls)
		}
	}
}

// Sites follow up to 3 redirects: WithMaxRedirects(4) follows the 3rd and refuses the 4th.
func TestMaxRedirectsFourFollowsThree(t *testing.T) {
	for _, tt := range []struct {
		redirects int
		ok        bool
	}{{3, true}, {4, false}} {
		var hits atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			n := hits.Add(1)
			if int(n) <= tt.redirects {
				http.Redirect(w, r, "/hop"+strconv.Itoa(int(n)), http.StatusFound)
				return
			}
			_, _ = w.Write([]byte("ok"))
		}))
		clk, rec := newFakeClock(), &memRecorder{}
		c := New(append(testOptions(clk, rec), WithMaxRedirects(4))...)
		resp, err := c.Get(context.Background(), KindSite, srv.URL+"/")
		srv.Close()
		if tt.ok && (err != nil || resp.Status != http.StatusOK || hits.Load() != 4) {
			t.Errorf("%d redirects: status %d, err %v, hits %d; want 200 after 4 requests", tt.redirects, resp.Status, err, hits.Load())
		}
		if !tt.ok && (err == nil || hits.Load() != 4) {
			t.Errorf("%d redirects: err %v, hits %d; want the 4th redirect refused after 4 requests", tt.redirects, err, hits.Load())
		}
	}
}

func TestSiteProfile(t *testing.T) {
	if got := Site.Header.Get("Accept"); got != "text/html,application/xhtml+xml;q=0.9,*/*;q=0.8" {
		t.Errorf("Accept = %q", got)
	}
	if got := Site.Header.Get("Accept-Language"); got != "en;q=0.9,*;q=0.5" {
		t.Errorf("Accept-Language = %q", got)
	}
	if Site.Header.Get("User-Agent") != "" || Site.Header.Get("X-Requested-With") != "" {
		t.Errorf("Site sends a User-Agent or X-Requested-With of its own")
	}
	for _, status := range []int{http.StatusForbidden, 999, http.StatusServiceUnavailable} {
		if Site.blocked(status, "/login") {
			t.Errorf("Site treats %d as blocked", status)
		}
	}
}
