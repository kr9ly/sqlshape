package migrate

// The migrate probe, PostgreSQL side: the same oracle check/mysql/migrate has (see its
// probe_test.go for the reasoning). Schemas are generated from the planner's vocabulary
// (column types, an ENUM type, identity and generated columns, unique constraints and
// indexes, foreign keys, CHECKs, table comments, views, functions, trigger functions with
// their triggers), mutated into a target (columns added / dropped / renamed / widened /
// re-nulled, constraints and indexes attached and detached, the primary key moved onto an
// existing or a new column, tables added / dropped / renamed, ENUM labels added and dropped,
// views, triggers and functions added / dropped / changed, generated expressions changed),
// and judged the way `sqlshape apply` runs the plan: the DDL applied to a database holding
// the source must read back as the target, column order aside (PostgreSQL cannot move a
// column; Verify reports order as notes), and a second plan from there must be empty.
//
//	go test ./migrate -run TestMigrateProbe [-migrate-probe-n 200] [-migrate-probe-seed 1] \
//	    [-migrate-probe-report /path/report.md]
//
// Skipped without pg_dump on PATH (nix-shell -p postgresql_17).

import (
	"context"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/kr9ly/sqlshape/check/postgres/v2/diff"
	"github.com/kr9ly/sqlshape/check/postgres/v2/dump"
)

var (
	probeN      = flag.Int("migrate-probe-n", 200, "schema pairs the migrate probe judges")
	probeSeed   = flag.Int64("migrate-probe-seed", 1, "seed of the migrate probe's generator")
	probeReport = flag.String("migrate-probe-report", "", "write the migrate probe's findings here (default: the test log)")
)

// ---- the model --------------------------------------------------------------------------

type pSchema struct {
	domains    []*pDomain
	enums      []*pEnum
	composites []*pComposite
	ranges     []*pRange
	tables     []*pTable
	partTables []*pPartTable
	views      []*pView
	triggers   []*pTrigger
	funcs      []*pFunc
	policies   []*pPolicy
	rules      []*pRule
	seqs       []*pSeq
	triggerFns []*pTriggerFn
	seedTable  *pSeedTable // at most one, for simplicity: see its own doc comment
	extensions []string
	intents    []string
	// checks are extra DO $$ ... $$ assertions a mutation wants run right after the plan's
	// own DDL, against the same live database, before judge() compares schemas -- so far
	// only "shrink partition bound" uses this, to confirm a partitioned table's total row
	// count survives a row actually being moved between partitions (diff.Compare alone
	// only ever sees schema shape, never data).
	checks   []string
	seq      int
	mutating bool // set on a clone: the tables that exist are the source's, and hold rows
	// pg18: this schema targets PostgreSQL 18 (TestMigrateProbe18), so render() declares
	// `-- sqlshape: postgres 18` (schema.DeclaredVersion defaults to 17 otherwise, which
	// analyze.Load -- Canonical's own seeds path when called with nil, judge()'s case --
	// would use to parse this text, refusing any PG18-only syntax a mutation wrote).
	pg18 bool
}

// pRange is a standalone range type over integer (CREATE TYPE name AS RANGE (subtype =
// integer)): never used as a column type -- only its own presence (+/-) is in the
// planner's vocabulary here (an ALTER of its subtype is note-only, see
// alphabetKnownUnreached), so it does not need one.
type pRange struct{ name string }

// pPartTable is a small, self-contained RANGE- or LIST-partitioned table: a parent
// (PARTITION BY RANGE (id) / LIST (kind)) with two ordinary partitions and (usually) a
// DEFAULT one -- brief-partition-pg.md's vocabulary items 1-4 (create / drop the whole
// thing, a partition added or removed, DETACH / ATTACH, a bound moved). Deliberately
// narrow: no key, no index, no foreign key, nothing else references it -- pTable already
// exercises that vocabulary against ordinary tables (items 5-6, propagation to a
// partition's own columns and keys resting on the partition key, are instead measured by
// hand against the planner directly; see the round's report for why this generator does
// not fold partitioning into pTable itself).
type pPartTable struct {
	name     string
	orig     string // the source schema's qualified name (a declaration's left side)
	strategy string // "RANGE" or "LIST"
	parts    []*pPartChild
}

// pPartChild is one partition (or, once detached, a plain standalone table that used to
// be one): RANGE bound is [lo, hi), LIST bound is one or more values, and a DEFAULT
// partition has neither.
type pPartChild struct {
	name      string
	orig      string
	isDefault bool
	lo, hi    int      // RANGE bound; 0 for LIST / DEFAULT
	values    []string // LIST bound; nil for RANGE / DEFAULT
	detached  bool     // stands alone now: rendered as a plain CREATE TABLE, no PARTITION OF
	gone      bool     // dropped outright (declared, like pTable's own drop)
}

// bound is the FOR VALUES clause (or DEFAULT) attached to a direct CREATE TABLE ...
// PARTITION OF, or ALTER TABLE ... ATTACH PARTITION -- the same text either way.
func (c *pPartChild) bound() string {
	switch {
	case c.isDefault:
		return "DEFAULT"
	case len(c.values) > 0:
		var q []string
		for _, v := range c.values {
			q = append(q, "'"+v+"'")
		}
		return "FOR VALUES IN (" + strings.Join(q, ", ") + ")"
	default:
		return fmt.Sprintf("FOR VALUES FROM (%d) TO (%d)", c.lo, c.hi)
	}
}

func (pt *pPartTable) child(name string) *pPartChild {
	for _, c := range pt.parts {
		if c.name == name {
			return c
		}
	}
	return nil
}

func (pt *pPartTable) full() string { return "public." + pt.name }
func (c *pPartChild) full() string  { return "public." + c.name }

// pRule is a CREATE RULE ... AS ON INSERT TO table [WHERE (id > 0)] DO INSTEAD NOTHING:
// a no-op rule (every row still holds id > 0, so the predicate is never false) whose
// presence, WHERE clause and enabled flag are the planner's rule vocabulary.
type pRule struct {
	name    string
	table   string
	where   bool
	enabled bool
}

// pSeq is a standalone CREATE SEQUENCE, optionally OWNED BY an existing integer column
// (never one already owning a bigserial / IDENTITY sequence of its own): "set sequence
// owner" / "clear sequence owner" toggle the OWNED BY clause on an existing sequence.
type pSeq struct {
	name  string
	table string // "" (unowned) or an owning table
	col   string
}

// pSeedTable is a standalone lookup table (schema.Seed): a CREATE TABLE the schema text
// itself gives fixed content via an INSERT right after it (`-- sqlshape: seed` for an
// additive one). It is a completely separate model from the probe's own rows() /
// s.value(): those exist only to give the migrated database something to hold when the
// plan runs; a seed's rows are declared CONTENT the migration keeps in step with the
// schema text, diffed by RowChanges ("~ rows row <key>"), not backfilled or inserted by
// the probe. Kept to at most one per schema (pSchema.seedTable) and to a plain two
// column (code, label) shape for simplicity.
type pSeedTable struct {
	name     string
	rows     []pSeedRow
	additive bool
}

type pSeedRow struct {
	code, label string
}

type pEnum struct {
	name   string
	labels []string
	fresh  bool // minted by a mutation (a fresh table's or column's): not in the source, so
	// "drop enum label" (which writes an @migrate declaration naming it) must leave it alone
}

// pDomain is a domain over integer, CHECK (VALUE > 0) or, once changed, (VALUE >= 0).
type pDomain struct {
	name    string
	ge      bool
	notNull bool
}

type pTable struct {
	schema      string // public or app
	name        string
	orig        string // the source schema's qualified name for the table (a declaration's left side)
	cols        []*pCol
	keys        []*pKey
	fks         []*pFK
	checks      []*pCheck
	comment     string
	rowSec      bool // ENABLE ROW LEVEL SECURITY
	forceRowSec bool // FORCE ROW LEVEL SECURITY (only meaningful with rowSec)
}

type pCol struct {
	name       string
	typ        string // integer, bigint, text, varchar(50), numeric(10,2), ..., or an enum type's name
	notNull    bool
	def        string
	pk         bool
	identity   bool
	serial     bool   // bigserial: a sequence and a nextval default
	gen        string // the column this generated column reads
	genVirtual bool   // VIRTUAL instead of STORED (PostgreSQL 18 only; mutations18 alone sets this)
	fresh      bool   // added by a mutation: not in the source, so its drop needs no declaration
	comment    string
	idAlways   bool // GENERATED ALWAYS (else BY DEFAULT) AS IDENTITY, when identity is set
}

type pKey struct {
	name    string
	unique  bool // a UNIQUE constraint; else a CREATE INDEX (unless exclude)
	cols    []string
	notDist bool   // UNIQUE NULLS NOT DISTINCT (unique only)
	defer_  bool   // DEFERRABLE INITIALLY DEFERRED (unique only)
	where   string // a partial index's predicate, rendered over cols[0] (plain index only): "notnull" or "positive"
	expr    string // an expression index's sole element over cols[0] (plain index only): "lower" or "plus1"
	exclude bool   // CONSTRAINT name EXCLUDE USING btree (cols[0] WITH =): unique in the row
	// sense, over an integer column, but declared as a table constraint rather than a
	// CREATE INDEX / UNIQUE
	opNE bool // WITH <> instead of =: cols[0] must then be a column every row shares one
	// value in (a fresh column's own default), since <> forbids any two rows differing
	usingGist bool // USING gist instead of btree (needs the btree_gist extension)
	// withoutOverlaps (unique only): the last column (a range type) is WITHOUT OVERLAPS,
	// a temporal UNIQUE (PostgreSQL 18 only; mutations18 alone sets this).
	withoutOverlaps bool
}

type pFK struct {
	name        string
	cols        []string
	refTable    string
	refCols     []string
	onDelete    string
	onUpdate    string
	defer_      bool // DEFERRABLE INITIALLY DEFERRED
	notEnforced bool // NOT ENFORCED (PostgreSQL 18 only; mutations18 alone sets this)
}

type pCheck struct {
	name        string
	col         string
	ge          bool // col >= 0 rather than col > 0 (both hold for every generated row)
	notEnforced bool // NOT ENFORCED (PostgreSQL 18 only; mutations18 alone sets this)
}

// pComposite is a composite type, CREATE TYPE name AS (attrs...). Kept out of every table's
// columns once its attributes are mutated (ALTER TYPE ADD/DROP/ALTER ATTRIBUTE on a
// composite type a table column uses needs CASCADE and a view rebuild the probe does not
// attempt yet; see design notes) -- a table may still gain or drop a column of the type
// as it stands.
type pComposite struct {
	name  string
	attrs []*pAttr
}

type pAttr struct {
	name string
	typ  string
}

// pPolicy is a row-level security policy. Its predicate (both USING and WITH CHECK, when
// present) is always "the table's primary key column > 0": true for every row the probe
// generates, so the policy never hides or refuses a row the probe itself writes.
type pPolicy struct {
	name       string
	table      string
	command    string // ALL, SELECT, INSERT, UPDATE, DELETE
	permissive bool
	using      bool
	withCheck  bool
	role       bool // TO CURRENT_USER (a pseudo-role that always exists) rather than PUBLIC
}

type pView struct {
	name        string
	table       string
	cols        []string
	mat         bool   // a materialized view
	checkOption string // "", "LOCAL" or "CASCADED" (plain views only)
}

type pTrigger struct {
	name     string
	table    string
	col      string
	n        int
	onUpdate bool   // also fires BEFORE UPDATE OF col (not just INSERT)
	callName string // the pTriggerFn (by name) this trigger EXECUTEs -- its own, at
	// creation; "change trigger function" repoints it to another trigger's function on
	// the same table. A pTriggerFn is never removed when the trigger that made it is
	// (it may still be called), so this can never dangle.
}

// pTriggerFn is a trigger function, kept independent of the pTrigger that first made it
// so dropping that trigger cannot leave another one's callName pointing at a function
// that stops being created (measured: 42883 "function ... does not exist").
type pTriggerFn struct {
	name string // the function is name + "_fn"
	col  string
	n    int
}

type pFunc struct {
	name       string
	n          int
	retType    string // "integer" or "bigint"
	strict     bool
	volatility string // "IMMUTABLE" or "STABLE"
	argDefault bool   // the sole argument gets " DEFAULT 1" (arguments text changes; same signature)
	lang       string // "sql" or "plpgsql", plain functions only
	kind       string // "" (plain function), "procedure", "aggregate" or "window": all
	// keep the same signature (a single integer argument) as a plain function, so
	// converting one into another is a same-name Alter, not a drop-and-recreate.
}

func (s *pSchema) next(prefix string) string {
	s.seq++
	return fmt.Sprintf("%s%d", prefix, s.seq)
}

