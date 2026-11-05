package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/gremy/webbership-leadhound/internal/leadrate"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
)

// TestGoldenDraftRefusesRepoPath: a golden draft holds the operator's private labels, so a
// path inside a checkout of this repository is refused before anything is read or written:
// a git-ignored folder in it, a path relative to a folder in it, and a path through a link
// that points into it. The data source points at a closed port, so reaching the database
// would fail differently. A path outside is not refused by the check.
func TestGoldenDraftRefusesRepoPath(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "repo-link")
	if err := os.Symlink(root, link); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LEADHOUND_DATABASE_URL", "postgres://nobody@127.0.0.1:1/none?sslmode=disable&connect_timeout=1")
	t.Setenv("LEADHOUND_DATA_DIR", t.TempDir())
	for _, out := range []string{
		filepath.Join(root, "data", "golden", "leads-relevance-test.jsonl"),
		"leads-relevance-test.jsonl",
		filepath.Join(link, "examples", "golden", "leads-relevance-test.jsonl"),
	} {
		var errOut bytes.Buffer
		captureStdout(t)
		code := run(context.Background(), []string{"leads", "relevance", "golden-draft", "--n", "30", "--out", out}, &errOut)
		if code != 2 || !strings.Contains(errOut.String(), "is inside the leadhound repository") {
			t.Errorf("--out %s: exit %d, stderr %q; want a refusal", out, code, errOut.String())
		}
		abs, _ := filepath.Abs(out)
		if _, err := os.Lstat(abs); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("--out %s: written or present (%v)", out, err)
		}
	}
	if r, inside := insideRepo(filepath.Join(t.TempDir(), "golden", "leads-relevance.jsonl")); inside {
		t.Errorf("a temp path is inside the repository at %s", r)
	}
}

// TestSpreadSampleSpreads: the sample takes one lead from each kind, major and country
// group before a second from any, the largest groups first, and is the same every time.
func TestSpreadSampleSpreads(t *testing.T) {
	var leads []leadstore.RateLead
	add := func(n int, kind string, major int, country string) {
		for range n {
			id := int64(len(leads) + 1)
			leads = append(leads, leadstore.RateLead{LeadID: id, Domain: kind + "-" + country + "-" + strconv.FormatInt(id, 10) + ".example", Kind: kind, Major: major, Country: country})
		}
	}
	add(20, leadstore.LeadUnsupported, 7, "GB")
	add(5, leadstore.LeadD10, 10, "DE")
	add(2, leadstore.LeadAgencyPartner, 0, "GB")
	add(1, leadstore.LeadUnsupported, 8, "NL")

	got := spreadSample(leads, 6)
	if len(got) != 6 {
		t.Fatalf("%d picked, want 6", len(got))
	}
	groups := map[string]int{}
	for _, l := range got[:4] {
		groups[l.Kind+l.Country]++
	}
	if len(groups) != 4 {
		t.Errorf("the first four picks cover %d groups, want all 4: %v", len(groups), groups)
	}
	if got[0].Kind != leadstore.LeadUnsupported || got[0].Major != 7 {
		t.Errorf("first pick %+v, want the largest group's", got[0])
	}
	again := spreadSample(leads, 6)
	for i := range got {
		if got[i].LeadID != again[i].LeadID {
			t.Fatalf("pick %d differs between runs", i)
		}
	}
	if n := len(spreadSample(leads, 100)); n != len(leads) {
		t.Errorf("asking for more than there are gave %d, want all %d", n, len(leads))
	}
}

// TestEvalRecordedOnlyForJevFrom30: only a jev eval on at least 30 labelled leads is recorded.
func TestEvalRecordedOnlyForJevFrom30(t *testing.T) {
	for _, tc := range []struct {
		kind string
		n    int
		want bool
	}{
		{rateDeciderJev, 30, true},
		{rateDeciderJev, 31, true},
		{rateDeciderJev, 29, false},
		{rateDeciderFake, 30, false},
	} {
		if got := evalNotRecorded(tc.kind, tc.n) == ""; got != tc.want {
			t.Errorf("%s on %d: recorded %v, want %v", tc.kind, tc.n, got, tc.want)
		}
	}
}

// evalLines is golden lines in the order of bands, each named by its place; "" is a line
// with no label.
func evalLines(bands ...string) []leadrate.Golden {
	out := make([]leadrate.Golden, len(bands))
	for i, b := range bands {
		out[i] = leadrate.Golden{ID: fmt.Sprintf("site%03d.example", i), Input: leadrate.Input{Kind: leadstore.LeadD10}}
		if b != "" {
			out[i].Label = &b
		}
	}
	return out
}

