package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/gremy/webbership-leadhound/internal/campaign"
	"github.com/gremy/webbership-leadhound/internal/leadconfig"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/store"
)

func init() { leadsCommands["campaigns"] = runLeadsCampaigns }

const leadsCampaignsUsage = "usage: leadhound leads campaigns list | show SLUG | check | register"

// campaignFlagUsage is the --campaign flag's help, shared by build, batch new and rate.
const campaignFlagUsage = "the campaign's short name (default " + campaign.LegacySlug + ")"

// runLeadsCampaigns is `leads campaigns list | show SLUG | check | register`. check reads
// every campaign file (and the Drupal campaign leads.yaml gives) and checks it without the
// database; register does the same and records each campaign's spec as a new version only
// when it changed; list and show read the database.
func runLeadsCampaigns(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return usageError(leadsCampaignsUsage)
	}
	switch args[0] {
	case "list":
		return runCampaignsList(ctx, args[1:])
	case "show":
		return runCampaignsShow(ctx, args[1:])
	case "check":
		return runCampaignsCheck(ctx, args[1:])
	case "register":
		return runCampaignsRegister(ctx, args[1:])
	}
	return usageError("unknown campaigns subcommand %q; %s", args[0], leadsCampaignsUsage)
}

// noArgs parses a subcommand that takes no flags and n positional arguments.
func noArgs(name string, args []string, n int) ([]string, error) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	pos, err := parseArgs(fs, args)
	if err != nil {
		return nil, err
	}
	if len(pos) != n {
		return nil, usageError(leadsCampaignsUsage)
	}
	return pos, nil
}

// loadCampaigns reads leads.yaml and every campaign spec from LEADHOUND_DATA_DIR, checked,
// writer: claude allowed only where trust says the draft gate passes (nil: nowhere). Any
// problem is a usage error naming every problem, one per line.
func loadCampaigns(trust func(slug string) bool) ([]campaign.Spec, leadconfig.Config, error) {
	cfg, err := loadConfig()
	if err != nil {
		return nil, leadconfig.Config{}, err
	}
	if cfg.DataDir == "" {
		return nil, leadconfig.Config{}, usageError("LEADHOUND_DATA_DIR is not set; the campaigns are read from it")
	}
	lc, err := leadconfig.Load(cfg.DataDir)
	if err != nil {
		return nil, leadconfig.Config{}, usageError("%s (values are not shown; run leads check-config)", strings.Join(configProblems(err), "; "))
	}
	specs, err := checkedCampaigns(cfg.DataDir, &lc, trust)
	return specs, lc, err
}

// privacyMissing is `campaigns check`'s warning for a campaign with a call step while
// leads.yaml has no sender.privacy_url: a warning, not a refusal (the notice is the
// operator's to write).
const privacyMissing = "The privacy notice link is missing: write the notice and set sender.privacy_url before calls start (by 7 Nov)."

// campaignWarnings are the warnings `campaigns check` prints under the specs, one line
// each: every campaign with a call step in any of its sequences (or moving leads onto the
// default call_first) while sender.privacy_url is not set.
func campaignWarnings(specs []campaign.Spec, lc leadconfig.Config) []string {
	if strings.TrimSpace(lc.Sender.PrivacyURL) != "" {
		return nil
	}
	var out []string
	for _, sp := range specs {
		seqs := [][]campaign.Step{sp.Sequence}
		for _, seq := range sp.Sequences {
			seqs = append(seqs, seq)
		}
		calls := false
		for _, seq := range seqs {
			calls = calls || slices.ContainsFunc(seq, func(st campaign.Step) bool { return st.Type == campaign.StepCall })
		}
		if calls {
			out = append(out, sp.Slug+": warning: "+privacyMissing)
		}
	}
	return out
}

