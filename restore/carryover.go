package restore

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/lib/pq"
	"github.com/nullstone-io/pg-snapshot/pg"
)

// Carryover copies the database-level state that a rename does not move.
//
// Database ACLs and per-database settings live on the database's OID, not its name. When the swap
// renames the target away, they go with it -- so the replacement has to be given them explicitly
// before the swap, or the restored environment comes up with its CONNECT grants and its timezone
// missing.
type Carryover struct {
	DB  pg.Querier
	Log *slog.Logger
}

func (c Carryover) log() *slog.Logger {
	if c.Log == nil {
		return slog.Default()
	}
	return c.Log
}

// Apply copies ACLs and settings from `from` onto `to`.
func (c Carryover) Apply(ctx context.Context, from, to string) error {
	if err := c.applyAcls(ctx, from, to); err != nil {
		return err
	}
	return c.applySettings(ctx, from, to)
}

const aclSql = `
SELECT COALESCE(datacl::text[], '{}') FROM pg_catalog.pg_database WHERE datname = $1`

func (c Carryover) applyAcls(ctx context.Context, from, to string) error {
	var items []string
	if err := c.DB.QueryRow(ctx, aclSql, from).Scan(&items); err != nil {
		return fmt.Errorf("error reading database privileges of %q: %w", from, err)
	}

	var applied int
	for _, item := range items {
		grant, ok := grantFromAclItem(item, to)
		if !ok {
			continue
		}
		if _, err := c.DB.Exec(ctx, grant); err != nil {
			return fmt.Errorf("error applying database privileges to %q: %w", to, err)
		}
		applied++
	}

	c.log().Info("database privileges carried over", "from", from, "to", to, "grants", applied)
	return nil
}

// grantFromAclItem turns one aclitem into a GRANT.
//
// An aclitem reads "grantee=privs/grantor", where an empty grantee is PUBLIC. Only the three
// database-level privileges are meaningful here; anything else in the string belongs to a
// different object type and is ignored rather than guessed at.
func grantFromAclItem(item, database string) (string, bool) {
	grantee, rest, ok := strings.Cut(item, "=")
	if !ok {
		return "", false
	}
	privChars, _, _ := strings.Cut(rest, "/")

	privileges := make([]string, 0, 3)
	withGrantOption := false
	for i, ch := range privChars {
		switch ch {
		case 'C':
			privileges = append(privileges, "CREATE")
		case 'T':
			privileges = append(privileges, "TEMPORARY")
		case 'c':
			privileges = append(privileges, "CONNECT")
		case '*':
			// Applies to the privilege just before it; treated as all-or-nothing because
			// postgres cannot express a per-privilege grant option in one statement anyway
			_ = i
			withGrantOption = true
		}
	}
	if len(privileges) < 1 {
		return "", false
	}

	target := "PUBLIC"
	if grantee != "" {
		target = pq.QuoteIdentifier(grantee)
	}

	sq := fmt.Sprintf("GRANT %s ON DATABASE %s TO %s",
		strings.Join(privileges, ", "), pq.QuoteIdentifier(database), target)
	if withGrantOption {
		sq += " WITH GRANT OPTION"
	}
	return sq, true
}

const settingsSql = `
SELECT COALESCE(pg_catalog.pg_get_userbyid(NULLIF(s.setrole, 0)), ''), s.setconfig
FROM pg_catalog.pg_db_role_setting s
JOIN pg_catalog.pg_database d ON d.oid = s.setdatabase
WHERE d.datname = $1`

func (c Carryover) applySettings(ctx context.Context, from, to string) error {
	rows, err := c.DB.Query(ctx, settingsSql, from)
	if err != nil {
		return fmt.Errorf("error reading settings of %q: %w", from, err)
	}
	defer rows.Close()

	type setting struct {
		role   string
		config []string
	}
	found := make([]setting, 0)
	for rows.Next() {
		var s setting
		if err := rows.Scan(&s.role, &s.config); err != nil {
			return fmt.Errorf("error reading settings of %q: %w", from, err)
		}
		found = append(found, s)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("error reading settings of %q: %w", from, err)
	}

	var applied int
	for _, s := range found {
		for _, kv := range s.config {
			key, value, ok := strings.Cut(kv, "=")
			if !ok {
				continue
			}
			// Per-role-per-database settings need a different statement than database-wide ones
			var sq string
			if s.role == "" {
				sq = fmt.Sprintf("ALTER DATABASE %s SET %s = %s",
					pq.QuoteIdentifier(to), pq.QuoteIdentifier(key), pq.QuoteLiteral(value))
			} else {
				sq = fmt.Sprintf("ALTER ROLE %s IN DATABASE %s SET %s = %s",
					pq.QuoteIdentifier(s.role), pq.QuoteIdentifier(to),
					pq.QuoteIdentifier(key), pq.QuoteLiteral(value))
			}
			if _, err := c.DB.Exec(ctx, sq); err != nil {
				return fmt.Errorf("error applying setting %q to %q: %w", key, to, err)
			}
			applied++
		}
	}

	c.log().Info("database settings carried over", "from", from, "to", to, "settings", applied)
	return nil
}
