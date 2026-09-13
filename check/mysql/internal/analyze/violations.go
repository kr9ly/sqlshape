package analyze

import (
	"strings"

	"github.com/kr9ly/sqlshape/check/mysql/v2/internal/mysqlast"
	"github.com/kr9ly/sqlshape/check/mysql/v2/internal/schema"
	"github.com/kr9ly/sqlshape/v2/x/facts"
)

// Failure modes: which constraints a statement can violate.
//
// The schema says how a write can fail: unique keys, foreign keys, CHECKs, NOT NULL.
// sqlshape lists them per statement so the caller's error handling can be checked against
// the schema instead of being guessed: the checker diffs this list with the
// `-- sqlshape: expect ...` line of the template, and the runtime maps the server's error
// number back to the constraint (mysql.ConstraintError).
//
// The list is "may violate", not "will": an INSERT can violate every unique key of the
// table (but not a key made only of an AUTO_INCREMENT column it leaves to the server), an
// UPDATE only those that include a SET column, a DELETE only the foreign keys that
// reference the table with RESTRICT / NO ACTION. A NOT NULL violation is listed only when
// the stored expression may be NULL; when that expression is a bare parameter the
// violation carries its number so the checker can drop it if the Go type cannot be NULL.
// INSERT / UPDATE / DELETE IGNORE turn every constraint error into a warning, so such a
// statement violates nothing; INSERT ... ON DUPLICATE KEY UPDATE absorbs the unique
// violations of the insert and may violate what its update assigns.

// Violation is one constraint a statement may violate.
type Violation struct {
	Code       int      // MySQL error number: 1062 duplicate key, 1452 foreign key (child), 1451 foreign key (parent row), 1048 NOT NULL, 3819 CHECK, 1442 a trigger writing its own table, 1172 SELECT INTO more than one row, 1644/1643/a custom MYSQL_ERRNO for a SIGNAL
	Constraint string   // the key's, FOREIGN KEY's or CHECK's name; "" for NOT NULL; for a SIGNAL, the key body.go computes (the MYSQL_ERRNO as text, or the SQLSTATE)
	Table      string   // the table the constraint belongs to (the referencing table for a foreign key, or a trigger's own table for a SIGNAL)
	Columns    []string // constrained columns
	RefTable   string   // foreign key: the referenced table
	Param      int      // NOT NULL: the bare parameter whose NULL would violate (0 otherwise)
	// Trigger / Name: a SIGNAL raised by a trigger's body (Trigger is the trigger's name);
	// Name is the `-- sqlshape: error <key> = <Name>` annotation's name, "" when none was
	// declared for this key. Neither is read by Key (see check/postgres/analyze/violation.go's
	// own Key, which does not prefer Name over Constraint either -- Name only decorates the
	// description body.go / dialect.go build).
	Trigger string
	Name    string
	// Function is the stored FUNCTION whose call site (through a SELECT/WHERE/... in the
	// analyzed statement, or the routine a CALL's own PROCEDURE runs) is where this failure
	// mode was found: the function's/procedure's own body raised it (a SIGNAL, or one of its
	// embedded writes), and it propagates to whatever statement called it (m6).
	Function string
	// SQLState is the failure's SQLSTATE class, body.go's own bookkeeping for a DECLARE ...
	// HANDLER's absorption (SQLWARNING is class "01", NOT FOUND is class "02", SQLEXCEPTION
	// is anything else); "" outside a trigger's/routine's body walk, where nothing absorbs.
	SQLState string
}

const (
	codeDuplicateKey   = 1062
	codeForeignKeyRow  = 1452 // "Cannot add or update a child row"
	codeForeignKeyRef  = 1451 // "Cannot delete or update a parent row"
	codeNotNull        = 1048
	codeCheckViolation = 3819
	code1442           = 1442 // a trigger writing its own table: always fails (measured)
	code1172           = 1172 // SELECT ... INTO with more than one row
)

// Key identifies a violation the way the expect line and mysql.Violates spell it: the
// constraint's name, or table.column for NOT NULL.
func (v Violation) Key() string {
	if v.Constraint != "" {
		return v.Constraint
	}
	if len(v.Columns) == 1 {
		return v.Table + "." + v.Columns[0]
	}
	return v.Table
}

