package candidate

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const demoPath = "../../examples/leadhound.demo.yaml"

func demoBytes(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(demoPath)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestDemoLoads(t *testing.T) {
	cfg, err := LoadFile(demoPath)
	if err != nil {
		t.Fatalf("demo: %v", err)
	}
	if cfg.ConfidenceThreshold != 0.7 {
		t.Errorf("confidence_threshold = %v", cfg.ConfidenceThreshold)
	}
	if got := cfg.Candidate.Criteria.Stack.Pass; !strings.Contains(got, "Rust") {
		t.Errorf("stack.pass = %q", got)
	}
	if cfg.Brief.MaxApply != 5 || cfg.Brief.MaxYourCall != 5 {
		t.Errorf("brief = %+v", cfg.Brief)
	}
	if kw := cfg.Candidate.EvidenceKeywords["stack"]; !slices.Contains(kw, "rust") {
		t.Errorf("evidence_keywords.stack = %v", kw)
	}
}

// TestLoadReadsDataDir: Load reads leadhound.yaml from the data directory.
func TestLoadReadsDataDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(demoBytes(t)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, err := Load(t.TempDir()); err == nil || !strings.Contains(err.Error(), FileName) {
		t.Errorf("missing file: err = %v, want one naming %s", err, FileName)
	}
}

func TestHelpers(t *testing.T) {
	cfg, err := LoadFile(demoPath)
	if err != nil {
		t.Fatal(err)
	}
	g := cfg.Gates()
	if !slices.Equal(g.Languages, []string{"en", "de"}) || !slices.Equal(g.AllowedCountries, []string{"Exampleland"}) ||
		!slices.Equal(g.AllowedCities, []string{"Exampleville"}) || !g.RemoteOnly || g.WorkRights != "Exampleland citizen" {
		t.Errorf("Gates() = %+v", g)
	}
	m := cfg.Matrix()
	if !slices.Equal(m.Keywords, []string{"rust developer", "rust backend"}) || !slices.Equal(m.Locations, []string{"European Union"}) || m.MaxPages != 2 {
		t.Errorf("Matrix() = %+v", m)
	}
	if kw, loc := cfg.CanaryQuery(); kw != "software engineer" || loc != "European Union" {
		t.Errorf("CanaryQuery() = %q, %q", kw, loc)
	}
	list := cfg.Candidate.Criteria.List()
	if len(list) != len(CriterionNames) {
		t.Fatalf("List() has %d criteria", len(list))
	}
	for i, nc := range list {
		if nc.Name != CriterionNames[i] || nc.Pass == "" {
			t.Errorf("List()[%d] = %+v", i, nc)
		}
	}
	// The slices are copies: changing one never changes the loaded config.
	g.Languages[0] = "xx"
	m.Keywords[0] = "xx"
	if cfg.Candidate.Languages[0] != "en" || cfg.LinkedIn.Keywords[0] != "rust developer" {
		t.Error("helpers share slices with the config")
	}
}

// TestInvalid: every broken file is rejected with ErrInvalid and a message naming the
// field.
func TestInvalid(t *testing.T) {
	demo := demoBytes(t)
	cases := []struct {
		name, old, new, want string
	}{
		{"unknown top-level key", "confidence_threshold: 0.7", "confidence_threshold: 0.7\nthreshold: 0.5", "threshold"},
		{"unknown candidate key", "  remote_only: true", "  remote_only: true\n  salary: 1", "salary"},
		{"unknown criterion", "    title:\n", "    culture:\n      pass: a\n      fail: b\n      unclear: c\n    title:\n", "culture"},
		{"missing criterion", "    seniority:\n      pass: \"Mid-level or senior: three to eight years of experience.\"\n      fail: \"Intern, junior or graduate roles, or principal and director roles.\"\n      unclear: \"No level or years of experience given.\"\n", "", "criteria.seniority"},
		{"empty unclear", "      unclear: \"The posting names no languages.\"", "      unclear: \"\"", "criteria.stack.unclear"},
		{"threshold zero", "confidence_threshold: 0.7", "confidence_threshold: 0", "confidence_threshold"},
		{"threshold one", "confidence_threshold: 0.7", "confidence_threshold: 1", "confidence_threshold"},
		{"threshold missing", "confidence_threshold: 0.7\n", "", "confidence_threshold"},
		{"language not ISO 639-1", "languages: [en, de]", "languages: [en, german]", "german"},
		{"language upper case", "languages: [en, de]", "languages: [en, DE]", "DE"},
		{"no languages", "languages: [en, de]", "languages: []", "languages"},
		{"evidence for unknown criterion", "    stack: [rust,", "    salary: [high]\n    stack: [rust,", "evidence_keywords.salary"},
		{"no keywords", "keywords: [rust developer, rust backend]", "keywords: []", "linkedin.keywords"},
		{"blank location", "locations: [European Union]", "locations: [\"\"]", "linkedin.locations"},
		{"no canary", "  canary: {keywords: \"software engineer\", location: \"European Union\"}\n", "", "linkedin.canary"},
		{"max pages zero", "max_pages: 2", "max_pages: 0", "linkedin.max_pages"},
		{"brief cap zero", "max_apply: 5", "max_apply: 0", "brief.max_apply"},
		{"wrong type names the line", "remote_only: true", "remote_only: sometimes", "line 9"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !strings.Contains(demo, tc.old) {
				t.Fatalf("demo has no %q", tc.old)
			}
			_, err := Parse([]byte(strings.Replace(demo, tc.old, tc.new, 1)))
			if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want ErrInvalid naming %q", err, tc.want)
			}
		})
	}
}

func TestEmptyFile(t *testing.T) {
	if _, err := Parse(nil); !errors.Is(err, ErrInvalid) {
		t.Errorf("empty: err = %v", err)
	}
}

// TestErrorsListEveryProblem: one load reports every problem, not only the first.
func TestErrorsListEveryProblem(t *testing.T) {
	demo := strings.Replace(demoBytes(t), "confidence_threshold: 0.7", "confidence_threshold: 2", 1)
	demo = strings.Replace(demo, "max_pages: 2", "max_pages: 0", 1)
	_, err := Parse([]byte(demo))
	if err == nil || !strings.Contains(err.Error(), "confidence_threshold") || !strings.Contains(err.Error(), "max_pages") {
		t.Fatalf("err = %v", err)
	}
}
