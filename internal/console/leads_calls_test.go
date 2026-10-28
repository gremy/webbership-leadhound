package console

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/gremy/webbership-leadhound/internal/campaign"
)

// outcomeCall is what the last LeadCallOutcome call was asked.
type outcomeCall struct {
	ID             int64
	Outcome, Note  string
	Consent        bool
	ConsentAddress string
}

func (f *fakeBackend) LeadsToday(_ context.Context, c string) (TodayView, error) {
	f.count("LeadsToday")
	f.mu.Lock()
	f.todayQ = c
	f.mu.Unlock()
	t := f.today
	t.Campaign = c // the backend echoes the campaign it took
	return t, f.leadsRead()
}

func (f *fakeBackend) LeadCall(_ context.Context, id int64) (CallCard, error) {
	f.count("LeadCall")
	if err := f.leadsRead(); err != nil {
		return CallCard{}, err
	}
	if id != f.call.MessageID {
		return CallCard{}, ErrNotFound
	}
	return f.call, nil
}

func (f *fakeBackend) LeadCallOutcome(_ context.Context, id int64, outcome, note string, consent bool, addr string) (string, error) {
	f.count("LeadCallOutcome")
	f.mu.Lock()
	f.lastOutcome = outcomeCall{ID: id, Outcome: outcome, Note: note, Consent: consent, ConsentAddress: addr}
	f.mu.Unlock()
	return f.outcomeMsg, f.outcomeErr
}

func (f *fakeBackend) LeadSequence(_ context.Context, id int64) ([]SequenceStep, error) {
	f.count("LeadSequence")
	if err := f.leadsRead(); err != nil {
		return nil, err
	}
	if id != f.detail.LeadID {
		return nil, ErrNotFound
	}
	return f.seq, nil
}

func richToday(text string) TodayView {
	at := testSent
	return TodayView{
		Campaigns:    []FilterOption{{"drupal-eol", "Drupal end-of-life"}, {"wp-eol", "WordPress end-of-life"}},
		CallsNow:     []TodayItem{{LeadID: 5, MessageID: 9, OrgName: text, Line: "step 3 call, a direct line", At: &at}},
		CallsOverdue: []TodayItem{{LeadID: 6, MessageID: 11, OrgName: "Contoso Ltd", Line: "step 3 call", At: &at}},
		Replies:      []TodayItem{{LeadID: 7, OrgName: "Fabrikam", Line: "replied to the first email", At: &at}},
		EmailsDue:    7,
		Emails:       []TodayItem{{LeadID: 8, OrgName: "Tailspin", Line: "the second email"}, {LeadID: 10, OrgName: "Litware", Line: "the last email"}},
		Manual:       []TodayItem{{LeadID: 12, MessageID: 13, OrgName: "Adatum", Line: "contact form"}},
	}
}

func richCall(text string) CallCard {
	at := testSent
	return CallCard{
		MessageID: 9, LeadID: 5, OrgName: text, Number: "+44 (0)20 7946 0000", Dial: "+442079460000",
		SourceURL: "https://northwind.example/contact", SourceReadAt: &at, LineKind: "direct",
		TalkingPoints: []string{"Your site runs Drupal 10, which ends on 9 December.", "The update keeps your modules working.", text},
		BlockedReason: "Outside calling hours now (09:30 to 17:00 their time, weekdays)",
		Screening:     "not checked yet: screen it before you call",
		Latest:        &CallLog{Outcome: campaign.OutcomeNoAnswer, Note: "rang out", At: testSent},
		Shadow:        true, Due: &at,
	}
}

func richSequence() []SequenceStep {
	at := testSent
	return []SequenceStep{
		{Step: 0, Type: "email", MessageID: 20, State: SeqSent, At: &at},
		{Step: 1, Type: "email", MessageID: 21, State: SeqSent, At: &at, Shadow: true},
		{Step: 2, Type: "call", MessageID: 9, State: SeqDue, At: &at},
		{Step: 3, Type: "email", MessageID: 22, State: SeqWaiting},
		{Step: 4, Type: "email", MessageID: 23, State: SeqCancelled, Reason: campaign.CancelCallConnected},
	}
}

