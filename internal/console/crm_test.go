package console

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// noCRM is a backend without the CRM part: the fake with its CRM methods hidden.
type noCRM struct{ Backend }

// newNoCRMHarness is newHarness over a backend that has no CRM.
func newNoCRMHarness(t *testing.T, b *fakeBackend) *harness {
	t.Helper()
	a, clock, calls := testAuth(t)
	logs := &syncBuffer{}
	h := New(noCRM{b}, a, Options{Location: time.UTC, Logger: slog.New(slog.NewTextHandler(logs, nil)), Now: func() time.Time { return statusNow }})
	return &harness{h: h, b: b, a: a, clock: clock, calls: calls, logs: logs, name: "__Host-lh_session"}
}

// crmPages are the CRM pages' paths.
var crmPages = []string{leadsRepliesPath, leadsPipelinePath, leadsCallsPath, leadsReportsPath}

// TestCRMPagesRender: each CRM page shows the backend's data, escaped, with its actions, and
// no inline script or style.
func TestCRMPagesRender(t *testing.T) {
	const evil = "<script>alert(1)</script>"
	h := newHarness(t, richBackend(evil), false)
	c := h.session(t)
	want := map[string][]string{
		leadsRepliesPath: {"<h1>Replies</h1>", `href="/leads/5"`, "“&lt;script&gt;alert(1)&lt;/script&gt;”", "Cafe loyalty",
			`action="/leads/5/crm"`, `name="section" value="stage"`, `name="back" value="/leads/replies"`,
			`name="stage" value="talking" class="btn btn-secondary btn-sm is-current" aria-pressed="true"`,
			`name="stage" value="proposal"`, `name="stage" value="won"`, `href="/leads/5#crm-stage">Lost…`,
			`action="/leads/5/snooze"`, `name="for" value="1d"`, `name="for" value="1w"`},
		leadsPipelinePath: {"<h1>Pipeline</h1>", `id="col-talking"`, `id="col-proposal"`, `id="col-won"`, `id="col-lost"`,
			"£2,400 · €950.50", "£1,200 · €475.25", "RON 1,200", "Halden &amp; Rook", "expected £1,200",
			`<a href="https://copperleaf.example.com" rel="noopener noreferrer nofollow">Tenant</a>`,
			"&lt;script&gt;alert(1)&lt;/script&gt; · Tue 3 Nov", `name="back" value="/leads/pipeline"`},
		leadsCallsPath: {"<h1>Calls</h1>", `href="/leads/calls/9"`, `<a href="tel:&#43;442079460177">&#43;44 20 7946 0177</a>`,
			`action="/leads/calls/9/outcome"`, `name="back" value="/leads/calls"`, `name="call_back_on" type="date" min="2026-10-24"`,
			`name="outcome" value="no_answer"`, "call-back Tue 3 Nov", "Already called", "Not callable now: outside calling hours now"},
		leadsReportsPath: {"<h1>Reports</h1>", "last 30 days, from 24 Sep", "&lt;script&gt;alert(1)&lt;/script&gt;", "<td class=\"num\">212</td>",
			"<th scope=\"col\" class=\"num\">Sent</th><th scope=\"col\" class=\"num\">Contacted</th><th scope=\"col\" class=\"num\">Replied</th>",
			"<td class=\"num\">212</td>\n          <td class=\"num\">180</td>\n          <td class=\"num\">19</td>",
			"Cost per lead divides by the leads contacted (by email, form or call), not by the emails sent.", "9%", "£2,400", "$0.09", "$0.85", "<td class=\"num\">140</td>", "Costs are Claude only", "Email checks are a count", "—"},
	}
	for _, p := range crmPages {
		w := h.do(get(p, c))
		if w.Code != http.StatusOK {
			t.Fatalf("%s: %d", p, w.Code)
		}
		body := w.Body.String()
		for _, s := range want[p] {
			if !strings.Contains(body, s) {
				t.Errorf("%s lacks %q", p, s)
			}
		}
		for _, bad := range []string{"<script>alert", "<style", " style=", "onclick"} {
			if strings.Contains(body, bad) {
				t.Errorf("%s contains %q", p, bad)
			}
		}
		// The call-back row is Done: no outcome form for message 11.
		if p == leadsCallsPath && strings.Contains(body, `action="/leads/calls/11/outcome"`) {
			t.Error("a call already made offers the outcome buttons")
		}
	}
	if got := h.b.queueDay; !got.Equal(time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("the call queue asked for %v, want today", got)
	}
	if got := h.b.reportSince; !got.Equal(time.Date(2026, 9, 24, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("the report asked from %v, want 30 days back", got)
	}
}

// TestCRMPagesColdStart: with nothing stored, each page says so in plain words.
func TestCRMPagesColdStart(t *testing.T) {
	h := newHarness(t, &fakeBackend{}, false)
	c := h.session(t)
	want := map[string]string{
		leadsRepliesPath:  "No replies waiting",
		leadsPipelinePath: "Nothing in the pipeline yet",
		leadsCallsPath:    "No calls today",
		leadsReportsPath:  "Nothing sent in the last 30 days",
	}
	for _, p := range crmPages {
		w := h.do(get(p, c))
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), want[p]) {
			t.Errorf("%s: %d, lacks %q", p, w.Code, want[p])
		}
	}
	body := h.do(get(leadsPipelinePath, c)).Body.String()
	for _, s := range []string{"<dd>0</dd>", "<dd>—</dd>", "None won yet", "None lost yet"} {
		if !strings.Contains(body, s) {
			t.Errorf("the empty pipeline lacks %q", s)
		}
	}
}

