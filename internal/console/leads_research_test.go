package console

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func (f *fakeBackend) LeadReport(_ context.Context, id int64) (LeadReport, error) {
	f.count("LeadReport")
	if err := f.leadsRead(); err != nil {
		return LeadReport{}, err
	}
	if f.reportErr != nil {
		return LeadReport{}, f.reportErr
	}
	if id != f.detail.LeadID {
		return LeadReport{}, ErrNotFound
	}
	return f.report, nil
}

// TestStarNotePerChannel: a lead with no usable route says up front what starring it does,
// on the list (both layouts) and on its page; a starred lead, or one with no note, says
// nothing.
func TestStarNotePerChannel(t *testing.T) {
	for _, note := range []string{StarNoteEmail, StarNotePhone} {
		b := richBackend("Northwind Trading")
		b.list.Rows[0].Starred, b.list.Rows[0].StarNote = false, note
		b.detail.StarNote = note
		h := newHarness(t, b, false)
		c := h.session(t)
		list := h.do(get("/leads", c)).Body.String()
		if n := strings.Count(list, `<span class="item-meta star-note">`+note+`</span>`); n != 2 {
			t.Errorf("%q shown %d times on the list, want once in each layout", note, n)
		}
		detail := h.do(get("/leads/5", c)).Body.String()
		if !strings.Contains(detail, `<p class="hint star-note">`+note+`.</p>`) {
			t.Errorf("the lead page lacks the star note %q", note)
		}
		b.list.Rows[0].Starred, b.detail.Starred = true, true
		if strings.Contains(h.do(get("/leads", c)).Body.String(), "star-note") ||
			strings.Contains(h.do(get("/leads/5", c)).Body.String(), "star-note") {
			t.Errorf("a starred lead still shows %q", note)
		}
	}
	if StarNoteEmail != "No email yet: starring it looks for one" ||
		StarNotePhone != "Calls-first country: starring it looks for a business phone" {
		t.Error("the star notes changed their words")
	}
	h := newHarness(t, richBackend("x"), false)
	if strings.Contains(h.do(get("/leads/5", h.session(t))).Body.String(), "star-note") {
		t.Error("a lead with no note shows one")
	}
}

// TestStarShowsResearchOutcome: the star's line, research words included, comes back on the
// page the star was pressed on; the console only passes the star to the backend (which
// queues the research and writes the request file), never anything else.
func TestStarShowsResearchOutcome(t *testing.T) {
	b := richBackend("Northwind Trading")
	b.leadOut = LeadOutcome{Message: "Lead 5 starred: it is last in your order. It has no email yet, so a search for one is queued for the research job."}
	h := newHarness(t, b, false)
	c := h.session(t)
	w := h.do(post("/leads/5/star", url.Values{"back": {"/leads/5"}}, c))
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/leads/5" {
		t.Fatalf("star: %d to %q", w.Code, w.Header().Get("Location"))
	}
	if !sameAction(b.lastLead, LeadAction{Do: LeadStar, ID: 5}) || b.n("LeadAct") != 1 {
		t.Errorf("star asked %+v, %d writes", b.lastLead, b.n("LeadAct"))
	}
	page := h.do(get("/leads/5", c, cookieNamed(w, "__Host-lh_flash"))).Body.String()
	if !strings.Contains(page, "It has no email yet, so a search for one is queued for the research job.") {
		t.Error("the lead page does not show the star's research line")
	}
}

