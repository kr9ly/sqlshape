// Package facts is the data contract between a dialect's analyzer and the obligation
// checker: what one analyzed statement (one expansion) provably does, written down
// without any parser node, so that x/obligation can judge declarations against it
// without knowing which database the statement was written for.
//
// Everything here is "provable" in the sense of the One proof (analyze/card.go): an
// equality is recorded only when it holds for every surviving row of its scope, a
// disjunction contributes nothing, a function call is opaque. The producer decides what
// it can prove; the consumer never re-derives from SQL text.
package facts

// StmtKind is the statement class an obligation's `on` list selects.
type StmtKind byte

const (
	Select StmtKind = iota + 1
	Insert
	Update
	Delete
	// Merge is the statement's kind only: its writes are the WHEN branches, each a Write
	// of the branch's own kind, so an obligation `on update` sees the UPDATE branch and
	// an INSERT-only MERGE owes nothing on update.
	Merge
	// Call is a procedure call: no scope of its own (the body's reads and writes are the
	// procedure's), and at most one row.
	Call
)

// Facts is the record of one statement.
type Facts struct {
	Kind StmtKind
	// Top is the outermost scope: the FROM of a SELECT, or the target plus FROM / USING of
	// a write. Subqueries, CTEs and expanded view bodies hang under it as Children.
	Top *Scope
	// Source is the query an INSERT ... SELECT reads, when the statement has one (also one
	// of Top's Children): its row count is the statement's. Nil otherwise; an INSERT of a
	// VALUES list says its row count through Top's Single / Many.
	Source *Scope
	// Writes are the tables the statement stores into, with the columns it assigns.
	Writes []Write
	// AtMostOne: the statement provably touches at most one row (the One proof).
	AtMostOne bool
	// Uses are the relation columns the statement references anywhere (a view's columns
	// as the view's), each once, in order of first appearance.
	Uses []Use
}

// Use is one column reference of the statement.
type Use struct {
	Table    string
	Column   string
	Position int32 // 0-based
	// Assigned: the column is only a write target here (an INSERT column, a SET target),
	// never read.
	Assigned bool
}

// Scope is one level of name resolution: a set of leaves joined together, and what the
// predicates at this level prove about their rows.
type Scope struct {
	// Leaves are the relations in FROM at this level, in order; Pred.Col.Leaf and
	// ColRef.Leaf index into it.
	Leaves []Leaf
	// Preds are the conjuncts that hold for every row this scope produces: WHERE, an
	// inner join's ON / USING, and -- for the nullable side only -- an outer join's ON.
	// Predicates inherited from an expanded view body or a row-security policy appear
	// here too, marked by Origin.
	Preds []Pred
	// Fixed are the columns equal to a value known before the statement runs (a literal,
	// a parameter, an outer reference), after closing over the equalities in Preds.
	Fixed []ColRef
	// Edges are the directed equalities between columns: From fixed implies To fixed. An
	// inner join's `a.x = b.y` yields both directions; an outer join's ON yields only the
	// direction into its nullable side. Fixed is already closed over them; the checker
	// uses them to carry an obligation across a join (foreign-key propagation).
	Edges []Edge
	// NotNull are the columns the predicates prove non-NULL for surviving rows.
	NotNull []ColRef
	// Children are the nested scopes (subqueries, CTE bodies, expanded view bodies,
	// RETURNING); an obligation is checked wherever its table appears, at any depth.
	Children []*Scope
	// Returning marks the RETURNING list's scope: its rows are the ones just written, so
	// read obligations do not apply.
	Returning bool
	// At is the 0-based offset of this level's WHERE (or ON) clause, -1 without one:
	// where a missing predicate is reported.
	At int32
	// The level's row count beyond what its leaves say, read by the cardinality proof
	// before the key argument. Single: at most one row whatever the leaves hold (a
	// constant LIMIT 0 / 1, a single VALUES row, an aggregate without GROUP BY). Many: why
	// the level may yield many rows whatever its predicates fix (a set operation, a VALUES
	// list of several rows, a FULL JOIN); empty when nothing structural says so. Single is
	// read first: LIMIT 1 over a UNION is one row.
	Single bool
	Many   string
	// Groups are the GROUP BY expressions when the level has a GROUP BY: it yields one row
	// per group, so it is one row when every expression is pinned -- a Column term whose
	// column is known, or a Param / Const / Known / Outer term; an Expr term never is.
	// GroupingSets marks GROUPING SETS / ROLLUP / CUBE (one row per set).
	Groups       []Term
	GroupingSets bool
}

