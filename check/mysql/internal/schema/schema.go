// Package schema loads a MySQL schema.sql into a model of its tables and views.
//
// It reads the DDL through mysqlast, the server's own parse tree: CREATE TABLE (column
// definitions, keys, foreign keys, checks, table options), CREATE INDEX, CREATE VIEW,
// ALTER TABLE, RENAME TABLE, DROP TABLE / INDEX / VIEW. Statements it cannot apply are
// reported as Problems with their position, and the schema is still built from the rest,
// so that a checker can say what it did not understand instead of failing outright.
//
// The model is MySQL's: types keep their length, precision, signedness, charset and
// collation; keys are the four MySQL kinds; a column knows its default, its ON UPDATE,
// its generation expression and its visibility.
package schema

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/kr9ly/sqlshape/check/mysql/v2/internal/mysqlast"
	"github.com/kr9ly/sqlshape/check/mysql/v2/internal/mysqlparse"
	"github.com/kr9ly/sqlshape/v2/x/dialect"
	"github.com/kr9ly/sqlshape/v2/x/sqlmode"
)

// Schema is the loaded schema.
type Schema struct {
	// Version is the declared MySQL version ("8.4"), from `-- sqlshape: mysql 8.4`.
	Version string
	// Settings are the server variables the schema declares (`-- sqlshape: server ...`),
	// the server's defaults where it declares none.
	Settings Settings
	Tables   []*Table
	Views    []*View
	Triggers []*Trigger
	Routines []*Routine
	Events   []*Event
	Problems []Problem
	// cur is the statement being applied, for the element texts (Column.Text ...)
	cur string
}

// Settings are the server variables that change how statements are judged. The schema
// declares them as `-- sqlshape: server <variable> = <value>` lines; these two are the
// variables MySQL's loader reads, any other name is a problem.
type Settings struct {
	// SQLMode is the sql_mode, expanded (sqlmode.Default when undeclared). The parser reads
	// its lexer bits; the analyzer ONLY_FULL_GROUP_BY, the strict flags, REAL_AS_FLOAT and
	// NO_UNSIGNED_SUBTRACTION.
	SQLMode sqlmode.Mode
	// LowerCaseTableNames is lower_case_table_names: 0 compares table and view names
	// case-sensitively (the Linux default), 1 stores them lower-cased and compares
	// case-insensitively (Windows), 2 stores them as declared and compares
	// case-insensitively (macOS).
	LowerCaseTableNames int
}

// SettingNames are the server variables the loader reads.
var SettingNames = []string{"sql_mode", "lower_case_table_names"}

// ParseMode is the sql_mode as the parser takes it.
func (st Settings) ParseMode() mysqlparse.Mode { return mysqlparse.Mode(uint32(st.SQLMode)) }

// Strict is whether either strict flag is set (THD::is_strict_mode).
func (st Settings) Strict() bool { return st.SQLMode.Strict() }

// tableKey is the form a table or view name is stored and compared in.
func (s *Schema) tableKey(name string) string {
	if s.Settings.LowerCaseTableNames == 1 {
		return strings.ToLower(name)
	}
	return name
}

// CanonicalName is the stored spelling of a table or view name written in a directive
// (`unfiltered Users`, `waive USERS ...`): the declared table's or view's name when one
// matches under lower_case_table_names, else the name in stored form. The facts name leaves
// by the stored spelling, so a waiver keyed by it meets its leaf.
func (s *Schema) CanonicalName(name string) string {
	if t := s.Table(name); t != nil {
		return t.Name
	}
	if v := s.View(name); v != nil {
		return v.Name
	}
	return s.tableKey(name)
}

// sameTable compares two table or view names as the server does under
// lower_case_table_names.
func (s *Schema) sameTable(a, b string) bool {
	if s.Settings.LowerCaseTableNames == 0 {
		return a == b
	}
	return strings.EqualFold(a, b)
}

// Problem is a statement, or part of one, the loader could not apply.
type Problem struct {
	Position int // byte offset in the schema text
	Message  string
}

func (p Problem) String() string { return fmt.Sprintf("%d: %s", p.Position, p.Message) }

// Table is a base table.
type Table struct {
	Name        string
	Columns     []*Column
	Keys        []*Key
	ForeignKeys []*ForeignKey
	Checks      []*Check
	Engine      string
	Charset     string
	Collation   string
	// RowFormat is the table's own `ROW_FORMAT=...` option ("DYNAMIC", "COMPRESSED", ...),
	// "" when the statement declares none (the server's own default, not written back).
	RowFormat string
	// AutoIncrementStart is the table's own `AUTO_INCREMENT=<n>` option as written, "" when
	// the statement declares none. A live counter is data, not schema (dump.normalizeTable
	// strips it from a canonicalized table read back from a server), but the value an
	// author wrote for a table's first creation is a schema decision the loader still keeps
	// here so a fresh CREATE TABLE can reproduce it (see dump.pinAutoIncrement).
	AutoIncrementStart string
	Comment            string
	Temporary          bool
	// Partitioning is the table's PARTITION BY clause, nil for an unpartitioned table.
	Partitioning *Partitioning
	// Definition is the CREATE TABLE text; Alters the ALTER TABLE texts applied after it.
	Definition string
	Alters     []string
	// Directives are the `-- sqlshape: ...` lines written above the CREATE, normalized:
	// the obligations x/obligation parses.
	Directives []string
}

// Partitioning is a table's PARTITION BY clause. Every shape this package's grammar admits
// is broken down structurally -- RANGE/LIST COLUMNS, KEY, LINEAR HASH/KEY and subpartitions
// included -- so there is no Text fallback: a clause this loader cannot break down (an
// explicit per-partition SUBPARTITION list, or anything partitioning's own PartTypeDef
// switch does not name) is a problem up front, not a whole-clause equality/rewrite escape
// hatch a later stage discovers cannot be migrated incrementally.
type Partitioning struct {
	// Kind is "RANGE", "LIST", "HASH" or "KEY".
	Kind string
	// Linear is LINEAR HASH / LINEAR KEY: the partition function spreads a value's own hash
	// by bit-shifting instead of MOD, so a partition count change never reshuffles every row
	// (a plain HASH/KEY does); this package writes the same ADD PARTITION PARTITIONS n /
	// COALESCE PARTITION n DDL regardless (the server picks the spread), and only rewrites
	// the whole clause if Linear itself flips.
	Linear bool
	// Columns is RANGE COLUMNS / LIST COLUMNS: the partitioning key is a column list (Cols),
	// not a single expression (Expr); a partition's own Bound is then a value tuple, one
	// literal per column, rather than a single scalar (see Partition's own doc comment).
	Columns bool
	// Expr is the partitioning key's own text, as written ("`id`", "(`id` + 1)"); set only
	// when Kind is RANGE, HASH or LIST and Columns is false.
	Expr string
	// Cols is KEY's own column list (opt_columns -- empty when the statement wrote "KEY ()",
	// the server's own cue to partition by the table's primary key; this package keeps that
	// empty list as written rather than resolving it, since the DDL this package writes back
	// spells it the same way), or RANGE/LIST COLUMNS' own column list; set only when Kind is
	// KEY, or Columns is true.
	Cols []string
	// Algorithm is KEY's own ALGORITHM=1|2 (pre-5.5 / 5.5+ hashing; see mysqlast/hooks_ddl.go's
	// own hand-written opt_key_algo rule), 0 for the default (not written).
	Algorithm int
	// Num is HASH/KEY's own partition count (PARTITIONS n); Parts is RANGE/LIST's ordered
	// partition list. Exactly one of them holds anything, matching Kind.
	Num   int
	Parts []Partition
	// Sub is the table's own SUBPARTITION BY clause, nil for none. Only RANGE and LIST ever
	// carry one (the grammar refuses it under HASH/KEY).
	Sub *SubPartitioning
}

// SubPartitioning is a RANGE or LIST Partitioning's own SUBPARTITION BY clause: every
// partition is split further by HASH or KEY, always to the same kind, expression/columns
// and count -- the default this package's own ADD PARTITION / REORGANIZE PARTITION rewrite
// for the parent still gets right without ever naming a single subpartition (an explicit,
// per-partition SUBPARTITION list overriding this default is not modeled at all, see
// partitionDef: information_schema.partitions' own per-subpartition names are the server's
// bookkeeping, not something a plan needs to reproduce).
type SubPartitioning struct {
	Kind      string // "HASH" or "KEY"
	Linear    bool
	Expr      string   // HASH's own expr text
	Cols      []string // KEY's own column list
	Algorithm int
	Num       int // SUBPARTITIONS n
}

// Partition is one partition of a RANGE or LIST Partitioning.
type Partition struct {
	Name string
	// MaxValue is a plain (non-COLUMNS) RANGE partition's own "VALUES LESS THAN MAXVALUE",
	// or its own omitted VALUES clause (only the last partition of a RANGE clause may say
	// either); Bound is the literal boundary's own text otherwise -- a single scalar for a
	// plain RANGE/LIST, or a value tuple (RANGE COLUMNS, comma-joined) / one or more value
	// tuples (LIST COLUMNS, `(a,b),(c,d)`, each parenthesized, comma-joined) for the COLUMNS
	// variants, where MAXVALUE folds into the text itself instead (every column still needs
	// its own literal, so there is no single "the whole partition is unbounded" case there).
	MaxValue bool
	Bound    string
	// Comment is this partition's own COMMENT option, "" for none: the one per-partition
	// option (of TABLESPACE / ENGINE / NODEGROUP / MAX_ROWS / MIN_ROWS / DATA DIRECTORY /
	// INDEX DIRECTORY / COMMENT) this package models -- ENGINE is always InnoDB in practice
	// (SHOW CREATE TABLE writes it on every partition regardless, measured, so it is dropped
	// rather than compared, see PartitioningProps) and the rest are rare enough that
	// partitionDef raises a problem rather than reading them.
	Comment string
}

// Column is a table column.
type Column struct {
	Name          string
	Type          Type
	NotNull       bool
	Default       mysqlast.Value // nil when none; a literal, CURRENT_TIMESTAMP, or an (expression)
	AutoIncrement bool
	OnUpdate      bool // ON UPDATE CURRENT_TIMESTAMP
	Generated     mysqlast.Value
	Stored        bool // a generated column that is STORED (else VIRTUAL)
	Comment       string
	Invisible     bool
	Collation     string
	// Text is the column's definition as written in the statement that declared it
	// (`\`id\` bigint unsigned NOT NULL AUTO_INCREMENT` in a CREATE TABLE), what an ALTER
	// TABLE ... MODIFY COLUMN takes; "" when the column came from a statement the loader
	// does not keep the text of
	Text string
}

// KeyKind is a MySQL index kind.
type KeyKind byte

const (
	Primary KeyKind = iota + 1
	Unique
	Index
	Fulltext
	Spatial
)

func (k KeyKind) String() string {
	return [...]string{"", "PRIMARY KEY", "UNIQUE", "INDEX", "FULLTEXT", "SPATIAL"}[k]
}

// Key is an index or a PRIMARY / UNIQUE constraint.
type Key struct {
	Name      string
	Kind      KeyKind
	Parts     []KeyPart
	Invisible bool
	Comment   string
	Text      string // the key's definition as written inline in its CREATE TABLE, "" otherwise
}

// KeyPart is one key column, or a prefix of it, or an expression.
type KeyPart struct {
	Column string
	Length int // prefix length, 0 for the whole column
	Desc   bool
	Expr   mysqlast.Value // functional key part
}

