package migrate

import (
	"fmt"
	"strings"
)

// ---- the model: a schema the generator can render and mutate --------------------------

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

// pPartitioning is a table's PARTITION BY clause: RANGE (id) [COLUMNS], an ordered list of
// parts, [LINEAR] HASH (id) PARTITIONS n, [LINEAR] KEY (id) [ALGORITHM=n] PARTITIONS n, or
// LIST (id), an unordered list of parts (each own value set, not a boundary). Always over
// the id column (see pTable.partition's own comment). columns is RANGE's own COLUMNS
// variant -- a single-column RANGE COLUMNS (id) spells VALUES LESS THAN the same way a
// plain RANGE (id) does (measured), so this reuses parts/render as they stand, only the
// clause's own header differs; sub is the table's own SUBPARTITION BY, RANGE only (this
// generator never gives HASH/KEY or LIST one).
type pPartitioning struct {
	kind      string  // "RANGE", "HASH", "KEY" or "LIST"
	linear    bool    // LINEAR HASH / LINEAR KEY
	columns   bool    // RANGE COLUMNS
	algorithm int     // KEY's own ALGORITHM=1|2, 0 for none written
	num       int     // HASH/KEY's PARTITIONS n
	parts     []pPart // RANGE's ordered partitions, or LIST's own (order is cosmetic there)
	sub       *pSubPartitioning
}

// pSubPartitioning is a RANGE Partitioning's own SUBPARTITION BY clause: every partition
// split further by HASH, to the same count every time (this generator never names a single
// subpartition -- see schema.SubPartitioning's own doc comment for why the loader does not
// model that either).
type pSubPartitioning struct {
	num int // SUBPARTITIONS n
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
			if t.partition.sub != nil {
				nsub := *t.partition.sub
				np.sub = &nsub
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
	linear := ""
	if p.linear {
		linear = "LINEAR "
	}
	if p.kind == "KEY" {
		algo := ""
		if p.algorithm != 0 {
			algo = fmt.Sprintf("ALGORITHM=%d ", p.algorithm)
		}
		return fmt.Sprintf("PARTITION BY %sKEY %s(%s)\nPARTITIONS %d", linear, algo, q(col), p.num)
	}
	if p.kind == "HASH" {
		return fmt.Sprintf("PARTITION BY %sHASH (%s)\nPARTITIONS %d", linear, q(col), p.num)
	}
	if p.kind == "LIST" {
		defs := make([]string, len(p.parts))
		for i, part := range p.parts {
			defs[i] = fmt.Sprintf("PARTITION %s VALUES IN (%s)", q(part.name), intList(part.values))
		}
		return fmt.Sprintf("PARTITION BY LIST (%s)\n(%s)", q(col), strings.Join(defs, ",\n "))
	}
	columns := ""
	if p.columns {
		columns = "COLUMNS "
	}
	defs := make([]string, len(p.parts))
	for i, part := range p.parts {
		if part.maxValue {
			defs[i] = "PARTITION " + q(part.name) + " VALUES LESS THAN MAXVALUE"
		} else {
			defs[i] = fmt.Sprintf("PARTITION %s VALUES LESS THAN (%d)", q(part.name), part.bound)
		}
	}
	var sub strings.Builder
	if p.sub != nil {
		fmt.Fprintf(&sub, "SUBPARTITION BY HASH (%s)\nSUBPARTITIONS %d\n", q(col), p.sub.num)
	}
	return fmt.Sprintf("PARTITION BY RANGE %s(%s)\n%s(%s)", columns, q(col), sub.String(), strings.Join(defs, ",\n "))
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
