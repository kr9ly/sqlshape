// Package schema layers a user's schema.sql (tables, views, enums, domains,
// composites, functions, indexes) over the bootstrap catalog. It parses DDL
// with libpg_query and resolves column / argument types; expression bodies
// (view queries, CHECK, DEFAULT) are kept as AST for the analyzer.
package schema

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	pg_query "github.com/pganalyze/pg_query_go/v6"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/kr9ly/sqlshape/internal/catalog"
)

// Expr is an unanalyzed expression from the DDL (kept as libpg_query AST).
type Expr = *pg_query.Node

// Schema is the loaded user schema.
type Schema struct {
	Catalog *catalog.Catalog
	Types   *Types

	Relations []*Relation
	Functions []*Function
	Triggers  []*Trigger
	Comments  map[string]string // "table" / "table.column" / "type:name" → comment
	// Operators and Casts are the user-defined ones (CREATE OPERATOR / CREATE CAST); the
	// analyzer consults them after the catalog's.
	Operators []*catalog.Operator
	Casts     []*catalog.Cast

	// Problems are statements the loader could not apply. Loading continues past them.
	Problems []Problem

	relByName  map[string]*Relation
	schemas    map[string]bool      // CREATE SCHEMA names
	sysRels    map[string]*Relation // system relations named so far (systemRelation)
	pending    []string             // directives preceding the statement being applied
	nextOID    catalog.OID
	searchPath []string // SET search_path, nil = public
	// datetime input GUCs (SET datestyle / intervalstyle / timezone); "" = PG default
	dateOrder, intervalStyle, timeZone string
	// TypeDefs holds the statement text that created each user type (enum, domain,
	// composite, range), for regenerating it elsewhere.
	TypeDefs map[catalog.OID]string
	// stmtText is the text of the statement being applied (without leading comments).
	stmtText string
	// ViewHook runs after every CREATE (MATERIALIZED) VIEW with the schema as it stands
	// at that point; the analyzer installs it to fill Relation.Frozen.
	ViewHook func(s *Schema, rel *Relation)
	// NotNullHook runs after a table's column NOT NULL changes (ALTER COLUMN SET/DROP NOT
	// NULL, or ADD CONSTRAINT PRIMARY KEY, which implies it), passing the table relation.
	// PG does not freeze a view's nullability the way it freezes its column list -- a view
	// reads the referenced column's current attnotnull on every query -- so the analyzer
	// installs this to re-derive Relation.Frozen's Nullable (and Type) for every view that
	// depends on rel, transitively, in declaration order (DependentViews).
	NotNullHook func(s *Schema, rel *Relation)

	prepared      map[string]*pg_query.Node // PREPARE name AS query, for CREATE TABLE AS EXECUTE
	xmlDocument   bool                      // SET xmloption = document
	restrictViews bool                      // SET restrict_nonsystem_relation_kind includes view
}

// Problem is a DDL statement (or part) that was skipped or rejected.
type Problem struct {
	Location int32 // byte offset into the source, 0 if unknown
	Message  string
}

func (p Problem) String() string { return fmt.Sprintf("@%d: %s", p.Location, p.Message) }

// ViewColumn is one frozen output column of a view (see Relation.Frozen).
type ViewColumn struct {
	Name      string
	Type      TypeRef
	Nullable  bool
	Collation string // the column's collation name, "" for none / default
	// SrcRel / Src: the base relation and column this output column is a plain reference
	// to (writes through the view land there); nil for computed columns. Pointers, so a
	// later RENAME of the base column is followed the way PG follows attnums. When the
	// base is itself a view (no Column objects) SrcColumn holds the column's name instead,
	// and SrcTable the base's full name when it is not a user relation (a catalog table).
	SrcRel    *Relation
	Src       *Column
	SrcTable  string
	SrcColumn string
}

// RelKind distinguishes tables from views.
type RelKind byte

const (
	Table    RelKind = 'r'
	View     RelKind = 'v'
	MatView  RelKind = 'm'
	Sequence RelKind = 'S'
)

// Relation is a table or view with its columns and constraints.
type Relation struct {
	OID     catalog.OID // synthetic pg_class oid
	Schema  string
	Name    string
	Kind    RelKind
	RowType catalog.OID // the composite type of a row
	// OfType is the composite type of a typed table (CREATE TABLE ... OF type), 0 otherwise.
	OfType  catalog.OID
	Columns []*Column
	// Constraints: PK / UNIQUE / FK / CHECK on the table, plus unique indexes (as Unique).
	Constraints []*Constraint
	// Query is the defining query of a view / matview (nil for tables).
	Query Expr
	// Definition is the text of the CREATE statement that made the relation; Alters the
	// ALTER TABLE statements that shaped it afterwards, other than ADD CONSTRAINT (a
	// constraint keeps its own Definition).
	Definition string
	Alters     []string
	// ColumnAliases are explicit column names given to a view (CREATE VIEW v (a, b) AS ...).
	ColumnAliases []string
	// CheckOption (views): WITH [LOCAL|CASCADED] CHECK OPTION, 0 when none. 'l' local: an
	// INSERT/UPDATE through the view is checked only against this view's own WHERE.
	// 'c' cascaded: also against the WHERE of every updatable view underneath it.
	CheckOption byte
	// Frozen (views / matviews) is the output column list as it was when the view was
	// created, filled by the ViewHook: PG fixes a view's columns at CREATE time (a later
	// RENAME / ADD COLUMN on a base table does not reach it), so readers use this rather
	// than re-resolving Query against the current schema. Nil when no hook ran or the
	// body did not analyze.
	Frozen []ViewColumn
	// ViewDefaults (views): ALTER VIEW ... ALTER COLUMN SET DEFAULT, by column name; an
	// INSERT through the view uses these in place of the base column's default.
	ViewDefaults map[string]Expr
	// Indexes are every index on the table (unique ones also appear in Constraints), for
	// the advisory "no index leads with a predicate column".
	Indexes []*Index
	// queryRefs caches the range vars of Query (see queryRangeVars); queryRefsFor is the
	// tree they were taken from.
	queryRefs    []rangeRef
	queryRefsFor Expr
	// Visible is the `-- sqlshape: visible where <predicate>` policy: rows of the table are
	// only meant to be seen through this predicate, so every statement reading it (and
	// every view over it) must carry the predicate. Nil when none.
	Visible Expr
	// Unfiltered (views): tables a `-- sqlshape: unfiltered t1, t2` directive before the
	// CREATE VIEW exempts from their visibility policy inside this view's query.
	Unfiltered map[string]bool
	// Waived (views): the obligations the view's definition opts out of, by table, from
	// `-- sqlshape: waive t1, t2 pinned(x)` and `unfiltered` directives before the CREATE:
	// "*" waives every obligation on the table, "unfiltered" the predicate ones, anything
	// else names one obligation's body (internal/obligation).
	Waived map[string][]string
	// Directives are the `-- sqlshape: ...` lines written before the CREATE, verbatim
	// (normalized whitespace), for the packages whose grammar they belong to
	// (internal/obligation reads `require ...`). The loader keeps them all, whether or
	// not it interprets them itself.
	Directives []string
	// InsteadRules (views): the write commands ("insert" / "update" / "delete") a
	// CREATE RULE ... DO INSTEAD makes the view take.
	InsteadRules map[string]bool
	// Parents are the tables this one INHERITS from / is a PARTITION OF.
	Parents []*Relation
	// Temp: CREATE TEMP; the relation is modeled in the creation schema (hiding a
	// permanent one of the same name) but PG keeps it in pg_temp
	Temp bool
	// IsPartition: created as PARTITION OF (dropped with its parent).
	IsPartition bool
	// OwnedBy (sequences): "schema.table.column" of the serial / identity column, or the
	// ALTER SEQUENCE ... OWNED BY target; the sequence goes with the table or column.
	OwnedBy string
	// OnCommitDrop: a temporary table created ON COMMIT DROP (gone at transaction end).
	OnCommitDrop bool
	// Seed (tables): the rows the schema text's INSERT statements give the table, nil
	// for a table that holds runtime data.
	Seed *Seed
	// Policies are the table's row-level security policies (rls.go); RowSecurity is ALTER
	// TABLE ... ENABLE ROW LEVEL SECURITY (without it the policies do not apply), and
	// ForceRowSecurity makes them apply to the table's owner as well.
	Policies         []*Policy
	RowSecurity      bool
	ForceRowSecurity bool
	// PartKey (partitioned tables): the columns the partition key names or its expressions
	// reference (they cannot be dropped; a type they depend on takes the table with it);
	// PartKeyFuncs: the functions the key expressions call (DROP FUNCTION CASCADE takes
	// the table).
	PartKey, PartKeyFuncs []string
	// QualifiedRules (views): write commands that have a conditional DO INSTEAD rule
	// (WHERE ...), which does not make the view take the write but does stop it from
	// being auto-updatable.
	QualifiedRules map[string]bool
	// RuleNoReturning: write commands taken by an unconditional DO INSTEAD rule whose
	// single action has no RETURNING (RETURNING on the command is 0A000).
	RuleNoReturning map[string]bool
	// RuleCTEUnsupported: write commands with a rule a data-modifying WITH item cannot use
	// (anything but one unconditional DO INSTEAD INSERT / UPDATE / DELETE).
	RuleCTEUnsupported map[string]bool
	// RuleInsertSelect: write commands with a DO INSTEAD INSERT ... SELECT action (0A000
	// when the query also has data-modifying WITH items).
	RuleInsertSelect map[string]bool
	// RuleNames maps each enabled rule's name to its event.
	RuleNames map[string]string
	// rules are the relation's rules by name; rulesOff the disabled ones. The Rule* maps
	// are derived from them (rebuildRules).
	rules    map[string]*pg_query.RuleStmt
	rulesOff map[string]bool
	// RuleEvents: the write commands ("insert" / "update" / "delete") that have any rule
	// on the relation (MERGE refuses an action kind that has one).
	RuleEvents map[string]bool
}

// InheritsFrom reports whether rel is anc or descends from it.
func (rel *Relation) InheritsFrom(anc *Relation) bool {
	if rel == anc {
		return true
	}
	for _, p := range rel.Parents {
		if p.InheritsFrom(anc) {
			return true
		}
	}
	return false
}

// Column is one attribute.
type Column struct {
	Num       int16 // 1-based attnum
	Name      string
	Type      TypeRef
	NotNull   bool
	Default   Expr // raw DEFAULT expression, nil if none
	Identity  byte // 'a' always / 'd' by default / 0
	Generated Expr // GENERATED ALWAYS AS (expr) STORED
	Collation string
	// Inherited: the column comes from a parent table (attinhcount > 0); LocalDef: the
	// child declares it too (attislocal). Dropping the parent's column drops an inherited
	// column without a local definition; a child cannot drop an inherited column.
	Inherited, LocalDef bool
	// Values is the closed value set a `CHECK (col IN ('a', 'b'))` constraint gives the
	// column (nil when none): a value set the checker diffs with Go constants like enum labels.
	Values []string
}

// ConstraintKind is the constraint flavour.
type ConstraintKind byte

const (
	PrimaryKey ConstraintKind = 'p'
	Unique     ConstraintKind = 'u'
	ForeignKey ConstraintKind = 'f'
	Check      ConstraintKind = 'c'
	Exclude    ConstraintKind = 'x'
)

