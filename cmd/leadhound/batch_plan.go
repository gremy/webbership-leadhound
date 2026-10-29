package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"text/template"

	"github.com/gremy/webbership-leadhound/internal/campaign"
	"github.com/gremy/webbership-leadhound/internal/compliance"
	"github.com/gremy/webbership-leadhound/internal/leadconfig"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/outreach"
)

// defaultTemplatesDir is a campaign's templates folder when its spec names none.
const defaultTemplatesDir = "templates"

// batchPlan is what one batch drafts with for its campaign: the sequences it freezes on the
// batch, the sequence every lead follows, the templates its email steps and call card render
// from, and the copy rules Check holds them to.
//
// The legacy plan is leads.yaml's flow: the three-step all-email sequence spaced by
// schedule.followup_business_days, leads.yaml's templates and copy bounds. A campaign whose
// registered version came from its own folder (<data dir>/campaigns/<slug>/campaign.yaml)
// drafts from that folder's spec and templates instead; the contact-form texts and LinkedIn
// notes stay leads.yaml's, and so does the footer, unless the spec's country names its own
// (countries.<code>.footer, read from leads.yaml's footers dir: footerFor).
type batchPlan struct {
	legacy    bool
	sequences leadstore.BatchSequences // frozen on the batch; never nil for a new batch
	seq       []campaign.Step          // the default sequence every lead follows
	templates leadconfig.Templates     // the email steps (and the call card) by kind and language
	rules     outreach.StepRules       // the zero value for the legacy plan
	forbid    map[string][]*regexp.Regexp
	// overrides are the spec's country rules as internal/compliance reads them (no campaign
	// rules for the legacy plan). A country whose call policy is off in them still gets its
	// call drafted (the operator's call), counted as calls off.
	overrides compliance.CampaignOverrides
	// callFirst is the sequence a lead whose country needs email consent follows (the spec's
	// sequences.call_first, else campaign.DefaultCallFirst); nil when the plan has no call
	// step, so no lead is moved off the default sequence.
	callFirst []campaign.Step
	// seqName is the name of the sequence seq is ("" for the default); its email templates
	// are read from that sequence's own folder (leadconfig.Set.SequenceEmail).
	seqName string
	// spec is the campaign the plan drafts for (campaign.Legacy for the legacy plan): its
	// writer, offer and grounding rules, which a Claude draft reads.
	spec campaign.Spec
}

// forLead is the plan one lead drafts with: for a lead whose country's email needs consent
// (compliance.CallFirst) and a plan with a call step, the plan on its call_first sequence
// and that sequence's name; else the plan itself and "" (the default sequence).
func (p batchPlan) forLead(country string) (batchPlan, string) {
	if p.callFirst == nil || !compliance.Default().CallFirst(country) {
		return p, ""
	}
	q := p
	q.seq, q.seqName = p.callFirst, campaign.SequenceCallFirst
	if !p.legacy {
		q.rules.MaxWords = p.spec.MaxWordsFor(campaign.SequenceCallFirst)
	}
	q.rules.EmailSteps = nil
	for _, st := range p.callFirst {
		if st.Type == campaign.StepEmail {
			q.rules.EmailSteps = append(q.rules.EmailSteps, st.Step)
		}
	}
	return q, campaign.SequenceCallFirst
}

// candidateCountries are the countries whose leads the plan's campaign may batch (D3): a
// campaign spec that lists countries batches only the leads of those with email or calls on
// (leads.yaml still gives each its footer and opt-out); nil for the legacy plan or a spec
// listing none, which keeps leads.yaml's channels as the only country rule.
func (p batchPlan) candidateCountries() []string {
	if p.legacy || len(p.spec.Countries) == 0 {
		return nil
	}
	out := []string{}
	for code, c := range p.spec.Countries {
		if c.Email || c.Call {
			out = append(out, countryCode(code))
		}
	}
	slices.Sort(out)
	return out
}