// ForeignKey is a REFERENCES constraint.
type ForeignKey struct {
	Name       string
	Columns    []string
	RefTable   string
	RefColumns []string
	OnDelete   string // CASCADE, SET NULL, RESTRICT, NO ACTION, SET DEFAULT, or "" for the default
	OnUpdate   string
	Text       string // the constraint's definition as written inline in its CREATE TABLE, "" otherwise
}

// Check is a CHECK constraint.
type Check struct {
	Name     string
	Expr     mysqlast.Value
	Enforced bool
	Text     string // the constraint's definition as written inline in its CREATE TABLE, "" otherwise
}

// View is a CREATE VIEW.
type View struct {
	Name        string
	Columns     []string // the declared column list, when given
	Query       mysqlast.Value
	Algorithm   string
	CheckOption string
	Definition  string
	// Directives are the `-- sqlshape: ...` lines written above the CREATE (obligations);
	// Unfiltered / Waived are the view's own opt-outs (`unfiltered t`, `waive t ...`): the
	// obligations its body does not owe, by table, carried on the body's leaves.
	Directives []string
	Unfiltered map[string]bool
	Waived     map[string][]string
}

// Event is a CREATE EVENT: a body the server runs on its own schedule. Nothing a program's
// statement runs reaches it, so its body is read the way a routine's is (analyze.AnalyzeEvent)
// for the schema's own sake -- an event whose body names a table the schema does not have is
// accepted by the server at CREATE time and fails at every run -- and its definition is what
// diff / apply manage.
type Event struct {
	Name string
	// At is a one-time event's `AT <expr>`; Every a recurring one's `EVERY <n> <unit>`
	// ("1 DAY"); Starts / Ends the recurring event's bounds, "" when not written. Each is
	// the expression's own text.
	At, Every, Starts, Ends string
	// AtLiteral / StartsLiteral / EndsLiteral: the time was written as a string literal,
	// a value the server stores as written. An expression (`CURRENT_TIMESTAMP`, `NOW() +
	// INTERVAL 1 DAY`) and an omitted STARTS (the server fills in the creation time,
	// measured) are evaluated when the event is created, so the time SHOW CREATE EVENT
	// reads back is an accident of when: the canonical form of such an event carries a time
	// that means nothing to compare (dump marks it from the source text; diff skips it).
	AtLiteral, StartsLiteral, EndsLiteral bool
	// Completion is PRESERVE or NOT PRESERVE (the default); Status ENABLE (the default),
	// DISABLE or DISABLE ON REPLICA.
	Completion string
	Status     string
	Comment    string
	Body       mysqlast.Value // the DO body (sp_block_content, or a single statement)
	BodyText   string         // the body's own text, for comparison
	Definition string         // the CREATE EVENT text
}

// Trigger is a CREATE TRIGGER.
type Trigger struct {
	Name  string
	Table string // the trigger's table, in stored form (tableKey)
	// Timing is BEFORE or AFTER; Event is INSERT, UPDATE or DELETE.
	Timing string
	Event  string
	// OrderClause is FOLLOWS or PRECEDES, "" when the trigger declares neither; OrderTrigger
	// is the anchor trigger's name it names, "" when OrderClause is "".
	OrderClause  string
	OrderTrigger string
	Body         mysqlast.Value // the trigger's body statement (sp_block_content, or a single statement)
	Definition   string         // the CREATE TRIGGER text
	// Directives are the `-- sqlshape: ...` lines written above the CREATE (currently only
	// `error <key> = <name>` is read here; a later stage parses it).
	Directives []string
}

// RoutineKind is whether a Routine is a PROCEDURE or a FUNCTION: they are separate
// namespaces (a schema may declare both p() and a PROCEDURE and a FUNCTION of that name).
type RoutineKind byte

const (
	Procedure RoutineKind = iota + 1
	Function
)

func (k RoutineKind) String() string {
	return [...]string{"", "PROCEDURE", "FUNCTION"}[k]
}

// Param is one parameter of a stored routine.
type Param struct {
	Mode string // IN, OUT or INOUT (a stored function's parameters are all IN)
	Name string
	Type Type
}

// Routine is a CREATE PROCEDURE or CREATE FUNCTION.
type Routine struct {
	Name    string
	Kind    RoutineKind
	Params  []Param
	Returns Type // a stored function's RETURNS type; the zero Type for a procedure
	// NotNull is the `-- sqlshape: not null` annotation above a CREATE FUNCTION: the
	// function's result is never NULL, the same override postgres/schema.Function.NotNull
	// documents for PostgreSQL (a stored function's RETURN can otherwise produce NULL
	// regardless of the declared type, with no static proof otherwise).
	NotNull bool
	// Deterministic, DataAccess and Security are the routine's characteristics as declared
	// (CREATE's defaults when not given: not deterministic, CONTAINS SQL, SQL SECURITY
	// DEFINER); ALTER PROCEDURE/FUNCTION does not change them (it has no schema effect here).
	Deterministic bool
	DataAccess    string // NO_SQL, CONTAINS_SQL, READS_SQL_DATA or MODIFIES_SQL_DATA
	Security      string // DEFINER or INVOKER
	Body          mysqlast.Value
	Definition    string // the CREATE PROCEDURE/FUNCTION text
	Directives    []string
}

// Table returns the table named name, or nil. Table and view names compare as the
// declared lower_case_table_names says (case-sensitively at 0, the Linux default); column
// and key names never mind case.
func (s *Schema) Table(name string) *Table {
	for _, t := range s.Tables {
		if s.sameTable(t.Name, name) {
			return t
		}
	}
	return nil
}

// View returns the view named name, or nil.
func (s *Schema) View(name string) *View {
	for _, v := range s.Views {
		if s.sameTable(v.Name, name) {
			return v
		}
	}
	return nil
}

// Trigger returns the trigger named name, or nil. Trigger names are not affected by
// lower_case_table_names (that setting is a table/view naming convention only).
func (s *Schema) Trigger(name string) *Trigger {
	for _, t := range s.Triggers {
		if strings.EqualFold(t.Name, name) {
			return t
		}
	}
	return nil
}

// Event returns the event named name, or nil. Event names are not affected by
// lower_case_table_names either.
func (s *Schema) Event(name string) *Event {
	for _, e := range s.Events {
		if strings.EqualFold(e.Name, name) {
			return e
		}
	}
	return nil
}

// Routine returns the procedure or function named name, or nil. PROCEDURE and FUNCTION are
// separate namespaces in MySQL; when a schema declares both of the same name, the first one
// declared wins here (dump/diff/migrate, not written yet, will need the kind to tell them
// apart when that matters).
func (s *Schema) Routine(name string) *Routine {
	for _, r := range s.Routines {
		if strings.EqualFold(r.Name, name) {
			return r
		}
	}
	return nil
}

// RoutineOf returns the procedure or function of the given kind named name, or nil: unlike
// Routine, it does not guess between a PROCEDURE and a FUNCTION of the same name (dump /
// diff / migrate need to tell them apart).
func (s *Schema) RoutineOf(kind RoutineKind, name string) *Routine {
	return s.routine(kind, name)
}

// routine returns the routine of the given kind named name, or nil.
func (s *Schema) routine(kind RoutineKind, name string) *Routine {
	for _, r := range s.Routines {
		if r.Kind == kind && strings.EqualFold(r.Name, name) {
			return r
		}
	}
	return nil
}

// Column returns the column named name, or nil.
func (t *Table) Column(name string) *Column {
	for _, c := range t.Columns {
		if strings.EqualFold(c.Name, name) {
			return c
		}
	}
	return nil
}

// PrimaryKey returns the table's primary key, or nil.
func (t *Table) PrimaryKey() *Key {
	for _, k := range t.Keys {
		if k.Kind == Primary {
			return k
		}
	}
	return nil
}

var versionLine = regexp.MustCompile(`(?m)^[ \t]*--[ \t]*sqlshape:[ \t]*mysql[ \t]+(\S+)[ \t]*$`)

// Supported lists the MySQL versions with an embedded parser.
func Supported() []string { return []string{"8.4"} }

// DeclaredVersion reads the `-- sqlshape: mysql <major.minor>` declaration; "8.4" when
// there is none.
func DeclaredVersion(schemaSQL string) (string, error) {
	v := ""
	for _, m := range versionLine.FindAllStringSubmatch(schemaSQL, -1) {
		ok := false
		for _, s := range Supported() {
			if s == m[1] {
				ok = true
			}
		}
		if !ok {
			return "", fmt.Errorf("schema: `-- sqlshape: mysql %s`: sqlshape supports MySQL %s", m[1], strings.Join(Supported(), ", "))
		}
		if v != "" && v != m[1] {
			return "", fmt.Errorf("schema: `-- sqlshape: mysql` is declared twice, as %s and %s", v, m[1])
		}
		v = m[1]
	}
	if v == "" {
		v = Supported()[0]
	}
	return v, nil
}

// Load parses schema.sql and applies its statements in order.
func Load(schemaSQL string) (*Schema, error) {
	v, err := DeclaredVersion(schemaSQL)
	if err != nil {
		return nil, err
	}
	s := &Schema{Version: v, Settings: Settings{SQLMode: sqlmode.Default}}
	settings, err := dialect.Settings(schemaSQL)
	if err != nil {
		return nil, err
	}
	for _, st := range settings {
		s.setting(st)
	}
	for _, st := range mysqlparse.Split(schemaSQL) {
		s.apply(st)
	}
	return s, nil
}

// setting applies one declared server variable.
func (s *Schema) setting(st dialect.Setting) {
	switch st.Name {
	case "sql_mode":
		m, err := sqlmode.Parse(st.Value)
		if err != nil {
			s.problem(st.Position, "server %s = '%s': %v", st.Name, st.Value, err)
			return
		}
		s.Settings.SQLMode = m
	case "lower_case_table_names":
		n, err := strconv.Atoi(st.Value)
		if err != nil || n < 0 || n > 2 {
			s.problem(st.Position, "server %s = %s: want 0, 1 or 2", st.Name, st.Value)
			return
		}
		s.Settings.LowerCaseTableNames = n
	default:
		s.problem(st.Position, "server %s: not a variable sqlshape reads for MySQL (%s)", st.Name, strings.Join(SettingNames, ", "))
	}
}

func (s *Schema) problem(pos int, format string, args ...any) {
	s.Problems = append(s.Problems, Problem{Position: pos, Message: fmt.Sprintf(format, args...)})
}

