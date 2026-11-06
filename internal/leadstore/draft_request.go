package leadstore

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Draft it (migration 00029): the console writes a draft_request row for one lead; the
// drafting service claims queued rows, makes the batch and finishes them. The store never
// calls Claude. It has the shape of contact research (research.go).

// A draft request's status (draft_request_status_check).
const (
	DraftRequestQueued   = "queued"
	DraftRequestDrafting = "drafting"
	DraftRequestDone     = "done"
	DraftRequestFailed   = "failed"
)

// DraftRequestStaleAfter is how long a row may stay drafting before it is taken for the
// remains of a run that crashed or was killed, and failed (reapStaleDraftRequests). The
// service's unit stops a run after 15 minutes, so a run still going is never reaped.
const DraftRequestStaleAfter = 30 * time.Minute

// DraftRequestStopped is the reason a stale row is failed with.
const DraftRequestStopped = "stopped before it finished"

// DraftRequestReasonMax is the most a failed row's reason holds (draft_request_reason_check).
const DraftRequestReasonMax = 200

var (
	// ErrDraftRequestOpen is returned by RequestDraft for a lead with a queued or drafting
	// row; the error is a *DraftRequestOpenError.
	ErrDraftRequestOpen = errors.New("leadstore: a draft is already asked for")
	// ErrBadDraftRequest is returned for an unknown channel or status, or a finish whose
	// batch and reason do not fit its status.
	ErrBadDraftRequest = errors.New("leadstore: bad draft request")
	// ErrDraftRequestNotDrafting is returned by FinishDraftRequest for a row that does not
	// exist or is not drafting (never claimed, already finished, or failed as stale).
	ErrDraftRequestNotDrafting = errors.New("leadstore: draft request is not drafting")
)

// DraftRequestOpenError is ErrDraftRequestOpen with the row that is open and when it was
// asked for.
type DraftRequestOpenError struct {
	ID          int64
	Status      string
	RequestedAt time.Time
}

func (e *DraftRequestOpenError) Error() string {
	return fmt.Sprintf("%s: request %d is %s, asked at %s",
		ErrDraftRequestOpen, e.ID, e.Status, e.RequestedAt.Format(time.DateTime))
}

// Is makes errors.Is(err, ErrDraftRequestOpen) true.
func (e *DraftRequestOpenError) Is(target error) bool { return target == ErrDraftRequestOpen }

// DraftRequest is one draft_request row.
type DraftRequest struct {
	ID          int64      `db:"id"`
	LeadID      int64      `db:"lead_id"`
	Channel     string     `db:"channel"` // "": the lead's own channel
	Status      string     `db:"status"`
	Reason      string     `db:"reason"`   // why it failed; "" otherwise
	BatchID     *int64     `db:"batch_id"` // the batch it made
	RequestedAt time.Time  `db:"requested_at"`
	StartedAt   *time.Time `db:"started_at"`
	FinishedAt  *time.Time `db:"finished_at"`
}

// draftRequestCols are DraftRequest's columns.
const draftRequestCols = `id, lead_id, coalesce(channel, '') AS channel, status, coalesce(reason, '') AS reason,
	batch_id, requested_at, started_at, finished_at`

