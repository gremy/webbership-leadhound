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
	"net/url"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"

	"github.com/gremy/webbership-leadhound/internal/cafe"
)

// ErrInvalid wraps every problem with a spec.
var ErrInvalid = errors.New("invalid campaign spec")

// Source kinds.
const (
	SourceSiteList = "site_list"
	SourceOverture = "overture" // Overture Maps Places, imported with leads places import overture
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
	// Chains is how a places campaign tells chains from independent places (nil: the
	// defaults, cafe.DefaultMaxLocations and no names); omitempty keeps older hashes.
	Chains *Chains `yaml:"chains" json:"chains,omitempty"`
}

// Chains is a places campaign's chain rule (internal/cafe ChainConfig).
type Chains struct {
	MaxLocations int      `yaml:"max_locations" json:"max_locations,omitempty"` // 0: the default (5)
	Names        []string `yaml:"names" json:"names,omitempty"`                 // known chain names
	KeepBranded  bool     `yaml:"keep_branded" json:"keep_branded,omitempty"`   // keep places Overture lists under a brand
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
	// The place filters of an overture source (each omitempty, so a site_list spec keeps its
	// hash): Overture primary categories (none: coffee_shop and cafe), ISO alpha-2 countries
	// (none: every country), and the confidence below which a place is skipped (0: 0.7).
	Categories    []string `yaml:"categories" json:"categories,omitempty"`
	Countries     []string `yaml:"countries" json:"countries,omitempty"`
	MinConfidence float64  `yaml:"min_confidence" json:"min_confidence,omitempty"`
}

// Country is one country's policy for this campaign. YAML on/off decode into the bools.
type Country struct {
	Email bool   `yaml:"email" json:"email"`
	Call  bool   `yaml:"call" json:"call"`
	Lang  string `yaml:"lang" json:"lang"` // ISO 639-1; needed when email or call is on
	// Acknowledge is the operator's written reason for calling or emailing a country the
	// compliance table holds or needs a reason for (compliance.CampaignCountry.Acknowledge);
	// one line, at most MaxAcknowledge characters.
	Acknowledge string `yaml:"acknowledge" json:"acknowledge,omitempty"`
	// Footer names the footer file (footers/<name>.txt) this campaign's emails to the country
	// end with, instead of leads.yaml's; a plain file name, only with email on. omitempty
	// keeps older hashes.
	Footer string `yaml:"footer" json:"footer,omitempty"`
}

// footerNameRe is leadconfig's plain file-name rule for footers (mirrored; a test pins it).
var footerNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// MaxAcknowledge is the longest country acknowledge, in characters.
const MaxAcknowledge = 500

// Offer is the proof line, the lead magnet, and what a Claude draft may say about it. The
// fields added for Claude drafting (CP4) are omitempty, so a spec without them keeps its hash.
type Offer struct {
	ProofLine string `yaml:"proof_line" json:"proof_line"`
	Magnet    string `yaml:"magnet" json:"magnet"`
	// Pains is the closed list of pains a draft may name; code picks the ones a lead's
	// stored signals allow (Spec.PainsFor), and Claude may reword a chosen one, never add one.
	Pains []Pain `yaml:"pains" json:"pains,omitempty"`
	// Report is the free report's name and ReportContains what it holds (offer facts).
	Report         string   `yaml:"report" json:"report,omitempty"`
	ReportContains []string `yaml:"report_contains" json:"report_contains,omitempty"`
	// CallToAction is the closing question, verbatim ("Worth 10 minutes to find out?").
	CallToAction string `yaml:"call_to_action" json:"call_to_action,omitempty"`
	// Links are the URLs an email may carry beside the product site: absolute https, at most
	// MaxOfferLinks, each once. omitempty keeps older hashes.
	Links []string `yaml:"links" json:"links,omitempty"`
	// VendorLinks maps a cafe@1 loyalty vendor id (or VendorLinkDefault) to the page an email
	// names for a café on that vendor: absolute https. omitempty keeps older hashes.
	VendorLinks map[string]string `yaml:"vendor_links" json:"vendor_links,omitempty"`
}

// MaxOfferLinks is the most offer.links a spec may list.
const MaxOfferLinks = 10

// VendorLinkDefault is the offer.vendor_links key used when a café's vendor has no own entry.
const VendorLinkDefault = "default"

