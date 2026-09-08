// Package obligation checks the boundary rules a schema declares against the facts an
// analyzer derived from a statement: a table (view, schema) requires something of every
// statement that touches it, the statement's facts either discharge that requirement or
// they do not, and the ways they can discharge it are few and named.
//
// The package knows the schema model (internal/schema) and the facts contract
// (internal/facts) and nothing else: no parser, no dialect, no Go types. A dialect plugs
// in twice -- by producing facts.Facts, and by implementing Lowerer so that a declared
// SQL predicate can be read in the facts language.
//
// Today's three rules (`visible where`, -require-columns, -no-table-reads / -no-tables)
// are the first declarations to land here; see docs/obligations.md.
package obligation

import (
	"github.com/kr9ly/sqlshape/internal/facts"
	"github.com/kr9ly/sqlshape/internal/schema"
)

// Kinds is the set of statement classes an obligation applies to.
type Kinds uint8

const (
	OnSelect Kinds = 1 << iota
	OnInsert
	OnUpdate
	OnDelete

	OnRead  = OnSelect // `on read`: SELECT and the reading side of writes
	OnWrite = OnInsert | OnUpdate | OnDelete
	OnAll   = OnRead | OnWrite
)

// Obligation is one declared requirement on one subject.
type Obligation struct {
	// Subject is the relation (or, for a schema-level declaration, every relation of that
	// schema) the obligation is attached to. Schema-qualified unless public.
	Subject string
	// Kinds are the statement classes it applies to.
	Kinds Kinds
	// Body is what is required. Exactly one field is set.
	Body Body
	// Source is where it was declared: the schema.sql directive text, or the vet flag
	// that expands to it. Used in diagnostics only.
	Source string
}

// Body is the requirement proper. Predicate obligations are SQL boolean expressions
// judged by implication; the others are predicates about the statement's structure,
// which no SQL expression can state, and are the only built-ins.
type Body struct {
	// Predicate: rows of the subject the statement reads (or modifies) must provably
	// satisfy this expression. `visible where` is `Predicate on read`.
	Predicate string
	// Pinned: the column must be equal to a value known before the statement runs
	// (reads, UPDATE, DELETE), or be assigned (INSERT). -require-columns is `Pinned`
	// on every table that has the column.
	Pinned string
	// Immutable: the column must not appear in an UPDATE's SET.
	Immutable string
	// ViaView: the statement must not reference the subject directly; with
	// IncludeWrites, not even as a write target. -no-table-reads / -no-tables, per table.
	ViaView       bool
	IncludeWrites bool
}

// Lowerer reads a declared predicate in the facts language. Implemented by the dialect's
// analyzer: the expression is parsed and type-checked against rel, and each conjunct
// becomes a facts.Pred whose ColRef.Leaf is 0, standing for the subject; the checker
// rebinds it to the leaf under judgment. A conjunct the language cannot decompose is
// returned as Opaque with its canonical text.
type Lowerer interface {
	Lower(expr string, rel *schema.Relation) ([]facts.Pred, error)
}

// Path is how an obligation was discharged, for audit output and diagnostics.
type Path byte

const (
	// ByStatement: the statement's own WHERE / ON / SET satisfies it.
	ByStatement Path = iota + 1
	// ByView: inherited from the definition of a view the statement reads.
	ByView
	// ByPolicy: a row-security policy's USING satisfies it (owner caveat applies).
	ByPolicy
	// ByForeignKey: satisfied on a joined table and carried over a composite foreign key.
	ByForeignKey
	// Waived: the statement opted out.
	Waived
	// NotApplicable: the obligation's kinds do not include this statement's class, or
	// the leaf is a RETURNING scope.
	NotApplicable
)

// Discharge is one (leaf, obligation) judgment. Every judgment is recorded, discharged
// or not, so an entry point can print the full audit (`sqlshape check`) or just the
// failures (vet).
type Discharge struct {
	Obligation *Obligation
	Leaf       facts.Leaf
	// Path is how it was discharged; zero when it was not.
	Path Path
	// Message explains a failure in the words of the diagnostic (`rows of memos are
	// visible where deleted_at IS NULL: add that predicate for m, or opt out with ...`),
	// and a policy discharge's owner caveat.
	Message string
	// Position is where in the statement to report it.
	Position int32
}

// Failed reports whether the obligation was not discharged.
func (d Discharge) Failed() bool { return d.Path == 0 }

// Problem is a declaration the checker could not use: unknown syntax, a column the
// subject lacks, a predicate that does not lower.
type Problem struct {
	Subject string
	Source  string
	Message string
}

// Options are the entry point's contribution: what the statement opted out of, and the
// context whose obligations apply.
type Options struct {
	// Strict adds the advisory discharges (a policy discharge without FORCE ROW LEVEL
	// SECURITY) to the failures.
	Strict bool
}

// Check judges every obligation against every leaf of f, at every depth, and returns
// all judgments in leaf order. Implemented in the next step; the signature is the
// contract.
func Check(s *schema.Schema, decls []Obligation, f *facts.Facts, l Lowerer, opt Options) []Discharge {
	panic("obligation.Check: not implemented")
}