// TestTodayPage: the Today tab shows its five sections in order (no calls-off banner: the
// call rules decide per call, each line saying why it cannot be made), each
// call linked to its card, the email count with the first few, and the campaign filter.
func TestTodayPage(t *testing.T) {
	b := richBackend("Northwind Trading")
	h := newHarness(t, b, false)
	w := h.do(get("/leads/today?campaign=wp-eol", h.session(t)))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	body := w.Body.String()
	last := -1
	for _, section := range []string{"Calls to make now", "Calls overdue", "Replies waiting", "Emails due today",
		"Forms and LinkedIn notes to send by hand"} {
		i := strings.Index(body, section)
		if i < 0 || i < last {
			t.Errorf("%s missing or out of order", section)
		}
		last = i
	}
	for _, want := range []string{
		`<a href="/leads/today" aria-current="page">Today</a>`,
		`<a href="/leads/calls/9">Northwind Trading</a>`, `<a href="/leads/calls/11">Contoso Ltd</a>`,
		`step 3 call, a direct line · <span class="nowrap">due Sat 24 Oct 04:50</span>`,
		`<a href="/leads/7">Fabrikam</a>`, "7 emails go out today, sent by the send job.",
		`<a href="/leads/8">Tailspin</a>`, "and 5 more.", `<a href="/leads/12">Adatum</a>`,
		`<option value="wp-eol" selected>WordPress end-of-life</option>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("Today lacks %q", want)
		}
	}
	if b.todayQ != "wp-eol" {
		t.Errorf("campaign %q reached the backend, want wp-eol", b.todayQ)
	}
}

// TestTodayEmpty: every section says what comes next when it has nothing; no banner when
// calls are allowed; no filter without campaigns.
func TestTodayEmpty(t *testing.T) {
	b := richBackend("x")
	b.today = TodayView{}
	h := newHarness(t, b, false)
	body := h.do(get("/leads/today", h.session(t))).Body.String()
	for _, want := range []string{"No calls to make right now.", "Nothing overdue.", "No replies waiting.",
		"No emails due today. The send job sends them on its own", "No forms or LinkedIn notes due."} {
		if !strings.Contains(body, want) {
			t.Errorf("empty Today lacks %q", want)
		}
	}
	for _, not := range []string{"calls-off", `id="today-campaign"`} {
		if strings.Contains(body, not) {
			t.Errorf("empty Today shows %q", not)
		}
	}
	if b.todayQ != "" {
		t.Errorf("campaign %q, want every campaign", b.todayQ)
	}
}

// TestCallCard: the card shows the number as a tel: link, where it came from, the line, the
// opener, the three talking points, the blocked banner and six outcome buttons posting to
// the card's outcome route.
func TestCallCard(t *testing.T) {
	h := newHarness(t, richBackend("Northwind Trading"), false)
	w := h.do(get("/leads/calls/9", h.session(t)))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		"<h1>Call Northwind Trading</h1>", `<a href="/leads/today" aria-current="true">Today</a>`,
		`href="tel:&#43;442079460000"`, "&#43;44 (0)20 7946 0000</a>",
		`Read on <a href="https://northwind.example/contact" rel="noopener noreferrer nofollow">`,
		"<dt>Line</dt><dd>a direct line</dd>",
		"Say who you are and why you're calling. Your number must be shown.",
		"<li>Your site runs Drupal 10, which ends on 9 December.</li>", "<li>Northwind Trading</li>",
		`class="notice notice-warn call-blocked"`,
		"<strong>Not callable: outside calling hours now (09:30 to 17:00 their time, weekdays).</strong> You can still record what happened below.",
		"not checked yet: screen it before you call", "No answer · Sat 24 Oct 04:50", "rang out", "shadow batch",
		`action="/leads/calls/9/outcome"`, `name="consent" value="yes"`, `name="consent_address"`, `maxlength="500"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the card lacks %q", want)
		}
	}
	for _, o := range campaign.Outcomes {
		if !strings.Contains(body, `name="outcome" value="`+o+`"`) {
			t.Errorf("no %s button", o)
		}
	}
	if n := strings.Count(body, `name="outcome"`); n != 6 {
		t.Errorf("%d outcome buttons, want 6", n)
	}
	// Calls allowed: no banner.
	b := richBackend("x")
	b.call.Callable, b.call.BlockedReason = true, ""
	h = newHarness(t, b, false)
	if body := h.do(get("/leads/calls/9", h.session(t))).Body.String(); strings.Contains(body, "call-blocked") {
		t.Error("a callable card shows the blocked banner")
	}
	if w := h.do(get("/leads/calls/99", h.session(t))); w.Code != http.StatusNotFound {
		t.Errorf("no such call: %d", w.Code)
	}
}

