package acc

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nullstone-io/pg-snapshot/pg"
	"github.com/stretchr/testify/require"
)

func withDatabase(url, database string) (string, error) {
	return pg.WithDatabase(url, database)
}

// markDatabase writes an identifiable value so a rename can be shown to have moved the actual
// database rather than just its name
func markDatabase(t *testing.T, ctx context.Context, url, mark string) {
	t.Helper()

	pool, err := pg.Open(ctx, url, 1)
	require.NoError(t, err)
	defer pool.Close()

	_, err = pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS pgsnap_mark (mark text)`)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO pgsnap_mark VALUES ($1)`, mark)
	require.NoError(t, err)
}

func readMark(t *testing.T, ctx context.Context, url string) string {
	t.Helper()

	pool, err := pg.Open(ctx, url, 1)
	require.NoError(t, err)
	defer pool.Close()

	var mark string
	require.NoError(t, pool.QueryRow(ctx, `SELECT mark FROM pgsnap_mark LIMIT 1`).Scan(&mark))
	return mark
}

// withUser rewrites a connection url to authenticate as a different role, so a test can act as
// the limited role a snapshot actually runs with rather than as the superuser that owns the
// fixtures.
func withUser(url, user, password string) (string, error) {
	cfg, err := pgx.ParseConfig(url)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=disable",
		user, password, cfg.Host, cfg.Port, cfg.Database), nil
}

// discardLogger silences the log output of code under test.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// captureLogger records what the code under test logged, for the assertions where the log *is* the
// behaviour -- a warning nobody emits is the same as no warning at all.
func captureLogger() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewTextHandler(&buf, nil)), &buf
}

// dbURL points the acceptance connection at another database on the same server.
func dbURL(t *testing.T, database string) string {
	t.Helper()

	url, err := withDatabase(URL(), database)
	require.NoError(t, err)
	return url
}

// twoDatabases creates the pair a carryover acts across: the target being replaced and the staging
// database that will take its place.
func twoDatabases(t *testing.T, ctx context.Context, pool *pgxpool.Pool, prefix string) (target, staging string, cleanup func()) {
	t.Helper()

	target, staging = prefix+"_target", prefix+"_staging"
	drop := func() {
		for _, db := range []string{target, staging} {
			pool.Exec(ctx, fmt.Sprintf(`DROP DATABASE IF EXISTS %s WITH (FORCE)`, db))
		}
	}
	drop()

	require.NoError(t, execAll(ctx, pool,
		fmt.Sprintf(`CREATE DATABASE %s`, target),
		fmt.Sprintf(`CREATE DATABASE %s`, staging),
	))
	return target, staging, drop
}

// execInDatabase runs statements against a database other than the suite's default.
func execInDatabase(t *testing.T, ctx context.Context, url string, stmts ...string) {
	t.Helper()

	pool, err := pg.Open(ctx, url, 1)
	require.NoError(t, err)
	defer pool.Close()

	require.NoError(t, execAll(ctx, pool, stmts...))
}
