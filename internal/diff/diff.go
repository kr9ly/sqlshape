// Package diff compares two schemas object by object and lists what it takes to turn one
// into the other. It reads what the loader holds of the database state and nothing else:
// `-- sqlshape:` directives, problems and analysis results are not schema.
//
// Expressions compare by their deparsed text, so the two sides should be in the same
// form: both loaded from schema text, or both canonical (dump.Canonical / dump.Load).
// A rename is not recognized: it shows as a drop and an add, and an intent declaration
// is the place to say otherwise.
package diff

import (
	"fmt"
	"sort"
	"strings"

	pg_query "github.com/pganalyze/pg_query_go/v6"

	"github.com/kr9ly/sqlshape/internal/catalog"
	"github.com/kr9ly/sqlshape/internal/schema"
)

// Op is what happens to an object.
type Op byte

const (
	Add   Op = '+'
	Drop  Op = '-'
	Alter Op = '~'
)

// Change is one object added, dropped or altered.
type Change struct {
	Op   Op
	Kind string // schema, extension, enum, domain, composite, range, table, view, matview, sequence, column, constraint, index, rule, function, trigger, comment
	Name string // qualified as the loader prints it: public omitted; column / constraint / index / rule as <relation>.<name>; trigger as <relation>.<name>
	// Fields are the properties that differ, for Alter.
	Fields []Field
}

// Field is one differing property.
type Field struct {
	Name     string
	From, To string
}

func (c Change) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%c %s %s", c.Op, c.Kind, c.Name)
	for _, f := range c.Fields {
		fmt.Fprintf(&b, "\n    %s: %s -> %s", f.Name, f.From, f.To)
	}
	return b.String()
}

// Compare lists the changes from a to b, sorted by kind order (dependencies first for
// additions: schemas, extensions, types, relations, their parts, functions, triggers,
// comments) then by name. Drops come in the same order; ordering for execution is the
// DDL generator's job.
func Compare(a, b *schema.Schema) []Change {
	d := &differ{}
	d.strings("schema", set(a.Schemas()), set(b.Schemas()))
	d.strings("extension", extensions(a), extensions(b))
	d.types(a, b)
	d.relations(a, b)
	d.functions(a, b)
	d.triggers(a, b)
	d.comments(a, b)
	return d.out
}

type differ struct{ out []Change }

func (d *differ) add(op Op, kind, name string, fields ...Field) {
	d.out = append(d.out, Change{Op: op, Kind: kind, Name: name, Fields: fields})
}

// props compares two property maps and records an Alter when they differ.
func (d *differ) props(kind, name string, from, to map[string]string) bool {
	var fields []Field
	for _, k := range sortedKeys(from) {
		if from[k] != to[k] {
			fields = append(fields, Field{Name: k, From: from[k], To: to[k]})
		}
	}
	for _, k := range sortedKeys(to) {
		if _, ok := from[k]; !ok {
			fields = append(fields, Field{Name: k, From: "", To: to[k]})
		}
	}
	if len(fields) == 0 {
		return false
	}
	d.add(Alter, kind, name, fields...)
	return true
}

// strings compares two name sets.
func (d *differ) strings(kind string, from, to map[string]bool) {
	for _, n := range sortedKeys(from) {
		if !to[n] {
			d.add(Drop, kind, n)
		}
	}
	for _, n := range sortedKeys(to) {
		if !from[n] {
			d.add(Add, kind, n)
		}
	}
}

// --- types ---------------------------------------------------------------------

type userType struct {
	kind  string
	props map[string]string
}

