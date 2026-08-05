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

func TestCommentOutExtensions(t *testing.T) {
	toc := []string{
		"2; 3079 16385 EXTENSION - pgcrypto ",
		"3; 3079 16512 EXTENSION - google_vacuum_mgmt ",
		"210; 1259 16640 TABLE public users postgres",
	}

	got := CommentOutExtensions(toc, map[string]bool{"google_vacuum_mgmt": true})

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
func TestCommentedEntriesAreNotExtensions(t *testing.T) {
	toc := []string{";3; 3079 16512 EXTENSION - google_vacuum_mgmt "}

	assert.Empty(t, ArchiveExtensions(toc))
	assert.Equal(t, toc, CommentOutExtensions(toc, map[string]bool{"google_vacuum_mgmt": true}))
}
