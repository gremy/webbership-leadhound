package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"strings"

	"github.com/gremy/webbership-leadhound/internal/console"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/leadview"
)

// The proof library and an org's sector, as `leads proof`, `leads sector` and the console run them.

func init() {
	leadsCommands["proof"] = runLeadsProof
	leadsCommands["sector"] = runLeadsSector
}

const (
	leadsProofUsage  = "usage: leadhound leads proof add --title T --line L [--url U] --sector S [--sector S]... [--public] | list | retire PROOF [--reason R]"
	leadsSectorUsage = "usage: leadhound leads sector set LEAD SECTOR"
)

// addProofLine stores a line of proof. The link is stored even when it is not public: a draft reads
// Public, and never carries a link that is not.
func addProofLine(ctx context.Context, lq leadstore.Queries, p console.NewProof) (int64, error) {
	for _, s := range p.Sectors {
		if leadview.SectorWords(s) == "" {
			return 0, refused("One of those sectors is not on the list. Nothing was saved.")
		}
	}
	id, err := lq.AddProof(ctx, leadstore.Proof{Title: p.Title, Line: p.Line, URL: p.URL, Sectors: p.Sectors, Public: p.Public, Actor: leadstore.ActorOperator})
	if errors.Is(err, leadstore.ErrBadProof) {
		return 0, storeRefusal(err, leadstore.ErrBadProof)
	}
	return id, err
}

// retireProofLine takes a proof out of use.
func retireProofLine(ctx context.Context, lq leadstore.Queries, id int64, reason string) error {
	err := lq.RetireProof(ctx, leadstore.DefaultAccountID, id, reason)
	switch {
	case errors.Is(err, leadstore.ErrProofNotFound):
		return console.ErrNotFound
	case errors.Is(err, leadstore.ErrBadProof):
		return storeRefusal(err, leadstore.ErrBadProof)
	}
	return err
}

// setLeadSector records the operator's sector for the org of lead leadID, from the closed list.
func setLeadSector(ctx context.Context, lq leadstore.Queries, leadID int64, sector string) error {
	orgID, err := leadOrgID(ctx, lq, leadID)
	if err != nil {
		return err
	}
	if leadview.SectorWords(sector) == "" {
		return refused("That sector is not on the list. Nothing was saved.")
	}
	err = lq.SetSector(ctx, orgID, sector, leadstore.ActorOperator)
	if errors.Is(err, leadstore.ErrBadSector) {
		return refused("That sector is not on the list. Nothing was saved.")
	}
	return err
}

// sectorOptions is the closed list of sectors as the console offers them.
func sectorOptions() []console.FilterOption {
	out := make([]console.FilterOption, len(leadview.Sectors))
	for i, s := range leadview.Sectors {
		out[i] = console.FilterOption{Value: s.Name, Label: s.Words}
	}
	return out
}

// sectorOptionsOf is names as console options, each with its words.
func sectorOptionsOf(names []string) []console.FilterOption {
	out := make([]console.FilterOption, 0, len(names))
	for _, n := range names {
		out = append(out, console.FilterOption{Value: n, Label: leadview.SectorWords(n)})
	}
	return out
}

func runLeadsProof(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return usageError(leadsProofUsage)
	}
	switch args[0] {
	case "add":
		return runProofAdd(ctx, args[1:])
	case "list":
		return runProofList(ctx, args[1:])
	case "retire":
		return runProofRetire(ctx, args[1:])
	}
	return usageError(leadsProofUsage)
}

func runProofAdd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("leads proof add", flag.ContinueOnError)
	title := fs.String("title", "", "its title")
	line := fs.String("line", "", "the sentence a draft may use")
	link := fs.String("url", "", "its page, https")
	public := fs.Bool("public", false, "its page may go in a draft")
	var sectors stringList
	fs.Var(&sectors, "sector", "a sector it serves (repeatable)")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return usageError(leadsProofUsage)
	}
	s, _, err := leadsOpen(ctx, leadsForProbe)
	if err != nil {
		return err
	}
	defer s.Close()
	id, err := addProofLine(ctx, leadstore.NewQueries(s.Pool()), console.NewProof{Title: *title, Line: *line, URL: *link, Sectors: sectors, Public: *public})
	if err != nil {
		return cliAnswer(err)
	}
	_, _ = fmt.Fprintf(stdout, "Proof %d added.\n", id)
	return nil
}

func runProofList(ctx context.Context, args []string) error {
	if len(args) != 0 {
		return usageError(leadsProofUsage)
	}
	s, _, err := leadsOpen(ctx, leadsForProbe)
	if err != nil {
		return err
	}
	defer s.Close()
	proofs, err := leadstore.NewQueries(s.Pool()).ListProofs(ctx, leadstore.DefaultAccountID)
	if err != nil {
		return err
	}
	for _, p := range proofs {
		link := "spoken line only"
		if p.Public && p.URL != "" {
			link = p.URL
		}
		_, _ = fmt.Fprintf(stdout, "%d %s [%s] %s · %s\n", p.ID, p.Title, strings.Join(p.Sectors, ", "), p.Line, link)
	}
	return nil
}

func runProofRetire(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("leads proof retire", flag.ContinueOnError)
	reason := fs.String("reason", "", "why")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageError(leadsProofUsage)
	}
	id, err := parsePositive(pos[0], "proof", leadsProofUsage)
	if err != nil {
		return err
	}
	s, _, err := leadsOpen(ctx, leadsForProbe)
	if err != nil {
		return err
	}
	defer s.Close()
	if err := retireProofLine(ctx, leadstore.NewQueries(s.Pool()), id, *reason); err != nil {
		return cliAnswer(err)
	}
	_, _ = fmt.Fprintf(stdout, "Proof %d retired. New drafts no longer use it.\n", id)
	return nil
}

func runLeadsSector(ctx context.Context, args []string) error {
	if len(args) != 3 || args[0] != "set" {
		return usageError(leadsSectorUsage)
	}
	lead, err := parsePositive(args[1], "lead", leadsSectorUsage)
	if err != nil {
		return err
	}
	s, _, err := leadsOpen(ctx, leadsForProbe)
	if err != nil {
		return err
	}
	defer s.Close()
	if err := setLeadSector(ctx, leadstore.NewQueries(s.Pool()), lead, args[2]); err != nil {
		return cliAnswer(err)
	}
	_, _ = fmt.Fprintf(stdout, "Lead %d: sector saved.\n", lead)
	return nil
}
