package leadstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/gremy/webbership-leadhound/internal/sitelist"
)

// The kinds a suppression can be (suppression_kind_check).
const (
	SuppressAddress = "address"
	SuppressDomain  = "domain"
	// SuppressPhone holds PhoneSuppressionValue of a normalised number, never the number.
	SuppressPhone = "phone"
)

// The reasons a suppression can carry (suppression_reason_check).
const (
	ReasonOptOut    = "opt_out"
	ReasonBounce    = "bounce"
	ReasonRepliedNo = "replied_no"
	ReasonManual    = "manual"
	// The phone-only reasons (suppression_phone_reason_check).
	ReasonDoNotCall = "do_not_call" // asked on a call not to be called again
	ReasonTPS       = "tps"         // on the UK Telephone Preference Service list
	ReasonCTPS      = "ctps"        // on the UK Corporate TPS list
)

// phoneValueRE is the shape of a phone suppression's value (suppression_phone_value_check).
var phoneValueRE = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// PhoneSuppressionValue is the value a phone suppression holds for the normalised number
// normalized (org_fact.normalized: + and 8 to 15 digits): "sha256:" and the lower-case hex
// sha256 of it, so a suppression never holds the number itself. phoneHashSQL computes the
// same in SQL; the two must agree.
func PhoneSuppressionValue(normalized string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(normalized)))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// phoneHashSQL is the SQL twin of PhoneSuppressionValue over the SQL expression expr.
func phoneHashSQL(expr string) string {
	return `('sha256:' || encode(sha256(convert_to(` + expr + `, 'UTF8')), 'hex'))`
}

// phoneSuppressedSQL is a SQL condition that is true when an active phone suppression of
// account acct (an SQL expression) holds the number of org_fact id factID (an SQL expression).
func phoneSuppressedSQL(acct, factID string) string {
	return `EXISTS (SELECT 1 FROM org_fact pf JOIN suppression_active px ON px.account_id = ` + acct + `
	  AND px.kind = 'phone' AND px.value = ` + phoneHashSQL("pf.normalized") + `
	  WHERE pf.id = ` + factID + `)`
}

