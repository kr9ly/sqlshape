// Package stmtprobe checks a dialect's analyzer against a running server on the claims a
// handwritten test cannot enumerate: it generates a small schema with rows and statements
// over it, asks the analyzer for each statement's verdict, runs the statement, and refutes
// the verdict with what the server did. Two probes share the generator: the facts probe
// (Run) refutes every claim of the statement facts (x/facts) -- a predicate that every row
// was said to satisfy, a column said to be fixed, a level said to be at most one row, the
// value a write stores -- with the rows the server returned; the failure probe
// (RunFailures) refutes the predicted failure modes -- every constraint error the server
// raises must be among the violations predicted for the statement, under the key the
// runtime's Violates would match. It is the oracle behind the One proof, the obligations
// and the expect lines, written once for every dialect; check/postgres and check/mysql
// drive it from their tests. Like pgtest and mysqltest it is sqlshape's own test tooling,
// with no compatibility promise.
package stmtprobe

import (
	"fmt"
	"sort"
	"strings"
)

// Value is one cell as the probe sees it: NULL, or a value spelled as text (an integer
// as its decimal digits, a string as its content), which is how both the model and the
// server's answers compare.
type Value struct {
	Null bool
	S    string
}

func intValue(i int) Value    { return Value{S: fmt.Sprint(i)} }
func strValue(s string) Value { return Value{S: s} }

var null = Value{Null: true}

func (v Value) String() string {
	if v.Null {
		return "NULL"
	}
	return v.S
}

// ColKind is a column's type family.
type ColKind byte

const (
	Int ColKind = iota + 1
	Str
)

// Col is one column of a model table.
type Col struct {
	Name    string
	Kind    ColKind
	NotNull bool
}

// Table is one table of the model, with its rows.
type Table struct {
	Name    string
	Cols    []Col
	PK      []string   // the primary key's columns, nil for none
	Uniques [][]string // UNIQUE keys over whole columns
	Rows    [][]Value  // parallel to Cols
	// view: the table is a view over view.base (CREATE VIEW); Rows are the base's rows
	// its conjuncts keep, PK and Uniques are nil
	view *derived
	// the failure probe's constraints (nil for the facts probe's tables)
	uniqueName string    // CONSTRAINT <name> UNIQUE for Uniques[0], "" for an unnamed one
	check      *checkDef // CHECK (col > 0)
	fk         *fkDef    // FOREIGN KEY (col) REFERENCES ref (id)
}

// checkDef is a CHECK (col > 0) constraint.
type checkDef struct {
	col  string
	name string // "" for the server's own name
}

// fkDef is a single-column foreign key to another table's id.
type fkDef struct {
	col      string
	ref      *Table
	name     string // "" for the server's own name
	onDelete string // "" (the default, RESTRICT / NO ACTION), "CASCADE" or "SET NULL"
}

func (t *Table) col(name string) int {
	for i, c := range t.Cols {
		if c.Name == name {
			return i
		}
	}
	return -1
}

// Schema is the model: tables in creation order.
type Schema struct {
	Tables []*Table
}

// view is the schema's view, nil without one.
func (s *Schema) view() *Table {
	for _, t := range s.Tables {
		if t.view != nil {
			return t
		}
	}
	return nil
}

func (s *Schema) table(name string) *Table {
	for _, t := range s.Tables {
		if t.Name == name {
			return t
		}
	}
	return nil
}

// Dialect is what the rendering needs to know about the target database.
type Dialect struct {
	// Header is the schema text's first line, the version declaration.
	Header string
	// IntType / StrType are the column types for the two kinds.
	IntType, StrType string
	// Positional: the server takes `?` placeholders in order (MySQL) rather than `$n`.
	// The analyzer always sees `$n`.
	Positional bool
}

// DDL renders the schema as the analyzer reads it: the header, then CREATE TABLE per
// table (rows are not part of it).
func (s *Schema) DDL(d Dialect) string {
	var b strings.Builder
	b.WriteString(d.Header)
	b.WriteString("\n")
	for _, t := range s.Tables {
		b.WriteString(t.create(d))
		b.WriteString("\n")
	}
	return b.String()
}

func (t *Table) create(d Dialect) string {
	if t.view != nil {
		return "CREATE VIEW " + t.Name + " AS " + t.view.sql() + ";"
	}
	var parts []string
	for _, c := range t.Cols {
		typ := d.IntType
		if c.Kind == Str {
			typ = d.StrType
		}
		p := c.Name + " " + typ
		if c.NotNull {
			p += " NOT NULL"
		}
		parts = append(parts, p)
	}
	if t.PK != nil {
		parts = append(parts, "PRIMARY KEY ("+strings.Join(t.PK, ", ")+")")
	}
	for i, u := range t.Uniques {
		if i == 0 && t.uniqueName != "" {
			parts = append(parts, "CONSTRAINT "+t.uniqueName+" UNIQUE ("+strings.Join(u, ", ")+")")
			continue
		}
		parts = append(parts, "UNIQUE ("+strings.Join(u, ", ")+")")
	}
	if c := t.check; c != nil {
		p := "CHECK (" + c.col + " > 0)"
		if c.name != "" {
			p = "CONSTRAINT " + c.name + " " + p
		}
		parts = append(parts, p)
	}
	if f := t.fk; f != nil {
		p := "FOREIGN KEY (" + f.col + ") REFERENCES " + f.ref.Name + " (id)"
		if f.name != "" {
			p = "CONSTRAINT " + f.name + " " + p
		}
		if f.onDelete != "" {
			p += " ON DELETE " + f.onDelete
		}
		parts = append(parts, p)
	}
	return "CREATE TABLE " + t.Name + " (" + strings.Join(parts, ", ") + ");"
}

// Inserts renders every table's rows as INSERT statements.
func (s *Schema) Inserts() []string {
	var out []string
	for _, t := range s.Tables {
		if t.view != nil {
			continue
		}
		for _, r := range t.Rows {
			vals := make([]string, len(r))
			for i, v := range r {
				vals[i] = literal(t.Cols[i].Kind, v)
			}
			out = append(out, "INSERT INTO "+t.Name+" VALUES ("+strings.Join(vals, ", ")+");")
		}
	}
	return out
}

// Drops renders the DROP TABLE statements, last table first.
func (s *Schema) Drops() []string {
	var out []string
	for i := len(s.Tables) - 1; i >= 0; i-- {
		if s.Tables[i].view != nil {
			out = append(out, "DROP VIEW IF EXISTS "+s.Tables[i].Name+";")
			continue
		}
		out = append(out, "DROP TABLE IF EXISTS "+s.Tables[i].Name+";")
	}
	return out
}

// literal spells a value of the given kind in SQL.
func literal(k ColKind, v Value) string {
	if v.Null {
		return "NULL"
	}
	if k == Str {
		return "'" + strings.ReplaceAll(v.S, "'", "''") + "'"
	}
	return v.S
}

// row is one produced row of a scope: the values of every leaf's columns, keyed by
// alias.column.
type row map[string]Value

func (r row) get(alias, col string) Value { return r[alias+"."+col] }

// keys lists r's keys in order, for a stable rendering.
func (r row) String() string {
	var ks []string
	for k := range r {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	var parts []string
	for _, k := range ks {
		parts = append(parts, k+"="+r[k].String())
	}
	return "{" + strings.Join(parts, " ") + "}"
}
