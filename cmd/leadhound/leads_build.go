package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gremy/webbership-leadhound/internal/leadconfig"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/outreach"
	"github.com/gremy/webbership-leadhound/internal/store"
)

func init() { leadsCommands["build"] = runLeadsBuild }

// The run row of a lead build.
const (
	commandLeadsBuild = "leads-build"
	sourceLeads       = "leads"
)

// leadsInTx runs fn over lead queries inside one transaction: committed when fn returns
// nil, rolled back otherwise.
func leadsInTx(ctx context.Context, pool *pgxpool.Pool, fn func(leadstore.Queries) error) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("leads: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // a no-op after Commit
	if err := fn(leadstore.NewQueries(tx)); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("leads: commit: %w", err)
	}
	return nil
}

// leadsOpen loads the config the leads commands need, opens the store and applies the
// console's saved settings (applyLeadSettings), so the command reads the effective config.
func leadsOpen(ctx context.Context, load leadsLoad) (*store.Store, leadsSetup, error) {
	cfg, err := loadConfig("LEADHOUND_DATABASE_URL")
	if err != nil {
		return nil, leadsSetup{}, err
	}
	setup, err := leadsEnv(cfg, load)
	if err != nil {
		return nil, leadsSetup{}, err
	}
	s, err := store.Open(ctx, cfg.DatabaseURL.Reveal(), appName())
	if err != nil {
		return nil, leadsSetup{}, err
	}
	if setup, err = applyLeadSettings(ctx, leadstore.NewQueries(s.Pool()), setup); err != nil {
		s.Close()
		return nil, leadsSetup{}, err
	}
	return s, setup, nil
}

// buildSummary is what one lead build did.
type buildSummary struct {
	Orgs      int
	Leads     int
	Created   int
	Refreshed int                       // new leads rewritten from the latest probe, finding and routes
	Skipped   int                       // new leads whose latest facts make no lead: moved to skipped
	Kept      int                       // leads past new, left as they are
	Reserved  int                       // leads on reserved TLDs (fixture data; never in a real batch)
	ByKind    map[string]map[string]int // lead kind → channel → leads
	NoLead    map[string]int            // reason → orgs
}

// runLeadsBuild is `leads build`: it classifies every org that has a finding (a site's
// last Drupal probe, or an agency's finding) and its routes into a lead, one per org. A
// lead still new is classified again from the latest facts: refreshed, or skipped when
// they no longer make a lead. A lead past new is left as it is.
func runLeadsBuild(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("leads build", flag.ContinueOnError)
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return usageError("usage: leadhound leads build")
	}
	s, setup, err := leadsOpen(ctx, leadsForProbe)
	if err != nil {
		return err
	}
	defer s.Close()
	q := s.Q()
	runID, err := q.StartRun(ctx, commandLeadsBuild, sourceLeads)
	if err != nil {
		return err
	}
	var sum buildSummary
	runErr := leadsInTx(ctx, s.Pool(), func(lq leadstore.Queries) error {
		var err error
		sum, err = buildLeads(ctx, lq, setup.leads)
		return err
	})
	result := store.RunResult{Note: fmt.Sprintf("%d orgs, %d leads, %d new", sum.Orgs, sum.Leads, sum.Created)}
	if runErr != nil {
		result.Note = runErr.Error()
	}
	if err := q.FinishRun(context.WithoutCancel(ctx), runID, result); err != nil && runErr == nil {
		return err
	}
	if runErr != nil {
		return runErr
	}
	printBuildSummary(stdout, sum)
	return nil
}

