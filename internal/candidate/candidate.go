// Package candidate loads the candidate profile, leadhound.yaml in LEADHOUND_DATA_DIR:
// the languages, places and work rights the gates read, the criteria the scorer asks of a
// posting, the LinkedIn search matrix and the brief's caps. The file is private; the
// repository ships a fictional one in examples/leadhound.demo.yaml.
package candidate

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/gremy/webbership-leadhound/internal/gates"
	"github.com/gremy/webbership-leadhound/internal/linkedin"
)

// FileName is the profile's name inside the data directory.
const FileName = "leadhound.yaml"

// ErrInvalid wraps every problem with the file's content.
var ErrInvalid = errors.New("invalid candidate profile")

// Config is the whole file.
type Config struct {
	Candidate Candidate `yaml:"candidate"`
	// ConfidenceThreshold is the scorer confidence below which a verdict reads unclear;
	// strictly between 0 and 1.
	ConfidenceThreshold float64  `yaml:"confidence_threshold"`
	LinkedIn            LinkedIn `yaml:"linkedin"`
	Brief               Brief    `yaml:"brief"`
	// TitleScreenRules is the optional title pre-filter; TitleScreen() compiles it.
	TitleScreenRules TitleRules `yaml:"title_screen"`
}

// Candidate is who the postings are screened for.
type Candidate struct {
	Languages        []string `yaml:"languages"`         // ISO 639-1 codes
	AllowedCountries []string `yaml:"allowed_countries"` // where they can be employed or contract from
	AllowedCities    []string `yaml:"allowed_cities"`    // residence or office requirements naming these pass
	RemoteOnly       bool     `yaml:"remote_only"`
	WorkRights       string   `yaml:"work_rights"` // e.g. "EU citizen"
	Criteria         Criteria `yaml:"criteria"`
	// EvidenceKeywords lists, per criterion name, the words whose lines the scorer quotes
	// as evidence for that criterion's reason.
	EvidenceKeywords map[string][]string `yaml:"evidence_keywords"`
}

// Criteria are the scorer's questions, one per name in CriterionNames.
type Criteria struct {
	Title      Criterion `yaml:"title"`
	Stack      Criterion `yaml:"stack"`
	Seniority  Criterion `yaml:"seniority"`
	Location   Criterion `yaml:"location"`
	WorkRights Criterion `yaml:"work_rights"`
}

// Criterion describes what a passing, failing and unclear posting looks like.
type Criterion struct {
	Pass    string `yaml:"pass"`
	Fail    string `yaml:"fail"`
	Unclear string `yaml:"unclear"`
}

// NamedCriterion is a criterion with its name.
type NamedCriterion struct {
	Name string
	Criterion
}

// CriterionNames are the criteria in the order the scorer asks them.
var CriterionNames = []string{"title", "stack", "seniority", "location", "work_rights"}

// List returns the criteria in CriterionNames order.
func (c Criteria) List() []NamedCriterion {
	return []NamedCriterion{
		{"title", c.Title},
		{"stack", c.Stack},
		{"seniority", c.Seniority},
		{"location", c.Location},
		{"work_rights", c.WorkRights},
	}
}

// LinkedIn is the search matrix and the daily canary's query.
type LinkedIn struct {
	Canary    CanaryQuery `yaml:"canary"`
	MaxPages  int         `yaml:"max_pages"`
	Keywords  []string    `yaml:"keywords"`
	Locations []string    `yaml:"locations"`
}

// CanaryQuery is the one search the canary runs.
type CanaryQuery struct {
	Keywords string `yaml:"keywords"`
	Location string `yaml:"location"`
}

// Brief caps the morning brief's sections.
type Brief struct {
	MaxApply    int `yaml:"max_apply"`
	MaxYourCall int `yaml:"max_your_call"`
}

// Load reads FileName from dataDir.
func Load(dataDir string) (Config, error) {
	return LoadFile(filepath.Join(dataDir, FileName))
}

// LoadFile reads and checks the profile at path.
func LoadFile(path string) (Config, error) {
	b, err := os.ReadFile(path) // #nosec G304 -- the operator's own data directory
	if err != nil {
		return Config{}, fmt.Errorf("candidate: %w", err)
	}
	cfg, err := Parse(b)
	if err != nil {
		return Config{}, fmt.Errorf("candidate: %s: %w", filepath.Base(path), err)
	}
	return cfg, nil
}

