package schema

import (
	"strconv"
	"strings"

	pg_query "github.com/pganalyze/pg_query_go/v6"

	"github.com/kr9ly/sqlshape/internal/catalog"
)

// The DDL the loader applies beyond CREATE TABLE / VIEW / FUNCTION: inheritance and
// partitions, LIKE, renames and drops, ALTER TYPE / DOMAIN, user-defined aggregates /
// operators / casts, collations, range types, search_path. None of it is exotic in a
// real schema.sql, and a statement the loader cannot apply leaves the schema wrong for
// everything after it.

// --- search_path -------------------------------------------------------------

// SearchPath is the schemas an unqualified name is looked up in, first match wins;
// the first entry is where unqualified CREATE puts new objects. SET search_path in
// schema.sql changes it for the statements that follow.
func (s *Schema) SearchPath() []string {
	if len(s.searchPath) == 0 {
		return []string{"public"}
	}
	return s.searchPath
}

// OnSearchPath reports whether an unqualified name may resolve into schema.
func (s *Schema) OnSearchPath(schema string) bool {
	if schema == "" || schema == "pg_catalog" {
		return true
	}
	for _, p := range s.SearchPath() {
		if p == schema {
			return true
		}
	}
	return false
}

// creationSchema is where an unqualified CREATE lands.
func (s *Schema) creationSchema() string { return s.SearchPath()[0] }

// DateTimeSettings is the datetime input state SET statements in the schema left behind:
// the DateStyle field order ("mdy" / "dmy" / "ymd"), the IntervalStyle and the TimeZone.
// Empty means the PG default (MDY, postgres, the session's zone).
func (s *Schema) DateTimeSettings() (dateOrder, intervalStyle, timeZone string) {
	return s.dateOrder, s.intervalStyle, s.timeZone
}

// XMLOptionDocument reports SET xmloption = document (XML literals are documents).
func (s *Schema) XMLOptionDocument() bool { return s.xmlDocument }

// ViewsRestricted reports SET restrict_nonsystem_relation_kind = view: user views may
// not be accessed.
func (s *Schema) ViewsRestricted() bool { return s.restrictViews }

// setVariableValues flattens SET's argument list: constants, identifiers and the
// comma-separated lists DateStyle accepts inside one string.
func setVariableValues(st *pg_query.VariableSetStmt) []string {
	var out []string
	for _, n := range st.Args {
		var v string
		if c := n.GetAConst(); c != nil {
			if sv := c.GetSval(); sv != nil {
				v = sv.GetSval()
			} else if iv := c.GetIval(); iv != nil {
				v = strconv.Itoa(int(iv.Ival))
			} else if fv := c.GetFval(); fv != nil {
				v = fv.Fval
			}
		} else if str := n.GetString_(); str != nil {
			v = str.Sval
		} else if tc := n.GetTypeCast(); tc != nil {
			// SET TIME ZONE INTERVAL '+05:30' HOUR TO MINUTE
			if c := tc.Arg.GetAConst(); c != nil {
				v = c.GetSval().GetSval()
			}
		}
		for _, part := range strings.Split(v, ",") {
			if part = strings.TrimSpace(part); part != "" {
				out = append(out, part)
			}
		}
	}
	return out
}

func (s *Schema) setVariable(st *pg_query.VariableSetStmt) {
	reset := st.Kind == pg_query.VariableSetKind_VAR_RESET || st.Kind == pg_query.VariableSetKind_VAR_RESET_ALL ||
		st.Kind == pg_query.VariableSetKind_VAR_SET_DEFAULT
	switch strings.ToLower(st.Name) {
	case "datestyle":
		if reset {
			s.dateOrder = ""
			return
		}
		for _, v := range setVariableValues(st) {
			switch strings.ToLower(v) {
			case "ymd", "iso":
				if strings.ToLower(v) == "ymd" {
					s.dateOrder = "ymd"
				}
			case "dmy", "euro", "european", "german":
				s.dateOrder = "dmy"
			case "mdy", "us", "noneuro", "noneuropean", "american":
				s.dateOrder = "mdy"
			case "default":
				s.dateOrder = ""
			}
		}
		return
	case "intervalstyle":
		s.intervalStyle = ""
		if !reset {
			if vs := setVariableValues(st); len(vs) > 0 {
				s.intervalStyle = strings.ToLower(vs[0])
			}
		}
		return
	case "restrict_nonsystem_relation_kind":
		s.restrictViews = false
		if !reset {
			for _, v := range setVariableValues(st) {
				if strings.EqualFold(strings.TrimSpace(v), "view") {
					s.restrictViews = true
				}
			}
		}
		return
	case "xmloption":
		s.xmlDocument = false
		if !reset {
			if vs := setVariableValues(st); len(vs) > 0 && strings.EqualFold(vs[0], "document") {
				s.xmlDocument = true
			}
		}
		return
	case "timezone":
		s.timeZone = ""
		if !reset {
			if vs := setVariableValues(st); len(vs) > 0 && !strings.EqualFold(vs[0], "default") {
				s.timeZone = vs[0]
			}
		}
		return
	case "search_path":
	default:
		return
	}
	if st.Kind == pg_query.VariableSetKind_VAR_RESET || st.Kind == pg_query.VariableSetKind_VAR_RESET_ALL {
		s.searchPath = nil
		return
	}
	var path []string
	for _, n := range st.Args {
		var v string
		if c := n.GetAConst(); c != nil {
			v = c.GetSval().GetSval()
		} else if str := n.GetString_(); str != nil {
			v = str.Sval
		}
		v = strings.TrimSpace(v)
		if v == "" || v == "pg_catalog" || strings.HasPrefix(v, "$") || strings.HasPrefix(v, "\"$") {
			continue
		}
		path = append(path, strings.Trim(v, `"`))
	}
	if len(path) == 0 {
		path = []string{"public"}
	}
	s.searchPath = path
	s.Types.searchPath = path
}

// --- inheritance, partitions, LIKE -----------------------------------------------

