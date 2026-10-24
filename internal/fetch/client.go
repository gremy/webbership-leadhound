package fetch

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"golang.org/x/time/rate"

	"github.com/gremy/webbership-leadhound/internal/config"
)

// Defaults for a Client built by New.
const (
	DefaultTimeout    = 20 * time.Second
	DefaultJitter     = 1500 * time.Millisecond
	DefaultMaxBody    = 5 << 20           // 5 MB
	DefaultRetryAfter = 30 * time.Second  // a 429 without a usable Retry-After
	MaxRetryAfter     = 120 * time.Second // the longest Retry-After honoured
	maxRedirects      = 10
)

// Client fetches pages for one source in one run. Build it with New; its fields are not
// meant to change after that. It is safe for concurrent use.
type Client struct {
	Limiter   *rate.Limiter // shared by every request in the run
	Jitter    time.Duration // up to this much random delay is added after each limiter wait
	HTTP      *http.Client
	Recorder  Recorder
	Breaker   *Breaker
	Profile   Profile
	UserAgent string
	MaxBody   int64 // bytes of body read; more is ErrBodyTooLarge

	clock     Clock
	randInt   func(n int64) int64 // [0, n)
	transport http.RoundTripper   // nil: the real network
	fixtures  bool                // transport is a *FixtureTransport given by WithFixtures
	live      config.Live

	publicOnly   bool          // WithPublicOnly: the SSRF guard is on
	timeout      time.Duration // the http.Client timeout
	maxRedirects int           // requests in a redirect chain before CheckRedirect stops
}

// Option configures a Client.
type Option func(*Client)

// WithLimiter sets the run's shared limiter: rate.Every(interval) with burst 1.
func WithLimiter(l *rate.Limiter) Option { return func(c *Client) { c.Limiter = l } }

// WithJitter sets the most random delay added after each limiter wait (0: none).
func WithJitter(max time.Duration) Option { return func(c *Client) { c.Jitter = max } }

// WithRand sets the jitter source; it returns a value in [0, n).
func WithRand(f func(n int64) int64) Option { return func(c *Client) { c.randInt = f } }

// WithClock sets the clock waits are measured and slept on.
func WithClock(clk Clock) Option { return func(c *Client) { c.clock = clk } }

// WithRecorder sets where every response is recorded. Get refuses to fetch without one.
func WithRecorder(r Recorder) Option { return func(c *Client) { c.Recorder = r } }

// WithBreaker sets the run's shared breaker.
func WithBreaker(b *Breaker) Option { return func(c *Client) { c.Breaker = b } }

// WithProfile sets the headers and blocked responses; the profile is copied.
func WithProfile(p Profile) Option { return func(c *Client) { c.Profile = p.clone() } }

// WithUserAgent sets the User-Agent (default UserAgentFor("dev")).
func WithUserAgent(ua string) Option { return func(c *Client) { c.UserAgent = ua } }

// WithMaxBody sets how many body bytes are read (default DefaultMaxBody).
func WithMaxBody(n int64) Option { return func(c *Client) { c.MaxBody = n } }

// WithTimeout sets the whole-request timeout (default DefaultTimeout).
func WithTimeout(d time.Duration) Option { return func(c *Client) { c.timeout = d } }

// WithMaxRedirects sets n, the most requests in one Get's redirect chain (the first request
// included, as net/http counts its own default), so at most n-1 redirects are followed and
// the n-th is refused. Following up to 3 redirects is WithMaxRedirects(4), which sites use.
// The default is 10.
func WithMaxRedirects(n int) Option { return func(c *Client) { c.maxRedirects = n } }

// WithTransport replaces the network with rt, for tests. The live gate still applies: a
// host that is not loopback goes out only when WithLiveGate allows it.
func WithTransport(rt http.RoundTripper) Option {
	return func(c *Client) { c.transport, c.fixtures = rt, false }
}

// WithFixtures serves every request from ft. Fixtures are not the network, so this is the
// one transport the live gate does not apply to.
func WithFixtures(ft *FixtureTransport) Option {
	return func(c *Client) { c.transport, c.fixtures = ft, ft != nil }
}

// New builds a Client. Without options it paces at config.DefaultLinkedInInterval with
// DefaultJitter, has an unlimited breaker of its own, sends only the User-Agent, and refuses
// every host but loopback (LEADHOUND_LIVE unset). Every client is live-gated but one built
// WithFixtures.
func New(opts ...Option) *Client {
	c := &Client{
		Jitter:    DefaultJitter,
		UserAgent: UserAgentFor("dev"),
		MaxBody:   DefaultMaxBody,
		clock:     realClock{},
		randInt:   jitterRand,

		timeout:      DefaultTimeout,
		maxRedirects: maxRedirects,
	}
	for _, o := range opts {
		o(c)
	}
	if c.Limiter == nil {
		c.Limiter = rate.NewLimiter(rate.Every(config.DefaultLinkedInInterval), 1)
	}
	if c.Breaker == nil {
		c.Breaker = NewBreaker(nil)
	}
	transport := c.transport
	if transport == nil && c.publicOnly {
		transport = guardedTransport()
	}
	c.HTTP = &http.Client{
		Transport: transport,
		Timeout:   c.timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= c.maxRedirects {
				return fmt.Errorf("fetch: stopped after %d redirects", c.maxRedirects)
			}
			return c.checkURL(req.URL)
		},
	}
	return c
}