func TestResearchLine(t *testing.T) {
	at := time.Date(2026, 10, 28, 9, 15, 0, 0, time.UTC)
	for _, c := range []struct {
		r    LeadResearch
		want string
	}{
		{LeadResearch{}, ""},
		{LeadResearch{Status: ResearchQueued, Want: []string{WantEmail}, At: &at}, "Asked Wed 28 Oct 09:15: waiting for the research job to look for an email."},
		{LeadResearch{Status: ResearchLooking, Want: []string{WantPhone}, At: &at}, "Looking for a business phone since Wed 28 Oct 09:15."},
		{LeadResearch{Status: ResearchFound, Want: []string{WantEmail}, Verified: 1, At: &at}, "Found 1 email on 28 Oct 2026."},
		{LeadResearch{Status: ResearchFound, Want: []string{WantPhone}, Verified: 2, At: &at}, "Found 2 business phones on 28 Oct 2026."},
		{LeadResearch{Status: ResearchNothing, Want: []string{WantEmail}, At: &at}, "Found nothing on 28 Oct 2026."},
		{LeadResearch{Status: ResearchCapped, At: &at}, "Paused on 28 Oct 2026: today's spend cap was reached before it looked."},
		{LeadResearch{Status: ResearchError, At: &at}, "Stopped by an error on 28 Oct 2026."},
	} {
		if got := researchLine(c.r, time.UTC); got != c.want {
			t.Errorf("%s: %q, want %q", c.r.Status, got, c.want)
		}
	}
}

// TestLeadPageResearchStates: the "Contact research" line says the latest attempt in plain
// words; "Look again" posts back to the line, is hidden while queued or looking, and inside
// the 90 days the page says the date it can run instead.
func TestLeadPageResearchStates(t *testing.T) {
	at := time.Date(2026, 10, 28, 9, 15, 0, 0, time.UTC)
	next := at.AddDate(0, 0, 90)
	button := `<form method="post" action="/leads/5/look-again" class="research-form">`
	for _, c := range []struct {
		name       string
		r          LeadResearch
		line       string
		offer      bool
		hint, none string
	}{
		{"never", LeadResearch{CanAsk: true, Want: []string{WantEmail}}, "Not looked for yet.", true, "an impersonal role email", ""},
		{"queued", LeadResearch{CanAsk: true, Status: ResearchQueued, Want: []string{WantEmail}, At: &at}, "Asked Wed 28 Oct 09:15", false, "", ""},
		{"looking", LeadResearch{CanAsk: true, Status: ResearchLooking, Want: []string{WantPhone}, At: &at}, "Looking for a business phone since", false, "", ""},
		{"found", LeadResearch{CanAsk: true, Status: ResearchFound, Want: []string{WantEmail}, Verified: 1, At: &at, NextAt: &next},
			"Found 1 email on 28 Oct 2026.", false, "It can look again from 26 Jan 2027", ""},
		{"nothing", LeadResearch{CanAsk: true, Status: ResearchNothing, Want: []string{WantPhone}, At: &at, NextAt: &next},
			"Found nothing on 28 Oct 2026.", false, "It can look again from 26 Jan 2027", ""},
		{"capped", LeadResearch{CanAsk: true, Status: ResearchCapped, Want: []string{WantEmail}, At: &at}, "today&#39;s spend cap", true, "", ""},
		{"error past the window", LeadResearch{CanAsk: true, Status: ResearchError, Want: []string{WantEmail}, At: &at}, "Stopped by an error", true, "", ""},
		{"not new", LeadResearch{Status: ResearchFound, Want: []string{WantEmail}, Verified: 1, At: &at}, "Found 1 email", false, "", ""},
		{"off country, never", LeadResearch{}, "", false, "", "Contact research"},
	} {
		t.Run(c.name, func(t *testing.T) {
			b := richBackend("Northwind Trading")
			b.detail.Research = c.r
			h := newHarness(t, b, false)
			body := h.do(get("/leads/5", h.session(t))).Body.String()
			if c.none != "" {
				if strings.Contains(body, c.none) {
					t.Errorf("the page shows %q", c.none)
				}
				return
			}
			if !strings.Contains(body, `<div class="research" id="contact-research">`) || !strings.Contains(body, c.line) {
				t.Errorf("the research line lacks %q", c.line)
			}
			if got := strings.Contains(body, button); got != c.offer {
				t.Errorf("Look again offered = %v, want %v", got, c.offer)
			}
			if c.offer && !strings.Contains(body, `<input type="hidden" name="back" value="/leads/5#contact-research">`) {
				t.Error("Look again does not come back to the research line")
			}
			if c.hint != "" && !strings.Contains(body, c.hint) {
				t.Errorf("the page lacks %q", c.hint)
			}
		})
	}
}