// Constraint is a table constraint. Unique indexes are recorded as Unique constraints
// (with Predicate set when partial).
type Constraint struct {
	Name    string
	Kind    ConstraintKind
	Columns []string
	// ForeignKey:
	RefTable   string   // schema-qualified unless public
	RefColumns []string // empty = referenced table's primary key
	// Check:
	Expr Expr
	// Partial unique index:
	Predicate        Expr
	NullsNotDistinct bool
	Deferrable       bool
	// Exclude: the access method (e.g. "gist") and, one per Columns entry, the WITH
	// operator (EXCLUDE USING gist (room WITH =, during WITH &&)). Predicate above doubles
	// as its optional WHERE clause.
	AccessMethod string
	Operators    []string
	// ForeignKey actions (pg_constraint confdeltype / confupdtype):
	// 'a' NO ACTION, 'r' RESTRICT, 'c' CASCADE, 'n' SET NULL, 'd' SET DEFAULT
	OnDelete, OnUpdate byte
	// Definition is the text of the ALTER TABLE ... ADD CONSTRAINT that made it, "" when
	// it was declared inside CREATE TABLE.
	Definition string
}

// Function is a user-defined function / procedure signature (body is not analyzed).
type Function struct {
	OID     catalog.OID
	Schema  string
	Name    string
	Args    []FuncArg
	RetType TypeRef // Record for RETURNS TABLE / OUT params without explicit type
	RetSet  bool
	IsProc  bool
	// IsWindow marks CREATE FUNCTION ... WINDOW (callable with OVER only).
	IsWindow bool
	// IsAgg marks a CREATE AGGREGATE (RetType is the final / state type); AggKind is
	// pg_aggregate.aggkind: n normal, o ordered-set, h hypothetical-set.
	IsAgg    bool
	AggKind  byte
	Language string
	Volatile byte // i / s / v (default v)
	Strict   bool
	// SecurityDefiner: the function runs with its owner's privileges, so the owner's
	// tables' row-level security policies do not apply inside it unless forced.
	SecurityDefiner bool
	// NotNull is the `-- sqlshape: not null` annotation: the result is never NULL (PG
	// cannot tell; the schema author asserts it).
	NotNull bool
	// Raises are the `-- sqlshape: error XX001 = Name` annotations: SQLSTATEs the function
	// (typically a trigger function) raises on purpose.
	Raises []RaisedError
	// Body is the `AS $$ ... $$` text of a LANGUAGE sql function (analyzable); SQLBody the
	// parsed BEGIN ATOMIC body (a List of statements, or a ReturnStmt). Nil for other languages.
	Body    string
	SQLBody Expr
	// Definition is the text of the CREATE FUNCTION statement.
	Definition string
}

// RaisedError is a custom SQLSTATE a function raises, with its application-side name.
type RaisedError struct {
	Code string
	Name string
}

// Index is an index on a table; Columns is empty when the leading element is an expression.
type Index struct {
	Name      string
	Columns   []string
	Unique    bool
	Predicate Expr
	// Definition is the text of the CREATE INDEX statement.
	Definition string
	// nameParts is what PG's ChooseIndexNameAddition sees: every index element's column
	// name, "expr" for expressions. Copies of the index (partitions, LIKE) are renamed from it.
	nameParts []string
}

// chooseIndexName is ChooseIndexName + ChooseRelationName for an unnamed CREATE INDEX:
// <table>_<elem1>_<elem2>_idx, the pieces cut down to fit NAMEDATALEN, a taken name
// numbered idx1, idx2, ... Index names share the relation namespace of their schema.
func (s *Schema) chooseIndexName(rel *Relation, parts []string) string {
	// ChooseIndexNameAddition: append "<name>_" while the buffer stays under NAMEDATALEN
	var add strings.Builder
	for _, p := range parts {
		if add.Len()+len(p) >= 63 {
			break
		}
		add.WriteString(p)
		add.WriteByte('_')
	}
	addition := strings.TrimSuffix(add.String(), "_")
	taken := func(n string) bool {
		for _, r := range s.Relations {
			if r.Schema != rel.Schema {
				continue
			}
			if r.Name == n {
				return true
			}
			for _, ix := range r.Indexes {
				if ix.Name == n {
					return true
				}
			}
			for _, c := range r.Constraints {
				if c.Name == n && (c.Kind == PrimaryKey || c.Kind == Unique) {
					return true
				}
			}
		}
		return false
	}
	for pass := 0; ; pass++ {
		label := "idx"
		if pass > 0 {
			label += strconv.Itoa(pass)
		}
		if n := makeObjectName(rel.Name, addition, label); !taken(n) {
			return n
		}
	}
}

// makeObjectName is PG's makeObjectName: name1[_name2]_label, the longer of name1 / name2
// trimmed until the whole fits in NAMEDATALEN-1 bytes. Truncation never splits a multibyte
// character (PG's pg_mbcliplen), matching the mb-safe truncation CREATE TABLE already does
// for over-long identifiers.
func makeObjectName(name1, name2, label string) string {
	overhead := len(label) + 1
	if name2 != "" {
		overhead++
	}
	n1, n2 := len(name1), len(name2)
	for n1+n2 > 63-overhead {
		if n1 > n2 {
			n1--
		} else {
			n2--
		}
	}
	n1 = mbClip(name1, n1)
	n2 = mbClip(name2, n2)
	name := name1[:n1]
	if name2 != "" {
		name += "_" + name2[:n2]
	}
	return name + "_" + label
}

