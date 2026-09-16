package migrate

// Split from probe_test.go (see its header for the probe's overall design): the schema
// model (types), the type vocabulary, and rendering the model to DDL / DML. Pure move --
// no behavior here changed by the split.

import (
	"fmt"
	"strings"
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
	// id is pt's own "id" column (see render(), rows()): a plain integer by default, or one
	// carrying a sequence a repartition (change partition strategy / departition table) has
	// to carry across the rebuild -- bigserial (id.serial, a column-owned sequence
	// detectRepartitions/repartitionTable now detach and re-own) or IDENTITY (id.identity,
	// the server's own -- BY DEFAULT or, with id.idAlways, ALWAYS), covering the vocabulary
	// brief-pg-repartition-seq.md widened repartitionTable's scope into. Reuses pCol/pCol.render
	// rather than its own text, so its SQL matches pTable's own bigserial / IDENTITY columns
	// exactly.
	id *pCol
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
	// withPeriod (PostgreSQL 18 only): the last column of cols / refCols is PERIOD -- a
	// temporal foreign key, needing a WITHOUT OVERLAPS primary key or unique constraint on
	// the referenced side over exactly refCols (pKey.withoutOverlaps; mutations18 alone
	// sets this).
	withPeriod bool
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
		if pt.id != nil {
			nid := *pt.id
			npt.id = &nid
		}
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
		s := "  CONSTRAINT " + qi(fk.name) + " FOREIGN KEY (" + periodList(fk.cols, fk.withPeriod) + ") REFERENCES " + s.qref(fk.refTable) + " (" + periodList(fk.refCols, fk.withPeriod) + ")"
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
// (pPartChild.detached); a gone one (dropped outright, declared) writes nothing. A detached
// child keeps pt's own id column definition, matching a real DETACH PARTITION (which leaves
// the child's column properties exactly as they were while attached, type and identity
// included; 42804 "different type for column" / 55000 "not an identity column" otherwise,
// measured, once "attach partition" / "detach partition" render it plain regardless).
// "attach partition" and "detach partition" (probe_mutations_test.go) only ever pick a
// plain-id pt to begin with, so this never has to carry a bigserial / IDENTITY column's own
// owned sequence across a detach -- out of this round's scope (brief-pg-repartition-seq.md's
// repartitionTable widening is about a whole table's own partition key changing, not a
// standalone table with its own sequence joining or leaving one; measured to otherwise
// leave a partition-attach plan's DROP SEQUENCE racing the row move, 2BP01).
func (pt *pPartTable) render() string {
	col, keyword := "id", "RANGE"
	if pt.strategy == "LIST" {
		col, keyword = "kind", "LIST"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "CREATE TABLE %s (%s, kind text NOT NULL) PARTITION BY %s (%s);\n", qi(pt.name), pt.idCol().render(), keyword, qi(col))
	for _, c := range pt.parts {
		if c.gone {
			continue
		}
		if c.detached {
			b.WriteString("CREATE TABLE " + qi(c.name) + " (" + pt.idCol().render() + ", kind text NOT NULL);\n")
			continue
		}
		b.WriteString("CREATE TABLE " + qi(c.name) + " PARTITION OF " + qi(pt.name) + " " + c.bound() + ";\n")
	}
	return b.String()
}

// idCol is pt.id, defaulted to a plain "id integer NOT NULL" the first time it is asked for
// (pTableFromPart and any pPartTable a test constructs directly, unlike newPartTable, never
// set pt.id themselves): keeps render()/rows() from having to nil-check it everywhere.
func (pt *pPartTable) idCol() *pCol {
	if pt.id == nil {
		pt.id = &pCol{name: "id", typ: "integer", notNull: true}
	}
	return pt.id
}

// maxRowID is the highest "id" rows() gives pt under its *current* parts, before any
// pending mutation replaces them: a RANGE child's own lower bound is its one row's id, and a
// LIST / DEFAULT row's is 100000 plus its position among the live parts (mirrors rows()'s
// own construction). A repartition (change partition strategy / departition table) computes
// this ahead of rebuilding pt.parts, so its own continuation check (an id-owning pt only)
// knows what a fresh row's id ought to pick up from.
func (pt *pPartTable) maxRowID() int {
	max, i := 0, 0
	for _, c := range pt.parts {
		if c.gone || c.detached {
			continue
		}
		i++
		id := 100000 + i
		if !c.isDefault && pt.strategy != "LIST" {
			id = c.lo
		}
		if id > max {
			max = id
		}
	}
	return max
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
	// a GENERATED ALWAYS AS IDENTITY id column refuses an explicit value without this
	// (428C9, measured) -- BY DEFAULT and a plain bigserial both accept one either way, so
	// this is harmless there too.
	overriding := ""
	if pt.idCol().identity && pt.idCol().idAlways {
		overriding = " OVERRIDING SYSTEM VALUE"
	}
	i, max := 0, 0
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
		if id > max {
			max = id
		}
		fmt.Fprintf(&b, "INSERT INTO %s (id, kind)%s VALUES (%d, '%s');\n", qi(pt.name), overriding, id, kind)
	}
	// an explicit id, serial or IDENTITY, never advances the sequence backing it on its own
	// (measured: PostgreSQL only calls nextval() through the column's own DEFAULT, never as
	// a side effect of an explicit value bypassing it) -- a later plain INSERT (no id given)
	// would otherwise collide back at the sequence's own start value, not continue from
	// what these rows already hold. Every repartition continuation check
	// (repartitionSeqCheck, probe_mutations_test.go) assumes this has already run.
	if max > 0 && (pt.idCol().serial || pt.idCol().identity) {
		fmt.Fprintf(&b, "SELECT setval(pg_get_serial_sequence(%s, 'id'), %d, true);\n", lit(qi(pt.name)), max)
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
	case "daterange":
		// distinct, non-overlapping per row (i and i+1 are consecutive days): the
		// PostgreSQL 18 temporal (WITHOUT OVERLAPS) key generate() seeds needs its rows
		// distinct in this column alone, whatever its other, ordinary column holds.
		return fmt.Sprintf("daterange('2024-01-%02d'::date, '2024-01-%02d'::date)", i, i+1)
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
