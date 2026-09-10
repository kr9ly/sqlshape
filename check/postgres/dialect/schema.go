package dialect

import (
	"fmt"
	"strings"

	"github.com/kr9ly/sqlshape/check/postgres/v2/analyze"
	"github.com/kr9ly/sqlshape/check/postgres/v2/schema"
	"github.com/kr9ly/sqlshape/v2/x/dialect"
	"github.com/kr9ly/sqlshape/v2/x/facts"
	"github.com/kr9ly/sqlshape/v2/x/obligation"
)

func init() {
	dialect.Register(dialect.Postgres, Load)
}

// Load is the PostgreSQL loader behind `-- sqlshape: postgres <version>` (and behind a
// schema that declares no dialect): the text must declare its version, since the version
// decides the grammar, the catalog and the server pgtest runs.
func Load(schemaSQL string) (dialect.Analyzer, error) {
	if err := schema.RequireVersion("schema.sql", schemaSQL); err != nil {
		return nil, err
	}
	s, err := analyze.Load(schemaSQL)
	if err != nil {
		return nil, err
	}
	return New(s), nil
}

// Schema is the loaded schema as the checker sees it.
func (a *Analyzer) Schema() dialect.Schema { return a }

// Contract is the schema as the obligations see it.
func (a *Analyzer) Contract() obligation.Schema { return a.S.Contract() }

// Lower parses and types a declared predicate against a relation.
func (a *Analyzer) Lower(expr string, rel obligation.Relation) ([]facts.Pred, error) {
	return analyze.Lower(a.S, expr, a.S.ByFullName(rel.FullName()))
}

// Type resolves a declared type name.
func (a *Analyzer) Type(name string) (dialect.Type, bool) {
	sch, n := "", name
	if i := strings.LastIndex(name, "."); i >= 0 {
		sch, n = name[:i], name[i+1:]
	}
	pt := a.S.Types.Lookup(sch, n)
	if pt == nil {
		return dialect.Type{}, false
	}
	return TypeOf(a.S, schema.TypeRef{OID: pt.OID, Typmod: -1}), true
}

// Source describes a column of a relation as a statement's Source would.
func (a *Analyzer) Source(table, column string) *dialect.Source {
	rel := a.Relation(table)
	if rel == nil {
		return nil
	}
	col := rel.Column(column)
	if col == nil {
		return nil
	}
	return SourceOf(a.S, &analyze.Source{Table: rel.Name, Column: column, NotNull: col.NotNull, Assigned: true})
}

// Relation resolves a name the way a program writes it.
func (a *Analyzer) Relation(name string) *dialect.Relation {
	sch, n := "", name
	if i := strings.LastIndex(name, "."); i >= 0 {
		sch, n = name[:i], name[i+1:]
	}
	rel := a.S.Relation(sch, n)
	if rel == nil {
		return nil
	}
	return a.relation(rel)
}

// Relations lists every relation, in declaration order.
func (a *Analyzer) Relations() []*dialect.Relation {
	out := make([]*dialect.Relation, 0, len(a.S.Relations))
	for _, rel := range a.S.Relations {
		out = append(out, a.relation(rel))
	}
	return out
}

func (a *Analyzer) relation(rel *schema.Relation) *dialect.Relation {
	r := &dialect.Relation{Name: rel.FullName(), Schema: rel.Schema, Kind: facts.Table, Comment: a.S.Comments[rel.FullName()]}
	switch rel.Kind {
	case schema.View:
		r.Kind = facts.View
	case schema.MatView:
		r.Kind = facts.MatView
	}
	for _, c := range rel.Columns {
		r.Columns = append(r.Columns, dialect.SchemaColumn{Name: c.Name, Type: TypeOf(a.S, c.Type), NotNull: c.NotNull,
			HasDefault: c.Default != nil, Identity: c.Identity != 0, Generated: c.Generated != nil, Comment: a.S.Comments[rel.FullName()+"."+c.Name]})
	}
	for _, con := range rel.Constraints {
		if con.Kind == schema.Unique && con.Predicate == nil {
			r.Unique = true
		}
	}
	return r
}

// Definitions analyzes the schema's own statements once: function bodies (SQL and
// PL/pgSQL, checked like PostgreSQL does at CREATE time), row-security policies (typed
// like CREATE POLICY does), and view bodies (sqlshape's own findings are the view's).
func (a *Analyzer) Definitions() []dialect.Definition {
	a.define()
	return a.defs
}

