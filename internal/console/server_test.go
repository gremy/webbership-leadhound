package console

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// htmxSHA256 is the SHA-256 recorded next to the embed in server.go.
const htmxSHA256 = "d6fdc75f204e6bdefa99b69bf1e6d4ac69b8a364f77929f45c13476b4000f717"

// fakeBackend returns canned data and counts every call.
type fakeBackend struct {
	mu      sync.Mutex
	calls   map[string]int
	last    TrackRequest
	lastDay string // the day the last BriefJobs call asked for

	dash    Dashboard
	brief   BriefJobs
	tracked []Tracked
	matched int // TrackedList.Total; len(tracked) when 0
	runs    []RunRow
	rules   []RuleRow
	out     TrackOutcome
	err     error // returned by Track
	readErr error // returned by every read

	job       JobPage
	ruleMsg   string
	ruleErr   error // returned by AddRule and DisableRule
	lastRule  RuleRequest
	leads     LeadsPage
	batch     LeadBatch
	settings  LeadSettingsPage
	saveErr   error // returned by SaveLeadSettings
	saves     []settingsSave
	leadOut   LeadOutcome
	leadErr   error // returned by LeadAct
	leadsErr  error // returned by every leads read, after readErr
	lastLead  LeadAction
	list      LeadListPage
	listQ     LeadListQuery // the query the last LeadList call asked for
	detail    LeadDetailPage
	campaigns []CampaignView
	report    LeadReport
	reportErr error // returned by LeadReport, after the leads read error

	today       TodayView
	todayQ      string // the campaign the last LeadsToday call asked for
	call        CallCard
	seq         []SequenceStep
	outcomeMsg  string
	outcomeErr  error // returned by LeadCallOutcome
	lastOutcome outcomeCall

	drafts     DraftsPage
	draftsQ    []string // the campaign each DraftSamples call asked for
	draftsErr  error    // returned by DraftSamples for a campaign other than ""
	reviewErr  error    // returned by ReviewDraft
	lastReview reviewCall
	gates      map[string]DraftGate

	runOut     RunOutcome
	runErr     error // returned by RunNow
	lastRun    RunRequest
	runCard    RunNowCard
	results    Results
	resultsErr error // returned by Results, after readErr

	mailboxes   MailboxesPage
	mailbox     MailboxPage
	mbCard      MailboxCheckView
	mbOut       MailboxOutcome
	mbErr       error // returned by MailboxAct
	lastMailbox MailboxAction

	controls    ControlsPage
	sendingWhy  SendingWhy
	whyErr      error // returned by SendingWhy before any leads read error
	ctlOut      ControlOutcome
	ctlErr      error // returned by ControlAct
	sendNowOut  ControlOutcome
	sendNowErr  error // returned by SendNow
	lastControl ControlAction
	campaignMbs []CampaignMailboxes
	mbSending   MailboxSendingView
}

func (f *fakeBackend) count(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.calls == nil {
		f.calls = make(map[string]int)
	}
	f.calls[name]++
}

func (f *fakeBackend) n(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[name]
}

func (f *fakeBackend) total() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	t := 0
	for _, n := range f.calls {
		t += n
	}
	return t
}

func (f *fakeBackend) Dashboard(context.Context) (Dashboard, error) {
	f.count("Dashboard")
	return f.dash, f.readErr
}

func (f *fakeBackend) BriefJobs(_ context.Context, day string) (BriefJobs, error) {
	f.count("BriefJobs")
	f.mu.Lock()
	f.lastDay = day
	f.mu.Unlock()
	return f.brief, f.readErr
}

func (f *fakeBackend) Tracked(context.Context, string) (TrackedList, error) {
	f.count("Tracked")
	return TrackedList{Rows: f.tracked, Total: max(f.matched, len(f.tracked))}, f.readErr
}

func (f *fakeBackend) Runs(context.Context, int) ([]RunRow, error) {
	f.count("Runs")
	return f.runs, f.readErr
}

func (f *fakeBackend) Rules(context.Context) ([]RuleRow, error) {
	f.count("Rules")
	return f.rules, f.readErr
}

func (f *fakeBackend) Track(_ context.Context, req TrackRequest) (TrackOutcome, error) {
	f.count("Track")
	f.mu.Lock()
	f.last = req
	f.mu.Unlock()
	return f.out, f.err
}

func (f *fakeBackend) RunNow(_ context.Context, req RunRequest) (RunOutcome, error) {
	f.count("RunNow")
	f.mu.Lock()
	f.lastRun = req
	f.mu.Unlock()
	return f.runOut, f.runErr
}

func (f *fakeBackend) RunNowCard(context.Context) (RunNowCard, error) {
	f.count("RunNowCard")
	return f.runCard, f.readErr
}

func (f *fakeBackend) Mailboxes(context.Context) (MailboxesPage, error) {
	f.count("Mailboxes")
	return f.mailboxes, f.leadsRead()
}

func (f *fakeBackend) Mailbox(_ context.Context, id int64) (MailboxPage, error) {
	f.count("Mailbox")
	if err := f.leadsRead(); err != nil {
		return MailboxPage{}, err
	}
	if id != f.mailbox.Row.ID {
		return MailboxPage{}, ErrNotFound
	}
	return f.mailbox, nil
}

func (f *fakeBackend) MailboxCheckCard(_ context.Context, id int64) (MailboxCheckView, error) {
	f.count("MailboxCheckCard")
	if err := f.leadsRead(); err != nil {
		return MailboxCheckView{}, err
	}
	if id != f.mbCard.MailboxID {
		return MailboxCheckView{}, ErrNotFound
	}
	return f.mbCard, nil
}

func (f *fakeBackend) MailboxAct(_ context.Context, a MailboxAction) (MailboxOutcome, error) {
	f.count("MailboxAct")
	f.mu.Lock()
	f.lastMailbox = a
	f.mu.Unlock()
	return f.mbOut, f.mbErr
}

func (f *fakeBackend) Controls(context.Context) (ControlsPage, error) {
	f.count("Controls")
	return f.controls, f.leadsRead()
}

func (f *fakeBackend) SendingWhy(context.Context) (SendingWhy, error) {
	f.count("SendingWhy")
	if f.whyErr != nil {
		return SendingWhy{}, f.whyErr
	}
	return f.sendingWhy, f.leadsRead()
}

func (f *fakeBackend) ControlAct(_ context.Context, a ControlAction) (ControlOutcome, error) {
	f.count("ControlAct")
	f.mu.Lock()
	f.lastControl = a
	f.mu.Unlock()
	return f.ctlOut, f.ctlErr
}

func (f *fakeBackend) SendNow(context.Context) (ControlOutcome, error) {
	f.count("SendNow")
	return f.sendNowOut, f.sendNowErr
}

func (f *fakeBackend) CampaignMailboxes(context.Context) ([]CampaignMailboxes, error) {
	f.count("CampaignMailboxes")
	return f.campaignMbs, f.leadsRead()
}

func (f *fakeBackend) MailboxSending(_ context.Context, id int64) (MailboxSendingView, error) {
	f.count("MailboxSending")
	if err := f.leadsRead(); err != nil {
		return MailboxSendingView{}, err
	}
	if id != f.mbSending.MailboxID {
		return MailboxSendingView{}, ErrNotFound
	}
	return f.mbSending, nil
}

func (f *fakeBackend) Results(context.Context) (Results, error) {
	f.count("Results")
	if f.readErr != nil {
		return Results{}, f.readErr
	}
	return f.results, f.resultsErr
}

var (
	testDay  = time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC)
	testSent = time.Date(2026, 10, 24, 4, 50, 0, 0, time.UTC)
)