// mbClip is pg_mbcliplen: the greatest byte length <= n that does not split a UTF-8
// character in s.
func mbClip(s string, n int) int {
	if n >= len(s) {
		return len(s)
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return n
}

// Trigger is a row / statement trigger on a table.
type Trigger struct {
	Name     string
	Table    string // FullName of the table
	Insert   bool
	Update   bool
	Delete   bool
	UpdateOf []string // UPDATE OF col, col: the trigger fires only when one of these is assigned (empty: any)
	Function string   // schema-qualified unless public
	// Definition is the text of the CREATE TRIGGER statement.
	Definition string
}

// FuncArg is one parameter.
type FuncArg struct {
	Name       string
	Type       TypeRef
	Mode       byte // i in, o out, b inout, v variadic, t table
	HasDefault bool
}

// Load parses schemaSQL and builds the Schema over the embedded bootstrap catalog.
func Load(schemaSQL string) (*Schema, error) {
	cat, err := catalog.Load()
	if err != nil {
		return nil, err
	}
	return LoadWith(cat, schemaSQL)
}

// LoadWith is Load with an explicit catalog.
func LoadWith(cat *catalog.Catalog, schemaSQL string) (*Schema, error) {
	return LoadWithHook(cat, schemaSQL, nil)
}

// LoadWithHook is LoadWith with a ViewHook installed before the first statement applies.
func LoadWithHook(cat *catalog.Catalog, schemaSQL string, hook func(*Schema, *Relation)) (*Schema, error) {
	return LoadWithHooks(cat, schemaSQL, hook, nil)
}

// LoadWithHooks is LoadWithHook with a NotNullHook installed too, before the first
// statement applies.
func LoadWithHooks(cat *catalog.Catalog, schemaSQL string, viewHook, notNullHook func(*Schema, *Relation)) (*Schema, error) {
	tree, err := pg_query.Parse(schemaSQL)
	if err != nil {
		return nil, fmt.Errorf("parse schema: %w", err)
	}
	// CREATE EXTENSION merges the extension's dumped catalog (types, functions, operators,
	// casts) under everything else, so it is resolved before any statement is applied
	var exts []string
	var extProblems []Problem
	for _, raw := range tree.Stmts {
		ce := raw.Stmt.GetCreateExtensionStmt()
		if ce == nil {
			continue
		}
		if _, err := catalog.ReadExtension(ce.Extname); err != nil {
			extProblems = append(extProblems, Problem{Location: raw.StmtLocation, Message: err.Error()})
			continue
		}
		exts = append(exts, ce.Extname)
	}
	if len(exts) > 0 {
		cat, err = cat.WithExtensions(exts)
		if err != nil {
			return nil, err
		}
	}
	s := &Schema{
		Catalog:     cat,
		Types:       newTypes(cat),
		Comments:    map[string]string{},
		TypeDefs:    map[catalog.OID]string{},
		relByName:   map[string]*Relation{},
		nextOID:     FirstUserOID + 100000, // relations / functions live in a separate range from types
		ViewHook:    viewHook,
		NotNullHook: notNullHook,
	}
	s.Problems = append(s.Problems, extProblems...)
	s.applyAll(tree, schemaSQL)
	return s, nil
}

// applyAll applies every statement of a parsed schema text in order.
func (s *Schema) applyAll(tree *pg_query.ParseResult, schemaSQL string) {
	prev := int32(0)
	for _, raw := range tree.Stmts {
		// `-- sqlshape: ...` comment lines in front of a statement annotate it (a statement's
		// span starts right after the previous semicolon, so the comments sit inside it)
		end := raw.StmtLocation + raw.StmtLen
		if raw.StmtLen == 0 {
			end = int32(len(schemaSQL))
		}
		span := schemaSQL[prev:end]
		lead := leadingComments(span)
		s.pending = directives(lead)
		s.stmtText = strings.TrimSuffix(strings.TrimSpace(span[len(lead):]), ";")
		s.apply(raw.Stmt, raw.StmtLocation)
		prev = end
	}
}

// Apply extends a loaded schema with more DDL, as if it had been appended to the text
// Load saw. CREATE EXTENSION cannot be added after the fact (the catalog is fixed at
// Load): it returns ErrNeedsReload and leaves the schema untouched.
func (s *Schema) Apply(schemaSQL string) error {
	tree, err := pg_query.Parse(schemaSQL)
	if err != nil {
		return fmt.Errorf("parse schema: %w", err)
	}
	for _, raw := range tree.Stmts {
		if raw.Stmt.GetCreateExtensionStmt() != nil {
			return ErrNeedsReload
		}
	}
	s.applyAll(tree, schemaSQL)
	return nil
}

// ErrNeedsReload is Apply's answer to DDL that only Load can take.
var ErrNeedsReload = errors.New("schema: statement needs a full reload")

// leadingComments returns the run of blank and `--` comment lines that opens text.
func leadingComments(text string) string {
	end := 0
	for _, line := range strings.SplitAfter(text, "\n") {
		t := strings.Trim(line, "; \t\r\n") // the previous statement's semicolon opens the span
		if t != "" && !strings.HasPrefix(t, "--") {
			break
		}
		end += len(line)
	}
	return text[:end]
}

var directiveLine = regexp.MustCompile(`(?m)^[ \t]*--[ \t]*sqlshape:[ \t]*(.+?)[ \t]*$`)

// Waivers reads the list of a `waive` directive: comma-separated (outside parentheses)
// entries of `<table> [<obligation>]`, where the obligation is the body as declared
// (`pinned(tenant_id)`, `via view`, a predicate) and its absence waives every obligation
// on the table ("*").
func Waivers(list string) map[string][]string {
	out := map[string][]string{}
	depth, start := 0, 0
	items := []string{}
	for i := 0; i <= len(list); i++ {
		if i == len(list) || (list[i] == ',' && depth == 0) {
			items = append(items, strings.TrimSpace(list[start:i]))
			start = i + 1
			continue
		}
		switch list[i] {
		case '(':
			depth++
		case ')':
			depth--
		}
	}
	for _, it := range items {
		if it == "" {
			continue
		}
		table, spec, _ := strings.Cut(it, " ")
		spec = strings.Join(strings.Fields(spec), " ")
		if spec == "" {
			spec = "*"
		}
		out = AddWaiver(out, table, spec)
	}
	return out
}

// AddWaiver appends specs to the table's waivers.
func AddWaiver(m map[string][]string, table string, specs ...string) map[string][]string {
	if m == nil {
		m = map[string][]string{}
	}
	m[table] = append(m[table], specs...)
	return m
}

// directives extracts the `-- sqlshape: <text>` comment lines of a text.
func directives(text string) []string {
	var out []string
	for _, m := range directiveLine.FindAllStringSubmatch(text, -1) {
		out = append(out, m[1])
	}
	return out
}

// Relation finds a table or view by (schema, name); schema "" means public.
func (s *Schema) Relation(schema, name string) *Relation {
	return s.findRelation(schema, name)
}

// Column finds a column by name.
func (r *Relation) Column(name string) *Column {
	for _, c := range r.Columns {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// FullName is schema.name, with public elided; a temporary relation is its bare name
// (regclass output: pg_temp is always visible, whatever the search path).
func (r *Relation) FullName() string {
	if r.Schema == "public" || r.Temp {
		return r.Name
	}
	return r.Schema + "." + r.Name
}

func (s *Schema) problem(loc int32, format string, args ...any) {
	s.Problems = append(s.Problems, Problem{Location: loc, Message: fmt.Sprintf(format, args...)})
}

func (s *Schema) apply(n *pg_query.Node, loc int32) {
	nFuncs, nTrigs, nTypes := len(s.Functions), len(s.Triggers), len(s.Types.user)
	defer func() {
		// remember the text that created what this statement added
		switch n.Node.(type) {
		case *pg_query.Node_CreateFunctionStmt:
			if len(s.Functions) > nFuncs {
				s.Functions[len(s.Functions)-1].Definition = s.stmtText
			}
		case *pg_query.Node_CreateTrigStmt:
			if len(s.Triggers) > nTrigs {
				s.Triggers[len(s.Triggers)-1].Definition = s.stmtText
			}
		case *pg_query.Node_CreateEnumStmt, *pg_query.Node_CreateDomainStmt, *pg_query.Node_CompositeTypeStmt, *pg_query.Node_CreateRangeStmt:
			if len(s.Types.user) > nTypes {
				s.TypeDefs[s.Types.user[nTypes].OID] = s.stmtText
			}
		}
	}()
	switch st := n.Node.(type) {
	case *pg_query.Node_CreateEnumStmt:
		s.createEnum(st.CreateEnumStmt, loc)
	case *pg_query.Node_CreateDomainStmt:
		s.createDomain(st.CreateDomainStmt, loc)
	case *pg_query.Node_CompositeTypeStmt:
		s.createComposite(st.CompositeTypeStmt, loc)
	case *pg_query.Node_CreateStmt:
		s.createTable(st.CreateStmt, loc)
	case *pg_query.Node_CreateForeignTableStmt:
		s.createTable(st.CreateForeignTableStmt.BaseStmt, loc) // typed like a table; the server is not ours
	case *pg_query.Node_PrepareStmt:
		if s.prepared == nil {
			s.prepared = map[string]*pg_query.Node{}
		}
		s.prepared[st.PrepareStmt.Name] = st.PrepareStmt.Query
	case *pg_query.Node_DeallocateStmt:
		if st.DeallocateStmt.Isall {
			s.prepared = nil
		} else {
			delete(s.prepared, st.DeallocateStmt.Name)
		}
	case *pg_query.Node_TransactionStmt:
		switch st.TransactionStmt.Kind {
		case pg_query.TransactionStmtKind_TRANS_STMT_COMMIT, pg_query.TransactionStmtKind_TRANS_STMT_ROLLBACK,
			pg_query.TransactionStmtKind_TRANS_STMT_PREPARE:
			// ON COMMIT DROP tables go at transaction end (a rolled-back CREATE never was)
			for _, r := range append([]*Relation{}, s.Relations...) {
				if r.OnCommitDrop {
					s.removeRelation(r)
				}
			}
		}
	case *pg_query.Node_ViewStmt:
		s.createView(st.ViewStmt, loc)
	case *pg_query.Node_InsertStmt:
		s.insert(st.InsertStmt, n, loc)
	case *pg_query.Node_CreatePolicyStmt:
		s.createPolicy(st.CreatePolicyStmt, loc)
	case *pg_query.Node_AlterPolicyStmt:
		s.alterPolicy(st.AlterPolicyStmt, loc)
	case *pg_query.Node_SelectStmt:
		if st.SelectStmt.IntoClause != nil {
			s.createTableAs(st.SelectStmt.IntoClause, n, loc)
		} else {
			s.problem(loc, "unsupported statement %T", n.Node)
		}
	case *pg_query.Node_CreateTableAsStmt:
		if st.CreateTableAsStmt.Objtype == pg_query.ObjectType_OBJECT_MATVIEW {
			s.createMatView(st.CreateTableAsStmt, loc)
		} else if st.CreateTableAsStmt.Objtype == pg_query.ObjectType_OBJECT_TABLE {
			query := st.CreateTableAsStmt.Query
			if ex := query.GetExecuteStmt(); ex != nil {
				// CREATE TABLE ... AS EXECUTE name: the prepared statement's query
				q, ok := s.prepared[ex.Name]
				if !ok {
					s.problem(loc, "CREATE TABLE AS EXECUTE: prepared statement %q does not exist", ex.Name)
					return
				}
				query = q
			}
			if st.CreateTableAsStmt.IfNotExists {
				if sch, name := s.rangeVar(st.CreateTableAsStmt.Into.Rel); s.relByName[sch+"."+name] != nil {
					return
				}
			}
			s.createTableAs(st.CreateTableAsStmt.Into, query, loc)
		} else {
			s.problem(loc, "CREATE TABLE AS is not supported in schema.sql")
		}
	case *pg_query.Node_AlterTableStmt:
		s.alterTable(st.AlterTableStmt, loc)
	case *pg_query.Node_IndexStmt:
		s.createIndex(st.IndexStmt, loc)
	case *pg_query.Node_AlterObjectSchemaStmt:
		s.alterObjectSchema(st.AlterObjectSchemaStmt, loc)
	case *pg_query.Node_CreateFunctionStmt:
		s.createFunction(st.CreateFunctionStmt, loc)
	case *pg_query.Node_CommentStmt:
		s.comment(st.CommentStmt, loc)
	case *pg_query.Node_CreateTrigStmt:
		s.createTrigger(st.CreateTrigStmt, loc)
	case *pg_query.Node_RenameStmt:
		s.rename(st.RenameStmt, loc)
	case *pg_query.Node_DropStmt:
		s.drop(st.DropStmt, loc)
	case *pg_query.Node_AlterEnumStmt:
		s.alterEnum(st.AlterEnumStmt, loc)
	case *pg_query.Node_AlterDomainStmt:
		s.alterDomain(st.AlterDomainStmt, loc)
	case *pg_query.Node_CreateRangeStmt:
		s.createRange(st.CreateRangeStmt, loc)
	case *pg_query.Node_DefineStmt:
		s.define(st.DefineStmt, loc)
	case *pg_query.Node_CreateCastStmt:
		s.createCast(st.CreateCastStmt, loc)
	case *pg_query.Node_VariableSetStmt:
		s.setVariable(st.VariableSetStmt)
	case *pg_query.Node_CreateSchemaStmt:
		if s.schemas == nil {
			s.schemas = map[string]bool{}
		}
		s.schemas[st.CreateSchemaStmt.Schemaname] = true
		// CREATE SCHEMA s CREATE TABLE t (...) ...: the elements are created in s
		saved, savedT := s.searchPath, s.Types.searchPath
		s.searchPath = append([]string{st.CreateSchemaStmt.Schemaname}, s.SearchPath()...)
		s.Types.searchPath = s.searchPath
		for _, elt := range st.CreateSchemaStmt.SchemaElts {
			s.apply(elt, loc)
		}
		s.searchPath, s.Types.searchPath = saved, savedT
	case *pg_query.Node_RuleStmt:
		// a DO INSTEAD rule on a view for INSERT / UPDATE / DELETE makes the view take that write
		r := st.RuleStmt
		if rel := s.findRelation(s.rangeVar(r.Relation)); rel != nil {
			if rel.rules == nil {
				rel.rules = map[string]*pg_query.RuleStmt{}
			}
			rel.rules[r.Rulename] = r // CREATE OR REPLACE RULE: the name's rule is this one now
			rel.rebuildRules()
		}
	case *pg_query.Node_CreateSeqStmt:
		s.createSequence(st.CreateSeqStmt.Sequence, loc)
	case *pg_query.Node_AlterSeqStmt:
		s.alterSequence(st.AlterSeqStmt, loc)
	case *pg_query.Node_CreateExtensionStmt,
		*pg_query.Node_GrantStmt, *pg_query.Node_AlterOwnerStmt,
		*pg_query.Node_CreateOpClassStmt, *pg_query.Node_CreateOpFamilyStmt, *pg_query.Node_AlterOpFamilyStmt,
		*pg_query.Node_CreateStatsStmt, *pg_query.Node_AlterExtensionStmt,
		*pg_query.Node_CreateEventTrigStmt, *pg_query.Node_AlterEventTrigStmt, *pg_query.Node_CreatePublicationStmt, *pg_query.Node_AlterPublicationStmt,
		*pg_query.Node_CreateSubscriptionStmt, *pg_query.Node_CreateRoleStmt, *pg_query.Node_AlterRoleStmt, *pg_query.Node_GrantRoleStmt,
		*pg_query.Node_CreateTableSpaceStmt, *pg_query.Node_SecLabelStmt, *pg_query.Node_ClusterStmt, *pg_query.Node_VacuumStmt,
		*pg_query.Node_DoStmt, *pg_query.Node_AlterDefaultPrivilegesStmt, *pg_query.Node_CreateFdwStmt,
		*pg_query.Node_CreateForeignServerStmt, *pg_query.Node_CreateUserMappingStmt, *pg_query.Node_AlterFunctionStmt, *pg_query.Node_AlterCollationStmt:
		// No effect on typing (CREATE EXTENSION was resolved up front in LoadWith).
	default:
		s.problem(loc, "unsupported statement %T", n.Node)
	}
}

// --- names -----------------------------------------------------------------

func strs(nodes []*pg_query.Node) []string {
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, n.GetString_().GetSval())
	}
	return out
}

func qualified(names []string) (schema, name string) {
	switch len(names) {
	case 1:
		return "", names[0]
	case 2:
		return names[0], names[1]
	}
	return "", strings.Join(names, ".")
}

func (s *Schema) rangeVar(rv *pg_query.RangeVar) (schema, name string) {
	schema = rv.GetSchemaname()
	if schema == "" {
		schema = s.creationSchema()
	}
	return schema, rv.GetRelname()
}

// lookupRangeVar is rangeVar for an existing relation: an unqualified name is searched
// the way the search path does (pg_catalog first, then the path), so a user table moved
// into pg_catalog is still found; nothing found falls back to the creation schema.
func (s *Schema) lookupRangeVar(rv *pg_query.RangeVar) (schema, name string) {
	if rv.GetSchemaname() != "" {
		return rv.GetSchemaname(), rv.GetRelname()
	}
	name = rv.GetRelname()
	for _, p := range append([]string{"pg_catalog"}, s.SearchPath()...) {
		if s.relByName[p+"."+name] != nil {
			return p, name
		}
	}
	return s.creationSchema(), name
}

// resolveType turns a TypeName AST into a TypeRef (handles serial pseudo-types and arrays).
func (s *Schema) resolveType(tn *pg_query.TypeName) (TypeRef, error) {
	if tn == nil {
		return TypeRef{}, fmt.Errorf("missing type")
	}
	if tn.PctType {
		// table.column%TYPE: the column's declared type
		names := strs(tn.Names)
		if len(names) < 2 {
			return TypeRef{}, fmt.Errorf("improper %%TYPE reference (too few dotted names): %s", strings.Join(names, "."))
		}
		rschema, rname := qualified(names[:len(names)-1])
		rel := s.Relation(rschema, rname)
		if rel == nil {
			return TypeRef{}, fmt.Errorf("relation %q does not exist", strings.Join(names[:len(names)-1], "."))
		}
		col := rel.Column(names[len(names)-1])
		if col == nil {
			return TypeRef{}, fmt.Errorf("column %q of relation %q does not exist", names[len(names)-1], rname)
		}
		return col.Type, nil
	}
	schema, name := qualified(strs(tn.Names))
	// serial pseudo-types (only valid in column definitions; caller handles NOT NULL/default)
	switch name {
	case "serial", "serial4":
		schema, name = "pg_catalog", "int4"
	case "bigserial", "serial8":
		schema, name = "pg_catalog", "int8"
	case "smallserial", "serial2":
		schema, name = "pg_catalog", "int2"
	}
	t := s.Types.Lookup(schema, name)
	if t == nil {
		return TypeRef{}, fmt.Errorf("type %q does not exist", strings.Join(strs(tn.Names), "."))
	}
	var mods []int64
	for _, m := range tn.Typmods {
		if c := m.GetAConst(); c != nil && c.GetIval() != nil {
			mods = append(mods, int64(c.GetIval().GetIval()))
		} else {
			return TypeRef{}, fmt.Errorf("unsupported type modifier for %s", t.Name)
		}
	}
	typmod, err := typmodFor(t, mods)
	if err != nil {
		return TypeRef{}, err
	}
	oid := t.OID
	if len(tn.ArrayBounds) > 0 {
		// PG arrays are effectively one-dimensional in the type system: int4[][] is still _int4.
		arr := s.Types.ArrayOf(oid)
		if arr == 0 {
			return TypeRef{}, fmt.Errorf("could not find array type for %s", t.Name)
		}
		oid = arr
	}
	return TypeRef{OID: oid, Typmod: typmod}, nil
}

func isSerial(tn *pg_query.TypeName) bool {
	_, name := qualified(strs(tn.GetNames()))
	switch name {
	case "serial", "serial4", "bigserial", "serial8", "smallserial", "serial2":
		return true
	}
	return false
}

// --- types -----------------------------------------------------------------

func (s *Schema) createEnum(st *pg_query.CreateEnumStmt, loc int32) {
	schema, name := qualified(strs(st.TypeName))
	if schema == "" {
		schema = s.creationSchema()
	}
	t := s.Types.addUser(schema, name, 'e', 'E', 0, 0)
	s.Types.Enums[t.OID] = strs(st.Vals)
}

func (s *Schema) createDomain(st *pg_query.CreateDomainStmt, loc int32) {
	schema, name := qualified(strs(st.Domainname))
	if schema == "" {
		schema = s.creationSchema()
	}
	base, err := s.resolveType(st.TypeName)
	if err != nil {
		s.problem(loc, "domain %s: %v", name, err)
		return
	}
	t := s.Types.addUser(schema, name, 'd', 0, base.OID, 0)
	t.Typmod = base.Typmod
	d := &Domain{}
	for _, cn := range st.Constraints {
		c := cn.GetConstraint()
		switch c.GetContype() {
		case pg_query.ConstrType_CONSTR_NOTNULL:
			d.NotNull = true
		case pg_query.ConstrType_CONSTR_CHECK:
			cn := c.Conname
			if cn == "" {
				cn = uniqueName(name+"_check", func(n string) bool {
					for _, x := range d.Checks {
						if x.Name == n {
							return true
						}
					}
					return false
				})
			}
			d.Checks = append(d.Checks, &Constraint{Name: cn, Kind: Check, Expr: c.RawExpr})
		case pg_query.ConstrType_CONSTR_DEFAULT, pg_query.ConstrType_CONSTR_NULL:
		default:
			s.problem(c.GetLocation(), "domain %s: unsupported constraint %v", name, c.GetContype())
		}
	}
	s.Types.Domains[t.OID] = d
}

func (s *Schema) createComposite(st *pg_query.CompositeTypeStmt, loc int32) {
	schema, name := s.rangeVar(st.Typevar)
	rel := &Relation{OID: s.nextOID, Schema: schema, Name: name, Kind: 'c', Definition: s.stmtText}
	s.nextOID++
	for i, cn := range st.Coldeflist {
		cd := cn.GetColumnDef()
		tr, err := s.resolveType(cd.TypeName)
		if err != nil {
			s.problem(cd.GetLocation(), "type %s.%s: %v", name, cd.Colname, err)
			continue
		}
		rel.Columns = append(rel.Columns, &Column{Num: int16(i + 1), Name: cd.Colname, Type: tr})
	}
	t := s.Types.addUser(schema, name, 'c', 'C', 0, rel.OID)
	rel.RowType = t.OID
	s.Relations = append(s.Relations, rel)
	s.relByName[schema+"."+name] = rel
}

// --- relations -------------------------------------------------------------

func (s *Schema) newRelation(schema, name string, kind RelKind) *Relation {
	rel := &Relation{OID: s.nextOID, Schema: schema, Name: name, Kind: kind, Definition: s.stmtText}
	s.nextOID++
	t := s.Types.addUser(schema, name, 'c', 'C', 0, rel.OID)
	rel.RowType = t.OID
	s.Relations = append(s.Relations, rel)
	s.relByName[schema+"."+name] = rel
	return rel
}

func (s *Schema) createTable(st *pg_query.CreateStmt, loc int32) {
	schema, name := s.rangeVar(st.Relation)
	if s.relByName[schema+"."+name] != nil {
		if st.IfNotExists {
			return
		}
		if st.Relation.Relpersistence != "t" {
			s.problem(loc, "relation %q already exists", name)
			return
		}
		// a temporary table hides the permanent one of the same name (pg_temp leads the
		// search path); the hidden relation stays for its other references
	}
	rel := s.newRelation(schema, name, Table)
	rel.Temp = st.Relation.Relpersistence == "t"
	rel.OnCommitDrop = st.Oncommit == pg_query.OnCommitAction_ONCOMMIT_DROP
	// CREATE TABLE ... OF type: the composite type's attributes are the columns
	if st.OfTypename != nil {
		tr, err := s.resolveType(st.OfTypename)
		if err != nil {
			s.problem(loc, "table %s: %v", name, err)
		} else {
			rel.OfType = tr.OID
			for _, r := range s.Relations {
				if r.RowType == tr.OID && r.Kind == 'c' {
					for _, c := range r.Columns {
						cp := *c
						cp.Num = int16(len(rel.Columns) + 1)
						rel.Columns = append(rel.Columns, &cp)
					}
				}
			}
		}
	}
	// a partition takes its parent's columns and constraints; an INHERITS child takes
	// the parent's columns (and CHECKs) and adds its own
	for _, pn := range st.InhRelations {
		pschema, pname := s.rangeVar(pn.GetRangeVar())
		parent := s.relByName[pschema+"."+pname]
		if parent == nil {
			s.problem(loc, "table %s: parent relation %q does not exist", name, pname)
			continue
		}
		s.inherit(rel, parent, st.Partbound != nil, loc)
	}
	for _, d := range s.pending {
		norm := strings.Join(strings.Fields(d), " ")
		rel.Directives = append(rel.Directives, norm)
		switch {
		case len(norm) > 8 && strings.EqualFold(norm[:8], "require "),
			len(norm) > 10 && strings.EqualFold(norm[:10], "aggregate "),
			len(norm) > 8 && strings.EqualFold(norm[:8], "context "):
			// an obligation (internal/obligation parses it)
		case len(norm) > 14 && strings.EqualFold(norm[:14], "visible where "):
			pred, err := parseExpr(norm[14:])
			if err != nil {
				s.problem(loc, "table %s: directive %q: %v", name, d, err)
				continue
			}
			rel.Visible = pred
		default:
			s.problem(loc, "table %s: unknown directive %q", name, d)
		}
	}
	for _, elt := range st.TableElts {
		switch e := elt.Node.(type) {
		case *pg_query.Node_ColumnDef:
			s.addColumn(rel, e.ColumnDef)
		case *pg_query.Node_Constraint:
			s.addTableConstraint(rel, e.Constraint)
		case *pg_query.Node_TableLikeClause:
			s.likeClause(rel, e.TableLikeClause, loc)
		default:
			s.problem(loc, "table %s: unsupported element %T", name, elt.Node)
		}
	}
	if ps := st.Partspec; ps != nil {
		if msg := s.partitionSpecProblem(rel, st, ps); msg != "" {
			s.problem(loc, "table %s: %s", name, msg)
			s.removeRelation(rel)
			return
		}
		for _, pn := range ps.PartParams {
			pe := pn.GetPartitionElem()
			switch {
			case pe == nil:
			case pe.Name != "":
				rel.PartKey = append(rel.PartKey, pe.Name)
			case pe.Expr != nil:
				rel.PartKey = append(rel.PartKey, ColumnRefs(pe.Expr)...)
				rel.PartKeyFuncs = append(rel.PartKeyFuncs, funcNamesIn(pe.Expr)...)
			}
		}
	}
}

// funcNamesIn lists the (unqualified) names of the functions an expression calls.
func funcNamesIn(e Expr) []string {
	var out []string
	WalkNodes(e, func(n *pg_query.Node) {
		if fc := n.GetFuncCall(); fc != nil && len(fc.Funcname) > 0 {
			names := strs(fc.Funcname)
			out = append(out, names[len(names)-1])
		}
	})
	return out
}

// partitionSpecProblem is the part of DefineRelation / transformPartitionSpec that needs
// no expression analysis: PARTITION BY does not combine with INHERITS, LIST takes one
// key column, and a named key column must be a real (non-system) column of the table.
func (s *Schema) partitionSpecProblem(rel *Relation, st *pg_query.CreateStmt, ps *pg_query.PartitionSpec) string {
	if len(st.InhRelations) > 0 && st.Partbound == nil {
		return "cannot create partitioned table as inheritance child"
	}
	if ps.Strategy == pg_query.PartitionStrategy_PARTITION_STRATEGY_LIST && len(ps.PartParams) > 1 {
		return "cannot use \"list\" partition strategy with more than one column"
	}
	for _, pn := range ps.PartParams {
		pe := pn.GetPartitionElem()
		if pe == nil {
			continue
		}
		if pe.Name == "" {
			if pe.Expr != nil && PartitionKeyProblem != nil {
				if msg := PartitionKeyProblem(s, rel, pe.Expr); msg != "" {
					return msg
				}
			}
			continue
		}
		switch pe.Name {
		case "ctid", "xmin", "xmax", "cmin", "cmax", "tableoid":
			return fmt.Sprintf("cannot use system column %q in partition key", pe.Name)
		}
		if rel.Column(pe.Name) == nil {
			return fmt.Sprintf("column %q named in partition key does not exist", pe.Name)
		}
	}
	return ""
}

func (s *Schema) addColumn(rel *Relation, cd *pg_query.ColumnDef) {
	tr, err := s.resolveType(cd.TypeName)
	if err != nil {
		s.problem(cd.GetLocation(), "%s.%s: %v", rel.Name, cd.Colname, err)
		return
	}
	col := &Column{Num: int16(len(rel.Columns) + 1), Name: cd.Colname, Type: tr, NotNull: cd.IsNotNull}
	merged := false
	if existing := rel.Column(cd.Colname); existing != nil {
		// a child redeclaring an inherited column merges with it: NOT NULL accumulates,
		// the child's default / identity / generated expression win
		col = existing
		col.NotNull = col.NotNull || cd.IsNotNull
		col.LocalDef = true
		merged = true
	}
	if isSerial(cd.TypeName) {
		col.NotNull = true
		col.Identity = 's' // serial: sequence default; behaves like identity for "who owns the value"
		s.createOwnedSequence(rel, cd.Colname, cd.GetLocation())
	}
	if cd.CollClause != nil {
		col.Collation = strings.Join(strs(cd.CollClause.Collname), ".")
		if parts := strs(cd.CollClause.Collname); !s.KnownCollation(parts[len(parts)-1]) {
			s.problem(cd.GetLocation(), "%s.%s: collation %q does not exist", rel.Name, cd.Colname, parts[len(parts)-1])
		}
	}
	if !merged {
		rel.Columns = append(rel.Columns, col)
	}
	for _, cn := range cd.Constraints {
		c := cn.GetConstraint()
		switch c.GetContype() {
		case pg_query.ConstrType_CONSTR_NOTNULL:
			col.NotNull = true
		case pg_query.ConstrType_CONSTR_NULL:
			col.NotNull = false
		case pg_query.ConstrType_CONSTR_DEFAULT:
			col.Default = c.RawExpr
		case pg_query.ConstrType_CONSTR_IDENTITY:
			col.NotNull = true
			col.Identity = c.GeneratedWhen[0]
			s.createOwnedSequence(rel, cd.Colname, cd.GetLocation())
		case pg_query.ConstrType_CONSTR_GENERATED:
			col.Generated = c.RawExpr
		case pg_query.ConstrType_CONSTR_PRIMARY:
			col.NotNull = true
			s.addConstraint(rel, &Constraint{Name: c.Conname, Kind: PrimaryKey, Columns: []string{col.Name}, Deferrable: c.Deferrable})
		case pg_query.ConstrType_CONSTR_UNIQUE:
			s.addConstraint(rel, &Constraint{Name: c.Conname, Kind: Unique, Columns: []string{col.Name}, NullsNotDistinct: c.NullsNotDistinct, Deferrable: c.Deferrable})
		case pg_query.ConstrType_CONSTR_CHECK:
			s.addConstraint(rel, &Constraint{Name: c.Conname, Kind: Check, Columns: []string{col.Name}, Expr: c.RawExpr})
		case pg_query.ConstrType_CONSTR_FOREIGN:
			fk := s.foreignKey(c)
			fk.Columns = []string{col.Name}
			s.addConstraint(rel, fk)
		case pg_query.ConstrType_CONSTR_ATTR_DEFERRABLE, pg_query.ConstrType_CONSTR_ATTR_NOT_DEFERRABLE,
			pg_query.ConstrType_CONSTR_ATTR_DEFERRED, pg_query.ConstrType_CONSTR_ATTR_IMMEDIATE:
		default:
			s.problem(c.GetLocation(), "%s.%s: unsupported constraint %v", rel.Name, col.Name, c.GetContype())
		}
	}
}

func (s *Schema) foreignKey(c *pg_query.Constraint) *Constraint {
	rs, rn := s.rangeVar(c.Pktable)
	fk := &Constraint{Name: c.Conname, Kind: ForeignKey, RefColumns: strs(c.PkAttrs), Deferrable: c.Deferrable, OnDelete: 'a', OnUpdate: 'a'}
	if c.FkDelAction != "" {
		fk.OnDelete = c.FkDelAction[0]
	}
	if c.FkUpdAction != "" {
		fk.OnUpdate = c.FkUpdAction[0]
	}
	if rs == "public" {
		fk.RefTable = rn
	} else {
		fk.RefTable = rs + "." + rn
	}
	return fk
}

// notifyNotNullChange runs NotNullHook for rel, when one is installed.
func (s *Schema) notifyNotNullChange(rel *Relation) {
	if s.NotNullHook != nil {
		s.NotNullHook(s, rel)
	}
}

func (s *Schema) addTableConstraint(rel *Relation, c *pg_query.Constraint) {
	switch c.GetContype() {
	case pg_query.ConstrType_CONSTR_PRIMARY:
		cols := strs(c.Keys)
		for _, n := range cols {
			if col := rel.Column(n); col != nil {
				col.NotNull = true
			} else {
				s.problem(c.GetLocation(), "%s: primary key column %q does not exist", rel.Name, n)
			}
		}
		s.addConstraint(rel, &Constraint{Name: c.Conname, Kind: PrimaryKey, Columns: cols, Deferrable: c.Deferrable})
		// a PRIMARY KEY added after the table exists (ALTER TABLE ... ADD CONSTRAINT ...
		// PRIMARY KEY) makes its columns NOT NULL too; a no-op when this runs for the
		// table's own inline/table-level constraints at CREATE TABLE time, since no view
		// can depend on it yet.
		s.notifyNotNullChange(rel)
	case pg_query.ConstrType_CONSTR_UNIQUE:
		s.addConstraint(rel, &Constraint{Name: c.Conname, Kind: Unique, Columns: strs(c.Keys), NullsNotDistinct: c.NullsNotDistinct, Deferrable: c.Deferrable})
	case pg_query.ConstrType_CONSTR_CHECK:
		s.addConstraint(rel, &Constraint{Name: c.Conname, Kind: Check, Expr: c.RawExpr})
	case pg_query.ConstrType_CONSTR_FOREIGN:
		fk := s.foreignKey(c)
		fk.Columns = strs(c.FkAttrs)
		s.addConstraint(rel, fk)
	case pg_query.ConstrType_CONSTR_EXCLUSION:
		ex := &Constraint{Name: c.Conname, Kind: Exclude, AccessMethod: c.AccessMethod, Predicate: c.WhereClause, Deferrable: c.Deferrable}
		for _, item := range c.Exclusions {
			parts := item.GetList().GetItems()
			if len(parts) != 2 {
				continue
			}
			ex.Columns = append(ex.Columns, parts[0].GetIndexElem().GetName())
			opNames := strs(parts[1].GetList().GetItems())
			op := ""
			if len(opNames) > 0 {
				op = opNames[len(opNames)-1]
			}
			ex.Operators = append(ex.Operators, op)
		}
		s.addConstraint(rel, ex)
	default:
		s.problem(c.GetLocation(), "%s: unsupported table constraint %v", rel.Name, c.GetContype())
	}
}

func (s *Schema) createView(st *pg_query.ViewStmt, loc int32) {
	schema, name := s.rangeVar(st.View)
	var rel *Relation
	if existing := s.relByName[schema+"."+name]; existing != nil {
		switch {
		case st.Replace && existing.Kind == View:
			// CREATE OR REPLACE VIEW keeps the relation (its rules, triggers and the views
			// built on it) and redefines the query
			rel = existing
			rel.Frozen, rel.Unfiltered, rel.Waived, rel.Directives = nil, nil, nil, nil
			// the redefinition is the definition (pg_dump writes a view a later object needs
			// as a dummy first and CREATE OR REPLACEs it once the object exists)
			rel.Definition = s.stmtText
		case st.View.Relpersistence == "t":
			// a temporary view hides the permanent relation of the same name (as createTable)
		default:
			s.problem(loc, "relation %q already exists", name)
			return
		}
	}
	if rel == nil {
		rel = s.newRelation(schema, name, View)
		rel.Temp = st.View.Relpersistence == "t"
	}
	oldQuery := rel.Query
	rel.Query = st.Query
	if oldQuery != nil && s.viewDependsOnItself(rel) {
		// CREATE OR REPLACE VIEW a AS ... FROM b, where b reads a: PG refuses (42P17)
		rel.Query = oldQuery
		s.problem(loc, "infinite recursion detected in rules for relation %q", name)
		return
	}
	rel.ColumnAliases = strs(st.Aliases)
	switch st.WithCheckOption {
	case pg_query.ViewCheckOption_LOCAL_CHECK_OPTION:
		rel.CheckOption = 'l'
	case pg_query.ViewCheckOption_CASCADED_CHECK_OPTION:
		rel.CheckOption = 'c'
	default:
		rel.CheckOption = 0
	}
	s.viewDirectives(rel, loc)
	if s.ViewHook != nil {
		s.ViewHook(s, rel)
	}
}

// viewDependsOnItself reports whether rel's query reaches rel again through the views it
// reads (a -> b -> a). PG rejects such a redefinition; every walk over view dependencies
// here assumes it never happens.
func (s *Schema) viewDependsOnItself(rel *Relation) bool {
	for _, d := range s.DependentViews(rel) {
		if d == rel {
			return true
		}
	}
	return false
}

// viewDirectives applies the directives written before a CREATE (MATERIALIZED) VIEW.
func (s *Schema) viewDirectives(rel *Relation, loc int32) {
	for _, d := range s.pending {
		norm := strings.Join(strings.Fields(d), " ")
		rel.Directives = append(rel.Directives, norm)
		switch {
		case len(norm) > 8 && strings.EqualFold(norm[:8], "require "),
			len(norm) > 8 && strings.EqualFold(norm[:8], "context "):
			// an obligation (internal/obligation parses it)
		case len(norm) > 11 && strings.EqualFold(norm[:11], "unfiltered "):
			if rel.Unfiltered == nil {
				rel.Unfiltered = map[string]bool{}
			}
			for _, t := range strings.Split(norm[11:], ",") {
				if t = strings.TrimSpace(t); t != "" {
					rel.Unfiltered[t] = true
					rel.Waived = AddWaiver(rel.Waived, t, "unfiltered")
				}
			}
		case len(norm) > 6 && strings.EqualFold(norm[:6], "waive "):
			for table, spec := range Waivers(norm[6:]) {
				rel.Waived = AddWaiver(rel.Waived, table, spec...)
			}
		default:
			s.problem(loc, "view %s: unknown directive %q", rel.Name, d)
		}
	}
}

func (s *Schema) createMatView(st *pg_query.CreateTableAsStmt, loc int32) {
	schema, name := s.rangeVar(st.Into.Rel)
	rel := s.newRelation(schema, name, MatView)
	rel.Query = st.Query
	rel.ColumnAliases = strs(st.Into.ColNames)
	s.viewDirectives(rel, loc)
	if s.ViewHook != nil {
		s.ViewHook(s, rel)
	}
}

func (s *Schema) alterTable(st *pg_query.AlterTableStmt, loc int32) {
	schema, name := s.lookupRangeVar(st.Relation)
	rel := s.relByName[schema+"."+name]
	if rel == nil {
		if !st.MissingOk {
			s.problem(loc, "ALTER TABLE: relation %q does not exist", name)
		}
		return
	}
	shaping := false
	for _, cn := range st.Cmds {
		if cmd := cn.GetAlterTableCmd(); cmd.GetSubtype() != pg_query.AlterTableType_AT_AddConstraint {
			shaping = true
		}
	}
	if shaping {
		rel.Alters = append(rel.Alters, s.stmtText)
	}
	for _, cn := range st.Cmds {
		cmd := cn.GetAlterTableCmd()
		switch cmd.GetSubtype() {
		case pg_query.AlterTableType_AT_AddColumn:
			s.addColumn(rel, cmd.Def.GetColumnDef())
			// children (INHERITS / partitions) gain the column too
			for _, child := range s.Relations {
				if child != rel && child.InheritsFrom(rel) && child.Column(cmd.Def.GetColumnDef().GetColname()) == nil {
					s.addColumn(child, cmd.Def.GetColumnDef())
				}
			}
		case pg_query.AlterTableType_AT_AddConstraint:
			n := len(rel.Constraints)
			s.addTableConstraint(rel, cmd.Def.GetConstraint())
			if len(rel.Constraints) > n && len(st.Cmds) == 1 {
				rel.Constraints[len(rel.Constraints)-1].Definition = s.stmtText
			}
		case pg_query.AlterTableType_AT_SetNotNull:
			if col := rel.Column(cmd.Name); col != nil {
				col.NotNull = true
				s.notifyNotNullChange(rel)
			}
		case pg_query.AlterTableType_AT_DropNotNull:
			if col := rel.Column(cmd.Name); col != nil {
				col.NotNull = false
				s.notifyNotNullChange(rel)
			}
		case pg_query.AlterTableType_AT_ColumnDefault:
			if col := rel.Column(cmd.Name); col != nil {
				col.Default = cmd.Def
			} else if rel.Kind == View {
				if cmd.Def == nil {
					delete(rel.ViewDefaults, cmd.Name)
				} else {
					if rel.ViewDefaults == nil {
						rel.ViewDefaults = map[string]Expr{}
					}
					rel.ViewDefaults[cmd.Name] = cmd.Def
				}
			}
		case pg_query.AlterTableType_AT_AlterColumnType:
			if col := rel.Column(cmd.Name); col != nil {
				if tr, err := s.resolveType(cmd.Def.GetColumnDef().GetTypeName()); err == nil {
					col.Type = tr
					for _, child := range s.Relations {
						// inherited columns keep the parent's type
						if child != rel && child.InheritsFrom(rel) {
							if cc := child.Column(cmd.Name); cc != nil && cc.Inherited {
								cc.Type = tr
							}
						}
					}
				} else {
					s.problem(loc, "%s.%s: %v", rel.Name, cmd.Name, err)
				}
			}
		case pg_query.AlterTableType_AT_DropColumn:
			if col := rel.Column(cmd.Name); col != nil && col.Inherited {
				s.problem(loc, "%s: cannot drop inherited column %q", rel.Name, cmd.Name)
				continue
			}
			if slices.Contains(rel.PartKey, cmd.Name) {
				s.problem(loc, "%s: cannot drop column %q because it is part of the partition key", rel.Name, cmd.Name)
				continue
			}
			s.dropColumn(rel, cmd.Name, loc, cmd.MissingOk)
			for _, child := range s.Relations {
				if child != rel && child.InheritsFrom(rel) {
					if cc := child.Column(cmd.Name); cc != nil && cc.Inherited {
						if cc.LocalDef {
							cc.Inherited = false // now the child's own column
						} else {
							s.dropColumn(child, cmd.Name, loc, true)
						}
					}
				}
			}
		case pg_query.AlterTableType_AT_DropConstraint:
			n := len(rel.Constraints)
			rel.Constraints = filterConstraints(rel.Constraints, func(c *Constraint) bool { return c.Name != cmd.Name })
			if len(rel.Constraints) == n && !cmd.MissingOk {
				s.problem(loc, "%s: constraint %q does not exist", rel.Name, cmd.Name)
			}
		case pg_query.AlterTableType_AT_AddIdentity:
			if col := rel.Column(cmd.Name); col != nil && col.Identity == 0 {
				col.NotNull = true
				col.Identity = 'd'
				if c := cmd.Def.GetConstraint(); c != nil && len(c.GeneratedWhen) > 0 {
					col.Identity = c.GeneratedWhen[0]
				}
				s.createOwnedSequence(rel, col.Name, loc)
			}
		case pg_query.AlterTableType_AT_EnableRule, pg_query.AlterTableType_AT_EnableAlwaysRule:
			rel.setRuleEnabled(cmd.Name, true)
		case pg_query.AlterTableType_AT_DisableRule, pg_query.AlterTableType_AT_EnableReplicaRule:
			rel.setRuleEnabled(cmd.Name, false)
		case pg_query.AlterTableType_AT_AddInherit, pg_query.AlterTableType_AT_DropInherit:
			// the child already has the parent's columns; only the link changes (a child
			// goes with DROP TABLE parent CASCADE, ALTER TABLE parent reaches it)
			pschema, pname := s.rangeVar(cmd.Def.GetRangeVar())
			parent := s.relByName[pschema+"."+pname]
			if parent == nil {
				s.problem(loc, "%s: relation %q does not exist", rel.Name, pname)
				continue
			}
			if cmd.Subtype == pg_query.AlterTableType_AT_AddInherit {
				rel.Parents = append(rel.Parents, parent)
				for _, pc := range parent.Columns {
					if c := rel.Column(pc.Name); c != nil {
						c.Inherited = true
					}
				}
			} else {
				var kept []*Relation
				for _, p := range rel.Parents {
					if p != parent {
						kept = append(kept, p)
					}
				}
				rel.Parents = kept
				for _, c := range rel.Columns {
					c.Inherited = false
					for _, p := range rel.Parents {
						if p.Column(c.Name) != nil {
							c.Inherited = true
						}
					}
				}
			}
		case pg_query.AlterTableType_AT_AttachPartition, pg_query.AlterTableType_AT_DetachPartition:
			pc := cmd.Def.GetPartitionCmd()
			pschema, pname := s.rangeVar(pc.GetName())
			part := s.relByName[pschema+"."+pname]
			if part == nil {
				s.problem(loc, "%s: partition %q does not exist", rel.Name, pname)
				continue
			}
			if cmd.Subtype == pg_query.AlterTableType_AT_AttachPartition {
				part.Parents = append(part.Parents, rel)
				part.IsPartition = true
				// a partition's identity columns are the parent's
				for _, pc := range rel.Columns {
					if c := part.Column(pc.Name); c != nil && pc.Identity != 0 && pc.Identity != 's' {
						c.Identity = pc.Identity
					}
				}
			} else {
				var kept []*Relation
				for _, p := range part.Parents {
					if p != rel {
						kept = append(kept, p)
					}
				}
				part.Parents = kept
				part.IsPartition = false // a detached partition stands on its own
				for _, c := range part.Columns {
					if c.Identity != 0 && c.Identity != 's' {
						c.Identity = 0 // detaching removes the identity property
					}
				}
			}
		case pg_query.AlterTableType_AT_DropIdentity:
			if col := rel.Column(cmd.Name); col != nil {
				col.Identity = 0
				s.dropOwnedSequences(rel, col.Name)
			}
		case pg_query.AlterTableType_AT_SetIdentity:
			// ALTER COLUMN ... SET GENERATED { ALWAYS | BY DEFAULT } [SET ... sequence options]
			if col := rel.Column(cmd.Name); col != nil && col.Identity != 0 {
				for _, dn := range cmd.Def.GetList().GetItems() {
					if de := dn.GetDefElem(); de != nil && de.Defname == "generated" {
						if iv := de.Arg.GetInteger(); iv != nil {
							col.Identity = byte(iv.Ival)
						}
					}
				}
				for _, child := range s.Relations {
					if child.IsPartition && child.InheritsFrom(rel) {
						if cc := child.Column(col.Name); cc != nil && cc.Identity != 0 {
							cc.Identity = col.Identity
						}
					}
				}
			}
		case pg_query.AlterTableType_AT_DropExpression:
			if col := rel.Column(cmd.Name); col != nil {
				col.Generated = nil
			}
		case pg_query.AlterTableType_AT_EnableRowSecurity:
			rel.RowSecurity = true
		case pg_query.AlterTableType_AT_DisableRowSecurity:
			rel.RowSecurity = false
		case pg_query.AlterTableType_AT_ForceRowSecurity:
			rel.ForceRowSecurity = true
		case pg_query.AlterTableType_AT_NoForceRowSecurity:
			rel.ForceRowSecurity = false
		case pg_query.AlterTableType_AT_ChangeOwner, pg_query.AlterTableType_AT_SetRelOptions,
			pg_query.AlterTableType_AT_ClusterOn, pg_query.AlterTableType_AT_SetStatistics,
			pg_query.AlterTableType_AT_EnableTrig, pg_query.AlterTableType_AT_DisableTrig, pg_query.AlterTableType_AT_EnableAlwaysTrig,
			pg_query.AlterTableType_AT_EnableReplicaTrig, pg_query.AlterTableType_AT_EnableTrigAll, pg_query.AlterTableType_AT_DisableTrigAll,
			pg_query.AlterTableType_AT_EnableTrigUser, pg_query.AlterTableType_AT_DisableTrigUser,
			pg_query.AlterTableType_AT_DetachPartitionFinalize,
			pg_query.AlterTableType_AT_ValidateConstraint, pg_query.AlterTableType_AT_ReplicaIdentity, pg_query.AlterTableType_AT_SetLogged,
			pg_query.AlterTableType_AT_SetUnLogged, pg_query.AlterTableType_AT_SetTableSpace, pg_query.AlterTableType_AT_SetStorage,
			pg_query.AlterTableType_AT_SetCompression, pg_query.AlterTableType_AT_AlterConstraint, pg_query.AlterTableType_AT_ResetRelOptions,
			pg_query.AlterTableType_AT_SetAccessMethod,
			pg_query.AlterTableType_AT_DropOids,
			pg_query.AlterTableType_AT_SetOptions, pg_query.AlterTableType_AT_ResetOptions, pg_query.AlterTableType_AT_GenericOptions,
			pg_query.AlterTableType_AT_AlterColumnGenericOptions, pg_query.AlterTableType_AT_SetExpression:
		default:
			s.problem(loc, "ALTER TABLE %s: unsupported action %v", name, cmd.GetSubtype())
		}
	}
}

func (s *Schema) createIndex(st *pg_query.IndexStmt, loc int32) {
	schema, name := s.lookupRangeVar(st.Relation)
	rel := s.relByName[schema+"."+name]
	if rel == nil {
		s.problem(loc, "CREATE INDEX: relation %q does not exist", name)
		return
	}
	idx := &Index{Name: st.Idxname, Unique: st.Unique, Predicate: st.WhereClause, Definition: s.stmtText}
	for _, pn := range st.IndexParams {
		ie := pn.GetIndexElem()
		if ie.GetName() == "" {
			idx.nameParts = append(idx.nameParts, "expr")
			continue
		}
		idx.nameParts = append(idx.nameParts, ie.GetName())
		if len(idx.Columns) == len(idx.nameParts)-1 {
			idx.Columns = append(idx.Columns, ie.GetName()) // columns up to the first expression lead the index
		}
	}
	if idx.Name == "" {
		idx.Name = s.chooseIndexName(rel, idx.nameParts)
	}
	rel.Indexes = append(rel.Indexes, idx)
	if !st.Unique || len(idx.Columns) != len(st.IndexParams) {
		return // only whole-column unique indexes give uniqueness proofs
	}
	c := &Constraint{Name: idx.Name, Kind: Unique, Predicate: st.WhereClause, NullsNotDistinct: st.NullsNotDistinct, Columns: idx.Columns}
	s.addConstraint(rel, c)
}

// --- functions -------------------------------------------------------------

func (s *Schema) createFunction(st *pg_query.CreateFunctionStmt, loc int32) {
	schema, name := qualified(strs(st.Funcname))
	if schema == "" {
		schema = s.creationSchema()
	}
	fn := &Function{OID: s.nextOID, Schema: schema, Name: name, IsProc: st.IsProcedure, Volatile: 'v'}
	s.nextOID++
	for _, d := range s.pending {
		norm := strings.Join(strings.Fields(d), " ")
		switch {
		case strings.EqualFold(norm, "not null"):
			fn.NotNull = true
		case len(norm) > 6 && strings.EqualFold(norm[:6], "error "):
			// error XX001 = Name
			rest := strings.TrimSpace(norm[6:])
			code, errName, _ := strings.Cut(rest, "=")
			code = strings.TrimSpace(code)
			if len(code) != 5 {
				s.problem(loc, "function %s: directive %q: SQLSTATE must be 5 characters", name, d)
				continue
			}
			fn.Raises = append(fn.Raises, RaisedError{Code: code, Name: strings.TrimSpace(errName)})
		default:
			s.problem(loc, "function %s: unknown directive %q", name, d)
		}
	}
	var tableCols []FuncArg
	for _, pn := range st.Parameters {
		p := pn.GetFunctionParameter()
		tr, err := s.resolveType(p.ArgType)
		if err != nil {
			s.problem(loc, "function %s: parameter %q: %v", name, p.Name, err)
			return
		}
		a := FuncArg{Name: p.Name, Type: tr, HasDefault: p.Defexpr != nil}
		switch p.Mode {
		case pg_query.FunctionParameterMode_FUNC_PARAM_IN, pg_query.FunctionParameterMode_FUNC_PARAM_DEFAULT:
			a.Mode = 'i'
		case pg_query.FunctionParameterMode_FUNC_PARAM_OUT:
			a.Mode = 'o'
		case pg_query.FunctionParameterMode_FUNC_PARAM_INOUT:
			a.Mode = 'b'
		case pg_query.FunctionParameterMode_FUNC_PARAM_VARIADIC:
			a.Mode = 'v'
		case pg_query.FunctionParameterMode_FUNC_PARAM_TABLE:
			a.Mode = 't'
			tableCols = append(tableCols, a)
		}
		fn.Args = append(fn.Args, a)
	}
	if st.ReturnType != nil {
		tr, err := s.resolveType(st.ReturnType)
		if err != nil {
			s.problem(loc, "function %s: return type: %v", name, err)
			return
		}
		fn.RetType = tr
		fn.RetSet = st.ReturnType.Setof
	} else if !st.IsProcedure {
		// no RETURNS: OUT / INOUT parameters shape the result (one is the type itself,
		// several a record), otherwise void
		var outs []FuncArg
		for _, arg := range fn.Args {
			if arg.Mode == 'o' || arg.Mode == 'b' {
				outs = append(outs, arg)
			}
		}
		switch len(outs) {
		case 0:
			fn.RetType = TypeRef{OID: catalog.Void, Typmod: -1}
		case 1:
			fn.RetType = outs[0].Type
		default:
			fn.RetType = TypeRef{OID: catalog.Record, Typmod: -1}
		}
	}
	if len(tableCols) > 0 {
		fn.RetSet = true
		if len(tableCols) == 1 {
			fn.RetType = tableCols[0].Type
		} else {
			fn.RetType = TypeRef{OID: catalog.Record, Typmod: -1}
		}
	}
	fn.SQLBody = st.SqlBody
	for _, on := range st.Options {
		d := on.GetDefElem()
		switch d.GetDefname() {
		case "language":
			fn.Language = d.GetArg().GetString_().GetSval()
		case "as":
			if items := d.GetArg().GetList().GetItems(); len(items) == 1 {
				fn.Body = items[0].GetString_().GetSval()
			}
		case "volatility":
			fn.Volatile = d.GetArg().GetString_().GetSval()[0]
		case "strict":
			fn.Strict = d.GetArg().GetBoolean().GetBoolval()
		case "security":
			fn.SecurityDefiner = d.GetArg().GetBoolean().GetBoolval()
		case "window":
			fn.IsWindow = d.GetArg().GetBoolean().GetBoolval()
		}
	}
	if st.Replace {
		// CREATE OR REPLACE: the function with the same input signature is redefined
		var kept []*Function
		for _, f := range s.Functions {
			if !(f.Schema == fn.Schema && f.Name == fn.Name && sameInputs(f, fn)) {
				kept = append(kept, f)
			}
		}
		s.Functions = kept
	}
	s.Functions = append(s.Functions, fn)
}

// --- comments --------------------------------------------------------------

func (s *Schema) comment(st *pg_query.CommentStmt, loc int32) {
	var key string
	switch st.Objtype {
	case pg_query.ObjectType_OBJECT_TABLE, pg_query.ObjectType_OBJECT_VIEW, pg_query.ObjectType_OBJECT_MATVIEW:
		key = strings.Join(strs(st.Object.GetList().GetItems()), ".")
	case pg_query.ObjectType_OBJECT_COLUMN:
		key = strings.Join(strs(st.Object.GetList().GetItems()), ".")
	case pg_query.ObjectType_OBJECT_TYPE, pg_query.ObjectType_OBJECT_DOMAIN:
		name := strings.TrimPrefix(strings.Join(strs(st.Object.GetTypeName().GetNames()), "."), "public.")
		key = "type:" + name
	default:
		return
	}
	key = strings.TrimPrefix(key, "public.")
	if st.Comment == "" {
		// COMMENT ON ... IS NULL removes the comment
		delete(s.Comments, key)
		return
	}
	s.Comments[key] = st.Comment
}

// ResolveType resolves a TypeName AST node against this schema (exported for the analyzer).
func (s *Schema) ResolveType(tn *pg_query.TypeName) (TypeRef, error) { return s.resolveType(tn) }

// addConstraint records a table constraint, naming it the way PG does when the schema
// does not: <table>_pkey, <table>_<cols>_key, <table>_<cols>_fkey, <table>_<cols>_check
// (a CHECK's columns are the ones its expression references, in order), with a numeric
// suffix on collision. Errors at runtime carry these names, so they must agree.
func (s *Schema) addConstraint(rel *Relation, c *Constraint) {
	if c.Name == "" {
		// PG names these with makeObjectName(table, cols, label): the table name and the
		// joined column names, the longer one truncated (at a character boundary) until
		// the whole fits NAMEDATALEN-1 bytes. A CHECK's columns are the ones its expression
		// references (in order) only when there is exactly one; a multi-column CHECK's
		// automatic name carries no column part at all (PG only passes name2 to
		// ChooseConstraintName for a single-column check).
		var base string
		switch c.Kind {
		case PrimaryKey:
			base = makeObjectName(rel.Name, "", "pkey")
		case Unique:
			base = makeObjectName(rel.Name, strings.Join(c.Columns, "_"), "key")
		case ForeignKey:
			base = makeObjectName(rel.Name, strings.Join(c.Columns, "_"), "fkey")
		case Exclude:
			base = makeObjectName(rel.Name, strings.Join(c.Columns, "_"), "excl")
		case Check:
			cols := c.Columns
			if len(cols) == 0 {
				cols = ColumnRefs(c.Expr)
			}
			if len(cols) == 1 {
				base = makeObjectName(rel.Name, cols[0], "check")
			} else {
				base = makeObjectName(rel.Name, "", "check")
			}
		}
		c.Name = uniqueName(base, func(n string) bool {
			for _, r := range s.Relations {
				for _, x := range r.Constraints {
					if x.Name == n {
						return true
					}
				}
			}
			return false
		})
	}
	rel.Constraints = append(rel.Constraints, c)
	if c.Kind == Check {
		if col, vals := checkInValues(c.Expr); col != "" {
			if column := rel.Column(col); column != nil {
				column.Values = vals
			}
		}
	}
}

// checkInValues recognizes `col IN ('a', 'b', ...)` and `col = ANY (ARRAY['a', ...])`.
func checkInValues(e Expr) (string, []string) {
	x := e.GetAExpr()
	if x == nil {
		return "", nil
	}
	cr := x.Lexpr.GetColumnRef()
	if cr == nil || len(cr.Fields) != 1 {
		return "", nil
	}
	col := cr.Fields[0].GetString_().GetSval()
	var items []*pg_query.Node
	switch x.Kind {
	case pg_query.A_Expr_Kind_AEXPR_IN:
		items = x.Rexpr.GetList().GetItems()
	case pg_query.A_Expr_Kind_AEXPR_OP_ANY:
		if strs(x.Name)[0] != "=" {
			return "", nil
		}
		arr := x.Rexpr.GetAArrayExpr()
		if arr == nil {
			// ARRAY[...]::text[]
			if tc := x.Rexpr.GetTypeCast(); tc != nil {
				arr = tc.Arg.GetAArrayExpr()
			}
		}
		if arr == nil {
			return "", nil
		}
		items = arr.Elements
	default:
		return "", nil
	}
	var vals []string
	for _, it := range items {
		c := it.GetAConst()
		if c == nil {
			return "", nil
		}
		sv, ok := c.Val.(*pg_query.A_Const_Sval)
		if !ok {
			return "", nil
		}
		vals = append(vals, sv.Sval.GetSval())
	}
	if len(vals) == 0 {
		return "", nil
	}
	return col, vals
}

func uniqueName(base string, taken func(string) bool) string {
	name := base
	for i := 1; taken(name); i++ {
		name = base + strconv.Itoa(i)
	}
	return name
}

// ColumnRefs lists the distinct unqualified column names an expression references, in order.
func ColumnRefs(e Expr) []string {
	var out []string
	seen := map[string]bool{}
	WalkNodes(e, func(n *pg_query.Node) {
		cr := n.GetColumnRef()
		if cr == nil || len(cr.Fields) == 0 {
			return
		}
		name := cr.Fields[len(cr.Fields)-1].GetString_().GetSval()
		if name != "" && !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	})
	return out
}

// WalkNodes visits every Node in a protobuf tree.
func WalkNodes(m proto.Message, f func(*pg_query.Node)) {
	if m == nil {
		return
	}
	if n, ok := m.(*pg_query.Node); ok {
		if n == nil {
			return
		}
		f(n)
	}
	m.ProtoReflect().Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		if fd.Kind() != protoreflect.MessageKind {
			return true
		}
		if fd.IsList() {
			l := v.List()
			for i := 0; i < l.Len(); i++ {
				WalkNodes(l.Get(i).Message().Interface(), f)
			}
			return true
		}
		WalkNodes(v.Message().Interface(), f)
		return true
	})
}

