// Package leadconfig loads the leads policy, leads.yaml in LEADHOUND_DATA_DIR: which site
// lists and probes to run, the country policy, the sender, the copy rules, the schedule and
// the send caps, plus the message templates under templates_dir. The file is private; the
// repository ships a fictional one in examples/leads.yaml with examples/leads-templates/.
//
// Load reads and checks everything, templates included, for the commands that draft or send.
// LoadForProbe reads the same file but checks only the fields the site and contact commands
// use, so a data directory without templates still serves them.
package leadconfig

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"text/template"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/gremy/webbership-leadhound/internal/config"
)

// FileName is the policy's name inside the data directory.
const FileName = "leads.yaml"

// HardDailyCeiling is the most sends per mailbox per day; no config value lifts it.
const HardDailyCeiling = 40

// HardDomainDailyCeiling is the most sends per sending domain per day, over all of its
// mailboxes; no config value lifts it. Providers judge reputation by domain, so this is the
// real safety line.
const HardDomainDailyCeiling = 100

// HardMailboxesPerDomain is the most mailboxes the operator may add on one sending domain; no
// config value lifts it.
const HardMailboxesPerDomain = 5

// Defaults for the domain caps and the health rules when leads.yaml leaves them out (or
// writes 0).
const (
	DefaultDomainPerDay           = 50
	DefaultMailboxesPerDomain     = 2
	DefaultBouncePauseBounces     = 2
	DefaultBouncePauseWithinSends = 50
)

// Bounds of health.bounce_pause, inclusive.
const (
	MinBouncePauseBounces     = 1
	MaxBouncePauseBounces     = 10
	MinBouncePauseWithinSends = 10
	MaxBouncePauseWithinSends = 500
)

// DefaultDKIMSelector is sender.dkim_selector when the file leaves it out.
const DefaultDKIMSelector = "default"

// ErrInvalid wraps every problem with the file's content or its templates.
var ErrInvalid = errors.New("invalid leads config")

// Channels a country policy may name.
const (
	ChannelEmail = "email"
	ChannelForm  = "form"
	ChannelNone  = "none"
)

// Lead kinds kinds_priority may name; each has its own templates.
const (
	KindAgencyPartner = "agency_partner"
	KindUnsupported   = "unsupported"
	KindD10           = "d10"
)

// Kinds are the lead kinds that have templates.
var Kinds = []string{KindAgencyPartner, KindUnsupported, KindD10}

// Where batch new draws its leads from (batches_from).
const (
	BatchesStarred         = "starred"           // starred leads only; the default
	BatchesStarredThenRest = "starred_then_rest" // starred leads first, then the rest of the queue
)

// BatchSources are the values batches_from accepts; empty means BatchesStarred.
var BatchSources = []string{BatchesStarred, BatchesStarredThenRest}

// SlotNames are the values a template may use, as {{.Name}}. Footer holds the rendered
// footer, so a footer itself may use every slot but Footer. VendorName and VendorLink are
// optional: a café lead's loyalty vendor and the campaign's page for it, empty for any other
// lead, so a template tests them ({{if .VendorName}}…{{else}}…{{end}}).
var SlotNames = []string{
	"OrgName", "Domain", "Host", "Major", "Exact", "EvidenceLine", "FindingURL",
	"EOLDate", "SenderName", "IdentityLine", "OptOutLine", "Footer", "VendorName", "VendorLink",
}

// OptionalSlots are the slot names a template may use while empty: Render passes them
// even when empty, and Check does not call them missing facts.
var OptionalSlots = []string{"VendorName", "VendorLink"}

// Steps is the number of email steps in a sequence: the first email and two follow-ups.
const Steps = 3

// Config is the whole file, plus the templates Load parsed.
type Config struct {
	SiteLists            SiteLists          `yaml:"site_lists"`
	Probe                Probe              `yaml:"probe"`
	PublicBodySuffixes   []string           `yaml:"public_body_suffixes"`
	RoleLocalParts       []string           `yaml:"role_local_parts"`
	Countries            map[string]Country `yaml:"countries"` // ISO 3166-1 alpha-2 → policy
	AgencyFindingPattern string             `yaml:"agency_finding_pattern"`
	Sender               Sender             `yaml:"sender"`
	Facts                Facts              `yaml:"facts"`
	TemplatesDir         string             `yaml:"templates_dir"` // relative to the data dir
	KindsPriority        []string           `yaml:"kinds_priority"`
	KindsOff             []string           `yaml:"kinds_off"`      // kinds left out of the queue and of new batches
	SkipRankOver         int                `yaml:"skip_rank_over"` // 0 is off; else a site lead ranked above it, or unranked, leaves the queue
	BatchSource          string             `yaml:"batches_from"`   // BatchesFrom gives the value in force
	Relevance            Relevance          `yaml:"relevance"`
	Priority             *Profile           `yaml:"priority"` // nil: kinds_priority alone orders the leads
	Copy                 Copy               `yaml:"copy"`
	Schedule             Schedule           `yaml:"schedule"`
	Caps                 Caps               `yaml:"caps"`
	Health               Health             `yaml:"health"`

	// Templates is filled by Load; LoadForProbe leaves it empty.
	Templates Templates `yaml:"-"`

	agencyFinding *regexp.Regexp
	dataDir       string // set by Load, so a check of an override reads the templates again
}

// Relevance holds the Jev relevance rating of leads.
type Relevance struct {
	Rating          *bool `yaml:"rating"`            // nil (left out) is on; RatingOn gives the value in force
	LeadsDailyCalls int   `yaml:"leads_daily_calls"` // 0 uses the server's cap; DailyCalls gives the value in force
}

// RatingOn reports whether leads are rated for relevance; it is on unless the file says false.
func (r Relevance) RatingOn() bool { return r.Rating == nil || *r.Rating }