func userTypes(s *schema.Schema) map[string]userType {
	rels := map[catalog.OID]*schema.Relation{}
	for _, r := range s.Relations {
		rels[r.OID] = r
	}
	out := map[string]userType{}
	for _, t := range s.Types.User() {
		if t.Elem != 0 {
			continue // array types follow their element
		}
		name := t.Name
		if sch := s.Types.Schemas[t.OID]; sch != "" && sch != "public" {
			name = sch + "." + t.Name
		}
		switch t.Kind {
		case 'e':
			out[name] = userType{"enum", map[string]string{"labels": strings.Join(s.Types.Enums[t.OID], ", ")}}
		case 'd':
			dom := s.Types.Domains[t.OID]
			p := map[string]string{"base": s.Types.Format(schema.TypeRef{OID: t.BaseType, Typmod: t.Typmod}), "not null": fmt.Sprint(dom != nil && dom.NotNull)}
			if dom != nil {
				for _, c := range dom.Checks {
					p["check "+c.Name] = schema.Deparse(c.Expr)
				}
			}
			out[name] = userType{"domain", p}
		case 'c':
			rel := rels[t.RelID]
			if rel == nil || rel.Kind != 'c' {
				continue // a relation's row type
			}
			p := map[string]string{}
			var order []string
			for _, c := range rel.Columns {
				order = append(order, c.Name)
				p["attribute "+c.Name] = s.Types.Format(c.Type) + collation(c.Collation)
			}
			p["attributes"] = strings.Join(order, ", ")
			out[name] = userType{"composite", p}
		case 'r':
			p := map[string]string{}
			if rng := s.Types.RangeOf(t.OID); rng != nil {
				p["subtype"] = s.Types.Format(schema.TypeRef{OID: rng.Subtype, Typmod: -1})
			}
			out[name] = userType{"range", p}
		}
	}
	return out
}

func (d *differ) types(a, b *schema.Schema) {
	from, to := userTypes(a), userTypes(b)
	for _, n := range sortedKeys(from) {
		t, ok := to[n]
		if !ok || t.kind != from[n].kind {
			d.add(Drop, from[n].kind, n)
		}
	}
	for _, n := range sortedKeys(to) {
		f, ok := from[n]
		if !ok || f.kind != to[n].kind {
			d.add(Add, to[n].kind, n)
			continue
		}
		d.props(f.kind, n, f.props, to[n].props)
	}
}

// --- relations -----------------------------------------------------------------

func relKind(r *schema.Relation) string {
	switch r.Kind {
	case schema.Table:
		return "table"
	case schema.View:
		return "view"
	case schema.MatView:
		return "matview"
	case schema.Sequence:
		return "sequence"
	}
	return ""
}

func relations(s *schema.Schema) map[string]*schema.Relation {
	out := map[string]*schema.Relation{}
	for _, r := range s.Relations {
		if r.Temp || relKind(r) == "" {
			continue
		}
		out[r.FullName()] = r
	}
	return out
}

func relProps(s *schema.Schema, r *schema.Relation) map[string]string {
	p := map[string]string{}
	var parents []string
	for _, par := range r.Parents {
		parents = append(parents, par.FullName())
	}
	if len(parents) > 0 {
		if r.IsPartition {
			p["partition of"] = strings.Join(parents, ", ")
		} else {
			p["inherits"] = strings.Join(parents, ", ")
		}
	}
	if len(r.PartKey) > 0 {
		p["partition key"] = strings.Join(r.PartKey, ", ")
	}
	if r.OfType != 0 {
		p["of type"] = s.Types.Format(schema.TypeRef{OID: r.OfType, Typmod: -1})
	}
	if r.Query != nil {
		p["query"] = schema.DeparseStmt(r.Query)
	}
	if r.Kind == schema.Sequence {
		p["owned by"] = r.OwnedBy
	}
	if r.Kind != schema.Sequence {
		p["columns"] = strings.Join(columnNames(r), ", ")
	}
	return p
}

// columnNames lists a relation's columns in order. A view's are its frozen output
// columns (the loader derives them from the query; Columns stays empty).
func columnNames(r *schema.Relation) []string {
	var out []string
	if r.Query != nil {
		for _, c := range r.Frozen {
			out = append(out, c.Name)
		}
		return out
	}
	for _, c := range r.Columns {
		out = append(out, c.Name)
	}
	return out
}

// columns is a relation's columns by name with their comparable properties.
func columns(s *schema.Schema, r *schema.Relation) map[string]map[string]string {
	out := map[string]map[string]string{}
	if r.Query != nil {
		for _, c := range r.Frozen {
			out[c.Name] = map[string]string{"type": s.Types.Format(c.Type) + collation(c.Collation)}
		}
		return out
	}
	for _, c := range r.Columns {
		out[c.Name] = colProps(s, c)
	}
	return out
}

func colProps(s *schema.Schema, c *schema.Column) map[string]string {
	p := map[string]string{
		"type":     s.Types.Format(c.Type) + collation(c.Collation),
		"not null": fmt.Sprint(c.NotNull),
	}
	if c.Default != nil {
		p["default"] = schema.Deparse(c.Default)
	}
	if c.Identity != 0 {
		p["identity"] = string(c.Identity)
	}
	if c.Generated != nil {
		p["generated"] = schema.Deparse(c.Generated)
	}
	return p
}

