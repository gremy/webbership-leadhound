package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/gremy/webbership-leadhound/internal/console"
	"github.com/gremy/webbership-leadhound/internal/leadrate"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
)

// The operator's own call on a lead from its page (lead_label, D9's answer key).

// label sets or clears the operator's call on one lead. A call keeps the lead's facts as the
// relevance question reads them now (rateInput over RateLeadByID, as leads rate builds
// them), so the eval asks about what he saw; a clear writes a cleared call, and only when
// there is a call to clear. The outcome names the lead by its domain (labelName); the count of leads
// labelled is the call block's own, so the outcome does not repeat it.
func (b *backend) label(ctx context.Context, setup leadsSetup, a console.LeadAction) (console.LeadOutcome, error) {
	lq := b.lq()
	l, err := lq.RateLeadByID(ctx, a.ID)
	if errors.Is(err, leadstore.ErrLeadNotFound) {
		return console.LeadOutcome{}, refused("There is no lead %d.", a.ID)
	}
	if err != nil {
		return console.LeadOutcome{}, err
	}
	var msg string
	name := labelName(l, a.ID)
	switch a.Value {
	case console.LabelClear:
		cur, err := lq.LatestLabels(ctx, []int64{a.ID})
		if err != nil {
			return console.LeadOutcome{}, err
		}
		if c, ok := cur[a.ID]; !ok || c.Band == nil {
			return console.LeadOutcome{Message: fmt.Sprintf("%s has no call to clear; nothing changed.", name)}, nil
		}
		if _, err := lq.SetLeadLabel(ctx, a.ID, nil, nil, leadstore.ActorOperator); err != nil {
			return console.LeadOutcome{}, err
		}
		msg = fmt.Sprintf("Your call on %s is cleared.", name)
	default:
		if _, ok := leadrate.Weight(a.Value); !ok {
			return console.LeadOutcome{}, refused("Choose strong, likely, maybe, unlikely or no, or Clear; your call was not changed.")
		}
		doc, err := callInput(l, setup)
		if err != nil {
			return console.LeadOutcome{}, err
		}
		band := a.Value
		if _, err := lq.SetLeadLabel(ctx, a.ID, &band, doc, leadstore.ActorOperator); err != nil {
			return console.LeadOutcome{}, err
		}
		msg = fmt.Sprintf("Saved your call on %s: %s.", name, band)
	}
	return console.LeadOutcome{Message: msg}, nil
}

// callInput is the document a call on lead l stores: the lead's facts as the relevance
// question reads them now.
func callInput(l leadstore.RateLead, setup leadsSetup) ([]byte, error) {
	doc, err := json.Marshal(rateInput(l, setup.leads))
	if err != nil {
		return nil, fmt.Errorf("console: call input: %w", err)
	}
	return doc, nil
}

// labelName is the lead as the call's outcome names it: its domain, or for a café with no
// website of its own, whose domain is the internal place id, its name (else "lead N").
func labelName(l leadstore.RateLead, id int64) string {
	if !strings.HasPrefix(l.Domain, leadstore.PlaceOrgPrefix) {
		return l.Domain
	}
	if l.SiteName != "" {
		return l.SiteName
	}
	return fmt.Sprintf("lead %d", id)
}
