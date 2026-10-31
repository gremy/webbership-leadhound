package console

import (
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"
)

// confirmForm is a confirm press of richSendPlan with every warning shown and the tick.
func confirmForm() url.Values {
	f := url.Values{"hash": {"fedcba987654"}, "read": {"1"}}
	f["code"] = slices.Clone(SendWarningCodes)
	return f
}

func newSendHarness(t *testing.T) *harness {
	t.Helper()
	b := richBackend("Brightwater Dental")
	b.confirmOut = ControlOutcome{Message: "Asked: 2 emails go out in the next minutes."}
	return newHarness(t, b, false)
}

// harnessAs is newHarness with the operator's email set.
func harnessAs(t *testing.T, b *fakeBackend, operator string) *harness {
	t.Helper()
	a, clock, calls := testAuth(t)
	logs := &syncBuffer{}
	hh := New(b, a, Options{Location: time.UTC, Logger: slog.New(slog.NewTextHandler(logs, nil)), Now: func() time.Time { return statusNow }, Operator: operator})
	return &harness{h: hh, b: b, a: a, clock: clock, calls: calls, logs: logs, name: "__Host-lh_session"}
}

func TestSendNowPreviewRedirects(t *testing.T) {
	for _, c := range []struct {
		name string
		form url.Values
		want ManualSendRequest
		next string
	}{
		{"one lead", url.Values{"lead": {"5"}, "back": {"/leads/5"}}, ManualSendRequest{LeadIDs: []int64{5}}, "/leads/send-now/7?back=%2Fleads%2F5"},
		{"chosen leads, repeats once", url.Values{"lead": {"5", "6", "5"}, "back": {"/leads?page=2"}}, ManualSendRequest{LeadIDs: []int64{5, 6}},
			"/leads/send-now/7?back=%2Fleads%3Fpage%3D2"},
		{"a batch", url.Values{"batch": {"3"}, "hash": {"0123456789ab"}, "back": {"/leads/batches/3"}}, ManualSendRequest{BatchID: 3},
			"/leads/send-now/7?back=%2Fleads%2Fbatches%2F3"},
		{"no back", url.Values{"lead": {"5"}}, ManualSendRequest{LeadIDs: []int64{5}}, "/leads/send-now/7"},
		{"another host is no back", url.Values{"lead": {"5"}, "back": {"https://elsewhere.example/"}}, ManualSendRequest{LeadIDs: []int64{5}},
			"/leads/send-now/7?back=%2Fleads"},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newSendHarness(t)
			w := h.do(post("/leads/send-now/preview", c.form, h.session(t)))
			if w.Code != http.StatusSeeOther || w.Header().Get("Location") != c.next {
				t.Fatalf("%d to %q, want 303 to %q", w.Code, w.Header().Get("Location"), c.next)
			}
			if got := h.b.lastPreview; !slices.Equal(got.LeadIDs, c.want.LeadIDs) || got.BatchID != c.want.BatchID {
				t.Errorf("asked %+v, want %+v", got, c.want)
			}
		})
	}
}

func TestSendNowPreviewRefused(t *testing.T) {
	for _, c := range []struct {
		name    string
		form    url.Values
		err     error
		words   string
		next    string
		backend bool
	}{
		{"nothing chosen", url.Values{"back": {"/leads"}}, nil, "Choose at least 1 lead to send now.", "/leads", false},
		{"both", url.Values{"lead": {"5", "6"}, "batch": {"3"}, "back": {"/leads/batches/3"}}, nil,
			"Send now takes chosen leads or a batch, not both; you sent a batch and 2 leads.", "/leads/batches/3", false},
		{"bad lead", url.Values{"lead": {"5", "x"}}, nil, "One of the chosen leads is not a lead number; choose the leads again.", "/leads", false},
		{"bad batch", url.Values{"batch": {"-3"}, "back": {"/leads/batches/3"}}, nil, "That is not a batch number; open the batch again.", "/leads/batches/3", false},
		{"leave-outs ticked", url.Values{"batch": {"3"}, "skip": {"1", "2"}, "back": {"/leads/batches/3"}}, nil,
			"Send now takes the whole batch; untick the 2 items left out, or use Approve.", "/leads/batches/3", false},
		{"backend refuses", url.Values{"lead": {"5"}, "back": {"/leads/5"}},
			fmt.Errorf("%w: Shadow mode is on: 0 of 1 leads can be sent.", ErrUserFixable), "Shadow mode is on: 0 of 1 leads can be sent.", "/leads/5", true},
		{"gone", url.Values{"lead": {"5"}, "back": {"/leads/5"}}, ErrNotFound, "That batch or lead no longer exists; nothing was sent.", "/leads/5", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newSendHarness(t)
			h.b.previewErr = c.err
			w := h.do(post("/leads/send-now/preview", c.form, h.session(t)))
			if w.Code != http.StatusSeeOther || w.Header().Get("Location") != c.next {
				t.Fatalf("%d to %q, want 303 to %q", w.Code, w.Header().Get("Location"), c.next)
			}
			if got := flashOf(t, w); got != c.words {
				t.Errorf("flash %q, want %q", got, c.words)
			}
			if called := h.b.n("SendNowPreview") == 1; called != c.backend {
				t.Errorf("backend called %v, want %v", called, c.backend)
			}
		})
	}
}

