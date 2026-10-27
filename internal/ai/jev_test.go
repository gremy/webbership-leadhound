package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/config"
)

var update = flag.Bool("update", false, "rewrite testdata/*.golden.json from the request builder")

// testKey is built at runtime so no key-shaped value sits whole in source.
var testKey = "ts-" + strings.Repeat("k", 8) + "-test-only"

// fiveQuestions is one job's batch: every question goes out in one call.
func fiveQuestions() []Question {
	yesNo := []Option{{Name: "yes", Description: "The posting clearly says so."}, {Name: "no", Description: "It does not."}}
	return []Question{
		{Name: "go_role", Instructions: "Is Go the main language of this role?", Options: yesNo},
		{Name: "ai_work", Instructions: "Does the role build with AI models?", Options: yesNo},
		{Name: "seniority", Instructions: "What seniority does the posting ask for?", Options: []Option{
			{Name: "junior", Description: "Up to two years."},
			{Name: "mid", Description: "Two to five years."},
			{Name: "senior", Description: "Five years or more."},
		}},
		{Name: "drupal", Instructions: "Is Drupal part of the stack?", Options: yesNo},
		{Name: "contract", Instructions: "Is this a contract or a permanent role?", Options: []Option{
			{Name: "contract", Description: "Fixed-term or freelance."},
			{Name: "permanent", Description: "An employee contract with no end date."},
		}},
	}
}

const fiveState = "Example Corp builds payment tools in Go at example.invalid."

// okBody is a 200 answer for the five questions.
func okBody(qs []Question) string {
	answers := map[string]any{}
	for _, q := range qs {
		probs := map[string]float64{}
		for i, o := range q.Options {
			if i == 0 {
				probs[o.Name] = 0.75
			} else {
				probs[o.Name] = 0.25 / float64(len(q.Options)-1)
			}
		}
		answers[q.Name] = map[string]any{"type": "choice", "choice": q.Options[0].Name, "confidence": 0.75, "probabilities": probs}
	}
	b, _ := json.Marshal(map[string]any{
		"model":   "jev-1.13.0",
		"answers": answers,
		"usage":   map[string]int{"input_tokens": 321, "output_tokens": 9},
	})
	return string(b)
}

// recorder is an httptest handler that replies from a script of (status, body) pairs and
// keeps every request it saw.
type recorder struct {
	mu      sync.Mutex
	replies []reply
	bodies  [][]byte
	auth    []string
	paths   []string
	ctypes  []string
}

type reply struct {
	status int
	body   string
}

func (r *recorder) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	b, _ := io.ReadAll(req.Body)
	r.mu.Lock()
	n := len(r.bodies)
	r.bodies = append(r.bodies, b)
	r.auth = append(r.auth, req.Header.Get("Authorization"))
	r.paths = append(r.paths, req.Method+" "+req.URL.Path)
	r.ctypes = append(r.ctypes, req.Header.Get("Content-Type"))
	rep := r.replies[min(n, len(r.replies)-1)]
	r.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(rep.status)
	_, _ = io.WriteString(w, rep.body)
}

func (r *recorder) hits() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.bodies)
}

// newTestJev points a Jev at srv and records its sleeps instead of sleeping.
func newTestJev(t *testing.T, srv *httptest.Server) (*Jev, *[]time.Duration) {
	t.Helper()
	j := NewJev(config.Secret(testKey), "jev-1.13.0")
	j.BaseURL = srv.URL
	var sleeps []time.Duration
	j.Sleep = func(_ context.Context, d time.Duration) error {
		sleeps = append(sleeps, d)
		return nil
	}
	return j, &sleeps
}

func serve(t *testing.T, replies ...reply) (*httptest.Server, *recorder) {
	t.Helper()
	rec := &recorder{replies: replies}
	srv := httptest.NewServer(rec)
	t.Cleanup(srv.Close)
	return srv, rec
}

func compact(t *testing.T, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := json.Compact(&buf, b); err != nil {
		t.Fatalf("compact %q: %v", b, err)
	}
	return buf.Bytes()
}

