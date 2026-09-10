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
	cst, err := mysqlparse.Parse(text, 0)
	if err != nil {
		if pe, ok := err.(*mysqlparse.Error); ok {
			return nil, &Error{Message: pe.Message, Code: 1064, Position: ph.Back(pe.Offset)}
		}
		return nil, err
	}
	root, err := mysqlast.Build(text, cst)
	if err != nil {
		return nil, err
	}
	a := &analyzer{s: s, text: text, ph: ph, params: make([]Param, ph.Count()), waived: obligation.StatementWaivers(sql)}
	if err := a.statement(root); err != nil {
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
	// outerRefs are the column references a nested query resolved in an enclosing block
	// (fullgroup.go reads them: a correlated reference is a column of the block it names)
	outerRefs []outerRef
	// blocks are the SELECT blocks typed so far, by their facts, for the functional
	// dependencies a derived table's body gives (fullgroup.go)
	blocks map[*facts.Scope]*blockInfo
	// fdConst says of an equality's known side (pred text + term text) whether the server
	// treats it as a constant for functional dependencies (a literal, not a parameter)
	fdConst map[string]bool
	// inHaving are the blocks whose HAVING is being typed: a nested query's unqualified
	// name may be one of their select aliases
	inHaving []*relation
	// subFacts are the subqueries' bodies by their PT_subquery node, for the EXISTS / IN
	// predicates; claimed are the bodies such a predicate carries (not Children then)
	subFacts map[*mysqlast.Node]*facts.Scope
	claimed  map[*facts.Scope]bool
	// paramSrc is the column each placeholder ($n, 1-based) met first
	paramSrc map[int]*ParamSource
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
}

