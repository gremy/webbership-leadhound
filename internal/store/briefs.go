package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ErrBriefSent is returned when the day's brief already has sent_at set: it is never
// sent or rewritten again.
var ErrBriefSent = errors.New("store: the brief for that day was already sent")

// ErrBriefPending is returned by UpsertBrief when the day already has an unsent brief that
// may have been delivered: it is resent as it is, never rewritten.
var ErrBriefPending = errors.New("store: the brief for that day is unsent and may have been delivered")

// Send states of an unsent brief (brief_log.send_state).
const (
	BriefPending   = "pending"    // not tried yet
	BriefMaybeSent = "maybe_sent" // a try began and may have delivered: resend it as it is
	BriefRefused   = "refused"    // its only try was refused: nothing went out
)

// BriefLog is one brief_log row: the day's brief and the job each number in it maps to.
type BriefLog struct {
	ID         int64
	Day        string // YYYY-MM-DD
	SentAt     *time.Time
	ProviderID string
	Subject    string
	BodyText   string
	BodyHTML   string
	JobIDs     []int64 // number n is JobIDs[n-1]
	SendKey    string  // the Idempotency-Key; "" reads as brief-<day> (see Key)
	SendState  string  // BriefPending, BriefMaybeSent or BriefRefused
}

// Key is the Idempotency-Key the stored payload goes out under.
func (b BriefLog) Key() string {
	if b.SendKey != "" {
		return b.SendKey
	}
	return "brief-" + b.Day
}

// BriefDraft is a composed brief about to be written.
type BriefDraft struct {
	Subject, Text, HTML string
	Key                 string  // the Idempotency-Key it is sent under
	JobIDs              []int64 // number n is JobIDs[n-1]
}

const briefColumns = `id, to_char(day, 'YYYY-MM-DD'), sent_at, provider_id, subject, body_text, body_html, job_ids, send_key, send_state`

func scanBrief(row pgx.Row) (BriefLog, error) {
	var b BriefLog
	err := row.Scan(&b.ID, &b.Day, &b.SentAt, &b.ProviderID, &b.Subject, &b.BodyText, &b.BodyHTML, &b.JobIDs, &b.SendKey, &b.SendState)
	return b, err
}

// UpsertBrief writes the day's brief unsent and untried. A day whose brief was refused
// (nothing went out) is rewritten, list and key included; a day already sent is
// ErrBriefSent, and one unsent that may have been delivered is ErrBriefPending, and
// neither changes. day is YYYY-MM-DD.
func (q Queries) UpsertBrief(ctx context.Context, day string, d BriefDraft) (BriefLog, error) {
	ids := d.JobIDs
	if ids == nil {
		ids = []int64{} // NOT NULL: a nil slice would be sent as NULL
	}
	b, err := scanBrief(q.db.QueryRow(ctx,
		`INSERT INTO brief_log (day, subject, body_text, body_html, send_key, job_ids) VALUES ($1::date, $2, $3, $4, $5, $6)
		 ON CONFLICT (day) DO UPDATE SET subject = EXCLUDED.subject, body_text = EXCLUDED.body_text,
		   body_html = EXCLUDED.body_html, send_key = EXCLUDED.send_key, job_ids = EXCLUDED.job_ids,
		   send_state = 'pending'
		 WHERE brief_log.sent_at IS NULL AND brief_log.send_state = 'refused'
		 RETURNING `+briefColumns, day, d.Subject, d.Text, d.HTML, d.Key, ids))
	if errors.Is(err, pgx.ErrNoRows) {
		prev, err := q.BriefByDay(ctx, day)
		switch {
		case err != nil:
			return BriefLog{}, err
		case prev.SentAt != nil:
			return BriefLog{}, fmt.Errorf("store: brief %s: %w", day, ErrBriefSent)
		}
		return BriefLog{}, fmt.Errorf("store: brief %s: %w", day, ErrBriefPending)
	}
	if err != nil {
		return BriefLog{}, fmt.Errorf("store: upsert brief %s: %w", day, err)
	}
	return b, nil
}

// BeginBriefSend records that a try of the day's unsent brief is starting (it may deliver
// from here on) and returns the state before it. A day with no unsent row is ErrNotFound.
func (q Queries) BeginBriefSend(ctx context.Context, day string) (string, error) {
	var prev string
	err := q.db.QueryRow(ctx,
		`UPDATE brief_log b SET send_state = 'maybe_sent'
		 FROM (SELECT id, send_state FROM brief_log WHERE day = $1::date AND sent_at IS NULL FOR UPDATE) old
		 WHERE b.id = old.id
		 RETURNING old.send_state`, day).Scan(&prev)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("store: begin brief %s send: %w", day, ErrNotFound)
	}
	if err != nil {
		return "", fmt.Errorf("store: begin brief %s send: %w", day, err)
	}
	return prev, nil
}

// MarkBriefRefused records that the day's only try was refused, so nothing went out and
// the day may be composed afresh. The caller knows it was the only try: BeginBriefSend
// returned BriefPending.
func (q Queries) MarkBriefRefused(ctx context.Context, day string) error {
	_, err := q.db.Exec(ctx,
		`UPDATE brief_log SET send_state = 'refused' WHERE day = $1::date AND sent_at IS NULL`, day)
	if err != nil {
		return fmt.Errorf("store: mark brief %s refused: %w", day, err)
	}
	return nil
}

// MarkBriefSent sets the day's sent_at and provider_id. A day with no row is ErrNotFound;
// one already sent is ErrBriefSent and keeps its first send.
func (q Queries) MarkBriefSent(ctx context.Context, day, providerID string) error {
	var sent bool
	err := q.db.QueryRow(ctx,
		`WITH upd AS (
		   UPDATE brief_log SET sent_at = now(), provider_id = $2
		   WHERE day = $1::date AND sent_at IS NULL RETURNING id)
		 SELECT NOT EXISTS (SELECT 1 FROM upd) FROM brief_log WHERE day = $1::date`, day, providerID).Scan(&sent)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return fmt.Errorf("store: mark brief %s sent: %w", day, ErrNotFound)
	case err != nil:
		return fmt.Errorf("store: mark brief %s sent: %w", day, err)
	case sent:
		return fmt.Errorf("store: mark brief %s sent: %w", day, ErrBriefSent)
	}
	return nil
}

// BriefByDay returns the day's brief_log row; none is ErrNotFound.
func (q Queries) BriefByDay(ctx context.Context, day string) (BriefLog, error) {
	b, err := scanBrief(q.db.QueryRow(ctx, `SELECT `+briefColumns+` FROM brief_log WHERE day = $1::date`, day))
	if errors.Is(err, pgx.ErrNoRows) {
		return BriefLog{}, fmt.Errorf("store: brief %s: %w", day, ErrNotFound)
	}
	if err != nil {
		return BriefLog{}, fmt.Errorf("store: brief %s: %w", day, err)
	}
	return b, nil
}

// LatestSentBrief returns the most recent day's brief that was sent; none is ErrNotFound.
func (q Queries) LatestSentBrief(ctx context.Context) (BriefLog, error) {
	b, err := scanBrief(q.db.QueryRow(ctx,
		`SELECT `+briefColumns+` FROM brief_log WHERE sent_at IS NOT NULL ORDER BY day DESC LIMIT 1`))
	if errors.Is(err, pgx.ErrNoRows) {
		return BriefLog{}, fmt.Errorf("store: latest sent brief: %w", ErrNotFound)
	}
	if err != nil {
		return BriefLog{}, fmt.Errorf("store: latest sent brief: %w", err)
	}
	return b, nil
}
