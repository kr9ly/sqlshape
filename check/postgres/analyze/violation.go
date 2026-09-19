package analyze

import (
	"strings"

	"github.com/kr9ly/sqlshape/check/postgres/v2/catalog"
	"github.com/kr9ly/sqlshape/check/postgres/v2/pgparse"
	"github.com/kr9ly/sqlshape/check/postgres/v2/schema"
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
	// Trigger / Name: a custom SQLSTATE raised by a trigger function (Constraint holds the code)
	Trigger string
	Name    string
	// Function is the user function whose body (or `-- sqlshape: error` annotation) this
	// violation comes from, when the statement reaches it through a call.
	Function string
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
	codeNotNullViolation     = "23502"
	codeForeignKeyViolation  = "23503"
	codeUniqueViolation      = "23505"
	codeCheckViolation       = "23514"
	codeExclusionViolation   = "23P01"
	codeCheckOptionViolation = "44000"
)

// assignment is one value stored into a column by the statement.
type assignment struct {
	rel *schema.Relation
	col *schema.Column
	e   *expr
	w   int // index into a.writeRecs of the write this assignment belongs to (-1: none)
}

// violations enumerates the constraints the analyzed statement may violate.
func (a *analyzer) violations(stmt *pgparse.Node) []Violation {
	switch st := stmt.Node.(type) {
	case *pgparse.Node_InsertStmt:
		rel := a.s.Relation(st.InsertStmt.Relation.Schemaname, st.InsertStmt.Relation.Relname)
		return append(a.insertViolations(st.InsertStmt), a.triggerViolations(rel, 'i', nil)...)
	case *pgparse.Node_UpdateStmt:
		rel := a.s.Relation(st.UpdateStmt.Relation.Schemaname, st.UpdateStmt.Relation.Relname)
		return append(a.updateViolations(rel, a.assignedColumns(rel), nil), a.triggerViolations(rel, 'u', a.assignedColumns(rel))...)
	case *pgparse.Node_DeleteStmt:
		rel := a.s.Relation(st.DeleteStmt.Relation.Schemaname, st.DeleteStmt.Relation.Relname)
		return append(a.referencingViolations(rel, nil, true), a.triggerViolations(rel, 'd', nil)...)
	case *pgparse.Node_TruncateStmt:
		a.truncateNotes(st.TruncateStmt)
		return nil
	case *pgparse.Node_MergeStmt:
		// the union of what its actions may violate
		rel := a.s.Relation(st.MergeStmt.Relation.Schemaname, st.MergeStmt.Relation.Relname)
		var out []Violation
		if a.mergeActions&mergeInsert != 0 {
			ins := &pgparse.InsertStmt{Relation: st.MergeStmt.Relation, SelectStmt: &pgparse.Node{}}
			for _, c := range a.mergeInserted {
				ins.Cols = append(ins.Cols, &pgparse.Node{Node: &pgparse.Node_ResTarget{ResTarget: &pgparse.ResTarget{Name: c}}})
			}
			out = append(out, a.insertViolations(ins)...)
			out = append(out, a.triggerViolations(rel, 'i', nil)...)
		}
		if a.mergeActions&mergeUpdate != 0 {
			out = append(out, a.updateViolations(rel, a.assignedColumns(rel), nil)...)
			out = append(out, a.triggerViolations(rel, 'u', a.assignedColumns(rel))...)
		}
		if a.mergeActions&mergeDelete != 0 {
			out = append(out, a.referencingViolations(rel, nil, true)...)
			out = append(out, a.triggerViolations(rel, 'd', nil)...)
		}
		return dedupe(out)
	}
	return nil
}

