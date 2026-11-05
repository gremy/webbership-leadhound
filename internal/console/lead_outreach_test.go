package console

import (
	"errors"
	"fmt"
	"html"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// outreachSite serves the routes as New does, without the session in front of them (TestOutreachRoutes
// covers that), so a test can read the lead's page after a write, on the real templates: a form
// field named differently from the handlers' fails them.
type outreachSite struct {
	mux *http.ServeMux
	b   *fakeBackend
	srv *server
}

func newOutreachSite(t *testing.T) *outreachSite {
	t.Helper()
	b := richBackend("x")
	b.detail = richOutreachDetail(b.detail, "x")
	a, _, _ := testAuth(t)
	s := &server{b: b, a: a, opt: Options{Location: time.UTC, Now: func() time.Time { return statusNow }}, log: slog.New(slog.DiscardHandler),
		cookie: "__Host-lh_session", flash: "__Host-lh_flash", device: "__Host-lh_device", themeName: "__Host-lh_theme"}
	s.pages = s.parseTemplates()
	site := &outreachSite{mux: http.NewServeMux(), b: b, srv: s}
	seen := map[string]bool{}
	for _, rt := range routes {
		if !seen[rt.pattern] {
			seen[rt.pattern] = true
			site.mux.HandleFunc(rt.pattern, func(w http.ResponseWriter, r *http.Request) { rt.handle(s, w, r) })
		}
	}
	return site
}

func (o *outreachSite) post(path string, form url.Values) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	o.mux.ServeHTTP(w, post(path, form))
	return w
}

// writes is how many outreach writes reached the backend.
func (o *outreachSite) writes() int {
	n := 0
	for _, name := range []string{"AddContact", "EditContact", "RetireContact", "SaveDraft", "MarkSent", "RequestDraft",
		"DiscardDraft", "SetSector", "AddProposal", "SetProposalStatus"} {
		n += o.b.n(name)
	}
	return n
}

// vals builds a form from name, value pairs.
func vals(pairs ...string) url.Values {
	f := url.Values{}
	for i := 0; i+1 < len(pairs); i += 2 {
		f.Set(pairs[i], pairs[i+1])
	}
	return f
}

// wantSaved checks the 303 and its anchor.
func wantSaved(t *testing.T, w *httptest.ResponseRecorder, loc string) {
	t.Helper()
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != loc {
		t.Errorf("got %d to %q, want 303 to %q; body %.200s", w.Code, w.Header().Get("Location"), loc, w.Body.String())
	}
}

// wantRefused checks the 422 and that the sentence is on the page.
func wantRefused(t *testing.T, w *httptest.ResponseRecorder, sentence string) {
	t.Helper()
	if w.Code != http.StatusUnprocessableEntity {
		t.Errorf("got %d, want 422", w.Code)
	}
	if !strings.Contains(w.Body.String(), html.EscapeString(sentence)) {
		t.Errorf("the refusal %q is not on the page", sentence)
	}
}

