package console

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

func (f *fakeBackend) Job(_ context.Context, id int64) (JobPage, error) {
	f.count("Job")
	if f.readErr != nil {
		return JobPage{}, f.readErr
	}
	if id != f.job.JobID {
		return JobPage{}, ErrNotFound
	}
	return f.job, nil
}

func (f *fakeBackend) AddRule(_ context.Context, req RuleRequest) (string, error) {
	f.count("AddRule")
	f.mu.Lock()
	f.lastRule = req
	f.mu.Unlock()
	return f.ruleMsg, f.ruleErr
}

func (f *fakeBackend) DisableRule(context.Context, int64, string) (string, error) {
	f.count("DisableRule")
	return f.ruleMsg, f.ruleErr
}

// leadsRead is the error every leads read returns.
func (f *fakeBackend) leadsRead() error {
	if f.readErr != nil {
		return f.readErr
	}
	return f.leadsErr
}

func (f *fakeBackend) Leads(context.Context) (LeadsPage, error) {
	f.count("Leads")
	return f.leads, f.leadsRead()
}

func (f *fakeBackend) LeadBatch(_ context.Context, id int64) (LeadBatch, error) {
	f.count("LeadBatch")
	if err := f.leadsRead(); err != nil {
		return LeadBatch{}, err
	}
	if id != f.batch.ID {
		return LeadBatch{}, ErrNotFound
	}
	return f.batch, nil
}

func (f *fakeBackend) LeadAct(_ context.Context, a LeadAction) (LeadOutcome, error) {
	f.count("LeadAct")
	f.mu.Lock()
	f.lastLead = a
	f.mu.Unlock()
	return f.leadOut, f.leadErr
}

func richJob(text string) JobPage {
	n := 37
	posted := testSent.Add(-48 * time.Hour)
	return JobPage{
		JobID: 42, Title: text, Company: text, Location: text, URL: "https://jobs.example/42", ApplyURL: "https://jobs.example/42/apply",
		ApplicantCount: &n, PostedAt: &posted, ApplyType: "ats", ListingStatus: "open", Status: "shortlisted",
		Description: text + "\nSecond line",
		Criteria:    []JobCriterion{{Name: "stack", Verdict: "pass", Keywords: text, Evidence: []string{text}}},
		Unclear:     []JobPoint{{Name: "location", Snippet: text}},
		History:     []StatusLine{{Status: "shortlisted", Reason: text, Actor: "system", At: testSent}},
	}
}

func richLeads(text string) LeadsPage {
	at := testSent
	return LeadsPage{
		Day: testDay,
		Sending: Sending{SetUp: true, SentToday: 2, CapToday: 4, SentYesterday: 3, Bounced: 1, OptedOut: 1, Stopped: text,
			DNS: &SenderCheck{At: testSent, SPF: true, DKIM: true, DMARC: false}},
		Batches:     []WaitingBatch{{ID: 3, CreatedAt: testSent, Leads: 4, Emails: 3, Forms: 1}},
		Manual:      []ManualItem{{MessageID: 9, BatchID: 2, Domain: text, Channel: "form", Route: text}},
		CheckByHand: []CheckItem{{MessageID: 11, Domain: text, Step: 1, State: "maybe_sent", At: &at}},
		Replies:     []string{text},
		Other:       2,
		Checked:     40,
		Drupal:      12,
		Suppressions: Suppressions{Counts: []SuppressionCount{{Kind: "domain", Reason: text, N: 1}},
			Rows: []SuppressionRow{{ID: 2, Kind: "address", Value: "j***@" + text, Reason: "opt_out", Source: "mailbox:1:INBOX:3", At: testSent}},
			More: 3,
			Lifted: []SuppressionRow{{ID: 1, Kind: "domain", Value: text, Reason: "opt_out", Source: "mailbox:1:INBOX:2", At: testSent,
				LiftedAt: &at, LiftReason: "quoted our opt-out line", LiftSource: "console"}}},
	}
}

func richBatch(text string) LeadBatch {
	return LeadBatch{
		ID: 3, CreatedAt: testSent, HashPrefix: "0123456789ab",
		Items: []LeadItem{{N: 1, LeadID: 5, Domain: text, Kind: "unsupported", Channel: "email", Finding: []string{"Finding: " + text},
			Messages: []LeadMessage{{ID: 21, Label: "email 1 of 3", Channel: "email", To: text, Subject: text, Body: text + "\n\nBest"}}}},
	}
}

// leadsRoute is one leads POST route with the form a test sends, where it goes on success
// and what it asks the backend.
type leadsRoute struct {
	path string
	form url.Values
	next string
	want LeadAction
}

func leadsPosts() []leadsRoute {
	return []leadsRoute{
		{"/leads/batches/3/approve", url.Values{"hash": {"0123456789ab"}, "skip": {"2", "1"}}, "/leads/batches/3",
			LeadAction{Do: LeadApprove, ID: 3, Hash: "0123456789ab", Skip: []int{1, 2}}},
		{"/leads/batches/3/discard", url.Values{}, "/leads/outreach", LeadAction{Do: LeadDiscard, ID: 3}},
		{"/leads/batches/new", url.Values{}, "/leads/outreach", LeadAction{Do: LeadNewBatch}},
		{"/leads/messages/9/resolve", url.Values{"sent": {"not-sent"}}, "/leads/outreach", LeadAction{Do: LeadResolve, ID: 9}},
		{"/leads/messages/9/resolve", url.Values{"sent": {"sent"}}, "/leads/outreach", LeadAction{Do: LeadResolve, ID: 9, Sent: true}},
		{"/leads/items/9/done", url.Values{}, "/leads/outreach", LeadAction{Do: LeadDone, ID: 9}},
		{"/leads/suppress", url.Values{"kind": {"domain"}, "value": {" northwind.example "}, "reason": {"manual"}}, "/leads/outreach",
			LeadAction{Do: LeadSuppress, Kind: "domain", Value: "northwind.example", Reason: "manual"}},
		{"/leads/suppressions/lift", url.Values{"id": {" 2 "}, "domain": {" domain.example "}, "reason": {" quoted our line "}}, "/leads/outreach",
			LeadAction{Do: LeadLift, ID: 2, Value: "domain.example", Reason: "quoted our line"}},
		{"/leads/contacted", url.Values{"domain": {"northwind.example"}, "note": {"called"}}, "/leads/outreach",
			LeadAction{Do: LeadContacted, Value: "northwind.example", Note: "called"}},
		{"/leads/sending", url.Values{"do": {"pause"}, "reason": {"holiday"}}, "/leads/outreach", LeadAction{Do: LeadPause, Reason: "holiday"}},
		{"/leads/sending", url.Values{"do": {"resume"}}, "/leads/outreach", LeadAction{Do: LeadResume}},
		{"/leads/5/star", url.Values{"back": {"/leads?sort=order#lead-w-5"}}, "/leads?sort=order#lead-w-5", LeadAction{Do: LeadStar, ID: 5}},
		{"/leads/5/unstar", url.Values{"back": {"/leads/5"}}, "/leads/5", LeadAction{Do: LeadUnstar, ID: 5}},
		{"/leads/5/up", url.Values{"back": {"/leads?sort=order"}}, "/leads?sort=order", LeadAction{Do: LeadUp, ID: 5}},
		{"/leads/5/down", url.Values{"back": {"/leads?sort=order&page=2"}}, "/leads?sort=order&page=2", LeadAction{Do: LeadDown, ID: 5}},
		{"/leads/5/top", url.Values{}, "/leads", LeadAction{Do: LeadTop, ID: 5}},
		{"/leads/5/defer", url.Values{"back": {"/leads/5"}}, "/leads/5", LeadAction{Do: LeadDefer, ID: 5}},
		{"/leads/5/defer", url.Values{"until": {" 2026-11-07 "}}, "/leads", LeadAction{Do: LeadDefer, ID: 5, Until: "2026-11-07"}},
		{"/leads/5/skip", url.Values{"back": {"//evil.example"}}, "/leads", LeadAction{Do: LeadSkip, ID: 5}},
		{"/leads/5/clear", url.Values{"until": {"2026-11-07"}}, "/leads", LeadAction{Do: LeadClear, ID: 5}},
		{"/leads/5/label", url.Values{"band": {" likely "}, "back": {"/leads/5"}}, "/leads/5#your-call", LeadAction{Do: LeadLabel, ID: 5, Value: "likely"}},
		{"/leads/5/label", url.Values{"band": {"clear"}, "back": {"/leads/5#lead-5"}}, "/leads/5#your-call", LeadAction{Do: LeadLabel, ID: 5, Value: "clear"}},
	}
}