// inherit copies a parent's columns (and, for partitions, its constraints) into rel.
func (s *Schema) inherit(rel *Relation, parent *Relation, partition bool, loc int32) {
	rel.Parents = append(rel.Parents, parent)
	rel.IsPartition = rel.IsPartition || partition
	for _, pc := range parent.Columns {
		if rel.Column(pc.Name) != nil {
			continue // a child may redeclare a parent column with the same type
		}
		c := *pc
		c.Num = int16(len(rel.Columns) + 1)
		rel.Columns = append(rel.Columns, &c)
	}
	for _, pc := range parent.Constraints {
		if pc.Kind == Check || partition {
			c := *pc
			c.Name = "" // named for the child, PG style
			s.addConstraint(rel, &c)
		}
	}
	if partition {
		for _, ix := range parent.Indexes {
			c := *ix
			c.Name = s.chooseIndexName(rel, c.nameParts) // partition indexes get their own names
			rel.Indexes = append(rel.Indexes, &c)
		}
	}
}

// likeClause copies what CREATE TABLE (LIKE source INCLUDING ...) copies: columns with
// NOT NULL and collation always; defaults, identity, generated expressions, CHECK
// constraints and indexes on request (INCLUDING ALL takes everything).
func (s *Schema) likeClause(rel *Relation, lk *pg_query.TableLikeClause, loc int32) {
	schema, name := s.rangeVar(lk.Relation)
	src := s.relByName[schema+"."+name]
	if src == nil {
		s.problem(loc, "table %s: LIKE: relation %q does not exist", rel.Name, name)
		return
	}
	has := func(o pg_query.TableLikeOption) bool {
		return lk.Options&(1<<uint(pg_query.TableLikeOption_CREATE_TABLE_LIKE_ALL)) != 0 || lk.Options&(1<<uint(o)) != 0
	}
	for _, sc := range src.Columns {
		c := &Column{Num: int16(len(rel.Columns) + 1), Name: sc.Name, Type: sc.Type, NotNull: sc.NotNull, Collation: sc.Collation, Values: sc.Values}
		if has(pg_query.TableLikeOption_CREATE_TABLE_LIKE_DEFAULTS) {
			c.Default = sc.Default
		}
		if has(pg_query.TableLikeOption_CREATE_TABLE_LIKE_IDENTITY) {
			c.Identity = sc.Identity
		}
		if has(pg_query.TableLikeOption_CREATE_TABLE_LIKE_GENERATED) {
			c.Generated = sc.Generated
		}
		rel.Columns = append(rel.Columns, c)
	}
	for _, pc := range src.Constraints {
		switch {
		case pc.Kind == Check && has(pg_query.TableLikeOption_CREATE_TABLE_LIKE_CONSTRAINTS),
			(pc.Kind == PrimaryKey || pc.Kind == Unique) && has(pg_query.TableLikeOption_CREATE_TABLE_LIKE_INDEXES):
			c := *pc
			c.Name = ""
			s.addConstraint(rel, &c)
		}
	}
	if has(pg_query.TableLikeOption_CREATE_TABLE_LIKE_INDEXES) {
		for _, ix := range src.Indexes {
			c := *ix
			c.Name = s.chooseIndexName(rel, c.nameParts)
			rel.Indexes = append(rel.Indexes, &c)
		}
	}
}

// --- rename / drop ---------------------------------------------------------------