// phoneOnlyCountries are the countries whose leads with no route but a phone number the
// plan may batch: those whose email needs consent won on a call (compliance.CallFirst),
// when the plan moves them onto a call_first sequence; nil when it has none. Narrowed to
// candidateCountries when the spec lists countries.
func (p batchPlan) phoneOnlyCountries() []string {
	if p.callFirst == nil {
		return nil
	}
	t := compliance.Default()
	codes := p.candidateCountries()
	if codes == nil {
		codes = t.Codes()
	}
	out := []string{}
	for _, code := range codes {
		if t.CallFirst(code) && !slices.Contains(out, countryCode(code)) {
			out = append(out, countryCode(code))
		}
	}
	slices.Sort(out)
	return out
}

// countryCode is country as the specs key it: trimmed, upper case.
func countryCode(country string) string { return strings.ToUpper(strings.TrimSpace(country)) }

// emailSteps is how many email steps a lead's sequence has at most: what the mailbox ramp
// counts per lead.
func (p batchPlan) emailSteps() int {
	n := 0
	for _, st := range p.seq {
		if st.Type == campaign.StepEmail {
			n++
		}
	}
	return max(n, 1)
}

// hasCall reports whether the sequence has a call step.
func (p batchPlan) hasCall() bool {
	return slices.ContainsFunc(p.seq, func(st campaign.Step) bool { return st.Type == campaign.StepCall })
}

// rulesFor is the plan's copy rules with the kind's forbidden patterns.
func (p batchPlan) rulesFor(kind string) outreach.StepRules {
	r := p.rules
	r.Forbid = p.forbid[kind]
	return r
}

// legacyPlan is leads.yaml's flow, the sequence stored explicitly.
func legacyPlan(cfg leadconfig.Config) batchPlan {
	seq := campaign.LegacySequence(cfg.Schedule.FollowupBusinessDays)
	return batchPlan{
		legacy:    true,
		sequences: leadstore.BatchSequences{campaign.SequenceDefault: seq},
		seq:       seq,
		templates: cfg.Templates,
		spec:      campaign.Legacy(&cfg),
	}
}

// campaignFile is campaign slug's spec file in dataDir, and whether it exists.
func campaignFile(dataDir, slug string) (string, bool, error) {
	if dataDir == "" {
		return "", false, nil
	}
	p := filepath.Join(dataDir, campaign.Dir, slug, campaign.FileName)
	_, err := os.Stat(p)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return p, false, nil
	case err != nil:
		return p, false, fmt.Errorf("campaign %s: %w", slug, err)
	}
	return p, true, nil
}

// campaignTemplates loads spec's templates from its folder in dataDir: for every kind of the
// spec and every language a country of the spec has email or calls on in, each email step of
// its default sequence (<kind>/<lang>/<step>.txt), each email step of every named sequence
// (<kind>/<lang>/<name>/<step>.txt) and, when one has a call step, call.txt. The footers its
// countries name (countries.<code>.footer) are read from lc's footers dir into Footers. A spec with no
// call_first of its own whose plan moves leads onto campaign.DefaultCallFirst (callFirstSeq)
// needs that sequence's email steps too, so a missing one is named, never a silent drop.
func campaignTemplates(dataDir string, spec campaign.Spec, lc leadconfig.Config) (leadconfig.Templates, error) {
	req := leadconfig.CampaignTemplates{Kinds: spec.Kinds}
	for _, c := range spec.Countries {
		if (c.Email || c.Call) && c.Lang != "" && !slices.Contains(req.Langs, c.Lang) {
			req.Langs = append(req.Langs, c.Lang)
		}
	}
	named := map[string][]campaign.Step{}
	for name, seq := range spec.Sequences {
		named[name] = seq
	}
	if _, ok := named[campaign.SequenceCallFirst]; !ok && specHasCallFirstLeads(spec) {
		named[campaign.SequenceCallFirst] = campaign.DefaultCallFirst()
	}
	steps := func(seq []campaign.Step) []int {
		var out []int
		for _, st := range seq {
			switch st.Type {
			case campaign.StepEmail:
				if !slices.Contains(out, st.Step) {
					out = append(out, st.Step)
				}
			case campaign.StepCall:
				req.Call = true
			}
		}
		slices.Sort(out)
		return out
	}
	req.EmailSteps = steps(spec.Sequence)
	for _, name := range slices.Sorted(maps.Keys(named)) {
		if s := steps(named[name]); len(s) > 0 {
			if req.Named == nil {
				req.Named = map[string][]int{}
			}
			req.Named[name] = s
		}
	}
	dir := filepath.Join(dataDir, campaign.Dir, spec.Slug, cmp.Or(spec.TemplatesDir, defaultTemplatesDir))
	t, err := leadconfig.LoadCampaignTemplates(dir, req)
	if err != nil {
		return leadconfig.Templates{}, err
	}
	var footers []string
	for _, code := range slices.Sorted(maps.Keys(spec.Countries)) {
		if f := spec.Countries[code].Footer; f != "" && !slices.Contains(footers, f) {
			footers = append(footers, f)
		}
	}
	if len(footers) > 0 {
		if t.Footers, err = lc.LoadFooters(footers); err != nil {
			return leadconfig.Templates{}, err
		}
	}
	return t, nil
}