func sameAction(a, b LeadAction) bool {
	return a.Do == b.Do && a.ID == b.ID && a.Hash == b.Hash && slices.Equal(a.Skip, b.Skip) && a.Kind == b.Kind &&
		a.Value == b.Value && a.Reason == b.Reason && a.Note == b.Note && a.Until == b.Until && a.Sent == b.Sent
}

// TestLeadsWrites: each leads POST asks the backend for its write and, done, goes on with
// the outcome in the one-time cookie; a refusal shows its words on the page (422); a
// server without leads settings says so.
func TestLeadsWrites(t *testing.T) {
	for _, rt := range leadsPosts() {
		t.Run(rt.path+" "+rt.want.Do, func(t *testing.T) {
			b := richBackend("x")
			h := newHarness(t, b, false)
			c := h.session(t)
			w := h.do(post(rt.path, rt.form, c))
			if w.Code != http.StatusSeeOther || w.Header().Get("Location") != rt.next {
				t.Fatalf("got %d to %q, want 303 to %s: %s", w.Code, w.Header().Get("Location"), rt.next, w.Body.String())
			}
			if !sameAction(b.lastLead, rt.want) {
				t.Errorf("asked %+v, want %+v", b.lastLead, rt.want)
			}
			if f := cookieNamed(w, "__Host-lh_flash"); f == nil {
				t.Error("no outcome cookie")
			}

			b.leadErr = fmt.Errorf("%w: Batch 3 still waits for approval; no new batch.", ErrUserFixable)
			w = h.do(post(rt.path, rt.form, c))
			if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), "Batch 3 still waits for approval; no new batch.") {
				t.Errorf("refusal: %d %s", w.Code, w.Body.String())
			}

			b.leadErr = ErrLeadsNotSetUp
			w = h.do(post(rt.path, rt.form, c))
			if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), leadsNotSetUp) {
				t.Errorf("not set up: %d %s", w.Code, w.Body.String())
			}

			b.leadErr = errors.New("database gone")
			if w = h.do(post(rt.path, rt.form, c)); w.Code != http.StatusInternalServerError || strings.Contains(w.Body.String(), "database gone") {
				t.Errorf("failure: %d %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestNewBatchGoesToIt(t *testing.T) {
	b := richBackend("x")
	b.leadOut = LeadOutcome{Message: "Batch 4 (live): 4 emails.", BatchID: 4}
	h := newHarness(t, b, false)
	w := h.do(post("/leads/batches/new", url.Values{}, h.session(t)))
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/leads/batches/4" {
		t.Fatalf("got %d to %q", w.Code, w.Header().Get("Location"))
	}
}

// TestLeadsFormsRefusedBeforeTheBackend: a form the console cannot read never reaches it.
func TestLeadsFormsRefusedBeforeTheBackend(t *testing.T) {
	cases := []struct {
		path string
		form url.Values
		code int
	}{
		{"/leads/batches/3/approve", url.Values{"hash": {"0123456789ab"}, "skip": {"one"}}, http.StatusUnprocessableEntity},
		{"/leads/messages/9/resolve", url.Values{"sent": {"maybe"}}, http.StatusUnprocessableEntity},
		{"/leads/sending", url.Values{"do": {"stop"}}, http.StatusUnprocessableEntity},
		{"/leads/5/promote", url.Values{}, http.StatusNotFound},
		{"/leads/5/pin", url.Values{}, http.StatusNotFound},
		{"/leads/0/star", url.Values{}, http.StatusNotFound},
		{"/leads/x/star", url.Values{}, http.StatusNotFound},
		{"/leads/next/5/pin", url.Values{}, http.StatusNotFound},
		{"/leads/batches/x/approve", url.Values{}, http.StatusNotFound},
		{"/leads/suppressions/lift", url.Values{"domain": {"domain.example"}, "reason": {"wrong"}}, http.StatusUnprocessableEntity},
		{"/leads/suppressions/lift", url.Values{"id": {"0"}, "domain": {"domain.example"}, "reason": {"wrong"}}, http.StatusUnprocessableEntity},
		{"/leads/suppressions/lift", url.Values{"id": {"-2"}, "domain": {"domain.example"}, "reason": {"wrong"}}, http.StatusUnprocessableEntity},
		{"/leads/suppressions/lift", url.Values{"id": {"#2"}, "domain": {"domain.example"}, "reason": {"wrong"}}, http.StatusUnprocessableEntity},
		{"/leads/suppressions/lift", url.Values{"id": {"2.5"}, "domain": {"domain.example"}, "reason": {"wrong"}}, http.StatusUnprocessableEntity},
	}
	for _, c := range cases {
		b := richBackend("x")
		h := newHarness(t, b, false)
		w := h.do(post(c.path, c.form, h.session(t)))
		if w.Code != c.code {
			t.Errorf("%s: %d, want %d", c.path, w.Code, c.code)
		}
		if n := b.n("LeadAct"); n != 0 {
			t.Errorf("%s: LeadAct called %d times", c.path, n)
		}
	}
}

// TestLeadsNotSetUp: every leads page says leads are not set up, and the jobs pages keep
// working.
func TestLeadsNotSetUp(t *testing.T) {
	b := richBackend("x")
	b.leadsErr = ErrLeadsNotSetUp
	b.dash.Leads = nil
	h := newHarness(t, b, false)
	c := h.session(t)
	for _, p := range []string{"/leads", "/leads/outreach", "/leads/settings", "/leads/batches/3", "/leads/5"} {
		w := h.do(get(p, c))
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), leadsNotSetUp) {
			t.Errorf("%s: %d %s", p, w.Code, w.Body.String())
		}
	}
	for _, p := range []string{"/", "/jobs", "/tracked", "/rules", "/jobs/42"} {
		w := h.do(get(p, c))
		if w.Code != http.StatusOK || strings.Contains(w.Body.String(), leadsNotSetUp) {
			t.Errorf("%s: %d", p, w.Code)
		}
	}
}

func TestLeadsNotFound(t *testing.T) {
	h := newHarness(t, richBackend("x"), false)
	c := h.session(t)
	for _, p := range []string{"/leads/batches/99", "/leads/99", "/jobs/99"} {
		if w := h.do(get(p, c)); w.Code != http.StatusNotFound {
			t.Errorf("%s: %d", p, w.Code)
		}
	}
}

// TestBatchApproveForm: the approve form appears only on a batch that can be approved, with
// the hash the backend recomputed for this read; a live batch's button says what happens.
func TestBatchApproveForm(t *testing.T) {
	cases := []struct {
		name   string
		edit   func(*LeadBatch)
		form   bool
		button string
	}{
		{"waiting live", func(*LeadBatch) {}, true, approveLiveFmt},
		{"waiting shadow", func(b *LeadBatch) { b.Shadow = true }, true, "Approve: a shadow batch, so nothing is sent."},
		{"hash mismatch", func(b *LeadBatch) { b.Changed, b.HashPrefix = true, "" }, false, ""},
		{"changed but a hash given", func(b *LeadBatch) { b.Changed = true }, false, ""},
		{"approved", func(b *LeadBatch) { b.HashPrefix, b.ApprovedAt = "", &testSent }, false, ""},
		{"discarded", func(b *LeadBatch) { b.HashPrefix, b.DiscardedAt = "", &testSent }, false, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b := richBackend("x")
			c.edit(&b.batch)
			h := newHarness(t, b, false)
			body := h.do(get("/leads/batches/3", h.session(t))).Body.String()
			has := strings.Contains(body, `action="/leads/batches/3/approve"`)
			if has != c.form {
				t.Fatalf("approve form %v, want %v: %s", has, c.form, body)
			}
			if c.form {
				for _, want := range []string{`name="hash" value="0123456789ab"`, template.HTMLEscapeString(c.button), `name="skip" value="1" form="approve-form"`} {
					if !strings.Contains(body, want) {
						t.Errorf("lacks %q", want)
					}
				}
			} else if strings.Contains(body, `name="skip"`) || strings.Contains(body, `name="hash"`) {
				t.Error("a batch that cannot be approved carries approve inputs")
			}
			if strings.Contains(body, "changed since it was drafted") != b.batch.Changed {
				t.Error("the changed line does not follow Changed")
			}
			discard := strings.Contains(body, `action="/leads/batches/3/discard"`)
			if discard != (b.batch.ApprovedAt == nil && b.batch.DiscardedAt == nil) {
				t.Errorf("discard form %v", discard)
			}
		})
	}
}

