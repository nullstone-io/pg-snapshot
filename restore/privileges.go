package restore

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lib/pq"
	"github.com/nullstone-io/pg-snapshot/pg"
)

// Privileges carries an environment's in-database grants across the swap.
//
// The snapshot is dumped and restored --no-acl so production's role names never enter the
// artifact, which leaves the staging database with no grants beyond what ownership confers.
// Carryover handles the ACLs that live on the database's own catalog row; everything *inside* the
// database -- schema USAGE, table SELECT, column grants, EXECUTE, default privileges -- is what
// this carries. Without it every other consumer of the environment can still connect after the
// swap and is then denied every schema and every table.
//
// What is carried is read from the target database on the same instance, never from the artifact.
// That keeps the security property intact, and it means every grantee already exists here.
//
// Grants are copied, revocations are not. A privilege postgres grants by default -- EXECUTE on a
// function to PUBLIC, USAGE on a type to PUBLIC -- that the target had revoked comes back in the
// restored environment. That is the permissive direction to be wrong in for a lower environment,
// and the alternative is diffing against built-in defaults that vary by object and version.
type Privileges struct {
	Log *slog.Logger
}

func (p Privileges) log() *slog.Logger {
	if p.Log == nil {
		return slog.Default()
	}
	return p.Log
}

// Carry replays the grants defined in `from` onto `to`.
//
// Run it after the migration step, so every object staging will have exists, and before the swap,
// so a failure leaves the target untouched.
//
// Two rules decide what an object in staging receives:
//
//   - An object that exists in both gets exactly the target's ACL on it, entry for entry.
//   - An object that exists only in staging -- a migration created it -- gets what the target's
//     default privileges would have given it at creation. Default privileges are keyed on the
//     creating role and schema, and the restore creates everything as the owner, so the rule the
//     target recorded for that owner and schema is the one that applies.
//
// The default privileges themselves are carried too, so objects created after the restore keep
// working without anyone re-running the grants.
func (p Privileges) Carry(ctx context.Context, from, to *pgxpool.Pool) error {
	plan, err := p.Plan(ctx, from, to)
	if err != nil {
		return err
	}
	if len(plan.Skipped) > 0 {
		p.log().Warn("target grants on objects the restored schema does not have were not carried",
			"objects", plan.Skipped,
			"reason", "the objects exist in the target but not in the restored database; a "+
				"migration most likely dropped them")
	}
	if len(plan.Statements) < 1 {
		p.log().Info("no privileges to carry")
		return nil
	}

	info, err := readConnInfo(ctx, to)
	if err != nil {
		return err
	}
	// Granting on an object requires owning it (or holding the privilege WITH GRANT OPTION), and
	// ALTER DEFAULT PRIVILEGES FOR ROLE requires membership in that role. A superuser needs neither.
	if !info.superuser {
		// A non-superuser cannot be granted membership in a superuser, on any platform: that is
		// what every managed service's reserved role is (rdsadmin, cloudsqladmin, azuresu), and
		// RDS only adds a friendlier error. Grants on what such a role owns are provably out of
		// reach, so they are skipped rather than failing the restore.
		reserved, err := superusersAmong(ctx, to, plan.Owners)
		if err != nil {
			return err
		}
		plan = p.dropUnreachable(plan, reserved, info.user+" is not a superuser and cannot act for one")

		held, err := borrowRoles(ctx, to, info.user, plan.Owners, p.log())
		defer held.release(ctx)
		if err != nil {
			return fmt.Errorf("%s could not acquire the privileges needed to carry grants: %w",
				info.user, err)
		}
		// The catalog check above is the explanation; postgres refusing the grant is the proof. A
		// platform that reserves a role some other way lands here, and the outcome is the same.
		if len(held.unavailable) > 0 {
			refused := map[string]bool{}
			for _, role := range held.unavailable {
				refused[role] = true
			}
			plan = p.dropUnreachable(plan, refused, "postgres refused to grant the role to "+info.user)
		}
	}

	for _, s := range plan.Statements {
		if _, err := to.Exec(ctx, s.SQL); err != nil {
			return fmt.Errorf("error carrying privileges: %s: %w", s.SQL, err)
		}
	}

	p.log().Info("privileges carried", "statements", len(plan.Statements), "skipped", len(plan.Skipped))
	return nil
}

