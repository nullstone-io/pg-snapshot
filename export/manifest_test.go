package export

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func validManifest() Manifest {
	return Manifest{
		ArtifactVersion: ArtifactVersion,
		Scrubbed:        true,
		Source:          Source{ServerMajor: 16, Database: "core"},
	}
}

func TestManifestValidate(t *testing.T) {
	t.Run("same major", func(t *testing.T) {
		assert.NoError(t, validManifest().Validate(16))
	})

	t.Run("restoring into a newer major is fine", func(t *testing.T) {
		assert.NoError(t, validManifest().Validate(18))
	})

	// The check that stops an unscrubbed pg_dump being restored through this tool by mistake
	t.Run("refuses an unscrubbed artifact", func(t *testing.T) {
		m := validManifest()
		m.Scrubbed = false

		err := m.Validate(16)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not marked scrubbed")
	})

	t.Run("refuses a newer dump in an older server", func(t *testing.T) {
		m := validManifest()
		m.Source.ServerMajor = 18

		err := m.Validate(16)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "cannot be restored into an older major version")
	})

	t.Run("refuses a target below the supported floor", func(t *testing.T) {
		m := validManifest()
		m.Source.ServerMajor = 15

		err := m.Validate(15)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "requires 16 or newer")
	})

	t.Run("refuses an unknown artifact version", func(t *testing.T) {
		m := validManifest()
		m.ArtifactVersion = ArtifactVersion + 1

		err := m.Validate(16)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "upgrade the restore module")
	})
}

func TestManifestRoundTrip(t *testing.T) {
	m := validManifest()
	m.Tables = []TableEntry{{
		Schema: "public", Name: "users",
		Columns:    []string{"id", "email"},
		Transforms: map[string]string{"email": "email"},
		RowCount:   42,
	}}
	m.Sequences = []Sequence{{
		Schema: "public", Name: "users_id_seq",
		Source: SourceMaxColumn, LastValue: 42, IsCalled: true,
	}}

	b, err := m.Marshal()
	require.NoError(t, err)

	got, err := ParseManifest(b)
	require.NoError(t, err)

	assert.Equal(t, m.Tables, got.Tables)
	assert.Equal(t, m.Sequences, got.Sequences)
	assert.Equal(t, int64(42), got.TotalRows())
}

func TestColumnsAdded(t *testing.T) {
	previous := validManifest()
	previous.Tables = []TableEntry{
		{Schema: "public", Name: "users", Columns: []string{"id", "email"}},
	}

	current := validManifest()
	current.Tables = []TableEntry{
		{Schema: "public", Name: "users", Columns: []string{"id", "email", "referral_code"}},
		{Schema: "public", Name: "orders", Columns: []string{"id"}},
	}

	// Reported, never blocked -- the user decides what is sensitive
	assert.Equal(t, []string{"public.orders.id", "public.users.referral_code"},
		current.ColumnsAdded(&previous))

	t.Run("first snapshot has nothing to compare against", func(t *testing.T) {
		assert.Nil(t, current.ColumnsAdded(nil))
	})

	t.Run("no drift", func(t *testing.T) {
		assert.Empty(t, previous.ColumnsAdded(&previous))
	})
}

// The column list comes from the manifest rather than the target's catalog, so that a migration
// which inserted a column mid-table cannot shift values into the wrong columns
func TestTableEntryCopyIn(t *testing.T) {
	e := TableEntry{Schema: "public", Name: "users", Columns: []string{"id", "email"}}
	assert.Equal(t, `COPY "public"."users" ("id", "email") FROM STDIN`, e.CopyIn())
}

func TestSetvalSql(t *testing.T) {
	s := Sequence{Schema: "public", Name: "users_id_seq", LastValue: 42, IsCalled: true}
	assert.Equal(t, `SELECT pg_catalog.setval('"public"."users_id_seq"', 42, true)`, s.SetvalSql())
}
