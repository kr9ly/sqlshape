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
	codeNullToNotNull  = 1263 // "NULL supplied to NOT NULL column": a LOAD DATA field (SQLSTATE 22004)
	codeCheckViolation = 3819
	code1442           = 1442 // a trigger writing its own table: always fails (measured)
	code1172           = 1172 // SELECT ... INTO with more than one row
	codeNoDefault      = 1364 // "Field '...' doesn't have a default value"
	codeViewCheck      = 1369 // ER_VIEW_CHECK_FAILED: a write through a WITH CHECK OPTION view
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
				out = a.updateViolations(w.table, w.values, nil, a.strictFor(w.table))
				out = append(out, a.checkOptionViolations(w.table)...)
				for _, m := range w.more {
					out = append(out, a.updateViolations(m.table, m.values, nil, a.strictFor(m.table))...)
					out = append(out, a.checkOptionViolations(m.table)...)
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
		if br == nil {
			// the callee's own body is certain to fail CREATE (a malformed SQLSTATE, a
			// FUNCTION's own direct recursion, ...): that certain failure is this call
			// site's own "may" (measured: TestAdv3ChainCallCascadeThroughAnotherTriggerSwallowed
			// and TestAdv3ChainCallRecursionAnd1442BothSwallowed reach the same swallow here
			// as triggerViolations' own, below), not nothing at all.
			if v, ok := errorAsViolation(err); ok {
				v.Function = cr.r.Name
				out = append(out, v)
			}
			continue
		}
		for _, v := range br.Violations {
			v.Function = cr.r.Name
			out = append(out, v)
		}
	}
	return out
}

// errorAsViolation folds a chained trigger's or called routine's own certain CREATE-time
// failure (an *Error, from cachedAnalyzeTrigger/AnalyzeRoutine) into a Violation the firing
// statement itself may raise, instead of discarding it outright (measured:
// TestAdv3ChainCallCascadeThroughAnotherTriggerSwallowed -- AnalyzeTrigger(ca_a_bi), asked
// directly, already computes the correct 1442 that firing `INSERT INTO ca_a` always
// reaches, but triggerViolations/calledRoutineViolations threw it away). Not every analysis
// failure is an *Error (a construct the analyzer does not understand yet is a plain error,
// never seen here in practice): ok is false for those, unchanged from before.
func errorAsViolation(err error) (Violation, bool) {
	ae, ok := err.(*Error)
	if !ok {
		return Violation{}, false
	}
	return Violation{Code: ae.Code, Constraint: itoa(ae.Code), SQLState: constraintSQLState(ae.Code)}, true
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
	// inUse seeds the table-reuse chain (triggerViolations) with the tables this very
	// statement already writes: a trigger reached further down the chain writing back into
	// one of them is 1442 too, the same certain way a trigger writing its own table
	// directly is (measured: a chain through a different table, back into one already in
	// use, always fails -- see triggerViolations).
	inUse := writeTableSet(w)
	var out []Violation
	switch w.kind {
	case facts.Insert:
		out = append(out, triggerViolations(a.s, w.table, "INSERT", inUse)...)
		if w.replace {
			out = append(out, triggerViolations(a.s, w.table, "DELETE", inUse)...)
		}
		if w.onDuplicate != nil {
			out = append(out, triggerViolations(a.s, w.table, "UPDATE", inUse)...)
		}
	case facts.Update:
		out = append(out, triggerViolations(a.s, w.table, "UPDATE", inUse)...)
		for _, m := range w.more {
			out = append(out, triggerViolations(a.s, m.table, "UPDATE", inUse)...)
		}
	case facts.Delete:
		out = append(out, triggerViolations(a.s, w.table, "DELETE", inUse)...)
		for _, m := range w.more {
			out = append(out, triggerViolations(a.s, m.table, "DELETE", inUse)...)
		}
	}
	return out
}

// writeTableSet is w's own tables (lower-cased, w.table plus every w.more target): what
// triggerFailureModes seeds triggerViolations' own "already in use" set with.
func writeTableSet(w *write) map[string]bool {
	set := map[string]bool{}
	if w.table != nil {
		set[strings.ToLower(w.table.Name)] = true
	}
	for _, m := range w.more {
		if m.table != nil {
			set[strings.ToLower(m.table.Name)] = true
		}
	}
	return set
}