func (s *Schema) rename(st *pg_query.RenameStmt, loc int32) {
	switch st.RenameType {
	case pg_query.ObjectType_OBJECT_TABLE, pg_query.ObjectType_OBJECT_VIEW, pg_query.ObjectType_OBJECT_MATVIEW, pg_query.ObjectType_OBJECT_SEQUENCE, pg_query.ObjectType_OBJECT_INDEX:
		schema, name := s.rangeVar(st.Relation)
		rel := s.relByName[schema+"."+name]
		if rel == nil {
			if st.RenameType == pg_query.ObjectType_OBJECT_INDEX {
				s.renameIndex(name, st.Newname, loc, st.MissingOk)
				return
			}
			if !st.MissingOk && st.RenameType != pg_query.ObjectType_OBJECT_SEQUENCE {
				s.problem(loc, "ALTER ... RENAME: relation %q does not exist", name)
			}
			return
		}
		delete(s.relByName, schema+"."+name)
		rel.Name = st.Newname
		s.relByName[schema+"."+st.Newname] = rel
		s.Types.renameUser(rel.RowType, st.Newname)
		for _, r := range s.Relations {
			if r != rel && r.Schema == schema && r.Name == name {
				s.relByName[schema+"."+name] = r // a permanent table the renamed temp one hid
			}
		}
		for _, tg := range s.Triggers {
			if tg.Table == fullName(schema, name) {
				tg.Table = fullName(schema, st.Newname)
			}
		}
		for _, r := range s.Relations {
			for _, c := range r.Constraints {
				if c.Kind == ForeignKey && c.RefTable == fullName(schema, name) {
					c.RefTable = fullName(schema, st.Newname)
				}
			}
		}
	case pg_query.ObjectType_OBJECT_COLUMN, pg_query.ObjectType_OBJECT_ATTRIBUTE:
		schema, name := s.rangeVar(st.Relation)
		rel := s.relByName[schema+"."+name]
		if rel == nil {
			if !st.MissingOk {
				s.problem(loc, "ALTER ... RENAME COLUMN: relation %q does not exist", name)
			}
			return
		}
		col := rel.Column(st.Subname)
		if col == nil {
			for i := range rel.Frozen {
				if rel.Frozen[i].Name == st.Subname {
					rel.Frozen[i].Name = st.Newname
					return
				}
			}
			s.problem(loc, "%s: column %q does not exist", rel.Name, st.Subname)
			return
		}
		col.Name = st.Newname
		if st.Relation.Inh {
			for _, child := range s.Relations {
				if child != rel && child.InheritsFrom(rel) {
					if cc := child.Column(st.Subname); cc != nil {
						cc.Name = st.Newname
					}
				}
			}
		}
		for _, c := range rel.Constraints {
			renameIn(c.Columns, st.Subname, st.Newname)
		}
		for _, ix := range rel.Indexes {
			renameIn(ix.Columns, st.Subname, st.Newname)
		}
		for _, r := range s.Relations {
			for _, c := range r.Constraints {
				if c.Kind == ForeignKey && c.RefTable == rel.FullName() {
					renameIn(c.RefColumns, st.Subname, st.Newname)
				}
			}
		}
	case pg_query.ObjectType_OBJECT_TABCONSTRAINT:
		schema, name := s.rangeVar(st.Relation)
		rel := s.relByName[schema+"."+name]
		if rel == nil {
			s.problem(loc, "ALTER TABLE ... RENAME CONSTRAINT: relation %q does not exist", name)
			return
		}
		for _, c := range rel.Constraints {
			if c.Name == st.Subname {
				c.Name = st.Newname
				return
			}
		}
		s.problem(loc, "%s: constraint %q does not exist", rel.Name, st.Subname)
	case pg_query.ObjectType_OBJECT_TYPE, pg_query.ObjectType_OBJECT_DOMAIN:
		var names []string
		if tn := st.Object.GetTypeName(); tn != nil {
			names = strs(tn.GetNames())
		} else {
			names = strs(st.Object.GetList().GetItems())
		}
		schema, name := qualified(names)
		if schema == "" {
			schema = s.creationSchema()
		}
		t := s.Types.byName[schema+"."+name]
		if t == nil {
			s.problem(loc, "ALTER TYPE ... RENAME: type %q does not exist", name)
			return
		}
		s.Types.renameUser(t.OID, st.Newname)
		if rel := s.relByName[schema+"."+name]; rel != nil && rel.Kind == 'c' {
			delete(s.relByName, schema+"."+name)
			rel.Name = st.Newname
			s.relByName[schema+"."+st.Newname] = rel
		}
	case pg_query.ObjectType_OBJECT_FUNCTION, pg_query.ObjectType_OBJECT_PROCEDURE, pg_query.ObjectType_OBJECT_AGGREGATE:
		owa := st.Object.GetObjectWithArgs()
		schema, name := qualified(strs(owa.GetObjname()))
		found := false
		for _, f := range s.Functions {
			if f.Name == name && (schema == "" && s.OnSearchPath(f.Schema) || f.Schema == schema) && s.funcArgsMatch(f, owa) {
				f.Name = st.Newname
				found = true
			}
		}
		if !found && !st.MissingOk {
			s.problem(loc, "ALTER FUNCTION ... RENAME: function %q does not exist", name)
		}
	case pg_query.ObjectType_OBJECT_TRIGGER:
		for _, tg := range s.Triggers {
			if tg.Name == st.Subname {
				tg.Name = st.Newname
			}
		}
	case pg_query.ObjectType_OBJECT_SCHEMA, pg_query.ObjectType_OBJECT_POLICY, pg_query.ObjectType_OBJECT_RULE, pg_query.ObjectType_OBJECT_COLLATION:
		// no typing consequence
	default:
		s.problem(loc, "unsupported RENAME of %v", st.RenameType)
	}
}

func (s *Schema) renameIndex(name, newName string, loc int32, missingOk bool) {
	for _, r := range s.Relations {
		for _, c := range r.Constraints {
			if c.Name == name {
				c.Name = newName
			}
		}
		for _, ix := range r.Indexes {
			if ix.Name == name {
				ix.Name = newName
				return
			}
		}
	}
	if !missingOk {
		s.problem(loc, "ALTER INDEX ... RENAME: index %q does not exist", name)
	}
}

func renameIn(cols []string, old, new string) {
	for i, c := range cols {
		if c == old {
			cols[i] = new
		}
	}
}

func fullName(schema, name string) string {
	if schema == "public" {
		return name
	}
	return schema + "." + name
}

