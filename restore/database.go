package restore

import (
	"context"
	"fmt"
	"hash/fnv"
	"log/slog"
	"time"

	"github.com/lib/pq"
	"github.com/nullstone-io/pg-snapshot/pg"
)

// Admin performs the database-level operations a restore needs.
//
// Every one of these runs against the `postgres` database rather than the target: a database
// cannot be renamed by a session connected to it, and the swap renames both sides.
type Admin struct {
	DB  pg.Querier
	Log *slog.Logger
}

func (a Admin) log() *slog.Logger {
	if a.Log == nil {
		return slog.Default()
	}
	return a.Log
}

const databaseSetSql = `
SELECT datname FROM pg_catalog.pg_database WHERE NOT datistemplate ORDER BY datname`

// Inspect reads the target database and its siblings out of the catalog.
func (a Admin) Inspect(ctx context.Context, target string) (DatabaseSet, error) {
	set := DatabaseSet{Target: target}

	rows, err := a.DB.Query(ctx, databaseSetSql)
	if err != nil {
		return set, fmt.Errorf("error listing databases: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return set, fmt.Errorf("error listing databases: %w", err)
		}
		switch {
		case name == target:
			set.TargetExists = true
		case IsBackupOf(target, name):
			set.Backups = append(set.Backups, name)
		case IsStaging(name):
			set.Staging = append(set.Staging, name)
		}
	}
	return set, rows.Err()
}

// Lock takes an advisory lock keyed on the target database name.
//
// Session-scoped rather than transaction-scoped because the restore spans many transactions and
// several minutes. Two restores of the same target must not interleave: both would create staging
// databases, and both would try to rename the same target.
func (a Admin) Lock(ctx context.Context, target string) (bool, error) {
	h := fnv.New64a()
	h.Write([]byte("pg-snapshot:" + target))

	var acquired bool
	if err := a.DB.QueryRow(ctx,
		`SELECT pg_catalog.pg_try_advisory_lock($1)`, int64(h.Sum64())).Scan(&acquired); err != nil {
		return false, fmt.Errorf("error acquiring restore lock: %w", err)
	}
	return acquired, nil
}

func (a Admin) Unlock(ctx context.Context, target string) error {
	h := fnv.New64a()
	h.Write([]byte("pg-snapshot:" + target))

	if _, err := a.DB.Exec(ctx, `SELECT pg_catalog.pg_advisory_unlock($1)`, int64(h.Sum64())); err != nil {
		return fmt.Errorf("error releasing restore lock: %w", err)
	}
	return nil
}

// CreateDatabase creates the staging database owned by the role that will own the restored data.
func (a Admin) CreateDatabase(ctx context.Context, name, owner string) error {
	sq := fmt.Sprintf("CREATE DATABASE %s", pq.QuoteIdentifier(name))
	if owner != "" {
		sq += " OWNER " + pq.QuoteIdentifier(owner)
	}

	a.log().Info("creating database", "database", name, "owner", owner)
	if _, err := a.DB.Exec(ctx, sq); err != nil {
		return fmt.Errorf("error creating database %q: %w", name, err)
	}
	return nil
}

// DropDatabase removes a database, disconnecting anything still attached.
//
// WITH (FORCE) is why this tool requires postgres 13 or newer at minimum; requiring 16 makes it
// unconditional. Without it, cleaning up a staging database left by a crashed run needs a manual
// disconnect first, which is exactly the situation where nobody is watching.
func (a Admin) DropDatabase(ctx context.Context, name string) error {
	a.log().Info("dropping database", "database", name)
	sq := fmt.Sprintf("DROP DATABASE IF EXISTS %s WITH (FORCE)", pq.QuoteIdentifier(name))
	if _, err := a.DB.Exec(ctx, sq); err != nil {
		return fmt.Errorf("error dropping database %q: %w", name, err)
	}
	return nil
}

// SetAllowConnections opens or closes a database to new sessions.
//
// Closing first is what makes the swap safe behind a connection pooler. Terminating backends
// alone loses the race: PgBouncer or RDS Proxy reconnects before the rename lands, and the rename
// fails because the database is in use.
func (a Admin) SetAllowConnections(ctx context.Context, name string, allow bool) error {
	sq := fmt.Sprintf("ALTER DATABASE %s WITH ALLOW_CONNECTIONS %t", pq.QuoteIdentifier(name), allow)
	if _, err := a.DB.Exec(ctx, sq); err != nil {
		return fmt.Errorf("error setting allow_connections on %q: %w", name, err)
	}
	return nil
}

const terminateSql = `
SELECT pg_catalog.pg_terminate_backend(pid)
FROM pg_catalog.pg_stat_activity
WHERE datname = $1 AND pid <> pg_catalog.pg_backend_pid()`

// TerminateConnections disconnects every other session on a database.
//
// Requires either pg_signal_backend or membership in the roles being disconnected; the restore
// role holds instance admin, which covers both.
func (a Admin) TerminateConnections(ctx context.Context, name string) error {
	tag, err := a.DB.Exec(ctx, terminateSql, name)
	if err != nil {
		return fmt.Errorf("error terminating connections to %q: %w", name, err)
	}
	a.log().Info("terminated connections", "database", name, "count", tag.RowsAffected())
	return nil
}

