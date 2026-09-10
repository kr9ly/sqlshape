// Package dialect registers MySQL with the checker: importing it (for its side effect,
// as cmd/sqlshape does) makes a schema.sql that declares `-- sqlshape: mysql 8.4` load
// through the MySQL schema loader and judge statements through the MySQL analyzer.
package dialect

import (
	"strconv"
	"strings"

	"github.com/kr9ly/sqlshape/check/mysql/v2/internal/analyze"
	"github.com/kr9ly/sqlshape/check/mysql/v2/internal/schema"
	"github.com/kr9ly/sqlshape/v2/x/dialect"
	"github.com/kr9ly/sqlshape/v2/x/facts"
)

// Name is the dialect's name in the schema declaration.
const Name = "mysql"

func init() {
	dialect.Register(Name, load)
}

func load(schemaSQL string) (dialect.Analyzer, error) {
	s, err := schema.Load(schemaSQL)
	if err != nil {
		return nil, err
	}
	return &mysql{s: s}, nil
}

type mysql struct {
	s *schema.Schema
	// views are the view queries analyzed once (schema.go), viewErrs the definitions that
	// did not analyze
	views    map[*schema.View]*analyze.ViewResult
	viewErrs []dialect.Definition
}

func (m *mysql) Problems() []string {
	var out []string
	for _, p := range m.s.Problems {
		out = append(out, p.String())
	}
	return out
}

func (m *mysql) Analyze(sql string) (*dialect.Result, error) {
	r, err := analyze.Analyze(m.s, sql)
	if err != nil {
		return nil, errorOf(err)
	}
	out := &dialect.Result{Facts: r.Facts, Uses: r.Uses}
	for _, p := range r.Params {
		out.Params = append(out.Params, dialect.Param{Type: typeOf(p.Type, p.Known), Source: m.paramSource(p.Source)})
	}
	for _, c := range r.Columns {
		out.Columns = append(out.Columns, dialect.Column{Name: c.Name, Type: typeOf(c.Type, c.Known), Nullable: c.Nullable})
	}
	for _, v := range r.Violations {
		out.Violations = append(out.Violations, dialect.Violation{Key: v.Key(), Code: "MySQL error " + strconv.Itoa(v.Code), Table: v.Table,
			Columns: v.Columns, Constraint: v.Constraint, Detail: describeViolation(v), Param: v.Param})
	}
	if r.Facts != nil {
		out.Relations = relationRefs(r.Facts)
	}
	return out, nil
}

// paramSource is the column a placeholder met, as the schema describes it, with whether
// the statement stores into it or compares with it.
func (m *mysql) paramSource(ps *analyze.ParamSource) *dialect.Source {
	if ps == nil {
		return nil
	}
	src := m.Source(ps.Table, ps.Column)
	if src == nil {
		return nil
	}
	src.Assigned = ps.Assigned
	return src
}

// errorOf spells an analyzer error in the contract; other errors pass through.
func errorOf(err error) error {
	if ae, ok := err.(*analyze.Error); ok {
		return &dialect.Error{Message: ae.Message, Code: "MySQL error " + strconv.Itoa(ae.Code), Position: ae.Position}
	}
	return err
}

// describeViolation spells a possible violation the way the checker reports it.
func describeViolation(v analyze.Violation) string {
	cols := strings.Join(v.Columns, ", ")
	code := ", MySQL error " + strconv.Itoa(v.Code)
	switch v.Code {
	case 1062:
		if v.Constraint == "PRIMARY" {
			return "PRIMARY KEY (" + cols + ") on " + v.Table + code
		}
		return "UNIQUE " + v.Constraint + " (" + cols + ") on " + v.Table + code
	case 1452:
		return "FOREIGN KEY " + v.Constraint + " (" + cols + ") on " + v.Table + " REFERENCES " + v.RefTable + code
	case 1451:
		return "FOREIGN KEY " + v.Constraint + " (" + cols + ") on " + v.Table + " REFERENCES " + v.RefTable + ": a row of " + v.Table + " still refers to the one changed" + code
	case 1048:
		return "NOT NULL on " + v.Table + "." + cols + code
	case 3819:
		return "CHECK " + v.Constraint + " on " + v.Table + " (" + cols + ")" + code
	}
	return v.Key() + code
}

