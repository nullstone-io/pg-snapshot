package restore

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lib/pq"
	"github.com/nullstone-io/pg-snapshot/pg"
)

// tempMembership borrows the privileges of other roles for the length of one statement.
//
// Ported from pg-db-admin's temp_role_membership.go, which does the same thing for the same reason:
// managed postgres hands out no superuser, so a role that has to act on objects it does not own
// grants itself the owner's membership, acts, and revokes.
//
// A superuser needs none of it, which is why the caller checks first.
type tempMembership struct {
	conn  *pgxpool.Conn
	tx    pgx.Tx
	pool  pg.Querier
	roles []string
	user  string
	log   *slog.Logger

	// unavailable are the roles postgres refused to grant: a superuser, or a role the platform
	// reserves. Recorded rather than fatal, because what to do about it depends on the caller.
	unavailable []string
}

// borrowRoles acquires the privileges of every role named, and returns a handle that gives them
// back. The returned handle is always non-nil, so release is safe to defer before checking err.
//
// The lock is taken once for the whole batch rather than per role. Granting one at a time would
// deadlock: every grant locks the same key -- the current user -- and holds it until its membership
// is revoked.
func borrowRoles(ctx context.Context, pool *pgxpool.Pool, user string, roles []string, log *slog.Logger) (*tempMembership, error) {
	held := &tempMembership{pool: pool, user: user, log: log}

	needed := make([]string, 0, len(roles))
	for _, role := range roles {
		if role == user {
			continue
		}
		has, err := hasPrivilegesOfRole(ctx, held.pool, user, role)
		if err != nil {
			return held, err
		}
		if !has {
			needed = append(needed, role)
		}
	}
	if len(needed) < 1 {
		return held, nil
	}

	log.Info("borrowing role privileges", "user", user, "roles", needed)

	conn, err := pool.Acquire(ctx)
	if err != nil {
		return held, fmt.Errorf("error acquiring a connection for the membership lock: %w", err)
	}
	held.conn = conn

	tx, err := conn.Begin(ctx)
	if err != nil {
		return held, fmt.Errorf("error starting the membership lock: %w", err)
	}
	held.tx = tx

	if err := lockRole(ctx, tx, user); err != nil {
		return held, err
	}

	for _, role := range needed {
		// WITH INHERIT TRUE is not optional. Since postgres 16 a CREATEROLE user is granted ADMIN
		// OPTION on the roles it creates with INHERIT FALSE, so the membership exists while
		// conferring nothing -- and the statement that follows is still denied.
		sq := fmt.Sprintf("GRANT %s TO %s WITH INHERIT TRUE",
			pq.QuoteIdentifier(role), pq.QuoteIdentifier(user))
		if _, err := held.pool.Exec(ctx, sq); err != nil {
			// insufficient_privilege is postgres saying the membership can never be granted by this
			// user -- a superuser, or a role the platform reserves (RDS refuses rdsadmin with its own
			// message under the same code). Asking postgres is the one check that holds on every
			// platform, so the role is recorded and the caller decides what its absence costs.
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "42501" {
				log.Warn("role cannot be borrowed", "role", role, "user", user, "detail", pgErr.Message)
				held.unavailable = append(held.unavailable, role)
				continue
			}
			return held, fmt.Errorf("error granting %q to %q: %w", role, user, err)
		}
		held.roles = append(held.roles, role)
	}
	return held, nil
}

// release gives back every borrowed membership, continuing past a failure so one stuck role does
// not leave the rest granted.
func (t *tempMembership) release(ctx context.Context) {
	defer func() {
		if t.tx != nil {
			t.tx.Rollback(ctx)
		}
		if t.conn != nil {
			t.conn.Release()
		}
	}()

	for _, role := range t.roles {
		has, err := hasPrivilegesOfRole(ctx, t.pool, t.user, role)
		if err != nil || !has {
			continue
		}
		sq := fmt.Sprintf("REVOKE %s FROM %s", pq.QuoteIdentifier(role), pq.QuoteIdentifier(t.user))
		if _, err := t.pool.Exec(ctx, sq); err != nil {
			t.log.Error("could not revoke a borrowed role membership",
				"role", role, "user", t.user, "error", err)
		}
	}
}

// lockRole serialises concurrent borrowers of the same user's memberships.
func lockRole(ctx context.Context, tx pgx.Tx, role string) error {
	if _, err := tx.Exec(ctx,
		`SELECT pg_advisory_xact_lock(oid::bigint) FROM pg_roles WHERE rolname = $1`, role); err != nil {
		return fmt.Errorf("error locking role %q: %w", role, err)
	}
	if _, err := tx.Exec(ctx,
		`SELECT pg_advisory_xact_lock(member::bigint) FROM pg_auth_members
		 JOIN pg_roles ON roleid = pg_roles.oid WHERE rolname = $1`, role); err != nil {
		return fmt.Errorf("error locking members of role %q: %w", role, err)
	}
	return nil
}

// hasPrivilegesOfRole reports whether member holds role's privileges.
//
// pg_has_role(..., 'USAGE') rather than a pg_auth_members lookup: since postgres 16 the membership
// row can exist with INHERIT FALSE, so reading the row reports "already a member" and skips a grant
// that is genuinely required.
func hasPrivilegesOfRole(ctx context.Context, db pg.Querier, member, role string) (bool, error) {
	var has bool
	if err := db.QueryRow(ctx, `SELECT pg_has_role($1, $2, 'USAGE')`, member, role).Scan(&has); err != nil {
		return false, fmt.Errorf("error reading role membership: %w", err)
	}
	return has, nil
}
