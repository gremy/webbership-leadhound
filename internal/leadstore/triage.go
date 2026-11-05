package leadstore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// The Leads list's triage: an operator's skip, the undo of that skip, and his Good. A skip of
// a café lead with no current call also writes the call "no", and Good writes "likely", so the
// triage fills the answer key of the café relevance eval. A current call is a lead's latest
// lead_label row when that row names a band; a latest row that was cleared is no call.
//
// Each operation is one statement, so the state, the event and the label commit together on
// whichever database handle runs it (store.DBTX has no Begin). A selftest lead is treated as
// not existing, as SetLeadLabel does.

// The reasons of the lead_events the triage writes, which the lead page shows as they are.
const (
	skippedFromListReason    = "skipped from the list"
	skipUndoneFromListReason = "skip undone from the list"
)

// triageLeadSQL is the FROM and WHERE of lead $1, as l, when it exists and is not a selftest.
const triageLeadSQL = `lead l JOIN org o ON o.id = l.org_id WHERE l.id = $1 AND l.kind <> 'selftest' AND o.kind <> 'selftest'`

// currentBandSQL is the band of the current call of the lead whose id is leadCol: NULL when
// the lead has no call or its latest row is cleared.
func currentBandSQL(leadCol string) string {
	return `(SELECT x.band FROM lead_label x WHERE x.lead_id = ` + leadCol + ` ORDER BY x.id DESC LIMIT 1)`
}

// ErrSystemSkip is returned by TriageUndoSkip for a lead the system skipped, such as a
// duplicate or a lead whose facts no longer make one: only an operator's own skip can be undone.
var ErrSystemSkip = errors.New("leadstore: the system skipped this lead, so there is no skip of yours to undo")

// TriageSkipResult is what TriageSkip did.
type TriageSkipResult struct {
	Moved       bool   // the lead went from new to skipped
	CallWritten bool   // the skip wrote the call "no"
	State       string // the lead's state afterwards, which says why it did not move
}

// TriageUndoResult is what TriageUndoSkip did.
type TriageUndoResult struct {
	Moved       bool   // the lead went from skipped back to new
	CallCleared bool   // the call the skip wrote was still the latest, and is now cleared
	State       string // the lead's state afterwards
}

// TriageGoodResult is what TriageGood did.
type TriageGoodResult struct {
	Band    string // the lead's current call afterwards: likely, or the strong that stayed
	Written bool   // a new call row was written
}

// triageLabelDoc is input as the text of a call's jsonb document, checked as SetLeadLabel
// checks it: a nil input is no document, and anything but a JSON object is ErrBadLabelInput.
func triageLabelDoc(input []byte) (*string, error) {
	if input == nil {
		return nil, nil
	}
	if !json.Valid(input) || !bytes.HasPrefix(bytes.TrimSpace(input), []byte("{")) {
		return nil, ErrBadLabelInput
	}
	s := string(input)
	return &s, nil
}

