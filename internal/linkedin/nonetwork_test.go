package linkedin

import (
	"errors"
	"net"
	"net/http"
	"os"
	"testing"
)

// TestMain replaces the default transport for the whole test binary, so a client that was
// accidentally built without a fixture transport fails instead of reaching a live site.
// Only loopback (httptest) may go out.
func TestMain(m *testing.M) {
	http.DefaultTransport = loopbackOnly{next: http.DefaultTransport}
	os.Exit(m.Run())
}

type loopbackOnly struct{ next http.RoundTripper }

func (l loopbackOnly) RoundTrip(r *http.Request) (*http.Response, error) {
	if ip := net.ParseIP(r.URL.Hostname()); r.URL.Hostname() != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return nil, errors.New("test guard: a request to " + r.URL.Hostname() + " left the fixtures")
	}
	return l.next.RoundTrip(r)
}
