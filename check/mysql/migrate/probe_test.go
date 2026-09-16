package migrate

// The migrate probe: an oracle for Plan that a hand-written adversarial round cannot be. The
// second adversarial round's migrate lane found six problems, all of one class -- the order
// the plan's DDL runs in against a real server -- and a hand-picked case only ever catches
// the one instance its author thought of. This probe generates schemas from the vocabulary
// the planner handles (column types, generated columns, keys, foreign keys, AUTO_INCREMENT,
// CHECKs, views, triggers, functions), mutates each into a target (columns added / dropped /
// renamed / retyped / moved, keys and foreign keys attached and detached, the primary key
// moved, tables added / dropped / renamed, ENUM labels, views, triggers, functions), and
// judges the plan the way `sqlshape apply` would live: the DDL runs on a server holding the
// source, the result must read back as the target's canonical form, and a second Plan from
// there must be empty. Every pair is a change a real mysqld accepts written as one schema
// (the target canonicalizes cleanly before the plan is judged), so a refused statement or a
// leftover difference is the planner's.
//
//	go test ./migrate -run TestMigrateProbe [-migrate-probe-n 200] [-migrate-probe-seed 1] \
//	    [-migrate-probe-report /path/report.md]
//
// A failing pair is minimized (mutations removed while the failure stands) and written to
// the report with its source, target, DDL and what went wrong; the test fails on any finding.
// Skipped without a mysqld on PATH (nix-shell -p mysql84).

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/kr9ly/sqlshape/check/mysql/v2/diff"
	"github.com/kr9ly/sqlshape/check/mysql/v2/dump"
	"github.com/kr9ly/sqlshape/mysqltest/v2"
)

var (
	probeN      = flag.Int("migrate-probe-n", 200, "schema pairs the migrate probe judges")
	probeSeed   = flag.Int64("migrate-probe-seed", 1, "seed of the migrate probe's generator")
	probeReport = flag.String("migrate-probe-report", "", "write the migrate probe's findings here (default: the test log)")
)

// ---- the model: a schema the generator can render and mutate --------------------------

type pSchema struct {
	tables   []*pTable
	views    []*pView
	triggers []*pTrigger
	funcs    []*pFunc
	procs    []*pProc
	events   []*pEvent
	intents  []string // `-- @migrate` lines the mutations declared (target side only)
	seq      int      // name counter
	mutating bool     // set on a clone: the tables that exist are the source's, and hold rows
}

type pTable struct {
	name    string
	orig    string // the name the source schema knows the table by (a declaration's left side)
	cols    []*pCol
	keys    []*pKey
	fks     []*pFK
	checks  []*pCheck
	comment string
	autoInc int // AUTO_INCREMENT=<n>, 0 for none
	// charset / collation: "" for the server's own default (utf8mb4 / utf8mb4_0900_ai_ci,
	// not written); rowFormat: "" for none, else DYNAMIC / COMPRESSED.
	charset, collation, rowFormat string
	// engine: "" for InnoDB (the default everywhere else in this generator), "MyISAM"
	// for the one mutation that exercises "~ table engine" (diff.Alphabet) -- guarded to
	// tables with no foreign key on either side, since MyISAM does not support them.
	engine string
	// partition: nil for an unpartitioned table. Every partition mutation guards its
	// candidates to a table with no foreign key on either side and no UNIQUE key besides
	// its own PRIMARY KEY (every UNIQUE key, PRIMARY included, must carry the partitioning
	// column -- Error 1503, measured -- and this generator always partitions by the id
	// column, so a plain UNIQUE elsewhere would refuse it).
	partition *pPartitioning
}

// pPartitioning is a table's PARTITION BY clause: RANGE (id), an ordered list of parts,
// HASH (id) PARTITIONS n, or LIST (id), an unordered list of parts (each own value set, not
// a boundary). Always over the id column (see pTable.partition's own comment).
type pPartitioning struct {
	kind  string  // "RANGE", "HASH" or "LIST"
	num   int     // HASH's PARTITIONS n
	parts []pPart // RANGE's ordered partitions, or LIST's own (order is cosmetic there)
}

// pPart is one partition of a RANGE pPartitioning (`VALUES LESS THAN (bound)`, or
// `VALUES LESS THAN MAXVALUE` when maxValue -- only the last partition may say this) or of a
// LIST pPartitioning (`VALUES IN (values...)`, always over the id column's own domain --
// rows 1..3, or a value this schema's rows never take, so a mutation adding or moving one
// never needs a declaration of its own the way dropping one still does).
type pPart struct {
	name     string
	maxValue bool
	bound    int
	values   []int // LIST's own VALUES IN, nil for RANGE
}

type pCol struct {
	name    string
	typ     string // as rendered: int, bigint unsigned, varchar(50), ...
	labels  []string
	notNull bool
	def     string // default expression text, "" for none
	pk      bool
	auto    bool
	gen     string // the column this generated column reads ("" for a plain column)
	stored  bool
	// collate: a varchar / text column declared COLLATE utf8mb4_bin; invisible: INVISIBLE;
	// onUpdate: a datetime(6) column with ON UPDATE CURRENT_TIMESTAMP(6)
	collate   bool
	invisible bool
	onUpdate  bool
	comment   string // COMMENT '...', "" for none
	fresh     bool   // added by a mutation: not in the source, so its drop needs no declaration
}

type pKey struct {
	name   string
	unique bool
	cols   []string
	prefix []int // one per column: a prefix length, 0 for the whole column
	// special is "" for a plain (or unique) KEY, "FULLTEXT" or "SPATIAL" for one of those
	// kinds; unique is meaningless when special is set (neither takes UNIQUE).
	special string
	desc    []bool   // one per column: DESC ordering
	expr    []string // one per column: "" for the plain column, "plus1" / "lower" for a
	// functional part over it (keyCols renders the expression; cols still holds the real
	// column it reads, for the bookkeeping every other mutation already does by name)
	invisible bool
}

type pFK struct {
	name     string
	cols     []string
	refTable string
	refCols  []string
	onDelete string
	onUpdate string
}

type pCheck struct {
	name string
	col  string
	// op is the comparison ("" defaults to ">"); enforced false renders NOT ENFORCED.
	op       string
	enforced bool
}

type pView struct {
	name  string
	table string
	cols  []string
}

type pTrigger struct {
	name  string
	table string
	col   string // the column its body reads and sets (NEW.col)
	n     int    // the constant the body adds (a body change is a different n)
}

type pFunc struct {
	name string
	n    int
}

// pProc is a CREATE PROCEDURE, the same shape as pFunc (a single SELECT body a mutation
// changes by n) but its own Kind ("procedure", never "function") in diff.Compare.
type pProc struct {
	name string
	n    int
}

// pEvent is a CREATE EVENT with a schedule of n hours (or, at.Set, a one-time AT) and a
// body that sets a user variable. starts / ends / at hold a literal datetime text (so the
// server stores it as written, AtLiteral / StartsLiteral / EndsLiteral true -- see
// schema.Event's own doc comment for why that is what diff.Compare ever looks at), "" for
// none; completion is "" (the default, NOT PRESERVE) or "PRESERVE"; status is "" (the
// default, ENABLE) or "DISABLE".
type pEvent struct {
	name               string
	n                  int
	at, starts, ends   string
	completion, status string
	comment            string
	// fresh: added by a mutation within the same recipe, not the source -- a step that
	// only sets one of the fields above on it produces a whole "+ event", never the
	// per-field "~ event ..." the toggle mutations below exist to exercise, so they skip it.
	fresh bool
}

func (s *pSchema) next(prefix string) string {
	s.seq++
	return fmt.Sprintf("%s%d", prefix, s.seq)
}

func (s *pSchema) clone() *pSchema {
	c := &pSchema{seq: s.seq}
	for _, t := range s.tables {
		nt := &pTable{name: t.name, orig: t.orig, comment: t.comment, autoInc: t.autoInc,
			charset: t.charset, collation: t.collation, rowFormat: t.rowFormat, engine: t.engine}
		if t.partition != nil {
			np := *t.partition
			np.parts = make([]pPart, len(t.partition.parts))
			for i, part := range t.partition.parts {
				np.parts[i] = part
				np.parts[i].values = append([]int(nil), part.values...)
			}
			nt.partition = &np
		}
		for _, col := range t.cols {
			nc := *col
			nc.labels = append([]string(nil), col.labels...)
			nt.cols = append(nt.cols, &nc)
		}
		for _, k := range t.keys {
			nk := *k
			nk.cols = append([]string(nil), k.cols...)
			nk.prefix = append([]int(nil), k.prefix...)
			nk.desc = append([]bool(nil), k.desc...)
			nk.expr = append([]string(nil), k.expr...)
			nt.keys = append(nt.keys, &nk)
		}
		for _, fk := range t.fks {
			nfk := *fk
			nfk.cols = append([]string(nil), fk.cols...)
			nfk.refCols = append([]string(nil), fk.refCols...)
			nt.fks = append(nt.fks, &nfk)
		}
		for _, ck := range t.checks {
			nck := *ck
			nt.checks = append(nt.checks, &nck)
		}
		c.tables = append(c.tables, nt)
	}
	for _, v := range s.views {
		nv := *v
		nv.cols = append([]string(nil), v.cols...)
		c.views = append(c.views, &nv)
	}
	for _, tr := range s.triggers {
		ntr := *tr
		c.triggers = append(c.triggers, &ntr)
	}
	for _, f := range s.funcs {
		nf := *f
		c.funcs = append(c.funcs, &nf)
	}
	for _, p := range s.procs {
		np := *p
		c.procs = append(c.procs, &np)
	}
	for _, e := range s.events {
		ne := *e
		c.events = append(c.events, &ne)
	}
	return c
}

func (s *pSchema) table(name string) *pTable {
	for _, t := range s.tables {
		if t.name == name {
			return t
		}
	}
	return nil
}

func (t *pTable) col(name string) *pCol {
	for _, c := range t.cols {
		if c.name == name {
			return c
		}
	}
	return nil
}

// inFK: a CHECK may not read a column a foreign key's referential action writes (Error 3823).
func (t *pTable) inFK(col string) bool {
	for _, fk := range t.fks {
		if indexOf(fk.cols, col) >= 0 {
			return true
		}
	}
	return false
}

func (t *pTable) pk() *pCol {
	for _, c := range t.cols {
		if c.pk {
			return c
		}
	}
	return nil
}

// ---- the type vocabulary --------------------------------------------------------------

var probeTypes = []string{"int", "bigint unsigned", "varchar(50)", "decimal(10,2)", "datetime(6)", "date", "text", "enum", "json", "tinyint(1)", "smallint", "double", "point"}

func isNumeric(typ string) bool {
	switch {
	case strings.HasPrefix(typ, "int"), strings.HasPrefix(typ, "bigint"), strings.HasPrefix(typ, "decimal"),
		strings.HasPrefix(typ, "tinyint"), strings.HasPrefix(typ, "smallint"), typ == "double":
		return true
	}
	return false
}

func isInteger(typ string) bool {
	return isNumeric(typ) && !strings.HasPrefix(typ, "decimal") && typ != "double"
}

// keyable: a type a plain (non-SPATIAL) index takes without a prefix length. json takes no
// index at all; point takes only a SPATIAL index (addSpatialKey), never a plain one.
func keyable(typ string) bool { return typ != "text" && typ != "json" && typ != "point" }

func defaultFor(c *pCol) string {
	switch {
	case isInteger(c.typ):
		return "'1'"
	case strings.HasPrefix(c.typ, "decimal"):
		return "'1.00'"
	case c.typ == "double":
		return "'1'"
	case strings.HasPrefix(c.typ, "varchar"):
		return "'x'"
	case c.typ == "datetime(6)":
		return "CURRENT_TIMESTAMP(6)"
	case c.typ == "date":
		return "'2020-01-01'"
	case c.typ == "enum":
		return "'" + c.labels[0] + "'"
	case c.typ == "point":
		// unlike the other types here, point has no implicit "zero value" MySQL can fill
		// existing rows with when a fresh NOT NULL point column is added (Error 1138
		// "Invalid use of NULL value", measured): the others (including text / json, which
		// take no literal default either) all do, so only point needs an explicit one, an
		// expression default (MySQL 8.0.13+, DEFAULT (expr)) since it takes no literal.
		return "(ST_GeomFromText('POINT(0 0)', 4326))"
	}
	return "" // text / json take no literal default
}