// richBackend is a backend with something on every page, every text field set to text.
func richBackend(text string) *fakeBackend {
	finished := testSent.Add(95 * time.Second)
	from := int64(7)
	sent := testSent
	job := BriefJob{N: 1, JobID: 42, Title: text, Company: text, Status: "shortlisted", ListingStatus: "open", URL: "https://jobs.example/42"}
	return &fakeBackend{
		dash: Dashboard{
			AsOf: testSent, Day: testDay,
			Health:   []string{"Health: " + text},
			ApplyNow: 2, YourCallNow: 3, Unscored: 1, Unreachable: 0,
			Funnel:    Funnel{NewBySource: map[string]int{text: 4}, Gated: 3, GateFailed: 1, Scored: 2},
			JobsCalls: 120, JobsCallCap: 300, LeadsCalls: 1400, LeadsCallCap: 10000, AIInputTokens: 2_700_000, AICostUSD: 0.12,
			Boards:    []BoardCount{{ATS: text, State: text, N: 5}},
			LastRuns:  []RunRow{{ID: 1, Command: text, Source: text, StartedAt: testSent, FinishedAt: &finished, StopReason: text, Note: text}},
			LastBrief: &BriefStamp{Day: testDay, SentAt: testSent, Listed: 1, Health: []string{"Health: " + text}},
			RunNow:    richCard(text),
		},
		brief:   BriefJobs{Day: testDay, SentAt: &sent, Jobs: []BriefJob{job}},
		tracked: []Tracked{{JobID: 42, Title: text, Company: text, Status: "applied", Reason: text, Since: testSent}},
		runs:    []RunRow{{ID: 1, Command: text, Source: text, StartedAt: testSent, FinishedAt: &finished, StopReason: text, Note: text}},
		rules:   []RuleRow{{ID: 7, Kind: "skip", Field: "company", Pattern: text, Reason: text, FromJobID: &from, CreatedAt: testSent, DisabledReason: text}},
		out: TrackOutcome{
			Job:  BriefJob{N: 1, JobID: 42, Title: "Go Engineer", Company: "Acme", Status: "applied", URL: "https://jobs.example/42"},
			From: "shortlisted", To: "applied", Message: "shortlisted → applied",
		},
		job:       richJob(text),
		ruleMsg:   "rule 8 added",
		leads:     richLeads(text),
		batch:     richBatch(text),
		settings:  richSettings(text),
		leadOut:   LeadOutcome{Message: text},
		list:      richLeadList(text),
		detail:    richLeadDetail(text),
		campaigns: richCampaigns(text),
		today:     richToday(text),
		call:      richCall(text),
		seq:       richSequence(),
		drafts:    richDrafts(text),
		gates:     map[string]DraftGate{"drupal-eol": {Summary: "Claude drafts: not trusted yet: 14 of 20 reviewed, 86% approved"}},
		runOut:    RunOutcome{Message: text},
		runCard:   richCard(text),
		results:   richResults(text),

		mailboxes: richMailboxes(text),
		mailbox:   richMailbox(text),
		mbCard:    passedCard(),
		mbOut:     MailboxOutcome{Message: text, MailboxID: 1},
	}
}

type harness struct {
	h     http.Handler
	b     *fakeBackend
	a     *Auth
	clock *fakeClock
	calls *int // bcrypt comparisons
	logs  *syncBuffer
	name  string // the session cookie's name
}

// syncBuffer is a bytes.Buffer safe for the logger and the test to share.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func newHarness(t *testing.T, b *fakeBackend, dev bool) *harness {
	t.Helper()
	a, clock, calls := testAuth(t)
	logs := &syncBuffer{}
	h := New(b, a, Options{Dev: dev, Location: time.UTC, Logger: slog.New(slog.NewTextHandler(logs, nil))})
	name := "__Host-lh_session"
	if dev {
		name = "lh_session"
	}
	return &harness{h: h, b: b, a: a, clock: clock, calls: calls, logs: logs, name: name}
}

// session starts a session directly and returns its cookie.
func (h *harness) session(t *testing.T) *http.Cookie {
	t.Helper()
	tok, err := h.a.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	return h.cookie(tok)
}

// cookie is the session cookie as a browser would send it.
func (h *harness) cookie(token string) *http.Cookie {
	return &http.Cookie{Name: h.name, Value: token, Path: "/", HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode}
}

func (h *harness) do(r *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.h.ServeHTTP(w, r)
	return w
}

func get(target string, cookies ...*http.Cookie) *http.Request {
	r := httptest.NewRequest(http.MethodGet, target, nil)
	for _, c := range cookies {
		r.AddCookie(c)
	}
	return r
}

