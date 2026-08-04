package acc

import (
	"fmt"

	"context"
	"github.com/jackc/pgx/v5"
	"testing"

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