func widen(typ string) string {
	switch typ {
	case "int":
		return "bigint"
	case "smallint", "tinyint(1)":
		return "int"
	case "varchar(50)":
		return "varchar(100)"
	case "decimal(10,2)":
		return "decimal(14,2)"
	case "bigint unsigned":
		return "decimal(20,0)"
	}
	return ""
}

// ---- rendering ------------------------------------------------------------------------

func (c *pCol) typeText() string {
	if c.typ == "enum" {
		quoted := make([]string, len(c.labels))
		for i, l := range c.labels {
			quoted[i] = "'" + l + "'"
		}
		return "enum(" + strings.Join(quoted, ",") + ")"
	}
	return c.typ
}

func (c *pCol) render() string {
	var b strings.Builder
	b.WriteString(q(c.name) + " " + c.typeText())
	if c.typ == "point" {
		// SRID is part of the spatial type's own clause, between the type and NOT NULL.
		b.WriteString(" SRID 4326")
	}
	if c.collate {
		b.WriteString(" COLLATE utf8mb4_bin")
	}
	if c.gen != "" {
		b.WriteString(" GENERATED ALWAYS AS ((" + q(c.gen) + " + 1))")
		if c.stored {
			b.WriteString(" STORED")
		} else {
			b.WriteString(" VIRTUAL")
		}
		if c.comment != "" {
			b.WriteString(" COMMENT " + lit(c.comment))
		}
		return b.String()
	}
	if c.notNull {
		b.WriteString(" NOT NULL")
	}
	if c.auto {
		b.WriteString(" AUTO_INCREMENT")
	} else if c.def != "" {
		b.WriteString(" DEFAULT " + c.def)
	}
	if c.onUpdate {
		b.WriteString(" ON UPDATE CURRENT_TIMESTAMP(6)")
	}
	if c.comment != "" {
		b.WriteString(" COMMENT " + lit(c.comment))
	}
	if c.invisible {
		b.WriteString(" INVISIBLE")
	}
	return b.String()
}

// keyCols renders a key's column list with its prefix lengths.
func (k *pKey) keyCols() string {
	parts := make([]string, len(k.cols))
	for i, c := range k.cols {
		kind := ""
		if i < len(k.expr) {
			kind = k.expr[i]
		}
		switch kind {
		case "plus1":
			parts[i] = "(" + q(c) + " + 1)"
		case "lower":
			parts[i] = "(lower(" + q(c) + "))"
		default:
			parts[i] = q(c)
			if i < len(k.prefix) && k.prefix[i] > 0 {
				parts[i] += fmt.Sprintf("(%d)", k.prefix[i])
			}
		}
		if i < len(k.desc) && k.desc[i] {
			parts[i] += " DESC"
		}
	}
	return strings.Join(parts, ",")
}

func (t *pTable) render() string {
	var parts []string
	for _, c := range t.cols {
		parts = append(parts, "  "+c.render())
	}
	if pk := t.pk(); pk != nil {
		parts = append(parts, "  PRIMARY KEY ("+q(pk.name)+")")
	}
	for _, k := range t.keys {
		kind := "KEY"
		switch {
		case k.special == "FULLTEXT":
			kind = "FULLTEXT KEY"
		case k.special == "SPATIAL":
			kind = "SPATIAL KEY"
		case k.unique:
			kind = "UNIQUE KEY"
		}
		s := "  " + kind + " " + q(k.name) + " (" + k.keyCols() + ")"
		if k.invisible {
			s += " INVISIBLE"
		}
		parts = append(parts, s)
	}
	for _, fk := range t.fks {
		s := "  CONSTRAINT " + q(fk.name) + " FOREIGN KEY (" + qlist(fk.cols) + ") REFERENCES " + q(fk.refTable) + " (" + qlist(fk.refCols) + ")"
		if fk.onDelete != "" {
			s += " ON DELETE " + fk.onDelete
		}
		if fk.onUpdate != "" {
			s += " ON UPDATE " + fk.onUpdate
		}
		parts = append(parts, s)
	}
	for _, ck := range t.checks {
		c := t.col(ck.col)
		op := ck.op
		if op == "" {
			op = ">"
		}
		expr := "(" + q(ck.col) + " " + op + " 0)"
		if !isNumeric(c.typ) {
			expr = "(char_length(" + q(ck.col) + ") " + op + " 0)"
		}
		s := "  CONSTRAINT " + q(ck.name) + " CHECK " + expr
		if !ck.enforced {
			s += " NOT ENFORCED"
		}
		parts = append(parts, s)
	}
	var b strings.Builder
	eng := t.engine
	if eng == "" {
		eng = "InnoDB"
	}
	b.WriteString("CREATE TABLE " + q(t.name) + " (\n" + strings.Join(parts, ",\n") + "\n) ENGINE=" + eng)
	if t.autoInc > 0 {
		fmt.Fprintf(&b, " AUTO_INCREMENT=%d", t.autoInc)
	}
	if t.charset != "" {
		b.WriteString(" DEFAULT CHARSET=" + t.charset)
	}
	if t.collation != "" {
		b.WriteString(" COLLATE=" + t.collation)
	}
	if t.rowFormat != "" {
		b.WriteString(" ROW_FORMAT=" + t.rowFormat)
	}
	if t.comment != "" {
		b.WriteString(" COMMENT=" + lit(t.comment))
	}
	if t.partition != nil {
		b.WriteString("\n" + t.partition.render(t.pk().name))
	}
	b.WriteString(";\n")
	return b.String()
}

// render spells p's clause, over col (always the table's id column -- see pTable.partition
// and pPartitioning's own doc comments for why).
func (p *pPartitioning) render(col string) string {
	if p.kind == "HASH" {
		return fmt.Sprintf("PARTITION BY HASH (%s) PARTITIONS %d", q(col), p.num)
	}
	if p.kind == "LIST" {
		defs := make([]string, len(p.parts))
		for i, part := range p.parts {
			defs[i] = fmt.Sprintf("PARTITION %s VALUES IN (%s)", q(part.name), intList(part.values))
		}
		return fmt.Sprintf("PARTITION BY LIST (%s)\n(%s)", q(col), strings.Join(defs, ",\n "))
	}
	defs := make([]string, len(p.parts))
	for i, part := range p.parts {
		if part.maxValue {
			defs[i] = "PARTITION " + q(part.name) + " VALUES LESS THAN MAXVALUE"
		} else {
			defs[i] = fmt.Sprintf("PARTITION %s VALUES LESS THAN (%d)", q(part.name), part.bound)
		}
	}
	return fmt.Sprintf("PARTITION BY RANGE (%s)\n(%s)", q(col), strings.Join(defs, ",\n "))
}

// intList renders a LIST partition's own value set, comma-separated.
func intList(values []int) string {
	strs := make([]string, len(values))
	for i, v := range values {
		strs[i] = fmt.Sprint(v)
	}
	return strings.Join(strs, ", ")
}

func (s *pSchema) render() string {
	var b strings.Builder
	b.WriteString("-- sqlshape: mysql 8.4\n")
	for _, in := range s.intents {
		b.WriteString(in + "\n")
	}
	for _, t := range s.tables {
		b.WriteString(t.render())
	}
	for _, v := range s.views {
		b.WriteString("CREATE VIEW " + q(v.name) + " AS SELECT " + qlist(v.cols) + " FROM " + q(v.table) + ";\n")
	}
	for _, f := range s.funcs {
		fmt.Fprintf(&b, "CREATE FUNCTION %s(a INT) RETURNS INT DETERMINISTIC RETURN a + %d;\n", q(f.name), f.n)
	}
	for _, p := range s.procs {
		fmt.Fprintf(&b, "CREATE PROCEDURE %s(IN a INT) BEGIN SELECT a + %d; END;\n", q(p.name), p.n)
	}
	for _, tr := range s.triggers {
		fmt.Fprintf(&b, "CREATE TRIGGER %s BEFORE INSERT ON %s FOR EACH ROW SET NEW.%s = COALESCE(NEW.%s, 0) + %d;\n",
			q(tr.name), q(tr.table), q(tr.col), q(tr.col), tr.n)
	}
	for _, e := range s.events {
		b.WriteString(e.render())
	}
	return b.String()
}

// render spells e's CREATE EVENT: a one-time AT schedule if e.at is set, else a recurring
// EVERY with an optional STARTS / ENDS, then the options every mutation here can toggle.
func (e *pEvent) render() string {
	var sched string
	if e.at != "" {
		sched = "AT '" + e.at + "'"
	} else {
		sched = fmt.Sprintf("EVERY %d HOUR", e.n)
		if e.starts != "" {
			sched += " STARTS '" + e.starts + "'"
		}
		if e.ends != "" {
			sched += " ENDS '" + e.ends + "'"
		}
	}
	var opts strings.Builder
	if e.completion != "" {
		opts.WriteString(" ON COMPLETION " + e.completion)
	}
	if e.status != "" {
		opts.WriteString(" " + e.status)
	}
	if e.comment != "" {
		opts.WriteString(" COMMENT " + lit(e.comment))
	}
	return fmt.Sprintf("CREATE EVENT %s ON SCHEDULE %s%s DO SET @probe = %d;\n", q(e.name), sched, opts.String(), e.n)
}

// rows renders three rows per table, parents first (the tables are declared in that order):
// integer columns carry the row number (distinct, positive, a valid reference to a parent's
// id), a nullable column is NULL in the third row, an AUTO_INCREMENT id is set explicitly so
// children can reference it, generated columns are left to the server.
func (s *pSchema) rows() string {
	var b strings.Builder
	for _, t := range s.tables {
		var names []string
		var cols []*pCol
		for _, c := range t.cols {
			if c.gen == "" {
				names = append(names, q(c.name))
				cols = append(cols, c)
			}
		}
		var rows []string
		for i := 1; i <= 3; i++ {
			var vals []string
			for _, c := range cols {
				if i == 3 && !c.notNull && !c.pk {
					vals = append(vals, "NULL")
					continue
				}
				vals = append(vals, value(c, i))
			}
			rows = append(rows, "("+strings.Join(vals, ", ")+")")
		}
		fmt.Fprintf(&b, "INSERT INTO %s (%s) VALUES %s;\n", q(t.name), strings.Join(names, ", "), strings.Join(rows, ", "))
	}
	return b.String()
}

// value is row i's value for column c.
func value(c *pCol, i int) string {
	switch {
	case isInteger(c.typ), c.typ == "double", strings.HasPrefix(c.typ, "decimal"):
		return fmt.Sprint(i)
	case strings.HasPrefix(c.typ, "varchar"), c.typ == "text":
		return fmt.Sprintf("'v%d'", i)
	case c.typ == "datetime(6)":
		return fmt.Sprintf("'2024-01-%02d 00:00:00'", i)
	case c.typ == "date":
		return fmt.Sprintf("'2024-01-%02d'", i)
	case c.typ == "enum":
		return "'" + c.labels[(i-1)%len(c.labels)] + "'"
	case c.typ == "json":
		return fmt.Sprintf(`'{"i": %d}'`, i)
	case c.typ == "point":
		return fmt.Sprintf("ST_GeomFromText('POINT(%d %d)', 4326)", i, i)
	}
	return "NULL"
}

// fill is the literal a backfill gives a column that turns NOT NULL under existing rows.
// (point is always NOT NULL from the start -- see newCol -- so this case is never reached
// by a mutation today; it is here so fill stays total over the type vocabulary.)
func fill(c *pCol) string {
	switch {
	case c.typ == "point":
		return "ST_GeomFromText('POINT(9 9)', 4326)"
	case isNumeric(c.typ):
		return "9"
	case c.typ == "enum":
		return "'" + c.labels[0] + "'"
	case c.typ == "json":
		return "'{}'"
	case c.typ == "datetime(6)":
		return "'2024-02-01 00:00:00'"
	case c.typ == "date":
		return "'2024-02-01'"
	}
	return "'fill'"
}

// ---- generating a source schema -------------------------------------------------------

func pick[T any](r *rand.Rand, list []T) T { return list[r.Intn(len(list))] }