// TestLeadsNeverLogSubmittedValues drives every leads route, read and write, done, refused,
// failed and not set up, with values found nowhere else, and reads every log line.
func TestLeadsNeverLogSubmittedValues(t *testing.T) {
	const secret = "zq7secretvalue"
	const secretID = "987654321"
	form := url.Values{}
	for _, k := range []string{"hash", "kind", "value", "reason", "domain", "note", "until", "back", "countries", "rank_bands", "exclude_countries", "exact_version", "kind.unsupported",
		"country", "wanted", "never_contact", "on", "skip_rank_over", "batches_from", "d6_eol", "d10_eol", "rating", "leads_cap"} {
		form.Set(k, secret)
	}
	form.Set("skip", secretID)
	form.Set("sent", "sent")
	form.Set("do", "pause")
	errs := []error{nil, fmt.Errorf("%w: no", ErrUserFixable), ErrLeadsNotSetUp, errors.New("database gone")}
	var routesSeen int
	for _, rt := range routes {
		path := strings.TrimPrefix(strings.TrimPrefix(rt.pattern, "GET "), "POST ")
		if !strings.HasPrefix(path, "/leads") {
			continue
		}
		routesSeen++
		target := strings.NewReplacer("{id}", secretID, "{lead}", secretID, "{action}", "defer", "{section}", "who").Replace(path)
		for _, do := range []string{"pause", "save", "preview", "reset", "up:" + secret} {
			for _, e := range errs {
				b := richBackend("x")
				b.leadErr, b.saveErr = e, e
				if e != nil && !errors.Is(e, ErrUserFixable) {
					b.leadsErr = e
				}
				h := newHarness(t, b, false)
				c := h.session(t)
				f := url.Values{}
				for k, v := range form {
					f[k] = v
				}
				f.Set("do", do)
				if strings.HasPrefix(rt.pattern, "POST ") {
					h.do(post(target, f, c))
				} else {
					h.do(get(target+"?value="+secret, c))
				}
				if logs := h.logs.String(); strings.Contains(logs, secret) || strings.Contains(logs, secretID) {
					t.Errorf("%s (%s, %v) logged a submitted value: %s", rt.pattern, do, e, logs)
				}
			}
		}
	}
	if routesSeen < 14 {
		t.Errorf("only %d leads routes swept", routesSeen)
	}
}

// TestNoLeadsWriteOnGet: no GET ever writes, whatever it is sent.
func TestNoLeadsWriteOnGet(t *testing.T) {
	h := newHarness(t, richBackend("x"), false)
	c := h.session(t)
	for _, rt := range routes {
		if !strings.HasPrefix(rt.pattern, "GET ") {
			continue
		}
		h.do(get(rt.sample+"?do=save&hash=0123456789ab&action=applied", c))
	}
	for _, name := range []string{"LeadAct", "AddRule", "DisableRule", "Track", "SaveLeadSettings"} {
		if n := h.b.n(name); n != 0 {
			t.Errorf("a GET called %s %d times", name, n)
		}
	}
	for _, name := range []string{"LeadList", "LeadDetail", "Leads", "LeadSettings", "Campaigns"} {
		if h.b.n(name) == 0 {
			t.Errorf("the sweep never reached %s", name)
		}
	}
}

// TestJobPage: one article with every labelled field Claude in Chrome reads, the mark
// form posting to the job's own track route, and the apply link opening a new tab.
func TestJobPage(t *testing.T) {
	h := newHarness(t, richBackend("Senior Go Engineer"), false)
	body := h.do(get("/jobs/42", h.session(t))).Body.String()
	if n := strings.Count(body, "<article"); n != 1 {
		t.Errorf("%d articles, want 1", n)
	}
	for _, want := range []string{
		`<article class="record" id="job-42">`,
		"<dt>Job</dt><dd>job:42</dd>", "<dt>Company</dt>", "<dt>Location</dt>", "<dt>Apply link</dt>", "<dt>Posting</dt>",
		"<dt>Applicants</dt><dd>37</dd>", "<dt>Posted</dt>", "<dt>Listing</dt><dd>open</dd>", "<dt>Status</dt>",
		`<a href="https://jobs.example/42/apply" rel="noreferrer noopener" target="_blank">`,
		`action="/jobs/42/track"`, `name="action" value="applied"`, `name="action" value="skip"`, `name="action" value="maybe"`,
		"<h2>Criteria</h2>", "<dt>stack</dt>", "matched Senior Go Engineer", "<h2>Unclear</h2>", "<h2>Description</h2>",
		"Senior Go Engineer\nSecond line", "<h2>History</h2>",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("job page lacks %q", want)
		}
	}
}

func TestJobPageTrack(t *testing.T) {
	b := richBackend("x")
	h := newHarness(t, b, false)
	c := h.session(t)
	w := h.do(post("/jobs/42/track", url.Values{"action": {"applied"}, "ref": {"7"}}, c))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `id="outcome">shortlisted → applied</p>`) {
		t.Fatalf("mark: %d %s", w.Code, w.Body.String())
	}
	if b.last.Ref != "job:42" || b.last.Action != "applied" {
		t.Errorf("asked %+v; the path names the job, not the form", b.last)
	}
	b.err = fmt.Errorf("%w: already applied; use withdrawn", ErrUserFixable)
	w = h.do(post("/jobs/42/track", url.Values{"action": {"applied"}}, c))
	if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), "already applied; use withdrawn") {
		t.Errorf("refusal: %d %s", w.Code, w.Body.String())
	}
}

func TestRuleForms(t *testing.T) {
	b := richBackend("x")
	h := newHarness(t, b, false)
	c := h.session(t)
	w := h.do(post("/rules", url.Values{"kind": {"skip"}, "field": {"company"}, "pattern": {"^acme"}, "reason": {" agency "}}, c))
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/rules" {
		t.Fatalf("add: %d %q", w.Code, w.Header().Get("Location"))
	}
	if b.lastRule != (RuleRequest{Kind: "skip", Field: "company", Pattern: "^acme", Reason: "agency"}) {
		t.Errorf("asked %+v", b.lastRule)
	}
	page := h.do(get("/rules", c, cookieNamed(w, "__Host-lh_flash"))).Body.String()
	if !strings.Contains(page, "rule 8 added") {
		t.Error("the rules page does not show what the add did")
	}
	if w := h.do(post("/rules/7/disable", url.Values{"reason": {"too broad"}}, c)); w.Code != http.StatusSeeOther {
		t.Errorf("disable: %d", w.Code)
	}
	b.ruleErr = fmt.Errorf("%w: rule 7 is already disabled; add it again to undo", ErrUserFixable)
	w = h.do(post("/rules/7/disable", url.Values{"reason": {"x"}}, c))
	if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), "rule 7 is already disabled") {
		t.Errorf("refusal: %d", w.Code)
	}
	if !strings.Contains(page, `action="/rules/7/disable"`) || !strings.Contains(page, `action="/rules"`) {
		t.Error("the rules page lacks its forms")
	}
	if strings.Count(page, `id="disable-reason-n-7"`) != 1 || strings.Count(page, `id="disable-reason-w-7"`) != 1 {
		t.Error("the two layouts' disable forms do not keep their ids apart")
	}
}