// violations enumerates the constraints the analyzed statement may violate: the schema's
// own (unique keys, foreign keys, CHECKs, NOT NULL -- absorbed by IGNORE) plus what the
// write's table's triggers may raise for this event (never absorbed by IGNORE: measured on
// mysqld, an INSERT IGNORE whose BEFORE INSERT trigger SIGNALs, or whose trigger's own
// embedded write collides on a constraint, still fails the whole statement).
func (a *analyzer) violations() []Violation {
	var out []Violation
	if w := a.write; w != nil && w.table != nil {
		if !w.ignore {
			switch w.kind {
			case facts.Insert:
				out = a.insertViolations(w)
			case facts.Update:
				strict := a.s.Settings.Strict()
				out = a.updateViolations(w.table, w.values, nil, strict)
				for _, m := range w.more {
					out = append(out, a.updateViolations(m.table, m.values, nil, strict)...)
				}
			case facts.Delete:
				out = a.referencingViolations(w.table, nil, true, map[*schema.Table]bool{})
				for _, m := range w.more {
					out = append(out, a.referencingViolations(m.table, nil, true, map[*schema.Table]bool{})...)
				}
			}
		}
		out = append(out, a.triggerFailureModes(w)...)
	}
	out = append(out, a.calledRoutineViolations()...)
	return dedupe(out)
}

// calledRoutineViolations lists what the FUNCTIONs a call site in this statement resolved
// (expr.go's call), and the PROCEDURE a CALL statement itself runs, may raise: each one's
// own body, analyzed once and cached (AnalyzeRoutine), already resolved to its own failure
// modes the same way a trigger's are (body.go). Tagged with Function so the description can
// say which one (dialect.go's describeViolation).
func (a *analyzer) calledRoutineViolations() []Violation {
	var out []Violation
	for _, cr := range a.calledRoutines {
		br, err := AnalyzeRoutine(a.s, cr.r)
		if err != nil || br == nil {
			continue
		}
		for _, v := range br.Violations {
			v.Function = cr.r.Name
			out = append(out, v)
		}
	}
	return out
}

// triggerFailureModes lists what the write's table's triggers may raise for this
// statement's event(s), through triggerViolations (which each trigger's own body,
// analyzed once and cached, has already resolved with its HANDLERs absorbed -- see
// body.go). REPLACE fires the INSERT event's triggers always and the DELETE event's when
// it displaces a row (measured: BI, then on a collision BD/AD before AI); ON DUPLICATE KEY
// UPDATE fires the INSERT event's triggers always and the UPDATE event's on a collision
// (measured: BI always, then BU/AU instead of AI on a collision) -- both "may", the way
// every other failure mode here is.
func (a *analyzer) triggerFailureModes(w *write) []Violation {
	var out []Violation
	switch w.kind {
	case facts.Insert:
		out = append(out, triggerViolations(a.s, w.table, "INSERT")...)
		if w.replace {
			out = append(out, triggerViolations(a.s, w.table, "DELETE")...)
		}
		if w.onDuplicate != nil {
			out = append(out, triggerViolations(a.s, w.table, "UPDATE")...)
		}
	case facts.Update:
		out = append(out, triggerViolations(a.s, w.table, "UPDATE")...)
		for _, m := range w.more {
			out = append(out, triggerViolations(a.s, m.table, "UPDATE")...)
		}
	case facts.Delete:
		out = append(out, triggerViolations(a.s, w.table, "DELETE")...)
		for _, m := range w.more {
			out = append(out, triggerViolations(a.s, m.table, "DELETE")...)
		}
	}
	return out
}

// triggerViolations lists what t's triggers for event (INSERT/UPDATE/DELETE) may raise: each
// matching trigger's own body, analyzed once and cached (cachedAnalyzeTrigger), already
// resolved to its own failure modes (its SIGNALs, and what its own embedded writes may
// violate -- including their own triggers, recursively, a cycle broken by the cache's
// in-progress marker: see body.go).
func triggerViolations(s *schema.Schema, t *schema.Table, event string) []Violation {
	if t == nil {
		return nil
	}
	var out []Violation
	for _, tg := range s.Triggers {
		if !strings.EqualFold(tg.Table, t.Name) || !strings.EqualFold(tg.Event, event) {
			continue
		}
		br, err := cachedAnalyzeTrigger(s, tg)
		if err != nil || br == nil {
			continue
		}
		out = append(out, br.Violations...)
	}
	return dedupe(out)
}

