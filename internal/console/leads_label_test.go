package console

import (
	"html/template"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// callDetail is richLeadDetail with the operator's call set to band ("" none), 12 leads of 30
// labelled, and every band carrying help.
func callDetail(band string) LeadDetailPage {
	d := richLeadDetail("Northwind Trading")
	d.Call = band
	at := testSent
	d.YourCall = LeadCall{Labelled: 12, LabelGoal: 30}
	if band != "" {
		d.YourCall.At = &at
	}
	for _, b := range LabelBands {
		d.YourCall.Bands = append(d.YourCall.Bands, CallBand{Value: b, Label: strings.ToUpper(b[:1]) + b[1:], Help: "Help for " + b + " & more."})
	}
	return d
}

// TestLeadCallBlock: a lead's page carries "Your call": the call and when, Jev's number
// beside it, one button per band with its meaning as a tooltip and in a list, Clear only
// when there is a call, the answer-key help line, and how many leads are labelled with a
// link to the leads to label.
func TestLeadCallBlock(t *testing.T) {
	b := richBackend("x")
	b.detail = callDetail("likely")
	h := newHarness(t, b, false)
	c := h.session(t)
	body := h.do(get("/leads/5", c)).Body.String()
	for _, want := range []string{
		`<h2 id="call-title">Your call</h2>`,
		`You said <strong>likely</strong> on `,
		"Jev says Northwind Trading.",
		`<form method="post" action="/leads/5/label" class="call-form">`,
		`<input type="hidden" name="back" value="/leads/5">`,
		`<button type="submit" name="band" value="strong" title="Help for strong &amp; more." class="btn btn-secondary">Strong</button>`,
		`<button type="submit" name="band" value="likely" title="Help for likely &amp; more." class="btn btn-secondary is-current" aria-current="true">Likely</button>`,
		`<button type="submit" name="band" value="no" title="Help for no &amp; more." class="btn btn-secondary">No</button>`,
		`<button type="submit" name="band" value="clear" class="btn btn-ghost call-clear">Clear</button>`,
		`<dt>Unlikely</dt><dd>Help for unlikely &amp; more.</dd>`,
		"Your call is the answer key. Jev's number only starts ordering your list once it agrees with your calls on at least 30 leads.",
		`12 of 30 labelled · <a href="/leads?to_label=1">Leads to label</a>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the lead page lacks %q", want)
		}
	}
	// The call sits by its evidence: after what was found on their site, before the rest.
	if i, j, k := strings.Index(body, "<h2>From their site</h2>"), strings.Index(body, `<section class="card lead-call" id="your-call"`),
		strings.Index(body, "<h2>Agency page</h2>"); i < 0 || j < i || k < j {
		t.Errorf("Your call is not between From their site and the agency page: %d %d %d", i, j, k)
	}
	// Clear sits on a row of its own, so the bands keep their widths once a call is made.
	if !strings.Contains(body, `</div>
      <div class="call-clear-row"><button type="submit" name="band" value="clear" class="btn btn-ghost call-clear">Clear</button></div>`) {
		t.Error("Clear is not on its own row after the bands")
	}

	b.detail = callDetail("")
	b.detail.Relevance = ""
	body = h.do(get("/leads/5", c)).Body.String()
	for _, want := range []string{"No call yet.", "Jev has not rated it yet.", `value="strong"`} {
		if !strings.Contains(body, want) {
			t.Errorf("with no call, the lead page lacks %q", want)
		}
	}
	for _, bad := range []string{`name="band" value="clear"`, "is-current", "You said"} {
		if strings.Contains(body, bad) {
			t.Errorf("with no call, the lead page has %q", bad)
		}
	}
}

// TestLabelFlashAtCall: a call goes back to the "Your call" block, and its outcome shows
// inside that block, not at the top of the page; another action's outcome on the same page
// stays at the top. A refused call says so in the block too.
func TestLabelFlashAtCall(t *testing.T) {
	b := richBackend("x")
	b.detail = callDetail("likely")
	const saved = "Saved your call on northwind.example: likely."
	b.leadOut = LeadOutcome{Message: saved}
	h := newHarness(t, b, false)
	c := h.session(t)
	w := h.do(post("/leads/5/label", url.Values{"band": {"likely"}, "back": {"/leads/5"}}, c))
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/leads/5#your-call" {
		t.Fatalf("label: %d to %q, want 303 to /leads/5#your-call", w.Code, w.Header().Get("Location"))
	}
	body := h.do(get("/leads/5", c, cookieNamed(w, "__Host-lh_flash"))).Body.String()
	block := strings.Index(body, `id="your-call"`)
	note := strings.Index(body, `<p class="notice call-notice" role="status">`+saved+`</p>`)
	if block < 0 || note < block || strings.Index(body, "call-now") < note {
		t.Errorf("the call's outcome is not at the top of the call block (block %d, notice %d)", block, note)
	}
	if n := strings.Count(body, saved); n != 1 {
		t.Errorf("the call's outcome shows %d times, want once", n)
	}

	const starred = "Starred northwind.example."
	b.leadOut = LeadOutcome{Message: starred}
	w = h.do(post("/leads/5/star", url.Values{"back": {"/leads/5"}}, c))
	if w.Header().Get("Location") != "/leads/5" {
		t.Errorf("star went back to %q, want /leads/5", w.Header().Get("Location"))
	}
	body = h.do(get("/leads/5", c, cookieNamed(w, "__Host-lh_flash"))).Body.String()
	if i := strings.Index(body, `<p class="notice" role="status">`+starred+`</p>`); i < 0 || i > strings.Index(body, `id="lead-5"`) {
		t.Error("another action's outcome is not at the top of the page")
	}

	body = h.do(post("/leads/5/label", url.Values{"band": {"great"}, "back": {"/leads/5"}}, c)).Body.String()
	if i := strings.Index(body, `<p class="notice call-notice notice-error" role="alert">`); i < strings.Index(body, `id="your-call"`) {
		t.Error("a refused call does not say so in the call block")
	}
}

// TestLabelBadBand: a band the form did not offer is refused before the backend with 422 and
// the lead's page again, saying what to choose; a missing band too.
func TestLabelBadBand(t *testing.T) {
	for _, form := range []url.Values{
		{"band": {"great"}}, {"band": {"Strong"}}, {"band": {""}}, {}, {"band": {"clear "}, "back": {"/leads/5"}},
	} {
		b := richBackend("x")
		b.detail = callDetail("")
		h := newHarness(t, b, false)
		if form.Get("band") == "clear " {
			// Trimmed, "clear " is Clear: the one value here that does reach the backend.
			if w := h.do(post("/leads/5/label", form, h.session(t))); w.Code != http.StatusSeeOther || b.lastLead.Value != LabelClear {
				t.Errorf("clear with a space: %d, asked %+v", w.Code, b.lastLead)
			}
			continue
		}
		form.Set("back", "/leads/5")
		w := h.do(post("/leads/5/label", form, h.session(t)))
		if w.Code != http.StatusUnprocessableEntity {
			t.Errorf("band %q: %d, want 422", form.Get("band"), w.Code)
		}
		if body := w.Body.String(); !strings.Contains(body, template.HTMLEscapeString(badBand)) || !strings.Contains(body, `id="lead-5"`) {
			t.Errorf("band %q: the refusal does not show the lead's page with its words:\n%s", form.Get("band"), body)
		}
		if n := b.n("LeadAct"); n != 0 {
			t.Errorf("band %q: LeadAct called %d times", form.Get("band"), n)
		}
	}
}

// TestToLabelList: the To label filter reaches the backend, stays on in the pager and the
// form, counts as a filter, shows the Your call column with each lead's call and the plain
// help; the list without it shows the progress and a link to it.
func TestToLabelList(t *testing.T) {
	b := richBackend("x")
	b.list.Labelled, b.list.LabelGoal = 7, 30
	b.list.Rows[0].Call = "maybe"
	h := newHarness(t, b, false)
	c := h.session(t)
	body := h.do(get("/leads?to_label=1", c)).Body.String()
	if b.listQ.ToLabel != "1" {
		t.Errorf("the backend was asked %+v, want ToLabel 1", b.listQ)
	}
	for _, want := range []string{
		"<strong>7 of 30 labelled.</strong>", `<a href="/leads">Show every lead</a>`,
		"These are 30 leads picked across kinds, Drupal versions and countries, plus every lead you have already labelled.",
		`<th scope="col">Your call</th>`, `<td class="call-cell"><span class="badge badge-accent">maybe</span></td>`, `title="x"`,
		`Your call: <span class="badge badge-accent">maybe</span>`,
		`name="to_label" value="1" checked`, "Filters · 1 on", "to_label=1",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/leads?to_label=1 lacks %q", want)
		}
	}
	b.list.Rows[0].Call = ""
	// No call reads the same at every width, and never wraps in the table.
	body = h.do(get("/leads?to_label=1", c)).Body.String()
	for _, want := range []string{`<td class="call-cell"><span class="muted">No call yet</span></td>`, `<span class="item-meta">No call yet</span>`} {
		if !strings.Contains(body, want) {
			t.Errorf("/leads?to_label=1 with no call lacks %q", want)
		}
	}
	body = h.do(get("/leads", c)).Body.String()
	for _, want := range []string{"<strong>7 of 30 labelled.</strong>", `<a href="/leads?to_label=1">To label</a>`,
		"Your calls are the answer key Jev is checked against."} {
		if !strings.Contains(body, want) {
			t.Errorf("/leads lacks %q", want)
		}
	}
	if strings.Contains(body, `<th scope="col">Your call</th>`) || strings.Contains(body, `name="to_label" value="1" checked`) {
		t.Error("/leads shows the Your call column with no call, or the To label box ticked")
	}
}

// TestLabelPagesKeepCSP: the lead page with its call block and the To label list carry the
// content security policy and no inline script or style.
func TestLabelPagesKeepCSP(t *testing.T) {
	b := richBackend("x")
	b.detail = callDetail("strong")
	b.list.LabelGoal = 30
	h := newHarness(t, b, false)
	c := h.session(t)
	for _, p := range []string{"/leads/5", "/leads?to_label=1"} {
		w := h.do(get(p, c))
		if got := w.Header().Get("Content-Security-Policy"); got != contentSecurityPolicy {
			t.Errorf("%s: CSP %q", p, got)
		}
		for _, bad := range []string{"<style", " style=", "hx-on", "onclick", "<script>"} {
			if strings.Contains(w.Body.String(), bad) {
				t.Errorf("%s contains %q", p, bad)
			}
		}
	}
}
