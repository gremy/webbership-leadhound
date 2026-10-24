// Package probe runs the Drupal version probe over a list of sites: robots.txt first, then
// the homepage, then drupal.Detect, and one transaction per site that stores what was seen.
//
// Each site gets its own fetch.Client from the caller's factory (its own breaker, so one
// site's failures never stop the run), and every client shares the run's limiter and one
// recorder. Run stops the whole run on its own "network down" rule: at least 100 sites
// done and over 80% of the last 100 ended in transport errors.
package probe

import (
	"context"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/gremy/webbership-leadhound/internal/drupal"
	"github.com/gremy/webbership-leadhound/internal/fetch"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/robots"
	"github.com/gremy/webbership-leadhound/internal/store"
)

// Probe-level signal names, beside drupal's own. Each carries the fetch it came from.
const (
	SigRobots    = "robots"     // value: why robots.txt stopped the probe (status N, unreachable, disallowed)
	SigNotPublic = "not_public" // a dial or URL refused by the public-only guard
	SigHomepage  = "homepage"   // value: why the homepage was not read (status N, unreachable, redirects, content type)
	SigRedirect  = "redirect"   // value: the final host, when it differs from the listed one
)

// StopFailureRate is the run's stop reason under the network-down rule.
const StopFailureRate = string(fetch.StopFailureRate)

// The network-down rule: at least downMinSites sites done, and more than downThreshold of
// the last downWindow ended in counted transport errors.
const (
	downMinSites  = 100
	downWindow    = 100
	downThreshold = 80
)

// Recorder is the run's one fetch recorder: store.FetchRecorder, or a fake in tests.
type Recorder interface {
	fetch.Recorder
	LastIDFor(rawURL string) (int64, bool)
}

// FetchHeader is the allowlisted headers of one fetch, to keep in fetch_header.
type FetchHeader struct {
	FetchID int64
	Header  http.Header
}

// FetchBody is a fetch body to attach to its size-only http_fetch row.
type FetchBody struct {
	FetchID int64
	Body    []byte
}

// Write is everything one site's probe stores, in one transaction.
type Write struct {
	SiteID  int64
	Headers []FetchHeader
	Bodies  []FetchBody // only on a Drupal hit: the homepage and the robots.txt answers
	Probe   leadstore.ProbeRow
}

// Deps is what Run needs from the command.
type Deps struct {
	// NewClient builds one site's client around rec; Run calls it once per site.
	NewClient func(rec fetch.Recorder) *fetch.Client
	// Recorder is the run's one recorder; Run serialises its writes.
	Recorder Recorder
	// Save stores one site's Write; an error stops the run and is returned.
	Save func(ctx context.Context, w Write) error
}

// Options tune one run.
type Options struct {
	Concurrency int // sites probed at once; below 1 means 1
}

// Summary is what one run did.
type Summary struct {
	Sites       int            // sites probed and stored
	Drupal      int            // of them, Drupal
	ByMajor     map[int]int    // Drupal major → sites (0: major unknown)
	Ambiguous   map[string]int // Drupal sites with an ambiguous major, by its label
	Exact       map[string]int // exact core versions seen → sites
	Outcomes    map[string]int // outcome → sites
	NotPublic   int            // sites refused by the public-only guard (outcome transport_error)
	Interrupted int            // sites cut short by a stop or a cancel; nothing stored, still due
	Requests    int            // HTTP requests sent, every request of a redirect chain included
	Failures    int            // failed requests the clients counted
	StopReason  string         // "failure_rate" under the network-down rule, else ""
	Cancelled   bool           // ctx ended the run
	Elapsed     time.Duration
}

func newSummary() Summary {
	return Summary{
		ByMajor: map[int]int{}, Ambiguous: map[string]int{}, Exact: map[string]int{}, Outcomes: map[string]int{},
	}
}

func (s *Summary) add(r siteResult) {
	p := r.write.Probe
	s.Sites++
	s.Outcomes[p.Outcome]++
	if r.notPublic {
		s.NotPublic++
	}
	if !p.IsDrupal {
		return
	}
	s.Drupal++
	s.ByMajor[p.Major]++
	if p.Ambiguous != "" {
		s.Ambiguous[p.Ambiguous]++
	}
	if p.ExactVersion != "" {
		s.Exact[p.ExactVersion]++
	}
}

// siteResult is one site's probe as a worker hands it back.
type siteResult struct {
	write       Write
	counted     bool // a transport error that counts toward the network-down rule
	notPublic   bool
	interrupted bool
	requests    int
	failures    int
	err         error // a recording error: the run must stop
}