// Pain is one pain a draft may name, applying to a lead when every one of its needs holds
// (see Need for the closed language).
type Pain struct {
	ID    string   `yaml:"id" json:"id"`
	Needs []string `yaml:"needs" json:"needs"`
	Text  string   `yaml:"text" json:"text"`
}

// Copy holds the drafting rules.
type Copy struct {
	BannedWords  []string            `yaml:"banned_words" json:"banned_words"`
	ForbidByKind map[string][]string `yaml:"forbid_by_kind" json:"forbid_by_kind"` // kind → regexps a draft must not match
	MaxWords     map[int]int         `yaml:"max_words" json:"max_words"`           // step → most words
	// MaxWordsBySequence gives a named sequence (call_first) its own word limits, step → most
	// words; a sequence it leaves out takes MaxWords. omitempty keeps older hashes.
	MaxWordsBySequence map[string]map[int]int `yaml:"max_words_by_sequence" json:"max_words_by_sequence,omitempty"`
	LinksInStep0       bool                   `yaml:"links_in_step0" json:"links_in_step0"`
	Exclamation        bool                   `yaml:"exclamation" json:"exclamation"`
	// The grounding rules of a Claude draft (CP4; omitempty keeps older specs' hashes).
	// AllowedNames are the proper nouns any text may use (the operator's own names, Drupal…).
	AllowedNames []string `yaml:"allowed_names" json:"allowed_names,omitempty"`
	// ResearchOpeners are regexps step 0's opening paragraph must not match; none listed
	// keeps the check's defaults.
	ResearchOpeners []string `yaml:"research_openers" json:"research_openers,omitempty"`
	// InflationWords are the boast words refused outside the proof line; none listed keeps
	// the check's defaults.
	InflationWords []string `yaml:"inflation_words" json:"inflation_words,omitempty"`
	// EchoWords is the run of words a draft may not share with the org's own text; 0 keeps
	// the check's default (6), else MinEchoWords..MaxEchoWords.
	EchoWords int `yaml:"echo_words" json:"echo_words,omitempty"`
}

// The echo run length a spec may set.
const (
	MinEchoWords = 3
	MaxEchoWords = 20
)

var painIDRe = regexp.MustCompile(`^[a-z0-9_]{1,40}$`)

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

	errs = append(errs, s.checkSource(opts.KnownKinds != nil)...)

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
		if c.Acknowledge != "" && !oneLine(c.Acknowledge, MaxAcknowledge) {
			add("countries.%s.acknowledge must be one non-blank line of at most %d characters", code, MaxAcknowledge)
		}
		if c.Footer != "" {
			if !footerNameRe.MatchString(c.Footer) {
				add("countries.%s.footer: %q is not a plain file name", code, c.Footer)
			}
			if !c.Email {
				add("countries.%s.footer is only for a country with email on", code)
			}
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

	for _, name := range sortedKeys(s.Copy.MaxWordsBySequence) {
		seq := s.SequenceFor(name)
		if seq == nil && name == SequenceCallFirst {
			seq = DefaultCallFirst()
		}
		switch {
		case name == "" || name == SequenceDefault:
			add("copy.max_words_by_sequence: %q: the default sequence's limits are copy.max_words", name)
			continue
		case seq == nil:
			add("copy.max_words_by_sequence: %q is not a sequence of the campaign", name)
			continue
		}
		for _, step := range sortedKeys(s.Copy.MaxWordsBySequence[name]) {
			if step < 0 || step >= len(seq) || seq[step].Type != StepEmail {
				add("copy.max_words_by_sequence.%s: step %d is not an email step of that sequence", name, step)
			}
			if s.Copy.MaxWordsBySequence[name][step] <= 0 {
				add("copy.max_words_by_sequence.%s.%d must be above 0", name, step)
			}
		}
	}

	errs = append(errs, s.checkDrafting()...)

	if s.TemplatesDir != "" && !filepath.IsLocal(s.TemplatesDir) {
		add("templates_dir %q must be a path inside the campaign folder", s.TemplatesDir)
	}

	errs = append(errs, s.checkSequence()...)
	return errors.Join(errs...)
}

