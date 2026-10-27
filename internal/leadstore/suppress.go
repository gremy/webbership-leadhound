package leadstore

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/gremy/webbership-leadhound/internal/sitelist"
)

// The kinds a suppression can be (suppression_kind_check).
const (
	SuppressAddress = "address"
	SuppressDomain  = "domain"
)

// The reasons a suppression can carry (suppression_reason_check).
const (
	ReasonOptOut    = "opt_out"
	ReasonBounce    = "bounce"
	ReasonRepliedNo = "replied_no"
	ReasonManual    = "manual"
)

var (
	// ErrUnknownSuppression is returned by Suppress for a kind or reason outside the checks.
	ErrUnknownSuppression = errors.New("leadstore: unknown suppression kind or reason")
	// ErrSuppressNotRegistrable is returned by Suppress for a domain value with no
	// registrable domain (a bare public suffix, an IP address).
	ErrSuppressNotRegistrable = errors.New("leadstore: suppression domain is not registrable")
	// ErrBadSuppressValue is returned by Suppress for a blank value, an address without a
	// domain, or a domain with an @.
	ErrBadSuppressValue = errors.New("leadstore: bad suppression value")
)

// Suppressed is what Suppress did.
type Suppressed struct {
	Recorded  bool  // a new suppression row was written (false: the same kind, value and reason was there)
	Leads     int   // leads the suppression reaches
	Moved     int   // of those, leads moved from new or batched to suppressed
	Cancelled int64 // pending messages cancelled
}

// Suppress records that kind value (lower-cased; a domain reduced to its registrable
// domain) must never be written to again, once per kind, value and reason among the active
// suppressions (after a lift the next one records again, FO6), and applies it (the grid's suppress column): every lead it
// reaches (its route address, its org's domain, or a pending message's recipient, a
// domain reaching its subdomains too) has its pending messages cancelled, and a lead in
// state new or batched moves to suppressed. A lead in another state keeps it. Run it in a
// transaction. The suppression is account accountID's (0: DefaultAccountID): it blocks every
// campaign of that account and reaches only that account's leads.
func (q Queries) Suppress(ctx context.Context, accountID int64, kind, value, reason, source string) (Suppressed, error) {
	accountID = orAccount(accountID)
	v := strings.ToLower(strings.TrimSpace(value))
	if (kind != SuppressAddress && kind != SuppressDomain) ||
		(reason != ReasonOptOut && reason != ReasonBounce && reason != ReasonRepliedNo && reason != ReasonManual) {
		return Suppressed{}, fmt.Errorf("leadstore: suppress: %w: %q %q", ErrUnknownSuppression, kind, reason)
	}
	local, dom, at := strings.Cut(v, "@")
	switch {
	case v == "":
		return Suppressed{}, fmt.Errorf("leadstore: suppress: %w: blank", ErrBadSuppressValue)
	case kind == SuppressAddress && (!at || local == "" || dom == "" || strings.Contains(dom, "@")):
		return Suppressed{}, fmt.Errorf("leadstore: suppress: %w: not an address", ErrBadSuppressValue)
	case kind == SuppressDomain && at:
		return Suppressed{}, fmt.Errorf("leadstore: suppress: %w: a domain has no @", ErrBadSuppressValue)
	}
	if kind == SuppressDomain {
		// A domain suppression covers the whole registrable domain: mail.acme.de → acme.de.
		r, err := sitelist.Registrable(v)
		if err != nil {
			return Suppressed{}, fmt.Errorf("leadstore: suppress: %w: %w", ErrSuppressNotRegistrable, err)
		}
		v = r
	}
	tag, err := q.db.Exec(ctx,
		`INSERT INTO suppression (kind, value, reason, source, account_id)
		 SELECT $1::text, $2::text, $3::text, $4::text, $5::bigint
		 WHERE NOT EXISTS (SELECT 1 FROM suppression_active
		   WHERE account_id = $5 AND kind = $1 AND value = $2 AND reason = $3)`,
		kind, v, reason, source, accountID)
	if err != nil {
		return Suppressed{}, fmt.Errorf("leadstore: suppress: %w", err)
	}
	res := Suppressed{Recorded: tag.RowsAffected() == 1}

	// The leads this one suppression reaches, locked.
	ids, err := collect(ctx, q.db, pgx.RowTo[int64],
		`SELECT l.id FROM lead l JOIN org o ON o.id = l.org_id LEFT JOIN contact_route r ON r.id = l.route_id
		 WHERE `+reachSQL(kind)+` AND `+inAccountSQL+` ORDER BY l.id FOR UPDATE OF l`, v, accountID)
	if err != nil {
		return Suppressed{}, fmt.Errorf("leadstore: suppress: %w", err)
	}
	actor := ActorSystem
	if reason == ReasonManual {
		actor = ActorGremy
	}
	cancel := "suppressed: " + reason
	for _, id := range ids {
		res.Leads++
		n, err := q.CancelPending(ctx, CancelTarget{LeadID: id}, cancel)
		if err != nil {
			return Suppressed{}, fmt.Errorf("leadstore: suppress: %w", err)
		}
		res.Cancelled += n
		moved, err := q.setLeadStateFrom(ctx, id, []string{LeadNew, LeadBatched}, LeadSuppressed,
			fmt.Sprintf("%s %s (%s)", kind, reason, source), actor)
		if err != nil {
			return Suppressed{}, fmt.Errorf("leadstore: suppress: %w", err)
		}
		if moved {
			res.Moved++
		}
	}
	return res, nil
}

