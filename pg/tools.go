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
//   - --no-publications --no-subscriptions because replication topology belongs to the environment
//     it runs in, not to the schema. A production publication feeding a data warehouse has no
//     business being recreated in a lower environment, and ALTER PUBLICATION ... ADD TABLES IN
//     SCHEMA needs superuser besides.
//
// snapshotID joins pg_dump to the export's own transaction snapshot, so the schema it captures is
// the schema the data was copied under. Without it the two are read at different instants and a
// migration landing mid-snapshot produces an artifact whose structure and rows disagree.
//
// excludeTables are patterns from ExcludeTablePattern, one per table left out of the snapshot
// entirely. This is the only way to keep a table out of the dump, and it is what makes such a
// table exportable by a role that cannot read it: pg_dump locks every table it dumps in ACCESS
// SHARE, --schema-only included, and that lock needs SELECT on the table. One it was never asked
// to dump is neither locked nor read.
func DumpSchema(ctx context.Context, url, outPath, snapshotID string, excludeTables []string) error {
	args := []string{
		"--format=custom",
		"--schema-only",
		"--no-owner",
		"--no-acl",
		"--no-comments",
		"--no-publications",
		"--no-subscriptions",
		"--file=" + outPath,
	}
	if snapshotID != "" {
		args = append(args, "--snapshot="+snapshotID)
	}
	for _, pattern := range excludeTables {
		args = append(args, "--exclude-table="+pattern)
	}
	return run(ctx, "pg_dump", append(args, url)...)
}

// ExcludeTablePattern renders one table as a pg_dump --exclude-table pattern.
//
// pg_dump reads that argument as a pattern rather than a name: `*`, `?` and `[` are wildcards
// there, and an unquoted letter folds to lower case the way an unquoted SQL identifier does. Both
// halves are quoted so a table excludes itself and only itself, whatever its name contains.
func ExcludeTablePattern(schema, table string) string {
	return quotePatternPart(schema) + "." + quotePatternPart(table)
}

func quotePatternPart(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// ListArchive reads an archive's table of contents, one entry per line.
//
// The entries are the same text pg_restore --use-list consumes, so a caller can comment lines out
// and hand the result straight back.
func ListArchive(ctx context.Context, archivePath string) ([]string, error) {
	out, err := output(ctx, "pg_restore", "--list", archivePath)
	if err != nil {
		return nil, err
	}
	return strings.Split(strings.ReplaceAll(out, "\r\n", "\n"), "\n"), nil
}

// Object descriptions this package can recognise in a table of contents.
//
// A description may contain spaces, so they are matched longest-first: "PUBLICATION TABLES IN
// SCHEMA" has to win over "PUBLICATION" or the tail is misread.
const (
	DescExtension          = "EXTENSION"
	DescPublication        = "PUBLICATION"
	DescPublicationTable   = "PUBLICATION TABLE"
	DescPublicationSchemas = "PUBLICATION TABLES IN SCHEMA"
	DescSubscription       = "SUBSCRIPTION"
)

var knownDescs = []string{
	DescPublicationSchemas,
	DescPublicationTable,
	DescPublication,
	DescSubscription,
	DescExtension,
}

// TocEntry is one line of a pg_restore --list table of contents.
type TocEntry struct {
	// Desc is the object type, e.g. "EXTENSION" or "PUBLICATION TABLES IN SCHEMA"
	Desc string

	// Schema is the object's schema, or "-" for objects that belong to none
	Schema string

	// Name is the object's tag
	Name string
}

// ParseTocEntry reads a table-of-contents line.
//
// Entries are "<id>; <tableoid> <oid> <DESC> <schema> <name> [owner]". Only the descriptions in
// knownDescs are recognised; everything else reports false and is left alone.
func ParseTocEntry(line string) (TocEntry, bool) {
	if !isEntryLine(line) {
		return TocEntry{}, false
	}
	_, rest, ok := strings.Cut(line, ";")
	if !ok {
		return TocEntry{}, false
	}
	fields := strings.Fields(rest)
	if len(fields) < 4 {
		return TocEntry{}, false
	}
	fields = fields[2:] // drop tableoid and oid

	for _, desc := range knownDescs {
		words := strings.Fields(desc)
		if len(fields) < len(words)+2 {
			continue
		}
		if strings.Join(fields[:len(words)], " ") != desc {
			continue
		}
		tail := fields[len(words):]
		return TocEntry{Desc: desc, Schema: tail[0], Name: tail[1]}, true
	}
	return TocEntry{}, false
}

// ArchiveExtensions reports the extensions a table of contents would create.
func ArchiveExtensions(toc []string) []string {
	out := make([]string, 0)
	for _, line := range toc {
		if e, ok := ParseTocEntry(line); ok && e.Desc == DescExtension {
			out = append(out, e.Name)
		}
	}
	return out
}

// isEntryLine reports whether a line is a real table-of-contents entry rather than a comment, a
// blank, or the archive header.
//
// The leading dump id has to parse as a number. Without that check a header line would be taken for
// an entry the moment it happened to contain a semicolon.
func isEntryLine(line string) bool {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || strings.HasPrefix(trimmed, ";") {
		return false
	}
	id, rest, ok := strings.Cut(line, ";")
	if !ok {
		return false
	}
	if _, err := strconv.Atoi(strings.TrimSpace(id)); err != nil {
		return false
	}
	return len(strings.Fields(rest)) >= 3
}

// IsPublication reports whether an entry is part of a publication definition.
func IsPublication(e TocEntry) bool {
	switch e.Desc {
	case DescPublication, DescPublicationTable, DescPublicationSchemas:
		return true
	}
	return false
}

// CommentOut disables the entries drop selects.
//
// Commenting rather than deleting because --use-list is an ordering as well as a selection: every
// surviving entry keeps its position, so the only thing that changes is the statement being skipped.
func CommentOut(toc []string, drop func(TocEntry) bool) []string {
	out := make([]string, 0, len(toc))
	for _, line := range toc {
		if e, ok := ParseTocEntry(line); ok && drop(e) {
			out = append(out, ";"+line)
			continue
		}
		out = append(out, line)
	}
	return out
}

type RestoreOptions struct {
	Section Section

	// ListPath is a --use-list file selecting which archive entries to replay. Empty replays
	// every entry in the section.
	ListPath string

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
	if opts.ListPath != "" {
		args = append(args, "--use-list="+opts.ListPath)
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
	_, err := output(ctx, name, args...)
	return err
}

// output executes a postgres client binary and returns its stdout.
func output(ctx context.Context, name string, args ...string) (string, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			return "", fmt.Errorf("%s failed: %w", name, err)
		}
		return "", fmt.Errorf("%s failed: %w\n%s", name, err, detail)
	}
	return stdout.String(), nil
}
