package console

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

func (f *fakeBackend) Campaigns(context.Context) ([]CampaignView, error) {
	f.count("Campaigns")
	return f.campaigns, f.leadsRead()
}

func richCampaigns(text string) []CampaignView {
	return []CampaignView{{
		Slug: "drupal-eol", Name: text,
		SpecSHA: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", SpecAt: testSent, SpecSource: "file",
		Kinds: []string{"Drupal 7 or older", "Drupal 10"},
		Countries: []CampaignCountry{
			{Code: "GB", Label: "United Kingdom", Email: true, Call: true, Language: "English"},
			{Code: "DE", Label: "Germany"},
		},
		Steps: []CampaignStep{
			{Step: 0, Type: "email"},
			{Step: 1, Type: "email", AfterBusinessDays: 3},
			{Step: 2, Type: "call", AfterBusinessDays: 2, When: "has_callable_phone"},
			{Step: 3, Type: "email", AfterBusinessDays: 1, When: "call_not_connected"},
		},
		Writer: "template",
	}}
}

// TestCampaignsPage: the Campaigns tab shows each campaign's name, slug, spec version, kinds,
// countries, steps, writer and Claude spend, in words, under the Leads sub-nav.
func TestCampaignsPage(t *testing.T) {
	b := richBackend("Drupal end-of-life")
	h := newHarness(t, b, false)
	w := h.do(get("/leads/campaigns", h.session(t)))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		`<a href="/leads/campaigns" aria-current="page">Campaigns</a>`, `<a href="/leads" aria-current="page">Leads</a>`,
		`<section class="card settings-section campaign" id="campaign-drupal-eol"`,
		`<h2 id="campaign-drupal-eol-title">Drupal end-of-life</h2>`,
		"<dt>Short name</dt><dd><code>drupal-eol</code></dd>",
		"<dt>Version</dt><dd><code>0123456789ab</code> · Sat 24 Oct 04:50 · from its file</dd>",
		"<dt>Lead kinds</dt><dd>Drupal 7 or older, Drupal 10</dd>",
		"<dt>Drafted by</dt><dd>Templates</dd>",
		"<dt>Claude spend today</dt><dd>not in use yet</dd>",
		`<span class="item-head">United Kingdom</span> <span class="item-meta">email on · calls on · writes in English</span>`,
		`<span class="item-head">Germany</span> <span class="item-meta">email off · calls off</span>`,
		`Step 1: an email</span> <span class="item-meta">first</span>`,
		`Step 2: an email</span> <span class="item-meta">3 business days after the step before</span>`,
		`Step 3: a phone call</span> <span class="item-meta">2 business days after the step before · only when the lead has a phone number we may call</span>`,
		`Step 4: an email</span> <span class="item-meta">1 business day after the step before · only when the call before it did not get through</span>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the Campaigns page lacks %q", want)
		}
	}
	for _, code := range []string{"has_callable_phone", "call_not_connected", "0123456789abcdef0123"} {
		if strings.Contains(body, code) {
			t.Errorf("the Campaigns page shows the internal %q", code)
		}
	}

	// Claude as writer and a spend line, a retired campaign, and no campaigns at all.
	b.campaigns[0].Writer, b.campaigns[0].ClaudeSpendToday, b.campaigns[0].Retired = "claude", "$0.84 of $5.00", true
	body = h.do(get("/leads/campaigns", h.session(t))).Body.String()
	for _, want := range []string{"<dt>Drafted by</dt><dd>Claude</dd>", "<dt>Claude spend today</dt><dd>$0.84 of $5.00</dd>", `<span class="tag">retired</span>`} {
		if !strings.Contains(body, want) {
			t.Errorf("the Campaigns page lacks %q", want)
		}
	}
	b.campaigns = nil
	body = h.do(get("/leads/campaigns", h.session(t))).Body.String()
	if !strings.Contains(body, "No campaigns yet.") || strings.Contains(body, "<section class=\"card settings-section campaign\"") {
		t.Errorf("no campaigns: %s", body)
	}
}

// TestCampaignsPageNeedsSession: without a session the page sends to sign-in and never
// reaches the backend; with one, it reads Campaigns once and writes nothing.
func TestCampaignsPageNeedsSession(t *testing.T) {
	b := richBackend("x")
	h := newHarness(t, b, false)
	w := h.do(get("/leads/campaigns"))
	if w.Code == http.StatusOK || b.total() != 0 {
		t.Errorf("no session: status %d, %d backend calls", w.Code, b.total())
	}
	h.do(get("/leads/campaigns?do=save", h.session(t)))
	if b.n("Campaigns") != 1 || b.total() != 1 {
		t.Errorf("Campaigns called %d times, %d calls in all", b.n("Campaigns"), b.total())
	}
	if w := h.do(post("/leads/campaigns", nil, h.session(t))); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST: status %d", w.Code)
	}
}

// TestCampaignsNotSetUp: with leads not set up the tab shows the not-set-up page.
func TestCampaignsNotSetUp(t *testing.T) {
	b := richBackend("x")
	b.leadsErr = ErrLeadsNotSetUp
	h := newHarness(t, b, false)
	if body := h.do(get("/leads/campaigns", h.session(t))).Body.String(); !strings.Contains(body, leadsNotSetUp) {
		t.Errorf("not set up: %s", body)
	}
}
