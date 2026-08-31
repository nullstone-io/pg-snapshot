package restore

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
)

// Migrator runs the customer's schema migrations against the staging database.
//
// This is what reconciles schema drift. The snapshot carries production's migration-tracking
// table, so the customer's own tool applies exactly the delta between production's schema and the
// target environment's -- no diffing on our side, and no assumption about which tool they use.
type Migrator struct {
	// Command runs through `sh -c`, so it may be a pipeline or a path to a script baked into the
	// image. A script is the natural shape for anything multi-step; naming it here rather than
	// having pgsnap look for it at a conventional path keeps one mechanism instead of two, and
	// keeps it visible in the app's configuration.
	Command string

	// DatabaseURL points at the staging database and is exported to the command
	DatabaseURL string

	// Owner is exported as OWNER_ROLE, so a hook can SET ROLE to it and leave migration-created
	// objects owned like everything pg_restore created
	Owner string

	Log *slog.Logger
}

func (m Migrator) Run(ctx context.Context) error {
	log := m.Log
	if log == nil {
		log = slog.Default()
	}

	if m.Command == "" {
		// Not an error. A snapshot restored into an environment whose schema already matches needs
		// no migration step, and forcing one would make the simple case harder.
		log.Warn("no migration command configured; restoring production's schema as-is",
			"hint", "set MIGRATE_COMMAND to the command that migrates your schema")
		return nil
	}

	log.Info("running migrations", "command", m.Command)
	cmd := exec.CommandContext(ctx, "sh", "-c", m.Command)

	// The command's whole contract. Everything else it needs, it already has from the image.
	//
	// Both URL names are set: POSTGRES_URL because that is what Nullstone publishes and what the
	// app's own environment already uses, DATABASE_URL because most migration tools look for that.
	// Both point at the staging database, deliberately shadowing the app's own POSTGRES_URL --
	// which points at the admin database and is not where migrations belong.
	//
	// OWNER_ROLE carries the *resolved* owner: the deployment may only set the RESTORE_OWNER_ROLE
	// alias, or nothing at all, and the hook should not have to reimplement that resolution.
	cmd.Env = append(os.Environ(),
		"POSTGRES_URL="+m.DatabaseURL,
		"DATABASE_URL="+m.DatabaseURL,
		"OWNER_ROLE="+m.Owner,
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		// Deliberately fatal, and deliberately before the swap: a restore whose migrations failed
		// would put a database with production's older schema in front of the target environment's
		// newer code.
		return fmt.Errorf("migrations failed, aborting before the swap: %w", err)
	}

	log.Info("migrations complete")
	return nil
}
