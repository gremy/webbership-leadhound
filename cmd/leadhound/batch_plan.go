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

	"github.com/gremy/webbership-leadhound/internal/campaign"
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
// drafts from that folder's spec and templates instead; the footers, contact-form texts and
// LinkedIn notes stay leads.yaml's.
type batchPlan struct {
	legacy    bool
	sequences leadstore.BatchSequences // frozen on the batch; never nil for a new batch
	seq       []campaign.Step          // the default sequence every lead follows
	templates leadconfig.Templates     // the email steps (and the call card) by kind and language
	rules     outreach.StepRules       // the zero value for the legacy plan
	forbid    map[string][]*regexp.Regexp
	// callsOn are the countries (ISO 3166-1 alpha-2) whose call policy is on in the spec. A
	// lead elsewhere still gets its call drafted (the operator's call), counted as calls off.
	callsOn map[string]bool
}

// callsAllowed reports whether country's call policy is on in the plan's campaign. The
// legacy plan has no call step, so it never asks.
func (p batchPlan) callsAllowed(country string) bool {
	return p.callsOn[countryCode(country)]
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
// its sequences and, when one has a call step, call.txt.
func campaignTemplates(dataDir string, spec campaign.Spec) (leadconfig.Templates, error) {
	req := leadconfig.CampaignTemplates{Kinds: spec.Kinds}
	for _, c := range spec.Countries {
		if (c.Email || c.Call) && c.Lang != "" && !slices.Contains(req.Langs, c.Lang) {
			req.Langs = append(req.Langs, c.Lang)
		}
	}
	seqs := [][]campaign.Step{spec.Sequence}
	for _, name := range slices.Sorted(maps.Keys(spec.Sequences)) {
		seqs = append(seqs, spec.Sequences[name])
	}
	for _, seq := range seqs {
		for _, st := range seq {
			switch st.Type {
			case campaign.StepEmail:
				if !slices.Contains(req.EmailSteps, st.Step) {
					req.EmailSteps = append(req.EmailSteps, st.Step)
				}
			case campaign.StepCall:
				req.Call = true
			}
		}
	}
	slices.Sort(req.EmailSteps)
	dir := filepath.Join(dataDir, campaign.Dir, spec.Slug, cmp.Or(spec.TemplatesDir, defaultTemplatesDir))
	return leadconfig.LoadCampaignTemplates(dir, req)
}

// filePlan is the plan of spec, read from its folder in dataDir.
func filePlan(dataDir string, spec campaign.Spec) (batchPlan, error) {
	t, err := campaignTemplates(dataDir, spec)
	if err != nil {
		return batchPlan{}, err
	}
	seqs := leadstore.BatchSequences{campaign.SequenceDefault: spec.Sequence}
	for name, seq := range spec.Sequences {
		seqs[name] = seq
	}
	p := batchPlan{sequences: seqs, seq: spec.Sequence, templates: t, forbid: map[string][]*regexp.Regexp{}, callsOn: map[string]bool{}}
	for code, c := range spec.Countries {
		if c.Call {
			p.callsOn[strings.ToUpper(code)] = true
		}
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
	if err := spec.Check(campaign.CheckOptions{KnownKinds: leadconfig.Kinds}); err != nil {
		return batchPlan{}, refused("Campaign %s does not pass its checks: %s", c.Slug, strings.ReplaceAll(err.Error(), "\n", "; "))
	}
	_, sha, err := campaign.Canonical(spec)
	if err != nil {
		return batchPlan{}, err
	}
	if sha != c.SpecSHA256 {
		return batchPlan{}, refused("Campaign %s changed since it was registered: run leadhound leads campaigns register, then make the batch.", c.Slug)
	}
	p, err := filePlan(bs.dataDir, spec)
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
