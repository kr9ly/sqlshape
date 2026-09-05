package analyze

import (
	pg_query "github.com/pganalyze/pg_query_go/v6"

	"github.com/kr9ly/sqlshape/internal/schema"
)

// Failure modes: which constraints a statement can violate.
//
// The catalog already says how a write can fail: unique keys, foreign keys, CHECKs,
// NOT NULL, domain CHECKs. sqlshape lists them per statement so the caller's error
// handling can be checked against the schema instead of being guessed: the checker
// diffs this list with the `-- sqlshape: expect ...` line of the template, and the
// runtime maps the PG error back to the constraint (ConstraintError).
//
// The list is "may violate", not "will": an INSERT can violate every unique key of the
// table, an UPDATE only those that include a SET column, a DELETE only foreign keys
// that reference the table with NO ACTION / RESTRICT. A NOT NULL violation is listed
// only when the assigned expression may be NULL; when that expression is a bare
// parameter the violation carries its number so the checker can drop it if the Go
// type cannot be NULL.

// Violation is one constraint a statement may violate.
type Violation struct {
	Code       string   // SQLSTATE: 23505 unique, 23503 foreign key, 23514 check, 23502 not null
	Constraint string   // constraint name; "" for NOT NULL (PG 17 does not name them)
	Table      string   // table the constraint belongs to (referencing table for FKs)
	Columns    []string // constrained columns
	RefTable   string   // FK: referenced table
	Param      int32    // NOT NULL: the bare parameter whose NULL would violate (0 otherwise)
}

// Key identifies a violation the way the expect line spells it.
func (v Violation) Key() string {
	if v.Constraint != "" {
		return v.Constraint
	}
	if len(v.Columns) == 1 {
		return v.Table + "." + v.Columns[0]
	}
	return v.Table
}

const (
	codeNotNullViolation    = "23502"
	codeForeignKeyViolation = "23503"
	codeUniqueViolation     = "23505"
	codeCheckViolation      = "23514"
)

// assignment is one value stored into a column by the statement.
type assignment struct {
	rel *schema.Relation
	col *schema.Column
	e   *expr
}

// violations enumerates the constraints the analyzed statement may violate.
func (a *analyzer) violations(stmt *pg_query.Node) []Violation {
	switch st := stmt.Node.(type) {
	case *pg_query.Node_InsertStmt:
		return a.insertViolations(st.InsertStmt)
	case *pg_query.Node_UpdateStmt:
		rel := a.s.Relation(st.UpdateStmt.Relation.Schemaname, st.UpdateStmt.Relation.Relname)
		return a.updateViolations(rel, a.assignedColumns(rel), nil)
	case *pg_query.Node_DeleteStmt:
		rel := a.s.Relation(st.DeleteStmt.Relation.Schemaname, st.DeleteStmt.Relation.Relname)
		return a.referencingViolations(rel, nil, true)
	}
	return nil
}

func (a *analyzer) insertViolations(ins *pg_query.InsertStmt) []Violation {
	rel := a.s.Relation(ins.Relation.Schemaname, ins.Relation.Relname)
	if rel == nil {
		return nil
	}
	inserted := map[string]bool{}
	if len(ins.Cols) == 0 {
		for _, c := range rel.Columns {
			inserted[c.Name] = true
		}
	} else {
		for _, cn := range ins.Cols {
			inserted[cn.GetResTarget().GetName()] = true
		}
	}
	if ins.SelectStmt == nil {
		inserted = map[string]bool{} // DEFAULT VALUES
	}
	// ON CONFLICT absorbs the arbiter's unique constraint
	absorbed := map[string]bool{}
	var conflictSet map[string]bool
	if oc := ins.OnConflictClause; oc != nil {
		if inf := oc.Infer; inf != nil {
			if inf.Conname != "" {
				absorbed[inf.Conname] = true
			} else {
				var cols []string
				for _, ie := range inf.IndexElems {
					cols = append(cols, ie.GetIndexElem().GetName())
				}
				for _, con := range rel.Constraints {
					if (con.Kind == schema.PrimaryKey || con.Kind == schema.Unique) && sameColumns(con.Columns, cols) {
						absorbed[con.Name] = true
					}
				}
			}
		} else {
			// ON CONFLICT DO NOTHING without arbiter: any unique violation is absorbed
			for _, con := range rel.Constraints {
				if con.Kind == schema.PrimaryKey || con.Kind == schema.Unique {
					absorbed[con.Name] = true
				}
			}
		}
		if oc.Action == pg_query.OnConflictAction_ONCONFLICT_UPDATE {
			conflictSet = map[string]bool{}
			for _, tn := range oc.TargetList {
				conflictSet[tn.GetResTarget().GetName()] = true
			}
		}
	}
	var out []Violation
	for _, con := range rel.Constraints {
		switch con.Kind {
		case schema.PrimaryKey, schema.Unique:
			if !absorbed[con.Name] {
				out = append(out, Violation{Code: codeUniqueViolation, Constraint: con.Name, Table: rel.Name, Columns: con.Columns})
			}
		case schema.ForeignKey:
			if anyIn(con.Columns, inserted) {
				out = append(out, Violation{Code: codeForeignKeyViolation, Constraint: con.Name, Table: rel.Name, Columns: con.Columns, RefTable: con.RefTable})
			}
		case schema.Check:
			if anyIn(checkColumns(con), inserted) {
				out = append(out, Violation{Code: codeCheckViolation, Constraint: con.Name, Table: rel.Name, Columns: checkColumns(con)})
			}
		}
	}
	for _, c := range rel.Columns {
		if !inserted[c.Name] {
			if c.NotNull && c.Default == nil && c.Identity == 0 && c.Generated == nil {
				a.note("always-fails", ins.Relation.Location, "INSERT omits "+rel.Name+"."+c.Name+", which is NOT NULL without a default: every execution fails")
			}
			continue
		}
		out = append(out, a.columnViolations(rel, c)...)
	}
	if conflictSet != nil {
		for _, v := range a.updateViolations(rel, conflictSet, absorbed) {
			out = append(out, v)
		}
	}
	return dedupe(out)
}