// constraintSQLState is the SQLSTATE class a schema-constraint violation carries, for a
// DECLARE ... HANDLER FOR SQLEXCEPTION inside a trigger/routine body to absorb it the same
// way it absorbs a SIGNAL (see body.go's block): every one of these is a real error MySQL
// raises, never a warning or a NOT FOUND condition, so the exact digits do not matter to
// absorption, only that they are not "01" or "02".
func constraintSQLState(code int) string {
	switch code {
	case codeDuplicateKey, codeForeignKeyRow, codeForeignKeyRef, codeNotNull, codeCheckViolation:
		return "23000"
	case code1442:
		return "HY000"
	case code1172:
		return "42000"
	}
	return "HY000"
}

func (a *analyzer) insertViolations(w *write) []Violation {
	t := w.table
	var out []Violation
	for _, k := range t.Keys {
		if k.Kind != schema.Primary && k.Kind != schema.Unique || w.onDuplicate != nil || w.replace {
			continue // ON DUPLICATE KEY UPDATE and REPLACE absorb a colliding key
		}
		cols, ok := keyColumns(k)
		if !ok || systemGenerated(t, cols, w.inserted) || leftNull(t, cols, w.inserted) {
			continue
		}
		out = append(out, Violation{Code: codeDuplicateKey, Constraint: k.Name, Table: t.Name, Columns: cols})
	}
	for _, fk := range t.ForeignKeys {
		if anyIn(fk.Columns, w.inserted) {
			out = append(out, Violation{Code: codeForeignKeyRow, Constraint: t.ForeignKeyName(fk), Table: t.Name, Columns: fk.Columns, RefTable: fk.RefTable})
		}
	}
	for _, c := range t.Checks {
		if cols := exprColumns(c.Expr); c.Enforced && anyIn(cols, w.inserted) {
			out = append(out, Violation{Code: codeCheckViolation, Constraint: t.CheckName(c), Table: t.Name, Columns: cols})
		}
	}
	// a NULL for a NOT NULL column: an error in strict mode, and for a single-row INSERT
	// (its ON DUPLICATE KEY UPDATE included) in any mode
	notNull := a.s.Settings.Strict() || w.rows == 1
	if notNull {
		out = append(out, notNullViolations(t, w.values)...)
	}
	if w.onDuplicate != nil {
		out = append(out, a.updateViolations(t, w.onDuplicate, nil, notNull)...)
	}
	if w.replace {
		// the row a REPLACE displaces is deleted: the rows referring to it object
		out = append(out, a.referencingViolations(t, nil, true, map[*schema.Table]bool{})...)
	}
	return dedupe(out)
}

// systemGenerated: the key is made only of AUTO_INCREMENT columns the INSERT leaves to the
// server, which never collide.
func systemGenerated(t *schema.Table, cols []string, inserted map[string]bool) bool {
	for _, name := range cols {
		c := t.Column(name)
		if c == nil || !c.AutoIncrement || inserted[name] {
			return false
		}
	}
	return true
}

// leftNull: no column of the key is inserted and each one defaults to NULL, so the key
// holds NULL, which a unique key never rejects.
func leftNull(t *schema.Table, cols []string, inserted map[string]bool) bool {
	for _, name := range cols {
		c := t.Column(name)
		if c == nil || inserted[name] || c.NotNull || c.Default != nil || c.AutoIncrement {
			return false
		}
	}
	return true
}

// updateViolations: the constraints an UPDATE (or the update of ON DUPLICATE KEY UPDATE)
// storing values into t may violate.
func (a *analyzer) updateViolations(t *schema.Table, values []assignment, skip map[string]bool, notNull bool) []Violation {
	set := map[string]bool{}
	for _, as := range values {
		set[as.col.Name] = true
	}
	var out []Violation
	for _, k := range t.Keys {
		if k.Kind != schema.Primary && k.Kind != schema.Unique || skip[k.Name] {
			continue
		}
		if cols, ok := keyColumns(k); ok && anyIn(cols, set) {
			out = append(out, Violation{Code: codeDuplicateKey, Constraint: k.Name, Table: t.Name, Columns: cols})
		}
	}
	for _, fk := range t.ForeignKeys {
		if anyIn(fk.Columns, set) {
			out = append(out, Violation{Code: codeForeignKeyRow, Constraint: t.ForeignKeyName(fk), Table: t.Name, Columns: fk.Columns, RefTable: fk.RefTable})
		}
	}
	for _, c := range t.Checks {
		if cols := exprColumns(c.Expr); c.Enforced && anyIn(cols, set) {
			out = append(out, Violation{Code: codeCheckViolation, Constraint: t.CheckName(c), Table: t.Name, Columns: cols})
		}
	}
	if notNull {
		out = append(out, notNullViolations(t, values)...)
	}
	out = append(out, a.referencingViolations(t, set, false, map[*schema.Table]bool{})...)
	return dedupe(out)
}

