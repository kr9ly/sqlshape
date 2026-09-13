package analyze

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/kr9ly/sqlshape/check/mysql/v2/internal/mysqlast"
	"github.com/kr9ly/sqlshape/check/mysql/v2/internal/schema"
	"github.com/kr9ly/sqlshape/v2/x/cardinality"
	"github.com/kr9ly/sqlshape/v2/x/facts"
	"github.com/kr9ly/sqlshape/v2/x/placeholder"
)

// BodyResult is a trigger's or a routine's body walked and typed on its own: the facts of
// each DML statement it contains (a Definition per the dialect's obligation, positioned at
// -1 the way a view's are), ready for dialect.Definitions.
type BodyResult struct {
	Statements []BodyStatement
	// Violations are the failure modes the body itself can produce once its own DECLARE ...
	// HANDLERs have absorbed what they catch (see block, walkSignal): its SIGNALs, what its
	// own embedded INSERT/UPDATE/DELETE may violate (including, recursively, what firing
	// their own tables' triggers may raise), and a SELECT ... INTO that cannot be proved to
	// return at most one row. A trigger's are what triggerViolations (violations.go) adds to
	// the statement that fires it; a routine's are not consumed by this milestone (m6's
	// concern for CALL and function-call sites), only computed correctly for it to use.
	Violations []Violation
}

// BodyStatement is one DML statement's facts, with the 1-based line of the definition
// text it starts at (0 when the position is not known), the way dialect.Definition's
// What names a function's statement ("function pay: line 3", check/postgres's own
// convention, mirrored here).
type BodyStatement struct {
	Facts *facts.Facts
	Line  int
}

// lineAt is pos's 1-based line within the definition text (schema.Trigger.Definition /
// schema.Routine.Definition), for BodyStatement.Line.
func (a *analyzer) lineAt(pos int) int {
	if pos < 0 || pos > len(a.text) {
		return 0
	}
	return strings.Count(a.text[:pos], "\n") + 1
}

// varScope is one block's declared variables (and parameters, in the outermost one),
// cursors and named conditions, chained to the enclosing block.
type varScope struct {
	outer   *varScope
	vars    map[string]bodyVar
	cursors map[string]*cursorInfo
	// conds are this block's DECLARE ... CONDITION FOR names, resolved to a condRef (a
	// SQLSTATE or a MySQL error number: the only two forms sp_cond accepts).
	conds map[string]condRef
}

// bodyVar is one declared variable or parameter: always potentially NULL (there is no NOT
// NULL for a routine variable; a DEFAULT does not stop a later assignment from clearing
// it), so nullable is always true.
type bodyVar struct {
	name string
	typ  schema.Type
}

// cursorInfo is a declared cursor: its query's result columns, for FETCH's count check.
type cursorInfo struct {
	name string
	cols []Column
}

// pushVars enters a new block.
func (a *analyzer) pushVars() { a.vars = &varScope{outer: a.vars} }

// popVars leaves the current block.
func (a *analyzer) popVars() {
	if a.vars != nil {
		a.vars = a.vars.outer
	}
}

// declareVar adds a variable or parameter to the current (innermost) block.
func (a *analyzer) declareVar(name string, t schema.Type) {
	if a.vars == nil {
		a.pushVars()
	}
	if a.vars.vars == nil {
		a.vars.vars = map[string]bodyVar{}
	}
	a.vars.vars[strings.ToLower(name)] = bodyVar{name: name, typ: t}
}

// declareCursor adds a cursor to the current block.
func (a *analyzer) declareCursor(name string, cols []Column) {
	if a.vars == nil {
		a.pushVars()
	}
	if a.vars.cursors == nil {
		a.vars.cursors = map[string]*cursorInfo{}
	}
	a.vars.cursors[strings.ToLower(name)] = &cursorInfo{name: name, cols: cols}
}

// lookupVar resolves a bare name against the block chain (innermost first): a local
// variable or parameter takes priority over a table column (MySQL's own rule), which is
// why analyze.go's column() calls this before falling back to the relations in scope.
func (a *analyzer) lookupVar(name string) (colRef, bool) {
	for s := a.vars; s != nil; s = s.outer {
		if v, ok := s.vars[strings.ToLower(name)]; ok {
			return colRef{c: Column{Name: v.name, Type: v.typ, Known: v.typ.Name != "", Nullable: true}}, true
		}
	}
	return colRef{}, false
}

// lookupCursor resolves a declared cursor's name against the block chain.
func (a *analyzer) lookupCursor(name string) (*cursorInfo, bool) {
	for s := a.vars; s != nil; s = s.outer {
		if c, ok := s.cursors[strings.ToLower(name)]; ok {
			return c, true
		}
	}
	return nil, false
}

// declareCondition adds a DECLARE ... CONDITION FOR to the current block.
func (a *analyzer) declareCondition(name string, ref condRef) {
	if a.vars == nil {
		a.pushVars()
	}
	if a.vars.conds == nil {
		a.vars.conds = map[string]condRef{}
	}
	a.vars.conds[strings.ToLower(name)] = ref
}

// lookupCondition resolves a declared condition's name against the block chain.
func (a *analyzer) lookupCondition(name string) (condRef, bool) {
	for s := a.vars; s != nil; s = s.outer {
		if c, ok := s.conds[strings.ToLower(name)]; ok {
			return c, true
		}
	}
	return condRef{}, false
}

