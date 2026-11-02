package console

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// tag is an HTML tag, so a test can read a page as the operator does.
var tag = regexp.MustCompile(`(?s)<[^>]*>`)

// onScreen is the page's main content with its tags gone: what the operator reads.
func onScreen(body string) string {
	if i := strings.Index(body, `<div class="page">`); i >= 0 {
		body = body[i:]
	}
	return tag.ReplaceAllString(body, " ")
}

// TestLeadPageOutreachCards: every outreach section renders in the plan's order, the old Messages
// and Ways to reach them sections are gone, and each form posts where its handler is.
func TestLeadPageOutreachCards(t *testing.T) {
	h := newHarness(t, richBackend("Harbour Lane Studio"), false)
	body := h.do(get("/leads/5", h.session(t))).Body.String()
	last := -1
	for _, s := range []string{`id="next-step"`, `id="drafts"`, `id="draft-9"`, `id="contacts"`, "<h2>Finding</h2>", `id="proposals"`, `id="sector"`, `id="crm-stage"`, `id="crm-timeline"`} {
		i := strings.Index(body, s)
		if i < 0 || i < last {
			t.Errorf("%s missing or out of order", s)
		}
		last = i
	}
	for _, gone := range []string{"<h2>Messages</h2>", "Ways to reach them", "<h3>Phone numbers</h3>"} {
		if strings.Contains(body, gone) {
			t.Errorf("the lead page still has %q", gone)
		}
	}
	for _, want := range []string{
		// Drafts.
		`<textarea class="input textarea" id="draft-text-9" rows="8" readonly>Harbour Lane Studio</textarea>`,
		`data-copy="draft-text-9">Copy</button>`,
		`action="/leads/5/drafts/9/sent"`, `name="hash" value="0123456789ab"`,
		`action="/leads/5/drafts/9/save"`, "<summary>Show the original</summary>", "Harbour Lane Studio before",
		"19 of 200 characters", "Written from the template, because Claude refused twice.", "Written by Claude.",
		`<a href="https://linkedin.com/company/harbourlane" rel="noopener noreferrer nofollow">`,
		`name="subject"`, "To hello@harbourlane.example", "Drafting… asked at 09:32.",
		`hx-get="/leads/draft-status/5" hx-trigger="every 5s"`, "Asked for at 09:12.",
		`action="/leads/5/draft-discard"`, `name="batch_id" value="3"`,
		// Contacts.
		`action="/leads/5/contacts"`, `action="/leads/5/contacts/r12/edit"`, `action="/leads/5/contacts/r12/retire"`,
		`<span class="badge">added by you</span>`, `<span class="badge">found by research</span>`,
		`<span class="badge">from their site</span>`, `<span class="badge">from the map</span>`,
		"used for this lead", "Email check: safe, may be sent", "Mail server found", "&#43;44 20 7946 0177",
		`<p class="notice notice-warn">Its country is off in leads.yaml; this is your own contact, so it is still used.</p>`,
		"Replaced by", `<option value="linkedin">LinkedIn</option>`, `<option value="3">Harbour Lane Studio, Director</option>`,
		// Proposals.
		`action="/leads/5/proposals"`, `action="/leads/5/proposals/3/status"`, "£2,400", "Declined",
		`<button type="submit" name="status" value="accepted"`, `<option value="RON">RON</option>`,
		// Sector.
		`action="/leads/5/sector"`, "Guessed from the domain.", `<option value="education" selected>Education</option>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the lead page lacks %q", want)
		}
	}
	// A message already sent offers neither Edit nor Mark sent; a retired contact offers no Retire.
	if strings.Contains(body, `/drafts/11/sent"`) || strings.Contains(body, `/drafts/11/save"`) {
		t.Error("a message marked sent still offers Edit or Mark sent")
	}
	if strings.Contains(body, `/drafts/12/sent"`) {
		t.Error("an email offers Mark sent")
	}
	if strings.Contains(body, `/contacts/r14/retire"`) || !strings.Contains(body, `/contacts/r14/edit"`) {
		t.Error("a retired contact's forms")
	}
	// A proposal's current status is not offered again; the other proposal's is.
	sent := body[strings.Index(body, `id="proposal-3"`):strings.Index(body, `id="proposal-2"`)]
	declined := body[strings.Index(body, `id="proposal-2"`):strings.Index(body, `id="proposal-add"`)]
	if strings.Contains(sent, `name="status" value="sent"`) || !strings.Contains(declined, `name="status" value="sent"`) {
		t.Error("a proposal offers its own status, or not another's")
	}
}