func (s *pSchema) clone() *pSchema {
	c := &pSchema{seq: s.seq, pg18: s.pg18}
	for _, d := range s.domains {
		nd := *d
		c.domains = append(c.domains, &nd)
	}
	for _, e := range s.enums {
		c.enums = append(c.enums, &pEnum{name: e.name, labels: append([]string(nil), e.labels...), fresh: e.fresh})
	}
	for _, co := range s.composites {
		nco := &pComposite{name: co.name}
		for _, a := range co.attrs {
			na := *a
			nco.attrs = append(nco.attrs, &na)
		}
		c.composites = append(c.composites, nco)
	}
	for _, t := range s.tables {
		nt := &pTable{schema: t.schema, name: t.name, orig: t.orig, comment: t.comment, rowSec: t.rowSec, forceRowSec: t.forceRowSec}
		for _, col := range t.cols {
			nc := *col
			nt.cols = append(nt.cols, &nc)
		}
		for _, k := range t.keys {
			nk := *k
			nk.cols = append([]string(nil), k.cols...)
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
	for _, pt := range s.partTables {
		npt := &pPartTable{name: pt.name, orig: pt.orig, strategy: pt.strategy}
		for _, ch := range pt.parts {
			nch := *ch
			nch.values = append([]string(nil), ch.values...)
			npt.parts = append(npt.parts, &nch)
		}
		c.partTables = append(c.partTables, npt)
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
	for _, p := range s.policies {
		np := *p
		c.policies = append(c.policies, &np)
	}
	for _, rg := range s.ranges {
		nrg := *rg
		c.ranges = append(c.ranges, &nrg)
	}
	for _, ru := range s.rules {
		nru := *ru
		c.rules = append(c.rules, &nru)
	}
	for _, sq := range s.seqs {
		nsq := *sq
		c.seqs = append(c.seqs, &nsq)
	}
	for _, fn := range s.triggerFns {
		nfn := *fn
		c.triggerFns = append(c.triggerFns, &nfn)
	}
	if s.seedTable != nil {
		nst := *s.seedTable
		nst.rows = append([]pSeedRow(nil), s.seedTable.rows...)
		c.seedTable = &nst
	}
	c.extensions = append([]string(nil), s.extensions...)
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

// full is the table's qualified name as a declaration writes it.
func (t *pTable) full() string { return t.schema + "." + t.name }

// ref is the table's qualified name as DDL writes it.
func (t *pTable) ref() string { return qi(t.schema) + "." + qi(t.name) }

// qref is the DDL spelling of the table named name.
func (s *pSchema) qref(name string) string {
	if t := s.table(name); t != nil {
		return t.ref()
	}
	return qi(name)
}

func (s *pSchema) enum(name string) *pEnum {
	for _, e := range s.enums {
		if e.name == name {
			return e
		}
	}
	return nil
}

func (s *pSchema) composite(name string) *pComposite {
	for _, c := range s.composites {
		if c.name == name {
			return c
		}
	}
	return nil
}

// compositeInUse reports whether any table column, anywhere in the schema, has the
// composite type name -- used to keep ALTER TYPE ... ATTRIBUTE and DROP TYPE mutations off
// a composite type a table is still using (see pComposite's doc comment).
func compositeInUse(s *pSchema, name string) bool { return typeInUse(s, name) }

// typeInUse reports whether any table column, anywhere in the schema, has type name --
// used to keep "drop" mutations for a domain / enum / composite type off one a column
// still carries.
func typeInUse(s *pSchema, name string) bool {
	for _, t := range s.tables {
		for _, c := range t.cols {
			if c.typ == name {
				return true
			}
		}
	}
	return false
}

// compositeArityFrozen reports whether some column's own DEFAULT is a ROW(...)::name
// literal (a NOT NULL composite column defaultFor gave one, over existing rows) --
// adding or dropping an attribute would leave that literal's value count stale for the
// changed type ("cannot cast type record to <name>", measured), so ADD / DROP ATTRIBUTE
// (which change the attribute count) stay off it; ALTER ATTRIBUTE TYPE does not change
// the count and is unaffected.
func compositeArityFrozen(s *pSchema, name string) bool {
	for _, t := range s.tables {
		for _, c := range t.cols {
			if c.typ == name && c.def != "" {
				return true
			}
		}
	}
	return false
}

func (t *pTable) col(name string) *pCol {
	for _, c := range t.cols {
		if c.name == name {
			return c
		}
	}
	return nil
}

func (t *pTable) pk() *pCol {
	for _, c := range t.cols {
		if c.pk {
			return c
		}
	}
	return nil
}

func (t *pTable) inFK(col string) bool {
	for _, fk := range t.fks {
		if indexOf(fk.cols, col) >= 0 {
			return true
		}
	}
	return false
}

// ---- the type vocabulary ----------------------------------------------------------------

var probeTypes = []string{"integer", "bigint", "smallint", "text", "varchar(50)", "numeric(10,2)", "double precision", "timestamptz", "date", "boolean", "jsonb", "enum", "domain", "composite"}

func isNumeric(typ string) bool {
	switch typ {
	case "integer", "bigint", "smallint", "numeric(10,2)", "numeric(14,2)", "double precision":
		return true
	}
	return strings.HasPrefix(typ, "num") // a domain over integer
}

func isInteger(typ string) bool { return typ == "integer" || typ == "bigint" || typ == "smallint" }

func (s *pSchema) defaultFor(c *pCol) string {
	switch c.typ {
	case "integer", "bigint", "smallint", "double precision":
		return "1"
	case "numeric(10,2)", "numeric(14,2)":
		return "1.00"
	case "text":
		return "'x'"
	case "varchar(50)", "varchar(100)":
		return "'x'"
	case "timestamptz":
		return "now()"
	case "date":
		return "'2020-01-01'"
	case "boolean":
		return "false"
	case "jsonb":
		return "'{}'"
	}
	if e := s.enum(c.typ); e != nil {
		return "'" + e.labels[0] + "'"
	}
	if co := s.composite(c.typ); co != nil {
		return co.literal(func(a *pAttr) string {
			if isNumeric(a.typ) {
				return "1"
			}
			return "'x'"
		})
	}
	if strings.HasPrefix(c.typ, "num") {
		return "1"
	}
	return ""
}

func widen(typ string) string {
	switch typ {
	case "integer":
		return "bigint"
	case "smallint":
		return "integer"
	case "varchar(50)":
		return "varchar(100)"
	case "numeric(10,2)":
		return "numeric(14,2)"
	}
	return ""
}

// ---- rendering --------------------------------------------------------------------------

func qi(name string) string { return `"` + name + `"` }

func qilist(names []string) string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = qi(n)
	}
	return strings.Join(out, ", ")
}

func (c *pCol) render() string {
	var b strings.Builder
	typ := c.typ
	if c.serial {
		typ = "bigserial"
	}
	b.WriteString(qi(c.name) + " " + typ)
	if c.gen != "" {
		kind := "STORED"
		if c.genVirtual {
			kind = "VIRTUAL"
		}
		b.WriteString(" GENERATED ALWAYS AS (" + qi(c.gen) + " + 1) " + kind)
		return b.String()
	}
	if c.notNull {
		b.WriteString(" NOT NULL")
	}
	if c.identity {
		kind := "BY DEFAULT"
		if c.idAlways {
			kind = "ALWAYS"
		}
		b.WriteString(" GENERATED " + kind + " AS IDENTITY")
	} else if c.def != "" {
		b.WriteString(" DEFAULT " + c.def)
	}
	return b.String()
}

func (t *pTable) render(s *pSchema) string {
	var parts []string
	for _, c := range t.cols {
		parts = append(parts, "  "+c.render())
	}
	if pk := t.pk(); pk != nil {
		parts = append(parts, "  CONSTRAINT "+qi(t.name+"_pkey")+" PRIMARY KEY ("+qi(pk.name)+")")
	}
	for _, k := range t.keys {
		if k.unique {
			u := "  CONSTRAINT " + qi(k.name) + " UNIQUE"
			if k.notDist {
				u += " NULLS NOT DISTINCT"
			}
			cols := qilist(k.cols)
			if k.withoutOverlaps && len(k.cols) > 0 {
				cols = qilist(k.cols[:len(k.cols)-1])
				if cols != "" {
					cols += ", "
				}
				cols += qi(k.cols[len(k.cols)-1]) + " WITHOUT OVERLAPS"
			}
			u += " (" + cols + ")"
			if k.defer_ {
				u += " DEFERRABLE INITIALLY DEFERRED"
			}
			parts = append(parts, u)
		}
		if k.exclude {
			method, op := "btree", "="
			if k.usingGist || k.opNE {
				// btree has no "<>" member in its integer operator family (42809,
				// measured); btree_gist's gist opclass does
				method = "gist"
			}
			if k.opNE {
				op = "<>"
			}
			s := "  CONSTRAINT " + qi(k.name) + " EXCLUDE USING " + method + " (" + qi(k.cols[0]) + " WITH " + op + ")"
			if k.where == "positive" {
				s += " WHERE (" + qi(k.cols[0]) + " > 0)"
			}
			parts = append(parts, s)
		}
	}
	for _, fk := range t.fks {
		s := "  CONSTRAINT " + qi(fk.name) + " FOREIGN KEY (" + qilist(fk.cols) + ") REFERENCES " + s.qref(fk.refTable) + " (" + qilist(fk.refCols) + ")"
		if fk.onDelete != "" {
			s += " ON DELETE " + fk.onDelete
		}
		if fk.onUpdate != "" {
			s += " ON UPDATE " + fk.onUpdate
		}
		if fk.defer_ {
			s += " DEFERRABLE INITIALLY DEFERRED"
		}
		if fk.notEnforced {
			s += " NOT ENFORCED"
		}
		parts = append(parts, s)
	}
	for _, ck := range t.checks {
		c := t.col(ck.col)
		op := ">"
		if ck.ge {
			op = ">="
		}
		expr := "(" + qi(ck.col) + " " + op + " 0)"
		if !isNumeric(c.typ) {
			expr = "(length(" + qi(ck.col) + ") > 0)"
		}
		s := "  CONSTRAINT " + qi(ck.name) + " CHECK " + expr
		if ck.notEnforced {
			s += " NOT ENFORCED"
		}
		parts = append(parts, s)
	}
	var b strings.Builder
	b.WriteString("CREATE TABLE " + t.ref() + " (\n" + strings.Join(parts, ",\n") + "\n);\n")
	for _, k := range t.keys {
		if !k.unique && !k.exclude {
			b.WriteString(k.renderIndex(t))
		}
	}
	if t.comment != "" {
		b.WriteString("COMMENT ON TABLE " + t.ref() + " IS '" + t.comment + "';\n")
	}
	for _, c := range t.cols {
		if c.comment != "" {
			b.WriteString("COMMENT ON COLUMN " + t.ref() + "." + qi(c.name) + " IS '" + c.comment + "';\n")
		}
	}
	if t.rowSec {
		b.WriteString("ALTER TABLE " + t.ref() + " ENABLE ROW LEVEL SECURITY;\n")
		if t.forceRowSec {
			b.WriteString("ALTER TABLE " + t.ref() + " FORCE ROW LEVEL SECURITY;\n")
		}
	}
	return b.String()
}

// render is the parent's CREATE TABLE ... PARTITION BY ..., then each surviving
// partition: PARTITION OF for one still attached, a plain CREATE TABLE for one detached
// (pPartChild.detached); a gone one (dropped outright, declared) writes nothing.
func (pt *pPartTable) render() string {
	col, keyword := "id", "RANGE"
	if pt.strategy == "LIST" {
		col, keyword = "kind", "LIST"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "CREATE TABLE %s (id integer NOT NULL, kind text NOT NULL) PARTITION BY %s (%s);\n", qi(pt.name), keyword, qi(col))
	for _, c := range pt.parts {
		if c.gone {
			continue
		}
		if c.detached {
			b.WriteString("CREATE TABLE " + qi(c.name) + " (id integer NOT NULL, kind text NOT NULL);\n")
			continue
		}
		b.WriteString("CREATE TABLE " + qi(c.name) + " PARTITION OF " + qi(pt.name) + " " + c.bound() + ";\n")
	}
	return b.String()
}

// renderIndex is a plain CREATE INDEX: a partial predicate over cols[0] ("notnull" /
// "positive", both true for every row a table holds), or an expression over cols[0]
// ("lower" for a text-like column, "plus1" for a numeric one) in place of the column list.
func (k *pKey) renderIndex(t *pTable) string {
	elems := qilist(k.cols)
	if k.expr != "" {
		c := t.col(k.cols[0])
		switch k.expr {
		case "lower":
			elems = "(lower(" + qi(c.name) + "))"
		case "plus1":
			elems = "((" + qi(c.name) + " + 1))"
		}
	}
	s := "CREATE INDEX " + qi(k.name) + " ON " + t.ref() + " (" + elems + ")"
	switch k.where {
	case "notnull":
		s += " WHERE (" + qi(k.cols[0]) + " IS NOT NULL)"
	case "positive":
		s += " WHERE (" + qi(k.cols[0]) + " > 0)"
	}
	return s + ";\n"
}

// render is a CREATE POLICY over the table's primary key column ("id > 0", true for
// every row the probe inserts, so a policy's predicate never hides or refuses a row the
// probe itself wrote, whatever ENABLE / FORCE the table carries).
func (pol *pPolicy) render(s *pSchema) string {
	t := s.table(pol.table)
	pred := qi(t.pk().name) + " > 0"
	b := "CREATE POLICY " + qi(pol.name) + " ON " + s.qref(pol.table)
	if !pol.permissive {
		b += " AS RESTRICTIVE"
	}
	if pol.command != "ALL" {
		b += " FOR " + pol.command
	}
	if pol.role {
		b += " TO CURRENT_USER"
	}
	if pol.using {
		b += " USING (" + pred + ")"
	}
	if pol.withCheck {
		b += " WITH CHECK (" + pred + ")"
	}
	return b + ";\n"
}

// render is CREATE RULE ... AS ON INSERT TO table [WHERE (id > 0)] DO ALSO NOTHING, then
// DISABLE RULE when the rule is off. DO ALSO (not DO INSTEAD): INSTEAD replaces the
// original INSERT with nothing, which would silently drop every row the probe inserts
// (id > 0 always holds) -- measured, "generator (rows)" against a foreign key that then
// has no parent row to reference.
func (ru *pRule) render(s *pSchema) string {
	t := s.table(ru.table)
	b := "CREATE RULE " + qi(ru.name) + " AS ON INSERT TO " + s.qref(ru.table)
	if ru.where {
		b += " WHERE (" + qi(t.pk().name) + " > 0)"
	}
	b += " DO ALSO NOTHING;\n"
	if !ru.enabled {
		b += "ALTER TABLE " + t.ref() + " DISABLE RULE " + qi(ru.name) + ";\n"
	}
	return b
}

// render is CREATE TABLE (code text PRIMARY KEY, label text NOT NULL) and the seeding
// INSERT right after it, `-- sqlshape: seed` ahead of it when additive.
func (st *pSeedTable) render() string {
	var b strings.Builder
	fmt.Fprintf(&b, "CREATE TABLE %s (code text PRIMARY KEY, label text NOT NULL);\n", qi(st.name))
	if len(st.rows) == 0 {
		return b.String()
	}
	if st.additive {
		b.WriteString("-- sqlshape: seed\n")
	}
	var vals []string
	for _, row := range st.rows {
		vals = append(vals, fmt.Sprintf("('%s', '%s')", row.code, row.label))
	}
	fmt.Fprintf(&b, "INSERT INTO %s (code, label) VALUES %s;\n", qi(st.name), strings.Join(vals, ", "))
	return b.String()
}

// render is CREATE FUNCTION, its RETURNS type, STRICT and volatility, and the sole
// argument's DEFAULT, all varying while the signature (the argument's type) stays put --
// so a change to any of them is a same-name Alter, not a drop-and-recreate.
func (f *pFunc) render() string {
	arg := "a integer"
	if f.argDefault {
		arg += " DEFAULT 1"
	}
	strict := ""
	if f.strict {
		strict = " STRICT"
	}
	switch f.kind {
	case "procedure":
		// a procedure body cannot use RETURN <value> (it returns nothing): a read-only
		// SELECT is the closest no-op equivalent of a plain function's body
		return fmt.Sprintf("CREATE PROCEDURE %s(%s) LANGUAGE sql AS $$ SELECT a + %d $$;\n", qi(f.name), arg, f.n)
	case "aggregate":
		// keeps the same (integer) signature as a plain function: sfunc / stype only,
		// no LANGUAGE or body -- CREATE AGGREGATE's own grammar
		return fmt.Sprintf("CREATE AGGREGATE %s(integer) (sfunc = int4pl, stype = integer);\n", qi(f.name))
	case "window":
		// LANGUAGE internal WINDOW binds to a real builtin fmgr symbol (window_row_number,
		// the C function behind row_number()); PostgreSQL does not check the declared
		// signature against the C function it names ("there is no built-in function named
		// row_number": the SQL-visible row_number() and its underlying fmgr symbol are
		// spelled differently), so the usual single-integer-argument shape still applies
		// and this stays a same-name Alter like procedure / aggregate
		return fmt.Sprintf("CREATE FUNCTION %s(%s) RETURNS %s LANGUAGE internal WINDOW AS 'window_row_number';\n", qi(f.name), arg, f.retType)
	}
	switch f.lang {
	case "plpgsql":
		return fmt.Sprintf("CREATE FUNCTION %s(%s) RETURNS %s LANGUAGE plpgsql %s%s AS $$ BEGIN RETURN a + %d; END $$;\n",
			qi(f.name), arg, f.retType, f.volatility, strict, f.n)
	default:
		return fmt.Sprintf("CREATE FUNCTION %s(%s) RETURNS %s LANGUAGE sql %s%s RETURN a + %d;\n",
			qi(f.name), arg, f.retType, f.volatility, strict, f.n)
	}
}

func (s *pSchema) render() string {
	var b strings.Builder
	if s.pg18 {
		b.WriteString("-- sqlshape: postgres 18\n")
	}
	for _, in := range s.intents {
		b.WriteString(in + "\n")
	}
	for _, t := range s.tables {
		if t.schema != "public" {
			b.WriteString("CREATE SCHEMA " + qi(t.schema) + ";\n")
			break
		}
	}
	for _, e := range s.extensions {
		b.WriteString("CREATE EXTENSION " + qi(e) + ";\n")
	}
	for _, d := range s.domains {
		op := ">"
		if d.ge {
			op = ">="
		}
		b.WriteString("CREATE DOMAIN " + qi(d.name) + " AS integer")
		if d.notNull {
			b.WriteString(" NOT NULL")
		}
		b.WriteString(" CHECK (VALUE " + op + " 0);\n")
	}
	for _, e := range s.enums {
		quoted := make([]string, len(e.labels))
		for i, l := range e.labels {
			quoted[i] = "'" + l + "'"
		}
		b.WriteString("CREATE TYPE " + qi(e.name) + " AS ENUM (" + strings.Join(quoted, ", ") + ");\n")
	}
	for _, co := range s.composites {
		var attrs []string
		for _, a := range co.attrs {
			attrs = append(attrs, qi(a.name)+" "+a.typ)
		}
		b.WriteString("CREATE TYPE " + qi(co.name) + " AS (" + strings.Join(attrs, ", ") + ");\n")
	}
	for _, rg := range s.ranges {
		b.WriteString("CREATE TYPE " + qi(rg.name) + " AS RANGE (subtype = integer);\n")
	}
	for _, t := range s.tables {
		b.WriteString(t.render(s))
	}
	for _, pt := range s.partTables {
		b.WriteString(pt.render())
	}
	if st := s.seedTable; st != nil {
		b.WriteString(st.render())
	}
	for _, pol := range s.policies {
		b.WriteString(pol.render(s))
	}
	for _, ru := range s.rules {
		b.WriteString(ru.render(s))
	}
	for _, sq := range s.seqs {
		// a sequence must live in the same schema as the table that owns it (55000,
		// measured): render it under its owner's schema rather than always "public"
		schema := "public"
		if sq.table != "" {
			schema = s.table(sq.table).schema
		}
		b.WriteString("CREATE SEQUENCE " + qi(schema) + "." + qi(sq.name) + ";\n")
		if sq.table != "" {
			b.WriteString("ALTER SEQUENCE " + qi(schema) + "." + qi(sq.name) + " OWNED BY " + s.qref(sq.table) + "." + qi(sq.col) + ";\n")
		}
	}
	for _, v := range s.views {
		kind := "VIEW"
		if v.mat {
			kind = "MATERIALIZED VIEW"
		}
		b.WriteString("CREATE " + kind + " " + qi(v.name) + " AS SELECT " + qilist(v.cols) + " FROM " + s.qref(v.table))
		if v.checkOption != "" {
			b.WriteString(" WITH " + v.checkOption + " CHECK OPTION")
		}
		b.WriteString(";\n")
	}
	for _, f := range s.funcs {
		b.WriteString(f.render())
	}
	for _, fn := range s.triggerFns {
		fmt.Fprintf(&b, "CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN NEW.%s := coalesce(NEW.%s, 0) + %d; RETURN NEW; END $$;\n",
			qi(fn.name+"_fn"), qi(fn.col), qi(fn.col), fn.n)
	}
	for _, tr := range s.triggers {
		events := "INSERT"
		if tr.onUpdate {
			events = "INSERT OR UPDATE OF " + qi(tr.col)
		}
		fmt.Fprintf(&b, "CREATE TRIGGER %s BEFORE %s ON %s FOR EACH ROW EXECUTE FUNCTION %s();\n", qi(tr.name), events, s.qref(tr.table), qi(tr.callName+"_fn"))
	}
	return b.String()
}

// rows renders three rows per table, parents first: integer columns carry the row number
// (distinct, positive, a valid reference to a parent's id), a nullable column is NULL in the
// third row, an identity id is set explicitly (BY DEFAULT allows it) so children can
// reference it, generated columns are left to the server.
func (s *pSchema) rows() string {
	var b strings.Builder
	for _, t := range s.tables {
		var names []string
		var cols []*pCol
		for _, c := range t.cols {
			if c.gen == "" {
				names = append(names, qi(c.name))
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
				vals = append(vals, s.value(c, i))
			}
			rows = append(rows, "("+strings.Join(vals, ", ")+")")
		}
		fmt.Fprintf(&b, "INSERT INTO %s (%s) VALUES %s;\n", t.ref(), strings.Join(names, ", "), strings.Join(rows, ", "))
	}
	for _, pt := range s.partTables {
		b.WriteString(pt.rows())
	}
	return b.String()
}

// rows is one row per partition still attached (gone / detached ones get none: a
// detached partition is a plain table now, holding whatever it held while attached, which
// INSERT INTO the parent never reaches again), landing in it by construction: a RANGE
// child's id is its own lower bound (any DEFAULT partition's is a value comfortably above
// every bound this generator ever produces), a LIST child's kind is its own first value
// (a DEFAULT partition's is a value no non-default child here is ever given).
func (pt *pPartTable) rows() string {
	var b strings.Builder
	i := 0
	for _, c := range pt.parts {
		if c.gone || c.detached {
			continue
		}
		i++
		id, kind := 100000+i, "zz"
		if !c.isDefault {
			if pt.strategy == "LIST" {
				kind = c.values[0]
			} else {
				id = c.lo
			}
		}
		fmt.Fprintf(&b, "INSERT INTO %s (id, kind) VALUES (%d, '%s');\n", qi(pt.name), id, kind)
	}
	return b.String()
}

func (s *pSchema) value(c *pCol, i int) string {
	switch c.typ {
	case "integer", "bigint", "smallint", "double precision", "numeric(10,2)", "numeric(14,2)":
		return fmt.Sprint(i)
	case "text", "varchar(50)", "varchar(100)":
		return fmt.Sprintf("'v%d'", i)
	case "timestamptz":
		return fmt.Sprintf("'2024-01-%02d 00:00:00+00'", i)
	case "date":
		return fmt.Sprintf("'2024-01-%02d'", i)
	case "boolean":
		return fmt.Sprint(i%2 == 1)
	case "jsonb":
		return fmt.Sprintf(`'{"i": %d}'`, i)
	}
	if e := s.enum(c.typ); e != nil {
		return "'" + e.labels[(i-1)%len(e.labels)] + "'"
	}
	if co := s.composite(c.typ); co != nil {
		return co.literal(func(a *pAttr) string { return attrValue(a, i) })
	}
	if strings.HasPrefix(c.typ, "num") {
		return fmt.Sprint(i)
	}
	return "NULL"
}

// attrValue is a composite attribute's value in row i: distinct per row like a plain
// column's, so a unique key over a composite column (its attributes compare in order) holds.
func attrValue(a *pAttr, i int) string {
	if isNumeric(a.typ) {
		return fmt.Sprint(i)
	}
	return fmt.Sprintf("'v%d'", i)
}

// literal is "ROW(...)::name", f giving each attribute's value.
func (co *pComposite) literal(f func(a *pAttr) string) string {
	var vals []string
	for _, a := range co.attrs {
		vals = append(vals, f(a))
	}
	return "ROW(" + strings.Join(vals, ", ") + ")::" + qi(co.name)
}

// fill is the literal a backfill gives a column that turns NOT NULL under existing rows.
func (s *pSchema) fill(c *pCol) string {
	switch {
	case isNumeric(c.typ):
		return "9"
	case c.typ == "boolean":
		return "true"
	case c.typ == "jsonb":
		return "'{}'"
	case c.typ == "timestamptz":
		return "'2024-02-01 00:00:00+00'"
	case c.typ == "date":
		return "'2024-02-01'"
	}
	if e := s.enum(c.typ); e != nil {
		return "'" + e.labels[0] + "'"
	}
	if co := s.composite(c.typ); co != nil {
		return co.literal(func(a *pAttr) string {
			if isNumeric(a.typ) {
				return "9"
			}
			return "'fill'"
		})
	}
	return "'fill'"
}

// ---- generating -------------------------------------------------------------------------

func pick[T any](r *rand.Rand, list []T) T { return list[r.Intn(len(list))] }

func (s *pSchema) newCol(r *rand.Rand) *pCol {
	c := &pCol{name: s.next("c"), typ: pick(r, probeTypes)}
	if c.typ == "domain" {
		if len(s.domains) == 0 || r.Intn(3) == 0 {
			s.domains = append(s.domains, &pDomain{name: s.next("num")})
		}
		c.typ = pick(r, s.domains).name
	}
	if c.typ == "enum" {
		if len(s.enums) == 0 || r.Intn(3) == 0 {
			e := &pEnum{name: s.next("e"), labels: []string{"a", "b", "c"}[:2+r.Intn(2)], fresh: s.mutating}
			s.enums = append(s.enums, e)
		}
		c.typ = pick(r, s.enums).name
	}
	if c.typ == "composite" {
		if len(s.composites) == 0 || r.Intn(3) == 0 {
			s.composites = append(s.composites, s.newComposite(r))
		}
		c.typ = pick(r, s.composites).name
	}
	c.notNull = r.Intn(2) == 0
	if c.notNull || r.Intn(3) == 0 {
		c.def = s.defaultFor(c)
	}
	if r.Intn(5) == 0 {
		c.comment = "about " + c.name
	}
	return c
}

// newComposite is a two-attribute composite type, one numeric and one text-like: attrValue
// gives both a value that varies by row, so a unique key or a plain comparison over a
// composite column holds like it would over a plain one.
// ensureExtension adds name to the schema's declared extensions unless already there.
func (s *pSchema) ensureExtension(name string) {
	for _, e := range s.extensions {
		if e == name {
			return
		}
	}
	s.extensions = append(s.extensions, name)
}

func (s *pSchema) newComposite(r *rand.Rand) *pComposite {
	return &pComposite{name: s.next("ct"), attrs: []*pAttr{
		{name: "n", typ: "integer"},
		{name: "w", typ: "text"},
	}}
}

func (t *pTable) numericCols() []*pCol {
	var out []*pCol
	for _, c := range t.cols {
		if isNumeric(c.typ) && c.gen == "" && !c.identity {
			out = append(out, c)
		}
	}
	return out
}

func (s *pSchema) newTable(r *rand.Rand) *pTable {
	t := &pTable{name: s.next("t"), schema: pick(r, []string{"public", "public", "app"})}
	t.orig = t.full()
	id := &pCol{name: "id", typ: pick(r, []string{"integer", "bigint"}), notNull: true, pk: true}
	switch r.Intn(3) {
	case 0:
		id.identity = true
	case 1:
		id.typ, id.serial = "bigint", true
	}
	t.cols = append(t.cols, id)
	n := 2 + r.Intn(4)
	for i := 0; i < n; i++ {
		t.cols = append(t.cols, s.newCol(r))
	}
	if src := t.numericCols(); len(src) > 0 && r.Intn(2) == 0 {
		g := &pCol{name: s.next("g"), typ: "bigint", gen: pick(r, src).name}
		at := r.Intn(len(t.cols) + 1)
		t.cols = append(t.cols[:at], append([]*pCol{g}, t.cols[at:]...)...)
	}
	s.addKey(r, t)
	if r.Intn(3) == 0 {
		s.addCheck(r, t)
	}
	if r.Intn(2) == 0 {
		s.addExclude(r, t)
	}
	if r.Intn(4) == 0 {
		s.addPartialIndex(r, t)
	}
	if len(s.tables) > 0 {
		if r.Intn(4) == 0 {
			s.addCompositeFK(r, t, pick(r, s.tables))
		} else {
			s.addFK(r, t, pick(r, s.tables))
		}
	}
	if r.Intn(4) == 0 {
		t.comment = "about " + t.name
	}
	return t
}

// newPartTable makes a RANGE or LIST partitioned table with three partitions: two
// ordinary ones and a DEFAULT.
func (s *pSchema) newPartTable(r *rand.Rand) *pPartTable {
	name := s.next("part")
	pt := &pPartTable{name: name, orig: "public." + name, strategy: "RANGE"}
	if r.Intn(2) == 0 {
		pt.strategy = "LIST"
	}
	c1, c2 := s.next(name+"_"), s.next(name+"_")
	def := s.next(name + "_")
	if pt.strategy == "LIST" {
		pt.parts = []*pPartChild{
			{name: c1, orig: "public." + c1, values: []string{"a", "b"}},
			{name: c2, orig: "public." + c2, values: []string{"c"}},
			{name: def, orig: "public." + def, isDefault: true},
		}
	} else {
		pt.parts = []*pPartChild{
			{name: c1, orig: "public." + c1, lo: 1, hi: 10},
			{name: c2, orig: "public." + c2, lo: 10, hi: 20},
			{name: def, orig: "public." + def, isDefault: true},
		}
	}
	return pt
}

func (s *pSchema) addKey(r *rand.Rand, t *pTable) bool {
	unique := r.Intn(2) == 0
	var cands []string
	for _, c := range t.cols {
		// three rows: a boolean or an enum column repeats a value, so no unique key over it
		// (a column a mutation added holds one default in every row: no unique key over it)
		if c.typ != "jsonb" && !c.pk && !(unique && (c.typ == "boolean" || s.enum(c.typ) != nil || c.fresh)) {
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
	t.keys = append(t.keys, &pKey{name: s.next("k"), unique: unique, cols: cands[:n]})
	return true
}

// addPartialIndex is a plain CREATE INDEX WHERE (...): "col IS NOT NULL" over any column, or
// "col > 0" over a numeric one -- both true for every row a table holds, so the index never
// needs to exclude a row the probe wrote.
func (s *pSchema) addPartialIndex(r *rand.Rand, t *pTable) bool {
	var cands []*pCol
	for _, c := range t.cols {
		if !c.pk && c.typ != "jsonb" {
			cands = append(cands, c)
		}
	}
	if len(cands) == 0 {
		return false
	}
	c := pick(r, cands)
	where := "notnull"
	if isNumeric(c.typ) && r.Intn(2) == 0 {
		where = "positive"
	}
	t.keys = append(t.keys, &pKey{name: s.next("k"), cols: []string{c.name}, where: where})
	return true
}

// addExprIndex is a plain CREATE INDEX over a single expression: lower(col) for a text-like
// column, (col + 1) for a numeric one.
func (s *pSchema) addExprIndex(r *rand.Rand, t *pTable) bool {
	var textCands, numCands []*pCol
	for _, c := range t.cols {
		if c.pk || c.gen != "" {
			continue
		}
		switch {
		case c.typ == "text" || strings.HasPrefix(c.typ, "varchar"):
			textCands = append(textCands, c)
		case isNumeric(c.typ):
			numCands = append(numCands, c)
		}
	}
	switch {
	case len(textCands) > 0 && (len(numCands) == 0 || r.Intn(2) == 0):
		c := pick(r, textCands)
		t.keys = append(t.keys, &pKey{name: s.next("k"), cols: []string{c.name}, expr: "lower"})
	case len(numCands) > 0:
		c := pick(r, numCands)
		t.keys = append(t.keys, &pKey{name: s.next("k"), cols: []string{c.name}, expr: "plus1"})
	default:
		return false
	}
	return true
}

// addExclude is CONSTRAINT ... EXCLUDE USING btree (col WITH =) over an integer column: a
// row's own value is always distinct from every other row's (the row number), so it holds
// under the fixture the same way a unique key would -- a fresh column excluded for the same
// reason (every row shares its one default).
func (s *pSchema) addExclude(r *rand.Rand, t *pTable) bool {
	var cands []string
	for _, c := range t.cols {
		if isInteger(c.typ) && !c.pk && !c.fresh {
			cands = append(cands, c.name)
		}
	}
	if len(cands) == 0 {
		return false
	}
	t.keys = append(t.keys, &pKey{name: s.next("k"), cols: []string{pick(r, cands)}, exclude: true})
	return true
}

func (s *pSchema) addCheck(r *rand.Rand, t *pTable) bool {
	var cands []string
	for _, c := range t.cols {
		if !c.identity && !c.pk && (isNumeric(c.typ) || c.typ == "text" || strings.HasPrefix(c.typ, "varchar")) {
			cands = append(cands, c.name)
		}
	}
	if len(cands) == 0 {
		return false
	}
	t.checks = append(t.checks, &pCheck{name: s.next("ck"), col: pick(r, cands)})
	return true
}

func (s *pSchema) addFK(r *rand.Rand, t, parent *pTable) bool {
	if parent == t {
		return false
	}
	ppk := parent.pk()
	c := &pCol{name: s.next("r"), typ: ppk.typ, notNull: r.Intn(2) == 0}
	t.cols = append(t.cols, c)
	fk := &pFK{name: s.next("fk"), cols: []string{c.name}, refTable: parent.name, refCols: []string{ppk.name},
		onDelete: pick(r, []string{"", "CASCADE", "SET NULL", "RESTRICT"}),
		onUpdate: pick(r, []string{"", "CASCADE", "RESTRICT", "NO ACTION"})}
	if fk.onDelete == "SET NULL" {
		c.notNull = false
	}
	t.fks = append(t.fks, fk)
	return true
}

// addCompositeFK gives t two new columns referencing a two-column UNIQUE key that parent
// gains over two new NOT NULL integer columns of its own (rows carry the row number in
// every integer column, so every (i, i) pair exists in the parent).
func (s *pSchema) addCompositeFK(r *rand.Rand, t, parent *pTable) bool {
	if parent == t {
		return false
	}
	a := &pCol{name: s.next("a"), typ: "integer", notNull: true, def: "0"}
	b := &pCol{name: s.next("b"), typ: "integer", notNull: true, def: "0"}
	parent.cols = append(parent.cols, a, b)
	if s.mutating {
		// the parent holds rows: the new key columns need distinct values (the key's)
		a.fresh, b.fresh = true, true
		pk := parent.pk().name
		s.intents = append(s.intents, fmt.Sprintf("-- @migrate backfill %s.%s = %s", parent.full(), a.name, qi(pk)),
			fmt.Sprintf("-- @migrate backfill %s.%s = %s", parent.full(), b.name, qi(pk)))
	}
	parent.keys = append(parent.keys, &pKey{name: s.next("k"), unique: true, cols: []string{a.name, b.name}})
	ra := &pCol{name: s.next("r"), typ: "integer", fresh: s.mutating}
	rb := &pCol{name: s.next("r"), typ: "integer", fresh: s.mutating}
	t.cols = append(t.cols, ra, rb)
	t.fks = append(t.fks, &pFK{name: s.next("fk"), cols: []string{ra.name, rb.name}, refTable: parent.name, refCols: []string{a.name, b.name},
		onDelete: pick(r, []string{"", "CASCADE", "SET NULL"})})
	return true
}

func (s *pSchema) addView(r *rand.Rand) bool { return s.addViewKind(r, r.Intn(3) == 0) }

func (s *pSchema) addViewKind(r *rand.Rand, mat bool) bool {
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
	s.views = append(s.views, &pView{name: s.next("v"), table: t.name, cols: cols, mat: mat})
	return true
}

func (s *pSchema) addTrigger(r *rand.Rand) bool { return s.addTriggerOn(r, pick(r, s.tables)) }

func (s *pSchema) addTriggerOn(r *rand.Rand, t *pTable) bool {
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
	name := s.next("tr")
	col, n := pick(r, src).name, 1+r.Intn(9)
	s.triggerFns = append(s.triggerFns, &pTriggerFn{name: name, col: col, n: n})
	s.triggers = append(s.triggers, &pTrigger{name: name, table: t.name, col: col, n: n, callName: name})
	return true
}

func generate(r *rand.Rand, pg18 bool) *pSchema {
	s := &pSchema{pg18: pg18}
	n := 1 + r.Intn(3)
	for i := 0; i < n; i++ {
		s.tables = append(s.tables, s.newTable(r))
	}
	// a partitioned table from pair 0 too (see untouched-style helpers below for why): a
	// spare "drop partition" / "detach partition" needs a candidate that isn't only one a
	// same-recipe "add partitioned table" step happened to leave behind
	s.partTables = append(s.partTables, s.newPartTable(r))
	// a spare already-detached partition from pair 0 too: "attach partition" alone
	// (directed coverage) needs one already standing apart, not only one a same-recipe
	// "detach partition" step happened to leave behind. Bound-picking mirrors "add
	// partition"'s own (a LIST value / RANGE bound the other children don't already use).
	{
		pt0 := s.partTables[0]
		name := s.next(pt0.name + "_")
		if pt0.strategy == "LIST" {
			used := map[string]bool{}
			for _, c := range pt0.parts {
				for _, v := range c.values {
					used[v] = true
				}
			}
			letter := "d"
			for _, cand := range []string{"d", "e", "f", "g", "h"} {
				if !used[cand] {
					letter = cand
					break
				}
			}
			pt0.parts = append(pt0.parts, &pPartChild{name: name, orig: "public." + name, values: []string{letter}, detached: true})
		} else {
			hi := 0
			for _, c := range pt0.parts {
				if !c.isDefault && c.hi > hi {
					hi = c.hi
				}
			}
			pt0.parts = append(pt0.parts, &pPartChild{name: name, orig: "public." + name, lo: hi, hi: hi + 10, detached: true})
		}
	}
	if r.Intn(2) == 0 {
		s.addViewKind(r, false) // a spare plain view, so "drop view" has one from pair 0
	}
	if r.Intn(3) == 0 {
		s.addViewKind(r, true) // a spare materialized view, same reason
	}
	if r.Intn(2) == 0 {
		tt := pick(r, s.tables)
		s.addTriggerOn(r, tt)
		s.addTriggerOn(r, tt) // a second one on the same table, so "change trigger function" has a pair from pair 0
	}
	s.funcs = append(s.funcs, &pFunc{name: s.next("f"), n: 1 + r.Intn(9), retType: "integer", volatility: "IMMUTABLE", lang: "sql"})
	// spares with no column using them: "drop domain" / "drop enum" / "drop range type"
	// need a candidate from pair 0, not just one a same-recipe "add" step happened to
	// leave behind
	s.domains = append(s.domains, &pDomain{name: s.next("num")})
	s.enums = append(s.enums, &pEnum{name: s.next("e"), labels: []string{"a", "b", "c"}[:2+r.Intn(2)]})
	s.composites = append(s.composites, s.newComposite(r))
	s.ranges = append(s.ranges, &pRange{name: s.next("rg")})
	s.extensions = append(s.extensions, "pgcrypto")
	// a policy / rule from pair 0 too, for the same reason as the spares above
	t := pick(r, s.tables)
	t.rowSec = true
	command := pick(r, []string{"ALL", "SELECT", "INSERT", "UPDATE", "DELETE"})
	using, withCheck := policyPredicates(command)
	s.policies = append(s.policies, &pPolicy{name: s.next("pol"), table: t.name, command: command,
		permissive: r.Intn(4) != 0, using: using, withCheck: withCheck})
	t2 := pick(r, s.tables)
	s.rules = append(s.rules, &pRule{name: s.next("ru"), table: t2.name, where: r.Intn(2) == 0, enabled: true})
	// a standalone sequence, owned by some table's plain integer column, from pair 0 too
	sq := &pSeq{name: s.next("sq")}
	s.seqs = append(s.seqs, sq)
	for _, t3 := range s.tables {
		for _, c := range t3.cols {
			if isInteger(c.typ) && !c.pk && !c.identity && !c.serial && c.gen == "" {
				sq.table, sq.col = t3.name, c.name
			}
		}
	}
	// a second, unowned standalone sequence from pair 0 too: "set sequence owner" alone
	// (directed coverage) needs one with no owner yet, not only one a same-recipe "clear
	// sequence owner" step happened to leave behind.
	s.seqs = append(s.seqs, &pSeq{name: s.next("sq")})
	// a seed table from pair 0 too, so "change" / "drop seed row" have rows to act on
	s.seedTable = &pSeedTable{name: s.next("lk"), rows: []pSeedRow{
		{code: s.next("code"), label: "a"},
		{code: s.next("code"), label: "b"},
	}}
	return s
}

// ---- mutations --------------------------------------------------------------------------

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

// keyIsReferenced reports whether any column of k is referenced by another table's foreign
// key (a composite unique key rests under a composite foreign key column by column, so any
// one of its columns being referenced is enough).
func keyIsReferenced(s *pSchema, t *pTable, k *pKey) bool {
	for _, col := range k.cols {
		if len(referencedBy(s, t, col)) > 0 {
			return true
		}
	}
	return false
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

func indexOf(list []string, s string) int {
	for i, x := range list {
		if x == s {
			return i
		}
	}
	return -1
}

func indexOfCol(t *pTable, name string) int {
	for i, c := range t.cols {
		if c.name == name {
			return i
		}
	}
	return -1
}

// detach removes everything that reads t's column col.
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
	for _, sq := range s.seqs {
		if sq.table == t.name && sq.col == from {
			sq.col = to
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
	var policies []*pPolicy
	for _, pol := range s.policies {
		if pol.table != t.name {
			policies = append(policies, pol)
		}
	}
	s.policies = policies
	var rules []*pRule
	for _, ru := range s.rules {
		if ru.table != t.name {
			rules = append(rules, ru)
		}
	}
	s.rules = rules
	for _, sq := range s.seqs {
		if sq.table == t.name {
			sq.table, sq.col = "", ""
		}
	}
}

func insertCol(r *rand.Rand, t *pTable, c *pCol) {
	at := r.Intn(len(t.cols) + 1)
	t.cols = append(t.cols[:at], append([]*pCol{c}, t.cols[at:]...)...)
}

// excludeChangeColumn and excludeToggleOperator are registered twice in mutations (a
// second entry point, same body): see "toggle trigger fires on update "'s comment for why.

func excludeChangeColumn(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
	for _, t := range untouched(s, touched) {
		var cands []*pKey
		for _, k := range t.keys {
			if k.exclude && !k.opNE { // opNE needs the shared-value column it has
				cands = append(cands, k)
			}
		}
		if len(cands) == 0 {
			continue
		}
		k := pick(r, cands)
		var others []string
		for _, c := range t.cols {
			if isInteger(c.typ) && !c.pk && !c.fresh && c.name != k.cols[0] {
				others = append(others, c.name)
			}
		}
		if len(others) == 0 {
			continue
		}
		k.cols[0] = pick(r, others)
		return true
	}
	return false
}

func excludeToggleOperator(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
	for _, t := range untouched(s, touched) {
		var cands []*pKey
		for _, k := range t.keys {
			if k.exclude {
				cands = append(cands, k)
			}
		}
		if len(cands) == 0 {
			continue
		}
		k := pick(r, cands)
		if !k.opNE {
			// WITH <> forbids any two rows differing: a fresh column, sharing one
			// default value in every row, always holds. It also needs GIST (btree's
			// integer operator family has no "<>" member, 42809, measured), so
			// btree_gist too.
			c := &pCol{name: s.next("ne"), typ: "integer", notNull: true, def: "1", fresh: true}
			t.cols = append(t.cols, c)
			k.cols[0], k.opNE = c.name, true
			s.ensureExtension("btree_gist")
			return true
		}
		var ints []string
		for _, c := range t.cols {
			if isInteger(c.typ) && !c.pk && !c.fresh && c.name != k.cols[0] {
				ints = append(ints, c.name)
			}
		}
		if len(ints) == 0 {
			continue
		}
		k.cols[0], k.opNE = pick(r, ints), false
		return true
	}
	return false
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
			c = &pCol{name: s.next("g"), typ: "bigint", gen: pick(r, src).name}
		} else {
			c = s.newCol(r)
		}
		c.fresh = true
		insertCol(r, t, c)
		touched[t.name+"."+c.name] = true
		return true
	}},
	{"add generated column reading a new column", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		ts := untouched(s, touched)
		if len(ts) == 0 {
			return false
		}
		t := pick(r, ts)
		x := &pCol{name: s.next("c"), typ: pick(r, []string{"integer", "numeric(10,2)", "smallint"})}
		g := &pCol{name: s.next("g"), typ: "bigint", gen: x.name}
		x.fresh, g.fresh = true, true
		insertCol(r, t, g)
		insertCol(r, t, x)
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
		for _, sq := range s.seqs {
			if sq.table == t.name && sq.col == c.name {
				return false // a standalone sequence is OWNED BY this column
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
		s.intents = append(s.intents, "-- @migrate rename "+t.orig+"."+c.name+" -> "+t.full()+"."+to)
		// a backfill expression written earlier names the column as the target spells it
		for i, in := range s.intents {
			if strings.HasPrefix(in, "-- @migrate backfill "+t.full()+".") {
				s.intents[i] = strings.ReplaceAll(in, " = "+qi(c.name), " = "+qi(to))
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
			if widen(c.typ) != "" && c.gen == "" && !c.identity && !c.serial && !touched[t.name+"."+c.name] {
				cands = append(cands, c)
			}
		}
		if len(cands) == 0 {
			return false
		}
		c := pick(r, cands)
		w := widen(c.typ)
		if t.inFK(c.name) {
			return false // a referencing column follows its parent's key
		}
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
			c.def = s.defaultFor(c)
		}
		touched[t.name+"."+c.name] = true
		return true
	}},
	{"change nullability and default", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
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
		if c.identity || c.serial {
			return false
		}
		for _, fk := range t.fks {
			if indexOf(fk.cols, c.name) >= 0 && fk.onDelete == "SET NULL" {
				return false
			}
		}
		c.notNull = !c.notNull
		if c.notNull || r.Intn(2) == 0 {
			c.def = s.defaultFor(c)
		} else {
			c.def = ""
		}
		if c.notNull {
			f := s.fill(c)
			if t.inFK(c.name) {
				// a parent every table has (rows() gives every table a row 3), and never
				// row 1's or row 2's own FK value: rows() only ever nulls a nullable
				// column's third row, so this backfill only ever reaches row 3 -- reusing
				// its own row number rather than a fixed "1" cannot duplicate row 1's or
				// row 2's, which an EXCLUDE (WITH =) over this column would otherwise
				// refuse as a duplicate (23P01, measured)
				f = "3"
			}
			s.intents = append(s.intents, fmt.Sprintf("-- @migrate backfill %s.%s = %s where %s is null", t.full(), c.name, f, qi(c.name)))
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
				// a unique constraint a foreign key references stays
				if t.keys[i].unique && len(referencedBy(s, t, t.keys[i].cols[0])) > 0 {
					return false
				}
				t.keys = append(t.keys[:i], t.keys[i+1:]...)
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
			// c.fresh is excluded: a column another mutation just added carries whatever
			// single literal DEFAULT that mutation gave it (shared by every row, not the
			// base generator's per-row-distinct value), so becoming the primary key
			// without its own backfill risks a duplicate key on apply (measured: "toggle
			// exclude constraint operator"'s "ne" column, shared default 1 so its WITH <>
			// exclude always holds, moved onto as a PK loses that assumption -- !c.notNull
			// alone missed it, since that column was already NOT NULL)
			if !c.pk && c.gen == "" && isInteger(c.typ) && !c.fresh && !touched[t.name+"."+c.name] && !t.inFK(c.name) {
				cands = append(cands, c)
			}
		}
		if len(cands) == 0 || touched[t.name+"."+old.name] {
			return false
		}
		c := pick(r, cands)
		if !c.notNull {
			s.intents = append(s.intents, fmt.Sprintf("-- @migrate backfill %s.%s = 9 where %s is null", t.full(), c.name, qi(c.name)))
		}
		c.pk, c.notNull, c.def = true, true, ""
		old.pk = false
		if len(referencedBy(s, t, old.name)) > 0 {
			t.keys = append(t.keys, &pKey{name: s.next("k"), unique: true, cols: []string{old.name}})
		} else if !old.identity && !old.serial && r.Intn(2) == 0 {
			old.notNull = false
		}
		touched[t.name+"."+old.name], touched[t.name+"."+c.name] = true, true
		return true
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
		c := &pCol{name: s.next("c"), typ: pick(r, []string{"integer", "bigint"}), notNull: true, pk: true, identity: r.Intn(2) == 0}
		insertCol(r, t, c)
		if !c.identity {
			// the rows need distinct values before the key goes on: the old key's
			s.intents = append(s.intents, fmt.Sprintf("-- @migrate backfill %s.%s = %s", t.full(), c.name, qi(old.name)))
		}
		old.pk = false
		if len(referencedBy(s, t, old.name)) > 0 {
			t.keys = append(t.keys, &pKey{name: s.next("k"), unique: true, cols: []string{old.name}})
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
		t, parent := ts[i], ts[j]
		if !s.addFK(r, t, parent) {
			return false
		}
		c := t.cols[len(t.cols)-1]
		if c.notNull {
			// existing rows need a parent before the column turns NOT NULL
			s.intents = append(s.intents, fmt.Sprintf("-- @migrate backfill %s.%s = 1", t.full(), c.name))
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
				return false
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
		s.intents = append(s.intents, "-- @migrate rename "+t.orig+" -> "+t.schema+"."+to)
		for i, in := range s.intents {
			in = strings.ReplaceAll(in, " -> "+t.full()+".", " -> "+t.schema+"."+to+".")
			in = strings.ReplaceAll(in, "backfill "+t.full()+".", "backfill "+t.schema+"."+to+".")
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
		for _, pol := range s.policies {
			if pol.table == t.name {
				pol.table = to
			}
		}
		for _, ru := range s.rules {
			if ru.table == t.name {
				ru.table = to
			}
		}
		for _, sq := range s.seqs {
			if sq.table == t.name {
				sq.table = to
			}
		}
		touched[t.name], touched[to] = true, true
		t.name = to
		return true
	}},
	{"move table to public schema", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		var cands []*pTable
		for _, t := range untouched(s, touched) {
			if t.schema != "public" {
				cands = append(cands, t)
			}
		}
		if len(cands) == 0 {
			return false
		}
		t := pick(r, cands)
		s.intents = append(s.intents, "-- @migrate rename "+t.orig+" -> public."+t.name)
		for i, in := range s.intents {
			in = strings.ReplaceAll(in, " -> "+t.full()+".", " -> public."+t.name+".")
			in = strings.ReplaceAll(in, "backfill "+t.full()+".", "backfill public."+t.name+".")
			s.intents[i] = in
		}
		touched[t.name] = true
		t.schema = "public"
		return true
	}},
	{"move table to app schema", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		var cands []*pTable
		for _, t := range untouched(s, touched) {
			if t.schema == "public" {
				cands = append(cands, t)
			}
		}
		if len(cands) == 0 {
			return false
		}
		t := pick(r, cands)
		s.intents = append(s.intents, "-- @migrate rename "+t.orig+" -> app."+t.name)
		for i, in := range s.intents {
			in = strings.ReplaceAll(in, " -> "+t.full()+".", " -> app."+t.name+".")
			in = strings.ReplaceAll(in, "backfill "+t.full()+".", "backfill app."+t.name+".")
			s.intents[i] = in
		}
		touched[t.name] = true
		t.schema = "app"
		return true
	}},
	{"collapse app schema", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		// every remaining "app" table at once, so the schema itself goes: leaving even
		// one behind (as "move table to public schema" alone might, one at a time)
		// keeps "app" declared
		var cands []*pTable
		for _, t := range untouched(s, touched) {
			if t.schema != "public" {
				cands = append(cands, t)
			}
		}
		if len(cands) == 0 {
			return false
		}
		for _, t := range cands {
			s.intents = append(s.intents, "-- @migrate rename "+t.orig+" -> public."+t.name)
			for i, in := range s.intents {
				in = strings.ReplaceAll(in, " -> "+t.full()+".", " -> public."+t.name+".")
				in = strings.ReplaceAll(in, "backfill "+t.full()+".", "backfill public."+t.name+".")
				s.intents[i] = in
			}
			touched[t.name] = true
			t.schema = "public"
		}
		return true
	}},
	{"change domain check", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		for _, d := range s.domains {
			if !touched["domain:"+d.name] {
				d.ge = !d.ge
				touched["domain:"+d.name] = true
				return true
			}
		}
		return false
	}},
	{"add enum label", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		for _, e := range s.enums {
			if !touched["enum:"+e.name] {
				e.labels = append(e.labels, "z")
				touched["enum:"+e.name] = true
				return true
			}
		}
		return false
	}},
	{"drop enum label", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		for _, e := range s.enums {
			// a fresh enum (minted by an earlier "add table" / "add column" step, not in the
			// source) has no from-side to declare the drop against: 42704 "no such enum"
			if e.fresh {
				continue
			}
			if len(e.labels) > 1 && !touched["enum:"+e.name] {
				gone := e.labels[len(e.labels)-1]
				e.labels = e.labels[:len(e.labels)-1]
				s.intents = append(s.intents, fmt.Sprintf("-- @migrate enum %s: drop '%s' using '%s'", e.name, gone, e.labels[0]))
				touched["enum:"+e.name] = true
				return true
			}
		}
		return false
	}},
	{"toggle domain not null", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		// a column of the domain may already hold NULL: keep this off a domain any
		// column uses (existing NULLs would refuse the ALTER DOMAIN SET NOT NULL, 23502,
		// the same way an ungueded "add identity" once did -- see its mutation)
		for _, d := range s.domains {
			if !typeInUse(s, d.name) && !touched["domain:"+d.name] {
				d.notNull = !d.notNull
				touched["domain:"+d.name] = true
				return true
			}
		}
		return false
	}},
	{"drop domain", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		var cands []int
		for i, d := range s.domains {
			if !typeInUse(s, d.name) && !touched["domain:"+d.name] {
				cands = append(cands, i)
			}
		}
		if len(cands) == 0 {
			return false
		}
		i := pick(r, cands)
		touched["domain:"+s.domains[i].name] = true
		s.domains = append(s.domains[:i], s.domains[i+1:]...)
		return true
	}},
	{"drop enum type", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		var cands []int
		for i, e := range s.enums {
			if !typeInUse(s, e.name) && !touched["enum:"+e.name] {
				cands = append(cands, i)
			}
		}
		if len(cands) == 0 {
			return false
		}
		i := pick(r, cands)
		touched["enum:"+s.enums[i].name] = true
		s.enums = append(s.enums[:i], s.enums[i+1:]...)
		return true
	}},
	{"add range type", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		s.ranges = append(s.ranges, &pRange{name: s.next("rg")})
		return true
	}},
	{"add composite type", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		s.composites = append(s.composites, s.newComposite(r))
		return true
	}},
	{"add enum type", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		// fresh: true, or a same-recipe "drop enum label" (unaware this enum has no
		// from-side of its own) can pick it and declare `-- @migrate enum <new>: drop ...`
		// against an enum "no such enum in the current schema" refuses (42704, measured).
		s.enums = append(s.enums, &pEnum{name: s.next("e"), labels: []string{"a", "b", "c"}[:2+r.Intn(2)], fresh: true})
		return true
	}},
	{"add domain type", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		s.domains = append(s.domains, &pDomain{name: s.next("num")})
		return true
	}},
	{"drop range type", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		if len(s.ranges) == 0 {
			return false
		}
		i := r.Intn(len(s.ranges))
		s.ranges = append(s.ranges[:i], s.ranges[i+1:]...)
		return true
	}},
	{"add extension", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		have := map[string]bool{}
		for _, e := range s.extensions {
			have[e] = true
		}
		var cands []string
		for _, e := range []string{"pgcrypto", "citext"} {
			if !have[e] {
				cands = append(cands, e)
			}
		}
		if len(cands) == 0 {
			return false
		}
		s.extensions = append(s.extensions, pick(r, cands))
		return true
	}},
	{"drop extension", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		gistInUse := false
		for _, t := range s.tables {
			for _, k := range t.keys {
				if k.exclude && (k.usingGist || k.opNE) {
					gistInUse = true
				}
			}
		}
		var cands []int
		for i, e := range s.extensions {
			if e == "btree_gist" && gistInUse {
				continue // a gist EXCLUDE constraint depends on it
			}
			cands = append(cands, i)
		}
		if len(cands) == 0 {
			return false
		}
		i := pick(r, cands)
		s.extensions = append(s.extensions[:i], s.extensions[i+1:]...)
		return true
	}},
	{"add rule", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		ts := untouched(s, touched)
		if len(ts) == 0 {
			return false
		}
		t := pick(r, ts)
		s.rules = append(s.rules, &pRule{name: s.next("ru"), table: t.name, where: r.Intn(2) == 0, enabled: true})
		return true
	}},
	{"add sequence", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		s.seqs = append(s.seqs, &pSeq{name: s.next("sq")})
		return true
	}},
	{"drop sequence", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		if len(s.seqs) == 0 {
			return false
		}
		i := r.Intn(len(s.seqs))
		s.seqs = append(s.seqs[:i], s.seqs[i+1:]...)
		return true
	}},
	{"set sequence owner", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		var cands []*pSeq
		for _, sq := range s.seqs {
			if sq.table == "" {
				cands = append(cands, sq)
			}
		}
		if len(cands) == 0 {
			return false
		}
		ts := untouched(s, touched)
		if len(ts) == 0 {
			return false
		}
		t := pick(r, ts)
		owned := map[string]bool{} // a column two sequences both claim OWNED BY is not
		// a shape any real schema author would declare, and adds()/renames() only expect
		// one from-side sequence to ever forward onto a given owner (sequenceFollowsRename,
		// migrate.go): picking a column another sequence already owns can match this
		// mutation's own new sequence to that unrelated one by coincidence and lose its
		// CREATE SEQUENCE entirely (measured).
		for _, sq := range s.seqs {
			if sq.table != "" {
				owned[sq.table+"."+sq.col] = true
			}
		}
		var cols []*pCol
		for _, c := range t.cols {
			if isInteger(c.typ) && !c.pk && !c.identity && !c.serial && !c.fresh && c.gen == "" && !owned[t.name+"."+c.name] {
				cols = append(cols, c)
			}
		}
		if len(cols) == 0 {
			return false
		}
		sq := pick(r, cands)
		c := pick(r, cols)
		sq.table, sq.col = t.name, c.name
		return true
	}},
	{"clear sequence owner", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		var cands []*pSeq
		for _, sq := range s.seqs {
			if sq.table != "" {
				cands = append(cands, sq)
			}
		}
		if len(cands) == 0 {
			return false
		}
		sq := pick(r, cands)
		sq.table, sq.col = "", ""
		return true
	}},
	{"add seed row", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		if s.seedTable == nil || touched["seed"] {
			return false
		}
		st := s.seedTable
		st.rows = append(st.rows, pSeedRow{code: s.next("code"), label: "z"})
		return true
	}},
	{"drop seed row", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		st := s.seedTable
		if st == nil || st.additive || len(st.rows) == 0 || touched["seed"] {
			return false
		}
		i := r.Intn(len(st.rows))
		st.rows = append(st.rows[:i], st.rows[i+1:]...)
		touched["seed"] = true
		return true
	}},
	{"change seed row", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		st := s.seedTable
		if st == nil || len(st.rows) == 0 || touched["seed"] {
			return false
		}
		row := &st.rows[r.Intn(len(st.rows))]
		row.label += "!"
		touched["seed"] = true
		return true
	}},
	{"toggle seed table additive", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		if s.seedTable == nil {
			return false
		}
		s.seedTable.additive = !s.seedTable.additive
		return true
	}},
	{"drop rule", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		var cands []int
		for i, ru := range s.rules {
			if !touched["rule:"+ru.name] {
				cands = append(cands, i)
			}
		}
		if len(cands) == 0 {
			return false
		}
		i := pick(r, cands)
		touched["rule:"+s.rules[i].name] = true
		s.rules = append(s.rules[:i], s.rules[i+1:]...)
		return true
	}},
	{"toggle rule enabled", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		var cands []*pRule
		for _, ru := range s.rules {
			if !touched["rule:"+ru.name] {
				cands = append(cands, ru)
			}
		}
		if len(cands) == 0 {
			return false
		}
		ru := pick(r, cands)
		ru.enabled = !ru.enabled
		touched["rule:"+ru.name] = true
		return true
	}},
	{"toggle rule where", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		var cands []*pRule
		for _, ru := range s.rules {
			if !touched["rule:"+ru.name] {
				cands = append(cands, ru)
			}
		}
		if len(cands) == 0 {
			return false
		}
		ru := pick(r, cands)
		ru.where = !ru.where
		touched["rule:"+ru.name] = true
		return true
	}},
	{"reword table comment", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		var cands []*pTable
		for _, t := range untouched(s, touched) {
			if t.comment != "" {
				cands = append(cands, t)
			}
		}
		if len(cands) == 0 {
			return false
		}
		t := pick(r, cands)
		t.comment += "!"
		return true
	}},
	{"reword column comment", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		var cands []*pCol
		var owner []*pTable
		for _, t := range s.tables {
			for _, c := range t.cols {
				if c.comment != "" && !touched[t.name+".comment."+c.name] {
					cands = append(cands, c)
					owner = append(owner, t)
				}
			}
		}
		if len(cands) == 0 {
			return false
		}
		i := r.Intn(len(cands))
		cands[i].comment += "!"
		touched[owner[i].name+".comment."+cands[i].name] = true
		return true
	}},
	{"change function return type", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		var cands []*pFunc
		for _, f := range s.funcs {
			if f.kind == "" && !touched["function:"+f.name] {
				cands = append(cands, f)
			}
		}
		if len(cands) == 0 {
			return false
		}
		f := pick(r, cands)
		if f.retType == "integer" {
			f.retType = "bigint"
		} else {
			f.retType = "integer"
		}
		touched["function:"+f.name] = true
		return true
	}},
	{"toggle function strict", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		var cands []*pFunc
		for _, f := range s.funcs {
			if f.kind == "" && !touched["function:"+f.name] {
				cands = append(cands, f)
			}
		}
		if len(cands) == 0 {
			return false
		}
		f := pick(r, cands)
		f.strict = !f.strict
		touched["function:"+f.name] = true
		return true
	}},
	{"toggle function volatility", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		var cands []*pFunc
		for _, f := range s.funcs {
			if f.kind == "" && !touched["function:"+f.name] {
				cands = append(cands, f)
			}
		}
		if len(cands) == 0 {
			return false
		}
		f := pick(r, cands)
		if f.volatility == "IMMUTABLE" {
			f.volatility = "STABLE"
		} else {
			f.volatility = "IMMUTABLE"
		}
		touched["function:"+f.name] = true
		return true
	}},
	{"toggle function argument default", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		var cands []*pFunc
		for _, f := range s.funcs {
			if f.kind == "" && !touched["function:"+f.name] {
				cands = append(cands, f)
			}
		}
		if len(cands) == 0 {
			return false
		}
		f := pick(r, cands)
		f.argDefault = !f.argDefault
		touched["function:"+f.name] = true
		return true
	}},
	{"toggle function language", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		var cands []*pFunc
		for _, f := range s.funcs {
			if f.kind == "" && !touched["function:"+f.name] {
				cands = append(cands, f)
			}
		}
		if len(cands) == 0 {
			return false
		}
		f := pick(r, cands)
		if f.lang == "plpgsql" {
			f.lang = "sql"
		} else {
			f.lang = "plpgsql"
		}
		touched["function:"+f.name] = true
		return true
	}},
	{"convert function to procedure", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		var cands []*pFunc
		for _, f := range s.funcs {
			if f.kind == "" && !touched["function:"+f.name] {
				cands = append(cands, f)
			}
		}
		if len(cands) == 0 {
			return false
		}
		f := pick(r, cands)
		f.kind = "procedure"
		touched["function:"+f.name] = true
		return true
	}},
	{"convert function to aggregate", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		var cands []*pFunc
		for _, f := range s.funcs {
			if f.kind == "" && f.retType == "integer" && !touched["function:"+f.name] {
				cands = append(cands, f)
			}
		}
		if len(cands) == 0 {
			return false
		}
		f := pick(r, cands)
		f.kind = "aggregate"
		touched["function:"+f.name] = true
		return true
	}},
	{"convert function to window", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		// migrate.go's functions() only drops a LANGUAGE internal function that is a
		// range/multirange constructor (isRangeConstructor); a user-declared internal
		// WINDOW function like this one is tracked and its "window" property diffed
		var cands []*pFunc
		for _, f := range s.funcs {
			if f.kind == "" && !touched["function:"+f.name] {
				cands = append(cands, f)
			}
		}
		if len(cands) == 0 {
			return false
		}
		f := pick(r, cands)
		f.kind = "window"
		touched["function:"+f.name] = true
		return true
	}},
	{"change exclude constraint column", excludeChangeColumn},
	{"change exclude constraint column ", excludeChangeColumn}, // a second entry point (same body): see "toggle trigger fires on update "
	{"toggle exclude constraint where", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		for _, t := range untouched(s, touched) {
			var cands []*pKey
			for _, k := range t.keys {
				if k.exclude {
					cands = append(cands, k)
				}
			}
			if len(cands) == 0 {
				continue
			}
			k := pick(r, cands)
			if k.where == "positive" {
				k.where = ""
			} else {
				k.where = "positive"
			}
			return true
		}
		return false
	}},
	{"toggle exclude constraint operator", excludeToggleOperator},
	{"toggle exclude constraint operator ", excludeToggleOperator}, // a second entry point (same body): see "toggle trigger fires on update "
	{"toggle exclude constraint using", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		for _, t := range untouched(s, touched) {
			var cands []*pKey
			for _, k := range t.keys {
				if k.exclude && !k.opNE { // opNE always renders as gist regardless
					cands = append(cands, k)
				}
			}
			if len(cands) == 0 {
				continue
			}
			k := pick(r, cands)
			if !k.usingGist {
				s.ensureExtension("btree_gist")
			}
			k.usingGist = !k.usingGist
			return true
		}
		return false
	}},
	{"rename foreign key column", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		for _, t := range untouched(s, touched) {
			var cands []*pFK
			for _, fk := range t.fks {
				if len(fk.cols) == 1 && !touched[t.name+"."+fk.cols[0]] {
					cands = append(cands, fk)
				}
			}
			if len(cands) == 0 {
				continue
			}
			c := t.col(pick(r, cands).cols[0])
			to := c.name + "_new"
			s.intents = append(s.intents, "-- @migrate rename "+t.orig+"."+c.name+" -> "+t.full()+"."+to)
			renameRefs(s, t, c.name, to)
			touched[t.name+"."+c.name], touched[t.name+"."+to] = true, true
			c.name = to
			return true
		}
		return false
	}},
	{"rename unique key column", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		for _, t := range untouched(s, touched) {
			var cands []string
			for _, k := range t.keys {
				if !k.unique {
					continue
				}
				for _, cn := range k.cols {
					if !touched[t.name+"."+cn] {
						cands = append(cands, cn)
					}
				}
			}
			if len(cands) == 0 {
				continue
			}
			c := t.col(pick(r, cands))
			to := c.name + "_new"
			s.intents = append(s.intents, "-- @migrate rename "+t.orig+"."+c.name+" -> "+t.full()+"."+to)
			renameRefs(s, t, c.name, to)
			touched[t.name+"."+c.name], touched[t.name+"."+to] = true, true
			c.name = to
			return true
		}
		return false
	}},
	{"change foreign key on delete", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		for _, t := range untouched(s, touched) {
			var cands []*pFK
			for _, fk := range t.fks {
				if len(fk.cols) == 1 {
					cands = append(cands, fk)
				}
			}
			if len(cands) == 0 {
				continue
			}
			fk := pick(r, cands)
			c := t.col(fk.cols[0])
			opts := []string{"", "CASCADE", "RESTRICT"}
			if !c.notNull {
				opts = append(opts, "SET NULL")
			}
			var filtered []string
			for _, o := range opts {
				if o != fk.onDelete {
					filtered = append(filtered, o)
				}
			}
			if len(filtered) == 0 {
				continue
			}
			next := pick(r, filtered)
			if next == "SET NULL" {
				c.notNull = false
			}
			fk.onDelete = next
			return true
		}
		return false
	}},
	{"toggle key unique", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		for _, t := range untouched(s, touched) {
			var cands []*pKey
			for _, k := range t.keys {
				if k.exclude || k.where != "" || k.expr != "" {
					continue
				}
				if k.unique {
					// a foreign key elsewhere may rest on this unique key (same guard as
					// "drop key"): going plain drops the index a REFERENCES needs (42830,
					// measured)
					if !keyIsReferenced(s, t, k) {
						cands = append(cands, k)
					}
					continue
				}
				// going plain -> unique: every column needs a unique key's usual
				// safety (see addKey's comment: no repeats, no shared fresh default)
				safe := true
				for _, cn := range k.cols {
					c := t.col(cn)
					if c.typ == "jsonb" || c.typ == "boolean" || s.enum(c.typ) != nil || c.fresh {
						safe = false
					}
				}
				if safe {
					cands = append(cands, k)
				}
			}
			if len(cands) == 0 {
				continue
			}
			k := pick(r, cands)
			k.unique = !k.unique
			if !k.unique {
				k.notDist, k.defer_ = false, false
			}
			return true
		}
		return false
	}},
	{"change index columns", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		for _, t := range untouched(s, touched) {
			var cands []*pKey
			for _, k := range t.keys {
				if !k.unique && !k.exclude && k.expr == "" {
					cands = append(cands, k)
				}
			}
			if len(cands) == 0 {
				continue
			}
			k := pick(r, cands)
			var others []string
			for _, c := range t.cols {
				if c.typ == "jsonb" || c.name == k.cols[0] {
					continue
				}
				if k.where == "positive" && !isNumeric(c.typ) {
					continue // WHERE (col > 0) needs a numeric column
				}
				others = append(others, c.name)
			}
			if len(others) == 0 {
				continue
			}
			k.cols[0] = pick(r, others)
			return true
		}
		return false
	}},
	{"change policy command", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		var cands []*pPolicy
		for _, pol := range s.policies {
			if !touched["policy:"+pol.name] {
				cands = append(cands, pol)
			}
		}
		if len(cands) == 0 {
			return false
		}
		pol := pick(r, cands)
		var opts []string
		for _, c := range []string{"ALL", "SELECT", "INSERT", "UPDATE", "DELETE"} {
			if c != pol.command {
				opts = append(opts, c)
			}
		}
		pol.command = pick(r, opts)
		pol.using, pol.withCheck = policyPredicates(pol.command)
		touched["policy:"+pol.name] = true
		return true
	}},
	{"toggle policy role", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		var cands []*pPolicy
		for _, pol := range s.policies {
			if !touched["policy:"+pol.name] {
				cands = append(cands, pol)
			}
		}
		if len(cands) == 0 {
			return false
		}
		pol := pick(r, cands)
		pol.role = !pol.role
		touched["policy:"+pol.name] = true
		return true
	}},
	{"toggle view check option", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		var cands []*pView
		for _, v := range s.views {
			if !v.mat {
				cands = append(cands, v)
			}
		}
		if len(cands) == 0 {
			return false
		}
		v := pick(r, cands)
		if v.checkOption == "" {
			v.checkOption = pick(r, []string{"LOCAL", "CASCADED"})
		} else {
			v.checkOption = ""
		}
		return true
	}},
	{"toggle trigger fires on update", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		if len(s.triggers) == 0 {
			return false
		}
		tr := pick(r, s.triggers)
		tr.onUpdate = !tr.onUpdate
		return true
	}},
	{"toggle trigger fires on update ", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		// a second entry point (same body): raises this comparatively rare field's
		// selection odds against the growing mutation list, same reasoning as the
		// generate()-time spares elsewhere in this file
		if len(s.triggers) == 0 {
			return false
		}
		tr := pick(r, s.triggers)
		tr.onUpdate = !tr.onUpdate
		return true
	}},
	{"add second trigger", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool { return s.addTrigger(r) }},
	{"change trigger function", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		// deterministically ordered (s.triggers is a slice, not a map): a repointed
		// trigger's own column still exists (same table as the one it now calls)
		var pairs [][2]*pTrigger
		for i, a := range s.triggers {
			for j, b := range s.triggers {
				if i != j && a.table == b.table {
					pairs = append(pairs, [2]*pTrigger{a, b})
				}
			}
		}
		if len(pairs) == 0 {
			return false
		}
		p := pick(r, pairs)
		p[0].callName = p[1].callName
		return true
	}},
	{"change trigger function ", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		// a second entry point (same body): raises this field's selection odds against
		// the growing mutation list, same reasoning as "toggle trigger fires on update "
		var pairs [][2]*pTrigger
		for i, a := range s.triggers {
			for j, b := range s.triggers {
				if i != j && a.table == b.table {
					pairs = append(pairs, [2]*pTrigger{a, b})
				}
			}
		}
		if len(pairs) == 0 {
			return false
		}
		p := pick(r, pairs)
		p[0].callName = p[1].callName
		return true
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
	{"add view", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool { return s.addViewKind(r, false) }},
	{"add materialized view", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool { return s.addViewKind(r, true) }},
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
		// its pTriggerFn is not tied to the trigger's own lifetime (see pTrigger's doc
		// comment), so this never needs to check who else might call it
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
		pick(r, s.triggers).n += 10
		return true
	}},
	{"add function", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		s.funcs = append(s.funcs, &pFunc{name: s.next("f"), n: 1 + r.Intn(9), retType: "integer", volatility: "IMMUTABLE", lang: "sql"})
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
	{"column comment", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		var cands []*pCol
		var owner []*pTable
		for _, t := range s.tables {
			for _, c := range t.cols {
				if c.gen == "" && !touched[t.name+".comment."+c.name] {
					cands = append(cands, c)
					owner = append(owner, t)
				}
			}
		}
		if len(cands) == 0 {
			return false
		}
		i := r.Intn(len(cands))
		c := cands[i]
		if c.comment == "" {
			c.comment = "about " + owner[i].name + "." + c.name
		} else {
			c.comment = ""
		}
		touched[owner[i].name+".comment."+c.name] = true
		return true
	}},
	{"change check operator", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		for _, t := range untouched(s, touched) {
			var cands []*pCheck
			for _, ck := range t.checks {
				if isNumeric(t.col(ck.col).typ) {
					cands = append(cands, ck)
				}
			}
			if len(cands) == 0 {
				continue
			}
			ck := pick(r, cands)
			ck.ge = !ck.ge
			return true
		}
		return false
	}},
	{"toggle unique nulls distinct", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		for _, t := range untouched(s, touched) {
			var cands []*pKey
			for _, k := range t.keys {
				if k.unique {
					cands = append(cands, k)
				}
			}
			if len(cands) == 0 {
				continue
			}
			k := pick(r, cands)
			k.notDist = !k.notDist
			return true
		}
		return false
	}},
	{"toggle unique deferrable", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		for _, t := range untouched(s, touched) {
			var cands []*pKey
			for _, k := range t.keys {
				// a foreign key elsewhere may rest on this unique constraint: PostgreSQL
				// refuses a deferrable one there (55000 "cannot use a deferrable unique
				// constraint for referenced table", measured)
				if k.unique && !keyIsReferenced(s, t, k) {
					cands = append(cands, k)
				}
			}
			if len(cands) == 0 {
				continue
			}
			k := pick(r, cands)
			k.defer_ = !k.defer_
			return true
		}
		return false
	}},
	{"toggle foreign key deferrable", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		for _, t := range untouched(s, touched) {
			if len(t.fks) == 0 {
				continue
			}
			pick(r, t.fks).defer_ = !pick(r, t.fks).defer_
			return true
		}
		return false
	}},
	{"change foreign key on update", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		for _, t := range untouched(s, touched) {
			if len(t.fks) == 0 {
				continue
			}
			fk := pick(r, t.fks)
			var opts []string
			for _, o := range []string{"", "CASCADE", "RESTRICT", "NO ACTION"} {
				if o != fk.onUpdate {
					opts = append(opts, o)
				}
			}
			fk.onUpdate = pick(r, opts)
			return true
		}
		return false
	}},
	{"add partial index", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		ts := untouched(s, touched)
		if len(ts) == 0 {
			return false
		}
		return s.addPartialIndex(r, pick(r, ts))
	}},
	{"add expression index", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		ts := untouched(s, touched)
		if len(ts) == 0 {
			return false
		}
		return s.addExprIndex(r, pick(r, ts))
	}},
	{"add exclude constraint", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		ts := untouched(s, touched)
		if len(ts) == 0 {
			return false
		}
		return s.addExclude(r, pick(r, ts))
	}},
	{"change index predicate", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		for _, t := range untouched(s, touched) {
			var cands []*pKey
			for _, k := range t.keys {
				if !k.unique && k.where != "" {
					cands = append(cands, k)
				}
			}
			if len(cands) == 0 {
				continue
			}
			k := pick(r, cands)
			c := t.col(k.cols[0])
			if k.where == "notnull" && isNumeric(c.typ) {
				k.where = "positive"
			} else {
				k.where = "notnull"
			}
			return true
		}
		return false
	}},
	{"toggle row security", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		ts := untouched(s, touched)
		if len(ts) == 0 {
			return false
		}
		t := pick(r, ts)
		t.rowSec = !t.rowSec
		if !t.rowSec {
			t.forceRowSec = false
		}
		return true
	}},
	{"toggle force row security", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		var cands []*pTable
		for _, t := range untouched(s, touched) {
			if t.rowSec {
				cands = append(cands, t)
			}
		}
		if len(cands) == 0 {
			return false
		}
		t := pick(r, cands)
		t.forceRowSec = !t.forceRowSec
		return true
	}},
	{"add policy", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		ts := untouched(s, touched)
		if len(ts) == 0 {
			return false
		}
		t := pick(r, ts)
		command := pick(r, []string{"ALL", "SELECT", "INSERT", "UPDATE", "DELETE"})
		using, withCheck := policyPredicates(command)
		s.policies = append(s.policies, &pPolicy{name: s.next("pol"), table: t.name, command: command,
			permissive: r.Intn(4) != 0, using: using, withCheck: withCheck})
		return true
	}},
	{"drop policy", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		var cands []int
		for i, pol := range s.policies {
			if !touched["policy:"+pol.name] {
				cands = append(cands, i)
			}
		}
		if len(cands) == 0 {
			return false
		}
		i := pick(r, cands)
		s.policies = append(s.policies[:i], s.policies[i+1:]...)
		return true
	}},
	{"change policy", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		var cands []*pPolicy
		for _, pol := range s.policies {
			if !touched["policy:"+pol.name] {
				cands = append(cands, pol)
			}
		}
		if len(cands) == 0 {
			return false
		}
		pol := pick(r, cands)
		pol.permissive = !pol.permissive
		touched["policy:"+pol.name] = true
		return true
	}},
	{"add composite attribute", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		// ADD ATTRIBUTE leaves a using column's stored values alone (measured); a table
		// using the type is not excluded here -- except one whose column default is a
		// frozen ROW(...) literal, which the new attribute count would leave stale
		var cands []*pComposite
		for _, co := range s.composites {
			if !compositeArityFrozen(s, co.name) && !touched["composite:"+co.name] {
				cands = append(cands, co)
			}
		}
		if len(cands) == 0 {
			return false
		}
		co := pick(r, cands)
		typ := "integer"
		if r.Intn(2) == 0 {
			typ = "text"
		}
		co.attrs = append(co.attrs, &pAttr{name: s.next("a"), typ: typ})
		touched["composite:"+co.name] = true
		return true
	}},
	{"drop composite attribute", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		// DROP ATTRIBUTE leaves a using column's stored values alone (measured); a table
		// using the type is not excluded here -- except one whose column default is a
		// frozen ROW(...) literal, same reason as "add composite attribute"
		var cands []*pComposite
		for _, co := range s.composites {
			if len(co.attrs) > 1 && !compositeArityFrozen(s, co.name) && !touched["composite:"+co.name] {
				cands = append(cands, co)
			}
		}
		if len(cands) == 0 {
			return false
		}
		co := pick(r, cands)
		co.attrs = co.attrs[:len(co.attrs)-1]
		touched["composite:"+co.name] = true
		return true
	}},
	{"change composite attribute type", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		// ALTER ATTRIBUTE TYPE, unlike ADD / DROP, is refused while a table uses the type
		// (0A000, measured; the planner turns it into a problem -- see alterType's
		// "composite" case): keep this one off a composite any column still carries.
		var cands []*pComposite
		for _, co := range s.composites {
			if !compositeInUse(s, co.name) && !touched["composite:"+co.name] {
				cands = append(cands, co)
			}
		}
		if len(cands) == 0 {
			return false
		}
		co := pick(r, cands)
		a := pick(r, co.attrs)
		switch a.typ {
		case "integer":
			a.typ = "bigint"
		case "bigint":
			a.typ = "integer"
		case "text":
			a.typ = "varchar(50)"
		default:
			a.typ = "text"
		}
		touched["composite:"+co.name] = true
		return true
	}},
	{"drop composite type", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		var cands []int
		for i, co := range s.composites {
			if !compositeInUse(s, co.name) && !touched["composite:"+co.name] {
				cands = append(cands, i)
			}
		}
		if len(cands) == 0 {
			return false
		}
		i := pick(r, cands)
		touched["composite:"+s.composites[i].name] = true
		s.composites = append(s.composites[:i], s.composites[i+1:]...)
		return true
	}},
	{"toggle identity kind", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		var cands []*pCol
		for _, t := range untouched(s, touched) {
			for _, c := range t.cols {
				if c.identity && !touched[t.name+"."+c.name] {
					cands = append(cands, c)
				}
			}
		}
		if len(cands) == 0 {
			return false
		}
		c := pick(r, cands)
		c.idAlways = !c.idAlways
		return true
	}},
	{"add identity", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		ts := untouched(s, touched)
		if len(ts) == 0 {
			return false
		}
		t := pick(r, ts)
		var cands []*pCol
		for _, c := range t.plainCols(touched) {
			if !isInteger(c.typ) || c.serial {
				continue
			}
			// ON DELETE SET NULL needs the column nullable; leave it alone (same guard as
			// "change nullability and default")
			settable := true
			for _, fk := range t.fks {
				if indexOf(fk.cols, c.name) >= 0 && fk.onDelete == "SET NULL" {
					settable = false
				}
			}
			if settable {
				cands = append(cands, c)
			}
		}
		if len(cands) == 0 {
			return false
		}
		c := pick(r, cands)
		c.identity, c.def = true, ""
		if c.notNull {
			c.idAlways = r.Intn(2) == 0
		} else {
			// IDENTITY implies NOT NULL: existing NULLs need a backfill before the server's
			// own SET NOT NULL (23502 "contains null values" otherwise, measured). A backfill
			// UPDATE assigns the column a plain value, which GENERATED ALWAYS forbids by
			// declaration (428C9 "can only be updated to DEFAULT") even though the UPDATE
			// runs before the column turns identity: keep the column BY DEFAULT, which an
			// explicit value is always allowed to override.
			f := s.fill(c)
			if t.inFK(c.name) {
				// a parent every table has (rows() gives every table a row 3), and never
				// row 1's or row 2's own FK value: rows() only ever nulls a nullable
				// column's third row, so this backfill only ever reaches row 3 -- reusing
				// its own row number rather than a fixed "1" cannot duplicate row 1's or
				// row 2's, which an EXCLUDE (WITH =) over this column would otherwise
				// refuse as a duplicate (23P01, measured)
				f = "3"
			}
			s.intents = append(s.intents, fmt.Sprintf("-- @migrate backfill %s.%s = %s where %s is null", t.full(), c.name, f, qi(c.name)))
			c.notNull, c.idAlways = true, false
		}
		touched[t.name+"."+c.name] = true
		return true
	}},
	{"drop identity", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		for _, t := range untouched(s, touched) {
			for _, c := range t.cols {
				if c.identity && !touched[t.name+"."+c.name] {
					c.identity = false
					touched[t.name+"."+c.name] = true
					return true
				}
			}
		}
		return false
	}},
	{"add partitioned table", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		pt := s.newPartTable(r)
		s.partTables = append(s.partTables, pt)
		touched[pt.name] = true
		return true
	}},
	{"add partition", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		pts := untouchedPartTables(s, touched)
		if len(pts) == 0 {
			return false
		}
		pt := pick(r, pts)
		name := s.next(pt.name + "_")
		if pt.strategy == "LIST" {
			used := map[string]bool{}
			for _, c := range pt.parts {
				for _, v := range c.values {
					used[v] = true
				}
			}
			letter := "d"
			for _, cand := range []string{"d", "e", "f", "g", "h"} {
				if !used[cand] {
					letter = cand
					break
				}
			}
			pt.parts = append(pt.parts, &pPartChild{name: name, orig: "public." + name, values: []string{letter}})
		} else {
			hi := 0
			for _, c := range pt.parts {
				if !c.isDefault && !c.gone && c.hi > hi {
					hi = c.hi
				}
			}
			pt.parts = append(pt.parts, &pPartChild{name: name, orig: "public." + name, lo: hi, hi: hi + 10})
		}
		touched[pt.name] = true
		return true
	}},
	{"drop partition", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		for _, pt := range untouchedPartTables(s, touched) {
			var live []*pPartChild
			for _, c := range pt.parts {
				if !c.gone && !c.detached && !c.isDefault {
					live = append(live, c)
				}
			}
			if len(live) < 2 {
				continue // keep at least one ordinary partition besides DEFAULT
			}
			c := pick(r, live)
			c.gone = true
			s.intents = append(s.intents, "-- @migrate drop "+c.orig)
			touched[pt.name] = true
			return true
		}
		return false
	}},
	{"detach partition", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		for _, pt := range untouchedPartTables(s, touched) {
			var live []*pPartChild
			for _, c := range pt.parts {
				if !c.gone && !c.detached && !c.isDefault {
					live = append(live, c)
				}
			}
			if len(live) < 2 {
				continue // keep at least one ordinary partition besides DEFAULT
			}
			c := pick(r, live)
			c.detached = true
			touched[pt.name] = true
			return true
		}
		return false
	}},
	{"attach partition", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		for _, pt := range untouchedPartTables(s, touched) {
			for _, c := range pt.parts {
				if c.detached && !c.gone {
					c.detached = false
					touched[pt.name] = true
					return true
				}
			}
		}
		return false
	}},
	{"move partition bound", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		for _, pt := range untouchedPartTables(s, touched) {
			var last *pPartChild
			for _, c := range pt.parts {
				if !c.gone && !c.detached && !c.isDefault {
					last = c // the highest RANGE bound / most recently added LIST child
				}
			}
			if last == nil {
				continue
			}
			if pt.strategy == "LIST" {
				last.values = append(last.values, "w")
			} else {
				last.hi += 5
			}
			touched[pt.name] = true
			return true
		}
		return false
	}},
	{"shrink partition bound", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		// unlike "move partition bound" (which only ever widens, so the row already
		// sitting at a child's own bound -- see pPartTable.rows -- always keeps fitting),
		// this one narrows a child's bound past that very row, so it no longer belongs and
		// the plan (partitionMovePredicate, migrate.go) has to move it back to the parent
		// before DETACH + ATTACH runs, into the DEFAULT partition (brief-holes-pg.md item
		// C). The row is not lost -- appends a DO $$ check confirming the parent's total
		// count still holds it, since diff.Compare (judge's usual check) never looks at
		// data.
		for _, pt := range untouchedPartTables(s, touched) {
			var live []*pPartChild
			for _, c := range pt.parts {
				if !c.gone && !c.detached && !c.isDefault {
					live = append(live, c)
				}
			}
			if len(live) == 0 {
				continue
			}
			c := pick(r, live)
			switch pt.strategy {
			case "LIST":
				used := map[string]bool{}
				for _, o := range pt.parts {
					for _, v := range o.values {
						used[v] = true
					}
				}
				letter := ""
				for _, cand := range []string{"m", "n", "o", "p", "q"} {
					if !used[cand] {
						letter = cand
						break
					}
				}
				if letter == "" {
					continue
				}
				c.values = []string{letter} // the row's own kind (c's former values[0]) no longer matches
			default: // RANGE
				if c.hi-c.lo < 2 {
					continue // no room to raise lo past the row sitting at the old lo and stay a valid bound
				}
				c.lo++ // the row sits exactly at the old lo (pPartTable.rows), now excluded
			}
			total := 0 // one row per part from pt.rows() that is neither gone nor detached
			for _, o := range pt.parts {
				if !o.gone && !o.detached {
					total++
				}
			}
			s.checks = append(s.checks, fmt.Sprintf(
				"DO $$ BEGIN IF (SELECT count(*) FROM %s) <> %d THEN RAISE EXCEPTION 'partition %s row count: %%', (SELECT count(*) FROM %s); END IF; END $$;",
				qi(pt.name), total, pt.name, qi(pt.name)))
			touched[pt.name] = true
			return true
		}
		return false
	}},
}