// TestLookAgain: the button asks the backend for look-again on that lead and comes back to
// the research line; a refusal inside the 90 days shows the page again with the date it can
// run.
func TestLookAgain(t *testing.T) {
	b := richBackend("Northwind Trading")
	b.leadOut = LeadOutcome{Message: "Looking again: a search for a role email is queued for the research job."}
	h := newHarness(t, b, false)
	c := h.session(t)
	w := h.do(post("/leads/5/look-again", url.Values{"back": {"/leads/5#contact-research"}}, c))
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/leads/5#contact-research" {
		t.Fatalf("look again: %d to %q", w.Code, w.Header().Get("Location"))
	}
	if !sameAction(b.lastLead, LeadAction{Do: LeadLookAgain, ID: 5}) {
		t.Errorf("look again asked %+v", b.lastLead)
	}

	b.leadErr = fmt.Errorf("%w: Not looked again: research last ran on 28 Oct 2026; it may run again from 26 Jan 2027.", ErrUserFixable)
	w = h.do(post("/leads/5/look-again", url.Values{"back": {"/leads/5#contact-research"}}, c))
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("refused look again: %d", w.Code)
	}
	if body := w.Body.String(); !strings.Contains(body, "it may run again from 26 Jan 2027.") || !strings.Contains(body, `role="alert"`) ||
		!strings.Contains(body, "<h1>Northwind Trading</h1>") {
		t.Errorf("the refusal does not show the date on the lead page: %s", body)
	}
}

// TestBatchRefusalLists: the batch refusal's per-lead list shows as a numbered list under
// its first line, in plain words; a one-line refusal stays one line.
func TestBatchRefusalLists(t *testing.T) {
	b := richBackend("x")
	b.leadErr = fmt.Errorf("%w: No starred leads are ready. Why each starred lead is out:\n1. northwind.example — no email yet\n2. contoso.example — country rule: DE is written to by form only", ErrUserFixable)
	h := newHarness(t, b, false)
	c := h.session(t)
	w := h.do(post("/leads/batches/new", url.Values{}, c))
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("refused batch: %d", w.Code)
	}
	want := `<div class="notice notice-error" role="alert"><p>No starred leads are ready. Why each starred lead is out:</p>` +
		`<ol class="notice-list"><li>northwind.example — no email yet</li><li>contoso.example — country rule: DE is written to by form only</li></ol></div>`
	if body := w.Body.String(); !strings.Contains(body, want) {
		t.Errorf("the refusal list is not shown as a numbered list: %s", body)
	}
	b.leadErr = fmt.Errorf("%w: No starred leads are ready: star some on Leads.", ErrUserFixable)
	if body := h.do(post("/leads/batches/new", url.Values{}, c)).Body.String(); !strings.Contains(body,
		`<p class="notice notice-error" role="alert">No starred leads are ready: star some on Leads.</p>`) {
		t.Error("a one-line refusal is not one line")
	}
}

func TestNoticeLines(t *testing.T) {
	n := noticeLines("Head\n1. a\n\n12. b — c. d\nplain")
	if n.Head != "Head" || strings.Join(n.Items, "|") != "a|b — c. d|plain" {
		t.Errorf("%+v", n)
	}
	if n := noticeLines("One line. Two."); n.Head != "One line. Two." || n.Items != nil {
		t.Errorf("%+v", n)
	}
}

