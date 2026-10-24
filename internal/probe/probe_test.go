package probe

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/time/rate"

	"github.com/gremy/webbership-leadhound/internal/config"
	"github.com/gremy/webbership-leadhound/internal/fetch"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
)

const sitesFixtures = "../drupal/testdata/sites"

// fakeRecorder hands out ids from 1 and keeps every record.
type fakeRecorder struct {
	mu    sync.Mutex
	recs  []fetch.Record
	byURL map[string]int64
}

// Record fails on a done ctx, as the store's insert does.
func (f *fakeRecorder) Record(ctx context.Context, r fetch.Record) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("fake recorder: %w", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recs = append(f.recs, r)
	if f.byURL == nil {
		f.byURL = map[string]int64{}
	}
	f.byURL[r.URL] = int64(len(f.recs))
	return nil
}

func (f *fakeRecorder) LastIDFor(u string) (int64, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id, ok := f.byURL[u]
	return id, ok
}

// kindCount counts the records of kind whose URL holds host.
func (f *fakeRecorder) kindCount(kind fetch.Kind, host string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, r := range f.recs {
		if r.Kind == kind && strings.Contains(r.URL, "://"+host+"/") {
			n++
		}
	}
	return n
}

// reply is one scripted answer.
type reply struct {
	status   int
	ctype    string
	header   http.Header
	body     string
	location string
	err      error
}

// scripted answers each URL from its map; an unknown URL is a 404.
type scripted struct {
	mu      sync.Mutex
	replies map[string]reply
	all     func(u string) (reply, bool) // consulted when the URL has no entry
	hits    map[string]int

	onRequest func(req *http.Request) error // called first; an error is the answer
}

func (s *scripted) total() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, c := range s.hits {
		n += c
	}
	return n
}

func (s *scripted) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := req.Context().Err(); err != nil {
		return nil, err // as a real transport does
	}
	u := req.URL.String()
	if s.onRequest != nil {
		if err := s.onRequest(req); err != nil {
			return nil, err
		}
	}
	s.mu.Lock()
	if s.hits == nil {
		s.hits = map[string]int{}
	}
	s.hits[u]++
	r, ok := s.replies[u]
	s.mu.Unlock()
	if !ok && s.all != nil {
		r, ok = s.all(u)
	}
	if !ok {
		r = reply{status: http.StatusNotFound}
	}
	if r.err != nil {
		return nil, r.err
	}
	h := http.Header{}
	for k, vs := range r.header {
		h[k] = vs
	}
	if r.ctype != "" {
		h.Set("Content-Type", r.ctype)
	}
	if r.location != "" {
		h.Set("Location", r.location)
	}
	return &http.Response{
		StatusCode: r.status, Status: strconv.Itoa(r.status), Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
		Header: h, Body: io.NopCloser(strings.NewReader(r.body)), ContentLength: int64(len(r.body)), Request: req,
	}, nil
}

func (s *scripted) hitsFor(substr string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for u, c := range s.hits {
		if strings.Contains(u, substr) {
			n += c
		}
	}
	return n
}

// clientOptions are the site client's settings, as siteClients builds them.
func clientOptions() []fetch.Option {
	return []fetch.Option{
		fetch.WithPublicOnly(),
		fetch.WithProfile(fetch.Site),
		fetch.WithTimeout(10 * time.Second),
		fetch.WithMaxRedirects(4),
		fetch.WithJitter(0),
		fetch.WithLimiter(rate.NewLimiter(rate.Inf, 1)),
	}
}

func factory(extra ...fetch.Option) func(fetch.Recorder) *fetch.Client {
	return func(rec fetch.Recorder) *fetch.Client {
		opts := append(clientOptions(), extra...)
		opts = append(opts, fetch.WithRecorder(rec), fetch.WithBreaker(fetch.NewBreaker(fetch.Budgets{
			fetch.KindRobots: 4, fetch.KindSite: 4,
		})))
		return fetch.New(opts...)
	}
}