// mutations18 is PostgreSQL-18-only vocabulary (brief-holes-pg.md item E), added to
// mutations for TestMigrateProbe18 alone: VIRTUAL generated columns, and NOT ENFORCED on
// a CHECK or a foreign key. WITHOUT OVERLAPS (a temporal PRIMARY KEY / UNIQUE, needing a
// range-typed column pTable's own vocabulary does not otherwise carry) and PERIOD foreign
// keys are measured by hand instead (see alphabetKnownUnreached's own entries for why).
var mutations18 = []mutation{
	{"add virtual generated column", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		ts := untouched(s, touched)
		if len(ts) == 0 {
			return false
		}
		t := pick(r, ts)
		x := &pCol{name: s.next("c"), typ: pick(r, []string{"integer", "numeric(10,2)", "smallint"})}
		g := &pCol{name: s.next("g"), typ: "bigint", gen: x.name, genVirtual: true}
		x.fresh, g.fresh = true, true
		insertCol(r, t, g)
		insertCol(r, t, x)
		touched[t.name+"."+x.name], touched[t.name+"."+g.name] = true, true
		return true
	}},
	{"toggle generated kind", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		for _, t := range untouched(s, touched) {
			var cands []*pCol
			for _, c := range t.cols {
				if c.gen != "" && !c.fresh && !touched[t.name+"."+c.name] {
					cands = append(cands, c)
				}
			}
			if len(cands) == 0 {
				continue
			}
			c := pick(r, cands)
			c.genVirtual = !c.genVirtual
			touched[t.name+"."+c.name] = true
			return true
		}
		return false
	}},
	{"toggle check not enforced", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		for _, t := range untouched(s, touched) {
			if len(t.checks) == 0 {
				continue
			}
			ck := pick(r, t.checks)
			ck.notEnforced = !ck.notEnforced
			touched[t.name] = true
			return true
		}
		return false
	}},
	{"toggle foreign key not enforced", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		for _, t := range untouched(s, touched) {
			if len(t.fks) == 0 {
				continue
			}
			fk := pick(r, t.fks)
			fk.notEnforced = !fk.notEnforced
			touched[t.name] = true
			return true
		}
		return false
	}},
	{"add without overlaps key", func(r *rand.Rand, s *pSchema, touched map[string]bool) bool {
		ts := untouched(s, touched)
		if len(ts) == 0 {
			return false
		}
		t := pick(r, ts)
		pk := t.pk()
		if pk == nil || !isInteger(pk.typ) {
			return false
		}
		// a fresh integer scalar column and a fresh daterange column, the range last
		// (WITHOUT OVERLAPS requires it there); both backfilled from the table's own
		// primary key so every row's span is [pk, pk+1) -- distinct integers make
		// distinct, non-overlapping ranges, whatever pk's own values are.
		scalar := &pCol{name: s.next("c"), typ: pk.typ, notNull: true, fresh: true}
		span := &pCol{name: s.next("span"), typ: "daterange", notNull: true, fresh: true}
		insertCol(r, t, scalar)
		insertCol(r, t, span)
		s.intents = append(s.intents,
			fmt.Sprintf("-- @migrate backfill %s.%s = %s", t.full(), scalar.name, qi(pk.name)),
			fmt.Sprintf("-- @migrate backfill %s.%s = daterange('2024-01-01'::date + %s::integer, '2024-01-02'::date + %s::integer)",
				t.full(), span.name, qi(pk.name), qi(pk.name)))
		t.keys = append(t.keys, &pKey{name: s.next("k"), unique: true, cols: []string{scalar.name, span.name}, withoutOverlaps: true})
		s.ensureExtension("btree_gist")
		touched[t.name+"."+scalar.name], touched[t.name+"."+span.name] = true, true
		return true
	}},
}