// triggerViolations lists what t's triggers for event (INSERT/UPDATE/DELETE) may raise: each
// matching trigger's own body, analyzed once and cached (cachedAnalyzeTrigger), already
// resolved to its own failure modes (its SIGNALs, and what its own embedded writes may
// violate -- including their own triggers, recursively, a cycle broken by the cache's
// in-progress marker: see body.go).
//
// inUse is the set of tables (lower-cased) already in use by the statement that reaches
// this point: the firing statement's own write table(s) to begin with, grown by each
// trigger's own table and writes as the chain is walked further down. A trigger's own body
// writing a table already in this set is 1442 the same way a trigger writing its OWN table
// is (walkDML's ownTableWrite, body.go) -- not only when it is the very same table, but
// anywhere up the chain that reached it (measured: `INSERT INTO x` fires x_bi, which
// INSERTs into y, which fires y_bi, which INSERTs into x -- x is already in use by the very
// first INSERT, and mysqld always raises 1442 there, though neither trigger's own table is
// the one its own body writes). Each of a trigger's own BodyStatements already carries its
// write's table and DML kind (facts.Write), which is what this walks to find the next
// table/event to chain into, and what it compares against inUse.
func triggerViolations(s *schema.Schema, t *schema.Table, event string, inUse map[string]bool) []Violation {
	if t == nil {
		// defensive: every caller passes w.table or a moreTarget's table, both resolved
		// (non-nil) *schema.Table values by the time a write is recorded.
		return nil
	}
	var out []Violation
	for _, tg := range s.Triggers {
		if !strings.EqualFold(tg.Table, t.Name) || !strings.EqualFold(tg.Event, event) {
			continue
		}
		br, err := cachedAnalyzeTrigger(s, tg)
		if br == nil {
			// tg's own body is certain to fail once fired here (a CALL cascading through
			// another trigger back into a table already in use, a CALL that both recurses
			// and would otherwise collide, ...): fold that failure into this firing
			// statement's own Violations instead of discarding it (measured:
			// TestAdv3ChainCallCascadeThroughAnotherTriggerSwallowed,
			// TestAdv3ChainCallRecursionAnd1442BothSwallowed -- AnalyzeTrigger(tg), asked
			// directly, already computes the right answer; this walk threw it away instead
			// of reaching for the firing statement's own prediction).
			if v, ok := errorAsViolation(err); ok {
				v.Table, v.Trigger = tg.Table, tg.Name
				out = append(out, v)
			}
			continue
		}
		out = append(out, br.Violations...)
		chained := addTable(inUse, tg.Table)
		for _, st := range br.Statements {
			for _, w := range st.Facts.Writes {
				lw := strings.ToLower(w.Table)
				if chained[lw] {
					out = append(out, Violation{
						Code: code1442, Constraint: itoa(code1442),
						Table: tg.Table, Trigger: tg.Name, SQLState: constraintSQLState(code1442),
					})
					continue
				}
				out = append(out, triggerViolations(s, s.Table(w.Table), w.Kind.String(), addTable(chained, w.Table))...)
			}
			// a locking read (SELECT ... FOR UPDATE / FOR SHARE / LOCK IN SHARE MODE) of a
			// table already in the chain is 1442 too, the same certain way a further write
			// back into it is (measured: TestAdv3ChainSelectForUpdateInsideTriggerIs1442 --
			// a plain SELECT or a subquery read does not collide, only a locking one; see
			// walkSelect's own markLastLockedReads, body.go, for how st.LockedReads is
			// populated).
			for _, lr := range st.LockedReads {
				if chained[strings.ToLower(lr)] {
					out = append(out, Violation{
						Code: code1442, Constraint: itoa(code1442),
						Table: tg.Table, Trigger: tg.Name, SQLState: constraintSQLState(code1442),
					})
				}
			}
		}
	}
	return dedupe(out)
}

