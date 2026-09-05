// Package analyze is the pure-Go PostgreSQL semantic analyzer: given a Schema
// and one SQL statement it derives parameter types, result columns (type,
// nullability, provenance) and PG-compatible errors, following the rules of
// PostgreSQL manual chapter 10 (type conversion) without running PostgreSQL.
package analyze

import (
	"fmt"

	"github.com/kr9ly/sqlshape/internal/schema"
)

// Error is a PG-style semantic error with SQLSTATE and 1-based position (0 if none).
type Error struct {
	Code     string
	Message  string
	Position int32
}

func (e *Error) Error() string {
	if e.Position > 0 {
		return fmt.Sprintf("%s: %s (at %d)", e.Code, e.Message, e.Position)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// SQLSTATE codes used.
const (
	codeSyntaxError           = "42601"
	codeUndefinedColumn       = "42703"
	codeUndefinedTable        = "42P01"
	codeUndefinedFunction     = "42883"
	codeUndefinedObject       = "42704"
	codeAmbiguousColumn       = "42702"
	codeAmbiguousFunction     = "42725"
	codeDatatypeMismatch      = "42804"
	codeIndeterminateDatatype = "42P18"
	codeCannotCoerce          = "42846"
	codeFeatureNotSupported   = "0A000"
	codeGroupingError         = "42803"
	codeInvalidColumnRef      = "42P10"
	codeWrongObjectType       = "42809"
	codeDuplicateAlias        = "42712"
)

// errAt builds an Error; loc is the 0-based node location (PG reports 1-based).
func errAt(code string, loc int32, format string, args ...any) *Error {
	pos := int32(0)
	if loc >= 0 {
		pos = loc + 1
	}
	return &Error{Code: code, Message: fmt.Sprintf(format, args...), Position: pos}
}

// Source is the table column a result column comes straight from.
type Source struct {
	Table   string
	Column  string
	NotNull bool
}

// Column is one result column.
type Column struct {
	Name string
	Type schema.TypeRef
	// Nullable is the analyzer's own inference (NOT NULL constraints, join shape, expression rules).
	Nullable bool
	// Source mirrors what PG's Describe reports (stops at views), for oracle parity.
	Source *Source
	// Fields describes a record / composite column (or an array of them): the columns of
	// the row type, or the positional f1.. fields of an anonymous row(...). Nil otherwise.
	Fields []Column
}

// Result is the analysis of one statement.
type Result struct {
	Params  []schema.TypeRef
	Columns []Column
	// ParamSources, indexed like Params, is the table column a parameter was compared
	// with or assigned to (nil when it met an expression). It carries "which identity"
	// a value stands for, beyond its type.
	ParamSources []*Source
	// Notes are findings PG itself would accept, e.g. mixing domains (domain.go).
	Notes []Note
	// AtMostOne is whether the statement provably returns at most one row (card.go);
	// ManyRowsWhy says what blocks the proof otherwise.
	AtMostOne   bool
	ManyRowsWhy string
	// Violations are the constraints a write may violate (violation.go).
	Violations []Violation
}