func scriptedFactory(rt http.RoundTripper) func(fetch.Recorder) *fetch.Client {
	return factory(fetch.WithTransport(rt), fetch.WithLiveGate(config.LiveAll))
}

func fixtureFactory(t *testing.T) func(fetch.Recorder) *fetch.Client {
	t.Helper()
	ft, err := fetch.NewFixtureTransport(sitesFixtures)
	if err != nil {
		t.Fatal(err)
	}
	return factory(fetch.WithFixtures(ft))
}

// sites lists domains as site rows with ids from 1.
func sites(domains ...string) []leadstore.Site {
	out := make([]leadstore.Site, len(domains))
	for i, d := range domains {
		out[i] = leadstore.Site{ID: int64(i + 1), Domain: d, State: leadstore.SiteNew}
	}
	return out
}

// run probes the sites and returns the summary, the recorder and each site's write by domain.
func run(t *testing.T, newClient func(fetch.Recorder) *fetch.Client, list []leadstore.Site, conc int) (Summary, *fakeRecorder, map[string]Write) {
	t.Helper()
	rec := &fakeRecorder{}
	byID := map[int64]string{}
	for _, s := range list {
		byID[s.ID] = s.Domain
	}
	writes := map[string]Write{}
	save := func(_ context.Context, w Write) error { writes[byID[w.SiteID]] = w; return nil }
	sum, err := Run(context.Background(), Deps{NewClient: newClient, Recorder: rec, Save: save}, list, Options{Concurrency: conc})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for d, w := range writes {
		checkWrite(t, d, w, rec)
	}
	return sum, rec, writes
}

// checkWrite holds the invariants of every write: each signal cites a fetch of its probe,
// every recorded fetch is listed, and bodies are kept only for Drupal hits and only for
// fetches the probe cites.
func checkWrite(t *testing.T, domain string, w Write, rec *fakeRecorder) {
	t.Helper()
	for _, s := range w.Probe.Signals {
		if !slices.Contains(w.Probe.FetchIDs, s.FetchID) {
			t.Errorf("%s: signal %s cites fetch %d, not in fetch_ids %v", domain, s.Signal, s.FetchID, w.Probe.FetchIDs)
		}
	}
	if len(w.Bodies) > 0 && !w.Probe.IsDrupal {
		t.Errorf("%s: %d bodies kept for a probe that is not Drupal", domain, len(w.Bodies))
	}
	for _, b := range w.Bodies {
		if !slices.Contains(w.Probe.FetchIDs, b.FetchID) {
			t.Errorf("%s: body of fetch %d not cited", domain, b.FetchID)
		}
		if got := rec.recs[b.FetchID-1].Body; !bytes.Equal(got, b.Body) {
			t.Errorf("%s: body of fetch %d is not the bytes that arrived", domain, b.FetchID)
		}
	}
	if w.Probe.DerivedBy != "drupal@1" {
		t.Errorf("%s: derived_by %q", domain, w.Probe.DerivedBy)
	}
}

func hasSignal(w Write, name, value string) bool {
	for _, s := range w.Probe.Signals {
		if s.Signal == name && s.Value == value {
			return true
		}
	}
	return false
}

