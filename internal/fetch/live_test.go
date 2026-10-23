package fetch

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/gremy/webbership-leadhound/internal/config"
)

// The hostnames below are only strings handed to a stub transport; no test reaches the network.
const (
	linkedInHost   = "www.linkedin.com"
	greenhouseHost = "boards-api.greenhouse.io"
)

func TestLiveOffRefusesNonLoopback(t *testing.T) {
	clk, rec := newFakeClock(), &memRecorder{}
	c := New(append(testOptions(clk, rec), WithTransport(failTransport{t}), WithLiveGate(config.LiveOff))...)
	for _, u := range []string{"https://jobs.example/x", "https://" + linkedInHost + "/jobs", "https://" + greenhouseHost + "/v1/boards/a/jobs"} {
		_, err := c.Get(context.Background(), KindSearch, u)
		if !errors.Is(err, ErrLiveOff) {
			t.Errorf("Get(%s) err = %v, want ErrLiveOff", u, err)
		}
	}
	if len(rec.all()) != 0 || c.Breaker.Requests() != 0 {
		t.Errorf("a refused request was recorded or counted")
	}
}

func TestLiveOffAllowsLoopback(t *testing.T) {
	st := &stubTransport{fn: func(r *http.Request) (*http.Response, error) { return okResponse(r, 200, "ok"), nil }}
	clk, rec := newFakeClock(), &memRecorder{}
	c := New(append(testOptions(clk, rec), WithTransport(st), WithLiveGate(config.LiveOff))...)
	for _, u := range []string{"http://127.0.0.1:8080/", "http://127.9.9.9/", "http://[::1]:9/", "http://localhost/", "http://LOCALHOST./"} {
		if _, err := c.Get(context.Background(), KindSearch, u); err != nil {
			t.Errorf("Get(%s): %v", u, err)
		}
	}
	if st.calls.Load() != 5 {
		t.Errorf("calls = %d, want 5", st.calls.Load())
	}
}

func TestLiveATS(t *testing.T) {
	st := &stubTransport{fn: func(r *http.Request) (*http.Response, error) { return okResponse(r, 200, "{}"), nil }}
	clk, rec := newFakeClock(), &memRecorder{}
	c := New(append(testOptions(clk, rec), WithTransport(st), WithLiveGate(config.LiveATS))...)
	ctx := context.Background()

	if _, err := c.Get(ctx, KindSearch, "https://"+linkedInHost+"/jobs-guest/x"); !errors.Is(err, ErrLiveOff) {
		t.Errorf("LinkedIn under ats: err = %v, want ErrLiveOff", err)
	}
	if st.calls.Load() != 0 {
		t.Fatalf("the refused LinkedIn request reached the transport")
	}
	if _, err := c.Get(ctx, KindBoard, "https://"+greenhouseHost+"/v1/boards/acme/jobs"); err != nil {
		t.Errorf("Greenhouse under ats: %v", err)
	}
	if st.calls.Load() != 1 {
		t.Errorf("calls = %d, want 1", st.calls.Load())
	}
}

func TestLiveGateChecksRedirects(t *testing.T) {
	st := &stubTransport{fn: func(r *http.Request) (*http.Response, error) {
		resp := okResponse(r, http.StatusFound, "")
		resp.Header.Set("Location", "https://"+linkedInHost+"/jobs")
		return resp, nil
	}}
	clk, rec := newFakeClock(), &memRecorder{}
	c := New(append(testOptions(clk, rec), WithTransport(st), WithLiveGate(config.LiveATS))...)
	_, err := c.Get(context.Background(), KindBoard, "https://"+greenhouseHost+"/v1/boards/acme/jobs")
	if !errors.Is(err, ErrLiveOff) {
		t.Fatalf("err = %v, want ErrLiveOff on the redirect", err)
	}
	if st.calls.Load() != 1 {
		t.Errorf("calls = %d, want 1: the redirect target is never requested", st.calls.Load())
	}
	if c.Breaker.Failures() != 0 || c.Breaker.Requests() != 1 {
		t.Errorf("failures %d, requests %d; want 0, 1: the hop counts, the refusal is no failure",
			c.Breaker.Failures(), c.Breaker.Requests())
	}
	if recs := rec.all(); len(recs) != 1 || recs[0].Status != http.StatusFound {
		t.Errorf("records = %+v, want the 302", recs)
	}
}