// TestCallCardDialsNormalised: the tel: link dials the stored normalised number, never one
// rebuilt from the printed text ("+44 (0)20 7803 1100" printed, +442078031100 dialled), and
// the visible text stays as printed. No normalised number (an erased fact): no link at all.
func TestCallCardDialsNormalised(t *testing.T) {
	for _, c := range []struct {
		number, dial, wantHref, wantText string
	}{
		{"+44 (0)20 7803 1100", "+442078031100", `href="tel:&#43;442078031100"`, "&#43;44 (0)20 7803 1100</a>"},
		{"0044 20 7803 1100", "+442078031100", `href="tel:&#43;442078031100"`, "0044 20 7803 1100</a>"},
		{"", "", "", "No number on file"},
		{"+44 (0)20 7803 1100", "", "", `<span class="call-number">&#43;44 (0)20 7803 1100</span>`},
	} {
		b := richBackend("x")
		b.call.Number, b.call.Dial = c.number, c.dial
		h := newHarness(t, b, false)
		body := h.do(get("/leads/calls/9", h.session(t))).Body.String()
		if c.wantHref == "" {
			if strings.Contains(body, `href="tel:`) {
				t.Errorf("%q / %q: a tel: link with no normalised number", c.number, c.dial)
			}
		} else if !strings.Contains(body, c.wantHref) {
			t.Errorf("%q / %q: no %s", c.number, c.dial, c.wantHref)
		}
		if !strings.Contains(body, c.wantText) {
			t.Errorf("%q / %q: the card lacks %q", c.number, c.dial, c.wantText)
		}
		if strings.Contains(body, "tel:&#43;4402078031100") {
			t.Errorf("%q: dials the printed number with its national 0", c.number)
		}
	}
}

// TestCallOutcome: each outcome reaches the backend with the note and, for connected only,
// the consent, then goes back (303) to the card with the backend's line.
func TestCallOutcome(t *testing.T) {
	cases := []struct {
		form url.Values
		want outcomeCall
		next string
	}{
		{url.Values{"outcome": {"no_answer"}, "note": {"  rang out "}, "back": {"/leads/calls/9"}},
			outcomeCall{ID: 9, Outcome: "no_answer", Note: "rang out"}, "/leads/calls/9"},
		{url.Values{"outcome": {"connected"}, "consent": {"yes"}, "consent_address": {" ana@northwind.example "}},
			outcomeCall{ID: 9, Outcome: "connected", Consent: true, ConsentAddress: "ana@northwind.example"}, "/leads/calls/9"},
		{url.Values{"outcome": {"voicemail"}, "consent": {"yes"}, "consent_address": {"ana@northwind.example"}, "back": {"/leads/today?campaign=wp-eol"}},
			outcomeCall{ID: 9, Outcome: "voicemail"}, "/leads/today?campaign=wp-eol"},
		{url.Values{"outcome": {"skipped"}, "back": {"https://evil.example/"}}, outcomeCall{ID: 9, Outcome: "skipped"}, "/leads"},
	}
	for _, c := range cases {
		b := richBackend("x")
		b.outcomeMsg = "Saved. The last email goes on Tue 27 Oct."
		h := newHarness(t, b, false)
		w := h.do(post("/leads/calls/9/outcome", c.form, h.session(t)))
		if w.Code != http.StatusSeeOther || w.Header().Get("Location") != c.next {
			t.Errorf("%v: %d to %q, want 303 to %q", c.form, w.Code, w.Header().Get("Location"), c.next)
		}
		if b.lastOutcome != c.want {
			t.Errorf("%v: backend asked %+v, want %+v", c.form, b.lastOutcome, c.want)
		}
		if fl := cookieNamed(w, "__Host-lh_flash"); fl == nil || fl.Value == "" {
			t.Errorf("%v: no flash", c.form)
		}
	}
	// The flash shows on the card.
	b := richBackend("x")
	b.outcomeMsg = "Saved. The last email goes on Tue 27 Oct."
	h := newHarness(t, b, false)
	c := h.session(t)
	w := h.do(post("/leads/calls/9/outcome", url.Values{"outcome": {"no_answer"}}, c))
	r := get("/leads/calls/9", c)
	r.AddCookie(cookieNamed(w, "__Host-lh_flash"))
	if body := h.do(r).Body.String(); !strings.Contains(body, "Saved. The last email goes on Tue 27 Oct.") {
		t.Error("the card does not show the saved line")
	}
}

