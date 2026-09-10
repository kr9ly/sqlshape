package dialect

import (
	"strings"

	"github.com/kr9ly/sqlshape/check/mysql/v2/internal/analyze"
	"github.com/kr9ly/sqlshape/check/mysql/v2/internal/schema"
	"github.com/kr9ly/sqlshape/v2/x/dialect"
	"github.com/kr9ly/sqlshape/v2/x/facts"
	"github.com/kr9ly/sqlshape/v2/x/obligation"
)

// The schema side of the contract: what the checker asks of the loaded schema beyond
// analyzing statements (dialect.Schema), and the schema as the obligations see it
// (obligation.Schema, with the lowering of a declared predicate).

// Schema is the loaded schema as the checker sees it.
func (m *mysql) Schema() dialect.Schema { return m }

// Contract is the schema as the obligations see it.
func (m *mysql) Contract() obligation.Schema { return contractSchema{m} }

// Lower parses and types a declared predicate against a relation.
func (m *mysql) Lower(expr string, rel obligation.Relation) ([]facts.Pred, error) {
	t := m.s.Table(rel.FullName())
	if t == nil {
		return nil, &dialect.Error{Message: "a predicate obligation is declared on a table; " + rel.FullName() + " is not one", Position: -1}
	}
	preds, err := analyze.Lower(m.s, expr, t)
	if err != nil {
		return nil, errorOf(err)
	}
	return preds, nil
}

// Relation resolves a name the way a program writes it.
func (m *mysql) Relation(name string) *dialect.Relation {
	if t := m.s.Table(name); t != nil {
		return m.tableRelation(t)
	}
	if v := m.s.View(name); v != nil {
		return m.viewRelation(v)
	}
	return nil
}

// Relations lists every relation: the tables, then the views, in declaration order.
func (m *mysql) Relations() []*dialect.Relation {
	var out []*dialect.Relation
	for _, t := range m.s.Tables {
		out = append(out, m.tableRelation(t))
	}
	for _, v := range m.s.Views {
		out = append(out, m.viewRelation(v))
	}
	return out
}

func (m *mysql) tableRelation(t *schema.Table) *dialect.Relation {
	r := &dialect.Relation{Name: t.Name, Kind: facts.Table, Comment: t.Comment}
	for _, c := range t.Columns {
		r.Columns = append(r.Columns, dialect.SchemaColumn{Name: c.Name, Type: typeOf(c.Type, true), NotNull: c.NotNull,
			HasDefault: c.Default != nil || c.AutoIncrement, Identity: c.AutoIncrement, Generated: c.Generated != nil, Comment: c.Comment})
	}
	for _, k := range t.Keys {
		if k.Kind == schema.Primary || k.Kind == schema.Unique {
			if _, whole := wholeColumns(k); whole {
				r.Unique = true
			}
		}
	}
	return r
}

func (m *mysql) viewRelation(v *schema.View) *dialect.Relation {
	r := &dialect.Relation{Name: v.Name, Kind: facts.View}
	if vr := m.viewResult(v); vr != nil {
		for _, c := range vr.Columns {
			col := dialect.SchemaColumn{Name: c.Name, NotNull: !c.Nullable}
			if c.Known {
				col.Type = typeOf(c.Type, true)
			}
			r.Columns = append(r.Columns, col)
		}
	}
	return r
}

// viewResult analyzes a view's query once; nil when the query does not analyze (the
// definition reports why).
func (m *mysql) viewResult(v *schema.View) *analyze.ViewResult {
	if m.views == nil {
		m.views = map[*schema.View]*analyze.ViewResult{}
	}
	if vr, ok := m.views[v]; ok {
		return vr
	}
	m.views[v] = nil
	vr, err := analyze.AnalyzeView(m.s, v)
	if err != nil {
		m.viewErrs = append(m.viewErrs, dialect.Definition{What: "view " + v.Name, Err: err.Error()})
		return nil
	}
	m.views[v] = vr
	return vr
}

// Definitions are the schema's own statements as the checker judges them: the view bodies,
// each with the facts the schema's obligations are judged on.
func (m *mysql) Definitions() []dialect.Definition {
	var out []dialect.Definition
	for _, v := range m.s.Views {
		vr := m.viewResult(v)
		if vr == nil {
			continue
		}
		out = append(out, dialect.Definition{What: "view " + v.Name, Facts: vr.Facts})
	}
	return append(out, m.viewErrs...)
}

// Advice: what the schema does less well than it looks (-strict). A table on an engine
// that enforces no constraints, and a CHECK declared NOT ENFORCED.
func (m *mysql) Advice() []string {
	var out []string
	for _, t := range m.s.Tables {
		if e := strings.ToUpper(t.Engine); e != "" && e != "INNODB" {
			out = append(out, "table "+t.Name+" uses ENGINE="+t.Engine+": foreign keys are not enforced, and a statement is not atomic under it")
		}
		for _, c := range t.Checks {
			if !c.Enforced {
				out = append(out, "table "+t.Name+": CHECK "+t.CheckName(c)+" is NOT ENFORCED, so it documents an intent the server does not check")
			}
		}
	}
	return out
}

// Type resolves a declared type name: MySQL declares no types of its own, so a
// `// sqlshape: type X` declaration has nothing to bind to.
func (m *mysql) Type(name string) (dialect.Type, bool) { return dialect.Type{}, false }