func TestRunManifestFixtures(t *testing.T) {
	list := sites("northwind.example", "contoso.example", "fabrikam.example", "tailspin.example", "wingtip.example", "adatum.example")
	sum, rec, w := run(t, fixtureFactory(t), list, 3)

	tests := []struct {
		domain  string
		outcome string
		drupal  bool
		major   int
		exact   string
	}{
		{"northwind.example", leadstore.OutcomeOK, true, 7, ""},
		{"contoso.example", leadstore.OutcomeOK, true, 10, "10.6.14"}, // robots.txt 404: every path allowed
		{"fabrikam.example", leadstore.OutcomeOK, true, 11, ""},
		{"tailspin.example", leadstore.OutcomeOK, false, 0, ""},
		{"wingtip.example", leadstore.OutcomeBotCheck, false, 0, ""},
		{"adatum.example", leadstore.OutcomeRobotsDisallowed, false, 0, ""},
	}
	for _, tt := range tests {
		p := w[tt.domain].Probe
		if p.Outcome != tt.outcome || p.IsDrupal != tt.drupal || p.Major != tt.major || p.ExactVersion != tt.exact {
			t.Errorf("%s: got outcome %s drupal %v major %d exact %q, want %s %v %d %q",
				tt.domain, p.Outcome, p.IsDrupal, p.Major, p.ExactVersion, tt.outcome, tt.drupal, tt.major, tt.exact)
		}
	}
	if n := rec.kindCount(fetch.KindSite, "adatum.example"); n != 0 {
		t.Errorf("robots-disallowed site: %d site fetches, want 0", n)
	}
	if got := w["northwind.example"].Probe.SiteName; got != "Northwind Trading" {
		t.Errorf("site name %q", got)
	}
	if got := w["northwind.example"].Probe.FinalHost; got != "northwind.example" {
		t.Errorf("final host %q", got)
	}
	// A Drupal hit keeps its homepage and robots bodies; contoso's robots 404 had no body.
	if n := len(w["northwind.example"].Bodies); n != 2 {
		t.Errorf("northwind: %d bodies kept, want 2 (robots + homepage)", n)
	}
	if n := len(w["contoso.example"].Bodies); n != 1 {
		t.Errorf("contoso: %d bodies kept, want 1 (homepage)", n)
	}
	if !hasSignal(w["wingtip.example"], "challenge-platform", "") {
		t.Errorf("bot check signals %v", w["wingtip.example"].Probe.Signals)
	}
	if sum.Sites != 6 || sum.Drupal != 3 || sum.ByMajor[7] != 1 || sum.ByMajor[10] != 1 || sum.ByMajor[11] != 1 ||
		sum.Exact["10.6.14"] != 1 || sum.Outcomes[leadstore.OutcomeBotCheck] != 1 || sum.Outcomes[leadstore.OutcomeRobotsDisallowed] != 1 {
		t.Errorf("summary %+v", sum)
	}
	if sum.StopReason != "" || sum.Cancelled {
		t.Errorf("stop %q cancelled %v", sum.StopReason, sum.Cancelled)
	}
}

const d10Page = `<!DOCTYPE html><html><head><meta name="Generator" content="Drupal 10 (https://www.drupal.org)" /></head>` +
	`<body><script type="application/json" data-drupal-selector="drupal-settings-json">{}</script></body></html>`

const html = "text/html; charset=utf-8"

