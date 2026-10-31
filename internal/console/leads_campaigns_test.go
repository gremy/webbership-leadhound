package console

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
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
		`<a href="/leads/campaigns" aria-current="page">Campaigns</a>`, `<a href="/leads/home" aria-current="true">Leads</a>`,
		`<section class="card settings-section campaign" id="campaign-drupal-eol"`,
		`<h2 id="campaign-drupal-eol-title">Drupal end-of-life</h2>`,
		"<dt>Short name</dt><dd><code>drupal-eol</code></dd>",
		"<dt>Version</dt><dd><code>0123456789ab</code> · Sat 24 Oct 04:50 · from its file</dd>",
		"<dt>Lead kinds</dt><dd>Drupal 7 or older, Drupal 10</dd>",
		"<dt>Drafted by</dt><dd>Templates</dd>",
		"<dt>Claude spend today (whole account)</dt><dd>not in use yet</dd>",
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
	for _, want := range []string{"<dt>Drafted by</dt><dd>Claude</dd>", "<dt>Claude spend today (whole account)</dt><dd>$0.84 of $5.00</dd>", `<span class="tag">retired</span>`} {
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
// reaches the backend; with one, it reads Campaigns, DraftGates and CampaignMailboxes once each and writes
// nothing.
func TestCampaignsPageNeedsSession(t *testing.T) {
	b := richBackend("x")
	h := newHarness(t, b, false)
	w := h.do(get("/leads/campaigns"))
	if w.Code == http.StatusOK || b.total() != 0 {
		t.Errorf("no session: status %d, %d backend calls", w.Code, b.total())
	}
	h.do(get("/leads/campaigns?do=save", h.session(t)))
	if b.n("Campaigns") != 1 || b.n("DraftGates") != 1 || b.n("CampaignMailboxes") != 1 || b.total() != 3 {
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

// campaignMbs is drupal-eol's mailboxes: 1 assigned and landing on the campaign's site, 2
// assigned but landing elsewhere, 3 not assigned and never checked, 4 paused with a website
// that didn't answer.
func campaignMbs() []CampaignMailboxes {
	at := testSent
	return []CampaignMailboxes{{
		Slug: "drupal-eol", Name: "Drupal end-of-life", SiteURL: "https://site.example/", SiteHost: "site.example",
		Assigned: 2,
		Rows: []CampaignMailboxRow{
			{MailboxID: 1, Address: "ana@northwind.example", Host: "northwind.example", Domain: "northwind.example", State: MailboxReady, Assigned: true,
				Landing: LandingView{State: LandingFound, URL: "https://site.example/", Host: "site.example", At: &at}},
			{MailboxID: 2, Address: "ana@contoso.example", Host: "contoso.example", Domain: "contoso.example", State: MailboxReady, Assigned: true,
				Landing: LandingView{State: LandingFound, URL: "https://other.example/cafe", Host: "other.example", At: &at, OffSite: true}},
			{MailboxID: 3, Address: "hi@new.example", Host: "new.example", Domain: "new.example", State: MailboxNeedsCheck,
				Landing: LandingView{State: LandingNotChecked}},
			{MailboxID: 4, Address: "out@quiet.example", Host: "quiet.example", Domain: "quiet.example", State: MailboxPaused, Reason: "too many bounces",
				Landing: LandingView{State: LandingNoAnswer, At: &at}},
		},
	}}
}

// TestCampaignMailboxesShown: each campaign lists its mailboxes as a tick list (ticked when
// assigned) with domain, state and where the domain lands in words, warns when a domain lands
// away from the campaign's site, and says when none is assigned.
func TestCampaignMailboxesShown(t *testing.T) {
	b := richBackend("Drupal end-of-life")
	b.campaignMbs = campaignMbs()
	h := newHarness(t, b, false)
	w := h.do(get("/leads/campaigns", h.session(t)))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		`<form method="post" action="/leads/campaigns/drupal-eol/mailboxes" class="settings-form">`,
		`<input type="hidden" name="shown" value="1">`,
		`<input type="checkbox" name="assigned" value="1" aria-describedby="campaign-drupal-eol-mb-1" checked> ana@northwind.example</label>`,
		`<input type="checkbox" name="assigned" value="3" aria-describedby="campaign-drupal-eol-mb-3"> hi@new.example</label>`,
		`northwind.example · <span class="mb-state mb-ready">Ready</span> · northwind.example → https://site.example/`,
		`new.example · <span class="mb-state mb-needs_check">Needs a check</span> · where its domain goes is not checked yet`,
		`<span class="mb-state mb-paused">Paused: too many bounces</span> · the domain&#39;s website didn&#39;t answer`,
		`<p class="notice notice-warn" role="status">contoso.example sends visitors to https://other.example/cafe, not to this campaign&#39;s site.</p>`,
		"which should be site.example.",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the Campaigns page lacks %q", want)
		}
	}
	for _, not := range []string{"No mailbox is assigned", "northwind.example sends visitors", LandingNotChecked, LandingNoAnswer, "not_checked"} {
		if strings.Contains(body, not) {
			t.Errorf("the Campaigns page shows %q", not)
		}
	}

	// None assigned, no site named.
	b.campaignMbs[0].Assigned, b.campaignMbs[0].SiteHost = 0, ""
	for i := range b.campaignMbs[0].Rows {
		b.campaignMbs[0].Rows[i].Assigned = false
	}
	body = h.do(get("/leads/campaigns", h.session(t))).Body.String()
	for _, want := range []string{"No mailbox is assigned, so this campaign drafts no live batch.", "This campaign names no site"} {
		if !strings.Contains(body, want) {
			t.Errorf("none assigned: lacks %q", want)
		}
	}
	if strings.Contains(body, " checked> ") {
		t.Errorf("none assigned, yet a box is ticked")
	}

	// No mailbox at all.
	b.campaignMbs[0].Rows = nil
	body = h.do(get("/leads/campaigns", h.session(t))).Body.String()
	if !strings.Contains(body, `No mailbox yet. <a href="/mailboxes">Add one on the Mailboxes page</a>.`) || strings.Contains(body, `name="shown"`) {
		t.Errorf("no mailboxes: %s", body)
	}
}

// TestCampaignMailboxesSave: a save sends the whole tick set; only the mailboxes whose tick
// differs from their assignment are assigned or unassigned, then the page comes back with the
// outcome. A mailbox the form did not show is never touched, an unchanged form writes nothing,
// a refusal shows the page again (422), and an unknown campaign or a bad id is refused.
func TestCampaignMailboxesSave(t *testing.T) {
	const path = "/leads/campaigns/drupal-eol/mailboxes"
	shown := []string{"1", "2", "3", "4"}
	cases := []struct {
		name     string
		form     url.Values
		acts     int
		last     ControlAction
		flash    string
		wantCode int
	}{
		{"assign one", url.Values{"shown": shown, "assigned": {"1", "2", "3"}}, 1,
			ControlAction{Do: ControlAssign, Campaign: "drupal-eol", MailboxID: 3}, "done.", http.StatusSeeOther},
		{"unassign one", url.Values{"shown": shown, "assigned": {"1"}}, 1,
			ControlAction{Do: ControlUnassign, Campaign: "drupal-eol", MailboxID: 2}, "done.", http.StatusSeeOther},
		{"swap two", url.Values{"shown": shown, "assigned": {"1", "4"}}, 2,
			ControlAction{Do: ControlAssign, Campaign: "drupal-eol", MailboxID: 4}, "done. done.", http.StatusSeeOther},
		{"nothing changed", url.Values{"shown": shown, "assigned": {"2", "1"}}, 0,
			ControlAction{}, "Nothing changed", http.StatusSeeOther},
		{"not shown is never touched", url.Values{"shown": {"1", "3"}, "assigned": {"1", "4"}}, 0,
			ControlAction{}, "Nothing changed", http.StatusSeeOther},
		{"untick all", url.Values{"shown": shown}, 2,
			ControlAction{Do: ControlUnassign, Campaign: "drupal-eol", MailboxID: 2}, "done. done.", http.StatusSeeOther},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b := richBackend("x")
			b.campaignMbs = campaignMbs()
			b.ctlOut = ControlOutcome{Message: "done."}
			h := newHarness(t, b, false)
			w := h.do(post(path, c.form, h.session(t)))
			if w.Code != c.wantCode || w.Header().Get("Location") != "/leads/campaigns#campaign-drupal-eol" {
				t.Fatalf("status %d, to %q", w.Code, w.Header().Get("Location"))
			}
			if b.n("ControlAct") != c.acts || b.lastControl != c.last {
				t.Errorf("%d writes, last %+v; want %d, last %+v", b.n("ControlAct"), b.lastControl, c.acts, c.last)
			}
			fl := cookieNamed(w, "__Host-lh_flash")
			if fl == nil {
				t.Fatal("no flash")
			}
			r := get("/leads/campaigns", h.session(t))
			r.AddCookie(fl)
			if body := h.do(r).Body.String(); !strings.Contains(body, c.flash) {
				t.Errorf("the page after lacks %q", c.flash)
			}
		})
	}

	t.Run("refused", func(t *testing.T) {
		b := richBackend("x")
		b.campaignMbs = campaignMbs()
		b.ctlErr = fmt.Errorf("%w: that mailbox is retired", ErrUserFixable)
		h := newHarness(t, b, false)
		w := h.do(post(path, url.Values{"shown": shown, "assigned": {"1", "2", "3"}}, h.session(t)))
		if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), "that mailbox is retired") {
			t.Errorf("refused: %d %s", w.Code, w.Body.String())
		}
	})
	for _, bad := range []struct {
		name, path string
		form       url.Values
		code       int
	}{
		{"unknown campaign", "/leads/campaigns/nope/mailboxes", url.Values{"shown": {"1"}}, http.StatusNotFound},
		{"bad id", path, url.Values{"shown": {"1"}, "assigned": {"x"}}, http.StatusBadRequest},
		{"zero id", path, url.Values{"shown": {"0"}}, http.StatusBadRequest},
	} {
		t.Run(bad.name, func(t *testing.T) {
			b := richBackend("x")
			b.campaignMbs = campaignMbs()
			h := newHarness(t, b, false)
			w := h.do(post(bad.path, bad.form, h.session(t)))
			if w.Code != bad.code || b.n("ControlAct") != 0 {
				t.Errorf("status %d, %d writes", w.Code, b.n("ControlAct"))
			}
		})
	}
	t.Run("not set up", func(t *testing.T) {
		b := richBackend("x")
		b.leadsErr = ErrLeadsNotSetUp
		h := newHarness(t, b, false)
		w := h.do(post(path, url.Values{"shown": shown}, h.session(t)))
		if w.Code != http.StatusUnprocessableEntity || b.n("ControlAct") != 0 || !strings.Contains(w.Body.String(), leadsNotSetUp) {
			t.Errorf("not set up: %d, %d writes", w.Code, b.n("ControlAct"))
		}
	})
	t.Run("no session", func(t *testing.T) {
		b := richBackend("x")
		h := newHarness(t, b, false)
		if w := h.do(post(path, url.Values{"shown": shown})); w.Code == http.StatusSeeOther && w.Header().Get("Location") != "/login" || b.total() != 0 {
			t.Errorf("no session: %d, %d calls", w.Code, b.total())
		}
	})
}