// assignment is one value stored into a column.
type assignment struct {
	col      *schema.Column
	nullable bool
	param    int // the bare placeholder stored ($n), 0 otherwise
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

// scope is the relations a name resolves against: the query's own, then, for a
// correlated subquery, the enclosing queries'. ctes are the common table expressions in
// force, by name, for the FROM clauses of this query and its subqueries.
type scope struct {
	rels  []relation
	outer *scope
	ctes  []relation
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
		return a.selectStmt(n)
	case "PT_insert":
		return a.insert(n)
	case "PT_update":
		return a.update(n)
	case "PT_delete":
		return a.delete(n)
	}
	return fmt.Errorf("analyze: %s is not supported yet", strings.TrimPrefix(n.Class, "PT_"))
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
	w := &write{kind: facts.Insert, table: rel.table, ignore: isTrue(arg(n, "ignore", 2)), inserted: map[string]bool{}}
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
	if q := arg(n, "insert_query_expression", 7); q != nil {
		// INSERT ... SELECT: the query's columns feed the targets in order; a bare
		// placeholder in its select list takes the target's type
		cols, body, err := a.queryExpressionFacts(q, nil)
		if err != nil {
			return err
		}
		if len(cols) != len(targets) {
			return &Error{Message: "Column count doesn't match value count at row 1", Code: 1136, Position: -1}
		}
		w.query = true
		for i, c := range cols {
			w.values = append(w.values, assignment{col: targets[i], nullable: c.Nullable || !c.Known})
			values = append(values, facts.Term{Kind: facts.Known, Text: "?"})
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
			as := a.assign(scope{rels: []relation{*rel}}, rel.table, targets[i], v)
			w.values = append(w.values, as)
			if ri == 0 {
				values = append(values, a.storedTerm(scope{rels: []relation{*rel}}, v))
			}
		}
	}
	a.facts.Writes = []facts.Write{a.writeFacts(facts.Insert, rel, targets, values)}
	dupCols, _ := arg(n, "opt_on_duplicate_column_list", 10).(mysqlast.List)
	dupVals, _ := arg(n, "opt_on_duplicate_value_list", 11).(mysqlast.List)
	if len(dupCols) > 0 {
		w.onDuplicate = []assignment{}
	}
	for i, c := range dupCols {
		a.assigning = true
		col, err := a.targetColumn(rel, c, "field list")
		a.assigning = false
		if err != nil {
			return err
		}
		if i < len(dupVals) {
			w.onDuplicate = append(w.onDuplicate, a.assign(scope{rels: []relation{*rel}}, rel.table, col, dupVals[i]))
		}
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
	var assigned []*schema.Column
	var values []facts.Term
	var target *relation
	w := &write{kind: facts.Update, ignore: isTrue(n.Arg("opt_ignore"))}
	a.write = w
	for i, c := range cols {
		a.assigning = true
		col, err := a.column(sc, c, "field list")
		a.assigning = false
		if err != nil {
			return err
		}
		if col.col == nil {
			return &Error{Message: fmt.Sprintf("The target table %s of the UPDATE is not updatable", col.rel.alias), Code: 1288, Position: a.ph.Back(nodeStart(c))}
		}
		col.rel.target = true
		target = col.rel
		if w.table == nil {
			w.table = col.rel.table
		}
		assigned = append(assigned, col.col)
		if i < len(vals) {
			w.values = append(w.values, a.assign(sc, col.rel.table, col.col, vals[i]))
			values = append(values, a.storedTerm(sc, vals[i]))
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
	if target != nil && target.table != nil {
		a.facts.Writes = []facts.Write{a.writeFacts(facts.Update, target, assigned, values)}
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
		if fields, ok := n.Arg("using_fields").(mysqlast.List); ok {
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
	return ref.col, nil
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
		return a.lookup(sc, "", str(n.Arg("ident")), where, n.Start)
	case "PTI_simple_ident_q_2d":
		return a.lookup(sc, str(n.Arg("table")), str(n.Arg("field")), where, n.Start)
	case "PTI_simple_ident_q_3d":
		return a.lookup(sc, str(n.Arg("table")), str(n.Arg("field")), where, n.Start)
	}
	return colRef{}, fmt.Errorf("analyze: column reference not understood: %s", mysqlast.Sprint(v))
}

// lookup resolves table.field (table may be "") in sc: the innermost query whose
// relations know the name wins, an outer query is tried only when none of the inner one's
// do (a correlated reference); two matches at one level are ambiguous. In a HAVING clause
// an unqualified name that no table has may be a select-list alias.
func (a *analyzer) lookup(sc scope, table, field, where string, at int) (colRef, error) {
	if where == "having clause" && table == "" {
		if ref, ok := a.lookupItem(sc, field); ok {
			return ref, nil
		}
	}
	for s := &sc; s != nil; s = s.outer {
		if s != &sc && table == "" && a.typingHaving(s) {
			if ref, ok := a.lookupItem(*s, field); ok {
				return ref, nil
			}
		}
		var found []colRef
		var leaf int
		for i := range s.rels {
			rel := &s.rels[i]
			if table != "" && !strings.EqualFold(rel.alias, table) {
				continue
			}
			if ref, ok := rel.column(field); ok {
				found = append(found, ref)
				leaf = i
			}
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

// typingHaving reports a block whose HAVING is being typed.
func (a *analyzer) typingHaving(s *scope) bool {
	id := blockID(s)
	if id == nil {
		return false
	}
	for _, h := range a.inHaving {
		if h == id {
			return true
		}
	}
	return false
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

// lookupItem resolves a name against the block's typed select list (HAVING, and the
// ORDER BY of the block): the item's column, with no schema column behind it.
func (a *analyzer) lookupItem(sc scope, name string) (colRef, bool) {
	var found *Column
	for i := range sc.items {
		if strings.EqualFold(sc.items[i].Name, name) {
			if found != nil {
				return colRef{}, false // ambiguous: let the tables decide, or the error say so
			}
			found = &sc.items[i]
		}
	}
	if found == nil {
		return colRef{}, false
	}
	return colRef{c: *found}, true
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
			for i := range sc.rels {
				rel := &sc.rels[i]
				if table != "" && !strings.EqualFold(rel.alias, table) {
					continue
				}
				matched = true
				for _, c := range rel.columns() {
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
			c := Column{Name: name, Type: t.typ, Known: t.known, Nullable: t.nullable}
			if ref, ok := a.plainColumn(sc, expr); ok {
				c.base, c.baseTable = ref.c.base, ref.c.baseTable
				for i := range sc.rels {
					if &sc.rels[i] == ref.rel || sc.rels[i].alias == ref.rel.alias {
						c.leaf1, c.leafCol = i+1, ref.c.Name
						break
					}
				}
			}
			out = append(out, c)
		default:
			return nil, fmt.Errorf("analyze: select item not understood: %s", n.Class)
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
	_, err := a.expr(sc, v, where)
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
// NULL violation when the Go type cannot be nil).
func (a *analyzer) assign(sc scope, table *schema.Table, col *schema.Column, v mysqlast.Value) assignment {
	if isParam(v) {
		a.setParam(v, col.Type)
		a.noteParamSource(v, table, col, true)
		return assignment{col: col, nullable: true, param: a.ph.Number(nodeStart(v))}
	}
	if n, ok := v.(*mysqlast.Node); ok && n.Class == "Item_default_value" {
		return assignment{col: col, nullable: col.Default == nil && !col.NotNull}
	}
	t, _ := a.expr(sc, v, "field list") // an INSERT's expressions: errors surface as unknown types
	return assignment{col: col, nullable: !t.known || t.nullable}
}

// storedTerm is the value stored into a column as the facts spell it: a parameter, a
// literal, a value known before the statement runs, or an expression (Known "?").
func (a *analyzer) storedTerm(sc scope, v mysqlast.Value) facts.Term {
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
