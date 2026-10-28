// Package compliance holds the country rules for cold email and calls (countries.yaml,
// embedded) and answers whether a channel is allowed for one lead.
//
// The table is the plan's own research of 27 Oct 2026, not legal advice. Every answer
// separates two kinds of "no":
//
//   - hard: the law table refuses it (a country marked off, a hold country no campaign has
//     acknowledged, a blocked US state, an unknown region, outside the calling hours, a
//     sole trader where individuals need consent, an Irish mobile). Nothing the operator
//     chooses per lead makes it allowed.
//   - soft: every legal rule passes, but the campaign's own country rules have the channel
//     off. The caller decides what that means (for calls: warn and let the operator choose,
//     the 28 Oct operator decision).
//
// Address-shape conditions (published_address, role_address_only) are reported as the rule
// of an allowed answer; checking the address itself is the caller's job.
package compliance

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
	_ "time/tzdata" // the zones resolve on a server image with no zoneinfo

	"go.yaml.in/yaml/v3"
)

//go:embed countries.yaml
var countriesYAML []byte

// Channel is a way of reaching a lead.
type Channel string

// The channels the table rules on.
const (
	ChannelEmail Channel = "email"
	ChannelCall  Channel = "call"
)

// Verdict words (the closed set a channel rule takes in countries.yaml).
const (
	VerdictOpen             = "open"
	VerdictPublishedAddress = "published_address"
	VerdictRoleAddressOnly  = "role_address_only"
	VerdictConsent          = "consent"
	VerdictOff              = "off"
	VerdictHold             = "hold"
)

// Individuals words: how a sole trader (a natural person) is treated on a channel.
const (
	IndividualsSame    = "same"
	IndividualsConsent = "consent"
	IndividualsOff     = "off"
)

// Confidence words.
const (
	ConfidenceVerified         = "verified"
	ConfidenceSecondary        = "secondary"
	ConfidenceUnverified       = "unverified"
	ConfidenceOperatorDecision = "operator_decision"
)

// Entity types (the org's entity_type fact).
const (
	EntityCompany    = "company"
	EntitySoleTrader = "sole_trader"
	EntityUnknown    = "unknown"
)

// Rule codes: the stable machine word Verdict returns beside its plain reason. An allowed
// answer's rule is the verdict it rests on (open, published_address, role_address_only,
// consent, hold_acknowledged).
const (
	RuleOpen             = VerdictOpen
	RulePublishedAddress = VerdictPublishedAddress
	RuleRoleAddressOnly  = VerdictRoleAddressOnly
	RuleConsent          = VerdictConsent         // allowed with recorded consent, or refused for want of it
	RuleOff              = VerdictOff             // hard: the table marks the country off
	RuleHold             = VerdictHold            // hard: a hold country with no campaign acknowledge
	RuleHoldAcknowledged = "hold_acknowledged"    // allowed: a hold country the campaign acknowledged
	RuleAcknowledge      = "acknowledge_required" // hard: the country needs the campaign's written reason
	RuleUnlisted         = "unlisted"             // hard: the country is not in the table
	RuleIndividual       = "individual"           // hard: a sole trader or unknown entity where individuals are off or need consent
	RuleRegionUnknown    = "region_unknown"       // hard: US, CA, AU need the lead's region
	RuleRegionBlocked    = "region_blocked"       // hard: the region's calls are off (US states)
	RuleMobile           = "mobile"               // hard: landlines only (IE)
	RuleWindow           = "window"               // hard, temporary: outside the calling hours now
	RuleCampaignOff      = "campaign_off"         // soft: the campaign's own rules have the channel off
)

// FreshnessDays is how old the research may get before Freshness warns.
const FreshnessDays = 180

