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
	"strings"

	"github.com/kr9ly/sqlshape/check/postgres/v2/schema"
	"github.com/kr9ly/sqlshape/v2/x/facts"
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
	// Context is the named context the obligation belongs to; "" is the base set that
	// applies everywhere. A context adds obligations and waives base ones (Waiver).
	Context string
	// Waiver marks a context's `waive <body>`: it removes the base obligation with the
	// same subject and body while that context is selected. Never judged itself.
	Waiver bool
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
	// Alone: the statement may not touch a table of another aggregate; the value is the
	// aggregate's root. Produced by an `aggregate` declaration, never written by hand.
	Alone string
	// Never: no statement of the obligation's kinds may exist (`require never on update,
	// delete`: an append-only table).
	Never bool
	// Paired: the statement must also write the named table, in the same statement (a
	// data-modifying CTE): `require paired(outbox) on insert`.
	Paired string
	// Single: the statement must provably touch at most one row (the One proof):
	// `require single on delete`.
	Single bool
	// Transitions: the column is a state machine; an UPDATE that sets it to a constant
	// must fix the column to one of that state's predecessors in its WHERE, and may not set
	// it to anything but a declared state. From a `transitions` declaration.
	Transitions *Transitions
	// Sensitive: the columns carry the label, and a statement may reference them only in
	// a context that `may read` the label. From a `sensitive` declaration.
	Sensitive *Sensitive
	// MayRead: a context's permission to read the label (a `context X: may read pii`
	// item). Not an obligation; InContext keeps it for the checker.
	MayRead string
}

// Transitions is a state machine over one column: for each target state, the states an
// UPDATE may move from.
type Transitions struct {
	Column string
	From   map[string][]string // target -> predecessors, in declaration order
	Order  []string            // targets in declaration order (for messages)
}

// Sensitive labels columns of a table.
type Sensitive struct {
	Label   string
	Columns []string
}

// Spec spells the body the way a `require` directive does, whitespace-normalized: the
// form a `waive` directive names it by.
func (b Body) Spec() string {
	switch {
	case b.Pinned != "":
		return "pinned(" + b.Pinned + ")"
	case b.Immutable != "":
		return "immutable(" + b.Immutable + ")"
	case b.ViaView:
		return "via view"
	case b.Alone != "":
		return "alone"
	case b.Never:
		return "never"
	case b.Paired != "":
		return "paired(" + b.Paired + ")"
	case b.Single:
		return "single"
	case b.Transitions != nil:
		return "transitions " + b.Transitions.Column
	case b.Sensitive != nil:
		return "sensitive " + b.Sensitive.Label
	case b.MayRead != "":
		return "may read " + b.MayRead
	}
	return strings.Join(strings.Fields(b.Predicate), " ")
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
)

// Discharge is one (leaf, obligation) judgment. Every applicable judgment is recorded,
// discharged or not, so an entry point can print the full audit (`sqlshape check`) or
// just the failures (vet). An obligation whose kinds do not include the statement's
// class at that leaf is not a judgment and is not recorded.
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
