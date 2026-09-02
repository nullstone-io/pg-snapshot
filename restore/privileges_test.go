package restore

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestParseAclItem(t *testing.T) {
	tests := []struct {
		name string
		item string
		want aclEntry
		ok   bool
	}{
		{
			name: "plain",
			item: "reader=r/app",
			want: aclEntry{Grantee: "reader", Privs: []aclPriv{{Char: 'r'}}},
			ok:   true,
		},
		{
			name: "public grantee",
			item: "=U/pg_database_owner",
			want: aclEntry{Grantee: "", Privs: []aclPriv{{Char: 'U'}}},
			ok:   true,
		},
		{
			name: "grant option marks the privilege before it",
			item: "reader=r*w/app",
			want: aclEntry{Grantee: "reader", Privs: []aclPriv{{Char: 'r', Option: true}, {Char: 'w'}}},
			ok:   true,
		},
		{
			name: "quoted grantee with a hyphen",
			item: `"other-app"=arwd/app`,
			want: aclEntry{Grantee: "other-app", Privs: []aclPriv{{Char: 'a'}, {Char: 'r'}, {Char: 'w'}, {Char: 'd'}}},
			ok:   true,
		},
		{
			name: "not an aclitem",
			item: "garbage",
			ok:   false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseAclItem(tc.item)
			assert.Equal(t, tc.ok, ok)
			if ok {
				assert.Equal(t, tc.want, got)
			}
		})
	}
}

func TestGrantStatements(t *testing.T) {
	tests := []struct {
		name   string
		acl    []string
		owner  string
		names  map[byte]string
		column string
		on     string
		want   []string
	}{
		{
			name:  "owner entry is skipped, others rendered",
			acl:   []string{"app=arwdDxtm/app", "reader=r/app"},
			owner: "app",
			names: tablePrivs,
			on:    `TABLE "public"."orders"`,
			want:  []string{`GRANT SELECT ON TABLE "public"."orders" TO "reader"`},
		},
		{
			name:  "grant option splits into a second statement",
			acl:   []string{"reader=r*w/app"},
			owner: "app",
			names: tablePrivs,
			on:    `TABLE "public"."orders"`,
			want: []string{
				`GRANT UPDATE ON TABLE "public"."orders" TO "reader"`,
				`GRANT SELECT ON TABLE "public"."orders" TO "reader" WITH GRANT OPTION`,
			},
		},
		{
			name:  "public grantee",
			acl:   []string{"=U/pg_database_owner", "pg_database_owner=UC/pg_database_owner"},
			owner: "pg_database_owner",
			names: schemaPrivs,
			on:    `SCHEMA "public"`,
			want:  []string{`GRANT USAGE ON SCHEMA "public" TO PUBLIC`},
		},
		{
			name:  "characters the object type does not have are ignored",
			acl:   []string{"reader=rwU/app"},
			owner: "app",
			names: sequencePrivs,
			on:    `SEQUENCE "public"."orders_id_seq"`,
			want:  []string{`GRANT SELECT, UPDATE, USAGE ON SEQUENCE "public"."orders_id_seq" TO "reader"`},
		},
		{
			name:   "column privileges name the column",
			acl:    []string{"reader=rw/app"},
			owner:  "app",
			names:  columnPrivs,
			column: "email",
			on:     `TABLE "public"."users"`,
			want:   []string{`GRANT SELECT ("email"), UPDATE ("email") ON TABLE "public"."users" TO "reader"`},
		},
		{
			name:  "quoted grantee is re-quoted once",
			acl:   []string{`"other-app"=r/app`},
			owner: "app",
			names: tablePrivs,
			on:    `TABLE "public"."orders"`,
			want:  []string{`GRANT SELECT ON TABLE "public"."orders" TO "other-app"`},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, grantStatements(tc.acl, tc.owner, tc.names, tc.column, tc.on))
		})
	}
}

func TestDefaultSql(t *testing.T) {
	g := grant{Grantee: "reader", Privileges: []string{"SELECT"}}
	assert.Equal(t,
		`ALTER DEFAULT PRIVILEGES FOR ROLE "app" IN SCHEMA "public" GRANT SELECT ON TABLES TO "reader"`,
		g.defaultSql("app", "public", "TABLES"))
	assert.Equal(t,
		`ALTER DEFAULT PRIVILEGES FOR ROLE "app" GRANT SELECT ON TABLES TO "reader"`,
		g.defaultSql("app", "", "TABLES"),
		"a global rule has no IN SCHEMA")

	g.Option = true
	assert.Equal(t,
		`ALTER DEFAULT PRIVILEGES FOR ROLE "app" GRANT SELECT ON TABLES TO "reader" WITH GRANT OPTION`,
		g.defaultSql("app", "", "TABLES"))
}

// createdBy is what decides which of staging's objects a default-privilege rule is applied to by
// hand: only the ones the target does not have, so an existing object keeps exactly the ACL the
// target had on it.
func TestCreatedBy(t *testing.T) {
	staging := objectSet{
		Relations: map[string]relationObject{
			"public.orders":   {Schema: "public", Name: "orders", Kind: "r", Owner: "app"},
			"public.added":    {Schema: "public", Name: "added", Kind: "r", Owner: "app"},
			"public.added_id": {Schema: "public", Name: "added_id", Kind: "S", Owner: "app"},
			"public.foreign":  {Schema: "public", Name: "foreign", Kind: "r", Owner: "someone_else"},
			"other.added":     {Schema: "other", Name: "added", Kind: "r", Owner: "app"},
		},
		relationNames: []string{"public.orders", "public.added", "public.added_id", "public.foreign", "other.added"},
		Routines: map[string]routineObject{
			"public.f()": {Schema: "public", Name: "f", Owner: "app"},
		},
		routineNames: []string{"public.f()"},
	}
	target := objectSet{
		Relations: map[string]relationObject{"public.orders": {}},
	}

	assert.Equal(t, []string{`"public"."added"`},
		staging.createdBy("app", "public", "r", target),
		"only the table the target lacks, in the rule's schema, owned by the rule's role")
	assert.Equal(t, []string{`"public"."added_id"`},
		staging.createdBy("app", "public", "S", target))
	assert.Equal(t, []string{`"public"."added"`, `"other"."added"`},
		staging.createdBy("app", "", "r", target),
		"a global rule spans every schema")
	assert.Equal(t, []string{`"public"."f"()`},
		staging.createdBy("app", "public", "f", target))
	assert.Empty(t, staging.createdBy("app", "public", "T", target),
		"types are carried as a rule only")
}