// DailyCalls returns the most relevance calls a day: leads_daily_calls, or envCap
// (LEADHOUND_JEV_LEADS_DAILY_CALL_CAP) when it is 0 or above envCap. A setting may lower the
// server's cap, never raise it.
func (r Relevance) DailyCalls(envCap int) int {
	if r.LeadsDailyCalls == 0 || r.LeadsDailyCalls > envCap {
		return envCap
	}
	return r.LeadsDailyCalls
}

// BatchesFrom returns batches_from in force: BatchesStarred when the file leaves it out.
func (cfg Config) BatchesFrom() string {
	if cfg.BatchSource == "" {
		return BatchesStarred
	}
	return cfg.BatchSource
}

// KindOff reports whether kinds_off turns a lead kind off.
func (cfg Config) KindOff(kind string) bool { return slices.Contains(cfg.KindsOff, kind) }

// Clone returns a deep copy of cfg: no slice or map of the copy is shared with cfg. Parsed
// templates and compiled patterns are shared, since nothing changes them after a load.
func (cfg Config) Clone() Config {
	c := cfg
	c.SiteLists.TLDs = slices.Clone(cfg.SiteLists.TLDs)
	c.PublicBodySuffixes = slices.Clone(cfg.PublicBodySuffixes)
	c.RoleLocalParts = slices.Clone(cfg.RoleLocalParts)
	c.Countries = maps.Clone(cfg.Countries)
	c.Sender.OperatorAddresses = slices.Clone(cfg.Sender.OperatorAddresses)
	c.Sender.OptOutLines = maps.Clone(cfg.Sender.OptOutLines)
	c.KindsPriority = slices.Clone(cfg.KindsPriority)
	c.KindsOff = slices.Clone(cfg.KindsOff)
	c.Probe.CafeOwnVendorHosts = slices.Clone(cfg.Probe.CafeOwnVendorHosts)
	if cfg.Relevance.Rating != nil {
		on := *cfg.Relevance.Rating
		c.Relevance.Rating = &on
	}
	if cfg.Priority != nil {
		p := cfg.Priority.Clone()
		c.Priority = &p
	}
	c.Copy.FirstWords = slices.Clone(cfg.Copy.FirstWords)
	c.Copy.FollowupWords = slices.Clone(cfg.Copy.FollowupWords)
	c.Copy.BannedWords = slices.Clone(cfg.Copy.BannedWords)
	c.Schedule.BusinessHours.Days = slices.Clone(cfg.Schedule.BusinessHours.Days)
	c.Schedule.FollowupBusinessDays = slices.Clone(cfg.Schedule.FollowupBusinessDays)
	c.Schedule.SpacingMinutes = slices.Clone(cfg.Schedule.SpacingMinutes)
	c.Caps.Ramp = slices.Clone(cfg.Caps.Ramp)
	if cfg.Templates.Sets != nil {
		c.Templates.Sets = make(map[SetKey]Set, len(cfg.Templates.Sets))
		for k, set := range cfg.Templates.Sets {
			set.Steps = slices.Clone(set.Steps)
			set.Email = maps.Clone(set.Email)
			c.Templates.Sets[k] = set
		}
	}
	c.Templates.Footers = maps.Clone(cfg.Templates.Footers)
	return c
}

// SiteLists filters imported site lists.
type SiteLists struct {
	TLDs []string `yaml:"tlds"` // registrable-domain suffixes kept on import
}

// Probe paces the Drupal version probe.
type Probe struct {
	ReprobeAfterDays int `yaml:"reprobe_after_days"`
	// CafeOwnVendorHosts are the hosts ("host" or "host/path-prefix") of the operator's own
	// loyalty product: a café whose homepage links or loads one is already a customer, and
	// the cafe@1 detector skips it (cafe.Config.WithOwnVendor). Private: they live in the
	// data dir's leads.yaml, never in the repository (the privacy check refuses the live
	// domain in tracked files). Empty: no café is recognised as a customer.
	CafeOwnVendorHosts []string `yaml:"cafe_own_vendor_hosts"`
}

// Country is one country's policy.
type Country struct {
	Channel string `yaml:"channel"` // email, form or none
	Lang    string `yaml:"lang"`    // ISO 639-1 code of the templates used
	TZ      string `yaml:"tz"`      // IANA zone for business hours
	Footer  string `yaml:"footer"`  // footers/<name>.txt; email countries need one

	loc *time.Location
}

// Location returns the country's time zone (UTC before the config is checked).
func (c Country) Location() *time.Location {
	if c.loc == nil {
		return time.UTC
	}
	return c.loc
}

// Sender is who the messages come from.
type Sender struct {
	Name              string          `yaml:"name"`
	IdentityLine      string          `yaml:"identity_line"`
	DKIMSelector      string          `yaml:"dkim_selector"`
	OperatorAddresses []config.Secret `yaml:"operator_addresses"`
	SiteURL           string          `yaml:"site_url"` // the one URL besides the finding's that a draft may hold
	// PrivacyURL is the page of the sender's privacy notice (optional, https). When set,
	// every email's footer ends with PrivacyLine, and Check allows this URL there only.
	PrivacyURL string `yaml:"privacy_url"`
	// OptOutLines maps a language to its opt-out line; every language an email country
	// uses needs one.
	OptOutLines map[string]string `yaml:"opt_out_lines"`
}

// PrivacyLinePrefix opens the footer's privacy line, before the notice's URL.
const PrivacyLinePrefix = "How I handle your details: "

// PrivacyLine is the footer line naming the privacy notice ("How I handle your details:
// <url>"); "" when no privacy_url is set.
func (s Sender) PrivacyLine() string {
	u := strings.TrimSpace(s.PrivacyURL)
	if u == "" {
		return ""
	}
	return PrivacyLinePrefix + u
}

// OptOutLine returns the opt-out line for a language.
func (s Sender) OptOutLine(lang string) (string, bool) {
	line, ok := s.OptOutLines[lang]
	return line, ok && strings.TrimSpace(line) != ""
}