// addTable copies set with name added (case-insensitively), leaving set itself untouched: a
// sibling branch of the trigger chain must not see another branch's own growth.
func addTable(set map[string]bool, name string) map[string]bool {
	out := make(map[string]bool, len(set)+1)
	for k := range set {
		out[k] = true
	}
	out[strings.ToLower(name)] = true
	return out
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
	case code1442, codeNoDefault:
		return "HY000"
	case code1172:
		return "42000"
	case codeNullToNotNull:
		return "22004"
	}
	return "HY000"
}

// strictFor reports whether a write into t is judged under strict mode: STRICT_ALL_TABLES
// is unconditional, but STRICT_TRANS_TABLES (without STRICT_ALL_TABLES) is strict only for
// a transactional storage engine -- on a nontransactional one (MyISAM et al.) a later row
// of a multi-row statement instead gets the column's implicit default with a warning, the
// same as with no strict mode at all (measured: TestAdv2StrictTransTablesIgnoresEngine).
func (a *analyzer) strictFor(t *schema.Table) bool {
	mode := a.s.Settings.SQLMode
	if mode.StrictAll() {
		return true
	}
	if mode.StrictTransOnly() {
		return transactionalEngine(t.Engine)
	}
	return false
}

// transactionalEngine reports whether a storage engine is transactional. An unspecified
// ENGINE (schema.Table.Engine == "") defaults to InnoDB, the server's own default, which is
// transactional.
func transactionalEngine(engine string) bool {
	switch strings.ToUpper(engine) {
	case "", "INNODB", "NDB", "NDBCLUSTER":
		return true
	default:
		return false
	}
}

func (a *analyzer) insertViolations(w *write) []Violation {
	t := w.table
	var out []Violation
	for _, k := range t.Keys {
		if k.Kind != schema.Primary && k.Kind != schema.Unique || w.onDuplicate != nil || w.replace {
			continue // ON DUPLICATE KEY UPDATE and REPLACE absorb a colliding key
		}
		cols, ok := violableKeyColumns(k)
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
		cols := exprColumns(c.Expr)
		if c.Enforced && anyIn(expandGeneratedColumns(t, cols), w.inserted) {
			out = append(out, Violation{Code: codeCheckViolation, Constraint: t.CheckName(c), Table: t.Name, Columns: cols})
		}
	}
	// a NULL for a NOT NULL column: an error in strict mode, and for a single-row INSERT
	// (its ON DUPLICATE KEY UPDATE included) in any mode
	notNull := a.strictFor(t) || w.rows == 1
	if notNull {
		out = append(out, notNullViolations(t, w.values)...)
		if !w.load {
			out = append(out, omittedNotNullViolations(t, w.inserted)...)
		}
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

// leftNull: at least one column of the key is left to default to NULL (not inserted,
// nullable, no DEFAULT, not AUTO_INCREMENT). MySQL's multi-column UNIQUE key semantics take
// the whole tuple out of duplicate checking when ANY of its columns holds NULL, not only
// when every column does (measured: TestAdv2CompositeUniqueKeyLeftPartiallyNull).
func leftNull(t *schema.Table, cols []string, inserted map[string]bool) bool {
	for _, name := range cols {
		c := t.Column(name)
		if c == nil {
			continue
		}
		if !inserted[name] && !c.NotNull && c.Default == nil && !c.AutoIncrement {
			return true
		}
	}
	return false
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
		if cols, ok := violableKeyColumns(k); ok && anyIn(cols, set) {
			out = append(out, Violation{Code: codeDuplicateKey, Constraint: k.Name, Table: t.Name, Columns: cols})
		}
	}
	for _, fk := range t.ForeignKeys {
		if anyIn(fk.Columns, set) {
			out = append(out, Violation{Code: codeForeignKeyRow, Constraint: t.ForeignKeyName(fk), Table: t.Name, Columns: fk.Columns, RefTable: fk.RefTable})
		}
	}
	for _, c := range t.Checks {
		cols := exprColumns(c.Expr)
		if c.Enforced && anyIn(expandGeneratedColumns(t, cols), set) {
			out = append(out, Violation{Code: codeCheckViolation, Constraint: t.CheckName(c), Table: t.Name, Columns: cols})
		}
	}
	if notNull {
		out = append(out, notNullViolations(t, values)...)
	}
	out = append(out, a.referencingViolations(t, set, false, map[*schema.Table]bool{})...)
	return dedupe(out)
}

// checkOptionViolations is the 1369 (ER_VIEW_CHECK_FAILED) an UPDATE reaching t through a
// WITH CHECK OPTION view may hit -- one entry per view the statement writes through
// directly (facts.go's checkOptionFacts records it there, alongside the pinned lift
// through the same view chain). MySQL's own message always names the view written
// through, whichever level's WHERE actually failed (measured: a CASCADED chain through an
// underlying view with no CHECK OPTION of its own still names the outer one), so unlike
// PostgreSQL's checkedViews there is nothing to walk here -- the view named in the
// statement is the only name that can appear.
func (a *analyzer) checkOptionViolations(t *schema.Table) []Violation {
	var out []Violation
	for _, name := range a.checkOptionViews[t] {
		out = append(out, Violation{Code: codeViewCheck, Constraint: name, Table: name, Name: name})
	}
	return out
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
		code := codeNotNull
		if as.fromFile {
			code = codeNullToNotNull
		}
		out = append(out, Violation{Code: code, Table: t.Name, Columns: []string{as.col.Name}, Param: as.param})
	}
	return out
}