// TestCallOutcomeRefused: an unknown outcome is 400 and a long note or bad address 422,
// all before the backend; a backend refusal goes back to the card, shown there in its words.
func TestCallOutcomeRefused(t *testing.T) {
	for _, form := range []url.Values{{"outcome": {"hung_up"}}, {}, {"outcome": {" "}}} {
		b := richBackend("x")
		h := newHarness(t, b, false)
		if w := h.do(post("/leads/calls/9/outcome", form, h.session(t))); w.Code != http.StatusBadRequest {
			t.Errorf("%v: %d, want 400", form, w.Code)
		}
		if b.total() != 0 {
			t.Errorf("%v reached the backend", form)
		}
	}
	for form, want := range map[string]url.Values{
		noteTooLong:    {"outcome": {"skipped"}, "note": {strings.Repeat("é", 501)}},
		badConsentAddr: {"outcome": {"connected"}, "consent": {"yes"}, "consent_address": {"not an address"}},
	} {
		b := richBackend("x")
		h := newHarness(t, b, false)
		w := h.do(post("/leads/calls/9/outcome", want, h.session(t)))
		if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), form) {
			t.Errorf("%q: %d", form, w.Code)
		}
		if b.n("LeadCallOutcome") != 0 {
			t.Errorf("%q reached LeadCallOutcome", form)
		}
	}
	// Exactly 500 characters is taken.
	b := richBackend("x")
	h := newHarness(t, b, false)
	if w := h.do(post("/leads/calls/9/outcome", url.Values{"outcome": {"skipped"}, "note": {strings.Repeat("é", 500)}}, h.session(t))); w.Code != http.StatusSeeOther {
		t.Errorf("a 500-character note: %d", w.Code)
	}
	for _, words := range []string{"Not due yet: this call is due on Mon 2 Nov.", "They replied: the sequence has ended."} {
		b := richBackend("x")
		b.outcomeErr = fmt.Errorf("%w: %s", ErrUserFixable, words)
		h := newHarness(t, b, false)
		c := h.session(t)
		w := h.do(post("/leads/calls/9/outcome", url.Values{"outcome": {"no_answer"}, "note": {"kept"}}, c))
		if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/leads/calls/9" {
			t.Errorf("%q: %d to %q", words, w.Code, w.Header().Get("Location"))
			continue
		}
		r := get("/leads/calls/9", c)
		r.AddCookie(cookieNamed(w, "__Host-lh_flash"))
		if body := h.do(r).Body.String(); !strings.Contains(body, strings.ReplaceAll(words, "'", "&#39;")) ||
			!strings.Contains(body, `class="notice notice-error"`) {
			t.Errorf("%q: the card does not show it: %s", words, body)
		}
	}
	b = richBackend("x")
	b.outcomeErr = errors.New("database gone")
	h = newHarness(t, b, false)
	if w := h.do(post("/leads/calls/9/outcome", url.Values{"outcome": {"no_answer"}}, h.session(t))); w.Code != http.StatusInternalServerError ||
		strings.Contains(w.Body.String(), "database gone") {
		t.Errorf("a failure: %d %s", w.Code, w.Body.String())
	}
	b = richBackend("x")
	b.outcomeErr = ErrNotFound
	h = newHarness(t, b, false)
	if w := h.do(post("/leads/calls/9/outcome", url.Values{"outcome": {"no_answer"}}, h.session(t))); w.Code != http.StatusNotFound {
		t.Errorf("no such call: %d", w.Code)
	}
	for _, p := range []string{"/leads/calls/0/outcome", "/leads/calls/x/outcome"} {
		b := richBackend("x")
		h := newHarness(t, b, false)
		if w := h.do(post(p, url.Values{"outcome": {"no_answer"}}, h.session(t))); w.Code != http.StatusNotFound || b.total() != 0 {
			t.Errorf("%s: %d, %d backend calls", p, w.Code, b.total())
		}
	}
}

// TestCallOutcomeCrossOrigin: an outcome from another site is refused before the backend,
// signed in or not, and one without a session never writes.
func TestCallOutcomeCrossOrigin(t *testing.T) {
	b := richBackend("x")
	h := newHarness(t, b, false)
	c := h.session(t)
	form := url.Values{"outcome": {"do_not_call"}}
	r := post("/leads/calls/9/outcome", form, c)
	r.Header.Set("Sec-Fetch-Site", "cross-site")
	if w := h.do(r); w.Code != http.StatusForbidden {
		t.Errorf("cross-site: %d, want 403", w.Code)
	}
	r = post("/leads/calls/9/outcome", form, c)
	r.Header.Set("Origin", "https://evil.example")
	if w := h.do(r); w.Code != http.StatusForbidden {
		t.Errorf("cross-origin: %d, want 403", w.Code)
	}
	if w := h.do(post("/leads/calls/9/outcome", form)); w.Code != http.StatusUnauthorized {
		t.Errorf("without a session: %d, want 401", w.Code)
	}
	if n := b.n("LeadCallOutcome"); n != 0 {
		t.Errorf("LeadCallOutcome called %d times", n)
	}
}

// TestLeadSequenceSection: the lead page words each step's state, after Messages, and links
// a call step to its card; a lead with no steps says when they start.
func TestLeadSequenceSection(t *testing.T) {
	h := newHarness(t, richBackend("Northwind Trading"), false)
	body := h.do(get("/leads/5", h.session(t))).Body.String()
	for _, want := range []string{
		`<span class="item-head">Step 1: an email</span><span class="item-meta">sent Sat 24 Oct 04:50</span>`,
		"sent in shadow Sat 24 Oct 04:50 (nothing left the server)",
		`Step 3: a phone call</span><span class="item-meta">due Sat 24 Oct 04:50 · <a href="/leads/calls/9">Open the call card</a>`,
		"Step 4: an email</span><span class=\"item-meta\">waiting on the call",
		"cancelled: the call got through, so the sequence ended",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the Sequence section lacks %q", want)
		}
	}
	b := richBackend("x")
	b.seq = nil
	h = newHarness(t, b, false)
	if body := h.do(get("/leads/5", h.session(t))).Body.String(); !strings.Contains(body, "No sequence yet") {
		t.Error("an empty sequence says nothing")
	}
}

