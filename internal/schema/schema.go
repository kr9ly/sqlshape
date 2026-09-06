// Package schema layers a user's schema.sql (tables, views, enums, domains,
// composites, functions, indexes) over the bootstrap catalog. It parses DDL
// with libpg_query and resolves column / argument types; expression bodies
// (view queries, CHECK, DEFAULT) are kept as AST for the analyzer.
package schema

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

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
	sysRels    map[string]*Relation // system relations named so far (systemRelation)
	pending    []string             // directives preceding the statement being applied
	nextOID    catalog.OID
	searchPath []string // SET search_path, nil = public
	// datetime input GUCs (SET datestyle / intervalstyle / timezone); "" = PG default
	dateOrder, intervalStyle, timeZone string
}

// Problem is a DDL statement (or part) that was skipped or rejected.
type Problem struct {
	Location int32 // byte offset into the source, 0 if unknown
	Message  string
}

func (p Problem) String() string { return fmt.Sprintf("@%d: %s", p.Location, p.Message) }

// RelKind distinguishes tables from views.
type RelKind byte

const (
	Table   RelKind = 'r'
	View    RelKind = 'v'
	MatView RelKind = 'm'
)

// Relation is a table or view with its columns and constraints.
type Relation struct {
	OID     catalog.OID // synthetic pg_class oid
	Schema  string
	Name    string
	Kind    RelKind
	RowType catalog.OID // the composite type of a row
	Columns []*Column
	// Constraints: PK / UNIQUE / FK / CHECK on the table, plus unique indexes (as Unique).
	Constraints []*Constraint
	// Query is the defining query of a view / matview (nil for tables).
	Query Expr
	// ColumnAliases are explicit column names given to a view (CREATE VIEW v (a, b) AS ...).
	ColumnAliases []string
	// Indexes are every index on the table (unique ones also appear in Constraints), for
	// the advisory "no index leads with a predicate column".
	Indexes []*Index
	// Visible is the `-- sqlshape: visible where <predicate>` policy: rows of the table are
	// only meant to be seen through this predicate, so every statement reading it (and
	// every view over it) must carry the predicate. Nil when none.
	Visible Expr
	// Unfiltered (views): tables a `-- sqlshape: unfiltered t1, t2` directive before the
	// CREATE VIEW exempts from their visibility policy inside this view's query.
	Unfiltered map[string]bool
	// InsteadRules (views): the write commands ("insert" / "update" / "delete") a
	// CREATE RULE ... DO INSTEAD makes the view take.
	InsteadRules map[string]bool
	// Parents are the tables this one INHERITS from / is a PARTITION OF.
	Parents []*Relation
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
	// ForeignKey actions (pg_constraint confdeltype / confupdtype):
	// 'a' NO ACTION, 'r' RESTRICT, 'c' CASCADE, 'n' SET NULL, 'd' SET DEFAULT
	OnDelete, OnUpdate byte
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
	// IsAgg marks a CREATE AGGREGATE (RetType is the final / state type).
	IsAgg    bool
	Language string
	Volatile byte // i / s / v (default v)
	Strict   bool
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
		Catalog:   cat,
		Types:     newTypes(cat),
		Comments:  map[string]string{},
		relByName: map[string]*Relation{},
		nextOID:   FirstUserOID + 100000, // relations / functions live in a separate range from types
	}
	s.Problems = append(s.Problems, extProblems...)
	prev := int32(0)
	for _, raw := range tree.Stmts {
		// `-- sqlshape: ...` comment lines in front of a statement annotate it (a statement's
		// span starts right after the previous semicolon, so the comments sit inside it)
		end := raw.StmtLocation + raw.StmtLen
		if raw.StmtLen == 0 {
			end = int32(len(schemaSQL))
		}
		s.pending = directives(leadingComments(schemaSQL[prev:end]))
		s.apply(raw.Stmt, raw.StmtLocation)
		prev = end
	}
	return s, nil
}

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

// FullName is schema.name, with public elided.
func (r *Relation) FullName() string {
	if r.Schema == "public" {
		return r.Name
	}
	return r.Schema + "." + r.Name
}

func (s *Schema) problem(loc int32, format string, args ...any) {
	s.Problems = append(s.Problems, Problem{Location: loc, Message: fmt.Sprintf(format, args...)})
}