// footerFor is the footer an email to a lead in country ends with: the plan's campaign's own
// (countries.<code>.footer) when the spec's country names one, else leads.yaml's for the
// country (cfg.Policy(country).Footer). nil when the one it names is not loaded.
func (p batchPlan) footerFor(country string, cfg leadconfig.Config) *template.Template {
	if !p.legacy {
		for code, c := range p.spec.Countries {
			if c.Footer != "" && countryCode(code) == countryCode(country) {
				return p.templates.Footers[c.Footer]
			}
		}
	}
	return cfg.Templates.Footers[cfg.Policy(country).Footer]
}

// specHasCallFirstLeads reports whether spec's plan sends some of its leads down
// campaign.DefaultCallFirst: its default sequence has a call step, it defines no call_first of
// its own, and one of its countries with email or calls on needs email consent
// (compliance.CallFirst). filePlan moves those leads; campaignTemplates loads their emails.
func specHasCallFirstLeads(spec campaign.Spec) bool {
	if len(spec.Sequences[campaign.SequenceCallFirst]) > 0 ||
		!slices.ContainsFunc(spec.Sequence, func(st campaign.Step) bool { return st.Type == campaign.StepCall }) {
		return false
	}
	t := compliance.Default()
	for code, c := range spec.Countries {
		if (c.Email || c.Call) && t.CallFirst(code) {
			return true
		}
	}
	return false
}

// filePlan is the plan of spec, read from its folder in dataDir.
func filePlan(dataDir string, spec campaign.Spec, lc leadconfig.Config) (batchPlan, error) {
	t, err := campaignTemplates(dataDir, spec, lc)
	if err != nil {
		return batchPlan{}, err
	}
	seqs := leadstore.BatchSequences{campaign.SequenceDefault: spec.Sequence}
	for name, seq := range spec.Sequences {
		seqs[name] = seq
	}
	p := batchPlan{sequences: seqs, seq: spec.Sequence, templates: t, forbid: map[string][]*regexp.Regexp{},
		overrides: specOverrides(spec.Countries), spec: spec}
	if cf := spec.Sequences[campaign.SequenceCallFirst]; len(cf) > 0 {
		p.callFirst = cf
	} else if p.hasCall() {
		p.callFirst = campaign.DefaultCallFirst()
	}
	for _, st := range spec.Sequence {
		if st.Type == campaign.StepEmail {
			p.rules.EmailSteps = append(p.rules.EmailSteps, st.Step)
		}
	}
	p.rules.MaxWords = spec.Copy.MaxWords
	p.rules.BannedWords = spec.Copy.BannedWords
	p.rules.NoLinksInStep0 = !spec.Copy.LinksInStep0
	p.rules.NoExclamation = !spec.Copy.Exclamation
	if spec.Product.SiteURL != "" {
		p.rules.Links = append(p.rules.Links, spec.Product.SiteURL)
	}
	p.rules.Links = append(p.rules.Links, spec.Offer.Links...)
	for kind, pats := range spec.Copy.ForbidByKind {
		for _, pat := range pats {
			re, err := regexp.Compile(pat)
			if err != nil {
				return batchPlan{}, fmt.Errorf("campaign %s: copy.forbid_by_kind.%s: %w", spec.Slug, kind, err)
			}
			p.forbid[kind] = append(p.forbid[kind], re)
		}
	}
	return p, nil
}

