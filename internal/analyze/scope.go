package analyze

import (
	pg_query "github.com/pganalyze/pg_query_go/v6"

	"github.com/kr9ly/sqlshape/internal/catalog"
	"github.com/kr9ly/sqlshape/internal/schema"
)

// rteCol is one visible column of a range-table entry.
type rteCol struct {
	name     string
	typ      schema.TypeRef
	nullable bool
	src      *Source
	lit      bool     // a constant target column, see expr.lit
	fields   []rteCol // record shape, see expr.fields
}

// rte is a FROM item: a table / view / CTE / subquery / function, or a join of two.
type rte struct {
	alias   string
	cols    []rteCol
	rowType catalog.OID // whole-row reference type, 0 for joins / subqueries
	// join structure (nil for leaves)
	join *joinInfo
	// unqualified-lookup hiding: names merged away by USING on the right side
	hidden map[string]bool
	// cardinality (card.go): the table, or the defining query, or "a scalar function"
	rel    *schema.Relation
	sub    *subquery
	single bool
	// outerNullable: an outer join made every column nullable (null-extended rows)
	outerNullable bool
}

type joinInfo struct {
	left, right *rte
	using       []string
	usingCols   []rteCol
	jointype    pg_query.JoinType
	quals       *pg_query.Node
}

// leaves returns the leaf rtes (the ones that can be referenced by alias).
func (r *rte) leaves() []*rte {
	if r.join == nil {
		return []*rte{r}
	}
	return append(r.join.left.leaves(), r.join.right.leaves()...)
}

// expand returns the columns in SELECT * order.
func (r *rte) expand() []rteCol {
	if r.join == nil {
		return r.cols
	}
	j := r.join
	out := append([]rteCol{}, j.usingCols...)
	skip := map[string]bool{}
	for _, u := range j.using {
		skip[u] = true
	}
	for _, c := range j.left.expand() {
		if !skip[c.name] {
			out = append(out, c)
		}
	}
	for _, c := range j.right.expand() {
		if !skip[c.name] {
			out = append(out, c)
		}
	}
	return out
}

// find looks a column name up in this rte (unqualified semantics for joins).
// Returns the matches (more than one = ambiguous).
func (r *rte) find(name string) []rteCol {
	if r.join == nil {
		var out []rteCol
		for _, c := range r.cols {
			if c.name == name && !r.hidden[name] {
				out = append(out, c)
			}
		}
		return out
	}
	for _, u := range r.join.using {
		if u == name {
			for _, c := range r.join.usingCols {
				if c.name == name {
					return []rteCol{c}
				}
			}
		}
	}
	return append(r.join.left.find(name), r.join.right.find(name)...)
}

// scope is one query level's name space.
type scope struct {
	parent *scope
	items  []*rte
	ctes   map[string]*cte
	// agg: this level's target list / HAVING has an aggregate (one row without GROUP BY)
	agg bool
}

type cte struct {
	name      string
	cols      []rteCol
	recursive bool
	sub       *subquery // defining query for cardinality proofs (nil when recursive)
}

func newScope(parent *scope) *scope {
	return &scope{parent: parent, ctes: map[string]*cte{}}
}

func (sc *scope) findCTE(name string) *cte {
	for s := sc; s != nil; s = s.parent {
		if c, ok := s.ctes[name]; ok {
			return c
		}
	}
	return nil
}

// byAlias finds a leaf rte by alias in this scope only.
func (sc *scope) byAlias(alias string) *rte {
	for _, it := range sc.items {
		for _, l := range it.leaves() {
			if l.alias == alias {
				return l
			}
		}
	}
	return nil
}

// resolveColumn resolves [tbl.]col across the scope chain.
func (a *analyzer) resolveColumn(sc *scope, tbl, col string, loc int32) (rteCol, *Error) {
	for s := sc; s != nil; s = s.parent {
		if tbl != "" {
			r := s.byAlias(tbl)
			if r == nil {
				continue
			}
			var hits []rteCol
			for _, c := range r.cols {
				if c.name == col {
					hits = append(hits, c)
				}
			}
			if len(hits) == 0 {
				return rteCol{}, errAt(codeUndefinedColumn, loc, "column %s.%s does not exist", tbl, col)
			}
			return hits[0], nil
		}
		var hits []rteCol
		for _, it := range s.items {
			hits = append(hits, it.find(col)...)
		}
		if len(hits) > 1 {
			return rteCol{}, errAt(codeAmbiguousColumn, loc, "column reference %q is ambiguous", col)
		}
		if len(hits) == 1 {
			return hits[0], nil
		}
	}
	if tbl != "" {
		// distinguish missing table from missing column like PG does
		for s := sc; s != nil; s = s.parent {
			if s.byAlias(tbl) != nil {
				return rteCol{}, errAt(codeUndefinedColumn, loc, "column %s.%s does not exist", tbl, col)
			}
		}
		return rteCol{}, errAt(codeUndefinedTable, loc, "missing FROM-clause entry for table %q", tbl)
	}
	return rteCol{}, errAt(codeUndefinedColumn, loc, "column %q does not exist", col)
}

