package console

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
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

func (f *fakeBackend) LeadsNext(context.Context) (LeadsQueue, error) {
	f.count("LeadsNext")
	return f.queue, f.leadsRead()
}

func (f *fakeBackend) LeadPriorities(_ context.Context, edit *PriorityForm) (PrioritiesPage, error) {
	f.count("LeadPriorities")
	f.mu.Lock()
	f.lastEdit = edit
	f.mu.Unlock()
	p := f.prios
	if edit != nil {
		p.Edited, p.Form = true, *edit
	}
	return p, f.leadsRead()
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
		Batches:      []WaitingBatch{{ID: 3, CreatedAt: testSent, Leads: 4, Emails: 3, Forms: 1}},
		Manual:       []ManualItem{{MessageID: 9, BatchID: 2, Domain: text, Channel: "form", Route: text}},
		CheckByHand:  []CheckItem{{MessageID: 11, Domain: text, Step: 1, State: "maybe_sent", At: &at}},
		Replies:      []string{text},
		Other:        2,
		Checked:      40,
		Drupal:       12,
		Suppressions: Suppressions{Counts: []SuppressionCount{{Kind: "domain", Reason: text, N: 1}}, Domains: []string{text}, Addresses: []string{"j***@" + text}},
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
		{"/leads/batches/3/discard", url.Values{}, "/leads", LeadAction{Do: LeadDiscard, ID: 3}},
		{"/leads/batches/new", url.Values{}, "/leads", LeadAction{Do: LeadNewBatch}},
		{"/leads/messages/9/resolve", url.Values{"sent": {"not-sent"}}, "/leads", LeadAction{Do: LeadResolve, ID: 9}},
		{"/leads/messages/9/resolve", url.Values{"sent": {"sent"}}, "/leads", LeadAction{Do: LeadResolve, ID: 9, Sent: true}},
		{"/leads/items/9/done", url.Values{}, "/leads", LeadAction{Do: LeadDone, ID: 9}},
		{"/leads/suppress", url.Values{"kind": {"domain"}, "value": {" northwind.example "}, "reason": {"manual"}}, "/leads",
			LeadAction{Do: LeadSuppress, Kind: "domain", Value: "northwind.example", Reason: "manual"}},
		{"/leads/contacted", url.Values{"domain": {"northwind.example"}, "note": {"called"}}, "/leads",
			LeadAction{Do: LeadContacted, Value: "northwind.example", Note: "called"}},
		{"/leads/sending", url.Values{"do": {"pause"}, "reason": {"holiday"}}, "/leads", LeadAction{Do: LeadPause, Reason: "holiday"}},
		{"/leads/sending", url.Values{"do": {"resume"}}, "/leads", LeadAction{Do: LeadResume}},
		{"/leads/next/5/pin", url.Values{}, "/leads/next", LeadAction{Do: LeadPin, ID: 5}},
		{"/leads/next/5/defer", url.Values{"until": {"2026-11-07"}}, "/leads/next", LeadAction{Do: LeadDefer, ID: 5, Until: "2026-11-07"}},
		{"/leads/next/5/skip", url.Values{}, "/leads/next", LeadAction{Do: LeadSkip, ID: 5}},
		{"/leads/next/5/clear", url.Values{}, "/leads/next", LeadAction{Do: LeadClear, ID: 5}},
		{"/leads/priorities", url.Values{"do": {"save"}, "kind.unsupported": {"25"}, "countries": {"GB 20"}}, "/leads/priorities",
			LeadAction{Do: LeadSavePriority, Form: PriorityForm{Points: []PointField{{Key: "kind.unsupported", Value: "25"}}, Countries: "GB 20"}}},
		{"/leads/priorities", url.Values{"do": {"reset"}}, "/leads/priorities", LeadAction{Do: LeadResetPrio}},
	}
}

