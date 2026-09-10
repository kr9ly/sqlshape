// Package dialect registers MySQL with the checker: importing it (for its side effect,
// as cmd/sqlshape does) makes a schema.sql that declares `-- sqlshape: mysql 8.4` load
// through the MySQL schema loader and judge statements through the MySQL analyzer.
package dialect

import (
	"strconv"

	"github.com/kr9ly/sqlshape/check/mysql/v2/internal/analyze"
	"github.com/kr9ly/sqlshape/check/mysql/v2/internal/schema"
	"github.com/kr9ly/sqlshape/v2/x/dialect"
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

type mysql struct{ s *schema.Schema }

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
		if ae, ok := err.(*analyze.Error); ok {
			return nil, &dialect.Error{Message: ae.Message, Code: "MySQL error " + strconv.Itoa(ae.Code), Position: ae.Position}
		}
		return nil, err
	}
	out := &dialect.Result{Facts: r.Facts}
	for _, p := range r.Params {
		out.Params = append(out.Params, dialect.Param{Type: typeOf(p.Type, p.Known)})
	}
	for _, c := range r.Columns {
		out.Columns = append(out.Columns, dialect.Column{Name: c.Name, Type: typeOf(c.Type, c.Known), Nullable: c.Nullable})
	}
	return out, nil
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
