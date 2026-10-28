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
)

// Contact research (CP3b, migration 00018): a starred lead whose org has no usable route
// queues a search for a published contact; the research job claims queued rows, runs
// internal/research and finishes them. The store never calls Claude.

// What asked for a research attempt (contact_research_trigger_check).
const (
	ResearchStar   = "star"
	ResearchButton = "button"
	ResearchCLI    = "cli"
)

// A research attempt's status (contact_research_status_check).
const (
	ResearchQueued  = "queued"
	ResearchLooking = "looking"
	ResearchFound   = "found"
	ResearchNothing = "nothing"
	ResearchCapped  = "capped"
	ResearchError   = "error"
)

// What a research attempt looks for (contact_research_want_check).
const (
	WantEmail = "email"
	WantPhone = "phone"
)

// ResearchWindow is how long after an attempt that ran another may run for the same org.
const ResearchWindow = researchWindowDays * 24 * time.Hour

// researchWindowDays is ResearchWindow in days, as the SQL counts it.
const researchWindowDays = 90

// ResearchStaleAfter is how long a row may stay looking before it is taken for the remains
// of a run that crashed or was killed, and finished as error (reapStaleResearch). It is well
// past one attempt's worst case, so a run still going is never reaped: the Claude call is at
// most 2 web-tool variants (research.Run retries a bad request with WebToolsBasic) x 3 legs
// (claude.MaxContinuations 2) x 3 tries (the SDK's 2 retries) x the 2-minute request timeout
// = 36 min; verifying is one site client's budgets of 4 robots and 4 site requests, each at
// most the 10 s site timeout plus one honoured Retry-After of at most 2 min = about 17 min;
// about 53 min in all, so two hours.
const ResearchStaleAfter = 2 * time.Hour

var (
	// ErrResearchTooSoon is returned by RequestResearch (trigger button or cli) when an
	// attempt for the org ran within ResearchWindow; the error is a *ResearchTooSoonError.
	ErrResearchTooSoon = errors.New("leadstore: research ran too recently")
	// ErrResearchBlocked is returned by RequestResearch (trigger button or cli) for a lead
	// that is suppressed, deferred or past new: no research is asked for it.
	ErrResearchBlocked = errors.New("leadstore: research not allowed for this lead")
	// ErrBadResearch is returned for an unknown trigger, status or want, an empty want, or
	// counts the table would refuse.
	ErrBadResearch = errors.New("leadstore: bad research")
	// ErrResearchNotLooking is returned by FinishResearch for a row that does not exist or
	// is not looking (never claimed, or already finished).
	ErrResearchNotLooking = errors.New("leadstore: research is not looking")
)

// ResearchTooSoonError is ErrResearchTooSoon with when: the attempt that ran last and the
// first time another may run.
type ResearchTooSoonError struct {
	LastID    int64
	StartedAt time.Time
	NextAt    time.Time
}

func (e *ResearchTooSoonError) Error() string {
	return fmt.Sprintf("%s: the last attempt ran %s, the next may run from %s",
		ErrResearchTooSoon, e.StartedAt.Format(time.DateOnly), e.NextAt.Format(time.DateOnly))
}

// Is makes errors.Is(err, ErrResearchTooSoon) true.
func (e *ResearchTooSoonError) Is(target error) bool { return target == ErrResearchTooSoon }

// WantFor is what to look for under a country's channel policy (leadconfig.Country.Channel,
// as cfg.Policy(country).Channel gives it, the rule leads build picks a channel by): an
// email country looks for a published role address; a form country (DE, CH: never email)
// looks for a business phone, stored as a fact and never callable before CP5's rules; a
// country that is off (none, or missing from leads.yaml) gets nothing, so no request.
func WantFor(policy string) []string {
	switch policy {
	case leadconfig.ChannelEmail:
		return []string{WantEmail}
	case leadconfig.ChannelForm:
		return []string{WantPhone}
	}
	return nil
}