// Leaf is one relation occurrence in a scope.
type Leaf struct {
	// Table is schema-qualified unless public, like analyze.Source.Table.
	Table string
	// Alias is the name the statement uses (the table name when unaliased).
	Alias string
	Kind  RelKind
	Role  Role
	// Position is the 0-based byte offset of the reference in the statement text.
	Position int32
	// Waived are the obligation names the statement (or, inside a view body, the view's
	// own directives) opts out of for this table. Reported, never silently dropped.
	Waived []string
	// Body is the defining query of a view, subquery or CTE leaf, when the producer
	// analyzed it (a subquery's or CTE's body is also one of the enclosing scope's
	// Children; a view's is reached only from here). The body's own leaves live in that
	// scope; a view's predicates are inherited into the outer scope's Preds with Origin ==
	// FromView. Set for a table leaf too when the write goes through an automatically
	// updatable view: the rows are the view's.
	Body *Scope
	// Outputs map the leaf's output columns, by name, to the Body column each projects
	// plainly (`SELECT o.id ... ` makes the output id the body's o.id); an output that is
	// an expression is absent. The cardinality proof seeds the body with the outputs the
	// enclosing level fixes.
	Outputs []Output
	// Single: the leaf is at most one row on its own (a scalar function in FROM).
	Single bool
	// Keys are the enforced unique keys of a table leaf (the primary key, the UNIQUE
	// constraints and unique indexes over whole columns), as the producer knows them from
	// the schema: what the cardinality proof fixes rows by. A key not enforced at every
	// statement (DEFERRABLE) is left out. Nil for a leaf that has none the producer can
	// vouch for.
	Keys []Key
}

// Key is one unique key of a table leaf.
type Key struct {
	Columns []string
	// Where are the conjuncts of a partial unique index's predicate, in the same form as
	// a scope's Preds and about this leaf (ColRef.Leaf is the leaf's own index in its
	// scope): the key holds only where the statement repeats every one of them. Nil for
	// a whole-table key.
	Where []Pred
	// Temporal: the last column is a range and the key is WITHOUT OVERLAPS -- no two rows
	// with the other columns equal have overlapping ranges, so a row whose range Contains
	// a known point is the only one for those values.
	Temporal bool
}

// Output is one output column of a derived leaf that is a plain column of its body.
type Output struct {
	Name string
	Col  ColRef // a column of Body's leaves
}

// RelKind is what the leaf is.
type RelKind byte

const (
	Table RelKind = iota + 1
	View
	MatView
	// Derived is a subquery or VALUES in FROM: it has no obligations of its own, but its
	// scope may.
	Derived
	// CTE is a reference to a WITH item (the same kind of leaf as Derived, named).
	CTE
	// Function is a function call in FROM (a set-returning function, XMLTABLE, ...).
	Function
)

// Role is how the statement touches the leaf.
type Role byte

const (
	// Read: the leaf is scanned (SELECT, or the reading part of a write).
	Read Role = iota + 1
	// Target: the leaf is the table an INSERT / UPDATE / DELETE / MERGE writes.
	Target
)

// Write is one table the statement stores into.
type Write struct {
	Table string
	Kind  StmtKind // Insert / Update / Delete (a MERGE branch reports its own kind)
	// Assigned are the columns given a value: INSERT's column list (or all columns), an
	// UPDATE / MERGE UPDATE branch's SET targets. Values are the terms stored, parallel
	// to Assigned (a Known term for an expression).
	Assigned []string
	Values   []Term
	Position int32
	// InWith marks a write done by a data-modifying WITH item rather than the statement
	// itself (the One proof does not cover it).
	InWith bool
}