// flashOf is the one-time message w set.
func flashOf(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	for _, c := range w.Result().Cookies() {
		if c.Name == "__Host-lh_flash" && c.MaxAge > 0 {
			b, err := base64.RawURLEncoding.DecodeString(c.Value)
			if err != nil {
				t.Fatal(err)
			}
			return string(b)
		}
	}
	return ""
}

func TestSendConfirmPage(t *testing.T) {
	h := newSendHarness(t)
	w := h.do(get("/leads/send-now/7?back=/leads/5", h.session(t)))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		`action="/leads/send-now/7/confirm"`, `name="hash" value="fedcba987654"`, `name="back" value="/leads/5"`,
		"Send 2 emails now?", "6 warnings you can go past", "1 lead left out (hard stops)",
		"The email check says invalid: the mailbox does not exist.", "Waiting · email not checked yet",
		`formaction="/leads/send-now/7/check-email"`, "One email per recipient domain goes in each run.",
		`name="read" value="1" required`, "I have read the 6 warnings and want to send past them.", "Send anyway (2)",
		`href="/leads/5">Cancel`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("lacks %q", want)
		}
	}
	for _, c := range SendWarningCodes {
		if !strings.Contains(body, `name="code" value="`+c+`"`) {
			t.Errorf("no hidden code %s", c)
		}
		if !strings.Contains(body, "Warning "+c+": 18 of 18 today. This goes 1 over.") {
			t.Errorf("no sentence for %s", c)
		}
	}
	for _, bad := range []string{"<style", " style=", "hx-on", "onclick", "<script>", "hx-trigger"} {
		if strings.Contains(body, bad) {
			t.Errorf("contains %q", bad)
		}
	}
}

func TestSendConfirmNoWarningsSaysSendNow(t *testing.T) {
	h := newSendHarness(t)
	h.b.plan.Warnings = nil
	body := h.do(get("/leads/send-now/7", h.session(t))).Body.String()
	if !strings.Contains(body, "Send now (2)") || strings.Contains(body, "Send anyway") {
		t.Error("the button is not Send now")
	}
	if strings.Contains(body, `name="read"`) || strings.Contains(body, `name="code"`) {
		t.Error("a page with no warnings asks for the tick or carries codes")
	}
	if !strings.Contains(body, `href="/leads/batches/7">Cancel`) {
		t.Error("Cancel without a back does not go to the batch")
	}
}

func TestSendConfirmNoButton(t *testing.T) {
	onlyStops := richSendPlan("x")
	onlyStops.Leads = []SendPlanLead{{LeadID: 8, Name: "Fernway Clinic", Stop: "Opted out on 2 Oct."}}
	onlyStops.CanSend = true // even when the backend says so, no lead goes
	cannot := richSendPlan("x")
	cannot.CanSend = false
	waiting := richSendPlan("x")
	waiting.Waiting = "Claude is writing 2 drafts for this batch."
	for name, p := range map[string]SendPlan{"only hard stops": onlyStops, "CanSend false": cannot, "waiting": waiting} {
		t.Run(name, func(t *testing.T) {
			h := newSendHarness(t)
			h.b.plan = p
			body := h.do(get("/leads/send-now/7", h.session(t))).Body.String()
			for _, bad := range []string{"Send anyway", "Send now (", `name="read"`} {
				if strings.Contains(body, bad) {
					t.Errorf("shows %q", bad)
				}
			}
			if !strings.Contains(body, ">Back</a>") {
				t.Error("no way back")
			}
		})
	}
}

