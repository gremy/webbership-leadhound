package console

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/store"
)

const (
	apiTestToken = "api-test-token-0123456789abcdef-0123456789abcdef" // #nosec G101 -- a made-up token for these tests
	// apiJobMarker is in every job field a fake answers, so a refusal that leaks one shows.
	apiJobMarker = "Zebra-Marker"
)

func apiTestHash() string {
	sum := sha256.Sum256([]byte(apiTestToken))
	return hex.EncodeToString(sum[:])
}

// apiFake is the fake backend, recording what the API's reads asked for.
type apiFake struct {
	*fakeBackend
	next                []NextJob
	hidden              []HiddenJob
	moreVisible         int
	nextErr, trackedErr error
	days, limit         int
	status              string
}

func (f *apiFake) NextJobs(_ context.Context, days, limit int) (NextJobList, error) {
	f.count("NextJobs")
	f.days, f.limit = days, limit
	return NextJobList{Jobs: f.next, Hidden: f.hidden, MoreVisible: f.moreVisible}, f.nextErr
}

func (f *apiFake) Tracked(ctx context.Context, status string) (TrackedList, error) {
	f.status = status
	l, err := f.fakeBackend.Tracked(ctx, status)
	if f.trackedErr != nil {
		err = f.trackedErr
	}
	return l, err
}

// newAPIFake holds one job of each kind, so a sweep has job data to leak.
func newAPIFake() *apiFake {
	since := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	return &apiFake{
		fakeBackend: &fakeBackend{
			job:     JobPage{JobID: 42, Title: apiJobMarker, Company: "Acme", Status: "new"},
			tracked: []Tracked{{JobID: 7, Title: apiJobMarker, Company: "Acme", Status: "applied", Reason: "note.md", Since: since}},
			out:     TrackOutcome{From: "new", To: "applied", Message: "Marked."},
		},
		next: []NextJob{{JobID: 42, Title: apiJobMarker, Company: "Acme", BriefDay: since, BriefN: 3}},
	}
}

type apiHarness struct {
	h    http.Handler
	b    *apiFake
	a    *Auth
	name string
}

// newAPIHarness serves the console with hash as the API token's. Every test's whole log is
// checked at its end: neither the token nor its hash is ever written.
func newAPIHarness(t *testing.T, hash string) *apiHarness {
	t.Helper()
	a, _, _ := testAuth(t)
	logs := &syncBuffer{}
	b := newAPIFake()
	t.Cleanup(func() {
		got := logs.String()
		for _, secret := range []string{apiTestToken, apiTestHash(), "wrong-token"} {
			if strings.Contains(got, secret) {
				t.Errorf("the log holds a token: %s", got)
			}
		}
	})
	return &apiHarness{a: a, b: b, name: "__Host-lh_session", h: New(b, a, Options{Location: time.UTC,
		Logger: slog.New(slog.NewTextHandler(logs, nil)), Now: func() time.Time { return statusNow }, APITokenSHA256: hash})}
}

func (h *apiHarness) do(r *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.h.ServeHTTP(w, r)
	return w
}

// sessionCookie is a live sign-in, as a browser would send it.
func (h *apiHarness) sessionCookie(t *testing.T) *http.Cookie {
	t.Helper()
	tok, err := h.a.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	return &http.Cookie{Name: h.name, Value: tok, Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode}
}

// apiRequest is a request with the given bearer token ("" sends none).
func apiRequest(method, target, body, token string) *http.Request {
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	return r
}

func apiGet(target string) *http.Request { return apiRequest(http.MethodGet, target, "", apiTestToken) }

func apiPost(target, body string) *http.Request {
	return apiRequest(http.MethodPost, target, body, apiTestToken)
}

// apiRouteRequest is the request an apiRoutes entry answers to.
func apiRouteRequest(rt apiRoute, target, token string) *http.Request {
	method, _, _ := strings.Cut(rt.pattern, " ")
	body := ""
	if method == http.MethodPost {
		body = `{"action":"applied","reason":"note.md"}`
	}
	return apiRequest(method, target, body, token)
}

// wantAPIError asserts an answer is code as a JSON error with a non-empty message, not a redirect,
// and returns the message.
func wantAPIError(t *testing.T, w *httptest.ResponseRecorder, code int) string {
	t.Helper()
	if w.Code != code {
		t.Errorf("status %d, want %d: %s", w.Code, code, w.Body)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type %q", ct)
	}
	if loc := w.Header().Get("Location"); loc != "" {
		t.Errorf("redirected to %q", loc)
	}
	var e APIError
	dec := json.NewDecoder(w.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&e); err != nil {
		t.Fatalf("not an APIError: %v", err)
	}
	if e.Error == "" {
		t.Error("empty error")
	}
	return e.Error
}

