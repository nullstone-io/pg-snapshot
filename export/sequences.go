package export

import (
	"context"
	"fmt"

	"github.com/lib/pq"
)

// SequenceSource records how a sequence's value was obtained.
//
// This matters enough to record in the manifest: the two methods do not always agree, and
// knowing which was used explains an off-by-some sequence in a restored environment.
type SequenceSource string

const (
	// SourceCatalog read last_value from pg_sequences -- exact
	SourceCatalog SequenceSource = "pg_sequences"

	// SourceMaxColumn derived the value from max() of the column the sequence owns.
	//
	// Used when the role cannot read the sequence itself. pg-db-admin's table_privileges grants
	// SELECT ON ALL TABLES, which does not cover sequences, so this is the common path rather
	// than the exceptional one. It is accurate enough: the restored sequence resumes above every
	// row that was actually exported. It differs from the catalog value when production burned
	// sequence numbers on rolled-back inserts, or when rows were filtered out by a `where`.
	SourceMaxColumn SequenceSource = "max_column"

	// SourceUnavailable means neither method worked -- no read access and no owned column
	SourceUnavailable SequenceSource = "unavailable"
)

type Sequence struct {
	Schema string         `json:"schema"`
	Name   string         `json:"name"`
	Source SequenceSource `json:"source"`

	// LastValue and IsCalled replay directly into setval()
	LastValue int64 `json:"lastValue"`
	IsCalled  bool  `json:"isCalled"`

	// OwnedBy names the column the sequence backs, when it backs one
	OwnedBy string `json:"ownedBy,omitempty"`
}

func (s Sequence) Qualified() string {
	return fmt.Sprintf("%s.%s", s.Schema, s.Name)
}

// SetvalSql renders the statement that restores this sequence's position
func (s Sequence) SetvalSql() string {
	return fmt.Sprintf("SELECT pg_catalog.setval(%s, %d, %t)",
		pq.QuoteLiteral(fmt.Sprintf("%s.%s", pq.QuoteIdentifier(s.Schema), pq.QuoteIdentifier(s.Name))),
		s.LastValue, s.IsCalled)
}

// sequencesSql lists every sequence and the column it owns, if any.
//
// pg_depend with deptype 'a' is the ownership a serial column creates; 'i' is the internal
// dependency an identity column creates. Both point back at the table and attribute.
const sequencesSql = `
SELECT n.nspname,
       s.relname,
       COALESCE(tn.nspname, ''),
       COALESCE(t.relname, ''),
       COALESCE(a.attname, '')
FROM pg_catalog.pg_class s
JOIN pg_catalog.pg_namespace n ON n.oid = s.relnamespace
LEFT JOIN pg_catalog.pg_depend d
       ON d.classid = 'pg_class'::regclass AND d.objid = s.oid
      AND d.refclassid = 'pg_class'::regclass AND d.deptype IN ('a', 'i')
LEFT JOIN pg_catalog.pg_class t ON t.oid = d.refobjid
LEFT JOIN pg_catalog.pg_namespace tn ON tn.oid = t.relnamespace
LEFT JOIN pg_catalog.pg_attribute a ON a.attrelid = t.oid AND a.attnum = d.refobjsubid
WHERE s.relkind = 'S' AND ` + systemSchemas + `
ORDER BY n.nspname, s.relname`

// readableSequencesSql reads the values the role is allowed to see.
//
// pg_sequences only returns rows for sequences the caller holds SELECT or USAGE on, so a missing
// row is a privilege signal rather than a missing sequence.
const readableSequencesSql = `
SELECT schemaname, sequencename, last_value
FROM pg_catalog.pg_sequences
WHERE schemaname NOT LIKE 'pg\_%' AND schemaname <> 'information_schema'`

// sequenceOwner is the column a sequence backs. Its schema is the *table's*, which is not
// necessarily the sequence's -- a sequence can live in a different schema than the table it feeds.
type sequenceOwner struct {
	schema, table, column string
}