// TestSequenceWords: every state and the store's own cancel reasons are worded, with no
// internal code left in.
func TestSequenceWords(t *testing.T) {
	s := &server{opt: Options{Location: testSent.Location()}}
	at := testSent
	rows := s.sequenceRows([]SequenceStep{
		{Step: 0, Type: "email", State: SeqDraft},
		{Step: 1, Type: "call", State: SeqNoNumber},
		{Step: 2, Type: "call", MessageID: 4, State: SeqDone, At: &at, Outcome: campaign.OutcomeWrongNumber},
		{Step: 3, Type: "email", State: SeqCancelled, Reason: "opt_out"},
		{Step: 4, Type: "email", MessageID: 6, State: SeqCheck},
		{Step: 5, Type: "call", MessageID: 5, State: SeqWaiting},
	})
	want := []string{"drafted, waiting for the batch to be approved", "skipped because there was no number we may call",
		"called Sat 24 Oct 04:50: wrong number", "cancelled: they opted out", "may have gone out: check it on the Outreach page",
		"waiting on the email before it"}
	for i, r := range rows {
		if r.Words != want[i] || strings.Contains(r.Words, "_") {
			t.Errorf("step %d: %q, want %q", i, r.Words, want[i])
		}
	}
	if rows[1].Card != "" || rows[2].Card != "/leads/calls/4" {
		t.Errorf("cards: %q %q", rows[1].Card, rows[2].Card)
	}
	for _, reason := range campaign.EngineReasons {
		if w := cancelWords(reason); strings.Contains(w, "_") || w == reason {
			t.Errorf("%s is not worded: %q", reason, w)
		}
	}
	for _, o := range campaign.Outcomes {
		if callOutcomeWords[o] == "" || callOutcomeHelp[o] == "" {
			t.Errorf("outcome %s has no words", o)
		}
	}
}

