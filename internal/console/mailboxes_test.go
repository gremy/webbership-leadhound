package console

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gremy/webbership-leadhound/internal/config"
)

// mailboxTestPassword is a mailbox password found nowhere else, so a test can look for it.
var mailboxTestPassword = "pw-" + "Zx81-never-shown"

func boolp(b bool) *bool { return &b }

func richMailboxes(text string) MailboxesPage {
	checked := testSent
	return MailboxesPage{
		Groups: []MailboxDomain{{
			Domain: "northwind.example",
			DNS: []HostCheck{{Host: "mail.northwind.example", Selector: "sel1", At: &checked, SPF: true, DKIM: true, DMARC: false},
				{Host: "northwind.example", Selector: "sel2"}},
			SentToday: 7, DomainLimit: 50, Mailboxes: 2, MailboxLimit: 2,
			Rows: []MailboxRow{
				{ID: 1, Address: "outreach@mail.northwind.example", State: MailboxReady, SentToday: 4, CapToday: 10, RampDay: 3, LastCheck: &checked},
				{ID: 2, Address: "hello@northwind.example", State: MailboxPaused, Reason: text, SentToday: 3, CapToday: 10},
			},
		}},
		Presets: []MailboxPreset{{Name: "Provider", SMTP: "smtp.provider.example:465", IMAP: "imap.provider.example:993", Selector: "prov"}, {Name: "Other"}},
	}
}

func richMailbox(text string) MailboxPage {
	checked := testSent
	return MailboxPage{
		Row:    MailboxRow{ID: 1, Address: "outreach@mail.northwind.example", State: MailboxReady, SentToday: 4, CapToday: 10, RampDay: 3, LastCheck: &checked},
		Domain: "northwind.example", Login: text, SMTPAddr: "smtp.northwind.example:465", IMAPAddr: "imap.northwind.example:993",
		Selector: "sel1", FromName: text,
		Check: passedCard(),
	}
}

func passedCard() MailboxCheckView {
	at := testSent
	return MailboxCheckView{MailboxID: 1, Outcome: MailboxCheckPassed, RequestedAt: &at, CheckedAt: &at,
		SMTPOK: boolp(true), IMAPOK: boolp(true), FoldersOK: boolp(true), SPF: boolp(true), DKIM: boolp(true), DMARC: boolp(true)}
}

func failedCard(fix string) MailboxCheckView {
	at := testSent
	return MailboxCheckView{MailboxID: 1, Outcome: MailboxCheckFailed, RequestedAt: &at, CheckedAt: &at,
		SMTPOK: boolp(true), IMAPOK: boolp(false), Fix: fix}
}

func requestedCard() MailboxCheckView {
	at := testSent
	return MailboxCheckView{MailboxID: 1, Outcome: MailboxCheckRequested, RequestedAt: &at}
}

// mailboxForm is a full add or edit form, password included.
func mailboxForm() url.Values {
	return url.Values{
		"address": {" outreach@mail.northwind.example "}, "login": {""}, "password": {mailboxTestPassword},
		"smtp_addr": {"smtp.northwind.example:465"}, "imap_addr": {"imap.northwind.example:993"},
		"selector": {"sel1"}, "from_name": {"Northwind Outreach"}, "preset": {"Provider"},
	}
}

func wantInput() MailboxInput {
	return MailboxInput{Address: "outreach@mail.northwind.example", SMTPAddr: "smtp.northwind.example:465", IMAPAddr: "imap.northwind.example:993",
		Selector: "sel1", FromName: "Northwind Outreach", Preset: "Provider", Password: config.Secret(mailboxTestPassword)}
}

// mailboxRoute is one mailbox POST route with the form a test sends, where it goes on
// success and what it asks the backend.
type mailboxRoute struct {
	path string
	form url.Values
	next string
	want MailboxAction
}

