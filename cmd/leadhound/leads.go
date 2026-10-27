package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"
	"time"

	"golang.org/x/time/rate"

	"github.com/gremy/webbership-leadhound/internal/config"
	"github.com/gremy/webbership-leadhound/internal/drupal"
	"github.com/gremy/webbership-leadhound/internal/fetch"
	"github.com/gremy/webbership-leadhound/internal/leadconfig"
	"github.com/gremy/webbership-leadhound/internal/leadstore"
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
	dataDir string // LEADHOUND_DATA_DIR
	// leads is leads.yaml, checked as the leadsLoad asked; through leadsOpen and
	// backend.leadsSetup it is the effective config, the console's saved settings applied.
	leads leadconfig.Config
	// file is leads.yaml alone, as leadsEnv loaded it, before the saved settings: what a
	// Settings save merges onto and checks against. Set by applyLeadSettings.
	file leadconfig.Config
	env  leadconfig.Env // the outreach and probe variables
	// jevLeadsEnvCap is LEADHOUND_JEV_LEADS_DAILY_CALL_CAP: the server's ceiling on lead
	// relevance calls a day; jevLeadsDailyCap gives the cap in force.
	jevLeadsEnvCap int
	settings       leadSettingsRead // which saved settings leads carries
}

// leadSettingsRead says which saved leads settings (lead_setting) an effective config
// carries.
type leadSettingsRead struct {
	ID      int64     // the latest lead_setting row; 0 when none was ever saved
	At      time.Time // when that row was saved; zero with none
	Applied bool      // the row's settings are in leads; false with no row, after a reset or when ignored
	// Overrides are the row's settings when Applied: which knobs the console changed.
	Overrides leadconfig.Overrides
	// Ignored is why a saved row was left out, in plain words: it no longer passes the
	// checks leads.yaml passes. The file's values apply instead.
	Ignored []leadconfig.Problem
}

// applyLeadSettings returns setup with the latest saved leads settings applied over
// leads.yaml (D10): LatestSettings, a strict decode into leadconfig.Overrides, Apply and the
// checks Load runs. No row, or a reset (NULL) row, leaves the file's values. A saved
// document the file no longer accepts is ignored with a note that names no value, and the
// file's values are used; it never stops the command. Never-contact countries and the
// fallback order are not here: they live in priority_profile. Only a failed read of the
// store is an error.
func applyLeadSettings(ctx context.Context, lq leadstore.Queries, setup leadsSetup) (leadsSetup, error) {
	raw, id, at, fromDB, err := lq.LatestSettings(ctx)
	if err != nil {
		return leadsSetup{}, err
	}
	setup.file = setup.leads
	setup.settings = leadSettingsRead{ID: id, At: at}
	if !fromDB {
		return setup, nil
	}
	eff, problems := effectiveLeads(raw, setup.leads)
	if len(problems) > 0 {
		setup.settings.Ignored = problems
		slog.Default().Warn("leads settings ignored: the saved settings no longer pass the checks leads.yaml passes, so the file's values apply",
			"setting", id, "problems", len(problems))
		return setup, nil
	}
	o, err := leadconfig.DecodeOverrides(raw) // effectiveLeads decoded it already, so it reads
	if err != nil {
		return leadsSetup{}, err
	}
	setup.leads, setup.settings.Applied, setup.settings.Overrides = eff, true, o
	return setup, nil
}

// effectiveLeads is file with the saved settings document raw applied, or the problems that
// make raw stale. A probe load checks only part of leads.yaml, so the rest of the file may
// already fail on its own; only the problems the settings bring count against them.
func effectiveLeads(raw []byte, file leadconfig.Config) (leadconfig.Config, []leadconfig.Problem) {
	o, err := leadconfig.DecodeOverrides(raw)
	if err != nil {
		msg := err.Error()
		if _, words, ok := strings.Cut(msg, leadconfig.ErrInvalid.Error()+": "); ok {
			msg = words
		}
		return leadconfig.Config{}, []leadconfig.Problem{{Message: msg}}
	}
	eff, problems := o.Effective(file)
	if len(problems) == 0 {
		return eff, nil
	}
	own := leadconfig.Overrides{}.Validate(file)
	var added []leadconfig.Problem
	for _, p := range problems {
		if !slices.Contains(own, p) {
			added = append(added, p)
		}
	}
	if len(added) > 0 {
		return leadconfig.Config{}, added
	}
	return o.Apply(file), nil
}

// jevLeadsDailyCap is the most lead relevance calls a day in force: relevance.leads_daily_calls
// of the effective config when above 0, never above LEADHOUND_JEV_LEADS_DAILY_CALL_CAP (D9: a
// setting may lower the server's cap, never raise it). Whether rating runs at all is
// leads.Relevance.RatingOn.
func (s leadsSetup) jevLeadsDailyCap() int { return s.leads.Relevance.DailyCalls(s.jevLeadsEnvCap) }

// leadsEnv loads leads.yaml from LEADHOUND_DATA_DIR, checked as load says, and the outreach
// variables. A missing data directory, a broken file or an unusable variable is a usage
// error listing every problem at once. It reads the file alone: a command that reads a knob
// Settings may change goes through leadsOpen or backend.leadsSetup instead, which apply the
// saved settings (TestSettingsReachEveryDoor names the readers allowed here).
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
	return leadsSetup{dataDir: cfg.DataDir, leads: lc, env: env, jevLeadsEnvCap: cfg.JevLeadsDailyCallCap}, nil
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