// relationRefs lists the tables and views the statement's facts reference, at every depth,
// each once: what the boundary checks (-schemas, -no-tables) and the consumers index read.
func relationRefs(f *facts.Facts) []dialect.RelationRef {
	var out []dialect.RelationRef
	seen := map[string]int{}
	var walk func(sc *facts.Scope)
	walk = func(sc *facts.Scope) {
		if sc == nil {
			return
		}
		for _, l := range sc.Leaves {
			if l.Table == "" || (l.Kind != facts.Table && l.Kind != facts.View) {
				continue
			}
			if i, ok := seen[l.Table]; ok {
				out[i].Target = out[i].Target || l.Role == facts.Target
				continue
			}
			seen[l.Table] = len(out)
			out = append(out, dialect.RelationRef{Name: l.Table, Kind: l.Kind, Position: int(l.Position), Target: l.Role == facts.Target})
		}
		for _, p := range sc.Preds {
			if p.Op == facts.Exists {
				walk(p.Sub)
			}
		}
		for _, ch := range sc.Children {
			walk(ch)
		}
	}
	walk(f.Top)
	return out
}

func typeOf(t schema.Type, known bool) dialect.Type {
	if !known {
		return dialect.Type{Name: "an expression the analyzer does not type yet"}
	}
	dt := dialect.Type{Name: t.String()}
	for _, g := range GoTypes(t) {
		dt.Result = append(dt.Result, dialect.GoFit{Go: g})
		dt.Param = append(dt.Param, dialect.GoFit{Go: g})
	}
	return dt
}

// Traits: go-sql-driver/mysql through database/sql. A Go string does not stand in for a
// number or a date (the server would coerce it, but the checker holds the type), and there
// is no family of Valid-carrying value types beyond sql.Null*.
func (m *mysql) Traits() dialect.Traits { return dialect.Traits{} }

// GoTypes is the Go side of a MySQL type: what go-sql-driver/mysql scans a column of the
// type into and encodes a parameter from, through database/sql. Integers arrive as int64
// (uint64 for bigint unsigned), DECIMAL as its decimal text, temporal types as time.Time
// (parseTime=true) or their text, binary strings and JSON as bytes. Nil for a type with no
// mapping; the checker then accepts the field with a note.
func GoTypes(t schema.Type) []string {
	switch t.Name {
	case "tinyint", "smallint", "mediumint", "int", "year":
		gos := []string{"int64", "int32", "int"}
		if t.Unsigned {
			gos = append(gos, "uint64", "uint32", "uint")
		}
		if t.Name == "tinyint" && t.Length == 1 {
			gos = append(gos, "bool")
		}
		return gos
	case "bigint":
		if t.Unsigned {
			return []string{"uint64", "int64"}
		}
		if t.Length == 1 { // a comparison or a logical operator: MySQL's bigint(1)
			return []string{"int64", "int", "bool"}
		}
		return []string{"int64", "int"}
	case "decimal":
		return []string{"string"}
	case "float":
		return []string{"float32", "float64"}
	case "double":
		return []string{"float64"}
	case "bit":
		return []string{"[]byte"}
	case "char", "varchar", "tinytext", "text", "mediumtext", "longtext", "enum", "set":
		return []string{"string", "[]byte"}
	case "binary", "varbinary", "tinyblob", "blob", "mediumblob", "longblob":
		return []string{"[]byte"}
	case "json":
		return []string{"[]byte", "string"}
	case "date", "datetime", "timestamp":
		return []string{"time.Time", "string"}
	case "time":
		return []string{"string"}
	}
	return nil
}