func (s *pSchema) newCol(r *rand.Rand) *pCol {
	c := &pCol{name: s.next("c"), typ: pick(r, probeTypes)}
	if c.typ == "enum" {
		c.labels = []string{"a", "b", "c"}[:2+r.Intn(2)]
	}
	if c.typ == "point" {
		c.notNull = true // a SPATIAL index (addSpatialKey) needs it; simplest to always have it
	} else {
		c.notNull = r.Intn(2) == 0
	}
	if c.notNull || r.Intn(3) == 0 {
		c.def = defaultFor(c)
	}
	if (c.typ == "text" || strings.HasPrefix(c.typ, "varchar")) && r.Intn(4) == 0 {
		c.collate = true
	}
	if c.typ == "datetime(6)" && c.def != "" && r.Intn(2) == 0 {
		c.onUpdate = true
	}
	if r.Intn(8) == 0 {
		c.invisible = true
	}
	if r.Intn(5) == 0 {
		c.comment = "note " + c.name
	}
	return c
}

// numericCols: the plain (not generated, not AUTO_INCREMENT, not a foreign key's -- MySQL
// refuses a generated column over a referencing column with a referential action, 1215)
// numeric columns a generated column or a trigger body may read.
func (t *pTable) numericCols() []*pCol {
	var out []*pCol
	for _, c := range t.cols {
		if isNumeric(c.typ) && c.gen == "" && !c.auto && !t.inFK(c.name) {
			out = append(out, c)
		}
	}
	return out
}

func (s *pSchema) newTable(r *rand.Rand) *pTable {
	t := &pTable{name: s.next("t")}
	t.orig = t.name
	id := &pCol{name: "id", typ: pick(r, []string{"int", "bigint unsigned"}), notNull: true, pk: true, auto: r.Intn(2) == 0}
	t.cols = append(t.cols, id)
	n := 2 + r.Intn(4)
	for i := 0; i < n; i++ {
		t.cols = append(t.cols, s.newCol(r))
	}
	if src := t.numericCols(); len(src) > 0 && r.Intn(3) == 0 {
		g := &pCol{name: s.next("g"), typ: "bigint", gen: pick(r, src).name, stored: r.Intn(2) == 0}
		// before or after the column it reads: both are one CREATE TABLE
		at := r.Intn(len(t.cols) + 1)
		t.cols = append(t.cols[:at], append([]*pCol{g}, t.cols[at:]...)...)
	}
	if r.Intn(2) == 0 {
		s.addKey(r, t)
	}
	if r.Intn(5) == 0 {
		s.addFulltextKey(r, t)
	}
	if r.Intn(5) == 0 {
		s.addSpatialKey(r, t)
	}
	if r.Intn(5) == 0 {
		s.addFunctionalKey(r, t)
	}
	if r.Intn(3) == 0 {
		s.addCheck(r, t)
	}
	if len(s.tables) > 0 && r.Intn(2) == 0 {
		if r.Intn(4) == 0 {
			s.addCompositeFK(r, t, pick(r, s.tables))
		} else {
			s.addFK(r, t, pick(r, s.tables))
		}
	}
	if r.Intn(4) == 0 {
		t.comment = "about " + t.name
	}
	if id.auto && r.Intn(3) == 0 {
		t.autoInc = 100 + r.Intn(900)
	}
	if r.Intn(5) == 0 {
		t.charset, t.collation = "utf8mb4", "utf8mb4_bin"
	}
	if r.Intn(5) == 0 {
		t.rowFormat = pick(r, []string{"DYNAMIC", "COMPRESSED"})
	}
	return t
}

func (s *pSchema) addKey(r *rand.Rand, t *pTable) bool {
	unique := r.Intn(2) == 0
	var cands []string
	for _, c := range t.cols {
		// (a column a mutation added holds one default in every row: no unique key over it;
		// a text column takes a prefix length below, a json column no key at all; a point
		// column takes only a SPATIAL index, addSpatialKey's job, never this plain kind)
		// (an ON UPDATE timestamp is one value in every row a backfill touches: no unique key)
		if c.typ != "json" && c.typ != "point" && !c.pk && (c.gen == "" || c.stored) && !(unique && (c.typ == "enum" || c.fresh || c.onUpdate)) {
			cands = append(cands, c.name)
		}
	}
	if len(cands) == 0 {
		return false
	}
	r.Shuffle(len(cands), func(i, j int) { cands[i], cands[j] = cands[j], cands[i] })
	n := 1
	if len(cands) > 1 && r.Intn(2) == 0 {
		n = 2
	}
	k := &pKey{name: s.next("k"), unique: unique, cols: cands[:n], prefix: make([]int, n), desc: make([]bool, n)}
	for i, name := range k.cols {
		c := t.col(name)
		if c.typ == "text" || (strings.HasPrefix(c.typ, "varchar") && r.Intn(3) == 0) {
			k.prefix[i] = 10
		}
		if r.Intn(4) == 0 {
			k.desc[i] = true
		}
	}
	if r.Intn(6) == 0 {
		k.invisible = true
	}
	t.keys = append(t.keys, k)
	return true
}

// addFunctionalKey gives t an index over an expression, not a plain column: `(col + 1)` for
// a numeric source or `(lower(col))` for a text one (MySQL implements a functional key part
// as a hidden generated column reading the real one -- cols still names it, so every
// mutation that keeps a key's columns in step by name, rename included, already does).
func (s *pSchema) addFunctionalKey(r *rand.Rand, t *pTable) bool {
	type cand struct {
		name, kind string
	}
	var cands []cand
	for _, c := range t.numericCols() {
		cands = append(cands, cand{c.name, "plus1"})
	}
	for _, c := range t.cols {
		if strings.HasPrefix(c.typ, "varchar") && c.gen == "" && !t.inFK(c.name) {
			cands = append(cands, cand{c.name, "lower"})
		}
	}
	if len(cands) == 0 {
		return false
	}
	picked := pick(r, cands)
	k := &pKey{name: s.next("k"), cols: []string{picked.name}, prefix: []int{0}, expr: []string{picked.kind}, desc: []bool{r.Intn(3) == 0}}
	t.keys = append(t.keys, k)
	return true
}

// addFulltextKey gives t a FULLTEXT index over one of its text / varchar columns.
func (s *pSchema) addFulltextKey(r *rand.Rand, t *pTable) bool {
	var cands []string
	for _, c := range t.cols {
		if (c.typ == "text" || strings.HasPrefix(c.typ, "varchar")) && (c.gen == "" || c.stored) {
			cands = append(cands, c.name)
		}
	}
	if len(cands) == 0 {
		return false
	}
	name := pick(r, cands)
	t.keys = append(t.keys, &pKey{name: s.next("k"), special: "FULLTEXT", cols: []string{name}, prefix: []int{0}})
	return true
}

// addSpatialKey gives t a SPATIAL index over one of its point columns (always NOT NULL --
// see newCol -- which a SPATIAL index requires).
func (s *pSchema) addSpatialKey(r *rand.Rand, t *pTable) bool {
	var cands []string
	for _, c := range t.cols {
		if c.typ == "point" {
			cands = append(cands, c.name)
		}
	}
	if len(cands) == 0 {
		return false
	}
	name := pick(r, cands)
	t.keys = append(t.keys, &pKey{name: s.next("k"), special: "SPATIAL", cols: []string{name}, prefix: []int{0}})
	return true
}

func (s *pSchema) addCheck(r *rand.Rand, t *pTable) bool {
	var cands []string
	for _, c := range t.cols {
		if !c.auto && !c.pk && !t.inFK(c.name) && (isNumeric(c.typ) || strings.HasPrefix(c.typ, "varchar")) {
			cands = append(cands, c.name)
		}
	}
	if len(cands) == 0 {
		return false
	}
	t.checks = append(t.checks, &pCheck{name: s.next("ck"), col: pick(r, cands), enforced: true})
	return true
}

// addFK gives t a new column referencing parent's primary key.
func (s *pSchema) addFK(r *rand.Rand, t, parent *pTable) bool {
	if parent == t {
		return false
	}
	ppk := parent.pk()
	c := &pCol{name: s.next("r"), typ: ppk.typ, notNull: r.Intn(2) == 0}
	t.cols = append(t.cols, c)
	fk := &pFK{name: s.next("fk"), cols: []string{c.name}, refTable: parent.name, refCols: []string{ppk.name},
		onDelete: pick(r, []string{"", "CASCADE", "SET NULL", "RESTRICT", "NO ACTION"}),
		onUpdate: pick(r, []string{"", "CASCADE", "SET NULL", "RESTRICT", "NO ACTION"})}
	t.fks = append(t.fks, fk)
	// a column an action may SET NULL cannot be declared NOT NULL
	if fk.onDelete == "SET NULL" || fk.onUpdate == "SET NULL" {
		c.notNull = false
	}
	return true
}

// addCompositeFK gives t two new columns referencing a two-column UNIQUE key that parent
// gains over two new NOT NULL integer columns of its own (rows carry the row number in
// every integer column, so every (i, i) pair exists in the parent).
func (s *pSchema) addCompositeFK(r *rand.Rand, t, parent *pTable) bool {
	if parent == t {
		return false
	}
	a := &pCol{name: s.next("a"), typ: "int", notNull: true, def: "0"}
	b := &pCol{name: s.next("b"), typ: "int", notNull: true, def: "0"}
	parent.cols = append(parent.cols, a, b)
	if s.mutating {
		// the parent holds rows: the new key columns need distinct values (the key's)
		a.fresh, b.fresh = true, true
		pk := parent.pk().name
		s.intents = append(s.intents, fmt.Sprintf("-- @migrate backfill %s.%s = %s", parent.name, a.name, q(pk)),
			fmt.Sprintf("-- @migrate backfill %s.%s = %s", parent.name, b.name, q(pk)))
	}
	parent.keys = append(parent.keys, &pKey{name: s.next("k"), unique: true, cols: []string{a.name, b.name}})
	ra := &pCol{name: s.next("r"), typ: "int", fresh: s.mutating}
	rb := &pCol{name: s.next("r"), typ: "int", fresh: s.mutating}
	t.cols = append(t.cols, ra, rb)
	t.fks = append(t.fks, &pFK{name: s.next("fk"), cols: []string{ra.name, rb.name}, refTable: parent.name, refCols: []string{a.name, b.name},
		onDelete: pick(r, []string{"", "CASCADE", "SET NULL"}), onUpdate: pick(r, []string{"", "CASCADE", "RESTRICT"})})
	return true
}

func (s *pSchema) addView(r *rand.Rand) bool {
	t := pick(r, s.tables)
	var cols []string
	for _, c := range t.cols {
		if r.Intn(2) == 0 {
			cols = append(cols, c.name)
		}
	}
	if len(cols) == 0 {
		cols = []string{t.cols[0].name}
	}
	s.views = append(s.views, &pView{name: s.next("v"), table: t.name, cols: cols})
	return true
}

func (s *pSchema) addTrigger(r *rand.Rand) bool {
	t := pick(r, s.tables)
	var src []*pCol
	for _, c := range t.numericCols() {
		// the body adds to the column: not a reference, not a key, not a referenced column
		if !t.inFK(c.name) && !c.pk && len(referencedBy(s, t, c.name)) == 0 {
			src = append(src, c)
		}
	}
	if len(src) == 0 {
		return false
	}
	s.triggers = append(s.triggers, &pTrigger{name: s.next("tr"), table: t.name, col: pick(r, src).name, n: 1 + r.Intn(9)})
	return true
}

func generate(r *rand.Rand) *pSchema {
	s := &pSchema{}
	n := 1 + r.Intn(3)
	for i := 0; i < n; i++ {
		s.tables = append(s.tables, s.newTable(r))
	}
	if r.Intn(2) == 0 {
		s.addView(r)
	}
	if r.Intn(3) == 0 {
		s.addTrigger(r)
	}
	if r.Intn(2) == 0 {
		s.funcs = append(s.funcs, &pFunc{name: s.next("f"), n: 1 + r.Intn(9)})
	}
	if r.Intn(2) == 0 {
		s.procs = append(s.procs, &pProc{name: s.next("p"), n: 1 + r.Intn(9)})
	}
	if r.Intn(3) == 0 {
		s.events = append(s.events, s.newEvent(r))
	}
	return s
}

