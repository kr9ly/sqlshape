package migrate

import (
	"sort"
	"strings"

	"github.com/kr9ly/sqlshape/check/postgres/v2/catalog"
	"github.com/kr9ly/sqlshape/check/postgres/v2/diff"
	"github.com/kr9ly/sqlshape/check/postgres/v2/pgparse"
	"github.com/kr9ly/sqlshape/check/postgres/v2/schema"
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
		kind := " STORED"
		if c.GeneratedVirtual {
			kind = " VIRTUAL"
		}
		b.WriteString(" GENERATED ALWAYS AS (" + schema.Deparse(c.Generated) + ")" + kind)
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

// typmodNarrows reports whether an ALTER COLUMN ... TYPE from fc's type to c's type can
// round or truncate values already in the column: same base type, both sides give an
// explicit typmod, and the new one holds less than the old one (numeric precision/scale,
// varchar(n)/char(n) length, a time family's fractional-second precision, bit(n) length).
// PostgreSQL runs a same-base-type ALTER without a USING clause and without complaint --
// unlike a NOT NULL addition (which the planner refuses without `@migrate backfill`) or an
// enum label drop (which it refuses without `@migrate enum ... drop`), so this can only
// leave a note: it is a proposal the schema author edits, not an error the plan can raise.
func typmodNarrows(s *schema.Schema, fc, c *schema.Column) bool {
	if fc.Type.OID != c.Type.OID {
		return false
	}
	fm, tm := fc.Type.Typmod, c.Type.Typmod
	if fm < 0 || tm < 0 {
		return false
	}
	switch fc.Type.OID {
	case catalog.Numeric:
		fPrec, fScale, fok := schema.NumericTypmod(fm)
		tPrec, tScale, tok := schema.NumericTypmod(tm)
		if !fok || !tok {
			return false
		}
		return tScale < fScale || tPrec-tScale < fPrec-fScale
	case catalog.Varchar, catalog.BPChar:
		if fm < 4 || tm < 4 {
			return false
		}
		return tm < fm
	case catalog.Time, catalog.TimeTZ, catalog.Timestamp, catalog.TimestampTZ:
		return tm < fm
	}
	if t := s.Types.ByOID(fc.Type.OID); t != nil && (t.Name == "bit" || t.Name == "varbit") {
		return tm < fm
	}
	return false
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
		return "PRIMARY KEY (" + keyList(c) + ")"
	case schema.Unique:
		t := "UNIQUE"
		if c.NullsNotDistinct {
			t += " NULLS NOT DISTINCT"
		}
		return t + " (" + keyList(c) + ")"
	case schema.ForeignKey:
		t := "FOREIGN KEY (" + periodList(c.Columns, c.WithPeriod) + ") REFERENCES " + qdot(c.RefTable) + " (" + periodList(c.RefColumns, c.WithPeriod) + ")"
		if w := actionWord(c.OnDelete); w != "" {
			t += " ON DELETE " + w
		}
		if w := actionWord(c.OnUpdate); w != "" {
			t += " ON UPDATE " + w
		}
		if c.Deferrable {
			t += " DEFERRABLE"
		}
		if c.NotEnforced {
			t += " NOT ENFORCED"
		}
		return t
	case schema.Check:
		t := "CHECK (" + schema.Deparse(c.Expr) + ")"
		if c.NotEnforced {
			t += " NOT ENFORCED"
		}
		return t
	case schema.Exclude:
		var elems []string
		for i, col := range c.Columns {
			op := ""
			if i < len(c.Operators) {
				op = c.Operators[i]
			}
			elems = append(elems, q(col)+" WITH "+op)
		}
		t := "EXCLUDE USING " + c.AccessMethod + " (" + strings.Join(elems, ", ") + ")"
		if c.Predicate != nil {
			t += " WHERE (" + schema.Deparse(c.Predicate) + ")"
		}
		return t
	}
	return ""
}

// keyList renders a PRIMARY KEY / UNIQUE column list, the last column WITHOUT OVERLAPS
// for a temporal key.
func keyList(c *schema.Constraint) string {
	if !c.WithoutOverlaps || len(c.Columns) == 0 {
		return qlist(c.Columns)
	}
	return qlist(c.Columns[:len(c.Columns)-1]) + ", " + q(c.Columns[len(c.Columns)-1]) + " WITHOUT OVERLAPS"
}

// periodList renders a FOREIGN KEY column list, the last column PERIOD for a temporal key.
func periodList(cols []string, period bool) string {
	if !period || len(cols) == 0 {
		return qlist(cols)
	}
	return qlist(cols[:len(cols)-1]) + ", PERIOD " + q(cols[len(cols)-1])
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

func ruleNode(rd schema.RuleDef) *pgparse.Node {
	return &pgparse.Node{Node: &pgparse.Node_RuleStmt{RuleStmt: rd.Stmt}}
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