// notNullViolations: a NOT NULL column stored a value that may be NULL.
func notNullViolations(t *schema.Table, values []assignment) []Violation {
	var out []Violation
	seen := map[*schema.Column]bool{}
	for _, as := range values {
		// an AUTO_INCREMENT column given NULL takes the next number instead
		if seen[as.col] || !as.col.NotNull || as.col.Generated != nil || as.col.AutoIncrement || !as.nullable {
			continue
		}
		seen[as.col] = true
		out = append(out, Violation{Code: codeNotNull, Table: t.Name, Columns: []string{as.col.Name}, Param: as.param})
	}
	return out
}

// referencingViolations: the foreign keys of other tables that reference t reject (or
// cascade) a DELETE of its rows, or an UPDATE of the columns they reference.
func (a *analyzer) referencingViolations(t *schema.Table, changed map[string]bool, del bool, visited map[*schema.Table]bool) []Violation {
	if visited[t] {
		return nil
	}
	visited[t] = true
	var out []Violation
	for _, other := range a.s.Tables {
		for _, fk := range other.ForeignKeys {
			if !strings.EqualFold(fk.RefTable, t.Name) {
				continue
			}
			action := fk.OnUpdate
			if del {
				action = fk.OnDelete
			}
			if !del {
				ref := fk.RefColumns
				if len(ref) == 0 {
					if pk := t.PrimaryKey(); pk != nil {
						ref, _ = keyColumns(pk)
					}
				}
				if !anyIn(ref, changed) {
					continue
				}
			}
			switch strings.ToUpper(action) {
			case "", "RESTRICT", "NO ACTION": // the change itself is rejected
				out = append(out, Violation{Code: codeForeignKeyRef, Constraint: other.ForeignKeyName(fk), Table: other.Name, Columns: fk.Columns, RefTable: t.Name})
			case "CASCADE": // the referencing rows are deleted / updated the same way, cascading further
				out = append(out, a.referencingViolations(other, colSet(fk.Columns), del, visited)...)
			}
			// SET NULL / SET DEFAULT: InnoDB refuses the declaration when the columns are NOT
			// NULL, so the action itself cannot fail
		}
	}
	return out
}

// keyColumns lists a key's columns when every part is a whole column (a prefix or an
// expression part is not something an equality on the column fixes).
func keyColumns(k *schema.Key) ([]string, bool) {
	cols := make([]string, 0, len(k.Parts))
	for _, p := range k.Parts {
		if p.Expr != nil || p.Length != 0 || p.Column == "" {
			return nil, false
		}
		cols = append(cols, p.Column)
	}
	return cols, len(cols) > 0
}

// exprColumns lists the column names an expression references (a CHECK's), in order of
// first appearance.
func exprColumns(v mysqlast.Value) []string {
	var out []string
	seen := map[string]bool{}
	var walk func(v mysqlast.Value)
	walk = func(v mysqlast.Value) {
		switch x := v.(type) {
		case *mysqlast.Node:
			switch x.Class {
			case "PTI_simple_ident_ident", "PTI_simple_ident_nospvar_ident":
				if name := str(x.Arg("ident")); name != "" && !seen[strings.ToLower(name)] {
					seen[strings.ToLower(name)] = true
					out = append(out, name)
				}
				return
			case "PTI_simple_ident_q_2d", "PTI_simple_ident_q_3d":
				if name := str(x.Arg("field")); name != "" && !seen[strings.ToLower(name)] {
					seen[strings.ToLower(name)] = true
					out = append(out, name)
				}
				return
			}
			for _, arg := range x.Args {
				walk(arg)
			}
		case mysqlast.List:
			for _, e := range x {
				walk(e)
			}
		case *mysqlast.Struct:
			for _, k := range x.Order {
				walk(x.Fields[k])
			}
		}
	}
	walk(v)
	return out
}

func colSet(cols []string) map[string]bool {
	m := map[string]bool{}
	for _, c := range cols {
		m[c] = true
	}
	return m
}

func anyIn(cols []string, set map[string]bool) bool {
	for _, c := range cols {
		if set[c] {
			return true
		}
	}
	return false
}

func dedupe(vs []Violation) []Violation {
	seen := map[string]bool{}
	var out []Violation
	for _, v := range vs {
		k := v.Key() + "\x00" + itoa(v.Code)
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, v)
	}
	return out
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for ; n > 0; n /= 10 {
		b = append([]byte{byte('0' + n%10)}, b...)
	}
	return string(b)
}
