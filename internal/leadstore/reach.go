package leadstore

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// A lead's next action (reach): computed, never stored, by one SQL definition that the List's
// filter, its tab counts and the lead page all read through listRowsSQL, so they can never
// disagree.

// The buckets a lead falls in (ListRow.Reach, ListFilter.Reach).
//
//   - contacted: someone was reached: state contacted, replied, bounced or opted_out, or batched
//     with a live message sent, maybe sent or done. A shadow batch's message reaches nobody.
//   - email: new or batched, not contacted, on channel email.
//   - manual: new or batched, not contacted, on channel form or linkedin, or on no channel with
//     a phone that is not retired.
//   - dead: new, on no channel, with no phone (ReachReason says why).
//   - other: every other lead: skipped, suppressed, closed, and a batched lead on no channel
//     with no phone. These show only under Status "all".
const (
	ReachContacted = "contacted"
	ReachEmail     = "email"
	ReachManual    = "manual"
	ReachDead      = "dead"
	ReachOther     = "other"
)

// Why a lead is a dead end (ListRow.ReachReason); "" for every other bucket.
//
// The database gives every reason but ReachCountry from the latest research attempt of the
// lead's org. ReachCountry comes from a config rule the database cannot see, so the caller
// passes that rule in (ListContext.CountryOff, ReachOf) and it wins over the rest, in the order
// StarredStatus judges them. It changes no bucket, so a count never needs it.
const (
	ReachLooking     = "looking"      // a research attempt is queued or looking
	ReachNothing     = "nothing"      // the latest research found nothing
	ReachFound       = "found"        // the latest research found a contact: the next leads build gives it its channel
	ReachNotSearched = "not_searched" // no search has finished: none asked, or the latest was capped or ended in an error
	ReachCountry     = "country"      // the caller's rule says its country is off
)

// reachRouteLiveSQL is true for a contact_route r whose latest mark is not a retire, and
// reachFactLiveSQL for an org_fact x. The latest mark is the highest id (migration 00029).
const (
	reachRouteLiveSQL = `coalesce((SELECT cm.mark FROM contact_mark cm WHERE cm.route_id = r.id ORDER BY cm.id DESC LIMIT 1), '') <> 'retire'`
	reachFactLiveSQL  = `coalesce((SELECT cm.mark FROM contact_mark cm WHERE cm.fact_id = x.id ORDER BY cm.id DESC LIMIT 1), '') <> 'retire'`
)

// reachBucketSQL is a lead l's bucket.
const reachBucketSQL = `CASE
	   WHEN l.state IN ('contacted', 'replied', 'bounced', 'opted_out') THEN 'contacted'
	   WHEN l.state NOT IN ('new', 'batched') THEN 'other'
	   WHEN l.state = 'batched' AND EXISTS (
	     SELECT 1 FROM outreach_message rm JOIN outreach_batch rmb ON rmb.id = rm.batch_id
	     WHERE rm.lead_id = l.id AND rm.state IN ('sent', 'maybe_sent', 'done') AND NOT rmb.shadow
	   ) THEN 'contacted'
	   WHEN l.channel = 'email' THEN 'email'
	   WHEN l.channel IN ('form', 'linkedin') THEN 'manual'
	   WHEN l.channel = 'none' AND EXISTS (
	     SELECT 1 FROM org_fact x WHERE x.org_id = l.org_id AND x.kind = 'phone' AND ` + reachFactLiveSQL + `
	   ) THEN 'manual'
	   WHEN l.state = 'new' AND l.channel = 'none' THEN 'dead'
	   ELSE 'other'
	 END`

// reachReasonSQL is a lead l's reason, over its bucket rc.reach; never ReachCountry.
const reachReasonSQL = `CASE WHEN rc.reach <> 'dead' THEN '' ELSE coalesce((
	     SELECT CASE
	       WHEN cr.status IN ('queued', 'looking') THEN 'looking'
	       WHEN cr.status = 'nothing' THEN 'nothing'
	       WHEN cr.status = 'found' THEN 'found'
	       ELSE 'not_searched'
	     END FROM contact_research cr WHERE cr.org_id = l.org_id ORDER BY cr.id DESC LIMIT 1
	   ), 'not_searched') END`

// reachReasonIn is a lead's reason once the caller's country rule has been weighed; off nil
// leaves the rule unjudged.
func reachReasonIn(bucket, reason, country string, off func(string) bool) string {
	if bucket == ReachDead && off != nil && off(country) {
		return ReachCountry
	}
	return reason
}

// Reach is a lead's next action.
type Reach struct {
	Bucket string // a Reach* bucket
	Reason string // a Reach* reason for a dead end, else ""
}

// ReachOf returns lead leadID's next action. countryOff says whether a country is off (nil: the
// rule is not judged). A selftest lead or a missing one is ErrLeadNotFound.
func (q Queries) ReachOf(ctx context.Context, leadID int64, countryOff func(country string) bool) (Reach, error) {
	var r Reach
	var country string
	err := q.db.QueryRow(ctx, `SELECT m.reach, m.reach_reason, m.country FROM (`+listRowsSQL+`) m WHERE m.lead_id = $3`,
		"{}", 0, leadID).Scan(&r.Bucket, &r.Reason, &country)
	if errors.Is(err, pgx.ErrNoRows) {
		return Reach{}, fmt.Errorf("leadstore: reach of lead %d: %w", leadID, ErrLeadNotFound)
	}
	if err != nil {
		return Reach{}, fmt.Errorf("leadstore: reach of lead %d: %w", leadID, err)
	}
	r.Reason = reachReasonIn(r.Bucket, r.Reason, country, countryOff)
	return r, nil
}

// ReachCounts is how many leads fall in each bucket.
type ReachCounts struct {
	Contacted, Email, Manual, Dead, Other int
}

// All is every lead counted.
func (c ReachCounts) All() int { return c.Contacted + c.Email + c.Manual + c.Dead + c.Other }

// ReachCounts counts the leads f matches by bucket, in one query, so each bucket's number is
// the total LeadList gives with that bucket as f.Reach. f.Reach and the paging are ignored:
// the counts are for every tab. As in LeadList with a Reach, a blank f.Status is every state;
// an explicit one narrows the counts to its states, so ListOpen counts no contacted lead. An
// unknown status, sort or channel is ErrBadListFilter.
func (q Queries) ReachCounts(ctx context.Context, f ListFilter) (ReachCounts, error) {
	var out ReachCounts
	f.Reach = ""
	if f.Status == "" {
		f.Status = ListAll
	}
	args := []any{"{}", 0}
	conds, err := listConds(f, func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	})
	if err != nil {
		return out, fmt.Errorf("leadstore: reach counts: %w", err)
	}
	type row struct {
		Reach string
		N     int
	}
	rows, err := collect(ctx, q.db, pgx.RowToStructByPos[row],
		`SELECT m.reach, count(*)::int FROM (`+listRowsSQL+`) m`+listWhere(conds)+` GROUP BY m.reach`, args...)
	if err != nil {
		return out, fmt.Errorf("leadstore: reach counts: %w", err)
	}
	for _, r := range rows {
		switch r.Reach {
		case ReachContacted:
			out.Contacted = r.N
		case ReachEmail:
			out.Email = r.N
		case ReachManual:
			out.Manual = r.N
		case ReachDead:
			out.Dead = r.N
		default:
			out.Other = r.N
		}
	}
	return out, nil
}
