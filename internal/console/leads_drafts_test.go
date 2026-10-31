package console

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
)

// reviewCall is what the last ReviewDraft call was asked.
type reviewCall struct {
	ID      int64
	Verdict string
	Reasons []string
	Note    string
}

func (f *fakeBackend) DraftSamples(_ context.Context, c string, limit int) (DraftsPage, error) {
	f.count("DraftSamples")
	f.mu.Lock()
	f.draftsQ = append(f.draftsQ, c)
	f.mu.Unlock()
	if err := f.leadsRead(); err != nil {
		return DraftsPage{}, err
	}
	if c != "" && f.draftsErr != nil {
		return DraftsPage{}, f.draftsErr
	}
	p := f.drafts
	if c != "" {
		p.Campaign = c
	}
	if limit > 0 && len(p.Samples) > limit {
		p.Samples = p.Samples[:limit]
	}
	return p, nil
}

func (f *fakeBackend) ReviewDraft(_ context.Context, id int64, verdict string, reasons []string, note string) error {
	f.count("ReviewDraft")
	f.mu.Lock()
	f.lastReview = reviewCall{ID: id, Verdict: verdict, Reasons: reasons, Note: note}
	f.mu.Unlock()
	return f.reviewErr
}

func (f *fakeBackend) DraftGates(context.Context) (map[string]DraftGate, error) {
	f.count("DraftGates")
	return f.gates, f.leadsRead()
}

// richDrafts is a campaign with three samples: a Claude draft that passed and was rejected,
// a fake draft not reviewed, and a draft the check refused.
func richDrafts(text string) DraftsPage {
	cite := DraftCite{ID: "F2", Label: "Drupal version", Value: "7.98", Table: "site_probe", RowID: 12}
	sender := DraftCite{ID: "S1", Label: "Sender", Value: "Demo Sender", Table: "campaign", RowID: 1}
	return DraftsPage{
		Campaign:  "drupal-eol",
		Campaigns: []FilterOption{{"drupal-eol", "Drupal end-of-life"}, {"cafes", "Cafés"}},
		Gate:      DraftGate{Summary: "Claude drafts: not trusted yet: 14 of 20 reviewed, 86% approved"},
		Samples: []DraftSample{
			{GenerationID: 41, LeadID: 5, Domain: text, At: testSent, Attempt: 1, CheckOK: true, Reviewable: true,
				Facts: []DraftFact{{ID: "F2", Label: "Drupal version", Value: "7.98", Group: "lead", Table: "site_probe", RowID: 12},
					{ID: "F5", Label: "Their description", Value: text, Group: "lead", Table: "org_fact", RowID: 3, ContextOnly: true}},
				Steps: []DraftStep{{Step: 0, Subject: "Your Drupal 7 site", Paragraphs: []DraftParagraph{
					{Text: "I'm Demo Sender, a Drupal developer.", Cites: []DraftCite{sender}},
					{Text: text, PainID: "eol_no_security", Cites: []DraftCite{cite}}}}},
				TalkingPoints: []DraftParagraph{{Text: "Ask about their Drupal 7.98 site", Cites: []DraftCite{cite}}},
				Review:        &DraftReview{Verdict: VerdictReject, Reasons: []string{"too_long", "other"}, Note: "rambles", At: testSent}},
			{GenerationID: 40, LeadID: 6, Domain: "fake.example", At: testSent, Attempt: 1, Fake: true, CheckOK: true, Reviewable: true,
				Steps: []DraftStep{{Step: 0, Subject: "Hello", Paragraphs: []DraftParagraph{{Text: "A fake paragraph.", Cites: []DraftCite{sender}}}}}},
			{GenerationID: 39, LeadID: 7, Domain: "refused.example", At: testSent, Attempt: 2,
				Failures: []DraftFailure{{Where: "Email 1, paragraph 2", Msg: "2019 is not in a cited fact", Code: "G3"}}},
		},
	}
}

