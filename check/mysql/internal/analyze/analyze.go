// Package analyze types a MySQL statement against a loaded schema: the result columns
// with their types and nullability, the parameters with the types their context gives
// them, and the errors MySQL itself would raise (unknown table or column, ambiguity).
//
// It reads the statement through mysqlast, the server's own parse tree, so the shapes it
// switches on are MySQL's PT_ / Item_ classes. What it does not know yet it leaves
// untyped rather than guessing: a column whose Known is false is accepted by the checker
// with a note.
package analyze

import (
	"fmt"
	"sort"
	"strings"

	"github.com/kr9ly/sqlshape/check/mysql/v2/internal/mysqlast"
	"github.com/kr9ly/sqlshape/check/mysql/v2/internal/mysqlparse"
	"github.com/kr9ly/sqlshape/check/mysql/v2/internal/schema"
	"github.com/kr9ly/sqlshape/v2/x/facts"
	"github.com/kr9ly/sqlshape/v2/x/obligation"
	"github.com/kr9ly/sqlshape/v2/x/placeholder"
)

// Result is the analysis of one statement.
type Result struct {
	// Params are the placeholders by number ($1 is Params[0]).
	Params  []Param
	Columns []Column
	// Facts is the statement's record for the contracts written on facts (One, the
	// obligations): its kind, the top scope's leaves, predicates, fixed columns and
	// equalities, its writes. Nil when the statement is one the analyzer does not record.
	Facts *facts.Facts
	// Violations are the constraints the statement may violate (violations.go).
	Violations []Violation
	// Uses are the relation columns the statement references (a view's as the view's).
	Uses []facts.Use
}

// Param is one placeholder: the type its context gives it, when the context is one the
// analyzer reads (compared with or assigned to a column, LIMIT).
type Param struct {
	Type  schema.Type
	Known bool
	// Source is the table column the placeholder met: compared with (`col = $1`, `col IN
	// ($1, ...)`, `$1 IN (SELECT col ...)`) or stored into (INSERT / UPDATE). Nil otherwise.
	Source *ParamSource
}

// ParamSource is the table column a placeholder stands for.
type ParamSource struct {
	Table    string
	Column   string
	NotNull  bool
	Assigned bool // stored into the column, not compared with it
}

// Column is one result column.
type Column struct {
	Name     string
	Type     schema.Type
	Known    bool // the type was inferred; false leaves Type empty
	Nullable bool

	base      *schema.Column // the base table column this is a plain reference to, if any
	baseTable *schema.Table  // its table
	aliased   bool           // a select item written with an alias (find_item_in_list prefers these)
	// leaf1 / leafCol: the relation of the producing block (1-based index into its rels,
	// 0 when the column is not a plain reference) and the column's name there
	leaf1   int
	leafCol string
}

// Error is what MySQL would raise for the statement.
type Error struct {
	Message  string
	Code     int // MySQL's error number: 1064 syntax, 1146 no table, 1054 no column, 1052 ambiguous
	Position int // 0-based byte offset into the statement as given ($n form); -1 when unknown
}

func (e *Error) Error() string { return fmt.Sprintf("%s (MySQL error %d)", e.Message, e.Code) }

// Analyze types sql, whose placeholders are `$n`, against s.
func Analyze(s *schema.Schema, sql string) (*Result, error) {
	text, ph := placeholder.Rewrite(sql)
	cst, err := mysqlparse.Parse(text, s.Settings.ParseMode())
	if err != nil {
		if pe, ok := err.(*mysqlparse.Error); ok {
			return nil, &Error{Message: pe.Message, Code: 1064, Position: ph.Back(pe.Offset)}
		}
		return nil, err
	}
	root, err := mysqlast.BuildMode(text, cst, s.Settings.ParseMode())
	if err != nil {
		if u, ok := err.(*mysqlast.Unsupported); ok && strings.Contains(u.Text, "syntax error") {
			// a construct the grammar accepts and the server's action rejects
			return nil, &Error{Message: fmt.Sprintf("syntax error at byte %d: %s", ph.Back(u.Start), u.Text), Code: 1064, Position: ph.Back(u.Start)}
		}
		return nil, err
	}
	a := &analyzer{s: s, text: text, ph: ph, params: make([]Param, ph.Count()), waived: canonicalWaivers(s, obligation.StatementWaivers(sql))}
	if err := a.statement(root); err != nil {
		return nil, err
	}
	if err := a.checkCalledRoutineOverlap(); err != nil {
		return nil, err
	}
	for i := range a.params {
		a.params[i].Source = a.paramSrc[i+1]
	}
	sort.SliceStable(a.uses, func(i, j int) bool { return a.uses[i].Position < a.uses[j].Position })
	if a.facts != nil {
		a.facts.Uses = a.uses
	}
	return &Result{Params: a.params, Columns: a.columns, Facts: a.facts, Violations: a.violations(), Uses: a.uses}, nil
}

// canonicalWaivers keys a statement's opt-outs by the tables' stored names, which the facts'
// leaves carry (lower_case_table_names decides how a directive's spelling matches).
func canonicalWaivers(s *schema.Schema, waived map[string][]string) map[string][]string {
	out := map[string][]string{}
	for table, specs := range waived {
		out = obligation.AddWaiver(out, s.CanonicalName(table), specs...)
	}
	return out
}

type analyzer struct {
	s       *schema.Schema
	text    string // the statement with `?` placeholders, what mysqlparse saw
	ph      placeholder.Map
	params  []Param
	columns []Column
	views   map[string]bool // the views being expanded, against a cycle
	facts   *facts.Facts    // the statement's record, assembled by statement
	depth   int             // query nesting: 0 at the statement's own block
	setOp   int             // > 0 while inside a set operation's arms
	// uses are the relation columns the statement references, each once, in order of
	// first appearance; readUses marks those read (not only assigned). assigning is set
	// while an INSERT's column list or an UPDATE's SET targets are resolved.
	uses      []facts.Use
	useIdx    map[string]int
	readUses  map[string]bool
	assigning bool
	// waived are the statement's opt-outs (`-- sqlshape: unfiltered t`, `waive t ...`), by
	// table; viewWaived are those of the views being expanded, innermost last
	waived     map[string][]string
	viewWaived []map[string][]string
	// definingView is the view AnalyzeView is analyzing: its body's references are its own
	// uses (a view expanded inside a statement contributes none)
	definingView string
	// write is the statement's write, for the failure modes: its target, the columns
	// assigned with what is stored in them, and IGNORE / ON DUPLICATE KEY UPDATE
	write *write
	// checkOptionViews are the WITH CHECK OPTION views (and, down a CASCADED chain,
	// further ones) a write reaching table through it must satisfy (1369), by the base
	// table: computed once in facts.go's checkOptionFacts, alongside the pinned lift it
	// does through the same view chain for the same reason
	checkOptionViews map[*schema.Table][]string
	// outerRefs are the column references a nested query resolved in an enclosing block
	// (fullgroup.go reads them: a correlated reference is a column of the block it names)
	outerRefs []outerRef
	// blocks are the SELECT blocks typed so far, by their facts, for the functional
	// dependencies a derived table's body gives (fullgroup.go)
	blocks map[*facts.Scope]*blockInfo
	// fdConst says of an equality's known side (pred text + term text) whether the server
	// treats it as a constant for functional dependencies (a literal, not a parameter)
	fdConst map[string]bool
	// nullEq are the columns a block compares with the literal NULL (`u = NULL`): never true,
	// so no fact fixes them, but the server's ONLY_FULL_GROUP_BY check takes the equality
	// for a constant one and derives functional dependencies from it (aggregate_check.cc)
	nullEq map[*facts.Scope][]facts.ColRef
	// lists are the SELECT blocks whose select list, GROUP BY, HAVING or ORDER BY is being
	// typed, innermost last: a nested query's unqualified name may be one of their select
	// aliases (Item_field::fix_outer_field's resolve_in_select_list; see lookup)
	lists []listLookup
	// subFacts are the subqueries' bodies by their PT_subquery node, for the EXISTS / IN
	// predicates; claimed are the bodies such a predicate carries (not Children then)
	subFacts map[*mysqlast.Node]*facts.Scope
	claimed  map[*facts.Scope]bool
	// paramSrc is the column each placeholder ($n, 1-based) met first
	paramSrc map[int]*ParamSource
	// calledRoutines are the schema-declared FUNCTIONs a call site in this statement
	// resolved to (expr.go's storedFuncCall), and the PROCEDURE a CALL statement itself
	// runs (call.go's callStmt), each once (calledSeen dedupes by pointer), in the order
	// first met: what violations() folds in (its own failure modes) and Analyze folds in
	// checkCalledRoutineOverlap (1442: a function writing a table this statement already
	// reads or writes).
	calledRoutines []calledRoutine
	calledSeen     map[*schema.Routine]bool
	// refRels are the tables and views the statement names (target(), a view's own
	// underlying tables included as its body is typed), lower-cased: what a called
	// routine's write collides with (1442) even where no column of them is read, as in
	// `SELECT f(1) FROM t` (measured: 1442 when f writes t).
	refRels map[string]bool
	// bodyResult is the BodyResult the trigger's or routine's body walk is filling, nil
	// for a top-level statement: exprAt hands the routines a body expression calls to it
	// (finishCalls).
	bodyResult *BodyResult

	// The rest is body.go's: a trigger's or routine's body walk (nil outside it). trig /
	// trigTable are set for a trigger's body (NEW / OLD resolve against trigTable, and the
	// trigger's own timing / event decide their rules); routine is set for a routine's
	// body (RETURN's context; nil for a trigger). vars is the block-scoped chain of
	// declared variables and parameters (innermost first); cursors are the declared
	// cursors of the innermost block that has one in scope; labels are the enclosing
	// labeled blocks / loops, for LEAVE / ITERATE. sawReturn records whether the routine's
	// body had a RETURN anywhere (a function without one is refused).
	trig *schema.Trigger
	// storeRow is the VALUES row (1-based) whose literals literalStore is judging; 0
	// outside a multi-row INSERT (the message then says row 1)
	storeRow int
	// noFold turns constant folding (fold.go) off while positive: a branch a constant
	// condition decides against, a GROUP BY / ORDER BY item, an EXISTS's select list;
	// foldPerRow says a constant's 1690 is the violation of an expression that runs per
	// row (a select item over a FROM, an UPDATE's SET), not the statement's error
	noFold     int
	foldPerRow bool
	// insertTarget is the INSERT's target relation while its ON DUPLICATE KEY UPDATE is
	// typed: what VALUES(c) resolves c against
	insertTarget *relation
	// parents are the expression nodes being typed, outermost first; condBases the
	// indexes in it where each condition being typed starts (optimizeTime reads them)
	parents    []*mysqlast.Node
	condBases  []int
	existsList bool // the next SELECT block typed is an EXISTS's: its select list is not evaluated
	limitZero  bool // the next SELECT block typed is under LIMIT 0: likewise
	// storeRaised are the failure modes a store of a value into a column adds (a geometry
	// of another type into a typed spatial column, geometryStore), folded into violations()
	storeRaised []Violation
	trigTable   *schema.Table
	routine     *schema.Routine
	vars        *varScope
	labels      []string
	sawReturn   bool
	// raised accumulates the body's own failure modes as the walk finds them (a SIGNAL, an
	// embedded write's own violations, a SELECT INTO's 1172): what block's own DECLARE ...
	// HANDLER absorption filters, and what AnalyzeTrigger/AnalyzeRoutine hand back as
	// BodyResult.Violations once the outermost block has closed.
	raised []Violation
	// handlerRaise is, while walking a HANDLER's own body, the exact violations this
	// handler is catching (the block's own subset that matched its conditions): a bare
	// RESIGNAL (no condition, no SET) re-raises them unchanged (its own SET MYSQL_ERRNO,
	// when it has one, overrides the number instead, walkSignal).
	handlerRaise []Violation
	// inHandler counts the HANDLER bodies currently being walked (absorb), nested ones
	// included: a bare RESIGNAL reached with this at 0 is outside any HANDLER, which the
	// server always refuses at 1645 (walkSignal), regardless of what handlerRaise holds
	// (left over from an enclosing statement's own last HANDLER, never this one's).
	inHandler int
	// raises names the trigger's/routine's own `-- sqlshape: error <key> = <Name>`
	// annotations, by key (a MYSQL_ERRNO as decimal text, or a SQLSTATE).
	raises map[string]string
}