// classifyMysqlerr reads a sp_condition_value's own `_mysqlerr` field (mysqlast folds it to
// a mysqlast.Number for a numeric literal, or a Go string for a SQLSTATE literal or one of
// the sp_condition_value::WARNING / NOT_FOUND / EXCEPTION constants -- measured against the
// generated shapes).
func classifyMysqlerr(v mysqlast.Value) condRef {
	switch x := v.(type) {
	case int:
		return condRef{kind: condNumber, number: x}
	case mysqlast.Number:
		return condRef{kind: condNumber, number: int(x)}
	case string:
		switch x {
		case "sp_condition_value::WARNING":
			return condRef{kind: condWarning}
		case "sp_condition_value::NOT_FOUND":
			return condRef{kind: condNotFound}
		case "sp_condition_value::EXCEPTION":
			return condRef{kind: condException}
		default:
			return condRef{kind: condSQLState, sqlstate: strings.ToUpper(x)}
		}
	}
	return condRef{}
}

// resolveCondValue resolves one condition value of a SIGNAL, a RESIGNAL or a HANDLER FOR
// list: a bare sp_condition_value (a literal), or a sp_condition_name (a DECLARE ...
// CONDITION FOR, resolved against the block chain).
func (a *analyzer) resolveCondValue(v mysqlast.Value) (condRef, bool) {
	n, ok := v.(*mysqlast.Node)
	if !ok {
		return condRef{}, false
	}
	switch n.Class {
	case "sp_condition_value":
		return classifyMysqlerr(n.Arg("_mysqlerr")), true
	case "sp_condition_name":
		return a.lookupCondition(str(n.Arg("name")))
	}
	return condRef{}, false
}

// resolveHandlerConditions resolves a HANDLER FOR's comma-separated condition list.
func (a *analyzer) resolveHandlerConditions(v mysqlast.Value) []condRef {
	list, _ := v.(mysqlast.List)
	out := make([]condRef, 0, len(list))
	for _, c := range list {
		if ref, ok := a.resolveCondValue(c); ok {
			out = append(out, ref)
		}
	}
	return out
}

// trigRowColumn resolves NEW.field / OLD.field inside a trigger body, applying the
// server's own rules (measured on mysqld 8.4):
//   - referencing OLD in an INSERT trigger, or NEW in a DELETE trigger, is 1363 ("There is
//     no OLD/NEW row in on INSERT/DELETE trigger") regardless of the column;
//   - an unknown column is 1054 ("Unknown column 'x' in 'NEW'"/"'OLD'"), the row alias
//     alone as the "in" part (not "NEW.x");
//   - write (an assignment target, SET NEW.x = ... / SET OLD.x = ...) additionally
//     forbids OLD always (1362 "Updating of OLD row is not allowed in trigger") and NEW
//     outside a BEFORE trigger (1362 "Updating of NEW row is not allowed in after
//     trigger");
//   - NEW.col reads NULL-able in a BEFORE INSERT/UPDATE trigger regardless of the column's
//     own NOT NULL (measured: the server's own NOT NULL check has not run yet at that
//     point); an AFTER trigger's NEW.col, and OLD.col at any timing, follow the column's
//     declared nullability (the row already exists as stored -- inferred, not measured
//     directly, since a NOT NULL column cannot hold NULL once committed).
func (a *analyzer) trigRowColumn(qualifier, field string, at int, write bool) (*schema.Column, bool, error) {
	tg := a.trig
	row := strings.ToUpper(qualifier)
	event := strings.ToUpper(tg.Event)
	timing := strings.ToUpper(tg.Timing)
	if row == "OLD" && event == "INSERT" {
		return nil, false, &Error{Message: "There is no OLD row in on INSERT trigger", Code: 1363, Position: a.ph.Back(at)}
	}
	if row == "NEW" && event == "DELETE" {
		return nil, false, &Error{Message: "There is no NEW row in on DELETE trigger", Code: 1363, Position: a.ph.Back(at)}
	}
	col := a.trigTable.Column(field)
	if col == nil {
		return nil, false, &Error{Message: fmt.Sprintf("Unknown column '%s' in '%s'", field, row), Code: 1054, Position: a.ph.Back(at)}
	}
	if write {
		if row == "OLD" {
			return nil, false, &Error{Message: "Updating of OLD row is not allowed in trigger", Code: 1362, Position: a.ph.Back(at)}
		}
		if row == "NEW" && timing == "AFTER" {
			return nil, false, &Error{Message: "Updating of NEW row is not allowed in after trigger", Code: 1362, Position: a.ph.Back(at)}
		}
	}
	nullable := !col.NotNull
	if row == "NEW" && timing == "BEFORE" {
		nullable = true
	}
	return col, nullable, nil
}

// AnalyzeTrigger types tg's body against s: NEW / OLD resolve as tg's table (under the
// rules trigRowColumn documents), a bare name resolves against the block's DECLAREs first,
// then as a table column of whatever relation an embedded statement (INSERT/UPDATE/DELETE/
// SELECT) brings into scope. The result is one Facts per DML statement the body contains,
// for dialect.Definitions; a construct the server itself refuses at CREATE time is an
// error instead (analyze.Error, the way Analyze's are).
func AnalyzeTrigger(s *schema.Schema, tg *schema.Trigger) (*BodyResult, error) {
	if c, ok := bodyCache.Load(tg); ok {
		cc := c.(*bodyCached)
		return cc.result, cc.err
	}
	c := &bodyCached{}
	actual, loaded := bodyCache.LoadOrStore(tg, c)
	if loaded {
		// a cycle: this trigger's own analysis, further up the call stack, reaches itself
		// (its body writes a table whose trigger writes back to this one). The in-progress
		// placeholder has nothing yet; the outer call finishes and overwrites it once done.
		cc := actual.(*bodyCached)
		return cc.result, cc.err
	}
	br, err := analyzeTriggerBody(s, tg)
	c.result, c.err = br, err
	return br, err
}

