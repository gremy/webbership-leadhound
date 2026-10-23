package fetch

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/time/rate"

	"github.com/gremy/webbership-leadhound/internal/config"
)

// countingServer serves handler and counts every request it receives.
func countingServer(t *testing.T, handler http.HandlerFunc) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func reasonOf(t *testing.T, err error) StopReason {
	t.Helper()
	if !errors.Is(err, ErrBreakerOpen) {
		t.Fatalf("err = %v, want ErrBreakerOpen", err)
	}
	var oe *OpenError
	if !errors.As(err, &oe) {
		t.Fatalf("err = %v, want *OpenError", err)
	}
	return oe.Reason
}

func TestGetOK(t *testing.T) {
	var got http.Header
	srv, _ := countingServer(t, func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte("<li>card</li>"))
	})
	clk, rec := newFakeClock(), &memRecorder{}
	c := New(append(testOptions(clk, rec), WithProfile(LinkedInJobs), WithUserAgent(UserAgentFor("1.2.3")))...)

	resp, err := c.Get(context.Background(), KindSearch, srv.URL+"/search?q=go")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if resp.Status != 200 || string(resp.Body) != "<li>card</li>" || resp.ContentType != "text/html; charset=utf-8" {
		t.Errorf("resp = %+v", resp)
	}
	if resp.FinalURL != srv.URL+"/search?q=go" || !resp.FetchedAt.Equal(clk.Now()) {
		t.Errorf("FinalURL = %q, FetchedAt = %v", resp.FinalURL, resp.FetchedAt)
	}
	for k, want := range map[string]string{
		"Accept":           "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8",
		"Accept-Language":  "en-US,en;q=0.9",
		"X-Requested-With": "XMLHttpRequest",
		"User-Agent":       "leadhound/1.2.3 (+https://github.com/gremy/webbership-leadhound)",
	} {
		if g := got.Get(k); g != want {
			t.Errorf("header %s = %q, want %q", k, g, want)
		}
	}
	recs := rec.all()
	if len(recs) != 1 {
		t.Fatalf("records = %d, want 1", len(recs))
	}
	r := recs[0]
	if r.Kind != KindSearch || r.Status != 200 || r.FinalURL != "" || string(r.Body) != "<li>card</li>" || len(r.SHA256) != 32 || r.Err != "" {
		t.Errorf("record = %+v", r)
	}
}

func TestGet404(t *testing.T) {
	srv, _ := countingServer(t, func(w http.ResponseWriter, _ *http.Request) { http.NotFound(w, nil) })
	clk, rec := newFakeClock(), &memRecorder{}
	c := New(append(testOptions(clk, rec), WithProfile(LinkedInJobs))...)

	resp, err := c.Get(context.Background(), KindDetail, srv.URL+"/job/1")
	if err != nil || resp.Status != 404 {
		t.Fatalf("Get = %d, %v; want 404, nil", resp.Status, err)
	}
	if c.Breaker.Reason() != "" || len(rec.all()) != 1 {
		t.Errorf("reason %q, records %d", c.Breaker.Reason(), len(rec.all()))
	}
}

func TestOtherStatusIsErrStatus(t *testing.T) {
	srv, _ := countingServer(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusGone) })
	clk, rec := newFakeClock(), &memRecorder{}
	c := New(testOptions(clk, rec)...)
	resp, err := c.Get(context.Background(), KindDetail, srv.URL)
	if !errors.Is(err, ErrStatus) || resp.Status != http.StatusGone {
		t.Fatalf("Get = %d, %v; want 410, ErrStatus", resp.Status, err)
	}
}