func mailboxPosts() []mailboxRoute {
	edit := wantInput()
	edit.Address, edit.Preset = "", ""
	return []mailboxRoute{
		{"/mailboxes", mailboxForm(), "/mailboxes/7", MailboxAction{Do: MailboxAdd, Input: wantInput()}},
		{"/mailboxes/1", mailboxForm(), "/mailboxes/1", MailboxAction{Do: MailboxEdit, ID: 1, Input: edit}},
		{"/mailboxes/1/check", url.Values{}, "/mailboxes/1", MailboxAction{Do: MailboxCheck, ID: 1}},
		{"/mailboxes/1/pause", url.Values{"reason": {" holiday "}}, "/mailboxes/1", MailboxAction{Do: MailboxPause, ID: 1, Reason: "holiday"}},
		{"/mailboxes/1/resume", url.Values{}, "/mailboxes/1", MailboxAction{Do: MailboxResume, ID: 1}},
		{"/mailboxes/1/retire", url.Values{"confirm": {"yes"}}, "/mailboxes/1", MailboxAction{Do: MailboxRetire, ID: 1, Confirm: true}},
		{"/mailboxes/1/retire", url.Values{}, "/mailboxes/1", MailboxAction{Do: MailboxRetire, ID: 1}},
		{"/mailboxes/domains/northwind.example/pause", url.Values{"reason": {"blocked"}}, "/mailboxes",
			MailboxAction{Do: MailboxPauseDomain, Domain: "northwind.example", Reason: "blocked"}},
		{"/mailboxes/domains/northwind.example/resume", url.Values{}, "/mailboxes", MailboxAction{Do: MailboxResumeDomain, Domain: "northwind.example"}},
	}
}

func sameMailboxAction(a, b MailboxAction) bool {
	return a.Do == b.Do && a.ID == b.ID && a.Domain == b.Domain && a.Reason == b.Reason && a.Confirm == b.Confirm &&
		a.Input.Password.Reveal() == b.Input.Password.Reveal() && fmt.Sprint(a.Input) == fmt.Sprint(b.Input)
}

// TestMailboxWrites: each mailbox POST asks the backend for its write and, done, goes on
// with the outcome in the one-time cookie; a refusal shows its words (422), with the form
// again for an add or an edit; a server without leads settings says so; a failure is a
// plain 500.
func TestMailboxWrites(t *testing.T) {
	for _, rt := range mailboxPosts() {
		t.Run(rt.path+" "+rt.want.Do, func(t *testing.T) {
			b := richBackend("x")
			b.mbOut = MailboxOutcome{Message: "Done.", MailboxID: 7}
			h := newHarness(t, b, false)
			c := h.session(t)
			w := h.do(post(rt.path, rt.form, c))
			if w.Code != http.StatusSeeOther || w.Header().Get("Location") != rt.next {
				t.Fatalf("got %d to %q, want 303 to %s: %s", w.Code, w.Header().Get("Location"), rt.next, w.Body.String())
			}
			if !sameMailboxAction(b.lastMailbox, rt.want) {
				t.Errorf("asked %+v, want %+v", b.lastMailbox, rt.want)
			}
			if f := cookieNamed(w, "__Host-lh_flash"); f == nil {
				t.Error("no outcome cookie")
			}

			b.mbErr = fmt.Errorf("%w: northwind.example already has 2 mailboxes.", ErrUserFixable)
			w = h.do(post(rt.path, rt.form, c))
			body := w.Body.String()
			if w.Code != http.StatusUnprocessableEntity || !strings.Contains(body, "northwind.example already has 2 mailboxes.") {
				t.Errorf("refusal: %d %s", w.Code, body)
			}
			if strings.Contains(body, mailboxTestPassword) {
				t.Error("the refusal page shows the password")
			}

			b.mbErr = ErrLeadsNotSetUp
			if w = h.do(post(rt.path, rt.form, c)); w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), leadsNotSetUp) {
				t.Errorf("not set up: %d %s", w.Code, w.Body.String())
			}

			b.mbErr = errors.New("database gone")
			if w = h.do(post(rt.path, rt.form, c)); w.Code != http.StatusInternalServerError || strings.Contains(w.Body.String(), "database gone") {
				t.Errorf("failure: %d %s", w.Code, w.Body.String())
			}
			if logs := h.logs.String(); strings.Contains(logs, mailboxTestPassword) {
				t.Errorf("the password was logged: %s", logs)
			}
		})
	}
}

