package console

import (
	"errors"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// goodProof is an add form the page takes; a case changes one thing.
func goodProof() url.Values {
	return url.Values{"title": {"Harbour Lane Studio"}, "line": {"We moved their site in six weeks."},
		"url": {"https://harbourlane.example/work"}, "sectors": {"education"}, "public": {"1"}}
}

// proofMarkup is the page's own part of body, past the layout's shell.
func proofMarkup(t *testing.T, body string) string {
	t.Helper()
	_, after, ok := strings.Cut(body, `<div class="page">`)
	if !ok {
		t.Fatalf("no page in %q", body)
	}
	return after
}

var proofTags = regexp.MustCompile(`<[^>]*>`)

// TestProofPageLists: each proof shows its title, line and sectors in words, and its link only when
// it is public. A proof with public off says it is spoken, even with a link stored, and no internal
// word is on screen.
func TestProofPageLists(t *testing.T) {
	b := richBackend("A true line")
	b.proofs.Proofs = append(b.proofs.Proofs, Proof{ID: 1, Title: "Kept", Line: "Kept line.", URL: "https://hidden.example/page",
		Sectors: []FilterOption{{"other", "Other"}}, At: statusNow})
	h := newHarness(t, b, false)
	w := h.do(get(proofPath, h.session(t)))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	body := proofMarkup(t, w.Body.String())
	for _, want := range []string{"A true line", "A line with no public page.", "Education", "Government",
		`href="https://harbourlane.example/work"`, "spoken line only", `action="/leads/proof/3/retire"`, "Reason (optional",
		"One true line per piece of work. A line without a public page is used without a link."} {
		if !strings.Contains(body, want) {
			t.Errorf("lacks %q", want)
		}
	}
	list, _, _ := strings.Cut(body, `id="proof-add"`)
	if n := strings.Count(list, "spoken line only"); n != 2 {
		t.Errorf("%d spoken lines, want 2", n)
	}
	if strings.Contains(body, "hidden.example") {
		t.Error("a proof with public off shows its link")
	}
	words := proofTags.ReplaceAllString(body, " ")
	for _, bad := range []string{"reach", "route", "fact", "generation"} {
		if strings.Contains(strings.ToLower(words), bad) {
			t.Errorf("the page says %q", bad)
		}
	}
}

// TestProofPageEmpty: the first visit has no proof and still has the form, with every sector.
func TestProofPageEmpty(t *testing.T) {
	b := richBackend("x")
	b.proofs.Proofs = nil
	h := newHarness(t, b, false)
	body := h.do(get(proofPath, h.session(t))).Body.String()
	for _, want := range []string{"No proof yet.", `name="title"`, `value="education"`, `value="other"`} {
		if !strings.Contains(body, want) {
			t.Errorf("lacks %q", want)
		}
	}
}

// TestProofAdd: a good form is stored trimmed, sectors in the list's order, then a redirect. A link
// typed with public off is stored as typed and the line says spoken line only; with public on, or
// with no link, it does not.
func TestProofAdd(t *testing.T) {
	const spoken = "Spoken line only"
	for _, tc := range []struct {
		name   string
		change func(url.Values)
		public bool
		link   string
		says   bool
	}{
		{"public with a link", func(url.Values) {}, true, "https://harbourlane.example/work", false},
		{"public off with a link", func(v url.Values) { v.Del("public") }, false, "https://harbourlane.example/work", true},
		{"public off, no link", func(v url.Values) { v.Del("public"); v.Del("url") }, false, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := richBackend("x")
			h := newHarness(t, b, false)
			v := goodProof()
			v["title"], v["sectors"] = []string{"  Harbour Lane Studio "}, []string{"other", "education"}
			tc.change(v)
			w := h.do(post(proofPath, v, h.session(t)))
			if w.Code != http.StatusSeeOther || w.Header().Get("Location") != proofPath {
				t.Fatalf("%d to %q", w.Code, w.Header().Get("Location"))
			}
			p := b.lastProof
			if p == nil {
				t.Fatal("nothing stored")
			}
			if p.Title != "Harbour Lane Studio" || p.Public != tc.public || p.URL != tc.link || !slices.Equal(p.Sectors, []string{"education", "other"}) {
				t.Errorf("stored %+v", *p)
			}
			if got := strings.Contains(flashOf(t, w), spoken); got != tc.says {
				t.Errorf("flash %q, says spoken %v", flashOf(t, w), tc.says)
			}
		})
	}
}