func TestTwo429sOpenThrottled(t *testing.T) {
	srv, hits := countingServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "2")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	clk, rec := newFakeClock(), &memRecorder{}
	c := New(append(testOptions(clk, rec), WithProfile(LinkedInJobs))...)

	_, err := c.Get(context.Background(), KindSearch, srv.URL)
	if r := reasonOf(t, err); r != StopThrottled {
		t.Fatalf("reason = %q, want throttled", r)
	}
	if hits.Load() != 2 {
		t.Fatalf("hits after first Get = %d, want 2 (one retry)", hits.Load())
	}
	_, err = c.Get(context.Background(), KindSearch, srv.URL)
	if r := reasonOf(t, err); r != StopThrottled {
		t.Fatalf("second Get reason = %q", r)
	}
	if hits.Load() != 2 {
		t.Errorf("hits = %d, want 2: an open breaker makes no request", hits.Load())
	}
	if c.Breaker.Reason() != StopThrottled {
		t.Errorf("Breaker.Reason = %q", c.Breaker.Reason())
	}
	// Both 429s were recorded, the first before the retry.
	recs := rec.all()
	if len(recs) != 2 || recs[0].Status != 429 || recs[1].Status != 429 {
		t.Errorf("records = %+v", recs)
	}
}

// A 429 whose retry the budget refuses opens the breaker as throttled, so the run pauses the
// day instead of ending on a budget stop.
func TestThrottledWhenBudgetRefusesTheRetry(t *testing.T) {
	srv, hits := countingServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	clk, rec := newFakeClock(), &memRecorder{}
	c := New(append(testOptions(clk, rec), WithBreaker(NewBreaker(Budgets{KindCanary: 1})))...)
	_, err := c.Get(context.Background(), KindCanary, srv.URL)
	if r := reasonOf(t, err); r != StopThrottled {
		t.Fatalf("reason = %q, want throttled", r)
	}
	if hits.Load() != 1 || c.Breaker.Reason() != StopThrottled || c.Breaker.Requests() != 1 {
		t.Errorf("hits %d, reason %q, requests %d; want 1, throttled, 1", hits.Load(), c.Breaker.Reason(), c.Breaker.Requests())
	}
	// Every kind is stopped now, not just the canary's.
	if _, err := c.Get(context.Background(), KindSearch, srv.URL); reasonOf(t, err) != StopThrottled {
		t.Errorf("next Get err = %v, want throttled", err)
	}
}

// With a budget of two, a 429 is retried once; a second 429 is throttled, a 200 passes.
func TestBudgetOfTwoRetriesA429(t *testing.T) {
	for _, tc := range []struct {
		name   string
		second int
		want   StopReason
	}{
		{"429 then 429", http.StatusTooManyRequests, StopThrottled},
		{"429 then 200", http.StatusOK, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var n atomic.Int32
			srv, hits := countingServer(t, func(w http.ResponseWriter, _ *http.Request) {
				if n.Add(1) == 1 {
					w.Header().Set("Retry-After", "0")
					w.WriteHeader(http.StatusTooManyRequests)
					return
				}
				w.Header().Set("Retry-After", "0")
				w.WriteHeader(tc.second)
			})
			clk, rec := newFakeClock(), &memRecorder{}
			c := New(append(testOptions(clk, rec), WithBreaker(NewBreaker(Budgets{KindCanary: 2})))...)
			_, err := c.Get(context.Background(), KindCanary, srv.URL)
			if tc.want == "" && err != nil {
				t.Fatalf("Get: %v", err)
			}
			if tc.want != "" && reasonOf(t, err) != tc.want {
				t.Fatalf("err = %v, want %s", err, tc.want)
			}
			if hits.Load() != 2 || c.Breaker.Reason() != tc.want {
				t.Errorf("hits %d, reason %q; want 2, %q", hits.Load(), c.Breaker.Reason(), tc.want)
			}
		})
	}
}

func TestRetryAfterSecondsHonoured(t *testing.T) {
	var n atomic.Int32
	srv, hits := countingServer(t, func(w http.ResponseWriter, _ *http.Request) {
		if n.Add(1) == 1 {
			w.Header().Set("Retry-After", "2")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte("ok"))
	})
	clk, rec := newFakeClock(), &memRecorder{}
	c := New(testOptions(clk, rec)...)

	resp, err := c.Get(context.Background(), KindSearch, srv.URL)
	if err != nil || resp.Status != 200 {
		t.Fatalf("Get = %d, %v", resp.Status, err)
	}
	if hits.Load() != 2 {
		t.Errorf("hits = %d, want 2", hits.Load())
	}
	if s := clk.slept(); len(s) != 1 || s[0] != 2*time.Second {
		t.Errorf("sleeps = %v, want [2s]", s)
	}
	if c.Breaker.Reason() != "" {
		t.Errorf("a 429 then a 200 opened the breaker: %q", c.Breaker.Reason())
	}
	if recs := rec.all(); len(recs) != 2 || recs[0].Status != 429 || recs[1].Status != 200 {
		t.Errorf("records = %+v", recs)
	}
}