func apiDecode[T any](t *testing.T, w *httptest.ResponseRecorder) T {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type %q", ct)
	}
	var v T
	dec := json.NewDecoder(w.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

// The sweep: every route, by every way in that is not the token, is refused with no job data and
// the backend never asked.
func TestAPIEveryRouteRefusedWithoutToken(t *testing.T) {
	for _, rt := range apiRoutes {
		for name, build := range map[string]func(h *apiHarness) *http.Request{
			"no token":    func(*apiHarness) *http.Request { return apiRouteRequest(rt, rt.sample, "") },
			"wrong token": func(*apiHarness) *http.Request { return apiRouteRequest(rt, rt.sample, "wrong-token") },
			"query token": func(*apiHarness) *http.Request {
				return apiRouteRequest(rt, rt.sample+"?token="+apiTestToken+"&access_token="+apiTestToken+"&Authorization=Bearer+"+apiTestToken, "")
			},
			"session": func(h *apiHarness) *http.Request {
				r := apiRouteRequest(rt, rt.sample, "")
				r.AddCookie(h.sessionCookie(t))
				return r
			},
		} {
			t.Run(rt.pattern+"/"+name, func(t *testing.T) {
				h := newAPIHarness(t, apiTestHash())
				w := h.do(build(h))
				body := w.Body.String()
				wantAPIError(t, w, http.StatusUnauthorized)
				if w.Header().Get("WWW-Authenticate") != "Bearer" {
					t.Error("no WWW-Authenticate")
				}
				if strings.Contains(body, apiJobMarker) || strings.Contains(body, "Acme") {
					t.Errorf("job data in the refusal: %s", body)
				}
				if n := h.b.total(); n != 0 {
					t.Errorf("backend called %d times", n)
				}
			})
		}
	}
}

// Each route works with the token, so the sweep above refuses something real.
func TestAPIEveryRouteOpensWithToken(t *testing.T) {
	for _, rt := range apiRoutes {
		h := newAPIHarness(t, apiTestHash())
		w := h.do(apiRouteRequest(rt, rt.sample, apiTestToken))
		if w.Code != http.StatusOK {
			t.Errorf("%s: status %d: %s", rt.pattern, w.Code, w.Body)
		}
		if h.b.total() != 1 {
			t.Errorf("%s: backend called %d times, want 1", rt.pattern, h.b.total())
		}
		for header, want := range map[string]string{"Cache-Control": "no-store", "X-Content-Type-Options": "nosniff", "Content-Security-Policy": contentSecurityPolicy} {
			if got := w.Header().Get(header); got != want {
				t.Errorf("%s: %s = %q", rt.pattern, header, got)
			}
		}
	}
}

func apiUnknownPaths() []string {
	return []string{"/api", "/api/", "/api/nope", "/api/leads", "/api/jobs", "/api/jobs/", "/api/jobs/next/", "/api/jobs/42/nope",
		"/api/jobs/abc", "/api/jobs/0", "/api/jobs/-1", "/api//jobs/next", "/api/../leads", "/api/./tracked", "/api/%2e%2e/leads", "/api%2Fjobs/next"}
}

// With no token configured the API is not there, whatever is presented: never a redirect to the
// sign-in page, never the session check.
func TestAPIOffAnswers404(t *testing.T) {
	var targets []*http.Request
	for _, rt := range apiRoutes {
		targets = append(targets, apiRouteRequest(rt, rt.sample, ""), apiRouteRequest(rt, rt.sample, apiTestToken))
	}
	for _, p := range apiUnknownPaths() {
		targets = append(targets, apiRequest(http.MethodGet, p, "", ""), apiRequest(http.MethodPost, p, "", ""))
	}
	h := newAPIHarness(t, "")
	session := h.sessionCookie(t)
	for _, r := range targets {
		label := r.Method + " " + r.URL.Path
		t.Run(label, func(t *testing.T) {
			wantAPIError(t, h.do(r), http.StatusNotFound)
			withSession := r.Clone(r.Context())
			withSession.AddCookie(session)
			wantAPIError(t, h.do(withSession), http.StatusNotFound)
			crossSite := r.Clone(r.Context())
			crossSite.Header.Set("Sec-Fetch-Site", "cross-site")
			wantAPIError(t, h.do(crossSite), http.StatusNotFound)
		})
	}
	if n := h.b.total(); n != 0 {
		t.Errorf("backend called %d times", n)
	}
}

// An unknown path never reaches the session check: 401 first, then JSON 404.
func TestAPIUnknownPaths(t *testing.T) {
	h := newAPIHarness(t, apiTestHash())
	for _, p := range apiUnknownPaths() {
		t.Run(p, func(t *testing.T) {
			wantAPIError(t, h.do(apiRequest(http.MethodGet, p, "", "")), http.StatusUnauthorized)
			wantAPIError(t, h.do(apiRequest(http.MethodGet, p, "", "wrong-token")), http.StatusUnauthorized)
			wantAPIError(t, h.do(apiRequest(http.MethodGet, p, "", apiTestToken)), http.StatusNotFound)
		})
	}
	if n := h.b.total(); n != 0 {
		t.Errorf("backend called %d times", n)
	}
}

func TestAPIMethodMismatchIsJSON(t *testing.T) {
	h := newAPIHarness(t, apiTestHash())
	for _, c := range []struct{ method, target, allow string }{
		{http.MethodPost, "/api/jobs/next", "GET"},
		{http.MethodPost, "/api/jobs/42", "GET"},
		{http.MethodDelete, "/api/tracked", "GET"},
		{http.MethodGet, "/api/jobs/42/track", "POST"},
		{http.MethodPut, "/api/jobs/42/track", "POST"},
	} {
		w := h.do(apiRequest(c.method, c.target, "", apiTestToken))
		if got := w.Header().Get("Allow"); got != c.allow {
			t.Errorf("%s %s: Allow %q, want %q", c.method, c.target, got, c.allow)
		}
		wantAPIError(t, w, http.StatusMethodNotAllowed)
		wantAPIError(t, h.do(apiRequest(c.method, c.target, "", "")), http.StatusUnauthorized)
	}
	if n := h.b.total(); n != 0 {
		t.Errorf("backend called %d times", n)
	}
}

func TestAPIMalformedAuthorization(t *testing.T) {
	rightLengthWrong := strings.Repeat("x", len(apiTestToken))
	for _, header := range []string{
		"", " ", "Bearer", "Bearer ", "Bearer  " + apiTestToken, " Bearer " + apiTestToken, "Bearer " + apiTestToken + " ",
		"bearer " + apiTestToken, "BEARER " + apiTestToken, "Basic " + apiTestToken, "Token " + apiTestToken, apiTestToken,
		"Bearer " + apiTestHash(), "Bearer " + rightLengthWrong, "Bearer\t" + apiTestToken,
		"Bearer " + strings.ToUpper(apiTestToken), "Bearer " + apiTestToken + "x", "Bearer " + apiTestToken[:len(apiTestToken)-1],
	} {
		h := newAPIHarness(t, apiTestHash())
		r := apiRequest(http.MethodGet, "/api/jobs/42", "", "")
		r.Header.Set("Authorization", header)
		w := h.do(r)
		if strings.Contains(w.Body.String(), apiJobMarker) {
			t.Errorf("%q: job data", header)
		}
		wantAPIError(t, w, http.StatusUnauthorized)
		if n := h.b.total(); n != 0 {
			t.Errorf("%q: backend called %d times", header, n)
		}
	}
}

// A stored hash in the wrong shape matches nothing: the check fails closed.
func TestAPIMalformedStoredHashOpensNothing(t *testing.T) {
	for _, stored := range []string{" ", "abc", strings.ToUpper(apiTestHash()), apiTestToken} {
		h := newAPIHarness(t, stored)
		wantAPIError(t, h.do(apiGet("/api/jobs/42")), http.StatusUnauthorized)
	}
}

// The token opens nothing outside /api/: every console route, with it and no session, answers as
// it does with neither.
func TestAPITokenOpensNothingOutside(t *testing.T) {
	for _, rt := range routes {
		t.Run(rt.pattern+" "+rt.sample, func(t *testing.T) {
			build := func() *http.Request {
				if strings.HasPrefix(rt.pattern, "POST ") {
					return post(rt.sample, url.Values{})
				}
				return get(rt.sample)
			}
			on := newAPIHarness(t, apiTestHash())
			r := build()
			r.Header.Set("Authorization", "Bearer "+apiTestToken)
			with := on.do(r)
			without := newAPIHarness(t, "").do(build())
			if with.Code != without.Code || with.Header().Get("Location") != without.Header().Get("Location") {
				t.Errorf("with the token %d %q, without %d %q", with.Code, with.Header().Get("Location"), without.Code, without.Header().Get("Location"))
			}
			if with.Body.String() != without.Body.String() {
				t.Errorf("the token changed the body:\n%s\n---\n%s", with.Body, without.Body)
			}
			if !rt.public && on.b.total() != 0 {
				t.Errorf("backend called %d times with no session", on.b.total())
			}
		})
	}
}

// No API path is somewhere a sign-in may return to, and API routes join no sign-in table.
func TestAPINeverASignInTarget(t *testing.T) {
	var targets []string
	for _, rt := range apiRoutes {
		targets = append(targets, rt.sample)
	}
	targets = append(targets, "/api/", "/api/tracked?status=applied")
	for _, next := range targets {
		h := newAPIHarness(t, apiTestHash())
		w := h.do(post("/login", url.Values{"email": {testEmail}, "password": {testPassword}, "next": {next}}))
		if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/" {
			t.Errorf("next=%s: %d to %q, want / ", next, w.Code, w.Header().Get("Location"))
		}
		w = h.do(get("/login?next=" + url.QueryEscape(next)))
		if strings.Contains(w.Body.String(), "/api/") {
			t.Errorf("next=%s: the sign-in form carries it", next)
		}
	}
	for _, rt := range routes {
		if strings.Contains(rt.pattern, "/api") {
			t.Errorf("%s is in routes", rt.pattern)
		}
	}
	for _, rt := range apiRoutes {
		if !strings.Contains(rt.pattern, " /api/") || !strings.HasPrefix(rt.sample, "/api/") {
			t.Errorf("%s (%s) is not under /api/", rt.pattern, rt.sample)
		}
	}
}