func (s *Schema) drop(st *pg_query.DropStmt, loc int32) {
	for _, on := range st.Objects {
		switch st.RemoveType {
		case pg_query.ObjectType_OBJECT_TABLE, pg_query.ObjectType_OBJECT_VIEW, pg_query.ObjectType_OBJECT_MATVIEW, pg_query.ObjectType_OBJECT_SEQUENCE:
			schema, name := qualified(strs(on.GetList().GetItems()))
			rel := s.findRelation(schema, name)
			if rel == nil {
				if !st.MissingOk {
					s.problem(loc, "DROP: relation %q does not exist", name)
				}
				continue
			}
			for _, child := range append([]*Relation{}, s.Relations...) {
				if child != rel && child.InheritsFrom(rel) && (st.Behavior == pg_query.DropBehavior_DROP_CASCADE || child.IsPartition) {
					s.removeRelation(child)
					s.dropDependentViews(child)
				}
			}
			s.removeRelation(rel)
			if rel.Kind == Table {
				s.dropOwnedSequences(rel, "")
			}
			if st.Behavior == pg_query.DropBehavior_DROP_CASCADE {
				s.dropDependentViews(rel)
			}
		case pg_query.ObjectType_OBJECT_INDEX:
			_, name := qualified(strs(on.GetList().GetItems()))
			found := false
			for _, r := range s.Relations {
				r.Constraints = filterConstraints(r.Constraints, func(c *Constraint) bool { return !(c.Kind == Unique && c.Name == name) })
				n := len(r.Indexes)
				r.Indexes = filterIndexes(r.Indexes, func(ix *Index) bool { return ix.Name != name })
				found = found || len(r.Indexes) != n
			}
			if !found && !st.MissingOk {
				s.problem(loc, "DROP INDEX: index %q does not exist", name)
			}
		case pg_query.ObjectType_OBJECT_TYPE, pg_query.ObjectType_OBJECT_DOMAIN:
			schema, name := qualified(strs(on.GetTypeName().GetNames()))
			if schema == "" {
				schema = s.creationSchema()
			}
			t := s.Types.byName[schema+"."+name]
			if t == nil {
				if !st.MissingOk {
					s.problem(loc, "DROP TYPE: type %q does not exist", name)
				}
				continue
			}
			s.dropTypeDependents(t, st.Behavior == pg_query.DropBehavior_DROP_CASCADE)
			s.Types.removeUser(t.OID)
			if rel := s.relByName[schema+"."+name]; rel != nil && rel.Kind == 'c' {
				s.removeRelation(rel)
			}
		case pg_query.ObjectType_OBJECT_FUNCTION, pg_query.ObjectType_OBJECT_PROCEDURE, pg_query.ObjectType_OBJECT_AGGREGATE, pg_query.ObjectType_OBJECT_ROUTINE:
			owa := on.GetObjectWithArgs()
			schema, name := qualified(strs(owa.GetObjname()))
			var kept []*Function
			removed := false
			for _, f := range s.Functions {
				if f.Name == name && (schema == "" && s.OnSearchPath(f.Schema) || f.Schema == schema) && (owa.ArgsUnspecified || s.argsMatch(f, owa.Objargs)) {
					removed = true
					continue
				}
				kept = append(kept, f)
			}
			s.Functions = kept
			if !removed && !st.MissingOk {
				s.problem(loc, "DROP FUNCTION: function %q does not exist", name)
			}
		case pg_query.ObjectType_OBJECT_TRIGGER:
			items := strs(on.GetList().GetItems())
			name := items[len(items)-1]
			var kept []*Trigger
			for _, tg := range s.Triggers {
				if tg.Name != name {
					kept = append(kept, tg)
				}
			}
			s.Triggers = kept
		case pg_query.ObjectType_OBJECT_CAST:
			// on is a List [source TypeName, target TypeName]
			items := on.GetList().GetItems()
			if len(items) == 2 {
				src, err1 := s.resolveType(items[0].GetTypeName())
				dst, err2 := s.resolveType(items[1].GetTypeName())
				if err1 == nil && err2 == nil {
					var kept []*catalog.Cast
					for _, c := range s.Casts {
						if !(c.Source == src.OID && c.Target == dst.OID) {
							kept = append(kept, c)
						}
					}
					s.Casts = kept
				}
			}
		case pg_query.ObjectType_OBJECT_OPERATOR:
			owa := on.GetObjectWithArgs()
			_, name := qualified(strs(owa.GetObjname()))
			var kept []*catalog.Operator
			for _, op := range s.Operators {
				if op.Name != name {
					kept = append(kept, op)
				}
			}
			s.Operators = kept
		case pg_query.ObjectType_OBJECT_SCHEMA:
			name := on.GetString_().GetSval()
			for _, r := range append([]*Relation{}, s.Relations...) {
				if r.Schema == name {
					s.removeRelation(r)
					s.dropDependentViews(r)
				}
			}
			var fns []*Function
			for _, f := range s.Functions {
				if f.Schema != name {
					fns = append(fns, f)
				}
			}
			s.Functions = fns
			s.Types.removeSchema(name)
		case pg_query.ObjectType_OBJECT_RULE:
			// DROP RULE name ON table
			parts := strs(on.GetList().GetItems())
			if len(parts) >= 2 {
				schema, name := qualified(parts[:len(parts)-1])
				if rel := s.findRelation(schema, name); rel != nil {
					if ev := rel.RuleNames[parts[len(parts)-1]]; ev != "" {
						rel.clearRules(ev)
					}
				}
			}
		case pg_query.ObjectType_OBJECT_EXTENSION,
			pg_query.ObjectType_OBJECT_POLICY, pg_query.ObjectType_OBJECT_COLLATION:
			// no typing consequence
		default:
			s.problem(loc, "unsupported DROP of %v", st.RemoveType)
		}
	}
}

// argsMatch reports whether f's input parameters are the given types (DROP FUNCTION f(int, text)).
func (s *Schema) argsMatch(f *Function, args []*pg_query.Node) bool {
	var in []TypeRef
	for _, a := range f.Args {
		if a.Mode == 'i' || a.Mode == 'b' || a.Mode == 'v' {
			in = append(in, a.Type)
		}
	}
	if len(in) != len(args) {
		return false
	}
	for i, n := range args {
		tr, err := s.resolveType(n.GetTypeName())
		if err != nil || tr.OID != in[i].OID {
			return false
		}
	}
	return true
}

func (s *Schema) findRelation(schema, name string) *Relation {
	if schema != "" {
		if r := s.relByName[schema+"."+name]; r != nil {
			return r
		}
		return s.systemRelation(schema, name)
	}
	// pg_catalog is implicitly first on the search path
	if r := s.systemRelation("pg_catalog", name); r != nil {
		return r
	}
	for _, p := range s.SearchPath() {
		if r := s.relByName[p+"."+name]; r != nil {
			return r
		}
	}
	for _, p := range s.SearchPath() {
		if r := s.systemRelation(p, name); r != nil {
			return r
		}
	}
	return nil
}

// systemRelation materializes a catalog relation (pg_class, information_schema.columns,
// an extension's view) as a read-only table the first time it is named.
func (s *Schema) systemRelation(schema, name string) *Relation {
	if r, ok := s.sysRels[schema+"."+name]; ok {
		return r
	}
	cr := s.Catalog.RelationByName(schema, name)
	if cr == nil {
		return nil
	}
	rel := &Relation{OID: cr.OID, Schema: cr.Schema, Name: cr.Name, Kind: Table}
	for _, t := range s.Catalog.Types {
		if t.Kind == 'c' && t.RelID == cr.OID {
			rel.RowType = t.OID
			break
		}
	}
	for i, c := range cr.Columns {
		rel.Columns = append(rel.Columns, &Column{Num: int16(i + 1), Name: c.Name, Type: TypeRef{OID: c.Type, Typmod: c.Typmod}, NotNull: c.NotNull})
	}
	if s.sysRels == nil {
		s.sysRels = map[string]*Relation{}
	}
	s.sysRels[schema+"."+name] = rel
	return rel
}

