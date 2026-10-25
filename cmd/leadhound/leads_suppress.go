package main

import (
	"context"
	"flag"
	"fmt"
	"strings"

	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/outreach"
)

func init() { leadsCommands["suppress"] = runLeadsSuppress }

const leadsSuppressUsage = "usage: leadhound leads suppress add address|domain VALUE [--reason manual|opt_out|bounce|replied_no] | list | check VALUE"

// runLeadsSuppress is `leads suppress add|list|check`. A suppression is never lifted.
func runLeadsSuppress(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return usageError(leadsSuppressUsage)
	}
	switch args[0] {
	case "add":
		return runSuppressAdd(ctx, args[1:])
	case "list":
		return runSuppressList(ctx, args[1:])
	case "check":
		return runSuppressCheck(ctx, args[1:])
	}
	return usageError("unknown suppress subcommand %q; %s", args[0], leadsSuppressUsage)
}

// maskAddress shows an address as its first letter and its domain, "j***@domain".
func maskAddress(a string) string {
	local, dom, ok := strings.Cut(a, "@")
	if !ok || local == "" {
		return "***"
	}
	return local[:1] + "***@" + dom
}

// shown is a suppression value as the commands print it: addresses masked.
func shown(kind, value string) string {
	if kind == leadstore.SuppressAddress {
		return maskAddress(value)
	}
	return value
}

func runSuppressAdd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("leads suppress add", flag.ContinueOnError)
	reason := fs.String("reason", leadstore.ReasonManual, "why: manual, opt_out, bounce or replied_no")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 2 || (pos[0] != leadstore.SuppressAddress && pos[0] != leadstore.SuppressDomain) {
		return usageError(leadsSuppressUsage)
	}
	kind, value := pos[0], strings.ToLower(strings.TrimSpace(pos[1]))
	s, _, err := leadsOpen(ctx, leadsForProbe)
	if err != nil {
		return err
	}
	defer s.Close()
	res, err := suppressAdd(ctx, s, kind, value, *reason, suppressFromCLI)
	if r := asRefusal(err); r != nil {
		switch r.Kind {
		case RefuseSuppressReason:
			return usageError("--reason %q is not one of manual, opt_out, bounce, replied_no", *reason)
		case RefuseNotRegistrable:
			return usageError("%s has no registrable domain (a bare public suffix cannot be suppressed)", value)
		case RefuseBadValue:
			return usageError("%s is not a usable %s", shown(kind, value), kind)
		}
	}
	if err != nil {
		return err
	}
	rec := "recorded"
	if !res.Recorded {
		rec = "already on file"
	}
	_, _ = fmt.Fprintf(stdout, "Suppressed %s %s (%s): %s · %s reached, %d moved to suppressed, %s cancelled.\n",
		kind, shown(kind, value), *reason, rec, plural(res.Leads, "lead", "leads"), res.Moved,
		plural(int(res.Cancelled), "pending message", "pending messages"))
	return nil
}

func runSuppressList(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("leads suppress list", flag.ContinueOnError)
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return usageError(leadsSuppressUsage)
	}
	s, _, err := leadsOpen(ctx, leadsForProbe)
	if err != nil {
		return err
	}
	defer s.Close()
	lq := leadstore.NewQueries(s.Pool())
	counts, err := lq.SuppressionCounts(ctx)
	if err != nil {
		return err
	}
	set, err := lq.SuppressionSet(ctx)
	if err != nil {
		return err
	}
	if len(counts) == 0 {
		_, _ = fmt.Fprintln(stdout, "No suppressions.")
		return nil
	}
	parts := make([]string, len(counts))
	for i, c := range counts {
		parts[i] = fmt.Sprintf("%s %s %d", c.Kind, c.Reason, c.N)
	}
	_, _ = fmt.Fprintf(stdout, "Suppressions: %s\n", strings.Join(parts, " · "))
	for _, a := range set.Addresses {
		_, _ = fmt.Fprintf(stdout, "  address %s\n", maskAddress(a))
	}
	for _, d := range set.Domains {
		_, _ = fmt.Fprintf(stdout, "  domain %s\n", d)
	}
	return nil
}

func runSuppressCheck(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("leads suppress check", flag.ContinueOnError)
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageError(leadsSuppressUsage)
	}
	v := strings.ToLower(strings.TrimSpace(pos[0]))
	s, _, err := leadsOpen(ctx, leadsForProbe)
	if err != nil {
		return err
	}
	defer s.Close()
	sup, err := leadstore.NewQueries(s.Pool()).SuppressionSet(ctx)
	if err != nil {
		return err
	}
	set := outreach.NewSuppressionSet(sup.Addresses, sup.Domains)
	kind, addr, dom := leadstore.SuppressDomain, "", v
	if strings.Contains(v, "@") {
		kind, addr, dom = leadstore.SuppressAddress, v, ""
	}
	verdict := "not suppressed"
	if outreach.Suppressed(addr, dom, set) {
		verdict = "suppressed: never written to"
	}
	_, _ = fmt.Fprintf(stdout, "%s %s: %s\n", kind, shown(kind, v), verdict)
	return nil
}