func TestJevRequestGoldenOneCall(t *testing.T) {
	qs := fiveQuestions()
	srv, rec := serve(t, reply{200, okBody(qs)})
	j, sleeps := newTestJev(t, srv)

	answers, usage, err := j.Decide(context.Background(), fiveState, qs)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if rec.hits() != 1 {
		t.Fatalf("HTTP calls = %d, want 1 for five questions", rec.hits())
	}
	if len(*sleeps) != 0 {
		t.Fatalf("sleeps = %v, want none", *sleeps)
	}
	if got, want := rec.auth[0], "Bearer "+testKey; got != want {
		t.Fatalf("Authorization header is wrong (len %d, want len %d)", len(got), len(want))
	}
	if got := rec.paths[0]; got != "POST /v1/systemone" {
		t.Fatalf("request = %q, want POST /v1/systemone", got)
	}
	if got := rec.ctypes[0]; got != "application/json" {
		t.Fatalf("Content-Type = %q", got)
	}

	golden := filepath.Join("testdata", "request_five.golden.json")
	if *update {
		var buf bytes.Buffer
		if err := json.Indent(&buf, rec.bodies[0], "", "  "); err != nil {
			t.Fatalf("indent: %v", err)
		}
		buf.WriteByte('\n')
		if err := os.WriteFile(golden, buf.Bytes(), 0o600); err != nil {
			t.Fatalf("write golden: %v", err)
		}
	}
	want, err := os.ReadFile(golden) // #nosec G304 -- fixed test fixture name
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if got, w := rec.bodies[0], compact(t, want); !bytes.Equal(got, w) {
		t.Fatalf("request body differs from golden\n got: %s\nwant: %s", got, w)
	}

	if usage.InputTokens != 321 || usage.Model != "jev-1.13.0" {
		t.Fatalf("usage = %+v, want 321 input tokens from the model the response names", usage)
	}
	if len(answers) != 5 {
		t.Fatalf("answers = %d, want 5", len(answers))
	}
	sen := answers["seniority"]
	if sen.Choice != "junior" || sen.Confidence != 0.75 || sen.Probabilities["senior"] != 0.125 {
		t.Fatalf("seniority answer = %+v", sen)
	}
}

func TestJevBackoffOn429ThenSuccess(t *testing.T) {
	qs := fiveQuestions()
	srv, rec := serve(t, reply{429, `{"detail":"rate limited"}`}, reply{529, `{"detail":"overloaded"}`}, reply{200, okBody(qs)})
	j, sleeps := newTestJev(t, srv)

	if _, _, err := j.Decide(context.Background(), fiveState, qs); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if rec.hits() != 3 {
		t.Fatalf("HTTP calls = %d, want 3", rec.hits())
	}
	want := []time.Duration{500 * time.Millisecond, time.Second}
	if fmt.Sprint(*sleeps) != fmt.Sprint(want) {
		t.Fatalf("sleeps = %v, want %v", *sleeps, want)
	}
	for i := 1; i < 3; i++ {
		if !bytes.Equal(rec.bodies[i], rec.bodies[0]) {
			t.Fatalf("retry %d sent a different body", i)
		}
	}
}

func TestJev529ThreeTimesFails(t *testing.T) {
	srv, rec := serve(t, reply{529, `{"detail":"overloaded"}`})
	j, sleeps := newTestJev(t, srv)

	_, _, err := j.Decide(context.Background(), fiveState, fiveQuestions())
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
	if rec.hits() != 3 {
		t.Fatalf("HTTP calls = %d, want 3 tries", rec.hits())
	}
	if len(*sleeps) != 2 {
		t.Fatalf("sleeps = %v, want 2", *sleeps)
	}
}