func conProps(c *schema.Constraint) map[string]string {
	p := map[string]string{}
	switch c.Kind {
	case schema.PrimaryKey:
		p["primary key"] = strings.Join(c.Columns, ", ")
	case schema.Unique:
		p["unique"] = strings.Join(c.Columns, ", ")
		if c.NullsNotDistinct {
			p["nulls not distinct"] = "true"
		}
	case schema.ForeignKey:
		p["foreign key"] = strings.Join(c.Columns, ", ")
		p["references"] = c.RefTable + " (" + strings.Join(c.RefColumns, ", ") + ")"
		if c.OnDelete != 0 && c.OnDelete != 'a' {
			p["on delete"] = string(c.OnDelete)
		}
		if c.OnUpdate != 0 && c.OnUpdate != 'a' {
			p["on update"] = string(c.OnUpdate)
		}
	case schema.Check:
		p["check"] = schema.Deparse(c.Expr)
	}
	if c.Deferrable {
		p["deferrable"] = "true"
	}
	return p
}

func idxProps(i *schema.Index) map[string]string {
	p := map[string]string{"columns": strings.Join(i.Columns, ", "), "unique": fmt.Sprint(i.Unique)}
	if i.Predicate != nil {
		p["where"] = schema.Deparse(i.Predicate)
	}
	return p
}

func ruleProps(r schema.RuleDef) map[string]string {
	return map[string]string{"definition": schema.DeparseStmt(&pg_query.Node{Node: &pg_query.Node_RuleStmt{RuleStmt: r.Stmt}}), "enabled": fmt.Sprint(r.Enabled)}
}

func (d *differ) relations(a, b *schema.Schema) {
	from, to := relations(a), relations(b)
	for _, n := range sortedKeys(from) {
		if r, ok := to[n]; !ok || relKind(r) != relKind(from[n]) {
			d.add(Drop, relKind(from[n]), n)
		}
	}
	for _, n := range sortedKeys(to) {
		r := to[n]
		f, ok := from[n]
		if !ok || relKind(f) != relKind(r) {
			d.add(Add, relKind(r), n)
			continue
		}
		d.props(relKind(r), n, relProps(a, f), relProps(b, r))
		if r.Kind == schema.Sequence {
			continue
		}
		d.parts("column", n, columns(a, f), columns(b, r))
		// constraints, indexes, rules
		fcon, tcon := constraints(f), constraints(r)
		d.parts("constraint", n, fcon, tcon)
		fidx, tidx := map[string]map[string]string{}, map[string]map[string]string{}
		for _, i := range f.Indexes {
			fidx[i.Name] = idxProps(i)
		}
		for _, i := range r.Indexes {
			tidx[i.Name] = idxProps(i)
		}
		d.parts("index", n, fidx, tidx)
		frule, trule := map[string]map[string]string{}, map[string]map[string]string{}
		for rn, rd := range f.Rules() {
			frule[rn] = ruleProps(rd)
		}
		for rn, rd := range r.Rules() {
			trule[rn] = ruleProps(rd)
		}
		d.parts("rule", n, frule, trule)
	}
}

// constraints are the relation's constraints by name, without the Unique entries the
// loader adds to mirror a unique index (those are the index, listed as such).
func constraints(r *schema.Relation) map[string]map[string]string {
	idx := map[string]bool{}
	for _, i := range r.Indexes {
		idx[i.Name] = true
	}
	out := map[string]map[string]string{}
	for _, c := range r.Constraints {
		if c.Kind == schema.Unique && idx[c.Name] {
			continue
		}
		out[c.Name] = conProps(c)
	}
	return out
}

// parts diffs named sub-objects of a relation.
func (d *differ) parts(kind, rel string, from, to map[string]map[string]string) {
	for _, n := range sortedKeys(from) {
		if to[n] == nil {
			d.add(Drop, kind, rel+"."+n)
		}
	}
	for _, n := range sortedKeys(to) {
		if from[n] == nil {
			d.add(Add, kind, rel+"."+n)
			continue
		}
		d.props(kind, rel+"."+n, from[n], to[n])
	}
}

// --- functions / triggers / comments -------------------------------------------

