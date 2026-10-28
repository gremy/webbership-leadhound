// Package campaign holds the campaign spec: one YAML file per campaign, in
// <data dir>/campaigns/<slug>/campaign.yaml, naming what is sold, where leads come from, which
// detectors and kinds apply, the country policy, who writes the drafts, the copy rules and the
// step sequence. With no campaigns folder, Legacy builds the Drupal campaign from leads.yaml.
//
// In CP1 a spec is parsed, checked and registered only: batch and send still run on the legacy
// three-step path.
package campaign

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

// ErrInvalid wraps every problem with a spec.
var ErrInvalid = errors.New("invalid campaign spec")

// Source kinds.
const (
	SourceSiteList = "site_list"
	SourcePlaces   = "places" // CP7
)

// Writers.
const (
	WriterTemplate = "template"
	WriterClaude   = "claude"
)

// Step types.
const (
	StepEmail = "email"
	StepCall  = "call"
)

// Step conditions (when). An empty when means WhenAlways.
const (
	WhenAlways           = "always"
	WhenHasCallablePhone = "has_callable_phone"
	WhenCallNotConnected = "call_not_connected"
	WhenEmailConsent     = "email_consent"
)

// MaxSteps is the most steps a sequence may hold (steps 0 to 9).
const MaxSteps = 10

// Spec is one campaign.
type Spec struct {
	Slug         string             `yaml:"slug" json:"slug"`
	Name         string             `yaml:"name" json:"name"`
	Product      Product            `yaml:"product" json:"product"`
	Source       Source             `yaml:"source" json:"source"`
	Detectors    []string           `yaml:"detectors" json:"detectors"`
	Kinds        []string           `yaml:"kinds" json:"kinds"`
	Countries    map[string]Country `yaml:"countries" json:"countries"` // ISO 3166-1 alpha-2 → policy
	Writer       string             `yaml:"writer" json:"writer"`
	Offer        Offer              `yaml:"offer" json:"offer"`
	Copy         Copy               `yaml:"copy" json:"copy"`
	Sequence     []Step             `yaml:"sequence" json:"sequence"`           // the default sequence
	TemplatesDir string             `yaml:"templates_dir" json:"templates_dir"` // relative to the campaign folder
	// Sequences holds the named sequences beside the default one (call_first). A
	// sequences.default in the file is moved into Sequence by Decode, so both spellings hash
	// alike; omitempty keeps the hash of a spec without named sequences as it was.
	Sequences map[string][]Step `yaml:"sequences" json:"sequences,omitempty"`
}

// Sequence names.
const (
	SequenceDefault   = "default"
	SequenceCallFirst = "call_first"
)

// SequenceFor returns the named sequence: "" or "default" is the default one; nil when the
// spec has no sequence of that name.
func (s Spec) SequenceFor(name string) []Step {
	if name == "" || name == SequenceDefault {
		if len(s.Sequence) > 0 {
			return s.Sequence
		}
		return s.Sequences[SequenceDefault]
	}
	return s.Sequences[name]
}

// Product is what the campaign sells.
type Product struct {
	Name    string `yaml:"name" json:"name"`
	SiteURL string `yaml:"site_url" json:"site_url"`
}

// Source is where the campaign's leads come from.
type Source struct {
	Kind string `yaml:"kind" json:"kind"`
}

// Country is one country's policy for this campaign. YAML on/off decode into the bools.
type Country struct {
	Email bool   `yaml:"email" json:"email"`
	Call  bool   `yaml:"call" json:"call"`
	Lang  string `yaml:"lang" json:"lang"` // ISO 639-1; needed when email or call is on
}

// Offer is the proof line and the lead magnet.
type Offer struct {
	ProofLine string `yaml:"proof_line" json:"proof_line"`
	Magnet    string `yaml:"magnet" json:"magnet"`
}

// Copy holds the drafting rules.
type Copy struct {
	BannedWords  []string            `yaml:"banned_words" json:"banned_words"`
	ForbidByKind map[string][]string `yaml:"forbid_by_kind" json:"forbid_by_kind"` // kind → regexps a draft must not match
	MaxWords     map[int]int         `yaml:"max_words" json:"max_words"`           // step → most words
	LinksInStep0 bool                `yaml:"links_in_step0" json:"links_in_step0"`
	Exclamation  bool                `yaml:"exclamation" json:"exclamation"`
}

// Step is one step of the sequence.
type Step struct {
	Step              int    `yaml:"step" json:"step"`
	Type              string `yaml:"type" json:"type"`
	AfterBusinessDays int    `yaml:"after_business_days" json:"after_business_days"`
	When              string `yaml:"when" json:"when"`
}

// CheckOptions carry what Check needs from outside the spec.
type CheckOptions struct {
	DraftsTrusted bool     // the draft gate has passed, so writer: claude is allowed
	KnownKinds    []string // the lead kinds that exist; nil skips the check
}

