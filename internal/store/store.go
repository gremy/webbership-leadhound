// Package store holds leadhound's Postgres access: the connection pool, the embedded
// migrations and the hand-written queries.
//
// The app role (LEADHOUND_DATABASE_URL) may SELECT, INSERT and UPDATE; it has no DELETE, no
// TRUNCATE and owns nothing. Migrations run as the owner role through Migrate.
package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// defaultMaxConns is small: leadhound is one process running one command at a time, and the
// server's Postgres is shared with other apps.
const defaultMaxConns = 4

// ErrNotFound is returned when a requested record does not exist.
var ErrNotFound = errors.New("not found")

// Store wraps the connection pool.
type Store struct {
	pool *pgxpool.Pool
}

// envInt reads a positive integer from the environment, or def when unset or invalid.
func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

// Open connects to Postgres with a pool capped at LEADHOUND_DB_MAX_CONNS connections (default 4)
// and checks the connection with a ping.
func Open(ctx context.Context, dsn string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		// The DSN may carry a password, so it never goes into the error.
		return nil, fmt.Errorf("store: parse dsn: %w", err)
	}
	cfg.MaxConns = int32(envInt("LEADHOUND_DB_MAX_CONNS", defaultMaxConns))
	cfg.MaxConnLifetime = 5 * time.Minute
	cfg.MaxConnIdleTime = 2 * time.Minute

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("store: open pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("store: ping: %w", err)
	}
	return &Store{pool: pool}, nil
}

// Pool returns the underlying pool, for queries that live outside this package's helpers.
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// Close closes every connection in the pool.
func (s *Store) Close() { s.pool.Close() }