// PrivilegePlan is every statement a carry will run, worked out before any of them does.
type PrivilegePlan struct {
	Statements []Statement

	// Owners are the roles whose privileges the statements need: the owners of every object being
	// granted on, and the role of every default-privilege rule
	Owners []string

	// Skipped are target objects carrying grants that staging does not have
	Skipped []string
}

// Statement is one GRANT or ALTER DEFAULT PRIVILEGES, with the role whose privileges running it
// needs and the object it is for.
type Statement struct {
	SQL    string
	Owner  string
	Object string
}

// dropUnreachable removes the statements that need any of the given roles and says which objects
// lost their grants because of it. Grants on what such a role owns are provably out of reach, so
// they are skipped rather than failing the restore.
func (p Privileges) dropUnreachable(plan PrivilegePlan, roles map[string]bool, reason string) PrivilegePlan {
	kept, unreachable := plan.without(roles)
	if len(unreachable) > 0 {
		p.log().Warn("grants on objects owned by a role the restore cannot act for were not carried",
			"owners", sortedKeys(roles), "objects", unreachable, "reason", reason)
	}
	return kept
}

// without drops every statement that needs one of the given roles, reporting the objects that
// lost their grants as a result.
func (p PrivilegePlan) without(roles map[string]bool) (PrivilegePlan, []string) {
	if len(roles) < 1 {
		return p, nil
	}
	kept := PrivilegePlan{Skipped: p.Skipped}
	owners := map[string]bool{}
	dropped := map[string]bool{}
	for _, s := range p.Statements {
		if roles[s.Owner] {
			dropped[s.Object] = true
			continue
		}
		kept.Statements = append(kept.Statements, s)
		owners[s.Owner] = true
	}
	kept.Owners = sortedKeys(owners)
	return kept, sortedKeys(dropped)
}

// superusersAmong reports which of the roles are superusers.
func superusersAmong(ctx context.Context, db pg.Querier, roles []string) (map[string]bool, error) {
	out := map[string]bool{}
	if len(roles) < 1 {
		return out, nil
	}
	rows, err := db.Query(ctx,
		`SELECT rolname FROM pg_catalog.pg_roles WHERE rolsuper AND rolname = ANY($1)`, roles)
	if err != nil {
		return nil, fmt.Errorf("error reading role attributes: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("error reading role attributes: %w", err)
		}
		out[name] = true
	}
	return out, rows.Err()
}