// newEvent gives about half its events some of the optional schedule / option vocabulary,
// so the source side already carries some of it for a mutation to toggle off, not only on.
func (s *pSchema) newEvent(r *rand.Rand) *pEvent {
	e := &pEvent{name: s.next("ev"), n: 1 + r.Intn(9)}
	if r.Intn(2) == 0 {
		e.starts, e.ends = "2099-01-01 00:00:00", "2099-06-01 00:00:00"
	}
	if r.Intn(3) == 0 {
		e.completion = "PRESERVE"
	}
	if r.Intn(2) == 0 {
		e.status = "DISABLE"
	}
	if r.Intn(3) == 0 {
		e.comment = "about " + e.name
	}
	return e
}

// ---- mutations ------------------------------------------------------------------------

// A mutation edits the schema toward the target and declares what the diff cannot see; it
// reports false when the schema offers nothing it applies to. touched is the set of names
// ("t", "t.c") a mutation already moved: a second mutation keeps off them so every pair's
// declarations describe one step per object (a column renamed and then dropped is a
// declaration the planner is right to refuse, not a probe of its DDL).
type mutation struct {
	name  string
	apply func(r *rand.Rand, s *pSchema, touched map[string]bool) bool
}

func (t *pTable) plainCols(touched map[string]bool) []*pCol {
	var out []*pCol
	for _, c := range t.cols {
		if !c.pk && c.gen == "" && !c.fresh && !touched[t.name+"."+c.name] {
			out = append(out, c)
		}
	}
	return out
}

func untouched(s *pSchema, touched map[string]bool) []*pTable {
	var out []*pTable
	for _, t := range s.tables {
		if !touched[t.name] {
			out = append(out, t)
		}
	}
	return out
}

// referencedByAny: the foreign keys (in any table) that reference table t at all, on any
// of its columns.
// nonFreshEvents are s's events that already existed in the source (not one an earlier
// step in the same recipe just added): a step that only sets one of their optional
// fields, on one of those, produces a whole "+ event" (diff.Compare never sees the from
// side at all), not the per-field "~ event ..." the event mutations below exist for.
func nonFreshEvents(s *pSchema) []*pEvent {
	var out []*pEvent
	for _, e := range s.events {
		if !e.fresh {
			out = append(out, e)
		}
	}
	return out
}

func referencedByAny(s *pSchema, t *pTable) []*pFK {
	var out []*pFK
	for _, other := range s.tables {
		for _, fk := range other.fks {
			if fk.refTable == t.name {
				out = append(out, fk)
			}
		}
	}
	return out
}

// referencedBy: the foreign keys (in any table) that reference table t's column col.
func referencedBy(s *pSchema, t *pTable, col string) []*pFK {
	var out []*pFK
	for _, other := range s.tables {
		for _, fk := range other.fks {
			if fk.refTable == t.name && indexOf(fk.refCols, col) >= 0 {
				out = append(out, fk)
			}
		}
	}
	return out
}

// detach removes every part of the schema that reads t's column col: keys and checks over
// it, foreign keys from it, generated columns reading it (dropped, declared), views'
// projections of it (the view goes when it projected nothing else), triggers on it.
func detach(s *pSchema, t *pTable, col string) {
	var keys []*pKey
	for _, k := range t.keys {
		if indexOf(k.cols, col) < 0 {
			keys = append(keys, k)
		}
	}
	t.keys = keys
	var checks []*pCheck
	for _, ck := range t.checks {
		if ck.col != col {
			checks = append(checks, ck)
		}
	}
	t.checks = checks
	var fks []*pFK
	for _, fk := range t.fks {
		if indexOf(fk.cols, col) < 0 {
			fks = append(fks, fk)
		}
	}
	t.fks = fks
	var cols []*pCol
	for _, c := range t.cols {
		if c.gen == col {
			if !c.fresh {
				s.intents = append(s.intents, "-- @migrate drop "+t.orig+"."+c.name)
			}
			detach(s, t, c.name)
			continue
		}
		cols = append(cols, c)
	}
	t.cols = cols
	var views []*pView
	for _, v := range s.views {
		if v.table == t.name {
			var keep []string
			for _, c := range v.cols {
				if c != col {
					keep = append(keep, c)
				}
			}
			if len(keep) == 0 {
				continue
			}
			v.cols = keep
		}
		views = append(views, v)
	}
	s.views = views
	var triggers []*pTrigger
	for _, tr := range s.triggers {
		if tr.table != t.name || tr.col != col {
			triggers = append(triggers, tr)
		}
	}
	s.triggers = triggers
}

// renameRefs spells t's column from as to everywhere it is read.
func renameRefs(s *pSchema, t *pTable, from, to string) {
	for _, k := range t.keys {
		for i, c := range k.cols {
			if c == from {
				k.cols[i] = to
			}
		}
	}
	for _, ck := range t.checks {
		if ck.col == from {
			ck.col = to
		}
	}
	for _, fk := range t.fks {
		if i := indexOf(fk.cols, from); i >= 0 {
			fk.cols[i] = to
		}
	}
	for _, c := range t.cols {
		if c.gen == from {
			c.gen = to
		}
	}
	for _, fk := range referencedBy(s, t, from) {
		fk.refCols[indexOf(fk.refCols, from)] = to
	}
	for _, v := range s.views {
		if v.table == t.name {
			for i, c := range v.cols {
				if c == from {
					v.cols[i] = to
				}
			}
		}
	}
	for _, tr := range s.triggers {
		if tr.table == t.name && tr.col == from {
			tr.col = to
		}
	}
}

func dropTable(s *pSchema, t *pTable) {
	var tables []*pTable
	for _, other := range s.tables {
		if other == t {
			continue
		}
		var fks []*pFK
		for _, fk := range other.fks {
			if fk.refTable != t.name {
				fks = append(fks, fk)
			}
		}
		other.fks = fks
		tables = append(tables, other)
	}
	s.tables = tables
	var views []*pView
	for _, v := range s.views {
		if v.table != t.name {
			views = append(views, v)
		}
	}
	s.views = views
	var triggers []*pTrigger
	for _, tr := range s.triggers {
		if tr.table != t.name {
			triggers = append(triggers, tr)
		}
	}
	s.triggers = triggers
}