// validSuppression reports whether kind and reason go together under the checks.
func validSuppression(kind, reason string) bool {
	switch kind {
	case SuppressAddress, SuppressDomain:
		return reason == ReasonOptOut || reason == ReasonBounce || reason == ReasonRepliedNo || reason == ReasonManual
	case SuppressPhone:
		return reason == ReasonDoNotCall || reason == ReasonTPS || reason == ReasonCTPS || reason == ReasonManual
	}
	return false
}

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
// state new or batched moves to suppressed. A lead in another state keeps it. A phone
// suppression (kind phone, value PhoneSuppressionValue of the number, reason do_not_call, tps,
// ctps or manual) reaches the leads whose org lists that number or whose pending call dials
// it, and cancels only their pending call rows: not being called is not an opt-out of email,
// so their lead state and email rows are left alone, and the engine runs for each lead whose
// call it cancelled, so its later steps go on without the call. Run it in a
// transaction. The suppression is account accountID's (0: DefaultAccountID): it blocks every
// campaign of that account and reaches only that account's leads.
func (q Queries) Suppress(ctx context.Context, accountID int64, kind, value, reason, source string) (Suppressed, error) {
	accountID = orAccount(accountID)
	v := strings.ToLower(strings.TrimSpace(value))
	if !validSuppression(kind, reason) {
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
	case kind == SuppressPhone && !phoneValueRE.MatchString(v):
		return Suppressed{}, fmt.Errorf("leadstore: suppress: %w: a phone suppression holds sha256:<hex>", ErrBadSuppressValue)
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
	var resequence []int64 // leads whose pending calls this phone suppression cancelled
	for _, id := range ids {
		res.Leads++
		if kind == SuppressPhone {
			tag, err := q.db.Exec(ctx,
				`UPDATE outreach_message SET state = 'cancelled', cancel_reason = $2
				 WHERE lead_id = $1 AND channel = 'phone' AND state IN ('draft', 'approved')`, id, cancel)
			if err != nil {
				return Suppressed{}, fmt.Errorf("leadstore: suppress: %w", err)
			}
			res.Cancelled += tag.RowsAffected()
			if tag.RowsAffected() > 0 {
				resequence = append(resequence, id)
			}
			continue
		}
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
	// A cancelled call must not hold up the rest of its lead's sequence: the engine passes
	// over it (campaign.Next) and its cancels apply now; the due times wait for
	// HealSequences, which has the lead's calendar.
	if _, err := q.resequence(ctx, resequence, time.Now(), nil); err != nil {
		return Suppressed{}, fmt.Errorf("leadstore: suppress: %w", err)
	}
	return res, nil
}

// reachSQL is the SQL condition on lead l (org o, route r) that is true when a suppression of
// kind on $1 reaches it: an address suppression reaches its route address, a pending
// message's recipient or the address given with an email consent its pending email_consent
// steps go to; a domain suppression reaches the org's domain, the route address's domain, a
// pending message's recipient domain or a consent address's domain, or a subdomain of any
// of them. Suppress
// (which leads to stop) and LiftSuppression (which leads it had stopped) share it, so the two
// cannot drift.
func reachSQL(kind string) string {
	if kind == SuppressPhone {
		return `(EXISTS (SELECT 1 FROM org_fact f WHERE f.org_id = l.org_id AND f.kind = 'phone'
		    AND ` + phoneHashSQL("f.normalized") + ` = $1)
		  OR EXISTS (SELECT 1 FROM outreach_message m JOIN org_fact f ON f.id = m.phone_fact_id
		    WHERE m.lead_id = l.id AND m.state IN ('draft', 'approved') AND ` + phoneHashSQL("f.normalized") + ` = $1))`
	}
	if kind == SuppressAddress {
		return `((r.kind = 'email' AND r.value = $1)
		  OR EXISTS (SELECT 1 FROM outreach_message m WHERE m.lead_id = l.id
		    AND m.state IN ('draft', 'approved') AND lower(m.to_address) = $1)
		  OR EXISTS (` + consentReachSQL + ` AND lower(co.consent_address) = $1))`
	}
	return `(o.domain = $1 OR right(o.domain, length($1) + 1) = '.' || $1
		  OR (r.kind = 'email' AND (split_part(r.value, '@', 2) = $1
		    OR right(split_part(r.value, '@', 2), length($1) + 1) = '.' || $1))
		  OR EXISTS (SELECT 1 FROM outreach_message m WHERE m.lead_id = l.id
		    AND m.state IN ('draft', 'approved')
		    AND (lower(split_part(m.to_address, '@', 2)) = $1
		      OR right(lower(split_part(m.to_address, '@', 2)), length($1) + 1) = '.' || $1))
		  OR EXISTS (` + consentReachSQL + `
		    AND (lower(split_part(co.consent_address, '@', 2)) = $1
		      OR right(lower(split_part(co.consent_address, '@', 2)), length($1) + 1) = '.' || $1)))`
}

// consentReachSQL opens the condition that lead l has a pending email_consent step and a
// consent address on one of its calls (co): the address its consent emails go to
// (consentAddressSQL), which its stored recipient does not name.
const consentReachSQL = `SELECT 1 FROM outreach_message cm JOIN call_outcome co ON co.message_id = cm.id
		    WHERE cm.lead_id = l.id AND co.email_consent
		      AND EXISTS (SELECT 1 FROM outreach_message pm WHERE pm.lead_id = l.id AND pm.batch_id = cm.batch_id
		        AND pm.condition = 'email_consent' AND pm.state IN ('draft', 'approved'))`

// orgDomainSuppressedSQL is a SQL condition that is true when an active domain suppression
// of any account covers org alias o's domain (the domain itself or a parent of it). A
// collector that fetches an org's pages or reads its facts (enrich, the contact search,
// contact research) skips such an org: `leads forget` and an opt-out suppress the domain,
// and nothing may gather about it again. Any account's, because the collectors serve every
// account and erasure must hold whichever account asked.
func orgDomainSuppressedSQL(o string) string {
	return `EXISTS (SELECT 1 FROM suppression_active sx WHERE sx.kind = 'domain'
	  AND (` + o + `.domain = sx.value OR right(` + o + `.domain, length(sx.value) + 1) = '.' || sx.value))`
}

// inAccountSQL is the SQL condition on lead l that is true when its campaign is account $2's.
const inAccountSQL = `l.campaign_id IN (SELECT c.id FROM campaign c WHERE c.account_id = $2)`

// Suppressions is every suppressed address, domain and phone, lower case, sorted, each once
// (outreach.NewSuppressionSet takes the first two lists). Phones holds phone suppression
// values (PhoneSuppressionValue), never a domain.
type Suppressions struct {
	Addresses []string
	Domains   []string
	Phones    []string
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
	s := Suppressions{Addresses: []string{}, Domains: []string{}, Phones: []string{}}
	for _, r := range rows {
		switch r.Kind {
		case SuppressAddress:
			s.Addresses = append(s.Addresses, r.Value)
		case SuppressDomain:
			s.Domains = append(s.Domains, r.Value)
		case SuppressPhone:
			s.Phones = append(s.Phones, r.Value)
		default:
			return Suppressions{}, fmt.Errorf("leadstore: suppression set: unknown kind %q", r.Kind)
		}
	}
	return s, nil
}