func post(target string, form url.Values, cookies ...*http.Cookie) *http.Request {
	r := httptest.NewRequest(http.MethodPost, target, strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for _, c := range cookies {
		r.AddCookie(c)
	}
	return r
}

func hx(r *http.Request) *http.Request {
	r.Header.Set("HX-Request", "true")
	return r
}

func cookieNamed(w *httptest.ResponseRecorder, name string) *http.Cookie {
	for _, c := range w.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// trackForm is a valid action on job 1 of the brief of testDay.
func trackForm() url.Values {
	return url.Values{"ref": {"1"}, "day": {"2026-10-24"}, "action": {"applied"}}
}

func requestFor(rt route, form url.Values, cookies ...*http.Cookie) *http.Request {
	if strings.HasPrefix(rt.pattern, "POST ") {
		return post(rt.sample, form, cookies...)
	}
	return get(rt.sample, cookies...)
}

func TestRoutesNeedSession(t *testing.T) {
	for _, rt := range routes {
		if rt.public {
			continue
		}
		t.Run(rt.pattern, func(t *testing.T) {
			h := newHarness(t, richBackend("x"), false)
			stale := h.cookie(strings.Repeat("ab", 32))
			for _, cookies := range [][]*http.Cookie{nil, {stale}} {
				w := h.do(requestFor(rt, trackForm(), cookies...))
				if strings.HasPrefix(rt.pattern, "GET ") {
					if w.Code != http.StatusSeeOther {
						t.Fatalf("GET without a session: %d, want 303", w.Code)
					}
					want := "/login?next=" + url.QueryEscape(rt.sample)
					if got := w.Header().Get("Location"); got != want {
						t.Errorf("Location %q, want %q", got, want)
					}
				} else if w.Code != http.StatusUnauthorized {
					t.Fatalf("POST without a session: %d, want 401", w.Code)
				}
				w = h.do(hx(requestFor(rt, trackForm(), cookies...)))
				if w.Code != http.StatusUnauthorized || w.Header().Get("HX-Redirect") != "/login" {
					t.Errorf("htmx without a session: %d HX-Redirect %q, want 401 /login", w.Code, w.Header().Get("HX-Redirect"))
				}
			}
			if n := h.b.total(); n != 0 {
				t.Errorf("the backend was called %d times without a session", n)
			}
		})
	}
}

func TestPublicRoutes(t *testing.T) {
	want := map[string]bool{"GET /login": true, "POST /login": true, "POST /theme": true, "GET /static/": true, "GET /robots.txt": true, "GET /favicon.ico": true, "GET /healthz": true}
	for _, rt := range routes {
		if rt.public != want[rt.pattern] {
			t.Errorf("%s public = %v, want %v", rt.pattern, rt.public, want[rt.pattern])
		}
	}
}

// everyResponse drives the console to every route signed in and signed out, and
// to each status code the console answers with, and returns what came back.
func everyResponse(t *testing.T, dev bool) map[string]*httptest.ResponseRecorder {
	t.Helper()
	out := make(map[string]*httptest.ResponseRecorder)
	for _, rt := range routes {
		h := newHarness(t, richBackend("x"), dev)
		out["signed out "+rt.pattern] = h.do(requestFor(rt, trackForm()))
		out["signed in "+rt.pattern] = h.do(requestFor(rt, trackForm(), h.session(t)))
	}

	h := newHarness(t, richBackend("x"), dev)
	c := h.session(t)
	out["404"] = h.do(get("/nope", c))
	out["404 static"] = h.do(get("/static/nope.css"))
	out["405"] = h.do(httptest.NewRequest(http.MethodDelete, "/jobs", nil))
	out["401 login"] = h.do(post("/login", url.Values{"email": {testEmail}, "password": {"wrong password!"}}))
	out["303 login"] = h.do(post("/login", url.Values{"email": {testEmail}, "password": {testPassword}}))
	out["401 hx"] = h.do(hx(get("/jobs")))
	big := post("/jobs/track", url.Values{"reason": {strings.Repeat("a", maxBody)}}, c)
	out["413"] = h.do(big)
	cross := post("/jobs/track", trackForm(), c)
	cross.Header.Set("Sec-Fetch-Site", "cross-site")
	out["403"] = h.do(cross)
	for range 6 {
		out["429"] = h.do(post("/login", url.Values{"email": {"x"}, "password": {"y"}}))
	}

	h = newHarness(t, richBackend("x"), dev)
	h.b.err = fmt.Errorf("%w: already applied; use withdrawn", ErrUserFixable)
	out["422"] = h.do(post("/jobs/track", trackForm(), h.session(t)))
	h.b.err = errors.New("database gone")
	out["500"] = h.do(post("/jobs/track", trackForm(), h.session(t)))
	return out
}

func TestSecurityHeadersEverywhere(t *testing.T) {
	for _, dev := range []bool{false, true} {
		got := everyResponse(t, dev)
		for _, code := range []string{"404", "405", "401 login", "303 login", "401 hx", "413", "403", "429", "422", "500"} {
			if got[code] == nil {
				t.Fatalf("missing case %s", code)
			}
		}
		for name, w := range got {
			t.Run(fmt.Sprintf("dev=%v %s %d", dev, name, w.Code), func(t *testing.T) {
				h := w.Header()
				want := map[string]string{
					"Content-Security-Policy": contentSecurityPolicy,
					"Referrer-Policy":         "no-referrer",
					"X-Content-Type-Options":  "nosniff",
					"X-Frame-Options":         "DENY",
					"X-Robots-Tag":            "noindex, nofollow",
				}
				for k, v := range want {
					if h.Get(k) != v {
						t.Errorf("%s = %q, want %q", k, h.Get(k), v)
					}
				}
				hsts := h.Get("Strict-Transport-Security")
				if dev && hsts != "" {
					t.Errorf("dev mode sent HSTS %q", hsts)
				}
				if !dev && hsts != "max-age=31536000" {
					t.Errorf("HSTS = %q", hsts)
				}
				static := strings.Contains(name, "/static/") && w.Code == http.StatusOK
				cc := h.Get("Cache-Control")
				if static && cc != "public, max-age=86400" {
					t.Errorf("static Cache-Control = %q", cc)
				}
				if !static && cc != "no-store" {
					t.Errorf("Cache-Control = %q, want no-store", cc)
				}
			})
		}
	}
	if !strings.Contains(contentSecurityPolicy, "script-src 'self'") || strings.Contains(contentSecurityPolicy, "unsafe") {
		t.Errorf("CSP %q", contentSecurityPolicy)
	}
}

func TestNoWriteOnGet(t *testing.T) {
	h := newHarness(t, richBackend("x"), false)
	c := h.session(t)
	for _, rt := range routes {
		if !strings.HasPrefix(rt.pattern, "GET ") {
			continue
		}
		r := get(rt.sample+"?ref=1&action=applied", c)
		h.do(r)
		h.do(hx(get(rt.sample+"?ref=1&action=applied", c)))
	}
	if n := h.b.n("Track"); n != 0 {
		t.Errorf("a GET called Track %d times", n)
	}
	if n := h.b.n("RunNow"); n != 0 {
		t.Errorf("a GET called RunNow %d times", n)
	}
}

func TestCrossOriginPostRefused(t *testing.T) {
	for _, rt := range routes {
		if !strings.HasPrefix(rt.pattern, "POST ") {
			continue
		}
		for _, signedIn := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s signed in %v", rt.pattern, signedIn), func(t *testing.T) {
				h := newHarness(t, richBackend("x"), false)
				var cookies []*http.Cookie
				if signedIn {
					cookies = append(cookies, h.session(t))
				}
				form := trackForm()
				form.Set("email", testEmail)
				form.Set("password", testPassword)
				r := requestFor(rt, form, cookies...)
				r.Header.Set("Sec-Fetch-Site", "cross-site")
				w := h.do(r)
				if w.Code != http.StatusForbidden {
					t.Errorf("status %d, want 403", w.Code)
				}
				if n := h.b.total(); n != 0 {
					t.Errorf("backend called %d times", n)
				}
				if *h.calls != 0 {
					t.Errorf("the password was checked %d times", *h.calls)
				}
				if c := cookieNamed(w, h.name); c != nil && c.Value != "" {
					t.Error("a cross-site login set a session")
				}
			})
		}
	}
}

func TestLoginSuccess(t *testing.T) {
	h := newHarness(t, richBackend("x"), false)
	old := h.session(t)
	w := h.do(post("/login", url.Values{"email": {testEmail}, "password": {testPassword}, "next": {"/jobs?x=1"}}, old))
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/jobs?x=1" {
		t.Fatalf("got %d to %q, want 303 to /jobs?x=1", w.Code, w.Header().Get("Location"))
	}
	c := cookieNamed(w, "__Host-lh_session")
	if c == nil {
		t.Fatal("no session cookie")
	}
	if !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteStrictMode || c.Path != "/" || c.Domain != "" {
		t.Errorf("cookie attributes: %+v", c)
	}
	if c.MaxAge != 12*60*60 {
		t.Errorf("MaxAge %d, want 12h", c.MaxAge)
	}
	if !h.a.Valid(c.Value) {
		t.Error("the new session is not valid")
	}
	dc := cookieNamed(w, "__Host-lh_device")
	if dc == nil || !h.a.TrustedDevice(dc.Value) {
		t.Fatalf("no trusted device cookie: %+v", dc)
	}
	if !dc.HttpOnly || !dc.Secure || dc.SameSite != http.SameSiteStrictMode || dc.Path != "/" || dc.Domain != "" || dc.MaxAge != int(deviceTTL/time.Second) {
		t.Errorf("device cookie attributes: %+v", dc)
	}
	if h.a.Valid(old.Value) {
		t.Error("the session the request carried was not revoked")
	}
	logs := h.logs.String()
	if !strings.Contains(logs, "console login") || !strings.Contains(logs, "outcome=ok") || !strings.Contains(logs, "ip=192.0.2.1") || !strings.Contains(logs, "trusted=false") {
		t.Errorf("log %q lacks the login line", logs)
	}
	if strings.Contains(logs, testEmail) || strings.Contains(logs, testPassword) {
		t.Errorf("log %q names the email or password", logs)
	}

	// Signed in, the sign-in page goes on to next.
	w = h.do(get("/login?next=/runs", c))
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/runs" {
		t.Errorf("signed-in GET /login: %d to %q", w.Code, w.Header().Get("Location"))
	}
}

func TestLoginDevCookie(t *testing.T) {
	h := newHarness(t, richBackend("x"), true)
	w := h.do(post("/login", url.Values{"email": {testEmail}, "password": {testPassword}}))
	c := cookieNamed(w, "lh_session")
	if c == nil || c.Secure || !c.HttpOnly || c.SameSite != http.SameSiteStrictMode {
		t.Fatalf("dev cookie: %+v", c)
	}
	if cookieNamed(w, "__Host-lh_session") != nil || cookieNamed(w, "__Host-lh_device") != nil {
		t.Error("dev mode set a __Host- cookie")
	}
	dc := cookieNamed(w, "lh_device")
	if dc == nil || dc.Secure || !dc.HttpOnly || dc.SameSite != http.SameSiteStrictMode || dc.Path != "/" || !h.a.TrustedDevice(dc.Value) {
		t.Errorf("dev device cookie: %+v", dc)
	}
}