var (
	slugRe = regexp.MustCompile(`^[a-z0-9-]{2,40}$`)
	langRe = regexp.MustCompile(`^[a-z]{2}$`)
)

// Decode reads one spec strictly: an unknown key is an error.
func Decode(b []byte) (Spec, error) {
	var s Spec
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&s); err != nil {
		if errors.Is(err, io.EOF) {
			return Spec{}, fmt.Errorf("campaign: %w: the file is empty", ErrInvalid)
		}
		return Spec{}, fmt.Errorf("campaign: %w: %s", ErrInvalid, err.Error())
	}
	if def, ok := s.Sequences[SequenceDefault]; ok && len(s.Sequence) == 0 {
		s.Sequence = def
		delete(s.Sequences, SequenceDefault)
	}
	if len(s.Sequences) == 0 {
		s.Sequences = nil
	}
	return s, nil
}

// Check reports every problem with the spec at once, joined; nil when it is sound.
func (s Spec) Check(opts CheckOptions) error {
	var errs []error
	add := func(format string, a ...any) {
		errs = append(errs, fmt.Errorf("campaign %q: %w: "+format, append([]any{s.Slug, ErrInvalid}, a...)...))
	}

	if !slugRe.MatchString(s.Slug) {
		add("slug %q must match %s", s.Slug, slugRe.String())
	}
	if strings.TrimSpace(s.Name) == "" {
		add("name is empty")
	}

	switch s.Source.Kind {
	case SourceSiteList:
	case SourcePlaces:
		add("source.kind %q comes in CP7; only %q is accepted now", s.Source.Kind, SourceSiteList)
	default:
		add("source.kind %q is unknown; only %q is accepted now", s.Source.Kind, SourceSiteList)
	}

	if len(s.Detectors) == 0 {
		add("detectors is empty")
	}
	for i, d := range s.Detectors {
		if !KnownDetector(d) {
			add("detectors: %q is not a known detector (known: %s)", d, strings.Join(Detectors(), ", "))
		}
		if slices.Contains(s.Detectors[:i], d) {
			add("detectors: %q is listed twice", d)
		}
	}

	if len(s.Kinds) == 0 {
		add("kinds is empty")
	}
	for i, k := range s.Kinds {
		if opts.KnownKinds != nil && !slices.Contains(opts.KnownKinds, k) {
			add("kinds: %q is not a known lead kind (known: %s)", k, strings.Join(opts.KnownKinds, ", "))
		}
		if slices.Contains(s.Kinds[:i], k) {
			add("kinds: %q is listed twice", k)
		}
	}

	for _, code := range sortedKeys(s.Countries) {
		c := s.Countries[code]
		if !isCountry(code) {
			add("countries: %q is not an ISO 3166-1 alpha-2 code (two upper-case letters)", code)
		}
		if c.Lang != "" && !langRe.MatchString(c.Lang) {
			add("countries.%s.lang %q is not a two-letter language code", code, c.Lang)
		}
		if (c.Email || c.Call) && c.Lang == "" {
			add("countries.%s: lang is needed when email or call is on", code)
		}
	}

	switch s.Writer {
	case WriterTemplate:
	case WriterClaude:
		if !opts.DraftsTrusted {
			add("writer %q is refused until the draft gate passes", WriterClaude)
		}
	default:
		add("writer %q must be %q or %q", s.Writer, WriterTemplate, WriterClaude)
	}

	for _, kind := range sortedKeys(s.Copy.ForbidByKind) {
		if !slices.Contains(s.Kinds, kind) {
			add("copy.forbid_by_kind: %q is not one of the campaign's kinds", kind)
		}
		for _, pat := range s.Copy.ForbidByKind[kind] {
			if _, err := regexp.Compile(pat); err != nil {
				add("copy.forbid_by_kind.%s: %q does not compile: %v", kind, pat, err)
			}
		}
	}
	longest := len(s.Sequence)
	for _, seq := range s.Sequences {
		longest = max(longest, len(seq))
	}
	for _, step := range sortedKeys(s.Copy.MaxWords) {
		if step < 0 || step >= longest {
			add("copy.max_words: step %d is not in the sequence", step)
		}
		if s.Copy.MaxWords[step] <= 0 {
			add("copy.max_words.%d must be above 0", step)
		}
	}

	if s.TemplatesDir != "" && !filepath.IsLocal(s.TemplatesDir) {
		add("templates_dir %q must be a path inside the campaign folder", s.TemplatesDir)
	}

	errs = append(errs, s.checkSequence()...)
	return errors.Join(errs...)
}