// apply parses one statement and folds it into the schema.
func (s *Schema) apply(st mysqlparse.Statement) {
	cst, err := mysqlparse.Parse(st.SQL, s.Settings.ParseMode())
	if err != nil {
		if pe, ok := err.(*mysqlparse.Error); ok {
			s.problem(st.Offset+pe.Offset, "%s", pe.Message)
			return
		}
		s.problem(st.Offset, "%v", err)
		return
	}
	v, err := mysqlast.BuildMode(st.SQL, cst, s.Settings.ParseMode())
	if err != nil {
		if u, ok := err.(*mysqlast.Unsupported); ok {
			s.problem(st.Offset+u.Start, "unsupported construct %s: %q", u.Rule, u.Text)
			return
		}
		s.problem(st.Offset, "%v", err)
		return
	}
	// DROP TRIGGER/PROCEDURE and ALTER PROCEDURE/FUNCTION have no NEW_PTN construction of
	// their own in the server's grammar (the action fills LEX by hand); their generic fold
	// is a by-value Struct (Lex's fields), not a Node.
	if x, ok := v.(*mysqlast.Struct); ok {
		s.applyStmt(x, st)
		return
	}
	n, ok := v.(*mysqlast.Node)
	if !ok {
		s.problem(st.Offset, "not a statement the schema loader reads: %s", mysqlast.Sprint(v))
		return
	}
	s.cur = st.SQL
	at := func(v mysqlast.Value) int {
		if x, ok := v.(*mysqlast.Node); ok {
			return st.Offset + x.Start
		}
		return st.Offset
	}
	switch n.Class {
	case "PT_create_table_stmt":
		s.createTable(n, st, at)
	case "PT_create_index_stmt":
		s.createIndex(n, st, at)
	case "Sql_cmd_create_view":
		s.createView(n, st)
	case "trigger_tail":
		s.createTrigger(n, st, at)
	case "event_tail":
		s.createEvent(n, st, at)
	case "alter_event_stmt":
		s.alterEvent(n, st, at)
	case "sp_tail":
		s.createRoutine(n, Procedure, st, at)
	case "sf_tail":
		s.createRoutine(n, Function, st, at)
	case "drop_function_stmt":
		s.dropRoutine(Function, spName(n.Arg("spname")), isTrue(n.Arg("if_exists")), at(n))
	case "PT_alter_table_stmt":
		s.alterTable(n, st, at)
	case "PT_alter_table_standalone_stmt":
		s.alterTable(n, st, at) // one action (ADD PARTITION ...), same shape for our purposes
	case "PT_drop_index_stmt":
		x, _ := mysqlast.AsPTDropIndexStmt(n)
		t := s.needTable(tableName(x.Table()), at(n))
		if t != nil && !t.dropKey(str(x.IndexName())) {
			s.problem(at(n), "DROP INDEX %s: no such index on %s", str(x.IndexName()), t.Name)
		}
	case "Sql_cmd_drop_table":
		for _, ti := range list(n.Arg("tables")) {
			name := tableName(ti)
			if s.Table(name) == nil {
				if n.Arg("if_exists") == nil || n.Arg("if_exists") == mysqlast.Const("0") || n.Arg("if_exists") == mysqlast.Const("false") {
					s.problem(at(ti), "DROP TABLE %s: no such table", name)
				}
				continue
			}
			s.dropTable(name)
		}
	case "Sql_cmd_drop_view":
		for _, ti := range list(n.Arg("views")) {
			name := tableName(ti)
			if s.View(name) == nil {
				s.problem(at(ti), "DROP VIEW %s: no such view", name)
				continue
			}
			s.dropView(name)
		}
	case "PT_truncate_table_stmt", "PT_set", "PT_option_value_list", "PT_start_option_value_list_no_type":
		// no schema effect
	default:
		if n.Implicit && n.Class == "table_to_table_list" || n.Implicit && n.Class == "table_to_table" {
			s.renameTables(n, at)
			return
		}
		s.problem(st.Offset, "statement not applied to the schema: %s", n.Class)
	}
}

// applyStmt folds a statement whose generic fold is a by-value Struct (Lex's fields), not a
// Node: DROP TRIGGER, DROP PROCEDURE, ALTER PROCEDURE, ALTER FUNCTION.
func (s *Schema) applyStmt(x *mysqlast.Struct, st mysqlparse.Statement) {
	switch str(x.Fields["sql_command"]) {
	case "SQLCOM_DROP_TRIGGER":
		s.dropTrigger(spName(x.Fields["spname"]), isTrue(x.Fields["drop_if_exists"]), st.Offset)
	case "SQLCOM_DROP_EVENT":
		s.dropEvent(spName(x.Fields["spname"]), isTrue(x.Fields["drop_if_exists"]), st.Offset)
	case "SQLCOM_DROP_PROCEDURE":
		s.dropRoutine(Procedure, spName(x.Fields["spname"]), isTrue(x.Fields["drop_if_exists"]), st.Offset)
	case "SQLCOM_ALTER_PROCEDURE":
		s.alterRoutine(Procedure, spName(x.Fields["spname"]), st.Offset)
	case "SQLCOM_ALTER_FUNCTION":
		s.alterRoutine(Function, spName(x.Fields["spname"]), st.Offset)
	default:
		s.problem(st.Offset, "statement not applied to the schema: %s", str(x.Fields["sql_command"]))
	}
}

// --- CREATE TABLE ------------------------------------------------------------------

func (s *Schema) createTable(n *mysqlast.Node, st mysqlparse.Statement, at func(mysqlast.Value) int) {
	x, _ := mysqlast.AsPTCreateTableStmt(n)
	name := s.tableKey(tableName(x.TableName()))
	if s.Table(name) != nil {
		if isTrue(x.OnlyIfNotExists()) {
			return
		}
		s.problem(at(n), "CREATE TABLE %s: table already exists", name)
		return
	}
	t := &Table{Name: name, Temporary: isTrue(x.IsTemporary()), Definition: st.SQL}
	if like := x.OptLikeClause(); like != nil {
		src := s.Table(tableName(like))
		if src == nil {
			s.problem(at(n), "CREATE TABLE %s LIKE %s: no such table", name, tableName(like))
			return
		}
		copyTable(t, src)
		s.Tables = append(s.Tables, t)
		return
	}
	if x.OptQueryExpression() != nil {
		s.problem(at(n), "CREATE TABLE %s AS SELECT: columns come from the query, which the loader does not evaluate", name)
	}
	for _, el := range list(x.OptTableElementList()) {
		s.tableElement(t, el, at)
	}
	s.tableOptions(t, list(x.OptCreateTableOptions()), at)
	if p := x.OptPartitioning(); p != nil {
		t.Partitioning = s.partitioning(p, at)
	}
	s.tableDirectives(t, st.SQL, st.Offset)
	s.Tables = append(s.Tables, t)
}

// partitioning reads v, a PT_partition node (CREATE TABLE's own, or an ALTER TABLE
// PARTITION BY's), into the model, or records a problem and returns nil for a clause this
// package does not break down at all (see Partitioning's own doc comment: an explicit
// per-partition SUBPARTITION list, or a PartTypeDef this switch does not name).
func (s *Schema) partitioning(v mysqlast.Value, at func(mysqlast.Value) int) *Partitioning {
	n, ok := v.(*mysqlast.Node)
	if !ok {
		s.problem(at(v), "PARTITION BY: clause not understood")
		return nil
	}
	x, _ := mysqlast.AsPTPartition(n)
	p := &Partitioning{}
	switch d := x.PartTypeDef().(type) {
	case *mysqlast.Node:
		switch d.Class {
		case "PT_part_type_def_range_expr":
			rx, _ := mysqlast.AsPTPartTypeDefRangeExpr(d)
			p.Kind, p.Expr = "RANGE", s.exprText(rx.Expr())
		case "PT_part_type_def_range_columns":
			rx, _ := mysqlast.AsPTPartTypeDefRangeColumns(d)
			p.Kind, p.Columns = "RANGE", true
			p.Cols = names(rx.Columns())
		case "PT_part_type_def_list_expr":
			lx, _ := mysqlast.AsPTPartTypeDefListExpr(d)
			p.Kind, p.Expr = "LIST", s.exprText(lx.Expr())
		case "PT_part_type_def_list_columns":
			lx, _ := mysqlast.AsPTPartTypeDefListColumns(d)
			p.Kind, p.Columns = "LIST", true
			p.Cols = names(lx.Columns())
		case "PT_part_type_def_hash":
			hx, _ := mysqlast.AsPTPartTypeDefHash(d)
			p.Kind, p.Linear, p.Expr = "HASH", isTrue(hx.IsLinear()), s.exprText(hx.Expr())
		case "PT_part_type_def_key":
			kx, _ := mysqlast.AsPTPartTypeDefKey(d)
			p.Kind, p.Linear = "KEY", isTrue(kx.IsLinear())
			p.Algorithm = keyAlgorithm(kx.KeyAlgo())
			p.Cols = names(kx.OptColumns())
		default:
			s.problem(at(n), "PARTITION BY: clause not understood")
			return nil
		}
	default:
		s.problem(at(n), "PARTITION BY: clause not understood")
		return nil
	}
	if num := x.OptNumParts(); num != nil {
		p.Num = numOf(num)
	}
	if sub := x.OptSubPart(); sub != nil {
		sn, ok := sub.(*mysqlast.Node)
		if !ok {
			s.problem(at(n), "SUBPARTITION BY: clause not understood")
			return nil
		}
		switch sn.Class {
		case "PT_sub_partition_by_hash":
			sx, _ := mysqlast.AsPTSubPartitionByHash(sn)
			p.Sub = &SubPartitioning{Kind: "HASH", Linear: isTrue(sx.IsLinear()), Expr: s.exprText(sx.Hash()), Num: numOf(sx.OptNumSubparts())}
		case "PT_sub_partition_by_key":
			sx, _ := mysqlast.AsPTSubPartitionByKey(sn)
			p.Sub = &SubPartitioning{Kind: "KEY", Linear: isTrue(sx.IsLinear()), Algorithm: keyAlgorithm(sx.KeyAlgo()), Cols: names(sx.FieldNames()), Num: numOf(sx.OptNumSubparts())}
		default:
			s.problem(at(n), "SUBPARTITION BY: clause not understood")
			return nil
		}
	}
	defs := list(x.PartDefs())
	if p.Kind == "HASH" || p.Kind == "KEY" {
		if len(defs) > 0 {
			s.problem(at(n), "PARTITION BY %s: an explicit partition list is not supported", p.Kind)
			return nil
		}
		return p
	}
	// RANGE: every partition definition names its own upper bound (or MAXVALUE); LIST: every
	// partition definition names its own value list (partitionDef tells the two apart by the
	// class OptPartValues() itself carries, guided by p.Columns).
	for _, el := range defs {
		pd, ok := el.(*mysqlast.Node)
		if !ok {
			s.problem(at(n), "PARTITION %s: definition not understood", p.Kind)
			return nil
		}
		part, ok := s.partitionDef(pd, p.Columns)
		if !ok {
			s.problem(at(n), "PARTITION %s: definition not understood", p.Kind)
			return nil
		}
		p.Parts = append(p.Parts, part)
	}
	return p
}

// names renders a name_list (KEY's own column list, or RANGE/LIST COLUMNS' own) as plain
// identifier strings, in order.
func names(v mysqlast.Value) []string {
	var out []string
	for _, el := range list(v) {
		out = append(out, str(el))
	}
	return out
}

// keyAlgorithm reads opt_key_algo's own constant (mysqlast/hooks_ddl.go's own hand-written
// rule folds ALGORITHM_SYM EQ real_ulong_num to one of these two) into KEY's own
// ALGORITHM=1|2, 0 for the default (not written).
func keyAlgorithm(v mysqlast.Value) int {
	switch str(v) {
	case "enum_key_algorithm::KEY_ALGORITHM_51":
		return 1
	case "enum_key_algorithm::KEY_ALGORITHM_55":
		return 2
	}
	return 0
}