// TestMailboxFormRefusalKeepsTheFields: a refused add or edit shows the form again with
// every field as typed except the password, which is never written into a page.
func TestMailboxFormRefusalKeepsTheFields(t *testing.T) {
	for _, path := range []string{"/mailboxes", "/mailboxes/1"} {
		b := richBackend("x")
		b.mbErr = fmt.Errorf("%w: Give the sending server as host:port.", ErrUserFixable)
		h := newHarness(t, b, false)
		form := mailboxForm()
		form.Set("smtp_addr", "smtp.northwind.example")
		body := h.do(post(path, form, h.session(t))).Body.String()
		for _, want := range []string{`value="smtp.northwind.example"`, `value="imap.northwind.example:993"`, `value="sel1"`,
			`value="Northwind Outreach"`, `type="password"`, `autocomplete="new-password"`, "Give the sending server as host:port."} {
			if !strings.Contains(body, want) {
				t.Errorf("%s: the form again lacks %q", path, want)
			}
		}
		if strings.Contains(body, mailboxTestPassword) {
			t.Errorf("%s: the password was written back into the page", path)
		}
		if path == "/mailboxes" && !strings.Contains(body, `value="outreach@mail.northwind.example"`) {
			t.Error("the add form again lacks the address")
		}
		if strings.Contains(fmt.Sprintf("%+v", b.lastMailbox), mailboxTestPassword) {
			t.Error("the action prints its password")
		}
	}
}

// TestMailboxFormsRefusedBeforeTheBackend: a path the console cannot read never reaches it.
func TestMailboxFormsRefusedBeforeTheBackend(t *testing.T) {
	for _, p := range []string{"/mailboxes/x/pause", "/mailboxes/0/resume", "/mailboxes/-1", "/mailboxes/1/explode"} {
		b := richBackend("x")
		h := newHarness(t, b, false)
		w := h.do(post(p, url.Values{}, h.session(t)))
		if w.Code != http.StatusNotFound && w.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s: %d, want 404 or 405", p, w.Code)
		}
		if n := b.n("MailboxAct"); n != 0 {
			t.Errorf("%s: MailboxAct called %d times", p, n)
		}
	}
	for _, p := range []string{"/mailboxes/x", "/mailboxes/0/edit", "/mailboxes/abc/check"} {
		h := newHarness(t, richBackend("x"), false)
		if w := h.do(get(p, h.session(t))); w.Code != http.StatusNotFound {
			t.Errorf("GET %s: %d, want 404", p, w.Code)
		}
	}
}

// TestMailboxesNeverLogSubmittedValues drives every mailbox route, read and write, done,
// refused, failed and not set up, with values found nowhere else, and reads every log line.
func TestMailboxesNeverLogSubmittedValues(t *testing.T) {
	const secret = "zq7secretvalue"
	const secretID = "987654321"
	form := url.Values{}
	for _, k := range []string{"address", "login", "password", "smtp_addr", "imap_addr", "selector", "from_name", "preset", "reason"} {
		form.Set(k, secret)
	}
	form.Set("confirm", "yes")
	errs := []error{nil, fmt.Errorf("%w: no", ErrUserFixable), ErrLeadsNotSetUp, errors.New("database gone")}
	seen := 0
	for _, rt := range routes {
		p := strings.TrimPrefix(strings.TrimPrefix(rt.pattern, "GET "), "POST ")
		if !strings.HasPrefix(p, "/mailboxes") {
			continue
		}
		seen++
		target := strings.NewReplacer("{id}", secretID, "{domain}", secret+".example").Replace(p)
		for _, e := range errs {
			for _, isHX := range []bool{false, true} {
				b := richBackend("x")
				b.mbErr = e
				if e != nil && !errors.Is(e, ErrUserFixable) {
					b.leadsErr = e
				}
				h := newHarness(t, b, false)
				c := h.session(t)
				r := get(target+"?preset="+secret, c)
				if strings.HasPrefix(rt.pattern, "POST ") {
					r = post(target, form, c)
				}
				if isHX {
					r = hx(r)
				}
				h.do(r)
				if logs := h.logs.String(); strings.Contains(logs, secret) || strings.Contains(logs, secretID) {
					t.Errorf("%s (%v) logged a submitted value: %s", rt.pattern, e, logs)
				}
			}
		}
	}
	if seen < 13 {
		t.Errorf("only %d mailbox routes swept", seen)
	}
}

