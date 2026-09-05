package analyze

import (
	"strconv"
	"strings"

	pg_query "github.com/pganalyze/pg_query_go/v6"

	"github.com/kr9ly/sqlshape/internal/catalog"
	"github.com/kr9ly/sqlshape/internal/schema"
)

// expr is the analysis of one expression node.
type expr struct {
	typ      schema.TypeRef
	nullable bool
	src      *Source
	node     *pg_query.Node
	// param is the parameter number when the expression is a bare $n
	param int32
	// lit marks a constant (or an expression built only from constants and parameters):
	// it carries no domain of its own, see domain.go
	lit bool
	// fields describes an anonymous record (row(...), whole-row reference) or an array of
	// them, so nested Go structs can be checked positionally
	fields []rteCol
}

func unknownRef() schema.TypeRef         { return schema.TypeRef{OID: catalog.Unknown, Typmod: -1} }
func ref(oid catalog.OID) schema.TypeRef { return schema.TypeRef{OID: oid, Typmod: -1} }

func (e expr) oid() catalog.OID { return e.typ.OID }

// bind gives an unknown-typed expression (untyped literal or $n) the type the context demands.
// For $n this records the parameter type; conflicting demands are an error.
func (a *analyzer) bind(e *expr, to catalog.OID, loc int32) *Error {
	if e.oid() != catalog.Unknown || to == catalog.Unknown || to == 0 {
		return nil
	}
	if e.param > 0 {
		if err := a.setParam(e.param, to, loc); err != nil {
			return err
		}
	} else if c := e.node.GetAConst(); c != nil {
		if sv, ok := c.Val.(*pg_query.A_Const_Sval); ok {
			if err := a.validateLiteral(sv.Sval.GetSval(), to, c.Location); err != nil {
				return err
			}
		}
	}
	e.typ = ref(to)
	return nil
}

func (a *analyzer) setParam(n int32, to catalog.OID, loc int32) *Error {
	if t := a.typ(to); t != nil && t.IsPolymorphic() {
		return nil
	}
	if cur, ok := a.params[n]; ok && cur != catalog.Unknown && cur != to {
		return errAt(codeIndeterminateDatatype, loc, "inconsistent types deduced for parameter $%d", n)
	}
	a.params[n] = to
	return nil
}

// bindAll binds each expr to the candidate's declared argument type, resolving polymorphic positions.
func (a *analyzer) bindArgs(es []*expr, c *candidate, loc int32) *Error {
	// resolve polymorphic element from the known args first
	actual := make([]catalog.OID, len(es))
	for i, e := range es {
		actual[i] = e.oid()
	}
	var elem catalog.OID
	for i, d := range c.args {
		if i >= len(actual) || actual[i] == catalog.Unknown {
			continue
		}
		switch d {
		case catalog.AnyElement, catalog.AnyNonArray, catalog.AnyEnum, catalog.AnyCompatible, catalog.AnyCompatibleNonArray:
			elem = a.baseType(actual[i])
		case catalog.AnyArray, catalog.AnyCompatibleArray:
			if t := a.typ(a.baseType(actual[i])); t != nil {
				elem = t.Elem
			}
		}
	}
	for i, e := range es {
		if i >= len(c.args) {
			break
		}
		to := c.args[i]
		if t := a.typ(to); t != nil && t.IsPolymorphic() {
			to = a.polymorphicArgType(to, 0, elem)
			if to == 0 {
				if e.oid() == catalog.Unknown {
					to = catalog.Text // PG resolves unknown at an unresolvable polymorphic slot to text
				} else {
					continue
				}
			}
		}
		if err := a.bind(e, to, loc); err != nil {
			return err
		}
	}
	return nil
}

func (a *analyzer) argOIDs(es []*expr) []catalog.OID {
	out := make([]catalog.OID, len(es))
	for i, e := range es {
		out[i] = e.oid()
	}
	return out
}

// typeNames renders types for error messages the way PG does (format_type, unknown as "unknown").
func (a *analyzer) typeNames(oids []catalog.OID) string {
	parts := make([]string, len(oids))
	for i, o := range oids {
		parts[i] = a.s.Types.Format(ref(o))
	}
	return strings.Join(parts, ", ")
}