// TestOutreachSavedGoesToItsCard: each write hands the backend what was posted, cleaned up as the
// store wants it, and answers 303 to its own card.
func TestOutreachSavedGoesToItsCard(t *testing.T) {
	for _, tc := range []struct {
		name, path string
		form       url.Values
		loc        string
		check      func(*fakeBackend) bool
	}{
		{"add an email", "/leads/5/contacts", vals("kind", "email", "value", " hello@harbourlane.example ", "note", " mine ", "name", "ignored", "person_id", "3"), "/leads/5#contacts",
			func(b *fakeBackend) bool {
				c := b.lastContact
				return c.LeadID == 5 && c.Kind == ContactEmail && c.Value == "hello@harbourlane.example" && c.Note == "mine" && c.Name == "" && c.PersonID == 3
			}},
		{"add a person keeps the value they typed", "/leads/5/contacts", vals("kind", "person", "value", " their-link ", "name", "A Name", "role", "Director", "person_id", "nope"), "/leads/5#contacts",
			func(b *fakeBackend) bool {
				c := b.lastContact
				return c.Kind == ContactPerson && c.Value == "their-link" && c.Name == "A Name" && c.Role == "Director" && c.PersonID == 0
			}},
		{"edit a phone", "/leads/5/contacts/f7/edit", vals("value", "+44 20 7946 0177", "note", "n"), "/leads/5#contacts",
			func(b *fakeBackend) bool {
				return b.lastEdit.Ref == ContactRef{FactID: 7} && b.lastEdit.Value == "+44 20 7946 0177" && b.lastEdit.Note == "n"
			}},
		{"edit a person keeps a value too", "/leads/5/contacts/p3/edit", vals("value", "their-link", "role", "Director"), "/leads/5#contacts",
			func(b *fakeBackend) bool {
				return b.lastEdit.Ref == ContactRef{PersonID: 3} && b.lastEdit.Value == "their-link" && b.lastEdit.Role == "Director"
			}},
		{"retire", "/leads/5/contacts/r13/retire", vals("note", " gone "), "/leads/5#contacts",
			func(b *fakeBackend) bool { return *b.lastRetire == ContactRef{RouteID: 13} && b.lastRetired == "gone" }},
		{"save an edit, a browser's line ends made plain", "/leads/5/drafts/9/save", vals("subject", " Hi ", "body", " one\r\ntwo\r\n"), "/leads/5#draft-9",
			func(b *fakeBackend) bool {
				return *b.lastDraft == DraftEdit{LeadID: 5, MessageID: 9, Subject: "Hi", Body: "one\ntwo"}
			}},
		{"mark sent posts the hash it was shown", "/leads/5/drafts/9/sent", vals("hash", "0123456789ab"), "/leads/5#draft-9",
			func(b *fakeBackend) bool {
				return *b.lastSent == MarkSentRequest{LeadID: 5, MessageID: 9, Hash: "0123456789ab"}
			}},
		{"draft it on the lead's own channel", "/leads/5/draft-it", vals(), "/leads/5#drafts",
			func(b *fakeBackend) bool { return b.lastAsked == "" && b.n("RequestDraft") == 1 }},
		{"draft it on LinkedIn", "/leads/5/draft-it", vals("channel", "linkedin"), "/leads/5#drafts",
			func(b *fakeBackend) bool { return b.lastAsked == ChannelLinkedIn }},
		{"discard", "/leads/5/draft-discard", vals("batch_id", "3"), "/leads/5#drafts",
			func(b *fakeBackend) bool { return b.lastDiscard == 3 }},
		{"sector", "/leads/5/sector", vals("sector", "education"), "/leads/5#sector",
			func(b *fakeBackend) bool { return b.lastSector == "education" }},
		{"a proposal with only a title is a sent one in the first currency", "/leads/5/proposals", vals("title", " Rebuild "), "/leads/5#proposals",
			func(b *fakeBackend) bool {
				p := b.lastProposal
				return p.LeadID == 5 && p.Title == "Rebuild" && p.Status == ProposalSent && p.Currency == Currencies[0] && p.ValueCents == nil && p.URL == ""
			}},
		{"a whole proposal", "/leads/5/proposals", vals("title", "Rebuild", "url", "https://harbourlane.example/p", "value", "2,400.50", "currency", "EUR", "status", "declined", "note", "n"), "/leads/5#proposals",
			func(b *fakeBackend) bool {
				p := b.lastProposal
				return p.URL == "https://harbourlane.example/p" && *p.ValueCents == 240050 && p.Currency == "EUR" && p.Status == ProposalDeclined && p.Note == "n"
			}},
		{"a proposal's status", "/leads/5/proposals/3/status", vals("status", "accepted", "note", "n"), "/leads/5#proposals",
			func(b *fakeBackend) bool {
				return *b.lastStatus == ProposalStatusChange{LeadID: 5, ProposalID: 3, Status: ProposalAccepted, Note: "n"}
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := newOutreachSite(t)
			wantSaved(t, o.post(tc.path, tc.form), tc.loc)
			if o.writes() != 1 {
				t.Fatalf("%d writes, want 1", o.writes())
			}
			if !tc.check(o.b) {
				t.Errorf("the backend was asked the wrong thing")
			}
		})
	}
}

// TestOutreachShowsTheLine: the backend's line wins over the console's own, on the page it lands on.
func TestOutreachShowsTheLine(t *testing.T) {
	o := newOutreachSite(t)
	o.b.outreachMsg = "Already there; now marked as yours."
	w := o.post("/leads/5/contacts", vals("kind", "email", "value", "hello@harbourlane.example"))
	c := cookieNamed(w, "__Host-lh_flash")
	if c == nil {
		t.Fatal("no line was set")
	}
	page := httptest.NewRecorder()
	o.mux.ServeHTTP(page, get("/leads/5", c))
	if !strings.Contains(page.Body.String(), "Already there; now marked as yours.") {
		t.Errorf("the backend's line is not on the page")
	}
}

