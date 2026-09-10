package schema

import (
	"fmt"
	"strings"

	"github.com/kr9ly/sqlshape/check/postgres/pgparse"
)

// Seed is the fixed content of a table: the rows the schema text itself gives it with
// ordinary INSERT statements. A table with a Seed is a lookup table whose rows are part
// of the schema (a value set the checker reads like enum labels, and data the migration
// keeps in step with the declaration); a table without one holds runtime data.
//
// The INSERT must be idempotent, so that applying the schema text twice means the same
// thing: rows are identified by a key (the primary key, or a non-partial unique
// constraint over NOT NULL columns) every row gives as constants, values are constant
// expressions (no volatile or stable functions, no subqueries, no DEFAULT), there is no
// ON CONFLICT clause, and no key appears twice.
type Seed struct {
	// Columns are the columns the INSERTs name (every table column when they name none),
	// in the first INSERT's order. Columns left out take their DEFAULT and are not part of
	// the declared content: the migration neither writes nor compares them.
	Columns []string
	// Key are the columns that identify a row, a subset of Columns.
	Key []string
	// Rows are the value expressions, one slice per row, aligned with Columns.
	Rows [][]Expr
	// Additive: `-- sqlshape: seed` before the INSERT. The declaration owns the rows it
	// lists but not the table: rows found in the database that it does not list stay.
	Additive bool
}

// CheckStatement type-checks a data statement of the schema text against the schema as
// it stands (the analyzer installs it); nil leaves the INSERTs unchecked.
var CheckStatement func(s *Schema, stmt *pgparse.Node) error

// KeyText renders the key of a row for identity comparison.
func (sd *Seed) KeyText(row []Expr) string {
	parts := make([]string, 0, len(sd.Key))
	for _, k := range sd.Key {
		parts = append(parts, Deparse(row[sd.index(k)]))
	}
	return strings.Join(parts, ", ")
}

// RowText renders a whole row for comparison.
func (sd *Seed) RowText(row []Expr) string {
	parts := make([]string, len(row))
	for i, v := range row {
		parts[i] = Deparse(v)
	}
	return strings.Join(parts, ", ")
}

// ByKey indexes the rows by KeyText.
func (sd *Seed) ByKey() map[string][]Expr {
	out := make(map[string][]Expr, len(sd.Rows))
	for _, r := range sd.Rows {
		out[sd.KeyText(r)] = r
	}
	return out
}

func (sd *Seed) index(col string) int {
	for i, c := range sd.Columns {
		if c == col {
			return i
		}
	}
	return -1
}

// insert records the rows of an INSERT ... VALUES in the target table's Seed.
func (s *Schema) insert(st *pgparse.InsertStmt, node *pgparse.Node, loc int32) {
	rel := s.findRelation(s.rangeVar(st.Relation))
	if rel == nil {
		sch, name := s.rangeVar(st.Relation)
		s.problem(loc, "INSERT: relation %s.%s does not exist", sch, name)
		return
	}
	if rel.Kind != Table {
		s.problem(loc, "INSERT into %s: only tables carry seed rows", rel.FullName())
		return
	}
	if st.OnConflictClause != nil {
		s.problem(loc, "INSERT into %s: ON CONFLICT is not needed in schema.sql (the rows are identified by their key and merged)", rel.FullName())
		return
	}
	sel := st.SelectStmt.GetSelectStmt()
	if sel == nil || len(sel.ValuesLists) == 0 || sel.WithClause != nil {
		s.problem(loc, "INSERT into %s: schema.sql rows must be a VALUES list", rel.FullName())
		return
	}
	additive := false
	s.pendingUsed = true
	for _, d := range s.pending {
		if strings.Join(strings.Fields(d), " ") == "seed" {
			additive = true
		} else {
			s.problem(loc, "INSERT into %s: unknown directive %q", rel.FullName(), d)
		}
	}
	// columns
	var cols []string
	if len(st.Cols) == 0 {
		for _, c := range rel.Columns {
			cols = append(cols, c.Name)
		}
	} else {
		for _, c := range st.Cols {
			rt := c.GetResTarget()
			if rt == nil || len(rt.Indirection) > 0 {
				s.problem(loc, "INSERT into %s: column list must name whole columns", rel.FullName())
				return
			}
			if rel.Column(rt.Name) == nil {
				s.problem(loc, "INSERT into %s: column %q does not exist", rel.FullName(), rt.Name)
				return
			}
			cols = append(cols, rt.Name)
		}
	}
	// values
	var rows [][]Expr
	for _, vl := range sel.ValuesLists {
		items := vl.GetList().GetItems()
		if len(items) != len(cols) {
			s.problem(loc, "INSERT into %s: a row has %d values for %d columns", rel.FullName(), len(items), len(cols))
			return
		}
		for i, v := range items {
			if why := s.notConstant(v); why != "" {
				s.problem(loc, "INSERT into %s: value for %s is not a constant expression (%s); schema.sql rows must mean the same each time they are applied", rel.FullName(), cols[i], why)
				return
			}
		}
		rows = append(rows, items)
	}
	if CheckStatement != nil {
		if err := CheckStatement(s, node); err != nil {
			s.problem(loc, "INSERT into %s: %v", rel.FullName(), err)
			return
		}
	}
	// the key
	given := map[string]bool{}
	for _, c := range cols {
		given[c] = true
	}
	key := seedKey(rel, given)
	if key == nil {
		s.problem(loc, "INSERT into %s: no key identifies the rows; give every column of the primary key, or of a unique constraint over NOT NULL columns, a constant", rel.FullName())
		return
	}
	sd := rel.Seed
	if sd == nil {
		sd = &Seed{Columns: cols, Key: key, Additive: additive}
		rel.Seed = sd
	} else {
		sd.Additive = sd.Additive || additive
		if !sameSet(sd.Columns, cols) {
			s.problem(loc, "INSERT into %s: names columns (%s) unlike the earlier INSERT (%s); every INSERT into a table must name the same columns", rel.FullName(), strings.Join(cols, ", "), strings.Join(sd.Columns, ", "))
			return
		}
		// align with the first INSERT's order
		for i, r := range rows {
			aligned := make([]Expr, len(cols))
			for j, c := range cols {
				aligned[sd.index(c)] = r[j]
			}
			rows[i] = aligned
		}
	}
	seen := sd.ByKey()
	for _, r := range rows {
		k := sd.KeyText(r)
		if seen[k] != nil {
			s.problem(loc, "INSERT into %s: key (%s) is given twice", rel.FullName(), k)
			return
		}
		seen[k] = r
		sd.Rows = append(sd.Rows, r)
	}
}