func TestRetryAfterValues(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	tests := []struct {
		name string
		in   string
		want time.Duration
	}{
		{"seconds", "2", 2 * time.Second},
		{"http date", now.Add(7 * time.Second).Format(http.TimeFormat), 7 * time.Second},
		{"date in the past", now.Add(-time.Minute).Format(http.TimeFormat), 0},
		{"capped seconds", "600", MaxRetryAfter},
		{"capped date", now.Add(time.Hour).Format(http.TimeFormat), MaxRetryAfter},
		{"huge seconds", "99999999999999999999", MaxRetryAfter},
		{"missing", "", DefaultRetryAfter},
		{"garbage", "soon", DefaultRetryAfter},
		{"negative", "-3", DefaultRetryAfter},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := RetryAfter(tt.in, now); got != tt.want {
				t.Errorf("RetryAfter(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestRetryAfterHTTPDateInGet(t *testing.T) {
	clk, rec := newFakeClock(), &memRecorder{}
	var n atomic.Int32
	srv, _ := countingServer(t, func(w http.ResponseWriter, _ *http.Request) {
		if n.Add(1) == 1 {
			w.Header().Set("Retry-After", clk.Now().Add(5*time.Second).Format(http.TimeFormat))
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte("ok"))
	})
	c := New(testOptions(clk, rec)...)
	if _, err := c.Get(context.Background(), KindSearch, srv.URL); err != nil {
		t.Fatal(err)
	}
	if s := clk.slept(); len(s) != 1 || s[0] != 5*time.Second {
		t.Errorf("sleeps = %v, want [5s]", s)
	}
}

func TestRetryAfterCappedInGet(t *testing.T) {
	var n atomic.Int32
	srv, _ := countingServer(t, func(w http.ResponseWriter, _ *http.Request) {
		if n.Add(1) == 1 {
			w.Header().Set("Retry-After", "3600")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte("ok"))
	})
	clk, rec := newFakeClock(), &memRecorder{}
	c := New(testOptions(clk, rec)...)
	if _, err := c.Get(context.Background(), KindSearch, srv.URL); err != nil {
		t.Fatal(err)
	}
	if s := clk.slept(); len(s) != 1 || s[0] != 120*time.Second {
		t.Errorf("sleeps = %v, want [2m0s]", s)
	}
}

func TestBlocked(t *testing.T) {
	tests := []struct {
		name    string
		handler http.HandlerFunc
		path    string
	}{
		{"status 999", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(999) }, "/job"},
		{"status 403", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) }, "/job"},
		{"redirect to authwall", func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/authwall" {
				_, _ = w.Write([]byte("sign in"))
				return
			}
			http.Redirect(w, r, "/authwall?trk=x", http.StatusFound)
		}, "/job"},
		{"redirect to login", func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, "/login") {
				_, _ = w.Write([]byte("sign in"))
				return
			}
			http.Redirect(w, r, "/login", http.StatusFound)
		}, "/job"},
		{"redirect to checkpoint", func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, "/checkpoint") {
				_, _ = w.Write([]byte("check"))
				return
			}
			http.Redirect(w, r, "/checkpoint/challenge", http.StatusFound)
		}, "/job"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, hits := countingServer(t, tt.handler)
			clk, rec := newFakeClock(), &memRecorder{}
			c := New(append(testOptions(clk, rec), WithProfile(LinkedInJobs))...)

			_, err := c.Get(context.Background(), KindDetail, srv.URL+tt.path)
			if r := reasonOf(t, err); r != StopBlocked {
				t.Fatalf("reason = %q, want blocked", r)
			}
			before := hits.Load()
			if _, err := c.Get(context.Background(), KindDetail, srv.URL+tt.path); !errors.Is(err, ErrBreakerOpen) {
				t.Fatalf("second Get err = %v", err)
			}
			if hits.Load() != before {
				t.Errorf("an open breaker made a request")
			}
			recs := rec.all()
			if len(recs) != 1 {
				t.Fatalf("records = %d, want 1", len(recs))
			}
			if strings.HasPrefix(tt.name, "redirect") && recs[0].FinalURL == "" {
				t.Errorf("redirect recorded without its final URL: %+v", recs[0])
			}
		})
	}
}