func TestSendConfirmWaitingPolls(t *testing.T) {
	h := newSendHarness(t)
	h.b.plan.Waiting = "Claude is writing 2 drafts for this batch."
	body := h.do(get("/leads/send-now/7?back=/leads", h.session(t))).Body.String()
	for _, want := range []string{
		`hx-get="/leads/send-now/7?back=%2Fleads" hx-trigger="every 3s" hx-select="#send-confirm"`,
		"Claude is writing 2 drafts for this batch.", `<a href="/leads/send-now/7?back=%2Fleads">Refresh</a>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("lacks %q", want)
		}
	}
	// The poll gets the same page; htmx picks the block out of it.
	w := h.do(hx(get("/leads/send-now/7?back=/leads", h.session(t))))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `id="send-confirm"`) {
		t.Errorf("poll: %d", w.Code)
	}
	h.b.plan.Waiting = ""
	if body := h.do(get("/leads/send-now/7", h.session(t))).Body.String(); strings.Contains(body, "hx-trigger") || strings.Contains(body, ">Refresh<") {
		t.Error("a ready page still polls")
	}
}

func TestSendConfirmUnknownBatch(t *testing.T) {
	h := newSendHarness(t)
	if w := h.do(get("/leads/send-now/99", h.session(t))); w.Code != http.StatusNotFound {
		t.Errorf("status %d, want 404", w.Code)
	}
	for _, p := range []string{"/leads/send-now/0", "/leads/send-now/x"} {
		if w := h.do(get(p, h.session(t))); w.Code != http.StatusNotFound {
			t.Errorf("%s: status %d, want 404", p, w.Code)
		}
	}
}

func TestSendNowConfirmPassesWhatWasShown(t *testing.T) {
	b := richBackend("x")
	b.confirmOut = ControlOutcome{Message: "Asked: 2 emails go out in the next minutes."}
	h := harnessAs(t, b, "operator@example.com")
	f := confirmForm()
	f["code"] = append(f["code"], "ramp") // a repeat is sent once
	f.Set("back", "/leads/batches/3")
	w := h.do(post("/leads/send-now/7/confirm", f, h.session(t)))
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/leads/batches/3" {
		t.Fatalf("%d to %q", w.Code, w.Header().Get("Location"))
	}
	want := ManualConfirm{BatchID: 7, HashPrefix: "fedcba987654", Codes: SendWarningCodes, Operator: "operator@example.com"}
	got := h.b.lastConfirm
	if got.BatchID != want.BatchID || got.HashPrefix != want.HashPrefix || !slices.Equal(got.Codes, want.Codes) || got.Operator != want.Operator {
		t.Errorf("confirmed %+v, want %+v", got, want)
	}
	if msg := flashOf(t, w); msg != "Asked: 2 emails go out in the next minutes." {
		t.Errorf("flash %q", msg)
	}
	// No back: the batch page.
	w = h.do(post("/leads/send-now/7/confirm", confirmForm(), h.session(t)))
	if w.Header().Get("Location") != "/leads/batches/7" {
		t.Errorf("no back: to %q", w.Header().Get("Location"))
	}
}

func TestSendNowConfirmNeedsTheTick(t *testing.T) {
	h := newSendHarness(t)
	f := confirmForm()
	f.Del("read")
	w := h.do(post("/leads/send-now/7/confirm", f, h.session(t)))
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status %d, want 422", w.Code)
	}
	if !strings.Contains(w.Body.String(), "Tick that you have read the 6 warnings before you send past them; nothing was sent.") {
		t.Error("the refusal is not on the page")
	}
	if n := h.b.n("SendNowConfirm"); n != 0 {
		t.Errorf("SendNowConfirm called %d times", n)
	}
	// With no warnings shown there is nothing to tick.
	h.b.plan.Warnings = nil
	w = h.do(post("/leads/send-now/7/confirm", url.Values{"hash": {"fedcba987654"}}, h.session(t)))
	if w.Code != http.StatusSeeOther || h.b.n("SendNowConfirm") != 1 {
		t.Errorf("no warnings: %d, confirm called %d", w.Code, h.b.n("SendNowConfirm"))
	}
}

func TestSendNowConfirmRefusesStrangeInput(t *testing.T) {
	for name, f := range map[string]url.Values{
		"unknown code": {"hash": {"fedcba987654"}, "read": {"1"}, "code": {"ramp", "ceiling"}},
		"no hash":      {"read": {"1"}, "code": {"ramp"}},
	} {
		t.Run(name, func(t *testing.T) {
			h := newSendHarness(t)
			w := h.do(post("/leads/send-now/7/confirm", f, h.session(t)))
			if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), "notice-error") {
				t.Errorf("status %d", w.Code)
			}
			if n := h.b.n("SendNowConfirm"); n != 0 {
				t.Errorf("SendNowConfirm called %d times", n)
			}
		})
	}
}

func TestSendNowConfirmPlanChanged(t *testing.T) {
	h := newSendHarness(t)
	now := richSendPlan("x")
	now.HashPrefix = "aaaabbbbcccc"
	now.Warnings = []SendWarning{{Code: WarnRamp, Sentence: "Mailbox 1 has sent 19 of 18 today (warm-up). This goes 2 over."}}
	h.b.confirmErr = &PlanChangedError{Plan: now}
	w := h.do(post("/leads/send-now/7/confirm", confirmForm(), h.session(t)))
	if w.Code != http.StatusConflict {
		t.Fatalf("status %d, want 409", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		"Something changed since you looked; check the warnings again.", "notice-error",
		"Mailbox 1 has sent 19 of 18 today (warm-up). This goes 2 over.", `name="hash" value="aaaabbbbcccc"`,
		`name="code" value="ramp"`, "I have read the warning and want to send past it.",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("lacks %q", want)
		}
	}
	if strings.Contains(body, `value="window"`) {
		t.Error("the old warnings are still posted")
	}
}

func TestSendNowConfirmRefusal(t *testing.T) {
	for _, c := range []struct {
		err  error
		code int
	}{
		{fmt.Errorf("%w: This batch was already sent at 19:44; 0 of 2 go again.", ErrUserFixable), http.StatusUnprocessableEntity},
		{ErrLeadsNotSetUp, http.StatusUnprocessableEntity},
		{errors.New("database gone"), http.StatusInternalServerError},
	} {
		h := newSendHarness(t)
		h.b.confirmErr = c.err
		w := h.do(post("/leads/send-now/7/confirm", confirmForm(), h.session(t)))
		if w.Code != c.code {
			t.Errorf("%v: status %d, want %d", c.err, w.Code, c.code)
		}
		if c.code == http.StatusUnprocessableEntity && errors.Is(c.err, ErrUserFixable) &&
			!strings.Contains(w.Body.String(), "This batch was already sent at 19:44; 0 of 2 go again.") {
			t.Error("the refusal is not on the page")
		}
		if strings.Contains(h.logs.String(), "database gone") && !strings.Contains(h.logs.String(), `route="POST /leads/send-now/{id}/confirm"`) {
			t.Error("the failure is not logged under its route")
		}
	}
}

func TestSendNowCheckEmail(t *testing.T) {
	h := newSendHarness(t)
	h.b.checkOut = ControlOutcome{Message: "Asked: 1 address is checked in the next minute."}
	w := h.do(post("/leads/send-now/7/check-email", url.Values{"back": {"/leads/5"}, "hash": {"x"}}, h.session(t)))
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/leads/send-now/7?back=%2Fleads%2F5" {
		t.Fatalf("%d to %q", w.Code, w.Header().Get("Location"))
	}
	if h.b.lastCheck != 7 || h.b.n("SendNowConfirm") != 0 {
		t.Errorf("checked %d, confirm called %d", h.b.lastCheck, h.b.n("SendNowConfirm"))
	}
	if msg := flashOf(t, w); msg != "Asked: 1 address is checked in the next minute." {
		t.Errorf("flash %q", msg)
	}
	h.b.checkErr = fmt.Errorf("%w: The email check is off; 0 of 1 addresses were checked.", ErrUserFixable)
	w = h.do(post("/leads/send-now/7/check-email", url.Values{}, h.session(t)))
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/leads/send-now/7" {
		t.Fatalf("refused: %d to %q", w.Code, w.Header().Get("Location"))
	}
	if msg := flashOf(t, w); msg != "The email check is off; 0 of 1 addresses were checked." {
		t.Errorf("flash %q", msg)
	}
}

func TestSendNowButtons(t *testing.T) {
	h := newSendHarness(t)
	c := h.session(t)
	lead := h.do(get("/leads/5", c)).Body.String()
	for _, want := range []string{`action="/leads/send-now/preview"`, `name="lead" value="5"`, `name="back" value="/leads/5"`, "Send now</button>"} {
		if !strings.Contains(lead, want) {
			t.Errorf("lead page lacks %q", want)
		}
	}
	list := h.do(get("/leads", c)).Body.String()
	for _, want := range []string{`id="send-now-form"`, `action="/leads/send-now/preview"`, `form="send-now-form"`, `data-send-count`} {
		if !strings.Contains(list, want) {
			t.Errorf("list lacks %q", want)
		}
	}
	// Forms do not nest: the send form closes before the rows start.
	if i, j := strings.Index(list, `id="send-now-form"`), strings.Index(list, `<ul class="list narrow-only">`); i < 0 || j < 0 ||
		!strings.Contains(list[i:j], "</form>") {
		t.Error("the send form is not closed before the rows")
	}
	batch := h.do(get("/leads/batches/3", c)).Body.String()
	for _, want := range []string{`formaction="/leads/send-now/preview" name="batch" value="3"`, "Approve and send now", `name="back" value="/leads/batches/3"`} {
		if !strings.Contains(batch, want) {
			t.Errorf("batch page lacks %q", want)
		}
	}
}

// TestSendNowAgainPage: Send now again's page is the confirm page posting to its own path, saying
// nothing is approved again, with the warnings and the tick box as before.
func TestSendNowAgainPage(t *testing.T) {
	h := newSendHarness(t)
	w := h.do(get("/leads/send-now/7/again", h.session(t)))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		`action="/leads/send-now/7/again"`, `name="again" value="1"`, "Manual batch 7 · Send now again", "nothing is approved again",
		"6 warnings you can go past", `name="read" value="1" required`, "Send anyway (2)", `href="/leads/batches/7">Cancel`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("lacks %q", want)
		}
	}
	if strings.Contains(body, `/leads/send-now/7/confirm"`) || h.b.n("SendNowAgainPlan") != 1 || h.b.n("SendNowPlan") != 0 {
		t.Errorf("the again page read the confirm plan (again %d, plan %d)", h.b.n("SendNowAgainPlan"), h.b.n("SendNowPlan"))
	}
	// A batch that went: back to its page with why.
	h.b.planErr = fmt.Errorf("%w: Batch 7 already went; nothing more to do here.", ErrUserFixable)
	w = h.do(get("/leads/send-now/7/again", h.session(t)))
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/leads/batches/7" {
		t.Fatalf("a batch that went: %d to %q", w.Code, w.Header().Get("Location"))
	}
	if msg := flashOf(t, w); msg != "Batch 7 already went; nothing more to do here." {
		t.Errorf("flash %q", msg)
	}
}