// jitterRand is the default jitter source. Jitter spreads requests out; it guards nothing,
// so it needs no cryptographic randomness.
func jitterRand(n int64) int64 { return rand.Int64N(n) } //nolint:gosec // G404: jitter, not a secret

// Get fetches rawURL as kind. It waits on the shared limiter plus jitter, records the
// response through the Recorder before returning, and on a 429 sleeps for Retry-After
// (capped at MaxRetryAfter) and retries once. A 429 whose retry the budget refuses opens
// the breaker as throttled, not budget: the 429 is the last word from the site.
//
// A 2xx or a 404 returns a nil error. Another status returns the Response and ErrStatus.
// Once the breaker is open, or this kind's budget is spent, Get returns an *OpenError
// (errors.Is ErrBreakerOpen) without a request; the request that opens it returns its
// Response with the *OpenError.
func (c *Client) Get(ctx context.Context, kind Kind, rawURL string) (Response, error) {
	if !kind.valid() {
		return Response{}, fmt.Errorf("%w: %q", ErrUnknownKind, kind)
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return Response{}, fmt.Errorf("%w: %w", ErrBadURL, err)
	}
	if err := c.checkURL(u); err != nil {
		return Response{}, err
	}
	if c.Recorder == nil {
		return Response{}, ErrNoRecorder
	}

	resp, header, err := c.attempt(ctx, kind, u)
	if err == nil && resp.Status == http.StatusTooManyRequests {
		wait := RetryAfter(header.Get("Retry-After"), c.clock.Now())
		if err := c.clock.Sleep(ctx, wait); err != nil {
			return resp, fmt.Errorf("fetch: %s %s: wait to retry: %w", kind, u.Redacted(), err)
		}
		retry, _, rerr := c.attempt(ctx, kind, u)
		var open *OpenError
		if errors.As(rerr, &open) && open.Reason == StopBudget && open.Err == nil {
			// No retry went out: the run's last answer was the 429.
			return resp, &OpenError{Reason: c.Breaker.open(StopThrottled)}
		}
		resp, err = retry, rerr
	}
	if err != nil {
		return resp, err
	}
	if resp.Status/100 == 2 || resp.Status == http.StatusNotFound {
		return resp, nil
	}
	return resp, fmt.Errorf("fetch: %s %s: %w %d", kind, u.Redacted(), ErrStatus, resp.Status)
}