// resolveBatchPlan is the plan a batch of campaign campaignID drafts with. The legacy plan
// unless the campaign's registered version came from its own folder in bs.dataDir; then that
// file must still be what was registered (a changed or unregistered file is refused, so the
// batch never drafts from a spec the console does not show), and must pass its checks.
func resolveBatchPlan(ctx context.Context, lq leadstore.Queries, bs batchSetup, campaignID int64) (batchPlan, error) {
	legacy := legacyPlan(bs.leads)
	cs, err := lq.Campaigns(ctx, leadstore.DefaultAccountID)
	if err != nil {
		return batchPlan{}, err
	}
	i := slices.IndexFunc(cs, func(c leadstore.Campaign) bool { return c.ID == campaignID })
	if i < 0 {
		return legacy, nil // CreateBatchFor refuses a campaign the leads are not in
	}
	c := cs[i]
	path, ok, err := campaignFile(bs.dataDir, c.Slug)
	if err != nil || !ok {
		return legacy, err
	}
	if c.SpecSHA256 == "" {
		return batchPlan{}, refused("Campaign %s has a file but no registered version: run leadhound leads campaigns register first.", c.Slug)
	}
	spec, err := campaign.Load(path)
	if err != nil {
		return batchPlan{}, refused("Campaign %s: %s", c.Slug, err.Error())
	}
	// writer: claude passes here: the trust gate is judged per batch (newBatchDrafter), and a
	// campaign it does not trust drafts from templates rather than being refused.
	if err := spec.Check(campaign.CheckOptions{KnownKinds: campaign.KnownKinds(), DraftsTrusted: true}); err != nil {
		return batchPlan{}, refused("Campaign %s does not pass its checks: %s", c.Slug, strings.ReplaceAll(err.Error(), "\n", "; "))
	}
	_, sha, err := campaign.Canonical(spec)
	if err != nil {
		return batchPlan{}, err
	}
	if sha != c.SpecSHA256 {
		return batchPlan{}, refused("Campaign %s changed since it was registered: run leadhound leads campaigns register, then make the batch.", c.Slug)
	}
	p, err := filePlan(bs.dataDir, spec, bs.leads)
	if err != nil {
		return batchPlan{}, refused("Campaign %s: %s", c.Slug, err.Error())
	}
	return p, nil
}

// Why a lead's call steps were left out of its batch (NewBatchOutcome.CallsLeftOut).
const (
	callNoNumber   = "no phone number on file"
	callSuppressed = "its number is on the do-not-call list"
)

// pickPhone is the number a lead's call steps dial, from its org's facts: the phone facts
// whose number no active phone suppression of the account holds (suppressed holds
// leadstore.PhoneSuppressionValue values), then the first in international form ("+…"), then
// the earliest found (lowest id), so the same facts always give the same number. ok is false
// with why when there is none.
func pickPhone(facts []leadstore.Fact, suppressed map[string]bool) (f leadstore.Fact, ok bool, why string) {
	var phones []leadstore.Fact
	for _, x := range facts {
		if x.Kind == leadstore.FactPhone {
			phones = append(phones, x)
		}
	}
	if len(phones) == 0 {
		return leadstore.Fact{}, false, callNoNumber
	}
	phones = slices.DeleteFunc(phones, func(x leadstore.Fact) bool {
		return suppressed[leadstore.PhoneSuppressionValue(x.Normalized)]
	})
	if len(phones) == 0 {
		return leadstore.Fact{}, false, callSuppressed
	}
	slices.SortFunc(phones, func(a, b leadstore.Fact) int {
		ai, bi := strings.HasPrefix(a.Normalized, "+"), strings.HasPrefix(b.Normalized, "+")
		if ai != bi {
			if ai {
				return -1
			}
			return 1
		}
		return cmp.Compare(a.ID, b.ID)
	})
	return phones[0], true, ""
}
