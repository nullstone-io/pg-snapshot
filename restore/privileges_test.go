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

// A non-superuser cannot act for a superuser, and every managed platform's reserved role is one.
// The statements that need such a role have to be dropped as a unit, with the rest untouched.
func TestPlanWithout(t *testing.T) {
	plan := PrivilegePlan{
		Statements: []Statement{
			{SQL: "GRANT USAGE ON SCHEMA public TO reader", Owner: "rdsadmin", Object: "schema public"},
			{SQL: "GRANT SELECT ON TABLE orders TO reader", Owner: "app", Object: "table public.orders"},
			{SQL: "GRANT SELECT ON TABLE rds_tools TO reader", Owner: "rdsadmin", Object: "table public.rds_tools"},
		},
		Owners:  []string{"app", "rdsadmin"},
		Skipped: []string{"table public.dropped"},
	}

	kept, unreachable := plan.without(map[string]bool{"rdsadmin": true})
	assert.Equal(t, []Statement{plan.Statements[1]}, kept.Statements)
	assert.Equal(t, []string{"app"}, kept.Owners, "a role no statement needs must not be borrowed")
	assert.Equal(t, []string{"schema public", "table public.rds_tools"}, unreachable)
	assert.Equal(t, plan.Skipped, kept.Skipped)

	same, none := plan.without(nil)
	assert.Equal(t, plan, same)
	assert.Nil(t, none)
}

func TestDefaultRuleStatements(t *testing.T) {
	global := defaultAcl{Role: "app", Schema: "", ObjType: "r",
		Acl: []string{"app=arwdDxtm/app", "owner=arwdDxtm/app", "reader=r*/app"}}
	got := defaultRuleStatements(global)
	assert.Equal(t, []Statement{
		{SQL: `ALTER DEFAULT PRIVILEGES FOR ROLE "app" GRANT INSERT, SELECT, UPDATE, DELETE, TRUNCATE, REFERENCES, TRIGGER, MAINTAIN ON TABLES TO "owner"`,
			Owner: "app", Object: "default privileges of app"},
		{SQL: `ALTER DEFAULT PRIVILEGES FOR ROLE "app" GRANT SELECT ON TABLES TO "reader" WITH GRANT OPTION`,
			Owner: "app", Object: "default privileges of app"},
	}, got, "the role's own entry is skipped; the rule needs the role's membership to recreate")

	scoped := defaultAcl{Role: "app", Schema: "public", ObjType: "S", Acl: []string{"reader=U/app"}}
	assert.Equal(t, `ALTER DEFAULT PRIVILEGES FOR ROLE "app" IN SCHEMA "public" GRANT USAGE ON SEQUENCES TO "reader"`,
		defaultRuleStatements(scoped)[0].SQL)
	assert.Equal(t, "default privileges of app in public", defaultRuleStatements(scoped)[0].Object)

	assert.Empty(t, defaultRuleStatements(defaultAcl{Role: "app", ObjType: "?", Acl: []string{"reader=r/app"}}),
		"an object type this build does not know is ignored rather than guessed at")
}
