package main

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/gremy/webbership-leadhound/internal/campaign"
	"github.com/gremy/webbership-leadhound/internal/compliance"
	"github.com/gremy/webbership-leadhound/internal/dial"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/phones"
)

// The call rules (CP5). A call row is callable only when every rule holds, and every place
// that says "callable" (Today's "Calls to make now", the call card, `leads calls list`, the
// lead page, a batch's drafting) asks callVerdictFor, so they cannot disagree:
//
//   - there is a number to dial (an erased fact leaves none);
//   - no active phone suppression of the account holds it (do-not-call, TPS/CTPS, forget);
//   - its phone fact is not marked wrong (phone_status bad: every pending call on it stops);
//   - the country table (internal/compliance) allows the call for this org: a country the law
//     table marks off (AT, PL, CZ, BG), a hold country with no acknowledge, a sole trader where
//     calls go to companies only, an unknown US/CA/AU region, an Irish mobile: never callable,
//     no override;
//   - it is screened clean within each required register's window (phones.Callable; a
//     register with no age counts from the batch's approval);
//   - it is inside the country's calling hours now, in the region's zone where one is needed.
//
// A campaign's own country rule `call: off` is the one soft rule (operator decision, 28 Oct):
// the call stays callable, and every place it shows warns "calls off in the campaign rules:
// your call", so the operator rings or chooses Skipped.

// Rule codes beside compliance's for the rules that are not the country table's.
const (
	ruleNoNumber   = "no_number"
	ruleSuppressed = "suppressed"
	ruleBadNumber  = "bad_number"
	ruleScreening  = "screening"
)

// callFacts is everything one call's verdict reads.
type callFacts struct {
	Country       string // ISO 3166-1 alpha-2 of the lead's org
	Number        string // the number in E.164 ("" when there is none to dial)
	Org           compliance.OrgFacts
	Suppressed    bool // an active phone suppression holds the number
	Bad           bool // the phone fact's latest status is bad
	Screenings    []phones.Screening
	CampaignStart time.Time // the batch's approval: a no-age register's screening must follow it
	Overrides     compliance.CampaignOverrides
}

// callVerdict is whether a call may be made now, and why not in plain words.
type callVerdict struct {
	Callable bool
	Rule     string // the failing rule's code (compliance.Rule* or rule*); "" when callable
	Reason   string // why not, in plain words, no closing full stop; "" when callable
	// CampaignOff is the warning when the campaign's own rules have calls to the country off
	// (warn and let the operator choose); "" when they are on. It never makes a call
	// uncallable.
	CampaignOff string
	Screening   string // the screening status in words ("not required", "TPS: clean, checked 3 Nov" …)
}

// lasting reports whether v's refusal cannot go away by itself: not the calling hours (they
// come round) and not the screening (the operator screens the number before calling).
func (v callVerdict) lasting() bool {
	return !v.Callable && v.Rule != compliance.RuleWindow && v.Rule != ruleScreening
}