func TestSafeNextWithIDs(t *testing.T) {
	h := newHarness(t, richBackend("x"), false)
	srv := &server{next: map[string]bool{}, nextAt: []string{"/jobs/{id}", "/leads/batches/{id}"}}
	tests := []struct{ in, want string }{
		{"/jobs/42", "/jobs/42"},
		{"/leads/batches/3", "/leads/batches/3"},
		{"/jobs/0", "/"},
		{"/jobs/007", "/"},
		{"/jobs/abc", "/"},
		{"/jobs/42/track", "/"},
		{"/leads/batches/3/approve", "/"},
		{"//jobs/42", "/"},
	}
	for _, tt := range tests {
		if got := srv.safeNext(tt.in); got != tt.want {
			t.Errorf("safeNext(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
	// A sign-in from a job page's link goes back to it.
	w := h.do(post("/login", url.Values{"email": {testEmail}, "password": {testPassword}, "next": {"/jobs/42"}}))
	if w.Header().Get("Location") != "/jobs/42" {
		t.Errorf("sign-in went to %q", w.Header().Get("Location"))
	}
}

// TestLeadsPages reads each leads page with data in it.
func TestLeadsPages(t *testing.T) {
	h := newHarness(t, richBackend("northwind.example"), false)
	c := h.session(t)
	pages := map[string][]string{
		"/leads/outreach": {"Sending is on.", "2 of 4", "DMARC <span class=\"bad\">missing</span>", `href="/leads/batches/3"`,
			`action="/leads/batches/new"`, "Make the next batch now", `action="/leads/messages/11/resolve"`, `action="/leads/items/9/done"`,
			`name="value" value="northwind.example"`, "Pause sending", `action="/leads/contacted"`, "j***@northwind.example"},
		"/leads/settings":  {`name="kind.unsupported" value="20"`, "Order changed here on 24 October 2026.", `action="/leads/settings/order"`},
		"/leads/batches/3": {"1. northwind.example", "To: northwind.example", "Subject: northwind.example", "Finding: northwind.example"},
	}
	for p, wants := range pages {
		body := h.do(get(p, c)).Body.String()
		for _, want := range wants {
			if !strings.Contains(body, want) {
				t.Errorf("%s lacks %q", p, want)
			}
		}
		if !strings.Contains(body, `<a href="/leads" aria-current="page">Leads</a>`) {
			t.Errorf("%s: the Leads nav entry is not current", p)
		}
	}
	b := richBackend("x")
	b.leads.Sending.Paused, b.leads.Sending.PausedAt, b.leads.Sending.PauseReason = true, testSent, "holiday"
	b.leads.Sending.Stopped = ""
	ph := newHarness(t, b, false)
	body := ph.do(get("/leads/outreach", ph.session(t))).Body.String()
	for _, want := range []string{"Sending is paused (since Sat 24 Oct 04:50): holiday.", "Resume sending"} {
		if !strings.Contains(body, want) {
			t.Errorf("paused /leads/outreach lacks %q", want)
		}
	}
	if strings.Contains(body, "Pause sending") {
		t.Error("a paused page offers to pause")
	}
	b.leads = LeadsPage{}
	eh := newHarness(t, b, false)
	body = eh.do(get("/leads/outreach", eh.session(t))).Body.String()
	for _, want := range []string{"Sending isn't set up on this server yet.", "No batch waits for approval.", "No replies since yesterday.",
		"Nothing to check.", "None waiting.", "No suppressions.", "The sender's records have not been checked yet."} {
		if !strings.Contains(body, want) {
			t.Errorf("empty /leads/outreach lacks %q", want)
		}
	}
}

// TestLeadsEscaping: every leads page escapes what the backend gives it, a hostile site
// name and description included, and a javascript: link is never a link.
func TestLeadsEscaping(t *testing.T) {
	const evil = "<script>alert(1)</script>"
	const jsLink = "javascript:alert(1)"
	b := richBackend(evil)
	b.detail.Social = append(b.detail.Social, LeadFact{Label: evil, Value: jsLink, PageURL: jsLink})
	b.detail.Signals[0].FetchURL = jsLink
	h := newHarness(t, b, false)
	c := h.session(t)
	for _, p := range []string{"/leads", "/leads/outreach", "/leads/5", "/leads/settings", "/leads/batches/3", "/jobs/42"} {
		body := h.do(get(p, c)).Body.String()
		if strings.Contains(body, "<script>alert") || !strings.Contains(body, "&lt;script&gt;alert(1)&lt;/script&gt;") {
			t.Errorf("%s does not escape backend text", p)
		}
	}
	body := h.do(get("/leads/5", c)).Body.String()
	if strings.Contains(body, `href="javascript`) || strings.Contains(body, "ZgotmplZ") || !strings.Contains(body, jsLink) {
		t.Errorf("a javascript: URL is a link, or is not shown: %s", body)
	}
	// A refusal shows the page back with its words, escaped, the list and the lead page alike;
	// and a hostile back value never reaches a page.
	b.leadErr = fmt.Errorf("%w: %s", ErrUserFixable, evil)
	for _, back := range []string{"/leads?sort=order", "/leads/5", `/leads?sort="><script>alert(1)</script>`} {
		w := h.do(post("/leads/5/star", url.Values{"back": {back}}, c))
		if body := w.Body.String(); w.Code != http.StatusUnprocessableEntity || strings.Contains(body, "<script>alert") ||
			!strings.Contains(body, "&lt;script&gt;alert(1)&lt;/script&gt;") {
			t.Errorf("refusal back to %q: %d, unescaped or missing", back, w.Code)
		}
	}
}

// TestLiftSuppression (FO6): the lift form needs a session and a same-origin POST, refuses a
// number it cannot read before the backend, hands the backend the trimmed id, domain and
// reason, shows the flash it returns, and logs only "lift" and the outcome.
func TestLiftSuppression(t *testing.T) {
	const domain, reason = "domain.example", "quoted our opt-out line back"
	form := url.Values{"id": {"2"}, "domain": {domain}, "reason": {reason}}

	b := richBackend("x")
	h := newHarness(t, b, false)
	if w := h.do(post("/leads/suppressions/lift", form)); w.Code == http.StatusSeeOther && w.Header().Get("Location") == "/leads/outreach" {
		t.Errorf("a lift without a session went through")
	}
	c := h.session(t)
	cross := post("/leads/suppressions/lift", form, c)
	cross.Header.Set("Sec-Fetch-Site", "cross-site")
	if w := h.do(cross); w.Code != http.StatusForbidden {
		t.Errorf("cross-site lift: %d, want 403", w.Code)
	}
	if n := b.n("LeadAct"); n != 0 {
		t.Fatalf("LeadAct called %d times before a good POST", n)
	}

	w := h.do(post("/leads/suppressions/lift", url.Values{"id": {"two"}, "domain": {domain}, "reason": {reason}}, c))
	if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), "Give the number of the suppression to lift.") {
		t.Errorf("bad id: %d %s", w.Code, w.Body.String())
	}
	if n := b.n("LeadAct"); n != 0 {
		t.Errorf("a bad id reached the backend %d times", n)
	}

	const flash = "Lifted suppression 2: address e***@domain.example (opt_out, from mailbox:1:INBOX:3). It may be written to again; lead states are unchanged."
	b.leadOut = LeadOutcome{Message: flash}
	w = h.do(post("/leads/suppressions/lift", url.Values{"id": {"2"}, "domain": {"  " + domain + " "}, "reason": {" " + reason + "  "}}, c))
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/leads/outreach" {
		t.Fatalf("lift: %d to %q", w.Code, w.Header().Get("Location"))
	}
	want := LeadAction{Do: "lift", ID: 2, Value: domain, Reason: reason}
	if !sameAction(b.lastLead, want) {
		t.Errorf("asked %+v, want %+v", b.lastLead, want)
	}
	page := h.do(get("/leads/outreach", c, cookieNamed(w, "__Host-lh_flash"))).Body.String()
	if !strings.Contains(page, template.HTMLEscapeString(flash)) {
		t.Errorf("the leads page does not show the lift's outcome: %s", page)
	}

	logs := h.logs.String()
	if !strings.Contains(logs, "action=lift") || !strings.Contains(logs, "outcome=ok") || !strings.Contains(logs, "outcome=refused") {
		t.Errorf("the log lacks the lift and its outcomes: %s", logs)
	}
	if strings.Contains(logs, domain) || strings.Contains(logs, "quoted our") {
		t.Errorf("the log holds the submitted domain or reason: %s", logs)
	}
}

// TestSuppressionsCard: the card lists active suppressions with their numbers, the lifted
// ones in their own section, the lift form, and the hint that a lift is kept.
func TestSuppressionsCard(t *testing.T) {
	h := newHarness(t, richBackend("domain.example"), false)
	body := h.do(get("/leads/outreach", h.session(t))).Body.String()
	for _, want := range []string{
		"#2 address j***@domain.example · opt_out · mailbox:1:INBOX:3 · Saturday 24 October",
		"1 suppression, 3 more not shown",
		"<summary>Lifted (1)</summary>",
		"#1 domain domain.example · lifted Saturday 24 October (console) · quoted our opt-out line",
		`action="/leads/suppressions/lift"`,
		`<input id="lift-id" name="id" inputmode="numeric" required>`,
		`<input id="lift-domain" name="domain" required maxlength="253" aria-describedby="lift-domain-hint">`,
		`<input id="lift-reason" name="reason" required minlength="3" maxlength="300">`,
		"the domain shown in the list, to confirm",
		"Lift one only when it was recorded by mistake: the lift is kept with its reason, and a new opt-out suppresses again.",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/leads/outreach lacks %q", want)
		}
	}
	if strings.Contains(body, "never lifted") {
		t.Error("/leads still says a suppression is never lifted")
	}

	b := richBackend("domain.example")
	b.leads.Suppressions.Lifted = nil
	eh := newHarness(t, b, false)
	if body := eh.do(get("/leads/outreach", eh.session(t))).Body.String(); strings.Contains(body, "Lifted (") {
		t.Error("an empty Lifted section is shown")
	}
}

