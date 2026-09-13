package analyze

import (
	"fmt"
	"strings"

	"github.com/kr9ly/sqlshape/check/mysql/v2/internal/mysqlast"
	"github.com/kr9ly/sqlshape/check/mysql/v2/internal/schema"
	"github.com/kr9ly/sqlshape/v2/x/facts"
)

// calledRoutine is one schema-declared routine a statement's analysis met: a FUNCTION
// resolved at a call site inside an expression (expr.go's storedFuncCall), or the
// PROCEDURE a CALL statement itself runs (callStmt, below). pos is the call's own start
// (a.ph.Back applied where it is read), for the 1442 overlap check's position.
type calledRoutine struct {
	r   *schema.Routine
	pos int
}

// noteCalledRoutine records r as called, once (a statement calling the same function twice,
// or through two argument positions, does not fold in its failure modes twice): violations()
// and checkCalledRoutineOverlap both read a.calledRoutines.
func (a *analyzer) noteCalledRoutine(r *schema.Routine, pos int) {
	if a.calledSeen == nil {
		a.calledSeen = map[*schema.Routine]bool{}
	}
	if a.calledSeen[r] {
		return
	}
	a.calledSeen[r] = true
	a.calledRoutines = append(a.calledRoutines, calledRoutine{r: r, pos: pos})
}

// checkCalledRoutineOverlap is 1442 ("Can't update table '%s' in stored function/trigger
// because it is already used by statement which invoked this stored function/trigger"),
// measured unconditionally true on mysqld 8.4 whenever a called routine's own body writes a
// table the invoking statement itself references -- reading it is enough (measured: `SELECT
// writer(v) FROM t` fails the same way an UPDATE of t calling writer() does), not only
// writing it; a routine's body that only reads the table (never a "may", the write is
// certain from the body alone) does not collide. A CALL whose own arguments reference no
// table (the ordinary case: constants, placeholders, `@vars`) never collides through the
// PROCEDURE it runs alone, matching the measurement (`CALL p3(7)`, p3's body writing t,
// raised nothing: the statement itself never mentions t).
func (a *analyzer) checkCalledRoutineOverlap() error {
	if len(a.calledRoutines) == 0 {
		return nil
	}
	refTables := map[string]bool{}
	for _, u := range a.uses {
		if u.Table != "" {
			refTables[strings.ToLower(u.Table)] = true
		}
	}
	if w := a.write; w != nil {
		if w.table != nil {
			refTables[strings.ToLower(w.table.Name)] = true
		}
		for _, m := range w.more {
			if m.table != nil {
				refTables[strings.ToLower(m.table.Name)] = true
			}
		}
	}
	for _, cr := range a.calledRoutines {
		br, err := AnalyzeRoutine(a.s, cr.r)
		if err != nil || br == nil {
			continue
		}
		for _, wt := range br.WriteTables {
			if refTables[strings.ToLower(wt)] {
				return &Error{
					Message:  fmt.Sprintf("Can't update table '%s' in stored function/trigger because it is already used by statement which invoked this stored function/trigger.", wt),
					Code:     1442,
					Position: a.ph.Back(cr.pos),
				}
			}
		}
	}
	return nil
}

// spNameOf reads a sp_name node's own db/name (db is "" when the call was not qualified);
// unqualified is the only form this milestone resolves against (db is ignored, the way a
// table's db qualifier already is in target(): sqlshape loads a single schema).
func spNameOf(v mysqlast.Value) (db, name string) {
	n, ok := v.(*mysqlast.Node)
	if !ok || n.Class != "sp_name" {
		return "", str(v)
	}
	return str(n.Arg("db")), str(n.Arg("name"))
}

// isCallVariableTarget reports whether v is something an OUT/INOUT argument can write back
// into: a placeholder (a prepared statement's own bind slot always qualifies -- measured:
// `CALL p(?, ?)` with any two bound values raises nothing even for the OUT position, since
// the placeholder acts as an anonymous variable slot the same way `@out` does), a `@user`
// variable, or -- inside a trigger's/routine's own body -- a declared local variable or
// parameter. Anything else (a literal, an expression) is 1414 (measured: `CALL p4(5)`, p4's
// sole parameter INOUT, a bare literal in that position).
func (a *analyzer) isCallVariableTarget(v mysqlast.Value) bool {
	if isParam(v) {
		return true
	}
	n, ok := v.(*mysqlast.Node)
	if !ok {
		return false
	}
	switch n.Class {
	case "PTI_user_variable":
		return true
	case "PTI_simple_ident_ident", "PTI_simple_ident_nospvar_ident":
		_, ok := a.lookupVar(str(n.Arg("ident")))
		return ok
	}
	return false
}