// Run probes sites with opts.Concurrency workers and stores each finished site through
// d.Save. It returns when every site is done, the network-down rule stops the run
// (StopReason "failure_rate"), ctx ends (Cancelled), or a recording or Save error stops it
// (returned). A site cut short by a stop or a cancel stores nothing and stays due.
func Run(ctx context.Context, d Deps, sites []leadstore.Site, opts Options) (Summary, error) {
	start := time.Now()
	n := max(opts.Concurrency, 1)
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	shared := &sharedRecorder{inner: d.Recorder}
	jobs := make(chan leadstore.Site)
	results := make(chan siteResult)
	var wg sync.WaitGroup
	for range n {
		wg.Go(func() {
			for s := range jobs {
				results <- probeSite(runCtx, d, shared, s)
			}
		})
	}
	go func() {
		defer close(jobs)
		for _, s := range sites {
			select {
			case jobs <- s:
			case <-runCtx.Done():
				return
			}
		}
	}()
	go func() { wg.Wait(); close(results) }()

	sum := newSummary()
	var window []bool // the last downWindow stored sites: counted transport error or not
	var firstErr error
	for r := range results {
		sum.Requests += r.requests
		sum.Failures += r.failures
		switch {
		case firstErr != nil:
			continue
		case r.err != nil:
			firstErr = r.err
			cancel()
			continue
		case r.interrupted:
			sum.Interrupted++
			continue
		}
		if err := d.Save(context.WithoutCancel(ctx), r.write); err != nil {
			firstErr = fmt.Errorf("probe: save site %d: %w", r.write.SiteID, err)
			cancel()
			continue
		}
		sum.add(r)
		window = append(window, r.counted)
		if len(window) > downWindow {
			window = window[1:]
		}
		if sum.StopReason == "" && sum.Sites >= downMinSites && countTrue(window) > downThreshold {
			sum.StopReason = StopFailureRate
			cancel()
		}
	}
	sum.Cancelled = ctx.Err() != nil
	sum.Elapsed = time.Since(start)
	return sum, firstErr
}

func countTrue(bs []bool) int {
	n := 0
	for _, b := range bs {
		if b {
			n++
		}
	}
	return n
}

// sharedRecorder serialises the run's recorder, so the id read back after a Record is the
// row that Record wrote even when two sites request the same URL.
type sharedRecorder struct {
	mu    sync.Mutex
	inner Recorder
}

func (s *sharedRecorder) record(ctx context.Context, r fetch.Record) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.inner.Record(ctx, r); err != nil {
		return 0, err
	}
	id, ok := s.inner.LastIDFor(r.URL)
	if !ok {
		return 0, fmt.Errorf("probe: no fetch id after recording %s", r.URL)
	}
	return id, nil
}

// fetched is one recorded fetch of a site.
type fetched struct {
	id  int64
	rec fetch.Record
}

// siteRecorder is one site's view of the shared recorder: it keeps every fetch the site's
// client recorded, in order. A site's client fetches one URL at a time.
type siteRecorder struct {
	shared  *sharedRecorder
	fetches []fetched
	err     error
}

// Record writes r without the run's cancel, so a fetch that ends because the run stopped is
// still recorded rather than turning the stop into a recording error.
func (s *siteRecorder) Record(ctx context.Context, r fetch.Record) error {
	id, err := s.shared.record(context.WithoutCancel(ctx), r)
	if err != nil {
		if s.err == nil {
			s.err = err
		}
		return err
	}
	s.fetches = append(s.fetches, fetched{id: id, rec: r})
	return nil
}

// answer is one Get as the probe reads it.
type answer struct {
	resp   fetch.Response
	err    error
	id     int64 // the fetch row; 0 when nothing was recorded
	rec    fetch.Record
	scheme string // the scheme of the URL asked for
}

// noAnswer reports a Get that got no HTTP response at all.
func (a answer) noAnswer() bool { return a.resp.Status == 0 }

func (a answer) notPublic() bool { return errors.Is(a.err, fetch.ErrNotPublic) }

// retryHTTP reports whether an https Get is worth one more try at http://: it got no
// answer, for a reason the other scheme might not share.
func (a answer) retryHTTP(ctx context.Context) bool {
	return a.noAnswer() && !a.notPublic() && ctx.Err() == nil && !errors.Is(a.err, fetch.ErrBreakerOpen)
}

// siteProbe is one site's probe in progress.
type siteProbe struct {
	ctx context.Context
	c   *fetch.Client
	rec *siteRecorder
	cut bool // a fetch got no answer once the run was stopping: the site is not stored

	homepageID int64       // the homepage fetch row, once fetched
	robots     *HostRobots // what each host's robots.txt said
}