// updateViolations lists what storing into the set columns of rel may violate.
func (a *analyzer) updateViolations(rel *schema.Relation, set map[string]bool, skip map[string]bool) []Violation {
	if rel == nil {
		return nil
	}
	var out []Violation
	for _, con := range rel.Constraints {
		if skip[con.Name] {
			continue
		}
		switch con.Kind {
		case schema.PrimaryKey, schema.Unique:
			if anyIn(con.Columns, set) {
				out = append(out, Violation{Code: codeUniqueViolation, Constraint: con.Name, Table: rel.Name, Columns: con.Columns})
			}
		case schema.ForeignKey:
			if anyIn(con.Columns, set) {
				out = append(out, Violation{Code: codeForeignKeyViolation, Constraint: con.Name, Table: rel.Name, Columns: con.Columns, RefTable: con.RefTable})
			}
		case schema.Check:
			if anyIn(checkColumns(con), set) {
				out = append(out, Violation{Code: codeCheckViolation, Constraint: con.Name, Table: rel.Name, Columns: checkColumns(con)})
			}
		}
	}
	for _, c := range rel.Columns {
		if set[c.Name] {
			out = append(out, a.columnViolations(rel, c)...)
		}
	}
	out = append(out, a.referencingViolations(rel, set, false)...)
	return dedupe(out)
}

// columnViolations: NOT NULL (when a nullable value is assigned) and domain CHECKs of one column.
func (a *analyzer) columnViolations(rel *schema.Relation, c *schema.Column) []Violation {
	var out []Violation
	if c.NotNull || a.domainNotNull(c.Type.OID) {
		for _, as := range a.assigned {
			if as.rel == rel && as.col == c && as.e.nullable {
				v := Violation{Code: codeNotNullViolation, Table: rel.Name, Columns: []string{c.Name}}
				v.Param = as.e.param
				out = append(out, v)
				break
			}
		}
	}
	for oid := c.Type.OID; ; {
		t := a.s.Types.ByOID(oid)
		if t == nil || t.Kind != 'd' {
			break
		}
		if d := a.s.Types.Domains[oid]; d != nil {
			for _, chk := range d.Checks {
				out = append(out, Violation{Code: codeCheckViolation, Constraint: chk.Name, Table: rel.Name, Columns: []string{c.Name}})
			}
		}
		oid = t.BaseType
	}
	return out
}

// referencingViolations lists foreign keys of other tables that point at rel and would
// reject the change: any on DELETE, those referencing a changed column on UPDATE
// (CASCADE / SET NULL / SET DEFAULT never fail here; they fail as the cascaded change).
func (a *analyzer) referencingViolations(rel *schema.Relation, changed map[string]bool, del bool) []Violation {
	if rel == nil {
		return nil
	}
	var out []Violation
	for _, other := range a.s.Relations {
		for _, con := range other.Constraints {
			if con.Kind != schema.ForeignKey || a.relByFullName(con.RefTable) != rel {
				continue
			}
			action := con.OnUpdate
			if del {
				action = con.OnDelete
			}
			if action != 'a' && action != 'r' {
				continue
			}
			if !del {
				ref := con.RefColumns
				if len(ref) == 0 {
					ref = a.primaryKey(rel)
				}
				if !anyIn(ref, changed) {
					continue
				}
			}
			out = append(out, Violation{Code: codeForeignKeyViolation, Constraint: con.Name, Table: other.Name, Columns: con.Columns, RefTable: rel.Name})
		}
	}
	return out
}

func (a *analyzer) relByFullName(name string) *schema.Relation {
	for _, r := range a.s.Relations {
		if r.FullName() == name || (r.Schema == "public" && r.Name == name) {
			return r
		}
	}
	return nil
}

func (a *analyzer) primaryKey(rel *schema.Relation) []string {
	for _, con := range rel.Constraints {
		if con.Kind == schema.PrimaryKey {
			return con.Columns
		}
	}
	return nil
}

// assignedColumns is the set of rel's columns the statement stores into.
func (a *analyzer) assignedColumns(rel *schema.Relation) map[string]bool {
	set := map[string]bool{}
	for _, as := range a.assigned {
		if as.rel == rel {
			set[as.col.Name] = true
		}
	}
	return set
}

func checkColumns(con *schema.Constraint) []string {
	if len(con.Columns) > 0 {
		return con.Columns
	}
	return schema.ColumnRefs(con.Expr)
}

func anyIn(cols []string, set map[string]bool) bool {
	for _, c := range cols {
		if set[c] {
			return true
		}
	}
	return false
}

func sameColumns(x, y []string) bool {
	if len(x) != len(y) {
		return false
	}
	m := map[string]bool{}
	for _, c := range x {
		m[c] = true
	}
	for _, c := range y {
		if !m[c] {
			return false
		}
	}
	return true
}

func dedupe(vs []Violation) []Violation {
	seen := map[string]bool{}
	var out []Violation
	for _, v := range vs {
		k := v.Code + " " + v.Key()
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, v)
	}
	return out
}
