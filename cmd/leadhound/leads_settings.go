package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gremy/webbership-leadhound/internal/console"
	"github.com/gremy/webbership-leadhound/internal/leadconfig"
)

func init() { leadsCommands["settings"] = runLeadsSettings }

const leadsSettingsUsage = "usage: leadhound leads settings show | reset [--section who|kinds|small-sites|batches|dates|relevance|order|all]"

// runLeadsSettings is `leads settings show|reset`: the settings in force, each with where it
// comes from (leads.yaml, or a change saved in the console), and a reset back to the file.
// The console's Settings page changes them; neither shows or stores a secret.
func runLeadsSettings(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return usageError(leadsSettingsUsage)
	}
	switch args[0] {
	case "show":
		return runSettingsShow(ctx, args[1:])
	case "reset":
		return runSettingsReset(ctx, args[1:])
	}
	return usageError("unknown settings subcommand %q; %s", args[0], leadsSettingsUsage)
}

// settingsZone is the operator's zone (LEADHOUND_TZ), the day a change was saved is said in.
func settingsZone() (*time.Location, error) {
	cfg, err := loadConfig("LEADHOUND_DATABASE_URL")
	if err != nil {
		return nil, err
	}
	loc, err := time.LoadLocation(cfg.TZ)
	if err != nil {
		return nil, usageError("LEADHOUND_TZ: %v", err)
	}
	return loc, nil
}

func runSettingsShow(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("leads settings show", flag.ContinueOnError)
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return usageError(leadsSettingsUsage)
	}
	loc, err := settingsZone()
	if err != nil {
		return err
	}
	s, setup, err := leadsOpen(ctx, leadsForProbe)
	if err != nil {
		return err
	}
	defer s.Close()
	st, err := readSettingsState(ctx, s.Pool(), setup)
	if err != nil {
		return err
	}
	printSettings(stdout, st, loc)
	return nil
}

// sourceText is where a value comes from, as `settings show` prints it: "file" or
// "console YYYY-MM-DD".
func sourceText(src console.SettingSource, loc *time.Location) string {
	if !src.Console {
		return "file"
	}
	return "console " + src.At.In(loc).Format(time.DateOnly)
}

// listText is a list as `settings show` prints it.
func listText(l []string) string {
	if len(l) == 0 {
		return "(none)"
	}
	return strings.Join(l, ", ")
}

// printSettings writes each setting the console may change, its value in force and where it
// comes from, then any saved change that is ignored.
func printSettings(w io.Writer, st settingsState, loc *time.Location) {
	cfg := st.setup.leads
	line := func(name, value string, src console.SettingSource) {
		_, _ = fmt.Fprintf(w, "%s: %s · %s\n", name, value, sourceText(src, loc))
	}
	_, _ = fmt.Fprintln(w, "leads settings in force (leads.yaml, with the changes saved in the console):")
	line("priority.exclude_countries (never contact)", listText(st.profile.ExcludeCountries), st.whoSource())
	line("kinds_priority", listText(cfg.KindsPriority), st.knob("kinds_priority"))
	line("kinds_off", listText(cfg.KindsOff), st.knob("kinds_off"))
	rank := strconv.Itoa(cfg.SkipRankOver)
	if cfg.SkipRankOver == 0 {
		rank += " (off)"
	}
	line("skip_rank_over", rank, st.knob("skip_rank_over"))
	line("batches_from", cfg.BatchesFrom(), st.knob("batches_from"))
	for _, d := range eolKeys {
		date, _ := cfg.Facts.EOL(d.major)
		val := "(missing)"
		if !date.IsZero() {
			val = date.Format(time.DateOnly)
		}
		line("facts."+d.name, val, st.knob("facts."+d.name))
	}
	rating := "off"
	if cfg.Relevance.RatingOn() {
		rating = "on"
	}
	line("relevance.rating", rating, st.knob("relevance.rating"))
	line("relevance.leads_daily_calls", fmt.Sprintf("%d (in force: %d a day; the server allows %d)",
		cfg.Relevance.LeadsDailyCalls, st.setup.jevLeadsDailyCap(), st.setup.jevLeadsEnvCap), st.knob("relevance.leads_daily_calls"))
	line("priority (fallback order)", fmt.Sprintf("%d rules", orderRules(st.profile)), st.orderSource())
	if n := st.ignoredNote(loc); n != "" {
		_, _ = fmt.Fprintln(w, "note: "+n)
	}
	if n := st.kindOrderNote(); n != "" {
		_, _ = fmt.Fprintln(w, "note: "+n)
	}
}

// orderRules counts the fallback order's rules: every rule but the never-contact countries.
func orderRules(p leadconfig.Profile) int {
	return p.Rules() - len(p.ExcludeCountries)
}

// settingsResetSections are the sections `settings reset --section` takes.
var settingsResetSections = []string{console.SettingsWho, console.SettingsKinds, console.SettingsSmallSites,
	console.SettingsBatches, console.SettingsDates, console.SettingsRelevance, console.SettingsOrder, console.SettingsAll}

func runSettingsReset(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("leads settings reset", flag.ContinueOnError)
	section := fs.String("section", console.SettingsAll, "the section to put back to leads.yaml; all is every one")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 || !slices.Contains(settingsResetSections, *section) {
		return usageError(leadsSettingsUsage)
	}
	// A reset is the way back from settings that no longer fit, so it needs only the
	// probe checks of leads.yaml to pass.
	s, setup, err := leadsOpen(ctx, leadsForProbe)
	if err != nil {
		return err
	}
	defer s.Close()
	wrote, err := saveLeadSettings(ctx, s, setup, *section, console.LeadSettingsValues{}, true)
	var refusal *console.SettingsRefused
	if errors.As(err, &refusal) {
		return usageError("nothing was reset: %s", strings.Join(refusal.Problems, " "))
	}
	if err != nil {
		return err
	}
	what := "every setting is"
	if *section != console.SettingsAll {
		what = "the " + *section + " settings are"
	}
	if !wrote {
		_, _ = fmt.Fprintln(stdout, "leads settings reset: nothing to reset; leads.yaml's values already apply")
		return nil
	}
	_, _ = fmt.Fprintf(stdout, "leads settings reset: %s back to leads.yaml\n", what)
	return nil
}