// seedKey picks the key that identifies the rows: the primary key if every column of
// it is given, else the first non-partial unique constraint over given NOT NULL columns.
func seedKey(rel *Relation, given map[string]bool) []string {
	covered := func(c *Constraint) bool {
		if len(c.Columns) == 0 || c.Predicate != nil {
			return false
		}
		for _, col := range c.Columns {
			if !given[col] {
				return false
			}
			if c.Kind == Unique {
				if cd := rel.Column(col); cd == nil || !cd.NotNull {
					return false
				}
			}
		}
		return true
	}
	for _, c := range rel.Constraints {
		if c.Kind == PrimaryKey && covered(c) {
			return c.Columns
		}
	}
	for _, c := range rel.Constraints {
		if c.Kind == Unique && covered(c) {
			return c.Columns
		}
	}
	return nil
}

// notConstant says why an expression is not a constant: it reads something (a column, a
// parameter, a subquery), or calls a function that is not IMMUTABLE. Empty when it is.
func (s *Schema) notConstant(e Expr) string {
	why := ""
	WalkNodes(e, func(n *pgparse.Node) {
		if why != "" {
			return
		}
		switch x := n.Node.(type) {
		case *pgparse.Node_ColumnRef:
			why = "it references a column"
		case *pgparse.Node_ParamRef:
			why = "it references a parameter"
		case *pgparse.Node_SubLink:
			why = "it contains a subquery"
		case *pgparse.Node_SetToDefault:
			why = "DEFAULT: leave the column out of the INSERT instead"
		case *pgparse.Node_SqlvalueFunction:
			why = "it reads the session (current_date and friends)"
		case *pgparse.Node_FuncCall:
			if v := s.volatility(strs(x.FuncCall.Funcname)); v != 'i' {
				why = fmt.Sprintf("%s() is not IMMUTABLE", strs(x.FuncCall.Funcname)[len(x.FuncCall.Funcname)-1])
			}
		}
	})
	return why
}

// volatility is the least stable volatility among the functions a name can resolve to
// ('i' only when every candidate is IMMUTABLE).
func (s *Schema) volatility(names []string) byte {
	sch, name := qualified(names)
	out := byte(0)
	worse := func(v byte) {
		switch {
		case v == 'v' || out == 'v':
			out = 'v'
		case v == 's' || out == 's':
			out = 's'
		default:
			out = 'i'
		}
	}
	if sch == "" || sch == "pg_catalog" {
		for _, f := range s.Catalog.FuncsByName(name) {
			worse(f.Volatile)
		}
	}
	for _, f := range s.Functions {
		if f.Name == name && (sch == "" || f.Schema == sch) {
			worse(f.Volatile)
		}
	}
	if out == 0 {
		return 'v' // unknown: not provably constant
	}
	return out
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	m := map[string]bool{}
	for _, x := range a {
		m[x] = true
	}
	for _, x := range b {
		if !m[x] {
			return false
		}
	}
	return true
}
