package leadstore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// SendPaused reports the operator's pause switch: the latest send_pause row (by id), with
// its reason and when it was set. No row means not paused, with a zero time.
func (q Queries) SendPaused(ctx context.Context) (paused bool, reason string, at time.Time, err error) {
	err = q.db.QueryRow(ctx,
		`SELECT paused, reason, at FROM send_pause ORDER BY id DESC LIMIT 1`).Scan(&paused, &reason, &at)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return false, "", time.Time{}, nil
	case err != nil:
		return false, "", time.Time{}, fmt.Errorf("leadstore: send paused: %w", err)
	}
	return paused, reason, at, nil
}

// SetSendPause appends a pause (paused true) or a resume row by the operator. The table is
// append-only, so the history of the switch stays.
func (q Queries) SetSendPause(ctx context.Context, paused bool, reason string) error {
	if _, err := q.db.Exec(ctx,
		`INSERT INTO send_pause (paused, reason, actor) VALUES ($1, $2, $3)`, paused, reason, ActorGremy); err != nil {
		return fmt.Errorf("leadstore: set send pause: %w", err)
	}
	return nil
}

// FirstLiveSend is when the first live message the database records as sent went out, or
// nil when none has: the day 0 of a ramp that starts at the first send. Shadow batches and
// selftests never count; nor do maybe_sent rows, until the operator resolves them as sent.
func (q Queries) FirstLiveSend(ctx context.Context) (*time.Time, error) {
	var at *time.Time
	if err := q.db.QueryRow(ctx,
		`SELECT min(m.sent_at) FROM outreach_message m
		 JOIN outreach_batch b ON b.id = m.batch_id JOIN lead l ON l.id = m.lead_id
		 WHERE NOT b.shadow AND l.kind <> 'selftest' AND m.state = 'sent'`).Scan(&at); err != nil {
		return nil, fmt.Errorf("leadstore: first live send: %w", err)
	}
	return at, nil
}