// The API exposes jobs only: adding a route over leads, mail or campaigns is a separate decision.
func TestAPIExposesJobsOnly(t *testing.T) {
	for _, rt := range apiRoutes {
		p := strings.ToLower(rt.pattern)
		if !strings.Contains(p, "/api/jobs/") && !strings.Contains(p, "/api/tracked") {
			t.Errorf("%s is not a job route", rt.pattern)
		}
	}
}

func TestAPINextJobs(t *testing.T) {
	h := newAPIHarness(t, apiTestHash())
	since := time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)
	h.b.next = []NextJob{
		{JobID: 1, Title: "Go Engineer", Company: "Acme", Location: "Remote", Status: "new", ListingStatus: "open", URL: "https://x.example/1?a=1&b=2",
			ApplyURL: "https://ats.example/1", ApplyType: "ats", BriefDay: time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC), BriefN: 4,
			Prior: []PriorJob{{JobID: 9, Title: "Go Dev", Status: "applied", Since: since}}},
		{JobID: 2}, // no observation: still listed, with empty fields
	}
	w := h.do(apiGet("/api/jobs/next"))
	raw := w.Body.String()
	got := apiDecode[APINextJobs](t, w)
	want := APINextJobs{Jobs: []APINextJob{
		{JobID: 1, Title: "Go Engineer", Company: "Acme", Location: "Remote", Status: "new", ListingStatus: "open", URL: "https://x.example/1?a=1&b=2",
			ApplyURL: "https://ats.example/1", ApplyType: "ats", BriefDay: "2026-11-02", BriefN: 4,
			Prior: []APIPriorJob{{JobID: 9, Title: "Go Dev", Status: "applied", Since: since}}},
		{JobID: 2, Prior: []APIPriorJob{}},
	}, Hidden: []APIHiddenJob{}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v\nwant %+v", got, want)
	}
	if !strings.Contains(raw, `"prior":[]`) || !strings.Contains(raw, "a=1&b=2") {
		t.Errorf("raw answer: %s", raw)
	}
	for _, field := range []string{"job_id", "title", "company", "location", "status", "listing_status", "url", "apply_url", "apply_type", "brief_day", "brief_n", "prior", "since"} {
		if !strings.Contains(raw, `"`+field+`":`) {
			t.Errorf("no %s in %s", field, raw)
		}
	}
	if h.b.days != APIDaysDefault || h.b.limit != apiLimitDefault {
		t.Errorf("days %d limit %d", h.b.days, h.b.limit)
	}
}

