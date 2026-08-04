package pg

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// Section is a pg_dump archive section.
//
// The split is the reason this design needs no session_replication_role: PreData creates tables
// with only their inline constraints, and every foreign key, index and trigger lives in PostData,
// which runs after the data is already in place.
type Section string

const (
	SectionPreData  Section = "pre-data"
	SectionPostData Section = "post-data"
)

// DumpSchema writes a custom-format, schema-only archive.
//
//   - --schema-only because the data comes from scrubbed COPY, never from pg_dump
//   - --no-owner --no-acl so production role names never leave production; the restore applies
//     its own ownership with --role
//   - --no-comments because COMMENT ON EXTENSION fails for non-superusers on both RDS and
//     Cloud SQL, and it is the single most common restore error
//
// snapshotID joins pg_dump to the export's own transaction snapshot, so the schema it captures is
// the schema the data was copied under. Without it the two are read at different instants and a
// migration landing mid-snapshot produces an artifact whose structure and rows disagree.
func DumpSchema(ctx context.Context, url, outPath, snapshotID string) error {
	args := []string{
		"--format=custom",
		"--schema-only",
		"--no-owner",
		"--no-acl",
		"--no-comments",
		"--file=" + outPath,
	}
	if snapshotID != "" {
		args = append(args, "--snapshot="+snapshotID)
	}
	return run(ctx, "pg_dump", append(args, url)...)
}

type RestoreOptions struct {
	Section Section

	// Jobs is only meaningful for post-data, where concurrent index builds and foreign key
	// validation scans are the bulk of the work. pre-data is a single dependency chain.
	Jobs int

	// Role is issued as SET ROLE before the restore, so objects are created owned correctly from
	// the start rather than reassigned afterwards
	Role string
}

// RestoreSection replays one section of a schema archive into a database.
func RestoreSection(ctx context.Context, url, archivePath string, opts RestoreOptions) error {
	args := []string{
		"--section=" + string(opts.Section),
		"--no-owner",
		"--no-acl",
		"--dbname=" + url,
	}
	if opts.Role != "" {
		args = append(args, "--role="+opts.Role)
	}
	// Parallel restore needs the archive on local disk, which is why the schema archive is
	// downloaded rather than streamed
	if opts.Section == SectionPostData && opts.Jobs > 1 {
		args = append(args, "--jobs="+strconv.Itoa(opts.Jobs))
	}
	args = append(args, archivePath)

	return run(ctx, "pg_restore", args...)
}

// Analyze refreshes planner statistics.
//
// Without it a restored environment has no statistics at all, every plan is a guess, and the
// tool gets blamed for the database being slow.
func Analyze(ctx context.Context, url string, jobs int) error {
	args := []string{"--analyze-only", "--dbname=" + url}
	if jobs > 1 {
		args = append(args, "--jobs="+strconv.Itoa(jobs))
	}
	return run(ctx, "vacuumdb", args...)
}

// ToolVersion reports the major version of a client binary, so a mismatch with the server is
// caught before it produces a confusing failure deep in a dump.
func ToolVersion(ctx context.Context, tool string) (int, error) {
	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, tool, "--version")
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return 0, fmt.Errorf("error running %s --version: %w", tool, err)
	}

	// "pg_dump (PostgreSQL) 18.1"
	fields := strings.Fields(out.String())
	if len(fields) < 3 {
		return 0, fmt.Errorf("could not read %s version from %q", tool, out.String())
	}
	major, _, _ := strings.Cut(fields[len(fields)-1], ".")
	n, err := strconv.Atoi(major)
	if err != nil {
		return 0, fmt.Errorf("could not read %s version from %q", tool, out.String())
	}
	return n, nil
}

// run executes a postgres client binary, surfacing its stderr on failure.
//
// Without this the caller gets "exit status 1" and none of the reason.
func run(ctx context.Context, name string, args ...string) error {
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			return fmt.Errorf("%s failed: %w", name, err)
		}
		return fmt.Errorf("%s failed: %w\n%s", name, err, detail)
	}
	return nil
}
