package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/gremy/webbership-leadhound/internal/store"
)

func init() { commands["rules"] = runRules }

const rulesUsage = "usage: leadhound rules list [--all]\n" +
	"       leadhound rules add --kind skip|flag --field company|title|description|location --pattern RE --reason TEXT [--from-job ID]\n" +
	"       leadhound rules disable ID --reason TEXT"

// runRules is `leadhound rules list|add|disable`. A rule is never deleted: disable sets
// disabled_at, and adding it again undoes that.
func runRules(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return usageError("%s", rulesUsage)
	}
	sub, rest := args[0], args[1:]
	fs := flag.NewFlagSet("rules "+sub, flag.ContinueOnError)
	var (
		all                          bool
		kind, field, pattern, reason string
		fromJob                      int64
	)
	switch sub {
	case "list":
		fs.BoolVar(&all, "all", false, "include disabled rules")
	case "add":
		fs.StringVar(&kind, "kind", "", "skip or flag")
		fs.StringVar(&field, "field", "", "company, title, description or location")
		fs.StringVar(&pattern, "pattern", "", "RE2 pattern, matched case-insensitively")
		fs.StringVar(&reason, "reason", "", "why the rule exists")
		fs.Int64Var(&fromJob, "from-job", 0, "the job the rule was made from")
	case "disable":
		fs.StringVar(&reason, "reason", "", "why the rule is disabled")
	default:
		return usageError("%s", rulesUsage)
	}
	pos, err := parseArgs(fs, rest)
	if err != nil {
		return err
	}

	var id int64
	switch sub {
	case "list", "add":
		if len(pos) != 0 {
			return usageError("%s", rulesUsage)
		}
		if sub == "add" && (kind == "" || field == "" || pattern == "" || strings.TrimSpace(reason) == "") {
			return usageError("rules add needs --kind, --field, --pattern and --reason")
		}
	case "disable":
		if len(pos) != 1 || strings.TrimSpace(reason) == "" {
			return usageError("rules disable needs one rule ID and --reason")
		}
		if id, err = strconv.ParseInt(pos[0], 10, 64); err != nil || id <= 0 {
			return usageError("rules disable: %q is not a rule ID", pos[0])
		}
	}

	cfg, err := loadConfig("LEADHOUND_DATABASE_URL")
	if err != nil {
		return err
	}
	s, loc, err := openStoreAndZone(ctx, cfg)
	if err != nil {
		return err
	}
	defer s.Close()
	q := s.Q()

	switch sub {
	case "add":
		var from *int64
		if fromJob > 0 {
			from = &fromJob
		}
		newID, err := q.AddRule(ctx, kind, field, pattern, reason, from)
		if errors.Is(err, store.ErrInvalidRule) {
			return usageError("%v", err)
		}
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(stdout, "rule %d added\n", newID)
	case "disable":
		err := q.DisableRule(ctx, id, reason)
		switch {
		case errors.Is(err, store.ErrNotFound):
			return usageError("rule %d does not exist", id)
		case errors.Is(err, store.ErrRuleDisabled):
			return usageError("rule %d is already disabled; add it again to undo", id)
		case err != nil:
			return err
		}
		_, _ = fmt.Fprintf(stdout, "rule %d disabled\n", id)
	default:
		rules, err := q.ListRules(ctx, all)
		if err != nil {
			return err
		}
		if len(rules) == 0 {
			_, _ = fmt.Fprintln(stdout, "no rules")
			return nil
		}
		for _, r := range rules {
			_, _ = fmt.Fprintln(stdout, ruleLine(r, loc))
		}
	}
	return nil
}

// ruleLine is one rule on one line: id, kind, field, pattern, reason, created, disabled.
func ruleLine(r store.Rule, loc *time.Location) string {
	const layout = "2006-01-02 15:04"
	disabled := "active"
	if r.DisabledAt != nil {
		disabled = "disabled " + r.DisabledAt.In(loc).Format(layout) + ": " + r.DisabledReason
	}
	from := ""
	if r.CreatedFromJobID != nil {
		from = fmt.Sprintf(" (from job %d)", *r.CreatedFromJobID)
	}
	return fmt.Sprintf("%d\t%s\t%s\t%q\t%s%s\tcreated %s\t%s",
		r.ID, r.Kind, r.Field, r.Pattern, r.Reason, from, r.CreatedAt.In(loc).Format(layout), disabled)
}