// TestCRMPagesNotSetUp: leads not set up shows the not-set-up page, as the other leads pages.
func TestCRMPagesNotSetUp(t *testing.T) {
	b := richBackend("x")
	b.leadsErr = ErrLeadsNotSetUp
	h := newHarness(t, b, false)
	c := h.session(t)
	for _, p := range crmPages {
		if body := h.do(get(p, c)).Body.String(); !strings.Contains(body, "Leads are not set up") {
			t.Errorf("%s does not say leads are not set up", p)
		}
	}
}

// TestCRMOff: a backend with no CRM shows the pages as not on this server yet, the lead page
// without its panels, refuses the CRM writes, and records a call outcome the old way.
func TestCRMOff(t *testing.T) {
	b := richBackend("x")
	h := newNoCRMHarness(t, b)
	c := h.session(t)
	for _, p := range crmPages {
		w := h.do(get(p, c))
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Not on this server yet") {
			t.Errorf("%s: %d without the not-yet line", p, w.Code)
		}
	}
	body := h.do(get("/leads/5", c)).Body.String()
	if strings.Contains(body, `id="crm-stage"`) || strings.Contains(body, "Timeline") {
		t.Error("the lead page shows CRM panels without a CRM backend")
	}
	if !strings.Contains(body, `action="/leads/send-now/preview"`) {
		t.Error("the lead page lost Send now")
	}
	for _, p := range []string{"/leads/5/crm", "/leads/5/snooze"} {
		if w := h.do(post(p, url.Values{"section": {"stage"}, "stage": {"won"}, "for": {"1d"}}, c)); w.Code != http.StatusNotFound {
			t.Errorf("%s without a CRM: %d, want 404", p, w.Code)
		}
	}
	if strings.Contains(h.do(get("/leads/calls/9", c)).Body.String(), `id="call-back-on"`) {
		t.Error("the call card offers a call-back date without a CRM")
	}
	w := h.do(post("/leads/calls/9/outcome", url.Values{"outcome": {"no_answer"}, "call_back_on": {"2026-10-27"}}, c))
	if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), callBackNoCRM) || b.n("LeadCallOutcome") != 0 {
		t.Errorf("a call-back date without a CRM: %d, LeadCallOutcome %d", w.Code, b.n("LeadCallOutcome"))
	}
	w = h.do(post("/leads/calls/9/outcome", url.Values{"outcome": {"no_answer"}}, c))
	if w.Code != http.StatusSeeOther || b.n("LeadCallOutcome") != 1 {
		t.Errorf("an outcome without a CRM: %d, LeadCallOutcome %d", w.Code, b.n("LeadCallOutcome"))
	}
}

