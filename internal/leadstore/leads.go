package leadstore

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/gremy/webbership-leadhound/internal/leadconfig"
	"github.com/gremy/webbership-leadhound/internal/sitelist"
)

// The kinds a lead can be (lead_kind_check).
const (
	LeadUnsupported   = "unsupported"
	LeadD10           = "d10"
	LeadAgencyPartner = "agency_partner"
	LeadSelftest      = "selftest"
	LeadHand          = "hand"
)

// The channels a lead can be reached on (lead_channel_check).
const (
	ChannelEmail = "email"
	ChannelForm  = "form"
	ChannelNone  = "none"
)

// The states a lead can be in (lead_state_check).
const (
	LeadNew        = "new"
	LeadBatched    = "batched"
	LeadContacted  = "contacted"
	LeadReplied    = "replied"
	LeadBounced    = "bounced"
	LeadOptedOut   = "opted_out"
	LeadSuppressed = "suppressed"
	LeadSkipped    = "skipped"
	LeadClosed     = "closed"
)

// The actors a lead_event can name (lead_event_actor_check).
const (
	ActorGremy  = "gremy"
	ActorSystem = "system"
)

var (
	// ErrLeadNotFound is returned when no lead row has that id.
	ErrLeadNotFound = errors.New("leadstore: lead not found")
	// ErrUnknownLeadState is returned by SetLeadState for a state outside lead_state_check.
	ErrUnknownLeadState = errors.New("leadstore: unknown lead state")
	// ErrUnknownActor is returned for an actor other than gremy or system.
	ErrUnknownActor = errors.New("leadstore: unknown actor")
)

// ReservedTLDs are the reserved top-level domains (RFC 2606) fixture data lives on: the one
// list the store judges reserved names by (RA25).
var ReservedTLDs = []string{"example", "invalid", "test", "localhost"}

// reservedDomainRE is a Postgres regular expression matching a domain on one of ReservedTLDs.
func reservedDomainRE() string {
	return `\.(` + strings.Join(ReservedTLDs, "|") + `)$`
}

// suppressedSQL is a SQL condition that is true when a suppression row blocks the address
// addr (an SQL expression, empty for none) or the domain dom: the address itself, or a domain
// suppression on dom, on the address's domain, or on a parent of either.
func suppressedSQL(dom, addr string) string {
	return fmt.Sprintf(`EXISTS (SELECT 1 FROM suppression x WHERE
	  (x.kind = 'address' AND x.value = lower(%[2]s))
	  OR (x.kind = 'domain' AND (
	    x.value = lower(%[1]s) OR right(lower(%[1]s), length(x.value) + 1) = '.' || x.value
	    OR x.value = lower(split_part(%[2]s, '@', 2))
	    OR right(lower(split_part(%[2]s, '@', 2)), length(x.value) + 1) = '.' || x.value)))`, dom, addr)
}

// LeadRow is one lead to create.
type LeadRow struct {
	OrgID           int64
	Kind            string // a Lead* kind
	ProbeID         *int64 // the finding of a site lead
	AgencyFindingID *int64 // the finding of an agency lead
	Channel         string // a Channel* value
	RouteID         *int64
}

// UpsertLead creates the lead for l.OrgID unless the org already has one: a lead is
// created once (ON CONFLICT (org_id) DO NOTHING) and an existing lead is left as it is.
// A new lead starts in state new with its first lead_event. It returns the lead's id and
// whether this call created it.
func (q Queries) UpsertLead(ctx context.Context, l LeadRow) (leadID int64, created bool, err error) {
	err = q.db.QueryRow(ctx,
		`WITH ins AS (
		   INSERT INTO lead (org_id, kind, probe_id, agency_finding_id, channel, route_id)
		   VALUES ($1, $2, $3, $4, $5, $6)
		   ON CONFLICT (org_id) DO NOTHING RETURNING id
		 ), ev AS (
		   INSERT INTO lead_event (lead_id, state, reason, actor)
		   SELECT id, 'new', 'created', 'system' FROM ins
		 )
		 SELECT id FROM ins`,
		l.OrgID, l.Kind, l.ProbeID, l.AgencyFindingID, l.Channel, l.RouteID).Scan(&leadID)
	if err == nil {
		return leadID, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return 0, false, fmt.Errorf("leadstore: upsert lead: %w", err)
	}
	if err := q.db.QueryRow(ctx, `SELECT id FROM lead WHERE org_id = $1`, l.OrgID).Scan(&leadID); err != nil {
		return 0, false, fmt.Errorf("leadstore: upsert lead: %w", err)
	}
	return leadID, false, nil
}