// analyzeTriggerBody is AnalyzeTrigger's own work, apart from the cache: cachedAnalyzeTrigger
// (violations.go) and AnalyzeTrigger itself both go through the cache instead.
func analyzeTriggerBody(s *schema.Schema, tg *schema.Trigger) (*BodyResult, error) {
	t := s.Table(tg.Table)
	if t == nil {
		return nil, fmt.Errorf("analyze: trigger %s: its table %s is not in the schema", tg.Name, tg.Table)
	}
	_, ph := placeholder.Rewrite(tg.Definition)
	a := &analyzer{s: s, text: tg.Definition, ph: ph, trig: tg, trigTable: t, raises: parseRaises(tg.Directives)}
	br := &BodyResult{}
	if err := a.walkOne(scope{}, tg.Body, br); err != nil {
		return nil, err
	}
	br.Violations = dedupe(a.raised)
	return br, nil
}

// bodyCache caches a trigger's own body analysis (AnalyzeTrigger's schema.Trigger pointer
// is stable once the schema is loaded, the same assumption check/postgres/analyze/plpgsql.go's
// own plBodies cache makes), so a trigger fired by several statements, or reached again
// through its own recursive write, is walked once. The in-progress placeholder
// (LoadOrStore's "loaded" branch) is how a cycle breaks: see AnalyzeTrigger.
var bodyCache sync.Map // map[*schema.Trigger]*bodyCached

type bodyCached struct {
	result *BodyResult
	err    error
}

// cachedAnalyzeTrigger is triggerViolations' own entry point (violations.go): the same
// cache AnalyzeTrigger uses, so neither walks a trigger's body twice.
func cachedAnalyzeTrigger(s *schema.Schema, tg *schema.Trigger) (*BodyResult, error) {
	return AnalyzeTrigger(s, tg)
}

// parseRaises reads a trigger's/routine's `-- sqlshape: error <key> = <Name>` directives
// (schema.go keeps them verbatim; this is the "later stage" that parses them) into a map
// by key -- a MYSQL_ERRNO as decimal text, or a SQLSTATE.
func parseRaises(directives []string) map[string]string {
	out := map[string]string{}
	for _, d := range directives {
		if len(d) < 6 || !strings.EqualFold(d[:6], "error ") {
			continue
		}
		rest := strings.TrimSpace(d[6:])
		key, name, ok := strings.Cut(rest, "=")
		if !ok {
			continue
		}
		out[strings.TrimSpace(key)] = strings.TrimSpace(name)
	}
	return out
}

// AnalyzeRoutine types r's body against s: parameters are declared variables from the
// start (always nullable, like every routine variable), RETURN is typed against Returns
// for a FUNCTION and refused (1313) for a PROCEDURE, and a FUNCTION with no RETURN
// anywhere in its body is refused (1320) -- the server's own checks, not a reachability
// analysis.
func AnalyzeRoutine(s *schema.Schema, r *schema.Routine) (*BodyResult, error) {
	if c, ok := routineCache.Load(r); ok {
		cc := c.(*bodyCached)
		return cc.result, cc.err
	}
	c := &bodyCached{}
	actual, loaded := routineCache.LoadOrStore(r, c)
	if loaded {
		cc := actual.(*bodyCached)
		return cc.result, cc.err
	}
	br, err := analyzeRoutineBody(s, r)
	c.result, c.err = br, err
	return br, err
}

// routineCache is AnalyzeRoutine's own cache (bodyCache's counterpart): see AnalyzeTrigger.
var routineCache sync.Map // map[*schema.Routine]*bodyCached

func analyzeRoutineBody(s *schema.Schema, r *schema.Routine) (*BodyResult, error) {
	_, ph := placeholder.Rewrite(r.Definition)
	a := &analyzer{s: s, text: r.Definition, ph: ph, routine: r, raises: parseRaises(r.Directives)}
	a.pushVars()
	for _, p := range r.Params {
		a.declareVar(p.Name, p.Type)
	}
	br := &BodyResult{}
	err := a.walkOne(scope{}, r.Body, br)
	a.popVars()
	if err != nil {
		return nil, err
	}
	if r.Kind == schema.Function && !a.sawReturn {
		return nil, &Error{Message: fmt.Sprintf("No RETURN found in FUNCTION %s", r.Name), Code: 1320, Position: -1}
	}
	br.Violations = dedupe(a.raised)
	return br, nil
}

// resetStatement clears the per-statement bookkeeping Analyze's entry points (insert/
// update/delete/selectStmt) fill in, so a fresh one can be typed independently of the
// last: the body's DML statements each get their own Facts, params, columns and uses.
func (a *analyzer) resetStatement() {
	a.columns = nil
	a.facts = nil
	a.uses = nil
	a.useIdx = nil
	a.readUses = nil
	a.assigning = false
	a.write = nil
	a.outerRefs = nil
	a.blocks = nil
	a.fdConst = nil
	a.nullEq = nil
	a.inHaving = nil
	a.subFacts = nil
	a.claimed = nil
	a.paramSrc = nil
	a.depth = 0
	a.setOp = 0
}

// finishFacts is the post-processing Analyze does for a top-level statement (sorting the
// uses by position and hanging them off Facts), replayed here since the body walk calls
// insert/update/delete/selectStmt directly rather than through Analyze.
func (a *analyzer) finishFacts() {
	sort.SliceStable(a.uses, func(i, j int) bool { return a.uses[i].Position < a.uses[j].Position })
	a.facts.Uses = a.uses
}

