package console

import (
	"context"
	"fmt"
	"html/template"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"
)

// igFake is the Instagram part of fakeBackend: a review list it answers from, and the calls it
// was asked for. Like the real backend it refuses a handle that is on none of its rows and a
// lead that is not one of its café leads, wrapping ErrUserFixable.
type igFake struct {
	igRows   map[string][]IGReviewRow // by status
	igCafes  []int64                  // the café leads a handle may be matched to
	igStatus string                   // the status the last InstagramReview asked for
	igErr    error                    // returned by InstagramAct before its own checks
	lastIG   *IGAction
}

func (f *fakeBackend) InstagramReview(_ context.Context, status string) (IGReviewPage, error) {
	f.count("InstagramReview")
	if err := f.leadsRead(); err != nil {
		return IGReviewPage{}, err
	}
	f.mu.Lock()
	f.igStatus = status
	f.mu.Unlock()
	p := IGReviewPage{Status: status, Counts: map[string]int{}, Rows: f.igRows[status]}
	for st, rows := range f.igRows {
		p.Counts[st] = len(rows)
	}
	return p, nil
}

func (f *fakeBackend) InstagramAct(_ context.Context, a IGAction) (string, error) {
	f.count("InstagramAct")
	f.mu.Lock()
	f.lastIG = &a
	f.mu.Unlock()
	if f.igErr != nil {
		return "", f.igErr
	}
	h := strings.ToLower(strings.TrimPrefix(a.Handle, "@"))
	known := false
	for _, rows := range f.igRows {
		known = known || slices.ContainsFunc(rows, func(r IGReviewRow) bool { return r.Handle == h })
	}
	switch {
	case !known:
		return "", fmt.Errorf("%w: @%s is in no imported capture; import it first", ErrUserFixable, h)
	case a.Do == IGMatch && !slices.Contains(f.igCafes, a.LeadID):
		return "", fmt.Errorf("%w: lead %d is not a café lead Instagram can be matched to", ErrUserFixable, a.LeadID)
	}
	return "@" + h + " done: " + a.Do, nil
}

// richIG is a review list with one new row holding text everywhere, a tie between two leads, one
// row per other status, and the café leads 5 and 6.
func richIG(text string) igFake {
	row := func(h string, cands ...IGCandidate) IGReviewRow {
		return IGReviewRow{Handle: h, URL: "https://www.instagram.com/" + h + "/", Name: text, Category: text, Place: "a café with seating", Candidates: cands}
	}
	return igFake{
		igRows: map[string][]IGReviewRow{
			IGReviewNew: {
				row("cafe.exemplu", IGCandidate{LeadID: 5, Name: text, Town: "Târgu Exemplu", Matchable: true},
					IGCandidate{LeadID: 6, Name: "Bean House", Matchable: true}, IGCandidate{LeadID: 9, Name: "Gone Café"}),
				{Handle: "roast_follow.example", URL: "https://www.instagram.com/roast_follow.example/"},
			},
			IGReviewWanted:    {row("wanted_example")},
			IGReviewDismissed: {row("dismissed_example")},
			IGReviewMatched:   {row("matched_example", IGCandidate{LeadID: 5, Name: "Brewlane Corner", Matchable: true})},
		},
		igCafes: []int64{5, 6},
	}
}

// igMarkup is the page's own part of body, past the layout's shell.
func igMarkup(t *testing.T, body string) string {
	t.Helper()
	_, after, ok := strings.Cut(body, `<div class="page">`)
	if !ok {
		t.Fatalf("no page in %q", body)
	}
	return after
}

