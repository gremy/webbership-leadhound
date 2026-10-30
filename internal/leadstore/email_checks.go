package leadstore

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// The email check before sending (email_check, 00025): what the verifier answered about an
// address, one append-only row per check. The latest fresh row of an address is its result;
// internal/verify.Decide turns an address's rows into whether it may enter a live batch.

// ErrBadEmailCheck is an email check row the table would refuse: an empty address or status.
var ErrBadEmailCheck = errors.New("leadstore: bad email check")

// EmailCheck is one email_check row.
type EmailCheck struct {
	ID        int64     `db:"id"`
	AccountID int64     `db:"account_id"`
	Address   string    `db:"address"` // lower case
	Provider  string    `db:"provider"`
	Status    string    `db:"status"`
	Score     *int      `db:"score"`
	CatchAll  *bool     `db:"catch_all"`
	Role      *bool     `db:"role"`
	CheckedAt time.Time `db:"checked_at"`
}

// AddEmailCheck stores one check of c.Address (lower-cased and trimmed here) for account
// c.AccountID (0: DefaultAccountID) and returns its id. CheckedAt zero is now().
func (q Queries) AddEmailCheck(ctx context.Context, c EmailCheck) (int64, error) {
	addr := strings.ToLower(strings.TrimSpace(c.Address))
	if addr == "" || c.Status == "" || c.Provider == "" {
		return 0, fmt.Errorf("%w: address %q, provider %q, status %q", ErrBadEmailCheck, addr, c.Provider, c.Status)
	}
	var at *time.Time
	if !c.CheckedAt.IsZero() {
		at = &c.CheckedAt
	}
	var id int64
	err := q.db.QueryRow(ctx,
		`INSERT INTO email_check (account_id, address, provider, status, score, catch_all, role, checked_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, coalesce($8::timestamptz, now())) RETURNING id`,
		orAccount(c.AccountID), addr, c.Provider, c.Status, c.Score, c.CatchAll, c.Role, at).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("leadstore: add email check: %w", err)
	}
	return id, nil
}

// EmailChecks returns the checks of each of addresses (any case) for account (0:
// DefaultAccountID) made after since, newest first, by lower-cased address. An address with
// none has no key.
func (q Queries) EmailChecks(ctx context.Context, account int64, addresses []string, since time.Time) (map[string][]EmailCheck, error) {
	out := map[string][]EmailCheck{}
	if len(addresses) == 0 {
		return out, nil
	}
	lower := make([]string, len(addresses))
	for i, a := range addresses {
		lower[i] = strings.ToLower(strings.TrimSpace(a))
	}
	rows, err := collect(ctx, q.db, pgx.RowToStructByName[EmailCheck],
		`SELECT id, account_id, address, provider, status, score, catch_all, role, checked_at FROM email_check
		 WHERE account_id = $1 AND address = ANY($2::text[]) AND checked_at > $3
		 ORDER BY address, id DESC`, orAccount(account), lower, since)
	if err != nil {
		return nil, fmt.Errorf("leadstore: email checks: %w", err)
	}
	for _, r := range rows {
		out[r.Address] = append(out[r.Address], r)
	}
	return out, nil
}

// EmailChecksSince returns every check of account (0: DefaultAccountID) made at or after
// since, oldest first: a day's checks, for the daily cap and the counts Controls shows.
func (q Queries) EmailChecksSince(ctx context.Context, account int64, since time.Time) ([]EmailCheck, error) {
	rows, err := collect(ctx, q.db, pgx.RowToStructByName[EmailCheck],
		`SELECT id, account_id, address, provider, status, score, catch_all, role, checked_at FROM email_check
		 WHERE account_id = $1 AND checked_at >= $2 ORDER BY id`, orAccount(account), since)
	if err != nil {
		return nil, fmt.Errorf("leadstore: email checks since: %w", err)
	}
	return rows, nil
}

// SetNewLeadRoute moves lead id, while it is still new, onto channel and route routeID: the
// email check's fallback when the address of the lead's email route is blocked and the lead
// has another route (a batch drafts it there, and its page shows the route it was drafted
// on). It reports whether the lead moved (false: not new, or already on that route).
func (q Queries) SetNewLeadRoute(ctx context.Context, id int64, channel string, routeID int64) (bool, error) {
	tag, err := q.db.Exec(ctx,
		`UPDATE lead SET channel = $2, route_id = $3
		 WHERE id = $1 AND state = 'new' AND (channel, route_id) IS DISTINCT FROM ($2::text, $3::bigint)
		   AND EXISTS (SELECT 1 FROM contact_route r WHERE r.id = $3 AND r.org_id = lead.org_id)`,
		id, channel, routeID)
	if err != nil {
		return false, fmt.Errorf("leadstore: set lead route: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}