// createTrigger records which events on which table run which function (trigger.h bits).
func (s *Schema) createTrigger(st *pg_query.CreateTrigStmt, loc int32) {
	schema, name := s.lookupRangeVar(st.Relation)
	rel := s.relByName[schema+"."+name]
	if rel == nil {
		s.problem(loc, "CREATE TRIGGER %s: relation %q does not exist", st.Trigname, name)
		return
	}
	fs, fn := qualified(strs(st.Funcname))
	if fs == "" || fs == "public" {
		fs = ""
	} else {
		fs += "."
	}
	s.Triggers = append(s.Triggers, &Trigger{
		Name: st.Trigname, Table: rel.FullName(),
		Insert: st.Events&(1<<2) != 0, Delete: st.Events&(1<<3) != 0, Update: st.Events&(1<<4) != 0,
		UpdateOf: strs(st.Columns),
		Function: fs + fn,
	})
}

// Function finds a user function by (schema, name); schema "" means public.
func (s *Schema) Function(schema, name string) *Function {
	if schema == "" {
		schema = "public"
	}
	for _, f := range s.Functions {
		if f.Schema == schema && f.Name == name {
			return f
		}
	}
	return nil
}

// parseExpr parses a standalone SQL expression.
func parseExpr(text string) (Expr, error) {
	tree, err := pg_query.Parse("SELECT " + text)
	if err != nil {
		return nil, err
	}
	if len(tree.Stmts) != 1 {
		return nil, fmt.Errorf("one expression expected")
	}
	targets := tree.Stmts[0].Stmt.GetSelectStmt().GetTargetList()
	if len(targets) != 1 {
		return nil, fmt.Errorf("one expression expected")
	}
	return targets[0].GetResTarget().GetVal(), nil
}

