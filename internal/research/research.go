// Package research asks Claude (Haiku 5.5, with web search and web fetch) for an
// organisation's published contact routes, then keeps only what LeadHound verifies itself:
// each candidate's cited page is fetched through LeadHound's own fetcher (public-address
// guard, robots.txt honoured) and the candidate must be on that page.
//
// An email counts only when contacts.Extract, the finder's own classification, returns it
// as a role address on the organisation's registrable domain and that domain has MX. A
// phone counts only when its normalised number is printed on the cited page. Everything
// else is counted and dropped.
//
// The model's quote (the text around a candidate) is read from the answer and discarded
// at once: verification rests on the page LeadHound fetched, never on what the model says
// the page holds. A quote can hold a person's name, so it is never kept in a Result,
// logged, or put in an error. This package logs nothing.
//
// The package never builds a Claude client, reads a key or writes the store: the caller
// passes a spend-capped Caller (a *claude.Capped) and stores the Result.
package research

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/gremy/webbership-leadhound/internal/ai"
	"github.com/gremy/webbership-leadhound/internal/claude"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
)

// DerivedBy names this rule set on the routes and facts it produces.
const DerivedBy = "claude-research@1"

// Model is the model a research call uses.
const Model = claude.ModelHaiku

// The call's limits. Worst case per leg, priced by claude.WorstResearchLeg, stays well
// inside the default $0.10 per-lead research cap.
const (
	SearchMaxUses         = 3
	FetchMaxUses          = 3
	FetchMaxContentTokens = 8000
	MaxTokens             = 2048
	// MaxCandidates is how many candidates of the answer are verified; the rest are counted
	// and dropped, so one answer can never make LeadHound fetch more than this many pages.
	MaxCandidates = 6
)

// The candidate kinds.
const (
	KindEmail = "email"
	KindPhone = "phone"
)

// The phone line kinds the model may name; anything else reads as "".
const (
	LineDirect        = "direct"
	LineDecisionMaker = "decision_maker"
	LineSwitchboard   = "switchboard"
)

// maxRoleRunes is the longest role kept from the model's answer.
const maxRoleRunes = 60

// Caller is the slice of a Claude caller research needs. *claude.Capped implements it
// (and so do *claude.Client and *claude.Fake, for tests).
type Caller interface {
	Research(ctx context.Context, r claude.ResearchRequest) (claude.ResearchResult, claude.Usage, error)
}

// Org is the organisation researched.
type Org struct {
	ID      int64
	Name    string
	Domain  string // its registrable domain
	Country string // ISO 3166-1 alpha-2, e.g. "GB"; "" when unknown
}

// Want says what to look for. The caller decides it from the lead's country rule.
type Want struct {
	Email bool // a published role address
	Phone bool // a published business phone
}

// Request is one research run for one lead's organisation.
type Request struct {
	LeadID int64 // the lead the spend counts against (the per-lead cap)
	Org    Org
	Want   Want
}

// Phone is one verified business phone: Fact is ready for leadstore.Queries.InsertFact;
// LineKind and Role are the model's own labels (Role a job title by the prompt's rule,
// unverified), for CP5's phone_research row.
type Phone struct {
	Fact     leadstore.FactRow
	LineKind string
	Role     string
}

// Result is what one research run produced. Routes and Phones are verified; Candidates
// counts every candidate in the model's answer and Verified those kept.
type Result struct {
	Routes     []leadstore.RouteRow // each ready for leadstore.Queries.InsertRoute
	Phones     []Phone
	Candidates int
	Verified   int
	Model      string
	WebTools   string       // the web tool version the answer came from
	Usage      claude.Usage // billed, every call and continuation included
	CostMicros int64        // Usage priced (0 for the fake model)
}

// candidate is one item of the model's answer, quote already dropped.
type candidate struct {
	kind, value, sourceURL, lineKind, role string
}

// Run asks c for Request's contact routes, then verifies each candidate through v. The
// request goes out with claude.WebTools20260209; a bad-request answer (the model not taking
// those tools) is retried once with claude.WebToolsBasic.
//
// The Result carries the usage and cost also when an error is returned: a spend-cap
// refusal (errors.Is ai.ErrSpendCap, nothing called), an answer that is not the candidates
// JSON (ai.ErrBadAnswer, billed), a failed call, or a verification stopped by the context
// or the fetch breaker (Routes and Phones then hold what was verified before the stop).
func Run(ctx context.Context, c Caller, v Verifier, req Request) (Result, error) {
	out := Result{Model: Model}
	if !req.Want.Email && !req.Want.Phone {
		return out, errors.New("research: nothing to look for")
	}
	if strings.TrimSpace(req.Org.Domain) == "" {
		return out, errors.New("research: org has no domain")
	}
	rr := claude.ResearchRequest{
		LeadID:                req.LeadID,
		Model:                 Model,
		System:                systemPrompt,
		Prompt:                userPrompt(req),
		MaxTokens:             MaxTokens,
		SearchMaxUses:         SearchMaxUses,
		FetchMaxUses:          FetchMaxUses,
		FetchMaxContentTokens: FetchMaxContentTokens,
		WebTools:              claude.WebTools20260209,
	}
	res, u, err := c.Research(ctx, rr)
	out.WebTools, out.Usage = rr.WebTools, u
	if errors.Is(err, claude.ErrBadRequest) {
		rr.WebTools = claude.WebToolsBasic
		var u2 claude.Usage
		res, u2, err = c.Research(ctx, rr)
		out.WebTools, out.Usage = rr.WebTools, addUsage(out.Usage, u2)
	}
	out.CostMicros = costOf(out.Usage)
	if err != nil {
		return out, fmt.Errorf("research: org %d: %w", req.Org.ID, err)
	}
	cands, total, err := parse(res.Text)
	out.Candidates = total
	if err != nil {
		return out, fmt.Errorf("research: org %d: %w", req.Org.ID, err)
	}
	vr, err := v.verify(ctx, req, cands)
	out.Routes, out.Phones = vr.routes, vr.phones
	out.Verified = len(out.Routes) + len(out.Phones)
	if err != nil {
		return out, fmt.Errorf("research: org %d: verify: %w", req.Org.ID, err)
	}
	return out, nil
}

