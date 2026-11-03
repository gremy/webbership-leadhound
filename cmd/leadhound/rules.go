package main

import (
	"context"
	"flag"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gremy/webbership-leadhound/internal/console"
	"github.com/gremy/webbership-leadhound/internal/store"
)

func init() { commands["rules"] = runRules }

const rulesUsage = "usage: leadhound rules list [--all]\n" +
	"       leadhound rules add --kind skip|flag --field company|title|description|location --pattern RE --reason TEXT [--from-job ID]\n" +
	"       leadhound rules disable ID --reason TEXT\n" +
	"       leadhound rules test --field company|title|description|location --pattern RE [--days N]"

// runRules is `leadhound rules list|add|disable|test`. A rule is never deleted: disable sets
// disabled_at, and adding it again undoes that. test writes nothing.
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
		days                         int
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
	case "test":
		fs.StringVar(&field, "field", "", "company, title, description or location")
		fs.StringVar(&pattern, "pattern", "", "RE2 pattern, matched case-insensitively")
		fs.IntVar(&days, "days", queueNeverBriefedDays, "how many days back to look for jobs that reached no brief")
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
	case "test":
		if len(pos) != 0 {
			return usageError("%s", rulesUsage)
		}
		if err := rulesTestArgs(field, pattern, days); err != nil {
			return err
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

	switch sub {
	case "add":
		var from *int64
		if fromJob > 0 {
			from = &fromJob
		}
		newID, err := ruleAdd(ctx, s, RuleAddRequest{Kind: kind, Field: field, Pattern: pattern, Reason: reason, FromJob: from})
		if r := asRefusal(err); r != nil && r.Kind == RefuseRuleInvalid {
			return usageError("%s", r.Detail)
		}
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(stdout, "rule %d added\n", newID)
	case "disable":
		err := ruleDisable(ctx, s, id, reason)
		if r := asRefusal(err); r != nil {
			switch r.Kind {
			case RefuseRuleInvalid:
				return usageError("%s", r.Detail)
			case RefuseRuleNotFound:
				return usageError("rule %d does not exist", id)
			case RefuseRuleDisabled:
				return usageError("rule %d is already disabled; add it again to undo", id)
			}
		}
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(stdout, "rule %d disabled\n", id)
	case "test":
		return runRulesTest(ctx, s, loc, field, pattern, days)
	default:
		rules, err := s.Q().ListRules(ctx, all)
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

// rulesTestArgs refuses what `rules add` would refuse of a field and a pattern, and a day count
// that is not positive.
func rulesTestArgs(field, pattern string, days int) error {
	if !slices.Contains(store.RuleFields, field) {
		return usageError("rules test: --field must be one of %s", strings.Join(store.RuleFields, ", "))
	}
	if strings.TrimSpace(pattern) == "" {
		return usageError("rules test needs --pattern")
	}
	if _, err := regexp.Compile("(?i)" + pattern); err != nil {
		return usageError("rules test: the pattern does not compile: %v", err)
	}
	if days <= 0 {
		return usageError("rules test: --days must be a positive whole number")
	}
	return nil
}

// runRulesTest prints what this one pattern would hide, as if it were the only active skip rule,
// through the queue's own read, so a rescued job is never listed. It writes nothing.
func runRulesTest(ctx context.Context, s *store.Store, loc *time.Location, field, pattern string, days int) error {
	now := time.Now().In(loc)
	rule := store.Rule{Kind: "skip", Field: field, Pattern: pattern}
	_, hidden, err := readQueue(ctx, s.Q(), now.AddDate(0, 0, -console.APIDaysDefault), now.AddDate(0, 0, -days), []store.Rule{rule})
	if err != nil {
		return err
	}
	for _, c := range []struct {
		briefed bool
		label   string
	}{
		{true, fmt.Sprintf("would hide from the queue (briefs of the last %d days)", console.APIDaysDefault)},
		{false, fmt.Sprintf("never reached a brief (first seen in the last %d days)", days)},
	} {
		var lines []string
		for _, h := range hidden {
			if h.Briefed == c.briefed {
				lines = append(lines, fmt.Sprintf("job:%d\t%s\t%s", h.JobID, apiClientLine(h.Company), apiClientLine(h.Title)))
			}
		}
		_, _ = fmt.Fprintf(stdout, "%s: %d\n", c.label, len(lines))
		for _, l := range lines {
			_, _ = fmt.Fprintln(stdout, l)
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