func (s Spec) checkSequence() []error {
	var errs []error
	add := func(format string, a ...any) {
		errs = append(errs, fmt.Errorf("campaign %q: %w: "+format, append([]any{s.Slug, ErrInvalid}, a...)...))
	}
	if _, ok := s.Sequences[SequenceDefault]; ok {
		add("set either sequence or sequences.default, not both")
	}
	checkSteps(add, "sequence", s.Sequence, StepEmail)
	for _, name := range sortedKeys(s.Sequences) {
		switch name {
		case SequenceDefault:
		case SequenceCallFirst:
			checkSteps(add, "sequences."+name, s.Sequences[name], StepCall)
		default:
			add("sequences: %q is not a known sequence (known: %s, %s)", name, SequenceDefault, SequenceCallFirst)
		}
	}
	return errs
}

// checkSteps checks one sequence, named label in the messages: steps 0, 1, 2 … with no gap,
// at most MaxSteps, starting with first, known types and conditions, and call_not_connected
// and email_consent only after a call step.
func checkSteps(add func(string, ...any), label string, seq []Step, first string) {
	if len(seq) == 0 {
		add("%s is empty", label)
		return
	}
	if len(seq) > MaxSteps {
		add("%s has %d steps; at most %d", label, len(seq), MaxSteps)
	}
	seenCall := false
	for i, st := range seq {
		if st.Step != i {
			add("%s[%d]: step is %d; steps run 0, 1, 2 … in order with no gap", label, i, st.Step)
		}
		switch st.Type {
		case StepEmail, StepCall:
		default:
			add("%s[%d]: type %q must be %q or %q", label, i, st.Type, StepEmail, StepCall)
		}
		if i == 0 && st.Type != first {
			add("%s must start with %s %s, not %q", label, article(first), first, st.Type)
		}
		if st.AfterBusinessDays < 0 {
			add("%s[%d]: after_business_days %d is below 0", label, i, st.AfterBusinessDays)
		}
		switch st.When {
		case "", WhenAlways, WhenHasCallablePhone:
		case WhenCallNotConnected, WhenEmailConsent:
			if !seenCall {
				add("%s[%d]: when %q needs a call step before it", label, i, st.When)
			}
		default:
			add("%s[%d]: when %q must be one of %s, %s, %s, %s", label, i, st.When,
				WhenAlways, WhenHasCallablePhone, WhenCallNotConnected, WhenEmailConsent)
		}
		if st.Type == StepCall {
			seenCall = true
		}
	}
}

func article(word string) string {
	if strings.ContainsRune("aeiou", rune(word[0])) {
		return "an"
	}
	return "a"
}

// Canonical returns the spec as stable JSON and its SHA-256 in hex. Map keys are sorted and
// struct fields keep their order, so the hash changes only when the spec does.
func Canonical(s Spec) ([]byte, string, error) {
	b, err := json.Marshal(s)
	if err != nil {
		return nil, "", fmt.Errorf("campaign: canonical %q: %w", s.Slug, err)
	}
	sum := sha256.Sum256(b)
	return b, hex.EncodeToString(sum[:]), nil
}

func sortedKeys[K int | string, V any](m map[K]V) []K {
	keys := make([]K, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// isoAlpha2 are the officially assigned ISO 3166-1 alpha-2 codes.
const isoAlpha2 = "AD AE AF AG AI AL AM AO AQ AR AS AT AU AW AX AZ BA BB BD BE BF BG BH BI BJ BL BM BN BO BQ BR BS BT BV BW BY BZ " +
	"CA CC CD CF CG CH CI CK CL CM CN CO CR CU CV CW CX CY CZ DE DJ DK DM DO DZ EC EE EG EH ER ES ET FI FJ FK FM FO FR " +
	"GA GB GD GE GF GG GH GI GL GM GN GP GQ GR GS GT GU GW GY HK HM HN HR HT HU ID IE IL IM IN IO IQ IR IS IT JE JM JO JP " +
	"KE KG KH KI KM KN KP KR KW KY KZ LA LB LC LI LK LR LS LT LU LV LY MA MC MD ME MF MG MH MK ML MM MN MO MP MQ MR MS MT MU MV MW MX MY MZ " +
	"NA NC NE NF NG NI NL NO NP NR NU NZ OM PA PE PF PG PH PK PL PM PN PR PS PT PW PY QA RE RO RS RU RW " +
	"SA SB SC SD SE SG SH SI SJ SK SL SM SN SO SR SS ST SV SX SY SZ TC TD TF TG TH TJ TK TL TM TN TO TR TT TV TW TZ " +
	"UA UG UM US UY UZ VA VC VE VG VI VN VU WF WS YE YT ZA ZM ZW"

var countries = func() map[string]bool {
	m := map[string]bool{}
	for _, c := range strings.Fields(isoAlpha2) {
		m[c] = true
	}
	return m
}()

func isCountry(code string) bool { return countries[code] }
