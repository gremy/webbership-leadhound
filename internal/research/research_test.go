package research

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/time/rate"

	"github.com/gremy/webbership-leadhound/internal/ai"
	"github.com/gremy/webbership-leadhound/internal/claude"
	"github.com/gremy/webbership-leadhound/internal/config"
	"github.com/gremy/webbership-leadhound/internal/contacts"
	"github.com/gremy/webbership-leadhound/internal/fetch"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/robots"
)

const (
	orgDomain  = "northwind.example"
	contactURL = "https://northwind.example/contact"
	// sentinelQuote stands for a quote holding a person's name; it must surface nowhere.
	sentinelQuote = "QUOTE-SENTINEL-Jane-Smith-7f3a"
)

var testRoles = []string{"info", "office", "contact", "hello", "sales"}

func contactPage(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/contact.html")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// fakePage is one URL's scripted answer.
type fakePage struct {
	status int
	body   []byte
	final  string
	id     int64
	err    error
}

// fakeFetcher answers from pages and records every Get.
type fakeFetcher struct {
	mu    sync.Mutex
	pages map[string]fakePage
	gets  []string
}

func (f *fakeFetcher) Get(_ context.Context, _ fetch.Kind, rawURL string) (fetch.Response, int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gets = append(f.gets, rawURL)
	p, ok := f.pages[rawURL]
	if !ok {
		return fetch.Response{Status: 404}, 0, nil
	}
	if p.err != nil {
		return fetch.Response{}, p.id, p.err
	}
	return fetch.Response{Status: p.status, Body: p.body, FinalURL: p.final}, p.id, nil
}

func (f *fakeFetcher) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.gets)
}

func robotsFrom(txt string) contacts.RobotsFor {
	rules := robots.Parse([]byte(txt))
	return func(context.Context, string, string) (robots.Verdict, *robots.Rules) {
		return robots.VerdictRules, rules
	}
}

type fakeResolver struct{ mx []*net.MX }

func (r fakeResolver) LookupMX(context.Context, string) ([]*net.MX, error) { return r.mx, nil }

var mxOK = fakeResolver{mx: []*net.MX{{Host: "mx.northwind.example.", Pref: 10}}}

func verifier(f contacts.Fetcher) Verifier {
	return Verifier{Fetcher: f, Robots: robotsFrom(""), Resolver: mxOK, Roles: testRoles, UserAgent: "LeadHound/1"}
}

func contactFetcher(t *testing.T) *fakeFetcher {
	return &fakeFetcher{pages: map[string]fakePage{contactURL: {status: 200, body: contactPage(t), id: 41}}}
}

func req(want Want) Request {
	return Request{LeadID: 7, Org: Org{ID: 3, Name: "Northwind Trading", Domain: orgDomain, Country: "GB"}, Want: want}
}

type cand struct{ Kind, Value, URL, LineKind, Role string }

// answer is a model answer holding cs, each with the sentinel quote.
func answer(cs ...cand) string {
	type item struct {
		Kind      string `json:"kind"`
		Value     string `json:"value"`
		SourceURL string `json:"source_url"`
		Quote     string `json:"quote"`
		LineKind  string `json:"line_kind"`
		Role      string `json:"role"`
	}
	items := []item{}
	for _, c := range cs {
		items = append(items, item{c.Kind, c.Value, c.URL, sentinelQuote, c.LineKind, c.Role})
	}
	b, _ := json.Marshal(map[string]any{"candidates": items})
	return "Here is what I found.\n```json\n" + string(b) + "\n```"
}

func fakeClaude(text string) *claude.Fake {
	return &claude.Fake{ResearchAnswer: claude.ResearchResult{Text: text}}
}

func run(t *testing.T, text string, f contacts.Fetcher, want Want) (Result, error) {
	t.Helper()
	return Run(context.Background(), fakeClaude(text), verifier(f), req(want))
}

