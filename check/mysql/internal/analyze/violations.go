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
	Code       int      // MySQL error number: 1062 duplicate key, 1452 foreign key (child), 1451 foreign key (parent row), 1048 NOT NULL, 3819 CHECK
	Constraint string   // the key's, FOREIGN KEY's or CHECK's name; "" for NOT NULL
	Table      string   // the table the constraint belongs to (the referencing table for a foreign key)
	Columns    []string // constrained columns
	RefTable   string   // foreign key: the referenced table
	Param      int      // NOT NULL: the bare parameter whose NULL would violate (0 otherwise)
}

const (
	codeDuplicateKey   = 1062
	codeForeignKeyRow  = 1452 // "Cannot add or update a child row"
	codeForeignKeyRef  = 1451 // "Cannot delete or update a parent row"
	codeNotNull        = 1048
	codeCheckViolation = 3819
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

// violations enumerates the constraints the analyzed statement may violate.
func (a *analyzer) violations() []Violation {
	w := a.write
	if w == nil || w.table == nil || w.ignore {
		return nil
	}
	var out []Violation
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
	return dedupe(out)
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