func TestBlockedNeedsProfile(t *testing.T) {
	srv, _ := countingServer(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) })
	clk, rec := newFakeClock(), &memRecorder{}
	c := New(testOptions(clk, rec)...) // the zero Profile blocks on nothing
	_, err := c.Get(context.Background(), KindBoard, srv.URL)
	if !errors.Is(err, ErrStatus) || errors.Is(err, ErrBreakerOpen) {
		t.Errorf("err = %v, want ErrStatus without opening", err)
	}
}

func TestFailureRate(t *testing.T) {
	srv, hits := countingServer(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadGateway) })
	clk, rec := newFakeClock(), &memRecorder{}
	c := New(testOptions(clk, rec)...)
	ctx := context.Background()

	for i := range 4 {
		_, err := c.Get(ctx, KindSearch, srv.URL)
		if !errors.Is(err, ErrStatus) || errors.Is(err, ErrBreakerOpen) {
			t.Fatalf("Get %d err = %v, want ErrStatus and still closed", i+1, err)
		}
	}
	_, err := c.Get(ctx, KindSearch, srv.URL)
	if r := reasonOf(t, err); r != StopFailureRate {
		t.Fatalf("fifth Get reason = %q, want failure_rate", r)
	}
	if _, err := c.Get(ctx, KindSearch, srv.URL); !errors.Is(err, ErrBreakerOpen) {
		t.Fatalf("sixth Get err = %v", err)
	}
	if hits.Load() != 5 {
		t.Errorf("hits = %d, want 5", hits.Load())
	}
	if c.Breaker.Requests() != 5 || c.Breaker.Failures() != 5 {
		t.Errorf("requests %d failures %d", c.Breaker.Requests(), c.Breaker.Failures())
	}
}

func TestFailureRateHalfStaysClosed(t *testing.T) {
	var n atomic.Int32
	srv, _ := countingServer(t, func(w http.ResponseWriter, _ *http.Request) {
		if n.Add(1)%2 == 0 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte("ok"))
	})
	clk, rec := newFakeClock(), &memRecorder{}
	c := New(testOptions(clk, rec)...)
	for range 6 { // 3 ok, 3 failed: exactly 50% is not over 50%
		_, _ = c.Get(context.Background(), KindSearch, srv.URL)
	}
	if c.Breaker.Reason() != "" {
		t.Errorf("reason = %q, want closed at 50%%", c.Breaker.Reason())
	}
}

func TestTransportFailuresCountAndAreRecorded(t *testing.T) {
	st := &stubTransport{fn: func(*http.Request) (*http.Response, error) { return nil, errors.New("connection reset") }}
	clk, rec := newFakeClock(), &memRecorder{}
	c := New(append(testOptions(clk, rec), WithTransport(st), WithLiveGate(config.LiveAll))...)
	ctx := context.Background()
	var err error
	for range 5 {
		_, err = c.Get(ctx, KindBoard, "https://jobs.example/board")
	}
	if r := reasonOf(t, err); r != StopFailureRate {
		t.Fatalf("reason = %q", r)
	}
	recs := rec.all()
	if len(recs) != 5 {
		t.Fatalf("records = %d, want 5", len(recs))
	}
	for _, r := range recs {
		if r.Status != 0 || !strings.Contains(r.Err, "connection reset") || r.URL != "https://jobs.example/board" || r.Kind != KindBoard {
			t.Errorf("record = %+v", r)
		}
	}
}