// partitionDef reads one PT_part_definition of a RANGE or LIST Partitioning (columns tells
// a plain clause apart from its COLUMNS variant, which admits a value tuple per column
// rather than a single scalar): a RANGE partition names its own upper bound (Bound) or none
// at all (MaxValue, VALUES LESS THAN MAXVALUE or no VALUES clause at all -- COLUMNS' own
// per-column MAXVALUE folds into Bound's own text instead, see Partition's own doc comment);
// a LIST partition names its own value list, joined into Bound the way SHOW CREATE spells it
// (a flat OR-set for a plain LIST, one or more parenthesized value tuples for COLUMNS). ok is
// false for anything this package does not break down (an explicit SUBPARTITION list,
// MAXVALUE inside a LIST partition's own list -- not valid SQL but the grammar admits it, or
// a per-partition option other than COMMENT / ENGINE), the caller's cue to raise a problem.
func (s *Schema) partitionDef(n *mysqlast.Node, columns bool) (Partition, bool) {
	x, _ := mysqlast.AsPTPartDefinition(n)
	if x.OptSubPartitions() != nil {
		return Partition{}, false
	}
	part := Partition{Name: str(x.Name())}
	for _, opt := range list(x.OptPartOptions()) {
		on, ok := opt.(*mysqlast.Node)
		if !ok {
			return Partition{}, false
		}
		switch on.Class {
		case "PT_partition_comment":
			part.Comment = str(on.Arg("comment"))
		case "PT_partition_engine":
			// always InnoDB in practice (Partition.Comment's own doc comment says why); dropped.
		default:
			return Partition{}, false // TABLESPACE / NODEGROUP / MAX_ROWS / MIN_ROWS / DATA|INDEX DIRECTORY
		}
	}
	values := x.OptPartValues()
	if values == nil {
		part.MaxValue = true // a RANGE partition with no VALUES clause: unbounded (MAXVALUE)
		return part, true
	}
	vn, ok := values.(*mysqlast.Node)
	if !ok {
		return Partition{}, false
	}
	switch vn.Class {
	case "PT_part_value_item_list_paren":
		// RANGE's own VALUES LESS THAN (...): a single scalar (plain RANGE) or a value
		// tuple, one literal per column (RANGE COLUMNS) -- part_func_max folds straight to
		// this class (measured against the grammar's own shapes.go), not wrapped in
		// anything naming it as RANGE's.
		items := list(vn.Arg("values"))
		texts := make([]string, len(items))
		for i, el := range items {
			item, ok := el.(*mysqlast.Node)
			if !ok {
				return Partition{}, false
			}
			switch item.Class {
			case "PT_part_value_item_expr":
				texts[i] = s.exprText(item.Arg("expr"))
			case "PT_part_value_item_max":
				texts[i] = "MAXVALUE"
			default:
				return Partition{}, false
			}
		}
		switch {
		case len(texts) == 1 && !columns && texts[0] == "MAXVALUE":
			part.MaxValue = true
		case len(texts) == 1 && !columns:
			part.Bound = texts[0]
		case columns:
			// RANGE COLUMNS spells a single-column bound the same way a plain RANGE does
			// (measured), so len(texts) == 1 is not special-cased away from here.
			part.Bound = strings.Join(texts, ", ")
		default:
			return Partition{}, false // a plain RANGE never carries more than one value
		}
	case "PT_part_values_in_item":
		// LIST's own single-row VALUES IN (v1, v2, ...): the flat OR-set a plain LIST
		// spells; never COLUMNS' own (see PT_part_values_in_list below).
		if columns {
			return Partition{}, false
		}
		ix, _ := mysqlast.AsPTPartValuesInItem(vn)
		inner, ok := ix.Item().(*mysqlast.Node)
		if !ok || inner.Class != "PT_part_value_item_list_paren" {
			return Partition{}, false
		}
		var vals []string
		for _, el := range list(inner.Arg("values")) {
			item, ok := el.(*mysqlast.Node)
			if !ok || item.Class != "PT_part_value_item_expr" {
				return Partition{}, false // MAXVALUE inside a LIST partition, or anything else
			}
			vals = append(vals, s.exprText(item.Arg("expr")))
		}
		part.Bound = strings.Join(vals, ", ")
	case "PT_part_values_in_list":
		// LIST COLUMNS' own value list: one or more rows, each its own value tuple
		// (`(a,b), (c,d)`) -- always this class, even for a single row (measured: MySQL
		// refuses the bare, unwrapped form its plain sibling accepts, "Inconsistency in
		// usage of column lists for partitioning").
		if !columns {
			return Partition{}, false
		}
		lx, _ := mysqlast.AsPTPartValuesInList(vn)
		var rows []string
		for _, row := range list(lx.List()) {
			rn, ok := row.(*mysqlast.Node)
			if !ok || rn.Class != "PT_part_value_item_list_paren" {
				return Partition{}, false
			}
			var vals []string
			for _, el := range list(rn.Arg("values")) {
				item, ok := el.(*mysqlast.Node)
				if !ok || item.Class != "PT_part_value_item_expr" {
					return Partition{}, false // MAXVALUE inside a LIST partition, or anything else
				}
				vals = append(vals, s.exprText(item.Arg("expr")))
			}
			rows = append(rows, "("+strings.Join(vals, ", ")+")")
		}
		part.Bound = strings.Join(rows, ", ")
	default:
		return Partition{}, false
	}
	return part, true
}

// exprText is v's own text, verbatim from the source: a partitioning expression or a
// partition boundary literal, neither of which this package evaluates -- only compares
// and reproduces as written, the same way a CHECK constraint's expression is kept as Text
// rather than a value this package understands (schema.Check's own doc comment).
func (s *Schema) exprText(v mysqlast.Value) string {
	n, ok := v.(*mysqlast.Node)
	if !ok || n.Start < 0 || n.End > len(s.cur) || n.Start >= n.End {
		return ""
	}
	return s.cur[n.Start:n.End]
}

// closeVersionComment extends [start, end) over the ` */` that closes a versioned comment
// the span opened: SHOW CREATE TABLE writes `/*!80023 INVISIBLE */` and the parser reads
// the comment's body as code, so a node's span ends before the closing `*/` (measured: a
// MODIFY COLUMN from such a text is a syntax error).
func closeVersionComment(sql string, start, end int) int {
	text := sql[start:end]
	if strings.Count(text, "/*") <= strings.Count(text, "*/") {
		return end
	}
	if i := strings.Index(sql[end:], "*/"); i >= 0 {
		return end + i + 2
	}
	return end
}

// tableElement applies one element of a CREATE TABLE body or an ADD of ALTER TABLE.
func (s *Schema) tableElement(t *Table, el mysqlast.Value, at func(mysqlast.Value) int) {
	n, ok := el.(*mysqlast.Node)
	if !ok {
		s.problem(at(el), "table element not understood: %s", mysqlast.Sprint(el))
		return
	}
	text := ""
	if n.Start >= 0 && n.End <= len(s.cur) && n.Start < n.End {
		text = s.cur[n.Start:closeVersionComment(s.cur, n.Start, n.End)]
	}
	switch n.Class {
	case "PT_column_def":
		x, _ := mysqlast.AsPTColumnDef(n)
		col, keys, fk, check := s.column(str(x.FieldIdent()), x.FieldDef(), at)
		col.Text = text
		t.Columns = append(t.Columns, col)
		t.Keys = append(t.Keys, keys...)
		if fk != nil {
			t.ForeignKeys = append(t.ForeignKeys, fk)
		}
		if check != nil {
			t.Checks = append(t.Checks, check)
		}
		if x.OptColumnConstraint() != nil {
			s.problem(at(n), "column %s: inline REFERENCES is parsed by MySQL but not enforced; declare a FOREIGN KEY", col.Name)
		}
	case "PT_inline_index_definition":
		x, _ := mysqlast.AsPTInlineIndexDefinition(n)
		k := &Key{Name: str(x.Name()), Kind: keyKind(str(x.TypePar())), Text: text}
		for _, p := range list(x.Cols()) {
			k.Parts = append(k.Parts, keyPart(p))
		}
		s.indexOptions(k, list(x.Options()))
		t.addKey(k)
	case "PT_foreign_key_definition":
		x, _ := mysqlast.AsPTForeignKeyDefinition(n)
		fk := &ForeignKey{Name: str(x.ConstraintName()), RefTable: s.tableKey(tableName(x.ReferencedTable())),
			OnDelete: fkOption(str(x.FkDeleteOpt())), OnUpdate: fkOption(str(x.FkUpdateOpt())), Text: text}
		if fk.Name == "" {
			fk.Name = str(x.KeyName())
		}
		for _, p := range list(x.Columns()) {
			fk.Columns = append(fk.Columns, keyPart(p).Column)
		}
		for _, p := range list(x.RefList()) {
			fk.RefColumns = append(fk.RefColumns, keyPart(p).Column)
		}
		t.ForeignKeys = append(t.ForeignKeys, fk)
	case "PT_check_constraint":
		x, _ := mysqlast.AsPTCheckConstraint(n)
		t.Checks = append(t.Checks, &Check{Name: str(x.Name()), Expr: x.Expr(), Enforced: !isFalse(x.IsEnforced()), Text: text})
		if fn, ok := s.exprCallsStoredFunction(x.Expr()); ok {
			s.problem(at(n), "An expression of a check constraint '%s' contains disallowed function: %s", str(x.Name()), fn)
		}
	default:
		s.problem(at(n), "table element not understood: %s", n.Class)
	}
}

// exprCallsStoredFunction walks v (a generated column's, a column DEFAULT's, or a CHECK
// constraint's own expression) looking for a call to a schema-declared FUNCTION anywhere
// inside it, and returns its name when found. Measured on mysqld 8.4: a stored FUNCTION in
// any of those three positions is refused at CREATE TABLE time regardless of whether the
// function itself is DETERMINISTIC (3763 for a generated column, 3770 for a column DEFAULT
// expression, 3814 for a CHECK constraint) -- unlike a routine's own body, where only a
// non-deterministic construct matters, here the position itself disallows every stored
// function outright.
func (s *Schema) exprCallsStoredFunction(v mysqlast.Value) (string, bool) {
	switch x := v.(type) {
	case *mysqlast.Node:
		switch x.Class {
		case "PTI_function_call_generic_2d":
			if r := s.RoutineOf(Function, str(x.Arg("func"))); r != nil {
				return r.Name, true
			}
		case "PTI_function_call_generic_ident_sys":
			if r := s.RoutineOf(Function, str(x.Arg("ident"))); r != nil {
				return r.Name, true
			}
		}
		for _, a := range x.Args {
			if fn, ok := s.exprCallsStoredFunction(a); ok {
				return fn, true
			}
		}
	case mysqlast.List:
		for _, e := range x {
			if fn, ok := s.exprCallsStoredFunction(e); ok {
				return fn, true
			}
		}
	}
	return "", false
}