// TestOutreachLengthLimits: each limit holds in characters, not bytes: at the limit it reaches the
// backend, one past it is refused with the form kept and nothing written.
func TestOutreachLengthLimits(t *testing.T) {
	for _, tc := range []struct {
		name, path, field string
		rest              []string
		limit             int
		refusal           string
	}{
		{"contact note on add", "/leads/5/contacts", "note", []string{"kind", "email", "value", "a@harbourlane.example"}, 200, contactNoteTooLong},
		{"contact value on add", "/leads/5/contacts", "value", []string{"kind", "form"}, MaxContactValue, contactTooLong},
		{"person name on add", "/leads/5/contacts", "name", []string{"kind", "person"}, MaxPersonName, personNameTooLong},
		{"person role on add", "/leads/5/contacts", "role", []string{"kind", "person"}, MaxPersonRole, personRoleTooLong},
		{"contact note on edit", "/leads/5/contacts/r13/edit", "note", []string{"value", "https://linkedin.com/company/harbourlane"}, 200, contactNoteTooLong},
		{"contact value on edit", "/leads/5/contacts/r13/edit", "value", nil, MaxContactValue, contactTooLong},
		{"person name on edit", "/leads/5/contacts/p3/edit", "name", nil, MaxPersonName, personNameTooLong},
		{"person role on edit", "/leads/5/contacts/p3/edit", "role", nil, MaxPersonRole, personRoleTooLong},
		{"retire note", "/leads/5/contacts/r13/retire", "note", nil, 200, contactNoteTooLong},
		{"draft body", "/leads/5/drafts/9/save", "body", nil, 5000, draftBodyTooLong},
		{"draft subject", "/leads/5/drafts/12/save", "subject", []string{"body", "text"}, MaxDraftSubject, draftSubjectLong},
		{"proposal title", "/leads/5/proposals", "title", nil, 120, proposalTitleLong},
		{"proposal note", "/leads/5/proposals", "note", []string{"title", "t"}, MaxProposalNote, proposalNoteLong},
		{"status note", "/leads/5/proposals/3/status", "note", []string{"status", "sent"}, MaxProposalNote, proposalNoteLong},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := newOutreachSite(t)
			form := vals(tc.rest...)
			form.Set(tc.field, strings.Repeat("é", tc.limit))
			if w := o.post(tc.path, form); w.Code != http.StatusSeeOther {
				t.Errorf("at the limit: %d, want 303; %.200s", w.Code, w.Body.String())
			}
			if o.writes() != 1 {
				t.Fatalf("at the limit: %d writes, want 1", o.writes())
			}
			form.Set(tc.field, strings.Repeat("é", tc.limit+1))
			w := o.post(tc.path, form)
			wantRefused(t, w, tc.refusal)
			if o.writes() != 1 {
				t.Errorf("one past the limit reached the backend")
			}
			if tc.field != "note" || tc.path != "/leads/5/proposals" { // the Add form asks no note; only a post can carry one
				if !strings.Contains(w.Body.String(), strings.Repeat("é", tc.limit+1)) {
					t.Errorf("what was typed is not kept")
				}
			}
		})
	}
}