func (f *fakeBackend) LeadList(_ context.Context, q LeadListQuery) (LeadListPage, error) {
	f.count("LeadList")
	f.mu.Lock()
	f.listQ = q
	f.mu.Unlock()
	p := f.list
	p.Filter = q // the backend echoes the filter it took
	return p, f.leadsRead()
}

func (f *fakeBackend) LeadDetail(_ context.Context, id int64) (LeadDetailPage, error) {
	f.count("LeadDetail")
	if err := f.leadsRead(); err != nil {
		return LeadDetailPage{}, err
	}
	if id != f.detail.LeadID {
		return LeadDetailPage{}, ErrNotFound
	}
	return f.detail, nil
}

func richLeadList(text string) LeadListPage {
	return LeadListPage{
		Rows: []LeadListRow{{LeadID: 5, OrgID: 2, Name: text, Domain: text, Country: "GB", Kind: text, Finding: text,
			Relevance: text, Position: "3", State: "new", HasEmail: true, HasPhone: true, Starred: true, SmallSite: true,
			Campaign: "Drupal end-of-life"}},
		Total: 120, TotalLeads: 130, Page: 2, PerPage: 50,
		Options: LeadFilterOptions{
			Kinds:     []FilterOption{{"d10", "Drupal 10"}, {"unsupported", text}},
			Countries: []FilterOption{{"GB", "United Kingdom"}, {"DE", "Germany"}},
			Channels:  []FilterOption{{"email", "Email"}, {"form", "Contact form"}},
			Statuses:  []FilterOption{{"open", "Open"}, {"contacted", "Contacted"}, {"all", "All"}},
			Sorts:     []FilterOption{{"relevance", "Relevance"}, {"newest", "Newest"}, {"order", "Outreach order"}},
			Campaigns: []FilterOption{{"drupal-eol", "Drupal end-of-life"}, {"wp-eol", "WordPress end-of-life"}},
		},
		Notes: []string{text},
	}
}

func richLeadDetail(text string) LeadDetailPage {
	at := testSent
	return LeadDetailPage{
		LeadListRow: LeadListRow{LeadID: 5, OrgID: 2, Name: text, Domain: text, Country: "GB", Kind: text, Finding: text,
			Relevance: text, Position: "3", State: "new", HasEmail: true, HasForm: true, Campaign: "Drupal end-of-life"},
		Signals: []LeadSignal{{Name: "generator", Value: text, FetchURL: "https://northwind.example/",
			FinalURL: "https://www.northwind.example/", Status: "200 OK", FetchedAt: &at}},
		Routes: []LeadRoute{{Kind: "email", Value: "info@northwind.example", Note: "mail server found",
			PageURL: "https://northwind.example/contact", FoundAt: &at}},
		Phones:       []LeadFact{{Value: "+44 20 7946 0000", PageURL: "https://northwind.example/contact", FoundAt: &at}},
		Descriptions: []LeadFact{{Value: text, PageURL: "https://northwind.example/"}},
		Social:       []LeadFact{{Label: "LinkedIn", Value: "https://social.example/northwind", PageURL: "https://northwind.example/"}},
		Agency:       &LeadAgency{URL: "https://agency.example/drupal", Phrase: text, FetchURL: "https://agency.example/drupal"},
		ListRank:     "12,345", Origin: text, CountryPolicy: text,
		Events:   []LeadEvent{{State: "new", Reason: text, Actor: "finder", At: testSent}},
		Priority: &LeadPriority{Words: text, Actor: "operator", At: testSent},
		Messages: []LeadDetailMessage{{BatchID: 3, Label: "email 1 of 3", Channel: "email", State: "draft", Subject: text,
			Body: text + "\n\nBest", Due: &at}},
		RelevanceHistory: []LeadRelevance{{Words: text, At: testSent}},
		Why:              []string{text, "Runs Drupal 10: +30"},
	}
}