// condKind is which failure modes a condition (a SIGNAL's, a HANDLER's, a DECLARE
// CONDITION's) names.
type condKind int

const (
	condSQLState  condKind = iota // an exact 5-character SQLSTATE
	condNumber                    // an exact MySQL error number
	condWarning                   // SQLWARNING: SQLSTATE class "01"
	condNotFound                  // NOT FOUND: SQLSTATE class "02"
	condException                 // SQLEXCEPTION: any class but "00", "01", "02"
)

// condRef is one resolved condition value (sp_condition_value's _mysqlerr, or a
// DECLARE ... CONDITION a sp_condition_name resolves to).
type condRef struct {
	kind     condKind
	number   int
	sqlstate string
}

// catches reports whether cond matches v, the way MySQL's HANDLER search does (measured:
// SQLEXCEPTION catches a custom SQLSTATE and a schema constraint's alike; a HANDLER FOR a
// MySQL error number matches the server's own number, whichever form raised it).
func (c condRef) catches(v Violation) bool {
	class := ""
	if len(v.SQLState) >= 2 {
		class = v.SQLState[:2]
	}
	switch c.kind {
	case condNumber:
		return v.Code == c.number
	case condSQLState:
		return strings.EqualFold(v.SQLState, c.sqlstate)
	case condWarning:
		return class == "01"
	case condNotFound:
		return class == "02"
	case condException:
		return class != "" && class != "00" && class != "01" && class != "02"
	}
	return false
}

// write is what a statement stores, for the failure modes (violations.go).
type write struct {
	kind   facts.StmtKind
	table  *schema.Table
	values []assignment
	// ignore: INSERT / UPDATE / DELETE IGNORE turns every constraint error into a warning
	ignore bool
	// onDuplicate: INSERT ... ON DUPLICATE KEY UPDATE absorbs the unique violations and
	// updates these columns instead
	onDuplicate []assignment
	// insertsAll: an INSERT without a column list, or a SET-form one, names every column
	inserted map[string]bool
	// query: the values come from a query (INSERT ... SELECT), nullability per column
	query bool
	// rows: the VALUES rows (0 for the query form). Without strict mode only a single-row
	// INSERT / REPLACE rejects a NULL for a NOT NULL column (1048); more rows, a query and
	// an UPDATE store the type's implicit default with a warning instead
	rows int
	// replace: REPLACE INTO -- a colliding row is deleted first, so no key is violated
	// but the rows referring to the replaced one are (1451)
	replace bool
	// load: LOAD DATA -- a NOT NULL column the column list leaves out takes its type's
	// implicit default, not 1364 (measured)
	load bool
	// more are the further tables a multi-table UPDATE assigns or a multi-table DELETE
	// deletes from, each with its own assignments; table / values are the first's
	more []moreTarget
}

// moreTarget is one further table of a multi-table write.
type moreTarget struct {
	table  *schema.Table
	values []assignment
}

// appendTarget adds an assignment to the table's entry in more.
func appendTarget(more []moreTarget, t *schema.Table, as assignment) []moreTarget {
	for i := range more {
		if more[i].table == t {
			more[i].values = append(more[i].values, as)
			return more
		}
	}
	return append(more, moreTarget{table: t, values: []assignment{as}})
}

// assignment is one value stored into a column.
type assignment struct {
	col      *schema.Column
	nullable bool
	param    int // the bare placeholder stored ($n), 0 otherwise
	// fromFile: a LOAD DATA field, whose NULL for a NOT NULL column is 1263, not 1048
	fromFile bool
}

// relation is a table in scope, under its alias: a base table, or a derived one (a
// derived table, a view, a common table expression) whose columns are its query's.
type relation struct {
	alias     string
	table     *schema.Table // nil for a derived relation
	cols      []Column      // the derived relation's columns
	updatable bool          // a derived relation whose plain column references write through (a mergeable view)
	nullable  bool          // on the nullable side of an outer join
	pos       int           // offset of the reference in the text
	view      string        // the view's name when the relation is a view
	target    bool          // a write's target
	body      *facts.Scope  // a derived relation's own block, for the proof to look into
	cte       bool          // a common table expression
	merged    bool          // a derived relation the server merges into the query (not materialized)
}

// columns lists the relation's columns as a SELECT * expands them (a base table's
// invisible columns are left out).
func (r *relation) columns() []Column {
	if r.table == nil {
		out := make([]Column, len(r.cols))
		for i, c := range r.cols {
			c.Nullable = c.Nullable || r.nullable
			out[i] = c
		}
		return out
	}
	var out []Column
	for _, col := range r.table.Columns {
		if col.Invisible {
			continue
		}
		out = append(out, Column{Name: col.Name, Type: col.Type, Known: true, Nullable: !col.NotNull || r.nullable, base: col, baseTable: r.table})
	}
	return out
}

// column resolves a column name in the relation.
func (r *relation) column(name string) (colRef, bool) {
	if r.table == nil {
		for _, c := range r.cols {
			if strings.EqualFold(c.Name, name) {
				c.Nullable = c.Nullable || r.nullable
				ref := colRef{rel: r, c: c}
				if r.updatable {
					ref.col = c.base
				}
				return ref, true
			}
		}
		return colRef{}, false
	}
	col := r.table.Column(name)
	if col == nil {
		return colRef{}, false
	}
	return colRef{rel: r, col: col, c: Column{Name: col.Name, Type: col.Type, Known: true, Nullable: !col.NotNull || r.nullable, base: col, baseTable: r.table}}, true
}

// rowid is the column `_rowid` names in a base table (find_field_in_table's rowid_field_offset,
// measured in TestNameResolutionServer): the table's first key once the server has sorted them
// (PRIMARY, then the unique keys over NOT NULL columns in declaration order, then the
// rest) when that key is unique over exactly one column of an integer type (the INT
// family, YEAR, BIT) that is NOT NULL; a view or a derived table has none.
func (r *relation) rowid() (colRef, bool) {
	if r.table == nil || r.view != "" {
		return colRef{}, false
	}
	var first *schema.Key
	rank := func(k *schema.Key) int {
		switch {
		case k.Kind == schema.Primary:
			return 0
		case k.Kind == schema.Unique:
			for _, p := range k.Parts {
				c := r.table.Column(p.Column)
				if p.Expr != nil || c == nil || !c.NotNull {
					return 2
				}
			}
			return 1
		}
		return 2
	}
	for _, k := range r.table.Keys {
		if first == nil || rank(k) < rank(first) {
			first = k
		}
	}
	if first == nil || (first.Kind != schema.Primary && first.Kind != schema.Unique) || len(first.Parts) != 1 || first.Parts[0].Expr != nil {
		return colRef{}, false
	}
	col := r.table.Column(first.Parts[0].Column)
	if col == nil || !col.NotNull {
		return colRef{}, false
	}
	switch col.Type.Name {
	case "tinyint", "smallint", "mediumint", "int", "bigint", "year", "bit":
	default:
		return colRef{}, false
	}
	return colRef{rel: r, col: col, c: Column{Name: col.Name, Type: col.Type, Known: true, Nullable: r.nullable, base: col, baseTable: r.table}}, true
}