var mutations = []mutation{
	{"add column", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		ts := untouched(s, touched)
		if len(ts) == 0 {
			return false
		}
		t := pick(r, ts)
		var c *pCol
		if src := t.numericCols(); len(src) > 0 && r.Intn(3) == 0 {
			c = &pCol{name: s.next("g"), typ: "bigint", gen: pick(r, src).name, stored: r.Intn(2) == 0}
		} else {
			c = s.newCol(r)
		}
		if c.typ == "point" && s.mutating {
			// t already holds rows (this is a mutation, not a fresh table): unlike every
			// other type here, MySQL has no implicit "zero value" for a NOT NULL geometry
			// column, and even an expression DEFAULT does not save it (Error 1138,
			// "Invalid use of NULL value", measured both ways) -- a point column added
			// under rows can only be nullable.
			c.notNull, c.def = false, ""
		}
		c.fresh = true
		at := r.Intn(len(t.cols) + 1)
		t.cols = append(t.cols[:at], append([]*pCol{c}, t.cols[at:]...)...)
		touched[t.name+"."+c.name] = true
		return true
	}},
	{"add generated column reading a new column", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		ts := untouched(s, touched)
		if len(ts) == 0 {
			return false
		}
		t := pick(r, ts)
		x := &pCol{name: s.next("c"), typ: pick(r, []string{"int", "decimal(10,2)", "smallint"})}
		g := &pCol{name: s.next("g"), typ: "bigint", gen: x.name, stored: r.Intn(2) == 0}
		x.fresh, g.fresh = true, true
		// the generated column first or last, before or after its source
		at := r.Intn(len(t.cols) + 1)
		t.cols = append(t.cols[:at], append([]*pCol{g}, t.cols[at:]...)...)
		at = r.Intn(len(t.cols) + 1)
		t.cols = append(t.cols[:at], append([]*pCol{x}, t.cols[at:]...)...)
		touched[t.name+"."+x.name], touched[t.name+"."+g.name] = true, true
		return true
	}},
	{"drop column", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		var cands []*pTable
		for _, t := range untouched(s, touched) {
			if len(t.plainCols(touched)) > 0 {
				cands = append(cands, t)
			}
		}
		if len(cands) == 0 {
			return false
		}
		t := pick(r, cands)
		c := pick(r, t.plainCols(touched))
		for _, g := range t.cols {
			if g.gen == c.name && touched[t.name+"."+g.name] {
				return false // the generated column reading it was moved by another step already
			}
		}
		for _, fk := range referencedBy(s, t, c.name) {
			owner := s.table(fkOwner(s, fk))
			var fks []*pFK
			for _, f := range owner.fks {
				if f != fk {
					fks = append(fks, f)
				}
			}
			owner.fks = fks
		}
		s.intents = append(s.intents, "-- @migrate drop "+t.orig+"."+c.name)
		detach(s, t, c.name)
		t.cols = append(t.cols[:indexOfCol(t, c.name)], t.cols[indexOfCol(t, c.name)+1:]...)
		touched[t.name+"."+c.name] = true
		return true
	}},
	{"drop generated column", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		for _, t := range untouched(s, touched) {
			for _, c := range t.cols {
				if c.gen != "" && !touched[t.name+"."+c.name] {
					s.intents = append(s.intents, "-- @migrate drop "+t.orig+"."+c.name)
					detach(s, t, c.name)
					t.cols = append(t.cols[:indexOfCol(t, c.name)], t.cols[indexOfCol(t, c.name)+1:]...)
					touched[t.name+"."+c.name] = true
					return true
				}
			}
		}
		return false
	}},
	{"rename column", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		ts := untouched(s, touched)
		if len(ts) == 0 {
			return false
		}
		t := pick(r, ts)
		var cands []*pCol
		for _, c := range t.cols {
			if !touched[t.name+"."+c.name] {
				cands = append(cands, c)
			}
		}
		if len(cands) == 0 {
			return false
		}
		c := pick(r, cands)
		to := c.name + "_new"
		s.intents = append(s.intents, "-- @migrate rename "+t.orig+"."+c.name+" -> "+t.name+"."+to)
		// a backfill expression written earlier names the column as the target spells it
		for i, in := range s.intents {
			if strings.HasPrefix(in, "-- @migrate backfill "+t.name+".") {
				s.intents[i] = strings.ReplaceAll(in, " = "+q(c.name), " = "+q(to))
			}
		}
		renameRefs(s, t, c.name, to)
		touched[t.name+"."+c.name], touched[t.name+"."+to] = true, true
		c.name = to
		return true
	}},
	{"widen column", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		ts := untouched(s, touched)
		if len(ts) == 0 {
			return false
		}
		t := pick(r, ts)
		var cands []*pCol
		for _, c := range t.cols {
			if widen(c.typ) != "" && c.gen == "" && !touched[t.name+"."+c.name] {
				cands = append(cands, c)
			}
		}
		if len(cands) == 0 {
			return false
		}
		c := pick(r, cands)
		w := widen(c.typ)
		if c.auto && !isInteger(w) {
			return false
		}
		if t.inFK(c.name) {
			return false // a referencing column follows its parent's key (widened below)
		}
		// a referenced key and every column referencing it widen together
		for _, fk := range referencedBy(s, t, c.name) {
			child := s.table(fkOwner(s, fk))
			cc := child.col(fk.cols[indexOf(fk.refCols, c.name)])
			if touched[child.name+"."+cc.name] {
				return false
			}
			cc.typ = w
			touched[child.name+"."+cc.name] = true
		}
		c.typ = w
		if c.def != "" {
			c.def = defaultFor(c)
		}
		touched[t.name+"."+c.name] = true
		return true
	}},
	{"move column", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		ts := untouched(s, touched)
		if len(ts) == 0 {
			return false
		}
		t := pick(r, ts)
		if len(t.cols) < 2 {
			return false
		}
		i := r.Intn(len(t.cols))
		c := t.cols[i]
		if touched[t.name+"."+c.name] {
			return false
		}
		t.cols = append(t.cols[:i], t.cols[i+1:]...)
		j := r.Intn(len(t.cols) + 1)
		t.cols = append(t.cols[:j], append([]*pCol{c}, t.cols[j:]...)...)
		touched[t.name+"."+c.name] = true
		return true
	}},
	{"change nullability and default", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		ts := untouched(s, touched)
		if len(ts) == 0 {
			return false
		}
		t := pick(r, ts)
		var cands []*pCol
		for _, c := range t.plainCols(touched) {
			// point is always NOT NULL (a SPATIAL index needs it, and it takes no plain
			// literal DEFAULT the way defaultFor spells one)
			if c.typ != "point" {
				cands = append(cands, c)
			}
		}
		if len(cands) == 0 {
			return false
		}
		c := pick(r, cands)
		for _, fk := range t.fks {
			if indexOf(fk.cols, c.name) >= 0 && fk.onDelete == "SET NULL" {
				return false
			}
		}
		c.notNull = !c.notNull
		if c.notNull || r.Intn(2) == 0 {
			c.def = defaultFor(c)
		} else {
			c.def = ""
		}
		if c.notNull {
			f := fill(c)
			if t.inFK(c.name) {
				f = "1" // a parent every table has
			}
			s.intents = append(s.intents, fmt.Sprintf("-- @migrate backfill %s.%s = %s where %s is null", t.name, c.name, f, q(c.name)))
		}
		touched[t.name+"."+c.name] = true
		return true
	}},
	{"add key", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		ts := untouched(s, touched)
		if len(ts) == 0 {
			return false
		}
		return s.addKey(r, pick(r, ts))
	}},
	{"drop key", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		for _, t := range untouched(s, touched) {
			if len(t.keys) > 0 {
				i := r.Intn(len(t.keys))
				// the index an AUTO_INCREMENT column or a foreign key (this table's, or one
				// referencing this table) needs stays: without it the target is not a schema
				lead := t.keys[i].cols[0]
				if c := t.col(lead); c.auto || len(referencedBy(s, t, lead)) > 0 {
					return false
				}
				for _, fk := range t.fks {
					if lead == fk.cols[0] {
						return false
					}
				}
				t.keys = append(t.keys[:i], t.keys[i+1:]...)
				return true
			}
		}
		return false
	}},
	{"add fulltext key", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		ts := untouched(s, touched)
		if len(ts) == 0 {
			return false
		}
		return s.addFulltextKey(r, pick(r, ts))
	}},
	{"add spatial key", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		ts := untouched(s, touched)
		if len(ts) == 0 {
			return false
		}
		return s.addSpatialKey(r, pick(r, ts))
	}},
	{"add functional key", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		ts := untouched(s, touched)
		if len(ts) == 0 {
			return false
		}
		return s.addFunctionalKey(r, pick(r, ts))
	}},
	{"toggle key invisible", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		for _, t := range untouched(s, touched) {
			if len(t.keys) > 0 {
				k := pick(r, t.keys)
				k.invisible = !k.invisible
				return true
			}
		}
		return false
	}},
	{"move primary key", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		ts := untouched(s, touched)
		if len(ts) == 0 {
			return false
		}
		t := pick(r, ts)
		old := t.pk()
		var cands []*pCol
		for _, c := range t.cols {
			if !c.pk && c.gen == "" && isInteger(c.typ) && !touched[t.name+"."+c.name] {
				cands = append(cands, c)
			}
		}
		if len(cands) == 0 || touched[t.name+"."+old.name] {
			return false
		}
		c := pick(r, cands)
		if t.inFK(c.name) {
			return false
		}
		if !c.notNull {
			s.intents = append(s.intents, fmt.Sprintf("-- @migrate backfill %s.%s = 9 where %s is null", t.name, c.name, q(c.name)))
		}
		c.pk, c.notNull, c.def = true, true, ""
		old.pk = false
		// the old key column: an AUTO_INCREMENT column must stay a key, and so must a
		// referenced one
		if old.auto || len(referencedBy(s, t, old.name)) > 0 {
			t.keys = append(t.keys, &pKey{name: s.next("k"), unique: true, cols: []string{old.name}})
		} else if r.Intn(2) == 0 {
			old.notNull = false
		}
		touched[t.name+"."+old.name], touched[t.name+"."+c.name] = true, true
		return true
	}},
	{"add foreign key", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		ts := untouched(s, touched)
		if len(ts) < 2 {
			return false
		}
		i, j := r.Intn(len(ts)), r.Intn(len(ts))
		if i == j {
			return false
		}
		if i < j {
			i, j = j, i
		}
		t, parent := ts[i], ts[j] // the parent is created first in the rendered schema
		if !s.addFK(r, t, parent) {
			return false
		}
		c := t.cols[len(t.cols)-1]
		if c.notNull {
			// existing rows need a parent before the column turns NOT NULL
			s.intents = append(s.intents, fmt.Sprintf("-- @migrate backfill %s.%s = 1", t.name, c.name))
		}
		touched[t.name+"."+c.name] = true
		return true
	}},
	{"add composite foreign key", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		ts := untouched(s, touched)
		if len(ts) < 2 {
			return false
		}
		i, j := r.Intn(len(ts)), r.Intn(len(ts))
		if i == j {
			return false
		}
		if i < j {
			i, j = j, i
		}
		t, parent := ts[i], ts[j]
		if !s.addCompositeFK(r, t, parent) {
			return false
		}
		for _, c := range parent.cols[len(parent.cols)-2:] {
			c.fresh = true
			touched[parent.name+"."+c.name] = true
		}
		for _, c := range t.cols[len(t.cols)-2:] {
			c.fresh = true
			touched[t.name+"."+c.name] = true
		}
		return true
	}},
	{"drop foreign key", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		for _, t := range untouched(s, touched) {
			if len(t.fks) > 0 {
				i := r.Intn(len(t.fks))
				t.fks = append(t.fks[:i], t.fks[i+1:]...)
				return true
			}
		}
		return false
	}},
	{"change foreign key actions", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		for _, t := range untouched(s, touched) {
			for _, fk := range t.fks {
				key := t.name + ".fk:" + fk.name
				if touched[key] {
					continue
				}
				fk.onDelete = pick(r, []string{"", "CASCADE", "SET NULL", "RESTRICT", "NO ACTION"})
				fk.onUpdate = pick(r, []string{"", "CASCADE", "SET NULL", "RESTRICT", "NO ACTION"})
				// SET NULL on either action refuses a NOT NULL referencing column (1216-class)
				if fk.onDelete == "SET NULL" || fk.onUpdate == "SET NULL" {
					for _, name := range fk.cols {
						t.col(name).notNull = false
					}
				}
				touched[key] = true
				return true
			}
		}
		return false
	}},
	{"add check", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		ts := untouched(s, touched)
		if len(ts) == 0 {
			return false
		}
		return s.addCheck(r, pick(r, ts))
	}},
	{"drop check", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		for _, t := range untouched(s, touched) {
			if len(t.checks) > 0 {
				i := r.Intn(len(t.checks))
				t.checks = append(t.checks[:i], t.checks[i+1:]...)
				return true
			}
		}
		return false
	}},
	{"change check expression", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		for _, t := range untouched(s, touched) {
			for _, ck := range t.checks {
				key := t.name + ".check:" + ck.name
				if touched[key] {
					continue
				}
				if ck.op == ">=" {
					ck.op = ">"
				} else {
					ck.op = ">="
				}
				touched[key] = true
				return true
			}
		}
		return false
	}},
	// CheckProps compares expression and enforced together (diff.CheckProps), so the planner
	// redoes the constraint as a DROP + ADD rather than ALTER TABLE ... ALTER CHECK ...
	// [NOT] ENFORCED; both a real mysqld accepts, and this exercises the DROP + ADD path.
	{"toggle check enforced", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		for _, t := range untouched(s, touched) {
			for _, ck := range t.checks {
				key := t.name + ".check:" + ck.name
				if touched[key] {
					continue
				}
				ck.enforced = !ck.enforced
				touched[key] = true
				return true
			}
		}
		return false
	}},
	{"add table", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		t := s.newTable(r)
		s.tables = append(s.tables, t)
		touched[t.name] = true
		return true
	}},
	{"drop table", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		ts := untouched(s, touched)
		if len(ts) < 2 {
			return false
		}
		t := pick(r, ts)
		for k := range touched {
			if strings.HasPrefix(k, t.name+".") {
				return false // a step already moved one of its columns: one declaration per object
			}
		}
		s.intents = append(s.intents, "-- @migrate drop "+t.orig)
		dropTable(s, t)
		touched[t.name] = true
		return true
	}},
	{"rename table", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		ts := untouched(s, touched)
		if len(ts) == 0 {
			return false
		}
		t := pick(r, ts)
		to := t.name + "_new"
		s.intents = append(s.intents, "-- @migrate rename "+t.orig+" -> "+to)
		// declarations written before this one name the table on their right side as it
		// will be: the new name
		for i, in := range s.intents {
			in = strings.ReplaceAll(in, " -> "+t.name+".", " -> "+to+".")
			in = strings.ReplaceAll(in, "enum "+t.name+".", "enum "+to+".")
			in = strings.ReplaceAll(in, "backfill "+t.name+".", "backfill "+to+".")
			s.intents[i] = in
		}
		for _, other := range s.tables {
			for _, fk := range other.fks {
				if fk.refTable == t.name {
					fk.refTable = to
				}
			}
		}
		for _, v := range s.views {
			if v.table == t.name {
				v.table = to
			}
		}
		for _, tr := range s.triggers {
			if tr.table == t.name {
				tr.table = to
			}
		}
		touched[t.name], touched[to] = true, true
		t.name = to
		return true
	}},
	{"add enum label", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		for _, t := range untouched(s, touched) {
			for _, c := range t.cols {
				if c.typ == "enum" && !touched[t.name+"."+c.name] {
					c.labels = append(c.labels, "z")
					touched[t.name+"."+c.name] = true
					return true
				}
			}
		}
		return false
	}},
	{"drop enum label", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		for _, t := range untouched(s, touched) {
			for _, c := range t.cols {
				if c.typ == "enum" && len(c.labels) > 1 && !touched[t.name+"."+c.name] {
					gone := c.labels[len(c.labels)-1]
					c.labels = c.labels[:len(c.labels)-1]
					s.intents = append(s.intents, fmt.Sprintf("-- @migrate enum %s.%s: drop '%s' using '%s'", t.name, c.name, gone, c.labels[0]))
					touched[t.name+"."+c.name] = true
					return true
				}
			}
		}
		return false
	}},
	{"change generated expression", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		for _, t := range untouched(s, touched) {
			for _, c := range t.cols {
				if c.gen == "" || touched[t.name+"."+c.name] {
					continue
				}
				for _, src := range t.numericCols() {
					if src.name != c.gen && !touched[t.name+"."+src.name] {
						c.gen = src.name
						touched[t.name+"."+c.name] = true
						return true
					}
				}
			}
		}
		return false
	}},
	{"move primary key onto a new column", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		ts := untouched(s, touched)
		if len(ts) == 0 {
			return false
		}
		t := pick(r, ts)
		old := t.pk()
		if touched[t.name+"."+old.name] {
			return false
		}
		c := &pCol{name: s.next("c"), typ: pick(r, []string{"int", "bigint unsigned"}), notNull: true, pk: true}
		at := r.Intn(len(t.cols) + 1)
		t.cols = append(t.cols[:at], append([]*pCol{c}, t.cols[at:]...)...)
		if !old.auto && r.Intn(2) == 0 {
			c.auto = true // the server numbers the existing rows
		} else {
			// the rows need distinct values before the key goes on: the old key's
			s.intents = append(s.intents, fmt.Sprintf("-- @migrate backfill %s.%s = %s", t.name, c.name, q(old.name)))
		}
		old.pk = false
		if old.auto || len(referencedBy(s, t, old.name)) > 0 {
			t.keys = append(t.keys, &pKey{name: s.next("k"), unique: true, cols: []string{old.name}})
		}
		touched[t.name+"."+old.name], touched[t.name+"."+c.name] = true, true
		return true
	}},
	{"toggle collation", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		for _, t := range untouched(s, touched) {
			for _, c := range t.cols {
				if (c.typ == "text" || strings.HasPrefix(c.typ, "varchar")) && c.gen == "" && !touched[t.name+"."+c.name] {
					c.collate = !c.collate
					touched[t.name+"."+c.name] = true
					return true
				}
			}
		}
		return false
	}},
	{"toggle invisible", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		for _, t := range untouched(s, touched) {
			for _, c := range t.cols {
				if !c.pk && c.gen == "" && !touched[t.name+"."+c.name] {
					c.invisible = !c.invisible
					touched[t.name+"."+c.name] = true
					return true
				}
			}
		}
		return false
	}},
	{"toggle on update", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		for _, t := range untouched(s, touched) {
			for _, c := range t.cols {
				if c.typ == "datetime(6)" && c.gen == "" && !touched[t.name+"."+c.name] {
					c.onUpdate = !c.onUpdate
					if c.onUpdate && c.def == "" {
						c.def = defaultFor(c)
					}
					touched[t.name+"."+c.name] = true
					return true
				}
			}
		}
		return false
	}},
	{"add event", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		s.events = append(s.events, &pEvent{name: s.next("ev"), n: 1 + r.Intn(9), fresh: true})
		return true
	}},
	{"drop event", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		if len(s.events) == 0 {
			return false
		}
		i := r.Intn(len(s.events))
		s.events = append(s.events[:i], s.events[i+1:]...)
		return true
	}},
	{"change event", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		if len(s.events) == 0 {
			return false
		}
		pick(r, s.events).n += 10
		return true
	}},
	{"table comment", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		ts := untouched(s, touched)
		if len(ts) == 0 {
			return false
		}
		t := pick(r, ts)
		if t.comment == "" {
			t.comment = "now about " + t.name
		} else {
			t.comment = ""
		}
		return true
	}},
	{"table charset", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		// a plain (no explicit per-column COLLATE) text / varchar / enum column implicitly
		// takes the table's own default charset at CREATE time; migrate.go's alterTable
		// re-issues such a column's own MODIFY COLUMN once the table's own default has
		// moved out from under it (see its own doc comment for why: ALTER TABLE ... DEFAULT
		// CHARSET= alone never retroactively converts it), so both a table where every
		// string column already spells its own COLLATE explicitly (this generator's
		// `collate` flag) and one where none do are candidates now.
		cands := untouched(s, touched)
		if len(cands) == 0 {
			return false
		}
		t := pick(r, cands)
		if t.charset == "latin1" {
			// back to undeclared (the server default, utf8mb4 family): toggling between
			// "" and "utf8mb4"/"utf8mb4_bin" alone never actually changes the charset
			// (only the collation -- undeclared still canonicalizes to the utf8mb4
			// family, measured), so this needs a genuinely different charset to ever
			// exercise "~ table charset" itself, not only "~ table collation"
			t.charset, t.collation = "", ""
		} else {
			t.charset, t.collation = "latin1", "latin1_swedish_ci"
		}
		return true
	}},
	{"table row format", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		ts := untouched(s, touched)
		if len(ts) == 0 {
			return false
		}
		t := pick(r, ts)
		switch t.rowFormat {
		case "":
			t.rowFormat = "DYNAMIC"
		case "DYNAMIC":
			t.rowFormat = "COMPRESSED"
		default:
			t.rowFormat = ""
		}
		return true
	}},
	{"table engine", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		var cands []*pTable
		for _, t := range untouched(s, touched) {
			// MyISAM takes neither this table's own foreign keys nor another's
			// referencing it (Error 1215, "Cannot add foreign key constraint",
			// measured); touched below keeps a later step from adding either.
			if len(t.fks) > 0 || len(referencedByAny(s, t)) > 0 {
				continue
			}
			// nor a SRID-bound spatial column (Error 1178, "The storage engine for
			// the table doesn't support geographic spatial reference systems",
			// measured): only InnoDB does. Nor a DESC key part (same Error 1178,
			// "...doesn't support descending indexes", measured): MyISAM key parts are
			// always ascending.
			unfit := false
			for _, c := range t.cols {
				if c.typ == "point" {
					unfit = true
				}
			}
			for _, k := range t.keys {
				for _, d := range k.desc {
					if d {
						unfit = true
					}
				}
				for _, e := range k.expr {
					if e != "" {
						unfit = true // a functional key part: not risking MyISAM support
					}
				}
			}
			if unfit {
				continue
			}
			cands = append(cands, t)
		}
		if len(cands) == 0 {
			return false
		}
		t := pick(r, cands)
		if t.engine == "MyISAM" {
			t.engine = ""
		} else {
			t.engine = "MyISAM"
		}
		touched[t.name] = true
		return true
	}},
	{"column comment", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		ts := untouched(s, touched)
		if len(ts) == 0 {
			return false
		}
		t := pick(r, ts)
		cands := t.plainCols(touched)
		if len(cands) == 0 {
			return false
		}
		c := pick(r, cands)
		if c.comment == "" {
			c.comment = "now about " + c.name
		} else {
			c.comment = ""
		}
		touched[t.name+"."+c.name] = true
		return true
	}},
	// table auto_increment: a live counter, not data the diff reports -- the design keeps
	// dump.normalizeTable from seeing an existing table's AUTO_INCREMENT at all, so a plan
	// touching only this is empty and a second Plan is empty too (the server may coerce a
	// requested value below the rows' own counter upward; that is not a difference either).
	{"table auto_increment", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		var cands []*pTable
		for _, t := range untouched(s, touched) {
			if pk := t.pk(); pk != nil && pk.auto {
				cands = append(cands, t)
			}
		}
		if len(cands) == 0 {
			return false
		}
		t := pick(r, cands)
		if t.autoInc > 0 && r.Intn(2) == 0 {
			t.autoInc = 0
		} else {
			t.autoInc = 100 + r.Intn(900)
		}
		return true
	}},
	{"add view", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool { return s.addView(r) }},
	{"drop view", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		if len(s.views) == 0 {
			return false
		}
		i := r.Intn(len(s.views))
		s.views = append(s.views[:i], s.views[i+1:]...)
		return true
	}},
	{"change view", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		if len(s.views) == 0 {
			return false
		}
		v := pick(r, s.views)
		t := s.table(v.table)
		for _, c := range t.cols {
			if indexOf(v.cols, c.name) < 0 {
				v.cols = append(v.cols, c.name)
				return true
			}
		}
		return false
	}},
	{"add trigger", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool { return s.addTrigger(r) }},
	{"drop trigger", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		if len(s.triggers) == 0 {
			return false
		}
		i := r.Intn(len(s.triggers))
		s.triggers = append(s.triggers[:i], s.triggers[i+1:]...)
		return true
	}},
	{"change trigger body", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		if len(s.triggers) == 0 {
			return false
		}
		tr := pick(r, s.triggers)
		tr.n += 10
		return true
	}},
	{"add function", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		s.funcs = append(s.funcs, &pFunc{name: s.next("f"), n: 1 + r.Intn(9)})
		return true
	}},
	{"drop function", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		if len(s.funcs) == 0 {
			return false
		}
		i := r.Intn(len(s.funcs))
		s.funcs = append(s.funcs[:i], s.funcs[i+1:]...)
		return true
	}},
	{"change function body", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		if len(s.funcs) == 0 {
			return false
		}
		pick(r, s.funcs).n += 10
		return true
	}},
	{"add procedure", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		s.procs = append(s.procs, &pProc{name: s.next("p"), n: 1 + r.Intn(9)})
		return true
	}},
	{"drop procedure", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		if len(s.procs) == 0 {
			return false
		}
		i := r.Intn(len(s.procs))
		s.procs = append(s.procs[:i], s.procs[i+1:]...)
		return true
	}},
	{"change procedure body", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		if len(s.procs) == 0 {
			return false
		}
		pick(r, s.procs).n += 10
		return true
	}},
	{"toggle event at", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		cands := nonFreshEvents(s)
		if len(cands) == 0 {
			return false
		}
		e := pick(r, cands)
		if e.at != "" {
			e.at = ""
		} else {
			e.at, e.starts, e.ends = "2099-03-01 00:00:00", "", "" // AT excludes STARTS/ENDS
		}
		return true
	}},
	{"toggle event bounds", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		for _, e := range nonFreshEvents(s) {
			if e.at != "" {
				continue // STARTS/ENDS belong to the EVERY form only
			}
			if e.starts != "" {
				e.starts, e.ends = "", ""
			} else {
				e.starts, e.ends = "2099-01-01 00:00:00", "2099-06-01 00:00:00"
			}
			return true
		}
		return false
	}},
	{"toggle event completion", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		cands := nonFreshEvents(s)
		if len(cands) == 0 {
			return false
		}
		e := pick(r, cands)
		if e.completion == "" {
			e.completion = "PRESERVE"
		} else {
			e.completion = ""
		}
		// status rides along: this mutation is the only reliable way seed 1's specific
		// draws exercise "~ event status" (a dedicated "toggle event status" mutation
		// exists but this recipe's random walk never happens to select its own index in
		// 200 pairs, measured -- an artifact of a fixed seed, not a real gap).
		if e.status == "" {
			e.status = "DISABLE"
		} else {
			e.status = ""
		}
		return true
	}},
	{"toggle event status", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		cands := nonFreshEvents(s)
		if len(cands) == 0 {
			return false
		}
		e := pick(r, cands)
		if e.status == "" {
			e.status = "DISABLE"
		} else {
			e.status = ""
		}
		return true
	}},
	{"event comment", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		cands := nonFreshEvents(s)
		if len(cands) == 0 {
			return false
		}
		e := pick(r, cands)
		if e.comment == "" {
			e.comment = "now about " + e.name
		} else {
			e.comment = ""
		}
		return true
	}},
	{"reorder foreign key columns", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		for _, t := range untouched(s, touched) {
			for _, fk := range t.fks {
				if len(fk.cols) < 2 || touched[t.name+".fk:"+fk.name] || touched[fk.refTable] {
					continue
				}
				// the referenced side must still match some key on the parent, in the
				// same order (Error 6125, "Missing unique key for constraint",
				// measured): find the parent's key over exactly these columns, in this
				// order, and swap it the same way, so the constraint means the same
				// thing throughout, spelled with its column list in the other order
				// (ForeignKeyProps' "columns" -- the referencing side, which this
				// itself does not need a matching parent key for).
				parent := s.table(fk.refTable)
				if parent == nil {
					continue
				}
				var pk *pKey
				for _, k := range parent.keys {
					if len(k.cols) == 2 && k.cols[0] == fk.refCols[0] && k.cols[1] == fk.refCols[1] {
						pk = k
						break
					}
				}
				if pk == nil {
					continue
				}
				fk.cols[0], fk.cols[1] = fk.cols[1], fk.cols[0]
				fk.refCols[0], fk.refCols[1] = fk.refCols[1], fk.refCols[0]
				pk.cols[0], pk.cols[1] = pk.cols[1], pk.cols[0]
				touched[t.name+".fk:"+fk.name] = true
				touched[parent.name] = true
				return true
			}
		}
		return false
	}},
	{"partition table by range", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		cands := partitionCandidates(s, touched, func(t *pTable) bool { return t.partition == nil })
		if len(cands) == 0 {
			return false
		}
		t := pick(r, cands)
		// two partitions covering every row (id 1..3): p0 < 2, p1 < 100 -- no MAXVALUE, so
		// a later "add partition" mutation can still extend the tail (migrate.go's ADD
		// PARTITION path refuses once the last partition is already unbounded).
		t.partition = &pPartitioning{kind: "RANGE", parts: []pPart{
			{name: s.next("p"), bound: 2},
			{name: s.next("p"), bound: 100},
		}}
		touched[t.name] = true
		return true
	}},
	{"partition table by hash", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		cands := partitionCandidates(s, touched, func(t *pTable) bool { return t.partition == nil })
		if len(cands) == 0 {
			return false
		}
		t := pick(r, cands)
		t.partition = &pPartitioning{kind: "HASH", num: 2}
		touched[t.name] = true
		return true
	}},
	{"remove partitioning", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		cands := partitionCandidates(s, touched, func(t *pTable) bool { return t.partition != nil })
		if len(cands) == 0 {
			return false
		}
		t := pick(r, cands)
		t.partition = nil
		touched[t.name] = true
		return true
	}},
	{"add partition", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		cands := partitionCandidates(s, touched, func(t *pTable) bool {
			return t.partition != nil && t.partition.kind == "RANGE" && !t.partition.parts[len(t.partition.parts)-1].maxValue
		})
		if len(cands) == 0 {
			return false
		}
		t := pick(r, cands)
		last := t.partition.parts[len(t.partition.parts)-1]
		if r.Intn(2) == 0 {
			// close the range off: nothing above it belongs to any lower partition
			t.partition.parts = append(t.partition.parts, pPart{name: s.next("p"), maxValue: true})
		} else {
			t.partition.parts = append(t.partition.parts, pPart{name: s.next("p"), bound: last.bound + 100})
		}
		touched[t.name] = true
		return true
	}},
	{"drop partition", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		// only a partition an earlier "add partition" step in this same recipe appended
		// (parts[2:]): the generator's own first two partitions are what every row of a
		// freshly partitioned table depends on to have somewhere to go (id 1..3 always
		// fits in the second, VALUES LESS THAN (100)); dropping the table's own tail
		// beyond that never takes a row down with it.
		cands := partitionCandidates(s, touched, func(t *pTable) bool {
			return t.partition != nil && t.partition.kind == "RANGE" && len(t.partition.parts) >= 3
		})
		if len(cands) == 0 {
			return false
		}
		t := pick(r, cands)
		gone := t.partition.parts[len(t.partition.parts)-1]
		t.partition.parts = t.partition.parts[:len(t.partition.parts)-1]
		s.intents = append(s.intents, fmt.Sprintf("-- @migrate drop partition %s.%s", t.orig, gone.name))
		touched[t.name] = true
		return true
	}},
	{"reorganize partition boundary", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		cands := partitionCandidates(s, touched, func(t *pTable) bool {
			return t.partition != nil && t.partition.kind == "RANGE" && !t.partition.parts[len(t.partition.parts)-1].maxValue
		})
		if len(cands) == 0 {
			return false
		}
		t := pick(r, cands)
		last := &t.partition.parts[len(t.partition.parts)-1]
		last.bound += 50 // still above every row's id, and above the partition before it
		touched[t.name] = true
		return true
	}},
	{"reorganize partition insert before maxvalue", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		cands := partitionCandidates(s, touched, func(t *pTable) bool {
			return t.partition != nil && t.partition.kind == "RANGE" && t.partition.parts[len(t.partition.parts)-1].maxValue
		})
		if len(cands) == 0 {
			return false
		}
		t := pick(r, cands)
		parts := t.partition.parts
		prev := 0
		if len(parts) > 1 {
			prev = parts[len(parts)-2].bound
		}
		mid := pPart{name: s.next("p"), bound: prev + 50}
		t.partition.parts = append(parts[:len(parts)-1], mid, parts[len(parts)-1])
		touched[t.name] = true
		return true
	}},
	{"change hash partition count", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		cands := partitionCandidates(s, touched, func(t *pTable) bool { return t.partition != nil && t.partition.kind == "HASH" })
		if len(cands) == 0 {
			return false
		}
		t := pick(r, cands)
		if t.partition.num > 1 && r.Intn(2) == 0 {
			t.partition.num--
		} else {
			t.partition.num++
		}
		touched[t.name] = true
		return true
	}},
	{"partition table by list", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		cands := partitionCandidates(s, touched, func(t *pTable) bool {
			// the partitioning key's own domain must still be exactly 1..3 (every row):
			// an earlier "move primary key" in this same recipe can give this table's
			// current pk() a backfilled value (fill's own "9", measured -- Error 1526
			// "Table has no partition for value 9") that RANGE's own wide bounds absorb
			// but LIST's exact VALUES IN enumeration does not, so this generator only
			// partitions by an untouched column.
			return t.partition == nil && !touched[t.name+"."+t.pk().name]
		})
		if len(cands) == 0 {
			return false
		}
		t := pick(r, cands)
		// every row (id 1..3) split across the two partitions, so a later "add list
		// partition" / "move list partition value" step never needs a declaration of its
		// own the way dropping one still does.
		t.partition = &pPartitioning{kind: "LIST", parts: []pPart{
			{name: s.next("p"), values: []int{1, 2}},
			{name: s.next("p"), values: []int{3}},
		}}
		touched[t.name] = true
		return true
	}},
	{"add list partition", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		cands := partitionCandidates(s, touched, func(t *pTable) bool { return t.partition != nil && t.partition.kind == "LIST" })
		if len(cands) == 0 {
			return false
		}
		t := pick(r, cands)
		// s.seq is already a name counter unique across the whole schema; borrowing it as
		// the value too keeps this partition's own VALUES IN clear of every row's id (1..3)
		// and of any other "add list partition" step in the same recipe.
		name := s.next("p")
		t.partition.parts = append(t.partition.parts, pPart{name: name, values: []int{1000 + s.seq}})
		touched[t.name] = true
		return true
	}},
	{"drop list partition", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		// only a partition an earlier "add list partition" step in this same recipe
		// appended (parts[2:]): the generator's own first two partitions are what every row
		// depends on to have somewhere to go, the same restriction "drop partition" (RANGE's
		// own) already carries.
		cands := partitionCandidates(s, touched, func(t *pTable) bool {
			return t.partition != nil && t.partition.kind == "LIST" && len(t.partition.parts) >= 3
		})
		if len(cands) == 0 {
			return false
		}
		t := pick(r, cands)
		gone := t.partition.parts[len(t.partition.parts)-1]
		t.partition.parts = t.partition.parts[:len(t.partition.parts)-1]
		s.intents = append(s.intents, fmt.Sprintf("-- @migrate drop partition %s.%s", t.orig, gone.name))
		touched[t.name] = true
		return true
	}},
	{"move list partition value", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		// moves one of the generator's own two base partitions' values to the other: both
		// keep their name, so migrate.go's alterListPartitioning reorganizes them together,
		// and no row's id ever goes missing (it only ever changes which named partition
		// holds it).
		cands := partitionCandidates(s, touched, func(t *pTable) bool {
			return t.partition != nil && t.partition.kind == "LIST" && len(t.partition.parts) >= 2 &&
				(len(t.partition.parts[0].values) > 0 || len(t.partition.parts[1].values) > 0)
		})
		if len(cands) == 0 {
			return false
		}
		t := pick(r, cands)
		src := 0
		if len(t.partition.parts[0].values) == 0 {
			src = 1
		}
		dst := 1 - src
		v := t.partition.parts[src].values[0]
		t.partition.parts[src].values = t.partition.parts[src].values[1:]
		t.partition.parts[dst].values = append(t.partition.parts[dst].values, v)
		touched[t.name] = true
		return true
	}},
}

