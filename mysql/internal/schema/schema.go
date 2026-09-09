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

	"github.com/kr9ly/sqlshape/mysql/internal/mysqlast"
	"github.com/kr9ly/sqlshape/mysql/internal/mysqlparse"
)

// Schema is the loaded schema.
type Schema struct {
	// Version is the declared MySQL version ("8.4"), from `-- sqlshape: mysql 8.4`.
	Version  string
	Tables   []*Table
	Views    []*View
	Problems []Problem
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
}

// Check is a CHECK constraint.
type Check struct {
	Name     string
	Expr     mysqlast.Value
	Enforced bool
}

// View is a CREATE VIEW.
type View struct {
	Name        string
	Columns     []string // the declared column list, when given
	Query       mysqlast.Value
	Algorithm   string
	CheckOption string
	Definition  string
}

// Table returns the table named name, or nil.
func (s *Schema) Table(name string) *Table {
	for _, t := range s.Tables {
		if strings.EqualFold(t.Name, name) {
			return t
		}
	}
	return nil
}

// View returns the view named name, or nil.
func (s *Schema) View(name string) *View {
	for _, v := range s.Views {
		if strings.EqualFold(v.Name, name) {
			return v
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
	s := &Schema{Version: v}
	for _, st := range mysqlparse.Split(schemaSQL) {
		s.apply(st)
	}
	return s, nil
}

func (s *Schema) problem(pos int, format string, args ...any) {
	s.Problems = append(s.Problems, Problem{Position: pos, Message: fmt.Sprintf(format, args...)})
}

// apply parses one statement and folds it into the schema.
func (s *Schema) apply(st mysqlparse.Statement) {
	cst, err := mysqlparse.Parse(st.SQL, 0)
	if err != nil {
		if pe, ok := err.(*mysqlparse.Error); ok {
			s.problem(st.Offset+pe.Offset, "%s", pe.Message)
			return
		}
		s.problem(st.Offset, "%v", err)
		return
	}
	v, err := mysqlast.Build(st.SQL, cst)
	if err != nil {
		if u, ok := err.(*mysqlast.Unsupported); ok {
			s.problem(st.Offset+u.Start, "unsupported construct %s: %q", u.Rule, u.Text)
			return
		}
		s.problem(st.Offset, "%v", err)
		return
	}
	n, ok := v.(*mysqlast.Node)
	if !ok {
		s.problem(st.Offset, "not a statement the schema loader reads: %s", mysqlast.Sprint(v))
		return
	}
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

// --- CREATE TABLE ------------------------------------------------------------------

func (s *Schema) createTable(n *mysqlast.Node, st mysqlparse.Statement, at func(mysqlast.Value) int) {
	x, _ := mysqlast.AsPTCreateTableStmt(n)
	name := tableName(x.TableName())
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
	s.Tables = append(s.Tables, t)
}

// tableElement applies one element of a CREATE TABLE body or an ADD of ALTER TABLE.
func (s *Schema) tableElement(t *Table, el mysqlast.Value, at func(mysqlast.Value) int) {
	n, ok := el.(*mysqlast.Node)
	if !ok {
		s.problem(at(el), "table element not understood: %s", mysqlast.Sprint(el))
		return
	}
	switch n.Class {
	case "PT_column_def":
		x, _ := mysqlast.AsPTColumnDef(n)
		col, keys, fk, check := s.column(str(x.FieldIdent()), x.FieldDef(), at)
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
		k := &Key{Name: str(x.Name()), Kind: keyKind(str(x.TypePar()))}
		for _, p := range list(x.Cols()) {
			k.Parts = append(k.Parts, keyPart(p))
		}
		s.indexOptions(k, list(x.Options()))
		t.addKey(k)
	case "PT_foreign_key_definition":
		x, _ := mysqlast.AsPTForeignKeyDefinition(n)
		fk := &ForeignKey{Name: str(x.ConstraintName()), RefTable: tableName(x.ReferencedTable()),
			OnDelete: fkOption(str(x.FkDeleteOpt())), OnUpdate: fkOption(str(x.FkUpdateOpt()))}
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
		t.Checks = append(t.Checks, &Check{Name: str(x.Name()), Expr: x.Expr(), Enforced: !isFalse(x.IsEnforced())})
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
	name := tableName(n.Arg("name"))
	v := &View{Name: name, Query: n.Arg("query"), Definition: st.SQL,
		Algorithm: strings.TrimPrefix(str(n.Arg("algorithm")), "VIEW_ALGORITHM_"), CheckOption: strings.TrimPrefix(str(n.Arg("check_option")), "VIEW_CHECK_")}
	for _, c := range list(n.Arg("column_list")) {
		v.Columns = append(v.Columns, str(c))
	}
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

func (s *Schema) dropTable(name string) {
	for i, t := range s.Tables {
		if strings.EqualFold(t.Name, name) {
			s.Tables = append(s.Tables[:i], s.Tables[i+1:]...)
			return
		}
	}
}

func (s *Schema) dropView(name string) {
	for i, v := range s.Views {
		if strings.EqualFold(v.Name, name) {
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
		from, to := tableName(p.Args[0]), tableName(p.Args[1])
		if t := s.Table(from); t != nil {
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
