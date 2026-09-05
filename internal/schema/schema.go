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
	Comments  map[string]string // "table" / "table.column" / "type:name" → comment

	// Problems are statements the loader could not apply. Loading continues past them.
	Problems []Problem

	relByName map[string]*Relation
	pending   []string // directives preceding the statement being applied
	nextOID   catalog.OID
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
	OID      catalog.OID
	Schema   string
	Name     string
	Args     []FuncArg
	RetType  TypeRef // Record for RETURNS TABLE / OUT params without explicit type
	RetSet   bool
	IsProc   bool
	Language string
	Volatile byte // i / s / v (default v)
	Strict   bool
	// NotNull is the `-- sqlshape: not null` annotation: the result is never NULL (PG
	// cannot tell; the schema author asserts it).
	NotNull bool
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
	s := &Schema{
		Catalog:   cat,
		Types:     newTypes(cat),
		Comments:  map[string]string{},
		relByName: map[string]*Relation{},
		nextOID:   FirstUserOID + 100000, // relations / functions live in a separate range from types
	}
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
	if schema == "" {
		schema = "public"
	}
	return s.relByName[schema+"."+name]
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
	case *pg_query.Node_CreateTableAsStmt:
		if st.CreateTableAsStmt.Objtype == pg_query.ObjectType_OBJECT_MATVIEW {
			s.createMatView(st.CreateTableAsStmt, loc)
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
	case *pg_query.Node_CreateSeqStmt, *pg_query.Node_CreateExtensionStmt, *pg_query.Node_CreateSchemaStmt,
		*pg_query.Node_CreateTrigStmt, *pg_query.Node_GrantStmt, *pg_query.Node_VariableSetStmt,
		*pg_query.Node_AlterSeqStmt, *pg_query.Node_CreatePolicyStmt, *pg_query.Node_AlterOwnerStmt:
		// No effect on typing. (Extensions' functions need a dumped catalog: not yet.)
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

func rangeVar(rv *pg_query.RangeVar) (schema, name string) {
	schema = rv.GetSchemaname()
	if schema == "" {
		schema = "public"
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
		schema = "public"
	}
	t := s.Types.addUser(schema, name, 'e', 'E', 0, 0)
	s.Types.Enums[t.OID] = strs(st.Vals)
}

func (s *Schema) createDomain(st *pg_query.CreateDomainStmt, loc int32) {
	schema, name := qualified(strs(st.Domainname))
	if schema == "" {
		schema = "public"
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
	schema, name := rangeVar(st.Typevar)
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
	schema, name := rangeVar(st.Relation)
	if s.relByName[schema+"."+name] != nil {
		if st.IfNotExists {
			return
		}
		s.problem(loc, "relation %q already exists", name)
		return
	}
	if len(st.InhRelations) > 0 || st.Partbound != nil {
		s.problem(loc, "table %s: inheritance / partitions are not supported", name)
	}
	rel := s.newRelation(schema, name, Table)
	for _, elt := range st.TableElts {
		switch e := elt.Node.(type) {
		case *pg_query.Node_ColumnDef:
			s.addColumn(rel, e.ColumnDef)
		case *pg_query.Node_Constraint:
			s.addTableConstraint(rel, e.Constraint)
		case *pg_query.Node_TableLikeClause:
			s.problem(loc, "table %s: LIKE is not supported", name)
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
	if isSerial(cd.TypeName) {
		col.NotNull = true
		col.Identity = 's' // serial: sequence default; behaves like identity for "who owns the value"
	}
	if cd.CollClause != nil {
		col.Collation = strings.Join(strs(cd.CollClause.Collname), ".")
	}
	rel.Columns = append(rel.Columns, col)
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
	rs, rn := rangeVar(c.Pktable)
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
	schema, name := rangeVar(st.View)
	if s.relByName[schema+"."+name] != nil && !st.Replace {
		s.problem(loc, "relation %q already exists", name)
		return
	}
	rel := s.newRelation(schema, name, View)
	rel.Query = st.Query
	rel.ColumnAliases = strs(st.Aliases)
}

func (s *Schema) createMatView(st *pg_query.CreateTableAsStmt, loc int32) {
	schema, name := rangeVar(st.Into.Rel)
	rel := s.newRelation(schema, name, MatView)
	rel.Query = st.Query
	rel.ColumnAliases = strs(st.Into.ColNames)
}

func (s *Schema) alterTable(st *pg_query.AlterTableStmt, loc int32) {
	schema, name := rangeVar(st.Relation)
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
		case pg_query.AlterTableType_AT_ChangeOwner, pg_query.AlterTableType_AT_EnableRowSecurity,
			pg_query.AlterTableType_AT_ForceRowSecurity, pg_query.AlterTableType_AT_SetRelOptions,
			pg_query.AlterTableType_AT_ClusterOn, pg_query.AlterTableType_AT_SetStatistics:
		default:
			s.problem(loc, "ALTER TABLE %s: unsupported action %v", name, cmd.GetSubtype())
		}
	}
}

func (s *Schema) createIndex(st *pg_query.IndexStmt, loc int32) {
	if !st.Unique {
		return // non-unique indexes have no typing / cardinality consequence
	}
	schema, name := rangeVar(st.Relation)
	rel := s.relByName[schema+"."+name]
	if rel == nil {
		s.problem(loc, "CREATE INDEX: relation %q does not exist", name)
		return
	}
	c := &Constraint{Name: st.Idxname, Kind: Unique, Predicate: st.WhereClause, NullsNotDistinct: st.NullsNotDistinct}
	for _, pn := range st.IndexParams {
		ie := pn.GetIndexElem()
		if ie.GetName() == "" {
			// expression index: cannot be matched to a column set for uniqueness proofs
			return
		}
		c.Columns = append(c.Columns, ie.GetName())
	}
	s.addConstraint(rel, c)
}

// --- functions -------------------------------------------------------------

func (s *Schema) createFunction(st *pg_query.CreateFunctionStmt, loc int32) {
	schema, name := qualified(strs(st.Funcname))
	if schema == "" {
		schema = "public"
	}
	fn := &Function{OID: s.nextOID, Schema: schema, Name: name, IsProc: st.IsProcedure, Volatile: 'v'}
	s.nextOID++
	for _, d := range s.pending {
		switch strings.ToLower(strings.Join(strings.Fields(d), " ")) {
		case "not null":
			fn.NotNull = true
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
		fn.RetType = TypeRef{OID: catalog.Void, Typmod: -1}
	}
	if len(tableCols) > 0 {
		fn.RetSet = true
		if len(tableCols) == 1 {
			fn.RetType = tableCols[0].Type
		} else {
			fn.RetType = TypeRef{OID: catalog.Record, Typmod: -1}
		}
	}
	for _, on := range st.Options {
		d := on.GetDefElem()
		switch d.GetDefname() {
		case "language":
			fn.Language = d.GetArg().GetString_().GetSval()
		case "volatility":
			fn.Volatile = d.GetArg().GetString_().GetSval()[0]
		case "strict":
			fn.Strict = d.GetArg().GetBoolean().GetBoolval()
		}
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
