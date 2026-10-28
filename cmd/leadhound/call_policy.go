package main

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/gremy/webbership-leadhound/internal/leadstore"
)

// A campaign's country call policy (countries: {XX: {call: off}}) never removes a call step:
// a lead with a callable number gets its call drafted wherever it is, and where the policy is
// off every place the call shows (the batch, the card, Today, the lead page, calls list) says
// so plainly, so the operator decides whether to ring.

// callPolicies are the countries whose call policy is on, by campaign id, from each
// campaign's latest registered spec. A campaign with no spec on file is absent: no warning.
type callPolicies map[int64]map[string]bool

// loadCallPolicies reads the call policies of the operator's campaigns.
func loadCallPolicies(ctx context.Context, lq leadstore.Queries) (callPolicies, error) {
	cs, err := lq.Campaigns(ctx, leadstore.DefaultAccountID)
	if err != nil {
		return nil, err
	}
	out := callPolicies{}
	for _, c := range cs {
		if len(c.Spec) == 0 {
			continue
		}
		var spec struct {
			Countries map[string]struct {
				Call bool `json:"call"`
			} `json:"countries"`
		}
		if err := json.Unmarshal(c.Spec, &spec); err != nil {
			return nil, fmt.Errorf("campaign %s: %w", c.Slug, err)
		}
		on := map[string]bool{}
		for code, cc := range spec.Countries {
			if cc.Call {
				on[countryCode(code)] = true
			}
		}
		out[c.ID] = on
	}
	return out, nil
}

// off reports whether calls to country are off in campaign campaignID's rules.
func (p callPolicies) off(campaignID int64, country string) bool {
	on, ok := p[campaignID]
	return ok && !on[countryCode(country)]
}

// callsOffShort is the short note a call row carries where its country's calls are off:
// "calls off for IE in the campaign rules".
func callsOffShort(country string) string {
	if c := countryCode(country); c != "" {
		return "calls off for " + c + " in the campaign rules"
	}
	return "calls off for this country in the campaign rules"
}

// callsOffCard is the call card's warning where the country's calls are off.
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