// Plan reads both catalogs and renders the statements Carry would run, without running them.
func (p Privileges) Plan(ctx context.Context, from, to pg.Querier) (PrivilegePlan, error) {
	var plan PrivilegePlan

	source, err := readObjects(ctx, from)
	if err != nil {
		return plan, fmt.Errorf("error reading privileges of the target: %w", err)
	}
	dest, err := readObjects(ctx, to)
	if err != nil {
		return plan, fmt.Errorf("error reading the restored database: %w", err)
	}

	owners := map[string]bool{}
	skipped := make([]string, 0)
	// Resolved once here rather than in every statement: pg_database_owner owns the public schema
	// in every database and cannot be granted to anyone, so membership has to be borrowed from the
	// role it currently stands for
	resolve := func(role string) string {
		if role == "pg_database_owner" {
			return dest.DatabaseOwner
		}
		return role
	}

	add := func(owner, object string, statements []string) {
		owner = resolve(owner)
		for _, sq := range statements {
			plan.Statements = append(plan.Statements, Statement{SQL: sq, Owner: owner, Object: object})
			owners[owner] = true
		}
	}

	// Schemas first: nothing else can be named until they can be looked up
	for _, s := range source.orderedSchemas() {
		if len(s.Acl) < 1 {
			continue
		}
		d, ok := dest.Schemas[s.Name]
		if !ok {
			skipped = append(skipped, "schema "+s.Name)
			continue
		}
		add(d.Owner, "schema "+s.Name,
			grantStatements(s.Acl, d.Owner, schemaPrivs, "", "SCHEMA "+quoteName(s.Name, "")))
	}

	// Default privileges next, in both of their forms: the rule itself, so objects created after the
	// restore are covered, and the rule applied by hand to the objects a migration already created
	// under it
	for _, d := range source.Defaults {
		kind, ok := defaultKinds[d.ObjType]
		if !ok {
			continue
		}
		created := dest.createdBy(d.Role, d.Schema, d.ObjType, source)
		for _, entry := range parseAcl(d.Acl) {
			if entry.Grantee == d.Role {
				continue
			}
			for _, g := range splitGrants(entry, kind.privs, "") {
				rule := "default privileges of " + d.Role
				if d.Schema != "" {
					rule += " in " + d.Schema
				}
				statements := []string{g.defaultSql(d.Role, d.Schema, kind.plural)}
				for _, object := range created {
					statements = append(statements, g.sql(kind.singular+" "+object))
				}
				add(d.Role, rule, statements)
			}
		}
	}

	for _, r := range source.orderedRelations() {
		if len(r.Acl) < 1 {
			continue
		}
		d, ok := dest.Relations[r.Qualified()]
		if !ok {
			skipped = append(skipped, relationDesc(r.Kind)+" "+r.Qualified())
			continue
		}
		on := relationKeyword(d.Kind) + " " + quoteName(r.Schema, r.Name)
		add(d.Owner, relationDesc(d.Kind)+" "+r.Qualified(),
			grantStatements(r.Acl, d.Owner, relationPrivs(d.Kind), "", on))
	}

	for _, c := range source.Columns {
		d, ok := dest.Relations[c.Qualified()]
		if !ok || !d.Columns[c.Column] {
			skipped = append(skipped, "column "+c.Qualified()+"."+c.Column)
			continue
		}
		on := "TABLE " + quoteName(c.Schema, c.Name)
		add(d.Owner, "column "+c.Qualified()+"."+c.Column,
			grantStatements(c.Acl, d.Owner, columnPrivs, c.Column, on))
	}

	for _, r := range source.orderedRoutines() {
		if len(r.Acl) < 1 {
			continue
		}
		d, ok := dest.Routines[r.Signature()]
		if !ok {
			skipped = append(skipped, "routine "+r.Signature())
			continue
		}
		on := "ROUTINE " + quoteName(r.Schema, r.Name) + "(" + r.Args + ")"
		add(d.Owner, "routine "+r.Signature(),
			grantStatements(r.Acl, d.Owner, routinePrivs, "", on))
	}

	plan.Owners = sortedKeys(owners)
	plan.Skipped = skipped
	return plan, nil
}

// Privilege characters as they appear in an aclitem, per object type.
//
// A character a map does not list belongs to a different object type and is ignored rather than
// guessed at, which is also what keeps a newer server's additions from breaking the parse.
var (
	schemaPrivs   = map[byte]string{'U': "USAGE", 'C': "CREATE"}
	tablePrivs    = map[byte]string{'r': "SELECT", 'w': "UPDATE", 'a': "INSERT", 'd': "DELETE", 'D': "TRUNCATE", 'x': "REFERENCES", 't': "TRIGGER", 'm': "MAINTAIN"}
	sequencePrivs = map[byte]string{'r': "SELECT", 'w': "UPDATE", 'U': "USAGE"}
	columnPrivs   = map[byte]string{'r': "SELECT", 'w': "UPDATE", 'a': "INSERT", 'x': "REFERENCES"}
	routinePrivs  = map[byte]string{'X': "EXECUTE"}
	typePrivs     = map[byte]string{'U': "USAGE"}
)