var (
	verdicts     = []string{VerdictOpen, VerdictPublishedAddress, VerdictRoleAddressOnly, VerdictConsent, VerdictOff, VerdictHold}
	individuals  = []string{IndividualsSame, IndividualsConsent, IndividualsOff}
	confidences  = []string{ConfidenceVerified, ConfidenceSecondary, ConfidenceUnverified, ConfidenceOperatorDecision}
	groups       = []string{"both_open", "call_first", "off", "operator_opened", "call_first_operator"}
	dayNames     = map[string]time.Weekday{"Sun": time.Sunday, "Mon": time.Monday, "Tue": time.Tuesday, "Wed": time.Wednesday, "Thu": time.Thursday, "Fri": time.Friday, "Sat": time.Saturday}
	countryRe    = regexp.MustCompile(`^[A-Z]{2}$`)
	regionRe     = regexp.MustCompile(`^[A-Z]{2,3}$`)
	registerRe   = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	clockRe      = regexp.MustCompile(`^([01][0-9]|2[0-3]):[0-5][0-9]$`)
	errNoCountry = errors.New("no countries")
)

// Table is the loaded country table.
type Table struct {
	ResearchDate       time.Time
	DefaultCallWindows []Window
	Registers          map[string]Register
	SoleTraderWords    []string
	Countries          map[string]Country

	soleTrader []*regexp.Regexp
	forms      map[string][]*regexp.Regexp // country → its legal-form patterns
	allForms   []*regexp.Regexp
}

// Country is one country's entry.
type Country struct {
	Code           string
	Name           string
	Group          string
	TZ             string
	RegionRequired bool
	LowConfidence  bool
	Email          ChannelRule
	Call           ChannelRule
	Individuals    Individuals
	Registers      []string
	LegalForms     []string
	CompanyNumber  bool // the GB company-number pattern marks a company
	Sources        []Source
	Confidence     string
	Acknowledge    string // the operator's reason, for operator_decision entries
	Regions        map[string]Region

	loc *time.Location
}

// ChannelRule is one channel's verdict and its concrete rules.
type ChannelRule struct {
	Verdict             string   `yaml:"verdict"`
	Rules               []string `yaml:"rules"`
	Windows             []Window `yaml:"windows"`
	LandlineOnly        bool     `yaml:"landline_only"`
	AcknowledgeRequired bool     `yaml:"acknowledge_required"`
}

// Individuals is how a natural person (sole trader) is treated per channel.
type Individuals struct {
	Email string `yaml:"email"`
	Call  string `yaml:"call"`
}

// Window is a span of calling hours on the listed days, in the callee's time.
type Window struct {
	Days  []string `yaml:"days"`
	Start string   `yaml:"start"`
	End   string   `yaml:"end"`

	days       []time.Weekday
	start, end int // minutes after midnight
}

// Register is a do-not-call register a number is screened against.
type Register struct {
	Code       string
	Country    string `yaml:"country"`
	Name       string `yaml:"name"`
	MaxAgeDays int    `yaml:"max_age_days"` // 0: the research named no age; screen before each campaign
}

// Source is where an entry's research came from. URL is empty where the research named none.
type Source struct {
	Title string `yaml:"title"`
	URL   string `yaml:"url"`
}

// Region is a state, province or territory (US, CA, AU).
type Region struct {
	TZ     string   `yaml:"tz"`
	TZAlso []string `yaml:"tz_also"` // other zones the region spans; a call must be in hours in all
	Call   string   `yaml:"call"`    // "" or off
	Reason string   `yaml:"reason"`

	locs []*time.Location
}

// OrgFacts are the lead's facts a verdict reads.
type OrgFacts struct {
	EntityType   string // company | sole_trader | unknown ("" is unknown, treated as an individual)
	Region       string // state/province/territory code (FL, ON, NSW; "US-FL" works too)
	PhoneType    string // landline | mobile | "" (unknown); see PhoneType
	EmailConsent bool   // a recorded "yes, email me"
}

// CampaignCountry is one country's rules in a campaign: each channel on or off, and the
// written acknowledge reason that enables a hold country (or a country that needs one).
type CampaignCountry struct {
	Email       bool
	Call        bool
	Acknowledge string
}