// omittedNotNullViolations: a NOT NULL column with no DEFAULT, not AUTO_INCREMENT and not
// GENERATED, left entirely out of the INSERT's column list. mysqld raises 1364 "Field
// '...' doesn't have a default value" for this in strict mode (measured:
// TestAdv2OmittedNotNullColumnNoDefault) -- notNullViolations never sees it since it only
// walks the columns the statement actually assigns.
func omittedNotNullViolations(t *schema.Table, inserted map[string]bool) []Violation {
	var out []Violation
	for _, c := range t.Columns {
		if inserted[c.Name] || !c.NotNull || c.Default != nil || c.AutoIncrement || c.Generated != nil {
			continue
		}
		out = append(out, Violation{Code: codeNoDefault, Table: t.Name, Columns: []string{c.Name}})
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

// violableKeyColumns lists the columns whose values a key constrains, prefix parts
// (`UNIQUE (c(10))`, judged on the first characters) and expression parts (`UNIQUE
// ((col + 1))`, judged on the columns the expression reads) included: a write that assigns
// any of them may collide on the key (1062), unlike keyColumns' reading, where such a
// part is not something an equality on the column fixes. Found by the corpus probe
// (ctype_utf8's prefix keys), pinned by TestViolationsServer.
func violableKeyColumns(k *schema.Key) ([]string, bool) {
	cols := make([]string, 0, len(k.Parts))
	seen := map[string]bool{}
	add := func(c string) {
		if c != "" && !seen[strings.ToLower(c)] {
			seen[strings.ToLower(c)] = true
			cols = append(cols, c)
		}
	}
	for _, p := range k.Parts {
		if p.Expr != nil {
			for _, c := range exprColumns(p.Expr) {
				add(c)
			}
			continue
		}
		add(p.Column)
	}
	return cols, len(cols) > 0
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

// expandGeneratedColumns: cols is a CHECK's own column set (exprColumns of its expression);
// for each one that is itself a generated column, the source columns it is computed from
// are added too, since a generated column is never itself assigned -- a write to the
// source column recomputes it and re-checks the CHECK on every write that can change it
// (measured: TestAdv2GeneratedColumnCheckViaSourceColumn). Recurses through chains of
// generated columns; a cycle cannot occur (the schema loader rejects a generated column
// whose expression refers to itself or a later column).
func expandGeneratedColumns(t *schema.Table, cols []string) []string {
	seen := map[string]bool{}
	var out []string
	var add func(name string)
	add = func(name string) {
		key := strings.ToLower(name)
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, name)
		if c := t.Column(name); c != nil && c.Generated != nil {
			for _, src := range exprColumns(c.Generated) {
				add(src)
			}
		}
	}
	for _, name := range cols {
		add(name)
	}
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
