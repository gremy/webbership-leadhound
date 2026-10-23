package fetch

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// ManifestFile is the file in a fixture directory that maps URL patterns to files.
const ManifestFile = "manifest.json"

// FixtureTransport is an http.RoundTripper that serves files from a directory and never
// touches the network. The directory's manifest.json maps URL patterns to file names; in a
// pattern `*` matches any run of characters (slashes included, or none) and every other
// character is literal. A pattern must match the whole request URL. The longest matching
// pattern wins (ties go to the lexically first); no match is a 404 with an empty body.
//
// It counts every request it answers, 404s included, and is safe for concurrent use.
type FixtureTransport struct {
	routes []fixtureRoute // longest pattern first

	mu   sync.Mutex
	hits map[string]int // request URL → requests
	n    int
}

type fixtureRoute struct {
	pattern string
	file    string
	body    []byte
}

// NewFixtureTransport reads dir/manifest.json and every file it names. A file must sit
// inside dir; a missing file, a bad manifest or an empty pattern is an error.
func NewFixtureTransport(dir string) (_ *FixtureTransport, err error) {
	root, err := os.OpenRoot(dir) // reads below cannot leave dir
	if err != nil {
		return nil, fmt.Errorf("fetch: fixtures: %w", err)
	}
	defer func() { _ = root.Close() }()
	raw, err := root.ReadFile(ManifestFile)
	if err != nil {
		return nil, fmt.Errorf("fetch: fixtures: %w", err)
	}
	var manifest map[string]string
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return nil, fmt.Errorf("fetch: fixtures: parse %s: %w", ManifestFile, err)
	}
	ft := &FixtureTransport{hits: map[string]int{}}
	for pattern, file := range manifest {
		if pattern == "" {
			return nil, fmt.Errorf("fetch: fixtures: empty pattern for %q", file)
		}
		if !filepath.IsLocal(file) {
			return nil, fmt.Errorf("fetch: fixtures: %q is not a file inside the fixture directory", file)
		}
		body, err := root.ReadFile(file)
		if err != nil {
			return nil, fmt.Errorf("fetch: fixtures: pattern %q: %w", pattern, err)
		}
		ft.routes = append(ft.routes, fixtureRoute{pattern: pattern, file: file, body: body})
	}
	sort.Slice(ft.routes, func(i, j int) bool {
		a, b := ft.routes[i].pattern, ft.routes[j].pattern
		if len(a) != len(b) {
			return len(a) > len(b)
		}
		return a < b
	})
	return ft, nil
}

// RoundTrip answers req from the fixtures.
func (ft *FixtureTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Body != nil {
		_ = req.Body.Close()
	}
	if err := req.Context().Err(); err != nil {
		return nil, err
	}
	u := req.URL.String()
	ft.mu.Lock()
	ft.n++
	ft.hits[u]++
	ft.mu.Unlock()

	status, body := http.StatusNotFound, []byte(nil)
	for _, r := range ft.routes {
		if globMatch(r.pattern, u) {
			status, body = http.StatusOK, r.body
			break
		}
	}
	h := http.Header{}
	if status == http.StatusOK {
		h.Set("Content-Type", "text/html; charset=utf-8")
	}
	h.Set("Content-Length", strconv.Itoa(len(body)))
	return &http.Response{
		Status:        strconv.Itoa(status) + " " + http.StatusText(status),
		StatusCode:    status,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        h,
		Body:          io.NopCloser(bytes.NewReader(body)),
		ContentLength: int64(len(body)),
		Request:       req,
	}, nil
}

// Hits is how many requests the transport has answered.
func (ft *FixtureTransport) Hits() int {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	return ft.n
}

// HitsFor is how many answered requests had a URL containing substr.
func (ft *FixtureTransport) HitsFor(substr string) int {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	n := 0
	for u, c := range ft.hits {
		if strings.Contains(u, substr) {
			n += c
		}
	}
	return n
}

// globMatch reports whether s matches pattern as a whole, where `*` matches any run of
// characters (none included) and everything else is literal.
func globMatch(pattern, s string) bool {
	parts := strings.Split(pattern, "*")
	if len(parts) == 1 {
		return pattern == s
	}
	first, last := parts[0], parts[len(parts)-1]
	if len(s) < len(first)+len(last) || !strings.HasPrefix(s, first) || !strings.HasSuffix(s, last) {
		return false
	}
	mid := s[len(first) : len(s)-len(last)]
	for _, p := range parts[1 : len(parts)-1] {
		i := strings.Index(mid, p)
		if i < 0 {
			return false
		}
		mid = mid[i+len(p):]
	}
	return true
}