// MaxWordsFor is the word limits of the named sequence: its own copy.max_words_by_sequence
// entry, else copy.max_words.
func (s Spec) MaxWordsFor(name string) map[int]int {
	if w, ok := s.Copy.MaxWordsBySequence[name]; ok && name != "" && name != SequenceDefault {
		return w
	}
	return s.Copy.MaxWords
}

// checkSource checks the source and that its detectors and kinds go with it: a site_list
// campaign runs the site detectors, an overture one cafe@1, and (with kinds checked) every
// kind is one its detectors make.
func (s Spec) checkSource(kinds bool) []error {
	var errs []error
	add := func(format string, a ...any) {
		errs = append(errs, fmt.Errorf("campaign %q: %w: "+format, append([]any{s.Slug, ErrInvalid}, a...)...))
	}
	src := s.Source
	switch src.Kind {
	case SourceSiteList:
		if len(src.Categories) > 0 || len(src.Countries) > 0 || src.MinConfidence != 0 {
			add("source: categories, countries and min_confidence are for an %q source", SourceOverture)
		}
		if s.Chains != nil {
			add("chains is for an %q source", SourceOverture)
		}
	case SourceOverture:
		for i, c := range src.Categories {
			if !categoryRe.MatchString(c) {
				add("source.categories: %q is not an Overture category name", c)
			}
			if slices.Contains(src.Categories[:i], c) {
				add("source.categories: %q is listed twice", c)
			}
		}
		for i, c := range src.Countries {
			if !isCountry(c) {
				add("source.countries: %q is not an ISO 3166-1 alpha-2 code (two upper-case letters)", c)
			}
			if slices.Contains(src.Countries[:i], c) {
				add("source.countries: %q is listed twice", c)
			}
		}
		if src.MinConfidence < 0 || src.MinConfidence > 1 {
			add("source.min_confidence %v must be 0 (the default) to 1", src.MinConfidence)
		}
		if c := s.Chains; c != nil {
			if c.MaxLocations < 0 {
				add("chains.max_locations %d is below 0", c.MaxLocations)
			}
			for i, n := range c.Names {
				if !oneLine(n, 100) {
					add("chains.names: %q must be one non-blank line of at most 100 characters", n)
				}
				if slices.Contains(c.Names[:i], n) {
					add("chains.names: %q is listed twice", n)
				}
			}
		}
	default:
		add("source.kind %q is unknown (known: %s, %s)", src.Kind, SourceSiteList, SourceOverture)
		return errs
	}
	made := map[string]bool{}
	for _, d := range s.Detectors {
		if want := DetectorSource(d); want != "" && want != src.Kind {
			add("detectors: %q runs on a %q source, not %q", d, want, src.Kind)
		}
		for _, k := range DetectorKinds(d) {
			made[k] = true
		}
	}
	if kinds && len(made) > 0 {
		for _, k := range s.Kinds {
			if !made[k] {
				add("kinds: %q is not a kind the campaign's detectors make", k)
			}
		}
	}
	return errs
}

var categoryRe = regexp.MustCompile(`^[a-z0-9_]{1,60}$`)