// TestInstagramReviewPage: the new tab lists each handle as a link to its profile in a new tab,
// with its name, category and Instagram's place call, its candidate leads linked to their pages
// with "It's this lead" (a lead it cannot be matched to has no button), and "Not a lead" and
// "Want as lead". The tabs carry their counts and mark the current one; the text is escaped.
func TestInstagramReviewPage(t *testing.T) {
	b := richBackend("<script>alert(1)</script>")
	h := newHarness(t, b, false)
	w := h.do(get(leadsIGPath, h.session(t)))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	if b.igStatus != IGReviewNew {
		t.Errorf("read status %q, want new", b.igStatus)
	}
	body := igMarkup(t, w.Body.String())
	for _, want := range []string{
		`<a href="/leads/instagram" aria-current="true">New (2)</a>`, `<a href="/leads/instagram?status=wanted">Wanted (1)</a>`,
		`<a href="/leads/instagram?status=dismissed">Dismissed (1)</a>`, `<a href="/leads/instagram?status=matched">Matched (1)</a>`,
		`<a href="https://www.instagram.com/cafe.exemplu/" rel="noopener noreferrer nofollow" target="_blank">@cafe.exemplu</a>`,
		"&lt;script&gt;alert(1)&lt;/script&gt; · &lt;script&gt;alert(1)&lt;/script&gt;", "Instagram says a café with seating",
		`<a href="/leads/5">`, `<a href="/leads/6">Bean House</a>`, `<a href="/leads/9">Gone Café</a>`, "Târgu Exemplu",
		`action="/leads/instagram/match"`, `name="lead" value="5"`, `name="lead" value="6"`,
		`action="/leads/instagram/dismiss"`, `action="/leads/instagram/want"`, `name="status" value="new"`,
		">It's this lead<", ">Not a lead<", ">Want as lead<",
		"Instagram says nothing yet: not read as a profile", "No lead it may be.",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("lacks %q", want)
		}
	}
	if strings.Contains(body, `name="lead" value="9"`) {
		t.Error("a lead Instagram cannot be matched to has the button")
	}
	if strings.Contains(body, "<script>alert") {
		t.Error("unescaped text")
	}
	// The shell's Leads nav links the page and marks it.
	if !strings.Contains(w.Body.String(), `<a href="/leads/instagram" aria-current="page"`) {
		t.Error("the Leads nav does not mark Instagram")
	}
}

// TestInstagramReviewRowLayout: each row's handle, name and answers sit straight in the card's
// body, on the one content edge its "No lead it may be." line and buttons use (no .list-item
// padding of their own); the loyalty and vendor answers follow "Instagram says" on one line,
// each part only when known, and a row with neither has no such line.
func TestInstagramReviewRowLayout(t *testing.T) {
	b := richBackend("x")
	b.igRows[IGReviewNew] = []IGReviewRow{
		{Handle: "both.example", URL: "https://www.instagram.com/both.example/", Place: "a café with seating",
			Loyalty: "runs a loyalty or stamp scheme", Vendor: "Stampino"},
		{Handle: "vendor.example", URL: "https://www.instagram.com/vendor.example/", Vendor: "a paper card"},
		{Handle: "loyal.example", URL: "https://www.instagram.com/loyal.example/", Loyalty: "runs a loyalty or stamp scheme"},
		{Handle: "none.example", URL: "https://www.instagram.com/none.example/"},
	}
	h := newHarness(t, b, false)
	body := igMarkup(t, h.do(get(leadsIGPath, h.session(t))).Body.String())
	if strings.Contains(body, "list-item") {
		t.Error("a row wraps its head in .list-item, indenting it past the card's content edge")
	}
	rows := map[string]string{}
	for _, part := range strings.Split(body, `<article class="card card-body stack-sm" id="ig-`)[1:] {
		handle, rest, _ := strings.Cut(part, `"`)
		rows[handle], _, _ = strings.Cut(rest, "</article>")
		if !strings.HasPrefix(rows[handle], ">\n    <div class=\"list-main ig-facts\">\n      <h2 class=\"list-title\">") {
			t.Errorf("@%s: the card body does not open on its head: %.120q", handle, rows[handle])
		}
	}
	for handle, want := range map[string]string{
		"both.example":   `Instagram says a café with seating</p>` + "\n      " + `<p class="list-meta">Loyalty: runs a loyalty or stamp scheme · Vendor: Stampino</p>`,
		"vendor.example": `<p class="list-meta">Vendor: a paper card</p>`,
		"loyal.example":  `<p class="list-meta">Loyalty: runs a loyalty or stamp scheme</p>`,
	} {
		if !strings.Contains(rows[handle], want) {
			t.Errorf("@%s lacks %q", handle, want)
		}
	}
	if strings.Contains(rows["vendor.example"], "Loyalty:") || strings.Contains(rows["loyal.example"], "Vendor:") {
		t.Error("an unknown answer shows its label")
	}
	if r := rows["none.example"]; r == "" || strings.Count(r, `<p class="list-meta">`) != 1 || strings.Contains(r, "Loyalty:") || strings.Contains(r, "Vendor:") {
		t.Errorf("a row with no loyalty or vendor answer has the line: %q", r)
	}
}