func addUsage(a, b claude.Usage) claude.Usage {
	if a.Model == "" {
		a.Model = b.Model
	}
	a.InputTokens += b.InputTokens
	a.OutputTokens += b.OutputTokens
	a.CacheReadTokens += b.CacheReadTokens
	a.CacheWriteTokens += b.CacheWriteTokens
	a.WebSearches += b.WebSearches
	a.WebFetches += b.WebFetches
	return a
}

// costOf prices u; an unbilled or fake usage, or one with no price, is 0 (Capped has
// already refused to go on with an unpriced billed model).
func costOf(u claude.Usage) int64 {
	if u.Model == "" || u.Model == claude.FakeModel {
		return 0
	}
	c, err := claude.Cost(u)
	if err != nil {
		return 0
	}
	return c
}

const systemPrompt = `You find an organisation's published contact details on the public web, for a business that wants to write to it or ring it.

Rules:
- An email address counts only when it is a published, impersonal role address (such as info@, office@, contact@, hello@, sales@) on the organisation's own domain, printed on the organisation's own website.
- Never give a named person's address (such as jane.smith@ or j.smith@), even when it is published.
- A phone counts only when it is a business number printed on the organisation's own website. Prefer a direct line or a decision-maker's line over the switchboard. A role is a job title, never a person's name.
- source_url is the exact address of the page on the organisation's own website where the value is printed. quote is the exact text around it on that page.
- Return nothing rather than guess. An empty list is a good answer.

Answer with one JSON object and nothing else, in this shape:
{"candidates":[{"kind":"email"|"phone","value":"...","source_url":"https://...","quote":"...","line_kind":"direct"|"decision_maker"|"switchboard"|"","role":"..."}]}`

func userPrompt(req Request) string {
	var want []string
	if req.Want.Email {
		want = append(want, "a published role email address")
	}
	if req.Want.Phone {
		want = append(want, "a published business phone number")
	}
	country := req.Org.Country
	if country == "" {
		country = "unknown"
	}
	return fmt.Sprintf("Organisation: %s\nWebsite domain: %s\nCountry: %s\nFind: %s.",
		oneLine(req.Org.Name), oneLine(req.Org.Domain), oneLine(country), strings.Join(want, " and "))
}

// oneLine keeps a stored value from breaking the prompt's layout.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// wire is the answer as the model writes it. The quote is read here only so a malformed
// one cannot fail the parse, and goes no further.
type wire struct {
	Candidates []struct {
		Kind      string `json:"kind"`
		Value     string `json:"value"`
		SourceURL string `json:"source_url"`
		Quote     string `json:"quote"`
		LineKind  string `json:"line_kind"`
		Role      string `json:"role"`
	} `json:"candidates"`
}

// errNotJSON is the answer-parse failure. Its text never quotes the answer.
var errNotJSON = fmt.Errorf("research: answer is not the candidates JSON: %w", ai.ErrBadAnswer)

// parse reads the candidates from the model's final text (one JSON object, possibly in a
// code fence or after a line of prose). It returns at most MaxCandidates and the total.
func parse(text string) ([]candidate, int, error) {
	start, end := strings.Index(text, "{"), strings.LastIndex(text, "}")
	if start < 0 || end < start {
		return nil, 0, errNotJSON
	}
	var w wire
	if err := json.Unmarshal([]byte(text[start:end+1]), &w); err != nil {
		return nil, 0, errNotJSON // the decoder's message can carry answer text: never wrapped
	}
	out := make([]candidate, 0, min(len(w.Candidates), MaxCandidates))
	for i, c := range w.Candidates {
		if i == MaxCandidates {
			break
		}
		out = append(out, candidate{
			kind:      strings.ToLower(strings.TrimSpace(c.Kind)),
			value:     strings.TrimSpace(c.Value),
			sourceURL: strings.TrimSpace(c.SourceURL),
			lineKind:  lineKind(c.LineKind),
			role:      role(c.Role),
		})
	}
	return out, len(w.Candidates), nil
}

func lineKind(s string) string {
	switch s = strings.ToLower(strings.TrimSpace(s)); s {
	case LineDirect, LineDecisionMaker, LineSwitchboard:
		return s
	}
	return ""
}

func role(s string) string {
	s = oneLine(s)
	if r := []rune(s); len(r) > maxRoleRunes {
		s = strings.TrimSpace(string(r[:maxRoleRunes]))
	}
	return s
}