// TestLeadListPage: the list shows each lead as a card at narrow widths and a table row at
// wide ones, under the Leads sub-nav, with its pager; and the two empty states.
func TestLeadListPage(t *testing.T) {
	h := newHarness(t, richBackend("Northwind Trading"), false)
	body := h.do(get("/leads", h.session(t))).Body.String()
	for _, want := range []string{
		`<a href="/leads" aria-current="page">Leads</a>`,
		`<a href="/leads" aria-current="page">List</a>`, `<a href="/leads/outreach">Outreach</a>`, `<a href="/leads/settings">Settings</a>`,
		`<form method="get" action="/leads" class="lead-filters">`,
		`<ul class="item-list narrow-only">`, `<div class="table-wrap wide-only">`,
		`<li id="lead-n-5">`, `<tr id="lead-w-5">`, `<a href="/leads/5">Northwind Trading</a>`,
		"email · phone", "number 3 in the outreach order", `<td class="num">3</td>`,
		`<form method="post" action="/leads/5/unstar" class="star-form"><input type="hidden" name="back" value="/leads?page=2#lead-w-5">`,
		`<input type="hidden" name="back" value="/leads?page=2#lead-n-5">`,
		`<span class="visually-hidden">Unstar Northwind Trading</span>`, `<span class="tag">small site</span>`,
		"120 leads · page 2 of 3", `rel="prev">Previous page</a>`, `rel="next">Next page</a>`,
		`<p class="hint">Northwind Trading</p>`,
		`<span class="item-meta">Drupal end-of-life</span>`, `<td>Northwind Trading<span class="item-meta">Drupal end-of-life</span></td>`,
		`<a href="/leads/campaigns">Campaigns</a>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/leads lacks %q", want)
		}
	}
	if strings.Contains(body, "No leads") {
		t.Error("a list with leads shows an empty state")
	}
	if strings.Contains(body, `action="/leads/5/up"`) {
		t.Error("a list not in the outreach order offers to move a lead")
	}

	b := richBackend("x")
	b.list = LeadListPage{}
	eh := newHarness(t, b, false)
	body = eh.do(get("/leads", eh.session(t))).Body.String()
	if !strings.Contains(body, "No leads yet. Finding builds them: on the server it is off until <code>LEADHOUND_LEADS_TIMERS=on</code>.") ||
		strings.Contains(body, "No leads match these filters.") || strings.Contains(body, "<table") {
		t.Errorf("no leads at all: %s", body)
	}
	b.list = LeadListPage{TotalLeads: 4, Page: 1, PerPage: 50}
	body = eh.do(get("/leads?kind=d10", eh.session(t))).Body.String()
	if !strings.Contains(body, "No leads match these filters.") || strings.Contains(body, "No leads yet.") || strings.Contains(body, "<table") {
		t.Errorf("no leads match: %s", body)
	}

	// The first page has no previous page, the last no next page.
	b.list = richLeadList("x")
	b.list.Page = 1
	if body := eh.do(get("/leads", eh.session(t))).Body.String(); strings.Contains(body, `rel="prev"`) || !strings.Contains(body, `rel="next"`) {
		t.Error("the first page's pager")
	}
	b.list.Page = 3
	if body := eh.do(get("/leads?page=3", eh.session(t))).Body.String(); !strings.Contains(body, `rel="prev"`) || strings.Contains(body, `rel="next"`) {
		t.Error("the last page's pager")
	}
}

// TestLeadListFiltersEchoed: the query reaches the backend trimmed and without empty
// values, the form shows the filter the backend took, and the pager keeps it.
func TestLeadListFiltersEchoed(t *testing.T) {
	b := richBackend("x")
	h := newHarness(t, b, false)
	target := "/leads?kind=d10&kind=+unsupported+&kind=&country=GB&channel=email&starred=yes&has_phone=1" +
		"&min_relevance=+40&status=all&sort=newest&campaign=+drupal-eol+&page=2"
	body := h.do(get(target, h.session(t))).Body.String()
	want := LeadListQuery{Kinds: []string{"d10", "unsupported"}, Countries: []string{"GB"}, Channels: []string{"email"},
		Starred: "yes", HasPhone: "1", MinRelevance: "40", Status: "all", Sort: "newest", Page: "2", Campaign: "drupal-eol"}
	if fmt.Sprint(b.listQ) != fmt.Sprint(want) {
		t.Errorf("asked %+v, want %+v", b.listQ, want)
	}
	for _, w := range []string{
		`name="kind" value="d10" checked`, `name="kind" value="unsupported" checked`,
		`name="country" value="GB" checked`, `name="country" value="DE"> Germany`,
		`name="channel" value="email" checked`, `name="channel" value="form"> Contact form`,
		`name="has_phone" value="1" checked`, `<option value="yes" selected>Starred</option>`,
		`name="min_relevance" type="number" min="0" max="100" step="1" inputmode="numeric" value="40"`,
		`<option value="all" selected>All</option>`, `<option value="newest" selected>Newest</option>`,
		`<option value="drupal-eol" selected>Drupal end-of-life</option>`, `<option value="wp-eol">WordPress end-of-life</option>`,
	} {
		if !strings.Contains(body, w) {
			t.Errorf("the form lacks %q", w)
		}
	}
	next := leadListURL(want, 3)
	prev := leadListURL(want, 1)
	if !strings.Contains(next, "kind=d10&kind=unsupported") || !strings.Contains(next, "page=3") || strings.Contains(prev, "page=") ||
		!strings.Contains(next, "campaign=drupal-eol") || !strings.Contains(prev, "campaign=drupal-eol") {
		t.Errorf("pager URLs %q %q", next, prev)
	}
	for _, u := range []string{next, prev} {
		if !strings.Contains(body, `href="`+strings.ReplaceAll(u, "&", "&amp;")+`"`) {
			t.Errorf("the pager lacks %q", u)
		}
	}
	if got := leadListURL(LeadListQuery{}, 1); got != "/leads" {
		t.Errorf("the plain list is %q", got)
	}

	many := url.Values{}
	for i := range maxFilterValues + 10 {
		many.Add("country", fmt.Sprint("C", i))
	}
	h.do(get("/leads?"+many.Encode(), h.session(t)))
	if len(b.listQ.Countries) != maxFilterValues {
		t.Errorf("%d countries reached the backend, want %d", len(b.listQ.Countries), maxFilterValues)
	}
}

// TestLeadDetailPage: a lead's page shows every section in the Detail order; a fetched URL
// is a link that sends no referrer and is not followed, and a message's body sits inside
// its own details.
func TestLeadDetailPage(t *testing.T) {
	h := newHarness(t, richBackend("Northwind Trading"), false)
	body := h.do(get("/leads/5", h.session(t))).Body.String()
	last := -1
	for _, section := range []string{"<h2>Finding</h2>", "<h2>Ways to reach them</h2>", "<h2>From their site</h2>",
		`<h2 id="call-title">Your call</h2>`, "<h2>Agency page</h2>", "<h2>The site</h2>", "<h2>History</h2>",
		"<h2>Place in the queue</h2>", "<h2>Messages</h2>", "<h2>Why it ranks here</h2>"} {
		i := strings.Index(body, section)
		if i < 0 || i < last {
			t.Errorf("%s missing or out of order", section)
		}
		last = i
	}
	for _, want := range []string{
		`<a href="/leads" aria-current="page">Leads</a>`, `<a href="/leads" aria-current="true">List</a>`,
		`<article class="record lead-record" id="lead-5">`, "<h1>Northwind Trading</h1>",
		"<dt>Relevance</dt><dd>Northwind Trading\n", "<summary>1 rating so far</summary>",
		`<li><span class="item-head">Northwind Trading</span><span class="item-meta">Sat 24 Oct 04:50</span></li>`,
		"<dt>Reach by</dt><dd>email · form</dd>", "<dt>Outreach order</dt><dd>number 3</dd>",
		"<dt>Campaign</dt><dd>Drupal end-of-life</dd>",
		"generator: Northwind Trading",
		`Fetched <a href="https://northwind.example/" rel="noopener noreferrer nofollow">https://northwind.example/</a>`,
		`ended at <a href="https://www.northwind.example/" rel="noopener noreferrer nofollow">`, "200 OK",
		"email: info@northwind.example", "mail server found",
		"&#43;44 20 7946 0000", `<div class="description">Northwind Trading</div>`,
		`LinkedIn: <a href="https://social.example/northwind" rel="noopener noreferrer nofollow">https://social.example/northwind</a>`,
		"LeadHound never opens them.",
		`<a href="https://agency.example/drupal" rel="noopener noreferrer nofollow">`, "“Northwind Trading”",
		"<dt>List rank</dt><dd>12,345</dd>", "<dt>Found through</dt><dd>Northwind Trading</dd>",
		"finder · Northwind Trading", `In <a href="/leads/batches/3">batch 3</a> · due Sat 24 Oct 04:50`,
		"Subject: Northwind Trading", "Runs Drupal 10: &#43;30",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the lead page lacks %q", want)
		}
	}
	// Relevance is said once, in the facts, with its ratings folded under it: no section of
	// its own.
	if strings.Contains(body, "<h2>Relevance</h2>") || strings.Contains(body, "Not rated yet.") {
		t.Error("the lead page still has a Relevance section")
	}
	open := strings.Index(body, `<details class="lead-message">`)
	msg := strings.Index(body, "<div class=\"message-body\">Northwind Trading\n\nBest</div>")
	shut := strings.Index(body[max(open, 0):], "</details>") + max(open, 0)
	if open < 0 || msg < open || msg > shut {
		t.Error("the message body is not inside its details")
	}
}

func TestLeadDetailNotFound(t *testing.T) {
	b := richBackend("x")
	h := newHarness(t, b, false)
	c := h.session(t)
	w := h.do(get("/leads/99", c))
	if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "There is no such item.") {
		t.Errorf("no such lead: %d %s", w.Code, w.Body.String())
	}
	for _, p := range []string{"/leads/abc", "/leads/0", "/leads/-1", "/leads/5x"} {
		if w := h.do(get(p, c)); w.Code != http.StatusNotFound {
			t.Errorf("%s: %d", p, w.Code)
		}
	}
	if n := b.n("LeadDetail"); n != 1 {
		t.Errorf("LeadDetail called %d times; a path that is not an id must not reach it", n)
	}
}

// numbered matches an id in a path, so paths of one route compare equal.
var numbered = regexp.MustCompile(`/[0-9]+(/|$)`)

// TestOutreachKeepsEveryAction: the Outreach page offers every POST the old /leads page
// did, at the same paths, and each goes back to /leads/outreach.
func TestOutreachKeepsEveryAction(t *testing.T) {
	h := newHarness(t, richBackend("northwind.example"), false)
	c := h.session(t)
	body := h.do(get("/leads/outreach", c)).Body.String()
	found := map[string]bool{}
	for _, m := range regexp.MustCompile(`<form method="post" action="([^"]+)"`).FindAllStringSubmatch(body, -1) {
		found[m[1]] = true
	}
	want := []string{"/leads/sending", "/leads/batches/new", "/leads/suppress", "/leads/messages/11/resolve",
		"/leads/items/9/done", "/leads/contacted", "/leads/suppressions/lift"}
	if len(found) != len(want) {
		t.Errorf("the page posts to %v, want %v", found, want)
	}
	for _, p := range want {
		if !found[p] {
			t.Errorf("the page lost its form to %s", p)
		}
		tried := 0
		for _, rt := range leadsPosts() {
			if numbered.ReplaceAllString(rt.path, "/N$1") != numbered.ReplaceAllString(p, "/N$1") {
				continue
			}
			tried++
			b := richBackend("x")
			ph := newHarness(t, b, false)
			w := ph.do(post(rt.path, rt.form, ph.session(t)))
			if w.Code != http.StatusSeeOther || w.Header().Get("Location") != leadsOutPath {
				t.Errorf("%s: %d to %q, want 303 to %s", rt.path, w.Code, w.Header().Get("Location"), leadsOutPath)
			}
		}
		if tried == 0 {
			t.Errorf("no write test covers %s", p)
		}
	}
	// A batch discarded from its own page goes back to the Outreach page too.
	if w := h.do(post("/leads/batches/3/discard", url.Values{}, c)); w.Header().Get("Location") != leadsOutPath {
		t.Errorf("discard went to %q", w.Header().Get("Location"))
	}
}