func (p *siteProbe) get(kind fetch.Kind, rawURL string) answer {
	before := len(p.rec.fetches)
	resp, err := p.c.Get(p.ctx, kind, rawURL)
	if err != nil && resp.Status == 0 && p.ctx.Err() != nil {
		p.cut = true
	}
	scheme, _, _ := strings.Cut(rawURL, "://")
	a := answer{resp: resp, err: err, scheme: scheme}
	if len(p.rec.fetches) > before {
		f := p.rec.fetches[len(p.rec.fetches)-1]
		a.id, a.rec = f.id, f.rec
	}
	return a
}

// getBoth gets path from host over https, and once more over http when https got no answer.
func (p *siteProbe) getBoth(kind fetch.Kind, host, path string) answer {
	a := p.get(kind, "https://"+host+path)
	if a.retryHTTP(p.ctx) {
		a = p.get(kind, "http://"+host+path)
	}
	return a
}

// signalID is the fetch a signal about a cites: a's own row, else the site's latest row.
func (p *siteProbe) signalID(a answer) int64 {
	if a.id != 0 {
		return a.id
	}
	if n := len(p.rec.fetches); n > 0 {
		return p.rec.fetches[n-1].id
	}
	return 0
}

// RobotsRead is what one host's robots.txt said for one scheme.
type RobotsRead struct {
	Verdict robots.Verdict
	Rules   *robots.Rules // nil allows every path
	a       answer
}

// HostRobots reads and keeps the robots.txt of each host and scheme one client asks about:
// over https with one http try when https got no answer (RA11), over http only for http.
// A read is kept under the scheme asked for and the scheme that answered, so each host and
// scheme is read at most once. It is not safe for concurrent use.
type HostRobots struct {
	get func(ctx context.Context, rawURL string) answer
	by  map[string]RobotsRead // scheme://host → what that host's robots.txt said
}

// NewHostRobots reads robots.txt through c, as fetch.KindRobots.
func NewHostRobots(c *fetch.Client) *HostRobots {
	return &HostRobots{get: func(ctx context.Context, rawURL string) answer {
		resp, err := c.Get(ctx, fetch.KindRobots, rawURL)
		scheme, _, _ := strings.Cut(rawURL, "://")
		return answer{resp: resp, err: err, scheme: scheme}
	}}
}

// NewHostRobotsFunc reads robots.txt through get, which fetches one robots.txt URL.
func NewHostRobotsFunc(get func(ctx context.Context, rawURL string) (fetch.Response, error)) *HostRobots {
	return &HostRobots{get: func(ctx context.Context, rawURL string) answer {
		resp, err := get(ctx, rawURL)
		scheme, _, _ := strings.Cut(rawURL, "://")
		return answer{resp: resp, err: err, scheme: scheme}
	}}
}

// At is what host's robots.txt says for scheme, read on the first ask.
func (h *HostRobots) At(ctx context.Context, scheme, host string) RobotsRead {
	if r, ok := h.by[scheme+"://"+host]; ok {
		return r
	}
	var a answer
	if scheme == "https" {
		a = h.get(ctx, "https://"+host+"/robots.txt")
		if a.retryHTTP(ctx) {
			a = h.get(ctx, "http://"+host+"/robots.txt")
		}
	} else {
		a = h.get(ctx, "http://"+host+"/robots.txt")
	}
	var decideErr error
	switch {
	case a.noAnswer():
		decideErr = a.err
		if decideErr == nil {
			decideErr = errors.New("probe: no answer")
		}
	case a.err != nil && !errors.Is(a.err, fetch.ErrStatus) && !errors.Is(a.err, fetch.ErrBodyTooLarge):
		decideErr = a.err // a refused redirect or a broken read
	}
	r := RobotsRead{Verdict: robots.Decide(a.resp.Status, a.resp.Body, decideErr), a: a}
	if r.Verdict == robots.VerdictRules {
		r.Rules = robots.Parse(a.resp.Body)
	}
	if h.by == nil {
		h.by = map[string]RobotsRead{}
	}
	h.by[scheme+"://"+host] = r
	if !a.noAnswer() {
		h.by[a.scheme+"://"+host] = r
	}
	return r
}

// For is At as a verdict and rules, the shape contacts.RobotsFor takes.
func (h *HostRobots) For(ctx context.Context, scheme, host string) (robots.Verdict, *robots.Rules) {
	r := h.At(ctx, scheme, host)
	return r.Verdict, r.Rules
}