// CampaignOverrides are a campaign's country rules. A nil Countries map means no campaign
// rules apply (no soft verdict). A non-nil map lists the countries the campaign turns on; a
// country missing from it has both channels off, as in the campaign spec.
type CampaignOverrides struct {
	Countries map[string]CampaignCountry
}

type fileShape struct {
	ResearchDate       string              `yaml:"research_date"`
	DefaultCallWindows []Window            `yaml:"default_call_windows"`
	Registers          map[string]Register `yaml:"registers"`
	SoleTraderWords    []string            `yaml:"sole_trader_words"`
	Countries          map[string]struct {
		Name           string            `yaml:"name"`
		Group          string            `yaml:"group"`
		TZ             string            `yaml:"tz"`
		RegionRequired bool              `yaml:"region_required"`
		LowConfidence  bool              `yaml:"low_confidence"`
		Email          ChannelRule       `yaml:"email"`
		Call           ChannelRule       `yaml:"call"`
		Individuals    Individuals       `yaml:"individuals"`
		Registers      []string          `yaml:"registers"`
		LegalForms     []string          `yaml:"legal_forms"`
		CompanyNumber  bool              `yaml:"company_number"`
		Sources        []Source          `yaml:"sources"`
		Confidence     string            `yaml:"confidence"`
		Acknowledge    string            `yaml:"acknowledge"`
		Regions        map[string]Region `yaml:"regions"`
	} `yaml:"countries"`
}

var defaultTable = func() *Table {
	t, err := Parse(countriesYAML)
	if err != nil {
		panic("compliance: embedded countries.yaml: " + err.Error())
	}
	return t
}()

// Default is the embedded table.
func Default() *Table { return defaultTable }