// TestLeadPageNoInternals: what the operator reads never carries a raw id, a stored word or a
// place id, and no text reaches the page unescaped.
func TestLeadPageNoInternals(t *testing.T) {
	const evil = "<script>alert(1)</script>"
	b := richBackend(evil)
	b.detail.Domain = PlaceOrgPrefix + "08f2a1b3-0c4d"
	h := newHarness(t, b, false)
	body := h.do(get("/leads/5", h.session(t))).Body.String()
	if strings.Contains(body, "<script>alert") {
		t.Error("the lead page shows a text unescaped")
	}
	if strings.Contains(body, PlaceOrgPrefix) {
		t.Error("the lead page shows a place id")
	}
	screen := onScreen(body)
	for _, bad := range []string{"r12", "r13", "r14", "f7", "p3", "0123456789ab", "ba9876543210", "draft_it", "not_searched",
		"crawl", "overture", " route", " fact", "generation"} {
		if strings.Contains(strings.ToLower(screen), bad) {
			t.Errorf("the lead page shows %q on screen", bad)
		}
	}
}

// TestLeadPageNextStep: the card says the bucket, its reason and exactly one primary action; the
// page shows one Send now at most.
func TestLeadPageNextStep(t *testing.T) {
	block := func(body string) string {
		i := strings.Index(body, `id="next-step"`)
		j := strings.Index(body, `id="drafts"`)
		if i < 0 || j < i {
			t.Fatal("no Next step card")
		}
		return body[i:j]
	}
	for _, tt := range []struct {
		name         string
		set          func(*LeadDetailPage)
		badge, words string
		action       string
	}{
		{"email", func(d *LeadDetailPage) { d.Reach, d.DiscardBatchID = ReachEmail, 0 }, "Email-ready", "They can be emailed.", "Send now"},
		{"email with a draft", func(d *LeadDetailPage) { d.Reach = ReachEmail }, "Email-ready", "A draft is waiting", `href="/leads/batches/3"`},
		{"manual with a draft", func(d *LeadDetailPage) { d.Reach = ReachManual }, "Manual", "send it by hand", `href="#draft-9">Copy &amp; mark sent`},
		{"manual, nothing drafted", func(d *LeadDetailPage) { d.Reach, d.Drafts, d.DraftItBlock = ReachManual, nil, "" }, "Manual", "Nothing is drafted for LinkedIn yet.", `action="/leads/5/draft-it"`},
		{"manual, a phone only", func(d *LeadDetailPage) { d.Reach, d.Channel, d.Drafts = ReachManual, "none", nil }, "Manual", "a phone number", `href="#contact-add"`},
		{"not searched", func(d *LeadDetailPage) {
			d.Reach, d.ReachReason, d.Research = ReachDead, ReachNotSearched, LeadResearch{CanAsk: true}
		}, "Dead end", "Not searched yet", `action="/leads/5/look-again"`},
		{"not searched, none allowed", func(d *LeadDetailPage) { d.Reach, d.ReachReason = ReachDead, ReachNotSearched }, "Dead end", "Not searched yet", `href="#contact-add"`},
		{"looking", func(d *LeadDetailPage) { d.Reach, d.ReachReason = ReachDead, ReachLooking }, "Dead end", "Looking for a contact", `href="#contact-add"`},
		{"nothing", func(d *LeadDetailPage) { d.Reach, d.ReachReason = ReachDead, ReachNothing }, "Dead end", "Nothing found", `href="#contact-add"`},
		{"found", func(d *LeadDetailPage) { d.Reach, d.ReachReason = ReachDead, ReachFound }, "Dead end", "A contact was found; the next build picks it up", `href="#contact-add"`},
		{"country", func(d *LeadDetailPage) { d.Reach, d.ReachReason = ReachDead, ReachCountry }, "Dead end", "This country is off in your settings", `href="#contact-add"`},
		{"contacted, a follow-up due", func(d *LeadDetailPage) { d.Reach = ReachContacted }, "Contacted", "The next message is yours", `href="#draft-9"`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			b := richBackend("x")
			tt.set(&b.detail)
			h := newHarness(t, b, false)
			body := h.do(get("/leads/5", h.session(t))).Body.String()
			card := block(body)
			for _, want := range []string{tt.badge, tt.words, tt.action} {
				if !strings.Contains(card, want) {
					t.Errorf("the card lacks %q: %s", want, card)
				}
			}
			if n := strings.Count(card, "btn-primary"); n != 1 {
				t.Errorf("%d primary actions: %s", n, card)
			}
			if n := strings.Count(body, "Send now</button>"); n > 1 {
				t.Errorf("%d Send now buttons", n)
			}
		})
	}
	b := richBackend("x")
	b.detail.Reach, b.detail.Drafts = ReachContacted, nil
	h := newHarness(t, b, false)
	if card := block(h.do(get("/leads/5", h.session(t))).Body.String()); strings.Contains(card, "btn-primary") {
		t.Errorf("a contacted lead with nothing due has an action: %s", card)
	}
}