// robotsAt reads host's robots.txt for scheme through the site's own HostRobots.
func (p *siteProbe) robotsAt(scheme, host string) RobotsRead {
	if p.robots == nil {
		p.robots = &HostRobots{get: func(_ context.Context, rawURL string) answer {
			return p.get(fetch.KindRobots, rawURL)
		}}
	}
	return p.robots.At(p.ctx, scheme, host)
}

// verdictFor is an outcome with its signals. failed builds the transport_error one with
// the run's one counting rule: a fetch with no answer counts toward the network-down
// rule, a guard refusal (not_public) never does.
type verdictFor struct {
	outcome   string
	signals   []leadstore.Signal
	counted   bool
	notPublic bool
}

func (p *siteProbe) failed(a answer, sigName string) verdictFor {
	v := verdictFor{outcome: leadstore.OutcomeTransportError}
	id := p.signalID(a)
	switch {
	case a.notPublic():
		v.notPublic = true
		v.signals = append(v.signals, leadstore.Signal{Signal: SigNotPublic, FetchID: id})
	case a.noAnswer():
		v.counted = true
		v.signals = append(v.signals, leadstore.Signal{Signal: sigName, Value: "unreachable", FetchID: id})
	default:
		v.signals = append(v.signals, leadstore.Signal{Signal: sigName, Value: "status " + strconv.Itoa(a.resp.Status), FetchID: id})
	}
	return v
}

// probeSite probes one site. Its client draws on its own budgets, so a forever-redirecting
// or failing site ends on its own: at most 2 homepage Gets (https, then http), each a
// redirect chain of at most 4 requests, and at most 4 robots.txt Gets.
func probeSite(ctx context.Context, d Deps, shared *sharedRecorder, site leadstore.Site) siteResult {
	rec := &siteRecorder{shared: shared}
	c := d.NewClient(rec)
	counter := &countingTransport{inner: c.HTTP.Transport}
	if counter.inner == nil {
		counter.inner = http.DefaultTransport // what a nil Transport means
	}
	c.HTTP.Transport = counter
	p := &siteProbe{ctx: ctx, c: c, rec: rec}
	row, v := p.run(strings.ToLower(site.Domain))

	res := siteResult{
		counted:   v.counted,
		notPublic: v.notPublic,
		requests:  int(counter.n.Load()),
		failures:  c.Breaker.Failures(),
		err:       rec.err,
	}
	if p.cut {
		res.interrupted = true
		return res
	}
	row.Outcome = v.outcome
	row.Signals = append(row.Signals, v.signals...)
	row.DerivedBy = drupal.DerivedBy
	w := Write{SiteID: site.ID}
	for _, f := range rec.fetches {
		row.FetchIDs = append(row.FetchIDs, f.id)
		if f.rec.Header != nil {
			w.Headers = append(w.Headers, FetchHeader{FetchID: f.id, Header: f.rec.Header})
		}
	}
	if row.IsDrupal {
		for _, f := range rec.fetches {
			keep := f.rec.Kind == fetch.KindRobots || f.id == p.homepageID
			if keep && f.rec.Status != 0 && len(f.rec.Body) > 0 {
				w.Bodies = append(w.Bodies, FetchBody{FetchID: f.id, Body: f.rec.Body})
			}
		}
	}
	w.Probe = row
	res.write = w
	return res
}