// Parse loads a table, enforcing every closed vocabulary; any fault fails the whole load.
func Parse(data []byte) (*Table, error) {
	var f fileShape
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("countries: %w", err)
	}
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	t := &Table{Registers: map[string]Register{}, Countries: map[string]Country{}, forms: map[string][]*regexp.Regexp{}}
	d, err := time.Parse(time.DateOnly, f.ResearchDate)
	if err != nil {
		add("research_date %q is not a YYYY-MM-DD date", f.ResearchDate)
	}
	t.ResearchDate = d

	t.DefaultCallWindows = checkWindows("default_call_windows", f.DefaultCallWindows, add)
	if len(t.DefaultCallWindows) == 0 {
		add("default_call_windows: at least one window is needed")
	}
	for code, r := range f.Registers {
		if !registerRe.MatchString(code) {
			add("registers: %q is not a lower-case register code", code)
		}
		if !countryRe.MatchString(r.Country) {
			add("registers.%s.country %q is not an ISO 3166-1 alpha-2 code", code, r.Country)
		}
		if r.MaxAgeDays < 0 {
			add("registers.%s.max_age_days must not be negative", code)
		}
		r.Code = code
		t.Registers[code] = r
	}
	t.SoleTraderWords = f.SoleTraderWords
	for _, w := range f.SoleTraderWords {
		t.soleTrader = append(t.soleTrader, formPattern(w))
	}
	if len(f.Countries) == 0 {
		errs = append(errs, errNoCountry)
	}
	for code, fc := range f.Countries {
		p := "countries." + code
		if !countryRe.MatchString(code) {
			add("countries: %q is not an ISO 3166-1 alpha-2 code", code)
		}
		if fc.Name == "" {
			add("%s.name is missing", p)
		}
		if !slices.Contains(groups, fc.Group) {
			add("%s.group %q is not one of %s", p, fc.Group, strings.Join(groups, ", "))
		}
		c := Country{
			Code: code, Name: fc.Name, Group: fc.Group, TZ: fc.TZ, RegionRequired: fc.RegionRequired,
			LowConfidence: fc.LowConfidence, Email: fc.Email, Call: fc.Call, Individuals: fc.Individuals,
			Registers: fc.Registers, LegalForms: fc.LegalForms, CompanyNumber: fc.CompanyNumber,
			Sources: fc.Sources, Confidence: fc.Confidence, Acknowledge: fc.Acknowledge, Regions: map[string]Region{},
		}
		for _, ch := range []struct {
			name string
			r    *ChannelRule
		}{{"email", &c.Email}, {"call", &c.Call}} {
			if !slices.Contains(verdicts, ch.r.Verdict) {
				add("%s.%s.verdict %q is not one of %s", p, ch.name, ch.r.Verdict, strings.Join(verdicts, ", "))
			}
			ch.r.Windows = checkWindows(p+"."+ch.name+".windows", ch.r.Windows, add)
		}
		if len(c.Email.Windows) > 0 || c.Email.LandlineOnly {
			add("%s.email: windows and landline_only are call rules", p)
		}
		if !slices.Contains(individuals, c.Individuals.Email) || !slices.Contains(individuals, c.Individuals.Call) {
			add("%s.individuals: email and call must each be one of %s", p, strings.Join(individuals, ", "))
		}
		if !slices.Contains(confidences, c.Confidence) {
			add("%s.confidence %q is not one of %s", p, c.Confidence, strings.Join(confidences, ", "))
		}
		if c.Confidence == ConfidenceOperatorDecision && strings.TrimSpace(c.Acknowledge) == "" {
			add("%s: an operator_decision entry needs its acknowledge reason", p)
		}
		if c.Confidence == ConfidenceVerified && len(c.Sources) == 0 {
			add("%s: a verified entry needs its sources", p)
		}
		for i, s := range c.Sources {
			if strings.TrimSpace(s.Title) == "" {
				add("%s.sources[%d].title is missing", p, i)
			}
			if s.URL != "" {
				if u, err := url.Parse(s.URL); err != nil || u.Scheme != "https" || u.Host == "" {
					add("%s.sources[%d].url %q is not an https URL", p, i, s.URL)
				}
			}
		}
		for _, r := range c.Registers {
			if _, ok := f.Registers[r]; !ok {
				add("%s.registers: %q is not a register in the file", p, r)
			}
		}
		if c.RegionRequired {
			if len(fc.Regions) == 0 {
				add("%s: region_required needs regions", p)
			}
			if c.TZ != "" {
				add("%s: a region_required country takes its zones from its regions, not tz", p)
			}
		} else {
			loc, err := time.LoadLocation(c.TZ)
			if c.TZ == "" || err != nil {
				add("%s.tz %q is not an IANA time zone", p, c.TZ)
			}
			c.loc = loc
		}
		for rc, r := range fc.Regions {
			if !regionRe.MatchString(rc) {
				add("%s.regions: %q is not an upper-case region code", p, rc)
			}
			for _, z := range append([]string{r.TZ}, r.TZAlso...) {
				loc, err := time.LoadLocation(z)
				if z == "" || err != nil {
					add("%s.regions.%s: %q is not an IANA time zone", p, rc, z)
					continue
				}
				r.locs = append(r.locs, loc)
			}
			if r.Call != "" && r.Call != VerdictOff {
				add("%s.regions.%s.call %q must be off or left out", p, rc, r.Call)
			}
			if r.Call == VerdictOff && strings.TrimSpace(r.Reason) == "" {
				add("%s.regions.%s: a region with calls off needs its reason", p, rc)
			}
			c.Regions[rc] = r
		}
		for _, lf := range c.LegalForms {
			re := formPattern(lf)
			t.forms[code] = append(t.forms[code], re)
			t.allForms = append(t.allForms, re)
		}
		t.Countries[code] = c
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return t, nil
}