// scope is the relations a name resolves against: the query's own, then, for a
// correlated subquery, the enclosing queries'. ctes are the common table expressions in
// force, by name, for the FROM clauses of this query and its subqueries.
type scope struct {
	rels  []relation
	outer *scope
	ctes  []relation
	// tag identifies the SELECT block across the copies of its scope (a block without a
	// FROM has no relation to stand for it), for lists
	tag *byte
	// items are the block's select-list columns once typed: HAVING resolves a name that
	// is not a table column against them (a MySQL extension).
	items []Column
	// facts is the block's record for the contracts (One and the obligations), filled by
	// querySpecification; nil until then.
	facts *facts.Scope
	// joins are the join conditions of the FROM clause, for the facts: which conjuncts
	// hold, and for an outer join which leaves they are allowed to restrict.
	joins []joinCond
	// kids collects the nested blocks analyzed under this one (derived tables, the
	// subqueries of its conditions and select list): the facts' Children. Shared by the
	// copies of the scope; nil when the block does not record them.
	kids *[]*facts.Scope
	// nnMarks are the columns the block's conjuncts reject NULL for (a comparison, LIKE,
	// BETWEEN, IN, IS NOT NULL with the column as a direct argument), each with the
	// nullable side of the outer join whose ON says so (nil: the WHERE), for the functional
	// dependencies (fullgroup.go)
	nnMarks []nnMark
	// ndJoins are the nullable sides of the outer joins whose ON is not deterministic:
	// its equalities give no dependency
	ndJoins [][]int
	// info is the block's record for the ONLY_FULL_GROUP_BY and DISTINCT checks, set by
	// querySpecification
	info *blockInfo
	// itemList is the select list as written, for the clauses that name its aliases
	itemList mysqlast.List
	// merged are the columns a USING / NATURAL join coalesces: one name over the leaves
	// that carry it. An unqualified reference to it is not ambiguous, and `*` lists it once,
	// first (the left side's).
	merged []mergedCol
	// wherePreds is how many of facts.Preds, and whereNN how many of nnMarks, came from
	// WHERE and JOIN ON (block() sets both right after those are recorded, before HAVING's
	// own are folded in): fullgroup.go's fdClosure reads only these prefixes, since a WHERE
	// (or an inner join's ON) equality or non-null mark genuinely extends ONLY_FULL_GROUP_BY's
	// functional dependency (measured: `WHERE u = 1 GROUP BY a` and `WHERE u IS NOT NULL
	// GROUP BY u` both accept a nonaggregated column that depends on u), but the same
	// predicate written in HAVING does not (measured: `GROUP BY u HAVING u IS NOT NULL` and
	// `GROUP BY a, u HAVING u = 1` still require every nonaggregated select-list column to
	// depend on the GROUP BY columns alone) -- HAVING runs after grouping, so it restricts
	// which groups come out, not what the server may assume about a row while deciding
	// whether the query is well-formed.
	wherePreds, whereNN int
}

// mergedCol is one coalesced join column.
type mergedCol struct {
	name   string
	leaves []int
}

// mergedLeaves reports whether every leaf in leaves is one a merged column of that name
// spans (the reference is to the coalesced column).
func (sc *scope) mergedLeaves(name string, leaves []int) bool {
	for _, m := range sc.merged {
		if !strings.EqualFold(m.name, name) {
			continue
		}
		all := true
		for _, l := range leaves {
			found := false
			for _, ml := range m.leaves {
				found = found || ml == l
			}
			all = all && found
		}
		if all {
			return true
		}
	}
	return false
}

// nnMark is a column a conjunct rejects NULL for; restrict is the outer join's nullable
// side when the conjunct is its ON (nil for the WHERE).
type nnMark struct {
	col      facts.ColRef
	restrict []int
}

// child records a nested block's facts under this one.
func (sc *scope) child(fs *facts.Scope) {
	if sc != nil && sc.kids != nil && fs != nil {
		*sc.kids = append(*sc.kids, fs)
	}
}

// joinCond is one join's condition as the facts see it.
type joinCond struct {
	on    mysqlast.Value // the ON expression, nil for USING
	using []string       // USING (a, b)
	left  []int          // leaf indices of the two sides
	right []int
	kind  string // JTT_INNER, JTT_LEFT, JTT_RIGHT ...
}

// derived makes a scope for a nested query: its relations start empty, the enclosing
// scope is outer, and the enclosing CTEs stay visible.
func (sc *scope) derived() scope {
	if sc == nil {
		return scope{}
	}
	return scope{outer: sc, ctes: sc.ctes}
}

func (a *analyzer) statement(v mysqlast.Value) error {
	n, ok := v.(*mysqlast.Node)
	if !ok {
		return fmt.Errorf("analyze: statement not understood: %s", mysqlast.Sprint(v))
	}
	switch n.Class {
	case "PT_select_stmt":
		if err := a.selectStmt(n); err != nil {
			return err
		}
		if len(selectInto(n)) > 0 {
			// INTO @var / INTO var: the row goes into the variables and no result set
			// reaches the client (measured on mysqld 8.4, the corpus probe's `SELECT ...
			// INTO @v` statements, in the trailing and the in-query positions alike); a
			// body's walkSelect keeps the columns to match them against the targets
			a.columns = nil
		}
		return nil
	case "PT_insert":
		return a.insert(n)
	case "PT_update":
		if err := a.update(n); err != nil {
			return err
		}
		return a.targetInSubquery("UPDATE")
	case "PT_delete":
		if err := a.delete(n); err != nil {
			return err
		}
		return a.targetInSubquery("DELETE")
	case "PT_call":
		return a.callStmt(n)
	case "lock_tables":
		return a.lockTables(n)
	case "unlock":
		return a.unlockTables(n)
	case "PT_load_table":
		return a.loadData(n)
	}
	return fmt.Errorf("analyze: %s is not supported yet", strings.TrimPrefix(n.Class, "PT_"))
}

// targetInSubquery is the server's refusal of an UPDATE / DELETE whose subquery reads the
// table it writes: 1093 "You can't specify target table 't' for update in FROM clause" for
// a subquery naming the target itself (in WHERE, EXISTS or IN alike), 1443 "The definition
// of table 'v' prevents operation UPDATE on table 't'." for one reading a view over it
// (measured on 8.4, found by x/stmtprobe). A derived table over the target is
// materialized and allowed (measured), as is an INSERT ... SELECT from its own table; a
// scalar subquery in SET is not recorded in the facts and goes unchecked here.
func (a *analyzer) targetInSubquery(op string) error {
	f := a.facts
	if f == nil || f.Top == nil {
		return nil
	}
	var target, alias string
	for _, l := range f.Top.Leaves {
		if l.Role == facts.Target && l.Kind == facts.Table {
			target, alias = l.Table, l.Alias
			break
		}
	}
	if target == "" {
		return nil
	}
	var visit func(sc *facts.Scope, top bool) error
	visit = func(sc *facts.Scope, top bool) error {
		if sc == nil {
			return nil
		}
		derived := map[*facts.Scope]bool{}
		for _, l := range sc.Leaves {
			if l.Kind == facts.Derived || l.Kind == facts.CTE {
				derived[l.Body] = true // materialized: allowed, and not followed
				continue
			}
			if top {
				continue
			}
			switch l.Kind {
			case facts.Table:
				if a.s.Table(l.Table) == a.s.Table(target) {
					return &Error{Message: fmt.Sprintf("You can't specify target table '%s' for update in FROM clause", alias), Code: 1093, Position: int(l.Position)}
				}
			case facts.View:
				if a.viewReads(l.Body, target) {
					return &Error{Message: fmt.Sprintf("The definition of table '%s' prevents operation %s on table '%s'.", l.Alias, op, alias), Code: 1443, Position: int(l.Position)}
				}
			}
		}
		for _, p := range sc.Preds {
			if err := visit(p.Sub, false); err != nil {
				return err
			}
		}
		for _, ch := range sc.Children {
			if derived[ch] {
				continue
			}
			if err := visit(ch, false); err != nil {
				return err
			}
		}
		return nil
	}
	return visit(f.Top, true)
}

// viewReads: a view body (or a view it reads in turn) names table.
func (a *analyzer) viewReads(body *facts.Scope, table string) bool {
	if body == nil {
		return false
	}
	for _, l := range body.Leaves {
		if l.Kind == facts.Table && a.s.Table(l.Table) == a.s.Table(table) || l.Kind == facts.View && a.viewReads(l.Body, table) {
			return true
		}
	}
	for _, p := range body.Preds {
		if a.viewReads(p.Sub, table) {
			return true
		}
	}
	for _, ch := range body.Children {
		if a.viewReads(ch, table) {
			return true
		}
	}
	return false
}