// exprAt types an expression that is not part of a query block (a SET's right-hand side,
// an IF/WHILE/REPEAT condition, a RETURN, a SIGNAL information item): no relation is in
// scope, only the block's variables (and NEW/OLD in a trigger), through column()'s own
// lookupVar / trigRowColumn.
func (a *analyzer) exprAt(v mysqlast.Value, where string) error {
	if v == nil {
		return nil
	}
	_, err := a.expr(scope{}, v, where)
	return err
}

// walkOne dispatches one body construct: a statement, a control-flow node, a list of
// statements, or the bare cursor name OPEN/CLOSE fold to (mysqlast cannot tell OPEN from
// CLOSE apart -- both need only the cursor to be declared, which is all this checks).
func (a *analyzer) walkOne(sc scope, v mysqlast.Value, br *BodyResult) error {
	switch x := v.(type) {
	case nil:
		return nil
	case mysqlast.List:
		for _, s := range x {
			if err := a.walkOne(sc, s, br); err != nil {
				return err
			}
		}
		return nil
	case mysqlast.Token:
		return a.walkCursorToken(str(x), x.Start)
	case *mysqlast.Struct:
		if cmd, ok := x.Fields["sql_command"].(mysqlast.Const); ok {
			return a.commitCheck(string(cmd), 0)
		}
		return nil
	case *mysqlast.Node:
		return a.walkNode(sc, x, br)
	}
	return nil
}

// walkCursorToken handles OPEN/CLOSE <cursor>, both of which fold to the bare cursor name
// (1324 "Undefined CURSOR" when it was never declared, measured on mysqld).
func (a *analyzer) walkCursorToken(name string, at int) error {
	if name == "" {
		return nil
	}
	if _, ok := a.lookupCursor(name); !ok {
		return &Error{Message: fmt.Sprintf("Undefined CURSOR: %s", name), Code: 1324, Position: a.ph.Back(at)}
	}
	return nil
}

// commitCheck is 1422 for a transaction-control statement (COMMIT / START TRANSACTION /
// ROLLBACK / SAVEPOINT, folded to a {sql_command: SQLCOM_*} Struct) or a DDL statement
// (a PT_create_*/PT_drop_*/PT_alter_*/PT_rename_*/PT_truncate_* node) in a trigger or
// function body -- measured on mysqld: a procedure is exempt.
func (a *analyzer) commitCheck(class string, at int) error {
	if a.trig == nil && (a.routine == nil || a.routine.Kind != schema.Function) {
		return nil // a procedure: COMMIT and DDL are allowed
	}
	ddl := strings.HasPrefix(class, "PT_create_") || strings.HasPrefix(class, "PT_drop_") ||
		strings.HasPrefix(class, "PT_alter_") || strings.HasPrefix(class, "PT_rename_") ||
		strings.HasPrefix(class, "PT_truncate_")
	txn := class == "SQLCOM_COMMIT" || class == "SQLCOM_BEGIN" || class == "SQLCOM_ROLLBACK" ||
		class == "SQLCOM_ROLLBACK_TO_SAVEPOINT" || class == "SQLCOM_SAVEPOINT" || class == "SQLCOM_RELEASE_SAVEPOINT"
	if ddl || txn {
		return &Error{Message: "Explicit or implicit commit is not allowed in stored function or trigger.", Code: 1422, Position: a.ph.Back(at)}
	}
	return nil
}

// walkNode dispatches a *mysqlast.Node body construct.
func (a *analyzer) walkNode(sc scope, n *mysqlast.Node, br *BodyResult) error {
	switch n.Class {
	case "sp_block_content":
		return a.walkBlock(sc, n, br)
	case "sp_decl_var", "sp_decl_condition", "sp_decl_handler", "sp_decl_cursor":
		return a.walkDecl(sc, n, br)
	case "sp_if":
		return a.walkIf(sc, n, br)
	case "sp_labeled_block":
		a.labels = append(a.labels, str(n.Arg("label")))
		err := a.walkOne(sc, n.Arg("body"), br)
		a.labels = a.labels[:len(a.labels)-1]
		return err
	case "sp_labeled_control":
		a.labels = append(a.labels, str(n.Arg("label")))
		err := a.walkOne(sc, n.Arg("body"), br)
		a.labels = a.labels[:len(a.labels)-1]
		return err
	case "sp_unlabeled_control":
		// WHILE (cond, body) / REPEAT (body, cond); a bare LOOP folds to its body List
		// directly (no wrapper node), so it never reaches this case.
		if _, ok := n.Args[0].(mysqlast.List); ok {
			if err := a.walkOne(sc, n.Args[0], br); err != nil {
				return err
			}
			return a.exprAt(n.Args[1], "repeat")
		}
		if err := a.exprAt(n.Args[0], "while"); err != nil {
			return err
		}
		return a.walkOne(sc, n.Args[1], br)
	case "simple_case_stmt":
		if err := a.exprAt(n.Args[0], "case"); err != nil {
			return err
		}
		for _, w := range flattenBinaryList(n.Args[1], "simple_when_clause_list") {
			wn, _ := w.(*mysqlast.Node)
			if wn == nil {
				continue
			}
			if err := a.exprAt(wn.Args[0], "when"); err != nil {
				return err
			}
			if err := a.walkOne(sc, wn.Args[1], br); err != nil {
				return err
			}
		}
		return a.walkOne(sc, n.Args[2], br)
	case "searched_case_stmt":
		for _, w := range flattenBinaryList(n.Args[0], "searched_when_clause_list") {
			wn, _ := w.(*mysqlast.Node)
			if wn == nil {
				continue
			}
			if err := a.exprAt(wn.Args[0], "when"); err != nil {
				return err
			}
			if err := a.walkOne(sc, wn.Args[1], br); err != nil {
				return err
			}
		}
		return a.walkOne(sc, n.Args[1], br)
	case "sp_leave":
		return a.checkLabel("LEAVE", str(n.Arg("label")), n.Start)
	case "sp_iterate":
		return a.checkLabel("ITERATE", str(n.Arg("label")), n.Start)
	case "sp_return":
		return a.walkReturn(n)
	case "sp_signal", "sp_resignal":
		return a.walkSignal(n)
	case "PT_set":
		return a.walkSet(sc, n)
	case "PT_call":
		return a.walkCall(n)
	case "sp_proc_stmt_fetch":
		return a.walkFetch(n)
	case "PT_select_stmt":
		return a.walkSelect(sc, n, br)
	case "PT_insert":
		return a.walkDML(n, facts.Insert, br, a.insert)
	case "PT_update":
		return a.walkDML(n, facts.Update, br, a.update)
	case "PT_delete":
		return a.walkDML(n, facts.Delete, br, a.delete)
	}
	if strings.HasPrefix(n.Class, "PT_create_") || strings.HasPrefix(n.Class, "PT_drop_") ||
		strings.HasPrefix(n.Class, "PT_alter_") || strings.HasPrefix(n.Class, "PT_rename_") ||
		strings.HasPrefix(n.Class, "PT_truncate_") {
		return a.commitCheck(n.Class, n.Start)
	}
	return nil // a body statement this milestone does not walk (SHOW, GET DIAGNOSTICS...): skipped, not an error
}