// column reads a column definition: the type, then the attributes, some of which are keys
// or constraints of the table.
func (s *Schema) column(name string, fieldDef mysqlast.Value, at func(mysqlast.Value) int) (*Column, []*Key, *ForeignKey, *Check) {
	col := &Column{Name: name}
	var keys []*Key
	var check *Check
	fd, ok := fieldDef.(*mysqlast.Node)
	if !ok {
		s.problem(at(fieldDef), "column %s: definition not understood", name)
		return col, nil, nil, nil
	}
	var attrs mysqlast.Value
	switch fd.Class {
	case "PT_field_def":
		x, _ := mysqlast.AsPTFieldDef(fd)
		col.Type = s.typeOf(x.TypeNode(), at)
		attrs = x.OptAttrs()
	case "PT_generated_field_def":
		col.Type = s.typeOf(fd.Arg("type_node"), at)
		col.Generated = fd.Arg("expr")
		col.Stored = str(fd.Arg("virtual_or_stored")) == "Virtual_or_stored::STORED"
		attrs = fd.Arg("opt_attrs")
		if fn, ok := s.exprCallsStoredFunction(col.Generated); ok {
			s.problem(at(fd), "Expression of generated column '%s' contains a disallowed function: `%s`", name, fn)
		}
	default:
		s.problem(at(fd), "column %s: %s not understood", name, fd.Class)
	}
	for _, a := range list(attrs) {
		an, ok := a.(*mysqlast.Node)
		if !ok {
			continue
		}
		switch an.Class {
		case "PT_not_null_column_attr":
			col.NotNull = true
		case "PT_null_column_attr":
			col.NotNull = false
		case "PT_default_column_attr":
			col.Default = an.Arg("item")
		case "PT_generated_default_val_column_attr":
			col.Default = an.Arg("expr")
			if fn, ok := s.exprCallsStoredFunction(col.Default); ok {
				s.problem(at(an), "Default value expression of column '%s' contains a disallowed function (%s)", name, fn)
			}
		case "PT_on_update_column_attr":
			col.OnUpdate = true
		case "PT_auto_increment_column_attr":
			col.AutoIncrement = true
		case "PT_serial_default_value_column_attr": // SERIAL DEFAULT VALUE = NOT NULL AUTO_INCREMENT UNIQUE
			col.NotNull, col.AutoIncrement = true, true
			keys = append(keys, &Key{Kind: Unique, Parts: []KeyPart{{Column: name}}})
		case "PT_primary_key_column_attr":
			col.NotNull = true
			keys = append(keys, &Key{Name: "PRIMARY", Kind: Primary, Parts: []KeyPart{{Column: name}}})
		case "PT_unique_key_column_attr":
			keys = append(keys, &Key{Kind: Unique, Parts: []KeyPart{{Column: name}}})
		case "PT_comment_column_attr":
			col.Comment = str(an.Arg("comment"))
		case "PT_collate_column_attr":
			col.Collation = str(an.Arg("collation"))
		case "PT_check_constraint_column_attr":
			check = &Check{Name: str(an.Arg("name")), Expr: an.Arg("expr"), Enforced: !isFalse(an.Arg("enforced"))}
			if fn, ok := s.exprCallsStoredFunction(check.Expr); ok {
				s.problem(at(an), "An expression of a check constraint '%s' contains disallowed function: %s", name, fn)
			}
		case "PT_column_visibility_attr":
			col.Invisible = isFalse(an.Arg("is_visible"))
		case "PT_srid_column_attr":
			if v, ok := an.Arg("srid").(mysqlast.Number); ok {
				col.Type.Srid = int(v)
			}
		case "PT_column_format_column_attr", "PT_storage_media_column_attr", "PT_secondary_column_attr", "PT_constraint_enforcement_attr":
			// storage details, no shape
		default:
			s.problem(at(an), "column %s: attribute %s not understood", name, an.Class)
		}
	}
	if col.Collation != "" {
		// col.Type.Charset carries no information a collation does not already (a
		// collation names its charset uniquely in MySQL), and whether the raw SQL spells
		// CHARACTER SET explicitly on the type alongside a column-level COLLATE is not
		// stable: an ENUM / SET column under a table with its own DEFAULT CHARSET /
		// COLLATE freshly created from declarative SQL omits it, but SHOW CREATE TABLE
		// read back from an existing table (canonicalizing a second time) always spells
		// it (measured against mysqld 8.4) -- canonicalizing would otherwise not be
		// idempotent, and a plan comparing the two spellings of the same column (Type is
		// what ColumnProps' "type" compares) would see one as changed forever.
		col.Type.Charset = ""
	}
	return col, keys, nil, check
}

func (s *Schema) tableOptions(t *Table, opts []mysqlast.Value, at func(mysqlast.Value) int) {
	for _, o := range opts {
		n, ok := o.(*mysqlast.Node)
		if !ok {
			continue
		}
		switch n.Class {
		case "PT_create_table_engine_option":
			t.Engine = str(n.Arg("engine"))
		case "PT_create_table_default_charset":
			t.Charset = str(n.Arg("value"))
		case "PT_create_table_default_collation":
			t.Collation = str(n.Arg("value"))
		case "PT_create_commen_option":
			t.Comment = str(n.Arg("value"))
		case "PT_create_auto_increment_option":
			t.AutoIncrementStart = str(n.Arg("value"))
		case "PT_create_row_format_option":
			t.RowFormat = rowFormat(str(n.Arg("value")))
		}
		// every other option (STATS_*, ...) has no shape
	}
}

// rowFormat spells a row_types constant (ROW_TYPE_DYNAMIC, ...) the way ROW_FORMAT=... in a
// CREATE / ALTER TABLE does; ROW_TYPE_DEFAULT is the server's own default, kept as "" the
// same way an undeclared option is (SHOW CREATE TABLE never writes ROW_FORMAT=DEFAULT).
func rowFormat(s string) string {
	switch s {
	case "ROW_TYPE_FIXED":
		return "FIXED"
	case "ROW_TYPE_DYNAMIC":
		return "DYNAMIC"
	case "ROW_TYPE_COMPRESSED":
		return "COMPRESSED"
	case "ROW_TYPE_REDUNDANT":
		return "REDUNDANT"
	case "ROW_TYPE_COMPACT":
		return "COMPACT"
	}
	return ""
}

func (s *Schema) indexOptions(k *Key, opts []mysqlast.Value) {
	for _, o := range opts {
		n, ok := o.(*mysqlast.Node)
		if !ok {
			continue
		}
		switch n.Class {
		case "PT_index_comment":
			k.Comment = str(n.Arg("option_value"))
		case "PT_index_visibility":
			k.Invisible = isFalse(n.Arg("option_value"))
		}
	}
}

func keyPart(v mysqlast.Value) KeyPart {
	n, ok := v.(*mysqlast.Node)
	if !ok {
		return KeyPart{Column: str(v)}
	}
	p := KeyPart{Column: str(n.Arg("column_name")), Expr: n.Arg("expression"), Desc: str(n.Arg("order")) == "ORDER_DESC"}
	if l, ok := n.Arg("prefix_length").(mysqlast.Number); ok {
		p.Length = int(l)
	}
	return p
}

func keyKind(s string) KeyKind {
	switch s {
	case "KEYTYPE_PRIMARY":
		return Primary
	case "KEYTYPE_UNIQUE":
		return Unique
	case "KEYTYPE_FULLTEXT":
		return Fulltext
	case "KEYTYPE_SPATIAL":
		return Spatial
	}
	return Index
}

func fkOption(s string) string {
	switch s {
	case "FK_OPTION_CASCADE":
		return "CASCADE"
	case "FK_OPTION_SET_NULL":
		return "SET NULL"
	case "FK_OPTION_RESTRICT":
		return "RESTRICT"
	case "FK_OPTION_NO_ACTION":
		return "NO ACTION"
	case "FK_OPTION_DEFAULT":
		return "SET DEFAULT"
	}
	return ""
}

// addKey appends a key, naming it MySQL's way when unnamed: the first column's name, with
// _2, _3 ... on collision; PRIMARY replaces any existing primary key.
func (t *Table) addKey(k *Key) {
	if k.Kind == Primary {
		k.Name = "PRIMARY"
		for _, p := range k.Parts {
			if c := t.Column(p.Column); c != nil {
				c.NotNull = true
			}
		}
	}
	if k.Name == "" && len(k.Parts) > 0 {
		base := k.Parts[0].Column
		if base == "" {
			base = "functional_index"
		}
		k.Name = base
		for i := 2; t.key(k.Name) != nil; i++ {
			k.Name = base + "_" + strconv.Itoa(i)
		}
	}
	t.Keys = append(t.Keys, k)
}

func (t *Table) key(name string) *Key {
	for _, k := range t.Keys {
		if strings.EqualFold(k.Name, name) {
			return k
		}
	}
	return nil
}

func (t *Table) dropKey(name string) bool {
	for i, k := range t.Keys {
		if strings.EqualFold(k.Name, name) {
			t.Keys = append(t.Keys[:i], t.Keys[i+1:]...)
			return true
		}
	}
	return false
}

func copyTable(dst, src *Table) {
	for _, c := range src.Columns {
		cc := *c
		dst.Columns = append(dst.Columns, &cc)
	}
	for _, k := range src.Keys {
		kk := *k
		dst.Keys = append(dst.Keys, &kk)
	}
	for _, c := range src.Checks {
		cc := *c
		dst.Checks = append(dst.Checks, &cc)
	}
	dst.Engine, dst.Charset, dst.Collation, dst.RowFormat = src.Engine, src.Charset, src.Collation, src.RowFormat
	// LIKE copies no foreign keys, and (measured against mysqld 8.4) no partitioning either
}

// addPartitions applies an ADD PARTITION (...) def_list to t's RANGE or LIST Partitioning;
// a new definition this package does not break down (see partitionDef) is a problem, and
// leaves the table's Partitioning as it stood before the statement.
func (t *Table) addPartitions(defs []mysqlast.Value, s *Schema, at func(mysqlast.Value) int) {
	if t.Partitioning == nil || (t.Partitioning.Kind != "RANGE" && t.Partitioning.Kind != "LIST") {
		return
	}
	var added []Partition
	for _, el := range defs {
		pd, ok := el.(*mysqlast.Node)
		if !ok {
			s.problem(at(el), "ADD PARTITION: definition not understood")
			return
		}
		part, ok := s.partitionDef(pd, t.Partitioning.Columns)
		if !ok {
			s.problem(at(el), "ADD PARTITION: definition not understood")
			return
		}
		added = append(added, part)
	}
	t.Partitioning.Parts = append(t.Partitioning.Parts, added...)
}

// --- CREATE INDEX / VIEW, DROP, RENAME -------------------------------------------------

func (s *Schema) createIndex(n *mysqlast.Node, st mysqlparse.Statement, at func(mysqlast.Value) int) {
	x, _ := mysqlast.AsPTCreateIndexStmt(n)
	t := s.needTable(tableName(x.TableIdent()), at(n))
	if t == nil {
		return
	}
	k := &Key{Name: str(x.Name()), Kind: keyKind(str(x.TypePar()))}
	for _, p := range list(x.Cols()) {
		k.Parts = append(k.Parts, keyPart(p))
	}
	s.indexOptions(k, list(x.Options()))
	if t.key(k.Name) != nil {
		s.problem(at(n), "CREATE INDEX %s: duplicate key name on %s", k.Name, t.Name)
		return
	}
	t.addKey(k)
	t.Alters = append(t.Alters, st.SQL)
}

func (s *Schema) createView(n *mysqlast.Node, st mysqlparse.Statement) {
	name := s.tableKey(tableName(n.Arg("name")))
	v := &View{Name: name, Query: n.Arg("query"), Definition: st.SQL,
		Algorithm: strings.TrimPrefix(str(n.Arg("algorithm")), "VIEW_ALGORITHM_"), CheckOption: strings.TrimPrefix(str(n.Arg("check_option")), "VIEW_CHECK_")}
	for _, c := range list(n.Arg("column_list")) {
		v.Columns = append(v.Columns, str(c))
	}
	s.viewDirectives(v, st.SQL, st.Offset)
	if old := s.View(name); old != nil {
		if n.Arg("replace") == nil {
			s.problem(st.Offset, "CREATE VIEW %s: view already exists", name)
			return
		}
		*old = *v
		return
	}
	if s.Table(name) != nil {
		s.problem(st.Offset, "CREATE VIEW %s: a table of that name exists", name)
		return
	}
	s.Views = append(s.Views, v)
}

// dropTable drops the table, and with it every trigger declared on it (a trigger has no
// existence apart from its table on the server: DROP TABLE drops them silently, without a
// DROP TRIGGER of their own).
func (s *Schema) dropTable(name string) {
	for i, t := range s.Tables {
		if s.sameTable(t.Name, name) {
			s.Tables = append(s.Tables[:i], s.Tables[i+1:]...)
			var kept []*Trigger
			for _, trg := range s.Triggers {
				if !s.sameTable(trg.Table, t.Name) {
					kept = append(kept, trg)
				}
			}
			s.Triggers = kept
			return
		}
	}
}

func (s *Schema) dropView(name string) {
	for i, v := range s.Views {
		if s.sameTable(v.Name, name) {
			s.Views = append(s.Views[:i], s.Views[i+1:]...)
			return
		}
	}
}