func (a *analyzer) selectStmt(n *mysqlast.Node) error {
	a.facts = &facts.Facts{Kind: facts.Select}
	cols, err := a.queryExpression(n.Arg("qe"), nil)
	if err != nil {
		return err
	}
	a.columns = cols
	if qe, ok := n.Arg("qe").(*mysqlast.Node); ok && limitOne(qe.Arg("limit")) {
		a.facts.AtMostOne = true
	}
	if selectIntoFile(n) {
		// INTO OUTFILE / DUMPFILE: the rows go to a file on the server, none to the
		// client (measured: the statement returns no result set)
		a.columns = nil
	}
	return nil
}

func (a *analyzer) insert(n *mysqlast.Node) error {
	rel, err := a.target(arg(n, "table_ident", 3), nil, nil)
	if err != nil {
		return err
	}
	if rel.table == nil {
		return fmt.Errorf("analyze: INSERT into a view is not supported yet")
	}
	rel.target = true
	w := &write{kind: facts.Insert, table: rel.table, ignore: isTrue(arg(n, "ignore", 2)), inserted: map[string]bool{}, replace: isTrue(n.Arg("is_replace"))}
	a.write = w
	// the column list names the targets; without one the row lists every column in order
	var targets []*schema.Column
	if cols, ok := arg(n, "column_list", 5).(mysqlast.List); ok && len(cols) > 0 {
		for _, c := range cols {
			a.assigning = true
			col, err := a.targetColumn(rel, c, "field list")
			a.assigning = false
			if err != nil {
				return err
			}
			targets = append(targets, col)
		}
	} else {
		targets = rel.table.Columns
	}
	for _, c := range targets {
		w.inserted[c.Name] = true
	}
	a.facts = &facts.Facts{Kind: facts.Insert, Top: &facts.Scope{At: -1, Leaves: []facts.Leaf{a.leafFacts(*rel)}}}
	var values []facts.Term
	dupScope := scope{rels: []relation{*rel}}
	a.insertTarget = rel
	defer func() { a.insertTarget = nil }()
	if q := arg(n, "insert_query_expression", 7); q != nil {
		// INSERT ... SELECT: the query's columns feed the targets in order; a bare
		// placeholder in its select list takes the target's type
		cols, body, block, err := a.queryExpressionBlock(q, nil)
		if err != nil {
			return err
		}
		if block != nil && !(block.info != nil && block.info.aggregated) {
			// ON DUPLICATE KEY UPDATE sees the SELECT's tables beside the target (measured:
			// `INSERT INTO t SELECT ... FROM u ... ON DUPLICATE KEY UPDATE c = u.c` runs, an
			// unqualified name both have is 1052) -- unless the SELECT is grouped or
			// aggregated, when only the target is in view (`... GROUP BY a ON DUPLICATE KEY
			// UPDATE k = a` is 1054, corpus). A select alias is never visible to it, and
			// VALUES(c) names the target's column alone (Item_insert_value in node)
			dupScope = *block
			dupScope.rels = append(append([]relation{}, block.rels...), *rel) // the target last: the join's merged columns keep their leaf indexes
		}
		if len(cols) != len(targets) {
			return &Error{Message: "Column count doesn't match value count at row 1", Code: 1136, Position: -1}
		}
		w.query = true
		for i, c := range cols {
			w.values = append(w.values, assignment{col: targets[i], nullable: c.Nullable || !c.Known})
			values = append(values, facts.Term{Kind: facts.Known, Text: "?"})
			if a.routine == nil && a.trig == nil && c.Known {
				if e, viol := a.geometryStore(rel.table, targets[i], nil, typed{typ: c.Type, known: true, nullable: c.Nullable}); e != nil {
					e.Position = a.ph.Back(nodeStart(q))
					return e
				} else if viol != nil {
					a.storeRaised = append(a.storeRaised, *viol)
				}
			}
		}
		if qe, ok := q.(*mysqlast.Node); ok {
			if body, ok := qe.Arg("body").(*mysqlast.Node); ok && body.Class == "PT_query_specification" {
				items, _ := body.Arg("item_list").(mysqlast.List)
				for i, item := range items {
					if it, ok := item.(*mysqlast.Node); ok && it.Class == "PTI_expr_with_alias" && isParam(it.Arg("expr")) && i < len(targets) {
						a.setParam(it.Arg("expr"), targets[i].Type)
						a.noteParamSource(it.Arg("expr"), rel.table, targets[i], true)
					}
				}
			}
		}
		if body != nil {
			a.facts.Source = body
			a.facts.Top.Children = append(a.facts.Top.Children, body)
		}
	}
	rows, _ := arg(n, "row_value_list", 6).(mysqlast.List)
	if len(rows) > 0 && arg(n, "insert_query_expression", 7) == nil {
		// `VALUES ()` (each row empty) inserts a row of defaults whatever the column list
		// says: no column is assigned, so no 1136 (measured on mysqld 8.4, the corpus
		// probe's `INSERT INTO t1 VALUES ()` and `INSERT INTO t1 () VALUES (), ()`); an
		// omitted NOT NULL column without a default is the usual 1364
		empty := true
		for _, row := range rows {
			if vals, _ := row.(mysqlast.List); len(vals) > 0 {
				empty = false
			}
		}
		if empty {
			targets = nil
			w.inserted = map[string]bool{}
		}
	}
	w.rows = len(rows)
	switch {
	case arg(n, "insert_query_expression", 7) != nil:
	case len(rows) == 1:
		a.facts.Top.Single = true
	case len(rows) > 1:
		a.facts.Top.Many = fmt.Sprintf("VALUES has %d rows", len(rows))
	}
	for ri, row := range rows {
		vals, _ := row.(mysqlast.List)
		if len(vals) != len(targets) {
			return &Error{Message: "Column count doesn't match value count at row 1", Code: 1136, Position: -1}
		}
		for i, v := range vals {
			a.storeRow = ri + 1
			as, err := a.assign(scope{rels: []relation{*rel}}, rel.table, targets[i], v)
			a.storeRow = 0
			if err != nil {
				return err
			}
			w.values = append(w.values, as)
			if ri == 0 {
				values = append(values, a.storedTerm(scope{rels: []relation{*rel}}, targets[i], v))
			}
		}
	}
	a.facts.Writes = []facts.Write{a.writeFacts(facts.Insert, rel, targets, values)}
	if w.replace {
		// REPLACE deletes the colliding row before it inserts the new one (measured: an
		// AFTER DELETE trigger on the table fires) -- a second write, of Kind Delete, so
		// `require never on delete` and other OnDelete obligations see it (x/obligation's
		// writes() keys off facts.Write.Kind, not the statement's own top-level Kind).
		a.facts.Writes = append(a.facts.Writes, facts.Write{Table: rel.table.Name, Kind: facts.Delete, Position: int32(a.ph.Back(rel.pos))})
	}
	dupCols, _ := arg(n, "opt_on_duplicate_column_list", 10).(mysqlast.List)
	dupVals, _ := arg(n, "opt_on_duplicate_value_list", 11).(mysqlast.List)
	if len(dupCols) > 0 {
		w.onDuplicate = []assignment{}
	}
	var dupTargets []*schema.Column
	var dupTerms []facts.Term
	for i, c := range dupCols {
		a.assigning = true
		col, err := a.targetColumn(rel, c, "field list")
		a.assigning = false
		if err != nil {
			return err
		}
		if i < len(dupVals) {
			a.foldPerRow = true // the update runs per colliding row (fold.go)
			as, err := a.assign(dupScope, rel.table, col, dupVals[i])
			a.foldPerRow = false
			if err != nil {
				return err
			}
			w.onDuplicate = append(w.onDuplicate, as)
			dupTargets = append(dupTargets, col)
			dupTerms = append(dupTerms, a.storedTerm(dupScope, col, dupVals[i]))
		}
	}
	if len(dupTargets) > 0 {
		// ON DUPLICATE KEY UPDATE is a second write, of Kind Update, on the same row as
		// the INSERT branch would have targeted -- the same shape x/obligation already
		// has a case for on PostgreSQL's ON CONFLICT DO UPDATE (a branch reassigning a
		// pinned column needs its own WHERE-less write checked, since nothing in the
		// UPDATE branch itself fixes the row it moves).
		a.facts.Writes = append(a.facts.Writes, a.writeFacts(facts.Update, rel, dupTargets, dupTerms))
	}
	return nil
}