// Research is one contact_research row, with its org.
type Research struct {
	ID          int64      `db:"id"`
	OrgID       int64      `db:"org_id"`
	LeadID      int64      `db:"lead_id"`
	CampaignID  int64      `db:"campaign_id"`
	Trigger     string     `db:"trigger"`
	Status      string     `db:"status"`
	Want        []string   `db:"want"`
	Model       string     `db:"model"`
	CostMicros  *int64     `db:"cost_micros"`
	Candidates  int        `db:"candidates"`
	Verified    int        `db:"verified"`
	RequestedAt time.Time  `db:"requested_at"`
	StartedAt   *time.Time `db:"started_at"`
	FinishedAt  *time.Time `db:"finished_at"`
	Actor       string     `db:"actor"`
	// The org researched.
	Domain  string `db:"domain"`
	OrgName string `db:"org_name"`
	Country string `db:"country"`
}

// researchCols are Research's columns over contact_research cr joined to org o.
const researchCols = `cr.id, cr.org_id, cr.lead_id, cr.campaign_id, cr.trigger, cr.status, cr.want, cr.model,
	cr.cost_micros, cr.candidates, cr.verified, cr.requested_at, cr.started_at, cr.finished_at, cr.actor,
	o.domain, o.name AS org_name, o.country`

// researchCountsSQL is true for a row of contact_research cr that counts toward the 90-day
// window: it ran (the job took it) and the spend cap did not refuse it before any call. A
// queued row that never ran does not count.
const researchCountsSQL = `cr.started_at IS NOT NULL AND cr.status <> 'capped'`