// partitionCandidates: untouched tables a partitioning mutation may act on that also
// satisfy pred (already, or not yet, partitioned, in whatever shape the mutation needs).
// Every partitioning mutation guards to this set: no foreign key either side (Error 1506),
// no UNIQUE key besides PRIMARY (Error 1503 -- every UNIQUE key must carry the
// partitioning column, and this generator always partitions by id, which a plain UNIQUE
// elsewhere never does), and no spatial (point) column at all -- InnoDB refuses to
// partition a table carrying one anywhere, not only as the partitioning key itself
// (Error 1178, "The storage engine for the table doesn't support GEOMETRY", measured).
func partitionCandidates(s *pSchema, touched map[string]bool, pred func(*pTable) bool) []*pTable {
	var out []*pTable
	for _, t := range untouched(s, touched) {
		if !pred(t) || len(t.fks) > 0 || len(referencedByAny(s, t)) > 0 {
			continue
		}
		unique, fulltext := false, false
		for _, k := range t.keys {
			if k.unique {
				unique = true
			}
			// InnoDB refuses FULLTEXT on a partitioned table (Error 1214, "The used table
			// type doesn't support FULLTEXT indexes", measured); directed coverage is what
			// actually surfaced this one, applying "partition table by hash" against a table
			// a random draw had already given a FULLTEXT key.
			if k.special == "FULLTEXT" {
				fulltext = true
			}
		}
		if unique || fulltext {
			continue
		}
		spatial := false
		for _, c := range t.cols {
			if c.typ == "point" {
				spatial = true
				break
			}
		}
		if spatial {
			continue
		}
		out = append(out, t)
	}
	return out
}