// TestLeadPageCRMPanels: the lead page shows the stage, deal, note and timeline panels with the
// stored values, and keeps its Send now and star.
func TestLeadPageCRMPanels(t *testing.T) {
	const evil = "<script>alert(1)</script>"
	h := newHarness(t, richBackend(evil), false)
	body := h.do(get("/leads/5", h.session(t))).Body.String()
	for _, s := range []string{
		`id="crm-stage"`, `<option value="proposal" selected>Proposal</option>`, `name="lost_reason"`, `name="tenant_url"`,
		`id="crm-deal"`, `name="value" inputmode="decimal" value="2400"`, `<option value="GBP" selected>GBP</option>`, "£2,400",
		`id="crm-note"`, `>&lt;script&gt;alert(1)&lt;/script&gt;</textarea>`, `name="next_action_on" type="date" value="2026-11-03"`,
		"Snoozed until Sun 25 Oct 10:32", `name="for" value="clear"`,
		`id="crm-timeline"`, "Reply received", `class="tl-detail">&lt;script&gt;alert(1)&lt;/script&gt;`, "Email sent · step 1 · $0.02",
		`<li class="tl-reply">`, `<li class="tl-email">`,
		`action="/leads/send-now/preview"`, `class="star-form"`,
	} {
		if !strings.Contains(body, s) {
			t.Errorf("the lead page lacks %q", s)
		}
	}
	if strings.Contains(body, "<script>alert") {
		t.Error("the lead page shows the text unescaped")
	}

	// A lead with nothing stored, that replied: Talking, worked out, and an empty timeline.
	b := richBackend("x")
	b.crm = LeadCRM{EffectiveStage: StageTalking}
	b.timeline = nil
	h2 := newHarness(t, b, false)
	body = h2.do(get("/leads/5", h2.session(t))).Body.String()
	for _, s := range []string{`<option value="" selected>No stage`, "it shows as Talking because they replied", "Nothing has happened to this lead yet",
		"Not snoozed.", `<option value="GBP" selected>`} {
		if !strings.Contains(body, s) {
			t.Errorf("the cold lead page lacks %q", s)
		}
	}
}

// TestCRMWrites: a valid form reaches the backend with the values parsed and goes back to the
// page back names (the panel when it is the lead page), with the line.
func TestCRMWrites(t *testing.T) {
	day := time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)
	cents := int64(240050)
	for _, tt := range []struct {
		name  string
		form  url.Values
		check func(*fakeBackend) string
		next  string
		flash string
	}{
		{"stage from replies", url.Values{"section": {"stage"}, "stage": {"proposal"}, "back": {"/leads/replies"}},
			func(b *fakeBackend) string {
				return fmt.Sprint(*b.lastStage == StageChange{LeadID: 5, Stage: StageProposal})
			}, "/leads/replies", "Stage set to Proposal."},
		{"lost with reason, tenant dropped", url.Values{"section": {"stage"}, "stage": {"lost"}, "lost_reason": {" No budget "}, "tenant_url": {"https://x.example"}},
			func(b *fakeBackend) string {
				return fmt.Sprint(*b.lastStage == StageChange{LeadID: 5, Stage: StageLost, LostReason: "No budget"})
			}, "/leads/5#crm-stage", "Stage set to Lost."},
		{"won with tenant, reason dropped", url.Values{"section": {"stage"}, "stage": {"won"}, "lost_reason": {"x"}, "tenant_url": {"https://cafe.example.com"}, "back": {"/leads/5"}},
			func(b *fakeBackend) string {
				return fmt.Sprint(*b.lastStage == StageChange{LeadID: 5, Stage: StageWon, TenantURL: "https://cafe.example.com"})
			}, "/leads/5#crm-stage", "Stage set to Won."},
		{"clear stage", url.Values{"section": {"stage"}, "stage": {""}, "back": {"/leads/pipeline"}},
			func(b *fakeBackend) string { return fmt.Sprint(*b.lastStage == StageChange{LeadID: 5}) },
			"/leads/pipeline", "Stage cleared: it follows the replies again."},
		{"note", url.Values{"section": {"note"}, "note": {" Prefers calls after 16:00 "}, "next_action": {"Call back"}, "next_action_on": {"2026-11-03"}},
			func(b *fakeBackend) string {
				n := b.lastNote
				return fmt.Sprint(n.LeadID == 5 && n.Note == "Prefers calls after 16:00" && n.NextAction == "Call back" && n.NextActionOn.Equal(day))
			}, "/leads/5#crm-note", "Note and next action saved."},
		{"deal", url.Values{"section": {"deal"}, "value": {"2,400.50"}, "currency": {"EUR"}},
			func(b *fakeBackend) string {
				d := b.lastDeal
				return fmt.Sprint(d.LeadID == 5 && *d.ValueCents == cents && d.Currency == "EUR")
			}, "/leads/5#crm-deal", "Deal value saved: €2,400.50."},
		{"deal cleared", url.Values{"section": {"deal"}, "value": {""}, "currency": {"GBP"}},
			func(b *fakeBackend) string { return fmt.Sprint(b.lastDeal.ValueCents == nil) }, "/leads/5#crm-deal", "Deal value cleared."},
		{"evil back", url.Values{"section": {"stage"}, "stage": {"talking"}, "back": {"https://evil.example/"}},
			func(*fakeBackend) string { return "true" }, "/leads", "Stage set to Talking."},
	} {
		t.Run(tt.name, func(t *testing.T) {
			b := richBackend("x")
			h := newHarness(t, b, false)
			c := h.session(t)
			w := h.do(post("/leads/5/crm", tt.form, c))
			if w.Code != http.StatusSeeOther || w.Header().Get("Location") != tt.next {
				t.Fatalf("%d to %q, want 303 to %q: %s", w.Code, w.Header().Get("Location"), tt.next, w.Body.String())
			}
			if got := tt.check(b); got != "true" {
				t.Errorf("the backend was asked otherwise: stage %+v note %+v deal %+v", b.lastStage, b.lastNote, b.lastDeal)
			}
			if got := h.do(get(leadsRepliesPath, c, cookieNamed(w, "__Host-lh_flash"))).Body.String(); !strings.Contains(got, tt.flash) {
				t.Errorf("the flash lacks %q", tt.flash)
			}
		})
	}
}