// flattenBinaryList unwraps a left-recursive when-clause list (a single clause folds to itself,
// with no wrapping node; two or more nest as wrapClass(list-so-far, clause)) into the
// clauses in source order.
func flattenBinaryList(v mysqlast.Value, wrapClass string) []mysqlast.Value {
	if n, ok := v.(*mysqlast.Node); ok && n.Class == wrapClass {
		return append(flattenBinaryList(n.Args[0], wrapClass), n.Args[1])
	}
	return []mysqlast.Value{v}
}

// pendingHandler is one DECLARE ... HANDLER of the block being walked, its body deferred
// until the block's own statements (and, so, its own failure modes) are known.
type pendingHandler struct {
	conds []condRef
	body  mysqlast.Value
}

// walkBlock walks BEGIN ... END: a fresh block of variables/conditions/cursors, then its
// statements, then the block closes (its DECLAREs go out of scope). A DECLARE ... HANDLER
// (which MySQL requires among the block's own declarations, ahead of its statements, the
// same position PL/pgSQL's EXCEPTION clause holds at the end of its block) is not walked
// there: it is collected, and its scope is exactly this block's own statements, so once
// they are walked its conditions filter what they raised (see block below) the way
// check/postgres/analyze/plpgsql.go's own block does for an EXCEPTION list.
func (a *analyzer) walkBlock(sc scope, n *mysqlast.Node, br *BodyResult) error {
	a.pushVars()
	defer a.popVars()
	raisedAt := len(a.raised)
	decls, _ := n.Args[0].(mysqlast.List)
	var handlers []pendingHandler
	for _, d := range decls {
		if dn, ok := d.(*mysqlast.Node); ok && dn.Class == "sp_decl_handler" {
			handlers = append(handlers, pendingHandler{conds: a.resolveHandlerConditions(dn.Arg("conditions")), body: dn.Arg("body")})
			continue
		}
		if err := a.walkOne(sc, d, br); err != nil {
			return err
		}
	}
	if err := a.walkOne(sc, n.Args[1], br); err != nil {
		return err
	}
	if len(handlers) == 0 {
		return nil
	}
	return a.absorb(sc, raisedAt, handlers, br)
}

// absorb is walkBlock's own handler pass: the block's statements (from raisedAt on) may
// have raised failure modes a HANDLER here catches; those are removed from what propagates
// (a caught SIGNAL, or a caught constraint violation from the block's own writes, never
// reaches the caller), then each handler's own body is walked as ordinary code (so what it
// itself raises -- a fresh SIGNAL, a RESIGNAL -- joins back on top, subject to an
// enclosing block's own HANDLERs in turn).
func (a *analyzer) absorb(sc scope, raisedAt int, handlers []pendingHandler, br *BodyResult) error {
	protected := append([]Violation(nil), a.raised[raisedAt:]...)
	a.raised = a.raised[:raisedAt]
	caughtBy := func(v Violation) bool {
		for _, h := range handlers {
			for _, c := range h.conds {
				if c.catches(v) {
					return true
				}
			}
		}
		return false
	}
	for _, v := range protected {
		if !caughtBy(v) {
			a.raised = append(a.raised, v)
		}
	}
	for _, h := range handlers {
		var caught []Violation
		for _, v := range protected {
			for _, c := range h.conds {
				if c.catches(v) {
					caught = append(caught, v)
					break
				}
			}
		}
		saved := a.handlerRaise
		a.handlerRaise = caught
		err := a.walkOne(sc, h.body, br)
		a.handlerRaise = saved
		if err != nil {
			return err
		}
	}
	return nil
}