func TestLiveAllAllowsAnyHost(t *testing.T) {
	st := &stubTransport{fn: func(r *http.Request) (*http.Response, error) { return okResponse(r, 200, "ok"), nil }}
	clk, rec := newFakeClock(), &memRecorder{}
	c := New(append(testOptions(clk, rec), WithTransport(st), WithLiveGate(config.LiveAll))...)
	if _, err := c.Get(context.Background(), KindSite, "https://jobs.example/careers"); err != nil {
		t.Fatal(err)
	}
}

// The gate is on by default for every client, an injected transport included; only a
// fixture transport given through WithFixtures is not gated.
func TestGateOnByDefault(t *testing.T) {
	c := New()
	if c.fixtures || c.live != config.LiveOff {
		t.Fatalf("default client: fixtures %v, live %v; want gated, off", c.fixtures, c.live)
	}
	if err := c.checkURL(mustParse(t, "https://jobs.example/")); !errors.Is(err, ErrLiveOff) {
		t.Errorf("checkURL err = %v, want ErrLiveOff", err)
	}

	// An injected transport is gated without WithLiveGate.
	clk, rec := newFakeClock(), &memRecorder{}
	injected := New(append(testOptions(clk, rec), WithTransport(failTransport{t}))...)
	if _, err := injected.Get(context.Background(), KindSearch, "https://"+linkedInHost+"/jobs"); !errors.Is(err, ErrLiveOff) {
		t.Errorf("injected transport: err = %v, want ErrLiveOff", err)
	}
	// So is a fixture transport handed in as a plain transport.
	ft := &FixtureTransport{hits: map[string]int{}}
	plain := New(append(testOptions(clk, rec), WithTransport(ft))...)
	if _, err := plain.Get(context.Background(), KindSearch, "https://"+linkedInHost+"/jobs"); !errors.Is(err, ErrLiveOff) {
		t.Errorf("fixtures through WithTransport: err = %v, want ErrLiveOff", err)
	}
	if ft.Hits() != 0 || len(rec.all()) != 0 {
		t.Fatalf("a gated request reached a transport or the recorder")
	}
	// WithFixtures is not gated: the fixture transport answers (404, no route).
	fixed := New(append(testOptions(clk, rec), WithFixtures(ft))...)
	if resp, err := fixed.Get(context.Background(), KindSearch, "https://"+linkedInHost+"/jobs"); err != nil || resp.Status != 404 {
		t.Errorf("WithFixtures: %d, %v; want 404, nil", resp.Status, err)
	}
	// A later WithTransport replaces the fixtures and brings the gate back.
	back := New(append(testOptions(clk, rec), WithFixtures(ft), WithTransport(failTransport{t}))...)
	if _, err := back.Get(context.Background(), KindSearch, "https://"+linkedInHost+"/jobs"); !errors.Is(err, ErrLiveOff) {
		t.Errorf("WithFixtures then WithTransport: err = %v, want ErrLiveOff", err)
	}
}

// A redirect the gate refuses after the first hop went out: the hop is counted as a request
// (not a failure) and its 3xx is recorded with the gate's error.
func TestRefusedRedirectRecordsTheHop(t *testing.T) {
	st := &stubTransport{fn: func(r *http.Request) (*http.Response, error) {
		resp := okResponse(r, http.StatusMovedPermanently, "")
		resp.Header.Set("Location", "https://jobs.example/elsewhere")
		return resp, nil
	}}
	clk, rec := newFakeClock(), &memRecorder{}
	c := New(append(testOptions(clk, rec), WithTransport(st), WithLiveGate(config.LiveOff))...)
	resp, err := c.Get(context.Background(), KindSite, "http://127.0.0.1:9/start")
	if !errors.Is(err, ErrLiveOff) || resp.Status != http.StatusMovedPermanently {
		t.Fatalf("Get = %d, %v; want 301, ErrLiveOff", resp.Status, err)
	}
	if c.Breaker.Requests() != 1 || c.Breaker.Failures() != 0 || c.Breaker.Reason() != "" {
		t.Errorf("breaker: requests %d, failures %d, reason %q; want 1, 0, closed",
			c.Breaker.Requests(), c.Breaker.Failures(), c.Breaker.Reason())
	}
	recs := rec.all()
	if len(recs) != 1 || recs[0].Status != http.StatusMovedPermanently || recs[0].URL != "http://127.0.0.1:9/start" ||
		!strings.Contains(recs[0].Err, "live fetching is off") {
		t.Errorf("records = %+v, want one 301 with the gate's error", recs)
	}
}
