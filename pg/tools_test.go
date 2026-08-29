package pg

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// The table-of-contents format is parsed by position, so the cases that matter are the ones whose
// shape differs from an extension entry but reads similarly.
func TestExtensionEntries(t *testing.T) {
	toc := []string{
		";",
		"; Archive created at 2026-08-05 01:21:47 UTC",
		";     dbname: fortuna",
		";",
		"; Selected TOC Entries:",
		";",
		"2; 3079 16385 EXTENSION - pgcrypto ",
		"3; 3079 16512 EXTENSION - google_vacuum_mgmt ",
		"5; 2615 16386 SCHEMA - google_vacuum_mgmt postgres",
		"210; 1259 16640 TABLE public users postgres",
		"4; 0 0 COMMENT - EXTENSION pgcrypto ",
		"",
	}

	assert.Equal(t, []string{"pgcrypto", "google_vacuum_mgmt"}, ArchiveExtensions(toc))
}

func TestCommentOut(t *testing.T) {
	toc := []string{
		"2; 3079 16385 EXTENSION - pgcrypto ",
		"3; 3079 16512 EXTENSION - google_vacuum_mgmt ",
		"210; 1259 16640 TABLE public users postgres",
	}

	got := CommentOut(toc, func(e TocEntry) bool {
		return e.Desc == DescExtension && e.Name == "google_vacuum_mgmt"
	})

	assert.Equal(t, []string{
		"2; 3079 16385 EXTENSION - pgcrypto ",
		";3; 3079 16512 EXTENSION - google_vacuum_mgmt ",
		"210; 1259 16640 TABLE public users postgres",
	}, got)

	// Every entry keeps its position: --use-list is an ordering, not only a selection
	assert.Len(t, got, len(toc))
}

// A commented entry must not be read back as a live one, or filtering an already-filtered list
// would resurrect it.
func TestCommentedEntriesAreNotParsed(t *testing.T) {
	toc := []string{";3; 3079 16512 EXTENSION - google_vacuum_mgmt "}

	assert.Empty(t, ArchiveExtensions(toc))
	assert.Equal(t, toc, CommentOut(toc, func(TocEntry) bool { return true }))
}

// A description containing spaces must not be mistaken for a shorter one that prefixes it, or the
// schema and name are read out of the wrong fields.
func TestParseTocEntryMultiWordDescriptions(t *testing.T) {
	for _, tc := range []struct {
		line string
		want TocEntry
	}{
		{
			"3460; 6106 16789 PUBLICATION - fortuna_ds_pub postgres",
			TocEntry{Desc: DescPublication, Schema: "-", Name: "fortuna_ds_pub"},
		},
		{
			"3461; 6108 16790 PUBLICATION TABLES IN SCHEMA - public postgres",
			TocEntry{Desc: DescPublicationSchemas, Schema: "-", Name: "public"},
		},
		{
			"3462; 6107 16791 PUBLICATION TABLE public users postgres",
			TocEntry{Desc: DescPublicationTable, Schema: "public", Name: "users"},
		},
	} {
		got, ok := ParseTocEntry(tc.line)
		assert.True(t, ok, tc.line)
		assert.Equal(t, tc.want, got, tc.line)
	}
}

// An ordinary table must not be swept up by a filter aimed at replication objects
func TestParseTocEntryIgnoresUnknownDescriptions(t *testing.T) {
	_, ok := ParseTocEntry("210; 1259 16640 TABLE public users postgres")
	assert.False(t, ok)
}

// pg_dump reads --exclude-table as a pattern, so an unquoted name is not the name: `*`, `?` and
// `[` are wildcards and unquoted letters fold to lower case. A pattern that excludes more than the
// table it names would silently drop tables the snapshot is supposed to carry.
func TestExcludeTablePattern(t *testing.T) {
	assert.Equal(t, `"public"."eng1189_keep"`, ExcludeTablePattern("public", "eng1189_keep"))

	// Case survives, so a table created as "Orders" is not confused with "orders"
	assert.Equal(t, `"public"."Orders"`, ExcludeTablePattern("public", "Orders"))

	// Wildcards in a real table name match themselves rather than a pattern
	assert.Equal(t, `"public"."weird[name]"`, ExcludeTablePattern("public", "weird[name]"))

	// A quote inside an identifier is doubled, the way postgres quotes it
	assert.Equal(t, `"public"."od""d"`, ExcludeTablePattern("public", `od"d`))
}