// Facts are dates the templates may state.
type Facts struct {
	D6EOL  Date `yaml:"d6_eol"`
	D7EOL  Date `yaml:"d7_eol"`
	D8EOL  Date `yaml:"d8_eol"`
	D9EOL  Date `yaml:"d9_eol"`
	D10EOL Date `yaml:"d10_eol"`
}

// EOL returns the end-of-life date of a Drupal major version, for majors 6 to 10.
func (f Facts) EOL(major int) (Date, bool) {
	switch major {
	case 6:
		return f.D6EOL, true
	case 7:
		return f.D7EOL, true
	case 8:
		return f.D8EOL, true
	case 9:
		return f.D9EOL, true
	case 10:
		return f.D10EOL, true
	}
	return Date{}, false
}

// Copy holds the drafting rules. The word bounds count the rendered body above the footer.
type Copy struct {
	FirstWords    []int    `yaml:"first_words"`    // [min, max] words in the first email
	FollowupWords []int    `yaml:"followup_words"` // [min, max] words in a follow-up
	BannedWords   []string `yaml:"banned_words"`
}

// Schedule says when emails go out.
type Schedule struct {
	BusinessHours        BusinessHours `yaml:"business_hours"`
	FollowupBusinessDays []int         `yaml:"followup_business_days"` // step 1 after step 0, step 2 after step 1
	SpacingMinutes       []int         `yaml:"spacing_minutes"`        // [min, max] between two sends
}

// BusinessHours is the recipient-local sending window.
type BusinessHours struct {
	Start string   `yaml:"start"` // HH:MM
	End   string   `yaml:"end"`   // HH:MM, after Start
	Days  []string `yaml:"days"`  // Mon … Sun
}

// Caps limit sending.
type Caps struct {
	StartDate Date   `yaml:"start_date"`
	Ramp      []Ramp `yaml:"ramp"`
	BatchSize int    `yaml:"batch_size"`
	// DomainPerDay caps live sends per sending domain per day, over all of its mailboxes
	// (default DefaultDomainPerDay, at most HardDomainDailyCeiling).
	DomainPerDay int `yaml:"domain_per_day"`
	// MailboxesPerDomain caps how many mailboxes the operator may add on one sending domain
	// (default DefaultMailboxesPerDomain, at most HardMailboxesPerDomain).
	MailboxesPerDomain int `yaml:"mailboxes_per_domain"`
}

// Health holds the rules by which a mailbox pauses itself; only the operator resumes it.
type Health struct {
	BouncePause BouncePause `yaml:"bounce_pause"`
}

// BouncePause pauses a mailbox when Bounces hard bounces fall within its last WithinSends
// live sends (defaults DefaultBouncePauseBounces and DefaultBouncePauseWithinSends).
type BouncePause struct {
	Bounces     int `yaml:"bounces"`
	WithinSends int `yaml:"within_sends"`
}

// FirstSend is the caps.start_date word that makes day 0 of the ramp the day of the first
// live send, whenever that is, instead of a fixed date.
const FirstSend = "first-send"

// Ramp is the daily cap from FromDay days after StartDate onwards.
type Ramp struct {
	FromDay int `yaml:"from_day"`
	PerDay  int `yaml:"per_day"`
}

// Date is a calendar date written 2006-01-02. A value that does not parse stays zero and is
// reported by the check, with every other problem.
type Date struct {
	time.Time
	raw string
}

// ParseDate reads a date as the file does: it keeps the text, and a value that does not
// parse stays zero for the check to report.
func ParseDate(s string) Date {
	d := Date{raw: s}
	if t, err := time.Parse(time.DateOnly, s); err == nil {
		d.Time = t
	}
	return d
}

// UnmarshalYAML keeps the raw text and parses it; it never fails the decode.
func (d *Date) UnmarshalYAML(n *yaml.Node) error {
	*d = ParseDate(n.Value)
	return nil
}

// MarshalJSON writes the date as the file writes it (2006-01-02), so a saved override reads
// back the same.
func (d Date) MarshalJSON() ([]byte, error) {
	raw := d.raw
	if raw == "" && !d.IsZero() {
		raw = d.Format(time.DateOnly)
	}
	return json.Marshal(raw)
}

// UnmarshalJSON reads a JSON string as ParseDate does; only a value that is not a string
// fails the decode.
func (d *Date) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	*d = ParseDate(s)
	return nil
}

// IsFirstSend reports whether the date is the word FirstSend (only caps.start_date accepts
// it); its Time is then zero.
func (d Date) IsFirstSend() bool { return d.raw == FirstSend }

// Load reads FileName from dataDir and checks all of it, templates included.
func Load(dataDir string) (Config, error) {
	cfg, err := read(dataDir)
	if err != nil {
		return Config{}, err
	}
	cfg.dataDir = dataDir
	if problems := cfg.check(); len(problems) > 0 {
		return Config{}, invalid(problems)
	}
	return cfg, nil
}

// check runs every check Load runs, compiling and filling cfg in place: the content, then,
// for a config Load read, the templates the policy needs. Overrides.Validate runs it too, so
// the file and an override pass the same rules.
func (cfg *Config) check() []string {
	problems := cfg.checkProbe()
	problems = append(problems, cfg.checkRest()...)
	if cfg.dataDir != "" && cfg.TemplatesDir != "" && filepath.IsLocal(cfg.TemplatesDir) {
		// Which files are needed follows from the policy; entries already reported above
		// (an unknown kind, a bad language or footer name) are left out of that set.
		var tp []string
		cfg.Templates, tp = loadTemplates(filepath.Join(cfg.dataDir, cfg.TemplatesDir), cfg.requiredTemplates())
		problems = append(problems, tp...)
	}
	return problems
}

