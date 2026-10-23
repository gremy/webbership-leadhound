package gates_test

// The tests over real detail pages live in this external package: the LinkedIn collector
// imports gates (through pipeline.Observe), so an in-package test that parses pages with
// the linkedin package would be an import cycle.

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gremy/webbership-leadhound/internal/gates"
	"github.com/gremy/webbership-leadhound/internal/linkedin"
	"github.com/gremy/webbership-leadhound/internal/postingguard"
)

// pageCandidate matches the in-package tests' fictional candidate: English and
// Portuguese, employable from Portugal, open to office work.
var pageCandidate = gates.CandidateGates{Languages: []string{"en", "pt"}, AllowedCountries: []string{"Portugal"}}

// observationFromPage parses a detail page and cleans its description as pipeline.Observe
// does, so the gates see what they see in production.
func observationFromPage(t testing.TB, path string) gates.Observation {
	t.Helper()
	body, err := os.ReadFile(path) //nolint:gosec // G304, G703: tests read fixture and probe pages by path
	if err != nil {
		t.Fatalf("read page: %v", err)
	}
	d, err := linkedin.ParseDetail(body, time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("parse page: %v", err)
	}
	return gates.Observation{Title: d.Title, Company: d.Company, Location: d.Location, Description: postingguard.Clean(d.Description, true).Visible}
}

func pageByGate(rs []gates.Result) map[string][]gates.Result {
	m := map[string][]gates.Result{}
	for _, r := range rs {
		m[r.Gate] = append(m[r.Gate], r)
	}
	return m
}

func pageOne(t *testing.T, rs []gates.Result, gate string) gates.Result {
	t.Helper()
	got := pageByGate(rs)[gate]
	if len(got) != 1 {
		t.Fatalf("gate %s: want 1 result, got %d (%+v)", gate, len(got), got)
	}
	return got[0]
}

func TestLanguageFixtures(t *testing.T) {
	dir := filepath.Join("..", "linkedin", "testdata")
	cases := []struct {
		file    string
		verdict string
		lang    string
	}{
		{"detail_fr.html", gates.Fail, "fr"},
		{"detail_es.html", gates.Fail, "es"},
		{"detail_nl.html", gates.Fail, "nl"},
		{"detail_offsite.html", gates.Pass, "en"},
	}
	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			obs := observationFromPage(t, filepath.Join(dir, tc.file))
			iso, conf := gates.DetectLanguage(obs.Description)
			if iso != tc.lang {
				t.Fatalf("DetectLanguage = %q (%.2f), want %q", iso, conf, tc.lang)
			}
			rs := gates.Evaluate(obs, pageCandidate, nil)
			lang := pageOne(t, rs, gates.GateLanguage)
			if lang.Verdict != tc.verdict {
				t.Fatalf("language verdict = %s (%s), want %s", lang.Verdict, lang.Reason, tc.verdict)
			}
			if tc.verdict == gates.Fail {
				// Short-circuit: no English-pattern gates on a posting in another language.
				for _, g := range []string{gates.GateLocation, gates.GateWorkplace, gates.GateWorkRights} {
					if n := len(pageByGate(rs)[g]); n != 0 {
						t.Errorf("gate %s ran on a language-failed posting (%d results)", g, n)
					}
				}
			} else {
				for _, g := range []string{gates.GateLocation, gates.GateWorkplace, gates.GateWorkRights} {
					pageOne(t, rs, g)
				}
				// "Fully remote within the EU." is in the fixture.
				if wp := pageOne(t, rs, gates.GateWorkplace); wp.Verdict != gates.Pass {
					t.Errorf("workplace = %s (%s), want pass", wp.Verdict, wp.Reason)
				}
			}
		})
	}
}

func TestLanguageReasonNamesTheLanguage(t *testing.T) {
	obs := observationFromPage(t, filepath.Join("..", "linkedin", "testdata", "detail_fr.html"))
	r := pageOne(t, gates.Evaluate(obs, pageCandidate, nil), gates.GateLanguage)
	if r.Reason != "posting is in French" {
		t.Errorf("reason = %q", r.Reason)
	}
}