// untouchedPartTables is untouched's counterpart for pPartTable: candidates a mutation may
// still act on, keyed the same way (touched[pt.name]) so a recipe never applies two
// partition mutations to the same partitioned table in one pair (matching how pTable
// mutations use touched[t.name] to keep a pair's changes disjoint and so unambiguous to
// re-derive @migrate declarations for).
func untouchedPartTables(s *pSchema, touched map[string]bool) []*pPartTable {
	var out []*pPartTable
	for _, pt := range s.partTables {
		if !touched[pt.name] {
			out = append(out, pt)
		}
	}
	return out
}

// policyPredicates is which of USING / WITH CHECK a policy for command needs: SELECT and
// DELETE read only (USING), INSERT writes only (WITH CHECK), ALL and UPDATE both.
func policyPredicates(command string) (using, withCheck bool) {
	switch command {
	case "SELECT", "DELETE":
		return true, false
	case "INSERT":
		return false, true
	}
	return true, true
}

type step struct {
	m    int
	seed int64
}

func mutate(muts []mutation, src *pSchema, recipe []step) (*pSchema, []string) {
	s := src.clone()
	s.mutating = true
	touched := map[string]bool{}
	var applied []string
	for _, st := range recipe {
		r := rand.New(rand.NewSource(st.seed))
		if muts[st.m].apply(r, s, touched) {
			applied = append(applied, muts[st.m].name)
		}
	}
	return s, applied
}