// LoadForProbe reads FileName from dataDir and checks only site_lists, probe,
// public_body_suffixes, role_local_parts, countries and agency_finding_pattern. The file must
// still decode whole (an unknown key is refused); templates are neither read nor required.
func LoadForProbe(dataDir string) (Config, error) {
	cfg, err := read(dataDir)
	if err != nil {
		return Config{}, err
	}
	if problems := cfg.checkProbe(); len(problems) > 0 {
		return Config{}, invalid(problems)
	}
	return cfg, nil
}

func invalid(problems []string) error {
	return fmt.Errorf("leadconfig: %s: %w: %s", FileName, ErrInvalid, strings.Join(problems, "; "))
}

// read decodes the file strictly: unknown keys are errors.
func read(dataDir string) (Config, error) {
	b, err := os.ReadFile(filepath.Join(dataDir, FileName)) // #nosec G304 -- the operator's own data directory
	if err != nil {
		return Config{}, fmt.Errorf("leadconfig: %w", err)
	}
	var cfg Config
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		if errors.Is(err, io.EOF) {
			return Config{}, invalid([]string{"the file is empty"})
		}
		return Config{}, invalid([]string{err.Error()})
	}
	if cfg.Sender.DKIMSelector == "" {
		cfg.Sender.DKIMSelector = DefaultDKIMSelector
	}
	defaultInt(&cfg.Caps.DomainPerDay, DefaultDomainPerDay)
	defaultInt(&cfg.Caps.MailboxesPerDomain, DefaultMailboxesPerDomain)
	defaultInt(&cfg.Health.BouncePause.Bounces, DefaultBouncePauseBounces)
	defaultInt(&cfg.Health.BouncePause.WithinSends, DefaultBouncePauseWithinSends)
	return cfg, nil
}

// defaultInt sets *v to def when the file left it out (or wrote 0).
func defaultInt(v *int, def int) {
	if *v == 0 {
		*v = def
	}
}

// AgencyFinding returns the compiled agency_finding_pattern (nil before a successful load).
func (cfg Config) AgencyFinding() *regexp.Regexp { return cfg.agencyFinding }

// Policy returns a country's policy; a country missing from the file has channel none.
func (cfg Config) Policy(country string) Country {
	if c, ok := cfg.Countries[country]; ok {
		return c
	}
	return Country{Channel: ChannelNone}
}