// TestNoMailboxWriteOnGet: no GET ever writes, whatever it is sent.
func TestNoMailboxWriteOnGet(t *testing.T) {
	h := newHarness(t, richBackend("x"), false)
	c := h.session(t)
	for _, rt := range routes {
		if !strings.HasPrefix(rt.pattern, "GET ") {
			continue
		}
		q := "?preset=Provider&do=retire&confirm=yes&password=" + mailboxTestPassword
		h.do(get(rt.sample+q, c))
		h.do(hx(get(rt.sample+q, c)))
	}
	if n := h.b.n("MailboxAct"); n != 0 {
		t.Errorf("a GET called MailboxAct %d times", n)
	}
}

// TestMailboxesEscaping: backend text on every mailbox page is shown escaped.
func TestMailboxesEscaping(t *testing.T) {
	const evil = "<script>alert(1)</script>"
	b := richBackend(evil)
	b.mbCard = failedCard(evil)
	b.mailbox.Check = failedCard(evil)
	b.mailboxes.RetiredEnv = []string{evil}
	h := newHarness(t, b, false)
	c := h.session(t)
	pages := map[string]string{}
	for _, p := range []string{"/mailboxes", "/mailboxes/1", "/mailboxes/1/edit"} {
		pages[p] = h.do(get(p, c)).Body.String()
	}
	pages["card"] = h.do(hx(get("/mailboxes/1/check", c))).Body.String()
	b.mbErr = fmt.Errorf("%w: %s", ErrUserFixable, evil)
	pages["refused add"] = h.do(post("/mailboxes", url.Values{"address": {evil}}, c)).Body.String()
	for name, body := range pages {
		if strings.Contains(body, "<script>alert") || !strings.Contains(body, "&lt;script&gt;alert(1)&lt;/script&gt;") {
			t.Errorf("%s does not escape backend text", name)
		}
	}
}