// reachSQL is the SQL condition on lead l (org o, route r) that is true when a suppression of
// kind on $1 reaches it: an address suppression reaches its route address or a pending
// message's recipient; a domain suppression reaches the org's domain, the route address's
// domain or a pending message's recipient domain, or a subdomain of any of them. Suppress
// (which leads to stop) and LiftSuppression (which leads it had stopped) share it, so the two
// cannot drift.
func reachSQL(kind string) string {
	if kind == SuppressAddress {
		return `((r.kind = 'email' AND r.value = $1)
		  OR EXISTS (SELECT 1 FROM outreach_message m WHERE m.lead_id = l.id
		    AND m.state IN ('draft', 'approved') AND lower(m.to_address) = $1))`
	}
	return `(o.domain = $1 OR right(o.domain, length($1) + 1) = '.' || $1
		  OR (r.kind = 'email' AND (split_part(r.value, '@', 2) = $1
		    OR right(split_part(r.value, '@', 2), length($1) + 1) = '.' || $1))
		  OR EXISTS (SELECT 1 FROM outreach_message m WHERE m.lead_id = l.id
		    AND m.state IN ('draft', 'approved')
		    AND (lower(split_part(m.to_address, '@', 2)) = $1
		      OR right(lower(split_part(m.to_address, '@', 2)), length($1) + 1) = '.' || $1)))`
}

// inAccountSQL is the SQL condition on lead l that is true when its campaign is account $2's.
const inAccountSQL = `l.campaign_id IN (SELECT c.id FROM campaign c WHERE c.account_id = $2)`

// Suppressions is every suppressed address and domain, lower case, sorted, each once
// (outreach.NewSuppressionSet takes the two lists).
type Suppressions struct {
	Addresses []string
	Domains   []string
}

// SuppressionSet returns every active suppression (none lifted, FO6) of account accountID
// (0: DefaultAccountID), whatever its reason.
func (q Queries) SuppressionSet(ctx context.Context, accountID int64) (Suppressions, error) {
	type row struct {
		Kind  string `db:"kind"`
		Value string `db:"value"`
	}
	rows, err := collect(ctx, q.db, pgx.RowToStructByName[row],
		`SELECT DISTINCT kind, value FROM suppression_active WHERE account_id = $1 ORDER BY kind, value`,
		orAccount(accountID))
	if err != nil {
		return Suppressions{}, fmt.Errorf("leadstore: suppression set: %w", err)
	}
	s := Suppressions{Addresses: []string{}, Domains: []string{}}
	for _, r := range rows {
		if r.Kind == SuppressAddress {
			s.Addresses = append(s.Addresses, r.Value)
		} else {
			s.Domains = append(s.Domains, r.Value)
		}
	}
	return s, nil
}