func sameAction(a, b LeadAction) bool {
	return a.Do == b.Do && a.ID == b.ID && a.Hash == b.Hash && slices.Equal(a.Skip, b.Skip) && a.Kind == b.Kind &&
		a.Value == b.Value && a.Reason == b.Reason && a.Note == b.Note && a.Until == b.Until && a.Sent == b.Sent &&
		fmt.Sprint(a.Form) == fmt.Sprint(b.Form)
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
		{"/leads/next/5/promote", url.Values{}, http.StatusNotFound},
		{"/leads/next/0/pin", url.Values{}, http.StatusNotFound},
		{"/leads/batches/x/approve", url.Values{}, http.StatusNotFound},
		{"/leads/priorities", url.Values{"do": {"drop"}}, http.StatusUnprocessableEntity},
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

func TestPrioritiesPreviewWritesNothing(t *testing.T) {
	b := richBackend("x")
	h := newHarness(t, b, false)
	form := url.Values{"do": {"preview"}, "major.7": {"40"}, "kind.d10": {""}, "rank_bands": {"10000 15"}, "exclude_countries": {"FR"}}
	w := h.do(post("/leads/priorities", form, h.session(t)))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	if b.n("LeadAct") != 0 {
		t.Error("a preview wrote")
	}
	want := PriorityForm{Points: []PointField{{Key: "kind.d10"}, {Key: "major.7", Value: "40"}}, RankBands: "10000 15", Exclude: "FR"}
	if b.lastEdit == nil || fmt.Sprint(*b.lastEdit) != fmt.Sprint(want) {
		t.Errorf("edit %+v, want %+v", b.lastEdit, want)
	}
	if body := w.Body.String(); !strings.Contains(body, "Your edit, not saved yet.") || !strings.Contains(body, "under this edit") {
		t.Errorf("preview page: %s", body)
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
	for _, p := range []string{"/leads", "/leads/next", "/leads/priorities", "/leads/batches/3"} {
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
	for _, p := range []string{"/leads/batches/99", "/jobs/99"} {
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
	for _, k := range []string{"hash", "kind", "value", "reason", "domain", "note", "until", "countries", "rank_bands", "exclude_countries", "exact_version", "kind.unsupported"} {
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
		target := strings.NewReplacer("{id}", secretID, "{lead}", secretID, "{action}", "defer").Replace(path)
		for _, do := range []string{"pause", "save", "preview"} {
			for _, e := range errs {
				b := richBackend("x")
				b.leadErr = e
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
	for _, name := range []string{"LeadAct", "AddRule", "DisableRule", "Track"} {
		if n := h.b.n(name); n != 0 {
			t.Errorf("a GET called %s %d times", name, n)
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
		"/leads": {"Sending is on.", "2 of 4", "DMARC <span class=\"bad\">missing</span>", `href="/leads/batches/3"`,
			`action="/leads/batches/new"`, "Make the next batch now", `action="/leads/messages/11/resolve"`, `action="/leads/items/9/done"`,
			`name="value" value="northwind.example"`, "Pause sending", `action="/leads/contacted"`, "j***@northwind.example"},
		"/leads/next":       {"Score 55: northwind.example · GB &#43;20", `action="/leads/next/5/pin"`, `value="2026-11-07"`, `action="/leads/next/5/skip"`},
		"/leads/priorities": {`name="kind.unsupported" value="20"`, "The defaults from the leads settings file.", "The top 20 now"},
		"/leads/batches/3":  {"1. northwind.example", "To: northwind.example", "Subject: northwind.example", "Finding: northwind.example"},
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
	body := ph.do(get("/leads", ph.session(t))).Body.String()
	for _, want := range []string{"Sending is paused (since Sat 24 Oct 04:50): holiday.", "Resume sending"} {
		if !strings.Contains(body, want) {
			t.Errorf("paused /leads lacks %q", want)
		}
	}
	if strings.Contains(body, "Pause sending") {
		t.Error("a paused page offers to pause")
	}
	b.leads = LeadsPage{}
	eh := newHarness(t, b, false)
	body = eh.do(get("/leads", eh.session(t))).Body.String()
	for _, want := range []string{"Sending isn't set up on this server yet.", "No batch waits for approval.", "No replies since yesterday.",
		"Nothing to check.", "None waiting.", "No suppressions.", "The sender's records have not been checked yet."} {
		if !strings.Contains(body, want) {
			t.Errorf("empty /leads lacks %q", want)
		}
	}
}

func TestLeadsEscaping(t *testing.T) {
	const evil = "<script>alert(1)</script>"
	h := newHarness(t, richBackend(evil), false)
	c := h.session(t)
	for _, p := range []string{"/leads", "/leads/next", "/leads/priorities", "/leads/batches/3", "/jobs/42"} {
		body := h.do(get(p, c)).Body.String()
		if strings.Contains(body, "<script>alert") || !strings.Contains(body, "&lt;script&gt;alert(1)&lt;/script&gt;") {
			t.Errorf("%s does not escape backend text", p)
		}
	}
}