// triggerViolations lists the custom SQLSTATEs the table's triggers for the event raise
// (declared with `-- sqlshape: error XX001 = Name` on the trigger function).
func (a *analyzer) triggerViolations(rel *schema.Relation, event byte, assigned map[string]bool) []Violation {
	if rel == nil {
		return nil
	}
	var out []Violation
	for _, tg := range a.s.Triggers {
		if tg.Table != rel.FullName() {
			continue
		}
		if (event == 'i' && !tg.Insert) || (event == 'u' && !tg.Update) || (event == 'd' && !tg.Delete) {
			continue
		}
		if event == 'u' && len(tg.UpdateOf) > 0 && !anyIn(tg.UpdateOf, assigned) {
			continue // UPDATE OF col: the trigger does not fire for these assignments
		}
		fs, fname := "", tg.Function
		if i := strings.LastIndex(fname, "."); i >= 0 {
			fs, fname = fname[:i], fname[i+1:]
		}
		fn := a.s.Function(fs, fname)
		if fn == nil {
			continue
		}
		for _, r := range raisedErrors(a.s, fn) {
			out = append(out, Violation{Code: r.Code, Constraint: r.Code, Table: rel.Name, Trigger: tg.Name, Name: r.Name})
		}
	}
	return dedupe(out)
}

