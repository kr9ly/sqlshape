package dialect

import (
	"strconv"

	"github.com/kr9ly/sqlshape/check/postgres/v2/analyze"
	"github.com/kr9ly/sqlshape/check/postgres/v2/catalog"
	"github.com/kr9ly/sqlshape/check/postgres/v2/pgparse"
	"github.com/kr9ly/sqlshape/check/postgres/v2/schema"
	"github.com/kr9ly/sqlshape/v2/x/dialect"
)

// ColumnOf spells an analyzed result column in the contract: PostgreSQL's unnamed
// column ("?column?") has no name, a void column has Kind Void, a record's fields are the
// analyzer's, a composite's the type's.
func ColumnOf(s *schema.Schema, c analyze.Column) dialect.Column {
	out := dialect.Column{Name: c.Name, Nullable: c.Nullable, Source: SourceOf(s, c.Source)}
	if out.Name == "?column?" {
		out.Name = ""
	}
	if c.Type.OID == catalog.Void {
		out.Type = dialect.Type{Name: "void", Kind: dialect.Void}
		return out
	}
	out.Type = TypeOf(s, c.Type)
	if len(c.Fields) > 0 {
		fields := make([]dialect.Column, 0, len(c.Fields))
		for _, f := range c.Fields {
			fields = append(fields, ColumnOf(s, f))
		}
		out.Type.Fields = fields
		if out.Type.Kind == dialect.Array && out.Type.Elem != nil {
			elem := *out.Type.Elem
			elem.Fields = fields
			out.Type.Elem = &elem
		}
	}
	return out
}

// ParamOf spells a parameter: its type and the column it stands for.
func ParamOf(s *schema.Schema, t schema.TypeRef, src *analyze.Source) dialect.Param {
	return dialect.Param{Type: TypeOf(s, t), Source: SourceOf(s, src)}
}

// SourceOf spells a column's provenance, with what the schema says about the column: the
// key it carries (Identity, following single-column foreign keys to their root primary
// key), the values it is limited to (a CHECK IN list, or the rows a lookup table is seeded
// with), its DEFAULT / generation, its COMMENT.
func SourceOf(s *schema.Schema, src *analyze.Source) *dialect.Source {
	if src == nil {
		return nil
	}
	out := &dialect.Source{Table: src.Table, Column: src.Column, NotNull: src.NotNull, Assigned: src.Assigned}
	out.Comment = s.Comments[src.Table+"."+src.Column]
	if rel := s.ByFullName(src.Table); rel != nil {
		if col := rel.Column(src.Column); col != nil {
			out.HasDefault = col.Default != nil
			out.Generated = col.Identity != 0 || col.Generated != nil
			if len(col.Values) > 0 {
				out.Values, out.ValuesFrom = col.Values, "check"
			}
		}
	}
	if id, ok := identity(s, src.Table, src.Column, 0); ok {
		out.Identity = id
		if out.Values == nil {
			if labels, ok := lookupLabels(s, id); ok {
				out.Values, out.ValuesFrom = labels, "seed"
			}
		}
	}
	return out
}

// identity follows single-column PK / FK structure to the root key column.
func identity(s *schema.Schema, table, column string, depth int) (string, bool) {
	if depth > 10 {
		return "", false
	}
	rel := s.ByFullName(table)
	if rel == nil {
		return "", false
	}
	for _, con := range rel.Constraints {
		if con.Kind != schema.ForeignKey {
			continue
		}
		pos := -1
		for i, col := range con.Columns {
			if col == column {
				pos = i
			}
		}
		if pos < 0 {
			continue
		}
		// a composite FK maps its columns to the referenced key by position
		refCols := con.RefColumns
		if len(refCols) == 0 {
			if ref := s.ByFullName(con.RefTable); ref != nil {
				for _, rc := range ref.Constraints {
					if rc.Kind == schema.PrimaryKey {
						refCols = rc.Columns
					}
				}
			}
		}
		if pos < len(refCols) {
			if id, ok := identity(s, con.RefTable, refCols[pos], depth+1); ok {
				return id, true
			}
		}
	}
	for _, con := range rel.Constraints {
		if con.Kind != schema.PrimaryKey {
			continue
		}
		for _, col := range con.Columns {
			if col == column {
				// each column of a (possibly composite) primary key is an identity of its own
				return table + "." + column, true
			}
		}
	}
	return "", false
}

// lookupLabels returns the values of the key column "table.column" when the table is
// seeded with rows identified by that column alone.
func lookupLabels(s *schema.Schema, id string) ([]string, bool) {
	dot := lastDot(id)
	if dot < 0 {
		return nil, false
	}
	rel := s.ByFullName(id[:dot])
	if rel == nil || rel.Seed == nil || len(rel.Seed.Key) != 1 || rel.Seed.Key[0] != id[dot+1:] {
		return nil, false
	}
	idx := -1
	for i, c := range rel.Seed.Columns {
		if c == id[dot+1:] {
			idx = i
		}
	}
	if idx < 0 {
		return nil, false
	}
	labels := make([]string, 0, len(rel.Seed.Rows))
	for _, row := range rel.Seed.Rows {
		labels = append(labels, constText(row[idx]))
	}
	return labels, true
}

func lastDot(s string) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == '.' {
			return i
		}
	}
	return -1
}

// constText is the value of a constant as Go constants spell it: the string itself for a
// string, digits for an integer; other expressions as SQL text.
func constText(e schema.Expr) string {
	if ac := e.GetAConst(); ac != nil {
		switch v := ac.Val.(type) {
		case *pgparse.A_Const_Sval:
			return v.Sval.GetSval()
		case *pgparse.A_Const_Ival:
			return strconv.Itoa(int(v.Ival.GetIval()))
		}
	}
	if tc := e.GetTypeCast(); tc != nil {
		return constText(tc.Arg)
	}
	return schema.Deparse(e)
}