// TestSendNowAgainPress: the again post passes what was shown with Again set; without the tick it
// shows the again page, never the confirm page; check email from it comes back to it.
func TestSendNowAgainPress(t *testing.T) {
	b := richBackend("x")
	b.confirmOut = ControlOutcome{Message: "Asked. 2 emails go out now."}
	h := harnessAs(t, b, "operator@example.com")
	w := h.do(post("/leads/send-now/7/again", confirmForm(), h.session(t)))
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/leads/batches/7" {
		t.Fatalf("%d to %q", w.Code, w.Header().Get("Location"))
	}
	if got := h.b.lastConfirm; !got.Again || got.BatchID != 7 || got.HashPrefix != "fedcba987654" || !slices.Equal(got.Codes, SendWarningCodes) || got.Operator != "operator@example.com" {
		t.Errorf("confirmed %+v", got)
	}
	f := confirmForm()
	f.Del("read")
	w = h.do(post("/leads/send-now/7/again", f, h.session(t)))
	if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), `action="/leads/send-now/7/again"`) ||
		!strings.Contains(w.Body.String(), "Tick that you have read the 6 warnings") {
		t.Errorf("no tick: %d", w.Code)
	}
	if h.b.n("SendNowConfirm") != 1 {
		t.Errorf("confirm called %d times", h.b.n("SendNowConfirm"))
	}
	// The plain confirm says Again false.
	h.do(post("/leads/send-now/7/confirm", confirmForm(), h.session(t)))
	if h.b.lastConfirm.Again {
		t.Error("the plain confirm asked again")
	}
	w = h.do(post("/leads/send-now/7/check-email", url.Values{"again": {"1"}}, h.session(t)))
	if w.Header().Get("Location") != "/leads/send-now/7/again" {
		t.Errorf("check email from the again page goes to %q", w.Header().Get("Location"))
	}
}

// TestBatchPageOffersSendNowAgain: only a batch whose send never ran shows Send now again.
func TestBatchPageOffersSendNowAgain(t *testing.T) {
	for _, again := range []bool{false, true} {
		b := richBackend("x")
		at := statusNow
		b.batch = LeadBatch{ID: 3, CreatedAt: at, ApprovedAt: &at, SendAgain: again}
		h := newHarness(t, b, false)
		w := h.do(get("/leads/batches/3", h.session(t)))
		if w.Code != http.StatusOK {
			t.Fatalf("status %d", w.Code)
		}
		if got := strings.Contains(w.Body.String(), `href="/leads/send-now/3/again"`); got != again {
			t.Errorf("SendAgain %v: link shown %v", again, got)
		}
	}
}