// RefreshLead brings the lead of l.OrgID up to the latest facts while it is still new: its
// kind, finding, channel and route become l's (lead_finding_check still holds). A lead past
// new never changes. changed reports whether a row was rewritten (false: not new, no lead,
// or nothing differed). A caller whose classification now gives no lead moves it to
// skipped with SetLeadState.
func (q Queries) RefreshLead(ctx context.Context, l LeadRow) (changed bool, err error) {
	tag, err := q.db.Exec(ctx,
		`UPDATE lead SET kind = $2, probe_id = $3, agency_finding_id = $4, channel = $5, route_id = $6
		 WHERE org_id = $1 AND state = 'new'
		   AND (kind, probe_id, agency_finding_id, channel, route_id)
		     IS DISTINCT FROM ($2::text, $3::bigint, $4::bigint, $5::text, $6::bigint)`,
		l.OrgID, l.Kind, l.ProbeID, l.AgencyFindingID, l.Channel, l.RouteID)
	if err != nil {
		return false, fmt.Errorf("leadstore: refresh lead: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

var leadStates = []string{LeadNew, LeadBatched, LeadContacted, LeadReplied, LeadBounced, LeadOptedOut, LeadSuppressed, LeadSkipped, LeadClosed}

func checkStateActor(state, actor string) error {
	if !slices.Contains(leadStates, state) {
		return fmt.Errorf("%w: %q", ErrUnknownLeadState, state)
	}
	if actor != ActorGremy && actor != ActorSystem {
		return fmt.Errorf("%w: %q", ErrUnknownActor, actor)
	}
	return nil
}

// SetLeadState moves lead leadID to state and writes its lead_event, in one statement, so
// the pointer and the history never disagree.
func (q Queries) SetLeadState(ctx context.Context, leadID int64, state, reason, actor string) error {
	if err := checkStateActor(state, actor); err != nil {
		return fmt.Errorf("leadstore: set lead state: %w", err)
	}
	ok, err := q.setLeadStateFrom(ctx, leadID, nil, state, reason, actor)
	if err != nil {
		return fmt.Errorf("leadstore: set lead state: %w", err)
	}
	if !ok {
		return fmt.Errorf("leadstore: set lead state: lead %d: %w", leadID, ErrLeadNotFound)
	}
	return nil
}

// setLeadStateFrom moves lead leadID to state when its current state is one of from (any
// state when from is nil), with its lead_event. It reports whether the lead moved.
func (q Queries) setLeadStateFrom(ctx context.Context, leadID int64, from []string, state, reason, actor string) (bool, error) {
	var id int64
	err := q.db.QueryRow(ctx,
		`WITH u AS (
		   UPDATE lead SET state = $2, state_at = now()
		   WHERE id = $1 AND ($5::text[] IS NULL OR state = ANY($5::text[]))
		   RETURNING id
		 ), ev AS (
		   INSERT INTO lead_event (lead_id, state, reason, actor) SELECT id, $2, $3, $4 FROM u
		 )
		 SELECT id FROM u`, leadID, state, reason, actor, from).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// Lead is one lead row.
type Lead struct {
	ID              int64  `db:"id"`
	OrgID           int64  `db:"org_id"`
	Kind            string `db:"kind"`
	ProbeID         *int64 `db:"probe_id"`
	AgencyFindingID *int64 `db:"agency_finding_id"`
	Channel         string `db:"channel"`
	RouteID         *int64 `db:"route_id"`
	State           string `db:"state"`
}

// LeadByID returns lead id, or ErrLeadNotFound.
func (q Queries) LeadByID(ctx context.Context, id int64) (Lead, error) {
	leads, err := collect(ctx, q.db, pgx.RowToStructByName[Lead],
		`SELECT id, org_id, kind, probe_id, agency_finding_id, channel, route_id, state FROM lead WHERE id = $1`, id)
	if err != nil {
		return Lead{}, fmt.Errorf("leadstore: lead by id: %w", err)
	}
	if len(leads) == 0 {
		return Lead{}, fmt.Errorf("leadstore: lead %d: %w", id, ErrLeadNotFound)
	}
	return leads[0], nil
}

// Candidate is one lead CandidateLeads offers for a new batch, with what its drafts need.
type Candidate struct {
	LeadID          int64  `db:"lead_id"`
	OrgID           int64  `db:"org_id"`
	Domain          string `db:"domain"`
	OrgName         string `db:"org_name"`
	Country         string `db:"country"`
	Kind            string `db:"kind"`
	Channel         string `db:"channel"`
	RouteID         *int64 `db:"route_id"`
	RouteKind       string `db:"route_kind"`
	Route           string `db:"route"`
	ProbeID         *int64 `db:"probe_id"`
	AgencyFindingID *int64 `db:"agency_finding_id"`
	ListRank        *int   `db:"list_rank"`
	// The finding: a site lead's probe, or an agency lead's finding page and phrase.
	Major        int    `db:"major"`
	ExactVersion string `db:"exact_version"`
	SiteName     string `db:"site_name"`
	Phrase       string `db:"phrase"`
	FindingURL   string `db:"finding_url"`
	// The priority: the score the profile gives, whether the operator pinned the lead, and
	// the reasons ("pinned" first, then Score's).
	Score  int      `db:"score"`
	Pinned bool     `db:"pinned"`
	Why    []string `db:"-"`
}

// CandidateOptions shapes CandidateLeads.
type CandidateOptions struct {
	Limit         int      // at most this many (<= 0: all of them)
	KindsPriority []string // lead kinds in the order they come; kinds not listed come last
	// Profile is the file's priority profile (leadconfig.Config.PriorityProfile). The
	// operator's latest saved version replaces it; a zero Profile means kind points from
	// KindsPriority alone (leadconfig.KindPoints), the order before priorities existed.
	Profile leadconfig.Profile
	// Today is the operator's date a defer is judged by (its year, month and day as given);
	// zero is the database's current date.
	Today time.Time
	// IncludeReserved lets leads on ReservedTLDs through. Only shadow and dry-run callers
	// (and tests) set it; a live batch never does (RA25).
	IncludeReserved bool
}

// CandidateLeads returns the leads a new batch may draft: state new, a channel other than
// none, no suppression on the route's address, the org's domain or the address's domain,
// no other lead on one of those domains already reached (batched, contacted, replied,
// bounced or opted out), not a selftest or hand lead, a site lead's probe still its site's
// latest (else it waits for RefreshLead), not deferred through o.Today, not in a country
// the profile excludes, and, unless o.IncludeReserved, not on a reserved TLD (ReservedTLDs;
// RA25). They come pinned first (the newest pin first), then by their score under the
// profile (EffectiveProfile over o.Profile, computed in SQL so the limit stays in the
// database), then by their site's list rank (unranked last), then by id.
func (q Queries) CandidateLeads(ctx context.Context, o CandidateOptions) ([]Candidate, error) {
	var lim *int
	if o.Limit > 0 {
		lim = &o.Limit
	}
	fallback := o.Profile
	if fallback.IsZero() {
		fallback = leadconfig.Profile{Kinds: leadconfig.KindPoints(o.KindsPriority)}
	}
	profile, _, err := q.EffectiveProfile(ctx, fallback)
	if err != nil {
		return nil, fmt.Errorf("leadstore: candidate leads: %w", err)
	}
	pf, err := profileJSON(profile)
	if err != nil {
		return nil, fmt.Errorf("leadstore: candidate leads: %w", err)
	}
	var today *string
	if !o.Today.IsZero() {
		d := o.Today.Format(time.DateOnly)
		today = &d
	}
	// The query is set-based: the candidates are read once (c) with their domains as plain
	// columns, every lead blocked by a suppression or a reached domain is found by equality
	// joins (blocked), and the candidates not blocked are ordered and limited. A correlated
	// anti-join per candidate over the suppressions and the reached leads grew with their
	// product. The checks are those of suppressedSQL and the reached-domain rule:
	//   - an address suppression on the route's address, lowered;
	//   - a domain suppression on the org's domain or the address's domain (both lowered),
	//     or on a parent of either at a label boundary (keys: the domain itself and every
	//     suffix after one of its dots);
	//   - another lead in a reached state whose org domain, or whose email route's domain,
	//     is the org's domain or the address's domain.
	rows, err := collect(ctx, q.db, pgx.RowToStructByName[Candidate],
		`WITH c AS MATERIALIZED (
		   SELECT l.id AS lead_id, l.org_id, o.domain, o.name AS org_name, o.country, l.kind, l.channel,
		     l.route_id, coalesce(r.kind, '') AS route_kind, coalesce(r.value, '') AS route,
		     l.probe_id, l.agency_finding_id, s.list_rank,
		     coalesce(p.major, 0) AS major, coalesce(p.exact_version, '') AS exact_version,
		     coalesce(p.site_name, '') AS site_name,
		     coalesce(af.phrase, '') AS phrase, coalesce(af.url, '') AS finding_url,
		     sc.score, coalesce(pr.action = 'pin', false) AS pinned,
		     CASE WHEN pr.action = 'pin' THEN pr.id END AS pin_id,
		     lower(a.addr) AS laddr, lower(o.domain) AS ldom, lower(split_part(a.addr, '@', 2)) AS sdom, ad.adom
		   FROM lead l
		   CROSS JOIN (SELECT $2::jsonb AS pf) pf
		   JOIN org o ON o.id = l.org_id
		   LEFT JOIN contact_route r ON r.id = l.route_id
		   LEFT JOIN site_probe p ON p.id = l.probe_id
		   LEFT JOIN site s ON s.id = coalesce(p.site_id, o.site_id)
		   LEFT JOIN agency_finding af ON af.id = l.agency_finding_id
		   LEFT JOIN LATERAL (
		     SELECT lp.id, lp.action, lp.until FROM lead_priority lp
		     WHERE lp.lead_id = l.id ORDER BY lp.id DESC LIMIT 1
		   ) pr ON true
		   CROSS JOIN LATERAL (
		     SELECT CASE WHEN r.kind = 'email' THEN r.value ELSE '' END AS addr
		   ) a
		   CROSS JOIN LATERAL (
		     SELECT nullif(lower(split_part(a.addr, '@', 2)), '') AS adom
		   ) ad
		   CROSS JOIN LATERAL (SELECT `+scoreSQL+` AS score) sc
		   WHERE l.state = 'new' AND l.channel <> 'none'
		     AND (l.probe_id IS NULL OR p.id = s.last_probe_id)
		     AND l.kind NOT IN ('selftest', 'hand') AND o.kind <> 'selftest'
		     AND ($4 OR (o.domain !~ $3 AND coalesce(ad.adom, '') !~ $3))
		     AND NOT coalesce(pr.action = 'defer' AND pr.until >= coalesce($5::date, current_date), false)
		     AND NOT pf.pf->'exclude_countries' @> to_jsonb(o.country)
		 ), reached AS MATERIALIZED (
		   SELECT l2.id AS lead_id, o2.domain AS d FROM lead l2 JOIN org o2 ON o2.id = l2.org_id
		   WHERE l2.state IN ('batched', 'contacted', 'replied', 'bounced', 'opted_out')
		   UNION ALL
		   SELECT l2.id, nullif(lower(split_part(r2.value, '@', 2)), '') FROM lead l2
		   JOIN contact_route r2 ON r2.id = l2.route_id AND r2.kind = 'email'
		   WHERE l2.state IN ('batched', 'contacted', 'replied', 'bounced', 'opted_out')
		 ), domain_keys AS (
		   SELECT c.lead_id, k.key FROM c
		   CROSS JOIN LATERAL (
		     SELECT c.ldom AS key UNION ALL SELECT c.sdom
		     UNION ALL
		     SELECT array_to_string(d.parts[i:], '.')
		     FROM (SELECT string_to_array(c.ldom, '.') AS parts UNION ALL SELECT string_to_array(c.sdom, '.')) d
		     CROSS JOIN LATERAL generate_subscripts(d.parts, 1) i
		     WHERE i > 1
		   ) k
		 ), blocked AS (
		   SELECT c.lead_id FROM c JOIN suppression x ON x.kind = 'address' AND x.value = c.laddr
		   UNION
		   SELECT k.lead_id FROM domain_keys k JOIN suppression x ON x.kind = 'domain' AND x.value = k.key
		   UNION
		   SELECT c.lead_id FROM c JOIN reached x ON x.d = c.domain AND x.lead_id <> c.lead_id
		   UNION
		   SELECT c.lead_id FROM c JOIN reached x ON x.d = c.adom AND x.lead_id <> c.lead_id
		 )
		 SELECT lead_id, org_id, domain, org_name, country, kind, channel, route_id, route_kind, route,
		   probe_id, agency_finding_id, list_rank, major, exact_version, site_name, phrase, finding_url,
		   score, pinned
		 FROM c
		 WHERE lead_id NOT IN (SELECT lead_id FROM blocked)
		 ORDER BY pinned DESC, pin_id DESC NULLS LAST, score DESC, list_rank NULLS LAST, lead_id
		 LIMIT $1`, lim, pf, reservedDomainRE(), o.IncludeReserved, today)
	if err != nil {
		return nil, fmt.Errorf("leadstore: candidate leads: %w", err)
	}
	for i := range rows {
		_, why := Score(profile, rows[i])
		if rows[i].Pinned {
			why = append([]string{"pinned"}, why...)
		}
		rows[i].Why = why
	}
	return rows, nil
}

// Contacted is what MarkContacted did.
type Contacted struct {
	OrgID      int64
	OrgCreated bool
	LeadID     int64
	LeadKind   string
	// State is the lead's state afterwards: contacted, unless it had already moved past
	// contact (replied, bounced, opted out, suppressed, closed), which is left as it is.
	State     string
	Moved     bool  // the lead moved to contacted in this call
	Cancelled int64 // pending messages cancelled ('hand-contacted')
}

// MarkContacted records that Gremy reached domain by hand (RA15). It makes sure a
// site_owner org exists for the domain's registrable domain (source probe, linked to its
// site row when one exists), then upserts its lead: from the org's finding when there is
// one (a Drupal 6–9 probe → unsupported, 10 → d10; an agency's first finding →
// agency_partner), else kind hand with no finding. A lead in state new, batched or skipped
// moves to contacted (actor gremy, reason note), and its pending messages are cancelled
// with cancel_reason 'hand-contacted'. No batch drafts it again. Run it in a transaction.
func (q Queries) MarkContacted(ctx context.Context, domain, note string) (Contacted, error) {
	d, err := sitelist.Registrable(domain)
	if err != nil {
		return Contacted{}, fmt.Errorf("leadstore: mark contacted: %w: %w", ErrNoDomain, err)
	}
	var siteID *int64
	if id, err := q.SiteIDByDomain(ctx, d); err == nil {
		siteID = &id
	} else if !errors.Is(err, ErrSiteNotFound) {
		return Contacted{}, fmt.Errorf("leadstore: mark contacted: %w", err)
	}
	orgID, orgKind, created, err := q.insertOrg(ctx, Org{
		Kind: OrgSiteOwner, Domain: d, Country: sitelist.CountryOf(d), SiteID: siteID, Source: OrgSourceProbe,
	})
	if err != nil {
		return Contacted{}, fmt.Errorf("leadstore: mark contacted: %w", err)
	}
	if !created && siteID != nil && orgKind == OrgSiteOwner {
		if err := q.OrgSetSite(ctx, orgID, *siteID); err != nil && !errors.Is(err, ErrOrgSiteSet) {
			return Contacted{}, fmt.Errorf("leadstore: mark contacted: %w", err)
		}
	}
	row, err := q.handLeadRow(ctx, orgID, orgKind)
	if err != nil {
		return Contacted{}, fmt.Errorf("leadstore: mark contacted: %w", err)
	}
	leadID, _, err := q.UpsertLead(ctx, row)
	if err != nil {
		return Contacted{}, fmt.Errorf("leadstore: mark contacted: %w", err)
	}
	lead, err := q.LeadByID(ctx, leadID)
	if err != nil {
		return Contacted{}, fmt.Errorf("leadstore: mark contacted: %w", err)
	}
	res := Contacted{OrgID: orgID, OrgCreated: created, LeadID: leadID, LeadKind: lead.Kind, State: lead.State}
	if lead.State == LeadContacted || !slices.Contains([]string{LeadNew, LeadBatched, LeadSkipped}, lead.State) {
		return res, nil
	}
	n, err := q.CancelPending(ctx, CancelTarget{LeadID: leadID}, CancelHandContacted)
	if err != nil {
		return Contacted{}, fmt.Errorf("leadstore: mark contacted: %w", err)
	}
	reason := note
	if reason == "" {
		reason = "hand-contacted"
	}
	moved, err := q.setLeadStateFrom(ctx, leadID, []string{LeadNew, LeadBatched, LeadSkipped}, LeadContacted, reason, ActorGremy)
	if err != nil {
		return Contacted{}, fmt.Errorf("leadstore: mark contacted: %w", err)
	}
	res.Cancelled, res.Moved = n, moved
	if moved {
		res.State = LeadContacted
	}
	return res, nil
}

// handLeadRow is the lead MarkContacted creates for org orgID: its finding when it has
// one, else a hand lead; its channel and route from its best route.
func (q Queries) handLeadRow(ctx context.Context, orgID int64, orgKind string) (LeadRow, error) {
	row := LeadRow{OrgID: orgID, Kind: LeadHand, Channel: ChannelNone}
	switch orgKind {
	case OrgSiteOwner:
		var probeID *int64
		var major int
		err := q.db.QueryRow(ctx,
			`SELECT p.id, p.major FROM org o JOIN site s ON s.id = o.site_id
			 JOIN site_probe p ON p.id = s.last_probe_id AND p.is_drupal
			 WHERE o.id = $1`, orgID).Scan(&probeID, &major)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return LeadRow{}, err
		}
		switch {
		case probeID != nil && major >= 6 && major <= 9:
			row.Kind, row.ProbeID = LeadUnsupported, probeID
		case probeID != nil && major == 10:
			row.Kind, row.ProbeID = LeadD10, probeID
		}
	case OrgAgency:
		var findingID int64
		err := q.db.QueryRow(ctx, `SELECT min(id) FROM agency_finding WHERE org_id = $1 HAVING count(*) > 0`, orgID).Scan(&findingID)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return LeadRow{}, err
		}
		if err == nil {
			row.Kind, row.AgencyFindingID = LeadAgencyPartner, &findingID
		}
	}
	var routeID int64
	var routeKind string
	err := q.db.QueryRow(ctx,
		`SELECT id, kind FROM contact_route WHERE org_id = $1
		 ORDER BY CASE WHEN kind = 'email' AND mx_ok THEN 0 WHEN kind = 'form' THEN 1 ELSE 2 END, id
		 LIMIT 1`, orgID).Scan(&routeID, &routeKind)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return LeadRow{}, err
	}
	if err == nil {
		row.RouteID, row.Channel = &routeID, routeKind
	}
	return row, nil
}
