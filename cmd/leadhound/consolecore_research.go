package main

import (
	"context"
	"errors"
	"log/slog"
	"slices"

	"github.com/gremy/webbership-leadhound/internal/console"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
)

// The console's contact research (CP3b), for the console wiring to call: the console never
// holds the Claude key and never calls Claude; it only queues an attempt for the research
// job and reads attempts back. The batch refusal's per-lead list is noStarredText of the
// RefuseNoStarred refusal newBatch returns (LeadRefusal.Starred).

// requestResearch queues research for lead leadID by trigger (leadstore.ResearchStar after a
// star, leadstore.ResearchButton for "Look again"), as the operator, under the effective
// leads config. A NoRequest in the answer is plain words for the page, not an error.
func (b *backend) requestResearch(ctx context.Context, leadID int64, trigger string) (ResearchAsk, error) {
	setup, err := b.leadsSetup(ctx, leadsForProbe)
	if err != nil {
		return ResearchAsk{}, err
	}
	return requestLeadResearch(ctx, b.lq(), setup.leads, leadID, trigger, b.now())
}

// leadResearch is the lead page's research status: the org's latest attempt and when
// "Look again" may next run.
func (b *backend) leadResearch(ctx context.Context, leadID int64) (leadstore.LeadResearch, error) {
	return b.lq().ResearchForLead(ctx, leadID)
}

// researchAction is the request file a console research request writes
// (<data dir>/run/leads-research), as Run now and the mailbox check do: its .path watcher
// starts leadhound-leads-research.service, the one unit that holds the Anthropic key.
const researchAction = "leads-research"

// researchOnStar is what a star adds after the lead is starred: a research request when
// the research decision (researchRules.forLead) gives something the lead lacks to look for,
// and the request file when one was queued. It never fails the star: an error is logged as a count
// and the star stands. It returns the words to add to the star's line ("" for none).
func (b *backend) researchOnStar(ctx context.Context, setup leadsSetup, leadID int64) string {
	ask, err := requestLeadResearch(ctx, b.lq(), setup.leads, leadID, leadstore.ResearchStar, b.now())
	if err != nil {
		slog.Default().Warn("console research on star", "failed", 1)
		return ""
	}
	if !ask.Created {
		return ""
	}
	words := " " + starQueuedWords(ask)
	if err := writeRunRequest(setup.dataDir, researchAction, b.now()); err != nil {
		slog.Default().Warn("console research request file", "failed", 1)
		words += " It runs on the research job's next run."
	}
	return words
}

// starQueuedWords is the sentence a star's line gains when it queued research: one sentence,
// so the line reads as two ("Lead 5 starred: it is last in your order. It has no email yet,
// so a search for one is queued for the research job.").
func starQueuedWords(ask ResearchAsk) string {
	switch {
	case slices.Contains(ask.Want, leadstore.WantEmail):
		return "It has no email yet, so a search for one is queued for the research job."
	case ask.CallFirst:
		return "Its country takes calls first, so a search for a business phone is queued for the research job."
	}
	return "It has no phone yet, so a search for a business phone is queued for the research job."
}

// lookAgain is the lead page's "Look again": a research request by the button, refused in
// plain words when it may not run (within 90 days of the last attempt that ran, with the
// date it may run; a country that is off; a lead past new, suppressed or deferred).
func (b *backend) lookAgain(ctx context.Context, setup leadsSetup, leadID int64) (console.LeadOutcome, error) {
	ask, err := requestLeadResearch(ctx, b.lq(), setup.leads, leadID, leadstore.ResearchButton, b.now())
	switch {
	case errors.Is(err, leadstore.ErrLeadNotFound):
		return console.LeadOutcome{}, refused("There is no lead %d.", leadID)
	case err != nil:
		return console.LeadOutcome{}, err
	case ask.NoRequest != "":
		return console.LeadOutcome{}, refused("Not looked again: %s.", ask.NoRequest)
	case !ask.Created:
		return console.LeadOutcome{Message: "A search for a contact is already waiting or running; nothing new was asked."}, nil
	}
	msg := "Looking again: a search for " + wantWords(ask.Want) + " is queued for the research job."
	if err := writeRunRequest(setup.dataDir, researchAction, b.now()); err != nil {
		slog.Default().Warn("console research request file", "failed", 1)
		msg += " It runs on the research job's next run."
	}
	return console.LeadOutcome{Message: msg}, nil
}

// consoleResearch is the lead page's "Contact research" from the org's latest attempt: the
// status, what it looks for, when, when it may next run, and whether "Look again" is
// offered: a new lead for which the research decision (researchRules.forLead, the rule the
// request itself applies) gives something it lacks to look for.
func consoleResearch(lr leadstore.LeadResearch, rr researchRules, state string, l researchLead) console.LeadResearch {
	want := rr.forLead(l, leadstore.ResearchButton).Want
	out := console.LeadResearch{NextAt: lr.NextAt, CanAsk: state == leadstore.LeadNew && want != nil, Want: want}
	if r := lr.Latest; r != nil {
		out.Status, out.Want, out.Verified = r.Status, r.Want, r.Verified
		switch r.Status {
		case leadstore.ResearchQueued:
			out.At = new(r.RequestedAt)
		case leadstore.ResearchLooking:
			out.At = r.StartedAt
		default:
			out.At = r.FinishedAt
		}
	}
	return out
}