func checkWindows(p string, ws []Window, add func(string, ...any)) []Window {
	for i := range ws {
		w := &ws[i]
		if len(w.Days) == 0 {
			add("%s[%d]: days are missing", p, i)
		}
		for _, d := range w.Days {
			wd, ok := dayNames[d]
			if !ok {
				add("%s[%d]: %q is not a day (Mon..Sun)", p, i, d)
				continue
			}
			w.days = append(w.days, wd)
		}
		if !clockRe.MatchString(w.Start) || !clockRe.MatchString(w.End) {
			add("%s[%d]: start and end must be HH:MM", p, i)
			continue
		}
		w.start, w.end = minutes(w.Start), minutes(w.End)
		if w.end <= w.start {
			add("%s[%d]: end %s is not after start %s", p, i, w.End, w.Start)
		}
	}
	return ws
}

func minutes(hhmm string) int {
	var h, m int
	_, _ = fmt.Sscanf(hhmm, "%d:%d", &h, &m)
	return h*60 + m
}

// Lookup returns a country's entry (the Campaigns tab shows its confidence).
func (t *Table) Lookup(country string) (Country, bool) {
	c, ok := t.Countries[code(country)]
	return c, ok
}

// Codes are the table's countries, sorted.
func (t *Table) Codes() []string {
	out := make([]string, 0, len(t.Countries))
	for c := range t.Countries {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// RegistersFor are the registers a number in country is screened against ("" if none).
func (t *Table) RegistersFor(country string) []Register {
	c, ok := t.Countries[code(country)]
	if !ok {
		return nil
	}
	out := make([]Register, 0, len(c.Registers))
	for _, r := range c.Registers {
		out = append(out, t.Registers[r])
	}
	return out
}

// CallFirst reports whether country's leads get the call_first sequence: email needs
// consent, and calls are not off or on hold in the table.
func (t *Table) CallFirst(country string) bool {
	c, ok := t.Countries[code(country)]
	return ok && c.Email.Verdict == VerdictConsent && c.Call.Verdict != VerdictOff && c.Call.Verdict != VerdictHold
}

// Freshness is the warning `campaigns check` shows when the research is more than
// FreshnessDays old; "" while it is fresh.
func (t *Table) Freshness(now time.Time) string {
	age := int(now.Sub(t.ResearchDate).Hours() / 24)
	if age <= FreshnessDays {
		return ""
	}
	return fmt.Sprintf("The country rules were researched on %s, %d days ago (more than %d): re-check them before relying on them.",
		t.ResearchDate.Format(time.DateOnly), age, FreshnessDays)
}

// CheckCampaign lists the faults in a campaign's country rules: a channel turned on where
// the table has it off, an unlisted country, a hold country with no acknowledge reason, a
// country needing a written reason without one. Empty when the rules are fine.
func (t *Table) CheckCampaign(camp CampaignOverrides) []string {
	var out []string
	codes := make([]string, 0, len(camp.Countries))
	for c := range camp.Countries {
		codes = append(codes, c)
	}
	sort.Strings(codes)
	for _, cc := range codes {
		cr := camp.Countries[cc]
		c, ok := t.Countries[code(cc)]
		for _, ch := range []struct {
			name Channel
			on   bool
		}{{ChannelEmail, cr.Email}, {ChannelCall, cr.Call}} {
			if !ch.on {
				continue
			}
			if !ok {
				out = append(out, fmt.Sprintf("countries.%s: %s is on, but %s is not in the country table", cc, ch.name, cc))
				continue
			}
			rule := c.rule(ch.name)
			switch {
			case rule.Verdict == VerdictOff:
				out = append(out, fmt.Sprintf("countries.%s: %s is off by law in %s and no campaign can turn it on", cc, ch.name, c.Name))
			case rule.Verdict == VerdictHold && strings.TrimSpace(cr.Acknowledge) == "":
				out = append(out, fmt.Sprintf("countries.%s: %s in %s is on hold; turning it on needs a written acknowledge reason", cc, ch.name, c.Name))
			case rule.AcknowledgeRequired && strings.TrimSpace(cr.Acknowledge) == "":
				out = append(out, fmt.Sprintf("countries.%s: %s in %s needs a written acknowledge reason (why the offer is relevant to the business)", cc, ch.name, c.Name))
			}
		}
	}
	return out
}

// Verdict answers whether channel ch may be used for a lead in country with org's facts
// under campaign camp at now. ok is true when it may; when it may not, hard says whether
// the law table refuses it (true) or only the campaign's own rules (false). rule is the
// stable code (the Rule* constants) and reason says it in plain words.
//
// Hard checks run first, so a soft answer means every legal rule passed.
func (t *Table) Verdict(country string, ch Channel, org OrgFacts, camp CampaignOverrides, now time.Time) (ok, hard bool, rule, reason string) {
	cc := code(country)
	c, found := t.Countries[cc]
	if !found {
		where := cc
		if where == "" {
			where = "an unknown country"
		}
		return false, true, RuleUnlisted, fmt.Sprintf("%s is not in the country rules, so no %s.", where, words(ch))
	}
	if ch != ChannelEmail && ch != ChannelCall {
		return false, true, RuleUnlisted, fmt.Sprintf("%q is not a channel the country rules cover.", ch)
	}
	cr := c.rule(ch)
	campCountry, campListed := camp.Countries[cc]
	ack := strings.TrimSpace(campCountry.Acknowledge)
	campOn := campListed && campCountry.on(ch)

	allowedRule := cr.Verdict
	switch cr.Verdict {
	case VerdictOff:
		return false, true, RuleOff, fmt.Sprintf("%s in %s is off by law (consent needed); no campaign can turn it on.", capital(words(ch)), c.Name)
	case VerdictHold:
		if !campOn || ack == "" {
			return false, true, RuleHold, fmt.Sprintf("%s in %s is on hold until a campaign turns it on with a written acknowledge reason.", capital(words(ch)), c.Name)
		}
		allowedRule = RuleHoldAcknowledged
	}
	if cr.AcknowledgeRequired && (!campOn || ack == "") {
		return false, true, RuleAcknowledge, fmt.Sprintf("%s in %s needs the campaign's written reason why the offer is relevant to the business.", capital(words(ch)), c.Name)
	}

	individualRule := c.Individuals.Email
	if ch == ChannelCall {
		individualRule = c.Individuals.Call
	}
	consentNeeded := cr.Verdict == VerdictConsent
	if org.EntityType != EntityCompany {
		switch individualRule {
		case IndividualsOff:
			verb := "goes"
			if ch == ChannelCall {
				verb = "go" // "Calls in the Netherlands go", "Email in Denmark goes"
			}
			return false, true, RuleIndividual, fmt.Sprintf("%s in %s %s to companies only; this one is %s.", capital(words(ch)), c.Name, verb, entityWords(org.EntityType))
		case IndividualsConsent:
			consentNeeded = true
		}
	}
	if consentNeeded {
		if ch != ChannelEmail || !org.EmailConsent {
			if cr.Verdict == VerdictConsent {
				if ch == ChannelCall {
					return false, true, RuleConsent, fmt.Sprintf("Calls to %s need their recorded yes first.", c.Name)
				}
				return false, true, RuleConsent, fmt.Sprintf("Email to %s needs their recorded yes first (call first).", c.Name)
			}
			return false, true, RuleIndividual, fmt.Sprintf("%s counts as an individual in %s, so %s needs their recorded consent.", capital(entityWords(org.EntityType)), c.Name, words(ch))
		}
		allowedRule = RuleConsent
	}

	if ch == ChannelCall {
		locs := []*time.Location{c.loc}
		if c.RegionRequired {
			rc := regionCode(cc, org.Region)
			r, ok := c.Regions[rc]
			if !ok {
				return false, true, RuleRegionUnknown, "region unknown: not callable (the lead's state or province is needed in " + c.Name + ")."
			}
			if r.Call == VerdictOff {
				return false, true, RuleRegionBlocked, fmt.Sprintf("Calls to %s, %s are blocked: %s.", rc, c.Name, r.Reason)
			}
			locs = r.locs
		}
		if cr.LandlineOnly && org.PhoneType != "landline" {
			if org.PhoneType == "mobile" {
				return false, true, RuleMobile, fmt.Sprintf("Calls in %s go to landlines only, never mobiles; this number is a mobile.", c.Name)
			}
			return false, true, RuleMobile, fmt.Sprintf("Calls in %s go to landlines only; this number is not known to be a landline.", c.Name)
		}
		windows := cr.Windows
		if len(windows) == 0 {
			windows = t.DefaultCallWindows
		}
		for _, loc := range locs {
			if !inWindows(now.In(loc), windows) {
				return false, true, RuleWindow, fmt.Sprintf("Outside the calling hours for %s now (%s there): %s.", c.Name, now.In(loc).Format("Mon 15:04"), windowWords(windows))
			}
		}
	}

	if camp.Countries != nil && !campOn {
		return false, false, RuleCampaignOff, fmt.Sprintf("%s to %s is off in this campaign's rules: your call.", capital(words(ch)), c.Name)
	}

	reason = fmt.Sprintf("%s to %s is allowed", capital(words(ch)), c.Name)
	if len(cr.Rules) > 0 {
		reason += ": " + strings.Join(cr.Rules, "; ")
	}
	if allowedRule == RuleConsent && consentNeeded {
		reason += " (they agreed on the call)"
	}
	if c.Acknowledge != "" {
		reason += ". " + c.Acknowledge
	}
	return true, false, allowedRule, reason + "."
}

func (c Country) rule(ch Channel) ChannelRule {
	if ch == ChannelCall {
		return c.Call
	}
	return c.Email
}

func (cc CampaignCountry) on(ch Channel) bool {
	if ch == ChannelCall {
		return cc.Call
	}
	return cc.Email
}

func inWindows(t time.Time, ws []Window) bool {
	m := t.Hour()*60 + t.Minute()
	for _, w := range ws {
		if slices.Contains(w.days, t.Weekday()) && m >= w.start && m < w.end {
			return true
		}
	}
	return false
}

func windowWords(ws []Window) string {
	parts := make([]string, 0, len(ws))
	for _, w := range ws {
		parts = append(parts, fmt.Sprintf("%s %s-%s", strings.Join(w.Days, ","), w.Start, w.End))
	}
	return strings.Join(parts, "; ")
}

func code(country string) string { return strings.ToUpper(strings.TrimSpace(country)) }

// regionCode is region as the table keys it: "US-FL", "us-fl" and "FL" are all FL.
func regionCode(country, region string) string {
	r := code(region)
	return strings.TrimPrefix(r, country+"-")
}

func words(ch Channel) string {
	if ch == ChannelCall {
		return "calls"
	}
	return string(ch)
}

func capital(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func entityWords(e string) string {
	if e == EntitySoleTrader {
		return "a sole trader"
	}
	return "not known to be a company"
}

// PhoneType classifies an E.164 number as landline or mobile where the table needs it
// (IE: +35383 to +35389 is mobile, any other Irish number is treated as a landline); ""
// where it cannot tell.
func PhoneType(e164 string) string {
	n := strings.ReplaceAll(strings.TrimSpace(e164), " ", "")
	if strings.HasPrefix(n, "+353") {
		if len(n) > 5 && n[4] == '8' && n[5] >= '3' && n[5] <= '9' {
			return "mobile"
		}
		return "landline"
	}
	return ""
}

// Package-level forms on the embedded table.

// Verdict is Default().Verdict.
func Verdict(country string, ch Channel, org OrgFacts, camp CampaignOverrides, now time.Time) (ok, hard bool, rule, reason string) {
	return defaultTable.Verdict(country, ch, org, camp, now)
}

// RegistersFor is Default().RegistersFor.
func RegistersFor(country string) []Register { return defaultTable.RegistersFor(country) }

// Freshness is Default().Freshness.
func Freshness(now time.Time) string { return defaultTable.Freshness(now) }