// Parse decodes a profile strictly (unknown keys are errors) and checks it. Every
// problem is reported at once, wrapped in ErrInvalid.
func Parse(b []byte) (Config, error) {
	var cfg Config
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		if errors.Is(err, io.EOF) {
			return Config{}, fmt.Errorf("%w: the file is empty", ErrInvalid)
		}
		return Config{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if problems := cfg.check(); len(problems) > 0 {
		return Config{}, fmt.Errorf("%w: %s", ErrInvalid, strings.Join(problems, "; "))
	}
	return cfg, nil
}

func (cfg Config) check() []string {
	var p []string
	c := cfg.Candidate
	if len(c.Languages) == 0 {
		p = append(p, "candidate.languages is empty")
	}
	for _, l := range c.Languages {
		if !isISO6391(l) {
			p = append(p, fmt.Sprintf("candidate.languages: %q is not an ISO 639-1 code (two lower-case letters, e.g. en)", l))
		}
	}
	p = append(p, blanks("candidate.allowed_countries", c.AllowedCountries)...)
	p = append(p, blanks("candidate.allowed_cities", c.AllowedCities)...)
	for _, nc := range c.Criteria.List() {
		field := "candidate.criteria." + nc.Name
		if nc.Pass == "" && nc.Fail == "" && nc.Unclear == "" {
			p = append(p, field+" is missing")
			continue
		}
		for _, part := range []struct{ name, text string }{{"pass", nc.Pass}, {"fail", nc.Fail}, {"unclear", nc.Unclear}} {
			if strings.TrimSpace(part.text) == "" {
				p = append(p, field+"."+part.name+" is empty")
			}
		}
	}
	for _, name := range sortedKeys(c.EvidenceKeywords) {
		if !slices.Contains(CriterionNames, name) {
			p = append(p, fmt.Sprintf("candidate.evidence_keywords.%s is not a criterion (want one of %s)", name, strings.Join(CriterionNames, ", ")))
		}
		p = append(p, blanks("candidate.evidence_keywords."+name, c.EvidenceKeywords[name])...)
	}
	if !(cfg.ConfidenceThreshold > 0 && cfg.ConfidenceThreshold < 1) {
		p = append(p, "confidence_threshold must be between 0 and 1, exclusive")
	}
	li := cfg.LinkedIn
	if strings.TrimSpace(li.Canary.Keywords) == "" || strings.TrimSpace(li.Canary.Location) == "" {
		p = append(p, "linkedin.canary needs both keywords and location")
	}
	if li.MaxPages < 1 {
		p = append(p, "linkedin.max_pages must be at least 1")
	}
	if len(li.Keywords) == 0 {
		p = append(p, "linkedin.keywords is empty")
	}
	p = append(p, blanks("linkedin.keywords", li.Keywords)...)
	if len(li.Locations) == 0 {
		p = append(p, "linkedin.locations is empty")
	}
	p = append(p, blanks("linkedin.locations", li.Locations)...)
	if cfg.Brief.MaxApply < 1 {
		p = append(p, "brief.max_apply must be at least 1")
	}
	if cfg.Brief.MaxYourCall < 1 {
		p = append(p, "brief.max_your_call must be at least 1")
	}
	p = append(p, checkTitleRules(cfg.TitleScreenRules)...)
	return p
}

// blanks reports a blank entry in a list.
func blanks(field string, list []string) []string {
	for _, s := range list {
		if strings.TrimSpace(s) == "" {
			return []string{field + " has a blank entry"}
		}
	}
	return nil
}

func sortedKeys(m map[string][]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// Gates returns the profile the gates read.
func (cfg Config) Gates() gates.CandidateGates {
	c := cfg.Candidate
	return gates.CandidateGates{
		Languages:        slices.Clone(c.Languages),
		AllowedCountries: slices.Clone(c.AllowedCountries),
		AllowedCities:    slices.Clone(c.AllowedCities),
		RemoteOnly:       c.RemoteOnly,
		WorkRights:       c.WorkRights,
	}
}

// Matrix returns the LinkedIn search matrix.
func (cfg Config) Matrix() linkedin.Matrix {
	return linkedin.Matrix{
		Keywords:  slices.Clone(cfg.LinkedIn.Keywords),
		Locations: slices.Clone(cfg.LinkedIn.Locations),
		MaxPages:  cfg.LinkedIn.MaxPages,
	}
}

// CanaryQuery returns the canary's keywords and location.
func (cfg Config) CanaryQuery() (keywords, location string) {
	return cfg.LinkedIn.Canary.Keywords, cfg.LinkedIn.Canary.Location
}