// TestOutreachRefusedBeforeTheBackend: what a form can never rightly post stops at the console.
func TestOutreachRefusedBeforeTheBackend(t *testing.T) {
	for _, tc := range []struct {
		name, path string
		form       url.Values
		refusal    string
	}{
		{"a kind the form does not offer", "/leads/5/contacts", vals("kind", "fax", "value", "1"), badContactKind},
		{"a contact with no value", "/leads/5/contacts", vals("kind", "email", "value", "  "), needContactValue},
		{"a person with neither name nor role", "/leads/5/contacts", vals("kind", "person", "note", "n"), needPersonText},
		{"a person id that is not one", "/leads/5/contacts", vals("kind", "email", "value", "a@harbourlane.example", "person_id", "x"), badContactPerson},
		{"an edit to no value", "/leads/5/contacts/r13/edit", vals("value", ""), needContactValue},
		{"a person edited to nothing", "/leads/5/contacts/p3/edit", vals("name", " ", "role", ""), needPersonText},
		{"an empty text", "/leads/5/drafts/9/save", vals("body", " \r\n "), needDraftBody},
		{"a subject of two lines", "/leads/5/drafts/12/save", vals("subject", "a\nBcc: b@harbourlane.example", "body", "text"), draftSubjectLines},
		{"Mark sent with no hash", "/leads/5/drafts/9/sent", vals(), draftChanged},
		{"a channel Draft it does not offer", "/leads/5/draft-it", vals("channel", "fax"), badDraftChannel},
		{"Discard with no batch", "/leads/5/draft-discard", vals(), badDiscard},
		{"no sector", "/leads/5/sector", vals("sector", " "), needSector},
		{"a proposal with no title", "/leads/5/proposals", vals("title", " "), needProposalTitle},
		{"a proposal link that is not https", "/leads/5/proposals", vals("title", "t", "url", "http://harbourlane.example/p"), badProposalLink},
		{"a proposal value that is not an amount", "/leads/5/proposals", vals("title", "t", "value", "lots"), badDealValue},
		{"a proposal's currency", "/leads/5/proposals", vals("title", "t", "currency", "XXX"), badCurrency},
		{"a proposal's first status", "/leads/5/proposals", vals("title", "t", "status", "lost"), badProposalStatus},
		{"a status the buttons do not offer", "/leads/5/proposals/3/status", vals("status", "lost"), badProposalStatus},
		{"no status", "/leads/5/proposals/3/status", vals(), badProposalStatus},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := newOutreachSite(t)
			wantRefused(t, o.post(tc.path, tc.form), tc.refusal)
			if o.writes() != 0 {
				t.Errorf("%d writes reached the backend", o.writes())
			}
		})
	}
}

// TestOutreachStatusGrid: the console keeps no status rules of its own, so the surprising cells of
// the grid are the backend's refusals arriving as a 422 on the right form, with what was typed
// kept, and the allowed cells reaching the backend whatever the lead's state says.
func TestOutreachStatusGrid(t *testing.T) {
	refused := func(sentence string) error { return fmt.Errorf("%w: %s", ErrUserFixable, sentence) }
	typed := "my kept words"
	for _, tc := range []struct {
		name, path string
		form       url.Values
		refusal    string
		kept       bool
	}{
		{"a stale hash", "/leads/5/drafts/9/sent", vals("hash", "000000000000"), "This draft changed; reload.", false},
		{"Mark sent on an email", "/leads/5/drafts/12/sent", vals("hash", "ab0123456789"), "An email is sent from its batch page.", false},
		{"Mark sent in a shadow batch", "/leads/5/drafts/9/sent", vals("hash", "0123456789ab"), "Shadow mode; nothing is marked sent.", false},
		{"Mark sent twice", "/leads/5/drafts/11/sent", vals("hash", "ba9876543210"), "Already marked sent.", false},
		{"Mark sent once they replied", "/leads/5/drafts/9/sent", vals("hash", "0123456789ab"), "They already answered.", false},
		{"Edit on an approved email", "/leads/5/drafts/12/save", vals("subject", "s", "body", typed), "This email is approved; it can no longer change.", true},
		{"Edit once it is done", "/leads/5/drafts/9/save", vals("body", typed), "This message was already sent.", true},
		{"Edit once they replied", "/leads/5/drafts/9/save", vals("body", typed), "They already answered.", true},
		{"a text the checks refuse", "/leads/5/drafts/9/save", vals("body", typed), "The note is over 200 characters.", true},
		{"Draft it with a request open", "/leads/5/draft-it", vals("channel", "linkedin"), "Already asked for at 09:12.", false},
		{"Draft it on a contacted lead", "/leads/5/draft-it", vals(), "Already contacted.", false},
		{"Draft it with an open batch", "/leads/5/draft-it", vals(), "It already has a draft.", false},
		{"Discard of a morning batch", "/leads/5/draft-discard", vals("batch_id", "3"), "That batch holds other leads.", false},
		{"a sector off the list", "/leads/5/sector", vals("sector", typed), "Choose a sector from the list.", false},
		{"an address the store refuses", "/leads/5/contacts", vals("kind", "email", "value", typed), "That is not an email address.", true},
		{"a retired contact", "/leads/5/contacts/r14/retire", vals("note", typed), "It is already retired.", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := newOutreachSite(t)
			o.b.outreachErr = refused(tc.refusal)
			w := o.post(tc.path, tc.form)
			wantRefused(t, w, tc.refusal)
			if o.writes() != 1 {
				t.Errorf("%d writes, want the one the backend refused", o.writes())
			}
			if tc.kept && !strings.Contains(w.Body.String(), typed) {
				t.Errorf("what was typed is not kept")
			}
		})
	}
	t.Run("a contacted lead keeps Edit and Mark sent", func(t *testing.T) {
		o := newOutreachSite(t)
		o.b.detail.State = "contacted"
		wantSaved(t, o.post("/leads/5/drafts/10/save", vals("body", "after they accept")), "/leads/5#draft-10")
		wantSaved(t, o.post("/leads/5/drafts/10/sent", vals("hash", "0123456789ab")), "/leads/5#draft-10")
	})
}

