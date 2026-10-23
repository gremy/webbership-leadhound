package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// DBTX is what the query helpers run on: the pool, one connection or a transaction.
type DBTX interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Queries holds the hand-written queries over one DBTX, so the same helpers run on the pool
// or inside a transaction.
type Queries struct {
	db DBTX
}

// NewQueries returns the query helpers over db.
func NewQueries(db DBTX) Queries { return Queries{db: db} }

// Q returns the query helpers over the pool.
func (s *Store) Q() Queries { return Queries{db: s.pool} }

// InTx runs fn inside one transaction: committed when fn returns nil, rolled back otherwise.
// fn's error is returned as it is.
func (s *Store) InTx(ctx context.Context, fn func(Queries) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // a no-op after Commit
	if err := fn(Queries{db: tx}); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: commit: %w", err)
	}
	return nil
}