// TestDraftsTab: the Drafts tab lists each sample with its check, steps, cites, facts and
// review; a fake draft is marked; a refused draft says why and has no form; codes stay inside
// a details element.
func TestDraftsTab(t *testing.T) {
	b := richBackend("<b>Northwind</b>")
	h := newHarness(t, b, false)
	w := h.do(get("/leads/drafts?campaign=drupal-eol", h.session(t)))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		`<a href="/leads/drafts" aria-current="page">Drafts</a>`,
		`<option value="drupal-eol" selected>Drupal end-of-life</option>`,
		"Claude drafts: not trusted yet: 14 of 20 reviewed, 86% approved",
		`<article class="card draft" id="draft-41"`,
		"&lt;b&gt;Northwind&lt;/b&gt;",
		"Passed the check: every claim rests on a stored fact.",
		"<h3>Email 1</h3>", "Subject: Your Drupal 7 site",
		`<span class="cite-fact">Drupal version: 7.98</span> <span class="cite-source">from the site check, row 12 · F2</span>`,
		"Talking points for the call", "Ask about their Drupal 7.98 site",
		"The facts it was given (2)", "context only, never quoted",
		"Rejected on Sat 24 Oct 04:50: Too long, Other · “rambles”",
		"Review it again (the latest review counts)",
		`<button type="submit" name="verdict" value="approve" class="btn btn-primary">Approve</button>`,
		`<span class="tag">test draft (not counted)</span>`,
		"Refused by the check", "Email 1, paragraph 2: 2019 is not in a cited fact",
		"<summary>Rule codes</summary>",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the Drafts tab lacks %q", want)
		}
	}
	if strings.Contains(body, "<b>Northwind</b>") {
		t.Error("the Drafts tab did not escape the domain")
	}
	if strings.Contains(body, `action="/leads/drafts/39/review"`) {
		t.Error("a refused draft has a review form")
	}
	for _, id := range []string{"41", "40"} {
		if !strings.Contains(body, `action="/leads/drafts/`+id+`/review"`) {
			t.Errorf("draft %s has no review form", id)
		}
	}
	// The rule code shows only inside the details.
	if i := strings.Index(body, "<code>G3</code>"); i < 0 || i < strings.Index(body, "<summary>Rule codes</summary>") {
		t.Error("the rule code is not inside its details")
	}
	if got := b.draftsQ; len(got) != 1 || got[0] != "drupal-eol" {
		t.Errorf("DraftSamples asked for %q", got)
	}
}

// TestDraftsEmptyAndNoCampaign: no samples says how to make them; no campaign says so; an
// unknown campaign is said over the first campaign.
func TestDraftsEmptyAndNoCampaign(t *testing.T) {
	b := richBackend("x")
	b.drafts.Samples = nil
	h := newHarness(t, b, false)
	body := h.do(get("/leads/drafts", h.session(t))).Body.String()
	if !strings.Contains(body, "Run <code>leads drafts sample --campaign drupal-eol --n 20</code> to draft samples for review.") {
		t.Errorf("empty state: %s", body)
	}
	b.drafts = DraftsPage{}
	body = h.do(get("/leads/drafts", h.session(t))).Body.String()
	if !strings.Contains(body, "No campaigns yet.") {
		t.Errorf("no campaign: %s", body)
	}
	b = richBackend("x")
	b.draftsErr = fmt.Errorf("%w: No campaign is called \"nope\".", ErrUserFixable)
	h = newHarness(t, b, false)
	w := h.do(get("/leads/drafts?campaign=nope", h.session(t)))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `No campaign is called &#34;nope&#34;.`) ||
		!strings.Contains(w.Body.String(), `id="draft-41"`) {
		t.Errorf("unknown campaign: %d %s", w.Code, w.Body.String())
	}
	b = richBackend("x")
	b.leadsErr = ErrLeadsNotSetUp
	h = newHarness(t, b, false)
	if body := h.do(get("/leads/drafts", h.session(t))).Body.String(); !strings.Contains(body, leadsNotSetUp) {
		t.Errorf("not set up: %s", body)
	}
}

