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

// Option changes the pool's configuration before Open connects.
type Option func(*pgxpool.Config)

// maxAppName is Postgres's limit on application_name (NAMEDATALEN - 1); a longer name is
// cut there.
const maxAppName = 63

// WithAppName sets application_name on every connection, so pg_stat_activity shows which
// command holds a connection or runs a query.
func WithAppName(name string) Option {
	if len(name) > maxAppName {
		name = name[:maxAppName]
	}
	return func(cfg *pgxpool.Config) {
		cfg.ConnConfig.RuntimeParams["application_name"] = name
	}
}

// Open connects to Postgres with a pool capped at LEADHOUND_DB_MAX_CONNS connections (default 4)
// and checks the connection with a ping.
func Open(ctx context.Context, dsn string, opts ...Option) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		// The DSN may carry a password, so it never goes into the error.
		return nil, fmt.Errorf("store: parse dsn: %w", err)
	}
	cfg.MaxConns = int32(envInt("LEADHOUND_DB_MAX_CONNS", defaultMaxConns))
	cfg.MaxConnLifetime = 5 * time.Minute
	cfg.MaxConnIdleTime = 2 * time.Minute
	for _, opt := range opts {
		opt(cfg)
	}

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

// WithStatementTimeout runs fn with statement_timeout set to d on one pool connection, then
// resets it to the role's default before the connection goes back, on error as well. It is the
// one sanctioned way to run a query longer than the role's default allows; callers state the
// bound they need. fn's error is returned as it is.
func (s *Store) WithStatementTimeout(ctx context.Context, d time.Duration, fn func(Queries) error) (err error) {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("store: acquire: %w", err)
	}
	defer func() {
		// A fresh context: the reset must run even when ctx was cancelled inside fn.
		resetCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if _, rerr := conn.Exec(resetCtx, "RESET statement_timeout"); rerr != nil {
			// The connection may still carry the long timeout, so it never goes back to the pool.
			_ = conn.Conn().Close(resetCtx)
			if err == nil {
				err = fmt.Errorf("store: reset statement_timeout: %w", rerr)
			}
		}
		conn.Release()
	}()
	// SET takes no bind parameters; the value is an integer of milliseconds.
	if _, err := conn.Exec(ctx, "SET statement_timeout = "+strconv.FormatInt(d.Milliseconds(), 10)); err != nil {
		return fmt.Errorf("store: set statement_timeout: %w", err)
	}
	return fn(Queries{db: conn})
}

// Setting returns the current value of a Postgres run-time setting, as SHOW prints it.
func (q Queries) Setting(ctx context.Context, name string) (string, error) {
	var v string
	if err := q.db.QueryRow(ctx, "SELECT current_setting($1)", name).Scan(&v); err != nil {
		return "", fmt.Errorf("store: setting %s: %w", name, err)
	}
	return v, nil
}

// Pool returns the underlying pool, for queries that live outside this package's helpers.
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// Close closes every connection in the pool.
func (s *Store) Close() { s.pool.Close() }
