//go:build private

package gates_test

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"testing"

	"github.com/gremy/webbership-leadhound/internal/gates"
)

// detailPage matches the probe's detail pages, not its search, view or company pages.
var detailPage = regexp.MustCompile(`^(d[0-9]+|detail)\.html$`)

// goldenCandidate reads English only and sets no country limit.
var goldenCandidate = gates.CandidateGates{Languages: []string{"en"}}

// TestGoldenGates runs the gates over the raw detail pages of a private probe
// (LEADHOUND_PROBE_DIR) and compares language verdicts with the private labels. Nothing
// from the pages is printed: a mismatch names the page's index in sorted order and the
// gate only.
func TestGoldenGates(t *testing.T) {
	dir := os.Getenv("LEADHOUND_PROBE_DIR")
	if dir == "" {
		t.Skip("LEADHOUND_PROBE_DIR is not set")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read probe dir: %v", err)
	}
	var pages []string
	for _, e := range entries {
		n := e.Name()
		if detailPage.MatchString(n) {
			pages = append(pages, n)
		}
	}
	sort.Strings(pages)
	if len(pages) != 10 {
		t.Fatalf("want 10 detail pages, found %d", len(pages))
	}

	fails, passes := 0, 0
	for i, name := range pages {
		obs := observationFromPage(t, filepath.Join(dir, name))
		rs := gates.Evaluate(obs, goldenCandidate, nil)
		lang := pageByGate(rs)[gates.GateLanguage]
		if len(lang) != 1 {
			t.Errorf("page %d: gate language: %d results", i, len(lang))
			continue
		}
		switch lang[0].Verdict {
		case gates.Fail:
			fails++
		case gates.Pass:
			passes++
			for _, r := range rs {
				t.Logf("page %d: gate %s: %s", i, r.Gate, r.Verdict)
			}
			wp := pageByGate(rs)[gates.GateWorkplace]
			if len(wp) != 1 || wp[0].Verdict != gates.Flag {
				t.Errorf("page %d: gate workplace: want flag", i)
			}
		default:
			t.Errorf("page %d: gate language: verdict %s", i, lang[0].Verdict)
		}
	}
	if fails != 9 || passes != 1 {
		t.Errorf("language verdicts: %d fail, %d pass; want 9 fail, 1 pass", fails, passes)
	}
	t.Logf("language: %d fail, %d pass", fails, passes)
}