// TriageSkip moves lead leadID from new to skipped by the operator. For a café lead with no
// current call and a non-nil input (its leadrate.Input as a JSON object) it also appends the
// call "no" by the operator and stores that call's id in the skip's lead_event; an existing
// call stays as it is, and any other lead is skipped with no call written. A lead past new
// does not move and nothing is written: Moved is false and State says where it stands. A
// missing lead is ErrLeadNotFound; an input that is not a JSON object is ErrBadLabelInput.
func (q Queries) TriageSkip(ctx context.Context, leadID int64, input []byte) (TriageSkipResult, error) {
	doc, err := triageLabelDoc(input)
	if err != nil {
		return TriageSkipResult{}, fmt.Errorf("leadstore: triage skip: %w", err)
	}
	var r TriageSkipResult
	err = q.db.QueryRow(ctx,
		`WITH target AS (
		   SELECT l.id, l.state FROM `+triageLeadSQL+` FOR UPDATE OF l
		 ), u AS (
		   UPDATE lead SET state = 'skipped', state_at = now()
		   WHERE id = (SELECT id FROM target) AND state = 'new'
		   RETURNING id, kind
		 ), lab AS (
		   INSERT INTO lead_label (lead_id, band, input, actor)
		   SELECT u.id, 'no', $2::jsonb, 'operator' FROM u
		   WHERE $2::jsonb IS NOT NULL AND u.kind IN (`+cafeKindsSQL()+`)
		     AND `+currentBandSQL("u.id")+` IS NULL
		   RETURNING id
		 ), ev AS (
		   INSERT INTO lead_event (lead_id, state, reason, actor, skip_label_id)
		   SELECT u.id, 'skipped', $3::text, 'gremy', (SELECT id FROM lab) FROM u
		 )
		 SELECT EXISTS (SELECT 1 FROM u), EXISTS (SELECT 1 FROM lab),
		        CASE WHEN EXISTS (SELECT 1 FROM u) THEN 'skipped' ELSE t.state END
		 FROM target t`, leadID, doc, skippedFromListReason).Scan(&r.Moved, &r.CallWritten, &r.State)
	if errors.Is(err, pgx.ErrNoRows) {
		return TriageSkipResult{}, fmt.Errorf("leadstore: triage skip %d: %w", leadID, ErrLeadNotFound)
	}
	if err != nil {
		return TriageSkipResult{}, fmt.Errorf("leadstore: triage skip: %w", err)
	}
	return r, nil
}

// TriageUndoSkip moves lead leadID from skipped back to new by the operator, when the lead's
// latest lead_event is the operator's. It appends a cleared call only when the call the skip
// wrote is still the lead's latest, so a call made since the skip, or one that was there before
// it, stays. A skip by the system is ErrSystemSkip and changes nothing. A lead that is not
// skipped does not move: Moved is false and State says where it stands. A missing lead is
// ErrLeadNotFound.
func (q Queries) TriageUndoSkip(ctx context.Context, leadID int64) (TriageUndoResult, error) {
	var r TriageUndoResult
	err := q.db.QueryRow(ctx,
		`WITH target AS (
		   SELECT l.id, l.state FROM `+triageLeadSQL+` FOR UPDATE OF l
		 ), last_event AS (
		   SELECT e.actor, e.skip_label_id FROM lead_event e WHERE e.lead_id = $1 ORDER BY e.id DESC LIMIT 1
		 ), u AS (
		   UPDATE lead SET state = 'new', state_at = now()
		   WHERE id = (SELECT id FROM target) AND state = 'skipped'
		     AND EXISTS (SELECT 1 FROM last_event WHERE actor = 'gremy')
		   RETURNING id
		 ), lab AS (
		   INSERT INTO lead_label (lead_id, band, input, actor)
		   SELECT u.id, NULL::text, NULL::jsonb, 'operator' FROM u
		   WHERE (SELECT skip_label_id FROM last_event) =
		         (SELECT x.id FROM lead_label x WHERE x.lead_id = u.id ORDER BY x.id DESC LIMIT 1)
		   RETURNING id
		 ), ev AS (
		   INSERT INTO lead_event (lead_id, state, reason, actor)
		   SELECT u.id, 'new', $2::text, 'gremy' FROM u
		 )
		 SELECT EXISTS (SELECT 1 FROM u), EXISTS (SELECT 1 FROM lab),
		        CASE WHEN EXISTS (SELECT 1 FROM u) THEN 'new' ELSE t.state END
		 FROM target t`, leadID, skipUndoneFromListReason).Scan(&r.Moved, &r.CallCleared, &r.State)
	if errors.Is(err, pgx.ErrNoRows) {
		return TriageUndoResult{}, fmt.Errorf("leadstore: triage undo skip %d: %w", leadID, ErrLeadNotFound)
	}
	if err != nil {
		return TriageUndoResult{}, fmt.Errorf("leadstore: triage undo skip: %w", err)
	}
	if !r.Moved && r.State == LeadSkipped {
		return r, fmt.Errorf("leadstore: triage undo skip %d: %w", leadID, ErrSystemSkip)
	}
	return r, nil
}