// walkDecl processes one DECLARE: a variable (registered, typed from the declaration), a
// condition (registered against the block, for a later SIGNAL/HANDLER to resolve by name),
// a handler (dead code along walkBlock's own path, which intercepts sp_decl_handler before
// reaching here to defer its body -- kept as a defensive fallback), or a cursor (its query
// is typed once, for FETCH's column count).
func (a *analyzer) walkDecl(sc scope, n *mysqlast.Node, br *BodyResult) error {
	switch n.Class {
	case "sp_decl_var":
		t, err := schema.TypeOf(n.Arg("type"))
		if err != nil {
			return err
		}
		names, _ := n.Arg("names").(mysqlast.List)
		for _, nm := range names {
			a.declareVar(str(nm), t)
		}
		if def := n.Arg("default"); def != nil {
			return a.exprAt(def, "default")
		}
		return nil
	case "sp_decl_condition":
		if ref, ok := a.resolveCondValue(n.Arg("value")); ok {
			a.declareCondition(str(n.Arg("name")), ref)
		}
		return nil
	case "sp_decl_handler":
		return a.walkOne(sc, n.Arg("body"), br)
	case "sp_decl_cursor":
		a.resetStatement()
		a.facts = &facts.Facts{Kind: facts.Select}
		cols, err := a.queryExpression(queryExprOf(n.Arg("query")), nil)
		if err != nil {
			return err
		}
		a.columns = cols
		a.appendStatement(br, n.Start)
		a.declareCursor(str(n.Arg("name")), cols)
		return nil
	}
	return nil
}

// queryExprOf digs a PT_select_stmt's own query expression out (the shape queryExpression
// itself expects), so a cursor's DECLARE ... FOR select_stmt can be typed the same way a
// top-level SELECT is.
func queryExprOf(v mysqlast.Value) mysqlast.Value {
	n, ok := v.(*mysqlast.Node)
	if !ok || n.Class != "PT_select_stmt" {
		return v
	}
	return n.Arg("qe")
}

// appendStatement finishes and records the current statement's Facts among the body's, at
// the line pos falls on (its own start, the way a view's body's position is dropped to -1
// but a routine's/trigger's statement keeps a line: check/postgres's own convention).
func (a *analyzer) appendStatement(br *BodyResult, pos int) {
	if a.facts == nil {
		return
	}
	a.finishFacts()
	br.Statements = append(br.Statements, BodyStatement{Facts: a.facts, Line: a.lineAt(a.ph.Back(pos))})
}

// walkIf types the condition, walks the THEN statements, and continues into ELSEIF (a
// nested sp_if, unwrapped the same way sp_block_content's fold reaches it) or ELSE (a
// plain statement list), or neither.
func (a *analyzer) walkIf(sc scope, n *mysqlast.Node, br *BodyResult) error {
	if err := a.exprAt(n.Args[0], "if"); err != nil {
		return err
	}
	if err := a.walkOne(sc, n.Args[1], br); err != nil {
		return err
	}
	switch e := n.Args[2].(type) {
	case nil:
		return nil
	case *mysqlast.Node:
		if e.Class == "sp_if" {
			return a.walkIf(sc, e, br)
		}
	}
	return a.walkOne(sc, n.Args[2], br)
}

// checkLabel is 1308 ("LEAVE/ITERATE with no matching label") when label names no
// enclosing labeled block or loop.
func (a *analyzer) checkLabel(kw, label string, at int) error {
	for _, l := range a.labels {
		if strings.EqualFold(l, label) {
			return nil
		}
	}
	return &Error{Message: fmt.Sprintf("%s with no matching label: %s", kw, label), Code: 1308, Position: a.ph.Back(at)}
}

// walkReturn is 1313 ("RETURN is only allowed in a FUNCTION") for a trigger or a
// procedure; for a function it types the expression against Returns and records that the
// body did have a RETURN (1320's check).
func (a *analyzer) walkReturn(n *mysqlast.Node) error {
	if a.routine == nil || a.routine.Kind != schema.Function {
		return &Error{Message: "RETURN is only allowed in a FUNCTION", Code: 1313, Position: a.ph.Back(n.Start)}
	}
	a.sawReturn = true
	return a.exprAt(n.Args[0], "return")
}

// walkSignal types SIGNAL/RESIGNAL's information items and records the failure mode it
// raises (block, above, is what absorbs it against an enclosing HANDLER).
//
// A SIGNAL's own condition (signal_value, sp_cond's grammar) is only ever a literal
// SQLSTATE or a literal MySQL error number, or a sp_condition_name naming a DECLARE ...
// CONDITION FOR one of those two (never SQLWARNING/NOT FOUND/SQLEXCEPTION, which sp_hcond
// alone accepts); a number, bare or through a named CONDITION, is 1646 at CREATE time
// (measured: "SIGNAL/RESIGNAL can only use a CONDITION defined with SQLSTATE"). A bare
// RESIGNAL (no condition given) re-raises whatever the innermost enclosing HANDLER is
// itself handling (a.handlerRaise), unchanged (measured: same number, same message) --
// RESIGNAL with a condition is an ordinary SIGNAL instead (measured: a fresh number).
//
// The key (Violation.Constraint here) is the SET MYSQL_ERRNO value as decimal text when
// the SIGNAL gives one, the SQLSTATE otherwise (mysql/errors.go's runtime uses the same
// rule to map an error back). SQLSTATE class "01" (SQLWARNING) is never a failure mode
// (measured: the statement succeeds); an unhandled class "02" (NOT FOUND) reports as 1643,
// anything else unhandled and without its own MYSQL_ERRNO as 1644 (both measured).
func (a *analyzer) walkSignal(n *mysqlast.Node) error {
	items, _ := n.Arg("info").(mysqlast.List)
	for _, it := range items {
		itn, ok := it.(*mysqlast.Node)
		if !ok {
			continue
		}
		if err := a.exprAt(itn.Arg("expr"), "signal"); err != nil {
			return err
		}
	}
	cond := n.Arg("condition")
	if cond == nil {
		if n.Class == "sp_resignal" {
			a.raised = append(a.raised, a.handlerRaise...)
		}
		return nil
	}
	ref, ok := a.resolveCondValue(cond)
	if !ok {
		return nil // an unresolved named condition: nothing to predict (defensive)
	}
	if ref.kind == condNumber {
		return &Error{Message: "SIGNAL/RESIGNAL can only use a CONDITION defined with SQLSTATE", Code: 1646, Position: a.ph.Back(n.Start)}
	}
	class := ""
	if len(ref.sqlstate) >= 2 {
		class = ref.sqlstate[:2]
	}
	if class == "01" {
		return nil // SQLWARNING: a warning, not a failure mode
	}
	errno, _ := signalErrno(items)
	code, key := errno, ref.sqlstate
	if code == 0 {
		if class == "02" {
			code = 1643
		} else {
			code = 1644
		}
	} else {
		key = strconv.Itoa(errno)
	}
	v := Violation{Code: code, Constraint: key, SQLState: ref.sqlstate, Name: a.raises[key]}
	if a.trig != nil {
		v.Table, v.Trigger = a.trigTable.Name, a.trig.Name
	}
	a.raised = append(a.raised, v)
	return nil
}