// TestInstagramReviewTabs: each other status reads its own rows; a tab hides the call that would
// leave a row where it is (no "Not a lead" on Dismissed, no "Want as lead" on Wanted, no "It's
// this lead" on Matched); an unknown status is a 404 and reads nothing; an empty tab says so.
func TestInstagramReviewTabs(t *testing.T) {
	b := richBackend("x")
	h := newHarness(t, b, false)
	c := h.session(t)
	for _, tc := range []struct{ status, handle, hidden string }{
		{IGReviewWanted, "wanted_example", "/leads/instagram/want"},
		{IGReviewDismissed, "dismissed_example", "/leads/instagram/dismiss"},
		{IGReviewMatched, "matched_example", "/leads/instagram/match"},
	} {
		body := igMarkup(t, h.do(get(leadsIGPath+"?status="+tc.status, c)).Body.String())
		if !strings.Contains(body, "@"+tc.handle) || strings.Contains(body, "@cafe.exemplu") {
			t.Errorf("%s: wrong rows", tc.status)
		}
		if strings.Contains(body, tc.hidden) {
			t.Errorf("%s offers %s", tc.status, tc.hidden)
		}
		if !strings.Contains(body, `name="status" value="`+tc.status+`"`) {
			t.Errorf("%s: its forms do not come back to it", tc.status)
		}
	}
	before := b.n("InstagramReview")
	if w := h.do(get(leadsIGPath+"?status=nope", c)); w.Code != http.StatusNotFound {
		t.Errorf("unknown status: %d", w.Code)
	}
	if b.n("InstagramReview") != before {
		t.Error("an unknown status was read")
	}
	b.igRows[IGReviewWanted] = nil
	if body := h.do(get(leadsIGPath+"?status=wanted", c)).Body.String(); !strings.Contains(body, "No account has this status yet.") {
		t.Error("an empty tab says nothing")
	}
}

// TestInstagramActs: each call posts the handle (and for a match the lead) to the backend and goes
// back to the tab it came from with the backend's line.
func TestInstagramActs(t *testing.T) {
	for _, tc := range []struct {
		action string
		form   url.Values
		want   IGAction
		next   string
	}{
		{"match", url.Values{"handle": {" cafe.exemplu "}, "lead": {"6"}, "status": {"new"}}, IGAction{Do: IGMatch, Handle: "cafe.exemplu", LeadID: 6}, "/leads/instagram"},
		{"dismiss", url.Values{"handle": {"cafe.exemplu"}, "status": {"wanted"}}, IGAction{Do: IGDismiss, Handle: "cafe.exemplu"}, "/leads/instagram?status=wanted"},
		{"want", url.Values{"handle": {"@Cafe.Exemplu"}, "status": {"bogus"}}, IGAction{Do: IGWant, Handle: "@Cafe.Exemplu"}, "/leads/instagram"},
	} {
		t.Run(tc.action, func(t *testing.T) {
			b := richBackend("x")
			h := newHarness(t, b, false)
			w := h.do(post(leadsIGPath+"/"+tc.action, tc.form, h.session(t)))
			if w.Code != http.StatusSeeOther || w.Header().Get("Location") != tc.next {
				t.Fatalf("%d to %q", w.Code, w.Header().Get("Location"))
			}
			if b.lastIG == nil || *b.lastIG != tc.want {
				t.Errorf("asked %+v, want %+v", b.lastIG, tc.want)
			}
			if got := flashOf(t, w); got != "@cafe.exemplu done: "+tc.want.Do {
				t.Errorf("flash %q", got)
			}
		})
	}
}