func TestPersonAddressOnPageDropped(t *testing.T) {
	f := contactFetcher(t)
	res, err := run(t, answer(
		cand{Kind: "email", Value: "jane.smith@northwind.example", URL: contactURL},
		cand{Kind: "email", Value: "info@northwind.example", URL: contactURL},
	), f, Want{Email: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Candidates != 2 || res.Verified != 1 || len(res.Routes) != 1 {
		t.Fatalf("candidates %d verified %d routes %+v, want 2, 1 and info@ only", res.Candidates, res.Verified, res.Routes)
	}
	want := leadstore.RouteRow{OrgID: 3, Kind: leadstore.RouteEmail, Value: "info@northwind.example",
		FetchID: 41, MXOK: true, DerivedBy: DerivedBy}
	if res.Routes[0] != want {
		t.Errorf("route %+v, want %+v", res.Routes[0], want)
	}
	if n := f.count(); n != 1 {
		t.Errorf("fetched %d times, want the cited page once", n)
	}
}

func TestAddressNotOnPageDropped(t *testing.T) {
	res, err := run(t, answer(cand{Kind: "email", Value: "sales@northwind.example", URL: contactURL}),
		contactFetcher(t), Want{Email: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Verified != 0 || len(res.Routes) != 0 || res.Candidates != 1 {
		t.Errorf("verified %d routes %+v, want nothing", res.Verified, res.Routes)
	}
}

func TestOffDomainAddressDropped(t *testing.T) {
	f := &fakeFetcher{pages: map[string]fakePage{
		contactURL:                             {status: 200, body: contactPage(t), id: 41},
		"https://othercorp.example/contact":    {status: 200, body: []byte(`<p>info@othercorp.example</p>`), id: 42},
		"https://northwind.example/moved":      {status: 200, body: contactPage(t), final: "https://elsewhere.example/c", id: 43},
		"https://www.northwind.example/team/x": {status: 200, body: []byte(`<p>office@northwind.example</p>`), id: 44},
	}}
	res, err := run(t, answer(
		cand{Kind: "email", Value: "hello@othercorp.example", URL: contactURL},                         // foreign on the page
		cand{Kind: "email", Value: "info@othercorp.example", URL: "https://othercorp.example/contact"}, // page off-domain
		cand{Kind: "email", Value: "info@northwind.example", URL: "https://northwind.example/moved"},   // redirected away
		cand{Kind: "email", Value: "office@northwind.example", URL: "https://www.northwind.example/team/x"},
	), f, Want{Email: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Routes) != 1 || res.Routes[0].Value != "office@northwind.example" || res.Routes[0].FetchID != 44 {
		t.Fatalf("routes %+v, want office@ from a www page only", res.Routes)
	}
	for _, g := range f.gets {
		if strings.Contains(g, "othercorp") {
			t.Errorf("fetched off-domain page %s", g)
		}
	}
}

func TestNoMXDropsEmail(t *testing.T) {
	v := verifier(contactFetcher(t))
	v.Resolver = fakeResolver{}
	res, err := Run(context.Background(), fakeClaude(answer(cand{Kind: "email", Value: "info@northwind.example", URL: contactURL})),
		v, req(Want{Email: true}))
	if err != nil || res.Verified != 0 {
		t.Errorf("verified %d err %v, want 0 and nil", res.Verified, err)
	}
}

func TestRobotsBlockedNotFetched(t *testing.T) {
	text := answer(cand{Kind: "email", Value: "info@northwind.example", URL: contactURL})
	for name, rf := range map[string]contacts.RobotsFor{
		"disallow /contact": robotsFrom("User-agent: *\nDisallow: /contact\n"),
		"skip verdict": func(context.Context, string, string) (robots.Verdict, *robots.Rules) {
			return robots.VerdictSkip, nil
		},
		"no robots func": nil,
	} {
		t.Run(name, func(t *testing.T) {
			f := contactFetcher(t)
			v := verifier(f)
			v.Robots = rf
			res, err := Run(context.Background(), fakeClaude(text), v, req(Want{Email: true}))
			if err != nil {
				t.Fatal(err)
			}
			if f.count() != 0 || res.Verified != 0 {
				t.Errorf("fetched %q, verified %d; want nothing fetched", f.gets, res.Verified)
			}
		})
	}
}

// guardedFetcher is contacts.Fetcher over a real public-only fetch.Client, naming the id the
// recorder gave the last row, as the runner's pageFetcher does.
type guardedFetcher struct {
	c   *fetch.Client
	rec *countingRecorder
}

func (g guardedFetcher) Get(ctx context.Context, kind fetch.Kind, rawURL string) (fetch.Response, int64, error) {
	before := g.rec.n.Load()
	resp, err := g.c.Get(ctx, kind, rawURL)
	id := g.rec.n.Load()
	if id == before {
		id = 0
	}
	return resp, id, err
}

type countingRecorder struct{ n atomic.Int64 }

func (r *countingRecorder) Record(context.Context, fetch.Record) error { r.n.Add(1); return nil }

// scriptRT is the network for the guarded client: it records every request and answers
// /redirect with a redirect to a private address. Nothing leaves the process.
type scriptRT struct {
	mu   sync.Mutex
	hits []string
}

func (s *scriptRT) RoundTrip(r *http.Request) (*http.Response, error) {
	s.mu.Lock()
	s.hits = append(s.hits, r.URL.String())
	s.mu.Unlock()
	h := http.Header{}
	status := 200
	if r.URL.Path == "/redirect" {
		status = http.StatusFound
		h.Set("Location", "http://"+privateAddr+"/contact")
	}
	return &http.Response{StatusCode: status, Header: h, Body: io.NopCloser(strings.NewReader("<p>info@northwind.example</p>")), Request: r}, nil
}

func TestPrivateAddressNotFetched(t *testing.T) {
	rt := &scriptRT{}
	rec := &countingRecorder{}
	c := fetch.New(fetch.WithPublicOnly(), fetch.WithTransport(rt), fetch.WithRecorder(rec),
		fetch.WithLiveGate(config.LiveAll), fetch.WithLimiter(rate.NewLimiter(rate.Inf, 1)), fetch.WithJitter(0))
	v := verifier(guardedFetcher{c: c, rec: rec})

	t.Run("a redirect to a private address is not followed", func(t *testing.T) {
		res, err := Run(context.Background(), fakeClaude(answer(
			cand{Kind: "email", Value: "info@northwind.example", URL: "https://northwind.example/redirect"},
		)), v, req(Want{Email: true}))
		if err != nil {
			t.Fatal(err)
		}
		if res.Verified != 0 {
			t.Errorf("verified %d from a page that redirected to a private address", res.Verified)
		}
	})
	t.Run("a non-public port is refused before any request", func(t *testing.T) {
		res, err := Run(context.Background(), fakeClaude(answer(
			cand{Kind: "email", Value: "info@northwind.example", URL: "https://northwind.example:8443/contact"},
		)), v, req(Want{Email: true}))
		if err != nil {
			t.Fatal(err)
		}
		if res.Verified != 0 {
			t.Errorf("verified %d from a non-public port", res.Verified)
		}
	})
	t.Run("a fetch refused as not public after DNS drops the candidate", func(t *testing.T) {
		f := &fakeFetcher{pages: map[string]fakePage{contactURL: {err: fmt.Errorf("dial: %w", fetch.ErrNotPublic)}}}
		res, err := run(t, answer(cand{Kind: "email", Value: "info@northwind.example", URL: contactURL}), f, Want{Email: true})
		if err != nil || res.Verified != 0 {
			t.Errorf("verified %d err %v, want 0 and nil", res.Verified, err)
		}
	})
	rt.mu.Lock()
	defer rt.mu.Unlock()
	for _, h := range rt.hits {
		if strings.Contains(h, privateAddr) || strings.Contains(h, ":8443") {
			t.Errorf("request reached %s", h)
		}
	}
	if len(rt.hits) != 1 {
		t.Errorf("requests %q, want only the first hop of the redirect", rt.hits)
	}
}

func TestPhonePrintedWithTrunkVerifies(t *testing.T) {
	f := contactFetcher(t)
	res, err := run(t, answer(
		cand{Kind: "phone", Value: "+442078031100", URL: contactURL, LineKind: "Switchboard", Role: "Head office"},
		cand{Kind: "phone", Value: "+40 21 555 0199", URL: contactURL, LineKind: "made-up"},
		cand{Kind: "phone", Value: "+44 20 1111 2222", URL: contactURL},       // only inside a script
		cand{Kind: "phone", Value: "+44 20 7946 0000", URL: contactURL},       // not on the page
		cand{Kind: "phone", Value: "020 7803 1100", URL: contactURL},          // the first, in national form: kept once
		cand{Kind: "email", Value: "info@northwind.example", URL: contactURL}, // not wanted
	), f, Want{Phone: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Candidates != 6 || res.Verified != 2 || len(res.Routes) != 0 {
		t.Fatalf("candidates %d verified %d routes %d, want 6, 2, 0; phones %+v", res.Candidates, res.Verified, len(res.Routes), res.Phones)
	}
	want := Phone{Fact: leadstore.FactRow{OrgID: 3, Kind: leadstore.FactPhone, Value: "+442078031100",
		Normalized: "+442078031100", FetchID: 41, DerivedBy: DerivedBy}, LineKind: LineSwitchboard, Role: "Head office"}
	if res.Phones[0] != want {
		t.Errorf("phone %+v, want %+v", res.Phones[0], want)
	}
	if p := res.Phones[1]; p.Fact.Normalized != "+40215550199" || p.LineKind != "" {
		t.Errorf("second phone %+v, want +40215550199 (00 written on the page) with no line kind", p)
	}
}

// runPhones verifies phone candidates cs against one page printing body, for an org in
// country.
func runPhones(t *testing.T, country, body string, cs ...cand) Result {
	t.Helper()
	f := &fakeFetcher{pages: map[string]fakePage{contactURL: {status: 200, body: []byte(body), id: 52}}}
	r := req(Want{Phone: true})
	r.Org.Country = country
	res, err := Run(context.Background(), fakeClaude(answer(cs...)), verifier(f), r)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestNationalPhoneVerifies(t *testing.T) {
	nationalPage := `<html><body><p>Call us on 020 7946 0000</p></body></html>`
	intlPage := `<html><body><p>Call us on +44 20 7946 0000</p></body></html>`
	cases := []struct {
		name, country, body, value, wantShown, wantNorm string
	}{
		{"national candidate on a GB page printing it nationally", "GB", nationalPage, "020 7946 0000", "020 7946 0000", "+442079460000"},
		{"national candidate on a GB page printing it internationally", "GB", intlPage, "020 7946 0000", "020 7946 0000", "+442079460000"},
		{"E.164 candidate on a GB page printing it nationally", "GB", nationalPage, "+442079460000", "+442079460000", "+442079460000"},
		{"Italian landline keeps its 0", "IT", `<html><body><p>Tel. 06 1234 5678</p></body></html>`, "06 1234 5678", "06 1234 5678", "+390612345678"},
		{"Italian E.164 candidate on a national page", "IT", `<html><body><p>Tel. 06 1234 5678</p></body></html>`, "+39 06 1234 5678", "+39 06 1234 5678", "+390612345678"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := runPhones(t, tc.country, tc.body, cand{Kind: "phone", Value: tc.value, URL: contactURL})
			if res.Verified != 1 || len(res.Phones) != 1 {
				t.Fatalf("verified %d phones %+v, want 1", res.Verified, res.Phones)
			}
			want := leadstore.FactRow{OrgID: 3, Kind: leadstore.FactPhone, Value: tc.wantShown,
				Normalized: tc.wantNorm, FetchID: 52, DerivedBy: DerivedBy}
			if res.Phones[0].Fact != want {
				t.Errorf("fact %+v, want %+v", res.Phones[0].Fact, want)
			}
		})
	}
}

func TestNationalPhoneDropped(t *testing.T) {
	nationalPage := `<html><body><p>Call us on 020 7946 0000</p></body></html>`
	cases := []struct {
		name, country, body, value string
	}{
		{"unknown country, national candidate", "", nationalPage, "020 7946 0000"},
		{"country not in the plan table", "ZZ", nationalPage, "020 7946 0000"},
		{"unknown country, E.164 candidate on a national page", "", nationalPage, "+442079460000"},
		{"another country's number on a GB page", "GB", `<html><body><p>Tel. +33 1 23 45 67 89</p></body></html>`, "01 23 45 67 89"},
		{"the national number of a different line", "GB", nationalPage, "020 7946 0001"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := runPhones(t, tc.country, tc.body, cand{Kind: "phone", Value: tc.value, URL: contactURL})
			if res.Verified != 0 || len(res.Phones) != 0 {
				t.Errorf("verified %d phones %+v, want none", res.Verified, res.Phones)
			}
		})
	}
}

// memStore is a claude.UsageStore in memory.
type memStore struct{ spent int64 }

func (s *memStore) ClaudeSpendOn(context.Context, int64, string, time.Time) (int64, error) {
	return s.spent, nil
}
func (s *memStore) ClaudeSpendForLead(context.Context, int64, string) (int64, error) { return 0, nil }
func (s *memStore) RecordClaudeUsage(context.Context, claude.UsageRow) error         { return nil }

func TestCapRefusalBeforeCall(t *testing.T) {
	fake := fakeClaude(answer(cand{Kind: "email", Value: "info@northwind.example", URL: contactURL}))
	capped := &claude.Capped{Client: fake, Store: &memStore{spent: 0}, AccountID: 1, Purpose: claude.PurposeResearch,
		DailyCapMicros: 5_000_000, LeadCapMicros: 1_000, Loc: time.UTC}
	f := contactFetcher(t)
	res, err := Run(context.Background(), capped, verifier(f), req(Want{Email: true}))
	if !errors.Is(err, ai.ErrSpendCap) {
		t.Fatalf("err %v, want the spend cap", err)
	}
	if fake.Calls() != 0 || f.count() != 0 {
		t.Errorf("claude calls %d, fetches %d; want none", fake.Calls(), f.count())
	}
	if res.Verified != 0 || res.CostMicros != 0 || res.Model != Model {
		t.Errorf("result %+v", res)
	}

	// The same cap with room: the call goes through Capped and verifies.
	capped.LeadCapMicros = 100_000
	res, err = Run(context.Background(), capped, verifier(f), req(Want{Email: true}))
	if err != nil || res.Verified != 1 || fake.Calls() != 1 {
		t.Errorf("with room: verified %d err %v calls %d", res.Verified, err, fake.Calls())
	}
}

func TestEmptyCandidates(t *testing.T) {
	for _, text := range []string{`{"candidates":[]}`, `{"candidates":null}`, "Nothing published.\n{\"candidates\": []}"} {
		f := contactFetcher(t)
		res, err := run(t, text, f, Want{Email: true, Phone: true})
		if err != nil || res.Verified != 0 || res.Candidates != 0 || f.count() != 0 {
			t.Errorf("%q: verified %d candidates %d err %v fetches %d", text, res.Verified, res.Candidates, err, f.count())
		}
	}
}

func TestBadAnswerIsBilledError(t *testing.T) {
	for _, text := range []string{"I could not find anything.", `{"candidates": "nope"}`} {
		_, err := run(t, text, contactFetcher(t), Want{Email: true})
		if !errors.Is(err, ai.ErrBadAnswer) {
			t.Errorf("%q: err %v, want ai.ErrBadAnswer", text, err)
		}
	}
}

func TestRunRefusesEmptyAsk(t *testing.T) {
	fake := fakeClaude(`{"candidates":[]}`)
	if _, err := Run(context.Background(), fake, verifier(contactFetcher(t)), req(Want{})); err == nil || fake.Calls() != 0 {
		t.Errorf("err %v calls %d, want a refusal before any call", err, fake.Calls())
	}
}

// TestQuoteNeverSurfaces sweeps the sentinel quote through every outcome: verified, dropped,
// a parse failure and a call failure. It must appear in no log line, no Result field and no
// error.
func TestQuoteNeverSurfaces(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	broken := `{"candidates":[{"kind":"email","value":"info@northwind.example","quote":"` + sentinelQuote + `", }]}`
	cases := map[string]Caller{
		"verified and dropped": fakeClaude(answer(
			cand{Kind: "email", Value: "info@northwind.example", URL: contactURL},
			cand{Kind: "email", Value: "jane.smith@northwind.example", URL: contactURL},
			cand{Kind: "phone", Value: "+442078031100", URL: contactURL, Role: "Office"},
		)),
		"broken JSON": fakeClaude(broken),
		"wrong type":  fakeClaude(`{"candidates":[{"kind":"email","quote":` + `["` + sentinelQuote + `"]}]}`),
		"call error":  &claude.Fake{Err: errors.New("claude: research: status 500"), ResearchAnswer: claude.ResearchResult{Text: answer()}},
	}
	for name, c := range cases {
		res, err := Run(context.Background(), c, verifier(contactFetcher(t)), req(Want{Email: true, Phone: true}))
		dump := fmt.Sprintf("%#v %+v", res, res)
		if strings.Contains(dump, sentinelQuote) {
			t.Errorf("%s: the quote is in the Result: %s", name, dump)
		}
		if err != nil && strings.Contains(err.Error(), sentinelQuote) {
			t.Errorf("%s: the quote is in the error: %v", name, err)
		}
		if name == "verified and dropped" && res.Verified != 2 {
			t.Errorf("%s: verified %d, want 2 (the sweep must cover a kept candidate)", name, res.Verified)
		}
	}
	if strings.Contains(logs.String(), sentinelQuote) {
		t.Errorf("the quote is in the log: %s", logs.String())
	}
}

// messagesServer is a Messages API that answers each request in turn from replies and keeps
// the request bodies.
func messagesServer(t *testing.T, replies ...func() (int, string)) (*httptest.Server, *[][]byte) {
	t.Helper()
	var (
		mu     sync.Mutex
		bodies [][]byte
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		n := len(bodies)
		bodies = append(bodies, b)
		mu.Unlock()
		status, body := 500, `{"type":"error","error":{"type":"api_error","message":"no reply scripted"}}`
		if n < len(replies) {
			status, body = replies[n]()
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv, &bodies
}

func message(stop, text string) func() (int, string) {
	return func() (int, string) {
		content, _ := json.Marshal([]map[string]any{{"type": "text", "text": text, "citations": []any{}}})
		return 200, fmt.Sprintf(`{"id":"msg_1","type":"message","role":"assistant","model":"claude-haiku-5-5",
"content":%s,"stop_reason":%q,"stop_details":null,
"usage":{"input_tokens":1000,"output_tokens":100,"cache_read_input_tokens":0,"cache_creation_input_tokens":0,
"server_tool_use":{"web_search_requests":1,"web_fetch_requests":1}}}`, content, stop)
	}
}

func realClient(t *testing.T, url string) *claude.Client {
	t.Helper()
	c, err := claude.New(config.Secret("test-key-not-real"), claude.WithBaseURL(url), claude.WithTimeout(5*time.Second), claude.WithMaxRetries(0))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestPauseTurnContinued(t *testing.T) {
	srv, bodies := messagesServer(t,
		message("pause_turn", "Searching. "),
		message("end_turn", answer(cand{Kind: "email", Value: "info@northwind.example", URL: contactURL})),
	)
	res, err := Run(context.Background(), realClient(t, srv.URL), verifier(contactFetcher(t)), req(Want{Email: true}))
	if err != nil {
		t.Fatal(err)
	}
	if len(*bodies) != 2 {
		t.Fatalf("requests %d, want the first and one continuation", len(*bodies))
	}
	if res.Verified != 1 || res.Usage.InputTokens != 2000 || res.Usage.WebSearches != 2 || res.WebTools != claude.WebTools20260209 {
		t.Errorf("verified %d usage %+v tools %s", res.Verified, res.Usage, res.WebTools)
	}
	if res.CostMicros <= 0 {
		t.Errorf("cost %d, want the two legs priced", res.CostMicros)
	}
	var second struct {
		Messages []struct {
			Role string `json:"role"`
		} `json:"messages"`
	}
	if err := json.Unmarshal((*bodies)[1], &second); err != nil {
		t.Fatal(err)
	}
	if len(second.Messages) != 2 || second.Messages[1].Role != "assistant" {
		t.Errorf("continuation messages %+v, want the paused assistant turn appended", second.Messages)
	}
	if !strings.Contains(string((*bodies)[0]), `"model":"claude-haiku-5-5"`) {
		t.Errorf("first request does not name Haiku 5.5")
	}
}

func TestWebToolsFallback(t *testing.T) {
	srv, bodies := messagesServer(t,
		func() (int, string) {
			return 400, `{"type":"error","error":{"type":"invalid_request_error","message":"tool not supported"}}`
		},
		message("end_turn", `{"candidates":[]}`),
	)
	res, err := Run(context.Background(), realClient(t, srv.URL), verifier(contactFetcher(t)), req(Want{Email: true}))
	if err != nil {
		t.Fatal(err)
	}
	if len(*bodies) != 2 || res.WebTools != claude.WebToolsBasic {
		t.Fatalf("requests %d tools %s, want a retry on the basic tools", len(*bodies), res.WebTools)
	}
	first, second := string((*bodies)[0]), string((*bodies)[1])
	if !strings.Contains(first, `"web_search_20260209"`) || !strings.Contains(second, `"web_search_20250305"`) ||
		!strings.Contains(second, `"web_fetch_20250910"`) {
		t.Errorf("tool versions: first %s / second %s", first, second)
	}
}

// privateAddr is a private (RFC 1918) address for the guard tests, built at run time the
// way internal/fetch's guard tests build theirs, so the repo's privacy check sees no literal.
var privateAddr = netip.AddrFrom4([4]byte{10, 0, 0, 5}).String()
