// Package catalog holds the shape of the source database as read from the postgres catalogs.
//
// These types are shared by the scrub and export packages so that neither has to depend on the
// other: scrub turns a Table plus a user's configuration into a SELECT list, and export is what
// reads Tables out of a live database and runs the result.
package catalog

import "fmt"

type RelKind string

const (
	// RelKindTable is an ordinary table, and the only kind whose rows are exported directly
	RelKindTable RelKind = "r"
	// RelKindPartitioned is a partitioned parent. Its rows live in leaf partitions, so it is
	// never exported itself -- copying from the parent on load would route every row through
	// tuple routing instead of landing it directly in a leaf.
	RelKindPartitioned RelKind = "p"
)

type Table struct {
	Schema string
	Name   string
	Kind   RelKind
	Owner  string

	// Parent is set on a leaf partition, naming the partitioned table it belongs to
	Parent string

	// RowSecurity reports ALTER TABLE ... ENABLE ROW LEVEL SECURITY
	RowSecurity bool
	// ForceRowSecurity reports ALTER TABLE ... FORCE ROW LEVEL SECURITY, which subjects even the
	// table's owner to its policies. When this is set, no role membership can make the table
	// exportable -- only BYPASSRLS can, and no managed postgres offers it.
	ForceRowSecurity bool

	Columns []Column
}

func (t Table) Qualified() string {
	return fmt.Sprintf("%s.%s", t.Schema, t.Name)
}

// Column describes one attribute of a table.
type Column struct {
	Name string

	// TypeName is the fully specified type as format_type() renders it, e.g. "character varying(20)".
	// It carries any length modifier, so it can be used directly as a cast target.
	TypeName string

	// BaseType is TypeName with any modifier stripped, e.g. "character varying". Transforms use it
	// to decide whether a text-producing expression is legal for this column.
	BaseType string

	// MaxLen is the declared character limit, or -1 when the type is unbounded. Transforms that
	// produce text must fit inside it or the load fails on a value too long for the target column.
	MaxLen int

	// Generated reports a GENERATED ... STORED column. COPY refuses these outright, so they are
	// excluded from the export and recomputed on load -- which means a generated column derived
	// from a scrubbed column gets a correctly scrubbed value for free.
	Generated bool

	// Identity reports a GENERATED ... AS IDENTITY column. Unlike generated columns these are
	// exported normally: COPY FROM is exempt from the GENERATED ALWAYS restriction that blocks
	// INSERT, which is how pg_dump round-trips them.
	Identity bool

	NotNull bool
}

// IsTextual reports whether the column can hold the output of a text-producing transform.
func (c Column) IsTextual() bool {
	switch c.BaseType {
	case "text", "character varying", "character", "citext", "name":
		return true
	}
	return false
}