func TestLoginFailures(t *testing.T) {
	tests := []struct {
		name string
		form url.Values
	}{
		{"wrong email", url.Values{"email": {"someone@mail.example"}, "password": {testPassword}}},
		{"wrong password", url.Values{"email": {testEmail}, "password": {"not the password"}}},
		{"empty fields", url.Values{"email": {""}, "password": {""}}},
		{"no fields", url.Values{}},
	}
	var bodies []string
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, richBackend("x"), false)
			w := h.do(post("/login", tt.form))
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("status %d, want 401", w.Code)
			}
			if c := cookieNamed(w, h.name); c != nil {
				t.Errorf("a failed login set a cookie: %+v", c)
			}
			body := w.Body.String()
			if !strings.Contains(body, "That email and password don&#39;t match.") {
				t.Errorf("body lacks the message: %s", body)
			}
			bodies = append(bodies, body)
			h.a.mu.Lock()
			failures := len(h.a.failures)
			h.a.mu.Unlock()
			if failures != 1 {
				t.Errorf("%d failures recorded, want 1", failures)
			}
			logs := h.logs.String()
			if !strings.Contains(logs, "outcome=denied") {
				t.Errorf("log %q lacks outcome=denied", logs)
			}
			if strings.Contains(logs, "someone@mail.example") || strings.Contains(logs, testPassword) || strings.Contains(logs, "not the password") {
				t.Errorf("log %q names the email or password", logs)
			}
		})
	}
	if len(bodies) >= 2 && bodies[0] != bodies[1] {
		t.Error("a wrong email and a wrong password got different pages")
	}
}

func TestLoginMalformedForm(t *testing.T) {
	h := newHarness(t, richBackend("x"), false)
	r := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader("%zz=%"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := h.do(r)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status %d, want 401", w.Code)
	}
	h.a.mu.Lock()
	defer h.a.mu.Unlock()
	if len(h.a.failures) != 1 {
		t.Errorf("%d failures recorded, want 1", len(h.a.failures))
	}
}

func TestLoginLimited(t *testing.T) {
	h := newHarness(t, richBackend("x"), false)
	form := url.Values{"email": {testEmail}, "password": {"not the password"}}
	for range 5 {
		h.do(post("/login", form))
	}
	before := *h.calls
	w := h.do(post("/login", url.Values{"email": {testEmail}, "password": {testPassword}}))
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("6th login: %d, want 429", w.Code)
	}
	if *h.calls != before {
		t.Error("a limited login was checked")
	}
	if cookieNamed(w, h.name) != nil {
		t.Error("a limited login set a cookie")
	}
	if !strings.Contains(h.logs.String(), "outcome=limited") {
		t.Error("no outcome=limited log line")
	}
	h.clock.advance(time.Minute + time.Second)
	w = h.do(post("/login", url.Values{"email": {testEmail}, "password": {testPassword}}))
	if w.Code != http.StatusSeeOther {
		t.Errorf("after the window: %d, want 303", w.Code)
	}
}

func TestLogout(t *testing.T) {
	h := newHarness(t, richBackend("x"), false)
	c := h.session(t)
	w := h.do(post("/logout", nil, c))
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/login" {
		t.Fatalf("logout: %d to %q", w.Code, w.Header().Get("Location"))
	}
	cleared := cookieNamed(w, h.name)
	if cleared == nil || cleared.MaxAge >= 0 || !cleared.Secure {
		t.Errorf("logout did not clear the cookie: %+v", cleared)
	}
	if w := h.do(get("/jobs", c)); w.Code != http.StatusSeeOther {
		t.Errorf("the old cookie still works: %d", w.Code)
	}
	if cookieNamed(w, "__Host-lh_device") != nil {
		t.Error("logout touched the device cookie")
	}
}

