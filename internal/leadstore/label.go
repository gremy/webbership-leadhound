package leadstore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
)

// The operator's own call on a lead (lead_label, migration 00015): the band he would give it,
// the answer key the relevance eval grades the decider against. Every call is a row and the
// latest per lead wins; a NULL band is a cleared call.

// labelBands are the bands a call may name (lead_label_band_check), best first.
var labelBands = []string{BandStrong, BandLikely, BandMaybe, BandUnlikely, BandNo}

var (
	// ErrBadLabel is returned by SetLeadLabel for a band that is not one of the five.
	ErrBadLabel = errors.New("leadstore: not a band")
	// ErrBadLabelInput is returned by SetLeadLabel for a call without a JSON object as its
	// input, or a cleared call with one.
	ErrBadLabelInput = errors.New("leadstore: a call needs its input as a JSON object, a cleared call none")
)

// LeadLabel is one stored call on a lead.
type LeadLabel struct {
	ID        int64     `db:"id"`
	LeadID    int64     `db:"lead_id"`
	Domain    string    `db:"domain"` // the lead's org's domain
	Band      *string   `db:"band"`   // nil: the call was cleared
	Input     []byte    `db:"input"`  // leadrate.Input as JSON at the time of the call; nil when cleared
	Actor     string    `db:"actor"`
	CreatedAt time.Time `db:"created_at"`
}

// SetLeadLabel appends a call on lead leadID by actor (operator or system) and returns its id:
// band one of the Band* values with input the lead's leadrate.Input as a JSON object, or a nil
// band (a cleared call) with a nil input. A lead that does not exist, or a selftest one, is
// ErrLeadNotFound.
func (q Queries) SetLeadLabel(ctx context.Context, leadID int64, band *string, input []byte, actor string) (int64, error) {
	if actor != ActorOperator && actor != ActorSystem {
		return 0, fmt.Errorf("leadstore: set lead label: %w: %q", ErrUnknownActor, actor)
	}
	var doc *string
	switch {
	case band == nil && input == nil:
	case band == nil:
		return 0, fmt.Errorf("leadstore: set lead label: %w", ErrBadLabelInput)
	case !slices.Contains(labelBands, *band):
		return 0, fmt.Errorf("leadstore: set lead label: %w: %q", ErrBadLabel, *band)
	case !json.Valid(input) || !bytes.HasPrefix(bytes.TrimSpace(input), []byte("{")):
		return 0, fmt.Errorf("leadstore: set lead label: %w", ErrBadLabelInput)
	default:
		s := string(input)
		doc = &s
	}
	var id int64
	err := q.db.QueryRow(ctx,
		`INSERT INTO lead_label (lead_id, band, input, actor)
		 SELECT l.id, $2, $3::jsonb, $4 FROM lead l JOIN org o ON o.id = l.org_id
		 WHERE l.id = $1 AND l.kind <> 'selftest' AND o.kind <> 'selftest'
		 RETURNING id`, leadID, band, doc, actor).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("leadstore: set lead label %d: %w", leadID, ErrLeadNotFound)
	}
	if err != nil {
		return 0, fmt.Errorf("leadstore: set lead label: %w", err)
	}
	return id, nil
}

// latestLabelSQL is the latest call of every lead that has one, cleared calls included, with
// the lead's domain.
const latestLabelSQL = `SELECT DISTINCT ON (x.lead_id) x.id, x.lead_id, o.domain, x.band, x.input, x.actor, x.created_at
	FROM lead_label x JOIN lead l ON l.id = x.lead_id JOIN org o ON o.id = l.org_id`

// LatestLabels returns the latest call on each of the leads leadIDs, by lead id, a cleared
// call included (its Band nil); a lead never called has no entry.
func (q Queries) LatestLabels(ctx context.Context, leadIDs []int64) (map[int64]LeadLabel, error) {
	out := map[int64]LeadLabel{}
	if len(leadIDs) == 0 {
		return out, nil
	}
	rows, err := collect(ctx, q.db, pgx.RowToStructByName[LeadLabel],
		`SELECT y.id, y.lead_id, o.domain, y.band, y.input, y.actor, y.created_at
		 FROM unnest($1::bigint[]) AS l(id)
		 CROSS JOIN LATERAL (
		   SELECT * FROM lead_label x WHERE x.lead_id = l.id ORDER BY x.id DESC LIMIT 1
		 ) y
		 JOIN lead ld ON ld.id = y.lead_id JOIN org o ON o.id = ld.org_id`, leadIDs)
	if err != nil {
		return nil, fmt.Errorf("leadstore: latest labels: %w", err)
	}
	for _, r := range rows {
		out[r.LeadID] = r
	}
	return out, nil
}

// LabelledLeads returns every lead whose latest call names a band, by lead id: the answer
// key of the relevance eval, each with the input the call was made on. A lead whose latest
// call was cleared is not in it, whatever its earlier calls said.
func (q Queries) LabelledLeads(ctx context.Context) ([]LeadLabel, error) {
	rows, err := collect(ctx, q.db, pgx.RowToStructByName[LeadLabel],
		`SELECT * FROM (`+latestLabelSQL+` ORDER BY x.lead_id, x.id DESC) m WHERE m.band IS NOT NULL ORDER BY m.lead_id`)
	if err != nil {
		return nil, fmt.Errorf("leadstore: labelled leads: %w", err)
	}
	return rows, nil
}

// LabelledLeadIDs returns the ids of the leads whose latest call names a band, ascending.
func (q Queries) LabelledLeadIDs(ctx context.Context) ([]int64, error) {
	rows, err := q.db.Query(ctx,
		`SELECT m.lead_id FROM (
		   SELECT DISTINCT ON (lead_id) lead_id, band FROM lead_label ORDER BY lead_id, id DESC
		 ) m WHERE m.band IS NOT NULL ORDER BY m.lead_id`)
	if err != nil {
		return nil, fmt.Errorf("leadstore: labelled lead ids: %w", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return nil, fmt.Errorf("leadstore: labelled lead ids: %w", err)
	}
	return ids, nil
}

// LabelCount is how many leads have a call: their latest call names a band.
func (q Queries) LabelCount(ctx context.Context) (int, error) {
	var n int
	if err := q.db.QueryRow(ctx,
		`SELECT count(*)::int FROM (
		   SELECT DISTINCT ON (lead_id) band FROM lead_label ORDER BY lead_id, id DESC
		 ) m WHERE m.band IS NOT NULL`).Scan(&n); err != nil {
		return 0, fmt.Errorf("leadstore: label count: %w", err)
	}
	return n, nil
}