func (s *Schema) removeRelation(rel *Relation) {
	delete(s.relByName, rel.Schema+"."+rel.Name)
	var kept []*Relation
	for _, r := range s.Relations {
		if r != rel {
			kept = append(kept, r)
			if r.Schema == rel.Schema && r.Name == rel.Name {
				s.relByName[r.Schema+"."+r.Name] = r // a permanent table a dropped temp one hid
			}
		}
	}
	s.Relations = kept
	s.Types.removeUser(rel.RowType)
	var tgs []*Trigger
	for _, tg := range s.Triggers {
		if tg.Table != rel.FullName() {
			tgs = append(tgs, tg)
		}
	}
	s.Triggers = tgs
}

func filterConstraints(cs []*Constraint, keep func(*Constraint) bool) []*Constraint {
	var out []*Constraint
	for _, c := range cs {
		if keep(c) {
			out = append(out, c)
		}
	}
	return out
}

func filterIndexes(ixs []*Index, keep func(*Index) bool) []*Index {
	var out []*Index
	for _, ix := range ixs {
		if keep(ix) {
			out = append(out, ix)
		}
	}
	return out
}

// dropColumn removes a column and every constraint or index that mentions it.
func (s *Schema) dropColumn(rel *Relation, name string, loc int32, missingOk bool) {
	col := rel.Column(name)
	if col == nil {
		if !missingOk {
			s.problem(loc, "%s: column %q does not exist", rel.Name, name)
		}
		return
	}
	s.dropOwnedSequences(rel, name)
	var cols []*Column
	for _, c := range rel.Columns {
		if c != col {
			c.Num = int16(len(cols) + 1)
			cols = append(cols, c)
		}
	}
	rel.Columns = cols
	mentions := func(names []string) bool {
		for _, n := range names {
			if n == name {
				return true
			}
		}
		return false
	}
	rel.Constraints = filterConstraints(rel.Constraints, func(c *Constraint) bool {
		if c.Kind == Check {
			return !mentions(ColumnRefs(c.Expr))
		}
		return !mentions(c.Columns)
	})
	rel.Indexes = filterIndexes(rel.Indexes, func(ix *Index) bool { return !mentions(ix.Columns) })
}

// --- ALTER TYPE / ALTER DOMAIN --------------------------------------------------------

func (s *Schema) alterEnum(st *pg_query.AlterEnumStmt, loc int32) {
	schema, name := qualified(strs(st.TypeName))
	if schema == "" {
		schema = s.creationSchema()
	}
	t := s.Types.byName[schema+"."+name]
	if t == nil || t.Kind != 'e' {
		s.problem(loc, "ALTER TYPE: enum %q does not exist", name)
		return
	}
	labels := s.Types.Enums[t.OID]
	if st.OldVal != "" {
		// RENAME VALUE
		for i, l := range labels {
			if l == st.OldVal {
				labels[i] = st.NewVal
				return
			}
		}
		s.problem(loc, "ALTER TYPE %s: label %q does not exist", name, st.OldVal)
		return
	}
	for _, l := range labels {
		if l == st.NewVal {
			if !st.SkipIfNewValExists {
				s.problem(loc, "ALTER TYPE %s: label %q already exists", name, st.NewVal)
			}
			return
		}
	}
	if st.NewValNeighbor == "" {
		s.Types.Enums[t.OID] = append(labels, st.NewVal)
		return
	}
	for i, l := range labels {
		if l == st.NewValNeighbor {
			at := i
			if st.NewValIsAfter {
				at = i + 1
			}
			out := append([]string{}, labels[:at]...)
			out = append(out, st.NewVal)
			out = append(out, labels[at:]...)
			s.Types.Enums[t.OID] = out
			return
		}
	}
	s.problem(loc, "ALTER TYPE %s: label %q does not exist", name, st.NewValNeighbor)
}

func (s *Schema) alterDomain(st *pg_query.AlterDomainStmt, loc int32) {
	schema, name := qualified(strs(st.TypeName))
	if schema == "" {
		schema = s.creationSchema()
	}
	t := s.Types.byName[schema+"."+name]
	if t == nil || t.Kind != 'd' {
		s.problem(loc, "ALTER DOMAIN: domain %q does not exist", name)
		return
	}
	d := s.Types.Domains[t.OID]
	switch st.Subtype {
	case "T": // SET / DROP DEFAULT
	case "N":
		d.NotNull = false
	case "O":
		d.NotNull = true
	case "C":
		c := st.Def.GetConstraint()
		switch c.GetContype() {
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
		case pg_query.ConstrType_CONSTR_NOTNULL:
			d.NotNull = true
		default:
			s.problem(loc, "ALTER DOMAIN %s: unsupported constraint %v", name, c.GetContype())
		}
	case "X":
		var kept []*Constraint
		found := false
		for _, c := range d.Checks {
			if c.Name == st.Name {
				found = true
				continue
			}
			kept = append(kept, c)
		}
		d.Checks = kept
		if !found && !st.MissingOk {
			s.problem(loc, "ALTER DOMAIN %s: constraint %q does not exist", name, st.Name)
		}
	case "V": // VALIDATE CONSTRAINT
	default:
		s.problem(loc, "ALTER DOMAIN %s: unsupported action %q", name, st.Subtype)
	}
}

// --- CREATE TYPE AS RANGE, CREATE COLLATION, CREATE AGGREGATE / OPERATOR / CAST ------------

