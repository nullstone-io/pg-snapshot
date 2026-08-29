package scrub

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParse(t *testing.T) {
	t.Run("full config", func(t *testing.T) {
		cfg, err := Parse([]byte(`
version: 1
fk_mode: not_valid
tables:
  public.users:
    columns:
      email: email
      ssn: "null"
  public.audit_logs:
    mode: skip
  public.legacy_imports:
    mode: skip-data
  public.events:
    where: "created_at > now() - interval '30 days'"
`))
		require.NoError(t, err)

		assert.Equal(t, FKModeNotValid, cfg.FKMode)
		assert.Len(t, cfg.Tables, 4)

		users, ok := cfg.TableConfigFor("public", "users")
		require.True(t, ok)
		assert.Equal(t, "email", users.Columns["email"])
		assert.Equal(t, TableModeFull, users.Mode)

		logs, _ := cfg.TableConfigFor("public", "audit_logs")
		assert.Equal(t, TableModeSkip, logs.Mode)
		assert.True(t, logs.Mode.SkipsData())

		imports, _ := cfg.TableConfigFor("public", "legacy_imports")
		assert.Equal(t, TableModeSkipData, imports.Mode)
		assert.True(t, imports.Mode.SkipsData())

		events, _ := cfg.TableConfigFor("public", "events")
		assert.Equal(t, "created_at > now() - interval '30 days'", events.Where)
	})

	t.Run("minimal config", func(t *testing.T) {
		cfg, err := Parse([]byte("version: 1\n"))
		require.NoError(t, err)
		assert.Equal(t, FKModeValidate, cfg.FKMode)
		assert.Empty(t, cfg.Tables)
	})

	// A misspelled key means a rule the user believes is in force is not
	t.Run("rejects unknown fields", func(t *testing.T) {
		_, err := Parse([]byte(`
version: 1
tables:
  public.users:
    colums:
      email: email
`))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "colums")
	})

	t.Run("rejects unsupported version", func(t *testing.T) {
		_, err := Parse([]byte("version: 2\n"))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "unsupported scrub config version 2")
	})

	t.Run("rejects missing version", func(t *testing.T) {
		_, err := Parse([]byte("tables: {}\n"))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "unsupported scrub config version 0")
	})
}

func TestConfigValidate(t *testing.T) {
	t.Run("rejects unqualified table name", func(t *testing.T) {
		err := Config{
			Version: 1,
			Tables:  map[string]TableConfig{"users": {}},
		}.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), `must be schema-qualified`)
	})

	t.Run("rejects invalid mode", func(t *testing.T) {
		err := Config{
			Version: 1,
			Tables:  map[string]TableConfig{"public.users": {Mode: "sample"}},
		}.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), `invalid mode "sample"`)
	})

	t.Run("rejects invalid fk_mode", func(t *testing.T) {
		err := Config{Version: 1, FKMode: "maybe"}.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), `invalid fk_mode "maybe"`)
	})

	// Rather than silently picking a winner between two settings that contradict each other
	t.Run("rejects rules that skip mode would ignore", func(t *testing.T) {
		err := Config{
			Version: 1,
			Tables: map[string]TableConfig{
				"public.users": {
					Mode:    TableModeSkip,
					Where:   "id > 0",
					Columns: map[string]string{"email": "email"},
				},
			},
		}.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "`where` has no effect with mode: skip")
		assert.Contains(t, err.Error(), "`columns` has no effect with mode: skip")
	})

	// Neither skip mode exports rows, so both make the same rules dead letters
	t.Run("rejects rules that skip-data mode would ignore", func(t *testing.T) {
		err := Config{
			Version: 1,
			Tables: map[string]TableConfig{
				"public.users": {
					Mode:    TableModeSkipData,
					Where:   "id > 0",
					Columns: map[string]string{"email": "email"},
				},
			},
		}.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "`where` has no effect with mode: skip-data")
		assert.Contains(t, err.Error(), "`columns` has no effect with mode: skip-data")
	})

	// The two modes differ only in whether the structure comes along, so an operator who reaches
	// for the wrong one has to be told the other exists
	t.Run("names both modes when one is misspelled", func(t *testing.T) {
		err := Config{
			Version: 1,
			Tables:  map[string]TableConfig{"public.users": {Mode: "skip_data"}},
		}.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), `invalid mode "skip_data"`)
		assert.Contains(t, err.Error(), "skip, skip-data")
	})

	t.Run("parses tail_rows", func(t *testing.T) {
		cfg, err := Parse([]byte(`
version: 1
fk_mode: not_valid
tables:
  public.activity_log:
    tail_rows: 2000
    tail_report_column: created_at
`))
		require.NoError(t, err)

		tc, ok := cfg.TableConfigFor("public", "activity_log")
		require.True(t, ok)
		require.NotNil(t, tc.TailRows)
		assert.Equal(t, int64(2000), *tc.TailRows)
		assert.Equal(t, "created_at", tc.TailReportColumn)
	})

	// An explicit zero is a rule that silently would not apply, which is the failure mode this
	// package exists to prevent -- hence the pointer in TableConfig
	t.Run("rejects tail_rows of zero", func(t *testing.T) {
		err := Config{
			Version: 1,
			Tables:  map[string]TableConfig{"public.activity_log": {TailRows: ptr(int64(0))}},
		}.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "tail_rows must be at least 1")
	})

	t.Run("rejects tail_rows with mode skip", func(t *testing.T) {
		err := Config{
			Version: 1,
			Tables:  map[string]TableConfig{"public.activity_log": {Mode: TableModeSkip, TailRows: ptr(int64(10))}},
		}.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "`tail_rows` has no effect with mode: skip")
	})

	t.Run("rejects tail_rows combined with where", func(t *testing.T) {
		err := Config{
			Version: 1,
			Tables: map[string]TableConfig{
				"public.activity_log": {Where: "id > 0", TailRows: ptr(int64(10))},
			},
		}.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "`tail_rows` and `where` cannot be combined")
	})

	t.Run("rejects tail_report_column without tail_rows", func(t *testing.T) {
		err := Config{
			Version: 1,
			Tables:  map[string]TableConfig{"public.activity_log": {TailReportColumn: "created_at"}},
		}.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "`tail_report_column` has no effect without tail_rows")
	})

	t.Run("rejects empty transform", func(t *testing.T) {
		err := Config{
			Version: 1,
			Tables:  map[string]TableConfig{"public.users": {Columns: map[string]string{"email": "  "}}},
		}.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "transform is empty")
	})
}

func ptr[T any](v T) *T { return &v }

func TestSplitQualified(t *testing.T) {
	schema, table, err := SplitQualified("public.users")
	require.NoError(t, err)
	assert.Equal(t, "public", schema)
	assert.Equal(t, "users", table)

	for _, bad := range []string{"users", "public.", ".users", "a.b.c", ""} {
		_, _, err := SplitQualified(bad)
		assert.Error(t, err, "expected %q to be rejected", bad)
	}
}
