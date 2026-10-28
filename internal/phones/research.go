package phones

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/gremy/webbership-leadhound/internal/ai"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/research"
)

// LeadPhone is one phone already stored for a lead's org, with the line kind its
// phone_research row gave it ("" when it came from enrich and its kind is unknown).
type LeadPhone struct {
	Normalized string
	LineKind   string // research.LineDirect, LineDecisionMaker, LineSwitchboard or ""
}

// CallLead is a lead with a call step ahead in an open or upcoming sequence of a campaign,
// as the store lists it. The store leaves out leads that are suppressed, deferred, whose
// country is off for calls, or whose org cannot be researched now (an attempt inside the
// 90 days or one queued or running); it lists them in its own order (the call step's due
// time first), which Select keeps among leads of equal star.
type CallLead struct {
	LeadID  int64
	OrgID   int64
	OrgName string
	Domain  string // the org's registrable domain
	Country string // ISO 3166-1 alpha-2
	Starred bool
	Phones  []LeadPhone
}

// CallLeadSource is the slice of the store phone research needs. internal/leadstore
// satisfies it (through a thin adapter where its own row types differ).
type CallLeadSource interface {
	CallLeadsAhead(ctx context.Context, campaignID int64) ([]CallLead, error)
}

// Researcher runs one research request. Production passes research.Run bound to the capped
// Claude caller and the fetch-backed Verifier:
//
//	func(ctx context.Context, r research.Request) (research.Result, error) {
//		return research.Run(ctx, capped, verifier, r)
//	}
type Researcher func(ctx context.Context, r research.Request) (research.Result, error)

// NeedsResearch reports whether l's org is worth a phone search: it has no phone, or only
// switchboard numbers. A number of unknown kind (found by enrich, usually the main number)
// counts as a switchboard; a direct or decision-maker line means no search.
func NeedsResearch(l CallLead) bool {
	for _, p := range l.Phones {
		if p.LineKind == research.LineDirect || p.LineKind == research.LineDecisionMaker {
			return false
		}
	}
	return true
}

// Select picks up to max leads (max <= 0: all) that need a phone search and can have one
// (a domain to research), starred first, otherwise in the store's order, one per org.
func Select(leads []CallLead, max int) []CallLead {
	var out []CallLead
	orgs := map[int64]bool{}
	for _, l := range leads {
		if !NeedsResearch(l) || strings.TrimSpace(l.Domain) == "" {
			continue
		}
		out = append(out, l)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Starred && !out[j].Starred })
	kept := out[:0]
	for _, l := range out {
		if orgs[l.OrgID] {
			continue
		}
		orgs[l.OrgID] = true
		kept = append(kept, l)
	}
	if max > 0 && len(kept) > max {
		kept = kept[:max]
	}
	return kept
}

// Request is the phone-only research request for l.
func Request(l CallLead) research.Request {
	return research.Request{
		LeadID: l.LeadID,
		Org:    research.Org{ID: l.OrgID, Name: l.OrgName, Domain: l.Domain, Country: l.Country},
		Want:   research.Want{Phone: true},
	}
}

// PhoneRecord is one verified phone to store: Fact for leadstore.Queries.InsertFact
// (org_fact kind phone, derived_by claude-research@1, the cited page's fetch id), and the
// phone_research row's line_kind, role (a job title, never a name) and model. The fact id
// and usage come from the store's insert and the run's ledger.
type PhoneRecord struct {
	Fact     leadstore.FactRow
	LineKind string
	Role     string
	Model    string
}

// Records turns a research result into what to store. Only verified phones are in it;
// research.Run already dropped the rest, and the model's quote never reaches here.
func Records(res research.Result) []PhoneRecord {
	out := make([]PhoneRecord, 0, len(res.Phones))
	for _, p := range res.Phones {
		out = append(out, PhoneRecord{Fact: p.Fact, LineKind: p.LineKind, Role: p.Role, Model: res.Model})
	}
	return out
}

// Outcome is one lead's research: the result (usage and cost included, also on error),
// the records to store, and the error, if any.
type Outcome struct {
	Lead    CallLead
	Result  research.Result
	Records []PhoneRecord
	Err     error
}

// Run researches phones for up to max of campaignID's call-step leads, chosen by Select,
// one research call per lead through rs. It stops before the next lead when a spend cap
// refuses (ai.ErrSpendCap: the daily caps hold for the rest of the run; a per-lead cap
// refusal reads the same and stops it too) or the context ends; other errors are kept on
// the lead's Outcome and the run goes on. The caller stores each Outcome's Records and its
// attempt row.
func Run(ctx context.Context, src CallLeadSource, rs Researcher, campaignID int64, max int) ([]Outcome, error) {
	leads, err := src.CallLeadsAhead(ctx, campaignID)
	if err != nil {
		return nil, fmt.Errorf("phones: call leads of campaign %d: %w", campaignID, err)
	}
	var out []Outcome
	for _, l := range Select(leads, max) {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		res, err := rs(ctx, Request(l))
		o := Outcome{Lead: l, Result: res, Records: Records(res), Err: err}
		out = append(out, o)
		if errors.Is(err, ai.ErrSpendCap) {
			return out, err
		}
		if err != nil && ctx.Err() != nil {
			return out, ctx.Err()
		}
	}
	return out, nil
}
