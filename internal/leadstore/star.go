package leadstore

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// The actions a lead_star row records (lead_star_action_check, migration 00011).
const (
	StarStar   = "star"
	StarUnstar = "unstar"
	StarPlace  = "place"
)

// starLockKey is the transaction-level advisory lock every star write holds from its read of
// the order to its rows, so two writes (two console tabs) never give two leads one position.
const starLockKey int64 = 0x4c48_5354_4152 // "LHSTAR"

// ErrNotStarred is returned by MoveUp, MoveDown and MoveTop for a lead that is not starred:
// only a starred lead has a place in the order.
var ErrNotStarred = errors.New("leadstore: lead is not starred")

// StarredLead is one starred lead and its place: the order is by Position, smallest first.
type StarredLead struct {
	LeadID   int64 `db:"lead_id"`
	Position int64 `db:"position"`
}

// starredSQL is every starred lead with its position: the latest lead_star row per lead,
// where it is not an unstar.
const starredSQL = `SELECT lead_id, position FROM (
	  SELECT DISTINCT ON (lead_id) lead_id, action, position FROM lead_star ORDER BY lead_id, id DESC
	) s WHERE action <> 'unstar'`

// StarOrder returns the starred leads in the operator's order: by position, then lead id.
func (q Queries) StarOrder(ctx context.Context) ([]StarredLead, error) {
	rows, err := collect(ctx, q.db, pgx.RowToStructByName[StarredLead], starredSQL+` ORDER BY position, lead_id`)
	if err != nil {
		return nil, fmt.Errorf("leadstore: star order: %w", err)
	}
	return rows, nil
}

// Star puts lead leadID at the bottom of the order (the largest position plus one). A lead
// already starred keeps its place: starred is false.
func (q Queries) Star(ctx context.Context, leadID int64, actor string) (starred bool, err error) {
	err = q.starWrite(ctx, "star", leadID, actor, func(tx pgx.Tx, pos *int64) error {
		if pos != nil {
			return nil
		}
		starred = true
		_, err := tx.Exec(ctx,
			`INSERT INTO lead_star (lead_id, action, position, actor)
			 SELECT $1, 'star', coalesce(max(position), 0) + 1, $2 FROM (`+starredSQL+`) s`, leadID, actor)
		return err
	})
	return starred, err
}

// Unstar takes lead leadID out of the order. A lead not starred is left as it is:
// unstarred is false.
func (q Queries) Unstar(ctx context.Context, leadID int64, actor string) (unstarred bool, err error) {
	err = q.starWrite(ctx, "unstar", leadID, actor, func(tx pgx.Tx, pos *int64) error {
		if pos == nil {
			return nil
		}
		unstarred = true
		_, err := tx.Exec(ctx, `INSERT INTO lead_star (lead_id, action, actor) VALUES ($1, 'unstar', $2)`, leadID, actor)
		return err
	})
	return unstarred, err
}

// MoveUp swaps lead leadID with the starred lead just before it. The first lead does not
// move: moved is false. A lead not starred is ErrNotStarred.
func (q Queries) MoveUp(ctx context.Context, leadID int64, actor string) (moved bool, err error) {
	return q.swap(ctx, "move up", leadID, actor,
		`SELECT lead_id, position FROM (`+starredSQL+`) s WHERE position < $1 ORDER BY position DESC, lead_id DESC LIMIT 1`)
}

// MoveDown swaps lead leadID with the starred lead just after it. The last lead does not
// move: moved is false. A lead not starred is ErrNotStarred.
func (q Queries) MoveDown(ctx context.Context, leadID int64, actor string) (moved bool, err error) {
	return q.swap(ctx, "move down", leadID, actor,
		`SELECT lead_id, position FROM (`+starredSQL+`) s WHERE position > $1 ORDER BY position, lead_id LIMIT 1`)
}