func (a *analyzer) insertViolations(ins *pgparse.InsertStmt) []Violation {
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
	var arbiter *schema.Constraint
	var conflictSet map[string]bool
	if oc := ins.OnConflictClause; oc != nil {
		if inf := oc.Infer; inf != nil {
			if inf.Conname != "" {
				for _, con := range rel.Constraints {
					if con.Name == inf.Conname {
						arbiter = con
						break
					}
				}
				absorbed[inf.Conname] = true
			} else {
				var cols []string
				for _, ie := range inf.IndexElems {
					cols = append(cols, ie.GetIndexElem().GetName())
				}
				for _, con := range rel.Constraints {
					// PG never accepts a DEFERRABLE unique/exclusion constraint as an
					// inference arbiter, and a partial one only when its own predicate is
					// exactly the ON CONFLICT specification's WHERE clause (SQLSTATE
					// 42P10 / 55000 otherwise) -- so an unmatched candidate is left
					// unabsorbed rather than silently accepted.
					if (con.Kind == schema.PrimaryKey || con.Kind == schema.Unique) && sameColumns(con.Columns, cols) &&
						!con.Deferrable && predicateMatches(con.Predicate, inf.WhereClause) {
						absorbed[con.Name] = true
					}
				}
			}
		} else {
			// ON CONFLICT DO NOTHING without arbiter: every unique constraint, unique index
			// (partial ones included) and exclusion constraint of the table is an arbiter, so
			// each one's violation is absorbed -- unless one of them is DEFERRABLE, which the
			// server refuses as an arbiter and, with no target to narrow the choice, refuses
			// the statement (SQLSTATE 55000) whether or not a row conflicts
			for _, con := range rel.Constraints {
				if con.Kind != schema.PrimaryKey && con.Kind != schema.Unique && con.Kind != schema.Exclude {
					continue
				}
				if con.Deferrable {
					a.note(noteAlwaysFails, ins.Relation.Location, "ON CONFLICT DO NOTHING without a conflict target on "+rel.Name+
						": every execution fails (SQLSTATE 55000): "+con.Name+" is DEFERRABLE, and a deferrable constraint cannot be an arbiter")
				}
				absorbed[con.Name] = true
			}
		}
		if oc.Action == pgparse.OnConflictAction_ONCONFLICT_UPDATE {
			if arbiter != nil && arbiter.Kind == schema.Exclude {
				// PG never allows DO UPDATE when the arbiter is an exclusion constraint
				// (only DO NOTHING is supported there), unconditionally.
				a.note(noteAlwaysFails, ins.Relation.Location, "ON CONFLICT ON CONSTRAINT "+arbiter.Name+
					" DO UPDATE: every execution fails (SQLSTATE 42809): exclusion constraints are not "+
					"supported as a DO UPDATE arbiter")
			}
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
			// a key made only of GENERATED ALWAYS AS IDENTITY columns the INSERT leaves to
			// the system cannot collide: nothing but the sequence ever writes it
			if !absorbed[con.Name] && !systemGenerated(rel, con.Columns, inserted) {
				out = append(out, Violation{Code: codeUniqueViolation, Constraint: con.Name, Table: rel.Name, Columns: con.Columns})
			}
		case schema.ForeignKey:
			if anyIn(con.Columns, inserted) && !con.NotEnforced {
				out = append(out, Violation{Code: codeForeignKeyViolation, Constraint: con.Name, Table: rel.Name, Columns: con.Columns, RefTable: con.RefTable})
			}
		case schema.Check:
			if anyIn(checkColumns(con), inserted) && !con.NotEnforced {
				out = append(out, Violation{Code: codeCheckViolation, Constraint: con.Name, Table: rel.Name, Columns: checkColumns(con)})
			}
		case schema.Exclude:
			if anyIn(con.Columns, inserted) && !absorbed[con.Name] {
				out = append(out, Violation{Code: codeExclusionViolation, Constraint: con.Name, Table: rel.Name, Columns: con.Columns})
			}
		}
	}
	for _, c := range rel.Columns {
		if !inserted[c.Name] {
			if c.NotNull && c.Default == nil && c.Identity == 0 && c.Generated == nil {
				a.note(noteAlwaysFails, ins.Relation.Location, "INSERT omits "+rel.Name+"."+c.Name+", which is NOT NULL without a default: every execution fails")
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
	out = append(out, a.checkOptionViolations(rel)...)
	return dedupe(out)
}

// systemGenerated reports whether every column is one the statement leaves to the system
// and the system never repeats: GENERATED ALWAYS AS IDENTITY, or a DEFAULT that generates
// a random UUID.
func systemGenerated(rel *schema.Relation, cols []string, inserted map[string]bool) bool {
	if len(cols) == 0 {
		return false
	}
	for _, name := range cols {
		c := rel.Column(name)
		if c == nil || inserted[name] || (c.Identity != 'a' && !uuidDefault(c.Default)) {
			return false
		}
	}
	return true
}

// uuidDefault reports whether a DEFAULT expression is a call to a UUID generator.
func uuidDefault(e schema.Expr) bool {
	f := e.GetFuncCall()
	if f == nil || len(f.Args) > 0 {
		return false
	}
	switch f.Funcname[len(f.Funcname)-1].GetString_().GetSval() {
	case "gen_random_uuid", "uuidv4", "uuidv7", "uuid_generate_v1", "uuid_generate_v1mc", "uuid_generate_v4":
		return true
	}
	return false
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
			if anyIn(con.Columns, set) && !con.NotEnforced {
				out = append(out, Violation{Code: codeForeignKeyViolation, Constraint: con.Name, Table: rel.Name, Columns: con.Columns, RefTable: con.RefTable})
			}
		case schema.Check:
			if anyIn(checkColumns(con), set) && !con.NotEnforced {
				out = append(out, Violation{Code: codeCheckViolation, Constraint: con.Name, Table: rel.Name, Columns: checkColumns(con)})
			}
		case schema.Exclude:
			if anyIn(con.Columns, set) {
				out = append(out, Violation{Code: codeExclusionViolation, Constraint: con.Name, Table: rel.Name, Columns: con.Columns})
			}
		}
	}
	for _, c := range rel.Columns {
		if set[c.Name] {
			out = append(out, a.columnViolations(rel, c)...)
		}
	}
	out = append(out, a.referencingViolations(rel, set, false)...)
	out = append(out, a.checkOptionViolations(rel)...)
	return dedupe(out)
}

// columnViolations: NOT NULL (when a nullable value is assigned) and domain CHECKs of one column.
func (a *analyzer) columnViolations(rel *schema.Relation, c *schema.Column) []Violation {
	var out []Violation
	if c.NotNull || a.domainNotNull(c.Type.OID) {
		for _, as := range a.assigned {
			if as.rel == rel && as.col == c && as.e.nullable {
				v := a.notNullViolation(rel.Name, c)
				v.Param = as.e.param
				if v.Param == 0 {
					v.Param = as.e.fparam
				}
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

// notNullViolation builds the 23502 for storing NULL into c of the named table: PG names a
// column's own NOT NULL by <table>.<column> (it has no constraint name to report), but a
// NOT NULL that comes from a domain instead is reported by the domain's own name -- PG's
// error there carries no table/column at all, so <table>.<column> could not be built even if
// wanted (a domain's DataTypeName is all pgconn.PgError gives the runtime; see
// postgres/runtime.go).
func (a *analyzer) notNullViolation(tableName string, c *schema.Column) Violation {
	v := Violation{Code: codeNotNullViolation, Table: tableName, Columns: []string{c.Name}}
	if !c.NotNull {
		v.Constraint = a.domainNotNullName(c.Type.OID)
	}
	return v
}

// domainNotNullName returns the key of the first NOT NULL domain in oid's domain chain,
// schema-qualified unless the domain lives in "public" (the same rule every other
// constraint name in this package follows: schema.Relation.FullName, Source.Table, ...).
// "" when oid is not a domain, or none of its layers is declared NOT NULL.
func (a *analyzer) domainNotNullName(oid catalog.OID) string {
	for {
		t := a.s.Types.ByOID(oid)
		if t == nil || t.Kind != 'd' {
			return ""
		}
		if d := a.s.Types.Domains[oid]; d != nil && d.NotNull {
			if t.Schema == "" || t.Schema == "public" {
				return t.Name
			}
			return t.Schema + "." + t.Name
		}
		oid = t.BaseType
	}
}

// referencingViolations lists foreign keys of other tables that point at rel and would
// reject the change: any on DELETE, those referencing a changed column on UPDATE.
// CASCADE / SET NULL / SET DEFAULT never fail on rel itself, but the cascaded change they
// make to the referencing table can itself fail: CASCADE deletes (or updates) the
// referencing rows, which is enumerated the same way one level down (recursively, since
// that table may in turn be referenced); SET NULL can hit the referencing column's NOT
// NULL; SET DEFAULT can hit the FK again (the default value need not exist in the parent)
// and the same NOT NULL if the default is absent.
func (a *analyzer) referencingViolations(rel *schema.Relation, changed map[string]bool, del bool) []Violation {
	return a.cascadingViolations(rel, changed, del, map[*schema.Relation]bool{})
}

func (a *analyzer) cascadingViolations(rel *schema.Relation, changed map[string]bool, del bool, visited map[*schema.Relation]bool) []Violation {
	if rel == nil || visited[rel] {
		return nil
	}
	visited[rel] = true
	var out []Violation
	for _, other := range a.s.Relations {
		for _, con := range other.Constraints {
			if con.Kind != schema.ForeignKey || con.NotEnforced || a.relByFullName(con.RefTable) != rel {
				continue
			}
			action := con.OnUpdate
			if del {
				action = con.OnDelete
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
			switch action {
			case 'a', 'r': // NO ACTION, RESTRICT: the change itself is rejected
				out = append(out, Violation{Code: codeForeignKeyViolation, Constraint: con.Name, Table: other.Name, Columns: con.Columns, RefTable: rel.Name})
			case 'c': // CASCADE: the referencing rows are deleted/updated the same way, cascading further
				out = append(out, a.cascadingViolations(other, colSet(con.Columns), del, visited)...)
			case 'n': // SET NULL: the FK columns are set to NULL
				for _, cn := range con.Columns {
					if c := other.Column(cn); c != nil && (c.NotNull || a.domainNotNull(c.Type.OID)) {
						out = append(out, a.notNullViolation(other.Name, c))
					}
				}
			case 'd': // SET DEFAULT: the FK columns take their DEFAULT, which the parent may not have
				out = append(out, Violation{Code: codeForeignKeyViolation, Constraint: con.Name, Table: other.Name, Columns: con.Columns, RefTable: rel.Name})
				for _, cn := range con.Columns {
					if c := other.Column(cn); c != nil && (c.NotNull || a.domainNotNull(c.Type.OID)) && c.Default == nil {
						out = append(out, a.notNullViolation(other.Name, c))
					}
				}
			}
		}
	}
	return out
}

// truncateNotes flags a TRUNCATE that can never succeed: PostgreSQL refuses to truncate a
// table still referenced, by an enforced foreign key, from a table that is neither named in
// the same TRUNCATE list nor reached by CASCADE (SQLSTATE 0A000). This is a purely
// structural, row-independent check -- like a NOT NULL column left out of an INSERT, it
// fails on every execution, so it is reported as a Note rather than a maybe-Violation.
func (a *analyzer) truncateNotes(tr *pgparse.TruncateStmt) {
	if tr.Behavior == pgparse.DropBehavior_DROP_CASCADE {
		return // CASCADE truncates every referencing table along with the named ones
	}
	targets := map[*schema.Relation]bool{}
	type target struct {
		rel *schema.Relation
		loc int32
	}
	var rels []target
	for _, rv := range tr.Relations {
		r := rv.GetRangeVar()
		rel := a.s.Relation(r.Schemaname, r.Relname)
		if rel == nil {
			continue
		}
		targets[rel] = true
		rels = append(rels, target{rel: rel, loc: r.Location})
	}
	for _, t := range rels {
		for _, other := range a.s.Relations {
			if targets[other] {
				continue
			}
			for _, con := range other.Constraints {
				if con.Kind == schema.ForeignKey && !con.NotEnforced && a.relByFullName(con.RefTable) == t.rel {
					a.note(noteAlwaysFails, t.loc, "TRUNCATE "+t.rel.Name+": every execution fails (SQLSTATE 0A000): "+
						other.Name+" references "+t.rel.Name+" and is not included in the same TRUNCATE (add it, or use TRUNCATE ... CASCADE)")
					return
				}
			}
		}
	}
}

func colSet(cols []string) map[string]bool {
	m := make(map[string]bool, len(cols))
	for _, c := range cols {
		m[c] = true
	}
	return m
}

// checkedViews walks the auto-updatable view chain starting at rel down toward its base
// table and returns the views whose WITH CHECK OPTION a write through rel must satisfy:
// rel's own check option (if any), plus every view further down once a CASCADED option
// forces checking to propagate (a LOCAL option only checks rel itself; a lower view still
// enforces its own CHECK OPTION independently when reached).
func (a *analyzer) checkedViews(rel *schema.Relation, cascading bool) []*schema.Relation {
	return a.checkedViewsFrom(rel, cascading, map[*schema.Relation]bool{})
}

func (a *analyzer) checkedViewsFrom(rel *schema.Relation, cascading bool, visited map[*schema.Relation]bool) []*schema.Relation {
	if rel == nil || rel.Kind != schema.View || rel.Query == nil || visited[rel] {
		return nil // visited: a view chain the loader accepted but PG would not (a -> b -> a)
	}
	visited[rel] = true
	var out []*schema.Relation
	if cascading || rel.CheckOption != 0 {
		out = append(out, rel)
	}
	nextCascading := cascading || rel.CheckOption == 'c'
	baseRV := autoUpdatableBase(a, rel.Query.GetSelectStmt())
	if baseRV == nil {
		return out
	}
	base := a.s.Relation(baseRV.Schemaname, baseRV.Relname)
	if base == nil || base.Kind != schema.View {
		return out
	}
	return append(out, a.checkedViewsFrom(base, nextCascading, visited)...)
}

// checkOptionViolations is the 44000 a write through rel (INSERT / UPDATE, including a
// MERGE action or an ON CONFLICT DO UPDATE) may hit: one entry per checked view. PG's own
// 44000 error carries no constraint name (unlike a table constraint violation), so -- like
// a trigger's custom SQLSTATE -- the code itself is the Constraint/Key expect lines name;
// Name carries the view for the checker's diagnostic text.
func (a *analyzer) checkOptionViolations(rel *schema.Relation) []Violation {
	var out []Violation
	for _, v := range a.checkedViews(rel, false) {
		out = append(out, Violation{Code: codeCheckOptionViolation, Constraint: codeCheckOptionViolation, Table: v.Name, Name: v.Name})
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

// predicateMatches reports whether a candidate unique/exclusion constraint's own predicate
// (nil for a non-partial one) is one the ON CONFLICT inference specification's WHERE clause
// satisfies as an arbiter: PostgreSQL only infers a partial index when its predicate is
// exactly the specification's WHERE clause (a non-partial index needs none).
func predicateMatches(conPredicate schema.Expr, whereClause *pgparse.Node) bool {
	if conPredicate == nil {
		return true
	}
	return whereClause != nil && equalIgnoringLocation(conPredicate, whereClause)
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