// sameInputs reports whether two functions take the same input parameter types.
func sameInputs(a, b *Function) bool {
	inputs := func(f *Function) []catalog.OID {
		var out []catalog.OID
		for _, arg := range f.Args {
			if arg.Mode == 'i' || arg.Mode == 'b' || arg.Mode == 'v' {
				out = append(out, arg.Type.OID)
			}
		}
		return out
	}
	x, y := inputs(a), inputs(b)
	if len(x) != len(y) {
		return false
	}
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}

// PartitionKeyProblem checks one PARTITION BY expression against the table (the analyzer
// installs it): the message PG would give, or "" when the expression is acceptable.
var PartitionKeyProblem func(s *Schema, rel *Relation, expr *pg_query.Node) string

// QueryColumns types a query's result columns for CREATE TABLE AS / SELECT INTO. The
// analyzer installs it (package analyze imports schema, not the reverse); nil leaves
// such tables as problems.
var QueryColumns func(s *Schema, query *pg_query.Node) ([]*Column, error)

// createTableAs creates the table a CREATE TABLE AS / SELECT INTO fills, with the
// query's columns (renamed by the INTO column list when given).
func (s *Schema) createTableAs(into *pg_query.IntoClause, query *pg_query.Node, loc int32) {
	schema, name := s.rangeVar(into.Rel)
	if s.relByName[schema+"."+name] != nil {
		if into.Rel.Relpersistence != "t" {
			s.problem(loc, "relation %q already exists", name)
			return
		}
	}
	if QueryColumns == nil {
		s.problem(loc, "CREATE TABLE AS %s: the query's columns need the analyzer", name)
		return
	}
	cols, err := QueryColumns(s, query)
	if err != nil {
		s.problem(loc, "CREATE TABLE AS %s: %v", name, err)
		return
	}
	rel := s.newRelation(schema, name, Table)
	rel.Temp = into.Rel.Relpersistence == "t"
	rel.OnCommitDrop = into.OnCommit == pg_query.OnCommitAction_ONCOMMIT_DROP
	for i, c := range cols {
		c.Num = int16(i + 1)
		c.NotNull = false // the created table has no constraints, whatever the query guaranteed
		if i < len(into.ColNames) {
			c.Name = into.ColNames[i].GetString_().GetSval()
		}
		rel.Columns = append(rel.Columns, c)
	}
}

