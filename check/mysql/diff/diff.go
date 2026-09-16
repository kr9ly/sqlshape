// Package diff compares two MySQL schemas object by object and lists what it takes to turn
// one into the other. It reads what the loader holds of the database state and nothing
// else: `-- sqlshape:` directives, problems and analysis results are not schema.
//
// Definitions compare by their text, so the two sides should be in the same form: both
// canonical (dump.Canonical / dump.Load), where the text is the server's own rendering. A
// rename is not recognized: it shows as a drop and an add, and an intent declaration is the
// place to say otherwise.
package diff

import (
	"fmt"
	"sort"
	"strings"

	"github.com/kr9ly/sqlshape/check/mysql/v2/internal/schema"
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
	Kind string // table, view, column, key, foreign key, check
	Name string // table / view by name; column / key / foreign key / check as <table>.<name>
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

// Retypes reports whether the change gives a column another type (what a statement reading
// the column may no longer fit).
func (c Change) Retypes() bool {
	if c.Op != Alter || c.Kind != "column" {
		return false
	}
	for _, f := range c.Fields {
		if f.Name == "type" {
			return true
		}
	}
	return false
}

// Compare lists the changes from a to b.
func Compare(a, b *schema.Schema) []Change {
	d := &differ{}
	d.tables(a, b)
	d.routines(a, b, schema.Procedure, "procedure")
	d.routines(a, b, schema.Function, "function")
	d.views(a, b)
	d.triggers(a, b)
	d.events(a, b)
	return d.out
}

type differ struct{ out []Change }

func (d *differ) add(op Op, kind, name string, fields ...Field) {
	d.out = append(d.out, Change{Op: op, Kind: kind, Name: name, Fields: fields})
}

// props reports the differing properties of one object as an Alter, and whether there were any.
func (d *differ) props(kind, name string, from, to map[string]string) bool {
	var fields []Field
	for _, k := range sortedKeys(from, to) {
		if from[k] != to[k] {
			fields = append(fields, Field{Name: k, From: from[k], To: to[k]})
		}
	}
	if len(fields) == 0 {
		return false
	}
	d.add(Alter, kind, name, fields...)
	return true
}

// parts compares the named parts of a table (columns, keys, ...): added, dropped, altered.
func (d *differ) parts(kind, table string, from, to map[string]map[string]string) {
	for _, k := range sortedKeys(from, to) {
		f, inFrom := from[k]
		t, inTo := to[k]
		switch {
		case inFrom && !inTo:
			d.add(Drop, kind, table+"."+k)
		case !inFrom && inTo:
			d.add(Add, kind, table+"."+k)
		default:
			d.props(kind, table+"."+k, f, t)
		}
	}
}

func tables(s *schema.Schema) map[string]*schema.Table {
	out := map[string]*schema.Table{}
	for _, t := range s.Tables {
		out[t.Name] = t
	}
	return out
}

func (d *differ) tables(a, b *schema.Schema) {
	from, to := tables(a), tables(b)
	for _, name := range sortedKeys(from, to) {
		f, inFrom := from[name]
		t, inTo := to[name]
		switch {
		case inFrom && !inTo:
			d.add(Drop, "table", name)
		case !inFrom && inTo:
			d.add(Add, "table", name)
		default:
			d.props("table", name, TableProps(f), TableProps(t))
			d.parts("column", name, columns(f), columns(t))
			d.parts("key", name, keys(f), keys(t))
			d.parts("foreign key", name, foreignKeys(f), foreignKeys(t))
			d.parts("check", name, checks(f), checks(t))
		}
	}
}

func (d *differ) views(a, b *schema.Schema) {
	from, to := map[string]*schema.View{}, map[string]*schema.View{}
	for _, v := range a.Views {
		from[v.Name] = v
	}
	for _, v := range b.Views {
		to[v.Name] = v
	}
	for _, name := range sortedKeys(from, to) {
		f, inFrom := from[name]
		t, inTo := to[name]
		switch {
		case inFrom && !inTo:
			d.add(Drop, "view", name)
		case !inFrom && inTo:
			d.add(Add, "view", name)
		default:
			d.props("view", name, ViewProps(f), ViewProps(t))
		}
	}
}

// routines compares the procedures or functions (kind picks which; MySQL keeps them in
// separate namespaces, so a schema may have both a PROCEDURE and a FUNCTION of the same
// name and they are compared independently).
func (d *differ) routines(a, b *schema.Schema, kind schema.RoutineKind, label string) {
	from, to := map[string]*schema.Routine{}, map[string]*schema.Routine{}
	for _, r := range a.Routines {
		if r.Kind == kind {
			from[r.Name] = r
		}
	}
	for _, r := range b.Routines {
		if r.Kind == kind {
			to[r.Name] = r
		}
	}
	for _, name := range sortedKeys(from, to) {
		f, inFrom := from[name]
		t, inTo := to[name]
		switch {
		case inFrom && !inTo:
			d.add(Drop, label, name)
		case !inFrom && inTo:
			d.add(Add, label, name)
		default:
			d.props(label, name, RoutineProps(f), RoutineProps(t))
		}
	}
}

func (d *differ) triggers(a, b *schema.Schema) {
	from, to := map[string]*schema.Trigger{}, map[string]*schema.Trigger{}
	for _, tr := range a.Triggers {
		from[tr.Name] = tr
	}
	for _, tr := range b.Triggers {
		to[tr.Name] = tr
	}
	for _, name := range sortedKeys(from, to) {
		f, inFrom := from[name]
		t, inTo := to[name]
		switch {
		case inFrom && !inTo:
			d.add(Drop, "trigger", name)
		case !inFrom && inTo:
			d.add(Add, "trigger", name)
		default:
			d.props("trigger", name, TriggerProps(f), TriggerProps(t))
		}
	}
}

func (d *differ) events(a, b *schema.Schema) {
	from, to := map[string]*schema.Event{}, map[string]*schema.Event{}
	for _, e := range a.Events {
		from[e.Name] = e
	}
	for _, e := range b.Events {
		to[e.Name] = e
	}
	for _, name := range sortedKeys(from, to) {
		f, inFrom := from[name]
		t, inTo := to[name]
		switch {
		case inFrom && !inTo:
			d.add(Drop, "event", name)
		case !inFrom && inTo:
			d.add(Add, "event", name)
		default:
			fp, tp := eventProps(f, t)
			d.props("event", name, fp, tp)
		}
	}
}

// EventProps are an event's properties: its schedule, the times bounding it, its
// completion, status and comment, and its body text. A time the target did not fix (an
// omitted or computed STARTS / AT / ENDS, Event.*Literal false) is left out on both sides:
// the server filled it in when each side's event was created, and two creation times
// never agree.
func EventProps(e *schema.Event) map[string]string {
	schedule := e.Every
	if e.At != "" {
		schedule = "AT"
	} else {
		schedule = "EVERY " + schedule
	}
	return map[string]string{
		"schedule":      schedule,
		"at":            e.At,
		"starts":        e.Starts,
		"ends":          e.Ends,
		"on completion": e.Completion,
		"status":        e.Status,
		"comment":       e.Comment,
		"body":          e.BodyText,
	}
}

// eventProps is EventProps of both sides, the times the target does not fix removed from
// each.
func eventProps(from, to *schema.Event) (map[string]string, map[string]string) {
	fp, tp := EventProps(from), EventProps(to)
	for _, u := range []struct {
		key string
		lit bool
	}{{"at", to.AtLiteral}, {"starts", to.StartsLiteral}, {"ends", to.EndsLiteral}} {
		if !u.lit {
			delete(fp, u.key)
			delete(tp, u.key)
		}
	}
	return fp, tp
}

// EventChanged reports whether the event as from has it differs from the target's to, by
// the same comparison Compare makes (the times to does not fix left out).
func EventChanged(from, to *schema.Event) bool {
	fp, tp := eventProps(from, to)
	for _, k := range sortedKeys(fp, tp) {
		if fp[k] != tp[k] {
			return true
		}
	}
	return false
}

// TableProps are a table's own properties: the table options.
func TableProps(t *schema.Table) map[string]string {
	return map[string]string{
		"engine":      t.Engine,
		"charset":     t.Charset,
		"collation":   t.Collation,
		"rowFormat":   t.RowFormat,
		"comment":     t.Comment,
		"partitioned": PartitioningProps(t.Partitioning),
	}
}

// PartitioningProps is a table's PARTITION BY clause, canonicalized to one comparable
// string: "" for an unpartitioned table (nil), Partitioning.Text for a clause too complex
// for this package's Kind to say more about (RANGE/LIST COLUMNS, KEY, LINEAR, LIST,
// subpartitions), else the pieces alterTable itself decides DDL from -- kind, expression
// and, in order, every partition's own name and boundary -- so two clauses this package
// tells apart the same way (an ADD PARTITION reordering nothing, say) never look changed
// on account of some detail alterTable does not look at either.
func PartitioningProps(p *schema.Partitioning) string {
	if p == nil {
		return ""
	}
	if p.Kind == "" {
		return p.Text
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s (%s)", p.Kind, p.Expr)
	if p.Kind == "HASH" {
		fmt.Fprintf(&b, " PARTITIONS %d", p.Num)
		return b.String()
	}
	for _, part := range p.Parts {
		b.WriteString(" " + part.Name + ":")
		if part.MaxValue {
			b.WriteString("MAXVALUE")
		} else {
			b.WriteString(part.Bound)
		}
	}
	return b.String()
}

// ColumnProps are a column's properties: its type, its whole definition as the server
// spells it (nullability, default, attributes and comment included), and its position.
func ColumnProps(t *schema.Table, c *schema.Column) map[string]string {
	pos := "first"
	for i, x := range t.Columns {
		if x == c && i > 0 {
			pos = "after " + t.Columns[i-1].Name
		}
	}
	return map[string]string{
		"type":       c.Type.String(),
		"definition": Definition(c.Text, c.Name),
		"position":   pos,
	}
}

// Definition is an element's definition text without its leading name (`\x60id\x60 bigint
// NOT NULL` -> `bigint NOT NULL`), so a renamed element compares by what it is.
func Definition(text, name string) string {
	text = strings.TrimSpace(text)
	for _, prefix := range []string{"`" + strings.ReplaceAll(name, "`", "``") + "`", name} {
		if strings.HasPrefix(text, prefix) {
			text = strings.TrimSpace(text[len(prefix):])
			break
		}
	}
	return stripRedundantCharset(text)
}

// stripRedundantCharset removes an explicit "CHARACTER SET x" clause from a definition that
// also carries an explicit COLLATE: a collation names its charset uniquely in MySQL, so the
// two say the same thing, and canonicalizing is not otherwise idempotent for an ENUM / SET
// column under a table with its own DEFAULT CHARSET / COLLATE -- freshly creating one from
// raw declarative SQL omits CHARACTER SET (COLLATE alone, matching the table default), but
// reading the same column back from an existing table (SHOW CREATE TABLE, canonicalizing a
// second time) always spells both (measured against mysqld 8.4), so a plan comparing the two
// spellings of the same column would see one as changed forever. Definitions with and
// without the clause must compare equal; ColumnProps / alterTable's own MODIFY-COLUMN check
// both go through Definition, so this covers both diff.Compare and the plan itself.
func stripRedundantCharset(def string) string {
	if !strings.Contains(def, "COLLATE ") {
		return def
	}
	const marker = "CHARACTER SET "
	i := strings.Index(def, marker)
	if i < 0 {
		return def
	}
	j := i + len(marker)
	for j < len(def) && def[j] != ' ' {
		j++
	}
	if j >= len(def) {
		return def[:i]
	}
	return def[:i] + def[j+1:]
}

func columns(t *schema.Table) map[string]map[string]string {
	out := map[string]map[string]string{}
	for _, c := range t.Columns {
		out[c.Name] = ColumnProps(t, c)
	}
	return out
}

// KeyProps are a key's properties: its definition as the server spells it (kind, columns
// with prefix lengths and order, options), rendered from the model when no text is held.
func KeyProps(k *schema.Key) map[string]string {
	def := k.Text
	if def == "" {
		def = RenderKey(k)
	}
	return map[string]string{"definition": stripKeyName(def, k)}
}

// stripKeyName removes the key's name from its definition text, leaving the kind and the
// columns: `UNIQUE KEY \x60k\x60 (\x60a\x60)` -> `UNIQUE KEY (\x60a\x60)`.
func stripKeyName(def string, k *schema.Key) string {
	if k.Kind == schema.Primary {
		return def
	}
	return strings.Replace(def, " `"+k.Name+"` ", " ", 1)
}

// RenderKey spells a key the way SHOW CREATE TABLE does.
func RenderKey(k *schema.Key) string {
	var b strings.Builder
	switch k.Kind {
	case schema.Primary:
		b.WriteString("PRIMARY KEY")
	case schema.Unique:
		b.WriteString("UNIQUE KEY `" + k.Name + "`")
	case schema.Fulltext:
		b.WriteString("FULLTEXT KEY `" + k.Name + "`")
	case schema.Spatial:
		b.WriteString("SPATIAL KEY `" + k.Name + "`")
	default:
		b.WriteString("KEY `" + k.Name + "`")
	}
	b.WriteString(" (")
	for i, p := range k.Parts {
		if i > 0 {
			b.WriteString(",")
		}
		if p.Expr != nil && p.Column == "" {
			b.WriteString("(<expression>)")
		} else {
			b.WriteString("`" + p.Column + "`")
		}
		if p.Length > 0 {
			fmt.Fprintf(&b, "(%d)", p.Length)
		}
		if p.Desc {
			b.WriteString(" DESC")
		}
	}
	b.WriteString(")")
	if k.Invisible {
		b.WriteString(" /*!80000 INVISIBLE */")
	}
	if k.Comment != "" {
		b.WriteString(" COMMENT '" + strings.ReplaceAll(k.Comment, "'", "''") + "'")
	}
	return b.String()
}

func keys(t *schema.Table) map[string]map[string]string {
	out := map[string]map[string]string{}
	for _, k := range t.Keys {
		name := k.Name
		if k.Kind == schema.Primary {
			name = "PRIMARY"
		}
		out[name] = KeyProps(k)
	}
	return out
}

// ForeignKeyProps are a foreign key's properties: its columns, what they reference, and the
// actions.
func ForeignKeyProps(fk *schema.ForeignKey) map[string]string {
	return map[string]string{
		"columns":    strings.Join(fk.Columns, ", "),
		"references": fk.RefTable + " (" + strings.Join(fk.RefColumns, ", ") + ")",
		"on delete":  fk.OnDelete,
		"on update":  fk.OnUpdate,
	}
}

func foreignKeys(t *schema.Table) map[string]map[string]string {
	out := map[string]map[string]string{}
	for _, fk := range t.ForeignKeys {
		out[t.ForeignKeyName(fk)] = ForeignKeyProps(fk)
	}
	return out
}

// CheckProps are a check constraint's properties: its expression as the server spells it
// and whether it is enforced.
func CheckProps(t *schema.Table, c *schema.Check) map[string]string {
	expr := c.Text
	if i := strings.Index(expr, "CHECK"); i >= 0 {
		expr = strings.TrimSpace(expr[i+len("CHECK"):])
	}
	if j := strings.Index(expr, "/*!80016 NOT ENFORCED */"); j >= 0 {
		expr = strings.TrimSpace(expr[:j])
	} else if j := strings.LastIndex(expr, " NOT ENFORCED"); j >= 0 {
		expr = strings.TrimSpace(expr[:j])
	}
	return map[string]string{"expression": expr, "enforced": fmt.Sprint(c.Enforced)}
}

func checks(t *schema.Table) map[string]map[string]string {
	out := map[string]map[string]string{}
	for _, c := range t.Checks {
		out[t.CheckName(c)] = CheckProps(t, c)
	}
	return out
}

// ViewProps are a view's properties: its definition as the server spells it.
func ViewProps(v *schema.View) map[string]string {
	def := strings.TrimSpace(v.Definition)
	def = strings.TrimPrefix(def, "CREATE ")
	def = strings.TrimPrefix(def, "OR REPLACE ")
	return map[string]string{"definition": def}
}

// RoutineProps are a procedure's or function's properties: its definition as the server
// spells it (its parameters, RETURNS, characteristics and body). Two routines with the same
// name in different kinds (a PROCEDURE and a FUNCTION) are never compared against each
// other; Compare keeps them apart by kind.
func RoutineProps(r *schema.Routine) map[string]string {
	def := strings.TrimSpace(r.Definition)
	def = strings.TrimPrefix(def, "CREATE ")
	return map[string]string{"definition": def}
}

// TriggerProps are a trigger's properties: its definition as the server spells it (the
// timing, event, table, FOLLOWS/PRECEDES ordering and body). A table rename changes the
// `ON <table>` a trigger's definition names, so a trigger whose table was renamed compares
// as changed even though its body did not.
func TriggerProps(t *schema.Trigger) map[string]string {
	def := strings.TrimSpace(t.Definition)
	def = strings.TrimPrefix(def, "CREATE ")
	return map[string]string{"definition": def}
}

func sortedKeys[V any](maps ...map[string]V) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range maps {
		for k := range m {
			if !seen[k] {
				seen[k] = true
				out = append(out, k)
			}
		}
	}
	sort.Strings(out)
	return out
}