// signalErrno reads a SIGNAL/RESIGNAL's own `SET MYSQL_ERRNO = n` item, when there is one.
func signalErrno(items mysqlast.List) (int, bool) {
	for _, it := range items {
		itn, ok := it.(*mysqlast.Node)
		if !ok || str(itn.Arg("name")) != "CIN_MYSQL_ERRNO" {
			continue
		}
		if n, ok := itn.Arg("expr").(*mysqlast.Node); ok && n.Class == "Item_int" {
			if tok, ok := n.Arg("i").(mysqlast.Token); ok {
				if i, err := strconv.Atoi(tok.Value); err == nil {
					return i, true
				}
			}
		}
	}
	return 0, false
}

// walkSet types SET's assignments: a local variable, NEW/OLD (a trigger's own rules,
// including the write-time 1362s), a `@user` variable (any name), or -- when the target
// resolves to none of those -- a system variable, silently skipped (m4's item 4: a body
// statement this milestone does not analyze is not an error).
func (a *analyzer) walkSet(sc scope, n *mysqlast.Node) error {
	list, ok := n.Arg("list").(*mysqlast.Node)
	if !ok {
		return nil
	}
	for _, item := range flattenSetList(list) {
		in, ok := item.(*mysqlast.Node)
		if !ok {
			continue
		}
		switch in.Class {
		case "PT_set_variable":
			if err := a.exprAt(in.Arg("opt_expr"), "set"); err != nil {
				return err
			}
			qual, name := bipartite(in.Arg("name"))
			if err := a.walkSetTarget(qual, name, in.Start); err != nil {
				return err
			}
		case "PT_option_value_no_option_type_user_var":
			if err := a.exprAt(in.Arg("expr"), "set"); err != nil {
				return err
			}
		}
	}
	return nil
}

// walkSetTarget resolves a `SET name = ...` / `SET qualifier.name = ...` target.
func (a *analyzer) walkSetTarget(qual, name string, at int) error {
	if qual == "" {
		if _, ok := a.lookupVar(name); ok {
			return nil // a local variable or parameter: the assignment needs no further check
		}
		return nil // a system variable: not validated (item 4)
	}
	if a.trig != nil && (strings.EqualFold(qual, "new") || strings.EqualFold(qual, "old")) {
		_, _, err := a.trigRowColumn(qual, name, at, true)
		return err
	}
	return nil // an unrecognized qualifier: not validated (item 4)
}

// flattenSetList walks a SET statement's `PT_start_option_value_list_no_type{head, tail}`
// into its assignments in order: head, then the PT_option_value_list_head chain's values.
func flattenSetList(list *mysqlast.Node) []mysqlast.Value {
	out := []mysqlast.Value{list.Arg("head")}
	tail := list.Arg("tail")
	for tail != nil {
		tn, ok := tail.(*mysqlast.Node)
		if !ok {
			break
		}
		out = append(out, tn.Arg("value"))
		tail = tn.Arg("tail")
	}
	return out
}

// bipartite reads a PT_set_variable's `name` argument (a `.name` field access wrapping a
// Bipartite_name(qualifier, name) node) into its two parts; qualifier is "" when the name
// was not written `x.y`.
func bipartite(v mysqlast.Value) (qual, name string) {
	wrap, ok := v.(*mysqlast.Node)
	if !ok || len(wrap.Args) == 0 {
		return "", ""
	}
	bp, ok := wrap.Args[0].(*mysqlast.Node)
	if !ok || bp.Class != "Bipartite_name" || len(bp.Args) != 2 {
		return "", ""
	}
	return str(bp.Args[0]), str(bp.Args[1])
}

// walkCall types CALL's arguments (m4's scope: walked and typed, not validated against the
// called routine's parameters, and its result is not modeled -- m6's concern).
func (a *analyzer) walkCall(n *mysqlast.Node) error {
	args, _ := n.Arg("opt_expr_list").(mysqlast.List)
	for _, arg := range args {
		if err := a.exprAt(arg, "call"); err != nil {
			return err
		}
	}
	return nil
}

// walkFetch is FETCH cur INTO vars: the cursor must be declared (1324, walkCursorToken's
// own check, reused here since sp_proc_stmt_fetch's own shape carries the cursor's name
// directly rather than folding to the bare token); each target is a declared variable.
// The column-count mismatch the server only catches at CALL time (1328, measured) is
// nonetheless certain from the declarations alone, so it is reported here too.
func (a *analyzer) walkFetch(n *mysqlast.Node) error {
	name := str(n.Args[1])
	cur, ok := a.lookupCursor(name)
	if !ok {
		return &Error{Message: fmt.Sprintf("Undefined CURSOR: %s", name), Code: 1324, Position: a.ph.Back(n.Start)}
	}
	targets := flattenBinaryList(n.Args[2], "sp_fetch_list")
	if len(targets) != len(cur.cols) {
		return &Error{Message: "Incorrect number of FETCH variables", Code: 1328, Position: a.ph.Back(n.Start)}
	}
	for _, t := range targets {
		if _, err := a.selectIntoTarget(t); err != nil {
			return err
		}
	}
	return nil
}