func (a *analyzer) update(n *mysqlast.Node) error {
	ctes, err := a.with(n.Arg("with_clause"), nil)
	if err != nil {
		return err
	}
	sc, err := a.from(n.Arg("join_table_list"), scope{ctes: ctes, kids: new([]*facts.Scope)})
	if err != nil {
		return err
	}
	cols, _ := n.Arg("column_list").(mysqlast.List)
	vals, _ := n.Arg("value_list").(mysqlast.List)
	type perTarget struct {
		rel      *relation
		table    *schema.Table
		assigned []*schema.Column
		values   []facts.Term
	}
	var targets []*perTarget
	w := &write{kind: facts.Update, ignore: isTrue(n.Arg("opt_ignore"))}
	a.write = w
	for i, c := range cols {
		a.assigning = true
		col, err := a.column(sc, c, "field list")
		a.assigning = false
		if err != nil {
			return err
		}
		if col.rel == nil {
			// the name resolved to a routine variable of the same name: a SET target is a
			// column, which the server resolves against the tables (the analyzer used to
			// fall over on the missing relation -- found by the corpus probe)
			name := identName(c)
			var found *relation
			for j := range sc.rels {
				if sc.rels[j].table != nil && sc.rels[j].table.Column(name) != nil {
					found = &sc.rels[j]
					break
				}
			}
			if found == nil {
				return &Error{Message: fmt.Sprintf("Unknown column '%s' in 'field list'", name), Code: 1054, Position: a.ph.Back(nodeStart(c))}
			}
			col = colRef{rel: found, col: found.table.Column(name)}
		}
		if col.col == nil {
			return &Error{Message: fmt.Sprintf("The target table %s of the UPDATE is not updatable", col.rel.alias), Code: 1288, Position: a.ph.Back(nodeStart(c))}
		}
		col.rel.target = true
		table := col.rel.table
		if table == nil {
			table = col.c.baseTable // an updatable view: the write reaches its base table
		}
		var tg *perTarget
		for _, t := range targets {
			if t.rel == col.rel {
				tg = t
			}
		}
		if tg == nil {
			tg = &perTarget{rel: col.rel, table: table}
			targets = append(targets, tg)
		}
		tg.assigned = append(tg.assigned, col.col)
		if i < len(vals) {
			a.foldPerRow = true // a SET expression runs per matched row (fold.go)
			as, err := a.assign(sc, table, col.col, vals[i])
			a.foldPerRow = false
			if err != nil {
				return err
			}
			tg.values = append(tg.values, a.storedTerm(sc, col.col, vals[i]))
			if w.table == nil {
				w.table = table
			}
			if table == w.table {
				w.values = append(w.values, as)
			} else {
				w.more = appendTarget(w.more, table, as)
			}
		}
	}
	if err := a.condition(sc, n.Arg("opt_where_clause"), "where clause"); err != nil {
		return err
	}
	if err := a.orderBy(n.Arg("opt_order_clause"), nil, &sc, "order clause"); err != nil {
		return err
	}
	if err := a.limit(n.Arg("opt_limit_clause")); err != nil {
		return err
	}
	a.facts = &facts.Facts{Kind: facts.Update, AtMostOne: limitOne(n.Arg("opt_limit_clause"))}
	for _, tg := range targets {
		if tg.rel.table != nil {
			a.facts.Writes = append(a.facts.Writes, a.writeFacts(facts.Update, tg.rel, tg.assigned, tg.values))
		}
	}
	a.facts.Top = a.block(&sc, n)
	a.facts.Top.Children = append(a.facts.Top.Children, cteBodies(ctes)...)
	return nil
}

func (a *analyzer) delete(n *mysqlast.Node) error {
	ctes, err := a.with(n.Arg("with_clause"), nil)
	if err != nil {
		return err
	}
	if list, ok := n.Arg("table_list").(mysqlast.List); ok && len(list) > 0 {
		return a.multiDelete(n, list, ctes)
	}
	rel, err := a.target(n.Arg("table_ident"), n.Arg("opt_table_alias"), &scope{ctes: ctes})
	if err != nil {
		return err
	}
	if rel.table == nil {
		return fmt.Errorf("analyze: DELETE from a view or a common table expression is not supported yet")
	}
	rel.target = true
	a.write = &write{kind: facts.Delete, table: rel.table, ignore: deleteIgnore(n.Arg("opt_delete_options"))}
	sc := scope{rels: []relation{*rel}, ctes: ctes, kids: new([]*facts.Scope)}
	if err := a.condition(sc, n.Arg("opt_where_clause"), "where clause"); err != nil {
		return err
	}
	if err := a.orderBy(n.Arg("opt_order_clause"), nil, &sc, "order clause"); err != nil {
		return err
	}
	if err := a.limit(n.Arg("opt_delete_limit_clause")); err != nil {
		return err
	}
	a.facts = &facts.Facts{Kind: facts.Delete, AtMostOne: limitOne(n.Arg("opt_delete_limit_clause")), Writes: []facts.Write{a.writeFacts(facts.Delete, rel, nil, nil)}}
	a.facts.Top = a.block(&sc, n)
	a.facts.Top.Children = append(a.facts.Top.Children, cteBodies(ctes)...)
	return nil
}

// deleteIgnore reads IGNORE among DELETE's options.
func deleteIgnore(v mysqlast.Value) bool {
	switch x := v.(type) {
	case mysqlast.Flags:
		for _, f := range x {
			if strings.Contains(strings.ToUpper(string(f)), "IGNORE") {
				return true
			}
		}
		return false
	case mysqlast.List:
		for _, e := range x {
			if deleteIgnore(e) {
				return true
			}
		}
	case *mysqlast.Struct:
		return isTrue(x.Fields["ignore"]) || isTrue(x.Fields["opt_ignore"])
	case *mysqlast.Node:
		return isTrue(x.Arg("ignore")) || isTrue(x.Arg("opt_ignore"))
	}
	return strings.Contains(strings.ToUpper(str(v)), "IGNORE") // the options come as "DELETE_IGNORE|..."
}

// cteBodies lists the recorded bodies of a WITH clause's items.
func cteBodies(ctes []relation) []*facts.Scope {
	var out []*facts.Scope
	for _, c := range ctes {
		if c.body != nil {
			out = append(out, c.body)
		}
	}
	return out
}

// arg reads a named argument, or the positional one when the node carries no names (a
// hook that built the node by position; PT_insert's positional form, INSERT ... SET, has
// no opt_hints, so its indices are the named ones less one from table_ident on).
func arg(n *mysqlast.Node, name string, i int) mysqlast.Value {
	if len(n.Names) > 0 {
		return n.Arg(name)
	}
	if i < len(n.Args) {
		return n.Args[i]
	}
	return nil
}

// multiDelete types `DELETE t1, t2 FROM ... JOIN ...` and `DELETE FROM t1, t2 USING ...`:
// the joined tables are the scope, the listed names (aliases of the join) the targets.
func (a *analyzer) multiDelete(n *mysqlast.Node, list mysqlast.List, ctes []relation) error {
	sc, err := a.from(n.Arg("join_table_list"), scope{ctes: ctes, kids: new([]*facts.Scope)})
	if err != nil {
		return err
	}
	w := &write{kind: facts.Delete, ignore: deleteIgnore(n.Arg("opt_delete_options"))}
	a.write = w
	a.facts = &facts.Facts{Kind: facts.Delete}
	for _, item := range list {
		ti, ok := item.(*mysqlast.Node)
		if !ok {
			return fmt.Errorf("analyze: DELETE target not understood: %s", mysqlast.Sprint(item))
		}
		name := str(ti.Arg("table"))
		var rel *relation
		for i := range sc.rels {
			if strings.EqualFold(sc.rels[i].alias, name) {
				rel = &sc.rels[i]
			}
		}
		if rel == nil {
			return &Error{Message: fmt.Sprintf("Unknown table '%s' in MULTI DELETE", name), Code: 1109, Position: a.ph.Back(ti.Start)}
		}
		if rel.table == nil {
			return &Error{Message: fmt.Sprintf("The target table %s of the DELETE is not updatable", rel.alias), Code: 1288, Position: a.ph.Back(ti.Start)}
		}
		rel.target = true
		if w.table == nil {
			w.table = rel.table
		} else {
			w.more = append(w.more, moreTarget{table: rel.table})
		}
		a.facts.Writes = append(a.facts.Writes, a.writeFacts(facts.Delete, rel, nil, nil))
	}
	if err := a.condition(sc, n.Arg("opt_where_clause"), "where clause"); err != nil {
		return err
	}
	a.facts.Top = a.block(&sc, n)
	a.facts.Top.Children = append(a.facts.Top.Children, cteBodies(ctes)...)
	return nil
}

// from builds the scope of a FROM / UPDATE table list, starting from sc (the enclosing
// scope and the CTEs in force).
func (a *analyzer) from(v mysqlast.Value, sc scope) (scope, error) {
	list, _ := v.(mysqlast.List)
	for _, t := range list {
		if err := a.tableRef(&sc, t, false); err != nil {
			return sc, err
		}
	}
	return sc, nil
}