// defaultKinds maps pg_default_acl.defaclobjtype onto the words ALTER DEFAULT PRIVILEGES and
// GRANT use for it.
var defaultKinds = map[string]struct {
	plural, singular string
	privs            map[byte]string
}{
	"r": {"TABLES", "TABLE", tablePrivs},
	"S": {"SEQUENCES", "SEQUENCE", sequencePrivs},
	"f": {"FUNCTIONS", "ROUTINE", routinePrivs},
	"T": {"TYPES", "TYPE", typePrivs},
	"n": {"SCHEMAS", "SCHEMA", schemaPrivs},
}

func relationPrivs(kind string) map[byte]string {
	if kind == "S" {
		return sequencePrivs
	}
	return tablePrivs
}

// relationKeyword is the word GRANT takes for a relation kind. Views, materialized views and
// foreign tables are all TABLE to GRANT; only sequences differ.
func relationKeyword(kind string) string {
	if kind == "S" {
		return "SEQUENCE"
	}
	return "TABLE"
}

func relationDesc(kind string) string {
	switch kind {
	case "S":
		return "sequence"
	case "v":
		return "view"
	case "m":
		return "materialized view"
	case "f":
		return "foreign table"
	}
	return "table"
}

// aclEntry is one grantee's privileges on one object, parsed out of an aclitem.
type aclEntry struct {
	// Grantee is the role name, or "" for PUBLIC
	Grantee string
	Privs   []aclPriv
}

type aclPriv struct {
	Char byte
	// Option is WITH GRANT OPTION, marked in an aclitem by a "*" after the privilege
	Option bool
}

// parseAcl reads every item of an ACL, dropping the ones that cannot be read.
func parseAcl(items []string) []aclEntry {
	out := make([]aclEntry, 0, len(items))
	for _, item := range items {
		if entry, ok := parseAclItem(item); ok {
			out = append(out, entry)
		}
	}
	return out
}

// parseAclItem reads "grantee=privs/grantor". The grantor is who made the grant in the target,
// and is not carried: the restore makes every grant as the object's owner.
func parseAclItem(item string) (aclEntry, bool) {
	grantee, rest, ok := splitAclGrantee(item)
	if !ok {
		return aclEntry{}, false
	}
	chars, _, _ := strings.Cut(rest, "/")

	entry := aclEntry{Grantee: grantee}
	for i := 0; i < len(chars); i++ {
		if chars[i] == '*' {
			if n := len(entry.Privs); n > 0 {
				entry.Privs[n-1].Option = true
			}
			continue
		}
		entry.Privs = append(entry.Privs, aclPriv{Char: chars[i]})
	}
	return entry, true
}

// grant is one GRANT statement's worth of privileges to one grantee.
type grant struct {
	Grantee string
	// Privileges are rendered, e.g. "SELECT" or "SELECT (id)"
	Privileges []string
	Option     bool
}

// splitGrants maps an entry onto SQL privilege names, in up to two groups: the privileges held
// WITH GRANT OPTION and those held without. Postgres cannot express the option per privilege in
// one statement, so they cannot share one. column, when set, renders column-level privileges.
func splitGrants(entry aclEntry, names map[byte]string, column string) []grant {
	plain := grant{Grantee: entry.Grantee}
	option := grant{Grantee: entry.Grantee, Option: true}
	for _, p := range entry.Privs {
		name, ok := names[p.Char]
		if !ok {
			continue
		}
		if column != "" {
			name += " (" + pq.QuoteIdentifier(column) + ")"
		}
		if p.Option {
			option.Privileges = append(option.Privileges, name)
		} else {
			plain.Privileges = append(plain.Privileges, name)
		}
	}

	out := make([]grant, 0, 2)
	if len(plain.Privileges) > 0 {
		out = append(out, plain)
	}
	if len(option.Privileges) > 0 {
		out = append(out, option)
	}
	return out
}