// Cold start: no brief ever sent is an empty list, never null.
func TestAPINextJobsEmpty(t *testing.T) {
	h := newAPIHarness(t, apiTestHash())
	h.b.next = nil
	w := h.do(apiGet("/api/jobs/next"))
	if w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != `{"jobs":[],"hidden":[],"more_visible":0}` {
		t.Errorf("%d %s", w.Code, w.Body)
	}
}

// What the skip rules hid reaches the answer whole, under the contract's names, and a job with no
// brief carries an empty day.
func TestAPINextJobsHidden(t *testing.T) {
	h := newAPIHarness(t, apiTestHash())
	h.b.next = []NextJob{{JobID: 3}}
	h.b.hidden = []HiddenJob{{JobID: 1, Company: "Acme", Title: "Go Engineer", Reason: "a reason", Briefed: true}, {JobID: 2}}
	h.b.moreVisible = 4
	w := h.do(apiGet("/api/jobs/next"))
	raw := w.Body.String()
	got := apiDecode[APINextJobs](t, w)
	want := []APIHiddenJob{{JobID: 1, Company: "Acme", Title: "Go Engineer", Reason: "a reason", Briefed: true}, {JobID: 2}}
	if !reflect.DeepEqual(got.Hidden, want) || got.MoreVisible != 4 {
		t.Errorf("got %+v", got)
	}
	if got.Jobs[0].BriefDay != "" || !strings.Contains(raw, `"brief_day":""`) || !strings.Contains(raw, `"more_visible":4`) {
		t.Errorf("raw answer: %s", raw)
	}
	for _, field := range []string{"hidden", "job_id", "company", "title", "reason", "briefed", "more_visible"} {
		if !strings.Contains(raw, `"`+field+`":`) {
			t.Errorf("no %s in %s", field, raw)
		}
	}
}