func (a *analyzer) tableRef(sc *scope, v mysqlast.Value, nullable bool) error {
	n, ok := v.(*mysqlast.Node)
	if !ok {
		return fmt.Errorf("analyze: table reference not understood: %s", mysqlast.Sprint(v))
	}
	switch n.Class {
	case "PT_table_factor_table_ident":
		rel, err := a.target(n.Arg("table_ident"), n.Arg("opt_table_alias"), sc)
		if err != nil {
			return err
		}
		rel.nullable = nullable
		return a.addRelation(sc, rel, n.Start)
	case "PT_derived_table":
		// (SELECT ...) AS alias [(col, ...)]: the columns are the query's, renamed by the
		// list when given; LATERAL sees the relations to its left
		alias := str(n.Arg("table_alias"))
		if alias == "" {
			return &Error{Message: "Every derived table must have its own alias", Code: 1248, Position: a.ph.Back(n.Start)}
		}
		outer := sc.outer
		if isTrue(n.Arg("lateral")) {
			outer = sc
		}
		cols, body, err := a.subqueryFacts(n.Arg("subquery"), outer)
		if err != nil {
			return err
		}
		merged := mergeable(subqueryExpression(n.Arg("subquery")))
		if !merged {
			cols = materialized(cols, subqueryExpression(n.Arg("subquery")))
		}
		names, _ := n.Arg("column_names").(mysqlast.List)
		if cols, err = renamed(cols, names, alias, a.ph.Back(n.Start)); err != nil {
			return err
		}
		sc.child(body)
		return a.addRelation(sc, &relation{alias: alias, cols: cols, nullable: nullable, body: body, merged: merged}, n.Start)
	case "PT_joined_table_on", "PT_joined_table_using", "PT_cross_join":
		jt := str(n.Arg("type"))
		left, right := nullable, nullable
		switch {
		case strings.Contains(jt, "LEFT"):
			right = true
		case strings.Contains(jt, "RIGHT"):
			left = true
		}
		before := len(sc.rels)
		if err := a.tableRef(sc, n.Arg("tab1_node"), left); err != nil {
			return err
		}
		mid := len(sc.rels)
		if err := a.tableRef(sc, n.Arg("tab2_node"), right); err != nil {
			return err
		}
		jc := joinCond{left: indices(before, mid), right: indices(mid, len(sc.rels)), kind: jt}
		if n.Class == "PT_joined_table_on" {
			jc.on = n.Arg("on")
			sc.joins = append(sc.joins, jc)
			return a.condition(*sc, n.Arg("on"), "on clause")
		}
		fields, _ := n.Arg("using_fields").(mysqlast.List)
		if strings.Contains(jt, "NATURAL") {
			// NATURAL JOIN: USING over every column name both sides have
			fields = nil
			for _, l := range jc.left {
				for _, c := range sc.rels[l].columns() {
					if _, ok := a.colIn(sc, jc.right, c.Name); ok {
						fields = append(fields, mysqlast.Token{Text: c.Name, Value: c.Name})
					}
				}
			}
		}
		if fields != nil {
			// USING (c): c must be a column of both sides (not ambiguous: it names the pair)
			for _, f := range fields {
				name := str(f)
				if _, okl := a.colIn(sc, jc.left, name); !okl {
					return &Error{Message: fmt.Sprintf("Unknown column '%s' in 'from clause'", name), Code: 1054, Position: a.ph.Back(nodeStart(f))}
				}
				if _, okr := a.colIn(sc, jc.right, name); !okr {
					return &Error{Message: fmt.Sprintf("Unknown column '%s' in 'from clause'", name), Code: 1054, Position: a.ph.Back(nodeStart(f))}
				}
				jc.using = append(jc.using, name)
				leaves := []int{}
				for _, i := range append(append([]int{}, jc.left...), jc.right...) {
					if _, ok := sc.rels[i].column(name); ok {
						leaves = append(leaves, i)
					}
				}
				sc.merged = append(sc.merged, mergedCol{name: name, leaves: leaves})
			}
			sc.joins = append(sc.joins, jc)
		}
		return nil
	case "PT_table_factor_joined_table":
		// (a JOIN b ON ...) as one side of a join: the nest is its joins
		return a.tableRef(sc, n.Arg("joined_table"), nullable)
	case "PT_table_reference_list_parens":
		list, _ := n.Arg("table_list").(mysqlast.List)
		for _, t := range list {
			if err := a.tableRef(sc, t, nullable); err != nil {
				return err
			}
		}
		return nil
	}
	return fmt.Errorf("analyze: %s is not supported yet", n.Class)
}

// indices lists the integers in [from, to).
func indices(from, to int) []int {
	out := make([]int, 0, to-from)
	for i := from; i < to; i++ {
		out = append(out, i)
	}
	return out
}

// addRelation puts rel into sc, rejecting a second relation under the same alias.
func (a *analyzer) addRelation(sc *scope, rel *relation, at int) error {
	rel.pos = at
	for _, r := range sc.rels {
		if strings.EqualFold(r.alias, rel.alias) {
			return &Error{Message: fmt.Sprintf("Not unique table/alias: '%s'", rel.alias), Code: 1066, Position: a.ph.Back(at)}
		}
	}
	sc.rels = append(sc.rels, *rel)
	return nil
}

// target resolves a Table_ident, under alias when given: a common table expression in
// force (the innermost wins, as in the server), else a base table, else a view (analyzed
// on the spot, its columns are its query's).
func (a *analyzer) target(ident, alias mysqlast.Value, sc *scope) (*relation, error) {
	n, ok := ident.(*mysqlast.Node)
	if !ok || n.Class != "Table_ident" {
		return nil, fmt.Errorf("analyze: table name not understood: %s", mysqlast.Sprint(ident))
	}
	name := str(n.Arg("table"))
	if name == "" && len(n.Args) > 0 {
		name = str(n.Args[len(n.Args)-1])
	}
	var rel *relation
	if sc != nil && str(n.Arg("db")) == "" {
		for i := len(sc.ctes) - 1; i >= 0; i-- {
			if strings.EqualFold(sc.ctes[i].alias, name) {
				r := sc.ctes[i]
				r.cte = true
				rel = &r
				break
			}
		}
	}
	if rel == nil {
		if a.refRels == nil {
			a.refRels = map[string]bool{}
		}
		a.refRels[strings.ToLower(name)] = true
		if t := a.s.Table(name); t != nil {
			rel = &relation{alias: t.Name, table: t}
		} else if v := a.s.View(name); v != nil {
			cols, body, err := a.view(v)
			if err != nil {
				return nil, err
			}
			// a view is merged into the query unless it says TEMPTABLE or its query cannot be
			// merged; a merged view's plain column references are updatable
			rel = &relation{alias: v.Name, cols: cols, updatable: true, view: v.Name, body: body, merged: true}
			if v.Algorithm == "TEMPTABLE" || !mergeable(v.Query) {
				rel.cols, rel.updatable, rel.merged = materialized(cols, v.Query), false, false
			}
		} else {
			return nil, &Error{Message: fmt.Sprintf("Table '%s' doesn't exist", name), Code: 1146, Position: a.ph.Back(n.Start)}
		}
	}
	if s := str(alias); s != "" {
		rel.alias = s
	}
	return rel, nil
}

// view types a view's query, in a scope of its own (a view sees no CTE and no outer query),
// and returns its block's facts for the proof.
func (a *analyzer) view(v *schema.View) ([]Column, *facts.Scope, error) {
	if a.views[v.Name] {
		return nil, nil, fmt.Errorf("analyze: view %s refers to itself", v.Name)
	}
	if a.views == nil {
		a.views = map[string]bool{}
	}
	a.views[v.Name] = true
	defer delete(a.views, v.Name)
	waived := v.Waived
	if waived == nil {
		waived = map[string][]string{} // the view's own, not the reading statement's
	}
	a.viewWaived = append(a.viewWaived, waived)
	defer func() { a.viewWaived = a.viewWaived[:len(a.viewWaived)-1] }()
	// the view's placeholders, if any, are not ours: keep the statement's parameter table;
	// its text is the definition's (the facts' opaque predicates read their text from it)
	saved, savedText, savedPh := a.params, a.text, a.ph
	a.params = nil
	a.text, a.ph = v.Definition, identityMap(v.Definition)
	defer func() { a.params, a.text, a.ph = saved, savedText, savedPh }()
	a.depth++
	cols, body, err := a.queryExpressionFacts(v.Query, nil)
	a.depth--
	if err != nil {
		if e, ok := err.(*Error); ok {
			return nil, nil, fmt.Errorf("analyze: view %s: %s (MySQL error %d)", v.Name, e.Message, e.Code)
		}
		return nil, nil, fmt.Errorf("analyze: view %s: %w", v.Name, err)
	}
	var names mysqlast.List
	for _, c := range v.Columns {
		names = append(names, c)
	}
	cols, err = renamed(cols, names, v.Name, -1)
	clearPositions(body) // offsets into the view's definition mean nothing to the statement
	return cols, body, err
}

// identityMap is the placeholder map of a text without placeholders.
func identityMap(text string) placeholder.Map {
	_, ph := placeholder.Rewrite(text)
	return ph
}

// renamed applies a derived relation's column list: the same count, the new names.
func renamed(cols []Column, names mysqlast.List, alias string, at int) ([]Column, error) {
	if len(names) == 0 {
		return cols, nil
	}
	if len(names) != len(cols) {
		return nil, &Error{Message: fmt.Sprintf("View's SELECT and view's field list have different column counts"), Code: 1353, Position: at}
	}
	out := make([]Column, len(cols))
	for i, c := range cols {
		c.Name = str(names[i])
		out[i] = c
	}
	return out, nil
}

// targetColumn resolves a column name against one relation (INSERT's column list).
func (a *analyzer) targetColumn(rel *relation, v mysqlast.Value, where string) (*schema.Column, error) {
	ref, err := a.column(scope{rels: []relation{*rel}}, v, where)
	if err != nil {
		return nil, err
	}
	if ref.col == nil {
		// resolved to something other than a column of the target table: a routine
		// variable of the same name (a body's INSERT INTO t (v) names the column, which the
		// server resolves against the table; the analyzer's lookup let the variable shadow
		// it and used to fall over on the nil column -- found by the corpus probe)
		if rel.table != nil {
			if c := rel.table.Column(identName(v)); c != nil {
				return c, nil
			}
		}
		return nil, &Error{Message: fmt.Sprintf("Unknown column '%s' in '%s'", identName(v), where), Code: 1054, Position: a.ph.Back(nodeStart(v))}
	}
	return ref.col, nil
}

// identName is the bare name a column reference node spells (its last part).
func identName(v mysqlast.Value) string {
	n, ok := v.(*mysqlast.Node)
	if !ok {
		return str(v)
	}
	for _, k := range []string{"field", "ident"} {
		if s := str(n.Arg(k)); s != "" {
			return s
		}
	}
	return mysqlast.Sprint(v)
}

