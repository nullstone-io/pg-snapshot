// Package pg holds the database plumbing shared by export and restore.
//
// The driver is pgx rather than lib/pq because the scrubbed export is built on
// COPY ... TO STDOUT, which lib/pq does not implement. lib/pq is still used, but only for its
// identifier and literal quoting helpers -- a small, stable API that pgx has no direct
// equivalent for on the literal side.
package pg

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Querier is the subset of pgx shared by a pool, a connection, and a transaction.
//
// Introspection is written against this so the same query runs on a pool during preflight and
// inside the export's REPEATABLE READ transaction without a second implementation.
type Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// IsNoRows reports whether a query returned nothing, which for a lookup is an answer rather than a
// failure.
func IsNoRows(err error) bool {
	return errors.Is(err, pgx.ErrNoRows)
}

// Open returns a pool sized for the requested concurrency.
//
// MaxConns is one above the worker count: the export holds a master connection open for the
// duration to keep its exported snapshot alive while the workers do the copying.
func Open(ctx context.Context, url string, workers int) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("error parsing connection url: %w", err)
	}
	if workers < 1 {
		workers = 1
	}
	cfg.MaxConns = int32(workers + 1)

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("error connecting: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("error connecting: %w", err)
	}
	return pool, nil
}

// WithDatabase rewrites a connection url to point at a different database on the same instance.
//
// The restore needs this constantly: it creates restored_<id> from a connection to `postgres`,
// loads into restored_<id>, and must return to `postgres` to rename either of them.
func WithDatabase(url, database string) (string, error) {
	cfg, err := pgx.ParseConfig(url)
	if err != nil {
		return "", fmt.Errorf("error parsing connection url: %w", err)
	}
	cfg.Database = database

	out := fmt.Sprintf("postgres://%s@%s:%d/%s",
		cfg.User, cfg.Host, cfg.Port, database)
	if cfg.Password != "" {
		out = fmt.Sprintf("postgres://%s:%s@%s:%d/%s",
			cfg.User, cfg.Password, cfg.Host, cfg.Port, database)
	}
	return out, nil
}

// DatabaseOf reports the database a connection url points at
func DatabaseOf(url string) (string, error) {
	cfg, err := pgx.ParseConfig(url)
	if err != nil {
		return "", fmt.Errorf("error parsing connection url: %w", err)
	}
	return cfg.Database, nil
}