// callStmt analyzes CALL name(args): the procedure resolves by name alone (db ignored, see
// spNameOf), its argument count must match (1318, the message measured the same shape a
// function's own mismatch takes), an OUT/INOUT argument must be a variable (1414), an IN or
// INOUT placeholder is typed from the parameter's declared type. The statement's own facts
// are Kind Call (x/facts' own doc: "no scope of its own" -- the body's reads/writes are the
// routine's, not this statement's); AtMostOne follows from there being at most one result
// row (a CALL is never part of an aggregate/GROUP BY question). Its result columns are the
// body's own INTO-less SELECT(s) (resultColumnsOf); its failure modes are the body's own
// (violations(), through calledRoutineViolations), and checkCalledRoutineOverlap covers the
// 1442 a body write can raise the same way a function call's does.
func (a *analyzer) callStmt(n *mysqlast.Node) error {
	_, name := spNameOf(n.Arg("proc_name"))
	r := a.s.RoutineOf(schema.Procedure, name)
	if r == nil {
		return &Error{Message: fmt.Sprintf("PROCEDURE %s does not exist", name), Code: 1305, Position: a.ph.Back(n.Start)}
	}
	args, _ := n.Arg("opt_expr_list").(mysqlast.List)
	if len(args) != len(r.Params) {
		return &Error{Message: fmt.Sprintf("Incorrect number of arguments for PROCEDURE %s; expected %d, got %d", r.Name, len(r.Params), len(args)), Code: 1318, Position: a.ph.Back(n.Start)}
	}
	a.facts = &facts.Facts{Kind: facts.Call, AtMostOne: true}
	for i, arg := range args {
		p := r.Params[i]
		if (p.Mode == "OUT" || p.Mode == "INOUT") && !a.isCallVariableTarget(arg) {
			return &Error{Message: fmt.Sprintf("OUT or INOUT argument %d for routine %s is not a variable", i+1, r.Name), Code: 1414, Position: a.ph.Back(n.Start)}
		}
		if _, err := a.expr(scope{}, arg, "call"); err != nil {
			return err
		}
		if isParam(arg) {
			a.setParam(arg, p.Type)
		}
	}
	a.noteCalledRoutine(r, n.Start)
	br, err := AnalyzeRoutine(a.s, r)
	if err != nil {
		return err
	}
	cols, ok := resultColumnsOf(br)
	if !ok {
		return &Error{Message: fmt.Sprintf("PROCEDURE %s returns result sets of more than one shape", r.Name), Code: 0, Position: a.ph.Back(n.Start)}
	}
	a.columns = cols
	return nil
}

// resultColumnsOf collects a routine's own INTO-less top-level SELECTs (walkSelect marks
// each such statement's own Columns; every other body statement's is nil, including a
// SELECT ... INTO or a cursor's DECLARE ... FOR, which consume their columns rather than
// returning them): with none, a CALL has no result columns; with one, its columns are the
// CALL's; with more than one, only when their shapes agree exactly (the same count, the
// same names in order, the same Known-ness and, when Known, the same type name -- the
// nullability instead ORs across them, the same way a schema constraint's does not have to
// match) do they still agree on one column list. A mismatch is not something mysqld itself
// refuses (it only ever returns whichever result set the execution path taken produced, at
// runtime): ok is false, and callStmt reports it as its own error, not a MySQL one (Code 0).
func resultColumnsOf(br *BodyResult) ([]Column, bool) {
	var sets [][]Column
	for _, st := range br.Statements {
		if len(st.Columns) > 0 {
			sets = append(sets, st.Columns)
		}
	}
	if len(sets) == 0 {
		return nil, true
	}
	out := append([]Column(nil), sets[0]...)
	for _, cols := range sets[1:] {
		if len(cols) != len(out) {
			return nil, false
		}
		for i, c := range cols {
			if !strings.EqualFold(c.Name, out[i].Name) || c.Known != out[i].Known || (c.Known && c.Type.Name != out[i].Type.Name) {
				return nil, false
			}
			if c.Nullable {
				out[i].Nullable = true
			}
		}
	}
	return out, true
}
