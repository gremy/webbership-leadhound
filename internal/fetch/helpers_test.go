package fetch

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/time/rate"
)

// fakeClock is a Clock whose Sleep only advances Now and records the duration.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	sleeps []time.Duration
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}
}

func (f *fakeClock) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *fakeClock) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sleeps = append(f.sleeps, d)
	f.now = f.now.Add(d)
	return nil
}

func (f *fakeClock) slept() []time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]time.Duration(nil), f.sleeps...)
}

func (f *fakeClock) total() time.Duration {
	var t time.Duration
	for _, d := range f.slept() {
		t += d
	}
	return t
}

// memRecorder keeps every Record in memory.
type memRecorder struct {
	mu   sync.Mutex
	recs []Record
	err  error
}

func (m *memRecorder) Record(_ context.Context, r Record) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.recs = append(m.recs, r)
	return m.err
}

func (m *memRecorder) all() []Record {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Record(nil), m.recs...)
}

// stubTransport answers every request with fn and counts the calls; it never touches the network.
type stubTransport struct {
	calls atomic.Int32
	fn    func(*http.Request) (*http.Response, error)
}

func (s *stubTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	s.calls.Add(1)
	return s.fn(r)
}

func okResponse(r *http.Request, status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": {"text/html"}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    r,
	}
}

// failTransport fails the test if any request reaches it.
type failTransport struct{ t *testing.T }

func (f failTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	f.t.Errorf("request reached the transport: %s", r.URL)
	return nil, io.ErrUnexpectedEOF
}

// testOptions are the options every test starts from: no limiter wait, no jitter, a fake
// clock and an in-memory recorder.
func testOptions(clk *fakeClock, rec *memRecorder) []Option {
	return []Option{
		WithLimiter(rate.NewLimiter(rate.Inf, 1)),
		WithJitter(0),
		WithClock(clk),
		WithRecorder(rec),
	}
}

func mustParse(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}
