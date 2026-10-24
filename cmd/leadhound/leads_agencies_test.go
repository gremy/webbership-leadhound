package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"golang.org/x/time/rate"

	"github.com/gremy/webbership-leadhound/internal/config"
	"github.com/gremy/webbership-leadhound/internal/fetch"
	"github.com/gremy/webbership-leadhound/internal/probe"
)

// fakeClock is a clock that only moves when slept on, and keeps every sleep.
type fakeClock struct {
	now    time.Time
	sleeps []time.Duration
}

func (c *fakeClock) Now() time.Time { return c.now }

func (c *fakeClock) Sleep(_ context.Context, d time.Duration) error {
	c.sleeps = append(c.sleeps, d)
	c.now = c.now.Add(d)
	return nil
}

// memRecorder is an in-memory recorder naming each record by a counter.
type memRecorder struct {
	n     int64
	byURL map[string]int64
}

func (m *memRecorder) Record(_ context.Context, r fetch.Record) error {
	m.n++
	if m.byURL == nil {
		m.byURL = map[string]int64{}
	}
	m.byURL[r.URL] = m.n
	return nil
}

func (m *memRecorder) LastIDFor(u string) (int64, bool) {
	id, ok := m.byURL[u]
	return id, ok
}

func testFetcher(t *testing.T, rt http.RoundTripper, timeout time.Duration, pacer *hostPacer) pageFetcher {
	t.Helper()
	pr := &pageRecorder{inner: &memRecorder{}, bodies: map[int64][]byte{}}
	c := fetch.New(fetch.WithTransport(rt), fetch.WithRecorder(pr), fetch.WithTimeout(timeout), fetch.WithJitter(0), fetch.WithLimiter(rate.NewLimiter(rate.Inf, 1)),
		fetch.WithProfile(fetch.Site), fetch.WithLiveGate(config.LiveAll)) // rt is never the network
	return pageFetcher{c: c, rec: pr, pacer: pacer}
}

func TestHostPacerSpacesOneHost(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC)}
	p := newHostPacer(clock, 10*time.Second)
	at := map[string][]time.Time{}
	for _, h := range []string{"a.example", "b.example", "a.example", "a.example", "b.example"} {
		if err := p.wait(context.Background(), h); err != nil {
			t.Fatal(err)
		}
		at[h] = append(at[h], clock.Now())
	}
	for host, times := range at {
		for i := 1; i < len(times); i++ {
			if gap := times[i].Sub(times[i-1]); gap < 10*time.Second {
				t.Errorf("%s: requests %d and %d only %s apart", host, i-1, i, gap)
			}
		}
	}
	// a, b (no wait: another host), a (+10 s), a (+10 s), b (already 20 s on).
	if len(clock.sleeps) != 2 || clock.sleeps[0] != 10*time.Second || clock.sleeps[1] != 10*time.Second {
		t.Errorf("sleeps = %v, want two of 10 s", clock.sleeps)
	}
	// A host a redirect ended on waits its turn too.
	p.mark("c.example")
	if err := p.wait(context.Background(), "c.example"); err != nil || len(clock.sleeps) != 3 {
		t.Errorf("after mark: %v, sleeps %v", err, clock.sleeps)
	}
}

// The wait sits outside the client's timeout: a pace longer than the timeout still lets the
// next request to a host through.
func TestPacingOutsideClientTimeout(t *testing.T) {
	ft, err := fetch.NewFixtureTransport("../../internal/contacts/testdata/run")
	if err != nil {
		t.Fatal(err)
	}
	pf := testFetcher(t, ft, 200*time.Millisecond, newHostPacer(wallClock{}, 300*time.Millisecond))
	start := time.Now()
	for _, u := range []string{"https://northwind.example/robots.txt", "https://northwind.example/", "https://northwind.example/contact"} {
		resp, _, err := pf.Get(context.Background(), fetch.KindSite, u)
		if err != nil || resp.Status != http.StatusOK {
			t.Fatalf("%s: %d, %v", u, resp.Status, err)
		}
	}
	if d := time.Since(start); d < 600*time.Millisecond {
		t.Errorf("three requests to one host took %s, want at least 600 ms", d)
	}
}

// redirector sends every request under from to to, and answers anything else with page.
type redirector struct {
	from, to, page string
	n              int
}

func (r *redirector) RoundTrip(req *http.Request) (*http.Response, error) {
	r.n++
	h := http.Header{}
	if strings.HasPrefix(req.URL.String(), r.from) {
		h.Set("Location", r.to)
		return &http.Response{StatusCode: http.StatusMovedPermanently, Header: h, Body: io.NopCloser(strings.NewReader("")), Request: req}, nil
	}
	h.Set("Content-Type", "text/html")
	return &http.Response{StatusCode: http.StatusOK, Header: h, Body: io.NopCloser(strings.NewReader(r.page)), Request: req}, nil
}

func TestPageFetcherRefusesDrupalOrg(t *testing.T) {
	rt := &redirector{from: "https://northwind.example/old", to: "https://www.drupal.org/x", page: "<p>x</p>"}
	pf := testFetcher(t, rt, time.Second, nil)
	if _, _, err := pf.Get(context.Background(), fetch.KindSite, "https://new.drupal.org/node/1"); !errors.Is(err, errDrupalOrg) || rt.n != 0 {
		t.Errorf("drupal.org Get: %v after %d requests, want errDrupalOrg and none", err, rt.n)
	}
	if _, _, err := pf.Get(context.Background(), fetch.KindSite, "https://northwind.example/old"); !errors.Is(err, errDrupalOrg) {
		t.Errorf("a redirect to drupal.org: %v, want errDrupalOrg", err)
	}
	hr := probe.NewHostRobotsFunc(pf.robots)
	if _, got := fetchHome(context.Background(), pf, hr, pf.c.UserAgent, "https://www.drupal.org/a", ""); got != homeRefused {
		t.Errorf("fetchHome on drupal.org = %v, want homeRefused", got)
	}
}

func TestFetchHomeOffDomain(t *testing.T) {
	rt := &redirector{from: "https://northwind.example/", to: "https://www.contoso.example/", page: "<p>info@contoso.example</p>"}
	pf := testFetcher(t, rt, time.Second, nil)
	hr := probe.NewHostRobotsFunc(pf.robots)
	if _, got := fetchHome(context.Background(), pf, hr, pf.c.UserAgent, "https://northwind.example/", "northwind.example"); got != homeOffDomain {
		t.Errorf("redirect to another domain = %v, want homeOffDomain", got)
	}
	// Without an org domain (an agency's evidence page) the page is used.
	if p, got := fetchHome(context.Background(), pf, hr, pf.c.UserAgent, "https://northwind.example/", ""); got != homeOK || p.URL != "https://www.contoso.example/" {
		t.Errorf("evidence page = %+v, %v", p, got)
	}
}

func TestLeadsAgenciesUsage(t *testing.T) {
	for _, args := range [][]string{
		{"leads", "agencies"},
		{"leads", "agencies", "list"},
		{"leads", "agencies", "import", "a.yaml", "b.yaml"},
	} {
		if code := run(context.Background(), args, io.Discard); code != 2 {
			t.Errorf("%q exited %d, want 2", args, code)
		}
	}
}
