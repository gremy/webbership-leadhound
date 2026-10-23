package fetch

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func writeFixture(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func fixtureDir(t *testing.T, manifest string, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	writeFixture(t, dir, "manifest.json", manifest)
	for name, body := range files {
		writeFixture(t, dir, name, body)
	}
	return dir
}

func roundTrip(t *testing.T, rt http.RoundTripper, rawURL string) (int, string, http.Header) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, rawURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip %s: %v", rawURL, err)
	}
	defer func() { _ = res.Body.Close() }()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return res.StatusCode, string(b), res.Header
}

func TestGlobMatch(t *testing.T) {
	cases := []struct {
		pattern, s string
		want       bool
	}{
		{"abc", "abc", true},
		{"abc", "abcd", false},
		{"abc", "xabc", false},
		{"*", "", true},
		{"*", "any/thing?at=all", true},
		{"a*", "a", true},
		{"a*c", "ac", true},
		{"a*c", "a/b/c", true},
		{"a*c", "a/b/cd", false},
		{"*b*", "abc", true},
		{"*b*", "ac", false},
		{"*x*x*", "x", false},
		{"*x*x*", "xx", true},
		{"a*b*a", "aba", true},
		{"a*b*a", "ab", false},
		{"?q=1", "?q=1", true}, // ? is literal
		{"a.c", "abc", false},  // . is literal
		{"[ab]", "a", false},   // brackets are literal
		{"*&start=0*", "x&start=0", true},
		{"*&start=0*", "x&start=10", false},
	}
	for _, tc := range cases {
		if got := globMatch(tc.pattern, tc.s); got != tc.want {
			t.Errorf("globMatch(%q, %q) = %v, want %v", tc.pattern, tc.s, got, tc.want)
		}
	}
}

func TestFixtureTransportServes(t *testing.T) {
	dir := fixtureDir(t,
		`{"*/jobs/*": "generic.html", "*/jobs/42": "specific.html", "https://a.example/exact": "exact.html"}`,
		map[string]string{"generic.html": "generic", "specific.html": "specific", "exact.html": "exact"})
	ft, err := NewFixtureTransport(dir)
	if err != nil {
		t.Fatalf("NewFixtureTransport: %v", err)
	}

	cases := []struct {
		url    string
		status int
		body   string
	}{
		{"https://a.example/jobs/42", 200, "specific"}, // longest pattern wins
		{"https://a.example/jobs/7", 200, "generic"},
		{"https://a.example/jobs/42/more", 200, "generic"},
		{"https://a.example/exact", 200, "exact"},
		{"https://a.example/exact?x=1", 404, ""},
		{"https://a.example/other", 404, ""},
	}
	for _, tc := range cases {
		status, body, h := roundTrip(t, ft, tc.url)
		if status != tc.status || body != tc.body {
			t.Errorf("%s = %d %q, want %d %q", tc.url, status, body, tc.status, tc.body)
		}
		if status == 200 && h.Get("Content-Type") != "text/html; charset=utf-8" {
			t.Errorf("%s Content-Type = %q", tc.url, h.Get("Content-Type"))
		}
	}
	if ft.Hits() != len(cases) {
		t.Fatalf("Hits = %d, want %d", ft.Hits(), len(cases))
	}
	if got := ft.HitsFor("/jobs/"); got != 3 {
		t.Fatalf("HitsFor(/jobs/) = %d, want 3", got)
	}
	if got := ft.HitsFor("exact"); got != 2 {
		t.Fatalf("HitsFor(exact) = %d, want 2", got)
	}
	if got := ft.HitsFor("nowhere"); got != 0 {
		t.Fatalf("HitsFor(nowhere) = %d, want 0", got)
	}
}

func TestFixtureTransportEqualLengthTieIsStable(t *testing.T) {
	dir := fixtureDir(t, `{"*a*": "one.html", "*b*": "two.html"}`, map[string]string{"one.html": "one", "two.html": "two"})
	ft, err := NewFixtureTransport(dir)
	if err != nil {
		t.Fatal(err)
	}
	for range 5 {
		if _, body, _ := roundTrip(t, ft, "https://ab.example/"); body != "one" {
			t.Fatalf("tie = %q, want the lexically first pattern's file", body)
		}
	}
}