// TestInstagramActRefused: a handle in no capture, a lead that is not a café lead, a match with no
// lead and a call with no handle are refused on the tab with the words, 422; the last two never
// reach the backend. An unknown call is a 404.
func TestInstagramActRefused(t *testing.T) {
	for _, tc := range []struct {
		name, action string
		form         url.Values
		words        string
		reached      bool
	}{
		{"unknown handle", "dismiss", url.Values{"handle": {"nobody.example"}}, "@nobody.example is in no imported capture; import it first", true},
		{"unknown handle match", "match", url.Values{"handle": {"nobody.example"}, "lead": {"5"}}, "@nobody.example is in no imported capture", true},
		{"not a café lead", "match", url.Values{"handle": {"cafe.exemplu"}, "lead": {"9"}}, "lead 9 is not a café lead Instagram can be matched to", true},
		{"no lead", "match", url.Values{"handle": {"cafe.exemplu"}, "lead": {"0"}}, "Pick the lead it is; nothing was changed.", false},
		{"no handle", "want", url.Values{"handle": {"  "}}, "Name the Instagram account; nothing was changed.", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := richBackend("x")
			h := newHarness(t, b, false)
			w := h.do(post(leadsIGPath+"/"+tc.action, tc.form, h.session(t)))
			if w.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status %d", w.Code)
			}
			body := w.Body.String()
			if !strings.Contains(body, tc.words) || !strings.Contains(body, `notice notice-error`) {
				t.Errorf("lacks the refusal %q", tc.words)
			}
			if !strings.Contains(body, `aria-current="true">New (2)</a>`) {
				t.Error("the refusal is not shown on the tab")
			}
			if got := b.n("InstagramAct") == 1; got != tc.reached {
				t.Errorf("reached the backend: %v", got)
			}
		})
	}
	b := richBackend("x")
	h := newHarness(t, b, false)
	if w := h.do(post(leadsIGPath+"/unmatch", url.Values{"handle": {"cafe.exemplu"}}, h.session(t))); w.Code != http.StatusNotFound {
		t.Errorf("unknown call: %d", w.Code)
	}
	if b.n("InstagramAct") != 0 {
		t.Error("an unknown call reached the backend")
	}
}

// TestInstagramActNeedsSessionAndSameOrigin: every call from another site is refused before the
// backend, and one without a session never writes; the page needs a session too.
func TestInstagramActNeedsSessionAndSameOrigin(t *testing.T) {
	b := richBackend("x")
	h := newHarness(t, b, false)
	c := h.session(t)
	for _, action := range []string{"match", "dismiss", "want"} {
		form := url.Values{"handle": {"cafe.exemplu"}, "lead": {"5"}}
		path := leadsIGPath + "/" + action
		r := post(path, form, c)
		r.Header.Set("Sec-Fetch-Site", "cross-site")
		if w := h.do(r); w.Code != http.StatusForbidden {
			t.Errorf("%s cross-site: %d, want 403", action, w.Code)
		}
		r = post(path, form, c)
		r.Header.Set("Origin", "https://evil.example")
		if w := h.do(r); w.Code != http.StatusForbidden {
			t.Errorf("%s cross-origin: %d, want 403", action, w.Code)
		}
		if w := h.do(post(path, form)); w.Code != http.StatusUnauthorized {
			t.Errorf("%s without a session: %d, want 401", action, w.Code)
		}
	}
	if n := b.n("InstagramAct"); n != 0 {
		t.Errorf("InstagramAct called %d times", n)
	}
	if w := h.do(get(leadsIGPath)); w.Code == http.StatusOK {
		t.Errorf("the page without a session: %d", w.Code)
	}
}

// TestInstagramNotSetUp: with no leads settings the page and a call show the not-set-up page.
func TestInstagramNotSetUp(t *testing.T) {
	b := richBackend("x")
	b.leadsErr = ErrLeadsNotSetUp
	b.igErr = ErrLeadsNotSetUp
	h := newHarness(t, b, false)
	c := h.session(t)
	if body := h.do(get(leadsIGPath, c)).Body.String(); !strings.Contains(body, leadsNotSetUp) {
		t.Error("the page does not say leads are not set up")
	}
	w := h.do(post(leadsIGPath+"/want", url.Values{"handle": {"cafe.exemplu"}}, c))
	if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), leadsNotSetUp) {
		t.Errorf("a call: %d", w.Code)
	}
}

// igCard is a full Instagram card: Instagram reads a roastery while the lead is a coffee shop.
func igCard() *LeadInstagramView {
	at := testSent
	return &LeadInstagramView{Handle: "brewlane.example", URL: "https://www.instagram.com/brewlane.example/", Followers: 12345, Counted: true,
		CapturedAt: &at, Matched: "its website", Place: "a roastery or online shop", Loyalty: "runs a loyalty or stamp scheme",
		Vendor: "Stampino", VendorVia: "from a link", Hints: []string{"followed by Stampino", "followed by Cardora"}, Disagree: "roastery"}
}

