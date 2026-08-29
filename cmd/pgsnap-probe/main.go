// Command pgsnap-probe answers, in seconds, the question a full restore takes an hour to reach:
// can this role recreate the target's publications and replication slots?
//
// It runs the *production* code path -- restore.Publications.Carry is the same call the restore
// makes after migrations -- against a scratch database, so a pass here is a real pass.
//
// It is deliberately read-only with respect to anything that matters:
//
//   - the target database is only ever read from
//   - every write goes into a scratch database it creates and drops
//   - existing replication slots are never dropped, and never even named; the slot check creates
//     one with its own name in the scratch database and drops it again
//
// Usage:
//
//	POSTGRES_URL='postgres://<restore-role>:<pw>@<host>:5432/postgres?sslmode=require' \
//	TARGET_DATABASE=acme \
//	go run ./cmd/pgsnap-probe
//
// Delete this command once the question is settled; it is a diagnostic, not a feature.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/nullstone-io/pg-snapshot/pg"
	"github.com/nullstone-io/pg-snapshot/restore"
)

func main() {
	if err := run(context.Background()); err != nil {
		fmt.Fprintf(os.Stderr, "\nprobe failed: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	url := strings.TrimSpace(os.Getenv("POSTGRES_URL"))
	target := strings.TrimSpace(os.Getenv("TARGET_DATABASE"))
	if url == "" || target == "" {
		return fmt.Errorf("POSTGRES_URL and TARGET_DATABASE are both required")
	}

	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	adminURL, err := pg.WithDatabase(url, restore.AdminDatabase)
	if err != nil {
		return err
	}
	adminPool, err := pg.Open(ctx, adminURL, 2)
	if err != nil {
		return fmt.Errorf("could not connect: %w", err)
	}
	defer adminPool.Close()

	// 1. Who are we, and what does the server think we can do
	var user string
	var superuser, replication bool
	if err := adminPool.QueryRow(ctx,
		`SELECT CURRENT_USER, rolsuper, rolreplication FROM pg_roles WHERE rolname = CURRENT_USER`,
	).Scan(&user, &superuser, &replication); err != nil {
		return fmt.Errorf("could not read connection identity: %w", err)
	}
	section("identity")
	fmt.Printf("  role            %s\n", user)
	fmt.Printf("  rolsuper        %v   %s\n", superuser, note(superuser,
		"FOR ALL TABLES / FOR TABLES IN SCHEMA are permitted",
		"those two forms need a real superuser; membership does not substitute"))
	fmt.Printf("  rolreplication  %v   %s\n", replication, note(replication,
		"replication slots can be created",
		"pg_create_logical_replication_slot will be denied"))

	// 2. Read the real publications. This is the same code the restore runs.
	targetURL, err := pg.WithDatabase(url, target)
	if err != nil {
		return err
	}
	targetPool, err := pg.Open(ctx, targetURL, 1)
	if err != nil {
		return fmt.Errorf("could not connect to %q: %w", target, err)
	}
	defer targetPool.Close()

	section(fmt.Sprintf("publications found in %q", target))
	pubs, err := restore.ReadPublications(ctx, targetPool)
	if err != nil {
		return fmt.Errorf("READ FAILED: %w", err)
	}
	if len(pubs) < 1 {
		fmt.Printf("  none -- there is nothing for the restore to carry\n")
	}
	for _, p := range pubs {
		fmt.Printf("  %s\n", p.Name)
		fmt.Printf("    %s\n", restore.DescribePublication(p))
		fmt.Printf("    would run: %s\n", restore.CreatePublicationSql(p))
	}

	// 3. Recreate them for real, in a scratch database that is dropped afterwards
	scratch := fmt.Sprintf("pgsnap_probe_%d", time.Now().UnixNano()%1e9)
	section(fmt.Sprintf("recreating them in a scratch database (%s)", scratch))

	admin := restore.Admin{DB: adminPool, Log: log}
	if err := admin.CreateDatabase(ctx, scratch, ""); err != nil {
		return fmt.Errorf("could not create the scratch database: %w", err)
	}
	defer func() {
		if err := admin.DropDatabase(context.WithoutCancel(ctx), scratch); err != nil {
			fmt.Fprintf(os.Stderr, "\nWARNING: could not drop scratch database %q: %v\n", scratch, err)
		}
	}()

	scratchURL, err := pg.WithDatabase(url, scratch)
	if err != nil {
		return err
	}
	scratchPool, err := pg.Open(ctx, scratchURL, 1)
	if err != nil {
		return err
	}
	defer scratchPool.Close()

	// Publications naming tables explicitly need those tables to exist. Recreate them empty so the
	// statement is exercised rather than skipped -- structure is all CREATE PUBLICATION looks at.
	if err := stubTables(ctx, targetPool, scratchPool, pubs); err != nil {
		return err
	}

	pubErr := (restore.Publications{Log: log}).Carry(ctx, targetPool, scratchPool)
	result("publications", pubErr)

	// 4. The slot half, with a name of its own so nothing existing is touched
	section("replication slot")
	slotErr := probeSlot(ctx, scratchPool)
	result("replication slot", slotErr)

	section("verdict")
	switch {
	case pubErr == nil && slotErr == nil:
		fmt.Printf("  Both halves work. The restore's final step will succeed.\n")
	case pubErr != nil && slotErr == nil:
		fmt.Printf("  Slots work; publications do not. The restore will abort before the swap,\n")
		fmt.Printf("  leaving the target untouched. Publications need a privileged path.\n")
	case pubErr == nil && slotErr != nil:
		fmt.Printf("  Publications work; slots do not. The restore will complete and swap in,\n")
		fmt.Printf("  then warn that the slot could not be recreated.\n")
	default:
		fmt.Printf("  Neither half works through this role.\n")
	}
	if pubErr != nil || slotErr != nil {
		return fmt.Errorf("see above")
	}
	return nil
}

// stubTables creates empty copies of the tables a publication names explicitly.
//
// Only the relation has to exist for CREATE PUBLICATION to be accepted, so a single dummy column
// is enough -- and it keeps the probe from needing any of the real schema.
func stubTables(ctx context.Context, from, to pg.Querier, pubs []restore.Publication) error {
	seen := map[string]bool{}
	for _, p := range pubs {
		for _, t := range p.Tables {
			key := t.Schema + "." + t.Name
			if seen[key] {
				continue
			}
			seen[key] = true
			if _, err := to.Exec(ctx, fmt.Sprintf(`CREATE SCHEMA IF NOT EXISTS %q`, t.Schema)); err != nil {
				return fmt.Errorf("could not create scratch schema %q: %w", t.Schema, err)
			}
			if _, err := to.Exec(ctx,
				fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %q.%q (probe_id int)`, t.Schema, t.Name)); err != nil {
				return fmt.Errorf("could not create scratch table %s: %w", key, err)
			}
		}
	}
	return nil
}

// probeSlot creates and drops a slot under its own name, in the scratch database.
func probeSlot(ctx context.Context, db pg.Querier) error {
	const name = "pgsnap_probe_slot"
	if _, err := db.Exec(ctx,
		`SELECT pg_create_logical_replication_slot($1, 'pgoutput')`, name); err != nil {
		return err
	}
	if _, err := db.Exec(ctx, `SELECT pg_drop_replication_slot($1)`, name); err != nil {
		return fmt.Errorf("created the slot but could not drop it (drop it by hand: "+
			"SELECT pg_drop_replication_slot('%s')): %w", name, err)
	}
	return nil
}

func section(title string) {
	fmt.Printf("\n== %s %s\n", title, strings.Repeat("=", max(0, 60-len(title))))
}

func result(what string, err error) {
	if err == nil {
		fmt.Printf("\n  PASS  %s\n", what)
		return
	}
	fmt.Printf("\n  FAIL  %s\n", what)
	for _, line := range strings.Split(err.Error(), "\n") {
		fmt.Printf("        %s\n", line)
	}
}

func note(ok bool, yes, no string) string {
	if ok {
		return "-> " + yes
	}
	return "-> " + no
}
