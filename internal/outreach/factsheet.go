package outreach

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// FactGroup says whose fact a Fact is: the lead's, the sender's, or the offer's.
type FactGroup string

// The fact groups, in the order a sheet lists them. Each group has its own ID prefix.
const (
	GroupLead   FactGroup = "lead"   // F1, F2 …
	GroupSender FactGroup = "sender" // S1, S2 …
	GroupOffer  FactGroup = "offer"  // O1, O2 …
)

// Fact labels a sheet built by BuildFactSheet uses. Extra facts carry their caller's label.
const (
	LabelOrgName      = "org_name"
	LabelOrgCountry   = "org_country"
	LabelOrgDomain    = "org_domain"
	LabelKind         = "kind"
	LabelMajor        = "finding_major"  // "Drupal 7"
	LabelVersion      = "drupal_version" // the exact version, "7.98"
	LabelSignal       = "finding_signal" // the stored signal the finding rests on
	LabelEvidence     = "finding_evidence"
	LabelFindingURL   = "finding_url"
	LabelEOLDate      = "eol_date" // the date in the sheet's language: "9 December 2026", "9 decembrie 2026"
	LabelOrgText      = "org_text" // a description or page text: context only, echo_guard
	LabelSenderName   = "sender_name"
	LabelSenderRole   = "sender_role"
	LabelIdentityLine = "identity_line"
	LabelProofLine    = "proof_line" // the one true proof line, verbatim
	LabelSenderSite   = "sender_site"
	LabelReport       = "report"          // the free report's name
	LabelReportPart   = "report_contains" // one thing the report holds
	LabelCallToAction = "call_to_action"
)

// Fact is one stored fact a Claude draft may cite. EchoGuard marks text that is context
// only (the org's own description or page text): it may never be cited or echoed.
type Fact struct {
	ID        string     `json:"id"`
	Label     string     `json:"label"`
	Value     string     `json:"value"`
	Source    Provenance `json:"source"`
	Group     FactGroup  `json:"group"`
	EchoGuard bool       `json:"echo_guard,omitempty"`
}

// FactSheet is every fact a draft for one lead may rest on, in a stable order: the lead's
// facts, then the sender's, then the offer's, each numbered from 1 within its group.
type FactSheet struct {
	Facts []Fact `json:"facts"`
}

// TextFact is one labelled value with where it came from.
type TextFact struct {
	Label  string
	Value  string
	Source Provenance
}

// OrgFacts are the org's own stored fields.
type OrgFacts struct {
	Name, Country, Domain string
	Source                Provenance
}

// FindingFacts are the finding a lead rests on (for Drupal: the site_probe row).
type FindingFacts struct {
	Major    int    // 0: unknown, left out
	Exact    string // the exact version, "7.98"
	Signal   string // the stored signal, "core-asset-v 7.98"
	Evidence string // the evidence line
	URL      string // the page the finding was read on
	Source   Provenance
}

// SenderFacts are the campaign's sender facts, verbatim.
type SenderFacts struct {
	Name, Role, IdentityLine string
	ProofLine                string // the one proof line, used verbatim or not at all
	SiteURL                  string
	Extra                    []TextFact
	Source                   Provenance
}

// OfferFacts are what the campaign offers: the free report and what it holds.
type OfferFacts struct {
	Report       string
	Contents     []string
	CallToAction string // e.g. "Worth 10 minutes to find out?"
	Extra        []TextFact
	Source       Provenance
}

// SheetInput is everything a fact sheet is built from. It is plain data, so BuildFactSheet
// is pure: the caller reads the store.
type SheetInput struct {
	// Lang is the language date facts are written in, as the drafts write them ("9 December
	// 2026"; "9 decembrie 2026" for ro): "" is English, a language FormatDate has no wording
	// for keeps the ISO form. G3 compares dates as dates, so any form of the same date passes.
	Lang      string
	Org       OrgFacts
	Kind      string
	Finding   *FindingFacts
	EOL       time.Time // the end-of-life date for the lead's major; zero: none
	EOLSource Provenance
	LeadExtra []TextFact // further lead facts (another campaign's signals)
	OrgTexts  []TextFact // descriptions and page texts of the org: echo_guard, never quotable
	Sender    SenderFacts
	Offer     OfferFacts
}