func (s *Schema) createRange(st *pg_query.CreateRangeStmt, loc int32) {
	schema, name := qualified(strs(st.TypeName))
	if schema == "" {
		schema = s.creationSchema()
	}
	var sub TypeRef
	found := false
	mname := ""
	for _, pn := range st.Params {
		d := pn.GetDefElem()
		switch d.GetDefname() {
		case "subtype":
			tr, err := s.resolveType(d.GetArg().GetTypeName())
			if err != nil {
				s.problem(loc, "range %s: %v", name, err)
				return
			}
			sub, found = tr, true
		case "multirange_type_name":
			_, mname = qualified(strs(d.GetArg().GetTypeName().GetNames()))
		}
	}
	if !found {
		s.problem(loc, "range %s: subtype is required", name)
		return
	}
	r := s.Types.addRange(schema, name, sub.OID, mname)
	rng := s.Types.RangeOf(r.OID)
	// CREATE TYPE AS RANGE also creates the constructors and the range → multirange cast
	// (DefineRange): name(sub, sub), name(sub, sub, text), multi(), multi(range),
	// multi(VARIADIC range[])
	mk := func(fname string, ret catalog.OID, args ...FuncArg) {
		fn := &Function{OID: s.nextOID, Schema: schema, Name: fname, Args: args, RetType: TypeRef{OID: ret}, Volatile: 'i', Language: "internal"}
		s.nextOID++
		s.Functions = append(s.Functions, fn)
	}
	subRef := TypeRef{OID: sub.OID}
	mk(name, r.OID, FuncArg{Type: subRef, Mode: 'i'}, FuncArg{Type: subRef, Mode: 'i'})
	mk(name, r.OID, FuncArg{Type: subRef, Mode: 'i'}, FuncArg{Type: subRef, Mode: 'i'}, FuncArg{Type: TypeRef{OID: catalog.Text}, Mode: 'i'})
	mt := s.Types.ByOID(rng.Multi)
	mk(mt.Name, rng.Multi)
	mk(mt.Name, rng.Multi, FuncArg{Type: TypeRef{OID: r.OID}, Mode: 'i'})
	mk(mt.Name, rng.Multi, FuncArg{Type: TypeRef{OID: r.Array}, Mode: 'v'})
	s.Casts = append(s.Casts, &catalog.Cast{Source: r.OID, Target: rng.Multi, Func: s.Functions[len(s.Functions)-2].OID, Context: 'e', Method: 'f'})
}

// define handles CREATE AGGREGATE / OPERATOR / COLLATION (DefineStmt).
func (s *Schema) define(st *pg_query.DefineStmt, loc int32) {
	schema, name := qualified(strs(st.Defnames))
	if schema == "" {
		schema = s.creationSchema()
	}
	defs := map[string]*pg_query.Node{}
	for _, dn := range st.Definition {
		d := dn.GetDefElem()
		defs[strings.ToLower(d.GetDefname())] = d.GetArg()
	}
	switch st.Kind {
	case pg_query.ObjectType_OBJECT_COLLATION:
		s.Types.Collations[name] = true
	case pg_query.ObjectType_OBJECT_AGGREGATE:
		s.createAggregate(schema, name, st, defs, loc)
	case pg_query.ObjectType_OBJECT_OPERATOR:
		s.createOperator(schema, name, defs, loc)
	case pg_query.ObjectType_OBJECT_TSCONFIGURATION, pg_query.ObjectType_OBJECT_TSDICTIONARY, pg_query.ObjectType_OBJECT_TSPARSER, pg_query.ObjectType_OBJECT_TSTEMPLATE:
		// text search objects: no typing consequence
	case pg_query.ObjectType_OBJECT_TYPE:
		s.createBaseType(schema, name, defs, loc)
	default:
		s.problem(loc, "unsupported CREATE %v", st.Kind)
	}
}

// createBaseType handles CREATE TYPE name (a shell type) and CREATE TYPE name (input = ...,
// output = ..., like = t): an opaque base type the analyzer can only meet through the
// casts and functions declared for it. LIKE lends its category, length and pass-by-value.
func (s *Schema) createBaseType(schema, name string, defs map[string]*pg_query.Node, loc int32) {
	t := s.Types.Lookup(schema, name)
	if t == nil {
		t = s.Types.addUser(schema, name, 'b', 'U', 0, 0)
	} else if t.OID < FirstUserOID {
		s.problem(loc, "type %q already exists", name)
		return
	}
	if like := defs["like"]; like != nil {
		lt, err := s.resolveType(like.GetTypeName())
		if err != nil {
			s.problem(loc, "type %s: like: %v", name, err)
			return
		}
		if l := s.Types.ByOID(lt.OID); l != nil {
			t.Category = l.Category
			t.Len = l.Len
			t.ByVal = l.ByVal
		}
	}
	if c := defs["category"]; c != nil {
		if v := c.GetString_().GetSval(); len(v) == 1 {
			t.Category = v[0]
		}
	}
}

// createAggregate registers CREATE AGGREGATE name (args) (sfunc = ..., stype = ...,
// finalfunc = ...) as a function of kind aggregate: the result is finalfunc's return type
// when there is one, the state type otherwise.
func (s *Schema) createAggregate(schema, name string, st *pg_query.DefineStmt, defs map[string]*pg_query.Node, loc int32) {
	fn := &Function{OID: s.nextOID, Schema: schema, Name: name, IsAgg: true, AggKind: 'n', Volatile: 'i'}
	s.nextOID++
	// Args: [list of FunctionParameter (nil for the old syntax), numDirectArgs]
	// (numDirectArgs >= 0 is an ordered-set aggregate; hypothetical marks the hypothetical kind)
	if len(st.Args) > 1 && st.Args[1].GetInteger().GetIval() >= 0 {
		fn.AggKind = 'o'
		if _, ok := defs["hypothetical"]; ok {
			fn.AggKind = 'h'
		}
	}
	if len(st.Args) > 0 {
		for _, pn := range st.Args[0].GetList().GetItems() {
			p := pn.GetFunctionParameter()
			if p == nil {
				continue
			}
			tr, err := s.resolveType(p.ArgType)
			if err != nil {
				s.problem(loc, "aggregate %s: %v", name, err)
				return
			}
			mode := byte('i')
			if p.Mode == pg_query.FunctionParameterMode_FUNC_PARAM_VARIADIC {
				mode = 'v'
			}
			fn.Args = append(fn.Args, FuncArg{Name: p.Name, Type: tr, Mode: mode})
		}
	}
	if bt := defs["basetype"]; bt != nil && len(fn.Args) == 0 {
		// old syntax: CREATE AGGREGATE name (basetype = t, ...)
		if tn := bt.GetTypeName(); tn != nil {
			if tr, err := s.resolveType(tn); err == nil {
				fn.Args = append(fn.Args, FuncArg{Type: tr, Mode: 'i'})
			}
		}
	}
	stype := defs["stype"]
	if stype == nil {
		stype = defs["stype1"] // the pre-8.2 spelling (sfunc1 / stype1 / initcond1)
	}
	if stype == nil {
		s.problem(loc, "aggregate %s: stype is required", name)
		return
	}
	st2, err := s.resolveType(stype.GetTypeName())
	if err != nil {
		s.problem(loc, "aggregate %s: %v", name, err)
		return
	}
	fn.RetType = st2
	if ff := defs["finalfunc"]; ff != nil {
		fschema, fname := qualified(strs(ff.GetList().GetItems()))
		if ff.GetTypeName() != nil { // a bare name parses as a TypeName
			fschema, fname = qualified(strs(ff.GetTypeName().GetNames()))
		}
		if ret, first, ok := s.functionReturn(fschema, fname, []TypeRef{st2}); ok {
			fn.RetType = s.resolveAggFinal(ret, first, st2)
		}
	}
	s.Functions = append(s.Functions, fn)
}