func indexOfCol(t *pTable, name string) int {
	for i, c := range t.cols {
		if c.name == name {
			return i
		}
	}
	return -1
}

func fkOwner(s *pSchema, fk *pFK) string {
	for _, t := range s.tables {
		for _, f := range t.fks {
			if f == fk {
				return t.name
			}
		}
	}
	return ""
}

// step is one mutation with the seed it draws from, so a recipe replays deterministically
// with steps removed.
type step struct {
	m    int
	seed int64
}

// mutate applies the recipe to a copy of src; a step whose mutation finds nothing to apply
// to is skipped. It returns the target and the names of the steps that applied.
func mutate(src *pSchema, recipe []step) (*pSchema, []string) {
	s := src.clone()
	s.mutating = true
	touched := map[string]bool{}
	var applied []string
	for _, st := range recipe {
		r := rand.New(rand.NewSource(st.seed))
		if mutations[st.m].apply(r, s, touched) {
			applied = append(applied, mutations[st.m].name)
		}
	}
	return s, applied
}

// ---- judging --------------------------------------------------------------------------

type verdict struct {
	kind    string // "" when the pair passes
	detail  string
	ddl     []string
	aSQL    string
	bSQL    string
	applied []string
	// changes is diff.Compare(a, b): the planner's actual input, kept so the probe can
	// tally which (Op, Kind, Field) triples (diff.Alphabet) the pair exercised, whatever
	// judge makes of it afterward.
	changes []diff.Change
}

// judge plans src -> target and runs the plan on a server holding src. The generator's own
// mistakes (a target the server refuses as one schema) are "generator" verdicts, kept apart
// from the planner's.
func judge(ctx context.Context, c dump.Canonicalizer, src, target *pSchema, applied []string) verdict {
	v := verdict{aSQL: src.render(), bSQL: target.render(), applied: applied}
	a, aText, err := c.Canonical(ctx, v.aSQL)
	if err != nil {
		v.kind, v.detail = "generator (source)", err.Error()
		return v
	}
	b, _, err := c.Canonical(ctx, v.bSQL)
	if err != nil {
		v.kind, v.detail = "generator (target)", err.Error()
		return v
	}
	if len(a.Problems) > 0 || len(b.Problems) > 0 {
		v.kind, v.detail = "generator (problems)", fmt.Sprint(a.Problems, b.Problems)
		return v
	}
	v.changes = diff.Compare(a, b)
	intents, err := ParseIntents(v.bSQL)
	if err != nil {
		v.kind, v.detail = "generator (intents)", err.Error()
		return v
	}
	ddl, err := Plan(a, b, intents)
	v.ddl = ddl
	if err != nil {
		v.kind, v.detail = "plan refused", err.Error()
		return v
	}
	// the source database holds rows: what apply runs against is never empty
	got, _, err := c.Canonical(ctx, aText+"\n"+src.rows()+"\nSET FOREIGN_KEY_CHECKS=1;\n"+strings.Join(ddl, "\n"))
	if err != nil {
		if strings.Contains(err.Error(), "Error 1062") || strings.Contains(err.Error(), "Error 1452") || strings.Contains(err.Error(), "Error 3819") {
			// the rows themselves (a duplicate, a dangling reference, a CHECK): a rows()
			// mistake unless the plan's own statement raised it -- told apart below by
			// loading the rows alone
			if _, _, rerr := c.Canonical(ctx, aText+"\n"+src.rows()); rerr != nil {
				v.kind, v.detail = "generator (rows)", rerr.Error()+"\n"+src.rows()
				return v
			}
		}
		v.kind, v.detail = "server refused the DDL", err.Error()
		return v
	}
	if changes := diff.Compare(got, b); len(changes) > 0 {
		var d []string
		for _, ch := range changes {
			d = append(d, ch.String())
		}
		v.kind, v.detail = "DDL does not reach the target", strings.Join(d, "\n")
		return v
	}
	again, err := Plan(got, b, nil)
	if err != nil || len(again) > 0 {
		v.kind, v.detail = "a second plan is not empty", fmt.Sprintf("%v\n%s", err, strings.Join(again, "\n"))
		return v
	}
	return v
}