// Advice: what the schema does less well than it looks (-strict). An enum column that a
// seeded lookup table would serve better, a materialized view without a unique index, row
// security enabled without a policy or read through current_setting, a SECURITY DEFINER
// function that policies do not bind, and the functions' own advisory notes.
func (a *Analyzer) Advice() []string {
	a.define()
	var out []string
	for _, rel := range a.S.Relations {
		if rel.Kind != schema.Table || !rel.RowSecurity {
			continue
		}
		if len(rel.Policies) == 0 {
			out = append(out, fmt.Sprintf("%s has row level security enabled and no policy: every role but the owner sees no rows", rel.FullName()))
		}
		for _, pol := range rel.Policies {
			for _, name := range analyze.SettingReads(pol) {
				out = append(out, fmt.Sprintf("policy %s on %s reads current_setting(%q, true): a session that never set it gets NULL, so the predicate hides every row silently; without missing_ok the session fails loudly instead", pol.Name, rel.FullName(), name))
			}
		}
		if rel.ForceRowSecurity {
			continue
		}
		// a SECURITY DEFINER function runs as its owner, whom the policies do not bind
		// unless the table forces them
		for _, fn := range a.S.Functions {
			if !fn.SecurityDefiner {
				continue
			}
			for _, ref := range a.fnRefs[fn] {
				if ref.Schema == rel.Schema && ref.Name == rel.Name {
					out = append(out, fmt.Sprintf("function %s is SECURITY DEFINER and reaches %s, whose policies do not bind the owner: rows are unrestricted inside it (ALTER TABLE %s FORCE ROW LEVEL SECURITY applies them)", fn.Name, rel.FullName(), rel.FullName()))
					break
				}
			}
		}
	}
	for _, fn := range a.S.Functions {
		for _, n := range a.fnAdvice[fn] {
			out = append(out, fmt.Sprintf("function %s: %s", fn.Name, n.Message))
		}
	}
	for _, rel := range a.S.Relations {
		if rel.Kind == schema.Table {
			// a value set kept as an enum cannot lose or reorder a label without the type
			// being rebuilt under every column (see migrate); a seeded lookup table changes
			// with a MERGE, its rows can carry a label and an order, and the checker reads
			// it just as well
			for _, col := range rel.Columns {
				if t := TypeOf(a.S, col.Type); t.Kind == dialect.Enum && !rel.Temp {
					out = append(out, fmt.Sprintf("%s.%s is enum %s: a seeded lookup table (rows in schema.sql, referenced by a foreign key) is easier to change — an enum cannot drop or reorder a label without being recreated under every column — and is checked the same way", rel.FullName(), col.Name, t.Named))
				}
			}
		}
		if rel.Kind != schema.MatView {
			continue
		}
		if !a.relation(rel).Unique {
			out = append(out, fmt.Sprintf("materialized view %s has no unique index, so REFRESH MATERIALIZED VIEW CONCURRENTLY is not possible", rel.FullName()))
		}
	}
	return out
}

// define runs the schema's own statements once.
func (a *Analyzer) define() {
	if a.defined {
		return
	}
	a.defined = true
	a.fnRefs = map[*schema.Function][]analyze.RelationRef{}
	a.fnAdvice = map[*schema.Function][]analyze.Note{}
	for _, fn := range a.S.Functions {
		fr, err := analyze.AnalyzeFunction(a.S, fn)
		if err != nil {
			a.defs = append(a.defs, dialect.Definition{What: "function " + fn.Name, Err: err.Error()})
			continue
		}
		a.fnRefs[fn] = fr.Relations
		for _, st := range fr.Statements {
			what := "function " + fn.Name
			if st.Line > 0 {
				what = fmt.Sprintf("%s: line %d", what, st.Line)
			}
			a.defs = append(a.defs, dialect.Definition{What: what, Facts: st.Facts})
		}
		d := dialect.Definition{What: "function " + fn.Name}
		for _, n := range fr.Notes {
			if n.Advisory() {
				a.fnAdvice[fn] = append(a.fnAdvice[fn], n)
			} else {
				d.Notes = append(d.Notes, dialect.Note{Message: n.Message, Position: pos(n.Position)})
			}
		}
		if len(d.Notes) > 0 {
			a.defs = append(a.defs, d)
		}
	}
	for _, rel := range a.S.Relations {
		for _, pol := range rel.Policies {
			d := dialect.Definition{What: fmt.Sprintf("policy %s on %s", pol.Name, rel.FullName())}
			notes, err := analyze.AnalyzePolicy(a.S, rel, pol)
			if err != nil {
				d.Err = err.Error()
			}
			for _, n := range notes {
				if !n.Advisory() {
					d.Notes = append(d.Notes, dialect.Note{Message: n.Message, Position: pos(n.Position)})
				}
			}
			if d.Err != "" || len(d.Notes) > 0 {
				a.defs = append(a.defs, d)
			}
		}
		if len(rel.Policies) > 0 && !rel.RowSecurity {
			a.defs = append(a.defs, dialect.Definition{Err: fmt.Sprintf("%s has policies but row level security is not enabled, so they do not apply: ALTER TABLE %s ENABLE ROW LEVEL SECURITY", rel.FullName(), rel.FullName())})
		}
	}
	for _, rel := range a.S.Relations {
		if rel.Kind != schema.View && rel.Kind != schema.MatView {
			continue
		}
		d := dialect.Definition{What: "view " + rel.Name}
		r, err := analyze.AnalyzeView(a.S, rel)
		if err != nil {
			d.Err = err.Error()
			a.defs = append(a.defs, d)
			continue
		}
		for _, n := range r.Notes {
			if !n.Advisory() {
				d.Notes = append(d.Notes, dialect.Note{Message: n.Message, Position: pos(n.Position)})
			}
		}
		d.Facts = r.Facts
		a.defs = append(a.defs, d)
	}
}