// grantStatements renders every GRANT an object's ACL amounts to, on the object named by `on`.
//
// The owner's own entry is skipped: ownership confers everything, and the entry only exists
// because postgres materialises it the moment any other grant is made.
func grantStatements(acl []string, owner string, names map[byte]string, column, on string) []string {
	out := make([]string, 0)
	for _, entry := range parseAcl(acl) {
		if entry.Grantee == owner {
			continue
		}
		for _, g := range splitGrants(entry, names, column) {
			out = append(out, g.sql(on))
		}
	}
	return out
}

func (g grant) target() string {
	if g.Grantee == "" {
		return "PUBLIC"
	}
	return pq.QuoteIdentifier(g.Grantee)
}

func (g grant) sql(on string) string {
	sq := fmt.Sprintf("GRANT %s ON %s TO %s", strings.Join(g.Privileges, ", "), on, g.target())
	if g.Option {
		sq += " WITH GRANT OPTION"
	}
	return sq
}

// defaultSql renders the grant as a default-privilege rule. schema is "" for a global rule.
func (g grant) defaultSql(role, schema, objects string) string {
	sq := "ALTER DEFAULT PRIVILEGES FOR ROLE " + pq.QuoteIdentifier(role)
	if schema != "" {
		sq += " IN SCHEMA " + pq.QuoteIdentifier(schema)
	}
	sq += fmt.Sprintf(" GRANT %s ON %s TO %s", strings.Join(g.Privileges, ", "), objects, g.target())
	if g.Option {
		sq += " WITH GRANT OPTION"
	}
	return sq
}

func quoteName(schema, name string) string {
	if name == "" {
		return pq.QuoteIdentifier(schema)
	}
	return pq.QuoteIdentifier(schema) + "." + pq.QuoteIdentifier(name)
}

// objectSet is one database's grantable objects, with the ACLs on them.
//
// Every object is read, not only those with an ACL: the set is also what decides whether an object
// exists in the other database, and which of staging's objects a migration created.
type objectSet struct {
	DatabaseOwner string
	Schemas       map[string]schemaObject
	Relations     map[string]relationObject
	Routines      map[string]routineObject
	Columns       []columnObject
	Defaults      []defaultAcl

	// Ordered views of the maps, so a plan is rendered in a stable order
	schemaNames   []string
	relationNames []string
	routineNames  []string
}

type schemaObject struct {
	Name  string
	Owner string
	Acl   []string
}

type relationObject struct {
	Schema, Name string
	// Kind is pg_class.relkind
	Kind    string
	Owner   string
	Acl     []string
	Columns map[string]bool
}

func (r relationObject) Qualified() string { return r.Schema + "." + r.Name }

type columnObject struct {
	Schema, Name, Column string
	Acl                  []string
}

func (c columnObject) Qualified() string { return c.Schema + "." + c.Name }

type routineObject struct {
	Schema, Name string
	// Args is pg_get_function_identity_arguments, which is what names an overload
	Args  string
	Owner string
	Acl   []string
}

func (r routineObject) Signature() string { return r.Schema + "." + r.Name + "(" + r.Args + ")" }

type defaultAcl struct {
	Role string
	// Schema is "" for a rule that applies in every schema
	Schema string
	// ObjType is pg_default_acl.defaclobjtype
	ObjType string
	Acl     []string
}

// iterators over the ordered views, so callers range over slices rather than maps
func (s objectSet) orderedSchemas() []schemaObject {
	out := make([]schemaObject, 0, len(s.schemaNames))
	for _, name := range s.schemaNames {
		out = append(out, s.Schemas[name])
	}
	return out
}

func (s objectSet) orderedRelations() []relationObject {
	out := make([]relationObject, 0, len(s.relationNames))
	for _, name := range s.relationNames {
		out = append(out, s.Relations[name])
	}
	return out
}