// selectIntoTarget resolves one SELECT ... INTO / FETCH ... INTO target: a declared
// variable (1327 "Undeclared variable" when it is not one, measured for FETCH; assumed the
// same for SELECT INTO, the same sp_pcontext lookup) or a `@user` variable (PT_select_var,
// always fine).
func (a *analyzer) selectIntoTarget(v mysqlast.Value) (bool, error) {
	n, ok := v.(*mysqlast.Node)
	if !ok {
		if name := str(v); name != "" {
			if _, ok := a.lookupVar(name); !ok {
				return false, &Error{Message: fmt.Sprintf("Undeclared variable: %s", name), Code: 1327, Position: 0}
			}
		}
		return true, nil
	}
	switch n.Class {
	case "PT_select_var":
		return true, nil // a `@user` variable: always fine
	case "PT_select_sp_var":
		name := str(n.Arg("name"))
		if _, ok := a.lookupVar(name); !ok {
			return false, &Error{Message: fmt.Sprintf("Undeclared variable: %s", name), Code: 1327, Position: a.ph.Back(n.Start)}
		}
		return true, nil
	}
	return true, nil
}

// walkSelect types a SELECT (INTO or not): INTO-less in a trigger or function body is
// 1415 ("Not allowed to return a result set from a trigger/function", measured); its
// column count must match its INTO targets' (1222, measured as a CALL-time error but
// certain from the declarations, the same reasoning as walkFetch's 1328), and each
// variable target must be declared (1327).
func (a *analyzer) walkSelect(sc scope, n *mysqlast.Node, br *BodyResult) error {
	a.resetStatement()
	if err := a.selectStmt(n); err != nil {
		return err
	}
	into := selectInto(n)
	if len(into) == 0 {
		if a.trig != nil {
			return &Error{Message: "Not allowed to return a result set from a trigger", Code: 1415, Position: a.ph.Back(n.Start)}
		}
		if a.routine != nil && a.routine.Kind == schema.Function {
			return &Error{Message: "Not allowed to return a result set from a function", Code: 1415, Position: a.ph.Back(n.Start)}
		}
		a.appendStatement(br, n.Start)
		return nil
	}
	if len(into) != len(a.columns) {
		return &Error{Message: "The used SELECT statements have a different number of columns", Code: 1222, Position: a.ph.Back(n.Start)}
	}
	for _, t := range into {
		if _, err := a.selectIntoTarget(t); err != nil {
			return err
		}
	}
	// 1172 ("Result consisted of more than one row"): possible unless the query itself
	// proves at most one row (x/cardinality's One argument, the same proof `LIMIT 1`
	// satisfies); zero rows is NOT FOUND (1329, a warning, measured), never a failure.
	if ok, _ := cardinality.AtMostOne(a.facts); !ok {
		v := Violation{Code: code1172, Constraint: strconv.Itoa(code1172), SQLState: "42000"}
		if a.trig != nil {
			v.Table, v.Trigger = a.trigTable.Name, a.trig.Name
		}
		a.raised = append(a.raised, v)
	}
	a.appendStatement(br, n.Start)
	return nil
}

// selectInto digs a SELECT's own INTO target list out (nil when there is none).
func selectInto(n *mysqlast.Node) mysqlast.List {
	qe, ok := n.Arg("qe").(*mysqlast.Node)
	if !ok {
		return nil
	}
	body, ok := qe.Arg("body").(*mysqlast.Node)
	if !ok || body.Class != "PT_query_specification" {
		return nil
	}
	l, _ := body.Arg("opt_into1").(mysqlast.List)
	return l
}

// walkDML types an embedded INSERT/UPDATE/DELETE with its own fresh per-statement state,
// records its Facts among the body's Definitions (position -1, the way a view's body's
// is), and folds what it may violate -- including, recursively, what firing its own
// table's triggers may raise (violations() already does, for any statement) -- into the
// body's own raised failure modes. A trigger writing its own table is instead 1442
// ("Can't update table ... because it is already used by statement which invoked this
// stored function/trigger"), measured unconditionally true on mysqld 8.4 for every
// (timing, event, own-write kind) combination: not a "may", so an Error like the server's
// own CREATE-time refusals, not a Violation.
func (a *analyzer) walkDML(n *mysqlast.Node, kind facts.StmtKind, br *BodyResult, run func(*mysqlast.Node) error) error {
	a.resetStatement()
	if err := run(n); err != nil {
		return err
	}
	if a.trig != nil && a.write != nil {
		if own, name := a.ownTableWrite(a.write); own {
			return &Error{Message: fmt.Sprintf("Can't update table '%s' in stored function/trigger because it is already used by statement which invoked this stored function/trigger.", name), Code: 1442, Position: a.ph.Back(n.Start)}
		}
	}
	for _, v := range a.violations() {
		if v.SQLState == "" {
			v.SQLState = constraintSQLState(v.Code)
		}
		a.raised = append(a.raised, v)
	}
	a.appendStatement(br, n.Start)
	return nil
}

// ownTableWrite reports whether w writes the trigger's own table (directly, or as one of a
// multi-table UPDATE/DELETE's further targets).
func (a *analyzer) ownTableWrite(w *write) (bool, string) {
	if w.table == a.trigTable {
		return true, a.trigTable.Name
	}
	for _, m := range w.more {
		if m.table == a.trigTable {
			return true, a.trigTable.Name
		}
	}
	return false, ""
}
