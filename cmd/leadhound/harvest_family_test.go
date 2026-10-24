package main

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestHarvestPatterns(t *testing.T) {
	for _, p := range moreHarvestPatterns {
		if !validPattern(p) {
			t.Errorf("documented pattern %q is refused", p)
		}
		if !strings.Contains(harvestUsage, "'"+p+"'") {
			t.Errorf("usage does not name %q", p)
		}
	}
	for p, want := range map[string]bool{
		"jobs.lever.co/*":    true,
		"*.teamtailor.com":   true,
		"*.jobs.personio.de": true,
		"*.teamtailor.com/*": false, // a domain pattern has no path
		"*teamtailor.com":    false,
		"*.com":              false,
		"**.teamtailor.com":  false,
		"*.teamtailor.com&x": false,
		"a.*.teamtailor.com": false,
	} {
		if got := validPattern(p); got != want {
			t.Errorf("validPattern(%q) = %v, want %v", p, got, want)
		}
	}
	for _, tc := range []struct{ pattern, want string }{
		{"jobs.smartrecruiters.com/*", "/CC-MAIN-2099-01-index?url=jobs.smartrecruiters.com%2F%2A&output=json&fl=url&page=0"},
		{"*.teamtailor.com", "/CC-MAIN-2099-01-index?url=teamtailor.com&matchType=domain&output=json&fl=url&page=0"},
		{"*.jobs.personio.com", "/CC-MAIN-2099-01-index?url=jobs.personio.com&matchType=domain&output=json&fl=url&page=0"},
	} {
		if got := indexURL("CC-MAIN-2099-01", tc.pattern, "page=0"); got != ccIndexBase+tc.want {
			t.Errorf("indexURL(%q) = %q, want %q", tc.pattern, got, ccIndexBase+tc.want)
		}
	}
	noDB(t)
	out := filepath.Join(t.TempDir(), "x.csv")
	if _, err := runHarvest(t, "--crawl", "CC-MAIN-2099-01", "--out", out, "--pattern", "*.teamtailor.com/*"); !errors.Is(err, errUsage) {
		t.Errorf("domain pattern with a path: %v, want a usage error", err)
	}
}

// TestHarvestHostFamilies: against a local index, the documented patterns for the three new
// ATSs ask a domain query for the host families and give one board per company.
func TestHarvestHostFamilies(t *testing.T) {
	noDB(t)
	t.Setenv("LEADHOUND_LIVE", "")
	var mu sync.Mutex
	seen := map[string]string{} // url → matchType
	lines := map[string]string{
		"jobs.smartrecruiters.com/*": `{"url": "https://jobs.smartrecruiters.com/ExampleRobotics/700000000000001-x"}`,
		"teamtailor.com": `{"url": "https://mockworks.teamtailor.com/jobs/1-go"}` + "\n" +
			`{"url": "https://www.teamtailor.com/en/"}` + "\n" + `{"url": "https://a.b.teamtailor.com/"}`,
		"jobs.personio.de":  `{"url": "https://musterfabrik.jobs.personio.de/job/100"}`,
		"jobs.personio.com": `{"url": "https://musterfabrik.jobs.personio.com/"}`,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		mu.Lock()
		seen[q.Get("url")] = q.Get("matchType")
		mu.Unlock()
		body, ok := lines[q.Get("url")]
		switch {
		case !ok:
			http.Error(w, "bad query", http.StatusBadRequest)
		case q.Get("showNumPages") == "true":
			_, _ = fmt.Fprint(w, `{"pages": 1}`)
		default:
			_, _ = fmt.Fprintln(w, body)
		}
	}))
	t.Cleanup(srv.Close)
	savedBase, savedInterval, savedJitter := ccIndexBase, harvestInterval, harvestJitter
	ccIndexBase, harvestInterval, harvestJitter = srv.URL, time.Millisecond, 0
	t.Cleanup(func() { ccIndexBase, harvestInterval, harvestJitter = savedBase, savedInterval, savedJitter })

	out := filepath.Join(t.TempDir(), "x.csv")
	args := []string{"--crawl", "CC-MAIN-2099-01", "--out", out}
	for _, p := range moreHarvestPatterns {
		args = append(args, "--pattern", p)
	}
	stdout, err := runHarvest(t, args...)
	if err != nil {
		t.Fatalf("%v\n%s", err, stdout)
	}
	for u, mt := range map[string]string{"jobs.smartrecruiters.com/*": "", "teamtailor.com": "domain", "jobs.personio.de": "domain", "jobs.personio.com": "domain"} {
		if got, ok := seen[u]; !ok || got != mt {
			t.Errorf("url %q: asked %v with matchType %q, want %q", u, ok, got, mt)
		}
	}
	got, err := os.ReadFile(out) // #nosec G304 -- the test's temp file
	if err != nil {
		t.Fatal(err)
	}
	want := "name,slug,url\n" +
		",musterfabrik,https://musterfabrik.jobs.personio.de/\n" +
		",musterfabrik,https://musterfabrik.jobs.personio.com/\n" +
		",ExampleRobotics,https://jobs.smartrecruiters.com/ExampleRobotics\n" +
		",mockworks,https://mockworks.teamtailor.com/jobs\n"
	if string(got) != want {
		t.Errorf("csv:\n%s\nwant:\n%s\nstdout:\n%s", got, want, stdout)
	}
	if !strings.Contains(stdout, "*.teamtailor.com: pages 1/1, page errors 0, lines 3, boards 1, refused 1, skipped 1") {
		t.Errorf("stdout:\n%s", stdout)
	}
}