// RenameDatabase renames a database. The caller must not be connected to it and no other session
// may be either.
func (a Admin) RenameDatabase(ctx context.Context, from, to string) error {
	a.log().Info("renaming database", "from", from, "to", to)
	sq := fmt.Sprintf("ALTER DATABASE %s RENAME TO %s", pq.QuoteIdentifier(from), pq.QuoteIdentifier(to))
	if _, err := a.DB.Exec(ctx, sq); err != nil {
		return fmt.Errorf("error renaming database %q to %q: %w", from, to, err)
	}
	return nil
}

// Swap replaces target with staging.
//
// The order is load-bearing and the window between the two renames is the only point in a restore
// where the target does not exist under its own name. Both renames are catalog-only operations,
// so that window is milliseconds -- but it is not zero, which is why Recover exists.
func (a Admin) Swap(ctx context.Context, target, staging string) (backup string, err error) {
	// First, so that a pooler's reconnect is refused rather than racing the rename
	if err := a.SetAllowConnections(ctx, target, false); err != nil {
		return "", err
	}
	if err := a.TerminateConnections(ctx, target); err != nil {
		return "", err
	}
	// The migration step leaves idle sessions on the staging database; they block its rename
	// exactly the same way
	if err := a.TerminateConnections(ctx, staging); err != nil {
		return "", err
	}

	backup, err = a.uniqueBackupName(ctx, target)
	if err != nil {
		return "", err
	}
	if err := a.RenameDatabase(ctx, target, backup); err != nil {
		// Nothing has moved; reopen the target and leave it exactly as it was
		if reopenErr := a.SetAllowConnections(ctx, target, true); reopenErr != nil {
			a.log().Error("could not reopen target after a failed rename",
				"database", target, "error", reopenErr)
		}
		return "", err
	}

	if err := a.RenameDatabase(ctx, staging, target); err != nil {
		// The dangerous window. Put the original back before returning.
		if backErr := a.RenameDatabase(ctx, backup, target); backErr != nil {
			return "", fmt.Errorf("swap failed and the original could not be restored; "+
				"%q still holds the previous database and must be renamed back to %q manually: %w",
				backup, target, backErr)
		}
		if reopenErr := a.SetAllowConnections(ctx, target, true); reopenErr != nil {
			a.log().Error("could not reopen target after rollback", "database", target, "error", reopenErr)
		}
		return "", err
	}

	if err := a.SetAllowConnections(ctx, target, true); err != nil {
		return backup, err
	}
	return backup, nil
}

// uniqueBackupName picks a backup name that is not already taken.
//
// Backup names carry a timestamp at second granularity, which is what makes them readable and
// sortable -- but two swaps of the same target inside one second would collide, and the rename
// would fail with the target already renamed away. Rare in production, since the advisory lock
// serialises restores and a run takes minutes; cheap enough to rule out entirely.
//
// The suffix does not disturb recovery: NewestBackup sorts lexicographically, and two backups from
// the same second are the same instant, so their relative order is arbitrary either way.
func (a Admin) uniqueBackupName(ctx context.Context, target string) (string, error) {
	set, err := a.Inspect(ctx, target)
	if err != nil {
		return "", err
	}
	taken := make(map[string]bool, len(set.Backups))
	for _, name := range set.Backups {
		taken[name] = true
	}

	base := BackupName(target, time.Now())
	if !taken[base] {
		return base, nil
	}

	for i := 1; i < 100; i++ {
		candidate := fmt.Sprintf("%s_%02d", base, i)
		if !taken[candidate] {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("could not find an unused backup name for %q; "+
		"drop some of the %d existing backups", target, len(set.Backups))
}

// Recover puts a target back after a run died mid-swap.
//
// Deliberately does not continue the interrupted restore. The staging database's state is unknown
// -- it may have been mid-migration -- and the safe outcome is the environment it had before.
func (a Admin) Recover(ctx context.Context, target string) error {
	set, err := a.Inspect(ctx, target)
	if err != nil {
		return err
	}

	switch state := Classify(set); state {
	case StateIdle:
		a.log().Info("nothing to recover", "state", state, "detail", set.Describe())
		return nil

	case StateOrphanedStaging:
		a.log().Info("dropping staging databases left by an interrupted run",
			"state", state, "detail", set.Describe())
		for _, name := range set.Staging {
			if err := a.DropDatabase(ctx, name); err != nil {
				return err
			}
		}
		return nil

	case StateCrashedMidSwap:
		backup := set.NewestBackup()
		a.log().Warn("target is missing; restoring it from the newest backup",
			"state", state, "backup", backup, "detail", set.Describe())
		if err := a.RenameDatabase(ctx, backup, target); err != nil {
			return err
		}
		if err := a.SetAllowConnections(ctx, target, true); err != nil {
			return err
		}
		for _, name := range set.Staging {
			if err := a.DropDatabase(ctx, name); err != nil {
				return err
			}
		}
		return nil

	default:
		return fmt.Errorf("database %q does not exist and there is no backup to restore it from; "+
			"this is not a state a restore can produce, so nothing has been changed (%s)",
			target, set.Describe())
	}
}