// TestCRMWriteRefusals: input the console can read but refuses is shown inside its panel with
// the values kept (422) and never reaches the backend; from another page it goes back with
// the sentence as an error. A section the forms never post is 400.
func TestCRMWriteRefusals(t *testing.T) {
	for _, tt := range []struct {
		form     url.Values
		sentence string
		field    string
		kept     string
	}{
		{url.Values{"section": {"stage"}, "stage": {"maybe"}}, badStage, `id="crm-stage-select" name="stage" aria-invalid="true"`, ""},
		{url.Values{"section": {"stage"}, "stage": {"lost"}, "lost_reason": {"  "}}, needLostReason, `name="lost_reason" maxlength="200" value="" autocomplete="off" aria-invalid="true"`, ""},
		{url.Values{"section": {"stage"}, "stage": {"lost"}, "lost_reason": {strings.Repeat("a", 201)}}, lostTooLong, `aria-invalid="true"`, strings.Repeat("a", 201)},
		{url.Values{"section": {"stage"}, "stage": {"won"}, "tenant_url": {"http://cafe.example.com"}}, badTenantURL, `name="tenant_url" type="url" maxlength="500" value="http://cafe.example.com"`, `<option value="won" selected>`},
		{url.Values{"section": {"stage"}, "stage": {"won"}, "tenant_url": {"javascript:alert(1)"}}, badTenantURL, `aria-invalid="true"`, ""},
		{url.Values{"section": {"note"}, "note": {strings.Repeat("n", 2001)}}, crmNoteTooLong, `aria-invalid="true"`, strings.Repeat("n", 2001)},
		{url.Values{"section": {"note"}, "note": {"keep me"}, "next_action": {strings.Repeat("x", 201)}}, nextTooLong, `aria-invalid="true"`, ">keep me</textarea>"},
		{url.Values{"section": {"note"}, "note": {"keep me"}, "next_action_on": {"3 Nov"}}, badDay, `name="next_action_on" type="date" value="3 Nov" aria-invalid="true"`, ">keep me</textarea>"},
		{url.Values{"section": {"note"}, "next_action_on": {"2026-11-03"}}, needNextAction, `name="next_action" maxlength="200" value=""`, `value="2026-11-03"`},
		{url.Values{"section": {"deal"}, "value": {"12abc"}, "currency": {"RON"}}, badDealValue, `name="value" inputmode="decimal" value="12abc"`, `<option value="RON" selected>`},
		{url.Values{"section": {"deal"}, "value": {"1.234"}}, badDealValue, `aria-invalid="true"`, ""},
		{url.Values{"section": {"deal"}, "value": {"1000000000"}}, dealTooBig, `aria-invalid="true"`, ""},
		{url.Values{"section": {"deal"}, "value": {"10"}, "currency": {"JPY"}}, badCurrency, `id="crm-currency" name="currency" aria-invalid="true"`, `value="10"`},
	} {
		t.Run(tt.sentence, func(t *testing.T) {
			b := richBackend("x")
			h := newHarness(t, b, false)
			c := h.session(t)
			form := url.Values{}
			for k, v := range tt.form {
				form[k] = v
			}
			form.Set("back", "/leads/5")
			w := h.do(post("/leads/5/crm", form, c))
			body := w.Body.String()
			if w.Code != http.StatusUnprocessableEntity || !strings.Contains(body, tt.sentence) {
				t.Fatalf("%d, lacks %q", w.Code, tt.sentence)
			}
			for _, s := range []string{tt.field, tt.kept} {
				if !strings.Contains(body, s) {
					t.Errorf("lacks %q", s)
				}
			}
			for _, m := range []string{"SetLeadStage", "SetLeadNote", "SetLeadDeal"} {
				if b.n(m) != 0 {
					t.Errorf("%s was called", m)
				}
			}
			// From the replies inbox: back there with the sentence as an error.
			form.Set("back", "/leads/replies")
			w = h.do(post("/leads/5/crm", form, c))
			if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/leads/replies" {
				t.Fatalf("from replies: %d to %q", w.Code, w.Header().Get("Location"))
			}
			got := h.do(get(leadsRepliesPath, c, cookieNamed(w, "__Host-lh_flash"))).Body.String()
			if !strings.Contains(got, `class="notice notice-error" role="alert">`+escaped(tt.sentence)) {
				t.Errorf("the replies page does not show %q as an error", tt.sentence)
			}
		})
	}
	h := newHarness(t, richBackend("x"), false)
	for _, section := range []string{"", "everything", "Stage"} {
		if w := h.do(post("/leads/5/crm", url.Values{"section": {section}, "stage": {"won"}}, h.session(t))); w.Code != http.StatusBadRequest {
			t.Errorf("section %q: %d, want 400", section, w.Code)
		}
	}
	if n := h.b.n("SetLeadStage"); n != 0 {
		t.Errorf("an unknown section reached the backend %d times", n)
	}
}