// TestLeadInstagramCard: a café lead's card sits under The place with the handle opening its
// profile in a new tab, the followers, the date read, how it was matched, Instagram's three
// answers, the vendor's source and the hints, and the disagreement naming the existing
// correction; in the light and the dark theme alike.
func TestLeadInstagramCard(t *testing.T) {
	b := richBackend("x")
	b.detail = cafeLeadDetail()
	b.detail.PlaceType, b.detail.PlaceTypeSource = "cafe", "jev"
	b.detail.Instagram = igCard()
	h := newHarness(t, b, false)
	c := h.session(t)
	for _, theme := range []string{"", "light", "dark"} {
		cookies := []*http.Cookie{c}
		if theme != "" {
			cookies = append(cookies, themeCookie(false, theme))
		}
		body := h.do(get("/leads/5", cookies...)).Body.String()
		_, card, ok := strings.Cut(body, `<section class="card" id="instagram" aria-labelledby="instagram-title">`)
		if !ok {
			t.Fatalf("theme %q: no Instagram card", theme)
		}
		card, _, _ = strings.Cut(card, "</section>")
		for _, want := range []string{
			`<h2 id="instagram-title">Instagram</h2><span class="muted small">matched by its website</span>`,
			`<a href="https://www.instagram.com/brewlane.example/" rel="noopener noreferrer nofollow" target="_blank">@brewlane.example</a>`,
			"<dt>Followers</dt><dd>12,345</dd>", "<dt>Read</dt><dd>" + testSent.Format("Mon 2 Jan"),
			"<dt>Instagram says</dt><dd>a roastery or online shop</dd>", "<dt>Loyalty</dt><dd>runs a loyalty or stamp scheme</dd>",
			`<dt>Vendor</dt><dd>Stampino <span class="muted small">from a link</span></dd>`,
			"<dt>Hints</dt><dd>followed by Stampino · followed by Cardora</dd>",
			`<div class="alert alert-warn mt-1" role="note">`,
			// The footnote keeps the space other cards' footnotes have under a key/value list.
			"</dl>\n    <p class=\"hint mt-1\">Read from the café's public profile; it never changes the lead.</p>",
			"Instagram reads this as a roastery or online shop. Nothing was changed: if it is, press It's a roastery above (r on the Leads list).",
		} {
			if !strings.Contains(card, want) {
				t.Errorf("theme %q: the card lacks %q", theme, want)
			}
		}
		place, _, _ := strings.Cut(body, `id="instagram"`)
		if !strings.Contains(place, "<h2>The place</h2>") {
			t.Errorf("theme %q: the card is not under The place", theme)
		}
		if theme != "" && !strings.Contains(body, `data-theme="`+theme+`"`) {
			t.Errorf("theme %q not applied", theme)
		}
	}

	// The reverse disagreement, a private account, nothing judged, no count, and no disagreement.
	b.detail.Instagram = &LeadInstagramView{Handle: "brewlane.example", URL: "https://www.instagram.com/brewlane.example/",
		Matched: "your call", Private: true, Disagree: "cafe"}
	body := h.do(get("/leads/5", c)).Body.String()
	for _, want := range []string{"matched by your call", `<span class="badge">private</span>`, "<dt>Followers</dt><dd>not counted</dd>",
		"<dt>Read</dt><dd>not read as a profile yet</dd>", "<dt>Instagram says</dt><dd>not judged yet</dd>",
		"Instagram reads this as a café with seating. Nothing was changed: if it is, press It's a coffee shop above"} {
		if !strings.Contains(body, want) {
			t.Errorf("the bare card lacks %q", want)
		}
	}
	if strings.Contains(body, "<dt>Hints</dt>") {
		t.Error("a card with no hints shows the line")
	}
	b.detail.Instagram.Disagree = ""
	if body := h.do(get("/leads/5", c)).Body.String(); strings.Contains(body, "Instagram reads this as") {
		t.Error("an agreeing card shows a disagreement")
	}
}