// TriageGood makes the operator's current call on café lead leadID "likely", in any state and
// without moving the lead, with input its leadrate.Input as a JSON object. A current "strong"
// stays and a current "likely" writes no new row. A lead that is not a café lead is
// ErrNotCafeLead, a missing lead ErrLeadNotFound, and a nil input or one that is not a JSON
// object ErrBadLabelInput.
func (q Queries) TriageGood(ctx context.Context, leadID int64, input []byte) (TriageGoodResult, error) {
	doc, err := triageLabelDoc(input)
	if err != nil {
		return TriageGoodResult{}, fmt.Errorf("leadstore: triage good: %w", err)
	}
	var (
		isCafe bool
		r      TriageGoodResult
		band   *string
	)
	err = q.db.QueryRow(ctx,
		`WITH target AS (
		   SELECT l.id, l.kind IN (`+cafeKindsSQL()+`) AS is_cafe FROM `+triageLeadSQL+`
		 ), cur AS (
		   SELECT `+currentBandSQL("$1")+` AS band
		 ), lab AS (
		   INSERT INTO lead_label (lead_id, band, input, actor)
		   SELECT t.id, 'likely', $2::jsonb, 'operator' FROM target t, cur
		   WHERE t.is_cafe AND $2::jsonb IS NOT NULL AND cur.band IS DISTINCT FROM 'likely' AND cur.band IS DISTINCT FROM 'strong'
		   RETURNING id
		 )
		 SELECT t.is_cafe, EXISTS (SELECT 1 FROM lab), cur.band FROM target t, cur`, leadID, doc).Scan(&isCafe, &r.Written, &band)
	if errors.Is(err, pgx.ErrNoRows) {
		return TriageGoodResult{}, fmt.Errorf("leadstore: triage good %d: %w", leadID, ErrLeadNotFound)
	}
	if err != nil {
		return TriageGoodResult{}, fmt.Errorf("leadstore: triage good: %w", err)
	}
	if !isCafe {
		return TriageGoodResult{}, fmt.Errorf("leadstore: triage good %d: %w", leadID, ErrNotCafeLead)
	}
	if doc == nil {
		return TriageGoodResult{}, fmt.Errorf("leadstore: triage good: %w", ErrBadLabelInput)
	}
	r.Band = BandLikely
	if band != nil && *band == BandStrong {
		r.Band = BandStrong
	}
	return r, nil
}

// CampaignProgress counts campaign campaignID's non-selftest leads for the list's "reviewed"
// line. total is the leads in state new, skipped by the operator, or having a current call;
// reviewed is those with a current call or skipped by the operator. A lead the system skipped,
// such as a duplicate, counts in neither unless it has a call. The latest event of each
// skipped lead is read in one pass (DISTINCT ON over the campaign's skipped leads), because
// lead_event has no index on lead_id and a read per lead would scan the table once for each.
func (q Queries) CampaignProgress(ctx context.Context, campaignID int64) (reviewed, total int, err error) {
	err = q.db.QueryRow(ctx,
		`SELECT count(*) FILTER (WHERE s.has_call OR s.skipped_by_gremy)::int,
		        count(*) FILTER (WHERE s.state = 'new' OR s.has_call OR s.skipped_by_gremy)::int
		 FROM (
		   SELECT l.state,
		          `+currentBandSQL("l.id")+` IS NOT NULL AS has_call,
		          l.state = 'skipped' AND ev.actor IS NOT DISTINCT FROM 'gremy' AS skipped_by_gremy
		   FROM lead l JOIN org o ON o.id = l.org_id
		   LEFT JOIN (
		     SELECT DISTINCT ON (e.lead_id) e.lead_id, e.actor
		     FROM lead_event e JOIN lead x ON x.id = e.lead_id
		     WHERE x.campaign_id = $1 AND x.state = 'skipped'
		     ORDER BY e.lead_id, e.id DESC
		   ) ev ON ev.lead_id = l.id
		   WHERE l.campaign_id = $1 AND l.kind <> 'selftest' AND o.kind <> 'selftest'
		 ) s`, campaignID).Scan(&reviewed, &total)
	if err != nil {
		return 0, 0, fmt.Errorf("leadstore: campaign progress: %w", err)
	}
	return reviewed, total, nil
}