// escaped is s as html/template escapes it in text.
func escaped(s string) string {
	return strings.NewReplacer("&", "&amp;", "'", "&#39;", `"`, "&#34;", "<", "&lt;", ">", "&gt;").Replace(s)
}

// TestCRMBackendRefusal: the backend's refusal is shown like the console's own: in the panel
// with its field marked, values kept; ErrNotFound is 404 and a failure 500.
func TestCRMBackendRefusal(t *testing.T) {
	b := richBackend("x")
	b.crmErr = &CRMRefused{Field: "tenant_url", Sentence: "That tenant is not a Webbership tenant."}
	h := newHarness(t, b, false)
	c := h.session(t)
	w := h.do(post("/leads/5/crm", url.Values{"section": {"stage"}, "stage": {"won"}, "tenant_url": {"https://other.example.com"}}, c))
	body := w.Body.String()
	for _, s := range []string{"That tenant is not a Webbership tenant.", `value="https://other.example.com" placeholder="https://cafe.example.com" autocomplete="off" aria-invalid="true"`} {
		if w.Code != http.StatusUnprocessableEntity || !strings.Contains(body, s) {
			t.Errorf("%d, lacks %q", w.Code, s)
		}
	}
	b.crmErr = fmt.Errorf("%w: Stage changes wait until the lead replied.", ErrUserFixable)
	w = h.do(post("/leads/5/crm", url.Values{"section": {"stage"}, "stage": {"talking"}, "back": {"/leads/pipeline"}}, c))
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/leads/pipeline" {
		t.Errorf("a plain refusal from the pipeline: %d to %q", w.Code, w.Header().Get("Location"))
	}
	b.crmErr = ErrNotFound
	if w := h.do(post("/leads/5/crm", url.Values{"section": {"note"}}, c)); w.Code != http.StatusNotFound {
		t.Errorf("not found: %d", w.Code)
	}
	b.crmErr = errors.New("database gone")
	if w := h.do(post("/leads/5/crm", url.Values{"section": {"note"}}, c)); w.Code != http.StatusInternalServerError || strings.Contains(w.Body.String(), "database") {
		t.Errorf("failure: %d %s", w.Code, w.Body.String())
	}
}