// TestLeadWithoutInstagramUnchanged: a café lead with no Instagram renders byte for byte what the
// page renders with the card's template emptied, so the card adds nothing at all; and a lead
// that is not a café never reaches the card, even with one set.
func TestLeadWithoutInstagramUnchanged(t *testing.T) {
	site := newOutreachSite(t)
	site.b.detail = cafeLeadDetail()
	srv := site.srv
	render := func() string {
		w := httptest.NewRecorder()
		site.mux.ServeHTTP(w, get("/leads/5"))
		if w.Code != http.StatusOK {
			t.Fatalf("status %d", w.Code)
		}
		return w.Body.String()
	}
	// Cloned before either executes: a template cannot be cloned once it has run.
	srv.pages = srv.parseTemplates()
	empty := template.Must(template.Must(srv.pages["lead_detail"].Clone()).Parse(`{{define "lead_instagram_card"}}{{end}}`))
	with := render()
	srv.pages["lead_detail"] = empty
	if without := render(); with != without {
		t.Error("a lead with no Instagram does not render as before")
	}
	if strings.Contains(with, `id="instagram"`) {
		t.Error("a lead with no Instagram shows the card")
	}

	site.b.detail = richLeadDetail("x")
	site.b.detail.Instagram = igCard()
	srv.pages = srv.parseTemplates()
	if strings.Contains(render(), `id="instagram"`) {
		t.Error("a lead that is not a café shows the card")
	}
}

// TestInstagramReviewRowAtAGlance: a read profile's row shows its counts, bio (escaped, its line
// breaks kept), website as a link, newest post, the list it was found in and when it was read,
// with an "Open on Instagram" button to its profile in a new tab; a website that is not an http
// or https address is text, never a link; private and missing accounts say so; a row never
// read shows none of it.
func TestInstagramReviewRowAtAGlance(t *testing.T) {
	b := richBackend("x")
	at := time.Date(2026, 11, 6, 21, 0, 0, 0, time.UTC)
	b.igRows[IGReviewNew] = []IGReviewRow{
		{Handle: "read.example", URL: "https://www.instagram.com/read.example/", Bio: "Roasting since 2019\n<b>Shop online</b>",
			Website: "https://read.example/shop", LatestPost: "New Ethiopia on the shelf", Followers: 12345, Posts: 678, Counted: true,
			CapturedAt: &at, FoundIn: "stampino.app"},
		{Handle: "odd.example", URL: "https://www.instagram.com/odd.example/", Website: "javascript:alert(1)", Private: true, CapturedAt: &at},
		{Handle: "gone.example", URL: "https://www.instagram.com/gone.example/", Missing: true, CapturedAt: &at},
		{Handle: "never.example", URL: "https://www.instagram.com/never.example/"},
	}
	h := newHarness(t, b, false)
	body := igMarkup(t, h.do(get(leadsIGPath, h.session(t))).Body.String())
	rows := map[string]string{}
	for _, part := range strings.Split(body, `<article class="card card-body stack-sm" id="ig-`)[1:] {
		handle, rest, _ := strings.Cut(part, `"`)
		rows[handle], _, _ = strings.Cut(rest, "</article>")
	}
	for handle, wants := range map[string][]string{
		"read.example": {"<span>12,345 followers · 678 posts</span>", `<p class="ig-bio">Roasting since 2019` + "\n" + `&lt;b&gt;Shop online&lt;/b&gt;</p>`,
			`Website: <a href="https://read.example/shop" rel="noopener noreferrer nofollow" target="_blank">https://read.example/shop</a>`,
			"Latest post: New Ethiopia on the shelf", "Found in @stampino.app's following · read Fri 6 Nov 21:00",
			`<a class="btn btn-primary btn-sm" href="https://www.instagram.com/read.example/" rel="noopener noreferrer nofollow" target="_blank">Open on Instagram`},
		"odd.example":   {"Website: javascript:alert(1)</p>", `<span class="badge">private</span>`, "read Fri 6 Nov 21:00", "Open on Instagram"},
		"gone.example":  {`<span class="badge badge-warn">no longer there</span>`},
		"never.example": {`href="https://www.instagram.com/never.example/" rel="noopener noreferrer nofollow" target="_blank">Open on Instagram`},
	} {
		for _, want := range wants {
			if !strings.Contains(rows[handle], want) {
				t.Errorf("@%s lacks %q", handle, want)
			}
		}
	}
	if strings.Contains(rows["odd.example"], `href="javascript`) || strings.Contains(rows["odd.example"], "followers") {
		t.Errorf("@odd.example links a non-web address or counts it did not read: %q", rows["odd.example"])
	}
	for _, not := range []string{"followers", "ig-bio", "Website:", "Latest post:", "Found in", " Nov ", "badge"} {
		if strings.Contains(rows["never.example"], not) {
			t.Errorf("the never-read row shows %q", not)
		}
	}
}