// attempt makes one request, records it and tells the breaker. Its error is a breaker,
// pacing, transport, body or recording error; the status is left to Get.
func (c *Client) attempt(ctx context.Context, kind Kind, u *url.URL) (Response, http.Header, error) {
	if err := c.Breaker.take(kind); err != nil {
		return Response{}, nil, err
	}
	if err := c.pace(ctx); err != nil {
		return Response{}, nil, fmt.Errorf("fetch: %s %s: wait: %w", kind, u.Redacted(), err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return Response{}, nil, fmt.Errorf("fetch: %s %s: %w", kind, u.Redacted(), err)
	}
	for k, vs := range c.Profile.Header {
		req.Header[k] = append([]string(nil), vs...)
	}
	req.Header.Set("User-Agent", c.UserAgent)

	res, err := c.HTTP.Do(req)
	at := c.clock.Now()
	if err != nil && res != nil {
		// A redirect CheckRedirect refused: the 3xx that named it came back (body already
		// closed), so it is recorded and counted as the response it was.
		return c.refusedRedirect(ctx, kind, u, res, err, at)
	}
	if err != nil {
		recErr := c.record(ctx, Record{Kind: kind, URL: u.String(), Err: err.Error(), FetchedAt: at})
		o := outcomeFailure
		if ctx.Err() != nil || errors.Is(err, ErrLiveOff) {
			o = outcomeNeutral
		}
		opened := c.Breaker.observe(o)
		return Response{FetchedAt: at}, nil, finish(fmt.Errorf("fetch: %s %s: %w", kind, u.Redacted(), err), recErr, opened)
	}
	defer func() { _ = res.Body.Close() }()

	body, readErr := readCapped(res.Body, c.MaxBody)
	final := res.Request.URL
	resp := Response{
		Status:      res.StatusCode,
		FinalURL:    final.String(),
		Body:        body,
		ContentType: res.Header.Get("Content-Type"),
		Header:      allowedHeaders(res.Header),
		FetchedAt:   at,
	}
	sum := sha256.Sum256(body)
	rec := Record{
		Kind:        kind,
		URL:         u.String(),
		Status:      res.StatusCode,
		Body:        body,
		ContentType: resp.ContentType,
		SHA256:      sum[:],
		Header:      resp.Header,
		FetchedAt:   at,
	}
	if resp.FinalURL != rec.URL {
		rec.FinalURL = resp.FinalURL
	}
	if readErr != nil {
		rec.Err = readErr.Error()
		readErr = fmt.Errorf("fetch: %s %s: read body: %w", kind, u.Redacted(), readErr)
	}
	recErr := c.record(ctx, rec)
	opened := c.Breaker.observe(c.classify(res.StatusCode, final.Path, readErr))
	return resp, res.Header, finish(readErr, recErr, opened)
}

// refusedRedirect records the 3xx whose redirect CheckRedirect refused and tells the breaker.
// A refusal by the live gate is not a failure; a redirect loop or a bad target is.
func (c *Client) refusedRedirect(ctx context.Context, kind Kind, u *url.URL, res *http.Response, err error, at time.Time) (Response, http.Header, error) {
	final := res.Request.URL
	resp := Response{Status: res.StatusCode, FinalURL: final.String(), ContentType: res.Header.Get("Content-Type"), Header: allowedHeaders(res.Header), FetchedAt: at}
	rec := Record{Kind: kind, URL: u.String(), Status: res.StatusCode, ContentType: resp.ContentType, Err: err.Error(), Header: resp.Header, FetchedAt: at}
	if resp.FinalURL != rec.URL {
		rec.FinalURL = resp.FinalURL
	}
	recErr := c.record(ctx, rec)
	o := c.classify(res.StatusCode, final.Path, nil)
	if o == outcomeOK && !errors.Is(err, ErrLiveOff) {
		o = outcomeFailure
	}
	opened := c.Breaker.observe(o)
	return resp, res.Header, finish(fmt.Errorf("fetch: %s %s: %w", kind, u.Redacted(), err), recErr, opened)
}

// classify turns a response into the breaker's outcome.
func (c *Client) classify(status int, path string, readErr error) outcome {
	switch {
	case readErr != nil && !errors.Is(readErr, ErrBodyTooLarge):
		return outcomeFailure
	case c.Profile.blocked(status, path):
		return outcomeBlocked
	case status == http.StatusTooManyRequests:
		return outcomeThrottle
	case status >= 500:
		return outcomeFailure
	default:
		return outcomeOK
	}
}

// pace waits for the shared limiter, then for a random jitter.
func (c *Client) pace(ctx context.Context) error {
	now := c.clock.Now()
	r := c.Limiter.ReserveN(now, 1)
	if !r.OK() {
		return errors.New("limiter allows no requests (burst 0)")
	}
	d := r.DelayFrom(now)
	if c.Jitter > 0 {
		d += time.Duration(c.randInt(int64(c.Jitter) + 1))
	}
	if d <= 0 {
		return nil
	}
	if err := c.clock.Sleep(ctx, d); err != nil {
		r.CancelAt(c.clock.Now())
		return err
	}
	return nil
}

func (c *Client) record(ctx context.Context, r Record) error {
	if err := c.Recorder.Record(ctx, r); err != nil {
		return fmt.Errorf("fetch: record %s %s: %w", r.Kind, r.URL, err)
	}
	return nil
}

// finish combines a request's own error, its recording error and a breaker that opened on
// it into the one error attempt returns.
func finish(err, recErr error, opened StopReason) error {
	err = errors.Join(err, recErr)
	if opened != "" {
		return &OpenError{Reason: opened, Err: err}
	}
	return err
}

// headerAllowlist is every response header copied into Response.Header and Record.Header;
// no other header is kept.
var headerAllowlist = []string{
	"X-Generator", "X-Drupal-Cache", "X-Drupal-Dynamic-Cache", "Expires",
	"Content-Type", "Server", "Cf-Mitigated", "Retry-After",
}

// allowedHeaders copies the allowlisted headers of h; nil when h has none of them.
func allowedHeaders(h http.Header) http.Header {
	var out http.Header
	for _, k := range headerAllowlist {
		if vs := h.Values(k); len(vs) > 0 {
			if out == nil {
				out = http.Header{}
			}
			out[k] = append([]string(nil), vs...)
		}
	}
	return out
}

// readCapped reads at most max bytes; a longer body is cut to max and ErrBodyTooLarge.
func readCapped(r io.Reader, max int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return b, err
	}
	if int64(len(b)) > max {
		return b[:max], fmt.Errorf("%w (%d bytes)", ErrBodyTooLarge, max)
	}
	return b, nil
}

// RetryAfter reads a Retry-After value, in seconds or as an HTTP date, as a wait from now:
// never negative, at most MaxRetryAfter, and DefaultRetryAfter when it is missing or unusable.
func RetryAfter(v string, now time.Time) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return DefaultRetryAfter
	}
	if isDigits(v) {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n > int64(MaxRetryAfter/time.Second) {
			return MaxRetryAfter
		}
		return time.Duration(n) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		return min(max(t.Sub(now), 0), MaxRetryAfter)
	}
	return DefaultRetryAfter
}

func isDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}