// MoveTop puts lead leadID at the top of the order (the smallest position minus one). The
// lead already at the top does not move: moved is false. A lead not starred is
// ErrNotStarred.
func (q Queries) MoveTop(ctx context.Context, leadID int64, actor string) (moved bool, err error) {
	err = q.starWrite(ctx, "move top", leadID, actor, func(tx pgx.Tx, pos *int64) error {
		if pos == nil {
			return ErrNotStarred
		}
		var top *int64 // the smallest other position; nil when no other lead is starred
		if err := tx.QueryRow(ctx, `SELECT min(position) FROM (`+starredSQL+`) s WHERE lead_id <> $1`, leadID).Scan(&top); err != nil {
			return err
		}
		if top == nil || *pos < *top {
			return nil // already the top, or the only star
		}
		moved = true
		_, err := tx.Exec(ctx, `INSERT INTO lead_star (lead_id, action, position, actor) VALUES ($1, 'place', $2, $3)`,
			leadID, *top-1, actor)
		return err
	})
	return moved, err
}

// swap moves lead leadID to the position of the neighbour neighbourSQL finds ($1: the lead's
// position) and the neighbour to the lead's: two place rows.
func (q Queries) swap(ctx context.Context, what string, leadID int64, actor, neighbourSQL string) (moved bool, err error) {
	err = q.starWrite(ctx, what, leadID, actor, func(tx pgx.Tx, pos *int64) error {
		if pos == nil {
			return ErrNotStarred
		}
		var other StarredLead
		err := tx.QueryRow(ctx, neighbourSQL, *pos).Scan(&other.LeadID, &other.Position)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // already at that end
		}
		if err != nil {
			return err
		}
		moved = true
		_, err = tx.Exec(ctx,
			`INSERT INTO lead_star (lead_id, action, position, actor) VALUES ($1, 'place', $2, $5), ($3, 'place', $4, $5)`,
			leadID, other.Position, other.LeadID, *pos, actor)
		return err
	})
	return moved, err
}

// starWrite runs fn in one transaction under starLockKey, with lead leadID's position (nil
// when it is not starred). A lead that does not exist is ErrLeadNotFound; an actor other
// than operator or system is ErrUnknownActor. Errors are wrapped with what.
func (q Queries) starWrite(ctx context.Context, what string, leadID int64, actor string, fn func(tx pgx.Tx, pos *int64) error) error {
	if actor != ActorOperator && actor != ActorSystem {
		return fmt.Errorf("leadstore: %s lead %d: %w: %q", what, leadID, ErrUnknownActor, actor)
	}
	b, ok := q.db.(interface {
		Begin(context.Context) (pgx.Tx, error)
	})
	if !ok {
		return fmt.Errorf("leadstore: %s lead %d: %T cannot begin a transaction", what, leadID, q.db)
	}
	tx, err := b.Begin(ctx)
	if err != nil {
		return fmt.Errorf("leadstore: %s lead %d: begin: %w", what, leadID, err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }() // a no-op after Commit
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, starLockKey); err != nil {
		return fmt.Errorf("leadstore: %s lead %d: lock: %w", what, leadID, err)
	}
	var exists bool
	var pos *int64
	err = tx.QueryRow(ctx,
		`SELECT true, (SELECT CASE WHEN s.action <> 'unstar' THEN s.position END
		               FROM lead_star s WHERE s.lead_id = l.id ORDER BY s.id DESC LIMIT 1)
		 FROM lead l WHERE l.id = $1`, leadID).Scan(&exists, &pos)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("leadstore: %s lead %d: %w", what, leadID, ErrLeadNotFound)
	}
	if err != nil {
		return fmt.Errorf("leadstore: %s lead %d: %w", what, leadID, err)
	}
	if err := fn(tx, pos); err != nil {
		return fmt.Errorf("leadstore: %s lead %d: %w", what, leadID, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("leadstore: %s lead %d: commit: %w", what, leadID, err)
	}
	return nil
}
