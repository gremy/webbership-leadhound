package console

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"
)

// stepsFake is fakeBackend with the morning counts, so the step bar can be read without
// changing the shared fake.
type stepsFake struct {
	*fakeBackend
	counts MorningCounts
	err    error
}

func (f *stepsFake) MorningCounts(context.Context) (MorningCounts, error) {
	f.count("MorningCounts")
	return f.counts, f.err
}

// newStepsHarness is newHarness over a backend that has the morning counts.
func newStepsHarness(t *testing.T, f *stepsFake) *harness {
	t.Helper()
	a, clock, calls := testAuth(t)
	logs := &syncBuffer{}
	h := New(f, a, Options{Location: time.UTC, Logger: slog.New(slog.NewTextHandler(logs, nil)), Now: func() time.Time { return statusNow }})
	return &harness{h: h, b: f.fakeBackend, a: a, clock: clock, calls: calls, logs: logs, name: "__Host-lh_session"}
}

// labels is the bar's step labels in order.
func labels(steps []step) []string {
	out := make([]string, len(steps))
	for i, st := range steps {
		out[i] = st.Label
	}
	return out
}

// marked is the labels of the steps for which pick is true.
func marked(steps []step, pick func(step) bool) []string {
	var out []string
	for _, st := range steps {
		if pick(st) {
			out = append(out, st.Label)
		}
	}
	return out
}

func TestMorningStepsOrderAndLinks(t *testing.T) {
	steps := morningSteps(nil, "")
	if got, want := strings.Join(labels(steps), ","), "Review,Approve,Calls,Replies,By hand"; got != want {
		t.Fatalf("steps %s, want %s", got, want)
	}
	links := []string{"/leads", "/leads/outreach", "/leads/today#today-calls-now", "/leads/today#today-replies", "/leads/today#today-manual"}
	for i, st := range steps {
		if st.Path != links[i] {
			t.Errorf("%s links to %s, want %s", st.Label, st.Path, links[i])
		}
		if st.Num != i+1 {
			t.Errorf("%s is number %d, want %d", st.Label, st.Num, i+1)
		}
		if st.Next || st.Clear || st.Count != 0 {
			t.Errorf("the static form marks %s: %+v", st.Label, st)
		}
	}
}

func TestMorningStepsCurrent(t *testing.T) {
	for _, tt := range []struct{ at, want string }{
		{"/leads", "Review"},
		{"/leads/outreach", "Approve"},
		{"/leads/batches/3", "Approve"},
		{"/leads/calls", "Calls"},
		{"/leads/calls/9", "Calls"},
		{"/leads/replies", "Replies"},
		{"/leads/today", ""},
		{"/leads/home", ""},
		{"/leads/drafts", ""},
		{"/leads/batches/new", ""},
		{"/leads/batches/0", ""},
		{"/leads/batches/03", ""},
		{"/leads/calls/9/outcome", ""},
		{"https://other.example/", ""},
		{"//other.example/leads", ""},
		{"", ""},
	} {
		got := strings.Join(marked(morningSteps(&MorningCounts{}, tt.at), func(s step) bool { return s.Current }), ",")
		if got != tt.want {
			t.Errorf("at %q marks %q, want %q", tt.at, got, tt.want)
		}
	}
}

func TestMorningStepsNext(t *testing.T) {
	for _, tt := range []struct {
		name      string
		c         MorningCounts
		next      string
		clear     string
		approveTo string
	}{
		{"all zeros", MorningCounts{}, "Review", "Approve,Calls,Replies,By hand", "/leads/outreach"},
		{"thousands to review only", MorningCounts{ToReview: 4200}, "Review", "Approve,Calls,Replies,By hand", "/leads/outreach"},
		{"a batch waits", MorningCounts{ToReview: 9, BatchID: 12, BatchItems: 30, Calls: 2}, "Approve", "Replies,By hand", "/leads/batches/12"},
		{"a call-only batch is not clear", MorningCounts{BatchID: 7}, "Approve", "Calls,Replies,By hand", "/leads/batches/7"},
		{"calls before replies", MorningCounts{Calls: 1, Replies: 3, ByHand: 2}, "Calls", "Approve", "/leads/outreach"},
		{"replies before by hand", MorningCounts{Replies: 1, ByHand: 2}, "Replies", "Approve,Calls", "/leads/outreach"},
		{"by hand only", MorningCounts{ByHand: 5}, "By hand", "Approve,Calls,Replies", "/leads/outreach"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := tt.c
			steps := morningSteps(&c, "")
			if got := strings.Join(marked(steps, func(s step) bool { return s.Next }), ","); got != tt.next {
				t.Errorf("next %q, want %q", got, tt.next)
			}
			if got := strings.Join(marked(steps, func(s step) bool { return s.Clear }), ","); got != tt.clear {
				t.Errorf("clear %q, want %q", got, tt.clear)
			}
			if steps[1].Path != tt.approveTo {
				t.Errorf("Approve links to %s, want %s", steps[1].Path, tt.approveTo)
			}
			counts := []int{c.ToReview, c.BatchItems, c.Calls, c.Replies, c.ByHand}
			for i, st := range steps {
				if st.Count != counts[i] {
					t.Errorf("%s counts %d, want %d", st.Label, st.Count, counts[i])
				}
			}
		})
	}
}

