package mailbox

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// hopServer redirects /hop/N to /hop/N-1 until /hop/0, which answers 200 with a body.
func hopServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, err := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/hop/"))
		if err != nil || n <= 0 {
			_, _ = w.Write([]byte(strings.Repeat("x", 64<<10)))
			return
		}
		http.Redirect(w, r, "/hop/"+strconv.Itoa(n-1), http.StatusFound)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestFollowLandingRedirectChainEndsAtLastURL(t *testing.T) {
	srv := hopServer(t)
	got, err := followLanding(context.Background(), srv.Client(), srv.URL+"/hop/5")
	if err != nil {
		t.Fatalf("five redirects: %v", err)
	}
	if want := srv.URL + "/hop/0"; got != want {
		t.Fatalf("landing = %q, want %q", got, want)
	}
}

func TestFollowLandingTooManyRedirects(t *testing.T) {
	srv := hopServer(t)
	if got, err := followLanding(context.Background(), srv.Client(), srv.URL+"/hop/6"); err == nil {
		t.Fatalf("six redirects landed at %q, want an error", got)
	}
}

func TestFollowLandingTimeout(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-block:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() { close(block); srv.Close() })
	old := landingTimeout
	landingTimeout = 200 * time.Millisecond
	t.Cleanup(func() { landingTimeout = old })
	start := time.Now()
	if _, err := followLanding(context.Background(), srv.Client(), srv.URL+"/"); err == nil {
		t.Fatal("a server that never answers landed")
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("timeout took %v", d)
	}
}

func TestLandingURLRefusesBadDomain(t *testing.T) {
	for _, d := range []string{"", "  ", "a/b", "x@y", "a b"} {
		if _, err := LandingURL(context.Background(), nil, d); err == nil {
			t.Errorf("domain %q accepted", d)
		}
	}
}

// failingTransport fails every request without touching the network.
type failingTransport struct{}

func (failingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, context.DeadlineExceeded
}

// rewriteTransport sends https://<domain>/ to the test server, so Landing's own URL
// building is exercised without the network.
type rewriteTransport struct {
	srv *httptest.Server
}

func (t rewriteTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Host != "example.test" || r.URL.Scheme != "https" || r.URL.Path != "/" {
		return nil, context.Canceled
	}
	r2 := r.Clone(r.Context())
	r2.URL.Host = strings.TrimPrefix(t.srv.URL, "https://")
	r2.Host = ""
	return t.srv.Client().Transport.RoundTrip(r2)
}

func TestLandingURLBuildsHTTPSRoot(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	t.Cleanup(srv.Close)
	u, err := LandingURL(context.Background(), &http.Client{Transport: rewriteTransport{srv}}, "Example.TEST.")
	if err != nil || !strings.HasPrefix(u, srv.URL) {
		t.Fatalf("landing = %q, %v; want under %q", u, err, srv.URL)
	}
	if _, err := LandingURL(context.Background(), &http.Client{Transport: failingTransport{}}, "example.test"); err == nil {
		t.Fatal("a transport that fails landed")
	}
}