// TestSnooze: 1d and 1w snooze from now, clear ends it; anything else is refused before the
// backend.
func TestSnooze(t *testing.T) {
	for _, tt := range []struct {
		for_  string
		until *time.Time
		flash string
	}{
		{"1d", ptr(statusNow.Add(24 * time.Hour)), "Snoozed until Sun 25 Oct 14:32."},
		{"1w", ptr(statusNow.Add(7 * 24 * time.Hour)), "Snoozed until Sat 31 Oct 14:32."},
		{"clear", nil, "Snooze cleared."},
	} {
		b := richBackend("x")
		h := newHarness(t, b, false)
		c := h.session(t)
		w := h.do(post("/leads/5/snooze", url.Values{"for": {tt.for_}, "back": {"/leads/replies"}}, c))
		if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/leads/replies" || b.snoozeCalls != 1 {
			t.Fatalf("%s: %d to %q, %d calls", tt.for_, w.Code, w.Header().Get("Location"), b.snoozeCalls)
		}
		if (tt.until == nil) != (b.lastSnooze == nil) || tt.until != nil && !tt.until.Equal(*b.lastSnooze) {
			t.Errorf("%s: until %v, want %v", tt.for_, b.lastSnooze, tt.until)
		}
		if got := h.do(get(leadsRepliesPath, c, cookieNamed(w, "__Host-lh_flash"))).Body.String(); !strings.Contains(got, tt.flash) {
			t.Errorf("%s: the flash lacks %q", tt.for_, tt.flash)
		}
	}
	b := richBackend("x")
	h := newHarness(t, b, false)
	c := h.session(t)
	for _, bad := range []string{"", "2d", "forever"} {
		w := h.do(post("/leads/5/snooze", url.Values{"for": {bad}}, c))
		if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), badSnooze) {
			t.Errorf("for %q: %d", bad, w.Code)
		}
	}
	if b.snoozeCalls != 0 {
		t.Errorf("a bad snooze reached the backend %d times", b.snoozeCalls)
	}
	w := h.do(post("/leads/5/snooze", url.Values{"for": {"1d"}}, c))
	if w.Header().Get("Location") != "/leads/5#crm-stage" {
		t.Errorf("no back: to %q, want the lead's stage panel", w.Header().Get("Location"))
	}
}

// TestCallBackDate: the outcome form's call-back date reaches the backend parsed; a past day,
// one over a year away, an unreadable one or one with an outcome that ends calling is refused
// with the note kept, and the backend is not called.
func TestCallBackDate(t *testing.T) {
	b := richBackend("x")
	h := newHarness(t, b, false)
	c := h.session(t)
	w := h.do(post("/leads/calls/9/outcome", url.Values{"outcome": {"no_answer"}, "call_back_on": {"2026-10-27"}, "back": {"/leads/calls"}}, c))
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/leads/calls" {
		t.Fatalf("%d to %q", w.Code, w.Header().Get("Location"))
	}
	got := b.lastOutcomeOn
	if got == nil || got.MessageID != 9 || got.Outcome != "no_answer" || got.CallBackOn == nil || !got.CallBackOn.Equal(time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("the backend was asked %+v", got)
	}
	if body := h.do(get(leadsCallsPath, c, cookieNamed(w, "__Host-lh_flash"))).Body.String(); !strings.Contains(body, "Saved: No answer. Call back on Tue 27 Oct.") {
		t.Error("the flash does not name the call-back day")
	}
	w = h.do(post("/leads/calls/9/outcome", url.Values{"outcome": {"skipped"}}, c))
	if w.Code != http.StatusSeeOther || b.lastOutcomeOn.CallBackOn != nil {
		t.Errorf("no date: %d, call-back %v", w.Code, b.lastOutcomeOn.CallBackOn)
	}
	before := b.n("LeadCallOutcomeOn")
	for _, tt := range []struct{ outcome, day, sentence string }{
		{"no_answer", "2026-10-23", callBackPast},
		{"no_answer", "2027-10-25", callBackFar},
		{"no_answer", "27/10/2026", "The call-back date is not a day like 2026-11-03."},
		{"do_not_call", "2026-10-27", callBackNotHere},
		{"wrong_number", "2026-10-27", callBackNotHere},
	} {
		w := h.do(post("/leads/calls/9/outcome", url.Values{"outcome": {tt.outcome}, "call_back_on": {tt.day}, "note": {"kept note"}}, c))
		body := w.Body.String()
		if w.Code != http.StatusUnprocessableEntity || !strings.Contains(body, tt.sentence) || !strings.Contains(body, `value="kept note"`) {
			t.Errorf("%s %s: %d, lacks the refusal or the note", tt.outcome, tt.day, w.Code)
		}
	}
	if n := b.n("LeadCallOutcomeOn"); n != before {
		t.Errorf("a refused call-back reached the backend %d times", n-before)
	}
	if body := h.do(get("/leads/calls/9", c)).Body.String(); !strings.Contains(body, `name="call_back_on" type="date" min="2026-10-24"`) {
		t.Error("the call card lacks the call-back date")
	}
}

