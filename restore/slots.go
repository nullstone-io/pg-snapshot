package restore

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/nullstone-io/pg-snapshot/pg"
)

// Slot is a logical replication slot to carry across the swap.
type Slot struct {
	Name   string
	Plugin string

	// TwoPhase reports whether the slot decodes prepared transactions
	TwoPhase bool
}

// Slots rebinds logical replication slots onto the swapped-in database.
//
// A slot is cluster-wide but bound to a database OID, and the rename does not move it: after the
// swap it still points at the backup, and a consumer reconnecting to the target by name is told the
// slot "was not created in this database". There is no operation that rebinds one, so the only
// route is to drop and recreate.
//
// The position is not preserved, and cannot be -- a new slot begins at the current LSN. That costs
// nothing here, because the restore replaced every row in the database and a consumer has to
// backfill regardless.
type Slots struct {
	// Admin connects to a database other than the target, so it survives the swap
	Admin pg.Querier

	Log *slog.Logger
}

func (s Slots) log() *slog.Logger {
	if s.Log == nil {
		return slog.Default()
	}
	return s.Log
}

const slotsSql = `
SELECT slot_name, plugin, two_phase
FROM pg_catalog.pg_replication_slots
WHERE database = $1 AND slot_type = 'logical'
ORDER BY slot_name`

// Capture reads the logical slots belonging to a database. Run before the swap, while the slots
// still point at the database being replaced.
func (s Slots) Capture(ctx context.Context, database string) ([]Slot, error) {
	rows, err := s.Admin.Query(ctx, slotsSql, database)
	if err != nil {
		return nil, fmt.Errorf("error reading replication slots: %w", err)
	}
	defer rows.Close()

	out := make([]Slot, 0)
	for rows.Next() {
		var slot Slot
		if err := rows.Scan(&slot.Name, &slot.Plugin, &slot.TwoPhase); err != nil {
			return nil, fmt.Errorf("error reading replication slots: %w", err)
		}
		out = append(out, slot)
	}
	return out, rows.Err()
}

// CanCreate reports whether the current role may create replication slots.
//
// Checked before the swap so the operator hears about a missing grant while it is still cheap to
// fix. REPLICATION is a role attribute and is *not* inherited through membership, so holding the
// managed superuser role does not confer it.
func (s Slots) CanCreate(ctx context.Context) (bool, error) {
	var ok bool
	if err := s.Admin.QueryRow(ctx,
		`SELECT rolsuper OR rolreplication FROM pg_catalog.pg_roles WHERE rolname = CURRENT_USER`,
	).Scan(&ok); err != nil {
		return false, fmt.Errorf("error checking replication privilege: %w", err)
	}
	return ok, nil
}

// Rebind recreates the captured slots against the swapped-in database.
//
// Every failure is reported and swallowed. By the time this runs the swap is done and the database
// is live and correct; a replication slot that could not be recreated is a broken pipeline, not a
// broken restore, and tearing down a good database over it would be worse.
//
// targetURL must name the target database, because a slot is created in whichever database the
// session is connected to -- that is the whole point of the exercise.
func (s Slots) Rebind(ctx context.Context, targetURL string, slots []Slot) {
	if len(slots) < 1 {
		return
	}

	pool, err := pg.Open(ctx, targetURL, 1)
	if err != nil {
		s.log().Error("could not connect to rebind replication slots",
			"slots", names(slots), "error", err)
		return
	}
	defer pool.Close()

	for _, slot := range slots {
		if err := s.rebindOne(ctx, pool, slot); err != nil {
			s.log().Error("could not rebind replication slot; the restore is complete and live, "+
				"but this slot is not replicating",
				"slot", slot.Name, "plugin", slot.Plugin, "error", err)
			continue
		}
		s.log().Info("replication slot rebound", "slot", slot.Name, "plugin", slot.Plugin)
	}
}

func (s Slots) rebindOne(ctx context.Context, pool pg.Querier, slot Slot) error {
	// The orphan holds the name until it is gone -- pg_replication_slots is cluster-wide, unlike
	// pg_publication. Dropping it is safe only because the swap left the backup with
	// ALLOW_CONNECTIONS false, so nothing can have reattached.
	var exists, active bool
	err := s.Admin.QueryRow(ctx,
		`SELECT true, active FROM pg_catalog.pg_replication_slots WHERE slot_name = $1`,
		slot.Name).Scan(&exists, &active)
	switch {
	case err != nil && !pg.IsNoRows(err):
		return fmt.Errorf("error inspecting slot: %w", err)
	case exists && active:
		return fmt.Errorf("slot is still in use by another session; " +
			"something reconnected to the backup database before it could be dropped")
	case exists:
		if _, err := s.Admin.Exec(ctx,
			`SELECT pg_catalog.pg_drop_replication_slot($1)`, slot.Name); err != nil {
			return fmt.Errorf("error dropping the orphaned slot: %w", err)
		}
	}

	if _, err := pool.Exec(ctx,
		`SELECT pg_catalog.pg_create_logical_replication_slot($1, $2, false, $3)`,
		slot.Name, slot.Plugin, slot.TwoPhase); err != nil {
		return fmt.Errorf("error creating the slot: %w", err)
	}
	return nil
}

func names(slots []Slot) []string {
	out := make([]string, 0, len(slots))
	for _, s := range slots {
		out = append(out, s.Name)
	}
	return out
}