func (a *analyzer) analyzeExpr(n *pg_query.Node, sc *scope) (*expr, *Error) {
	if n == nil {
		return nil, errAt(codeSyntaxError, -1, "missing expression")
	}
	switch v := n.Node.(type) {
	case *pg_query.Node_AConst:
		return a.constExpr(v.AConst, n), nil
	case *pg_query.Node_ParamRef:
		num := v.ParamRef.Number
		if num > a.maxParam {
			a.maxParam = num
		}
		e := &expr{typ: unknownRef(), nullable: true, node: n, param: num}
		if t, ok := a.params[num]; ok && t != catalog.Unknown {
			e.typ = ref(t)
			e.param = 0
		} else if _, ok := a.params[num]; !ok {
			a.params[num] = catalog.Unknown
		}
		return e, nil
	case *pg_query.Node_ColumnRef:
		return a.columnRef(v.ColumnRef, sc)
	case *pg_query.Node_TypeCast:
		return a.typeCast(v.TypeCast, sc)
	case *pg_query.Node_AExpr:
		return a.aExpr(v.AExpr, sc)
	case *pg_query.Node_FuncCall:
		return a.funcCall(v.FuncCall, sc)
	case *pg_query.Node_BoolExpr:
		nullable := false
		for _, arg := range v.BoolExpr.Args {
			e, err := a.analyzeExpr(arg, sc)
			if err != nil {
				return nil, err
			}
			if err := a.bind(e, catalog.Bool, v.BoolExpr.Location); err != nil {
				return nil, err
			}
			if !a.canCoerce(e.oid(), catalog.Bool, implicitCoercion) {
				return nil, errAt(codeDatatypeMismatch, loc(arg), "argument of %s must be type boolean, not type %s", boolOpName(v.BoolExpr.Boolop), a.s.Types.Format(e.typ))
			}
			nullable = nullable || e.nullable
		}
		return &expr{typ: ref(catalog.Bool), nullable: nullable, node: n}, nil
	case *pg_query.Node_NullTest:
		if _, err := a.analyzeExpr(v.NullTest.Arg, sc); err != nil {
			return nil, err
		}
		return &expr{typ: ref(catalog.Bool), node: n}, nil
	case *pg_query.Node_BooleanTest:
		e, err := a.analyzeExpr(v.BooleanTest.Arg, sc)
		if err != nil {
			return nil, err
		}
		if err := a.bind(e, catalog.Bool, v.BooleanTest.Location); err != nil {
			return nil, err
		}
		return &expr{typ: ref(catalog.Bool), node: n}, nil
	case *pg_query.Node_CaseExpr:
		return a.caseExpr(v.CaseExpr, sc)
	case *pg_query.Node_CoalesceExpr:
		es, err := a.analyzeList(v.CoalesceExpr.Args, sc)
		if err != nil {
			return nil, err
		}
		t, err := a.unify(es, v.CoalesceExpr.Location, "COALESCE")
		if err != nil {
			return nil, err
		}
		nullable := true
		for _, e := range es {
			if !e.nullable {
				nullable = false
			}
		}
		return &expr{typ: t, nullable: nullable, node: n}, nil
	case *pg_query.Node_MinMaxExpr:
		es, err := a.analyzeList(v.MinMaxExpr.Args, sc)
		if err != nil {
			return nil, err
		}
		name := "GREATEST"
		if v.MinMaxExpr.Op == pg_query.MinMaxOp_IS_LEAST {
			name = "LEAST"
		}
		t, err := a.unify(es, v.MinMaxExpr.Location, name)
		if err != nil {
			return nil, err
		}
		nullable := true
		for _, e := range es {
			if !e.nullable {
				nullable = false
			}
		}
		return &expr{typ: t, nullable: nullable, node: n}, nil
	case *pg_query.Node_AArrayExpr:
		es, err := a.analyzeList(v.AArrayExpr.Elements, sc)
		if err != nil {
			return nil, err
		}
		if len(es) == 0 {
			return nil, errAt(codeIndeterminateDatatype, v.AArrayExpr.Location, "cannot determine type of empty array")
		}
		// nested ARRAY[ARRAY[..]] : elements are arrays; the result is the same array type
		t, err := a.unify(es, v.AArrayExpr.Location, "ARRAY")
		if err != nil {
			return nil, err
		}
		if et := a.typ(t.OID); et != nil && et.IsArray() {
			return &expr{typ: ref(t.OID), node: n}, nil
		}
		arr := a.s.Types.ArrayOf(t.OID)
		if arr == 0 {
			return nil, errAt(codeUndefinedObject, v.AArrayExpr.Location, "could not find array type for data type %s", a.s.Types.Format(t))
		}
		return &expr{typ: ref(arr), node: n}, nil
	case *pg_query.Node_RowExpr:
		args, err := a.analyzeList(v.RowExpr.Args, sc)
		if err != nil {
			return nil, err
		}
		var fields []rteCol
		for i, e := range args {
			if e.oid() == catalog.Unknown {
				if err := a.bind(e, catalog.Text, loc(v.RowExpr.Args[i])); err != nil {
					return nil, err
				}
			}
			fields = append(fields, rteCol{name: "f" + strconv.Itoa(i+1), typ: e.typ, nullable: e.nullable, src: e.src, fields: e.fields})
		}
		return &expr{typ: ref(catalog.Record), node: n, fields: fields}, nil
	case *pg_query.Node_SubLink:
		return a.subLink(v.SubLink, sc)
	case *pg_query.Node_AIndirection:
		return a.indirection(v.AIndirection, sc)
	case *pg_query.Node_SqlvalueFunction:
		return &expr{typ: ref(sqlValueType(v.SqlvalueFunction.Op)), node: n}, nil
	case *pg_query.Node_CollateClause:
		e, err := a.analyzeExpr(v.CollateClause.Arg, sc)
		if err != nil {
			return nil, err
		}
		e.node = n
		return e, nil
	case *pg_query.Node_NamedArgExpr:
		return a.analyzeExpr(v.NamedArgExpr.Arg, sc)
	case *pg_query.Node_GroupingFunc:
		return &expr{typ: ref(catalog.Int4), node: n}, nil
	case *pg_query.Node_SetToDefault:
		return &expr{typ: unknownRef(), nullable: true, node: n}, nil
	case *pg_query.Node_List:
		return nil, errAt(codeSyntaxError, -1, "unexpected list expression")
	}
	return nil, errAt(codeFeatureNotSupported, loc(n), "unsupported expression %T", n.Node)
}