// BuildFactSheet builds the sheet. Empty values are left out; the fixed facts keep a fixed
// order and lists are sorted by label then value (duplicates dropped), so the same input in
// any order gives the same sheet, the same IDs and the same hash.
func BuildFactSheet(in SheetInput) FactSheet {
	var s FactSheet
	add := func(g FactGroup, label, value string, src Provenance, echo bool) {
		value = strings.TrimSpace(value)
		if value == "" {
			return
		}
		s.Facts = append(s.Facts, Fact{Label: label, Value: value, Source: src, Group: g, EchoGuard: echo})
	}
	addAll := func(g FactGroup, list []TextFact, echo bool) {
		for _, f := range sortedTextFacts(list) {
			add(g, f.Label, f.Value, f.Source, echo)
		}
	}

	add(GroupLead, LabelOrgName, in.Org.Name, in.Org.Source, false)
	add(GroupLead, LabelOrgCountry, in.Org.Country, in.Org.Source, false)
	add(GroupLead, LabelOrgDomain, in.Org.Domain, in.Org.Source, false)
	add(GroupLead, LabelKind, in.Kind, in.Org.Source, false)
	if f := in.Finding; f != nil {
		if f.Major > 0 {
			add(GroupLead, LabelMajor, "Drupal "+strconv.Itoa(f.Major), f.Source, false)
		}
		add(GroupLead, LabelVersion, f.Exact, f.Source, false)
		add(GroupLead, LabelSignal, f.Signal, f.Source, false)
		add(GroupLead, LabelEvidence, f.Evidence, f.Source, false)
		add(GroupLead, LabelFindingURL, f.URL, f.Source, false)
	}
	if !in.EOL.IsZero() {
		add(GroupLead, LabelEOLDate, SheetDate(in.EOL, in.Lang), in.EOLSource, false)
	}
	addAll(GroupLead, in.LeadExtra, false)
	texts := make([]TextFact, len(in.OrgTexts))
	for i, t := range in.OrgTexts {
		texts[i] = TextFact{Label: LabelOrgText, Value: t.Value, Source: t.Source}
	}
	addAll(GroupLead, texts, true)

	sd := in.Sender
	add(GroupSender, LabelSenderName, sd.Name, sd.Source, false)
	add(GroupSender, LabelSenderRole, sd.Role, sd.Source, false)
	add(GroupSender, LabelIdentityLine, sd.IdentityLine, sd.Source, false)
	add(GroupSender, LabelProofLine, sd.ProofLine, sd.Source, false)
	add(GroupSender, LabelSenderSite, sd.SiteURL, sd.Source, false)
	addAll(GroupSender, sd.Extra, false)

	of := in.Offer
	add(GroupOffer, LabelReport, of.Report, of.Source, false)
	parts := make([]TextFact, len(of.Contents))
	for i, c := range of.Contents {
		parts[i] = TextFact{Label: LabelReportPart, Value: c, Source: of.Source}
	}
	addAll(GroupOffer, parts, false)
	add(GroupOffer, LabelCallToAction, of.CallToAction, of.Source, false)
	addAll(GroupOffer, of.Extra, false)

	n := map[FactGroup]int{}
	for i := range s.Facts {
		g := s.Facts[i].Group
		n[g]++
		s.Facts[i].ID = groupPrefix(g) + strconv.Itoa(n[g])
	}
	return s
}

// SheetDate is t as a date fact states it in lang: FormatDate's words ("" reads as English),
// or the ISO date for a language it has no wording for.
func SheetDate(t time.Time, lang string) string {
	if lang == "" {
		lang = LangEnglish
	}
	if s, err := FormatDate(t, lang); err == nil {
		return s
	}
	return t.Format(time.DateOnly)
}

func groupPrefix(g FactGroup) string {
	switch g {
	case GroupLead:
		return "F"
	case GroupSender:
		return "S"
	default:
		return "O"
	}
}

// sortedTextFacts sorts by label then value (empty values out, duplicates dropped).
func sortedTextFacts(list []TextFact) []TextFact {
	out := make([]TextFact, 0, len(list))
	for _, f := range list {
		f.Value = strings.TrimSpace(f.Value)
		if f.Value != "" {
			out = append(out, f)
		}
	}
	slices.SortStableFunc(out, func(a, b TextFact) int {
		if c := strings.Compare(a.Label, b.Label); c != 0 {
			return c
		}
		return strings.Compare(a.Value, b.Value)
	})
	return slices.CompactFunc(out, func(a, b TextFact) bool { return a.Label == b.Label && a.Value == b.Value })
}

// factSheetVersion opens the canonical encoding, so a future change of shape changes every hash.
const factSheetVersion = "leadhound-factsheet/1\n"

// Canonical returns the sheet's stable bytes: what the model sees (ID, label, value, group,
// echo guard), in sheet order. Provenance row ids are left out: a re-probe that stores the
// same values in a new row keeps the hash, so a stored generation stays reusable.
func (s FactSheet) Canonical() []byte {
	type canon struct {
		ID    string    `json:"id"`
		Label string    `json:"label"`
		Value string    `json:"value"`
		Group FactGroup `json:"group"`
		Echo  bool      `json:"echo_guard"`
	}
	list := make([]canon, len(s.Facts))
	for i, f := range s.Facts {
		list[i] = canon{f.ID, f.Label, f.Value, f.Group, f.EchoGuard}
	}
	b, err := json.Marshal(list)
	if err != nil { // strings and bools only: cannot fail
		panic(fmt.Sprintf("outreach: fact sheet canonical: %v", err))
	}
	return append([]byte(factSheetVersion), b...)
}

// SHA256 is the hex SHA-256 of Canonical: the fact_sha256 a stored generation is keyed by.
func (s FactSheet) SHA256() string {
	sum := sha256.Sum256(s.Canonical())
	return hex.EncodeToString(sum[:])
}

// Fact returns the fact with that ID.
func (s FactSheet) Fact(id string) (Fact, bool) {
	for _, f := range s.Facts {
		if f.ID == id {
			return f, true
		}
	}
	return Fact{}, false
}

// WithLabel returns the facts with that label, in sheet order.
func (s FactSheet) WithLabel(label string) []Fact {
	var out []Fact
	for _, f := range s.Facts {
		if f.Label == label {
			out = append(out, f)
		}
	}
	return out
}