func TestAPINextJobsNumbers(t *testing.T) {
	for _, c := range []struct {
		query       string
		limit, days int
	}{
		{"", 20, 14}, {"limit=1", 1, 14}, {"limit=50", 50, 14}, {"limit=51", 50, 14}, {"days=1", 20, 1}, {"days=60", 20, 60},
		{"days=61", 20, 60}, {"limit=5&days=7", 5, 7}, {"limit=1000000&days=1000000", 50, 60},
		{"limit=99999999999999999999999", 50, 14}, {"days=99999999999999999999999", 20, 60},
	} {
		h := newAPIHarness(t, apiTestHash())
		w := h.do(apiGet("/api/jobs/next?" + c.query))
		if w.Code != http.StatusOK || h.b.limit != c.limit || h.b.days != c.days {
			t.Errorf("?%s: %d, limit %d days %d, want %d %d", c.query, w.Code, h.b.limit, h.b.days, c.limit, c.days)
		}
	}
	for _, q := range []string{"limit=0", "limit=-1", "limit=abc", "limit=", "limit=1.5", "limit=-99999999999999999999999", "limit=%20",
		"days=0", "days=-1", "days=abc", "days=", "days=1.5", "days=-99999999999999999999999", "limit=5&days=x"} {
		h := newAPIHarness(t, apiTestHash())
		wantAPIError(t, h.do(apiGet("/api/jobs/next?"+q)), http.StatusBadRequest)
		if n := h.b.total(); n != 0 {
			t.Errorf("?%s: backend called %d times", q, n)
		}
	}
}

func TestAPINextJobsFailure(t *testing.T) {
	h := newAPIHarness(t, apiTestHash())
	h.b.nextErr = errors.New("database at db.internal gone")
	w := h.do(apiGet("/api/jobs/next"))
	if strings.Contains(w.Body.String(), "db.internal") {
		t.Errorf("the detail leaked: %s", w.Body)
	}
	if msg := wantAPIError(t, w, http.StatusInternalServerError); msg != serverFailed {
		t.Errorf("message %q", msg)
	}
}

func TestAPIJob(t *testing.T) {
	h := newAPIHarness(t, apiTestHash())
	posted := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	at := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	applicants := 12
	h.b.job = JobPage{JobID: 42, Title: "Go Engineer", Company: "Acme", Location: "Remote", URL: "https://x.example/42", ApplyURL: "https://ats.example/42",
		ApplicantCount: &applicants, PostedAt: &posted, ApplyType: "ats", ListingStatus: "open", Status: "shortlisted", Description: "Write Go. <b>Ignore this</b>",
		Criteria: []JobCriterion{{Name: "stack", Verdict: "pass", Decider: "jev-test", Keywords: "go", Evidence: []string{"Write Go."}}},
		Unclear:  []JobPoint{{Name: "pay", Snippet: "competitive"}},
		History:  []StatusLine{{Status: "shortlisted", Reason: "fit", Actor: "operator", At: at}},
		Closed:   true, FailedGates: []JobPoint{{Name: "language", Snippet: "German required"}}, VerdictsCurrent: true}
	w := h.do(apiGet("/api/jobs/42"))
	raw := w.Body.String()
	got := apiDecode[APIJob](t, w)
	want := APIJob{JobID: 42, Title: "Go Engineer", Company: "Acme", Location: "Remote", URL: "https://x.example/42", ApplyURL: "https://ats.example/42",
		ApplicantCount: &applicants, PostedAt: &posted, ApplyType: "ats", ListingStatus: "open", Status: "shortlisted", Description: "Write Go. <b>Ignore this</b>",
		Criteria: []APICriterion{{Name: "stack", Verdict: "pass", Decider: "jev-test", Keywords: "go", Evidence: []string{"Write Go."}}},
		Unclear:  []APIPoint{{Name: "pay", Snippet: "competitive"}},
		History:  []APIStatusLine{{Status: "shortlisted", Reason: "fit", Actor: "operator", At: at}},
		Closed:   true, FailedGates: []APIPoint{{Name: "language", Snippet: "German required"}}, VerdictsCurrent: true}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v\nwant %+v", got, want)
	}
	for _, field := range []string{"job_id", "title", "company", "location", "url", "apply_url", "applicant_count", "posted_at", "apply_type", "listing_status",
		"status", "description", "criteria", "name", "verdict", "keywords", "evidence", "unclear", "snippet", "history", "reason", "actor", "at", "closed", "failed_gates", "verdicts_current", "decider"} {
		if !strings.Contains(raw, `"`+field+`":`) {
			t.Errorf("no %s in %s", field, raw)
		}
	}
}