// TestProofAddBackendLine: the backend's own line is shown when it words one.
func TestProofAddBackendLine(t *testing.T) {
	b := richBackend("x")
	b.outreachMsg = "Added to your library."
	h := newHarness(t, b, false)
	if got := flashOf(t, h.do(post(proofPath, goodProof(), h.session(t)))); got != b.outreachMsg {
		t.Errorf("flash %q", got)
	}
}

// TestProofAddRefused: everything the table's checks would refuse is refused ahead of the store, on
// the limit it passes and one over it does not, and the form keeps what was typed.
func TestProofAddRefused(t *testing.T) {
	// Characters, not bytes: each is two bytes.
	title, line, link := strings.Repeat("é", MaxProofTitle), strings.Repeat("é", MaxProofLine),
		"https://harbourlane.example/"+strings.Repeat("a", MaxOutreachURL-len("https://harbourlane.example/"))
	for _, tc := range []struct {
		name  string
		field string
		value []string
		words string // "" when the form passes
	}{
		{"title at the limit", "title", []string{title}, ""},
		{"line at the limit", "line", []string{line}, ""},
		{"link at the limit", "url", []string{link}, ""},
		{"every sector", "sectors", []string{"education", "government", "other"}, ""},
		{"no title", "title", []string{"   "}, "needs a title"},
		{"title over", "title", []string{title + "é"}, "over 80"},
		{"title two lines", "title", []string{"a\nb"}, "one line"},
		{"no line", "line", []string{""}, "needs a line"},
		{"line over", "line", []string{line + "é"}, "over 240"},
		{"line two lines", "line", []string{"a\r\nb"}, "one line"},
		{"link over", "url", []string{link + "a"}, "https://"},
		{"plain http", "url", []string{"http://harbourlane.example"}, "https://"},
		{"capital scheme", "url", []string{"HTTPS://harbourlane.example"}, "https://"},
		{"no host", "url", []string{"https://"}, "https://"},
		{"a login", "url", []string{"https://me:pw@harbourlane.example"}, "https://"},
		{"a space", "url", []string{"https://harbourlane.example/a b"}, "https://"},
		{"no sector", "sectors", nil, "at least one sector"},
		{"a sector off the list", "sectors", []string{"education", "spaceflight"}, "not on the list"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := richBackend("x")
			h := newHarness(t, b, false)
			v := goodProof()
			v[tc.field] = tc.value
			w := h.do(post(proofPath, v, h.session(t)))
			if tc.words == "" {
				if w.Code != http.StatusSeeOther || b.n("AddProof") != 1 {
					t.Errorf("%d, AddProof %d", w.Code, b.n("AddProof"))
				}
				return
			}
			if w.Code != http.StatusUnprocessableEntity || b.n("AddProof") != 0 {
				t.Fatalf("%d, AddProof %d", w.Code, b.n("AddProof"))
			}
			body := html.UnescapeString(w.Body.String())
			if !strings.Contains(body, tc.words) || !strings.Contains(body, "Nothing was saved.") {
				t.Errorf("lacks %q", tc.words)
			}
			if !strings.Contains(body, `aria-invalid="true"`) {
				t.Error("no input is marked")
			}
			for _, kept := range []string{"line", "title", "url"} {
				if got := v.Get(kept); got != "" && !strings.Contains(body, got) {
					t.Errorf("%s not kept: %q", kept, got)
				}
			}
		})
	}
}

// TestProofRefusalKeepsTicks: a refused form keeps its sectors and its public tick, both ways.
func TestProofRefusalKeepsTicks(t *testing.T) {
	for _, public := range []bool{true, false} {
		b := richBackend("x")
		h := newHarness(t, b, false)
		v := goodProof()
		v["title"], v["sectors"] = []string{""}, []string{"government"}
		if !public {
			v.Del("public")
		}
		body := h.do(post(proofPath, v, h.session(t))).Body.String()
		if !strings.Contains(body, `value="government" checked`) || strings.Contains(body, `value="education" checked`) {
			t.Error("the sector ticks changed")
		}
		if got := strings.Contains(body, `aria-describedby="proof-public-hint" checked`); got != public {
			t.Errorf("public %v came back %v", public, got)
		}
	}
}