// checkedCampaigns is campaign.LoadAll, and for each campaign with its own folder its
// templates (campaignTemplates: every email step of its sequences and the call card), its
// problems as one usage error, one per line.
func checkedCampaigns(dataDir string, lc *leadconfig.Config, trust func(slug string) bool) ([]campaign.Spec, error) {
	specs, err := campaign.LoadAllWith(dataDir, lc, campaign.LoadOptions{DraftsTrusted: trust})
	if err != nil {
		return nil, usageError("the campaigns do not pass their checks:\n%s", campaignProblems(err))
	}
	var errs []error
	for _, sp := range specs {
		if _, ok, err := campaignFile(dataDir, sp.Slug); err != nil {
			errs = append(errs, err)
		} else if ok {
			if _, err := campaignTemplates(dataDir, sp); err != nil {
				errs = append(errs, fmt.Errorf("campaign %q: %w", sp.Slug, err))
			}
		}
	}
	if len(errs) > 0 {
		return nil, usageError("the campaigns do not pass their checks:\n%s", campaignProblems(errors.Join(errs...)))
	}
	return specs, nil
}

// campaignProblems is err's every problem, one per line: a joined error splits into its
// parts, each of them possibly joined again.
func campaignProblems(err error) string {
	var lines []string
	var walk func(error)
	walk = func(e error) {
		if j, ok := e.(interface{ Unwrap() []error }); ok {
			for _, x := range j.Unwrap() {
				walk(x)
			}
			return
		}
		for _, l := range strings.Split(e.Error(), "\n") {
			if l = strings.TrimSpace(l); l != "" {
				lines = append(lines, "  "+l)
			}
		}
	}
	walk(err)
	return strings.Join(lines, "\n")
}

func runCampaignsCheck(ctx context.Context, args []string) error {
	if _, err := noArgs("leads campaigns check", args, 0); err != nil {
		return err
	}
	// The draft gate lives in the database; without one, writer: claude stays refused.
	var gates *campaignGates
	if cfg, err := loadConfig(); err == nil && cfg.DatabaseURL.Reveal() != "" {
		gates = lazyCampaignGates(ctx, openLeadsStore)
		defer gates.close()
	}
	specs, lc, err := loadCampaigns(gates.trust())
	if err != nil {
		return gates.explain(err)
	}
	printCampaignSpecs(stdout, specs)
	for _, w := range campaignWarnings(specs, lc) {
		_, _ = fmt.Fprintln(stdout, w)
	}
	return nil
}

func printCampaignSpecs(w io.Writer, specs []campaign.Spec) {
	for _, s := range specs {
		_, _ = fmt.Fprintf(w, "%s: ok · %s · %s · %d steps · writer %s\n", s.Slug, s.Name,
			plural(len(s.Kinds), "kind", "kinds"), len(s.Sequence), s.Writer)
	}
}

func runCampaignsRegister(ctx context.Context, args []string) error {
	if _, err := noArgs("leads campaigns register", args, 0); err != nil {
		return err
	}
	s, err := openLeadsStore(ctx)
	if err != nil {
		return err
	}
	defer s.Close()
	gates := newCampaignGates(ctx, leadstore.NewQueries(s.Pool()))
	specs, _, err := loadCampaigns(gates.trust())
	if err != nil {
		return gates.explain(err)
	}
	return registerCampaigns(ctx, leadstore.NewQueries(s.Pool()), specs, stdout)
}

// registerCampaigns records each spec as its campaign's latest version, from its file, by
// the operator, and says per campaign whether a new version was written.
func registerCampaigns(ctx context.Context, lq leadstore.Queries, specs []campaign.Spec, w io.Writer) error {
	for _, sp := range specs {
		b, sha, err := campaign.Canonical(sp)
		if err != nil {
			return err
		}
		_, written, err := lq.RegisterCampaign(ctx, leadstore.DefaultAccountID, sp.Slug, sp.Name, b, sha,
			leadstore.CampaignFromFile, leadstore.ActorOperator)
		if err != nil {
			return err
		}
		what := "unchanged"
		if written {
			what = "new version"
		}
		_, _ = fmt.Fprintf(w, "%s: %s (%s)\n", sp.Slug, what, shortHash(sha))
	}
	return nil
}

