package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/gremy/webbership-leadhound/internal/agencies"
	"github.com/gremy/webbership-leadhound/internal/leadconfig"
)

func init() { leadsCommands["check-config"] = runLeadsCheckConfig }

const leadsCheckConfigUsage = "usage: leadhound leads check-config [--agencies FILE]"

// runLeadsCheckConfig is `leads check-config [--agencies FILE]`: it loads leads.yaml with its
// templates and the agency list (default <data dir>/agencies.yaml) as the leads commands
// do, and prints counts only. Any problem is a usage error listing them all, the priority
// block's among them (leadconfig.Profile.Problems: points out of bounds, unknown kinds,
// majors, channels or countries, rank bands out of order). A variable of the old single
// mailbox still set is named on a line of its own: it is no longer read, and the mailbox
// belongs in the console. It needs no database.
func runLeadsCheckConfig(_ context.Context, args []string) error {
	fs := flag.NewFlagSet("leads check-config", flag.ContinueOnError)
	agenciesPath := fs.String("agencies", "", "the agency list (default <data dir>/agencies.yaml)")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return usageError(leadsCheckConfigUsage)
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	if cfg.DataDir == "" {
		return usageError("LEADHOUND_DATA_DIR is not set; check-config reads leads.yaml from it")
	}
	var problems []string
	lc, err := leadconfig.Load(cfg.DataDir)
	if err != nil {
		problems = append(problems, configProblems(err)...)
	}
	path := *agenciesPath
	if path == "" {
		path = filepath.Join(cfg.DataDir, agencies.FileName)
	}
	list, skipped, ap := agencies.LoadFile(path)
	for _, p := range ap {
		problems = append(problems, agencies.FileName+": "+valueFree(agencyRowName.ReplaceAllString(p, "$1")))
	}
	if len(problems) > 0 {
		return usageError("%s (values are not shown; open the file at each place named)", strings.Join(problems, "; "))
	}
	var steps, forms int
	for _, s := range lc.Templates.Sets {
		steps += len(s.Steps)
		if s.Form != nil {
			forms++
		}
	}
	_, _ = fmt.Fprintf(stdout, "leads check-config: %s ok · templates: %d sets (%d email steps, %d forms) · %s · "+
		"agencies: %d with a website, %d without · TLDs: %d · countries: %d · operator addresses: %d\n",
		leadconfig.FileName, len(lc.Templates.Sets), steps, forms, plural(len(lc.Templates.Footers), "footer", "footers"),
		len(list), skipped, len(lc.SiteLists.TLDs), len(lc.Countries), len(lc.Sender.OperatorAddresses))
	env, _ := leadconfig.LoadEnv(os.Getenv)
	switch n := len(env.Retired); {
	case n == 1:
		_, _ = fmt.Fprintf(stdout, "%s is no longer read: add the mailbox in the console (Mailboxes)\n", env.Retired[0])
	case n > 1:
		_, _ = fmt.Fprintf(stdout, "%s (and %d more) are no longer read: add the mailbox in the console (Mailboxes)\n", env.Retired[0], n-1)
	}
	return nil
}

// configProblems is leads.yaml's load error as value-free problems, one per problem the
// check listed.
func configProblems(err error) []string {
	msg := err.Error()
	if errors.Is(err, leadconfig.ErrInvalid) {
		if _, list, ok := strings.Cut(msg, leadconfig.ErrInvalid.Error()+": "); ok {
			var out []string
			for _, p := range strings.Split(list, "; ") {
				out = append(out, leadconfig.FileName+": "+valueFree(p))
			}
			return out
		}
	}
	return []string{valueFree(msg)}
}

var (
	// quotedRE matches a value the config and agency checks quote: "…" or `…`.
	quotedRE = regexp.MustCompile("\"(?:[^\"\\\\]|\\\\.)*\"|`[^`]*`")
	// agencyRowName matches the agency name an agency-file problem names its row by.
	agencyRowName = regexp.MustCompile(`^(row \d+) \([^)]*\)`)
)

// valueFree is a problem with what it quotes blanked and whatever follows its second ": "
// (an error's own text, which can quote the file) cut, so it keeps the place and the kind
// of the problem and never a value: `countries.GB.channel: "…" is not one of email, form,
// none`, `template d10/en/1.txt is missing`.
func valueFree(p string) string {
	p = quotedRE.ReplaceAllStringFunc(p, func(q string) string { return q[:1] + "…" + q[len(q)-1:] })
	if first := strings.Index(p, ": "); first >= 0 {
		if second := strings.Index(p[first+2:], ": "); second >= 0 {
			p = p[:first+2+second]
		}
	}
	return p
}