// TestOldLeadsLinksPointToOutreach: every link that went to the old /leads page goes to
// /leads/outreach; only the nav, the sub-nav and the list's own links still name /leads.
func TestOldLeadsLinksPointToOutreach(t *testing.T) {
	nav := regexp.MustCompile(`<a href="/leads"( aria-current="(page|true)")?>(Leads|List)</a>`)
	b := richBackend("x")
	b.dash.Leads = &LeadsSummary{}
	h := newHarness(t, b, false)
	c := h.session(t)
	off := richBackend("x")
	off.leadsErr = ErrLeadsNotSetUp
	oh := newHarness(t, off, false)
	for name, body := range map[string]string{
		"dashboard":  h.do(get("/", c)).Body.String(),
		"batch":      h.do(get("/leads/batches/3", c)).Body.String(),
		"mailboxes":  h.do(get("/mailboxes", c)).Body.String(),
		"not set up": oh.do(get("/leads/outreach", oh.session(t))).Body.String(),
		"outreach":   h.do(get("/leads/outreach", c)).Body.String(),
		"settings":   h.do(get("/leads/settings", c)).Body.String(),
	} {
		if !strings.Contains(body, `href="/leads/outreach"`) {
			t.Errorf("%s has no link to /leads/outreach", name)
		}
		if strings.Contains(nav.ReplaceAllString(body, ""), `href="/leads"`) {
			t.Errorf("%s still links to /leads outside the nav", name)
		}
	}
	// In the templates, /leads is named only by the sub-nav and the list's own pages.
	entries, err := templateFS.ReadDir("templates")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		raw, err := templateFS.ReadFile("templates/" + e.Name())
		if err != nil {
			t.Fatal(err)
		}
		has := strings.Contains(string(raw), `href="/leads"`)
		if allowed := slices.Contains([]string{"leads_tabs.html", "leads_list.html", "lead_detail.html"}, e.Name()); has && !allowed {
			t.Errorf("%s links to /leads", e.Name())
		}
	}
	w := h.do(get("/leads/next", c))
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/leads?sort=order" {
		t.Errorf("/leads/next: %d to %q", w.Code, w.Header().Get("Location"))
	}
}

// TestLeadsRoutesDoNotShadow: each literal /leads/... page reaches its own read, never the
// lead page's; a path that is not an id reaches no read at all.
func TestLeadsRoutesDoNotShadow(t *testing.T) {
	cases := []struct {
		path, read string
		code       int
	}{
		{"/leads", "LeadList", http.StatusOK},
		{"/leads/outreach", "Leads", http.StatusOK},
		{"/leads/settings", "LeadSettings", http.StatusOK},
		{"/leads/campaigns", "Campaigns", http.StatusOK},
		{"/leads/priorities", "", http.StatusSeeOther},
		{"/leads/batches/3", "LeadBatch", http.StatusOK},
		{"/leads/5", "LeadDetail", http.StatusOK},
		{"/leads/next", "", http.StatusSeeOther},
		{"/leads/batches", "", http.StatusNotFound},
		{"/leads/settings/who", "", http.StatusMethodNotAllowed},
		{"/leads/5/star", "", http.StatusMethodNotAllowed},
	}
	for _, c := range cases {
		b := richBackend("x")
		h := newHarness(t, b, false)
		w := h.do(get(c.path, h.session(t)))
		if w.Code != c.code {
			t.Errorf("%s: %d, want %d", c.path, w.Code, c.code)
		}
		switch {
		case c.read == "" && b.total() != 0:
			t.Errorf("%s reached the backend %d times", c.path, b.total())
		case c.read != "" && (b.n(c.read) != 1 || b.total() != 1):
			t.Errorf("%s: %s called %d times, %d reads in all", c.path, c.read, b.n(c.read), b.total())
		}
	}
}

// TestStarActions: every row of the list and the lead's own page carry a star toggle that
// names the lead's next state; each move posts its action for that lead and comes back to
// the page it was sent from with the outcome; nothing else is sent to the backend.
func TestStarActions(t *testing.T) {
	b := richBackend("Northwind Trading")
	b.list.Rows = append(b.list.Rows, LeadListRow{LeadID: 6, Name: "Contoso", Domain: "contoso.example"})
	h := newHarness(t, b, false)
	c := h.session(t)
	list := h.do(get("/leads", c)).Body.String()
	for _, want := range []string{
		`action="/leads/5/unstar" class="star-form"`, `action="/leads/6/star" class="star-form"`,
		`<span class="visually-hidden">Star Contoso</span>`, `title="Star"`, `title="Unstar"`,
	} {
		if !strings.Contains(list, want) {
			t.Errorf("/leads lacks %q", want)
		}
	}
	if n := strings.Count(list, `class="star-form"`); n != 4 {
		t.Errorf("%d star toggles on the list, want one per row in each layout (4)", n)
	}
	detail := h.do(get("/leads/5", c)).Body.String()
	if !strings.Contains(detail, `<form method="post" action="/leads/5/star" class="star-form"><input type="hidden" name="back" value="/leads/5">`) ||
		!strings.Contains(detail, "☆</span> Star</button>") {
		t.Errorf("the lead page lacks its star toggle: %s", detail)
	}
	b.detail.Starred = true
	if detail := h.do(get("/leads/5", c)).Body.String(); !strings.Contains(detail, `action="/leads/5/unstar"`) ||
		!strings.Contains(detail, "★</span> Unstar</button>") {
		t.Error("a starred lead's page does not offer to unstar it")
	}

	for _, do := range []string{LeadStar, LeadUnstar, LeadUp, LeadDown, LeadTop, LeadDefer, LeadSkip, LeadClear} {
		b.leadOut = LeadOutcome{Message: "Lead 5: " + do + " done."}
		w := h.do(post("/leads/5/"+do, url.Values{"back": {"/leads?sort=order#lead-w-5"}}, c))
		if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/leads?sort=order#lead-w-5" {
			t.Errorf("%s: %d to %q", do, w.Code, w.Header().Get("Location"))
			continue
		}
		if !sameAction(b.lastLead, LeadAction{Do: do, ID: 5}) {
			t.Errorf("%s asked %+v", do, b.lastLead)
		}
		page := h.do(get("/leads?sort=order", c, cookieNamed(w, "__Host-lh_flash"))).Body.String()
		if !strings.Contains(page, "Lead 5: "+do+" done.") {
			t.Errorf("%s: the list does not show the outcome", do)
		}
	}
}

// TestOrderRowsOfferMoves: sorted by the outreach order, each starred row shows its place
// and Up, Down, Pin to top, Push back and Skip, coming back to the same row; its star
// toggle is its Unstar.
func TestOrderRowsOfferMoves(t *testing.T) {
	b := richBackend("Northwind Trading")
	h := newHarness(t, b, false)
	body := h.do(get("/leads?sort=order", h.session(t))).Body.String()
	for _, do := range []string{"up", "down", "top", "defer", "skip"} {
		if n := strings.Count(body, `action="/leads/5/`+do+`"`); n != 2 {
			t.Errorf("%s offered %d times, want once in each layout", do, n)
		}
	}
	for _, want := range []string{">Up<span", ">Down<span", ">Pin to top<span", ">Push back<span", ">Skip<span",
		`<span class="visually-hidden"> Northwind Trading</span>`, `<th scope="col">Move</th>`, `<td class="num">3</td>`,
		"number 3 in the outreach order", `name="back" value="/leads?page=2&amp;sort=order#lead-w-5"`,
		`name="back" value="/leads?page=2&amp;sort=order#lead-n-5"`, `action="/leads/5/unstar"`} {
		if !strings.Contains(body, want) {
			t.Errorf("the order lacks %q", want)
		}
	}
	b.list.Rows[0].Starred = false
	if body := h.do(get("/leads?sort=order", h.session(t))).Body.String(); strings.Contains(body, `action="/leads/5/up"`) {
		t.Error("a lead not starred is offered moves in the order")
	}
}