// TestTelHref: only a normalised number (optional +, then digits) becomes a tel: target;
// printed text, a stray + or anything else gives no link.
func TestTelHref(t *testing.T) {
	for in, want := range map[string]string{
		"+442078031100": "+442078031100", "02079460000": "02079460000", "+": "", "": "",
		"+44 (0)20 7803 1100": "", "(020) 7946-0000": "", "javascript:alert(1)": "", "0040+721": "", "++44": "",
	} {
		if got := telHref(in); got != want {
			t.Errorf("telHref(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestSequenceWaitsOn: a waiting step names the step it really waits on, the nearest earlier
// step that has a row and was not cancelled: "the call" only when that is the call, "the
// email before it" when it is the email just before, else the email by its step number. A
// lead with no number never reads "waiting on the call".
func TestSequenceWaitsOn(t *testing.T) {
	s := &server{opt: Options{Location: testSent.Location()}}
	at := testSent
	phone := s.sequenceRows([]SequenceStep{
		{Step: 0, Type: "email", MessageID: 1, State: SeqDue, At: &at},
		{Step: 1, Type: "email", MessageID: 2, State: SeqWaiting},
		{Step: 2, Type: "call", MessageID: 3, State: SeqWaiting},
		{Step: 3, Type: "email", MessageID: 4, State: SeqWaiting},
		{Step: 4, Type: "email", MessageID: 5, State: SeqWaiting},
	})
	noPhone := s.sequenceRows([]SequenceStep{
		{Step: 0, Type: "email", MessageID: 1, State: SeqDue, At: &at},
		{Step: 1, Type: "email", MessageID: 2, State: SeqWaiting},
		{Step: 2, Type: "call", State: SeqNoNumber},
		{Step: 3, Type: "email", State: SeqNoNumber},
		{Step: 4, Type: "email", MessageID: 3, State: SeqWaiting},
	})
	skipped := s.sequenceRows([]SequenceStep{
		{Step: 0, Type: "email", MessageID: 1, State: SeqSent, At: &at},
		{Step: 1, Type: "email", MessageID: 2, State: SeqSent, At: &at},
		{Step: 2, Type: "call", MessageID: 3, State: SeqWaiting},
		{Step: 3, Type: "email", MessageID: 4, State: SeqCancelled, Reason: campaign.CancelCallSkipped},
		{Step: 4, Type: "email", MessageID: 5, State: SeqWaiting},
	})
	for _, c := range []struct {
		name string
		rows []seqRow
		want []string
	}{
		{"phone", phone, []string{"", "waiting on the email before it", "waiting on the email before it", "waiting on the call", "waiting on the email before it"}},
		{"no phone", noPhone, []string{"", "waiting on the email before it", "", "", "waiting on the email at step 2"}},
		{"cancelled before", skipped, []string{"", "", "waiting on the email before it", "", "waiting on the call at step 3"}},
	} {
		for i, w := range c.want {
			if w == "" {
				continue
			}
			if c.rows[i].Words != w {
				t.Errorf("%s step %d: %q, want %q", c.name, i+1, c.rows[i].Words, w)
			}
		}
		if c.name == "no phone" {
			for _, r := range c.rows {
				if strings.Contains(r.Words, "call") && !strings.Contains(r.Words, "no number") {
					t.Errorf("no phone: %q mentions a call", r.Words)
				}
			}
		}
	}
}

// TestCallCardNotDue: a call that is not due says why near the top and its six outcome
// buttons stay visible but disabled; a due call's buttons are live. A calls-off warning
// shows when the backend sets one.
func TestCallCardNotDue(t *testing.T) {
	b := richBackend("Northwind Trading")
	b.call.NotDue = "Not due yet: this call waits on the email before it."
	b.call.CallsOff = "Calls to Ireland are off in this campaign's rules. Your call whether to ring; if you don't, choose Skipped."
	h := newHarness(t, b, false)
	body := h.do(get("/leads/calls/9", h.session(t))).Body.String()
	for _, want := range []string{
		`<p class="notice notice-warn call-not-due" role="note">Not due yet: this call waits on the email before it.</p>`,
		"Calls to Ireland are off in this campaign&#39;s rules. Your call whether to ring; if you don&#39;t, choose Skipped.",
		"<strong>Not callable: outside calling hours now (09:30 to 17:00 their time, weekdays).</strong> Once it is due, you can record what happened below.",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the card lacks %q", want)
		}
	}
	if n := strings.Count(body, `name="outcome"`); n != 6 {
		t.Errorf("%d outcome buttons, want 6 (visible)", n)
	}
	if n := strings.Count(body, `disabled>`); n != 6 {
		t.Errorf("%d disabled buttons, want 6", n)
	}
	h = newHarness(t, richBackend("x"), false)
	if body := h.do(get("/leads/calls/9", h.session(t))).Body.String(); strings.Contains(body, "disabled>") || strings.Contains(body, "call-not-due") {
		t.Error("a due call's card is disabled")
	}
}

// TestCallOutcomeRefusedRedirects: a backend refusal goes back (303) to the card, never
// sitting on the POST URL, and the card shows the refusal as an error once.
func TestCallOutcomeRefusedRedirects(t *testing.T) {
	b := richBackend("x")
	b.outcomeErr = fmt.Errorf("%w: %s", ErrUserFixable, "This call isn't due yet: it waits on step 2 (email). Record what happened once it's due. Nothing was saved.")
	h := newHarness(t, b, false)
	c := h.session(t)
	w := h.do(post("/leads/calls/9/outcome", url.Values{"outcome": {"no_answer"}, "back": {"/leads/today"}}, c))
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/leads/calls/9" {
		t.Fatalf("refused: %d to %q, want 303 to the card", w.Code, w.Header().Get("Location"))
	}
	r := get("/leads/calls/9", c)
	r.AddCookie(cookieNamed(w, "__Host-lh_flash"))
	body := h.do(r).Body.String()
	if !strings.Contains(body, `<p class="notice notice-error" role="alert">This call isn&#39;t due yet: it waits on step 2 (email). Record what happened once it&#39;s due. Nothing was saved.</p>`) {
		t.Errorf("the card does not show the refusal as an error:\n%s", body)
	}
}

// TestTodayCallsWaiting: calls not due yet are listed with what they wait on, each linked to
// its card, under their own heading; no calls-off banner shows (the call rules decide per
// call).
func TestTodayCallsWaiting(t *testing.T) {
	b := richBackend("x")
	b.today = TodayView{
		CallsWaiting:      []TodayItem{{LeadID: 5, MessageID: 9, OrgName: "Northwind", Line: "Step 3 · call · waits on the email before it"}},
		CallsWaitingTotal: 3}
	h := newHarness(t, b, false)
	body := h.do(get("/leads/today", h.session(t))).Body.String()
	for _, want := range []string{
		"Calls waiting", `<a href="/leads/calls/9">Northwind</a>`, "Step 3 · call · waits on the email before it",
		"and 2 more.",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("Today lacks %q", want)
		}
	}
	for _, not := range []string{"calls-off", "walk the flow", "The calls below still show"} {
		if strings.Contains(body, not) {
			t.Errorf("Today shows the retired calls-off banner (%q)", not)
		}
	}
}

// TestTodayCallsDue: calls due but not callable now are listed under "Calls due" with their
// reason, never under "Calls to make now", whose heading then carries no count and whose
// empty line points at them; the section sits straight after "Calls to make now".
func TestTodayCallsDue(t *testing.T) {
	b := richBackend("x")
	b.today = TodayView{
		CallsDue: []TodayItem{
			{LeadID: 5, MessageID: 9, OrgName: "Northwind", Line: "Step 3 · call · outside calling hours now (09:30 to 17:00 their time, weekdays)"},
			{LeadID: 6, MessageID: 11, OrgName: "Contoso", Line: "Step 3 · call · calls are off"},
		}}
	h := newHarness(t, b, false)
	body := h.do(get("/leads/today", h.session(t))).Body.String()
	now := strings.Index(body, "Calls to make now")
	due := strings.Index(body, `id="today-calls-due-title"><span class="today-step" aria-hidden="true">2</span> Calls due <span class="today-count">2</span>`)
	overdue := strings.Index(body, "Calls overdue")
	if now < 0 || due < now || overdue < due {
		t.Fatalf("sections out of order: now %d, due %d, overdue %d", now, due, overdue)
	}
	if head := body[now : strings.Index(body[now:], "</h2>")+now]; strings.Contains(head, "today-count") {
		t.Errorf("Calls to make now counts calls that cannot be made: %q", head)
	}
	for _, want := range []string{
		"None of the calls due can be made right now; they are under Calls due, each with the reason.",
		`<a href="/leads/calls/9">Northwind</a>`, "outside calling hours now (09:30 to 17:00 their time, weekdays)",
		`<a href="/leads/calls/11">Contoso</a>`, "Step 3 · call · calls are off",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("Today lacks %q", want)
		}
	}
	if strings.Index(body, "Northwind") > strings.Index(body, "Contoso") {
		t.Error("Calls due lost the store's order")
	}
	// No calls due: no section.
	b.today = TodayView{}
	if body := h.do(get("/leads/today", h.session(t))).Body.String(); strings.Contains(body, "today-calls-due") {
		t.Error("an empty Today shows Calls due")
	}
}