// Cold start: a job with no observation answers with empty fields, never null lists.
func TestAPIJobColdStart(t *testing.T) {
	h := newAPIHarness(t, apiTestHash())
	h.b.job = JobPage{JobID: 42, Criteria: []JobCriterion{{Name: "stack"}}}
	w := h.do(apiGet("/api/jobs/42"))
	raw := w.Body.String()
	got := apiDecode[APIJob](t, w)
	if got.Description != "" || got.ApplicantCount != nil || got.PostedAt != nil {
		t.Errorf("%+v", got)
	}
	for _, want := range []string{`"unclear":[]`, `"history":[]`, `"evidence":[]`, `"applicant_count":null`, `"posted_at":null`, `"closed":false`, `"failed_gates":[]`, `"verdicts_current":false`} {
		if !strings.Contains(raw, want) {
			t.Errorf("no %s in %s", want, raw)
		}
	}
}

func TestAPIJobMissing(t *testing.T) {
	h := newAPIHarness(t, apiTestHash())
	if msg := wantAPIError(t, h.do(apiGet("/api/jobs/7")), http.StatusNotFound); !strings.Contains(msg, "7") {
		t.Errorf("message %q", msg)
	}
	h.b.readErr = errors.New("database at db.internal gone")
	w := h.do(apiGet("/api/jobs/42"))
	if strings.Contains(w.Body.String(), "db.internal") {
		t.Errorf("the detail leaked: %s", w.Body)
	}
	wantAPIError(t, w, http.StatusInternalServerError)
}

// The status the route accepts is exactly store.JobStatuses, though the console takes the list
// from its own Tracked filters, because it never imports the store outside this test.
func TestAPITrackedStatuses(t *testing.T) {
	for _, status := range store.JobStatuses {
		h := newAPIHarness(t, apiTestHash())
		if w := h.do(apiGet("/api/tracked?status=" + status)); w.Code != http.StatusOK || h.b.status != status {
			t.Errorf("%s: %d, asked for %q", status, w.Code, h.b.status)
		}
	}
	for _, f := range trackedFilters {
		if f.Path != "" && f.Path != "all" && !slices.Contains(store.JobStatuses, f.Path) {
			t.Errorf("filter %q is not a job status", f.Path)
		}
	}
	for _, status := range []string{"all", "ALL", "Applied", "offered", "' OR 1=1", "applied,new", "%20"} {
		h := newAPIHarness(t, apiTestHash())
		wantAPIError(t, h.do(apiGet("/api/tracked?status="+url.QueryEscape(status))), http.StatusBadRequest)
		if n := h.b.total(); n != 0 {
			t.Errorf("%q: backend called %d times", status, n)
		}
	}
}

func TestAPITracked(t *testing.T) {
	h := newAPIHarness(t, apiTestHash())
	h.b.matched = 900 // more than the page's cap
	for _, target := range []string{"/api/tracked", "/api/tracked?status="} {
		h.b.status = ""
		got := apiDecode[APITracked](t, h.do(apiGet(target)))
		if h.b.status != "applied" {
			t.Errorf("%s: asked for %q, want applied", target, h.b.status)
		}
		want := APITracked{Total: 900, Rows: []APITrackedRow{{JobID: 7, Title: apiJobMarker, Company: "Acme", Status: "applied", Reason: "note.md", Since: time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)}}}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: got %+v", target, got)
		}
	}
}

