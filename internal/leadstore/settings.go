package leadstore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ErrSettingsNotObject is returned by SaveSettings for a document that is not one JSON
// object.
var ErrSettingsNotObject = errors.New("leadstore: settings are not a JSON object")

// SaveSettings appends settings (a JSON object of the knobs changed; leadconfig types it) as
// the latest leads settings, by actor (operator or system), and returns its id.
func (q Queries) SaveSettings(ctx context.Context, settings []byte, actor string) (int64, error) {
	if !json.Valid(settings) || !bytes.HasPrefix(bytes.TrimSpace(settings), []byte("{")) {
		return 0, fmt.Errorf("leadstore: save settings: %w", ErrSettingsNotObject)
	}
	return q.appendSettings(ctx, "save settings", settings, actor)
}

// ResetSettings appends a reset: from now on the file's values apply again.
func (q Queries) ResetSettings(ctx context.Context, actor string) (int64, error) {
	return q.appendSettings(ctx, "reset settings", nil, actor)
}

func (q Queries) appendSettings(ctx context.Context, what string, settings []byte, actor string) (int64, error) {
	if actor != ActorOperator && actor != ActorSystem {
		return 0, fmt.Errorf("leadstore: %s: %w: %q", what, ErrUnknownActor, actor)
	}
	var doc *string
	if settings != nil {
		s := string(settings)
		doc = &s
	}
	var id int64
	if err := q.db.QueryRow(ctx,
		`INSERT INTO lead_setting (settings, actor) VALUES ($1::jsonb, $2) RETURNING id`, doc, actor).Scan(&id); err != nil {
		return 0, fmt.Errorf("leadstore: %s: %w", what, err)
	}
	return id, nil
}

// LatestSettings returns the latest leads settings row: its document, id and time. fromDB is
// true when that row holds settings; it is false with no row (id 0, raw nil) and after a
// reset (raw nil, with the reset's id and time), when the file's values apply.
func (q Queries) LatestSettings(ctx context.Context) (raw []byte, id int64, at time.Time, fromDB bool, err error) {
	err = q.db.QueryRow(ctx, `SELECT id, settings, at FROM lead_setting ORDER BY id DESC LIMIT 1`).Scan(&id, &raw, &at)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, 0, time.Time{}, false, nil
	}
	if err != nil {
		return nil, 0, time.Time{}, false, fmt.Errorf("leadstore: latest settings: %w", err)
	}
	return raw, id, at, raw != nil, nil
}