// TestMailboxesList: the list groups mailboxes by domain with the domain's limits and
// records, says each mailbox's state in plain words, and offers the domain switch.
func TestMailboxesList(t *testing.T) {
	h := newHarness(t, richBackend("holiday"), false)
	body := h.do(get("/mailboxes", h.session(t))).Body.String()
	for _, want := range []string{
		`href="/mailboxes" aria-current="page">`,
		"<h2>northwind.example</h2>", "7 of 50", "2 of 2",
		"mail.northwind.example", "SPF ok", `DMARC <span class="bad">missing</span>`, "not checked today",
		`href="/mailboxes/1"`, "outreach@mail.northwind.example", "Ready", "Paused: holiday", "4 of 10", "day 3",
		`action="/mailboxes/domains/northwind.example/pause"`, `href="/mailboxes/new"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/mailboxes lacks %q", want)
		}
	}
	if strings.Index(body, ">Leads<") > strings.Index(body, ">Mailboxes<") {
		t.Error("Mailboxes is not after Leads in the nav")
	}

	b := richBackend("x")
	b.mailboxes.Groups[0].Paused, b.mailboxes.Groups[0].PauseReason = true, "policy block"
	ph := newHarness(t, b, false)
	body = ph.do(get("/mailboxes", ph.session(t))).Body.String()
	if !strings.Contains(body, `action="/mailboxes/domains/northwind.example/resume"`) || !strings.Contains(body, "policy block") {
		t.Error("a paused domain does not offer resume with its reason")
	}

	b.mailboxes = MailboxesPage{}
	eh := newHarness(t, b, false)
	if body = eh.do(get("/mailboxes", eh.session(t))).Body.String(); !strings.Contains(body, "No mailbox yet.") {
		t.Error("the empty list lacks its line")
	}
}

func TestMailboxStateWords(t *testing.T) {
	cases := []struct{ state, reason, want string }{
		{MailboxReady, "", "Ready"}, {MailboxNeedsCheck, "", "Needs a check"}, {MailboxChecking, "", "Checking…"},
		{MailboxPaused, "login refused", "Paused: login refused"}, {MailboxPaused, "", "Paused"}, {MailboxRetired, "x", "Retired"},
	}
	for _, c := range cases {
		if got := mailboxState(c.state, c.reason); got != c.want {
			t.Errorf("mailboxState(%q, %q) = %q, want %q", c.state, c.reason, got, c.want)
		}
	}
}

// TestMailboxNotices: a missing key and env lines no longer read are said on the list and
// the form, and a missing key hides the add form.
func TestMailboxNotices(t *testing.T) {
	b := richBackend("x")
	b.mailboxes.KeyMissing = true
	b.mailboxes.RetiredEnv = []string{"LEADHOUND_OUTREACH_USER", "LEADHOUND_IMAP_ADDR"}
	h := newHarness(t, b, false)
	c := h.session(t)
	const key = "Mailboxes can't be added yet: the mailbox key is not set up. See deploy/README.md, Mailboxes."
	list := h.do(get("/mailboxes", c)).Body.String()
	for _, want := range []string{key, "LEADHOUND_OUTREACH_USER", "LEADHOUND_IMAP_ADDR", "no longer read"} {
		if !strings.Contains(list, want) {
			t.Errorf("/mailboxes lacks %q", want)
		}
	}
	if strings.Contains(list, `href="/mailboxes/new"`) {
		t.Error("the list offers an add while the key is missing")
	}
	form := h.do(get("/mailboxes/new", c)).Body.String()
	if !strings.Contains(form, key) || strings.Contains(form, `action="/mailboxes"`) {
		t.Error("the add page shows a form while the key is missing")
	}
	b.mailboxes.KeyMissing, b.mailboxes.RetiredEnv = false, nil
	list = h.do(get("/mailboxes", c)).Body.String()
	if strings.Contains(list, "no longer read") || strings.Contains(list, "key is not set up") {
		t.Error("a notice shows with nothing to say")
	}
}

// TestMailboxPreset: choosing a preset reloads the form with its servers and selector
// filled in, read from the backend, and writes nothing.
func TestMailboxPreset(t *testing.T) {
	b := richBackend("x")
	h := newHarness(t, b, false)
	c := h.session(t)
	body := h.do(get("/mailboxes/new?preset=Provider", c)).Body.String()
	for _, want := range []string{`value="smtp.provider.example:465"`, `value="imap.provider.example:993"`, `value="prov"`,
		`<option value="Provider" selected>`, `name="preset" value="Provider"`, `placeholder="same as the address"`,
		`type="password"`, `autocomplete="new-password"`, `method="get" action="/mailboxes/new"`} {
		if !strings.Contains(body, want) {
			t.Errorf("preset form lacks %q", want)
		}
	}
	body = h.do(get("/mailboxes/new?preset=Nope", c)).Body.String()
	if strings.Contains(body, "smtp.provider.example") || strings.Contains(body, " selected>") {
		t.Error("an unknown preset filled the form")
	}
	if b.n("MailboxAct") != 0 {
		t.Error("a preset reload wrote")
	}
}

// TestMailboxEditForm: the edit form shows the mailbox's settings, never a password, and
// the address as text, since it cannot change.
func TestMailboxEditForm(t *testing.T) {
	b := richBackend("x")
	b.mailbox.Login = "outreach-login"
	h := newHarness(t, b, false)
	body := h.do(get("/mailboxes/1/edit", h.session(t))).Body.String()
	for _, want := range []string{`action="/mailboxes/1"`, `value="outreach-login"`, `value="smtp.northwind.example:465"`,
		"outreach@mail.northwind.example", `type="password"`, "blank keeps the saved password"} {
		if !strings.Contains(body, want) {
			t.Errorf("edit form lacks %q", want)
		}
	}
	if strings.Contains(body, `name="address"`) {
		t.Error("the edit form offers to change the address")
	}
}

// TestMailboxCheckCard: the card polls every 5 s while the check is requested and stops
// once it is done; each tick is named; a failure shows its plain fix; a press from the
// card answers with the card.
func TestMailboxCheckCard(t *testing.T) {
	b := richBackend("x")
	b.mbCard = requestedCard()
	h := newHarness(t, b, false)
	c := h.session(t)
	w := h.do(hx(get("/mailboxes/1/check", c)))
	body := w.Body.String()
	if w.Code != http.StatusOK || !strings.Contains(body, `hx-get="/mailboxes/1/check" hx-trigger="every 5s" hx-swap="outerHTML"`) ||
		!strings.Contains(body, "Checking…") || strings.Contains(body, "<html") {
		t.Fatalf("requested card: %d %s", w.Code, body)
	}
	if strings.Contains(body, `action="/mailboxes/1/check"`) {
		t.Error("a check that waits offers another")
	}

	b.mbCard = passedCard()
	body = h.do(hx(get("/mailboxes/1/check", c))).Body.String()
	if strings.Contains(body, "hx-trigger") {
		t.Error("a finished card still polls")
	}
	for _, want := range []string{"SMTP sign-in", "IMAP sign-in", "Sent and Junk folders", "SPF", "DKIM", "DMARC", "Passed", `action="/mailboxes/1/check"`} {
		if !strings.Contains(body, want) {
			t.Errorf("passed card lacks %q", want)
		}
	}

	b.mbCard = failedCard("Turn on IMAP in the mailbox's settings.")
	body = h.do(hx(get("/mailboxes/1/check", c))).Body.String()
	for _, want := range []string{"Turn on IMAP in the mailbox&#39;s settings.", "tick-bad", "not run"} {
		if !strings.Contains(body, want) {
			t.Errorf("failed card lacks %q", want)
		}
	}

	if w = h.do(get("/mailboxes/1/check", c)); w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/mailboxes/1" {
		t.Errorf("a plain visit: %d %q", w.Code, w.Header().Get("Location"))
	}

	b.mbCard = requestedCard()
	b.mbOut = MailboxOutcome{Message: "Check asked for."}
	w = h.do(hx(post("/mailboxes/1/check", url.Values{}, c)))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Check asked for.") || !strings.Contains(w.Body.String(), `id="mailbox-check"`) {
		t.Errorf("htmx check press: %d %s", w.Code, w.Body.String())
	}
	b.mbErr = fmt.Errorf("%w: That mailbox is retired.", ErrUserFixable)
	w = h.do(hx(post("/mailboxes/1/check", url.Values{}, c)))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "That mailbox is retired.") {
		t.Errorf("htmx refused check: %d %s", w.Code, w.Body.String())
	}
}

// TestMailboxCheckCardNotStarted: a check asked for over five minutes ago that has still
// not started (its request file was missed) stops the card polling, says so and offers
// Check now again, which asks the backend for a check.
func TestMailboxCheckCardNotStarted(t *testing.T) {
	b := richBackend("x")
	b.mbCard = requestedCard()
	b.mbCard.NotStarted = true
	h := newHarness(t, b, false)
	c := h.session(t)
	for _, path := range []string{"/mailboxes/1/check", "/mailboxes/1"} {
		if path == "/mailboxes/1" {
			b.mailbox.Check = b.mbCard
		}
		r := get(path, c)
		if path == "/mailboxes/1/check" {
			r = hx(r)
		}
		body := h.do(r).Body.String()
		if strings.Contains(body, `hx-trigger="every 5s"`) || strings.Contains(body, "Checking…") {
			t.Errorf("%s: a check that has not started still polls:\n%s", path, body)
		}
		for _, want := range []string{"The check has not started.", `action="/mailboxes/1/check"`, "Check now"} {
			if !strings.Contains(body, want) {
				t.Errorf("%s lacks %q:\n%s", path, want, body)
			}
		}
	}
	b.mbOut = MailboxOutcome{Message: "Check asked for."}
	if w := h.do(hx(post("/mailboxes/1/check", url.Values{}, c))); w.Code != http.StatusOK || b.lastMailbox.Do != MailboxCheck {
		t.Errorf("Check now on a check not started: %d, asked %+v", w.Code, b.lastMailbox)
	}
}

// TestMailboxDetail: the detail page carries the mailbox's settings, the check card and
// the switches for its state; a retired mailbox offers none.
func TestMailboxDetail(t *testing.T) {
	b := richBackend("x")
	h := newHarness(t, b, false)
	c := h.session(t)
	body := h.do(get("/mailboxes/1", c)).Body.String()
	for _, want := range []string{"outreach@mail.northwind.example", "smtp.northwind.example:465", `id="mailbox-check"`,
		`action="/mailboxes/1/pause"`, `action="/mailboxes/1/retire"`, `name="confirm" value="yes" required`, `href="/mailboxes/1/edit"`,
		`href="/mailboxes" aria-current="true">`} {
		if !strings.Contains(body, want) {
			t.Errorf("detail lacks %q", want)
		}
	}
	b.mailbox.Row.State, b.mailbox.Row.Reason = MailboxPaused, "login refused"
	body = h.do(get("/mailboxes/1", c)).Body.String()
	if !strings.Contains(body, `action="/mailboxes/1/resume"`) || strings.Contains(body, `action="/mailboxes/1/pause"`) {
		t.Error("a paused mailbox does not offer resume alone")
	}
	b.mailbox.Row.State = MailboxRetired
	b.mailbox.Check.Retired = true
	body = h.do(get("/mailboxes/1", c)).Body.String()
	for _, bad := range []string{`action="/mailboxes/1/resume"`, `action="/mailboxes/1/pause"`, `action="/mailboxes/1/retire"`, `action="/mailboxes/1/check"`, `href="/mailboxes/1/edit"`} {
		if strings.Contains(body, bad) {
			t.Errorf("a retired mailbox offers %q", bad)
		}
	}
	if w := h.do(get("/mailboxes/2", c)); w.Code != http.StatusNotFound {
		t.Errorf("no such mailbox: %d", w.Code)
	}
}

// TestLeadsShowsMailboxes: the Leads page's sending card lists each mailbox with its day
// and links to the Mailboxes page; the approval page shows each message's From.
func TestLeadsShowsMailboxes(t *testing.T) {
	b := richBackend("x")
	b.leads.Sending.Mailboxes = []SendingMailbox{{Address: "outreach@mail.northwind.example", Domain: "northwind.example",
		State: MailboxReady, SentToday: 2, CapToday: 10, DNS: &SenderCheck{At: testSent, SPF: true, DKIM: true, DMARC: true}}}
	b.batch.Items[0].Messages[0].From = MessageFrom{Name: "Northwind Outreach", Address: "outreach@mail.northwind.example"}
	h := newHarness(t, b, false)
	c := h.session(t)
	body := h.do(get("/leads/outreach", c)).Body.String()
	for _, want := range []string{"outreach@mail.northwind.example", "2 of 10", `href="/mailboxes"`, "Ready"} {
		if !strings.Contains(body, want) {
			t.Errorf("/leads/outreach lacks %q", want)
		}
	}
	body = h.do(get("/leads/batches/3", c)).Body.String()
	if !strings.Contains(body, "From: Northwind Outreach &lt;outreach@mail.northwind.example&gt;") {
		t.Error("the batch page does not show the From")
	}
}

// TestSaveMailboxPages renders each mailbox page once for a person to look at, when
// LEADHOUND_CONSOLE_PAGES names a folder.
func TestSaveMailboxPages(t *testing.T) {
	dir := os.Getenv("LEADHOUND_CONSOLE_PAGES")
	if dir == "" {
		t.Skip("LEADHOUND_CONSOLE_PAGES is not set")
	}
	b := richBackend("holiday")
	h := newHarness(t, b, true)
	c := h.session(t)
	save := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil { // #nosec G703 -- a test temp dir
			t.Fatal(err)
		}
	}
	save("list.html", h.do(get("/mailboxes", c)).Body.String())
	b.mbErr = fmt.Errorf("%w: Give the sending server as host:port, or choose a preset.", ErrUserFixable)
	form := mailboxForm()
	form.Set("smtp_addr", "smtp.northwind.example")
	save("form-refused.html", h.do(post("/mailboxes", form, c)).Body.String())
	b.mbErr = nil
	save("detail-passed.html", h.do(get("/mailboxes/1", c)).Body.String())
	b.mailbox.Check = failedCard("The reading server refused the login: check the login and password, then save the password again.")
	save("detail-failed.html", h.do(get("/mailboxes/1", c)).Body.String())
	b.mailbox.Row.State = MailboxChecking
	b.mailbox.Check = requestedCard()
	save("detail-checking.html", h.do(get("/mailboxes/1", c)).Body.String())
	b.mbCard = requestedCard()
	save("check-card-requested.html", h.do(hx(get("/mailboxes/1/check", c))).Body.String())
}

// TestMailboxSendingCard: the card names its campaigns and where its domain lands; before its
// first email the warm-up line says why nothing has gone out, never a bare "not started"; a
// mailbox on the ramp says why nothing goes now on its own line; the list says when warm-up
// starts.
func TestMailboxSendingCard(t *testing.T) {
	b := richBackend("x")
	b.mailbox = richMailbox("x")
	b.mailbox.Row.RampDay = 0
	b.mbSending = MailboxSendingView{MailboxID: 1,
		Campaigns: []FilterOption{{Value: "drupal-eol", Label: "Drupal end-of-life"}, {Value: "cafe", Label: "Cafés"}},
		Landing:   LandingView{State: LandingFound, URL: "https://site.example/", Host: "site.example"},
		Why:       SendingWhy{Blocked: true, Sentence: "No approved email uses this mailbox."}}
	h := newHarness(t, b, false)
	body := h.do(get("/mailboxes/1", h.session(t))).Body.String()
	for _, want := range []string{
		"<dt>Warm-up</dt><dd>Warm-up starts with this mailbox's first email. Nothing has gone out yet because: No approved email uses this mailbox.</dd>",
		`<dt>Campaigns</dt><dd><a href="/leads/campaigns#campaign-drupal-eol">Drupal end-of-life</a>, <a href="/leads/campaigns#campaign-cafe">Cafés</a></dd>`,
		"<dt>Its website goes to</dt><dd>https://site.example/</dd>",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the mailbox card lacks %q", want)
		}
	}
	for _, not := range []string{"not started", "<dt>Sending</dt>"} {
		if strings.Contains(body, not) {
			t.Errorf("the mailbox card shows %q", not)
		}
	}

	// On the ramp, no campaign, the website never answered.
	b.mailbox.Row.RampDay = 4
	b.mbSending.Campaigns, b.mbSending.Landing = nil, LandingView{State: LandingNoAnswer}
	b.mbSending.Why = SendingWhy{Blocked: true, Sentence: "3 approved emails wait: ana@contoso.example is not assigned to Drupal EOL."}
	body = h.do(get("/mailboxes/1", h.session(t))).Body.String()
	for _, want := range []string{
		"<dt>Warm-up</dt><dd>day 4 of the ramp</dd>",
		"<dt>Sending</dt><dd>3 approved emails wait: ana@contoso.example is not assigned to Drupal EOL.</dd>",
		`<dt>Campaigns</dt><dd>none, so it sends nothing. <a href="/leads/campaigns">Assign it on Campaigns</a></dd>`,
		"<dt>Its website goes to</dt><dd>the domain&#39;s website didn&#39;t answer</dd>",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("on the ramp: lacks %q", want)
		}
	}

	// Never checked; and a mailbox the sending read no longer finds keeps its page.
	b.mbSending.Landing = LandingView{State: LandingNotChecked}
	if body = h.do(get("/mailboxes/1", h.session(t))).Body.String(); !strings.Contains(body, "<dt>Its website goes to</dt><dd>not checked yet</dd>") {
		t.Errorf("not checked: %s", body)
	}
	b.mbSending.MailboxID = 99
	w := h.do(get("/mailboxes/1", h.session(t)))
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), "<dt>Campaigns</dt>") || !strings.Contains(w.Body.String(), "day 4 of the ramp") {
		t.Errorf("no sending card: %d", w.Code)
	}

	// The list: no bare "not started".
	b.mailboxes = richMailboxes("x")
	body = h.do(get("/mailboxes", h.session(t))).Body.String()
	if !strings.Contains(body, "warm-up starts with its first email") || strings.Contains(body, "not started") {
		t.Errorf("the list's warm-up words: %s", body)
	}
}
