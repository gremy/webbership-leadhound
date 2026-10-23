package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// ErrEmptyBoard is returned when a board-scoped update is asked for with no board name: an
// empty prefix would reach every job of the source.
var ErrEmptyBoard = errors.New("store: empty board name")

// ExpireMissing marks expired every open job of source on one board whose id is not in
// seenIDs, and stamps status_checked_at. A board's jobs are those whose source_job_id is
// "<boardPrefix>/<id>": the prefix is compared whole, so board "acme" never reaches
// "acme-labs/…", and it is a plain comparison, so no character in a board name is a
// pattern. seenIDs are full source_job_ids, as the successful fetch of that board listed
// them; an empty list expires every open job of the board. It returns how many jobs it
// expired. Call it only after a fetch that parsed: a failed fetch says nothing about
// which postings are gone.
func (q Queries) ExpireMissing(ctx context.Context, source, boardPrefix string, seenIDs []string) (int64, error) {
	if strings.TrimSpace(boardPrefix) == "" {
		return 0, fmt.Errorf("store: expire missing %s: %w", source, ErrEmptyBoard)
	}
	if seenIDs == nil {
		seenIDs = []string{} // NULL would make NOT (… = ANY(NULL)) unknown and expire nothing
	}
	tag, err := q.db.Exec(ctx,
		`UPDATE job SET listing_status = 'expired', status_checked_at = now()
		 WHERE source = $1 AND listing_status = 'open'
		   AND left(source_job_id, length($2) + 1) = $2 || '/'
		   AND NOT (source_job_id = ANY($3::text[]))`, source, boardPrefix, seenIDs)
	if err != nil {
		return 0, fmt.Errorf("store: expire missing %s/%s: %w", source, boardPrefix, err)
	}
	return tag.RowsAffected(), nil
}

// Reopen marks open again every expired job of source whose source_job_id is in ids (a
// posting listed again after it had gone), and stamps status_checked_at. Jobs that are open
// or unreachable are left as they are. It returns how many jobs it reopened.
func (q Queries) Reopen(ctx context.Context, source string, ids []string) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	tag, err := q.db.Exec(ctx,
		`UPDATE job SET listing_status = 'open', status_checked_at = now()
		 WHERE source = $1 AND listing_status = 'expired' AND source_job_id = ANY($2::text[])`, source, ids)
	if err != nil {
		return 0, fmt.Errorf("store: reopen %s: %w", source, err)
	}
	return tag.RowsAffected(), nil
}

// MarkChecked stamps status_checked_at on every job of source whose source_job_id is in
// ids: a board that answered and still lists them has just confirmed they are there. The
// listing status is left as it is. It returns how many jobs it stamped.
func (q Queries) MarkChecked(ctx context.Context, source string, ids []string) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	tag, err := q.db.Exec(ctx,
		`UPDATE job SET status_checked_at = now() WHERE source = $1 AND source_job_id = ANY($2::text[])`, source, ids)
	if err != nil {
		return 0, fmt.Errorf("store: mark checked %s: %w", source, err)
	}
	return tag.RowsAffected(), nil
}
