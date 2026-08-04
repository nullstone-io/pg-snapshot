package scrub

import (
	"testing"

	"github.com/nullstone-io/pg-snapshot/catalog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testSalt = "s4lt"

func textCol(name string) catalog.Column {
	return catalog.Column{Name: name, TypeName: "text", BaseType: "text", MaxLen: -1}
}

func varcharCol(name string, n int) catalog.Column {
	return catalog.Column{
		Name:     name,
		TypeName: "character varying(" + itoa(n) + ")",
		BaseType: "character varying",
		MaxLen:   n,
	}
}

func intCol(name string) catalog.Column {
	return catalog.Column{Name: name, TypeName: "integer", BaseType: "integer", MaxLen: -1}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for ; n > 0; n /= 10 {
		b = append([]byte{byte('0' + n%10)}, b...)
	}
	return string(b)
}

func usersTable(cols ...catalog.Column) catalog.Table {
	return catalog.Table{Schema: "public", Name: "users", Kind: catalog.RelKindTable, Columns: cols}
}

func TestBuildProjection(t *testing.T) {
	t.Run("no config exports every column as-is", func(t *testing.T) {
		tbl := usersTable(intCol("id"), textCol("email"))

		p, err := BuildProjection(tbl, Config{Version: 1}, testSalt)
		require.NoError(t, err)

		assert.Equal(t, []string{"id", "email"}, p.Columns)
		assert.Empty(t, p.Transforms)
		assert.Equal(t, `SELECT "id", "email" FROM "public"."users"`, p.SelectSQL)
	})

	// COPY refuses generated columns; postgres recomputes them on load
	t.Run("excludes generated columns", func(t *testing.T) {
		gen := textCol("search_vector")
		gen.Generated = true
		tbl := usersTable(intCol("id"), textCol("email"), gen)

		p, err := BuildProjection(tbl, Config{Version: 1}, testSalt)
		require.NoError(t, err)

		assert.Equal(t, []string{"id", "email"}, p.Columns)
		assert.NotContains(t, p.SelectSQL, "search_vector")
	})

	// COPY FROM is exempt from the GENERATED ALWAYS restriction that blocks INSERT
	t.Run("includes identity columns", func(t *testing.T) {
		id := intCol("id")
		id.Identity = true
		p, err := BuildProjection(usersTable(id), Config{Version: 1}, testSalt)
		require.NoError(t, err)
		assert.Equal(t, []string{"id"}, p.Columns)
	})

	t.Run("skip mode exports no rows", func(t *testing.T) {
		cfg := Config{Version: 1, Tables: map[string]TableConfig{
			"public.users": {Mode: TableModeSkip},
		}}

		p, err := BuildProjection(usersTable(intCol("id")), cfg, testSalt)
		require.NoError(t, err)

		assert.True(t, p.Skipped)
		assert.Empty(t, p.SelectSQL)
	})

	t.Run("appends where clause", func(t *testing.T) {
		cfg := Config{Version: 1, Tables: map[string]TableConfig{
			"public.users": {Where: "created_at > now() - interval '30 days'"},
		}}

		p, err := BuildProjection(usersTable(intCol("id")), cfg, testSalt)
		require.NoError(t, err)
		assert.Equal(t,
			`SELECT "id" FROM "public"."users" WHERE created_at > now() - interval '30 days'`,
			p.SelectSQL)
	})

	t.Run("partitioned parent is never exported directly", func(t *testing.T) {
		tbl := catalog.Table{Schema: "public", Name: "events", Kind: catalog.RelKindPartitioned}
		_, err := BuildProjection(tbl, Config{Version: 1}, testSalt)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "leaf partitions")
	})
}