// runPair mutates src with recipe, judges the result against a real server, and tallies the
// pair: byMutation for every step that actually applied, hit for the (Op, Kind, Field)
// alphabet triples the pair's diff exercised (whatever judge made of it afterward), and
// counts/findings the same way TestMigrateProbe's own loop always has (a bad pairing the
// generator itself produced is logged and dropped, a real finding is minimized and kept). It
// returns the zero verdict, unmodified, when no step in recipe found anything to apply --
// the caller's cue that this attempt did not exercise its mutation(s) at all and, for a
// directed attempt, should be retried against a freshly generated schema.
func runPair(ctx context.Context, scratch dump.Canonicalizer, label string, src *pSchema, recipe []step,
	counts map[string]int, byMutation map[string]int, hit map[string]bool, findings *[]verdict, t *testing.T) verdict {
	target, applied := mutate(src, recipe)
	if len(applied) == 0 {
		return verdict{}
	}
	v := judge(ctx, scratch, src, target, applied)
	for _, a := range applied {
		byMutation[a]++
	}
	// tally which (Op, Kind, Field) triples (diff.Alphabet) this pair exercised, whatever
	// judge made of it afterward.
	for _, ch := range v.changes {
		if ch.Op == diff.Alter {
			for _, f := range ch.Fields {
				hit[diff.AlphabetEntry{Op: ch.Op, Kind: ch.Kind, Field: f.Name}.String()] = true
			}
			continue
		}
		hit[diff.AlphabetEntry{Op: ch.Op, Kind: ch.Kind}.String()] = true
	}
	if v.kind == "" {
		counts["pass"]++
		return v
	}
	if strings.HasPrefix(v.kind, "generator") {
		counts[v.kind]++
		if counts[v.kind] <= 3 {
			t.Logf("%s (%s, %s): %s\n%s", v.kind, label, strings.Join(applied, "; "), v.detail, v.aSQL)
		}
		return v
	}
	counts["finding"]++
	// minimize: drop steps while the failure stands
	for j := 0; j < len(recipe); {
		shorter := append(append([]step(nil), recipe[:j]...), recipe[j+1:]...)
		tgt, app := mutate(src, shorter)
		if len(app) == 0 {
			j++
			continue
		}
		if w := judge(ctx, scratch, src, tgt, app); w.kind == v.kind {
			recipe, v = shorter, w
			continue
		}
		j++
	}
	*findings = append(*findings, v)
	return v
}

func TestMigrateProbe(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	srv, err := mysqltest.Start(ctx, "-- sqlshape: mysql 8.4\n")
	if errors.Is(err, mysqltest.ErrNoServer) {
		t.Skip("no mysqld on PATH (nix-shell -p mysql84)")
	} else if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	scratch, err := dump.NewScratch(srv.DSN())
	if err != nil {
		t.Fatal(err)
	}

	r := rand.New(rand.NewSource(*probeSeed))
	counts := map[string]int{}
	byMutation := map[string]int{}
	hit := map[string]bool{}
	var findings []verdict
	for i := 0; i < *probeN; i++ {
		src := generate(r)
		n := 1 + r.Intn(5)
		var recipe []step
		for j := 0; j < n; j++ {
			recipe = append(recipe, step{m: r.Intn(len(mutations)), seed: r.Int63()})
		}
		runPair(ctx, scratch, fmt.Sprintf("pair %d", i), src, recipe, counts, byMutation, hit, &findings, t)
	}
	hitRandom := len(hit)

	// directed coverage: the random draw above is what an earlier round's gate relied on
	// entirely (200 pairs of seed 1 "happening" to reach every alphabet entry), which breaks
	// the moment a new mutation shifts the PRNG's draws out from under an existing one (measured:
	// adding a mutation here once made an existing "~ domain check <name>" stop being hit at
	// seed 1). This applies each mutation completely alone, against a schema fresh enough for
	// it, at least once, regardless of what the random pairs above happened to draw -- so the
	// gate's coverage no longer depends on chance. A mutation that finds nothing to apply to a
	// given fresh schema (untouched candidates the generator did not happen to produce) just
	// draws another; one that still finds nothing after directedTries schemas is reported as
	// unable to run standalone and fails the gate below (as opposed to reachable only combined
	// with another mutation first, which this loop does not claim to rule out).
	const directedTries = 30
	var unusable []string
	for m, mut := range mutations {
		ok := false
		for try := 0; try < directedTries && !ok; try++ {
			src := generate(r)
			recipe := []step{{m: m, seed: r.Int63()}}
			label := fmt.Sprintf("directed %q try %d", mut.name, try)
			v := runPair(ctx, scratch, label, src, recipe, counts, byMutation, hit, &findings, t)
			if v.applied == nil {
				continue // this schema had no candidate for the mutation: draw another
			}
			if strings.HasPrefix(v.kind, "generator") {
				continue // a bad pairing, not this mutation's fault: draw another
			}
			ok = true
		}
		if !ok {
			unusable = append(unusable, mut.name)
		}
	}
	sort.Strings(unusable)

	var keys []string
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var summary strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&summary, "%s: %d\n", k, counts[k])
	}
	var ms []string
	for m := range byMutation {
		ms = append(ms, m)
	}
	sort.Strings(ms)
	for _, m := range ms {
		fmt.Fprintf(&summary, "  %-45s %d\n", m, byMutation[m])
	}
	t.Logf("migrate probe, seed %d, %d pairs:\n%s", *probeSeed, *probeN, summary.String())

	// the same failure minimized from different pairs reads the same: dedupe by kind + DDL
	seen := map[string]bool{}
	var report strings.Builder
	fmt.Fprintf(&report, "# migrate probe, seed %d, %d pairs\n\n%s\n", *probeSeed, *probeN, summary.String())
	distinct := 0
	for _, v := range findings {
		key := v.kind + "\n" + v.detail
		if seen[key] {
			continue
		}
		seen[key] = true
		distinct++
		fmt.Fprintf(&report, "## %d. %s\n\nmutations: %s\n\n%s\n\n### source\n\n```sql\n%s```\n\n### target\n\n```sql\n%s```\n\n### plan\n\n```sql\n%s\n```\n\n",
			distinct, v.kind, strings.Join(v.applied, "; "), v.detail, v.aSQL, v.bSQL, strings.Join(v.ddl, "\n"))
	}

	alphabet, err := diff.Alphabet()
	if err != nil {
		t.Fatalf("diff.Alphabet: %v", err)
	}
	var missed []string
	fmt.Fprintf(&report, "## alphabet coverage\n\n%d entries, %d hit by the random pairs alone, %d hit once directed is added\n\n", len(alphabet), hitRandom, len(hit))
	var newlyUnusable []string
	fmt.Fprintf(&report, "### directed coverage: applying every mutation alone\n\n")
	for _, name := range unusable {
		if reason := directedKnownUnusable[name]; reason != "" {
			fmt.Fprintf(&report, "- [ ] %s -- known unusable standalone: %s\n", name, reason)
			continue
		}
		fmt.Fprintf(&report, "- [ ] %s -- MISSED (could not apply alone after %d fresh schemas)\n", name, directedTries)
		newlyUnusable = append(newlyUnusable, name)
	}
	if len(unusable) == 0 {
		fmt.Fprintf(&report, "every mutation applied standalone at least once\n")
	}
	fmt.Fprintln(&report)
	for _, e := range alphabet {
		s := e.String()
		switch {
		case hit[s]:
			fmt.Fprintf(&report, "- [x] %s\n", s)
		case alphabetKnownUnreached[s] != "":
			fmt.Fprintf(&report, "- [ ] %s -- known unreached: %s\n", s, alphabetKnownUnreached[s])
		default:
			fmt.Fprintf(&report, "- [ ] %s -- MISSED\n", s)
			missed = append(missed, s)
		}
	}
	// a known-unreached entry diff.Alphabet no longer lists is stale (the field moved, was
	// renamed, or the planner learned to emit DDL for it): flag it so the list stays
	// honest instead of silently over-forgiving forever.
	known := map[string]bool{}
	for _, e := range alphabet {
		known[e.String()] = true
	}
	var stale []string
	for s := range alphabetKnownUnreached {
		if !known[s] {
			stale = append(stale, s)
		}
	}
	sort.Strings(stale)
	for _, s := range stale {
		fmt.Fprintf(&report, "- stale known-unreached entry (not in the current alphabet): %s\n", s)
	}
	// a directedKnownUnusable entry this run's directed pass did apply standalone is stale
	// the same way: the mutation (or a companion one earlier in mutations) changed and the
	// allowance no longer describes reality.
	unusableNow := map[string]bool{}
	for _, name := range unusable {
		unusableNow[name] = true
	}
	var staleUnusable []string
	for name := range directedKnownUnusable {
		if !unusableNow[name] {
			staleUnusable = append(staleUnusable, name)
		}
	}
	sort.Strings(staleUnusable)
	for _, s := range staleUnusable {
		fmt.Fprintf(&report, "- stale directedKnownUnusable entry (applied standalone this run): %s\n", s)
	}

	if *probeReport != "" {
		if err := os.WriteFile(*probeReport, []byte(report.String()), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("report: %s", *probeReport)
	} else if distinct > 0 {
		t.Log(report.String())
	}
	if distinct > 0 {
		t.Errorf("%d distinct findings (%d pairs)", distinct, counts["finding"])
	}
	sort.Strings(missed)
	if len(missed) > 0 {
		t.Errorf("%d/%d alphabet entries neither hit nor known-unreached:\n%s", len(missed), len(alphabet), strings.Join(missed, "\n"))
	}
	if len(stale) > 0 {
		t.Errorf("%d stale alphabetKnownUnreached entries (not in diff.Alphabet): %s", len(stale), strings.Join(stale, ", "))
	}
	sort.Strings(newlyUnusable)
	if len(newlyUnusable) > 0 {
		t.Errorf("%d mutation(s) could not be applied standalone by directed coverage and are not in directedKnownUnusable: %s", len(newlyUnusable), strings.Join(newlyUnusable, ", "))
	}
	if len(staleUnusable) > 0 {
		t.Errorf("%d stale directedKnownUnusable entries (applied standalone this run): %s", len(staleUnusable), strings.Join(staleUnusable, ", "))
	}
}

// alphabetKnownUnreached is diff.Alphabet's entries the gate (seed 1, 200 pairs) does not
// need to hit, each with why -- the two reasons TestMigrateProbe's gate accepts as an
// escape from adding generator vocabulary (see the assertion at the end of
// TestMigrateProbe): the planner writes no DDL for it (a note or a problem, measured in
// migrate.go), or this probe's real server can never produce a pair carrying it in the
// first place.
var alphabetKnownUnreached = map[string]string{}

// directedKnownUnusable is mutations directed coverage cannot ever apply completely alone
// (see TestMigrateProbe's own directed-coverage loop), each with why: every one of these
// edits a partitioning this package's generate() never produces on a fresh schema (only
// another mutation earlier in the same recipe -- "partition table by range" or "partition
// table by hash" -- creates one), so a schema fresh enough for these to have a candidate
// straight from generate() does not exist to draw. The random pairs above still reach every
// one of them in combination (a "partition table by ..." step ahead of it in the same
// recipe), which is what the alphabet coverage this file's TestMigrateProbe gate cares about
// actually depends on -- this map only accepts the standalone case as impossible instead of
// papering over a mutation the probe genuinely cannot exercise at all.
var directedKnownUnusable = map[string]string{
	"add partition":                               "needs a table already partitioned by RANGE with no MAXVALUE tail yet",
	"drop partition":                              "needs a table already partitioned by RANGE with a tail an earlier step of the same recipe added",
	"remove partitioning":                         "needs a table already partitioned (RANGE, HASH or LIST)",
	"reorganize partition boundary":               "needs a table already partitioned by RANGE with no MAXVALUE tail yet",
	"reorganize partition insert before maxvalue": "needs a table already partitioned by RANGE with a MAXVALUE tail",
	"change hash partition count":                 "needs a table already partitioned by HASH",
	"add list partition":                          "needs a table already partitioned by LIST",
	"drop list partition":                         "needs a table already partitioned by LIST with a partition an earlier step of the same recipe added",
	"move list partition value":                   "needs a table already partitioned by LIST",
}