// resolveAggFinal is the aggregate's result when its final function is polymorphic: the
// state type stands in for the final function's first parameter (ffp(anyarray) returns
// anyarray over an int4[] state is int4[]).
func (s *Schema) resolveAggFinal(ret TypeRef, first catalog.OID, stype TypeRef) TypeRef {
	rt := s.Types.ByOID(ret.OID)
	if rt == nil || !rt.IsPolymorphic() {
		return ret
	}
	st := s.Types.ByOID(stype.OID)
	switch {
	case first == ret.OID:
		return stype
	case (first == catalog.AnyElement || first == catalog.AnyCompatible) && (ret.OID == catalog.AnyArray || ret.OID == catalog.AnyCompatibleArray):
		if arr := s.Types.ArrayOf(stype.OID); arr != 0 {
			return TypeRef{OID: arr, Typmod: -1}
		}
	case (first == catalog.AnyArray || first == catalog.AnyCompatibleArray) && (ret.OID == catalog.AnyElement || ret.OID == catalog.AnyCompatible):
		if st != nil && st.IsArray() {
			return TypeRef{OID: st.Elem, Typmod: -1}
		}
	}
	return ret
}

// functionReturn finds the return type and first parameter type of a user or catalog
// function whose first parameters are args (extra parameters may follow: finalfunc_extra).
func (s *Schema) functionReturn(schema, name string, args []TypeRef) (ret TypeRef, first catalog.OID, ok bool) {
	for _, f := range s.Functions {
		if f.Name != name || (schema != "" && f.Schema != schema) {
			continue
		}
		if len(f.Args) > 0 {
			first = f.Args[0].Type.OID
		}
		return f.RetType, first, true
	}
	if schema == "" || schema == "pg_catalog" {
		for _, f := range s.Catalog.FuncsByName(name) {
			if len(f.ArgTypes) >= len(args) && (len(args) == 0 || f.ArgTypes[0] == args[0].OID) {
				if len(f.ArgTypes) > 0 {
					first = f.ArgTypes[0]
				}
				return TypeRef{OID: f.RetType, Typmod: -1}, first, true
			}
		}
	}
	return TypeRef{}, 0, false
}

// createOperator registers CREATE OPERATOR name (leftarg = ..., rightarg = ..., function = ...).
// The result type is the implementing function's.
func (s *Schema) createOperator(schema, name string, defs map[string]*pg_query.Node, loc int32) {
	op := &catalog.Operator{OID: s.nextOID, Name: name, Kind: 'b', Schema: schema}
	s.nextOID++
	if l := defs["leftarg"]; l != nil {
		tr, err := s.resolveType(l.GetTypeName())
		if err != nil {
			s.problem(loc, "operator %s: %v", name, err)
			return
		}
		op.Left = tr.OID
	} else {
		op.Kind = 'l'
	}
	r := defs["rightarg"]
	if r == nil {
		s.problem(loc, "operator %s: rightarg is required", name)
		return
	}
	tr, err := s.resolveType(r.GetTypeName())
	if err != nil {
		s.problem(loc, "operator %s: %v", name, err)
		return
	}
	op.Right = tr.OID
	fnode := defs["function"]
	if fnode == nil {
		fnode = defs["procedure"]
	}
	if fnode == nil {
		s.problem(loc, "operator %s: function is required", name)
		return
	}
	var fschema, fname string
	if tn := fnode.GetTypeName(); tn != nil {
		fschema, fname = qualified(strs(tn.GetNames()))
	} else {
		fschema, fname = qualified(strs(fnode.GetList().GetItems()))
	}
	var args []TypeRef
	if op.Left != 0 {
		args = append(args, TypeRef{OID: op.Left, Typmod: -1})
	}
	args = append(args, TypeRef{OID: op.Right, Typmod: -1})
	ret, _, ok := s.functionReturn(fschema, fname, args)
	if !ok {
		s.problem(loc, "operator %s: function %q does not exist", name, fname)
		return
	}
	op.Result = ret.OID
	s.Operators = append(s.Operators, op)
}

func (s *Schema) createCast(st *pg_query.CreateCastStmt, loc int32) {
	src, err := s.resolveType(st.Sourcetype)
	if err != nil {
		s.problem(loc, "CREATE CAST: %v", err)
		return
	}
	dst, err := s.resolveType(st.Targettype)
	if err != nil {
		s.problem(loc, "CREATE CAST: %v", err)
		return
	}
	c := &catalog.Cast{OID: s.nextOID, Source: src.OID, Target: dst.OID, Context: 'e', Method: 'b'}
	s.nextOID++
	switch st.Context {
	case pg_query.CoercionContext_COERCION_IMPLICIT:
		c.Context = 'i'
	case pg_query.CoercionContext_COERCION_ASSIGNMENT:
		c.Context = 'a'
	}
	switch {
	case st.Inout:
		c.Method = 'i'
	case st.Func != nil:
		c.Method = 'f'
	}
	s.Casts = append(s.Casts, c)
}

// --- collations ---------------------------------------------------------------------------