func TestJevErrors(t *testing.T) {
	qs := fiveQuestions()
	cases := []struct {
		name   string
		status int
		body   string
		want   error  // errors.Is target, or nil
		text   string // substring the error must hold
		tokens int    // input tokens returned with the error
	}{
		{"too long", 400, `{"detail":{"type":"max_tokens_exceeded","msg":"too long"}}`, ErrTooLong, "jev", 0},
		{"other 400", 400, `{"detail":"invalid utf-8"}`, nil, "status 400", 0},
		{"unauthorized", 401, `{"detail":"bad key"}`, nil, "status 401", 0},
		{"validation", 422, `{"detail":[{"loc":["body","model"],"msg":"field required","type":"missing"}]}`, nil, "status 422", 0},
		// A 200 is billed whatever it holds: ErrBadAnswer, with the usage the body reports.
		{"missing answer", 200, `{"model":"jev-1.13.0","answers":{},"usage":{"input_tokens":1,"output_tokens":0}}`, ErrBadAnswer, "no answer", 1},
		{"unknown choice", 200, strings.Replace(okBody(qs), `"choice":"yes"`, `"choice":"maybe"`, 1), ErrBadAnswer, "maybe", 321},
		{"bad json", 200, `{"answers":`, ErrBadAnswer, "decode", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, rec := serve(t, reply{tc.status, tc.body})
			j, _ := newTestJev(t, srv)
			_, u, err := j.Decide(context.Background(), fiveState, qs)
			if err == nil {
				t.Fatal("err = nil")
			}
			if u.InputTokens != tc.tokens {
				t.Fatalf("usage = %+v, want %d input tokens", u, tc.tokens)
			}
			if tc.status != 200 && errors.Is(err, ErrBadAnswer) {
				t.Fatalf("err = %v: a %d is not a billed answer", err, tc.status)
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if tc.want == nil && errors.Is(err, ErrTooLong) {
				t.Fatalf("err = %v, must not be ErrTooLong", err)
			}
			if !strings.Contains(err.Error(), tc.text) {
				t.Fatalf("err = %q, want it to mention %q", err, tc.text)
			}
			if rec.hits() != 1 {
				t.Fatalf("HTTP calls = %d, want 1 (no retry)", rec.hits())
			}
		})
	}
}

func TestJevRejectsBadQuestions(t *testing.T) {
	cases := map[string][]Question{
		"none":         nil,
		"no name":      {{Instructions: "x", Options: []Option{{Name: "a"}}}},
		"no options":   {{Name: "q", Instructions: "x"}},
		"duplicate":    {{Name: "q", Options: []Option{{Name: "a"}}}, {Name: "q", Options: []Option{{Name: "b"}}}},
		"dup option":   {{Name: "q", Options: []Option{{Name: "a"}, {Name: "a"}}}},
		"empty option": {{Name: "q", Options: []Option{{Name: ""}}}},
	}
	for name, qs := range cases {
		t.Run(name, func(t *testing.T) {
			srv, rec := serve(t, reply{200, "{}"})
			j, _ := newTestJev(t, srv)
			if _, _, err := j.Decide(context.Background(), fiveState, qs); err == nil {
				t.Fatal("err = nil, want a question error")
			}
			if rec.hits() != 0 {
				t.Fatalf("HTTP calls = %d, want 0", rec.hits())
			}
		})
	}
}

func TestJevSleepCancelled(t *testing.T) {
	srv, rec := serve(t, reply{429, `{}`})
	j, _ := newTestJev(t, srv)
	j.Sleep = func(ctx context.Context, _ time.Duration) error { return context.Canceled }
	_, _, err := j.Decide(context.Background(), fiveState, fiveQuestions())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if rec.hits() != 1 {
		t.Fatalf("HTTP calls = %d, want 1", rec.hits())
	}
}

// TestJevKeyNeverPrinted formats the client and every kind of error it returns with %v, %+v
// and %#v, including a server that echoes the Authorization header back in its error body.
func TestJevKeyNeverPrinted(t *testing.T) {
	echo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"detail":"rejected header `+r.Header.Get("Authorization")+`"}`) // #nosec G705 -- a test server echoing a header on purpose
	}))
	t.Cleanup(echo.Close)
	echoTooLong := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"detail":"max_tokens_exceeded for `+r.Header.Get("Authorization")+`"}`) // #nosec G705 -- a test server echoing a header on purpose
	}))
	t.Cleanup(echoTooLong.Close)
	busy, _ := serve(t, reply{529, `{}`})
	unauth, _ := serve(t, reply{401, `{"detail":"bad key"}`})

	var values []any
	for _, base := range []string{echo.URL, echoTooLong.URL, busy.URL, unauth.URL, "http://192.0.2.1:1", "::not a url"} {
		j := NewJev(config.Secret(testKey), "jev-1.13.0")
		j.BaseURL = base
		j.Sleep = func(context.Context, time.Duration) error { return nil }
		j.HTTPClient = &http.Client{Timeout: 200 * time.Millisecond}
		_, _, err := j.Decide(context.Background(), fiveState, fiveQuestions())
		if err == nil {
			t.Fatalf("base %s: err = nil", base)
		}
		values = append(values, err, j, *j)
	}
	for _, v := range values {
		for _, verb := range []string{"%v", "%+v", "%#v", "%s"} {
			if s := fmt.Sprintf(verb, v); strings.Contains(s, testKey) {
				t.Fatalf("%s of %T printed the key", verb, v)
			}
		}
	}
}