// TestBackIsRelative: a lead action goes back only to a Leads page on this site; any other
// back value goes to the list.
func TestBackIsRelative(t *testing.T) {
	for in, want := range map[string]string{
		"":                                      "/leads",
		"/leads":                                "/leads",
		"/leads?sort=order":                     "/leads?sort=order",
		"/leads#lead-w-5":                       "/leads#lead-w-5",
		"/leads/5":                              "/leads/5",
		"/leads/outreach":                       "/leads/outreach",
		"/leads?kind=d10&page=2#x":              "/leads?kind=d10&page=2#x",
		"/leads?sort=order&kind=a&kind=b":       "/leads?sort=order&kind=a&kind=b",
		"/leads?q=a%20b&x=%2F%2Fevil":           "/leads?q=a+b&x=%2F%2Fevil",
		"/leads?q=%zz":                          "/leads",
		"/leads/5#a%2Fb":                        "/leads/5#a%2Fb",
		"//evil.example":                        "/leads",
		"//evil.example/leads":                  "/leads",
		"https://evil.example/leads":            "/leads",
		"http:/leads":                           "/leads",
		"javascript:alert(1)":                   "/leads",
		"/leadsx":                               "/leads",
		"/leads.evil.example":                   "/leads",
		"/leads@evil.example":                   "/leads",
		"/leads/":                               "/leads",
		"/leads/../evil":                        "/leads",
		"/leads/5/../../x":                      "/leads",
		"/leads//evil.example":                  "/leads",
		`/leads\evil.example`:                   "/leads",
		`/leads/\evil.example`:                  "/leads",
		"/leads\r\nSet-Cookie: x=1":             "/leads",
		"/leads?x=\x00":                         "/leads",
		"leads":                                 "/leads",
		" /leads":                               "/leads",
		"/jobs":                                 "/leads",
		"/leads" + strings.Repeat("?", maxBack): "/leads",
	} {
		if got := leadsBack(in); got != want {
			t.Errorf("leadsBack(%q) = %q, want %q", in, got, want)
		}
	}
	b := richBackend("x")
	h := newHarness(t, b, false)
	c := h.session(t)
	for _, back := range []string{"//evil.example", "https://evil.example/", "/leadsx"} {
		w := h.do(post("/leads/5/star", url.Values{"back": {back}}, c))
		if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/leads" {
			t.Errorf("back %q: %d to %q, want 303 to /leads", back, w.Code, w.Header().Get("Location"))
		}
	}
}

// TestLeadActCrossOrigin: a lead action from another site is refused before the backend,
// signed in or not, and one without a session never writes.
func TestLeadActCrossOrigin(t *testing.T) {
	b := richBackend("x")
	h := newHarness(t, b, false)
	c := h.session(t)
	for _, do := range leadActions {
		r := post("/leads/5/"+do, url.Values{"back": {"/leads"}}, c)
		r.Header.Set("Sec-Fetch-Site", "cross-site")
		if w := h.do(r); w.Code != http.StatusForbidden {
			t.Errorf("cross-site %s: %d, want 403", do, w.Code)
		}
		r = post("/leads/5/"+do, url.Values{"back": {"/leads"}}, c)
		r.Header.Set("Origin", "https://evil.example")
		if w := h.do(r); w.Code != http.StatusForbidden {
			t.Errorf("cross-origin %s: %d, want 403", do, w.Code)
		}
		if w := h.do(post("/leads/5/"+do, url.Values{})); w.Code == http.StatusSeeOther && strings.HasPrefix(w.Header().Get("Location"), "/leads") {
			t.Errorf("%s without a session went through", do)
		}
	}
	if n := b.n("LeadAct"); n != 0 {
		t.Errorf("LeadAct called %d times", n)
	}
}

// TestLeadActRefusalShowsThePage: a refused action shows the page it came from again, with
// the refusal: the list under the same filter, or the lead's own page.
func TestLeadActRefusalShowsThePage(t *testing.T) {
	b := richBackend("x")
	b.leadErr = fmt.Errorf("%w: Lead 5 is already at the top.", ErrUserFixable)
	h := newHarness(t, b, false)
	c := h.session(t)
	w := h.do(post("/leads/5/up", url.Values{"back": {"/leads?sort=order&kind=d10#lead-w-5"}}, c))
	if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), "Lead 5 is already at the top.") ||
		!strings.Contains(w.Body.String(), `class="lead-filters"`) {
		t.Errorf("list refusal: %d %s", w.Code, w.Body.String())
	}
	if want := (LeadListQuery{Kinds: []string{"d10"}, Sort: "order"}); fmt.Sprint(b.listQ) != fmt.Sprint(want) {
		t.Errorf("the list came back under %+v, want %+v", b.listQ, want)
	}
	w = h.do(post("/leads/5/star", url.Values{"back": {"/leads/5"}}, c))
	if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), "Lead 5 is already at the top.") ||
		!strings.Contains(w.Body.String(), `id="lead-5"`) {
		t.Errorf("lead page refusal: %d %s", w.Code, w.Body.String())
	}
}

// TestLeadListColumns: the table leaves out State under the default filter (open leads), and
// Relevance and Order when no row has one.
func TestLeadListColumns(t *testing.T) {
	b := richBackend("x")
	h := newHarness(t, b, false)
	c := h.session(t)
	head := func(target string) string {
		body := h.do(get(target, c)).Body.String()
		i, j := strings.Index(body, "<thead>"), strings.Index(body, "</thead>")
		if i < 0 || j < i {
			t.Fatalf("%s has no table head", target)
		}
		return body[i:j]
	}
	for target, want := range map[string]map[string]bool{
		"/leads":             {"State": false, "Relevance": true, "Order": true},
		"/leads?status=open": {"State": false, "Relevance": true, "Order": true},
		"/leads?status=all":  {"State": true, "Relevance": true, "Order": true},
	} {
		h := head(target)
		for col, on := range want {
			if strings.Contains(h, ">"+col+"</th>") != on {
				t.Errorf("%s: column %s shown %v, want %v", target, col, !on, on)
			}
		}
	}
	b.list.Rows[0].Relevance, b.list.Rows[0].Position = "", ""
	h2 := head("/leads?status=contacted")
	if strings.Contains(h2, ">Relevance</th>") || strings.Contains(h2, ">Order</th>") || !strings.Contains(h2, ">State</th>") {
		t.Errorf("empty columns shown, or State hidden off the default filter: %s", h2)
	}
}

// TestLeadListFilterForm: a phone number is asked for once ("Has a phone number", has_phone),
// unless the query itself asked for the phone channel; the form folds behind "Filters",
// which counts the filters on; and Sort by has its own wider field.
func TestLeadListFilterForm(t *testing.T) {
	b := richBackend("x")
	b.list.Options.Channels = append(b.list.Options.Channels, FilterOption{"phone", "Phone number"})
	h := newHarness(t, b, false)
	c := h.session(t)
	body := h.do(get("/leads", c)).Body.String()
	if strings.Contains(body, "Phone number</label>") || !strings.Contains(body, `name="has_phone" value="1"> Has a phone number`) {
		t.Error("the phone filter is offered twice, or not at all")
	}
	for _, want := range []string{`<details class="filter-fold">`, "<summary>Filters</summary>", `<div class="filter-sort">`} {
		if !strings.Contains(body, want) {
			t.Errorf("/leads lacks %q", want)
		}
	}
	body = h.do(get("/leads?channel=phone&has_phone=1&kind=d10&status=all", c)).Body.String()
	if !strings.Contains(body, `name="channel" value="phone" checked> Phone number`) {
		t.Error("a phone channel the query asked for is dropped from the form")
	}
	if !strings.Contains(body, "<summary>Filters · 4 on</summary>") {
		t.Error("the folded form does not count the filters on")
	}
	if b.listQ.HasPhone != "1" {
		t.Errorf("has_phone reached the backend as %q", b.listQ.HasPhone)
	}
}

// TestLeadDetailSignalsByFetch: the finding's signals are listed under the fetch they came
// from, each fetch once.
func TestLeadDetailSignalsByFetch(t *testing.T) {
	b := richBackend("x")
	at := testSent
	sig := b.detail.Signals[0]
	b.detail.Signals = []LeadSignal{sig, {Name: "version", Value: "10.2", FetchURL: sig.FetchURL, FinalURL: sig.FinalURL, Status: sig.Status, FetchedAt: &at},
		{Name: "changelog", Value: "10.2.1", FetchURL: "https://northwind.example/CHANGELOG.txt", Status: "200 OK", FetchedAt: &at},
		{Name: "header", Value: "Drupal 10"}}
	h := newHarness(t, b, false)
	body := h.do(get("/leads/5", h.session(t))).Body.String()
	if n := strings.Count(body, `Fetched <a href="https://northwind.example/"`); n != 1 {
		t.Errorf("the shared fetch is shown %d times, want once", n)
	}
	if n := strings.Count(body, `<div class="signal-fetch">`); n != 3 {
		t.Errorf("%d fetch groups, want 3", n)
	}
	i, j, k := strings.Index(body, "generator: x"), strings.Index(body, "version: 10.2"), strings.Index(body, "CHANGELOG.txt")
	if i < 0 || j < i || k < j || !strings.Contains(body, "header: Drupal 10") {
		t.Error("the signals are not under their fetches in order")
	}
	if got := signalFetches(nil); got != nil {
		t.Errorf("no signals made %v", got)
	}
}
