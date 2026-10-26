package main

import (
	"context"
	"flag"
	"fmt"
	"strconv"
	"strings"

	"github.com/gremy/webbership-leadhound/internal/leadstore"
	"github.com/gremy/webbership-leadhound/internal/outreach"
)

func init() { leadsCommands["suppress"] = runLeadsSuppress }

const leadsSuppressUsage = "usage: leadhound leads suppress add address|domain VALUE [--reason manual|opt_out|bounce|replied_no] | list | check VALUE | lift ID --domain DOMAIN --reason TEXT"

// runLeadsSuppress is `leads suppress add|list|check|lift`. A suppression is lifted only by
// the operator, by id, with a reason (FO6).
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
	case "lift":
		return runSuppressLift(ctx, args[1:])
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
	active, lifted, err := lq.SuppressionRows(ctx, 0)
	if err != nil {
		return err
	}
	if len(counts) == 0 {
		_, _ = fmt.Fprintln(stdout, "No suppressions.")
	} else {
		parts := make([]string, len(counts))
		for i, c := range counts {
			parts[i] = fmt.Sprintf("%s %s %d", c.Kind, c.Reason, c.N)
		}
		_, _ = fmt.Fprintf(stdout, "Suppressions: %s\n", strings.Join(parts, " · "))
	}
	for _, r := range active {
		_, _ = fmt.Fprintf(stdout, "  #%d %s %s %s %s %s\n", r.ID, r.Kind, shown(r.Kind, r.Value), r.Reason, r.Source, leadsDay(r.CreatedAt))
	}
	// The lifted ones whatever the active count: with everything lifted they are still shown.
	if len(lifted) > 0 {
		_, _ = fmt.Fprintf(stdout, "Lifted (%d):\n", len(lifted))
		for _, r := range lifted {
			_, _ = fmt.Fprintf(stdout, "  #%d %s %s %s · lifted %s (%s): %s\n", r.ID, r.Kind, shown(r.Kind, r.Value), r.Reason,
				leadsDay(*r.LiftedAt), r.LiftSource, r.LiftReason)
		}
	}
	return nil
}

// runSuppressLift is `leads suppress lift ID --domain DOMAIN --reason TEXT` (FO6): it records
// that suppression ID was wrong, confirmed by its domain, with the operator's reason. The
// words, success or refusal, are liftWords'; a refusal exits 2.
func runSuppressLift(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("leads suppress lift", flag.ContinueOnError)
	domain := fs.String("domain", "", "the suppression's domain (an address suppression's part after the @), to confirm")
	reason := fs.String("reason", "", "why the suppression was wrong: 3 to 300 characters, one line, no address")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 || strings.TrimSpace(*domain) == "" || strings.TrimSpace(*reason) == "" {
		return usageError(leadsSuppressUsage)
	}
	id, err := strconv.ParseInt(pos[0], 10, 64)
	if err != nil || id <= 0 {
		return usageError("%q is not a suppression number; %s", pos[0], leadsSuppressUsage)
	}
	s, _, err := leadsOpen(ctx, leadsForProbe)
	if err != nil {
		return err
	}
	defer s.Close()
	res, err := liftSuppression(ctx, s, id, *domain, *reason, leadstore.LiftFromCLI)
	if asRefusal(err) != nil {
		return usageError("%s", liftWords(res, err, forCLI))
	}
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintln(stdout, liftWords(res, nil, forCLI))
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
