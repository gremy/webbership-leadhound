package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"slices"
	"time"

	"github.com/gremy/webbership-leadhound/internal/console"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
)

// A lead's proposals, as `leads proposals` and the lead page's forms run them.

func init() { leadsCommands["proposals"] = runLeadsProposals }

const leadsProposalsUsage = "usage: leadhound leads proposals add LEAD --title T [--url U] [--value-cents N] [--currency C] [--status S] [--note N] | " +
	"status PROPOSAL STATUS [--note N] | list LEAD"

// addLeadProposal adds a proposal: one the store takes as sent moves the lead's stage on.
func addLeadProposal(ctx context.Context, lq leadstore.Queries, p console.NewProposal, now time.Time) (int64, error) {
	id, err := lq.AddProposal(ctx, leadstore.NewProposal{LeadID: p.LeadID, Title: p.Title, URL: p.URL, ValueCents: p.ValueCents,
		Currency: p.Currency, Status: p.Status, Note: p.Note, Actor: leadstore.ActorOperator}, now)
	return id, proposalErr(err)
}

// setLeadProposalStatus records a proposal's new status. A LeadID names the lead it must belong to;
// the CLI names only the proposal.
func setLeadProposalStatus(ctx context.Context, lq leadstore.Queries, c console.ProposalStatusChange, now time.Time) error {
	if c.LeadID != 0 {
		own, err := lq.ProposalsFor(ctx, c.LeadID)
		if err != nil {
			return err
		}
		if !slices.ContainsFunc(own, func(p leadstore.Proposal) bool { return p.ID == c.ProposalID }) {
			return console.ErrNotFound
		}
	}
	return proposalErr(lq.SetProposalStatus(ctx, c.ProposalID, c.Status, c.Note, leadstore.ActorOperator, now))
}

func proposalErr(err error) error {
	if errors.Is(err, leadstore.ErrProposalNotFound) {
		return console.ErrNotFound
	}
	return crmErr(err)
}

func runLeadsProposals(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return usageError(leadsProposalsUsage)
	}
	switch args[0] {
	case "add":
		return runProposalsAdd(ctx, args[1:])
	case "status":
		return runProposalsStatus(ctx, args[1:])
	case "list":
		return runProposalsList(ctx, args[1:])
	}
	return usageError(leadsProposalsUsage)
}

func runProposalsAdd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("leads proposals add", flag.ContinueOnError)
	title := fs.String("title", "", "its title")
	link := fs.String("url", "", "its page, https")
	value := fs.Int64("value-cents", -1, "its value in cents")
	currency := fs.String("currency", "", "GBP, EUR, RON or USD")
	status := fs.String("status", "", "sent, accepted, declined or withdrawn")
	note := fs.String("note", "", "your note")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageError(leadsProposalsUsage)
	}
	lead, err := parsePositive(pos[0], "lead", leadsProposalsUsage)
	if err != nil {
		return err
	}
	s, _, err := leadsOpen(ctx, leadsForProbe)
	if err != nil {
		return err
	}
	defer s.Close()
	p := console.NewProposal{LeadID: lead, Title: *title, URL: *link, Currency: *currency, Status: *status, Note: *note}
	if flagWasSet(fs, "value-cents") {
		p.ValueCents = value
	}
	id, err := addLeadProposal(ctx, leadstore.NewQueries(s.Pool()), p, leadsNow())
	if err != nil {
		return cliAnswer(err)
	}
	_, _ = fmt.Fprintf(stdout, "Proposal %d added to lead %d.\n", id, lead)
	return nil
}

func runProposalsStatus(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("leads proposals status", flag.ContinueOnError)
	note := fs.String("note", "", "your note")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 2 {
		return usageError(leadsProposalsUsage)
	}
	id, err := parsePositive(pos[0], "proposal", leadsProposalsUsage)
	if err != nil {
		return err
	}
	s, _, err := leadsOpen(ctx, leadsForProbe)
	if err != nil {
		return err
	}
	defer s.Close()
	c := console.ProposalStatusChange{ProposalID: id, Status: pos[1], Note: *note}
	if err := setLeadProposalStatus(ctx, leadstore.NewQueries(s.Pool()), c, leadsNow()); err != nil {
		return cliAnswer(err)
	}
	_, _ = fmt.Fprintf(stdout, "Proposal %d is now %s.\n", id, pos[1])
	return nil
}

func runProposalsList(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return usageError(leadsProposalsUsage)
	}
	lead, err := parsePositive(args[0], "lead", leadsProposalsUsage)
	if err != nil {
		return err
	}
	s, _, err := leadsOpen(ctx, leadsForProbe)
	if err != nil {
		return err
	}
	defer s.Close()
	proposals, err := leadstore.NewQueries(s.Pool()).ProposalsFor(ctx, lead)
	if err != nil {
		return err
	}
	for _, p := range proposals {
		_, _ = fmt.Fprintf(stdout, "%d %s · %s\n", p.ID, p.Title, p.Status)
	}
	return nil
}
