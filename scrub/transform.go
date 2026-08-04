package scrub

import (
	"fmt"
	"sort"
	"strings"

	"github.com/lib/pq"
	"github.com/nullstone-io/pg-snapshot/catalog"
)

// Builtin is a named transform.
//
// A configured transform is read as a builtin only when it matches one of these exactly.
// Anything else is passed to postgres as a raw SQL expression -- the escape hatch is the
// default, and the builtins are shorthand for the cases that come up every time.
type Builtin string

const (
	// BuiltinNull replaces the value with NULL
	BuiltinNull Builtin = "null"

	// BuiltinMD5 replaces the value with a salted hash of itself.
	//
	// Deterministic within a run, which is what keeps joins intact: a value scrubbed in one
	// table matches the same value scrubbed in another, so foreign keys over scrubbed columns
	// still resolve. The salt rotates between runs, so hashes are not comparable across
	// snapshots.
	BuiltinMD5 Builtin = "md5"

	// BuiltinEmail replaces the value with a deterministic address in the reserved .invalid TLD.
	//
	// Derived from a hash of the original rather than from a row number, so it preserves
	// uniqueness -- a UNIQUE index on the column still builds during post-data.
	BuiltinEmail Builtin = "email"

	// BuiltinRedact replaces the value with a fixed marker
	BuiltinRedact Builtin = "redact"
)

const (
	emailPrefix = "user_"
	emailDomain = "@example.invalid"
	md5Len      = 32
	redactValue = "REDACTED"
)

type builtinFn func(col catalog.Column, salt string) (string, error)

var builtins = map[Builtin]builtinFn{
	BuiltinNull:   nullExpr,
	BuiltinMD5:    md5Expr,
	BuiltinEmail:  emailExpr,
	BuiltinRedact: redactExpr,
}

// LookupBuiltin resolves a configured transform to a builtin, reporting whether it is one.
// Matching is case-insensitive but otherwise exact.
func LookupBuiltin(transform string) (Builtin, bool) {
	b := Builtin(strings.ToLower(strings.TrimSpace(transform)))
	_, ok := builtins[b]
	return b, ok
}

// BuiltinNames lists the builtins in a stable order, for error messages
func BuiltinNames() []string {
	names := make([]string, 0, len(builtins))
	for b := range builtins {
		names = append(names, string(b))
	}
	sort.Strings(names)
	return names
}

// Expr renders the builtin as a SQL expression over col.
func (b Builtin) Expr(col catalog.Column, salt string) (string, error) {
	fn, ok := builtins[b]
	if !ok {
		return "", fmt.Errorf("unknown builtin transform %q, expected one of: %s",
			b, strings.Join(BuiltinNames(), ", "))
	}
	return fn(col, salt)
}

func nullExpr(col catalog.Column, _ string) (string, error) {
	// Caught here rather than at load time, where it would surface as a NOT NULL violation
	// partway through a COPY with no indication of which rule caused it
	if col.NotNull {
		return "", fmt.Errorf("column %q is NOT NULL and cannot be scrubbed to null; "+
			"use a constant expression instead, e.g. \"''\"", col.Name)
	}
	return "NULL", nil
}

func md5Expr(col catalog.Column, salt string) (string, error) {
	if err := requireTextual(col, BuiltinMD5); err != nil {
		return "", err
	}
	return fitText(saltedHash(col, salt), col, md5Len)
}

func emailExpr(col catalog.Column, salt string) (string, error) {
	if err := requireTextual(col, BuiltinEmail); err != nil {
		return "", err
	}

	// The prefix and domain are fixed, so a bounded column only constrains the hash between them
	hashLen := md5Len
	if fixed := len(emailPrefix) + len(emailDomain); col.MaxLen > 0 {
		if col.MaxLen-fixed < 1 {
			return "", fmt.Errorf("column %q holds at most %d characters, too short for an email "+
				"address (needs at least %d); use a constant expression instead",
				col.Name, col.MaxLen, fixed+1)
		}
		if avail := col.MaxLen - fixed; avail < hashLen {
			hashLen = avail
		}
	}

	expr := fmt.Sprintf("%s || left(%s, %d) || %s",
		pq.QuoteLiteral(emailPrefix), saltedHash(col, salt), hashLen, pq.QuoteLiteral(emailDomain))
	return castTo(expr, col), nil
}

func redactExpr(col catalog.Column, _ string) (string, error) {
	if err := requireTextual(col, BuiltinRedact); err != nil {
		return "", err
	}
	return fitText(pq.QuoteLiteral(redactValue), col, len(redactValue))
}

// saltedHash renders md5 over the column's text representation plus the run's salt.
//
// The salt is a literal in the generated SQL rather than a bind parameter because these
// expressions are assembled into a COPY (SELECT ...) TO STDOUT statement, which takes no
// parameters.
func saltedHash(col catalog.Column, salt string) string {
	return fmt.Sprintf("md5(%s::text || %s)", pq.QuoteIdentifier(col.Name), pq.QuoteLiteral(salt))
}

// fitText truncates a text-producing expression to the column's limit and casts it back.
//
// Without this a 32-character hash lands in a varchar(20) and the *restore* fails on a value
// too long for the target column -- an hour after the export that caused it.
func fitText(expr string, col catalog.Column, produces int) (string, error) {
	if col.MaxLen > 0 && col.MaxLen < produces {
		expr = fmt.Sprintf("left(%s, %d)", expr, col.MaxLen)
	}
	return castTo(expr, col), nil
}

// castTo casts an expression to the column's exact declared type.
//
// TypeName comes from format_type(), which renders the length modifier as part of the name, so
// it is directly usable as a cast target.
func castTo(expr string, col catalog.Column) string {
	return fmt.Sprintf("(%s)::%s", expr, col.TypeName)
}

func requireTextual(col catalog.Column, b Builtin) error {
	if col.IsTextual() {
		return nil
	}
	return fmt.Errorf("transform %q produces text but column %q is %s; "+
		"use a raw SQL expression that yields %s", b, col.Name, col.TypeName, col.TypeName)
}