// wholeRow finds an rte by alias for a bare `t` reference; returns nil if none.
func (sc *scope) wholeRow(name string) *rte {
	for s := sc; s != nil; s = s.parent {
		if r := s.byAlias(name); r != nil {
			return r
		}
	}
	return nil
}

// relationRTE builds a leaf rte for a table / view.
func (a *analyzer) relationRTE(rel *schema.Relation, alias *pg_query.Alias, loc int32) (*rte, *Error) {
	if a.inView == 0 && rel.Kind != 'c' {
		dup := false
		for _, ref := range a.refs {
			if ref.Schema == rel.Schema && ref.Name == rel.Name {
				dup = true
			}
		}
		if !dup {
			a.refs = append(a.refs, RelationRef{Schema: rel.Schema, Name: rel.Name, Kind: byte(rel.Kind), Position: loc + 1})
		}
	}
	r := &rte{alias: rel.Name, rowType: rel.RowType}
	if alias != nil && alias.Aliasname != "" {
		r.alias = alias.Aliasname
	}
	var cols []rteCol
	switch rel.Kind {
	case schema.Table, 'c':
		r.rel = rel
		for _, c := range rel.Columns {
			cols = append(cols, rteCol{
				name: c.Name, typ: c.Type, nullable: !c.NotNull && !a.domainNotNull(c.Type.OID),
				src: &Source{Table: rel.FullName(), Column: c.Name, NotNull: c.NotNull},
			})
		}
	case schema.View, schema.MatView:
		vc, err := a.viewColumns(rel)
		if err != nil {
			return nil, err
		}
		r.sub = a.viewScopes[rel]
		for _, c := range vc {
			cols = append(cols, rteCol{
				name: c.name, typ: c.typ, nullable: c.nullable,
				// PG's Describe reports the view itself as the source, never the base table
				src: &Source{Table: rel.FullName(), Column: c.name, NotNull: false},
			})
		}
	}
	if alias != nil {
		for i, n := range alias.Colnames {
			if i < len(cols) {
				cols[i].name = n.GetString_().GetSval()
			}
		}
	}
	r.cols = cols
	return r, nil
}

// viewColumns analyzes a view's defining query once.
func (a *analyzer) viewColumns(rel *schema.Relation) ([]rteCol, *Error) {
	if cols, ok := a.viewCache[rel]; ok {
		return cols, nil
	}
	if a.viewBusy[rel] {
		return nil, errAt(codeFeatureNotSupported, -1, "recursive view %s", rel.Name)
	}
	a.viewBusy[rel] = true
	defer delete(a.viewBusy, rel)
	a.inView++
	defer func() { a.inView-- }()
	sel := rel.Query.GetSelectStmt()
	if sel == nil {
		return nil, errAt(codeFeatureNotSupported, -1, "view %s: unsupported defining query", rel.Name)
	}
	vsc := newScope(nil)
	savedNotes := a.notes
	cols, err := a.selectStmt(sel, vsc)
	a.notes = savedNotes // a view body's findings belong to the view (AnalyzeView), not to its readers
	if err != nil {
		return nil, err
	}
	for i, n := range rel.ColumnAliases {
		if i < len(cols) {
			cols[i].name = n
		}
	}
	a.viewCache[rel] = cols
	a.viewScopes[rel] = &subquery{what: "view", sel: sel, sc: vsc}
	return cols, nil
}

// domainNotNull reports whether oid is (or wraps) a domain declared NOT NULL.
func (a *analyzer) domainNotNull(oid catalog.OID) bool {
	for {
		t := a.s.Types.ByOID(oid)
		if t == nil || t.Kind != 'd' {
			return false
		}
		if d := a.s.Types.Domains[oid]; d != nil && d.NotNull {
			return true
		}
		oid = t.BaseType
	}
}