// TestDraftStatusFragment: the line polls while a request is open and stops once it ended, and a
// lead that never asked gets no trigger.
func TestDraftStatusFragment(t *testing.T) {
	for _, tt := range []struct {
		name  string
		req   *LeadDraftRequest
		poll  bool
		words string
	}{
		{"never asked", nil, false, ""},
		{"queued", &LeadDraftRequest{Status: DraftQueued, RequestedAt: statusNow}, true, "Drafting… asked at"},
		{"drafting", &LeadDraftRequest{Status: DraftDrafting, RequestedAt: statusNow}, true, "Drafting… asked at"},
		{"done", &LeadDraftRequest{Status: DraftDone, RequestedAt: statusNow, FinishedAt: &statusNow, BatchID: 3}, false, "Show the draft"},
		{"failed", &LeadDraftRequest{Status: DraftFailed, Reason: "It already has a batch", RequestedAt: statusNow}, false, "Draft it did not finish: It already has a batch"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			b := richBackend("x")
			b.draftRequest = tt.req
			h := newHarness(t, b, false)
			body := h.do(get("/leads/draft-status/5", h.session(t))).Body.String()
			if got := strings.Contains(body, "hx-trigger"); got != tt.poll {
				t.Errorf("polls = %v, want %v: %s", got, tt.poll, body)
			}
			if !strings.Contains(body, tt.words) {
				t.Errorf("lacks %q: %s", tt.words, body)
			}
			if strings.Contains(body, "<html") {
				t.Error("the fragment carries the layout")
			}
		})
	}
}

// TestLeadPagePostedKept: a refused form is shown again in its place with what was typed; the same
// field of every other form keeps its stored value.
func TestLeadPagePostedKept(t *testing.T) {
	for _, tt := range []struct {
		name   string
		posted OutreachPosted
		wants  []string
	}{
		{"draft", OutreachPosted{Form: "draft", ID: strconv.Itoa(9), Refusal: "It names a banned word.", Values: map[string]string{"body": "typed text"}},
			[]string{"typed text</textarea>", "It names a banned word."}},
		{"contact", OutreachPosted{Form: "contact", ID: "r13", Refusal: "Not a LinkedIn page.", Values: map[string]string{"value": "typed-value", "note": "typed-note"}},
			[]string{`value="typed-value"`, `value="typed-note"`, "Not a LinkedIn page."}},
		{"contact-add", OutreachPosted{Form: "contact-add", Refusal: "It needs a value.", Values: map[string]string{"kind": "phone", "note": "typed-note"}},
			[]string{`<option value="phone" selected>`, `value="typed-note"`, "It needs a value."}},
		{"proposal-add", OutreachPosted{Form: "proposal-add", Refusal: "It needs a title.", Values: map[string]string{"url": "typed-url", "currency": "RON"}},
			[]string{`value="typed-url"`, `<option value="RON" selected>`, "It needs a title."}},
		{"proposal", OutreachPosted{Form: "proposal", ID: "2", Refusal: "Too long.", Values: map[string]string{"note": "typed-note"}},
			[]string{`value="typed-note"`, "Too long."}},
		{"sector", OutreachPosted{Form: "sector", Refusal: "Pick one.", Values: map[string]string{"sector": "other"}},
			[]string{`<option value="other" selected>`, "Pick one."}},
		{"draft-it", OutreachPosted{Form: "draft-it", Refusal: "Already contacted."}, []string{"Already contacted."}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			b := richBackend("Harbour Lane Studio")
			b.detail.Posted = tt.posted
			h := newHarness(t, b, false)
			body := h.do(get("/leads/5", h.session(t))).Body.String()
			for _, want := range tt.wants {
				if !strings.Contains(body, want) {
					t.Errorf("the page lacks %q", want)
				}
			}
			if n := strings.Count(body, `class="alert alert-stop"`); n != 1 {
				t.Errorf("%d refusals shown, want 1", n)
			}
		})
	}
}