// TestLeadReportRoute: the report downloads as an HTML file with a clean name to the
// signed-in operator; no such lead is 404; a lead with no report shows its page with the
// reason; signed out is sent to sign in and the backend is never asked.
func TestLeadReportRoute(t *testing.T) {
	b := richBackend("Northwind Trading")
	b.report = LeadReport{Filename: "upgrade-risk-northwind.example-2026-10-28.html", HTML: []byte("<!doctype html><title>Report</title>")}
	h := newHarness(t, b, false)
	c := h.session(t)
	w := h.do(get("/leads/reports/5", c))
	if w.Code != http.StatusOK || w.Body.String() != "<!doctype html><title>Report</title>" {
		t.Fatalf("report: %d %q", w.Code, w.Body.String())
	}
	for k, v := range map[string]string{
		"Content-Type":           "text/html; charset=utf-8",
		"Content-Disposition":    `attachment; filename=upgrade-risk-northwind.example-2026-10-28.html`,
		"Cache-Control":          "no-store",
		"X-Content-Type-Options": "nosniff",
	} {
		if got := w.Header().Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	if b.n("LeadAct") != 0 {
		t.Error("the report wrote something")
	}
	if w := h.do(get("/leads/reports/99", c)); w.Code != http.StatusNotFound {
		t.Errorf("no such lead: %d", w.Code)
	}
	if w := h.do(get("/leads/reports/abc", c)); w.Code != http.StatusNotFound {
		t.Errorf("not an id: %d", w.Code)
	}
	b.reportErr = fmt.Errorf("%w: Lead 5 has no Drupal site finding, so it has no upgrade-risk report.", ErrUserFixable)
	if w := h.do(get("/leads/reports/5", c)); w.Code != http.StatusUnprocessableEntity ||
		!strings.Contains(w.Body.String(), "has no Drupal site finding") || !strings.Contains(w.Body.String(), "<h1>Northwind Trading</h1>") {
		t.Errorf("no report: %d %s", w.Code, w.Body.String())
	}
	b.reportErr = nil
	before := b.n("LeadReport")
	w = h.do(get("/leads/reports/5"))
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/login?next="+url.QueryEscape("/leads/reports/5") {
		t.Errorf("signed out: %d to %q", w.Code, w.Header().Get("Location"))
	}
	if b.n("LeadReport") != before {
		t.Error("signed out reached the backend")
	}
}

// TestLeadPageReportLink: a lead with a report offers the Report link; one without says why
// and links nothing; with nothing to say, there is no report section.
func TestLeadPageReportLink(t *testing.T) {
	b := richBackend("Northwind Trading")
	b.detail.Report = ReportLink{Ready: true}
	h := newHarness(t, b, false)
	c := h.session(t)
	if body := h.do(get("/leads/5", c)).Body.String(); !strings.Contains(body, `<a href="/leads/reports/5" download>Report</a>`) {
		t.Error("a lead with a report does not offer it")
	}
	b.detail.Report = ReportLink{Why: "Lead 5 has no Drupal site finding, so it has no upgrade-risk report."}
	body := h.do(get("/leads/5", c)).Body.String()
	if strings.Contains(body, "/leads/reports/5") || !strings.Contains(body, `<p class="muted">Lead 5 has no Drupal site finding`) {
		t.Error("a lead with no report links it or does not say why")
	}
	b.detail.Report = ReportLink{}
	if strings.Contains(h.do(get("/leads/5", c)).Body.String(), "Upgrade-risk report") {
		t.Error("a lead with nothing to say about its report shows the section")
	}
}

func TestReportFilename(t *testing.T) {
	for in, want := range map[string]string{
		"upgrade-risk-northwind.example-2026-10-28.html": "upgrade-risk-northwind.example-2026-10-28.html",
		"Upgrade Risk \"x\";\r\n.html":                   "upgrade-risk--x.html",
		"":                                               "upgrade-risk-lead-5.html",
		"../..":                                          "upgrade-risk-lead-5.html",
	} {
		if got := reportFilename(in, 5); got != want {
			t.Errorf("reportFilename(%q) = %q, want %q", in, got, want)
		}
	}
}