// run is the probe's steps for host; the row it returns lacks Outcome, the verdict's
// signals and FetchIDs, which probeSite fills.
func (p *siteProbe) run(host string) (leadstore.ProbeRow, verdictFor) {
	var row leadstore.ProbeRow
	rr := p.robotsAt("https", host)
	if rr.Verdict == robots.VerdictSkip {
		return row, p.failed(rr.a, SigRobots)
	}
	if !rr.Rules.Allowed(p.c.UserAgent, "/") {
		return row, disallowed(p.signalID(rr.a))
	}

	home := p.getBoth(fetch.KindSite, host, "/")
	p.homepageID = home.id
	if home.noAnswer() || home.notPublic() {
		return row, p.failed(home, SigHomepage)
	}
	final, err := url.Parse(home.resp.FinalURL)
	if err != nil || final.Hostname() == "" {
		final = &url.URL{Scheme: home.scheme, Host: host, Path: "/"}
	}
	finalHost := strings.ToLower(final.Hostname())
	row.FinalHost = finalHost
	if finalHost != host {
		row.Signals = append(row.Signals, leadstore.Signal{Signal: SigRedirect, Value: finalHost, FetchID: home.id})
	}
	// The page is used only when the robots.txt of its own scheme and host allows its path.
	fr := p.robotsAt(final.Scheme, finalHost)
	if fr.Verdict == robots.VerdictSkip {
		return row, p.failed(fr.a, SigRobots)
	}
	if !fr.Rules.Allowed(p.c.UserAgent, final.RequestURI()) {
		return row, disallowed(p.signalID(fr.a))
	}

	f := drupal.Detect(home.resp.Status, home.resp.Header, home.resp.Body, home.resp.FinalURL)
	switch {
	case f.BotCheck:
		row.Signals = append(row.Signals, signalsOf(f, home.id)...)
		return row, verdictFor{outcome: leadstore.OutcomeBotCheck}
	case home.err != nil && home.resp.Status/100 == 3:
		return row, verdictFor{outcome: leadstore.OutcomeHTTPError, signals: []leadstore.Signal{
			{Signal: SigHomepage, Value: "redirects", FetchID: home.id},
		}}
	case home.resp.Status/100 != 2:
		return row, verdictFor{outcome: leadstore.OutcomeHTTPError, signals: []leadstore.Signal{
			{Signal: SigHomepage, Value: "status " + strconv.Itoa(home.resp.Status), FetchID: home.id},
		}}
	case !isHTML(home.resp.ContentType, home.resp.Body):
		ct := home.resp.ContentType
		if ct == "" {
			ct = "none"
		}
		return row, verdictFor{outcome: leadstore.OutcomeNotHTML, signals: []leadstore.Signal{
			{Signal: SigHomepage, Value: "content-type " + ct, FetchID: home.id},
		}}
	}
	row.IsDrupal = f.IsDrupal
	row.Major = f.Major
	row.ExactVersion = f.Exact
	row.Ambiguous = f.Ambiguous
	row.SiteName = f.SiteName
	row.Signals = append(row.Signals, signalsOf(f, home.id)...)
	return row, verdictFor{outcome: leadstore.OutcomeOK}
}

func disallowed(id int64) verdictFor {
	return verdictFor{outcome: leadstore.OutcomeRobotsDisallowed, signals: []leadstore.Signal{
		{Signal: SigRobots, Value: "disallowed", FetchID: id},
	}}
}

func signalsOf(f drupal.Finding, fetchID int64) []leadstore.Signal {
	out := make([]leadstore.Signal, 0, len(f.Signals))
	for _, s := range f.Signals {
		out = append(out, leadstore.Signal{Signal: s.Name, Value: s.Value, FetchID: fetchID})
	}
	return out
}

// countingTransport counts the HTTP requests a site's client sends, every request of a
// redirect chain included.
type countingTransport struct {
	inner http.RoundTripper
	n     atomic.Int64
}

func (t *countingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.n.Add(1)
	return t.inner.RoundTrip(req)
}

// isHTML reports whether a homepage is HTML: by its Content-Type, or, when it sent none,
// by sniffing the body.
func isHTML(contentType string, body []byte) bool {
	if strings.TrimSpace(contentType) == "" {
		contentType = http.DetectContentType(body)
	}
	mt, _, err := mime.ParseMediaType(contentType)
	if err != nil && !errors.Is(err, mime.ErrInvalidMediaParameter) {
		return false
	}
	return mt == "text/html" || mt == "application/xhtml+xml"
}

// TxBeginner opens a transaction: a *pgxpool.Pool or a *pgx.Conn.
type TxBeginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// SaveTx returns a Deps.Save that writes each site's probe in one transaction on db: the
// headers of every fetch, the bodies of a Drupal hit, and the probe row for runID.
func SaveTx(db TxBeginner, runID int64) func(ctx context.Context, w Write) error {
	return func(ctx context.Context, w Write) error {
		tx, err := db.Begin(ctx)
		if err != nil {
			return fmt.Errorf("probe: begin: %w", err)
		}
		defer func() { _ = tx.Rollback(ctx) }() // a no-op after Commit
		lq, sq := leadstore.NewQueries(tx), store.NewQueries(tx)
		for _, h := range w.Headers {
			if err := lq.InsertFetchHeader(ctx, h.FetchID, h.Header); err != nil {
				return err
			}
		}
		for _, b := range w.Bodies {
			if err := sq.AttachFetchBody(ctx, b.FetchID, b.Body); err != nil {
				return err
			}
		}
		if _, err := lq.RecordProbe(ctx, w.SiteID, runID, w.Probe); err != nil {
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("probe: commit: %w", err)
		}
		return nil
	}
}