func (a *analyzer) analyzeList(nodes []*pg_query.Node, sc *scope) ([]*expr, *Error) {
	out := make([]*expr, 0, len(nodes))
	for _, n := range nodes {
		e, err := a.analyzeExpr(n, sc)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, nil
}

// unify applies select_common_type and binds unknown inputs to the result (§10.5).
func (a *analyzer) unify(es []*expr, at int32, context string) (schema.TypeRef, *Error) {
	oids := a.argOIDs(es)
	t, ok := a.commonType(oids)
	if !ok {
		return schema.TypeRef{}, errAt(codeDatatypeMismatch, at, "%s types %s cannot be matched", context, a.typeNames(oids))
	}
	typmod := int32(-2)
	for _, e := range es {
		if e.oid() == catalog.Unknown {
			typmod = -1 // an untyped literal / parameter has typmod -1 and breaks agreement
			continue
		}
		if e.oid() != t {
			typmod = -1
		} else if typmod == -2 {
			typmod = e.typ.Typmod
		} else if typmod != e.typ.Typmod {
			typmod = -1
		}
	}
	if typmod == -2 {
		typmod = -1
	}
	for _, e := range es {
		if err := a.bind(e, t, at); err != nil {
			return schema.TypeRef{}, err
		}
	}
	t = a.domainUnify(es, t, at, context)
	return schema.TypeRef{OID: t, Typmod: typmod}, nil
}

func (a *analyzer) constExpr(c *pg_query.A_Const, n *pg_query.Node) *expr {
	if c.Isnull {
		return &expr{typ: unknownRef(), nullable: true, node: n, lit: true}
	}
	switch v := c.Val.(type) {
	case *pg_query.A_Const_Ival:
		return &expr{typ: ref(catalog.Int4), node: n, lit: true}
	case *pg_query.A_Const_Fval:
		s := v.Fval.GetFval()
		if !strings.ContainsAny(s, ".eE") {
			if _, err := strconv.ParseInt(s, 10, 64); err == nil {
				return &expr{typ: ref(catalog.Int8), node: n, lit: true}
			}
		}
		return &expr{typ: ref(catalog.Numeric), node: n, lit: true}
	case *pg_query.A_Const_Boolval:
		return &expr{typ: ref(catalog.Bool), node: n, lit: true}
	case *pg_query.A_Const_Bsval:
		return &expr{typ: ref(a.s.Types.Lookup("pg_catalog", "bit").OID), node: n, lit: true}
	default: // string
		return &expr{typ: unknownRef(), node: n, lit: true}
	}
}

func (a *analyzer) columnRef(c *pg_query.ColumnRef, sc *scope) (*expr, *Error) {
	var names []string
	star := false
	for _, f := range c.Fields {
		if f.GetAStar() != nil {
			star = true
		} else {
			names = append(names, f.GetString_().GetSval())
		}
	}
	if star {
		return nil, errAt(codeSyntaxError, c.Location, "improper use of \"*\"")
	}
	var tbl, col string
	switch len(names) {
	case 1:
		col = names[0]
	case 2:
		tbl, col = names[0], names[1]
	case 3:
		tbl, col = names[1], names[2]
	default:
		return nil, errAt(codeSyntaxError, c.Location, "improper qualified name (too many dotted names): %s", strings.Join(names, "."))
	}
	rc, err := a.resolveColumn(sc, tbl, col, c.Location)
	if err != nil {
		if tbl == "" {
			if r := sc.wholeRow(col); r != nil && r.rowType != 0 {
				return &expr{typ: ref(r.rowType), node: nodeOf(c), fields: r.cols}, nil
			}
			// a SQL function's parameter (a column of the same name takes precedence)
			for _, p := range a.funcParams {
				if p.name == col {
					return &expr{typ: p.typ, nullable: true, node: nodeOf(c)}, nil
				}
			}
		} else if r := sc.wholeRow(tbl); r != nil {
			// t.field where field is a composite column's field? not supported; fall through
		}
		return nil, err
	}
	return &expr{typ: rc.typ, nullable: rc.nullable, src: rc.src, node: nodeOf(c), fields: rc.fields}, nil
}

func (a *analyzer) typeCast(tc *pg_query.TypeCast, sc *scope) (*expr, *Error) {
	target, rerr := a.s.ResolveType(tc.TypeName)
	if rerr != nil {
		return nil, errAt(codeUndefinedObject, tc.TypeName.Location, "%v", rerr)
	}
	// ARRAY[]::type[] is allowed
	if arr := tc.Arg.GetAArrayExpr(); arr != nil && len(arr.Elements) == 0 {
		return &expr{typ: target, node: nodeOf(tc)}, nil
	}
	e, err := a.analyzeExpr(tc.Arg, sc)
	if err != nil {
		return nil, err
	}
	if e.oid() == catalog.Unknown {
		if err := a.bind(e, target.OID, tc.Location); err != nil {
			return nil, err
		}
	} else if !a.canCoerce(e.oid(), target.OID, explicitCoercion) {
		return nil, errAt(codeCannotCoerce, tc.Location, "cannot cast type %s to %s", a.s.Types.Format(e.typ), a.s.Types.Format(target))
	}
	// `$1::bigint` is still a unitless value; `$1::yen` asserts the unit
	return &expr{typ: target, nullable: e.nullable, node: nodeOf(tc), lit: isLit(e) && a.domainType(target.OID) == nil}, nil
}

func (a *analyzer) opName(nodes []*pg_query.Node) string {
	parts := strs(nodes)
	return parts[len(parts)-1]
}

func (a *analyzer) aExpr(x *pg_query.A_Expr, sc *scope) (*expr, *Error) {
	self := nodeOf(x)
	switch x.Kind {
	case pg_query.A_Expr_Kind_AEXPR_OP, pg_query.A_Expr_Kind_AEXPR_LIKE, pg_query.A_Expr_Kind_AEXPR_ILIKE, pg_query.A_Expr_Kind_AEXPR_SIMILAR:
		name := a.opName(x.Name)
		var l *expr
		var err *Error
		if x.Lexpr != nil {
			if l, err = a.analyzeExpr(x.Lexpr, sc); err != nil {
				return nil, err
			}
		}
		r, err := a.analyzeExpr(x.Rexpr, sc)
		if err != nil {
			return nil, err
		}
		if x.Kind == pg_query.A_Expr_Kind_AEXPR_SIMILAR {
			// x SIMILAR TO y is  x ~ similar_to_escape(y)
			if err := a.bind(r, catalog.Text, x.Location); err != nil {
				return nil, err
			}
			r = &expr{typ: ref(catalog.Text), nullable: r.nullable}
			name = "~"
		}
		return a.applyOperator(name, l, r, x.Location, self)
	case pg_query.A_Expr_Kind_AEXPR_OP_ANY, pg_query.A_Expr_Kind_AEXPR_OP_ALL:
		name := a.opName(x.Name)
		l, err := a.analyzeExpr(x.Lexpr, sc)
		if err != nil {
			return nil, err
		}
		r, err := a.analyzeExpr(x.Rexpr, sc)
		if err != nil {
			return nil, err
		}
		var elem catalog.OID = catalog.Unknown
		if r.oid() != catalog.Unknown {
			rt := a.typ(a.baseType(r.oid()))
			if rt == nil || !rt.IsArray() {
				return nil, errAt(codeDatatypeMismatch, x.Location, "op ANY/ALL (array) requires array on right side")
			}
			elem = rt.Elem
		}
		re := &expr{typ: ref(elem), nullable: true, src: r.src, lit: isLit(r)}
		_, err = a.applyOperator(name, l, re, x.Location, self)
		if err != nil {
			return nil, err
		}
		a.noteParamSource(r, l)
		if r.oid() == catalog.Unknown {
			if err := a.bind(r, a.s.Types.ArrayOf(re.oid()), x.Location); err != nil {
				return nil, err
			}
		}
		return &expr{typ: ref(catalog.Bool), nullable: l.nullable || r.nullable, node: self}, nil
	case pg_query.A_Expr_Kind_AEXPR_IN:
		l, err := a.analyzeExpr(x.Lexpr, sc)
		if err != nil {
			return nil, err
		}
		items, err := a.analyzeList(x.Rexpr.GetList().GetItems(), sc)
		if err != nil {
			return nil, err
		}
		all := append([]*expr{l}, items...)
		if _, ok := a.commonType(a.argOIDs(all)); ok {
			if _, err := a.unify(all, x.Location, "IN"); err != nil {
				return nil, err
			}
		}
		name := a.opName(x.Name)
		for _, it := range items {
			if _, err := a.applyOperator(name, l, it, x.Location, self); err != nil {
				return nil, err
			}
		}
		return &expr{typ: ref(catalog.Bool), nullable: true, node: self}, nil
	case pg_query.A_Expr_Kind_AEXPR_BETWEEN, pg_query.A_Expr_Kind_AEXPR_NOT_BETWEEN,
		pg_query.A_Expr_Kind_AEXPR_BETWEEN_SYM, pg_query.A_Expr_Kind_AEXPR_NOT_BETWEEN_SYM:
		l, err := a.analyzeExpr(x.Lexpr, sc)
		if err != nil {
			return nil, err
		}
		bounds, err := a.analyzeList(x.Rexpr.GetList().GetItems(), sc)
		if err != nil {
			return nil, err
		}
		if _, err := a.applyOperator(">=", l, bounds[0], x.Location, self); err != nil {
			return nil, err
		}
		if _, err := a.applyOperator("<=", l, bounds[1], x.Location, self); err != nil {
			return nil, err
		}
		return &expr{typ: ref(catalog.Bool), nullable: true, node: self}, nil
	case pg_query.A_Expr_Kind_AEXPR_DISTINCT, pg_query.A_Expr_Kind_AEXPR_NOT_DISTINCT:
		l, err := a.analyzeExpr(x.Lexpr, sc)
		if err != nil {
			return nil, err
		}
		r, err := a.analyzeExpr(x.Rexpr, sc)
		if err != nil {
			return nil, err
		}
		if _, err := a.applyOperator("=", l, r, x.Location, self); err != nil {
			return nil, err
		}
		return &expr{typ: ref(catalog.Bool), node: self}, nil
	case pg_query.A_Expr_Kind_AEXPR_NULLIF:
		l, err := a.analyzeExpr(x.Lexpr, sc)
		if err != nil {
			return nil, err
		}
		r, err := a.analyzeExpr(x.Rexpr, sc)
		if err != nil {
			return nil, err
		}
		if _, err := a.applyOperator("=", l, r, x.Location, self); err != nil {
			return nil, err
		}
		return &expr{typ: l.typ, nullable: true, node: self}, nil
	}
	return nil, errAt(codeFeatureNotSupported, x.Location, "unsupported operator expression kind %v", x.Kind)
}

// applyOperator resolves name(l, r) (l nil for prefix), binds unknowns, returns the result expr.
func (a *analyzer) applyOperator(name string, l, r *expr, at int32, self *pg_query.Node) (*expr, *Error) {
	var left catalog.OID
	if l != nil {
		left = l.oid()
	}
	c := a.resolveOperator(name, left, r.oid())
	if c == nil {
		if l != nil && l.oid() == catalog.Unknown && r.oid() == catalog.Unknown {
			return nil, errAt(codeIndeterminateDatatype, at, "could not determine data type of parameter $%d", firstParam(l, r))
		}
		if l != nil {
			return nil, errAt(codeUndefinedFunction, at, "operator does not exist: %s %s %s", a.s.Types.Format(l.typ), name, a.s.Types.Format(r.typ))
		}
		return nil, errAt(codeUndefinedFunction, at, "operator does not exist: %s %s", name, a.s.Types.Format(r.typ))
	}
	es := []*expr{r}
	if l != nil {
		es = []*expr{l, r}
	}
	if err := a.bindArgs(es, c, at); err != nil {
		return nil, err
	}
	// a parameter compared with a column stands for that column's identity
	if l != nil {
		a.noteParamSource(l, r)
		a.noteParamSource(r, l)
	}
	res := c.op.Result
	if t := a.typ(res); t != nil && t.IsPolymorphic() {
		rr, ok := a.resolvePolymorphic(c.args, a.argOIDs(es), res)
		if !ok {
			return nil, errAt(codeDatatypeMismatch, at, "could not determine polymorphic type for operator %s", name)
		}
		res = rr
	}
	res = a.domainOp(name, l, r, res, at)
	switch name {
	case "<", ">", "<=", ">=":
		if t := a.typ(a.baseType(r.oid())); l != nil && t != nil && t.Kind == 'e' {
			a.note(noteEnumOrder, at, "enum "+t.Name+" compares in declaration order, not alphabetically")
		}
	}
	nullable := r.nullable || (l != nil && l.nullable)
	return &expr{typ: ref(res), nullable: nullable, node: self, lit: isLit(l) && isLit(r)}, nil
}

func firstParam(es ...*expr) int32 {
	for _, e := range es {
		if e != nil && e.param > 0 {
			return e.param
		}
	}
	return 1
}

func (a *analyzer) funcCall(f *pg_query.FuncCall, sc *scope) (*expr, *Error) {
	self := nodeOf(f)
	names := strs(f.Funcname)
	schemaName, name := "", names[len(names)-1]
	if len(names) > 1 {
		schemaName = names[len(names)-2]
	}
	var args []*expr
	if f.AggStar {
		// count(*)
	} else {
		var err *Error
		if args, err = a.analyzeList(f.Args, sc); err != nil {
			return nil, err
		}
	}
	if f.AggFilter != nil {
		fe, err := a.analyzeExpr(f.AggFilter, sc)
		if err != nil {
			return nil, err
		}
		if err := a.bind(fe, catalog.Bool, f.Location); err != nil {
			return nil, err
		}
	}
	if f.Over != nil {
		for _, n := range append(append([]*pg_query.Node{}, f.Over.PartitionClause...), f.Over.OrderClause...) {
			if sb := n.GetSortBy(); sb != nil {
				n = sb.Node
			}
			if _, err := a.analyzeExpr(n, sc); err != nil {
				return nil, err
			}
		}
	}
	for _, n := range f.AggOrder {
		if _, err := a.analyzeExpr(n.GetSortBy().GetNode(), sc); err != nil {
			return nil, err
		}
	}
	actual := a.argOIDs(args)
	c, ambiguous := a.resolveFunction(schemaName, name, actual)
	if c == nil {
		// type-name(x) is a cast
		if len(args) == 1 {
			if t := a.s.Types.Lookup(schemaName, name); t != nil {
				if args[0].oid() == catalog.Unknown {
					if err := a.bind(args[0], t.OID, f.Location); err != nil {
						return nil, err
					}
				} else if !a.canCoerce(args[0].oid(), t.OID, explicitCoercion) {
					return nil, errAt(codeCannotCoerce, f.Location, "cannot cast type %s to %s", a.s.Types.Format(args[0].typ), t.Name)
				}
				return &expr{typ: ref(t.OID), nullable: args[0].nullable, node: self}, nil
			}
		}
		if ambiguous {
			return nil, errAt(codeAmbiguousFunction, f.Location, "function %s(%s) is not unique", name, a.typeNames(actual))
		}
		return nil, errAt(codeUndefinedFunction, f.Location, "function %s(%s) does not exist", name, a.typeNames(actual))
	}
	if c.ufn != nil && c.ufn.IsProc && !a.inCall {
		return nil, errAt(codeWrongObjectType, f.Location, "%s(%s) is a procedure", name, a.typeNames(actual))
	}
	if err := a.bindArgs(args, c, f.Location); err != nil {
		return nil, err
	}
	a.lastUserFunc = c.ufn
	a.lastFuncRetSet = (c.fn != nil && c.fn.RetSet) || (c.ufn != nil && c.ufn.RetSet)
	if c.fn != nil && c.fn.Kind == 'a' && f.Over == nil {
		sc.agg = true
	}
	res := c.result()
	if t := a.typ(res); t != nil && t.IsPolymorphic() {
		rr, ok := a.resolvePolymorphic(c.args, a.argOIDs(args), res)
		if !ok {
			return nil, errAt(codeDatatypeMismatch, f.Location, "could not determine polymorphic type because input has type unknown")
		}
		res = rr
	} else {
		res = a.domainFunc(name, args, res)
	}
	// an aggregate / function returning the record shape of its argument keeps the field list
	var fields []rteCol
	if rt := a.typ(res); rt != nil && (res == catalog.Record || rt.Elem == catalog.Record) {
		for _, e := range args {
			if len(e.fields) > 0 {
				fields = e.fields
				break
			}
		}
	}
	nullable := true
	switch {
	case c.fn != nil && c.fn.Kind == 'a':
		nullable = !strings.HasPrefix(name, "count")
	case c.fn != nil && c.fn.Kind == 'w':
		nullable = !(name == "row_number" || name == "rank" || name == "dense_rank" || name == "ntile" || name == "percent_rank" || name == "cume_dist")
	case c.fn != nil && c.fn.IsStrict:
		nullable = false
		for _, e := range args {
			if e.nullable {
				nullable = true
			}
		}
	case c.fn != nil && !c.fn.IsStrict && res == catalog.Bool:
	case c.ufn != nil && c.ufn.NotNull:
		nullable = false
	case c.ufn != nil && c.ufn.Strict:
		nullable = false
		for _, e := range args {
			if e.nullable {
				nullable = true
			}
		}
	}
	return &expr{typ: ref(res), nullable: nullable, node: self, fields: fields}, nil
}

func (a *analyzer) caseExpr(c *pg_query.CaseExpr, sc *scope) (*expr, *Error) {
	var arg *expr
	if c.Arg != nil {
		var err *Error
		if arg, err = a.analyzeExpr(c.Arg, sc); err != nil {
			return nil, err
		}
	}
	var results []*expr
	nullable := c.Defresult == nil
	for _, wn := range c.Args {
		w := wn.GetCaseWhen()
		cond, err := a.analyzeExpr(w.Expr, sc)
		if err != nil {
			return nil, err
		}
		if arg != nil {
			if _, err := a.applyOperator("=", arg, cond, w.Location, nil); err != nil {
				return nil, err
			}
		} else {
			if err := a.bind(cond, catalog.Bool, w.Location); err != nil {
				return nil, err
			}
			if !a.canCoerce(cond.oid(), catalog.Bool, implicitCoercion) {
				return nil, errAt(codeDatatypeMismatch, loc(w.Expr), "argument of CASE/WHEN must be type boolean, not type %s", a.s.Types.Format(cond.typ))
			}
		}
		r, err := a.analyzeExpr(w.Result, sc)
		if err != nil {
			return nil, err
		}
		results = append(results, r)
		nullable = nullable || r.nullable
	}
	if c.Defresult != nil {
		d, err := a.analyzeExpr(c.Defresult, sc)
		if err != nil {
			return nil, err
		}
		results = append(results, d)
		nullable = nullable || d.nullable
	}
	t, err := a.unify(results, c.Location, "CASE")
	if err != nil {
		return nil, err
	}
	return &expr{typ: t, nullable: nullable, node: nodeOf(c)}, nil
}

func (a *analyzer) subLink(s *pg_query.SubLink, sc *scope) (*expr, *Error) {
	sel := s.Subselect.GetSelectStmt()
	if sel == nil {
		return nil, errAt(codeFeatureNotSupported, s.Location, "unsupported subquery")
	}
	cols, err := a.selectStmt(sel, newScope(sc))
	if err != nil {
		return nil, err
	}
	self := nodeOf(s)
	switch s.SubLinkType {
	case pg_query.SubLinkType_EXISTS_SUBLINK:
		return &expr{typ: ref(catalog.Bool), node: self}, nil
	case pg_query.SubLinkType_EXPR_SUBLINK:
		if len(cols) != 1 {
			return nil, errAt(codeSyntaxError, s.Location, "subquery must return only one column")
		}
		return &expr{typ: cols[0].typ, nullable: true, node: self}, nil
	case pg_query.SubLinkType_ARRAY_SUBLINK:
		if len(cols) != 1 {
			return nil, errAt(codeSyntaxError, s.Location, "subquery must return only one column")
		}
		arr := a.s.Types.ArrayOf(cols[0].typ.OID)
		if arr == 0 {
			return nil, errAt(codeUndefinedObject, s.Location, "could not find array type for data type %s", a.s.Types.Format(cols[0].typ))
		}
		return &expr{typ: ref(arr), nullable: false, node: self}, nil
	case pg_query.SubLinkType_ANY_SUBLINK, pg_query.SubLinkType_ALL_SUBLINK:
		if len(cols) != 1 {
			return nil, errAt(codeSyntaxError, s.Location, "subquery has too many columns")
		}
		l, err := a.analyzeExpr(s.Testexpr, sc)
		if err != nil {
			return nil, err
		}
		name := "="
		if len(s.OperName) > 0 {
			name = a.opName(s.OperName)
		}
		r := &expr{typ: cols[0].typ, nullable: cols[0].nullable}
		if _, err := a.applyOperator(name, l, r, s.Location, nil); err != nil {
			return nil, err
		}
		return &expr{typ: ref(catalog.Bool), nullable: true, node: self}, nil
	}
	return nil, errAt(codeFeatureNotSupported, s.Location, "unsupported sublink type %v", s.SubLinkType)
}

func (a *analyzer) indirection(x *pg_query.A_Indirection, sc *scope) (*expr, *Error) {
	// (t).field / t.field where t is a whole-row var: pg_query gives ColumnRef with 2 fields
	// for the latter, so here we mostly see array subscripts and (rowexpr).field
	e, err := a.analyzeExpr(x.Arg, sc)
	if err != nil {
		return nil, err
	}
	cur := e.typ
	for _, ind := range x.Indirection {
		switch v := ind.Node.(type) {
		case *pg_query.Node_AIndices:
			t := a.typ(a.baseType(cur.OID))
			if t == nil || !t.IsArray() {
				return nil, errAt(codeDatatypeMismatch, loc(x.Arg), "cannot subscript type %s because it does not support subscripting", a.s.Types.Format(cur))
			}
			for _, idx := range []*pg_query.Node{v.AIndices.Lidx, v.AIndices.Uidx} {
				if idx == nil {
					continue
				}
				ie, err := a.analyzeExpr(idx, sc)
				if err != nil {
					return nil, err
				}
				if err := a.bind(ie, catalog.Int4, loc(idx)); err != nil {
					return nil, err
				}
			}
			if !v.AIndices.IsSlice {
				cur = ref(t.Elem)
			}
		case *pg_query.Node_String_:
			t := a.typ(cur.OID)
			if t == nil || t.Kind != 'c' {
				return nil, errAt(codeDatatypeMismatch, loc(x.Arg), "column notation .%s applied to type %s, which is not a composite type", v.String_.Sval, a.s.Types.Format(cur))
			}
			rel := a.relByRowType(t.OID)
			col := rel.Column(v.String_.Sval)
			if col == nil {
				return nil, errAt(codeUndefinedColumn, loc(x.Arg), "column %q not found in data type %s", v.String_.Sval, t.Name)
			}
			cur = col.Type
		default:
			return nil, errAt(codeFeatureNotSupported, loc(x.Arg), "unsupported indirection")
		}
	}
	return &expr{typ: cur, nullable: true, node: nodeOf(x)}, nil
}

func (a *analyzer) relByRowType(oid catalog.OID) *schema.Relation {
	for _, r := range a.s.Relations {
		if r.RowType == oid {
			return r
		}
	}
	return nil
}

func sqlValueType(op pg_query.SQLValueFunctionOp) catalog.OID {
	switch op {
	case pg_query.SQLValueFunctionOp_SVFOP_CURRENT_DATE:
		return catalog.Date
	case pg_query.SQLValueFunctionOp_SVFOP_CURRENT_TIME, pg_query.SQLValueFunctionOp_SVFOP_CURRENT_TIME_N:
		return catalog.TimeTZ
	case pg_query.SQLValueFunctionOp_SVFOP_CURRENT_TIMESTAMP, pg_query.SQLValueFunctionOp_SVFOP_CURRENT_TIMESTAMP_N:
		return catalog.TimestampTZ
	case pg_query.SQLValueFunctionOp_SVFOP_LOCALTIME, pg_query.SQLValueFunctionOp_SVFOP_LOCALTIME_N:
		return catalog.Time
	case pg_query.SQLValueFunctionOp_SVFOP_LOCALTIMESTAMP, pg_query.SQLValueFunctionOp_SVFOP_LOCALTIMESTAMP_N:
		return catalog.Timestamp
	}
	return catalog.Name
}

func boolOpName(op pg_query.BoolExprType) string {
	switch op {
	case pg_query.BoolExprType_AND_EXPR:
		return "AND"
	case pg_query.BoolExprType_OR_EXPR:
		return "OR"
	}
	return "NOT"
}

func strs(nodes []*pg_query.Node) []string {
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, n.GetString_().GetSval())
	}
	return out
}