// KnownCollation reports whether a COLLATE name can be trusted to exist: the built-in
// ones, one declared with CREATE COLLATION, or a locale-style name (en_US, en-US-x-icu,
// de_DE.utf8), whose presence depends on the server and is not checked.
func (s *Schema) KnownCollation(name string) bool {
	switch name {
	case "", "default", "C", "POSIX", "ucs_basic", "pg_c_utf8", "unicode":
		return true
	}
	if s.Types.Collations[name] {
		return true
	}
	return strings.ContainsAny(name, "_-.@")
}

// dropDependentViews removes the views whose defining query names rel (DROP ... CASCADE),
// and theirs in turn.
func (s *Schema) dropDependentViews(rel *Relation) {
	var dependents []*Relation
	for _, r := range s.Relations {
		if r.Query == nil {
			continue
		}
		for _, rv := range r.queryRangeVars() {
			if rv.name == rel.Name && (rv.schema == "" || rv.schema == rel.Schema) {
				dependents = append(dependents, r)
				break
			}
		}
	}
	for _, r := range dependents {
		s.removeRelation(r)
		s.dropDependentViews(r)
	}
}

// queryRangeVars lists the relations a view's query names, cached per query tree (the
// reflective walk is what made every DROP expensive).
func (r *Relation) queryRangeVars() []rangeRef {
	if r.Query == nil {
		return nil
	}
	if r.queryRefsFor != r.Query {
		r.queryRefs = r.queryRefs[:0]
		WalkNodes(r.Query, func(n *pg_query.Node) {
			if rv := n.GetRangeVar(); rv != nil {
				r.queryRefs = append(r.queryRefs, rangeRef{rv.Schemaname, rv.Relname})
			}
		})
		r.queryRefsFor = r.Query
	}
	return r.queryRefs
}

type rangeRef struct{ schema, name string }

// alterObjectSchema is ALTER ... SET SCHEMA for the objects the loader models.
func (s *Schema) alterObjectSchema(st *pg_query.AlterObjectSchemaStmt, loc int32) {
	to := st.Newschema
	switch st.ObjectType {
	case pg_query.ObjectType_OBJECT_TABLE, pg_query.ObjectType_OBJECT_VIEW, pg_query.ObjectType_OBJECT_MATVIEW,
		pg_query.ObjectType_OBJECT_SEQUENCE, pg_query.ObjectType_OBJECT_FOREIGN_TABLE:
		schema, name := s.rangeVar(st.Relation)
		rel := s.relByName[schema+"."+name]
		if rel == nil {
			if !st.MissingOk {
				s.problem(loc, "ALTER ... SET SCHEMA: relation %q does not exist", name)
			}
			return
		}
		delete(s.relByName, rel.Schema+"."+rel.Name)
		rel.Schema = to
		s.relByName[to+"."+name] = rel
		s.Types.moveUser(rel.RowType, to)
	case pg_query.ObjectType_OBJECT_TYPE, pg_query.ObjectType_OBJECT_DOMAIN:
		tn := st.Object.GetTypeName()
		tr, err := s.resolveType(tn)
		if err != nil {
			s.problem(loc, "ALTER TYPE SET SCHEMA: %v", err)
			return
		}
		s.Types.moveUser(tr.OID, to)
	case pg_query.ObjectType_OBJECT_FUNCTION, pg_query.ObjectType_OBJECT_PROCEDURE, pg_query.ObjectType_OBJECT_AGGREGATE, pg_query.ObjectType_OBJECT_ROUTINE:
		owa := st.Object.GetObjectWithArgs()
		schema, name := qualified(strs(owa.GetObjname()))
		if schema == "" {
			schema = s.creationSchema()
		}
		moved := false
		for _, fn := range s.Functions {
			if fn.Name == name && fn.Schema == schema && s.funcArgsMatch(fn, owa) {
				fn.Schema = to
				moved = true
			}
		}
		if !moved && !st.MissingOk {
			s.problem(loc, "ALTER FUNCTION SET SCHEMA: function %s does not exist", name)
		}
	}
}

// funcArgsMatch is whether an ObjectWithArgs signature (no args given = any) picks fn.
func (s *Schema) funcArgsMatch(fn *Function, owa *pg_query.ObjectWithArgs) bool {
	if owa.ArgsUnspecified || (len(owa.Objargs) == 0 && len(owa.Objfuncargs) == 0) {
		return true
	}
	var want []catalog.OID
	for _, n := range owa.Objargs {
		tr, err := s.resolveType(n.GetTypeName())
		if err != nil {
			return false
		}
		want = append(want, tr.OID)
	}
	var have []catalog.OID
	for _, a := range fn.Args {
		if a.Mode != 'o' && a.Mode != 't' {
			have = append(have, a.Type.OID)
		}
	}
	if len(want) != len(have) {
		return false
	}
	for i := range want {
		if want[i] != have[i] {
			return false
		}
	}
	return true
}

// dropTypeDependents removes what a DROP TYPE takes along: the range / multirange
// constructor functions and casts made with the type (internal dependencies), and, with
// CASCADE, the typed tables OF it.
func (s *Schema) dropTypeDependents(t *catalog.Type, cascade bool) {
	gone := map[catalog.OID]bool{t.OID: true, t.Array: true}
	if r := s.Types.RangeOf(t.OID); r != nil && t.Kind == 'r' {
		gone[r.Multi] = true
		if mt := s.Types.ByOID(r.Multi); mt != nil {
			gone[mt.Array] = true
		}
	}
	var kept []*Function
	goneFns := map[catalog.OID]bool{}
	for _, f := range s.Functions {
		if f.Language == "internal" && (gone[f.RetType.OID] || len(f.Args) > 0 && gone[f.Args[0].Type.OID]) {
			goneFns[f.OID] = true
			continue
		}
		kept = append(kept, f)
	}
	s.Functions = kept
	var casts []*catalog.Cast
	for _, c := range s.Casts {
		if gone[c.Source] || gone[c.Target] || goneFns[c.Func] {
			continue
		}
		casts = append(casts, c)
	}
	s.Casts = casts
	if cascade {
		var typed []*Relation
		for _, r := range s.Relations {
			if r.OfType == t.OID {
				typed = append(typed, r)
			}
		}
		for _, r := range typed {
			s.removeRelation(r)
			s.dropDependentViews(r)
		}
	}
}