// TestTodaySectionsNumbered: every section shown has a number, in display order with no gap;
// "Calls due" (due today, can't be made now) has one only when it shows, straight after
// "Calls to make now"; the calls not due yet come last.
func TestTodaySectionsNumbered(t *testing.T) {
	step := regexp.MustCompile(`id="today-([a-z-]+)-title"><span class="today-step" aria-hidden="true">(\d+)</span>`)
	for _, c := range []struct {
		name string
		due  bool
		want []string
	}{
		{"no calls due", false, []string{"calls-now", "calls-overdue", "replies", "emails", "manual", "calls-waiting"}},
		{"calls due", true, []string{"calls-now", "calls-due", "calls-overdue", "replies", "emails", "manual", "calls-waiting"}},
	} {
		b := richBackend("Northwind Trading")
		if c.due {
			at := testSent
			b.today.CallsDue = []TodayItem{{LeadID: 14, MessageID: 15, OrgName: "Wingtip", Line: "Step 3 · call · calls off for BE in the campaign rules", At: &at}}
		}
		h := newHarness(t, b, false)
		body := h.do(get("/leads/today", h.session(t))).Body.String()
		got := step.FindAllStringSubmatch(body, -1)
		if len(got) != len(c.want) {
			t.Fatalf("%s: %d numbered sections, want %d: %v", c.name, len(got), len(c.want), got)
		}
		for i, m := range got {
			if m[1] != c.want[i] || m[2] != strconv.Itoa(i+1) {
				t.Errorf("%s: section %d is %s numbered %s, want %s numbered %d", c.name, i, m[1], m[2], c.want[i], i+1)
			}
		}
		if strings.Count(body, `class="card today-section"`) != len(c.want) {
			t.Errorf("%s: a section without a number", c.name)
		}
	}
}

// TestTodayDueTimeKeptTogether: a call's due date and time sit in one no-wrap span, so a long
// reason never splits "due Sat 24 Oct 04:50" across lines.
func TestTodayDueTimeKeptTogether(t *testing.T) {
	h := newHarness(t, richBackend("Northwind Trading"), false)
	body := h.do(get("/leads/today", h.session(t))).Body.String()
	if !strings.Contains(body, `<span class="nowrap">due Sat 24 Oct 04:50</span>`) {
		t.Error("the due time is not in a no-wrap span")
	}
	css, err := os.ReadFile("static/console.css")
	if err != nil || !regexp.MustCompile(`\.nowrap \{\s*white-space: nowrap;`).Match(css) {
		t.Errorf("no .nowrap rule (%v)", err)
	}
}

// TestTodayShadowManual: with no form or LinkedIn note listed but some in shadow batches, the
// empty state says they are practice instead of that none exist.
func TestTodayShadowManual(t *testing.T) {
	b := richBackend("x")
	b.today = TodayView{ShadowManual: 54}
	h := newHarness(t, b, false)
	body := h.do(get("/leads/today", h.session(t))).Body.String()
	if !strings.Contains(body, "54 forms and LinkedIn notes are in shadow batches and not listed here: shadow batches are practice.") {
		t.Error("the shadow empty state is missing")
	}
	if strings.Contains(body, "No forms or LinkedIn notes due.") {
		t.Error("says none are due while shadow batches hold some")
	}
	b.today = TodayView{ShadowManual: 1}
	if body := h.do(get("/leads/today", h.session(t))).Body.String(); !strings.Contains(body, "1 form or LinkedIn note is in shadow batches") {
		t.Error("the singular shadow line is missing")
	}
}