func (s *Schema) apply(n *pg_query.Node, loc int32) {
	switch st := n.Node.(type) {
	case *pg_query.Node_CreateEnumStmt:
		s.createEnum(st.CreateEnumStmt, loc)
	case *pg_query.Node_CreateDomainStmt:
		s.createDomain(st.CreateDomainStmt, loc)
	case *pg_query.Node_CompositeTypeStmt:
		s.createComposite(st.CompositeTypeStmt, loc)
	case *pg_query.Node_CreateStmt:
		s.createTable(st.CreateStmt, loc)
	case *pg_query.Node_ViewStmt:
		s.createView(st.ViewStmt, loc)
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
			s.createTableAs(st.CreateTableAsStmt.Into, st.CreateTableAsStmt.Query, loc)
		} else {
			s.problem(loc, "CREATE TABLE AS is not supported in schema.sql")
		}
	case *pg_query.Node_AlterTableStmt:
		s.alterTable(st.AlterTableStmt, loc)
	case *pg_query.Node_IndexStmt:
		s.createIndex(st.IndexStmt, loc)
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
		if r.Instead {
			if rel := s.findRelation(s.rangeVar(r.Relation)); rel != nil && rel.Kind == View {
				if rel.InsteadRules == nil {
					rel.InsteadRules = map[string]bool{}
				}
				switch r.Event {
				case pg_query.CmdType_CMD_INSERT:
					rel.InsteadRules["insert"] = true
				case pg_query.CmdType_CMD_UPDATE:
					rel.InsteadRules["update"] = true
				case pg_query.CmdType_CMD_DELETE:
					rel.InsteadRules["delete"] = true
				}
			}
		}
	case *pg_query.Node_CreateSeqStmt, *pg_query.Node_CreateExtensionStmt,
		*pg_query.Node_GrantStmt, *pg_query.Node_AlterSeqStmt, *pg_query.Node_CreatePolicyStmt, *pg_query.Node_AlterOwnerStmt,
		*pg_query.Node_CreateOpClassStmt, *pg_query.Node_CreateOpFamilyStmt, *pg_query.Node_AlterOpFamilyStmt,
		*pg_query.Node_CreateStatsStmt, *pg_query.Node_AlterPolicyStmt, *pg_query.Node_AlterExtensionStmt, *pg_query.Node_AlterObjectSchemaStmt,
		*pg_query.Node_CreateEventTrigStmt, *pg_query.Node_AlterEventTrigStmt, *pg_query.Node_CreatePublicationStmt, *pg_query.Node_AlterPublicationStmt,
		*pg_query.Node_CreateSubscriptionStmt, *pg_query.Node_CreateRoleStmt, *pg_query.Node_AlterRoleStmt, *pg_query.Node_GrantRoleStmt,
		*pg_query.Node_CreateTableSpaceStmt, *pg_query.Node_SecLabelStmt, *pg_query.Node_ClusterStmt, *pg_query.Node_VacuumStmt,
		*pg_query.Node_TransactionStmt, *pg_query.Node_DoStmt, *pg_query.Node_AlterDefaultPrivilegesStmt, *pg_query.Node_CreateFdwStmt,
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

// resolveType turns a TypeName AST into a TypeRef (handles serial pseudo-types and arrays).
func (s *Schema) resolveType(tn *pg_query.TypeName) (TypeRef, error) {
	if tn == nil {
		return TypeRef{}, fmt.Errorf("missing type")
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
	rel := &Relation{OID: s.nextOID, Schema: schema, Name: name, Kind: 'c'}
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
	rel := &Relation{OID: s.nextOID, Schema: schema, Name: name, Kind: kind}
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
	// CREATE TABLE ... OF type: the composite type's attributes are the columns
	if st.OfTypename != nil {
		tr, err := s.resolveType(st.OfTypename)
		if err != nil {
			s.problem(loc, "table %s: %v", name, err)
		} else {
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
		switch {
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
		merged = true
	}
	if isSerial(cd.TypeName) {
		col.NotNull = true
		col.Identity = 's' // serial: sequence default; behaves like identity for "who owns the value"
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
		case pg_query.ConstrType_CONSTR_GENERATED:
			col.Generated = c.RawExpr
		case pg_query.ConstrType_CONSTR_PRIMARY:
			col.NotNull = true
			s.addConstraint(rel, &Constraint{Name: c.Conname, Kind: PrimaryKey, Columns: []string{col.Name}})
		case pg_query.ConstrType_CONSTR_UNIQUE:
			s.addConstraint(rel, &Constraint{Name: c.Conname, Kind: Unique, Columns: []string{col.Name}, NullsNotDistinct: c.NullsNotDistinct})
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
		s.addConstraint(rel, &Constraint{Name: c.Conname, Kind: PrimaryKey, Columns: cols})
	case pg_query.ConstrType_CONSTR_UNIQUE:
		s.addConstraint(rel, &Constraint{Name: c.Conname, Kind: Unique, Columns: strs(c.Keys), NullsNotDistinct: c.NullsNotDistinct})
	case pg_query.ConstrType_CONSTR_CHECK:
		s.addConstraint(rel, &Constraint{Name: c.Conname, Kind: Check, Expr: c.RawExpr})
	case pg_query.ConstrType_CONSTR_FOREIGN:
		fk := s.foreignKey(c)
		fk.Columns = strs(c.FkAttrs)
		s.addConstraint(rel, fk)
	case pg_query.ConstrType_CONSTR_EXCLUSION:
		// no typing consequence
	default:
		s.problem(c.GetLocation(), "%s: unsupported table constraint %v", rel.Name, c.GetContype())
	}
}

func (s *Schema) createView(st *pg_query.ViewStmt, loc int32) {
	schema, name := s.rangeVar(st.View)
	if s.relByName[schema+"."+name] != nil && !st.Replace {
		s.problem(loc, "relation %q already exists", name)
		return
	}
	rel := s.newRelation(schema, name, View)
	rel.Query = st.Query
	rel.ColumnAliases = strs(st.Aliases)
	s.viewDirectives(rel, loc)
}

// viewDirectives applies the directives written before a CREATE (MATERIALIZED) VIEW.
func (s *Schema) viewDirectives(rel *Relation, loc int32) {
	for _, d := range s.pending {
		norm := strings.Join(strings.Fields(d), " ")
		switch {
		case len(norm) > 11 && strings.EqualFold(norm[:11], "unfiltered "):
			if rel.Unfiltered == nil {
				rel.Unfiltered = map[string]bool{}
			}
			for _, t := range strings.Split(norm[11:], ",") {
				if t = strings.TrimSpace(t); t != "" {
					rel.Unfiltered[t] = true
				}
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
}

func (s *Schema) alterTable(st *pg_query.AlterTableStmt, loc int32) {
	schema, name := s.rangeVar(st.Relation)
	rel := s.relByName[schema+"."+name]
	if rel == nil {
		if !st.MissingOk {
			s.problem(loc, "ALTER TABLE: relation %q does not exist", name)
		}
		return
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
			s.addTableConstraint(rel, cmd.Def.GetConstraint())
		case pg_query.AlterTableType_AT_SetNotNull:
			if col := rel.Column(cmd.Name); col != nil {
				col.NotNull = true
			}
		case pg_query.AlterTableType_AT_DropNotNull:
			if col := rel.Column(cmd.Name); col != nil {
				col.NotNull = false
			}
		case pg_query.AlterTableType_AT_ColumnDefault:
			if col := rel.Column(cmd.Name); col != nil {
				col.Default = cmd.Def
			}
		case pg_query.AlterTableType_AT_AlterColumnType:
			if col := rel.Column(cmd.Name); col != nil {
				if tr, err := s.resolveType(cmd.Def.GetColumnDef().GetTypeName()); err == nil {
					col.Type = tr
				} else {
					s.problem(loc, "%s.%s: %v", rel.Name, cmd.Name, err)
				}
			}
		case pg_query.AlterTableType_AT_DropColumn:
			s.dropColumn(rel, cmd.Name, loc, cmd.MissingOk)
		case pg_query.AlterTableType_AT_DropConstraint:
			n := len(rel.Constraints)
			rel.Constraints = filterConstraints(rel.Constraints, func(c *Constraint) bool { return c.Name != cmd.Name })
			if len(rel.Constraints) == n && !cmd.MissingOk {
				s.problem(loc, "%s: constraint %q does not exist", rel.Name, cmd.Name)
			}
		case pg_query.AlterTableType_AT_AddIdentity:
			if col := rel.Column(cmd.Name); col != nil {
				col.NotNull = true
				col.Identity = 'd'
				if c := cmd.Def.GetConstraint(); c != nil && len(c.GeneratedWhen) > 0 {
					col.Identity = c.GeneratedWhen[0]
				}
			}
		case pg_query.AlterTableType_AT_DropIdentity:
			if col := rel.Column(cmd.Name); col != nil {
				col.Identity = 0
			}
		case pg_query.AlterTableType_AT_DropExpression:
			if col := rel.Column(cmd.Name); col != nil {
				col.Generated = nil
			}
		case pg_query.AlterTableType_AT_ChangeOwner, pg_query.AlterTableType_AT_EnableRowSecurity,
			pg_query.AlterTableType_AT_ForceRowSecurity, pg_query.AlterTableType_AT_SetRelOptions,
			pg_query.AlterTableType_AT_ClusterOn, pg_query.AlterTableType_AT_SetStatistics,
			pg_query.AlterTableType_AT_EnableTrig, pg_query.AlterTableType_AT_DisableTrig, pg_query.AlterTableType_AT_EnableAlwaysTrig,
			pg_query.AlterTableType_AT_EnableReplicaTrig, pg_query.AlterTableType_AT_EnableTrigAll, pg_query.AlterTableType_AT_DisableTrigAll,
			pg_query.AlterTableType_AT_EnableTrigUser, pg_query.AlterTableType_AT_DisableTrigUser,
			pg_query.AlterTableType_AT_AttachPartition, pg_query.AlterTableType_AT_DetachPartition, pg_query.AlterTableType_AT_DetachPartitionFinalize,
			pg_query.AlterTableType_AT_ValidateConstraint, pg_query.AlterTableType_AT_ReplicaIdentity, pg_query.AlterTableType_AT_SetLogged,
			pg_query.AlterTableType_AT_SetUnLogged, pg_query.AlterTableType_AT_SetTableSpace, pg_query.AlterTableType_AT_SetStorage,
			pg_query.AlterTableType_AT_SetCompression, pg_query.AlterTableType_AT_AlterConstraint, pg_query.AlterTableType_AT_ResetRelOptions,
			pg_query.AlterTableType_AT_AddInherit, pg_query.AlterTableType_AT_DropInherit, pg_query.AlterTableType_AT_SetAccessMethod,
			pg_query.AlterTableType_AT_NoForceRowSecurity, pg_query.AlterTableType_AT_DisableRowSecurity, pg_query.AlterTableType_AT_SetIdentity,
			pg_query.AlterTableType_AT_EnableRule, pg_query.AlterTableType_AT_DisableRule,
			pg_query.AlterTableType_AT_EnableAlwaysRule, pg_query.AlterTableType_AT_EnableReplicaRule, pg_query.AlterTableType_AT_DropOids,
			pg_query.AlterTableType_AT_SetOptions, pg_query.AlterTableType_AT_ResetOptions, pg_query.AlterTableType_AT_GenericOptions,
			pg_query.AlterTableType_AT_AlterColumnGenericOptions, pg_query.AlterTableType_AT_SetExpression:
		default:
			s.problem(loc, "ALTER TABLE %s: unsupported action %v", name, cmd.GetSubtype())
		}
	}
}

func (s *Schema) createIndex(st *pg_query.IndexStmt, loc int32) {
	schema, name := s.rangeVar(st.Relation)
	rel := s.relByName[schema+"."+name]
	if rel == nil {
		s.problem(loc, "CREATE INDEX: relation %q does not exist", name)
		return
	}
	idx := &Index{Name: st.Idxname, Unique: st.Unique, Predicate: st.WhereClause}
	for _, pn := range st.IndexParams {
		ie := pn.GetIndexElem()
		if ie.GetName() == "" {
			break // expression element: the columns up to here still lead the index
		}
		idx.Columns = append(idx.Columns, ie.GetName())
	}
	rel.Indexes = append(rel.Indexes, idx)
	if !st.Unique || len(idx.Columns) != len(st.IndexParams) {
		return // only whole-column unique indexes give uniqueness proofs
	}
	c := &Constraint{Name: st.Idxname, Kind: Unique, Predicate: st.WhereClause, NullsNotDistinct: st.NullsNotDistinct, Columns: idx.Columns}
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
		key = "type:" + strings.Join(strs(st.Object.GetTypeName().GetNames()), ".")
	default:
		return
	}
	key = strings.TrimPrefix(key, "public.")
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
		var base string
		switch c.Kind {
		case PrimaryKey:
			base = rel.Name + "_pkey"
		case Unique:
			base = rel.Name + "_" + strings.Join(c.Columns, "_") + "_key"
		case ForeignKey:
			base = rel.Name + "_" + strings.Join(c.Columns, "_") + "_fkey"
		case Check:
			cols := c.Columns
			if len(cols) == 0 {
				cols = ColumnRefs(c.Expr)
			}
			if len(cols) > 0 {
				base = rel.Name + "_" + strings.Join(cols, "_") + "_check"
			} else {
				base = rel.Name + "_check"
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
	schema, name := s.rangeVar(st.Relation)
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
	for i, c := range cols {
		c.Num = int16(i + 1)
		if i < len(into.ColNames) {
			c.Name = into.ColNames[i].GetString_().GetSval()
		}
		rel.Columns = append(rel.Columns, c)
	}
}