// ---- judging ----------------------------------------------------------------------------

type verdict struct {
	kind    string
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

func judge(ctx context.Context, srv *dump.Server, src, target *pSchema, applied []string) verdict {
	v := verdict{aSQL: src.render(), bSQL: target.render(), applied: applied}
	a, aText, err := srv.Canonical(ctx, v.aSQL, nil)
	if err != nil {
		v.kind, v.detail = "generator (source)", err.Error()
		return v
	}
	b, _, err := srv.Canonical(ctx, v.bSQL, nil)
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
	loaded := aText + "\nRESET search_path;\n" + src.rows()
	script := loaded + "\n" + strings.Join(ddl, "\n")
	if len(target.checks) > 0 {
		// a mutation's own data assertions (so far: a partition bound shrinking past a
		// row it used to hold, "shrink partition bound") run in the same script, right
		// after the plan's DDL and before judge() ever compares schemas -- a RAISE
		// EXCEPTION here surfaces as a plain Canonical error below, same as a bad DDL
		// statement would.
		script += "\n" + strings.Join(target.checks, "\n")
	}
	got, _, err := srv.Canonical(ctx, script, b)
	if err != nil {
		if _, _, rerr := srv.Canonical(ctx, loaded, a); rerr != nil {
			v.kind, v.detail = "generator (rows)", rerr.Error()+"\n"+src.rows()
			return v
		}
		v.kind, v.detail = "server refused the DDL", err.Error()
		return v
	}
	var d []string
	for _, ch := range diff.Compare(got, b) {
		if !ch.OrderOnly() {
			d = append(d, ch.String())
		}
	}
	if len(d) > 0 {
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

// alphabetKnownUnreached is diff.Alphabet's entries the gate (seed 1, 200 pairs) does not
// need to hit, each with why -- the two reasons TestMigrateProbe's gate accepts as an
// escape from adding generator vocabulary (see the assertion at the end of
// TestMigrateProbe): the planner writes no DDL for it (a note or a problem, measured in
// migrate.go), or this probe's real server can never produce a pair carrying it in the
// first place.
var alphabetKnownUnreached = map[string]string{
	"~ table inherits":              "migrate.go's alterTable only notes inherits/of type changes (\"cannot be altered by the plan\"); never DDL. Regular table INHERITS (distinct from PARTITION OF, now in this round's vocabulary) is still out of this probe's vocabulary",
	"~ table partition key":         "migrate.go's repartitionTable now has lossless DDL for this (rename aside, rebuild the target's own shape fresh, let the router move every row across, brief-holes-pg.md item D), but only for a table nothing else structurally depends on (repartitionEligible: no policy / rule / trigger / comment of its own, no identity column or sequence it owns, no other table's foreign key resting on it) -- pTable's own vocabulary deliberately keeps all of those (the same reason pPartTable never folds into pTable, see its doc comment), so the generator has no candidate that both carries this field and stays in scope; measured by hand instead (TestProbeRepartitionEmptyToRange / RangeToEmpty / RangeToList), empty -> X, X -> empty and X -> Y all pinned. A table outside repartitionEligible's scope still gets the old problem (halts apply) rather than an incomplete plan.",
	"~ table of type":               "same note-only path as table inherits; typed tables (CREATE TABLE OF) are also not in this probe's vocabulary",
	"~ domain base":                 "migrate.go's alterType domain case only notes a base type change (\"cannot be altered\"); never DDL",
	"~ range subtype":               "migrate.go's alterType range case only notes any change (\"cannot be altered\"); never DDL. The generator also has no range type vocabulary",
	"~ constraint without overlaps": "a PostgreSQL 18 constraint attribute (WITHOUT OVERLAPS). Under the 17 gate the server refuses the syntax outright. mutations18's own \"add without overlaps key\" (TestMigrateProbe18) does add such a key -- but always a brand new one (+ constraint), never a change to an existing key's own WITHOUT OVERLAPS flag (the ~ field diff.Alphabet mines), since no table in this probe's vocabulary already carries a range-typed column to build one on before the mutation runs; toggling one in place, the way \"toggle foreign key not enforced\" toggles an existing key, needs a base table seeded with one from the start (like the partition tables' own spares) -- future work, not yet wired through generate()",
	"~ constraint period":           "a PostgreSQL 18 constraint attribute (FOREIGN KEY ... PERIOD, needing a WITHOUT OVERLAPS primary key on the referenced side): under the 17 gate, the server itself refuses the syntax outright (same ceiling as \"without overlaps\"); brief-holes-pg.md item E calls it optional (\"時間があれば\") and mutations18 does not add it, so it stays an accepted excuse under the 18 gate too, unlike without overlaps / not enforced / generated kind",
	"~ column generated kind":       "a PostgreSQL 18 generated-column form (VIRTUAL, versus this probe's STORED); same PostgreSQL 17 server ceiling as \"without overlaps\"",
	"~ constraint not enforced":     "a PostgreSQL 18 constraint attribute (NOT ENFORCED); same PostgreSQL 17 server ceiling as \"without overlaps\"",
	"~ index unique":                "structurally unreachable (measured): pg_dump always renders a named UNIQUE constraint as ALTER TABLE ... ADD CONSTRAINT, never a CREATE INDEX + ADD CONSTRAINT ... UNIQUE USING INDEX pair, so toggling a key between UNIQUE and a plain index changes its diff.Change Kind (constraint <-> index) rather than the same-named index's \"unique\" property -- no vocabulary addition reaches it, since the generator would have to reproduce a pg_dump form pg_dump itself never produces",
}

// pg18OnlyUnreached is the subset of alphabetKnownUnreached that stops being an accepted
// excuse under TestMigrateProbe18 (brief-holes-pg.md item E): PostgreSQL 18 syntax
// mutations18 exercises there, so alphabetKnownUnreached's own text (written for the 17
// gate, where the server itself refuses the syntax) does not apply -- isKnownUnreached
// reports these as needing an actual hit instead once pg18 is true.
var pg18OnlyUnreached = map[string]bool{
	"~ column generated kind":   true,
	"~ constraint not enforced": true,
}

// isKnownUnreached is alphabetKnownUnreached's excuse for s, "" when none applies --
// except a pg18OnlyUnreached entry under TestMigrateProbe18, whose whole excuse was the 17
// server's syntax ceiling: gone once pg18 is true, so it must be hit like anything else.
func isKnownUnreached(s string, pg18 bool) string {
	if pg18 && pg18OnlyUnreached[s] {
		return ""
	}
	return alphabetKnownUnreached[s]
}

// tallyHit folds one judged pair's diff.Compare output into the alphabet coverage set:
// every Alter's Fields as their own (Op, Kind, Field) entries, every Add / Drop as one
// (Op, Kind) entry with no Field (matching diff.Alphabet's own grouping for rows and
// comment's single field, and for schema / extension which are never Alter).
func tallyHit(hit map[string]bool, changes []diff.Change) {
	for _, ch := range changes {
		if ch.Op == diff.Alter {
			for _, f := range ch.Fields {
				hit[diff.AlphabetEntry{Op: ch.Op, Kind: ch.Kind, Field: diff.NormalizeField(ch.Kind, f.Name)}.String()] = true
			}
			continue
		}
		hit[diff.AlphabetEntry{Op: ch.Op, Kind: ch.Kind}.String()] = true
	}
}

// directedAttempts bounds how many freshly generated schemas one mutation gets to find a
// candidate against on its own, in directedCoverage, before it is reported unable to
// apply alone.
const directedAttempts = 30

// directedCoverage runs one pair per mutation -- that mutation alone against a freshly
// generated schema, retried against a new schema up to directedAttempts times if it finds
// no candidate -- so the alphabet coverage gate no longer depends on TestMigrateProbe's
// random pairs happening to draw every mutation at least once. That dependency is real:
// adding a mutation shifts every later draw's position in the PRNG sequence, so a vocabulary
// addition can silently stop a *different*, already-covered alphabet entry from being hit
// by the random pairs at the same seed (brief-holes-common.md's whole reason for this
// pass). judge and the finding / hit bookkeeping are exactly what the random loop does;
// unlike a random pair (up to 7 mutations), a directed pair is already minimal, so there is
// nothing to shrink if it turns up a finding.
func directedCoverage(t *testing.T, ctx context.Context, srv *dump.Server, muts []mutation, pg18 bool, seed int64, counts, byMutation map[string]int, hit map[string]bool, findings *[]verdict) []string {
	var stuck []string
	for mi, m := range muts {
		r := rand.New(rand.NewSource(seed*100003 + int64(mi)))
		var src, target *pSchema
		var applied []string
		ok := false
		for attempt := 0; attempt < directedAttempts; attempt++ {
			src = generate(r, pg18)
			target, applied = mutate(muts, src, []step{{m: mi, seed: r.Int63()}})
			if len(applied) > 0 {
				ok = true
				break
			}
		}
		if !ok {
			stuck = append(stuck, m.name)
			continue
		}
		v := judge(ctx, srv, src, target, applied)
		for _, a := range applied {
			byMutation[a]++
		}
		tallyHit(hit, v.changes)
		switch {
		case v.kind == "":
			counts["pass"]++
		case strings.HasPrefix(v.kind, "generator"):
			counts[v.kind]++
			if counts[v.kind] <= 3 {
				t.Logf("%s (directed, %s): %s\n%s", v.kind, m.name, v.detail, v.aSQL)
			}
		default:
			counts["finding"]++
			*findings = append(*findings, v)
		}
	}
	return stuck
}

func TestMigrateProbe(t *testing.T) {
	requirePgDump(t)
	runMigrateProbe(t, server, mutations, false, "postgres", *probeSeed, *probeN)
}

// TestMigrateProbe18 is TestMigrateProbe against an embedded PostgreSQL 18 instead of 17,
// with mutations18 added to the vocabulary (brief-holes-pg.md item E): VIRTUAL generated
// columns, NOT ENFORCED CHECK / FOREIGN KEY, WITHOUT OVERLAPS PRIMARY KEY / UNIQUE. Its own
// gate, seed 1 / 200 pairs, same as the 17 one; run separately (-run TestMigrateProbe18).
func TestMigrateProbe18(t *testing.T) {
	requirePgDump18(t)
	muts := append(append([]mutation(nil), mutations...), mutations18...)
	runMigrateProbe(t, server18, muts, true, "postgres18", *probeSeed, *probeN)
}

// runMigrateProbe is TestMigrateProbe's body, shared with TestMigrateProbe18: srv is which
// embedded server judge() applies DDL to, muts the vocabulary directedCoverage and the
// random pairs draw from, pg18 whether generate() declares `-- sqlshape: postgres 18`
// (schema.DeclaredVersion) and which alphabetKnownUnreached entries stop being an accepted
// excuse (isKnownUnreached).
func runMigrateProbe(t *testing.T, srv *dump.Server, muts []mutation, pg18 bool, reportName string, seed int64, n int) {
	ctx := context.Background()
	r := rand.New(rand.NewSource(seed))
	counts := map[string]int{}
	byMutation := map[string]int{}
	hit := map[string]bool{}
	var findings []verdict
	for i := 0; i < n; i++ {
		src := generate(r, pg18)
		nmut := 1 + r.Intn(5)
		var recipe []step
		for j := 0; j < nmut; j++ {
			recipe = append(recipe, step{m: r.Intn(len(muts)), seed: r.Int63()})
		}
		target, applied := mutate(muts, src, recipe)
		if len(applied) == 0 {
			continue
		}
		v := judge(ctx, srv, src, target, applied)
		for _, a := range applied {
			byMutation[a]++
		}
		tallyHit(hit, v.changes)
		if v.kind == "" {
			counts["pass"]++
			continue
		}
		if strings.HasPrefix(v.kind, "generator") {
			counts[v.kind]++
			if counts[v.kind] <= 3 {
				t.Logf("%s (pair %d, %s): %s\n%s", v.kind, i, strings.Join(applied, "; "), v.detail, v.aSQL)
			}
			continue
		}
		counts["finding"]++
		for j := 0; j < len(recipe); {
			shorter := append(append([]step(nil), recipe[:j]...), recipe[j+1:]...)
			tgt, app := mutate(muts, src, shorter)
			if len(app) == 0 {
				j++
				continue
			}
			if w := judge(ctx, srv, src, tgt, app); w.kind == v.kind {
				recipe, v = shorter, w
				continue
			}
			j++
		}
		findings = append(findings, v)
	}
	randomHit := len(hit)
	stuck := directedCoverage(t, ctx, srv, muts, pg18, seed, counts, byMutation, hit, &findings)
	combinedHit := len(hit)

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
	t.Logf("migrate probe (%s), seed %d, %d pairs:\n%s", reportName, seed, n, summary.String())

	seen := map[string]bool{}
	var report strings.Builder
	fmt.Fprintf(&report, "# migrate probe (%s), seed %d, %d pairs\n\n%s\n", reportName, seed, n, summary.String())
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
	fmt.Fprintf(&report, "## alphabet coverage\n\n%d entries, %d hit (%d by the random pairs alone, %d with directed coverage added)\n\n",
		len(alphabet), len(hit), randomHit, combinedHit)
	if len(stuck) > 0 {
		fmt.Fprintf(&report, "%d mutation(s) never found a candidate alone in %d attempts: %s\n\n", len(stuck), directedAttempts, strings.Join(stuck, ", "))
	}
	for _, e := range alphabet {
		s := e.String()
		switch {
		case hit[s]:
			fmt.Fprintf(&report, "- [x] %s\n", s)
		case isKnownUnreached(s, pg18) != "":
			fmt.Fprintf(&report, "- [ ] %s -- known unreached: %s\n", s, isKnownUnreached(s, pg18))
		default:
			fmt.Fprintf(&report, "- [ ] %s -- MISSED\n", s)
			missed = append(missed, s)
		}
	}
	// a known-unreached entry diff.Alphabet no longer lists is stale (the field moved,
	// was renamed, or the planner learned to emit DDL for it): flag it so the list stays
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
	if len(stuck) > 0 {
		t.Errorf("%d mutation(s) never found a candidate alone in %d directed attempts: %s", len(stuck), directedAttempts, strings.Join(stuck, ", "))
	}
}