// callVerdictFor is the one call rule: every place that says a call is callable asks it.
// The order of the reasons is what the operator can act on first: the number itself, the
// country's law, then the screening, then the hours.
func callVerdictFor(t *compliance.Table, f callFacts, now time.Time) (v callVerdict) {
	regs := t.RegistersFor(f.Country)
	v = callVerdict{Screening: screeningWords(f.Number, f.Screenings, regs, f.CampaignStart, now)}
	// The campaign-rules warning goes only with a call that is or will be callable: a "your
	// call whether to ring" beside a call the law refuses would read as a choice.
	if campaignCallsOff(f.Overrides, f.Country) {
		defer func() {
			if !v.lasting() {
				v.CampaignOff = callsOffCard(f.Country)
			}
		}()
	}
	switch {
	case f.Number == "":
		v.Rule, v.Reason = ruleNoNumber, "There is no number to dial (it was erased, or cannot be dialled)"
		return v
	case f.Suppressed:
		v.Rule, v.Reason = ruleSuppressed, "Their number is on the do-not-call list"
		return v
	case f.Bad:
		v.Rule, v.Reason = ruleBadNumber, "This number was marked wrong on a call"
		return v
	}
	org := f.Org
	if org.PhoneType == "" {
		org.PhoneType = compliance.PhoneType(f.Number)
	}
	ok, hard, rule, reason := t.Verdict(f.Country, compliance.ChannelCall, org, f.Overrides, now)
	window := ""
	if !ok && hard {
		if rule != compliance.RuleWindow {
			v.Rule, v.Reason = rule, strings.TrimSuffix(reason, ".")
			return v
		}
		window = reason
	}
	if sok, why := phones.Callable(f.Number, f.Screenings, regs, f.CampaignStart, now); !sok {
		v.Rule, v.Reason = ruleScreening, strings.ToUpper(why[:1])+why[1:]
		return v
	}
	if window != "" {
		v.Rule, v.Reason = compliance.RuleWindow, strings.TrimSuffix(window, ".")
		return v
	}
	v.Callable = true
	return v
}

// screeningWords is number's screening status for the card: "not required" where the
// country names no register, else each register's latest result and date.
func screeningWords(number string, screenings []phones.Screening, regs []compliance.Register, start, now time.Time) string {
	if len(regs) == 0 {
		return "not required"
	}
	parts := make([]string, 0, len(regs))
	for _, r := range regs {
		name := strings.ToUpper(r.Code)
		var latest *phones.Screening
		for i, s := range screenings {
			if s.Number == number && s.Register == r.Code && !s.CheckedAt.After(now) &&
				(latest == nil || s.CheckedAt.After(latest.CheckedAt)) {
				latest = &screenings[i]
			}
		}
		if latest == nil {
			parts = append(parts, name+": not screened yet")
			continue
		}
		word := latest.Result
		if ok, _ := phones.Callable(number, []phones.Screening{*latest}, []compliance.Register{r}, start, now); !ok && word == phones.Clean {
			word = "clean but too old"
		}
		parts = append(parts, fmt.Sprintf("%s: %s, checked %s", name, word, latest.CheckedAt.Format("2 Jan 2006")))
	}
	return strings.Join(parts, " · ")
}

// campaignCallsOff reports whether a campaign's own country rules have calls to country off
// (a country missing from a campaign's rules has both channels off). No campaign rules (a
// campaign with no spec on file): false.
func campaignCallsOff(ov compliance.CampaignOverrides, country string) bool {
	return ov.Countries != nil && !ov.Countries[countryCode(country)].Call
}

// specOverrides is a campaign spec's country rules as compliance reads them.
func specOverrides(countries map[string]campaign.Country) compliance.CampaignOverrides {
	ov := compliance.CampaignOverrides{Countries: map[string]compliance.CampaignCountry{}}
	for code, c := range countries {
		ov.Countries[countryCode(code)] = compliance.CampaignCountry{Email: c.Email, Call: c.Call, Acknowledge: c.Acknowledge}
	}
	return ov
}

// callRules reads what callVerdictFor needs from the store, once per request, caching what
// many rows share (the campaigns' rules, the account's phone suppressions, batch approvals,
// org entity types).
type callRules struct {
	lq       leadstore.Queries
	table    *compliance.Table
	camps    map[int64]compliance.CampaignOverrides // by campaign id; absent: no campaign rules
	phoneSup map[string]bool                        // PhoneSuppressionValue values
	approved map[int64]time.Time                    // batch id → approved_at (zero: not approved)
	entity   map[int64]string                       // org id → entity type
}

