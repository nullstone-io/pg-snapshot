// Package acc holds acceptance tests that need a real postgres.
//
// They cover what unit tests structurally cannot: that the SQL this tool generates is actually
// accepted by a server, that a scrubbed projection round-trips through COPY, and that the swap
// behaves when the databases genuinely exist. Gated behind ACC so `go test ./...` stays fast and
// dependency-free.
package acc

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nullstone-io/pg-snapshot/pg"
)

const (
	envEnabled = "ACC"
	envURL     = "ACC_DATABASE_URL"

	defaultURL = "postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable"
)

// URL is the connection the acceptance suite runs against
func URL() string {
	if url := os.Getenv(envURL); url != "" {
		return url
	}
	return defaultURL
}

// Connect skips the test unless the acceptance suite is enabled.
func Connect(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()

	if os.Getenv(envEnabled) == "" {
		t.Skipf("set %s=1 to run acceptance tests (needs a postgres at %s)", envEnabled, envURL)
	}

	ctx := context.Background()
	pool, err := pg.Open(ctx, URL(), 4)
	if err != nil {
		t.Fatalf("could not connect to %s: %v", URL(), err)
	}
	t.Cleanup(pool.Close)

	return pool, ctx
}