// loc returns the 0-based location of a node, or -1.
func loc(n *pg_query.Node) int32 {
	if n == nil {
		return -1
	}
	switch v := n.Node.(type) {
	case *pg_query.Node_AConst:
		return v.AConst.Location
	case *pg_query.Node_ParamRef:
		return v.ParamRef.Location
	case *pg_query.Node_ColumnRef:
		return v.ColumnRef.Location
	case *pg_query.Node_TypeCast:
		return v.TypeCast.Location
	case *pg_query.Node_AExpr:
		return v.AExpr.Location
	case *pg_query.Node_FuncCall:
		return v.FuncCall.Location
	case *pg_query.Node_BoolExpr:
		return v.BoolExpr.Location
	case *pg_query.Node_CaseExpr:
		return v.CaseExpr.Location
	case *pg_query.Node_SubLink:
		return v.SubLink.Location
	case *pg_query.Node_AArrayExpr:
		return v.AArrayExpr.Location
	case *pg_query.Node_ResTarget:
		return v.ResTarget.Location
	case *pg_query.Node_RangeVar:
		return v.RangeVar.Location
	case *pg_query.Node_CoalesceExpr:
		return v.CoalesceExpr.Location
	}
	return -1
}

// nodeOf wraps a concrete AST message back into a Node (for name figuring).
func nodeOf(m any) *pg_query.Node {
	switch v := m.(type) {
	case *pg_query.ColumnRef:
		return &pg_query.Node{Node: &pg_query.Node_ColumnRef{ColumnRef: v}}
	case *pg_query.TypeCast:
		return &pg_query.Node{Node: &pg_query.Node_TypeCast{TypeCast: v}}
	case *pg_query.A_Expr:
		return &pg_query.Node{Node: &pg_query.Node_AExpr{AExpr: v}}
	case *pg_query.FuncCall:
		return &pg_query.Node{Node: &pg_query.Node_FuncCall{FuncCall: v}}
	case *pg_query.CaseExpr:
		return &pg_query.Node{Node: &pg_query.Node_CaseExpr{CaseExpr: v}}
	case *pg_query.SubLink:
		return &pg_query.Node{Node: &pg_query.Node_SubLink{SubLink: v}}
	case *pg_query.A_Indirection:
		return &pg_query.Node{Node: &pg_query.Node_AIndirection{AIndirection: v}}
	}
	return nil
}

// noteParamSource records that parameter e (if it is one) met column other.
func (a *analyzer) noteParamSource(e, other *expr) {
	if e != nil && e.param > 0 && other != nil && other.src != nil {
		if _, done := a.paramSrc[e.param]; !done {
			a.paramSrc[e.param] = other.src
		}
	}
}