// Edge is one directed equality between two columns of a scope.
type Edge struct {
	From, To ColRef
}

// ColRef names a column of a leaf in the enclosing scope.
type ColRef struct {
	Leaf   int
	Column string
}

// Pred is one normalized conjunct. Both the producer (statement predicates, view bodies,
// policies) and the obligation checker (declared predicates, through a Lowerer) speak
// this form, so implication is decided in one place over one language.
type Pred struct {
	Op PredOp
	// Col is the column the predicate is about (Eq / IsNull / IsNotNull). Unused for Opaque.
	Col ColRef
	// Term is the other side of an Eq.
	Term Term
	// Terms are the alternatives of an In (`col IN (a, b)`, `col = a OR col = b`): the
	// column holds one of them. Each is a Param, Const or Known term.
	Terms []Term
	// Sub is the body of an Exists predicate: a scope of its own (its leaves, what holds
	// for their rows), correlated to this scope through Outer terms. `x IN (SELECT y
	// ...)` is recorded as Exists with the equality y = x added to the body.
	Sub *Scope
	// Text is the canonical rendering of a predicate the language cannot decompose
	// (`amount > 0`, `status IN ('a','b')`), with every column reference that resolves
	// to a leaf of the scope reduced to its bare column name; Cols lists those columns.
	// Two Opaque predicates match when their Text is equal and they are about the same
	// leaf. This is the syntactic fallback.
	Text string
	Cols []ColRef
	// Restricts lists the leaves this conjunct is allowed to restrict; nil means all. An
	// outer join's ON restricts only its nullable side.
	Restricts []int
	Origin    Origin
	// Name is the policy that contributed a FromPolicy predicate.
	Name string
}

// PredOp is the shape of a Pred.
type PredOp byte

const (
	Eq PredOp = iota + 1
	IsNull
	IsNotNull
	// Exists: an unnegated EXISTS / IN subquery conjunct; Sub is its body. The rows of
	// this scope have a witness row in Sub's leaves satisfying Sub's predicates.
	Exists
	// In: Col holds one of Terms (a value list, or a disjunction of equalities on the
	// column). Weaker than Eq; enough to know the column's value is among a set.
	In
	// Contains: Col, a range or multirange column, contains the point Term (`col @> $1`,
	// `$1 <@ col`). The producer records it only when the point is visibly not a range
	// itself (an empty range is contained by every range). With a Temporal key it fixes
	// the row the way an equality does.
	Contains
	Opaque
)

// Term is the right-hand side of an equality.
type Term struct {
	Kind TermKind
	// Param is the 1-based $n for a Param term.
	Param int32
	// Const is the literal's text for a Const term.
	Const string
	// Col is the other column for a Column term.
	Col ColRef
	// Text is the canonical rendering for a Known term (an uncorrelated scalar subquery,
	// a stable function of the session, an outer reference the producer could not place):
	// a value fixed before the row is examined, so it pins the column like a parameter.
	Text string
}

// TermKind is the class of a Term.
type TermKind byte

const (
	Param TermKind = iota + 1
	Const
	Column
	Known
	// Outer: a column of the enclosing scope (Col indexes the parent's leaves), from a
	// correlated subquery's predicate. Known to the inner row, and the link by which the
	// inner scope witnesses something about the outer row.
	Outer
	// Expr: an expression over the row the producer does not decompose (Text is its
	// rendering); never a known value. Only in Scope.Groups.
	Expr
)

// Origin says where a Pred came from; the checker reports the discharge path with it.
type Origin byte

const (
	// FromStatement: written in the statement itself.
	FromStatement Origin = iota + 1
	// FromView: carried by the definition of a view the statement reads.
	FromView
	// FromPolicy: the USING predicate of a row-security policy on the table, holding for
	// roles subject to row security (for the owner only under FORCE ROW LEVEL SECURITY).
	FromPolicy
	// FromForeignKey: derived by the checker through a composite foreign key whose other
	// columns are already equal (items.tenant_id = orders.tenant_id from
	// items.order_id = orders.id). Never produced by the analyzer.
	FromForeignKey
)
