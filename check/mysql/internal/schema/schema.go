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
	Comment     string
	Temporary   bool
	Partitioned bool
	// Definition is the CREATE TABLE text; Alters the ALTER TABLE texts applied after it.
	Definition string
	Alters     []string
	// Directives are the `-- sqlshape: ...` lines written above the CREATE, normalized:
	// the obligations x/obligation parses.
	Directives []string
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
	Name   string
	Kind   RoutineKind
	Params []Param
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
		// an event runs on the server's own schedule; no statement of the program reaches
		// it, so there is nothing of it the checker would read -- and the migration
		// commands do not read events back from a server either, so a declared one would
		// be silently unmanaged: say so rather than accept it
		s.problem(at(n), "CREATE EVENT %s: sqlshape does not read events (nothing a statement of the program runs reaches one, and diff / apply do not manage them); keep it out of schema.sql", spName(n.Arg("name")))
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
	if x.OptPartitioning() != nil {
		t.Partitioned = true
	}
	s.tableDirectives(t, st.SQL, st.Offset)
	s.Tables = append(s.Tables, t)
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
		text = s.cur[n.Start:n.End]
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
	default:
		s.problem(at(n), "table element not understood: %s", n.Class)
	}
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
		}
		// every other option (ROW_FORMAT, AUTO_INCREMENT, STATS_*, ...) has no shape
	}
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
	dst.Engine, dst.Charset, dst.Collation = src.Engine, src.Charset, src.Collation
	// LIKE copies no foreign keys
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
		case "PT_create_table_engine_option", "PT_create_table_default_charset", "PT_create_table_default_collation", "PT_create_commen_option":
			s.tableOptions(t, []mysqlast.Value{an}, at)
		case "PT_alter_table_partition_by", "PT_alter_table_add_partition", "PT_alter_table_add_partition_num", "PT_alter_table_add_partition_def_list":
			t.Partitioned = true
		case "PT_alter_table_remove_partitioning":
			t.Partitioned = false
		case "PT_alter_table_order", "PT_alter_table_force", "PT_alter_table_enable_keys", "PT_alter_table_set_default_row_format",
			"PT_alter_table_discard_tablespace", "PT_alter_table_import_tablespace", "PT_alter_table_secondary_load", "PT_alter_table_secondary_unload":
			// storage-level, no shape
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
	trg.Directives, _ = s.spDirectives("trigger", name, st.SQL, st.Offset)
	s.Triggers = append(s.Triggers, trg)
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
	r.Directives, r.NotNull = s.spDirectives(strings.ToLower(kind.String()), name, st.SQL, st.Offset)
	s.Routines = append(s.Routines, r)
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