var (
	countryCode = regexp.MustCompile(`^[A-Z]{2}$`)
	langCode    = regexp.MustCompile(`^[a-z]{2}$`)
	dnsName     = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*$`)
	localPart   = regexp.MustCompile(`^[a-z0-9][a-z0-9._+-]*$`)
	weekdays    = []string{"Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"}
)

// FileNameRe is the plain file-name rule for a footer, kind, sequence or DKIM selector name:
// lower-case letters, digits, "_" and "-", starting with a letter or digit. campaign mirrors
// it for its countries' footers (a test compares the two).
var (
	FileNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)
)

// checkProbe checks the fields LoadForProbe needs, compiling the pattern and the zones.
func (cfg *Config) checkProbe() []string {
	var p []string
	if len(cfg.SiteLists.TLDs) == 0 {
		p = append(p, "site_lists.tlds is empty")
	}
	p = append(p, names("site_lists.tlds", cfg.SiteLists.TLDs, dnsName)...)
	if cfg.Probe.ReprobeAfterDays < 1 {
		p = append(p, "probe.reprobe_after_days must be at least 1")
	}
	for _, h := range cfg.Probe.CafeOwnVendorHosts {
		if !vendorHost.MatchString(h) {
			p = append(p, fmt.Sprintf("probe.cafe_own_vendor_hosts: %q is not host or host/path (lower case, no scheme)", h))
		}
	}
	p = append(p, names("public_body_suffixes", cfg.PublicBodySuffixes, dnsName)...)
	if len(cfg.RoleLocalParts) == 0 {
		p = append(p, "role_local_parts is empty")
	}
	p = append(p, names("role_local_parts", cfg.RoleLocalParts, localPart)...)

	if len(cfg.Countries) == 0 {
		p = append(p, "countries is empty")
	}
	for _, code := range sortedKeys(cfg.Countries) {
		c := cfg.Countries[code]
		field := "countries." + code
		if !countryCode.MatchString(code) {
			p = append(p, fmt.Sprintf("countries: %q is not an ISO 3166-1 alpha-2 code (two capital letters, e.g. GB)", code))
		}
		switch c.Channel {
		case ChannelEmail, ChannelForm:
			if !langCode.MatchString(c.Lang) {
				p = append(p, fmt.Sprintf("%s.lang: %q is not an ISO 639-1 code (two lower-case letters, e.g. en)", field, c.Lang))
			}
			if c.TZ == "" {
				p = append(p, field+".tz is missing")
			}
		case ChannelNone:
			if c.Lang != "" && !langCode.MatchString(c.Lang) {
				p = append(p, fmt.Sprintf("%s.lang: %q is not an ISO 639-1 code (two lower-case letters, e.g. en)", field, c.Lang))
			}
		default:
			p = append(p, fmt.Sprintf("%s.channel: %q is not one of email, form, none", field, c.Channel))
		}
		if c.TZ != "" {
			loc, err := time.LoadLocation(c.TZ)
			if err != nil {
				p = append(p, fmt.Sprintf("%s.tz: %q is not an IANA time zone such as Europe/London", field, c.TZ))
			}
			c.loc = loc
			cfg.Countries[code] = c
		}
	}

	if strings.TrimSpace(cfg.AgencyFindingPattern) == "" {
		p = append(p, "agency_finding_pattern is missing")
	} else if re, err := regexp.Compile(cfg.AgencyFindingPattern); err != nil {
		p = append(p, fmt.Sprintf("agency_finding_pattern does not compile: %v", err))
	} else {
		cfg.agencyFinding = re
	}
	return p
}

// vendorHost is one probe.cafe_own_vendor_hosts entry: a lower-case host with a dot, and an
// optional path prefix.
var vendorHost = regexp.MustCompile(`^[a-z0-9-]+(\.[a-z0-9-]+)+(/[a-z0-9._~-]+)*$`)

// checkRest checks every field outside checkProbe's set.
func (cfg *Config) checkRest() []string {
	var p []string
	s := cfg.Sender
	if strings.TrimSpace(s.Name) == "" {
		p = append(p, "sender.name is missing")
	}
	if strings.TrimSpace(s.IdentityLine) == "" {
		p = append(p, "sender.identity_line is missing")
	}
	if !FileNameRe.MatchString(s.DKIMSelector) {
		p = append(p, fmt.Sprintf("sender.dkim_selector: %q is not a DNS label", s.DKIMSelector))
	}
	for i, a := range s.OperatorAddresses {
		local, domain, ok := strings.Cut(a.Reveal(), "@")
		if !ok || local == "" || !strings.Contains(domain, ".") {
			// The value is a private address: the problem names its position only.
			p = append(p, fmt.Sprintf("sender.operator_addresses[%d] is not an email address", i))
		}
	}
	if s.SiteURL != "" {
		u, err := url.Parse(s.SiteURL)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			p = append(p, fmt.Sprintf("sender.site_url: %q is not an absolute http(s) URL", s.SiteURL))
		}
	}

	if s.PrivacyURL != "" {
		u, err := url.Parse(s.PrivacyURL)
		if err != nil || u.Scheme != "https" || u.Host == "" || strings.ContainsAny(s.PrivacyURL, " \t\r\n") {
			p = append(p, fmt.Sprintf("sender.privacy_url: %q is not an absolute https URL", s.PrivacyURL))
		}
	}

	p = append(p, cfg.Facts.D6EOL.check("facts.d6_eol")...)
	p = append(p, cfg.Facts.D7EOL.check("facts.d7_eol")...)
	p = append(p, cfg.Facts.D8EOL.check("facts.d8_eol")...)
	p = append(p, cfg.Facts.D9EOL.check("facts.d9_eol")...)
	p = append(p, cfg.Facts.D10EOL.check("facts.d10_eol")...)
	for _, lang := range sortedKeys(s.OptOutLines) {
		if !langCode.MatchString(lang) {
			p = append(p, fmt.Sprintf("sender.opt_out_lines: %q is not an ISO 639-1 code", lang))
		}
	}

	if cfg.TemplatesDir == "" {
		p = append(p, "templates_dir is missing")
	} else if !filepath.IsLocal(cfg.TemplatesDir) {
		p = append(p, fmt.Sprintf("templates_dir: %q must be a path inside the data dir", cfg.TemplatesDir))
	}

	if len(cfg.KindsPriority) == 0 {
		p = append(p, "kinds_priority is empty")
	}
	p = append(p, kindList("kinds_priority", cfg.KindsPriority)...)
	p = append(p, kindList("kinds_off", cfg.KindsOff)...)
	if cfg.SkipRankOver < 0 {
		p = append(p, fmt.Sprintf("skip_rank_over must be 0 (off) or more, not %d", cfg.SkipRankOver))
	}
	if cfg.BatchSource != "" && !slices.Contains(BatchSources, cfg.BatchSource) {
		p = append(p, fmt.Sprintf("batches_from: %q is not one of %s", cfg.BatchSource, strings.Join(BatchSources, ", ")))
	}
	if cfg.Relevance.LeadsDailyCalls < 0 {
		p = append(p, fmt.Sprintf("relevance.leads_daily_calls must be 0 (the server's cap) or more, not %d", cfg.Relevance.LeadsDailyCalls))
	}
	if cfg.Priority != nil {
		p = append(p, cfg.Priority.Problems()...)
	}
	for _, code := range sortedKeys(cfg.Countries) {
		c := cfg.Countries[code]
		if c.Channel == ChannelEmail && langCode.MatchString(c.Lang) {
			if _, ok := s.OptOutLine(c.Lang); !ok {
				p = append(p, fmt.Sprintf("sender.opt_out_lines.%s is missing (email country %s uses it)", c.Lang, code))
			}
		}
		if c.Channel == ChannelEmail && c.Footer == "" {
			p = append(p, "countries."+code+".footer is missing (an email country needs one)")
		}
		if c.Footer != "" && !FileNameRe.MatchString(c.Footer) {
			p = append(p, fmt.Sprintf("countries.%s.footer: %q is not a plain file name", code, c.Footer))
		}
	}

	p = append(p, pair("copy.first_words", cfg.Copy.FirstWords, 1)...)
	p = append(p, pair("copy.followup_words", cfg.Copy.FollowupWords, 1)...)
	for _, w := range cfg.Copy.BannedWords {
		if strings.TrimSpace(w) == "" {
			p = append(p, "copy.banned_words has a blank entry")
			break
		}
	}

	bh := cfg.Schedule.BusinessHours
	start, errS := time.Parse("15:04", bh.Start)
	end, errE := time.Parse("15:04", bh.End)
	if errS != nil || errE != nil || !end.After(start) {
		p = append(p, fmt.Sprintf("schedule.business_hours: start %q and end %q must be HH:MM with end after start", bh.Start, bh.End))
	}
	if len(bh.Days) == 0 {
		p = append(p, "schedule.business_hours.days is empty")
	}
	for i, d := range bh.Days {
		if !slices.Contains(weekdays, d) {
			p = append(p, fmt.Sprintf("schedule.business_hours.days: %q is not one of %s", d, strings.Join(weekdays, ", ")))
		} else if slices.Index(bh.Days, d) != i {
			p = append(p, fmt.Sprintf("schedule.business_hours.days: %q appears twice", d))
		}
	}
	fb := cfg.Schedule.FollowupBusinessDays
	if len(fb) != Steps-1 || slices.Min(append([]int{1}, fb...)) < 1 {
		p = append(p, fmt.Sprintf("schedule.followup_business_days must be %d numbers of 1 or more", Steps-1))
	}
	p = append(p, pair("schedule.spacing_minutes", cfg.Schedule.SpacingMinutes, 0)...)

	if !cfg.Caps.StartDate.IsFirstSend() {
		p = append(p, cfg.Caps.StartDate.check("caps.start_date", " or "+FirstSend)...)
	}
	if len(cfg.Caps.Ramp) == 0 {
		p = append(p, "caps.ramp is empty")
	}
	for i, r := range cfg.Caps.Ramp {
		switch {
		case i == 0 && r.FromDay != 0:
			p = append(p, "caps.ramp must start at from_day 0")
		case i > 0 && r.FromDay <= cfg.Caps.Ramp[i-1].FromDay:
			p = append(p, fmt.Sprintf("caps.ramp[%d].from_day must be after the step before it", i))
		}
		if r.PerDay < 1 || r.PerDay > HardDailyCeiling {
			p = append(p, fmt.Sprintf("caps.ramp[%d].per_day must be from 1 to %d (the ceiling per mailbox per day), not %d", i, HardDailyCeiling, r.PerDay))
		}
	}
	if cfg.Caps.BatchSize < 1 {
		p = append(p, "caps.batch_size must be at least 1")
	}
	p = append(p, bounded("caps.domain_per_day", cfg.Caps.DomainPerDay, 1, HardDomainDailyCeiling, " (the ceiling per sending domain per day)")...)
	p = append(p, bounded("caps.mailboxes_per_domain", cfg.Caps.MailboxesPerDomain, 1, HardMailboxesPerDomain, " (the ceiling of mailboxes per sending domain)")...)
	bp := cfg.Health.BouncePause
	p = append(p, bounded("health.bounce_pause.bounces", bp.Bounces, MinBouncePauseBounces, MaxBouncePauseBounces, "")...)
	p = append(p, bounded("health.bounce_pause.within_sends", bp.WithinSends, MinBouncePauseWithinSends, MaxBouncePauseWithinSends, "")...)
	return p
}

// check reports a missing or unparsed date; or names what else the field accepts.
func (d Date) check(field string, or ...string) []string {
	switch {
	case d.raw == "":
		return []string{field + " is missing"}
	case d.IsZero():
		return []string{fmt.Sprintf("%s: %q is not a date like 2026-01-31%s", field, d.raw, strings.Join(or, ""))}
	}
	return nil
}

// kindList reports entries of a list of lead kinds that are not one of Kinds, or that appear
// twice.
func kindList(field string, list []string) []string {
	var p []string
	for i, k := range list {
		if !slices.Contains(Kinds, k) {
			p = append(p, fmt.Sprintf("%s: %q is not one of %s", field, k, strings.Join(Kinds, ", ")))
		} else if slices.Index(list, k) != i {
			p = append(p, fmt.Sprintf("%s: %q appears twice", field, k))
		}
	}
	return p
}

// bounded reports v outside lo..hi; note says what hi is, when it is a ceiling.
func bounded(field string, v, lo, hi int, note string) []string {
	if v < lo || v > hi {
		return []string{fmt.Sprintf("%s must be from %d to %d%s, not %d", field, lo, hi, note, v)}
	}
	return nil
}

// pair checks a [min, max] list whose min is at least lo.
func pair(field string, v []int, lo int) []string {
	if len(v) != 2 || v[0] < lo || v[1] < v[0] {
		return []string{fmt.Sprintf("%s must be [min, max] with %d <= min <= max", field, lo)}
	}
	return nil
}

// names reports entries of a list that do not match re (lower-case names, no blanks).
func names(field string, list []string, re *regexp.Regexp) []string {
	var p []string
	for _, s := range list {
		if !re.MatchString(s) {
			p = append(p, fmt.Sprintf("%s: %q is not a lower-case name", field, s))
		}
	}
	return p
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// SetKey names one kind's templates in one language.
type SetKey struct {
	Kind, Lang string
}

// Email is one email step: its subject line and body.
type Email struct {
	Subject, Body *template.Template
}

// Set is one kind's templates in one language. Steps is empty when the language is used by
// form countries only. A campaign's own templates (LoadCampaignTemplates) fill Email, by step
// number, and Call instead of Steps, Form and LinkedIn.
type Set struct {
	Steps          []Email
	Form, LinkedIn *template.Template
	Email          map[int]Email      // a campaign's email steps by step number; nil for leads.yaml's
	Call           *template.Template // a campaign's call.txt: three talking points, one per line
	// Named are a campaign's named sequences' email steps (call_first), by sequence name and
	// step number, read from <kind>/<lang>/<name>/<step>.txt: their step numbers overlap the
	// default sequence's, so they never share its files.
	Named map[string]map[int]Email
}

// SequenceEmail is the template of email step in sequence name ("" or "default": the
// default sequence's Email).
func (s Set) SequenceEmail(name string, step int) (Email, bool) {
	if name == "" || name == "default" {
		e, ok := s.Email[step]
		return e, ok
	}
	e, ok := s.Named[name][step]
	return e, ok
}

// Templates are the parsed templates. Every template is parsed with missingkey=error and
// renders from a map of SlotNames.
type Templates struct {
	Sets    map[SetKey]Set
	Footers map[string]*template.Template
	// SHA256 is the hex digest over every file read, path and content, in path order.
	SHA256 string
}

// Set returns the templates for a kind and language.
func (t Templates) Set(kind, lang string) (Set, bool) {
	s, ok := t.Sets[SetKey{kind, lang}]
	return s, ok
}

// required lists the templates the policy needs.
type required struct {
	email   map[SetKey]bool // the three email steps
	form    map[SetKey]bool // form.txt and linkedin.txt
	footers []string
}

// requiredTemplates derives which files Load demands from the policy. For every kind in
// kinds_priority and every language a country with channel email or form uses:
//   - an email country's language needs <kind>/<lang>/{0,1,2}.txt;
//   - an email or form country's language needs <kind>/<lang>/form.txt and linkedin.txt,
//     because an email lead without a usable address falls back to the form route;
//   - every footer an email country names needs footers/<name>.txt.
//
// A country with channel none needs nothing, and files beyond these are ignored.
func (cfg Config) requiredTemplates() required {
	r := required{email: map[SetKey]bool{}, form: map[SetKey]bool{}}
	for _, code := range sortedKeys(cfg.Countries) {
		c := cfg.Countries[code]
		if (c.Channel != ChannelEmail && c.Channel != ChannelForm) || !langCode.MatchString(c.Lang) {
			continue
		}
		for _, kind := range cfg.KindsPriority {
			if !slices.Contains(Kinds, kind) {
				continue
			}
			key := SetKey{kind, c.Lang}
			r.form[key] = true
			if c.Channel == ChannelEmail {
				r.email[key] = true
			}
		}
		if c.Channel == ChannelEmail && FileNameRe.MatchString(c.Footer) && !slices.Contains(r.footers, c.Footer) {
			r.footers = append(r.footers, c.Footer)
		}
	}
	return r
}

// loadTemplates reads, parses and test-renders every required template, reporting each
// missing, unparsable or unrenderable file.
func loadTemplates(dir string, req required) (Templates, []string) {
	var p []string
	files := map[string][]byte{}
	readT := func(rel string) (string, bool) {
		b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel))) // #nosec G304 -- the operator's own data directory; rel is built from checked names
		switch {
		case errors.Is(err, os.ErrNotExist):
			p = append(p, "template "+rel+" is missing")
			return "", false
		case err != nil:
			p = append(p, fmt.Sprintf("template %s: %v", rel, err))
			return "", false
		}
		files[rel] = b
		if strings.TrimSpace(string(b)) == "" {
			p = append(p, "template "+rel+" is empty")
			return "", false
		}
		return string(b), true
	}
	parse := func(rel, text string, slots map[string]string) *template.Template {
		t, err := template.New(rel).Option("missingkey=error").Parse(text)
		if err != nil {
			p = append(p, fmt.Sprintf("template %s does not parse: %v", rel, err))
			return nil
		}
		if err := t.Execute(io.Discard, slots); err != nil {
			p = append(p, fmt.Sprintf("template %s does not render: %v (slots: %s)", rel, err, strings.Join(slotsOf(slots), ", ")))
			return nil
		}
		return t
	}

	all := sampleSlots(true)
	noFooter := sampleSlots(false)
	t := Templates{Sets: map[SetKey]Set{}, Footers: map[string]*template.Template{}}
	keys := make([]SetKey, 0, len(req.form))
	for k := range req.form {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].Kind != keys[j].Kind {
			return keys[i].Kind < keys[j].Kind
		}
		return keys[i].Lang < keys[j].Lang
	})
	for _, key := range keys {
		base := key.Kind + "/" + key.Lang + "/"
		var set Set
		if req.email[key] {
			for step := range Steps {
				rel := fmt.Sprintf("%s%d.txt", base, step)
				text, ok := readT(rel)
				if !ok {
					continue
				}
				subject, body, err := splitEmail(text)
				if err != nil {
					p = append(p, fmt.Sprintf("template %s: %v", rel, err))
					continue
				}
				set.Steps = append(set.Steps, Email{
					Subject: parse(rel, subject, all),
					Body:    parse(rel, body, all),
				})
			}
		}
		if text, ok := readT(base + "form.txt"); ok {
			set.Form = parse(base+"form.txt", text, all)
		}
		if text, ok := readT(base + "linkedin.txt"); ok {
			set.LinkedIn = parse(base+"linkedin.txt", text, all)
		}
		t.Sets[key] = set
	}
	for _, name := range req.footers {
		rel := "footers/" + name + ".txt"
		if text, ok := readT(rel); ok {
			t.Footers[name] = parse(rel, text, noFooter)
		}
	}
	if len(p) > 0 {
		return Templates{}, p
	}
	t.SHA256 = digest(files)
	return t, nil
}

// LoadFooters reads, parses and test-renders footers/<name>.txt for each name from the
// templates dir of a config Load read (leads.yaml's footers dir), as Load does for the
// footers its countries name: a campaign country's own footer is loaded alongside them.
// Every missing, unparsable or unrenderable file is reported at once.
func (cfg Config) LoadFooters(names []string) (map[string]*template.Template, error) {
	if len(names) == 0 {
		return map[string]*template.Template{}, nil
	}
	if cfg.dataDir == "" || cfg.TemplatesDir == "" || !filepath.IsLocal(cfg.TemplatesDir) {
		return nil, fmt.Errorf("leadconfig: footers %s: %w: the config has no templates dir to read them from", strings.Join(names, ", "), ErrInvalid)
	}
	var p []string
	var req required
	for _, n := range names {
		switch {
		case !FileNameRe.MatchString(n):
			p = append(p, fmt.Sprintf("footer %q is not a plain file name", n))
		case !slices.Contains(req.footers, n):
			req.footers = append(req.footers, n)
		}
	}
	if len(p) == 0 {
		var t Templates
		t, p = loadTemplates(filepath.Join(cfg.dataDir, cfg.TemplatesDir), req)
		if len(p) == 0 {
			return t.Footers, nil
		}
	}
	return nil, fmt.Errorf("leadconfig: footers: %w: %s", ErrInvalid, strings.Join(p, "; "))
}

// splitEmail splits an email template into its subject (the first line, "Subject: …") and
// its body (after one blank line).
func splitEmail(text string) (subject, body string, err error) {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	first, rest, _ := strings.Cut(text, "\n")
	subject, ok := strings.CutPrefix(first, "Subject: ")
	if !ok || strings.TrimSpace(subject) == "" {
		return "", "", errors.New(`the first line must be "Subject: …"`)
	}
	blank, body, _ := strings.Cut(rest, "\n")
	if strings.TrimSpace(blank) != "" {
		return "", "", errors.New("the subject line must be followed by a blank line")
	}
	if strings.TrimSpace(body) == "" {
		return "", "", errors.New("the body is empty")
	}
	return strings.TrimSpace(subject), body, nil
}

// sampleSlots returns a value for every slot name, Footer included or not.
func sampleSlots(withFooter bool) map[string]string {
	m := map[string]string{}
	for _, n := range SlotNames {
		if n == "Footer" && !withFooter {
			continue
		}
		m[n] = "x"
	}
	return m
}

func slotsOf(m map[string]string) []string {
	return sortedKeys(m)
}

func digest(files map[string][]byte) string {
	h := sha256.New()
	for _, rel := range sortedKeys(files) {
		h.Write([]byte(rel))
		h.Write([]byte{0})
		h.Write(files[rel])
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// CallFile is a campaign's call card template; CallPoints is how many lines it holds: one
// talking point each.
const (
	CallFile   = "call.txt"
	CallPoints = 3
)

// CampaignTemplates names what a campaign's template folder must hold: for every kind and
// language, <kind>/<lang>/<step>.txt for each email step of its sequence and, when the
// sequence has a call step, call.txt. The footers, contact-form and LinkedIn texts stay
// leads.yaml's.
type CampaignTemplates struct {
	Kinds      []string
	Langs      []string
	EmailSteps []int // the sequence's email steps, in order
	Call       bool  // the sequence has a call step
	// Named are the email steps of each named sequence (call_first), read from
	// <kind>/<lang>/<name>/<step>.txt.
	Named map[string][]int
}

// LoadCampaignTemplates reads, parses and test-renders a campaign's templates from dir, as
// req names them, reporting every missing, unparsable or unrenderable file at once. call.txt
// must hold exactly CallPoints non-blank lines. Each Set has Email (by step number) and Call
// filled; Footers is empty. SHA256 is the digest over every file read.
func LoadCampaignTemplates(dir string, req CampaignTemplates) (Templates, error) {
	var p []string
	files := map[string][]byte{}
	readT := func(rel string) (string, bool) {
		b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel))) // #nosec G304 -- the operator's own data directory; rel is built from checked names
		switch {
		case errors.Is(err, os.ErrNotExist):
			p = append(p, "template "+rel+" is missing")
			return "", false
		case err != nil:
			p = append(p, fmt.Sprintf("template %s: %v", rel, err))
			return "", false
		}
		files[rel] = b
		if strings.TrimSpace(string(b)) == "" {
			p = append(p, "template "+rel+" is empty")
			return "", false
		}
		return string(b), true
	}
	all := sampleSlots(true)
	parse := func(rel, text string) *template.Template {
		t, err := template.New(rel).Option("missingkey=error").Parse(text)
		if err != nil {
			p = append(p, fmt.Sprintf("template %s does not parse: %v", rel, err))
			return nil
		}
		if err := t.Execute(io.Discard, all); err != nil {
			p = append(p, fmt.Sprintf("template %s does not render: %v (slots: %s)", rel, err, strings.Join(slotsOf(all), ", ")))
			return nil
		}
		return t
	}

	t := Templates{Sets: map[SetKey]Set{}, Footers: map[string]*template.Template{}}
	kinds, langs := slices.Clone(req.Kinds), slices.Clone(req.Langs)
	slices.Sort(kinds)
	slices.Sort(langs)
	for _, kind := range slices.Compact(kinds) {
		if !FileNameRe.MatchString(kind) {
			p = append(p, fmt.Sprintf("kind %q is not a folder name", kind))
			continue
		}
		for _, lang := range slices.Compact(langs) {
			if !langCode.MatchString(lang) {
				p = append(p, fmt.Sprintf("language %q is not a two-letter code", lang))
				continue
			}
			base := kind + "/" + lang + "/"
			set := Set{Email: map[int]Email{}}
			readSteps := func(dir string, steps []int, into map[int]Email) {
				for _, step := range steps {
					rel := fmt.Sprintf("%s%d.txt", dir, step)
					text, ok := readT(rel)
					if !ok {
						continue
					}
					subject, body, err := splitEmail(text)
					if err != nil {
						p = append(p, fmt.Sprintf("template %s: %v", rel, err))
						continue
					}
					into[step] = Email{Subject: parse(rel, subject), Body: parse(rel, body)}
				}
			}
			readSteps(base, req.EmailSteps, set.Email)
			for _, name := range slices.Sorted(maps.Keys(req.Named)) {
				if !FileNameRe.MatchString(name) {
					p = append(p, fmt.Sprintf("sequence %q is not a folder name", name))
					continue
				}
				if set.Named == nil {
					set.Named = map[string]map[int]Email{}
				}
				set.Named[name] = map[int]Email{}
				readSteps(base+name+"/", req.Named[name], set.Named[name])
			}
			if req.Call {
				rel := base + CallFile
				if text, ok := readT(rel); ok {
					raw := strings.Split(strings.TrimSpace(strings.ReplaceAll(text, "\r\n", "\n")), "\n")
					if n := len(callLines(text)); n != CallPoints || len(raw) != CallPoints {
						p = append(p, fmt.Sprintf("template %s has %d lines; it must hold exactly %d talking points, one per line, no blank line between", rel, len(raw), CallPoints))
					} else {
						set.Call = parse(rel, text)
					}
				}
			}
			t.Sets[SetKey{kind, lang}] = set
		}
	}
	if len(p) > 0 {
		return Templates{}, fmt.Errorf("leadconfig: templates in %s: %w: %s", dir, ErrInvalid, strings.Join(p, "; "))
	}
	t.SHA256 = digest(files)
	return t, nil
}

// callLines is a call card's lines, line ends normalised and blank lines left out.
func callLines(text string) []string {
	var out []string
	for _, l := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

// CallLines splits a rendered call card into its talking points, one per non-blank line.
func CallLines(text string) []string { return callLines(text) }