// TestDraftReview: approve and reject go to the backend and back to the draft with a line;
// an approve drops ticked reasons; a reject needs a reason, "other" a note, a note at most
// 500 characters; a verdict or reason the form never offers is refused before the backend;
// a backend refusal shows on the tab.
func TestDraftReview(t *testing.T) {
	b := richBackend("x")
	h := newHarness(t, b, false)
	c := h.session(t)

	w := h.do(post("/leads/drafts/41/review", url.Values{"verdict": {"approve"}, "reason": {"too_long"}, "campaign": {"drupal-eol"}}, c))
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/leads/drafts?campaign=drupal-eol#draft-41" {
		t.Fatalf("approve: %d %q", w.Code, w.Header().Get("Location"))
	}
	if r := b.lastReview; r.ID != 41 || r.Verdict != VerdictApprove || r.Reasons != nil {
		t.Errorf("approve asked %+v", r)
	}
	flash := w.Result().Cookies()
	body := h.do(get("/leads/drafts?campaign=drupal-eol", append(flash, c)...)).Body.String()
	if !strings.Contains(body, "Approved draft 41.") {
		t.Errorf("approve line missing: %s", body)
	}

	w = h.do(post("/leads/drafts/41/review", url.Values{"verdict": {"reject"}, "reason": {"other", "too_long", "too_long"}, "note": {" too salesy "}}, c))
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/leads/drafts#draft-41" {
		t.Fatalf("reject: %d %q", w.Code, w.Header().Get("Location"))
	}
	if r := b.lastReview; r.Verdict != VerdictReject || !slices.Equal(r.Reasons, []string{"other", "too_long"}) || r.Note != "too salesy" {
		t.Errorf("reject asked %+v", r)
	}

	n := b.n("ReviewDraft")
	for _, tc := range []struct {
		form url.Values
		code int
		want string
	}{
		{url.Values{"verdict": {"reject"}}, http.StatusUnprocessableEntity, needReason},
		{url.Values{"verdict": {"reject"}, "reason": {"other", "unclear"}, "note": {"  "}}, http.StatusUnprocessableEntity, "needs a note saying what is wrong"},
		{url.Values{"verdict": {"approve"}, "note": {strings.Repeat("a", 501)}}, http.StatusUnprocessableEntity, "longer than 500 characters"},
		{url.Values{"verdict": {"maybe"}}, http.StatusBadRequest, badVerdict},
		{url.Values{"verdict": {"reject"}, "reason": {"rude"}}, http.StatusBadRequest, badReason},
	} {
		w := h.do(post("/leads/drafts/41/review", tc.form, c))
		if w.Code != tc.code || !strings.Contains(w.Body.String(), strings.ReplaceAll(tc.want, `"`, "&#34;")) && !strings.Contains(w.Body.String(), tc.want) {
			t.Errorf("%v: %d %s", tc.form, w.Code, w.Body.String())
		}
	}
	if b.n("ReviewDraft") != n {
		t.Errorf("a refused form reached the backend")
	}
	// A refusal keeps the ticks and the note on that draft's form.
	body = h.do(post("/leads/drafts/41/review", url.Values{"verdict": {"reject"}, "reason": {"other", "unclear"}, "note": {""}}, c)).Body.String()
	if !strings.Contains(body, `value="unclear" aria-describedby="reasons-hint-41" checked`) ||
		strings.Contains(body, `value="unclear" aria-describedby="reasons-hint-40" checked`) {
		t.Errorf("the refusal lost the ticks: %s", body)
	}

	b.reviewErr = fmt.Errorf("%w: Draft 39 was refused by the check, so it cannot be reviewed.", ErrUserFixable)
	w = h.do(post("/leads/drafts/39/review", url.Values{"verdict": {"approve"}}, c))
	if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), "refused by the check, so it cannot be reviewed") {
		t.Errorf("backend refusal: %d", w.Code)
	}
	for _, p := range []string{"/leads/drafts/0/review", "/leads/drafts/x/review"} {
		if w := h.do(post(p, url.Values{"verdict": {"approve"}}, c)); w.Code != http.StatusNotFound {
			t.Errorf("%s: %d", p, w.Code)
		}
	}
}