// createSequence registers a sequence as a relation with the three columns SELECT * FROM
// seq yields; serial and identity columns create theirs implicitly.
func (s *Schema) createSequence(rv *pg_query.RangeVar, loc int32) {
	schema, name := s.rangeVar(rv)
	if s.relByName[schema+"."+name] != nil {
		return
	}
	rel := s.newRelation(schema, name, Sequence)
	rel.Temp = rv.Relpersistence == "t"
	for i, c := range []struct {
		name string
		typ  catalog.OID
	}{{"last_value", catalog.Int8}, {"log_cnt", catalog.Int8}, {"is_called", catalog.Bool}} {
		rel.Columns = append(rel.Columns, &Column{Num: int16(i + 1), Name: c.name, Type: TypeRef{OID: c.typ, Typmod: -1}, NotNull: true})
	}
}

// rebuildRules recomputes what the relation's enabled rules make of it (a rule created,
// replaced, dropped, disabled or enabled). A rule disabled or firing only on replicas
// (ALTER TABLE ... DISABLE / ENABLE REPLICA RULE) is not there for the rewriter.
func (rel *Relation) rebuildRules() {
	rel.RuleNames, rel.RuleEvents, rel.InsteadRules, rel.QualifiedRules, rel.RuleNoReturning, rel.RuleCTEUnsupported, rel.RuleInsertSelect = nil, nil, nil, nil, nil, nil, nil
	names := make([]string, 0, len(rel.rules))
	for n := range rel.rules {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if !rel.rulesOff[n] {
			rel.applyRule(rel.rules[n])
		}
	}
}