// repeatBand is band n times.
func repeatBand(band string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = band
	}
	return out
}

// goldenIDs is the id of each line.
func goldenIDs(lines []leadrate.Golden) string {
	ids := make([]string, len(lines))
	for i, g := range lines {
		ids[i] = strings.TrimSuffix(strings.TrimPrefix(g.ID, "site"), ".example")
	}
	return strings.Join(ids, " ")
}

// TestRelevanceCapEvalLines pins the selection: calls other than no first, then the newest
// no calls, the kept lines in their order and unlabelled lines always kept.
func TestRelevanceCapEvalLines(t *testing.T) {
	for _, tc := range []struct {
		name    string
		bands   []string
		limit   int
		want    string
		leftOut int
	}{
		{"under the limit keeps all", []string{"no", "maybe"}, 3, "000 001", 0},
		{"exactly the limit keeps all", []string{"no", "no", "maybe"}, 3, "000 001 002", 0},
		{"other calls first, then the newest no", []string{"no", "strong", "no", "maybe", "no"}, 3, "001 003 004", 2},
		{"more other calls than room keeps the newest of them", []string{"likely", "no", "maybe", "strong"}, 2, "002 003", 2},
		{"only no calls keeps the newest", []string{"no", "no", "no"}, 2, "001 002", 1},
		{"unlabelled lines are kept and not counted", []string{"", "no", "no", "", "no"}, 2, "000 002 003 004", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kept, leftOut := capEvalLines(evalLines(tc.bands...), tc.limit)
			if got := goldenIDs(kept); got != tc.want || leftOut != tc.leftOut {
				t.Errorf("kept %q, left out %d; want %q, %d", got, leftOut, tc.want, tc.leftOut)
			}
		})
	}
}

// TestRelevanceCapCannotStarveTheGate: the cap is above the gate's minimum, and hundreds of
// skips still leave an eval on maxJevEvalLeads leads, which is recorded.
func TestRelevanceCapCannotStarveTheGate(t *testing.T) {
	if maxJevEvalLeads < leadrate.TrustMinN {
		t.Fatalf("the cap %d is under the gate's minimum %d", maxJevEvalLeads, leadrate.TrustMinN)
	}
	kept, _ := capEvalLines(evalLines(repeatBand("no", 500)...), maxJevEvalLeads)
	if n := len(kept); n != maxJevEvalLeads || evalNotRecorded(rateDeciderJev, n) != "" {
		t.Errorf("500 skips kept %d leads, not recorded: %q", n, evalNotRecorded(rateDeciderJev, n))
	}
}

// TestRelevanceCapJevEvalWords: the left-out count and the note on the gate are printed only
// when something was left out.
func TestRelevanceCapJevEvalWords(t *testing.T) {
	out := captureStdout(t)
	if kept := capJevEval(evalLines(repeatBand("no", maxJevEvalLeads)...)); len(kept) != maxJevEvalLeads || out.Len() != 0 {
		t.Errorf("at the cap: kept %d, printed %q", len(kept), out.String())
	}
	kept := capJevEval(evalLines(append(repeatBand("no", 200), "maybe")...))
	if len(kept) != maxJevEvalLeads {
		t.Errorf("over the cap: kept %d", len(kept))
	}
	if kept[len(kept)-1].Label == nil || *kept[len(kept)-1].Label != "maybe" {
		t.Errorf("the call other than no was not kept: %+v", kept[len(kept)-1])
	}
	for _, want := range []string{"capped at 120 labelled leads, 81 left out", "the cap changes what the trust gate measures", "not all 201"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}

// TestRelevanceFakeEvalIsNotCapped: the cap belongs to the paid decider, so a fake eval of
// more lines than the cap asks about all of them and says nothing about a cap.
func TestRelevanceFakeEvalIsNotCapped(t *testing.T) {
	var b strings.Builder
	for _, g := range evalLines(repeatBand("no", maxJevEvalLeads+10)...) {
		line, err := json.Marshal(g)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	path := filepath.Join(t.TempDir(), "golden.jsonl")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	out := captureStdout(t)
	var errOut bytes.Buffer
	if code := run(context.Background(), []string{"leads", "relevance", "eval", "--golden", path, "--runs", "1"}, &errOut); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "relevance eval: 130 labelled lead(s)") || strings.Contains(out.String(), "capped") {
		t.Errorf("fake eval:\n%s", out.String())
	}
}