// Cold start: an empty tracker is total 0 and an empty list, never null.
func TestAPITrackedEmpty(t *testing.T) {
	h := newAPIHarness(t, apiTestHash())
	h.b.tracked = nil
	w := h.do(apiGet("/api/tracked"))
	if w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != `{"total":0,"rows":[]}` {
		t.Errorf("%d %s", w.Code, w.Body)
	}
}

// Tracked's own refusal is a bad request here, not the 422 the track route answers.
func TestAPITrackedRefusalIs400(t *testing.T) {
	h := newAPIHarness(t, apiTestHash())
	h.b.trackedErr = fmt.Errorf("%w: status must be one of these", ErrUserFixable)
	if msg := wantAPIError(t, h.do(apiGet("/api/tracked")), http.StatusBadRequest); msg != "status must be one of these" {
		t.Errorf("message %q", msg)
	}
	h.b.trackedErr = errors.New("database at db.internal gone")
	w := h.do(apiGet("/api/tracked"))
	if strings.Contains(w.Body.String(), "db.internal") {
		t.Errorf("the detail leaked: %s", w.Body)
	}
	wantAPIError(t, w, http.StatusInternalServerError)
}

func TestAPITrack(t *testing.T) {
	allowed := map[string]string{"applied": "new shortlisted applied", "skip": "new shortlisted skipped", "maybe": ""}
	for _, action := range apiActions {
		h := newAPIHarness(t, apiTestHash())
		h.b.out = TrackOutcome{From: "new", To: "applied", NoOp: true, Noted: true, Message: "Already applied."}
		w := h.do(apiPost("/api/jobs/42/track", `{"action":"`+action+`","reason":"note.md"}`))
		raw := w.Body.String()
		got := apiDecode[APITrackOutcome](t, w)
		if want := (APITrackOutcome{From: "new", To: "applied", NoOp: true, Noted: true, Message: "Already applied."}); got != want {
			t.Errorf("%s: got %+v", action, got)
		}
		for _, field := range []string{"from", "to", "no_op", "noted", "message"} {
			if !strings.Contains(raw, `"`+field+`":`) {
				t.Errorf("no %s in %s", field, raw)
			}
		}
		if want := (TrackRequest{Ref: "job:42", Action: action, Reason: "note.md", AllowedFrom: allowed[action]}); h.b.last != want {
			t.Errorf("%s: Track got %+v", action, h.b.last)
		}
	}
	// Whitespace after the object is not trailing data, and the reason is optional.
	h := newAPIHarness(t, apiTestHash())
	if w := h.do(apiPost("/api/jobs/42/track", "{\"action\":\"skip\"}\n \n")); w.Code != http.StatusOK {
		t.Errorf("%d %s", w.Code, w.Body)
	}
}

func TestAPITrackRefusedBodies(t *testing.T) {
	for name, body := range map[string]string{
		"empty":         "",
		"not json":      "action=applied",
		"null":          "null",
		"array":         `["applied"]`,
		"no action":     `{"reason":"x"}`,
		"blank action":  `{"action":""}`,
		"offer":         `{"action":"offer"}`,
		"shortlist":     `{"action":"shortlist"}`,
		"withdraw":      `{"action":"withdraw"}`,
		"upper case":    `{"action":"APPLIED"}`,
		"unknown field": `{"action":"applied","extra":1}`,
		"rule field":    `{"action":"skip","rule_field":"company"}`,
		"rule pattern":  `{"action":"skip","rule_pattern":"x"}`,
		"ref":           `{"action":"applied","ref":"job:1"}`,
		"trailing":      `{"action":"applied"}{"action":"skip"}`,
		"trailing junk": `{"action":"applied"} x`,
		"wrong type":    `{"action":1}`,
		"truncated":     `{"action":"applied"`,
	} {
		t.Run(name, func(t *testing.T) {
			h := newAPIHarness(t, apiTestHash())
			wantAPIError(t, h.do(apiPost("/api/jobs/42/track", body)), http.StatusBadRequest)
			if n := h.b.total(); n != 0 {
				t.Errorf("backend called %d times", n)
			}
		})
	}
}

func TestAPITrackBadID(t *testing.T) {
	for _, id := range []string{"abc", "0", "-1", "99999999999999999999999"} {
		h := newAPIHarness(t, apiTestHash())
		wantAPIError(t, h.do(apiPost("/api/jobs/"+id+"/track", `{"action":"applied"}`)), http.StatusNotFound)
		if n := h.b.total(); n != 0 {
			t.Errorf("%s: backend called %d times", id, n)
		}
	}
}