func TestFixtureTransportBadManifests(t *testing.T) {
	cases := []struct {
		name     string
		manifest string
		files    map[string]string
	}{
		{"not json", `{`, nil},
		{"missing file", `{"*": "gone.html"}`, nil},
		{"escaping path", `{"*": "../outside.html"}`, nil},
		{"absolute path", `{"*": "/etc/hosts"}`, nil},
		{"empty pattern", `{"": "a.html"}`, map[string]string{"a.html": "a"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := fixtureDir(t, tc.manifest, tc.files)
			if _, err := NewFixtureTransport(dir); err == nil {
				t.Fatal("NewFixtureTransport succeeded; want an error")
			}
		})
	}
	if _, err := NewFixtureTransport(t.TempDir()); err == nil {
		t.Fatal("NewFixtureTransport without a manifest succeeded")
	}
}

func TestFixtureTransportCancelledContext(t *testing.T) {
	dir := fixtureDir(t, `{"*": "a.html"}`, map[string]string{"a.html": "a"})
	ft, err := NewFixtureTransport(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://a.example/", nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := ft.RoundTrip(req)
	if res != nil {
		_ = res.Body.Close()
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("RoundTrip on a cancelled context = %v, want context.Canceled", err)
	}
}

func TestFixtureTransportConcurrent(t *testing.T) {
	dir := fixtureDir(t, `{"*/a": "a.html"}`, map[string]string{"a.html": "a"})
	ft, err := NewFixtureTransport(dir)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			for range 10 {
				req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://x.example/a", nil)
				res, err := ft.RoundTrip(req)
				if err != nil {
					t.Error(err)
					return
				}
				_, _ = io.Copy(io.Discard, res.Body)
				_ = res.Body.Close()
			}
		})
	}
	wg.Wait()
	if ft.Hits() != 200 || ft.HitsFor("/a") != 200 {
		t.Fatalf("Hits = %d, HitsFor = %d; want 200, 200", ft.Hits(), ft.HitsFor("/a"))
	}
}

// TestFixtureTransportThroughClient proves a Client over the fixture transport fetches
// offline: it needs no live gate, and every response reaches the recorder.
func TestFixtureTransportThroughClient(t *testing.T) {
	dir := fixtureDir(t, `{"*/a": "a.html"}`, map[string]string{"a.html": "<p>a</p>"})
	ft, err := NewFixtureTransport(dir)
	if err != nil {
		t.Fatal(err)
	}
	rec := &memRecorder{}
	c := New(append(testOptions(newFakeClock(), rec), WithFixtures(ft))...)
	resp, err := c.Get(context.Background(), KindSearch, "https://jobs.example/a")
	if err != nil || resp.Status != 200 || string(resp.Body) != "<p>a</p>" {
		t.Fatalf("Get = %d %q, %v", resp.Status, resp.Body, err)
	}
	resp, err = c.Get(context.Background(), KindDetail, "https://jobs.example/b")
	if err != nil || resp.Status != 404 {
		t.Fatalf("Get missing = %d, %v; want 404, nil", resp.Status, err)
	}
	if n := len(rec.all()); n != 2 {
		t.Fatalf("recorded %d, want 2", n)
	}
}

// TestFixtureTransportRealManifest maps URLs of the shapes the collector builds against the
// scrubbed fixtures in internal/linkedin/testdata.
func TestFixtureTransportRealManifest(t *testing.T) {
	ft, err := NewFixtureTransport(filepath.Join("..", "linkedin", "testdata"))
	if err != nil {
		t.Fatalf("NewFixtureTransport: %v", err)
	}
	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join("..", "linkedin", "testdata", name)) // #nosec G304 -- the repo's own fixtures
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	const api = "https://www.linkedin.com/jobs-guest/jobs/api/"
	cases := []struct {
		url    string
		status int
		file   string
	}{
		{api + "seeMoreJobPostings/search?keywords=golang&location=Remote&f_TPR=r604800&start=0", 200, "search.html"},
		{api + "seeMoreJobPostings/search?keywords=golang&location=Remote&f_TPR=r86400&start=10", 200, "search_empty.html"},
		{api + "jobPosting/1000000004", 200, "detail_offsite.html"},
		{api + "jobPosting/1000000003", 404, ""},
	}
	for _, tc := range cases {
		status, body, _ := roundTrip(t, ft, tc.url)
		if status != tc.status {
			t.Errorf("%s status = %d, want %d", tc.url, status, tc.status)
			continue
		}
		if tc.file != "" && body != read(tc.file) {
			t.Errorf("%s did not serve %s", tc.url, tc.file)
		}
		if tc.file == "" && body != "" {
			t.Errorf("%s 404 body = %q, want empty", tc.url, body)
		}
	}
	if ft.HitsFor("jobPosting/") != 2 || ft.HitsFor("seeMoreJobPostings") != 2 {
		t.Fatalf("HitsFor detail = %d, search = %d; want 2, 2", ft.HitsFor("jobPosting/"), ft.HitsFor("seeMoreJobPostings"))
	}
}