// Source describes a column of a table as a statement's Source would: the key identity
// it carries (following single-column foreign keys to the root primary key), the values
// an ENUM limits it to, its DEFAULT / generation, its COMMENT.
func (m *mysql) Source(table, column string) *dialect.Source {
	t := m.s.Table(table)
	if t == nil {
		return nil
	}
	c := t.Column(column)
	if c == nil {
		return nil
	}
	out := &dialect.Source{Table: t.Name, Column: c.Name, NotNull: c.NotNull, Assigned: true, Comment: c.Comment,
		HasDefault: c.Default != nil || c.AutoIncrement, Generated: c.AutoIncrement || c.Generated != nil}
	if c.Type.Name == "enum" && len(c.Type.Values) > 0 {
		out.Values, out.ValuesFrom = append([]string{}, c.Type.Values...), "check"
	}
	if id, ok := m.identity(t, c.Name, 0); ok {
		out.Identity = id
	}
	return out
}

// identity follows single-column primary / foreign key structure to the root key column.
func (m *mysql) identity(t *schema.Table, column string, depth int) (string, bool) {
	if depth > 10 {
		return "", false
	}
	for _, fk := range t.ForeignKeys {
		pos := -1
		for i, col := range fk.Columns {
			if strings.EqualFold(col, column) {
				pos = i
			}
		}
		if pos < 0 {
			continue
		}
		parent := m.s.Table(fk.RefTable)
		if parent == nil {
			continue
		}
		refCols := fk.RefColumns
		if len(refCols) == 0 {
			if pk := parent.PrimaryKey(); pk != nil {
				refCols, _ = wholeColumns(pk)
			}
		}
		if pos < len(refCols) {
			if id, ok := m.identity(parent, refCols[pos], depth+1); ok {
				return id, true
			}
		}
	}
	if pk := t.PrimaryKey(); pk != nil {
		if cols, ok := wholeColumns(pk); ok {
			for _, col := range cols {
				if strings.EqualFold(col, column) {
					return t.Name + "." + col, true // each column of a (possibly composite) primary key is an identity of its own
				}
			}
		}
	}
	return "", false
}

// wholeColumns lists a key's columns when every part is a whole column.
func wholeColumns(k *schema.Key) ([]string, bool) {
	cols := make([]string, 0, len(k.Parts))
	for _, p := range k.Parts {
		if p.Expr != nil || p.Length != 0 || p.Column == "" {
			return nil, false
		}
		cols = append(cols, p.Column)
	}
	return cols, len(cols) > 0
}

// contractSchema is the schema as the obligations see it.
type contractSchema struct{ m *mysql }

func (c contractSchema) Relations() []obligation.Relation {
	var out []obligation.Relation
	for _, t := range c.m.s.Tables {
		out = append(out, contractTable{c.m, t})
	}
	for _, v := range c.m.s.Views {
		out = append(out, contractView{c.m, v})
	}
	return out
}

func (c contractSchema) Relation(name string) obligation.Relation {
	if t := c.m.s.Table(name); t != nil {
		return contractTable{c.m, t}
	}
	if v := c.m.s.View(name); v != nil {
		return contractView{c.m, v}
	}
	return nil
}

type contractTable struct {
	m *mysql
	t *schema.Table
}

func (c contractTable) Name() string              { return c.t.Name }
func (c contractTable) FullName() string          { return c.t.Name }
func (c contractTable) Kind() facts.RelKind       { return facts.Table }
func (c contractTable) HasColumn(col string) bool { return c.t.Column(col) != nil }
func (c contractTable) Directives() []string      { return c.t.Directives }
func (c contractTable) ForceRowSecurity() bool    { return false }

func (c contractTable) ViewSource(col string) (string, string, bool) { return "", "", false }

// ForeignKeys lists the REFERENCES constraints; a key with no column list references the
// parent's primary key, spelled out here.
func (c contractTable) ForeignKeys() []obligation.ForeignKey {
	var out []obligation.ForeignKey
	for _, fk := range c.t.ForeignKeys {
		o := obligation.ForeignKey{Columns: fk.Columns, RefTable: fk.RefTable, RefColumns: fk.RefColumns}
		if len(o.RefColumns) == 0 {
			if parent := c.m.s.Table(fk.RefTable); parent != nil {
				if pk := parent.PrimaryKey(); pk != nil {
					o.RefColumns, _ = wholeColumns(pk)
				}
			}
		}
		out = append(out, o)
	}
	return out
}

type contractView struct {
	m *mysql
	v *schema.View
}

func (c contractView) Name() string                         { return c.v.Name }
func (c contractView) FullName() string                     { return c.v.Name }
func (c contractView) Kind() facts.RelKind                  { return facts.View }
func (c contractView) Directives() []string                 { return c.v.Directives }
func (c contractView) ForeignKeys() []obligation.ForeignKey { return nil }
func (c contractView) ForceRowSecurity() bool               { return false }

// HasColumn: one of the view's output columns.
func (c contractView) HasColumn(col string) bool {
	vr := c.m.viewResult(c.v)
	if vr == nil {
		return false
	}
	for _, vc := range vr.Columns {
		if strings.EqualFold(vc.Name, col) {
			return true
		}
	}
	return false
}

// ViewSource is the base column an output column passes through unchanged.
func (c contractView) ViewSource(col string) (string, string, bool) {
	vr := c.m.viewResult(c.v)
	if vr == nil {
		return "", "", false
	}
	for i, vc := range vr.Columns {
		if strings.EqualFold(vc.Name, col) && i < len(vr.Sources) && vr.Sources[i].Table != "" {
			return vr.Sources[i].Table, vr.Sources[i].Column, true
		}
	}
	return "", "", false
}