func TestRunScripted(t *testing.T) {
	notPublic := fmt.Errorf("dial tcp: %w", fetch.ErrNotPublic)
	refused := errors.New("dial tcp: connection refused")
	rt := &scripted{replies: map[string]reply{
		// robots.txt answering 500: the site is skipped.
		"https://fivehundred.example/robots.txt": {status: 500},
		// D11 from the header alone.
		"https://litware.example/": {status: 200, ctype: html, header: http.Header{"X-Generator": {"Drupal 11 (https://www.drupal.org)"}}, body: "<html><body>Litware</body></html>"},
		// A redirect to another registrable domain: counted by the final host, its robots read.
		"https://relecloud.example/":            {status: 301, location: "https://www.margie.example/home"},
		"https://www.margie.example/robots.txt": {status: 200, body: "User-agent: *\nDisallow: /admin/\n"},
		"https://www.margie.example/home":       {status: 200, ctype: html, body: d10Page},
		// The same, where the final host's robots.txt disallows the page.
		"https://fourth.example/":                  {status: 302, location: "https://closed-target.example/"},
		"https://closed-target.example/robots.txt": {status: 200, body: "User-agent: *\nDisallow: /\n"},
		"https://closed-target.example/":           {status: 200, ctype: html, body: d10Page},
		// Not HTML.
		"https://pdfonly.example/": {status: 200, ctype: "application/pdf", body: "%PDF-1.7"},
		// https unreachable, http answers: both robots and homepage fall back.
		"https://plainhttp.example/robots.txt": {err: refused},
		"https://plainhttp.example/":           {err: refused},
		"http://plainhttp.example/":            {status: 200, ctype: html, body: d10Page},
		// The guard refuses the dial.
		"https://private.example/robots.txt": {err: notPublic},
		// A homepage 404.
		"https://gone.example/robots.txt": {status: 410},
		// A same-host redirect into a path robots.txt disallows.
		"https://samehost.example/robots.txt": {status: 200, body: "User-agent: *\nDisallow: /en/\n"},
		"https://samehost.example/":           {status: 301, location: "https://samehost.example/en/"},
		"https://samehost.example/en/":        {status: 200, ctype: html, body: d10Page},
		// https robots.txt allows, the homepage answers only over http, whose robots.txt disallows.
		"https://schemesplit.example/robots.txt": {status: 200, body: "User-agent: *\nDisallow: /admin/\n"},
		"https://schemesplit.example/":           {err: refused},
		"http://schemesplit.example/robots.txt":  {status: 200, body: "User-agent: *\nDisallow: /\n"},
		"http://schemesplit.example/":            {status: 200, ctype: html, body: d10Page},
		// A Cloudflare challenge on the homepage, robots.txt fine.
		"https://challenged.example/": {status: 403, ctype: html, header: http.Header{"Cf-Mitigated": {"challenge"}}, body: "<html></html>"},
	}}
	rt.all = func(u string) (reply, bool) {
		// Every page of loop.example redirects to the next one, forever.
		if strings.HasPrefix(u, "https://loop.example/") && !strings.HasSuffix(u, "/robots.txt") {
			return reply{status: 302, location: u + "x"}, true
		}
		if u == "https://gone.example/" {
			return reply{status: 404, ctype: html, body: "<html>Not found</html>"}, true
		}
		return reply{}, false
	}
	list := sites("fivehundred.example", "litware.example", "relecloud.example", "fourth.example", "pdfonly.example",
		"plainhttp.example", "private.example", "loop.example", "gone.example", "challenged.example",
		"samehost.example", "schemesplit.example")
	sum, rec, w := run(t, scriptedFactory(rt), list, 4)

	check := func(domain, outcome string) Write {
		t.Helper()
		got := w[domain]
		if got.Probe.Outcome != outcome {
			t.Errorf("%s: outcome %s, want %s (signals %v)", domain, got.Probe.Outcome, outcome, got.Probe.Signals)
		}
		return got
	}
	if p := check("fivehundred.example", leadstore.OutcomeTransportError); !hasSignal(p, SigRobots, "status 500") {
		t.Errorf("500 robots signals %v", p.Probe.Signals)
	}
	if n := rec.kindCount(fetch.KindSite, "fivehundred.example"); n != 0 {
		t.Errorf("500 robots: %d site fetches, want 0", n)
	}
	if p := check("litware.example", leadstore.OutcomeOK); !p.Probe.IsDrupal || p.Probe.Major != 11 {
		t.Errorf("D11 header: %+v", p.Probe)
	}
	p := check("relecloud.example", leadstore.OutcomeOK)
	if p.Probe.FinalHost != "www.margie.example" || !p.Probe.IsDrupal || p.Probe.Major != 10 || !hasSignal(p, SigRedirect, "www.margie.example") {
		t.Errorf("redirect: %+v", p.Probe)
	}
	if rt.hitsFor("https://www.margie.example/robots.txt") != 1 {
		t.Errorf("final host's robots.txt read %d times, want 1", rt.hitsFor("https://www.margie.example/robots.txt"))
	}
	if p := check("fourth.example", leadstore.OutcomeRobotsDisallowed); p.Probe.FinalHost != "closed-target.example" || p.Probe.IsDrupal || len(p.Bodies) != 0 {
		t.Errorf("redirect to a disallowed host: %+v", p.Probe)
	}
	check("pdfonly.example", leadstore.OutcomeNotHTML)
	if p := check("plainhttp.example", leadstore.OutcomeOK); !p.Probe.IsDrupal {
		t.Errorf("http fallback: %+v", p.Probe)
	}
	if rt.hitsFor("http://plainhttp.example/robots.txt") != 1 {
		t.Errorf("robots.txt over http not tried")
	}
	if p := check("private.example", leadstore.OutcomeTransportError); !hasSignal(p, SigNotPublic, "") {
		t.Errorf("not public signals %v", p.Probe.Signals)
	}
	if rt.hitsFor("http://private.example/") != 0 {
		t.Errorf("a guard refusal was retried over http")
	}
	check("loop.example", leadstore.OutcomeHTTPError)
	if n := rt.hitsFor("loop.example/") - rt.hitsFor("loop.example/robots.txt"); n > 4 {
		t.Errorf("forever-redirecting site: %d site requests, want at most 4", n)
	}
	if p := check("gone.example", leadstore.OutcomeHTTPError); !hasSignal(p, SigHomepage, "status 404") {
		t.Errorf("404 homepage signals %v", p.Probe.Signals)
	}
	if p := check("challenged.example", leadstore.OutcomeBotCheck); !hasSignal(p, "cf-mitigated", "challenge") {
		t.Errorf("challenge signals %v", p.Probe.Signals)
	}
	if rt.hitsFor("challenged.example/") != 2 {
		t.Errorf("bot check retried: %d requests, want 2 (robots + homepage)", rt.hitsFor("challenged.example/"))
	}
	if p := check("samehost.example", leadstore.OutcomeRobotsDisallowed); p.Probe.IsDrupal || len(p.Bodies) != 0 || p.Probe.FinalHost != "samehost.example" {
		t.Errorf("same-host redirect into a disallowed path: %+v", p.Probe)
	}
	if rt.hitsFor("samehost.example/robots.txt") != 1 {
		t.Errorf("same host's robots.txt read %d times, want 1", rt.hitsFor("samehost.example/robots.txt"))
	}
	check("schemesplit.example", leadstore.OutcomeRobotsDisallowed)
	if rt.hitsFor("http://schemesplit.example/robots.txt") != 1 {
		t.Errorf("http robots.txt not read for an http homepage")
	}
	if rt.hitsFor("http://plainhttp.example/robots.txt") != 1 {
		t.Errorf("plainhttp: http robots.txt read %d times, want 1 (reused)", rt.hitsFor("http://plainhttp.example/robots.txt"))
	}
	if sum.Requests != rt.total() {
		t.Errorf("summary counts %d requests, the transport saw %d", sum.Requests, rt.total())
	}
	if sum.NotPublic != 1 || sum.Sites != len(list) {
		t.Errorf("summary %+v", sum)
	}
	// Headers are kept for every fetch that had allowlisted ones.
	if len(w["litware.example"].Headers) == 0 {
		t.Errorf("no headers kept for litware")
	}
}