func (s objectSet) orderedRoutines() []routineObject {
	out := make([]routineObject, 0, len(s.routineNames))
	for _, name := range s.routineNames {
		out = append(out, s.Routines[name])
	}
	return out
}

// createdBy lists the objects in this set that a default-privilege rule would have applied to at
// creation and that `other` does not have -- the ones a migration created, which the target's
// explicit ACLs cannot say anything about.
//
// Objects present in both are left to the entry-for-entry copy: applying the rule to them as well
// would widen a deliberately narrower grant, a column-level SELECT for instance, into a table-level
// one.
func (s objectSet) createdBy(role, schema, objType string, other objectSet) []string {
	out := make([]string, 0)
	inSchema := func(name string) bool { return schema == "" || name == schema }

	switch objType {
	case "r", "S":
		for _, r := range s.orderedRelations() {
			if r.Owner != role || !inSchema(r.Schema) {
				continue
			}
			if (objType == "S") != (r.Kind == "S") {
				continue
			}
			if _, exists := other.Relations[r.Qualified()]; exists {
				continue
			}
			out = append(out, quoteName(r.Schema, r.Name))
		}
	case "f":
		for _, r := range s.orderedRoutines() {
			if r.Owner != role || !inSchema(r.Schema) {
				continue
			}
			if _, exists := other.Routines[r.Signature()]; exists {
				continue
			}
			out = append(out, quoteName(r.Schema, r.Name)+"("+r.Args+")")
		}
	case "n":
		for _, sch := range s.orderedSchemas() {
			if sch.Owner != role {
				continue
			}
			if _, exists := other.Schemas[sch.Name]; exists {
				continue
			}
			out = append(out, quoteName(sch.Name, ""))
		}
	}
	// "T" (types) is carried as a rule only; the restore does not enumerate types
	return out
}

// The queries below leave out postgres' own schemas. Nothing in them is restored, so nothing in
// them can be missing a grant.
const (
	userSchemas = `n.nspname NOT LIKE 'pg\_%' AND n.nspname <> 'information_schema'`

	schemasSql = `
SELECT n.nspname, pg_catalog.pg_get_userbyid(n.nspowner), COALESCE(n.nspacl::text[], '{}')
FROM pg_catalog.pg_namespace n
WHERE ` + userSchemas + `
ORDER BY n.nspname`

	relationsSql = `
SELECT n.nspname, c.relname, c.relkind::text, pg_catalog.pg_get_userbyid(c.relowner),
       COALESCE(c.relacl::text[], '{}')
FROM pg_catalog.pg_class c
JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
WHERE c.relkind IN ('r', 'p', 'v', 'm', 'S', 'f') AND ` + userSchemas + `
ORDER BY n.nspname, c.relname`

	columnsSql = `
SELECT n.nspname, c.relname, a.attname, COALESCE(a.attacl::text[], '{}')
FROM pg_catalog.pg_attribute a
JOIN pg_catalog.pg_class c ON c.oid = a.attrelid
JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
WHERE a.attnum > 0 AND NOT a.attisdropped
  AND c.relkind IN ('r', 'p', 'v', 'm', 'f') AND ` + userSchemas + `
ORDER BY n.nspname, c.relname, a.attnum`

	routinesSql = `
SELECT n.nspname, p.proname, pg_catalog.pg_get_function_identity_arguments(p.oid),
       pg_catalog.pg_get_userbyid(p.proowner), COALESCE(p.proacl::text[], '{}')
FROM pg_catalog.pg_proc p
JOIN pg_catalog.pg_namespace n ON n.oid = p.pronamespace
WHERE ` + userSchemas + `
ORDER BY n.nspname, p.proname, 3`

	defaultsSql = `
SELECT pg_catalog.pg_get_userbyid(d.defaclrole), COALESCE(n.nspname, ''), d.defaclobjtype::text,
       COALESCE(d.defaclacl::text[], '{}')
FROM pg_catalog.pg_default_acl d
LEFT JOIN pg_catalog.pg_namespace n ON n.oid = d.defaclnamespace
ORDER BY 1, 2, 3`
)