// normWant checks want and returns it sorted, each once.
func normWant(want []string) ([]string, error) {
	out := []string{}
	for _, w := range want {
		if w != WantEmail && w != WantPhone {
			return nil, fmt.Errorf("%w: want %q", ErrBadResearch, w)
		}
		if !slices.Contains(out, w) {
			out = append(out, w)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: want is empty", ErrBadResearch)
	}
	slices.Sort(out)
	return out, nil
}

// RequestResearch queues a research attempt for lead leadID's org, looking for want (see
// WantFor), asked by trigger (star, button or cli) and actor (operator or system).
//
//   - An org with a queued or looking row gets no second one: that row's id, created false.
//     A row looking longer than ResearchStaleAfter is first finished as error
//     (reapStaleResearch), and then counts as an attempt that ran.
//   - An org with an attempt that ran within ResearchWindow gets none: a star returns the
//     last attempt's id, created false and no error (the star itself must still succeed);
//     button and cli get a *ResearchTooSoonError naming the date the next may run.
//   - A lead past new, suppressed (on its org's domain or its route's address, for its
//     campaign's account) or deferred today gets none: a star returns 0, created false, no
//     error; button and cli get ErrResearchBlocked.
//
// A lead that does not exist is ErrLeadNotFound.
func (q Queries) RequestResearch(ctx context.Context, leadID int64, trigger string, want []string, actor string) (id int64, created bool, err error) {
	wrap := func(err error) error { return fmt.Errorf("leadstore: request research lead %d: %w", leadID, err) }
	if trigger != ResearchStar && trigger != ResearchButton && trigger != ResearchCLI {
		return 0, false, wrap(fmt.Errorf("%w: trigger %q", ErrBadResearch, trigger))
	}
	if actor != ActorOperator && actor != ActorSystem {
		return 0, false, wrap(fmt.Errorf("%w: %q", ErrUnknownActor, actor))
	}
	want, err = normWant(want)
	if err != nil {
		return 0, false, wrap(err)
	}
	var orgID, campaignID int64
	var state string
	var suppressed, deferred bool
	err = q.db.QueryRow(ctx,
		`SELECT l.org_id, l.campaign_id, l.state,
		   `+suppressedSQL("cg.account_id", "og.domain", "CASE WHEN r.kind = 'email' THEN r.value ELSE '' END")+`,
		   coalesce((SELECT lp.action = 'defer' AND lp.until >= current_date FROM lead_priority lp
		             WHERE lp.lead_id = l.id ORDER BY lp.id DESC LIMIT 1), false)
		 FROM lead l JOIN org og ON og.id = l.org_id JOIN campaign cg ON cg.id = l.campaign_id
		 LEFT JOIN contact_route r ON r.id = l.route_id
		 WHERE l.id = $1`, leadID).Scan(&orgID, &campaignID, &state, &suppressed, &deferred)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, wrap(ErrLeadNotFound)
	}
	if err != nil {
		return 0, false, wrap(err)
	}
	if state != LeadNew || suppressed || deferred {
		if trigger == ResearchStar {
			return 0, false, nil
		}
		why := "deferred"
		switch {
		case state != LeadNew:
			why = "state " + state
		case suppressed:
			why = "suppressed"
		}
		return 0, false, wrap(fmt.Errorf("%w: %s", ErrResearchBlocked, why))
	}
	if err := q.reapStaleResearch(ctx, orgID); err != nil {
		return 0, false, wrap(err)
	}
	if id, ok, err := q.openResearch(ctx, orgID); err != nil || ok {
		if err != nil {
			return 0, false, wrap(err)
		}
		return id, false, nil
	}
	var lastID int64
	var lastAt time.Time
	err = q.db.QueryRow(ctx,
		`SELECT cr.id, cr.started_at FROM contact_research cr
		 WHERE cr.org_id = $1 AND `+researchCountsSQL+` AND cr.started_at > now() - make_interval(days => $2::int)
		 ORDER BY cr.started_at DESC, cr.id DESC LIMIT 1`, orgID, researchWindowDays).Scan(&lastID, &lastAt)
	switch {
	case err == nil:
		if trigger == ResearchStar {
			return lastID, false, nil
		}
		return 0, false, wrap(&ResearchTooSoonError{LastID: lastID, StartedAt: lastAt, NextAt: lastAt.Add(ResearchWindow)})
	case !errors.Is(err, pgx.ErrNoRows):
		return 0, false, wrap(err)
	}
	// The open-row index settles a race with another request: the loser reads the winner's.
	err = q.db.QueryRow(ctx,
		`INSERT INTO contact_research (org_id, lead_id, campaign_id, trigger, want, actor)
		 VALUES ($1, $2, $3, $4, $5, $6)
		 ON CONFLICT (org_id) WHERE status IN ('queued', 'looking') DO NOTHING RETURNING id`,
		orgID, leadID, campaignID, trigger, want, actor).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		id, _, err = q.openResearch(ctx, orgID)
		if err != nil {
			return 0, false, wrap(err)
		}
		return id, false, nil
	}
	if err != nil {
		return 0, false, wrap(err)
	}
	return id, true, nil
}