func TestBudgetsIndependent(t *testing.T) {
	srv, hits := countingServer(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	clk, rec := newFakeClock(), &memRecorder{}
	b := NewBreaker(Budgets{KindSearch: 2, KindDetail: 1})
	c := New(append(testOptions(clk, rec), WithBreaker(b))...)
	ctx := context.Background()

	for i := range 2 {
		if _, err := c.Get(ctx, KindSearch, srv.URL); err != nil {
			t.Fatalf("search %d: %v", i+1, err)
		}
	}
	_, err := c.Get(ctx, KindSearch, srv.URL)
	if r := reasonOf(t, err); r != StopBudget {
		t.Fatalf("third search reason = %q, want budget", r)
	}
	var oe *OpenError
	if errors.As(err, &oe); oe.Kind != KindSearch {
		t.Errorf("OpenError.Kind = %q", oe.Kind)
	}
	if hits.Load() != 2 {
		t.Fatalf("hits = %d, want 2", hits.Load())
	}
	// The search budget is spent; detail still has its own.
	if _, err := c.Get(ctx, KindDetail, srv.URL); err != nil {
		t.Fatalf("detail after search budget: %v", err)
	}
	if _, err := c.Get(ctx, KindDetail, srv.URL); reasonOf(t, err) != StopBudget {
		t.Fatalf("second detail err = %v", err)
	}
	// Kinds without a budget are unlimited.
	if _, err := c.Get(ctx, KindCanary, srv.URL); err != nil {
		t.Fatalf("canary: %v", err)
	}
	if hits.Load() != 4 {
		t.Errorf("hits = %d, want 4", hits.Load())
	}
	if b.Reason() != StopBudget || b.Used(KindSearch) != 2 || b.Used(KindDetail) != 1 {
		t.Errorf("reason %q, used search %d detail %d", b.Reason(), b.Used(KindSearch), b.Used(KindDetail))
	}
}

func TestRecorderSeesEveryResponse(t *testing.T) {
	var n atomic.Int32
	srv, _ := countingServer(t, func(w http.ResponseWriter, _ *http.Request) {
		switch n.Add(1) {
		case 1:
			_, _ = w.Write([]byte("a"))
		case 2:
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
		case 3:
			_, _ = w.Write([]byte("b"))
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	})
	st := &stubTransport{fn: func(*http.Request) (*http.Response, error) { return nil, errors.New("dial refused") }}
	clk, rec := newFakeClock(), &memRecorder{}
	c := New(testOptions(clk, rec)...)
	ctx := context.Background()
	_, _ = c.Get(ctx, KindSearch, srv.URL) // 200
	_, _ = c.Get(ctx, KindSearch, srv.URL) // 429 then 200
	_, _ = c.Get(ctx, KindDetail, srv.URL) // 500
	broken := New(append(testOptions(clk, rec), WithTransport(st), WithLiveGate(config.LiveAll))...)
	_, _ = broken.Get(ctx, KindDetail, "https://jobs.example/x") // transport error

	var statuses []int
	for _, r := range rec.all() {
		statuses = append(statuses, r.Status)
	}
	want := []int{200, 429, 200, 500, 0}
	if len(statuses) != len(want) {
		t.Fatalf("statuses = %v, want %v", statuses, want)
	}
	for i := range want {
		if statuses[i] != want[i] {
			t.Fatalf("statuses = %v, want %v", statuses, want)
		}
	}
}

func TestRecorderErrorIsReturned(t *testing.T) {
	srv, _ := countingServer(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	clk := newFakeClock()
	rec := &memRecorder{err: errors.New("db down")}
	c := New(testOptions(clk, rec)...)
	if _, err := c.Get(context.Background(), KindSearch, srv.URL); err == nil || !strings.Contains(err.Error(), "db down") {
		t.Errorf("err = %v, want the recorder's error", err)
	}
}

func TestNoRecorderRefuses(t *testing.T) {
	srv, hits := countingServer(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	c := New(WithLimiter(rate.NewLimiter(rate.Inf, 1)), WithJitter(0), WithClock(newFakeClock()))
	if _, err := c.Get(context.Background(), KindSearch, srv.URL); !errors.Is(err, ErrNoRecorder) {
		t.Errorf("err = %v, want ErrNoRecorder", err)
	}
	if hits.Load() != 0 {
		t.Errorf("a request went out with nowhere to record it")
	}
}

func TestLimiterShared(t *testing.T) {
	srv, _ := countingServer(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	clk, rec := newFakeClock(), &memRecorder{}
	lim := rate.NewLimiter(rate.Every(time.Second), 1)
	// Two clients on one limiter, as the LinkedIn job and company clients share one per run.
	a := New(append(testOptions(clk, rec), WithLimiter(lim))...)
	b := New(append(testOptions(clk, rec), WithLimiter(lim))...)
	ctx := context.Background()
	for _, c := range []*Client{a, b, a} {
		if _, err := c.Get(ctx, KindSearch, srv.URL); err != nil {
			t.Fatal(err)
		}
	}
	if got := clk.total(); got < 2*time.Second {
		t.Errorf("waited %v (%v), want at least 2s", got, clk.slept())
	}
}

func TestJitterAdded(t *testing.T) {
	srv, _ := countingServer(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	clk, rec := newFakeClock(), &memRecorder{}
	var asked int64
	c := New(append(testOptions(clk, rec), WithJitter(1500*time.Millisecond), WithRand(func(n int64) int64 {
		asked = n
		return n - 1
	}))...)
	if _, err := c.Get(context.Background(), KindSearch, srv.URL); err != nil {
		t.Fatal(err)
	}
	if asked != int64(1500*time.Millisecond)+1 {
		t.Errorf("rand asked for %d", asked)
	}
	if s := clk.slept(); len(s) != 1 || s[0] != 1500*time.Millisecond {
		t.Errorf("sleeps = %v, want [1.5s]", s)
	}
}

func TestBodyCapped(t *testing.T) {
	srv, _ := countingServer(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("0123456789")) })
	clk, rec := newFakeClock(), &memRecorder{}
	c := New(append(testOptions(clk, rec), WithMaxBody(4))...)
	resp, err := c.Get(context.Background(), KindSite, srv.URL)
	if !errors.Is(err, ErrBodyTooLarge) || string(resp.Body) != "0123" {
		t.Fatalf("Get = %q, %v", resp.Body, err)
	}
	if recs := rec.all(); len(recs) != 1 || recs[0].Err == "" || string(recs[0].Body) != "0123" {
		t.Errorf("records = %+v", recs)
	}
}

func TestUnknownKindAndBadURL(t *testing.T) {
	clk, rec := newFakeClock(), &memRecorder{}
	c := New(append(testOptions(clk, rec), WithTransport(failTransport{t}), WithLiveGate(0))...)
	ctx := context.Background()
	if _, err := c.Get(ctx, Kind("crawl"), "http://127.0.0.1/"); !errors.Is(err, ErrUnknownKind) {
		t.Errorf("unknown kind err = %v", err)
	}
	for _, u := range []string{"file:///etc/hosts", "mailto:a@jobs.example", "://", "http:///nohost"} {
		if _, err := c.Get(ctx, KindSite, u); !errors.Is(err, ErrBadURL) {
			t.Errorf("Get(%q) err = %v, want ErrBadURL", u, err)
		}
	}
}

func TestContextCancelledIsNotAFailure(t *testing.T) {
	st := &stubTransport{fn: func(r *http.Request) (*http.Response, error) { return nil, r.Context().Err() }}
	clk, rec := newFakeClock(), &memRecorder{}
	c := New(append(testOptions(clk, rec), WithTransport(st), WithLiveGate(config.LiveAll))...)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for range 6 {
		_, _ = c.Get(ctx, KindSearch, "https://jobs.example/")
	}
	if c.Breaker.Failures() != 0 || c.Breaker.Reason() != "" {
		t.Errorf("cancelled requests counted: failures %d, reason %q", c.Breaker.Failures(), c.Breaker.Reason())
	}
}