// TestOutreachHostileValuesAreEscaped: what a POST carries comes back as text, whichever form it
// went to and whether the console or the backend refused it.
func TestOutreachHostileValuesAreEscaped(t *testing.T) {
	const hostile = `</textarea><script>alert(1)</script>"'><img src=x onerror=alert(2)>`
	over := strings.Repeat("x", 5001) + hostile
	for _, tc := range []struct {
		name, path string
		form       url.Values
		err        error
		shown      bool // a text field or the refusal shows it; a select or a button has nowhere to
	}{
		{"a body too long", "/leads/5/drafts/9/save", vals("body", over), nil, true},
		{"a refused body", "/leads/5/drafts/9/save", vals("body", hostile), fmt.Errorf("%w: refused %s", ErrUserFixable, hostile), true},
		{"a contact", "/leads/5/contacts", vals("kind", "email", "value", hostile, "note", hostile), fmt.Errorf("%w: bad %s", ErrUserFixable, hostile), true},
		{"a contact's kind", "/leads/5/contacts", vals("kind", hostile, "value", hostile), nil, true},
		{"a contact edit", "/leads/5/contacts/r13/edit", vals("value", hostile, "note", strings.Repeat("n", 201)+hostile), nil, true},
		{"a proposal", "/leads/5/proposals", vals("title", hostile, "url", hostile), nil, true},
		{"a proposal's status", "/leads/5/proposals/3/status", vals("status", hostile, "note", hostile), nil, false},
		{"a sector", "/leads/5/sector", vals("sector", hostile), fmt.Errorf("%w: no %s", ErrUserFixable, hostile), true},
		{"a channel", "/leads/5/draft-it", vals("channel", hostile), nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := newOutreachSite(t)
			o.b.outreachErr = tc.err
			w := o.post(tc.path, tc.form)
			if w.Code != http.StatusUnprocessableEntity {
				t.Fatalf("got %d, want 422", w.Code)
			}
			body := w.Body.String()
			for _, raw := range []string{"<script>alert", "<img src=x", "</textarea><script"} {
				if strings.Contains(body, raw) {
					t.Errorf("%q came back raw", raw)
				}
			}
			if tc.shown && !strings.Contains(body, "&lt;script&gt;alert(1)") {
				t.Errorf("the value is not on the page as text")
			}
		})
	}
}

// TestOutreachFailureAnswers: each kind of backend failure gets its own answer on every write.
func TestOutreachFailureAnswers(t *testing.T) {
	writes := []struct {
		path string
		form url.Values
	}{
		{"/leads/5/contacts", vals("kind", "email", "value", "a@harbourlane.example")},
		{"/leads/5/contacts/r13/edit", vals("value", "v")},
		{"/leads/5/contacts/r13/retire", vals()},
		{"/leads/5/drafts/9/save", vals("body", "b")},
		{"/leads/5/drafts/9/sent", vals("hash", "h")},
		{"/leads/5/draft-it", vals()},
		{"/leads/5/draft-discard", vals("batch_id", "3")},
		{"/leads/5/sector", vals("sector", "other")},
		{"/leads/5/proposals", vals("title", "t")},
		{"/leads/5/proposals/3/status", vals("status", "sent")},
	}
	for _, c := range writes {
		for _, f := range []struct {
			err  error
			code int
		}{
			{ErrNotFound, http.StatusNotFound},
			{ErrLeadsNotSetUp, http.StatusUnprocessableEntity},
			{errors.New("database secret words"), http.StatusInternalServerError},
		} {
			o := newOutreachSite(t)
			o.b.outreachErr = f.err
			w := o.post(c.path, c.form)
			if w.Code != f.code {
				t.Errorf("%s with %v: %d, want %d", c.path, f.err, w.Code, f.code)
			}
			if strings.Contains(w.Body.String(), "secret words") {
				t.Errorf("%s: the failure's words reached the page", c.path)
			}
		}
	}
}