// TestPipelineView: cards go to their stage's column in the backend's order; totals are per
// currency and never mixed; open sums and expected count talking and proposal only.
func TestPipelineView(t *testing.T) {
	v := newPipelineView(richCRM("x").pipeline)
	if len(v.Columns) != 4 || v.Columns[0].Stage != StageTalking || v.Columns[3].Stage != StageLost {
		t.Fatalf("columns %+v", v.Columns)
	}
	if got := moneyLine(v.Columns[1].Totals); got != "£2,400 · €950.50" {
		t.Errorf("proposal total %q", got)
	}
	if v.Open != 3 || moneyLine(v.OpenSum) != "£2,400 · €950.50" || moneyLine(v.Expected) != "£1,200 · €475.25" || moneyLine(v.WonSum) != "RON 1,200" {
		t.Errorf("open %d sum %q expected %q won %q", v.Open, moneyLine(v.OpenSum), moneyLine(v.Expected), moneyLine(v.WonSum))
	}
	if v.Empty || !newPipelineView(nil).Empty {
		t.Error("Empty is wrong")
	}
	if got := newPipelineView([]PipelineCard{{Stage: "nurture"}}); !got.Empty {
		t.Error("a stage the console does not know was shown")
	}
}

// TestMoneyAndDeal: amounts are worded per currency; the deal field takes digits, commas and
// two decimals.
func TestMoneyAndDeal(t *testing.T) {
	for m, want := range map[Money]string{
		{"GBP", 240000}: "£2,400", {"EUR", 95050}: "€950.50", {"RON", 5}: "RON 0.05", {"USD", 100000000}: "$1,000,000", {"", 100}: "GBP 1",
	} {
		if got := MoneyWords(m); got != want {
			t.Errorf("%+v: %q, want %q", m, got, want)
		}
	}
	for in, want := range map[string]int64{"2400": 240000, "2,400": 240000, "2400.5": 240050, "0": 0, "99999999.99": 9999999999, "100000000": 10000000000} {
		got, refusal := parseDeal(in)
		if refusal != "" || *got != want {
			t.Errorf("%q: %v %q, want %d", in, got, refusal, want)
		}
	}
	for _, in := range []string{"-1", "1.", ".5", "1.234", "1e3", "£10", "1 000"} {
		if _, refusal := parseDeal(in); refusal == "" {
			t.Errorf("%q was taken", in)
		}
	}
	if got, refusal := parseDeal(""); got != nil || refusal != "" {
		t.Error("an empty deal is not a clear")
	}
	if centsInput(240050) != "2400.50" || centsInput(240000) != "2400" || centsInput(5) != "0.05" {
		t.Error("centsInput")
	}
}

// TestSubnavCRMLinks: the Leads sub-nav carries the four CRM pages.
func TestSubnavCRMLinks(t *testing.T) {
	h := newHarness(t, richBackend("x"), false)
	body := h.do(get(leadsPipelinePath, h.session(t))).Body.String()
	for _, s := range []string{`<a href="/leads/replies">Replies</a>`, `<a href="/leads/pipeline" aria-current="page">Pipeline</a>`,
		`<a href="/leads/calls">Calls</a>`, `<a href="/leads/reports">Reports</a>`, `<a href="/leads/campaigns">Campaigns</a>`} {
		if !strings.Contains(body, s) {
			t.Errorf("the sub-nav lacks %q", s)
		}
	}
}