// readObjects reads a database's grantable objects and the ACLs on them.
func readObjects(ctx context.Context, db pg.Querier) (objectSet, error) {
	set := objectSet{
		Schemas:   map[string]schemaObject{},
		Relations: map[string]relationObject{},
		Routines:  map[string]routineObject{},
	}

	var err error
	if set.DatabaseOwner, err = readDatabaseOwner(ctx, db); err != nil {
		return set, err
	}

	if err := scanRows(ctx, db, schemasSql, func(rows scanner) error {
		var s schemaObject
		if err := rows.Scan(&s.Name, &s.Owner, &s.Acl); err != nil {
			return err
		}
		set.Schemas[s.Name] = s
		set.schemaNames = append(set.schemaNames, s.Name)
		return nil
	}); err != nil {
		return set, fmt.Errorf("error reading schemas: %w", err)
	}

	if err := scanRows(ctx, db, relationsSql, func(rows scanner) error {
		r := relationObject{Columns: map[string]bool{}}
		if err := rows.Scan(&r.Schema, &r.Name, &r.Kind, &r.Owner, &r.Acl); err != nil {
			return err
		}
		set.Relations[r.Qualified()] = r
		set.relationNames = append(set.relationNames, r.Qualified())
		return nil
	}); err != nil {
		return set, fmt.Errorf("error reading relations: %w", err)
	}

	if err := scanRows(ctx, db, columnsSql, func(rows scanner) error {
		var c columnObject
		if err := rows.Scan(&c.Schema, &c.Name, &c.Column, &c.Acl); err != nil {
			return err
		}
		if r, ok := set.Relations[c.Qualified()]; ok {
			r.Columns[c.Column] = true
		}
		if len(c.Acl) > 0 {
			set.Columns = append(set.Columns, c)
		}
		return nil
	}); err != nil {
		return set, fmt.Errorf("error reading columns: %w", err)
	}

	if err := scanRows(ctx, db, routinesSql, func(rows scanner) error {
		var r routineObject
		if err := rows.Scan(&r.Schema, &r.Name, &r.Args, &r.Owner, &r.Acl); err != nil {
			return err
		}
		set.Routines[r.Signature()] = r
		set.routineNames = append(set.routineNames, r.Signature())
		return nil
	}); err != nil {
		return set, fmt.Errorf("error reading routines: %w", err)
	}

	if err := scanRows(ctx, db, defaultsSql, func(rows scanner) error {
		var d defaultAcl
		if err := rows.Scan(&d.Role, &d.Schema, &d.ObjType, &d.Acl); err != nil {
			return err
		}
		set.Defaults = append(set.Defaults, d)
		return nil
	}); err != nil {
		return set, fmt.Errorf("error reading default privileges: %w", err)
	}

	return set, nil
}

// scanner is the one method of pgx.Rows a row callback needs.
type scanner interface {
	Scan(dest ...any) error
}

// scanRows runs a query and hands each row to fn, closing the rows whatever happens.
func scanRows(ctx context.Context, db pg.Querier, sql string, fn func(scanner) error) error {
	rows, err := db.Query(ctx, sql)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err := fn(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}

// readDatabaseOwner reports who owns the database the session is connected to.
func readDatabaseOwner(ctx context.Context, db pg.Querier) (string, error) {
	var owner string
	if err := db.QueryRow(ctx,
		`SELECT pg_catalog.pg_get_userbyid(datdba) FROM pg_catalog.pg_database WHERE datname = current_database()`,
	).Scan(&owner); err != nil {
		return "", fmt.Errorf("error reading the database owner: %w", err)
	}
	return owner, nil
}