// TestOutreachBadPaths: a path that names no message, contact or proposal is a 404 before the
// backend.
func TestOutreachBadPaths(t *testing.T) {
	o := newOutreachSite(t)
	for _, p := range []string{
		"/leads/5/drafts/nope/save", "/leads/5/drafts/0/save", "/leads/5/drafts/-1/sent", "/leads/nope/drafts/9/save", "/leads/0/drafts/9/sent",
		"/leads/5/contacts/x9/edit", "/leads/5/contacts/r0/retire", "/leads/5/contacts/r05/edit", "/leads/0/contacts/r12/edit",
		"/leads/5/proposals/nope/status", "/leads/5/proposals/0/status", "/leads/0/proposals/3/status",
	} {
		if w := o.post(p, vals("body", "b", "value", "v", "note", "n", "status", "sent", "hash", "h")); w.Code != http.StatusNotFound {
			t.Errorf("POST %s: %d, want 404", p, w.Code)
		}
	}
	if o.writes() != 0 {
		t.Errorf("%d writes reached the backend", o.writes())
	}
}

// TestDraftStatusPolling: the fragment carries the 5-second trigger only while a request is open,
// so a request that ended, or none, stops the polling.
func TestDraftStatusPolling(t *testing.T) {
	const trigger = `hx-trigger="every 5s"`
	at := statusNow.Add(-time.Hour)
	for _, tc := range []struct {
		name string
		req  *LeadDraftRequest
		poll bool
	}{
		{"never asked", nil, false},
		{"queued", &LeadDraftRequest{Status: DraftQueued, RequestedAt: at}, true},
		{"drafting", &LeadDraftRequest{Status: DraftDrafting, RequestedAt: at}, true},
		{"done", &LeadDraftRequest{Status: DraftDone, BatchID: 3, RequestedAt: at}, false},
		{"failed", &LeadDraftRequest{Status: DraftFailed, Reason: "<b>no key</b>", RequestedAt: at}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := newOutreachSite(t)
			o.b.draftRequest = tc.req
			w := httptest.NewRecorder()
			o.mux.ServeHTTP(w, hx(get("/leads/draft-status/5")))
			body := w.Body.String()
			if w.Code != http.StatusOK || strings.Contains(body, "<html") {
				t.Fatalf("got %d, %q", w.Code, body)
			}
			if strings.Contains(body, trigger) != tc.poll {
				t.Errorf("polling: %q, want %v", body, tc.poll)
			}
			if tc.poll && !strings.Contains(body, `hx-get="/leads/draft-status/5"`) {
				t.Errorf("no hx-get: %q", body)
			}
			if strings.Contains(body, "<b>no key") {
				t.Errorf("a reason came back raw: %q", body)
			}
		})
	}
}

// TestDraftStatusFailures: the fragment's read answers like any leads read.
func TestDraftStatusFailures(t *testing.T) {
	o := newOutreachSite(t)
	o.b.leadsErr = ErrNotFound
	w := httptest.NewRecorder()
	o.mux.ServeHTTP(w, hx(get("/leads/draft-status/5")))
	if w.Code != http.StatusNotFound {
		t.Errorf("got %d, want 404", w.Code)
	}
}

// TestTimelineDotKinds: every kind has its dot, and each dot other than the sent one has its colour
// in console.css (tl-email takes the timeline's own colour).
func TestTimelineDotKinds(t *testing.T) {
	css, err := staticFS.ReadFile("static/console.css")
	if err != nil {
		t.Fatal(err)
	}
	for kind, want := range map[string]string{
		TimelineEvent: "tl-event", TimelineEmail: "tl-email", TimelineReply: "tl-reply", TimelineCall: "tl-call", TimelineCheck: "tl-check", TimelineStage: "tl-stage",
		TimelineDraft: "tl-check", TimelineEdit: "tl-check", TimelineDone: "tl-email", TimelineContact: "tl-event", TimelineResearch: "tl-check",
		TimelineProposal: "tl-stage", TimelineSector: "tl-event", "unknown": "tl-event",
	} {
		got := timelineDot(kind)
		if got != want {
			t.Errorf("%s: %s, want %s", kind, got, want)
		}
		if got != "tl-email" && !strings.Contains(string(css), "li."+got+"::before") {
			t.Errorf("%s has no colour in console.css", got)
		}
	}
}