// Signature is how a function is named: schema.name(input types).
func Signature(s *schema.Schema, f *schema.Function) string {
	var ins []string
	for _, a := range f.Args {
		if a.Mode == 'o' || a.Mode == 't' {
			continue
		}
		ins = append(ins, s.Types.Format(a.Type))
	}
	name := f.Name
	if f.Schema != "" && f.Schema != "public" {
		name = f.Schema + "." + f.Name
	}
	return name + "(" + strings.Join(ins, ", ") + ")"
}

func fnProps(s *schema.Schema, f *schema.Function) map[string]string {
	p := map[string]string{
		"returns":  s.Types.Format(f.RetType),
		"language": f.Language,
		"body":     strings.TrimSpace(f.Body),
	}
	if f.SQLBody != nil {
		// a SQL-standard body (BEGIN ATOMIC / RETURN) is held parsed
		p["body"] = schema.DeparseBody(f.SQLBody)
	}
	if f.RetSet {
		p["returns"] = "setof " + p["returns"]
	}
	if f.IsProc {
		p["procedure"] = "true"
	}
	if f.Volatile != 0 {
		p["volatility"] = string(f.Volatile)
	}
	if f.Strict {
		p["strict"] = "true"
	}
	if f.IsAgg {
		p["aggregate"] = "true"
	}
	if f.IsWindow {
		p["window"] = "true"
	}
	var args []string
	for _, a := range f.Args {
		arg := s.Types.Format(a.Type)
		if a.Name != "" {
			arg = a.Name + " " + arg
		}
		if a.Mode != 0 && a.Mode != 'i' {
			arg = string(a.Mode) + " " + arg
		}
		if a.HasDefault {
			arg += " default"
		}
		args = append(args, arg)
	}
	p["arguments"] = strings.Join(args, ", ")
	return p
}

func (d *differ) functions(a, b *schema.Schema) {
	from, to := map[string]*schema.Function{}, map[string]*schema.Function{}
	for _, f := range a.Functions {
		from[Signature(a, f)] = f
	}
	for _, f := range b.Functions {
		to[Signature(b, f)] = f
	}
	for _, n := range sortedKeys(from) {
		if to[n] == nil {
			d.add(Drop, "function", n)
		}
	}
	for _, n := range sortedKeys(to) {
		if from[n] == nil {
			d.add(Add, "function", n)
			continue
		}
		d.props("function", n, fnProps(a, from[n]), fnProps(b, to[n]))
	}
}

func trgProps(t *schema.Trigger) map[string]string {
	var ev []string
	if t.Insert {
		ev = append(ev, "insert")
	}
	if t.Update {
		if len(t.UpdateOf) > 0 {
			ev = append(ev, "update of "+strings.Join(t.UpdateOf, ", "))
		} else {
			ev = append(ev, "update")
		}
	}
	if t.Delete {
		ev = append(ev, "delete")
	}
	return map[string]string{"events": strings.Join(ev, " or "), "function": t.Function}
}

func (d *differ) triggers(a, b *schema.Schema) {
	from, to := map[string]*schema.Trigger{}, map[string]*schema.Trigger{}
	for _, t := range a.Triggers {
		from[t.Table+"."+t.Name] = t
	}
	for _, t := range b.Triggers {
		to[t.Table+"."+t.Name] = t
	}
	for _, n := range sortedKeys(from) {
		if to[n] == nil {
			d.add(Drop, "trigger", n)
		}
	}
	for _, n := range sortedKeys(to) {
		if from[n] == nil {
			d.add(Add, "trigger", n)
			continue
		}
		d.props("trigger", n, trgProps(from[n]), trgProps(to[n]))
	}
}

func (d *differ) comments(a, b *schema.Schema) {
	for _, k := range sortedKeys(a.Comments) {
		if _, ok := b.Comments[k]; !ok {
			d.add(Drop, "comment", k)
		}
	}
	for _, k := range sortedKeys(b.Comments) {
		v, ok := a.Comments[k]
		if !ok {
			d.add(Add, "comment", k)
		} else if v != b.Comments[k] {
			d.add(Alter, "comment", k, Field{Name: "text", From: v, To: b.Comments[k]})
		}
	}
}

// --- helpers -------------------------------------------------------------------

func extensions(s *schema.Schema) map[string]bool {
	out := map[string]bool{}
	for _, e := range s.Catalog.Extensions {
		out[e.Name] = true
	}
	return out
}

func collation(c string) string {
	if c == "" {
		return ""
	}
	return " collate " + c
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