// TestLoginTrustedDevice: once 100 sign-ins have failed, only a device that signed in
// before (its cookie made under the current password hash) may still try.
func TestLoginTrustedDevice(t *testing.T) {
	h := newHarness(t, richBackend("x"), false)
	device, err := h.a.NewDevice()
	if err != nil {
		t.Fatal(err)
	}
	otherHash, err := bcrypt.GenerateFromPassword([]byte("another password!"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	other, err := newAuth(testEmail, string(otherHash), nil, bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	oldDevice, err := other.NewDevice()
	if err != nil {
		t.Fatal(err)
	}
	for range 100 {
		h.a.RecordFailure()
	}
	right := url.Values{"email": {testEmail}, "password": {testPassword}}
	named := func(name, v string) *http.Cookie {
		return &http.Cookie{Name: name, Value: v, Path: "/", HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode}
	}
	deviceCookie := func(v string) *http.Cookie { return named("__Host-lh_device", v) }

	for _, c := range []struct {
		name   string
		cookie *http.Cookie
	}{
		{"no device cookie", nil},
		{"forged", deviceCookie(strings.Repeat("ab", 16) + "." + strings.Repeat("cd", 32))},
		{"made under the old password", deviceCookie(oldDevice)},
		{"the dev cookie name", named("lh_device", device)},
	} {
		before := *h.calls
		r := post("/login", right)
		if c.cookie != nil {
			r.AddCookie(c.cookie)
		}
		w := h.do(r)
		if w.Code != http.StatusTooManyRequests {
			t.Errorf("%s: %d, want 429", c.name, w.Code)
		}
		if *h.calls != before {
			t.Errorf("%s: the password was checked", c.name)
		}
	}

	w := h.do(post("/login", right, deviceCookie(device)))
	if w.Code != http.StatusSeeOther || cookieNamed(w, "__Host-lh_session") == nil {
		t.Fatalf("trusted device: %d, want 303 with a session", w.Code)
	}
	if nc := cookieNamed(w, "__Host-lh_device"); nc == nil || nc.Value == device || !h.a.TrustedDevice(nc.Value) {
		t.Errorf("the sign-in did not set a fresh device cookie: %+v", nc)
	}
	logs := h.logs.String()
	if !strings.Contains(logs, "outcome=limited ip=192.0.2.1 trusted=false") || !strings.Contains(logs, "outcome=ok ip=192.0.2.1 trusted=true") {
		t.Errorf("log %q lacks trusted on the login lines", logs)
	}
	if strings.Contains(logs, device) || strings.Contains(logs, oldDevice) {
		t.Error("the log names a device cookie")
	}
}

func TestSignedOutMessage(t *testing.T) {
	h := newHarness(t, richBackend("x"), false)
	stale := h.cookie(strings.Repeat("cd", 32))
	w := h.do(get("/login?next=%2Fjobs", stale))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), signedOut) {
		t.Errorf("stale cookie: %d, body lacks %q", w.Code, signedOut)
	}
	if c := cookieNamed(w, h.name); c == nil || c.MaxAge >= 0 {
		t.Errorf("the stale cookie was not cleared: %+v", c)
	}
	if !strings.Contains(w.Body.String(), `name="next" value="/jobs"`) {
		t.Error("the form lost next")
	}
	w = h.do(get("/login"))
	if strings.Contains(w.Body.String(), signedOut) {
		t.Error("a first visit says signed out")
	}

	// An expired session reads the same way.
	c := h.session(t)
	h.clock.advance(12*time.Hour + time.Second)
	w = h.do(get("/login", c))
	if !strings.Contains(w.Body.String(), signedOut) {
		t.Error("an expired session is not told it was signed out")
	}
}

func TestSafeNext(t *testing.T) {
	srv := &server{next: map[string]bool{"/": true, "/jobs": true, "/tracked": true, "/runs": true, "/rules": true}}
	tests := []struct{ in, want string }{
		{"//evil.example", "/"},
		{`/\evil.example`, "/"},
		{"https://evil.example/", "/"},
		{"http:/jobs", "/"},
		{"/jobs", "/jobs"},
		{"/nope", "/"},
		{"/jobs?x=1", "/jobs?x=1"},
		{"/tracked?status=offer", "/tracked?status=offer"},
		{"/jobs/../runs", "/"},
		{"/jobs/track", "/"},
		{"/login", "/"},
		{"", "/"},
		{"jobs", "/"},
		{"/%2F%2Fevil.example", "/"},
	}
	for _, tt := range tests {
		if got := srv.safeNext(tt.in); got != tt.want {
			t.Errorf("safeNext(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
	// New fills the same set from the route table.
	full := &server{next: map[string]bool{}}
	for _, rt := range routes {
		if !rt.public && strings.HasPrefix(rt.pattern, "GET ") {
			full.next[rt.sample] = true
		}
	}
	for p := range srv.next {
		if !full.next[p] {
			t.Errorf("%s is not a signed-in GET route", p)
		}
	}
}

func TestTrackPlainPost(t *testing.T) {
	h := newHarness(t, richBackend("x"), false)
	c := h.session(t)
	form := url.Values{"ref": {"1"}, "day": {"2026-10-24"}, "action": {"skip"}, "reason": {"too far"}, "rule_field": {"company"}, "rule_pattern": {"acme"}}
	w := h.do(post("/jobs/track", form, c))
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/jobs#job-1" {
		t.Fatalf("got %d to %q, want 303 /jobs#job-1", w.Code, w.Header().Get("Location"))
	}
	want := TrackRequest{Ref: "1", Day: "2026-10-24", Action: "skip", Reason: "too far", RuleField: "company", RulePattern: "acme"}
	if h.b.last != want {
		t.Errorf("request %+v, want %+v", h.b.last, want)
	}
	flash := cookieNamed(w, "__Host-lh_flash")
	if flash == nil || !flash.Secure || !flash.HttpOnly {
		t.Fatalf("flash cookie %+v", flash)
	}
	logs := h.logs.String()
	if !strings.Contains(logs, "console track") || !strings.Contains(logs, "job=42") || !strings.Contains(logs, "from=shortlisted") || !strings.Contains(logs, "to=applied") {
		t.Errorf("log %q lacks the track line", logs)
	}

	w = h.do(get("/jobs", c, flash))
	if !strings.Contains(w.Body.String(), "Go Engineer: shortlisted → applied") {
		t.Errorf("the jobs page does not show the result: %s", w.Body.String())
	}
	if cl := cookieNamed(w, "__Host-lh_flash"); cl == nil || cl.MaxAge >= 0 {
		t.Error("the flash was not cleared")
	}
}

func TestTrackHX(t *testing.T) {
	h := newHarness(t, richBackend("x"), false)
	w := h.do(hx(post("/jobs/track", trackForm(), h.session(t))))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	body := w.Body.String()
	if strings.Contains(body, "<html") {
		t.Error("an htmx answer carried the whole page")
	}
	for _, want := range []string{`id="job-1"`, `status-applied`, "shortlisted → applied", "Go Engineer"} {
		if !strings.Contains(body, want) {
			t.Errorf("row lacks %q: %s", want, body)
		}
	}
}

func TestTrackDoubleSubmit(t *testing.T) {
	b := richBackend("x")
	b.out = TrackOutcome{Job: BriefJob{N: 1, JobID: 42, Title: "Go Engineer", Status: "applied"}, From: "applied", To: "applied", NoOp: true, Message: "already applied; nothing changed"}
	h := newHarness(t, b, false)
	c := h.session(t)
	w := h.do(hx(post("/jobs/track", trackForm(), c)))
	if !strings.Contains(w.Body.String(), "already applied; nothing changed") {
		t.Errorf("htmx: %s", w.Body.String())
	}
	w = h.do(post("/jobs/track", trackForm(), c))
	w = h.do(get("/jobs", c, cookieNamed(w, "__Host-lh_flash")))
	if !strings.Contains(w.Body.String(), "already applied; nothing changed") {
		t.Errorf("plain: %s", w.Body.String())
	}
}

func TestTrackUserFixable(t *testing.T) {
	const words = "already applied; use withdrawn"
	b := richBackend("x")
	b.err = fmt.Errorf("%w: %s", ErrUserFixable, words)
	b.out = TrackOutcome{}
	h := newHarness(t, b, false)
	c := h.session(t)

	w := h.do(hx(post("/jobs/track", trackForm(), c)))
	if w.Code != http.StatusOK {
		t.Errorf("htmx refusal: %d, want 200", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{words, `id="job-1"`, "status-shortlisted"} {
		if !strings.Contains(body, want) {
			t.Errorf("htmx refusal lacks %q: %s", want, body)
		}
	}
	if b.lastDay != "2026-10-24" {
		t.Errorf("the refused row came from the brief of %q, want 2026-10-24", b.lastDay)
	}

	w = h.do(post("/jobs/track", trackForm(), c))
	if w.Code != http.StatusUnprocessableEntity {
		t.Errorf("plain refusal: %d, want 422", w.Code)
	}
	if body := w.Body.String(); !strings.Contains(body, words) || !strings.Contains(body, `id="job-1"`) {
		t.Errorf("plain refusal is not the jobs page with the message: %s", body)
	}
}

// TestTrackCarriesDay: every form of the Jobs page names the brief's day, the day
// reaches the backend, and the row an htmx success shows keeps it.
func TestTrackCarriesDay(t *testing.T) {
	h := newHarness(t, richBackend("x"), false)
	c := h.session(t)
	body := h.do(get("/jobs", c)).Body.String()
	forms, days := strings.Count(body, "<form method=\"post\" action=\"/jobs/track\""), strings.Count(body, `<input type="hidden" name="day" value="2026-10-24">`)
	if forms != 3 || days != forms {
		t.Errorf("%d track forms, %d with the brief's day: %s", forms, days, body)
	}
	if h.b.lastDay != "" {
		t.Errorf("the Jobs page asked for the brief of %q, want the latest", h.b.lastDay)
	}

	w := h.do(hx(post("/jobs/track", trackForm(), c)))
	if h.b.last.Day != "2026-10-24" || h.b.last.Ref != "1" {
		t.Errorf("request %+v, want ref 1 of 2026-10-24", h.b.last)
	}
	if n := strings.Count(w.Body.String(), `name="day" value="2026-10-24"`); n != 3 {
		t.Errorf("the updated row has %d forms with the day: %s", n, w.Body.String())
	}

	form := trackForm()
	form.Del("day")
	h.do(post("/jobs/track", form, c))
	if h.b.last.Day != "" {
		t.Errorf("no day posted, got %q", h.b.last.Day)
	}
}

// TestTrackRefusedRowGone: a refused htmx action whose row is not in that day's brief
// reloads the page instead of showing an empty row; a plain form gets the Jobs page.
func TestTrackRefusedRowGone(t *testing.T) {
	const words = "the brief of 2026-10-24 listed 1 jobs; there is no number 9"
	b := richBackend("x")
	b.err = fmt.Errorf("%w: %s", ErrUserFixable, words)
	b.out = TrackOutcome{}
	h := newHarness(t, b, false)
	c := h.session(t)
	form := trackForm()
	form.Set("ref", "9")

	w := h.do(hx(post("/jobs/track", form, c)))
	if w.Code != http.StatusOK || w.Header().Get("HX-Refresh") != "true" {
		t.Errorf("htmx: %d, HX-Refresh %q; want 200 and true", w.Code, w.Header().Get("HX-Refresh"))
	}
	if strings.Contains(w.Body.String(), "job-0") || strings.Contains(w.Body.String(), "<article") {
		t.Errorf("htmx answered with a row: %s", w.Body.String())
	}
	w = h.do(get("/jobs", c, cookieNamed(w, "__Host-lh_flash")))
	if !strings.Contains(w.Body.String(), words) {
		t.Errorf("the reloaded page lacks the refusal: %s", w.Body.String())
	}

	b.brief = BriefJobs{} // that day's brief is gone, or was never sent
	form.Set("ref", "1")
	if w := h.do(hx(post("/jobs/track", form, c))); w.Header().Get("HX-Refresh") != "true" {
		t.Errorf("no brief for the day: HX-Refresh %q", w.Header().Get("HX-Refresh"))
	}

	w = h.do(post("/jobs/track", form, c))
	if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), words) || !strings.Contains(w.Body.String(), "<html") {
		t.Errorf("plain: %d %s", w.Code, w.Body.String())
	}
}

// TestSkipPatternHint: the skip form says what the pattern is, tied to its input.
func TestSkipPatternHint(t *testing.T) {
	h := newHarness(t, richBackend("x"), false)
	body := h.do(get("/jobs", h.session(t))).Body.String()
	for _, want := range []string{
		`<input class="input" id="skip-pattern-1" name="rule_pattern" maxlength="500" aria-describedby="skip-pattern-hint-1">`,
		`<p id="skip-pattern-hint-1" class="hint">A regular expression; case is ignored.</p>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("jobs page lacks %s", want)
		}
	}
}

func TestTrackFromTracked(t *testing.T) {
	b := richBackend("x")
	b.out = TrackOutcome{Job: BriefJob{JobID: 42, Title: "Go Engineer", Status: "offer"}, From: "interviewing", To: "offer", Message: "interviewing → offer"}
	h := newHarness(t, b, false)
	c := h.session(t)
	form := url.Values{"ref": {"job:42"}, "action": {"offer"}, "status": {"interviewing"}}

	w := h.do(post("/jobs/track", form, c))
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/tracked?status=interviewing" {
		t.Errorf("plain: %d to %q, want 303 /tracked?status=interviewing", w.Code, w.Header().Get("Location"))
	}
	form.Set("status", "")
	w = h.do(post("/jobs/track", form, c))
	if w.Header().Get("Location") != "/tracked" {
		t.Errorf("no filter: to %q, want /tracked", w.Header().Get("Location"))
	}
	form.Set("status", "bogus")
	w = h.do(post("/jobs/track", form, c))
	if w.Header().Get("Location") != "/tracked" {
		t.Errorf("unknown filter: to %q, want /tracked", w.Header().Get("Location"))
	}

	w = h.do(hx(post("/jobs/track", form, c)))
	body := w.Body.String()
	if w.Code != http.StatusOK || !strings.Contains(body, `id="tracked-list"`) || !strings.Contains(body, "interviewing → offer") || strings.Contains(body, "<html") {
		t.Errorf("htmx: %d %s", w.Code, body)
	}

	b.err = fmt.Errorf("%w: no such job", ErrUserFixable)
	w = h.do(hx(post("/jobs/track", form, c)))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "no such job") {
		t.Errorf("htmx refusal: %d %s", w.Code, w.Body.String())
	}
	w = h.do(post("/jobs/track", form, c))
	if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), "no such job") || !strings.Contains(w.Body.String(), "<html") {
		t.Errorf("plain refusal: %d %s", w.Code, w.Body.String())
	}
}

func TestTrackServerError(t *testing.T) {
	b := richBackend("x")
	b.err = errors.New("pool closed at 192.0.2.7")
	h := newHarness(t, b, false)
	c := h.session(t)
	for _, r := range []*http.Request{post("/jobs/track", trackForm(), c), hx(post("/jobs/track", trackForm(), c))} {
		w := h.do(r)
		if w.Code != http.StatusInternalServerError {
			t.Errorf("status %d, want 500", w.Code)
		}
		if strings.Contains(w.Body.String(), "pool closed") {
			t.Error("the error reached the page")
		}
	}
	if !strings.Contains(h.logs.String(), "pool closed") {
		t.Error("the error was not logged")
	}
}

func TestReadErrorIs500(t *testing.T) {
	b := richBackend("x")
	b.readErr = errors.New("query failed")
	h := newHarness(t, b, false)
	c := h.session(t)
	for _, p := range []string{"/", "/jobs", "/tracked", "/runs", "/rules"} {
		w := h.do(get(p, c))
		if w.Code != http.StatusInternalServerError || strings.Contains(w.Body.String(), "query failed") {
			t.Errorf("%s: %d %q", p, w.Code, w.Body.String())
		}
	}
}

func TestBodyCap(t *testing.T) {
	h := newHarness(t, richBackend("x"), false)
	form := trackForm()
	form.Set("reason", strings.Repeat("a", maxBody))
	w := h.do(post("/jobs/track", form, h.session(t)))
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status %d, want 413", w.Code)
	}
	if n := h.b.n("Track"); n != 0 {
		t.Errorf("Track called %d times", n)
	}
	form.Set("reason", strings.Repeat("a", maxBody-200))
	if w := h.do(post("/jobs/track", form, h.session(t))); w.Code != http.StatusSeeOther {
		t.Errorf("a body under the cap: %d", w.Code)
	}
}

func TestSessionExpiresMidHX(t *testing.T) {
	h := newHarness(t, richBackend("x"), false)
	c := h.session(t)
	h.clock.advance(12*time.Hour + time.Second)
	w := h.do(hx(post("/jobs/track", trackForm(), c)))
	if w.Code != http.StatusUnauthorized || w.Header().Get("HX-Redirect") != "/login" {
		t.Errorf("got %d HX-Redirect %q", w.Code, w.Header().Get("HX-Redirect"))
	}
	if h.b.total() != 0 {
		t.Error("the backend was called")
	}
}

func TestColdStart(t *testing.T) {
	h := newHarness(t, &fakeBackend{}, false)
	c := h.session(t)
	for _, p := range []string{"/", "/jobs"} {
		w := h.do(get(p, c))
		if w.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", p, w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), noBriefYet) {
			t.Errorf("%s lacks %q", p, noBriefYet)
		}
	}
	w := h.do(get("/jobs", c))
	if strings.Contains(w.Body.String(), `action="/jobs/track"`) {
		t.Error("the empty jobs page offers an action")
	}
	w = h.do(get("/", c))
	for _, line := range []string{"Jev today: 0 calls for jobs · 0 for leads", "$0.00 · 0 input tokens"} {
		if !strings.Contains(w.Body.String(), line) {
			t.Errorf("cold dashboard lacks %q: %s", line, w.Body.String())
		}
	}
	for _, p := range []string{"/tracked", "/runs", "/rules"} {
		if w := h.do(get(p, c)); w.Code != http.StatusOK {
			t.Errorf("%s: %d", p, w.Code)
		}
	}
}

func TestEscaping(t *testing.T) {
	const evil = "<script>alert(1)</script>"
	b := richBackend(evil)
	b.out.Job.Title = evil
	b.out.Message = evil
	h := newHarness(t, b, false)
	c := h.session(t)

	pages := map[string]*httptest.ResponseRecorder{}
	for _, p := range []string{"/", "/jobs", "/tracked", "/tracked?status=all", "/runs", "/rules"} {
		pages[p] = h.do(get(p, c))
	}
	pages["track htmx"] = h.do(hx(post("/jobs/track", trackForm(), c)))
	pages["track tracked htmx"] = h.do(hx(post("/jobs/track", url.Values{"ref": {"job:42"}, "action": {"offer"}}, c)))
	w := h.do(post("/jobs/track", trackForm(), c))
	pages["flash"] = h.do(get("/jobs", c, cookieNamed(w, "__Host-lh_flash")))
	b.err = fmt.Errorf("%w: %s", ErrUserFixable, evil)
	b.out = TrackOutcome{}
	pages["refusal htmx"] = h.do(hx(post("/jobs/track", trackForm(), c)))
	pages["refusal plain"] = h.do(post("/jobs/track", trackForm(), c))

	for name, w := range pages {
		body := w.Body.String()
		if strings.Contains(body, "<script>alert") {
			t.Errorf("%s: unescaped script", name)
		}
		if !strings.Contains(body, "&lt;script&gt;alert(1)&lt;/script&gt;") {
			t.Errorf("%s: the text is not shown escaped", name)
		}
	}
}

func TestExternalLinks(t *testing.T) {
	b := richBackend("x")
	b.brief.Jobs = append(b.brief.Jobs, BriefJob{N: 2, JobID: 43, Title: "y", Status: "new", URL: "javascript:alert(1)"})
	h := newHarness(t, b, false)
	body := h.do(get("/jobs", h.session(t))).Body.String()
	if !strings.Contains(body, `href="https://jobs.example/42" rel="noreferrer noopener" target="_blank"`) {
		t.Errorf("job link lacks rel and target: %s", body)
	}
	if strings.Contains(body, "javascript:") {
		t.Error("a javascript: URL was rendered as a link")
	}
}

func TestNoInlineScriptOrStyle(t *testing.T) {
	h := newHarness(t, richBackend("x"), false)
	c := h.session(t)
	for _, p := range []string{"/login", "/", "/jobs", "/tracked", "/runs", "/rules", "/results",
		"/mailboxes", "/mailboxes/new", "/mailboxes/1", "/mailboxes/1/edit"} {
		cookies := []*http.Cookie{c}
		if p == "/login" {
			cookies = nil
		}
		body := h.do(get(p, cookies...)).Body.String()
		for _, bad := range []string{"<style", " style=", "hx-on", "onclick", "<script>"} {
			if strings.Contains(body, bad) {
				t.Errorf("%s contains %q", p, bad)
			}
		}
		if !strings.Contains(body, `<link rel="stylesheet" href="/static/console.css?v=`+cssVersion+`">`) || len(cssVersion) != 8 {
			t.Errorf("%s does not link the stylesheet with its version", p)
		}
		if !strings.Contains(body, `<link rel="icon" href="/static/favicon.svg" type="image/svg+xml">`) {
			t.Errorf("%s does not link the favicon", p)
		}
		if !strings.Contains(body, `<script src="/static/htmx-2.0.11.min.js" defer></script>`) {
			t.Errorf("%s does not load htmx from /static/", p)
		}
		if !strings.Contains(body, `"includeIndicatorStyles":false,"allowEval":false,"allowScriptTags":false,"selfRequestsOnly":true,"attributesToSettle":["class","width","height"]}`) {
			t.Errorf("%s lacks the htmx config", p)
		}
	}
}

func TestDashboard(t *testing.T) {
	b := richBackend("x")
	b.dash.Health = []string{"Health: linkedin ok · 4 new · canary ok"}
	b.dash.LastBrief.Health = []string{"Health: linkedin ok · 2 new · canary ok"}
	b.dash.AsOf = time.Date(2026, 10, 24, 9, 12, 0, 0, time.UTC)
	h := newHarness(t, b, false)
	a := newHarnessIn(t, b, "Europe/Bucharest")
	body := h.do(get("/", h.session(t))).Body.String()
	for _, want := range []string{
		"This morning's email", "Now", "as of 09:12",
		"linkedin ok · 4 new · canary ok", "linkedin ok · 2 new · canary ok",
		"Jev today: 120 of 300 calls for jobs · 1,400 of 10,000 for leads", "$0.12 · 2.7M input tokens",
		"Gate failed", "Last runs", "1m 35s",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard lacks %q", want)
		}
	}
	if strings.Contains(body, "Health: ") {
		t.Error("the Health: prefix is shown")
	}
	if n := strings.Count(body, "as of "); n != 1 || strings.Index(body, "as of ") < strings.Index(body, "<h2>Now</h2>") {
		t.Errorf("the as-of time is shown %d times, or outside the Now card", n)
	}
	body = a.do(get("/", a.session(t))).Body.String()
	if !strings.Contains(body, "as of 12:12") {
		t.Error("times are not shown in Options.Location")
	}
}

func newHarnessIn(t *testing.T, b *fakeBackend, zone string) *harness {
	t.Helper()
	loc, err := time.LoadLocation(zone)
	if err != nil {
		t.Skip("no zone data:", err)
	}
	a, clock, calls := testAuth(t)
	return &harness{h: New(b, a, Options{Location: loc}), b: b, a: a, clock: clock, calls: calls, logs: &syncBuffer{}, name: "__Host-lh_session"}
}

func TestStaticFiles(t *testing.T) {
	h := newHarness(t, &fakeBackend{}, false)
	w := h.do(get("/static/htmx-2.0.11.min.js"))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	sum := sha256.Sum256(w.Body.Bytes())
	if got := hex.EncodeToString(sum[:]); got != htmxSHA256 {
		t.Errorf("htmx SHA-256 %s, want %s", got, htmxSHA256)
	}
	if !strings.HasPrefix(w.Body.String(), "var htmx=function()") {
		t.Error("the file is not htmx")
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "javascript") {
		t.Errorf("Content-Type %q", ct)
	}
	w = h.do(get("/static/console.css"))
	if w.Code != http.StatusOK || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/css") {
		t.Errorf("css: %d %q", w.Code, w.Header().Get("Content-Type"))
	}
	for _, p := range []string{"/static/", "/static/../templates/login.html", "/static/x/y"} {
		if w := h.do(get(p)); w.Code == http.StatusOK {
			t.Errorf("%s: 200", p)
		}
	}
	for _, p := range []string{"/static/favicon.svg", "/favicon.ico"} {
		w := h.do(get(p))
		if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "image/svg+xml" || !strings.HasPrefix(w.Body.String(), "<svg") {
			t.Errorf("%s: %d %q", p, w.Code, w.Header().Get("Content-Type"))
		}
	}
	w = h.do(get("/robots.txt"))
	if body, _ := io.ReadAll(w.Body); string(body) != "User-agent: *\nDisallow: /\n" {
		t.Errorf("robots.txt %q", body)
	}
	w = h.do(get("/healthz"))
	if w.Code != http.StatusOK || w.Body.String() != "ok\n" {
		t.Errorf("healthz %d %q", w.Code, w.Body.String())
	}
}

func TestCostLine(t *testing.T) {
	tests := []struct {
		d    Dashboard
		want string
	}{
		{Dashboard{AICostUSD: 0.12, AIInputTokens: 2_700_000}, "$0.12 · 2.7M input tokens"},
		{Dashboard{}, "$0.00 · 0 input tokens"},
		{Dashboard{AICostUSD: 1.005, AIInputTokens: 12_400}, "$1.00 · 12.4K input tokens"},
		{Dashboard{AIInputTokens: 999}, "$0.00 · 999 input tokens"},
	}
	for _, tt := range tests {
		if got := costLine(tt.d); got != tt.want {
			t.Errorf("costLine = %q, want %q", got, tt.want)
		}
	}
}

// TestCallsLine: the dashboard counts Jev's calls for jobs and for leads apart, each against
// its own cap, never one total against the jobs cap.
func TestCallsLine(t *testing.T) {
	tests := []struct {
		d    Dashboard
		want string
	}{
		{Dashboard{JobsCalls: 120, JobsCallCap: 300, LeadsCalls: 1400, LeadsCallCap: 10000},
			"Jev today: 120 of 300 calls for jobs · 1,400 of 10,000 for leads"},
		{Dashboard{}, "Jev today: 0 calls for jobs · 0 for leads"},
		{Dashboard{JobsCalls: 300, JobsCallCap: 300, LeadsCallCap: 2000}, "Jev today: 300 of 300 calls for jobs · 0 of 2,000 for leads"},
		{Dashboard{JobsCalls: 1_234_567, LeadsCalls: 12}, "Jev today: 1,234,567 calls for jobs · 12 for leads"},
	}
	for _, tt := range tests {
		if got := callsLine(tt.d); got != tt.want {
			t.Errorf("callsLine = %q, want %q", got, tt.want)
		}
	}
}

// TestClaudeLine: the dashboard's Claude line is the account's spend against the daily
// ceiling, naming only the purposes that spent; a quiet day reads $0.00, and spend under a
// cent never reads as nothing.
func TestClaudeLine(t *testing.T) {
	tests := []struct {
		d    Dashboard
		want string
	}{
		{Dashboard{ClaudeMicros: 840_000, ClaudeCapMicros: 5_000_000, ClaudeByPurpose: []PurposeSpend{
			{"drafting", 610_000}, {"research", 230_000}}}, "Claude today: $0.84 of $5.00 (drafting $0.61, research $0.23)"},
		{Dashboard{ClaudeCapMicros: 5_000_000}, "Claude today: $0.00 of $5.00"},
		{Dashboard{ClaudeMicros: 2_000, ClaudeCapMicros: 5_000_000, ClaudeByPurpose: []PurposeSpend{{"drafts_eval", 2_000}, {"research", 0}}},
			"Claude today: under $0.01 of $5.00 (drafts eval under $0.01)"},
		{Dashboard{ClaudeMicros: 5_000_000, ClaudeCapMicros: 5_000_000, ClaudeByPurpose: []PurposeSpend{{"drafting", 5_000_000}}},
			"Claude today: $5.00 of $5.00 (drafting $5.00)"},
	}
	for _, tt := range tests {
		if got := claudeLine(tt.d); got != tt.want {
			t.Errorf("claudeLine = %q, want %q", got, tt.want)
		}
	}
}

func TestTrackedFilters(t *testing.T) {
	h := newHarness(t, richBackend("x"), false)
	c := h.session(t)
	body := h.do(get("/tracked?status=applied", c)).Body.String()
	if !strings.Contains(body, `href="/tracked?status=applied" aria-current="true"`) {
		t.Error("the current filter is not marked")
	}
	for _, s := range []string{"interviewing", "offer", "rejected", "withdrawn"} {
		if !strings.Contains(body, `name="action" value="`+s+`"`) {
			t.Errorf("an applied job is not offered %s", s)
		}
	}
	if !strings.Contains(body, `name="ref" value="job:42"`) || !strings.Contains(body, `name="status" value="applied"`) {
		t.Error("the row's forms lack ref or the filter")
	}
}

// TestTrackedCapped: a capped list says how many it shows of how many match; a whole
// one says nothing.
func TestTrackedCapped(t *testing.T) {
	b := richBackend("x")
	h := newHarness(t, b, false)
	c := h.session(t)
	if body := h.do(get("/tracked?status=all", c)).Body.String(); strings.Contains(body, "Showing ") {
		t.Error("a whole list says it is capped")
	}
	b.matched = 432_306
	body := h.do(get("/tracked?status=all", c)).Body.String()
	if !strings.Contains(body, "Showing 1 of 432,306, the latest changes first.") {
		t.Errorf("a capped list does not say so: %s", body)
	}
	b.tracked = nil
	if body := h.do(get("/tracked?status=all", c)).Body.String(); strings.Contains(body, "Showing ") {
		t.Error("an empty list says it is capped")
	}
}

// TestDashboardSnapshotStates: the Now card and the day's numbers in each state of the
// snapshot: fresh, updating, failed with older numbers, the first build running, and the
// first build failed.
func TestDashboardSnapshotStates(t *testing.T) {
	asOf := time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC)
	failed := time.Date(2026, 10, 24, 9, 12, 0, 0, time.UTC)
	const (
		building  = "The day's numbers are being worked out; refresh in a minute."
		later     = "Shown once the day's numbers are worked out."
		failedOld = "The last update failed at 09:12; showing 09:00's numbers."
		failedNew = "The day's numbers could not be worked out at 09:12; refresh in a few minutes."
	)
	for _, c := range []struct {
		name     string
		set      func(*Dashboard)
		want     []string
		dontWant []string
	}{
		{"fresh", func(d *Dashboard) {}, []string{"as of 09:00</p>", "Apply", "Gate failed"},
			[]string{"updating", building, later, failedOld, failedNew}},
		{"updating", func(d *Dashboard) { d.Updating = true }, []string{"as of 09:00 · updating…", "Apply", "Gate failed"},
			[]string{building, later, failedOld, failedNew}},
		{"failed", func(d *Dashboard) { d.LastBuildFailedAt = &failed }, []string{"as of 09:00</p>", failedOld, "Apply"},
			[]string{"updating", building, failedNew}},
		{"failed retrying", func(d *Dashboard) { d.LastBuildFailedAt, d.Updating = &failed, true },
			[]string{"as of 09:00 · updating…", failedOld}, []string{building}},
		{"building", func(d *Dashboard) { *d = Dashboard{Building: true, JobsCallCap: 10, LeadsCallCap: 20} }, []string{building, later, "Jev today: 0 of 10 calls for jobs · 0 of 20 for leads"},
			[]string{"as of", "Apply", "Gate failed", "Nothing to report yet."}},
		{"first failed", func(d *Dashboard) { *d = Dashboard{LastBuildFailedAt: &failed} }, []string{failedNew, later},
			[]string{"as of", building, "Apply"}},
		{"first failed retrying", func(d *Dashboard) { *d = Dashboard{LastBuildFailedAt: &failed, Building: true} },
			[]string{building}, []string{failedNew, "as of"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			b := richBackend("x")
			b.dash.AsOf = asOf
			c.set(&b.dash)
			h := newHarness(t, b, false)
			body := h.do(get("/", h.session(t))).Body.String()
			for _, w := range c.want {
				if !strings.Contains(body, w) {
					t.Errorf("lacks %q", w)
				}
			}
			for _, w := range c.dontWant {
				if strings.Contains(body, w) {
					t.Errorf("shows %q", w)
				}
			}
		})
	}
}

func TestSourcesLargestFirst(t *testing.T) {
	got := sources(map[string]int{"lever": 859, "ashby": 1789, "greenhouse": 3519, "persono-fixture": 4, "a-fixture": 4})
	want := []sourceCount{{"greenhouse", 3519}, {"ashby", 1789}, {"lever", 859}, {"a-fixture", 4}, {"persono-fixture", 4}}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("sources = %v, want %v", got, want)
	}
}

func TestDashboardNewToday(t *testing.T) {
	b := richBackend("x")
	b.dash.Funnel.NewBySource = map[string]int{"lever": 859, "greenhouse": 3519}
	h := newHarness(t, b, false)
	body := h.do(get("/", h.session(t))).Body.String()
	gh := strings.Index(body, "<span class=\"list-title\">greenhouse</span></span><span class=\"list-end num\">3,519</span>")
	lv := strings.Index(body, "<span class=\"list-title\">lever</span></span><span class=\"list-end num\">859</span>")
	if !strings.Contains(body, "New today") || gh < 0 || lv < 0 || gh > lv {
		t.Errorf("new jobs by source are not a list, largest first: %s", body)
	}
	if strings.Contains(body, "New · ") {
		t.Error("per-source tiles are still shown")
	}
	b.dash.Funnel.NewBySource = nil
	if body := h.do(get("/", h.session(t))).Body.String(); !strings.Contains(body, "No new jobs today.") {
		t.Error("an empty funnel does not say so")
	}
}

func TestRunsAndRulesStackOnNarrowScreens(t *testing.T) {
	h := newHarness(t, richBackend("x"), false)
	c := h.session(t)
	for _, p := range []string{"/runs", "/rules"} {
		body := h.do(get(p, c)).Body.String()
		if !strings.Contains(body, "narrow-only") || !strings.Contains(body, "wide-only") {
			t.Errorf("%s lacks the stacked list or the wide table", p)
		}
	}
}