// TestProofAddAnswers: the backend's refusal keeps the form; not set up, and a failure, are their
// own answers; a read that fails saves nothing.
func TestProofAddAnswers(t *testing.T) {
	b := richBackend("x")
	h := newHarness(t, b, false)
	c := h.session(t)
	b.outreachErr = fmt.Errorf("%w: That title is taken.", ErrUserFixable)
	w := h.do(post(proofPath, goodProof(), c))
	if body := w.Body.String(); w.Code != http.StatusUnprocessableEntity || !strings.Contains(body, "That title is taken.") ||
		!strings.Contains(body, `value="Harbour Lane Studio"`) {
		t.Errorf("a refusal: %d", w.Code)
	}
	b.outreachErr = ErrLeadsNotSetUp
	if w := h.do(post(proofPath, goodProof(), c)); w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), leadsNotSetUp) {
		t.Errorf("not set up: %d", w.Code)
	}
	b.outreachErr = errors.New("database gone")
	w = h.do(post(proofPath, goodProof(), c))
	if w.Code != http.StatusInternalServerError || strings.Contains(w.Body.String(), "database gone") {
		t.Errorf("a failure: %d %q", w.Code, w.Body.String())
	}
	b.outreachErr = nil
	before := b.n("AddProof")
	b.leadsErr = ErrLeadsNotSetUp
	if w := h.do(post(proofPath, goodProof(), c)); w.Code != http.StatusUnprocessableEntity || b.n("AddProof") != before {
		t.Errorf("a read not set up: %d, AddProof %d", w.Code, b.n("AddProof"))
	}
}

// TestProofRetire: a retire stores the id and the trimmed reason; a reason is optional and has a
// limit, in characters; a refusal leaves that proof's form open with what was typed.
func TestProofRetire(t *testing.T) {
	b := richBackend("x")
	h := newHarness(t, b, false)
	c := h.session(t)
	const path = "/leads/proof/3/retire"
	w := h.do(post(path, url.Values{"reason": {"  old work "}}, c))
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != proofPath || b.lastProofOut != 3 || b.lastRetired != "old work" {
		t.Errorf("%d to %q, %d %q", w.Code, w.Header().Get("Location"), b.lastProofOut, b.lastRetired)
	}
	if got := flashOf(t, w); !strings.Contains(got, "retired") {
		t.Errorf("flash %q", got)
	}
	if w := h.do(post(path, url.Values{}, c)); w.Code != http.StatusSeeOther || b.lastRetired != "" {
		t.Errorf("no reason: %d, %q", w.Code, b.lastRetired)
	}
	long := strings.Repeat("é", MaxProofReason)
	if w := h.do(post(path, url.Values{"reason": {long}}, c)); w.Code != http.StatusSeeOther || b.lastRetired != long {
		t.Errorf("at the limit: %d", w.Code)
	}
	before := b.n("RetireProof")
	w = h.do(post(path, url.Values{"reason": {long + "é"}}, c))
	body := w.Body.String()
	if w.Code != http.StatusUnprocessableEntity || b.n("RetireProof") != before {
		t.Fatalf("over the limit: %d, RetireProof %d", w.Code, b.n("RetireProof"))
	}
	if !strings.Contains(body, "over 200") || !strings.Contains(body, `value="`+long+`é"`) || strings.Count(body, `<details class="mt-1" open>`) != 1 {
		t.Error("the refusal does not keep the reason in its open form")
	}
}

// TestProofRetireAnswers: a bad id and no such proof are 404; a refusal and a failure their own.
func TestProofRetireAnswers(t *testing.T) {
	b := richBackend("x")
	h := newHarness(t, b, false)
	c := h.session(t)
	for _, id := range []string{"nope", "0", "-1"} {
		if w := h.do(post("/leads/proof/"+id+"/retire", url.Values{}, c)); w.Code != http.StatusNotFound {
			t.Errorf("id %q: %d", id, w.Code)
		}
	}
	if b.n("RetireProof") != 0 {
		t.Errorf("RetireProof %d", b.n("RetireProof"))
	}
	for _, tc := range []struct {
		err  error
		code int
	}{
		{ErrNotFound, http.StatusNotFound},
		{ErrLeadsNotSetUp, http.StatusUnprocessableEntity},
		{fmt.Errorf("%w: Not that one.", ErrUserFixable), http.StatusUnprocessableEntity},
		{errors.New("database gone"), http.StatusInternalServerError},
	} {
		b.outreachErr = tc.err
		if w := h.do(post("/leads/proof/3/retire", url.Values{"reason": {"kept"}}, c)); w.Code != tc.code {
			t.Errorf("%v: %d, want %d", tc.err, w.Code, tc.code)
		}
	}
}

// TestProofNotSetUp: the page answers the not-set-up page, not a list.
func TestProofNotSetUp(t *testing.T) {
	b := richBackend("x")
	b.leadsErr = ErrLeadsNotSetUp
	h := newHarness(t, b, false)
	if body := h.do(get(proofPath, h.session(t))).Body.String(); !strings.Contains(body, leadsNotSetUp) {
		t.Error("no not-set-up page")
	}
}