func manySites(n int, suffix string) []leadstore.Site {
	ds := make([]string, n)
	for i := range ds {
		ds[i] = fmt.Sprintf("site%03d.%s", i, suffix)
	}
	return sites(ds...)
}

func TestRunNetworkDown(t *testing.T) {
	for _, conc := range []int{1, 8} {
		t.Run(strconv.Itoa(conc), func(t *testing.T) {
			rt := &scripted{all: func(string) (reply, bool) {
				return reply{err: errors.New("dial tcp: network is unreachable")}, true
			}}
			sum, _, w := run(t, scriptedFactory(rt), manySites(120, "example"), conc)
			if sum.StopReason != StopFailureRate {
				t.Fatalf("stop %q, want failure_rate", sum.StopReason)
			}
			if sum.Sites < 100 || sum.Sites >= 120 || len(w) != sum.Sites {
				t.Errorf("stored %d sites (%d writes), want 100 to 119", sum.Sites, len(w))
			}
			if conc == 1 && sum.Sites != 100 {
				t.Errorf("one worker: stored %d, want exactly 100", sum.Sites)
			}
		})
	}
}

func TestRunNotPublicNotCounted(t *testing.T) {
	rt := &scripted{all: func(string) (reply, bool) {
		return reply{err: fmt.Errorf("dial tcp: %w", fetch.ErrNotPublic)}, true
	}}
	sum, _, _ := run(t, scriptedFactory(rt), manySites(120, "invalid"), 4)
	if sum.StopReason != "" || sum.Sites != 120 || sum.NotPublic != 120 || sum.Outcomes[leadstore.OutcomeTransportError] != 120 {
		t.Errorf("summary %+v", sum)
	}
}

func TestRunCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rt := &scripted{}
	saved := 0
	sum, err := Run(ctx, Deps{NewClient: scriptedFactory(rt), Recorder: &fakeRecorder{},
		Save: func(context.Context, Write) error { saved++; return nil }}, manySites(5, "example"), Options{Concurrency: 2})
	if err != nil {
		t.Fatal(err)
	}
	if !sum.Cancelled || saved != 0 {
		t.Errorf("cancelled %v, saved %d", sum.Cancelled, saved)
	}
}

// A cancel mid-run ends the run cleanly: no error, the finished sites stored, the one cut
// short not stored.
func TestRunCancelMidRun(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rt := &scripted{}
	rt.onRequest = func(req *http.Request) error {
		if req.URL.String() == "https://site003.example/" {
			cancel()
			return context.Canceled
		}
		return nil
	}
	rec := &fakeRecorder{}
	var saved []int64
	sum, err := Run(ctx, Deps{NewClient: scriptedFactory(rt), Recorder: rec,
		Save: func(_ context.Context, w Write) error { saved = append(saved, w.SiteID); return nil }},
		manySites(10, "example"), Options{Concurrency: 1})
	if err != nil {
		t.Fatalf("Run: %v, want a clean end", err)
	}
	if !sum.Cancelled || sum.Sites != 3 || fmt.Sprint(saved) != "[1 2 3]" || sum.Interrupted < 1 {
		t.Errorf("cancelled %v, sites %d, saved %v, interrupted %d; want true, 3, [1 2 3], at least 1",
			sum.Cancelled, sum.Sites, saved, sum.Interrupted)
	}
}

func TestRunSaveErrorStops(t *testing.T) {
	ft, err := fetch.NewFixtureTransport(sitesFixtures)
	if err != nil {
		t.Fatal(err)
	}
	boom := errors.New("boom")
	_, err = Run(context.Background(), Deps{NewClient: factory(fetch.WithFixtures(ft)), Recorder: &fakeRecorder{},
		Save: func(context.Context, Write) error { return boom }}, sites("northwind.example", "contoso.example"), Options{})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want boom", err)
	}
}

func TestIsHTML(t *testing.T) {
	tests := []struct {
		ct   string
		body string
		want bool
	}{
		{"text/html; charset=utf-8", "", true},
		{"application/xhtml+xml", "", true},
		{"application/pdf", "<html>", false},
		{"", "<!DOCTYPE html><html></html>", true},
		{"", "%PDF-1.7", false},
		{"text/plain", "<html>", false},
		{"text/html; charset", "", true}, // a broken parameter still names the type
	}
	for _, tt := range tests {
		if got := isHTML(tt.ct, []byte(tt.body)); got != tt.want {
			t.Errorf("isHTML(%q, %q) = %v, want %v", tt.ct, tt.body, got, tt.want)
		}
	}
}
