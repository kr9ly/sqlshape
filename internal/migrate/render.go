package migrate

import (
	"sort"
	"strings"

	pg_query "github.com/pganalyze/pg_query_go/v6"

	"github.com/kr9ly/sqlshape/internal/diff"
	"github.com/kr9ly/sqlshape/internal/schema"
)

func colProps(s *schema.Schema, c *schema.Column) map[string]string  { return diff.Props(s, c) }
func fnProps(s *schema.Schema, f *schema.Function) map[string]string { return diff.Props(s, f) }

// typeText is a column's type with its collation.
func typeText(s *schema.Schema, c *schema.Column) string {
	t := s.Types.Format(c.Type)
	if c.Collation != "" {
		t += " COLLATE " + c.Collation
	}
	return t
}

// columnText renders a column definition for ADD COLUMN (notNull false leaves the NOT
// NULL off, for a column that is backfilled first).
func columnText(s *schema.Schema, c *schema.Column, notNull bool) string {
	var b strings.Builder
	b.WriteString(q(c.Name) + " " + typeText(s, c))
	if c.Generated != nil {
		b.WriteString(" GENERATED ALWAYS AS (" + schema.Deparse(c.Generated) + ") STORED")
	}
	if c.Identity != 0 {
		b.WriteString(" GENERATED " + identityWord(c.Identity) + " AS IDENTITY")
	}
	if c.Default != nil {
		b.WriteString(" DEFAULT " + schema.Deparse(c.Default))
	}
	if c.NotNull && notNull {
		b.WriteString(" NOT NULL")
	}
	return b.String()
}

func identityWord(id byte) string {
	if id == 'a' {
		return "ALWAYS"
	}
	return "BY DEFAULT"
}

// constraintText renders a table constraint body (after ADD CONSTRAINT name).
func constraintText(s *schema.Schema, c *schema.Constraint) string {
	switch c.Kind {
	case schema.PrimaryKey:
		return "PRIMARY KEY (" + qlist(c.Columns) + ")"
	case schema.Unique:
		t := "UNIQUE"
		if c.NullsNotDistinct {
			t += " NULLS NOT DISTINCT"
		}
		return t + " (" + qlist(c.Columns) + ")"
	case schema.ForeignKey:
		t := "FOREIGN KEY (" + qlist(c.Columns) + ") REFERENCES " + qdot(c.RefTable) + " (" + qlist(c.RefColumns) + ")"
		if w := actionWord(c.OnDelete); w != "" {
			t += " ON DELETE " + w
		}
		if w := actionWord(c.OnUpdate); w != "" {
			t += " ON UPDATE " + w
		}
		if c.Deferrable {
			t += " DEFERRABLE"
		}
		return t
	case schema.Check:
		return "CHECK (" + schema.Deparse(c.Expr) + ")"
	}
	return ""
}

func actionWord(a byte) string {
	switch a {
	case 'r':
		return "RESTRICT"
	case 'c':
		return "CASCADE"
	case 'n':
		return "SET NULL"
	case 'd':
		return "SET DEFAULT"
	}
	return ""
}

func relWord(r *schema.Relation) string {
	switch r.Kind {
	case schema.View:
		return "VIEW"
	case schema.MatView:
		return "MATERIALIZED VIEW"
	case schema.Sequence:
		return "SEQUENCE"
	}
	return "TABLE"
}

func fnWord(f *schema.Function) string {
	switch {
	case f.IsProc:
		return "PROCEDURE"
	case f.IsAgg:
		return "AGGREGATE"
	}
	return "FUNCTION"
}

func typeWord(kind string) string {
	if kind == "domain" {
		return "DOMAIN"
	}
	return "TYPE"
}

// labels splits a ", "-joined property back into its items.
func labels(joined string) []string {
	if joined == "" {
		return nil
	}
	return strings.Split(joined, ", ")
}

func ruleNode(rd schema.RuleDef) *pg_query.Node {
	return &pg_query.Node{Node: &pg_query.Node_RuleStmt{RuleStmt: rd.Stmt}}
}

// ownerRelation / ownerColumn split a sequence's OwnedBy ("schema.table.column").
func ownerRelation(owned string) string {
	parts := strings.Split(owned, ".")
	rel := strings.Join(parts[:len(parts)-1], ".")
	return strings.TrimPrefix(rel, "public.")
}

func ownerColumn(owned string) string {
	parts := strings.Split(owned, ".")
	return parts[len(parts)-1]
}

// commentTarget resolves a comment key against a schema: the object kind word and its
// quoted name, "" if the object is not there.
func commentTarget(s *schema.Schema, key string) (kind, name string) {
	if t := strings.TrimPrefix(key, "type:"); t != key {
		for n, ut := range diff.UserTypes(s) {
			if n == t {
				return typeWord(ut.Kind), t
			}
		}
		return "", ""
	}
	rels, _ := relations(s)
	if r := rels[key]; r != nil {
		return relWord(r), qrel(r)
	}
	if i := strings.LastIndex(key, "."); i > 0 {
		if r := rels[key[:i]]; r != nil {
			if r.Query != nil || r.Column(key[i+1:]) != nil {
				return "COLUMN", qrel(r) + "." + q(key[i+1:])
			}
		}
	}
	return "", ""
}

// commentText renders COMMENT ON for a key; "" if the object does not exist in s.
func commentText(s *schema.Schema, key, text string) string {
	kind, name := commentTarget(s, key)
	if kind == "" {
		return ""
	}
	v := "NULL"
	if text != "" {
		v = lit(text)
	}
	return "COMMENT ON " + kind + " " + name + " IS " + v
}

func commentRelation(key string) string {
	if i := strings.LastIndex(key, "."); i > 0 {
		return key[:i]
	}
	return key
}

func commentObjectSurvives(s *schema.Schema, key string) bool {
	kind, _ := commentTarget(s, key)
	return kind != ""
}

func extensions(s *schema.Schema) map[string]bool {
	out := map[string]bool{}
	for _, e := range s.Catalog.Extensions {
		out[e.Name] = true
	}
	return out
}

func set(names []string) map[string]bool {
	out := map[string]bool{}
	for _, n := range names {
		out[n] = true
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// qdot quotes each part of a dotted name.
func qdot(dotted string) string {
	return strings.Join(strings.Split(qlist(strings.Split(dotted, ".")), ", "), ".")
}

// replaceable is whether CREATE OR REPLACE VIEW can turn view f into r: the old columns
// stay, in place, with their types, and new ones only append.
func replaceable(a, b *schema.Schema, f, r *schema.Relation) bool {
	if len(f.Frozen) > len(r.Frozen) {
		return false
	}
	for i, c := range f.Frozen {
		if r.Frozen[i].Name != c.Name || a.Types.Format(c.Type) != b.Types.Format(r.Frozen[i].Type) {
			return false
		}
	}
	return true
}