func TestStepsBarNothingWaiting(t *testing.T) {
	if !stepsBar(&MorningCounts{ToReview: 50}, "").Nothing {
		t.Error("no work past Review does not say Nothing waiting")
	}
	if stepsBar(&MorningCounts{Replies: 1}, "").Nothing {
		t.Error("a reply waiting says Nothing waiting")
	}
	if stepsBar(nil, "/leads").Nothing {
		t.Error("the static form says Nothing waiting")
	}
}

// barPages are the pages that show the bar, by sample path, and the step each marks.
var barPages = map[string]string{
	"/leads/home":      "",
	"/leads":           "Review",
	"/leads/outreach":  "Approve",
	"/leads/batches/3": "Approve",
	"/leads/today":     "",
	"/leads/calls":     "Calls",
	"/leads/calls/9":   "Calls",
	"/leads/replies":   "Replies",
}

var (
	barBlock = regexp.MustCompile(`(?s)<nav class="steps".*?</nav>`)
	barHref  = regexp.MustCompile(`href="([^"]*)"`)
)

var currentStep = regexp.MustCompile(`<a href="[^"]*" aria-current="step">.*?<span class="step-label">([^<]+)</span>`)

func TestStepsBarOnMorningPagesOnly(t *testing.T) {
	h := newHarness(t, richBackend("x"), false)
	c := h.session(t)
	for _, rt := range routes {
		if rt.public || !strings.HasPrefix(rt.pattern, "GET ") || rt.sample == "/leads/steps" {
			continue
		}
		w := h.do(get(rt.sample, c))
		body := w.Body.String()
		want, isBar := barPages[rt.sample]
		has := strings.Contains(body, `<nav class="steps" aria-label="Morning steps"`)
		if has != isBar {
			t.Errorf("%s: bar shown=%v, want %v", rt.sample, has, isBar)
			continue
		}
		if !isBar {
			continue
		}
		fetch := `hx-get="/leads/steps?at=` + strings.ReplaceAll(rt.sample, "/", "%2F") + `" hx-trigger="load" hx-swap="outerHTML"`
		if !strings.Contains(body, fetch) {
			t.Errorf("%s: the bar does not fetch its counts with %s", rt.sample, fetch)
		}
		got := ""
		if m := currentStep.FindStringSubmatch(body); m != nil {
			got = m[1]
		}
		if got != want {
			t.Errorf("%s: current step %q, want %q", rt.sample, got, want)
		}
		if strings.Contains(body, "step-next") || strings.Contains(body, "step-clear") || strings.Contains(body, "Nothing waiting") {
			t.Errorf("%s: the static bar carries marks", rt.sample)
		}
		bar := barBlock.FindString(body)
		for _, m := range barHref.FindAllStringSubmatch(bar, -1) {
			if strings.Contains(m[1], "?") {
				t.Errorf("%s: the bar adds a query to %s", rt.sample, m[1])
			}
		}
	}
	for p := range barPages {
		if _, ok := shellProduct[p]; !ok {
			t.Errorf("barPages names %s, which no GET route samples", p)
		}
	}
}

// The Today sections the steps link to exist.
func TestStepsTodayAnchors(t *testing.T) {
	h := newHarness(t, richBackend("x"), false)
	body := h.do(get("/leads/today", h.session(t))).Body.String()
	for _, id := range []string{"today-calls-now", "today-replies", "today-manual"} {
		if !strings.Contains(body, `id="`+id+`"`) {
			t.Errorf("/leads/today has no section %s", id)
		}
	}
}