// openResearch returns org orgID's queued or looking row, if it has one.
func (q Queries) openResearch(ctx context.Context, orgID int64) (int64, bool, error) {
	var id int64
	err := q.db.QueryRow(ctx,
		`SELECT id FROM contact_research WHERE org_id = $1 AND status IN ('queued', 'looking')`, orgID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return id, true, nil
}

// reapStaleResearch finishes as error every row that has been looking longer than
// ResearchStaleAfter (orgID 0: every org's), the remains of a run that crashed or was
// killed before FinishResearch. It is finished, never put back in the queue: the dead run
// may already have been billed (claude.Capped writes its ai_usage row after the call, so a
// run killed mid-call leaves no proof either way), so running it again could pay twice. As
// an error it counts toward the 90-day window like any attempt that ran (researchCountsSQL),
// stores nothing, and frees contact_research_open_key, so the org is never stranded: the
// next request after the window queues a fresh row. Its cost is unknown (cost_micros NULL)
// and it verified nothing. Only status and finished_at change (both in the column grant);
// finished_at is now, after started_at, as contact_research_times_check wants.
func (q Queries) reapStaleResearch(ctx context.Context, orgID int64) error {
	if _, err := q.db.Exec(ctx,
		`UPDATE contact_research SET status = 'error', finished_at = greatest(now(), started_at)
		 WHERE status = 'looking' AND started_at < now() - make_interval(secs => $1)
		   AND ($2::bigint = 0 OR org_id = $2::bigint)`,
		ResearchStaleAfter.Seconds(), orgID); err != nil {
		return fmt.Errorf("reap stale research: %w", err)
	}
	return nil
}

// ClaimResearch takes at most max queued rows (max <= 0: one) for the research job and
// marks them looking (started_at now): starred leads' rows first, then the rest, each oldest
// first. A row asked for by a star whose lead is no longer starred is skipped and left
// queued (starring the lead again lets the job take it); a row whose org's domain is
// suppressed (forgotten, opted out) is never taken (orgDomainSuppressedSQL). Two jobs never take one row (SKIP
// LOCKED). The rows come back in the order they were taken. First, every row looking
// longer than ResearchStaleAfter is finished as error (reapStaleResearch), so a crashed
// run's row never holds its org's open slot for good.
func (q Queries) ClaimResearch(ctx context.Context, max int) ([]Research, error) {
	if max <= 0 {
		max = 1
	}
	if err := q.reapStaleResearch(ctx, 0); err != nil {
		return nil, fmt.Errorf("leadstore: claim research: %w", err)
	}
	rows, err := collect(ctx, q.db, pgx.RowToStructByName[Research],
		`WITH pick AS (
		   SELECT cr.id, coalesce(st.action <> 'unstar', false) AS starred, cr.requested_at
		   FROM contact_research cr
		   LEFT JOIN LATERAL (
		     SELECT x.action FROM lead_star x WHERE x.lead_id = cr.lead_id ORDER BY x.id DESC LIMIT 1
		   ) st ON true
		   JOIN org ro ON ro.id = cr.org_id
		   WHERE cr.status = 'queued'
		     AND (cr.trigger <> 'star' OR coalesce(st.action <> 'unstar', false))
		     AND NOT `+orgDomainSuppressedSQL("ro")+`
		   ORDER BY 2 DESC, cr.requested_at, cr.id
		   LIMIT $1
		   FOR UPDATE OF cr SKIP LOCKED
		 ), took AS (
		   UPDATE contact_research c SET status = 'looking', started_at = now()
		   FROM pick WHERE c.id = pick.id
		   RETURNING c.*, pick.starred
		 )
		 SELECT `+researchCols+`
		 FROM took cr JOIN org o ON o.id = cr.org_id
		 ORDER BY cr.starred DESC, cr.requested_at, cr.id`, max)
	if err != nil {
		return nil, fmt.Errorf("leadstore: claim research: %w", err)
	}
	return rows, nil
}

// FinishResearch ends looking row id with status (found, nothing, capped or error), the
// model and priced cost (nil when unknown) of the run, and its candidate and verified
// counts. found needs verified > 0 and nothing verified = 0 (internal/research's mapping:
// ai.ErrSpendCap → capped, another error → error, Verified 0 → nothing, else found). A row
// that is not looking is ErrResearchNotLooking.
func (q Queries) FinishResearch(ctx context.Context, id int64, status, model string, costMicros *int64, candidates, verified int) error {
	wrap := func(err error) error { return fmt.Errorf("leadstore: finish research %d: %w", id, err) }
	switch {
	case status != ResearchFound && status != ResearchNothing && status != ResearchCapped && status != ResearchError:
		return wrap(fmt.Errorf("%w: status %q", ErrBadResearch, status))
	case candidates < 0 || verified < 0 || verified > candidates:
		return wrap(fmt.Errorf("%w: %d verified of %d candidates", ErrBadResearch, verified, candidates))
	case status == ResearchFound && verified == 0, status == ResearchNothing && verified > 0:
		return wrap(fmt.Errorf("%w: status %s with %d verified", ErrBadResearch, status, verified))
	case costMicros != nil && *costMicros < 0:
		return wrap(fmt.Errorf("%w: cost %d", ErrBadResearch, *costMicros))
	}
	tag, err := q.db.Exec(ctx,
		`UPDATE contact_research SET status = $2, model = $3, cost_micros = $4, candidates = $5, verified = $6,
		   finished_at = greatest(now(), started_at)
		 WHERE id = $1 AND status = 'looking'`, id, status, model, costMicros, candidates, verified)
	if err != nil {
		return wrap(err)
	}
	if tag.RowsAffected() == 0 {
		return wrap(ErrResearchNotLooking)
	}
	return nil
}

// LeadResearch is what the lead page shows: the latest research attempt of the lead's org
// (asked through any of its leads), and when "Look again" may run.
type LeadResearch struct {
	Latest *Research // nil: never researched
	// NextAt is the first time another attempt may run when one ran within ResearchWindow;
	// nil when one may run now (or one is queued or looking: Latest says so).
	NextAt *time.Time
}

// ResearchForLead returns lead leadID's LeadResearch. A lead that does not exist is
// ErrLeadNotFound.
func (q Queries) ResearchForLead(ctx context.Context, leadID int64) (LeadResearch, error) {
	var orgID int64
	err := q.db.QueryRow(ctx, `SELECT org_id FROM lead WHERE id = $1`, leadID).Scan(&orgID)
	if errors.Is(err, pgx.ErrNoRows) {
		return LeadResearch{}, fmt.Errorf("leadstore: research for lead %d: %w", leadID, ErrLeadNotFound)
	}
	if err != nil {
		return LeadResearch{}, fmt.Errorf("leadstore: research for lead %d: %w", leadID, err)
	}
	rows, err := collect(ctx, q.db, pgx.RowToStructByName[Research],
		`SELECT `+researchCols+` FROM contact_research cr JOIN org o ON o.id = cr.org_id
		 WHERE cr.org_id = $1 ORDER BY cr.id DESC LIMIT 1`, orgID)
	if err != nil {
		return LeadResearch{}, fmt.Errorf("leadstore: research for lead %d: %w", leadID, err)
	}
	var out LeadResearch
	if len(rows) == 0 {
		return out, nil
	}
	out.Latest = &rows[0]
	var last *time.Time
	if err := q.db.QueryRow(ctx,
		`SELECT max(cr.started_at) FROM contact_research cr
		 WHERE cr.org_id = $1 AND `+researchCountsSQL+` AND cr.started_at > now() - make_interval(days => $2::int)`,
		orgID, researchWindowDays).Scan(&last); err != nil {
		return LeadResearch{}, fmt.Errorf("leadstore: research for lead %d: %w", leadID, err)
	}
	if last != nil {
		next := last.Add(ResearchWindow)
		out.NextAt = &next
	}
	return out, nil
}

// ResearchListOptions shapes ResearchList.
type ResearchListOptions struct {
	Status string // only rows in this status; "" every status
	Limit  int    // at most this many (<= 0: 50)
}

// ResearchList returns research attempts, newest first, for leads research list.
func (q Queries) ResearchList(ctx context.Context, o ResearchListOptions) ([]Research, error) {
	if o.Limit <= 0 {
		o.Limit = 50
	}
	rows, err := collect(ctx, q.db, pgx.RowToStructByName[Research],
		`SELECT `+researchCols+` FROM contact_research cr JOIN org o ON o.id = cr.org_id
		 WHERE ($1::text = '' OR cr.status = $1::text) ORDER BY cr.id DESC LIMIT $2`, o.Status, o.Limit)
	if err != nil {
		return nil, fmt.Errorf("leadstore: research list: %w", err)
	}
	return rows, nil
}

// Why a starred lead is or is not in the next batch (StarredStatus), one per lead.
const (
	StarredReady        = "ready"         // the batch's candidate query offers it
	StarredNoEmailYet   = "no_email_yet"  // no usable route (channel none), not researched yet
	StarredLooking      = "looking"       // a research attempt is queued or looking
	StarredFoundNothing = "found_nothing" // the last research found nothing (Date: when)
	StarredCountryRule  = "country_rule"  // its country is off, excluded, or form-only with no form
	StarredSuppressed   = "suppressed"    // a suppression, or another lead on its domain already reached
	StarredDeferred     = "deferred"      // deferred through Date
	StarredRankedOut    = "ranked_out"    // offered, but past the batch's limit or skip_rank_over
	StarredNotNew       = "not_new"       // already batched, contacted, skipped, ... (Detail: the state)
	StarredOther        = "other"         // left out for another reason (Detail says which)
)

// StarredStatusOptions shapes StarredStatus: the batch's own CandidateOptions (the account,
// campaign, limit and settings it would draft with), plus the country rule.
type StarredStatusOptions struct {
	CandidateOptions
	// Countries maps a country to its channel policy (leadconfig.Country.Channel: email,
	// form or none); a country missing from it is off, as cfg.Policy has it. nil: the
	// country rule is not judged (only the profile's exclude_countries is).
	Countries map[string]string
}

// CampaignCountryWords is why a starred lead whose country its campaign does not list is
// out (StarredCountryRule's detail).
const CampaignCountryWords = "not in this campaign's countries"

// StarredState is one starred lead and the one reason it is or is not ready.
type StarredState struct {
	LeadID   int64
	OrgID    int64
	Position int64
	Domain   string
	Country  string
	Kind     string
	Channel  string
	State    string
	Reason   string     // one of the Starred* reasons
	Detail   string     // plain words naming the rule, state or setting; "" when Reason says it all
	Date     *time.Time // found_nothing: when it finished; deferred: the last day deferred
}

// starredFacts is what StarredStatus reads about every starred lead to name a reason.
type starredFacts struct {
	LeadID         int64      `db:"lead_id"`
	OrgID          int64      `db:"org_id"`
	Position       int64      `db:"position"`
	Domain         string     `db:"domain"`
	Country        string     `db:"country"`
	Kind           string     `db:"kind"`
	Channel        string     `db:"channel"`
	State          string     `db:"state"`
	Outreach       bool       `db:"outreach"`
	Selftest       bool       `db:"selftest"`
	ProbeStale     bool       `db:"probe_stale"`
	Reserved       bool       `db:"reserved"`
	HasProbe       bool       `db:"has_probe"`
	ListRank       *int       `db:"list_rank"`
	DeferUntil     *time.Time `db:"defer_until"`
	Suppressed     bool       `db:"suppressed"`
	Reached        bool       `db:"reached"`
	ResearchStatus *string    `db:"research_status"`
	ResearchAt     *time.Time `db:"research_at"`
}

// StarredStatus returns every starred lead of o's account (and campaign, when set) in the
// operator's order, each with one reason. Ready is decided by CandidateLeads itself, called
// with o's own options (its Limit applied to the same order), so a lead called ready is
// exactly one the batch's candidate query offers; the reasons for the rest read the same
// predicates CandidateLeads applies (leads.go), checked in this order: suppressed (or opted out), not
// new, reached, deferred, the country rule (the profile's exclude_countries, then o.Countries),
// another exclusion (kind not drafted or turned off, a stale probe, a selftest or reserved
// domain), skip_rank_over, then for a lead with no usable route its research (looking,
// found nothing) or no email yet. A lead offered past o.Limit is ranked out. Cross-campaign
// skips (cmd's OpenElsewhere) are not judged here.
func (q Queries) StarredStatus(ctx context.Context, o StarredStatusOptions) ([]StarredState, error) {
	all := o.CandidateOptions
	all.Limit = 0
	cands, err := q.CandidateLeads(ctx, all)
	if err != nil {
		return nil, fmt.Errorf("leadstore: starred status: %w", err)
	}
	offered := map[int64]int{} // lead → its place in the candidate order
	for i, c := range cands {
		offered[c.LeadID] = i
	}
	fallback := o.Profile
	if fallback.IsZero() {
		fallback = leadconfig.Profile{Kinds: leadconfig.KindPoints(o.KindsPriority)}
	}
	profile, _, err := q.EffectiveProfile(ctx, fallback)
	if err != nil {
		return nil, fmt.Errorf("leadstore: starred status: %w", err)
	}
	var today *string
	if !o.Today.IsZero() {
		d := o.Today.Format(time.DateOnly)
		today = &d
	}
	facts, err := collect(ctx, q.db, pgx.RowToStructByName[starredFacts],
		`SELECT l.id AS lead_id, l.org_id, st.position, o.domain, o.country, l.kind, l.channel, l.state,
		   coalesce(lk.outreach, false) AS outreach, o.kind = 'selftest' AS selftest,
		   (l.probe_id IS NOT NULL AND p.id IS DISTINCT FROM s.last_probe_id) AS probe_stale,
		   (o.domain ~ $3 OR coalesce(lower(split_part(CASE WHEN r.kind = 'email' THEN r.value END, '@', 2)), '') ~ $3) AS reserved,
		   l.probe_id IS NOT NULL AS has_probe, s.list_rank,
		   CASE WHEN pr.action = 'defer' AND pr.until >= coalesce($4::date, current_date) THEN pr.until::timestamptz END AS defer_until,
		   `+suppressedSQL("$1", "o.domain", "CASE WHEN r.kind = 'email' THEN r.value ELSE '' END")+` AS suppressed,
		   EXISTS (
		     SELECT 1 FROM lead l2 JOIN org o2 ON o2.id = l2.org_id
		     LEFT JOIN contact_route r2 ON r2.id = l2.route_id AND r2.kind = 'email'
		     WHERE l2.id <> l.id AND l2.state IN ('batched', 'contacted', 'replied', 'bounced', 'opted_out')
		       AND l2.campaign_id IN (SELECT id FROM campaign WHERE account_id = $1)
		       AND (o2.domain IN (o.domain, nullif(lower(split_part(CASE WHEN r.kind = 'email' THEN r.value END, '@', 2)), ''))
		         OR nullif(lower(split_part(r2.value, '@', 2)), '') IN (o.domain, nullif(lower(split_part(CASE WHEN r.kind = 'email' THEN r.value END, '@', 2)), '')))
		   ) AS reached,
		   rs.status AS research_status, coalesce(rs.finished_at, rs.requested_at) AS research_at
		 FROM lead l
		 JOIN LATERAL (
		   SELECT x.action, x.position FROM lead_star x WHERE x.lead_id = l.id ORDER BY x.id DESC LIMIT 1
		 ) st ON st.action <> 'unstar'
		 JOIN campaign cg ON cg.id = l.campaign_id AND cg.account_id = $1
		 JOIN org o ON o.id = l.org_id
		 LEFT JOIN lead_kind lk ON lk.name = l.kind
		 LEFT JOIN contact_route r ON r.id = l.route_id
		 LEFT JOIN site_probe p ON p.id = l.probe_id
		 LEFT JOIN site s ON s.id = coalesce(p.site_id, o.site_id)
		 LEFT JOIN LATERAL (
		   SELECT lp.action, lp.until FROM lead_priority lp WHERE lp.lead_id = l.id ORDER BY lp.id DESC LIMIT 1
		 ) pr ON true
		 LEFT JOIN LATERAL (
		   SELECT cr.status, cr.finished_at, cr.requested_at FROM contact_research cr
		   WHERE cr.org_id = l.org_id ORDER BY cr.id DESC LIMIT 1
		 ) rs ON true
		 WHERE ($2::bigint = 0 OR l.campaign_id = $2::bigint)
		 ORDER BY st.position, l.id`,
		orAccount(o.AccountID), o.CampaignID, reservedDomainRE(), today)
	if err != nil {
		return nil, fmt.Errorf("leadstore: starred status: %w", err)
	}
	out := make([]StarredState, 0, len(facts))
	for _, f := range facts {
		s := StarredState{LeadID: f.LeadID, OrgID: f.OrgID, Position: f.Position, Domain: f.Domain,
			Country: f.Country, Kind: f.Kind, Channel: f.Channel, State: f.State}
		s.Reason, s.Detail, s.Date = starredReason(f, offered, o, profile)
		out = append(out, s)
	}
	return out, nil
}

// starredReason names starred lead f's reason (StarredStatus's order).
func starredReason(f starredFacts, offered map[int64]int, o StarredStatusOptions, profile leadconfig.Profile) (string, string, *time.Time) {
	if i, ok := offered[f.LeadID]; ok {
		if o.Limit > 0 && i >= o.Limit {
			return StarredRankedOut, fmt.Sprintf("the batch takes %d and this lead comes after them", o.Limit), nil
		}
		return StarredReady, "", nil
	}
	policy, judged := "", o.Countries != nil
	if judged {
		policy = o.Countries[f.Country]
		if policy == "" {
			policy = leadconfig.ChannelNone
		}
	}
	country := f.Country
	if country == "" {
		country = "unknown country"
	}
	switch {
	case f.Suppressed || f.State == LeadSuppressed || f.State == LeadOptedOut:
		return StarredSuppressed, "", nil
	case f.State != LeadNew:
		return StarredNotNew, "it is " + f.State, nil
	case f.Reached:
		return StarredSuppressed, "another lead on its domain was already reached", nil
	case f.DeferUntil != nil:
		return StarredDeferred, "", f.DeferUntil
	case slices.Contains(profile.ExcludeCountries, f.Country):
		return StarredCountryRule, country + " is excluded by the priority profile", nil
	case o.CandidateOptions.Countries != nil && !slices.Contains(o.CandidateOptions.Countries, strings.ToUpper(f.Country)):
		return StarredCountryRule, CampaignCountryWords, nil
	case judged && policy == leadconfig.ChannelNone:
		return StarredCountryRule, country + " is off in leads.yaml", nil
	case !f.Outreach:
		return StarredOther, "batches do not draft kind " + f.Kind, nil
	case slices.Contains(o.KindsOff, f.Kind):
		return StarredOther, "kind " + f.Kind + " is turned off", nil
	case f.Selftest:
		return StarredOther, "it is a selftest org", nil
	case f.ProbeStale:
		return StarredOther, "its site was probed again: it waits for the next leads build", nil
	case f.Reserved && !o.IncludeReserved:
		return StarredOther, "it is on a reserved domain", nil
	case f.HasProbe && o.SkipRankOver > 0 && (f.ListRank == nil || *f.ListRank > o.SkipRankOver):
		return StarredRankedOut, fmt.Sprintf("skip_rank_over is %d", o.SkipRankOver), nil
	case f.Channel != ChannelNone:
		return StarredOther, "the batch does not offer it", nil // a predicate this list does not name
	}
	switch {
	case f.ResearchStatus != nil && (*f.ResearchStatus == ResearchQueued || *f.ResearchStatus == ResearchLooking):
		return StarredLooking, "", nil
	case f.ResearchStatus != nil && *f.ResearchStatus == ResearchNothing:
		return StarredFoundNothing, "", f.ResearchAt
	case judged && policy == leadconfig.ChannelForm:
		return StarredCountryRule, country + " takes contact forms only, and none was found", nil
	case f.ResearchStatus != nil && *f.ResearchStatus == ResearchFound:
		return StarredNoEmailYet, "research found a contact: the next leads build gives it its channel", nil
	case f.ResearchStatus != nil:
		return StarredNoEmailYet, "the last research ended " + *f.ResearchStatus + ": it may be asked again", nil
	}
	return StarredNoEmailYet, "", nil
}
