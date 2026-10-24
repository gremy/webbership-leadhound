package main

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"golang.org/x/time/rate"

	"github.com/gremy/webbership-leadhound/internal/config"
	"github.com/gremy/webbership-leadhound/internal/drupal"
	"github.com/gremy/webbership-leadhound/internal/fetch"
	"github.com/gremy/webbership-leadhound/internal/leadconfig"
)

func init() { commands["leads"] = runLeads }

// leadsCommands maps a `leads` subcommand name to its function. Each leads_*.go file adds
// its own entry from init, as the top-level commands do, so no leads name can collide with
// a top-level command.
var leadsCommands = map[string]command{}

// runLeads is `leadhound leads <sub> [flags]`: it runs the registered subcommand with the
// arguments after its name.
func runLeads(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return leadsUsageError("")
	}
	sub, ok := leadsCommands[args[0]]
	if !ok {
		return leadsUsageError(fmt.Sprintf("unknown subcommand %q; ", args[0]))
	}
	return sub(ctx, args[1:])
}

// leadsUsageError is a usage error that starts with prefix and lists the registered
// subcommands in order.
func leadsUsageError(prefix string) error {
	names := make([]string, 0, len(leadsCommands))
	for name := range leadsCommands {
		names = append(names, name)
	}
	slices.Sort(names)
	list := strings.Join(names, ", ")
	if list == "" {
		list = "(none yet)"
	}
	return usageError("%susage: leadhound leads <sub> [flags]; subcommands: %s", prefix, list)
}

// leadsLoad says how much of leads.yaml a subcommand needs checked.
type leadsLoad int

const (
	// leadsForProbe is for the site and contact commands: leadconfig.LoadForProbe, so a
	// data directory without templates still serves them.
	leadsForProbe leadsLoad = iota
	// leadsForOutreach is for the commands that draft or send: leadconfig.Load, templates
	// included.
	leadsForOutreach
)

// leadsSetup is what every leads subcommand reads before it starts.
type leadsSetup struct {
	dataDir string            // LEADHOUND_DATA_DIR
	leads   leadconfig.Config // leads.yaml, checked as the leadsLoad asked
	env     leadconfig.Env    // the outreach and probe variables
}

// leadsEnv loads leads.yaml from LEADHOUND_DATA_DIR, checked as load says, and the outreach
// variables. A missing data directory, a broken file or an unusable variable is a usage
// error listing every problem at once.
func leadsEnv(cfg config.Config, load leadsLoad) (leadsSetup, error) {
	if cfg.DataDir == "" {
		return leadsSetup{}, usageError("LEADHOUND_DATA_DIR is not set; the leads commands read leads.yaml from it")
	}
	read := leadconfig.LoadForProbe
	if load == leadsForOutreach {
		read = leadconfig.Load
	}
	lc, err := read(cfg.DataDir)
	if err != nil {
		return leadsSetup{}, usageError("%v", err)
	}
	env, problems := leadconfig.LoadEnv(os.Getenv)
	if len(problems) > 0 {
		return leadsSetup{}, usageError("%s", strings.Join(problems, "; "))
	}
	return leadsSetup{dataDir: cfg.DataDir, leads: lc, env: env}, nil
}

// Site client settings: each organisation's site gets its own client, so these are per site.
const (
	siteTimeout = 10 * time.Second
	// siteMaxRedirects counts the requests in a chain, so at most 3 redirects are followed.
	siteMaxRedirects = 4
	siteRobotsBudget = 4
	siteBudget       = 4
)

// siteLimiter is the run-wide limiter every site client shares: LEADHOUND_PROBE_RATE
// requests per second, with a burst of LEADHOUND_PROBE_CONCURRENCY.
func siteLimiter(env leadconfig.Env) *rate.Limiter {
	return rate.NewLimiter(rate.Limit(env.ProbeRate), env.ProbeConcurrency)
}

// siteClients returns a constructor for one organisation's site client; call it once per
// site with the run's recorder. Every client has its own breaker (robots and site budgets of
// four requests each, the per-site cap, so one site's failures never stop the run), the Site
// profile, the public-only guard, a 10 s timeout, at most 3 redirects and a body cap of
// drupal.MaxBody. With --dry-run it
// reads fixtures and is not paced; otherwise it waits on lim, shared by the whole run, with
// no added jitter. errLiveOff means LEADHOUND_LIVE is not 1 (ats is not enough), so the
// command must not run.
func siteClients(cfg config.Config, f fetchFlags, lim *rate.Limiter) (func(fetch.Recorder) *fetch.Client, error) {
	opts := []fetch.Option{
		fetch.WithPublicOnly(),
		fetch.WithProfile(fetch.Site),
		fetch.WithTimeout(siteTimeout),
		fetch.WithMaxRedirects(siteMaxRedirects),
		fetch.WithMaxBody(drupal.MaxBody),
		fetch.WithJitter(0),
	}
	if f.dryRun {
		ft, err := fetch.NewFixtureTransport(f.fixtures)
		if err != nil {
			return nil, usageError("%v", err)
		}
		opts = append(opts, fetch.WithFixtures(ft))
		lim = rate.NewLimiter(rate.Inf, 1)
	} else {
		if cfg.Live != config.LiveAll {
			return nil, errLiveOff
		}
		opts = append(opts, fetch.WithLiveGate(cfg.Live))
	}
	return func(rec fetch.Recorder) *fetch.Client {
		return fetch.New(append(slices.Clone(opts),
			fetch.WithRecorder(rec),
			fetch.WithLimiter(lim),
			fetch.WithBreaker(fetch.NewBreaker(fetch.Budgets{
				fetch.KindRobots: siteRobotsBudget,
				fetch.KindSite:   siteBudget,
			})),
		)...)
	}, nil
}
