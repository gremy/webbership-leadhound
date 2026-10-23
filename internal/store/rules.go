package store

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Rule kinds and fields, as the rule table's CHECKs allow them.
var (
	RuleKinds  = []string{"skip", "flag"}
	RuleFields = []string{"company", "title", "description", "location"}
)

// ErrInvalidRule is returned when a rule to add has an unknown kind or field, a pattern that
// does not compile, or no reason.
var ErrInvalidRule = errors.New("invalid rule")

// ErrRuleDisabled is returned when disabling a rule that is already disabled.
var ErrRuleDisabled = errors.New("rule already disabled")

// Rule is one rule row. A rule is never deleted: disabling it sets DisabledAt, and adding
// it again undoes that.
type Rule struct {
	ID               int64      `db:"id"`
	Kind             string     `db:"kind"`
	Field            string     `db:"field"`
	Pattern          string     `db:"pattern"`
	Reason           string     `db:"reason"`
	CreatedFromJobID *int64     `db:"created_from_job_id"`
	CreatedAt        time.Time  `db:"created_at"`
	DisabledAt       *time.Time `db:"disabled_at"`
	DisabledReason   string     `db:"disabled_reason"`
}

const ruleColumns = `id, kind, field, pattern, reason, created_from_job_id, created_at, disabled_at, disabled_reason`

// AddRule validates and appends a rule, returning its id. The pattern is RE2 and is matched
// case-insensitively, so it must compile with (?i) in front. fromJobID, when set, is the
// job the rule was made from.
func (q Queries) AddRule(ctx context.Context, kind, field, pattern, reason string, fromJobID *int64) (int64, error) {
	switch {
	case !slices.Contains(RuleKinds, kind):
		return 0, fmt.Errorf("store: add rule: kind %q is not one of %s: %w", kind, strings.Join(RuleKinds, ", "), ErrInvalidRule)
	case !slices.Contains(RuleFields, field):
		return 0, fmt.Errorf("store: add rule: field %q is not one of %s: %w", field, strings.Join(RuleFields, ", "), ErrInvalidRule)
	case strings.TrimSpace(pattern) == "":
		return 0, fmt.Errorf("store: add rule: empty pattern: %w", ErrInvalidRule)
	case strings.TrimSpace(reason) == "":
		return 0, fmt.Errorf("store: add rule: a reason is required: %w", ErrInvalidRule)
	}
	if _, err := regexp.Compile("(?i)" + pattern); err != nil {
		return 0, fmt.Errorf("store: add rule: pattern does not compile (%v): %w", err, ErrInvalidRule)
	}
	var id int64
	err := q.db.QueryRow(ctx,
		`INSERT INTO rule (kind, field, pattern, reason, created_from_job_id) VALUES ($1, $2, $3, $4, $5)
		 RETURNING id`, kind, field, pattern, reason, fromJobID).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("store: add rule: %w", err)
	}
	return id, nil
}

// DisableRule sets the rule's disabled_at and disabled_reason. An unknown id is ErrNotFound
// and an already-disabled rule is ErrRuleDisabled; neither changes anything.
func (q Queries) DisableRule(ctx context.Context, id int64, reason string) error {
	if strings.TrimSpace(reason) == "" {
		return fmt.Errorf("store: disable rule %d: a reason is required: %w", id, ErrInvalidRule)
	}
	var disabled bool
	err := q.db.QueryRow(ctx,
		`WITH upd AS (
		   UPDATE rule SET disabled_at = now(), disabled_reason = $2
		   WHERE id = $1 AND disabled_at IS NULL RETURNING id)
		 SELECT NOT EXISTS (SELECT 1 FROM upd) FROM rule WHERE id = $1`, id, reason).Scan(&disabled)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return fmt.Errorf("store: disable rule %d: %w", id, ErrNotFound)
	case err != nil:
		return fmt.Errorf("store: disable rule %d: %w", id, err)
	case disabled:
		return fmt.Errorf("store: disable rule %d: %w", id, ErrRuleDisabled)
	}
	return nil
}

// ActiveRules returns the rules not disabled, oldest first: the ones the gates apply.
func (q Queries) ActiveRules(ctx context.Context) ([]Rule, error) {
	return q.listRules(ctx, `SELECT `+ruleColumns+` FROM rule WHERE disabled_at IS NULL ORDER BY id`)
}

// ListRules returns the active rules, or every rule with includeDisabled, oldest first.
func (q Queries) ListRules(ctx context.Context, includeDisabled bool) ([]Rule, error) {
	if includeDisabled {
		return q.listRules(ctx, `SELECT `+ruleColumns+` FROM rule ORDER BY id`)
	}
	return q.ActiveRules(ctx)
}

func (q Queries) listRules(ctx context.Context, sql string) ([]Rule, error) {
	rows, err := q.db.Query(ctx, sql)
	if err != nil {
		return nil, fmt.Errorf("store: list rules: %w", err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowToStructByName[Rule])
	if err != nil {
		return nil, fmt.Errorf("store: list rules: %w", err)
	}
	return out, nil
}