// RequestDraft queues a draft for lead leadID on channel (email, form, linkedin or instagram; "": the
// lead's own channel) and returns its id. A lead with a queued or drafting row gets no second
// one: a *DraftRequestOpenError says which and when it was asked for. A row of this lead
// drafting longer than DraftRequestStaleAfter is first failed, because a refused request
// wakes no service, so nothing else would free the lead. A lead that does not exist is
// ErrLeadNotFound. Whether the lead may be drafted at all is the service's to judge when it
// runs the row.
func (q Queries) RequestDraft(ctx context.Context, leadID int64, channel string) (int64, error) {
	wrap := func(err error) error { return fmt.Errorf("leadstore: request draft lead %d: %w", leadID, err) }
	if channel != "" && channel != ChannelEmail && !manualChannel(channel) {
		return 0, wrap(fmt.Errorf("%w: channel %q", ErrBadDraftRequest, channel))
	}
	var exists bool
	if err := q.db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM lead WHERE id = $1)`, leadID).Scan(&exists); err != nil {
		return 0, wrap(err)
	}
	if !exists {
		return 0, wrap(ErrLeadNotFound)
	}
	if err := q.reapStaleDraftRequests(ctx, leadID); err != nil {
		return 0, wrap(err)
	}
	// The open-row index settles a race with another request: the loser reads the winner's.
	// That row may have finished by then, so a loser who finds none tries once more.
	for range 2 {
		var id int64
		err := q.db.QueryRow(ctx,
			`INSERT INTO draft_request (lead_id, channel) VALUES ($1, nullif($2, ''))
			 ON CONFLICT (lead_id) WHERE status IN ('queued', 'drafting') DO NOTHING RETURNING id`,
			leadID, channel).Scan(&id)
		if err == nil {
			return id, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return 0, wrap(err)
		}
		open := DraftRequestOpenError{}
		err = q.db.QueryRow(ctx,
			`SELECT id, status, requested_at FROM draft_request
			 WHERE lead_id = $1 AND status IN ('queued', 'drafting')`, leadID).Scan(&open.ID, &open.Status, &open.RequestedAt)
		if err == nil {
			return 0, wrap(&open)
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return 0, wrap(err)
		}
	}
	return 0, wrap(errors.New("the open request kept changing"))
}

// reapStaleDraftRequests fails every row that has been drafting longer than
// DraftRequestStaleAfter (leadID 0: every lead's), the remains of a run that crashed or was
// killed before FinishDraftRequest. It is failed, never put back in the queue: the dead run
// may already have been billed, so the operator decides whether to ask again. Failing it
// frees draft_request_open_key. started_at is already set, as draft_request_times_check
// wants of a finished row, and finished_at is now, after it.
func (q Queries) reapStaleDraftRequests(ctx context.Context, leadID int64) error {
	if _, err := q.db.Exec(ctx,
		`UPDATE draft_request SET status = 'failed', reason = $1, finished_at = greatest(now(), started_at)
		 WHERE status = 'drafting' AND started_at < now() - make_interval(secs => $2)
		   AND ($3::bigint = 0 OR lead_id = $3::bigint)`,
		DraftRequestStopped, DraftRequestStaleAfter.Seconds(), leadID); err != nil {
		return fmt.Errorf("reap stale draft requests: %w", err)
	}
	return nil
}

// ClaimDraftRequests takes at most limit queued rows (limit <= 0: one) for the drafting
// service and marks them drafting (started_at now), oldest first. Two services never take one
// row (SKIP LOCKED). First, every row drafting longer than DraftRequestStaleAfter is failed
// (reapStaleDraftRequests), so a crashed run's row never holds its lead's open slot for good.
func (q Queries) ClaimDraftRequests(ctx context.Context, limit int) ([]DraftRequest, error) {
	if limit <= 0 {
		limit = 1
	}
	if err := q.reapStaleDraftRequests(ctx, 0); err != nil {
		return nil, fmt.Errorf("leadstore: claim draft requests: %w", err)
	}
	rows, err := collect(ctx, q.db, pgx.RowToStructByName[DraftRequest],
		`WITH pick AS (
		   SELECT id FROM draft_request WHERE status = 'queued'
		   ORDER BY requested_at, id
		   LIMIT $1
		   FOR UPDATE SKIP LOCKED
		 ), took AS (
		   UPDATE draft_request d SET status = 'drafting', started_at = now()
		   FROM pick WHERE d.id = pick.id
		   RETURNING d.*
		 )
		 SELECT `+draftRequestCols+` FROM took ORDER BY requested_at, id`, limit)
	if err != nil {
		return nil, fmt.Errorf("leadstore: claim draft requests: %w", err)
	}
	return rows, nil
}

// FinishDraftRequest ends drafting row id: done names the batch it made and no reason; failed
// names a reason and no batch. A reason longer than DraftRequestReasonMax is cut to fit,
// because a refused reason would leave the row drafting and the lead locked until it went
// stale. A row that is not drafting is ErrDraftRequestNotDrafting.
func (q Queries) FinishDraftRequest(ctx context.Context, id int64, status, reason string, batchID *int64) error {
	wrap := func(err error) error { return fmt.Errorf("leadstore: finish draft request %d: %w", id, err) }
	reason = strings.TrimSpace(reason)
	switch status {
	case DraftRequestDone:
		if batchID == nil || reason != "" {
			return wrap(fmt.Errorf("%w: done needs a batch and no reason", ErrBadDraftRequest))
		}
	case DraftRequestFailed:
		if batchID != nil || reason == "" {
			return wrap(fmt.Errorf("%w: failed needs a reason and no batch", ErrBadDraftRequest))
		}
	default:
		return wrap(fmt.Errorf("%w: status %q", ErrBadDraftRequest, status))
	}
	if r := []rune(reason); len(r) > DraftRequestReasonMax {
		reason = string(r[:DraftRequestReasonMax])
	}
	tag, err := q.db.Exec(ctx,
		`UPDATE draft_request SET status = $2, reason = nullif($3, ''), batch_id = $4,
		   finished_at = greatest(now(), started_at)
		 WHERE id = $1 AND status = 'drafting'`, id, status, reason, batchID)
	if err != nil {
		return wrap(err)
	}
	if tag.RowsAffected() == 0 {
		return wrap(ErrDraftRequestNotDrafting)
	}
	return nil
}

// DraftRequestFor returns lead leadID's latest request, for the lead page; nil when it has
// none.
func (q Queries) DraftRequestFor(ctx context.Context, leadID int64) (*DraftRequest, error) {
	rows, err := collect(ctx, q.db, pgx.RowToStructByName[DraftRequest],
		`SELECT `+draftRequestCols+` FROM draft_request WHERE lead_id = $1 ORDER BY id DESC LIMIT 1`, leadID)
	if err != nil {
		return nil, fmt.Errorf("leadstore: draft request for lead %d: %w", leadID, err)
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return &rows[0], nil
}