func (s *Schema) renameTables(n *mysqlast.Node, at func(mysqlast.Value) int) {
	pairs := []*mysqlast.Node{n}
	if n.Class == "table_to_table_list" {
		pairs = nil
		for _, p := range n.Args {
			if pn, ok := p.(*mysqlast.Node); ok {
				pairs = append(pairs, pn)
			}
		}
	}
	for _, p := range pairs {
		if len(p.Args) != 2 {
			s.problem(at(p), "RENAME TABLE: pair not understood")
			continue
		}
		from, to := tableName(p.Args[0]), s.tableKey(tableName(p.Args[1]))
		if t := s.Table(from); t != nil {
			for _, trg := range s.Triggers {
				if s.sameTable(trg.Table, t.Name) {
					trg.Table = to
				}
			}
			t.Name = to
		} else if v := s.View(from); v != nil {
			v.Name = to
		} else {
			s.problem(at(p), "RENAME TABLE %s: no such table", from)
		}
	}
}

// needTable returns the table or records a problem.
func (s *Schema) needTable(name string, pos int) *Table {
	t := s.Table(name)
	if t == nil {
		s.problem(pos, "no such table: %s", name)
	}
	return t
}

// --- ALTER TABLE ---------------------------------------------------------------------

func (s *Schema) alterTable(n *mysqlast.Node, st mysqlparse.Statement, at func(mysqlast.Value) int) {
	t := s.needTable(tableName(n.Arg("table_name")), at(n))
	if t == nil {
		return
	}
	t.Alters = append(t.Alters, st.SQL)
	actions := list(n.Arg("opt_actions"))
	if a := n.Arg("action"); a != nil {
		actions = append(actions, a)
	}
	for _, a := range actions {
		an, ok := a.(*mysqlast.Node)
		if !ok {
			continue
		}
		switch an.Class {
		case "PT_alter_table_add_column":
			x, _ := mysqlast.AsPTAlterTableAddColumn(an)
			col, keys, _, check := s.column(str(x.FieldIdent()), x.FieldDef(), at)
			if t.Column(col.Name) != nil {
				s.problem(at(an), "ADD COLUMN %s: duplicate column", col.Name)
				continue
			}
			t.insertColumn(col, x.OptPlace())
			t.Keys = append(t.Keys, keys...)
			if check != nil {
				t.Checks = append(t.Checks, check)
			}
		case "PT_alter_table_add_columns":
			for _, el := range list(an.Arg("columns")) {
				s.tableElement(t, el, at)
			}
		case "PT_alter_table_add_constraint":
			s.tableElement(t, an.Arg("constraint"), at)
		case "PT_alter_table_drop_column":
			if !t.dropColumn(str(an.Arg("name"))) {
				s.problem(at(an), "DROP COLUMN %s: no such column", str(an.Arg("name")))
			}
		case "PT_alter_table_change_column":
			x, _ := mysqlast.AsPTAlterTableChangeColumn(an)
			old := str(x.OldName())
			if old == "" {
				old = str(x.Name()) // MODIFY: one name
			}
			c := t.Column(old)
			if c == nil {
				s.problem(at(an), "CHANGE / MODIFY %s: no such column", old)
				continue
			}
			newName := str(x.NewName())
			if newName == "" {
				newName = old
			}
			col, keys, _, check := s.column(newName, x.FieldDef(), at)
			*c = *col
			t.Keys = append(t.Keys, keys...)
			if check != nil {
				t.Checks = append(t.Checks, check)
			}
			t.renameColumnRefs(old, newName)
			if x.OptPlace() != nil {
				t.dropColumn(newName)
				t.insertColumn(c, x.OptPlace())
			}
		case "PT_alter_table_rename_column":
			from, to := str(an.Arg("from")), str(an.Arg("to"))
			c := t.Column(from)
			if c == nil {
				s.problem(at(an), "RENAME COLUMN %s: no such column", from)
				continue
			}
			c.Name = to
			t.renameColumnRefs(from, to)
		case "PT_alter_table_rename":
			t.Name = tableName(an.Arg("ident"))
		case "PT_alter_table_drop_key":
			name := str(an.Arg("name"))
			if name == "primary_key_name" {
				name = "PRIMARY"
			}
			if !t.dropKey(name) {
				s.problem(at(an), "DROP INDEX %s: no such index", name)
			}
		case "PT_alter_table_drop_foreign_key", "PT_alter_table_drop_constraint", "PT_alter_table_drop_check_constraint":
			name := str(an.Arg("name"))
			if !t.dropConstraint(name) {
				s.problem(at(an), "DROP %s: no such constraint", name)
			}
		case "PT_alter_table_rename_key":
			if k := t.key(str(an.Arg("from"))); k != nil {
				k.Name = str(an.Arg("to"))
			} else {
				s.problem(at(an), "RENAME INDEX %s: no such index", str(an.Arg("from")))
			}
		case "PT_alter_table_set_default":
			if c := t.Column(str(an.Arg("col_name"))); c != nil {
				c.Default = an.Arg("opt_default_expr")
			} else {
				s.problem(at(an), "ALTER COLUMN %s: no such column", str(an.Arg("col_name")))
			}
		case "PT_alter_table_column_visibility":
			if c := t.Column(str(an.Arg("col_name"))); c != nil {
				c.Invisible = isFalse(an.Arg("is_visible"))
			}
		case "PT_alter_table_index_visible":
			if k := t.key(str(an.Arg("name"))); k != nil {
				k.Invisible = isFalse(an.Arg("visible"))
			}
		case "PT_alter_table_enforce_check_constraint":
			for _, c := range t.Checks {
				if strings.EqualFold(c.Name, str(an.Arg("name"))) {
					c.Enforced = !isFalse(an.Arg("is_enforced"))
				}
			}
		case "PT_alter_table_convert_to_charset":
			t.Charset = str(an.Arg("charset"))
			t.Collation = str(an.Arg("opt_collation"))
		case "PT_create_table_engine_option", "PT_create_table_default_charset", "PT_create_table_default_collation", "PT_create_commen_option", "PT_create_row_format_option":
			s.tableOptions(t, []mysqlast.Value{an}, at)
		case "PT_alter_table_partition_by":
			x, _ := mysqlast.AsPTAlterTablePartitionBy(an)
			t.Partitioning = s.partitioning(x.Partition(), at)
		case "PT_alter_table_remove_partitioning":
			t.Partitioning = nil
		case "PT_alter_table_add_partition_def_list":
			x, _ := mysqlast.AsPTAlterTableAddPartitionDefList(an)
			t.addPartitions(list(x.DefList()), s, at)
		case "PT_alter_table_add_partition_num":
			x, _ := mysqlast.AsPTAlterTableAddPartitionNum(an)
			if t.Partitioning != nil && (t.Partitioning.Kind == "HASH" || t.Partitioning.Kind == "KEY") {
				t.Partitioning.Num += numOf(x.NumParts())
			}
		case "PT_alter_table_coalesce_partition":
			x, _ := mysqlast.AsPTAlterTableCoalescePartition(an)
			if t.Partitioning != nil && (t.Partitioning.Kind == "HASH" || t.Partitioning.Kind == "KEY") {
				t.Partitioning.Num -= numOf(x.NumParts())
			}
		case "PT_alter_table_drop_partition":
			x, _ := mysqlast.AsPTAlterTableDropPartition(an)
			if t.Partitioning != nil {
				names := map[string]bool{}
				for _, nm := range list(x.Partitions()) {
					names[str(nm)] = true
				}
				var kept []Partition
				for _, part := range t.Partitioning.Parts {
					if !names[part.Name] {
						kept = append(kept, part)
					}
				}
				t.Partitioning.Parts = kept
			}
		case "PT_alter_table_reorganize_partition_into":
			x, _ := mysqlast.AsPTAlterTableReorganizePartitionInto(an)
			if t.Partitioning != nil && (t.Partitioning.Kind == "RANGE" || t.Partitioning.Kind == "LIST") {
				names := map[string]bool{}
				for _, nm := range list(x.PartitionNames()) {
					names[str(nm)] = true
				}
				var into []Partition
				ok := true
				for _, el := range list(x.Into()) {
					pd, isNode := el.(*mysqlast.Node)
					if !isNode {
						ok = false
						break
					}
					part, partOK := s.partitionDef(pd, t.Partitioning.Columns)
					if !partOK {
						ok = false
						break
					}
					into = append(into, part)
				}
				if !ok {
					s.problem(at(an), "REORGANIZE PARTITION: definition not understood")
				} else {
					var kept []Partition
					done := false
					for _, part := range t.Partitioning.Parts {
						if names[part.Name] {
							if !done {
								kept = append(kept, into...)
								done = true
							}
							continue
						}
						kept = append(kept, part)
					}
					t.Partitioning.Parts = kept
				}
			}
		case "PT_alter_table_order", "PT_alter_table_force", "PT_alter_table_enable_keys", "PT_alter_table_set_default_row_format",
			"PT_alter_table_discard_tablespace", "PT_alter_table_import_tablespace", "PT_alter_table_secondary_load", "PT_alter_table_secondary_unload",
			"PT_alter_table_add_partition":
			// storage-level, no shape (PT_alter_table_add_partition: measured unreachable --
			// every ADD PARTITION this grammar accepts folds to _def_list or _num instead)
		default:
			if strings.HasPrefix(an.Class, "PT_create_") { // any other table option
				continue
			}
			s.problem(at(an), "ALTER TABLE action not understood: %s", an.Class)
		}
	}
}

// insertColumn adds col at the place ALTER TABLE names: FIRST, AFTER x, or the end.
func (t *Table) insertColumn(col *Column, place mysqlast.Value) {
	switch p := place.(type) {
	case nil:
		t.Columns = append(t.Columns, col)
	case mysqlast.Const:
		if p == "first_keyword" {
			t.Columns = append([]*Column{col}, t.Columns...)
			return
		}
		t.Columns = append(t.Columns, col)
	default:
		after := str(place)
		for i, c := range t.Columns {
			if strings.EqualFold(c.Name, after) {
				t.Columns = append(t.Columns[:i+1], append([]*Column{col}, t.Columns[i+1:]...)...)
				return
			}
		}
		t.Columns = append(t.Columns, col)
	}
}

func (t *Table) dropColumn(name string) bool {
	for i, c := range t.Columns {
		if strings.EqualFold(c.Name, name) {
			t.Columns = append(t.Columns[:i], t.Columns[i+1:]...)
			// keys over the column lose the part; an emptied key goes
			var keys []*Key
			for _, k := range t.Keys {
				var parts []KeyPart
				for _, p := range k.Parts {
					if !strings.EqualFold(p.Column, name) {
						parts = append(parts, p)
					}
				}
				if len(parts) > 0 {
					k.Parts = parts
					keys = append(keys, k)
				}
			}
			t.Keys = keys
			return true
		}
	}
	return false
}

func (t *Table) dropConstraint(name string) bool {
	t.nameUnnamed() // the server's generated names are what a DROP names
	for i, fk := range t.ForeignKeys {
		if strings.EqualFold(fk.Name, name) {
			t.ForeignKeys = append(t.ForeignKeys[:i], t.ForeignKeys[i+1:]...)
			return true
		}
	}
	for i, c := range t.Checks {
		if strings.EqualFold(c.Name, name) {
			t.Checks = append(t.Checks[:i], t.Checks[i+1:]...)
			return true
		}
	}
	return t.dropKey(name)
}