// loadCallRules reads the campaigns' country rules and the account's phone suppressions.
func loadCallRules(ctx context.Context, lq leadstore.Queries) (*callRules, error) {
	cs, err := lq.Campaigns(ctx, leadstore.DefaultAccountID)
	if err != nil {
		return nil, err
	}
	r := &callRules{lq: lq, table: compliance.Default(), camps: map[int64]compliance.CampaignOverrides{},
		phoneSup: map[string]bool{}, approved: map[int64]time.Time{}, entity: map[int64]string{}}
	for _, c := range cs {
		if len(c.Spec) == 0 {
			continue
		}
		var spec struct {
			Countries map[string]campaign.Country `json:"countries"`
		}
		if err := json.Unmarshal(c.Spec, &spec); err != nil {
			return nil, fmt.Errorf("campaign %s: %w", c.Slug, err)
		}
		r.camps[c.ID] = specOverrides(spec.Countries)
	}
	set, err := lq.SuppressionSet(ctx, leadstore.DefaultAccountID)
	if err != nil {
		return nil, err
	}
	for _, v := range set.Phones {
		r.phoneSup[v] = true
	}
	return r, nil
}

// off reports whether calls to country are off in campaign campaignID's own rules (the soft,
// warn-and-choose rule).
func (r *callRules) off(campaignID int64, country string) bool {
	return campaignCallsOff(r.camps[campaignID], country)
}

// suppressed reports whether an active phone suppression holds the number, in its stored
// form or its E.164 form.
func (r *callRules) suppressed(stored, e164 string) bool {
	return (stored != "" && r.phoneSup[leadstore.PhoneSuppressionValue(stored)]) ||
		(e164 != "" && r.phoneSup[leadstore.PhoneSuppressionValue(e164)])
}

// check is call row c's verdict at now.
func (r *callRules) check(ctx context.Context, c leadstore.CallRow, now time.Time) (callVerdict, error) {
	f := callFacts{Country: c.Country, Overrides: r.camps[c.CampaignID]}
	if c.PhoneFactID != nil && c.PhoneNormalized != "" {
		f.Number = phones.E164(c.PhoneNormalized, c.Country)
		if f.Number == "" {
			f.Number = dial.Number(c.PhoneNormalized, c.Country)
			if !strings.HasPrefix(f.Number, "+") {
				f.Number = ""
			}
		}
	}
	if f.Number != "" {
		f.Suppressed = r.suppressed(c.PhoneNormalized, f.Number)
		bad, err := r.lq.PhoneFactBad(ctx, *c.PhoneFactID)
		if err != nil {
			return callVerdict{}, err
		}
		f.Bad = bad
		for _, reg := range r.table.RegistersFor(c.Country) {
			s, ok, err := r.lq.LatestScreening(ctx, f.Number, reg.Code)
			if err != nil {
				return callVerdict{}, err
			}
			if ok {
				f.Screenings = append(f.Screenings, phones.Screening{Number: s.Number, Register: s.Register, Result: s.Result, CheckedAt: s.CheckedAt})
			}
		}
		start, err := r.approvedAt(ctx, c.BatchID)
		if err != nil {
			return callVerdict{}, err
		}
		f.CampaignStart = start
	}
	ent, err := r.entityType(ctx, c.OrgID, c.Country)
	if err != nil {
		return callVerdict{}, err
	}
	f.Org.EntityType = ent
	f.Org.Region = c.Region // a place import stores it (00021); "" leaves US, CA and AU uncallable
	return callVerdictFor(r.table, f, now), nil
}

// approvedAt is batch id's approval time (zero when not approved).
func (r *callRules) approvedAt(ctx context.Context, id int64) (time.Time, error) {
	if t, ok := r.approved[id]; ok {
		return t, nil
	}
	b, err := r.lq.BatchByID(ctx, id)
	if err != nil {
		return time.Time{}, err
	}
	var t time.Time
	if b.ApprovedAt != nil {
		t = *b.ApprovedAt
	}
	r.approved[id] = t
	return t, nil
}