func (o sequenceOwner) exists() bool { return o.table != "" }

type sequenceRef struct {
	schema string
	name   string
	owner  sequenceOwner
}

func (s sequenceRef) key() string { return fmt.Sprintf("%s.%s", s.schema, s.name) }

// Sequences captures every sequence's position so the restore can replay it.
//
// Sequence values are not carried by pg_dump --schema-only, so without this step a restored
// environment starts every sequence at 1 and the first insert collides with existing data.
func (i Introspector) Sequences(ctx context.Context) ([]Sequence, error) {
	refs, err := i.sequenceRefs(ctx)
	if err != nil {
		return nil, err
	}
	readable, err := i.readableSequences(ctx)
	if err != nil {
		return nil, err
	}

	out := make([]Sequence, 0, len(refs))
	for _, ref := range refs {
		seq := Sequence{Schema: ref.schema, Name: ref.name}
		if ref.owner.exists() {
			seq.OwnedBy = fmt.Sprintf("%s.%s.%s", ref.owner.schema, ref.owner.table, ref.owner.column)
		}

		switch last, ok := readable[ref.key()]; {
		case ok:
			seq.Source = SourceCatalog
			if last != nil {
				seq.LastValue, seq.IsCalled = *last, true
			} else {
				// Never advanced. Leaving is_called false makes the next nextval return the start
				// value rather than one past it.
				seq.LastValue, seq.IsCalled = 1, false
			}

		case !ref.owner.exists():
			seq.Source = SourceUnavailable

		default:
			max, err := i.maxOwnedValue(ctx, ref.owner)
			if err != nil {
				return nil, err
			}
			seq.Source = SourceMaxColumn
			if max != nil {
				seq.LastValue, seq.IsCalled = *max, true
			} else {
				seq.LastValue, seq.IsCalled = 1, false
			}
		}
		out = append(out, seq)
	}
	return out, nil
}

func (i Introspector) sequenceRefs(ctx context.Context) ([]sequenceRef, error) {
	rows, err := i.DB.Query(ctx, sequencesSql)
	if err != nil {
		return nil, fmt.Errorf("error reading sequences: %w", err)
	}
	defer rows.Close()

	refs := make([]sequenceRef, 0)
	for rows.Next() {
		var ref sequenceRef
		if err := rows.Scan(&ref.schema, &ref.name,
			&ref.owner.schema, &ref.owner.table, &ref.owner.column); err != nil {
			return nil, fmt.Errorf("error reading sequences: %w", err)
		}
		refs = append(refs, ref)
	}
	return refs, rows.Err()
}

func (i Introspector) readableSequences(ctx context.Context) (map[string]*int64, error) {
	rows, err := i.DB.Query(ctx, readableSequencesSql)
	if err != nil {
		return nil, fmt.Errorf("error reading sequence values: %w", err)
	}
	defer rows.Close()

	out := map[string]*int64{}
	for rows.Next() {
		var schema, name string
		var last *int64
		if err := rows.Scan(&schema, &name, &last); err != nil {
			return nil, fmt.Errorf("error reading sequence values: %w", err)
		}
		out[fmt.Sprintf("%s.%s", schema, name)] = last
	}
	return out, rows.Err()
}

// maxOwnedValue reads the high-water mark of the column a sequence backs.
//
// Runs inside the export's snapshot when called from there, so it agrees with the rows that were
// actually copied.
func (i Introspector) maxOwnedValue(ctx context.Context, o sequenceOwner) (*int64, error) {
	sq := fmt.Sprintf("SELECT max(%s)::bigint FROM %s.%s",
		pq.QuoteIdentifier(o.column), pq.QuoteIdentifier(o.schema), pq.QuoteIdentifier(o.table))

	var max *int64
	if err := i.DB.QueryRow(ctx, sq).Scan(&max); err != nil {
		return nil, fmt.Errorf("error reading max(%s.%s.%s) for sequence position: %w",
			o.schema, o.table, o.column, err)
	}
	return max, nil
}