// buildLeads classifies every org and upserts its lead.
func buildLeads(ctx context.Context, lq leadstore.Queries, cfg leadconfig.Config) (buildSummary, error) {
	sum := buildSummary{ByKind: map[string]map[string]int{}, NoLead: map[string]int{}}
	orgs, err := lq.OrgsForBuild(ctx)
	if err != nil {
		return sum, err
	}
	for _, o := range orgs {
		sum.Orgs++
		var probe *outreach.Probe
		if o.ProbeID != nil {
			probe = &outreach.Probe{ID: *o.ProbeID, IsDrupal: o.IsDrupal, Major: o.Major, Ambiguous: o.Ambiguous}
		}
		var finding *outreach.AgencyFinding
		if o.AgencyFindingID != nil {
			finding = &outreach.AgencyFinding{ID: *o.AgencyFindingID}
		}
		stored, err := lq.RoutesFor(ctx, o.ID)
		if err != nil {
			return sum, err
		}
		routes := make([]outreach.Route, len(stored))
		for i, r := range stored {
			routes[i] = outreach.Route{ID: r.ID, Kind: r.Kind, Value: r.Value, MXOK: r.MXOK}
		}
		if o.LeadID != nil && o.LeadState != leadstore.LeadNew {
			sum.Kept++
			sum.Leads++
			sum.count(o.LeadKind, o.LeadChannel)
			continue
		}
		l, reason, ok := outreach.Classify(outreach.Org{ID: o.ID, Kind: o.Kind, Domain: o.Domain, Country: o.Country}, probe, finding, routes, cfg)
		if !ok {
			sum.NoLead[reason]++
			if o.LeadID != nil {
				moved, err := lq.SkipNewLead(ctx, *o.LeadID, "build: "+reason)
				if err != nil {
					return sum, err
				}
				if moved {
					sum.Skipped++
				} else {
					// It left new after the read above: past new, left as it is.
					sum.Kept++
					sum.Leads++
					sum.count(o.LeadKind, o.LeadChannel)
				}
			}
			continue
		}
		row := leadstore.LeadRow{
			OrgID: l.OrgID, Kind: l.Kind, ProbeID: l.ProbeID, AgencyFindingID: l.AgencyFindingID,
			Channel: l.Channel, RouteID: l.RouteID,
		}
		if o.LeadID != nil {
			changed, err := lq.RefreshLead(ctx, row)
			if err != nil {
				return sum, err
			}
			if changed {
				sum.Refreshed++
			}
		} else {
			if _, created, err := lq.UpsertLead(ctx, row); err != nil {
				return sum, err
			} else if created {
				sum.Created++
			}
		}
		sum.Leads++
		if l.Reserved {
			sum.Reserved++
		}
		sum.count(l.Kind, l.Channel)
	}
	return sum, nil
}

func (sum *buildSummary) count(kind, channel string) {
	if sum.ByKind[kind] == nil {
		sum.ByKind[kind] = map[string]int{}
	}
	sum.ByKind[kind][channel]++
}

func printBuildSummary(w io.Writer, sum buildSummary) {
	_, _ = fmt.Fprintf(w, "leads build: %d leads (%d new · %d refreshed · %d past new, kept) from %d orgs",
		sum.Leads, sum.Created, sum.Refreshed, sum.Kept, sum.Orgs)
	if sum.Skipped > 0 {
		_, _ = fmt.Fprintf(w, " · %d new leads skipped (no longer a lead)", sum.Skipped)
	}
	if sum.Reserved > 0 {
		_, _ = fmt.Fprintf(w, " · fixture leads (reserved names, never in a real batch) %d", sum.Reserved)
	}
	_, _ = fmt.Fprintln(w)
	kinds := append([]string{}, leadconfig.Kinds...)
	for k := range sum.ByKind {
		if !slices.Contains(kinds, k) {
			kinds = append(kinds, k)
		}
	}
	for _, k := range kinds {
		ch := sum.ByKind[k]
		if len(ch) == 0 {
			continue
		}
		var parts []string
		for _, c := range []string{leadconfig.ChannelEmail, leadconfig.ChannelForm, leadconfig.ChannelNone} {
			if ch[c] > 0 {
				parts = append(parts, fmt.Sprintf("%s %d", c, ch[c]))
			}
		}
		_, _ = fmt.Fprintf(w, "  %s: %s\n", k, strings.Join(parts, " · "))
	}
	if len(sum.NoLead) > 0 {
		reasons := make([]string, 0, len(sum.NoLead))
		for r := range sum.NoLead {
			reasons = append(reasons, r)
		}
		slices.Sort(reasons)
		parts := make([]string, len(reasons))
		for i, r := range reasons {
			parts[i] = fmt.Sprintf("%s %d", r, sum.NoLead[r])
		}
		_, _ = fmt.Fprintf(w, "no lead: %s\n", strings.Join(parts, " · "))
	}
}