// applyRule records one enabled rule.
func (rel *Relation) applyRule(r *pg_query.RuleStmt) {
	event := map[pg_query.CmdType]string{pg_query.CmdType_CMD_INSERT: "insert", pg_query.CmdType_CMD_UPDATE: "update", pg_query.CmdType_CMD_DELETE: "delete"}[r.Event]
	if event == "" {
		return
	}
	if rel.RuleNames == nil {
		rel.RuleNames = map[string]string{}
	}
	rel.RuleNames[r.Rulename] = event
	if rel.RuleEvents == nil {
		rel.RuleEvents = map[string]bool{}
	}
	rel.RuleEvents[event] = true
	set := func(m *map[string]bool) {
		if *m == nil {
			*m = map[string]bool{}
		}
		(*m)[event] = true
	}
	single := r.Instead && r.WhereClause == nil && len(r.Actions) == 1
	var dml bool
	if single {
		switch act := r.Actions[0].Node.(type) {
		case *pg_query.Node_InsertStmt:
			dml = true
			if q := act.InsertStmt.SelectStmt.GetSelectStmt(); q != nil && len(q.ValuesLists) == 0 {
				set(&rel.RuleInsertSelect)
			}
			if len(act.InsertStmt.ReturningList) == 0 {
				set(&rel.RuleNoReturning)
			}
		case *pg_query.Node_UpdateStmt:
			dml = true
			if len(act.UpdateStmt.ReturningList) == 0 {
				set(&rel.RuleNoReturning)
			}
		case *pg_query.Node_DeleteStmt:
			dml = true
			if len(act.DeleteStmt.ReturningList) == 0 {
				set(&rel.RuleNoReturning)
			}
		}
	}
	if r.Instead && r.WhereClause == nil && len(r.Actions) == 0 {
		set(&rel.RuleNoReturning) // DO INSTEAD NOTHING
	}
	if !(single && dml) {
		set(&rel.RuleCTEUnsupported)
	}
	if r.Instead && rel.Kind == View {
		if r.WhereClause == nil {
			if rel.InsteadRules == nil {
				rel.InsteadRules = map[string]bool{}
			}
			rel.InsteadRules[event] = true
		} else {
			if rel.QualifiedRules == nil {
				rel.QualifiedRules = map[string]bool{}
			}
			rel.QualifiedRules[event] = true
		}
	}
}