func shortHash(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// openLeadsStore opens the store for a command that needs no leads.yaml.
func openLeadsStore(ctx context.Context) (*store.Store, error) {
	cfg, err := loadConfig("LEADHOUND_DATABASE_URL")
	if err != nil {
		return nil, err
	}
	return store.Open(ctx, cfg.DatabaseURL.Reveal(), appName())
}

func runCampaignsList(ctx context.Context, args []string) error {
	if _, err := noArgs("leads campaigns list", args, 0); err != nil {
		return err
	}
	s, err := openLeadsStore(ctx)
	if err != nil {
		return err
	}
	defer s.Close()
	return listCampaigns(ctx, leadstore.NewQueries(s.Pool()), stdout)
}

// listCampaigns prints each campaign of the operator's account: slug, name, its latest
// version and whether it is retired.
func listCampaigns(ctx context.Context, lq leadstore.Queries, w io.Writer) error {
	cs, err := lq.Campaigns(ctx, leadstore.DefaultAccountID)
	if err != nil {
		return err
	}
	if len(cs) == 0 {
		_, _ = fmt.Fprintln(w, "No campaigns.")
		return nil
	}
	for _, c := range cs {
		_, _ = fmt.Fprintf(w, "%s · %s · %s", c.Slug, c.Name, versionWords(c))
		if c.RetiredAt != nil {
			_, _ = fmt.Fprintf(w, " · retired %s", c.RetiredAt.Format(time.DateOnly))
		}
		_, _ = fmt.Fprintln(w)
	}
	return nil
}

func versionWords(c leadstore.Campaign) string {
	if c.SpecSHA256 == "" {
		return "no version registered (leads campaigns register)"
	}
	at := ""
	if c.VersionAt != nil {
		at = " of " + c.VersionAt.Format(time.DateTime)
	}
	return "version " + shortHash(c.SpecSHA256) + at
}

func runCampaignsShow(ctx context.Context, args []string) error {
	pos, err := noArgs("leads campaigns show", args, 1)
	if err != nil {
		return err
	}
	s, err := openLeadsStore(ctx)
	if err != nil {
		return err
	}
	defer s.Close()
	return showCampaign(ctx, leadstore.NewQueries(s.Pool()), pos[0], stdout)
}

// showCampaign prints campaign slug and its latest spec, indented JSON; an unknown slug is a
// usage error.
func showCampaign(ctx context.Context, lq leadstore.Queries, slug string, w io.Writer) error {
	c, err := lq.CampaignBySlug(ctx, leadstore.DefaultAccountID, slug)
	if errors.Is(err, leadstore.ErrCampaignNotFound) {
		return usageError("no campaign is called %q; leads campaigns list names them", slug)
	}
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(w, "%s · %s · %s\n", c.Slug, c.Name, versionWords(c))
	if c.RetiredAt != nil {
		_, _ = fmt.Fprintf(w, "retired %s\n", c.RetiredAt.Format(time.DateTime))
	}
	if len(c.Spec) == 0 {
		return nil
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, c.Spec, "", "  "); err != nil {
		return fmt.Errorf("leads campaigns show: %w", err)
	}
	_, _ = fmt.Fprintln(w, buf.String())
	return nil
}

// activeCampaign is campaign slug of the operator's account; an unknown or retired slug is a
// usage error.
func activeCampaign(ctx context.Context, lq leadstore.Queries, slug string) (leadstore.Campaign, error) {
	c, err := lq.CampaignBySlug(ctx, leadstore.DefaultAccountID, slug)
	if errors.Is(err, leadstore.ErrCampaignNotFound) {
		return leadstore.Campaign{}, usageError("no campaign is called %q; leads campaigns list names them", slug)
	}
	if err != nil {
		return leadstore.Campaign{}, err
	}
	if c.RetiredAt != nil {
		return leadstore.Campaign{}, usageError("campaign %q is retired", slug)
	}
	return c, nil
}