// checkDrafting checks what a Claude draft is grounded on: the copy rules and the offer's
// pains, report and call to action.
func (s Spec) checkDrafting() []error {
	var errs []error
	add := func(format string, a ...any) {
		errs = append(errs, fmt.Errorf("campaign %q: %w: "+format, append([]any{s.Slug, ErrInvalid}, a...)...))
	}
	words := func(key string, list []string, most int) {
		for i, w := range list {
			if !oneLine(w, most) {
				add("%s: %q must be one non-blank line of at most %d characters", key, w, most)
			}
			for _, prev := range list[:i] {
				if strings.EqualFold(strings.TrimSpace(prev), strings.TrimSpace(w)) {
					add("%s: %q is listed twice", key, w)
				}
			}
		}
	}
	words("copy.allowed_names", s.Copy.AllowedNames, 60)
	words("copy.inflation_words", s.Copy.InflationWords, 60)
	for _, pat := range s.Copy.ResearchOpeners {
		if strings.TrimSpace(pat) == "" {
			add("copy.research_openers: a pattern is blank")
		} else if _, err := regexp.Compile(pat); err != nil {
			add("copy.research_openers: %q does not compile: %v", pat, err)
		}
	}
	if s.Copy.EchoWords != 0 && (s.Copy.EchoWords < MinEchoWords || s.Copy.EchoWords > MaxEchoWords) {
		add("copy.echo_words %d must be 0 (the default) or %d to %d", s.Copy.EchoWords, MinEchoWords, MaxEchoWords)
	}

	for i, p := range s.Offer.Pains {
		label := fmt.Sprintf("offer.pains[%d]", i)
		if !painIDRe.MatchString(p.ID) {
			add("%s: id %q must match %s", label, p.ID, painIDRe.String())
		}
		for _, prev := range s.Offer.Pains[:i] {
			if prev.ID == p.ID {
				add("%s: id %q is used twice", label, p.ID)
			}
		}
		if !oneLine(p.Text, 300) {
			add("%s (%s): text must be one non-blank line of at most 300 characters", label, p.ID)
		}
		if len(p.Needs) == 0 {
			add("%s (%s): needs is empty; a pain applies only on a stored signal", label, p.ID)
		}
		for _, raw := range p.Needs {
			n, err := ParseNeed(raw)
			if err != nil {
				add("%s (%s): %v", label, p.ID, err)
				continue
			}
			if n.field == needKind && !slices.Contains(s.Kinds, n.kind) {
				add("%s (%s): need %q names a kind the campaign does not have", label, p.ID, raw)
			}
		}
	}
	if s.Offer.Report != "" && !oneLine(s.Offer.Report, 100) {
		add("offer.report must be one line of at most 100 characters")
	}
	if len(s.Offer.ReportContains) > 0 && s.Offer.Report == "" {
		add("offer.report_contains needs offer.report")
	}
	words("offer.report_contains", s.Offer.ReportContains, 200)
	if s.Offer.CallToAction != "" && !oneLine(s.Offer.CallToAction, 120) {
		add("offer.call_to_action must be one line of at most 120 characters")
	}
	return append(errs, s.checkLinks()...)
}

// checkLinks checks offer.links and offer.vendor_links.
func (s Spec) checkLinks() []error {
	var errs []error
	add := func(format string, a ...any) {
		errs = append(errs, fmt.Errorf("campaign %q: %w: "+format, append([]any{s.Slug, ErrInvalid}, a...)...))
	}
	if len(s.Offer.Links) > MaxOfferLinks {
		add("offer.links has %d links; at most %d", len(s.Offer.Links), MaxOfferLinks)
	}
	seen := map[string]bool{}
	for _, l := range s.Offer.Links {
		if !httpsURL(l) {
			add("offer.links: %q is not an absolute https:// URL with a host", l)
			continue
		}
		key := linkKey(l)
		if seen[key] {
			add("offer.links: %q is listed twice", l)
		}
		seen[key] = true
	}
	if len(s.Offer.VendorLinks) > 0 && !slices.Contains(s.Detectors, DetectorCafe) {
		add("offer.vendor_links is only for a campaign with the %s detector", DetectorCafe)
	}
	for _, id := range sortedKeys(s.Offer.VendorLinks) {
		if id != VendorLinkDefault {
			if v := cafe.DefaultConfig().Vendor(id); v == nil || v.Kind != cafe.SigLoyalty {
				add("offer.vendor_links: %q is not a known loyalty vendor id (or %q)", id, VendorLinkDefault)
			}
		}
		if l := s.Offer.VendorLinks[id]; !httpsURL(l) {
			add("offer.vendor_links.%s: %q is not an absolute https:// URL with a host", id, l)
		}
	}
	return errs
}

// httpsURL reports whether v is an absolute https URL with a host and no spaces.
func httpsURL(v string) bool {
	if v != strings.TrimSpace(v) || strings.ContainsAny(v, " \t\r\n") {
		return false
	}
	u, err := url.Parse(v)
	return err == nil && u.Scheme == "https" && u.Host != "" && u.Hostname() != "" && u.User == nil
}

// linkKey is the form two links are compared in (as outreach's URL check does): lower-case,
// trailing slashes trimmed.
func linkKey(v string) string { return strings.TrimRight(strings.ToLower(v), "/") }

// oneLine reports whether v is non-blank, holds no line break and has at most most runes.
func oneLine(v string, most int) bool {
	return strings.TrimSpace(v) != "" && !strings.ContainsAny(v, "\r\n") && utf8.RuneCountInString(v) <= most
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