// Over the 64 KiB cap is JSON 413, whatever the body holds, and the backend is never asked.
func TestAPITrackBodyOverCap(t *testing.T) {
	for name, body := range map[string]string{
		"valid json": `{"action":"applied","reason":"` + strings.Repeat("x", maxBody) + `"}`,
		"junk":       strings.Repeat("x", maxBody+1),
	} {
		t.Run(name, func(t *testing.T) {
			h := newAPIHarness(t, apiTestHash())
			wantAPIError(t, h.do(apiPost("/api/jobs/42/track", body)), http.StatusRequestEntityTooLarge)
			if n := h.b.total(); n != 0 {
				t.Errorf("backend called %d times", n)
			}
		})
	}
	// Just under the cap reaches the backend, which judges the reason's length.
	h := newAPIHarness(t, apiTestHash())
	prefix, suffix := `{"action":"applied","reason":"`, `"}`
	if w := h.do(apiPost("/api/jobs/42/track", prefix+strings.Repeat("x", maxBody-len(prefix)-len(suffix))+suffix)); w.Code != http.StatusOK {
		t.Errorf("at the cap: %d", w.Code)
	}
}

func TestAPITrackOutcomes(t *testing.T) {
	for _, c := range []struct {
		name string
		err  error
		code int
		msg  string
	}{
		{"a refusal", fmt.Errorf("%w: it was already applied to", ErrUserFixable), http.StatusUnprocessableEntity, "it was already applied to"},
		{"no such job", ErrNotFound, http.StatusNotFound, "There is no job 42."},
		{"a wrapped no such job", fmt.Errorf("track: %w", ErrNotFound), http.StatusNotFound, "There is no job 42."},
		{"a failure", errors.New("database at db.internal gone"), http.StatusInternalServerError, serverFailed},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newAPIHarness(t, apiTestHash())
			h.b.err = c.err
			if msg := wantAPIError(t, h.do(apiPost("/api/jobs/42/track", `{"action":"skip","reason":"private-reason-words"}`)), c.code); msg != c.msg {
				t.Errorf("message %q, want %q", msg, c.msg)
			}
		})
	}
}

// The reason is never logged, as the console's own mark never logs it.
func TestAPITrackLogsNoReason(t *testing.T) {
	a, _, _ := testAuth(t)
	logs := &syncBuffer{}
	b := newAPIFake()
	h := New(b, a, Options{Location: time.UTC, Logger: slog.New(slog.NewTextHandler(logs, nil)), APITokenSHA256: apiTestHash()})
	for _, err := range []error{nil, fmt.Errorf("%w: no", ErrUserFixable)} {
		b.err = err
		h.ServeHTTP(httptest.NewRecorder(), apiPost("/api/jobs/42/track", `{"action":"skip","reason":"private-reason-words"}`))
	}
	if got := logs.String(); !strings.Contains(got, "console api track") || strings.Contains(got, "private-reason-words") {
		t.Errorf("log: %s", got)
	}
}

// A browser's cross-site POST is refused even with the token; a client with no such header is not
// a browser and passes.
func TestAPICrossOrigin(t *testing.T) {
	const body = `{"action":"applied"}`
	for _, site := range []string{"cross-site", "same-site"} {
		h := newAPIHarness(t, apiTestHash())
		r := apiPost("/api/jobs/42/track", body)
		r.Header.Set("Sec-Fetch-Site", site)
		wantAPIError(t, h.do(r), http.StatusForbidden)
		if n := h.b.total(); n != 0 {
			t.Errorf("%s: backend called %d times", site, n)
		}
	}
	h := newAPIHarness(t, apiTestHash())
	r := apiPost("/api/jobs/42/track", body)
	r.Header.Set("Origin", "https://evil.example")
	wantAPIError(t, h.do(r), http.StatusForbidden)

	r = apiRequest(http.MethodPost, "/api/jobs/42/track", body, "")
	r.Header.Set("Sec-Fetch-Site", "cross-site")
	wantAPIError(t, h.do(r), http.StatusUnauthorized)
	if n := h.b.total(); n != 0 {
		t.Errorf("backend called %d times", n)
	}

	if w := h.do(apiPost("/api/jobs/42/track", body)); w.Code != http.StatusOK {
		t.Errorf("no header: %d %s", w.Code, w.Body)
	}
	r = apiPost("/api/jobs/42/track", body)
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	if w := h.do(r); w.Code != http.StatusOK {
		t.Errorf("same-origin: %d %s", w.Code, w.Body)
	}
}