// TestDraftReviewCrossOrigin: a review from another site is refused before the backend, and
// one without a session never writes.
func TestDraftReviewCrossOrigin(t *testing.T) {
	b := richBackend("x")
	h := newHarness(t, b, false)
	c := h.session(t)
	form := url.Values{"verdict": {"approve"}}
	r := post("/leads/drafts/41/review", form, c)
	r.Header.Set("Sec-Fetch-Site", "cross-site")
	if w := h.do(r); w.Code != http.StatusForbidden {
		t.Errorf("cross-site: %d, want 403", w.Code)
	}
	r = post("/leads/drafts/41/review", form, c)
	r.Header.Set("Origin", "https://evil.example")
	if w := h.do(r); w.Code != http.StatusForbidden {
		t.Errorf("cross-origin: %d, want 403", w.Code)
	}
	if w := h.do(post("/leads/drafts/41/review", form)); w.Code != http.StatusUnauthorized {
		t.Errorf("without a session: %d, want 401", w.Code)
	}
	if n := b.n("ReviewDraft"); n != 0 {
		t.Errorf("ReviewDraft called %d times", n)
	}
}

// TestCampaignsGateLine: the Campaigns tab shows each campaign's Claude-draft gate, with a
// link to review drafts while it is not trusted.
func TestCampaignsGateLine(t *testing.T) {
	b := richBackend("x")
	h := newHarness(t, b, false)
	body := h.do(get("/leads/campaigns", h.session(t))).Body.String()
	want := `<div><dt>Claude drafts</dt><dd class="campaign-gate">Claude drafts: not trusted yet: 14 of 20 reviewed, 86% approved · <a href="/leads/drafts?campaign=drupal-eol">review drafts</a></dd></div>`
	if !strings.Contains(body, want) {
		t.Errorf("the gate line is missing: %s", body)
	}
	b.gates["drupal-eol"] = DraftGate{Trusted: true, Summary: "Claude drafts: trusted: 20 of 20 reviewed, 95% approved"}
	body = h.do(get("/leads/campaigns", h.session(t))).Body.String()
	if !strings.Contains(body, "Claude drafts: trusted: 20 of 20 reviewed, 95% approved</dd>") {
		t.Errorf("trusted gate line: %s", body)
	}
	b.gates = nil
	if body := h.do(get("/leads/campaigns", h.session(t))).Body.String(); strings.Contains(body, "campaign-gate") {
		t.Error("a gate line with no gate")
	}
}

// TestBatchClaudeClaims: a message Claude wrote is marked and shows each paragraph with its
// cited facts behind a details element; a template message shows nothing new.
func TestBatchClaudeClaims(t *testing.T) {
	b := richBackend("x")
	h := newHarness(t, b, false)
	before := h.do(get("/leads/batches/3", h.session(t))).Body.String()
	for _, no := range []string{"Written by Claude", "claim-check", "draft_paragraph"} {
		if strings.Contains(before, no) {
			t.Errorf("a template message shows %q", no)
		}
	}
	b.batch.Items[0].Messages[0].Claude = &ClaudeWriting{GenerationID: 41, Paragraphs: []DraftParagraph{
		{Text: "I'm Demo Sender, a Drupal developer.", Cites: []DraftCite{{ID: "S1", Label: "Sender", Value: "Demo Sender", Table: "campaign", RowID: 1}}},
		{Text: "Drupal 7 has had no security fixes since January 2025.", Cites: []DraftCite{{ID: "F2", Label: "Drupal version", Value: "7.98", Table: "site_probe", RowID: 12}, {ID: "F9", Missing: true}}},
	}}
	body := h.do(get("/leads/batches/3", h.session(t))).Body.String()
	for _, want := range []string{
		`<span class="tag call-tag">Written by Claude</span>`,
		`<details class="claim-check">`, "<summary>What each paragraph rests on</summary>",
		"Drupal 7 has had no security fixes since January 2025.",
		`<summary>2 sources</summary>`,
		`<span class="cite-fact">Drupal version: 7.98</span> <span class="cite-source">from the site check, row 12 · F2</span>`,
		"F9 is not on the fact sheet",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the batch page lacks %q", want)
		}
	}
}