func TestStepsFragment(t *testing.T) {
	f := &stepsFake{fakeBackend: richBackend("x"), counts: MorningCounts{ToReview: 1234, BatchID: 12, BatchItems: 30, Calls: 2, Replies: 1}}
	h := newStepsHarness(t, f)
	r := get("/leads/steps?at=%2Fleads%2Fbatches%2F12", h.session(t))
	r.Header.Set("HX-Request", "true")
	w := h.do(r)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		`<nav class="steps" aria-label="Morning steps">`,
		`<span class="step-count">1,234</span>`,
		`<span class="step-count">30</span>`,
		`href="/leads/batches/12" aria-current="step"`,
		`class="step step-next"`,
		`class="step step-clear"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("fragment lacks %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "hx-get") {
		t.Error("the counted bar fetches itself again")
	}
	if strings.Contains(body, "<html") || strings.Contains(body, `class="subnav"`) {
		t.Error("the fragment carries the page around the bar")
	}
	if n := h.b.total(); n != 1 || h.b.n("MorningCounts") != 1 {
		t.Errorf("the fragment made %d backend calls, want only MorningCounts", n)
	}
}

func TestStepsFragmentForeignAt(t *testing.T) {
	h := newStepsHarness(t, &stepsFake{fakeBackend: richBackend("x"), counts: MorningCounts{Replies: 1}})
	body := h.do(get("/leads/steps?at=https%3A%2F%2Fother.example%2F", h.session(t))).Body.String()
	if strings.Contains(body, `aria-current`) {
		t.Error("a foreign at marks a step")
	}
	if strings.Contains(body, "other.example") {
		t.Error("the fragment reflects the at value")
	}
}

func TestStepsFragmentNothingWaiting(t *testing.T) {
	h := newStepsHarness(t, &stepsFake{fakeBackend: richBackend("x")})
	body := h.do(get("/leads/steps?at=%2Fleads", h.session(t))).Body.String()
	for _, want := range []string{"Nothing waiting", `class="step step-next"><a href="/leads" aria-current="step">`} {
		if !strings.Contains(body, want) {
			t.Errorf("all zeros lacks %q:\n%s", want, body)
		}
	}
}

func TestStepsFragmentNoSession(t *testing.T) {
	h := newStepsHarness(t, &stepsFake{fakeBackend: richBackend("x")})
	r := get("/leads/steps?at=%2Fleads")
	r.Header.Set("HX-Request", "true")
	w := h.do(r)
	if w.Code != http.StatusUnauthorized || w.Header().Get("HX-Redirect") != "/login" {
		t.Errorf("status %d, HX-Redirect %q", w.Code, w.Header().Get("HX-Redirect"))
	}
	if n := h.b.total(); n != 0 {
		t.Errorf("backend called %d times with no session", n)
	}
}

func TestStepsFragmentAnswers204(t *testing.T) {
	t.Run("backend without counts", func(t *testing.T) {
		h := newHarness(t, richBackend("x"), false)
		w := h.do(get("/leads/steps?at=%2Fleads", h.session(t)))
		if w.Code != http.StatusNoContent || w.Body.Len() != 0 {
			t.Errorf("status %d, body %q", w.Code, w.Body)
		}
	})
	for _, tt := range []struct {
		name string
		err  error
	}{
		{"leads off", fmt.Errorf("leads today: %w", ErrLeadsNotSetUp)},
		{"read error", errors.New("database gone")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := newStepsHarness(t, &stepsFake{fakeBackend: richBackend("x"), err: tt.err})
			w := h.do(get("/leads/steps?at=%2Fleads", h.session(t)))
			if w.Code != http.StatusNoContent || w.Body.Len() != 0 {
				t.Errorf("status %d, body %q", w.Code, w.Body)
			}
			if n := strings.Count(strings.TrimSpace(h.logs.String()), "\n") + 1; h.logs.String() == "" || n != 1 {
				t.Errorf("logged %d lines, want 1: %s", n, h.logs)
			}
		})
	}
}

// TestCSSHasStepsClasses: every class the bar uses has a rule, and the row scrolls inside itself.
func TestCSSHasStepsClasses(t *testing.T) {
	b, err := staticFS.ReadFile("static/console.css")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"steps", "steps-inner", "steps-list", "step", "step-num", "step-label", "step-count", "step-next", "step-clear", "step-mark", "step-tick", "steps-note"} {
		if !regexp.MustCompile(`\.` + name + `\b`).Match(b) {
			t.Errorf("console.css has no rule for .%s", name)
		}
	}
	rule := regexp.MustCompile(`(?s)\.steps-inner \{[^}]*\}`).Find(b)
	if !strings.Contains(string(rule), "overflow-x: auto") {
		t.Errorf(".steps-inner does not scroll inside itself: %s", rule)
	}
}