// colRef is a resolved column reference: the relation, the schema column when the
// relation is a base table, and the column as the query sees it.
type colRef struct {
	rel *relation
	col *schema.Column
	c   Column
}

// column resolves a PTI_simple_ident_* node in sc. where names the clause the way MySQL's
// message does ("field list", "where clause", "on clause").
func (a *analyzer) column(sc scope, v mysqlast.Value, where string) (colRef, error) {
	n, ok := v.(*mysqlast.Node)
	if !ok {
		if s := str(v); s != "" { // a bare identifier token (USING (id))
			return a.lookup(sc, "", s, where, 0)
		}
		return colRef{}, fmt.Errorf("analyze: column reference not understood: %s", mysqlast.Sprint(v))
	}
	switch n.Class {
	case "PTI_simple_ident_ident", "PTI_simple_ident_nospvar_ident":
		name := str(n.Arg("ident"))
		if ref, ok := a.lookupVar(name); ok {
			return ref, nil
		}
		return a.lookup(sc, "", name, where, n.Start)
	case "PTI_simple_ident_q_2d":
		table, field := str(n.Arg("table")), str(n.Arg("field"))
		if a.trig != nil && (strings.EqualFold(table, "new") || strings.EqualFold(table, "old")) {
			col, nullable, err := a.trigRowColumn(table, field, n.Start, false)
			if err != nil {
				return colRef{}, err
			}
			return colRef{c: Column{Name: col.Name, Type: col.Type, Known: true, Nullable: nullable, base: col, baseTable: a.trigTable}}, nil
		}
		return a.lookup(sc, table, field, where, n.Start)
	case "PTI_simple_ident_q_3d":
		return a.lookup(sc, str(n.Arg("table")), str(n.Arg("field")), where, n.Start)
	}
	return colRef{}, fmt.Errorf("analyze: column reference not understood: %s", mysqlast.Sprint(v))
}

// lookup resolves table.field (table may be "") in sc: the innermost query whose
// relations know the name wins, an outer query is tried only when none of the inner one's
// do (a correlated reference); two matches at one level are ambiguous.
//
// The select list's aliases are visible where the server's is_item_list_lookup /
// resolve_in_select_list say (measured, TestNameResolutionServer): an unqualified name in
// the block's own HAVING, GROUP BY or ORDER BY expression resolves against them first
// (two different items of the name: 1052); a nested query placed in the select list,
// GROUP BY, HAVING or ORDER BY of an enclosing block sees that block's aliases too (not
// one placed in its WHERE or ON), the ones declared before it when placed in the select
// list -- a later one is 1247 "forward reference in item list" -- and an alias of an
// aggregate only from the nested query's HAVING, and never from a GROUP BY placement
// (1247 "reference to group function").
//
// `_rowid` names the table's first key when that key is unique over one NOT NULL integer
// column (relation.rowid), for a qualified reference or a level with a single table.
func (a *analyzer) lookup(sc scope, table, field, where string, at int) (colRef, error) {
	if table == "" && (where == "having clause" || where == "order clause" || where == "group statement") {
		ref, ok, ambiguous := a.lookupItem(sc, field)
		if ambiguous {
			return colRef{}, &Error{Message: fmt.Sprintf("Column '%s' in %s is ambiguous", field, where), Code: 1052, Position: a.ph.Back(at)}
		}
		if ok {
			return ref, nil
		}
	}
	for s := &sc; s != nil; s = s.outer {
		if s != &sc && table == "" {
			if ll := a.listOf(s); ll != nil {
				ref, found, err := a.outerItem(ll, *s, field, where, at)
				if err != nil {
					return colRef{}, err
				}
				if found {
					return ref, nil
				}
			}
		}
		var found []colRef
		var leaves []int
		leaf := 0
		for i := range s.rels {
			rel := &s.rels[i]
			if table != "" && !strings.EqualFold(rel.alias, table) {
				continue
			}
			ref, ok := rel.column(field)
			if !ok && strings.EqualFold(field, "_rowid") && (table != "" || len(s.rels) == 1) {
				ref, ok = rel.rowid()
			}
			if ok {
				found = append(found, ref)
				leaves = append(leaves, i)
				leaf = i
			}
		}
		if len(found) > 1 && table == "" && s.mergedLeaves(field, leaves) {
			found, leaf = found[:1], leaves[0] // the coalesced column of a USING / NATURAL join
		}
		switch len(found) {
		case 1:
			a.use(found[0], at)
			if s != &sc {
				a.outerRefs = append(a.outerRefs, outerRef{block: &s.rels[0], leaf: leaf, col: found[0].c.Name, at: at, where: where})
			}
			return found[0], nil
		case 0:
			continue
		}
		return colRef{}, &Error{Message: fmt.Sprintf("Column '%s' in %s is ambiguous", field, where), Code: 1052, Position: a.ph.Back(at)}
	}
	qualified := field
	if table != "" {
		qualified = table + "." + field
	}
	return colRef{}, &Error{Message: fmt.Sprintf("Unknown column '%s' in '%s'", qualified, where), Code: 1054, Position: a.ph.Back(at)}
}

// listLookup is a block whose clause resolves names against its select list: items are
// the select-list columns typed so far (all of them once the list is done), all the
// list's nodes, for the aliases not yet typed and for what each item is.
type listLookup struct {
	tag    *byte
	clause string // "field list", "group statement", "having clause", "order clause"
	items  []Column
	all    mysqlast.List
}

// listOf is the list lookup in force for the block s, nil when none of its clauses that
// see the select list is being typed.
func (a *analyzer) listOf(s *scope) *listLookup {
	if s.tag == nil {
		return nil
	}
	for i := len(a.lists) - 1; i >= 0; i-- {
		if a.lists[i].tag == s.tag {
			return &a.lists[i]
		}
	}
	return nil
}

// pushList / popList bracket the typing of a block's clause that sees its select list.
func (a *analyzer) pushList(sc *scope, clause string, items []Column, all mysqlast.List) {
	a.lists = append(a.lists, listLookup{tag: sc.tag, clause: clause, items: items, all: all})
}

func (a *analyzer) popList() { a.lists = a.lists[:len(a.lists)-1] }

// outerItem resolves an unqualified name of a nested query against an enclosing block's
// select list (the block's clause ll is being typed; where is the nested query's own
// clause): the alias found, a 1247 the server raises for it, or nothing.
func (a *analyzer) outerItem(ll *listLookup, s scope, field, where string, at int) (colRef, bool, error) {
	items := ll.items
	if ll.clause != "field list" {
		items = s.items
	}
	var found *Column
	for i := range items {
		if strings.EqualFold(items[i].Name, field) {
			found = &items[i]
			break
		}
	}
	if found == nil {
		if ll.clause == "field list" {
			for _, it := range ll.all {
				if ewa, ok := it.(*mysqlast.Node); ok && ewa.Class == "PTI_expr_with_alias" && strings.EqualFold(str(ewa.Arg("alias")), field) {
					return colRef{}, false, &Error{Message: fmt.Sprintf("Reference '%s' not supported (forward reference in item list)", field), Code: 1247, Position: a.ph.Back(at)}
				}
			}
		}
		return colRef{}, false, nil
	}
	if aliasOfWindow(ll.all, field) {
		return colRef{}, false, &Error{Message: fmt.Sprintf("You cannot use the alias '%s' of an expression containing a window function in this context.'", field), Code: 3594, Position: a.ph.Back(at)}
	}
	if aliasOfAggregate(ll.all, field) && (where != "having clause" || ll.clause == "group statement") {
		return colRef{}, false, &Error{Message: fmt.Sprintf("Reference '%s' not supported (reference to group function)", field), Code: 1247, Position: a.ph.Back(at)}
	}
	return colRef{c: *found}, true, nil
}

// use records a resolved reference to a table's or a view's column, once, in order of
// first appearance; a reference inside a view's body is the view's, not the statement's.
func (a *analyzer) use(ref colRef, at int) {
	inView := len(a.views) > 0 && !(a.definingView != "" && len(a.views) == 1 && a.views[a.definingView])
	if inView || ref.rel == nil {
		return
	}
	table := ref.rel.view
	if ref.rel.table != nil {
		table = ref.rel.table.Name
	}
	if table == "" {
		return
	}
	key := table + "." + ref.c.Name
	if a.useIdx == nil {
		a.useIdx = map[string]int{}
		a.readUses = map[string]bool{}
	}
	if !a.assigning {
		a.readUses[key] = true
	}
	if i, ok := a.useIdx[key]; ok {
		a.uses[i].Assigned = !a.readUses[key]
		if p := int32(a.ph.Back(at)); p < a.uses[i].Position {
			a.uses[i].Position = p // the select list is typed after FROM and WHERE: first appearance is by position
		}
		return
	}
	a.useIdx[key] = len(a.uses)
	a.uses = append(a.uses, facts.Use{Table: table, Column: ref.c.Name, Position: int32(a.ph.Back(at)), Assigned: !a.readUses[key]})
}

// lookupItem resolves a name against the block's typed select list (HAVING, GROUP BY and
// ORDER BY expressions): the item's column, with no schema column behind it. As
// find_item_in_list does, an item written with the alias wins over an unaliased column of
// the name; two aliased items, or two unaliased ones that are not the same table column
// of the same relation, are ambiguous.
func (a *analyzer) lookupItem(sc scope, name string) (ref colRef, ok, ambiguous bool) {
	var found *Column
	for aliased := true; ; aliased = false {
		for i := range sc.items {
			c := &sc.items[i]
			if c.aliased != aliased || !strings.EqualFold(c.Name, name) {
				continue
			}
			if found != nil {
				if found.base == nil || found.base != c.base || found.leaf1 != c.leaf1 {
					return colRef{}, false, true
				}
				continue
			}
			found = c
		}
		if found != nil || !aliased {
			break
		}
	}
	if found == nil {
		return colRef{}, false, false
	}
	return colRef{c: *found}, true, false
}