// setRuleEnabled is ALTER TABLE ... ENABLE / DISABLE RULE.
func (rel *Relation) setRuleEnabled(name string, enabled bool) {
	if rel.rulesOff == nil {
		rel.rulesOff = map[string]bool{}
	}
	if enabled {
		delete(rel.rulesOff, name)
	} else {
		rel.rulesOff[name] = true
	}
	rel.rebuildRules()
}

// createOwnedSequence is the implicit sequence of a serial / identity column.
func (s *Schema) createOwnedSequence(rel *Relation, col string, loc int32) {
	name := makeObjectName(rel.Name, col, "seq") // ChooseRelationName: truncated like every generated name
	s.createSequence(&pg_query.RangeVar{Schemaname: rel.Schema, Relname: name}, loc)
	if seq := s.relByName[rel.Schema+"."+name]; seq != nil && seq.Kind == Sequence {
		seq.OwnedBy = rel.Schema + "." + rel.Name + "." + col
	}
}

// dropOwnedSequences drops the sequences owned by rel's column col ("" = any column):
// dropping the table, the column or its identity takes them along.
func (s *Schema) dropOwnedSequences(rel *Relation, col string) {
	prefix := rel.Schema + "." + rel.Name + "."
	for _, r := range append([]*Relation{}, s.Relations...) {
		if r.Kind == Sequence && r.OwnedBy != "" && strings.HasPrefix(r.OwnedBy, prefix) && (col == "" || r.OwnedBy == prefix+col) {
			s.removeRelation(r)
		}
	}
}

// alterSequence applies ALTER SEQUENCE ... OWNED BY { table.column | NONE }; the other
// options do not affect typing.
func (s *Schema) alterSequence(st *pg_query.AlterSeqStmt, loc int32) {
	schema, name := s.rangeVar(st.Sequence)
	seq := s.relByName[schema+"."+name]
	if seq == nil || seq.Kind != Sequence {
		if !st.MissingOk {
			s.problem(loc, "ALTER SEQUENCE: relation %q does not exist", name)
		}
		return
	}
	for _, on := range st.Options {
		de := on.GetDefElem()
		if de == nil || de.Defname != "owned_by" {
			continue
		}
		parts := strs(de.Arg.GetList().GetItems())
		if len(parts) == 1 && strings.EqualFold(parts[0], "none") {
			seq.OwnedBy = ""
			continue
		}
		if len(parts) < 2 {
			continue
		}
		tschema, tname := qualified(parts[:len(parts)-1])
		if rel := s.findRelation(tschema, tname); rel != nil {
			seq.OwnedBy = rel.Schema + "." + rel.Name + "." + parts[len(parts)-1]
		}
	}
}

// HasSchema is whether a schema of that name exists: built in, created with CREATE SCHEMA,
// or holding an object.
func (s *Schema) HasSchema(name string) bool {
	switch name {
	case "public", "pg_catalog", "pg_temp", "information_schema", "pg_toast":
		return true
	}
	if s.schemas[name] {
		return true
	}
	for _, r := range s.Relations {
		if r.Schema == name {
			return true
		}
	}
	for _, f := range s.Functions {
		if f.Schema == name {
			return true
		}
	}
	for _, sch := range s.Types.Schemas {
		if sch == name {
			return true
		}
	}
	return false
}

// Schemas lists the names created with CREATE SCHEMA, sorted.
func (s *Schema) Schemas() []string {
	out := make([]string, 0, len(s.schemas))
	for n := range s.schemas {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// RuleDef is one rule of a relation as declared, with whether it is enabled.
type RuleDef struct {
	Stmt    *pg_query.RuleStmt
	Enabled bool
}

// Rules returns the relation's rules by name (CREATE RULE, minus DROP RULE).
func (rel *Relation) Rules() map[string]RuleDef {
	out := make(map[string]RuleDef, len(rel.rules))
	for n, st := range rel.rules {
		out[n] = RuleDef{Stmt: st, Enabled: !rel.rulesOff[n]}
	}
	return out
}

// Deparse renders an expression back to SQL text. Two expressions that deparse the
// same are the same for the loader's purposes; "" if the node cannot be rendered.
func Deparse(e Expr) string {
	if e == nil {
		return ""
	}
	res := &pg_query.ParseResult{Stmts: []*pg_query.RawStmt{{Stmt: &pg_query.Node{Node: &pg_query.Node_SelectStmt{SelectStmt: &pg_query.SelectStmt{
		TargetList: []*pg_query.Node{{Node: &pg_query.Node_ResTarget{ResTarget: &pg_query.ResTarget{Val: e}}}},
	}}}}}}
	s, err := pg_query.Deparse(res)
	if err != nil {
		return ""
	}
	return strings.TrimPrefix(s, "SELECT ")
}

// DeparseStmt renders a whole statement (a view's query, a rule) back to SQL text.
func DeparseStmt(n *pg_query.Node) string {
	if n == nil {
		return ""
	}
	s, err := pg_query.Deparse(&pg_query.ParseResult{Stmts: []*pg_query.RawStmt{{Stmt: n}}})
	if err != nil {
		return ""
	}
	return s
}

// DeparseBody renders a SQL-standard function body (Function.SQLBody): `RETURN expr` or
// `BEGIN ATOMIC stmt; ... END`, which Deparse cannot render on their own.
func DeparseBody(body Expr) string {
	if body == nil {
		return ""
	}
	if r := body.GetReturnStmt(); r != nil {
		return "RETURN " + Deparse(r.Returnval)
	}
	if l := body.GetList(); l != nil {
		var stmts []string
		for _, item := range l.Items {
			inner := item.GetList()
			if inner == nil {
				stmts = append(stmts, DeparseStmt(item))
				continue
			}
			for _, st := range inner.Items {
				stmts = append(stmts, DeparseStmt(st))
			}
		}
		return "BEGIN ATOMIC " + strings.Join(stmts, "; ") + "; END"
	}
	return DeparseStmt(body)
}
