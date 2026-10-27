package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

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