func (t *Table) renameColumnRefs(from, to string) {
	for _, k := range t.Keys {
		for i := range k.Parts {
			if strings.EqualFold(k.Parts[i].Column, from) {
				k.Parts[i].Column = to
			}
		}
	}
	for _, fk := range t.ForeignKeys {
		for i := range fk.Columns {
			if strings.EqualFold(fk.Columns[i], from) {
				fk.Columns[i] = to
			}
		}
	}
}

// --- TRIGGER / PROCEDURE / FUNCTION ----------------------------------------------------

// createTrigger applies a CREATE TRIGGER (trigger_tail). Its args, in the grammar's order
// once terminals drop out: if_not_exists, sp_name, trg_action_time, trg_event, table_ident,
// trigger_follows_precedes_clause (a Struct), the body.
func (s *Schema) createTrigger(n *mysqlast.Node, st mysqlparse.Statement, at func(mysqlast.Value) int) {
	if len(n.Args) != 7 {
		// defensive: trigger_tail's own grammar always builds all 7 fields.
		s.problem(at(n), "CREATE TRIGGER: statement not understood")
		return
	}
	name := spName(n.Args[1])
	if s.Trigger(name) != nil {
		if isTrue(n.Args[0]) { // IF NOT EXISTS
			return
		}
		s.problem(at(n), "CREATE TRIGGER %s: trigger already exists", name)
		return
	}
	table := s.tableKey(tableName(n.Args[4]))
	if s.Table(table) == nil {
		s.problem(at(n), "CREATE TRIGGER %s: no such table: %s", name, table)
		return
	}
	trg := &Trigger{
		Name:       name,
		Table:      table,
		Timing:     strings.TrimPrefix(str(n.Args[2]), "TRG_ACTION_"),
		Event:      strings.TrimPrefix(str(n.Args[3]), "TRG_EVENT_"),
		Body:       n.Args[6],
		Definition: st.SQL,
	}
	if ord, ok := n.Args[5].(*mysqlast.Struct); ok {
		if oc := str(ord.Fields["ordering_clause"]); oc != "" && oc != "TRG_ORDER_NONE" {
			trg.OrderClause = strings.TrimPrefix(oc, "TRG_ORDER_")
			trg.OrderTrigger = str(ord.Fields["anchor_trigger_name"])
			// the server rejects FOLLOWS/PRECEDES a trigger that does not exist (for that
			// action time and event type) at CREATE TIME, 3011, measured against mysqld
			if s.Trigger(trg.OrderTrigger) == nil {
				s.problem(at(n), "CREATE TRIGGER %s: %s %s: no such trigger", name, trg.OrderClause, trg.OrderTrigger)
				return
			}
		}
	}
	if msg := bodyRefusalProblem(trg.Body, true); msg != "" {
		// the server refuses this CREATE TRIGGER outright (1336 dynamic SQL, 1331 a
		// duplicate variable): a problem the same way an unknown table is, but the
		// trigger is still loaded -- analyze.AnalyzeTrigger reads the very same body and
		// raises the matching error for whatever statement's own check reaches it there.
		s.problem(at(n), "CREATE TRIGGER %s: %s", name, msg)
	}
	trg.Directives, _ = s.spDirectives("trigger", name, st.SQL, st.Offset)
	s.Triggers = append(s.Triggers, trg)
}

// createEvent applies a CREATE EVENT (event_tail: if_not_exists, sp_name, ev_schedule,
// on_completion, status, comment, body). The server checks nothing of the body at CREATE
// time (measured: a DELETE from a table that does not exist is accepted, and fails at every
// run), only a RETURN (1313); the body's own reading is analyze.AnalyzeEvent's.
func (s *Schema) createEvent(n *mysqlast.Node, st mysqlparse.Statement, at func(mysqlast.Value) int) {
	if len(n.Args) != 7 {
		// defensive: event_tail's own grammar always builds all 7 fields.
		s.problem(at(n), "CREATE EVENT: statement not understood")
		return
	}
	name := spName(n.Args[1])
	if s.Event(name) != nil {
		if isTrue(n.Args[0]) { // IF NOT EXISTS
			return
		}
		s.problem(at(n), "CREATE EVENT %s: event already exists", name)
		return
	}
	e := &Event{Name: name, Completion: "NOT PRESERVE", Status: "ENABLE", Definition: st.SQL}
	e.setSchedule(n.Args[2], st.SQL)
	e.setOptions(n.Args[3], n.Args[4], n.Args[5])
	e.setBody(n.Args[6], st.SQL)
	s.Events = append(s.Events, e)
}

// alterEvent applies an ALTER EVENT (alter_event_stmt: definer, sp_name, schedule and/or
// completion, RENAME TO, status, comment, DO body): each clause written replaces that part
// of the event, the rest stays (measured: SHOW CREATE EVENT after `ALTER EVENT e ON SCHEDULE
// EVERY 2 DAY` keeps the STARTS, completion, status and body as they were). The Definition
// stays the CREATE's text: the canonical form, which diff and apply read, comes from the
// server's own SHOW CREATE EVENT of the altered event.
func (s *Schema) alterEvent(n *mysqlast.Node, st mysqlparse.Statement, at func(mysqlast.Value) int) {
	if len(n.Args) != 7 {
		// defensive: alter_event_stmt's own grammar always builds all 7 fields.
		s.problem(at(n), "ALTER EVENT: statement not understood")
		return
	}
	name := spName(n.Args[1])
	e := s.Event(name)
	if e == nil {
		s.problem(at(n), "ALTER EVENT %s: no such event", name)
		return
	}
	if sc, ok := n.Args[2].(*mysqlast.Node); ok && sc.Class == "ev_alter_schedule" {
		if sch := sc.Arg("schedule"); sch != nil {
			e.At, e.Every, e.Starts, e.Ends = "", "", "", ""
			e.AtLiteral, e.StartsLiteral, e.EndsLiteral = false, false, false
			e.setSchedule(sch, st.SQL)
		}
		if c := str(sc.Arg("completion")); c != "" {
			e.Completion = c
		}
	}
	if newName := spName(n.Args[3]); newName != "" && newName != "0" {
		if s.Event(newName) != nil && !strings.EqualFold(newName, name) {
			s.problem(at(n), "ALTER EVENT %s RENAME TO %s: event already exists", name, newName)
			return
		}
		e.Name = newName
	}
	var comment mysqlast.Value = mysqlast.Const("0")
	if str(n.Args[5]) != "0" {
		comment = n.Args[5]
	}
	e.setOptions(mysqlast.Const("0"), n.Args[4], comment)
	if str(n.Args[6]) != "0" && n.Args[6] != nil {
		e.setBody(n.Args[6], st.SQL)
	}
}

// exprText is the text of an expression node in sql, trimmed (str(v) for a non-node).
func exprText(v mysqlast.Value, sql string) string {
	if x, ok := v.(*mysqlast.Node); ok && x.End > x.Start && x.End <= len(sql) {
		return strings.TrimSpace(sql[x.Start:x.End])
	}
	return str(v)
}

// isLiteral reports a string literal expression (a time the server stores as written).
func isLiteral(v mysqlast.Value) bool {
	x, ok := v.(*mysqlast.Node)
	return ok && strings.HasPrefix(x.Class, "PTI_text_literal")
}

// setSchedule reads an ev_schedule node into e.
func (e *Event) setSchedule(v mysqlast.Value, sql string) {
	sch, ok := v.(*mysqlast.Node)
	if !ok || sch.Class != "ev_schedule" {
		return
	}
	if v := sch.Arg("at"); v != nil {
		e.At, e.AtLiteral = exprText(v, sql), isLiteral(v)
	}
	if v := sch.Arg("every"); v != nil {
		e.Every = exprText(v, sql) + " " + strings.TrimPrefix(str(sch.Arg("interval")), "INTERVAL_")
	}
	if v := sch.Arg("starts"); v != nil {
		e.Starts, e.StartsLiteral = exprText(v, sql), isLiteral(v)
	}
	if v := sch.Arg("ends"); v != nil {
		e.Ends, e.EndsLiteral = exprText(v, sql), isLiteral(v)
	}
}

// setOptions reads the completion, status and comment values (each "0" or "" when not
// written, which leaves e's as it is).
func (e *Event) setOptions(completion, status, comment mysqlast.Value) {
	if c := str(completion); c != "" && c != "0" {
		e.Completion = c
	}
	if st := str(status); st != "" && st != "0" {
		e.Status = st
	}
	if c := str(comment); c != "0" {
		e.Comment = c
	}
}

// setBody reads the DO body.
func (e *Event) setBody(v mysqlast.Value, sql string) {
	e.Body = v
	e.BodyText = exprText(v, sql)
}

// dropEvent applies a DROP EVENT (1539 "Unknown event" on the server when it does not
// exist, measured).
func (s *Schema) dropEvent(name string, ifExists bool, pos int) {
	for i, e := range s.Events {
		if strings.EqualFold(e.Name, name) {
			s.Events = append(s.Events[:i], s.Events[i+1:]...)
			return
		}
	}
	if !ifExists {
		s.problem(pos, "DROP EVENT %s: no such event", name)
	}
}

// dropTrigger applies a DROP TRIGGER.
func (s *Schema) dropTrigger(name string, ifExists bool, pos int) {
	for i, t := range s.Triggers {
		if strings.EqualFold(t.Name, name) {
			s.Triggers = append(s.Triggers[:i], s.Triggers[i+1:]...)
			return
		}
	}
	if !ifExists {
		s.problem(pos, "DROP TRIGGER %s: no such trigger", name)
	}
}

// createRoutine applies a CREATE PROCEDURE (sp_tail) or CREATE FUNCTION (sf_tail). sp_tail's
// args are if_not_exists, sp_name, params, characteristics, body; sf_tail's are
// if_not_exists, sp_name, params, returns type, opt_collate (on the return type, not kept),
// characteristics, body.
func (s *Schema) createRoutine(n *mysqlast.Node, kind RoutineKind, st mysqlparse.Statement, at func(mysqlast.Value) int) {
	if kind == Function && len(n.Args) != 7 || kind == Procedure && len(n.Args) != 5 {
		// defensive: sp_tail's and sf_tail's own grammar always build every field.
		s.problem(at(n), "CREATE %s: statement not understood", kind)
		return
	}
	name := spName(n.Args[1])
	if s.routine(kind, name) != nil {
		if isTrue(n.Args[0]) { // IF NOT EXISTS
			return
		}
		s.problem(at(n), "CREATE %s %s: %s already exists", kind, name, strings.ToLower(kind.String()))
		return
	}
	r := &Routine{Name: name, Kind: kind, Params: s.paramsOf(n.Args[2], at), Definition: st.SQL, DataAccess: "CONTAINS_SQL", Security: "DEFINER"}
	if kind == Function {
		r.Returns = s.typeOf(n.Args[3], at)
		s.applyChistics(r, list(n.Args[5]))
		r.Body = n.Args[6]
	} else {
		s.applyChistics(r, list(n.Args[3]))
		r.Body = n.Args[4]
	}
	if msg := bodyRefusalProblem(r.Body, kind == Function); msg != "" {
		// the server refuses this CREATE outright (1336 dynamic SQL in a FUNCTION, 1331 a
		// duplicate variable in either kind): a problem the same way an unknown table is,
		// but the routine is still loaded -- analyze.AnalyzeRoutine reads the very same
		// body and raises the matching error wherever a statement's own check reaches it.
		s.problem(at(n), "CREATE %s %s: %s", kind, name, msg)
	}
	r.Directives, r.NotNull = s.spDirectives(strings.ToLower(kind.String()), name, st.SQL, st.Offset)
	s.Routines = append(s.Routines, r)
}