// items types the select list.
func (a *analyzer) items(sc scope, v mysqlast.Value) ([]Column, error) {
	var out []Column
	list, _ := v.(mysqlast.List)
	for _, item := range list {
		n, ok := item.(*mysqlast.Node)
		if !ok {
			return nil, fmt.Errorf("analyze: select item not understood: %s", mysqlast.Sprint(item))
		}
		switch n.Class {
		case "Item_asterisk":
			table := str(arg(n, "opt_table_name", 1))
			matched := false
			seen := map[string]bool{} // the coalesced columns already listed
			if table == "" {
				for _, m := range sc.merged {
					if seen[strings.ToLower(m.name)] || len(m.leaves) == 0 {
						continue
					}
					rel := &sc.rels[m.leaves[0]]
					if c, ok := rel.column(m.name); ok {
						col := c.c
						col.leaf1, col.leafCol = m.leaves[0]+1, col.Name
						out = append(out, col)
						seen[strings.ToLower(m.name)] = true
					}
				}
			}
			for i := range sc.rels {
				rel := &sc.rels[i]
				if table != "" && !strings.EqualFold(rel.alias, table) {
					continue
				}
				matched = true
				for _, c := range rel.columns() {
					if table == "" && seen[strings.ToLower(c.Name)] && sc.mergedLeaves(c.Name, []int{i}) {
						continue
					}
					c.leaf1, c.leafCol = i+1, c.Name
					out = append(out, c)
				}
			}
			if !matched {
				if table != "" {
					return nil, &Error{Message: fmt.Sprintf("Unknown table '%s'", table), Code: 1051, Position: a.ph.Back(n.Start)}
				}
				return nil, &Error{Message: "No tables used", Code: 1096, Position: a.ph.Back(n.Start)}
			}
		case "PTI_expr_with_alias":
			expr := n.Arg("expr")
			t, err := a.expr(sc, expr, "field list")
			if err != nil {
				return nil, err
			}
			name := str(n.Arg("alias"))
			if name == "" {
				name = a.itemName(expr)
			}
			c := Column{Name: name, Type: t.typ, Known: t.known, Nullable: t.nullable, aliased: str(n.Arg("alias")) != ""}
			if ref, ok := a.plainColumn(sc, expr); ok {
				c.base, c.baseTable = ref.c.base, ref.c.baseTable
				if ref.rel != nil { // nil for a body-walk variable/NEW/OLD reference: no leaf to record
					for i := range sc.rels {
						if &sc.rels[i] == ref.rel || sc.rels[i].alias == ref.rel.alias {
							c.leaf1, c.leafCol = i+1, ref.c.Name
							break
						}
					}
				}
			}
			out = append(out, c)
		default:
			return nil, fmt.Errorf("analyze: select item not understood: %s", n.Class)
		}
		if ll := a.listOf(&sc); ll != nil && ll.clause == "field list" {
			ll.items = out // a nested query later in the list sees the items typed so far
		}
	}
	return out, nil
}

// plainColumn resolves expr when it is a bare column reference (already typed without
// error by the caller).
func (a *analyzer) plainColumn(sc scope, expr mysqlast.Value) (colRef, bool) {
	n, ok := expr.(*mysqlast.Node)
	if !ok {
		return colRef{}, false
	}
	switch n.Class {
	case "PTI_simple_ident_ident", "PTI_simple_ident_nospvar_ident", "PTI_simple_ident_q_2d", "PTI_simple_ident_q_3d":
		ref, err := a.column(sc, n, "field list")
		return ref, err == nil
	}
	return colRef{}, false
}

// itemName is the name MySQL gives an unaliased select item: the column's name for a
// column reference, else the expression's text.
func (a *analyzer) itemName(v mysqlast.Value) string {
	if n, ok := v.(*mysqlast.Node); ok {
		switch n.Class {
		case "PTI_simple_ident_ident", "PTI_simple_ident_nospvar_ident":
			return str(n.Arg("ident"))
		case "PTI_simple_ident_q_2d", "PTI_simple_ident_q_3d":
			return str(n.Arg("field"))
		case "PTI_text_literal_text_string", "PTI_text_literal_nchar_string", "PTI_text_literal_underscore_charset":
			if tok, ok := n.Arg("literal").(mysqlast.Token); ok {
				return tok.Value // a string literal is named by its value, without the quotes
			}
		}
		if n.Start >= 0 && n.End <= len(a.text) && n.Start < n.End {
			return a.text[n.Start:n.End]
		}
	}
	return ""
}

// condition types a WHERE / HAVING / ON clause for its errors and its parameters.
func (a *analyzer) condition(sc scope, v mysqlast.Value, where string) error {
	if v == nil {
		return nil
	}
	if n, ok := v.(*mysqlast.Node); ok && (n.Class == "PTI_where" || n.Class == "PTI_having") {
		v = n.Arg("expr")
	}
	a.condBases = append(a.condBases, len(a.parents))
	_, err := a.expr(sc, v, where)
	a.condBases = a.condBases[:len(a.condBases)-1]
	return err
}

// limit types LIMIT / OFFSET placeholders as bigint unsigned. SELECT and UPDATE / DELETE
// differ in shape: a PT_limit_clause with a limit_options struct, or the bare option.
func (a *analyzer) limit(v mysqlast.Value) error {
	n, ok := v.(*mysqlast.Node)
	if !ok {
		return nil
	}
	options := []mysqlast.Value{n}
	if opts, ok := n.Arg("limit_options").(*mysqlast.Struct); ok {
		options = []mysqlast.Value{opts.Fields["limit"], opts.Fields["opt_offset"]}
	}
	for _, opt := range options {
		if o, ok := opt.(*mysqlast.Node); ok && o.Class == "PTI_limit_option_param_marker" {
			a.setParam(o.Arg("param_marker"), schema.Type{Name: "bigint", Unsigned: true, Length: -1, Dec: -1})
		}
	}
	return nil
}

// assign types v, stored into col: a placeholder takes the column's type. The assignment
// says whether the value may be NULL (a placeholder always may; the checker drops the NOT
// NULL violation when the Go type cannot be nil). An expression the server rejects (an
// unknown column, 1054) is the statement's error.
func (a *analyzer) assign(sc scope, table *schema.Table, col *schema.Column, v mysqlast.Value) (assignment, error) {
	if isParam(v) {
		a.setParam(v, col.Type)
		a.noteParamSource(v, table, col, true)
		return assignment{col: col, nullable: true, param: a.ph.Number(nodeStart(v))}, nil
	}
	if n, ok := v.(*mysqlast.Node); ok && n.Class == "Item_default_value" {
		return assignment{col: col, nullable: col.Default == nil && !col.NotNull}, nil
	}
	t, err := a.expr(sc, v, "field list")
	if err != nil {
		return assignment{}, err
	}
	if a.routine == nil && a.trig == nil {
		if e, viol := a.geometryStore(table, col, v, t); e != nil {
			return assignment{}, e
		} else if viol != nil {
			a.storeRaised = append(a.storeRaised, *viol)
		}
	}
	if a.storeChecks(table, a.storeRow) {
		if e := a.literalStore(col, v, max(a.storeRow, 1)); e != nil {
			return assignment{}, e
		}
	}
	return assignment{col: col, nullable: !t.known || t.nullable}, nil
}

// storedTerm is the value stored into col as the facts spell it: a parameter, a literal,
// a value known before the statement runs, or an expression (Known "?"). DEFAULT is the
// column's default: a literal default is the value the row gets, any other default an
// expression the statement does not spell (Known "DEFAULT").
func (a *analyzer) storedTerm(sc scope, col *schema.Column, v mysqlast.Value) facts.Term {
	if n, ok := v.(*mysqlast.Node); ok && n.Class == "Item_default_value" {
		if col != nil && col.Default != nil {
			if d, ok := col.Default.(*mysqlast.Node); ok && literalClass(d.Class) {
				// constText (facts.go), not literalText: a Const must carry the same
				// tagged spelling termFacts gives every other literal, so a DEFAULT's
				// value compares equal to a literal written in the statement (Eq/state
				// comparisons in x/obligation/check.go read Const as plain text).
				if text := constText(d); text != "" {
					return facts.Term{Kind: facts.Const, Const: text}
				}
			}
		}
		return facts.Term{Kind: facts.Known, Text: "DEFAULT"}
	}
	if t, ok := a.termFacts(&sc, v); ok {
		return t
	}
	return facts.Term{Kind: facts.Known, Text: "?"}
}

// nodeStart is v's start offset in the text, -1 when it has none.
func nodeStart(v mysqlast.Value) int {
	switch x := v.(type) {
	case *mysqlast.Node:
		return x.Start
	case mysqlast.Token:
		return x.Start
	}
	return -1
}

// str reads a Token's value or a Const's text; "" for anything else.
func str(v mysqlast.Value) string {
	switch x := v.(type) {
	case mysqlast.Token:
		if x.Value != "" {
			return x.Value
		}
		return strings.Trim(x.Text, "`")
	case mysqlast.Const:
		return string(x)
	case string:
		return x
	}
	return ""
}