func TestBuildProjectionTransforms(t *testing.T) {
	build := func(t *testing.T, col catalog.Column, transform string) (*Projection, error) {
		t.Helper()
		cfg := Config{Version: 1, Tables: map[string]TableConfig{
			"public.users": {Columns: map[string]string{col.Name: transform}},
		}}
		return BuildProjection(usersTable(col), cfg, testSalt)
	}

	t.Run("null", func(t *testing.T) {
		p, err := build(t, textCol("ssn"), "null")
		require.NoError(t, err)
		assert.Equal(t, `SELECT NULL AS "ssn" FROM "public"."users"`, p.SelectSQL)
	})

	// Caught here rather than as a NOT NULL violation partway through the load
	t.Run("null on a NOT NULL column is rejected", func(t *testing.T) {
		col := textCol("ssn")
		col.NotNull = true
		_, err := build(t, col, "null")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "NOT NULL")
	})

	t.Run("md5 on unbounded text", func(t *testing.T) {
		p, err := build(t, textCol("last_name"), "md5")
		require.NoError(t, err)
		assert.Contains(t, p.SelectSQL, `(md5("last_name"::text || 's4lt'))::text`)
	})

	// A 32-character hash in a varchar(20) would fail the *restore*, an hour after the export
	t.Run("md5 truncates to fit a bounded column", func(t *testing.T) {
		p, err := build(t, varcharCol("last_name", 20), "md5")
		require.NoError(t, err)
		assert.Contains(t, p.SelectSQL,
			`(left(md5("last_name"::text || 's4lt'), 20))::character varying(20)`)
	})

	t.Run("email on unbounded text uses the full hash", func(t *testing.T) {
		p, err := build(t, textCol("email"), "email")
		require.NoError(t, err)
		assert.Contains(t, p.SelectSQL,
			`('user_' || left(md5("email"::text || 's4lt'), 32) || '@example.invalid')::text`)
	})

	t.Run("email shrinks the hash to fit a bounded column", func(t *testing.T) {
		// 30 - len("user_") - len("@example.invalid") == 9
		p, err := build(t, varcharCol("email", 30), "email")
		require.NoError(t, err)
		assert.Contains(t, p.SelectSQL, `left(md5("email"::text || 's4lt'), 9)`)
	})

	t.Run("email rejects a column too short to hold one", func(t *testing.T) {
		_, err := build(t, varcharCol("email", 12), "email")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "too short for an email")
	})

	t.Run("redact", func(t *testing.T) {
		p, err := build(t, textCol("notes"), "redact")
		require.NoError(t, err)
		assert.Contains(t, p.SelectSQL, `('REDACTED')::text`)
	})

	t.Run("text transforms reject non-textual columns", func(t *testing.T) {
		_, err := build(t, intCol("account_id"), "md5")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "produces text but column")
	})

	t.Run("raw sql passes through untouched", func(t *testing.T) {
		p, err := build(t, intCol("age"), "42")
		require.NoError(t, err)
		assert.Equal(t, `SELECT (42) AS "age" FROM "public"."users"`, p.SelectSQL)
	})

	t.Run("builtin lookup is case-insensitive", func(t *testing.T) {
		p, err := build(t, textCol("ssn"), "NULL")
		require.NoError(t, err)
		assert.Contains(t, p.SelectSQL, `NULL AS "ssn"`)
	})

	// The manifest ships to the bucket, and the generated SQL embeds the run's salt
	t.Run("records the configured transform, never the generated sql", func(t *testing.T) {
		p, err := build(t, textCol("email"), "email")
		require.NoError(t, err)
		assert.Equal(t, map[string]string{"email": "email"}, p.Transforms)
		assert.NotContains(t, p.Transforms["email"], testSalt)
	})
}

func TestBuildProjectionRejectsStaleRules(t *testing.T) {
	// A rule naming a dropped column would silently not apply
	t.Run("rule on a column that does not exist", func(t *testing.T) {
		cfg := Config{Version: 1, Tables: map[string]TableConfig{
			"public.users": {Columns: map[string]string{"ssn": "null"}},
		}}
		_, err := BuildProjection(usersTable(intCol("id")), cfg, testSalt)
		require.Error(t, err)
		assert.Contains(t, err.Error(), `no column "ssn"`)
	})

	t.Run("rule on a generated column", func(t *testing.T) {
		gen := textCol("full_name")
		gen.Generated = true
		cfg := Config{Version: 1, Tables: map[string]TableConfig{
			"public.users": {Columns: map[string]string{"full_name": "redact"}},
		}}
		_, err := BuildProjection(usersTable(intCol("id"), gen), cfg, testSalt)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "generated columns are not exported")
	})
}

func TestCopyStatements(t *testing.T) {
	p, err := BuildProjection(usersTable(intCol("id"), textCol("email")), Config{Version: 1}, testSalt)
	require.NoError(t, err)

	assert.Equal(t, `COPY (SELECT "id", "email" FROM "public"."users") TO STDOUT`, p.CopyOut())
	assert.Equal(t, `COPY "public"."users" ("id", "email") FROM STDIN`, p.CopyIn())
}