// bodyRefusalProblem walks a trigger's or routine's body for a construct the server itself
// refuses at CREATE time that createTrigger/createRoutine cannot otherwise see: dynamic SQL
// (PREPARE / EXECUTE / DEALLOCATE PREPARE) inside a trigger or FUNCTION body (1336, measured
// -- a PROCEDURE is exempt, so dynamicSQL gates it), and two DECLAREs of the same variable
// name in the same block, any kind (1331, measured). Mirrors analyze/body.go's own
// commitCheck / walkDecl duplicate check, which this package cannot import (analyze imports
// schema, not the other way around) -- so analyze.AnalyzeTrigger / AnalyzeRoutine raise the
// matching error independently when a statement's own check reaches the same body.
func bodyRefusalProblem(v mysqlast.Value, dynamicSQL bool) string {
	switch x := v.(type) {
	case mysqlast.List:
		for _, e := range x {
			if msg := bodyRefusalProblem(e, dynamicSQL); msg != "" {
				return msg
			}
		}
	case *mysqlast.Struct:
		if dynamicSQL {
			if cmd, ok := x.Fields["sql_command"].(mysqlast.Const); ok &&
				(cmd == "SQLCOM_PREPARE" || cmd == "SQLCOM_DEALLOCATE_PREPARE") {
				return "Dynamic SQL is not allowed in stored function or trigger"
			}
		}
		for _, k := range x.Order {
			if msg := bodyRefusalProblem(x.Fields[k], dynamicSQL); msg != "" {
				return msg
			}
		}
	case *mysqlast.Node:
		if dynamicSQL && x.Class == "execute" {
			return "Dynamic SQL is not allowed in stored function or trigger"
		}
		if x.Class == "sp_block_content" {
			if msg := duplicateVariableProblem(x.Args[0]); msg != "" {
				return msg
			}
		}
		for _, a := range x.Args {
			if msg := bodyRefusalProblem(a, dynamicSQL); msg != "" {
				return msg
			}
		}
	}
	return ""
}

// duplicateVariableProblem is bodyRefusalProblem's own check for one block's own
// declarations (sp_block_content's first argument): two DECLAREs of the same name,
// case-insensitively, is 1331 ("Duplicate variable: x", measured); a nested block's own
// DECLARE of the same name is ordinary shadowing, not a duplicate, so this never looks past
// decls' own top level (bodyRefusalProblem's own recursion finds a nested block's decls in
// turn, as its own sp_block_content).
//
// The same block-level rule holds for the block's other declarations, each in its own
// namespace (measured on 8.4): two DECLARE ... CONDITIONs of one name are 1332 ("Duplicate
// condition: x"), two DECLARE ... CURSORs 1333 ("Duplicate cursor: x"), and two HANDLERs
// naming the same condition value -- a SQLSTATE, an error number, one of the three
// classes, or a named condition resolved to its value -- 1413 ("Duplicate handler declared
// in the same block"); a variable and a cursor of one name coexist.
func duplicateVariableProblem(decls mysqlast.Value) string {
	names, _ := decls.(mysqlast.List)
	seen := map[string]bool{}
	conds := map[string]bool{}
	condValues := map[string]string{} // named condition -> its value's key, this block's own
	cursors := map[string]bool{}
	var handled []string // every condition value key an earlier HANDLER of the block names
	for _, d := range names {
		dn, ok := d.(*mysqlast.Node)
		if !ok {
			continue
		}
		switch dn.Class {
		case "sp_decl_var":
			for _, nm := range list(dn.Arg("names")) {
				name := str(nm)
				key := strings.ToLower(name)
				if seen[key] {
					return fmt.Sprintf("Duplicate variable: %s", name)
				}
				seen[key] = true
			}
		case "sp_decl_condition":
			name := str(dn.Arg("name"))
			key := strings.ToLower(name)
			if conds[key] {
				return fmt.Sprintf("Duplicate condition: %s", name)
			}
			conds[key] = true
			if vn, ok := dn.Arg("value").(*mysqlast.Node); ok {
				condValues[key] = conditionKey(vn, condValues)
			}
		case "sp_decl_cursor":
			name := str(dn.Arg("name"))
			key := strings.ToLower(name)
			if cursors[key] {
				return fmt.Sprintf("Duplicate cursor: %s", name)
			}
			cursors[key] = true
		case "sp_decl_handler":
			var mine []string
			for _, c := range list(dn.Arg("conditions")) {
				cn, ok := c.(*mysqlast.Node)
				if !ok {
					continue
				}
				k := conditionKey(cn, condValues)
				if k == "" {
					continue
				}
				for _, h := range handled {
					if h == k {
						return "Duplicate handler declared in the same block"
					}
				}
				mine = append(mine, k)
			}
			handled = append(handled, mine...)
		}
	}
	return ""
}

// conditionKey spells one HANDLER FOR / DECLARE CONDITION FOR value so that two values the
// server treats as the same compare equal: a SQLSTATE upper-cased, an error number, one of
// the three classes by its keyword, and a named condition by the value it was declared
// with in this block (an outer block's name resolves to nothing here, so it never collides
// -- analyze/body.go raises the exact error on the resolved chain).
func conditionKey(n *mysqlast.Node, named map[string]string) string {
	switch n.Class {
	case "sp_condition_value":
		switch x := n.Arg("_mysqlerr").(type) {
		case int:
			return fmt.Sprintf("number:%d", x)
		case mysqlast.Number:
			return fmt.Sprintf("number:%d", int(x))
		case string:
			return "value:" + strings.ToUpper(x)
		case mysqlast.Const:
			return "value:" + strings.ToUpper(string(x))
		}
	case "sp_condition_name":
		return named[strings.ToLower(str(n.Arg("name")))]
	}
	return ""
}

// paramsOf reads a routine's parameter list (a List of sp_param nodes).
func (s *Schema) paramsOf(v mysqlast.Value, at func(mysqlast.Value) int) []Param {
	var out []Param
	for _, p := range list(v) {
		pn, ok := p.(*mysqlast.Node)
		if !ok || pn.Class != "sp_param" {
			continue // defensive: a routine's own parameter list is always sp_param Nodes
		}
		out = append(out, Param{Mode: paramMode(str(pn.Arg("mode"))), Name: str(pn.Arg("name")), Type: s.typeOf(pn.Arg("type"), at)})
	}
	return out
}

func paramMode(s string) string {
	switch s {
	case "sp_variable::MODE_OUT":
		return "OUT"
	case "sp_variable::MODE_INOUT":
		return "INOUT"
	}
	return "IN"
}

// applyChistics reads a routine's characteristics (a List of the {kind, value} tags
// hooks_sp.go's sp_chistic hooks build): DETERMINISTIC, the SQL data access class, SQL
// SECURITY. COMMENT and LANGUAGE carry no field on Routine (nothing reads them yet).
func (s *Schema) applyChistics(r *Routine, items []mysqlast.Value) {
	for _, it := range items {
		cn, ok := it.(*mysqlast.Node)
		if !ok || cn.Class != "sp_chistic" {
			continue // defensive: a routine's own characteristics list is always sp_chistic Nodes
		}
		switch str(cn.Arg("kind")) {
		case "DETERMINISTIC":
			r.Deterministic = str(cn.Arg("value")) == "true"
		case "SQL_DATA_ACCESS":
			r.DataAccess = str(cn.Arg("value"))
		case "SQL_SECURITY":
			r.Security = str(cn.Arg("value"))
		}
	}
}

// dropRoutine applies a DROP PROCEDURE / DROP FUNCTION.
func (s *Schema) dropRoutine(kind RoutineKind, name string, ifExists bool, pos int) {
	for i, r := range s.Routines {
		if r.Kind == kind && strings.EqualFold(r.Name, name) {
			s.Routines = append(s.Routines[:i], s.Routines[i+1:]...)
			return
		}
	}
	if !ifExists {
		s.problem(pos, "DROP %s %s: no such %s", kind, name, strings.ToLower(kind.String()))
	}
}

// alterRoutine applies an ALTER PROCEDURE / ALTER FUNCTION: MySQL's grammar only lets it
// change characteristics (COMMENT, SQL SECURITY, ...), none of which Routine models, so it
// has no schema effect beyond requiring the routine to exist.
func (s *Schema) alterRoutine(kind RoutineKind, name string, pos int) {
	if s.routine(kind, name) == nil {
		s.problem(pos, "ALTER %s %s: no such %s", kind, name, strings.ToLower(kind.String()))
	}
}

// spName is the unqualified name of a sp_name (the schema part is dropped, as tableName
// drops a Table_ident's: sqlshape works within one database).
func spName(v mysqlast.Value) string {
	if n, ok := v.(*mysqlast.Node); ok && n.Class == "sp_name" {
		return str(n.Arg("name"))
	}
	// defensive: every caller passes a CREATE/DROP TRIGGER/PROCEDURE/FUNCTION's own
	// sp_name, always this Node class.
	return str(v)
}

// spDirectives applies the directives written above a CREATE TRIGGER / PROCEDURE / FUNCTION.
// `error <key> = <name>` is read here (kept verbatim; a later analysis stage parses it and
// resolves <key> against the routine's/trigger's own failure modes). `not null` (the same
// override postgres/schema.go's createFunction reads) is read too, but only above a
// FUNCTION: a stored function alone has a single result to mark never-NULL, the same
// reason docs/checks.md and docs/templates.md only document it there. Anything else is a
// problem.
func (s *Schema) spDirectives(kind, name, sql string, pos int) (out []string, notNull bool) {
	for _, d := range leadingDirectives(sql) {
		norm := strings.Join(strings.Fields(strings.ToLower(d)), " ")
		switch {
		case strings.HasPrefix(strings.ToLower(d), "error "):
			for _, r := range ParseRaises([]string{d}) {
				if !validErrorName(r.Name) {
					s.problem(pos, "%s %s: directive %q: %q is not a valid name (a Go identifier)", kind, name, d, r.Name)
				}
			}
			out = append(out, d)
		case norm == "not null" && kind == "function":
			notNull = true
		default:
			s.problem(pos, "%s %s: unknown directive %q", kind, name, d)
		}
	}
	return out, notNull
}

// --- values ----------------------------------------------------------------------------

// tableName is the unqualified name of a Table_ident (the schema part is dropped: sqlshape
// works within one database).
func tableName(v mysqlast.Value) string {
	if n, ok := v.(*mysqlast.Node); ok && n.Class == "Table_ident" {
		return str(n.Arg("table"))
	}
	return str(v)
}

// str renders a token, constant or string value as text; nodes and lists give "".
func str(v mysqlast.Value) string {
	switch x := v.(type) {
	case mysqlast.Token:
		if x.Value != "" || x.Kind.String() == "IDENT_QUOTED" || x.Kind.String() == "TEXT_STRING" {
			return x.Value
		}
		return x.Text
	case mysqlast.Const:
		return string(x)
	case string:
		return x
	case mysqlast.Number:
		return strconv.FormatInt(int64(x), 10)
	}
	return ""
}

func list(v mysqlast.Value) []mysqlast.Value {
	l, _ := v.(mysqlast.List)
	return l
}

// numOf is v's own integer value (a Number literal); 0 for anything else.
func numOf(v mysqlast.Value) int {
	if n, ok := v.(mysqlast.Number); ok {
		return int(n)
	}
	return 0
}

func isTrue(v mysqlast.Value) bool {
	switch x := v.(type) {
	case mysqlast.Const:
		return x == "true" || x == "1"
	case mysqlast.Number:
		return x != 0
	}
	return false
}

func isFalse(v mysqlast.Value) bool {
	switch x := v.(type) {
	case mysqlast.Const:
		return x == "false" || x == "0"
	case mysqlast.Number:
		return x == 0
	}
	return false
}