// TestCallCardDone: once the sequence has moved on after the outcome, the card says Done with
// the outcome in the buttons' words and its time, drops the due line and the calls-off
// banners, and every outcome button is disabled with the reason.
func TestCallCardDone(t *testing.T) {
	b := richBackend("Northwind Trading")
	at := testSent
	b.call.Done = true
	b.call.Latest = &CallLog{Outcome: campaign.OutcomeConnected, Note: "spoke to Ana", At: at}
	h := newHarness(t, b, false)
	body := h.do(get("/leads/calls/9", h.session(t))).Body.String()
	for _, want := range []string{"<strong>Done: Got through · Sat 24 Oct 04:50.</strong>",
		"The sequence has moved on, so another outcome can't be recorded.", "The buttons are off"} {
		if !strings.Contains(body, want) {
			t.Errorf("a done card lacks %q", want)
		}
	}
	if n := strings.Count(body, `" disabled>`); n != 6 {
		t.Errorf("%d disabled outcome buttons, want 6", n)
	}
	for _, not := range []string{"<dt>Due</dt>", "call-blocked", "connected"} {
		if strings.Contains(strings.ReplaceAll(body, `value="connected"`, ""), not) {
			t.Errorf("a done card shows %q", not)
		}
	}
	// Not done: the buttons work.
	b = richBackend("Northwind Trading")
	h = newHarness(t, b, false)
	if body := h.do(get("/leads/calls/9", h.session(t))).Body.String(); strings.Contains(body, " disabled>") || strings.Contains(body, "call-done") {
		t.Error("a card not done shows as done")
	}
}

// TestSequenceCallNote: a made call's line carries the operator's note, on one line, cut when
// long, and escaped on the page.
func TestSequenceCallNote(t *testing.T) {
	s := &server{opt: Options{Location: testSent.Location()}}
	at := testSent
	long := strings.Repeat("word ", 40)
	rows := s.sequenceRows([]SequenceStep{
		{Step: 2, Type: "call", MessageID: 4, State: SeqDone, At: &at, Outcome: campaign.OutcomeConnected, CallNote: "spoke to\nAna"},
		{Step: 2, Type: "call", MessageID: 5, State: SeqDone, At: &at, Outcome: campaign.OutcomeNoAnswer, CallNote: long},
	})
	if want := `called Sat 24 Oct 04:50: got through · "spoke to Ana"`; rows[0].Words != want {
		t.Errorf("got %q, want %q", rows[0].Words, want)
	}
	if n := utf8.RuneCountInString(rows[1].Words); !strings.HasSuffix(rows[1].Words, `…"`) || n > 160 {
		t.Errorf("long note not cut: %d %q", n, rows[1].Words)
	}
	b := richBackend("Northwind Trading")
	b.seq = []SequenceStep{{Step: 2, Type: "call", MessageID: 9, State: SeqDone, At: &at, Outcome: campaign.OutcomeConnected, CallNote: "<b>ok</b>"}}
	h := newHarness(t, b, false)
	body := h.do(get("/leads/5", h.session(t))).Body.String()
	if strings.Contains(body, "<b>ok</b>") || !strings.Contains(body, "&lt;b&gt;ok&lt;/b&gt;") {
		t.Error("the note is not escaped on the lead page")
	}
}

// TestOutcomeWordsAreTheButtons: the saved line the console falls back to uses the button's words.
func TestOutcomeWordsAreTheButtons(t *testing.T) {
	b := richBackend("x")
	h := newHarness(t, b, false)
	c := h.session(t)
	w := h.do(post("/leads/calls/9/outcome", url.Values{"outcome": {"voicemail"}}, c))
	r := get("/leads/calls/9", c)
	r.AddCookie(cookieNamed(w, "__Host-lh_flash"))
	if body := h.do(r).Body.String(); !strings.Contains(body, "Saved: Left a voicemail.") {
		t.Error("the fallback saved line does not use the button words")
	}
}

// TestAfterColon: a reason after "Not callable:" reads lower-case, an acronym stays.
func TestAfterColon(t *testing.T) {
	for in, want := range map[string]string{
		"Not screened": "not screened", "TPS: not screened": "TPS: not screened", "": "", "already": "already", "É bine": "é bine",
	} {
		if got := AfterColon(in); got != want {
			t.Errorf("AfterColon(%q) = %q, want %q", in, got, want)
		}
	}
}