// entityType is org orgID's entity type, read only where the country treats a sole trader
// differently on calls (DK, NL, LV and the operator-opened countries): compliance.EntityType
// over the org's kept page texts. "" (unknown, treated as an individual) where nothing marks
// it; "" also where the country does not care, which the verdict then never reads.
func (r *callRules) entityType(ctx context.Context, orgID int64, country string) (string, error) {
	if e, ok := r.entity[orgID]; ok {
		return e, nil
	}
	e, err := orgEntityType(ctx, r.lq, r.table, orgID, country)
	if err != nil {
		return "", err
	}
	r.entity[orgID] = e
	return e, nil
}

// orgEntityType is org orgID's entity type for country's call rule; "" where the country
// treats sole traders as companies on calls (no page is read then).
func orgEntityType(ctx context.Context, lq leadstore.Queries, t *compliance.Table, orgID int64, country string) (string, error) {
	c, ok := t.Lookup(country)
	if !ok || c.Individuals.Call == compliance.IndividualsSame || orgID == 0 {
		return "", nil
	}
	bodies, err := lq.EnrichBodies(ctx, orgID)
	if err != nil {
		return "", err
	}
	texts := make([]string, 0, len(bodies))
	for _, b := range bodies {
		texts = append(texts, pageText(b.Body))
	}
	return t.EntityType(texts, country), nil
}

var (
	tagRe   = regexp.MustCompile(`(?s)<script.*?</script>|<style.*?</style>|<[^>]*>`)
	spaceRe = regexp.MustCompile(`\s+`)
)

// pageText is an HTML body's visible text, roughly: tags, scripts and styles dropped, spaces
// collapsed. Enough for the legal-form words EntityType looks for.
func pageText(body []byte) string {
	s := tagRe.ReplaceAllString(string(body), " ")
	s = strings.NewReplacer("&amp;", "&", "&nbsp;", " ", "&#160;", " ").Replace(s)
	return spaceRe.ReplaceAllString(s, " ")
}

// callsOffShort is the short note a call row carries where its country's calls are off in
// the campaign's own rules: "calls off for IE in the campaign rules".
func callsOffShort(country string) string {
	if c := countryCode(country); c != "" {
		return "calls off for " + c + " in the campaign rules"
	}
	return "calls off for this country in the campaign rules"
}

// callsOffCard is the call card's warning where the country's calls are off in the
// campaign's own rules.
func callsOffCard(country string) string {
	where := "this country"
	if c := countryCode(country); c != "" {
		where = countryName(c)
	}
	return "Calls to " + where + " are off in this campaign's rules. Your call whether to ring; if you don't, choose Skipped."
}

// callsOffLine counts the calls drafted where the campaign has calls off, by country:
// "Calls drafted where the campaign has calls off: 1 (IE): your call." "" for none.
func callsOffLine(byCountry map[string]int) string {
	total := 0
	var codes []string
	for _, c := range slices.Sorted(maps.Keys(byCountry)) {
		total += byCountry[c]
		codes = append(codes, cmp.Or(c, "country not known"))
	}
	if total == 0 {
		return ""
	}
	return fmt.Sprintf("Calls drafted where the campaign has calls off: %d (%s): your call.", total, strings.Join(codes, ", "))
}

// draftCallVerdict is the call rule's verdict for drafting lead c's calls to phone fact f
// under plan's campaign rules at now: the batch is not approved yet and the number not yet
// screened, so only a lasting refusal (callVerdict.lasting) leaves the call out.
func draftCallVerdict(ctx context.Context, lq leadstore.Queries, plan batchPlan, c leadstore.Candidate, f leadstore.Fact, now time.Time) (callVerdict, error) {
	t := compliance.Default()
	ent, err := orgEntityType(ctx, lq, t, c.OrgID, c.Country)
	if err != nil {
		return callVerdict{}, err
	}
	facts := callFacts{Country: c.Country, Number: phones.E164(f.Normalized, c.Country), Overrides: plan.overrides,
		Org: compliance.OrgFacts{EntityType: ent, Region: c.Region}}
	return callVerdictFor(t, facts, now), nil
}
