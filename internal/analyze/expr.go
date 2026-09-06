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
	// fparam is the 1-based position of the SQL function parameter this expression is,
	// while a function body is analyzed (a NOT NULL violation blames it on the call's argument)
	fparam int32
	// lit marks a constant (or an expression built only from constants and parameters):
	// it carries no domain of its own, see domain.go
	lit bool
	// fields describes an anonymous record (row(...), whole-row reference) or an array of
	// them, so nested Go structs can be checked positionally
	fields []rteCol
	// coll is the collation and its derivation, see collation.go
	coll collation
}

func unknownRef() schema.TypeRef         { return schema.TypeRef{OID: catalog.Unknown, Typmod: -1} }
func ref(oid catalog.OID) schema.TypeRef { return schema.TypeRef{OID: oid, Typmod: -1} }

func (e expr) oid() catalog.OID { return e.typ.OID }

// bind gives an unknown-typed expression (untyped literal or $n) the type the context demands.
// For $n this records the parameter type; conflicting demands are an error.
func (a *analyzer) bind(e *expr, to catalog.OID, loc int32) *Error {
	return a.bindTypmod(e, ref(to), loc)
}

// bindTypmod is bind with the target's typmod (a cast's INTERVAL '1' YEAR range).
func (a *analyzer) bindTypmod(e *expr, target schema.TypeRef, loc int32) *Error {
	to := target.OID
	if e.oid() != catalog.Unknown || to == catalog.Unknown || to == 0 {
		return nil
	}
	if e.param > 0 {
		if err := a.setParam(e.param, to, loc); err != nil {
			return err
		}
	} else if c := e.node.GetAConst(); c != nil {
		if sv, ok := c.Val.(*pg_query.A_Const_Sval); ok {
			if err := a.validateLiteralTypmod(sv.Sval.GetSval(), to, target.Typmod, c.Location); err != nil {
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
	var elem, rng, multi catalog.OID
	var compatElems []catalog.OID // the anycompatible family resolves to one common type
	for i, d := range c.args {
		if i >= len(actual) || actual[i] == catalog.Unknown {
			continue
		}
		switch d {
		case catalog.AnyElement, catalog.AnyNonArray, catalog.AnyEnum:
			elem = a.baseType(actual[i])
		case catalog.AnyCompatible, catalog.AnyCompatibleNonArray:
			compatElems = append(compatElems, a.baseType(actual[i]))
		case catalog.AnyArray:
			if t := a.typ(a.baseType(actual[i])); t != nil {
				elem = t.Elem
			}
		case catalog.AnyCompatibleArray:
			if t := a.typ(a.baseType(actual[i])); t != nil && t.Elem != 0 {
				compatElems = append(compatElems, t.Elem)
			}
		case catalog.AnyRange:
			rng = a.baseType(actual[i])
		case catalog.AnyCompatibleRange:
			rng = a.baseType(actual[i])
			if r := a.s.Types.RangeOf(rng); r != nil {
				compatElems = append(compatElems, r.Subtype)
			}
		case catalog.AnyMultirange:
			multi = a.baseType(actual[i])
		case catalog.AnyCompatibleMultirange:
			multi = a.baseType(actual[i])
			if r := a.s.Types.RangeOfMulti(multi); r != nil {
				compatElems = append(compatElems, r.Subtype)
			}
		}
	}
	var compat catalog.OID
	if len(compatElems) > 0 {
		if ct, ok := a.commonType(compatElems); ok {
			compat = ct
		}
	}
	if compat == 0 && len(compatElems) == 0 && elem != 0 {
		compat = elem // no anycompatible argument at all: nothing to keep apart
	}
	// a range and its multirange (or subtype) determine each other
	if rng == 0 && multi != 0 {
		if r := a.s.Types.RangeOfMulti(multi); r != nil {
			rng = r.OID
		}
	}
	if rng == 0 && elem != 0 {
		if r := a.s.Types.RangeForSubtype(elem); r != nil {
			rng = r.OID
		}
	}
	if rng != 0 {
		if r := a.s.Types.RangeOf(rng); r != nil {
			if multi == 0 {
				multi = r.Multi
			}
			if elem == 0 {
				elem = r.Subtype
			}
		}
	}
	for i, e := range es {
		if i >= len(c.args) {
			break
		}
		to := c.args[i]
		if t := a.typ(to); t != nil && t.IsPolymorphic() {
			switch to {
			case catalog.AnyRange, catalog.AnyCompatibleRange:
				to = rng
			case catalog.AnyMultirange, catalog.AnyCompatibleMultirange:
				to = multi
			case catalog.AnyCompatible, catalog.AnyCompatibleNonArray:
				to = compat
			case catalog.AnyCompatibleArray:
				to = 0
				if compat != 0 {
					to = a.s.Types.ArrayOf(compat)
				}
			default:
				to = a.polymorphicArgType(to, 0, elem)
			}
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
		t, coll, err := a.unify(es, v.CoalesceExpr.Location, "COALESCE")
		if err != nil {
			return nil, err
		}
		nullable := true
		for _, e := range es {
			if !e.nullable {
				nullable = false
			}
		}
		return &expr{typ: t, nullable: nullable, node: n, coll: coll}, nil
	case *pg_query.Node_MinMaxExpr:
		es, err := a.analyzeList(v.MinMaxExpr.Args, sc)
		if err != nil {
			return nil, err
		}
		name := "GREATEST"
		if v.MinMaxExpr.Op == pg_query.MinMaxOp_IS_LEAST {
			name = "LEAST"
		}
		t, coll, err := a.unify(es, v.MinMaxExpr.Location, name)
		if err != nil {
			return nil, err
		}
		a.noteCollConflict(coll, v.MinMaxExpr.Location, name)
		nullable := true
		for _, e := range es {
			if !e.nullable {
				nullable = false
			}
		}
		return &expr{typ: t, nullable: nullable, node: n, coll: coll}, nil
	case *pg_query.Node_AArrayExpr:
		es, err := a.analyzeList(v.AArrayExpr.Elements, sc)
		if err != nil {
			return nil, err
		}
		if len(es) == 0 {
			return nil, errAt(codeIndeterminateDatatype, v.AArrayExpr.Location, "cannot determine type of empty array")
		}
		// nested ARRAY[ARRAY[..]]: written arrays as elements make one multidimensional
		// array of the same type; an array-typed element written any other way is an
		// element (int2vector[] over int2vector), and only when no such array type exists
		// does the result stay the element's array type
		t, coll, err := a.unify(es, v.AArrayExpr.Location, "ARRAY")
		if err != nil {
			return nil, err
		}
		written := false
		for _, el := range v.AArrayExpr.Elements {
			if el.GetAArrayExpr() != nil {
				written = true
			}
		}
		if et := a.typ(t.OID); et != nil && et.IsArray() && (written || a.s.Types.ArrayOf(t.OID) == 0) {
			return &expr{typ: ref(t.OID), node: n, coll: coll}, nil
		}
		arr := a.s.Types.ArrayOf(t.OID)
		if arr == 0 {
			return nil, errAt(codeUndefinedObject, v.AArrayExpr.Location, "could not find array type for data type %s", a.s.Types.Format(t))
		}
		return &expr{typ: ref(arr), node: n, coll: coll}, nil
	case *pg_query.Node_RowExpr:
		args, err := a.analyzeList(v.RowExpr.Args, sc)
		if err != nil {
			return nil, err
		}
		var fields []rteCol
		for i, e := range args {
			lit := false
			if e.oid() == catalog.Unknown {
				// an unknown literal field becomes text, but stays coercible to whatever a
				// row(...)::composite cast asks of that position
				lit = e.lit
				if err := a.bind(e, catalog.Text, loc(v.RowExpr.Args[i])); err != nil {
					return nil, err
				}
			}
			fields = append(fields, rteCol{name: "f" + strconv.Itoa(i+1), typ: e.typ, nullable: e.nullable, src: e.src, fields: e.fields, lit: lit})
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
		if err := a.explicitCollate(e, strs(v.CollateClause.Collname), v.CollateClause.Location); err != nil {
			return nil, err
		}
		e.node = n
		return e, nil
	case *pg_query.Node_NamedArgExpr:
		return a.analyzeExpr(v.NamedArgExpr.Arg, sc)
	case *pg_query.Node_GroupingFunc:
		return &expr{typ: ref(catalog.Int4), node: n}, nil
	case *pg_query.Node_JsonObjectConstructor:
		return a.jsonConstructorList(v.JsonObjectConstructor.Exprs, v.JsonObjectConstructor.Output, sc, n)
	case *pg_query.Node_JsonArrayConstructor:
		return a.jsonConstructorList(v.JsonArrayConstructor.Exprs, v.JsonArrayConstructor.Output, sc, n)
	case *pg_query.Node_JsonArrayQueryConstructor:
		q := v.JsonArrayQueryConstructor
		if sel := q.Query.GetSelectStmt(); sel != nil {
			cols, err := a.selectStmt(sel, newScope(sc))
			if err != nil {
				return nil, err
			}
			if len(cols) != 1 {
				return nil, errAt(codeSyntaxError, q.Location, "subquery must return only one column")
			}
		}
		t, err := a.jsonOutput(q.Output, catalog.JSON)
		if err != nil {
			return nil, err
		}
		return &expr{typ: t, node: n}, nil
	case *pg_query.Node_JsonArrayAgg:
		ag := v.JsonArrayAgg
		return a.jsonAgg(ag.Constructor, sc, n, func() *Error { _, err := a.jsonValue(ag.Arg, sc); return err })
	case *pg_query.Node_JsonObjectAgg:
		ag := v.JsonObjectAgg
		return a.jsonAgg(ag.Constructor, sc, n, func() *Error {
			k, err := a.analyzeExpr(ag.Arg.Key, sc)
			if err != nil {
				return err
			}
			if err := a.bind(k, catalog.Text, loc(ag.Arg.Key)); err != nil {
				return err
			}
			_, err = a.jsonValue(ag.Arg.Value, sc)
			return err
		})
	case *pg_query.Node_JsonFuncExpr:
		return a.jsonFuncExpr(v.JsonFuncExpr, sc, n)
	case *pg_query.Node_JsonParseExpr:
		if _, err := a.jsonValue(v.JsonParseExpr.Expr, sc); err != nil {
			return nil, err
		}
		t, err := a.jsonOutput(v.JsonParseExpr.Output, catalog.JSON)
		if err != nil {
			return nil, err
		}
		return &expr{typ: t, nullable: true, node: n}, nil
	case *pg_query.Node_JsonScalarExpr:
		if _, err := a.analyzeExpr(v.JsonScalarExpr.Expr, sc); err != nil {
			return nil, err
		}
		t, err := a.jsonOutput(v.JsonScalarExpr.Output, catalog.JSON)
		if err != nil {
			return nil, err
		}
		return &expr{typ: t, nullable: true, node: n}, nil
	case *pg_query.Node_JsonSerializeExpr:
		if _, err := a.jsonValue(v.JsonSerializeExpr.Expr, sc); err != nil {
			return nil, err
		}
		t, err := a.jsonOutput(v.JsonSerializeExpr.Output, catalog.Text)
		if err != nil {
			return nil, err
		}
		return &expr{typ: t, nullable: true, node: n}, nil
	case *pg_query.Node_XmlExpr:
		return a.xmlExpr(v.XmlExpr, sc, n)
	case *pg_query.Node_XmlSerialize:
		return a.xmlSerialize(v.XmlSerialize, sc, n)
	case *pg_query.Node_CurrentOfExpr:
		return &expr{typ: ref(catalog.Bool), node: n}, nil
	case *pg_query.Node_MergeSupportFunc:
		// merge_action() in MERGE ... RETURNING (PG 17)
		if !a.inMerge {
			return nil, errAt(codeSyntaxError, v.MergeSupportFunc.Location, "MERGE_ACTION() can only be used in the RETURNING list of a MERGE command")
		}
		return &expr{typ: ref(catalog.Text), node: n}, nil
	case *pg_query.Node_JsonIsPredicate:
		e, err := a.analyzeExpr(v.JsonIsPredicate.Expr, sc)
		if err != nil {
			return nil, err
		}
		if err := a.bind(e, catalog.Text, v.JsonIsPredicate.Location); err != nil {
			return nil, err
		}
		switch a.baseType(e.oid()) {
		case catalog.Text, catalog.JSON, catalog.JSONB, catalog.Bytea, catalog.Varchar, catalog.BPChar, catalog.Name:
		default:
			return nil, errAt(codeDatatypeMismatch, v.JsonIsPredicate.Location, "cannot use type %s in IS JSON predicate", a.s.Types.Format(e.typ))
		}
		return &expr{typ: ref(catalog.Bool), nullable: e.nullable, node: n}, nil
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
func (a *analyzer) unify(es []*expr, at int32, context string) (schema.TypeRef, collation, *Error) {
	oids := a.argOIDs(es)
	t, ok := a.commonType(oids)
	if !ok {
		return schema.TypeRef{}, collation{}, errAt(codeDatatypeMismatch, at, "%s types %s cannot be matched", context, a.typeNames(oids))
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
			return schema.TypeRef{}, collation{}, err
		}
	}
	t = a.domainUnify(es, t, at, context)
	c, err := a.collOf(es)
	if err != nil {
		return schema.TypeRef{}, collation{}, err
	}
	return schema.TypeRef{OID: t, Typmod: typmod}, a.resultColl(c, t), nil
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
		if len(names) > 0 {
			if r := sc.wholeRow(names[len(names)-1]); r != nil {
				if r.rowType != 0 {
					return &expr{typ: ref(r.rowType), node: nodeOf(c), fields: r.cols}, nil
				}
				return &expr{typ: ref(catalog.Record), node: nodeOf(c), fields: r.expand()}, nil
			}
		}
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
			if r := sc.wholeRow(col); r != nil {
				if r.rowType != 0 {
					return &expr{typ: ref(r.rowType), node: nodeOf(c), fields: r.cols}, nil
				}
				if r.join == nil {
					return &expr{typ: ref(catalog.Record), node: nodeOf(c), fields: r.cols}, nil
				}
			}
			// a SQL function's parameter (a column of the same name takes precedence)
			for i, p := range a.funcParams {
				if p.name == col {
					return &expr{typ: p.typ, nullable: true, node: nodeOf(c), fparam: int32(i + 1)}, nil
				}
			}
		} else if r := sc.wholeRow(tbl); r != nil {
			// t.field where field is a composite column's field? not supported; fall through
		}
		return nil, err
	}
	e := &expr{typ: rc.typ, nullable: rc.nullable, src: rc.src, node: nodeOf(c), fields: rc.fields, coll: rc.coll.asVar()}
	if e.coll.strength == collNone && a.collatable(rc.typ.OID) {
		e.coll = collation{strength: collImplicit, loc: c.Location}
	}
	return e, nil
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
		// coerce_type: the input function sees the typmod only for interval; other types
		// are read unconstrained and then length-coerced, which an explicit cast never fails
		inputTarget := target
		if a.baseType(target.OID) != catalog.Interval {
			inputTarget.Typmod = -1
		}
		if err := a.bindTypmod(e, inputTarget, tc.Location); err != nil {
			return nil, err
		}
	} else if tt := a.typ(a.baseType(target.OID)); e.oid() == catalog.Record && len(e.fields) > 0 && tt != nil && tt.Kind == 'c' {
		// row(a, b)::composite (coerce_record_to_complex): field by field, by position; a
		// domain over a composite casts through its base type
		rel := relByRowType(a.s, a.baseType(target.OID))
		if rel == nil || len(rel.Columns) != len(e.fields) {
			return nil, errAt(codeCannotCoerce, tc.Location, "cannot cast type record to %s", a.s.Types.Format(target))
		}
		var cols []rteCol
		for i, f := range e.fields {
			c := rel.Columns[i]
			if f.typ.OID != catalog.Unknown && !f.lit && !a.canCoerce(f.typ.OID, c.Type.OID, assignmentCoercion) {
				return nil, errAt(codeCannotCoerce, tc.Location, "cannot cast type %s to %s in column %d of %s", a.s.Types.Format(f.typ), a.s.Types.Format(c.Type), i+1, a.s.Types.Format(target))
			}
			cols = append(cols, rteCol{name: c.Name, typ: c.Type, nullable: f.nullable})
		}
		return &expr{typ: target, nullable: e.nullable, node: nodeOf(tc), fields: cols}, nil
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
		var r *expr
		if l != nil && l.oid() == catalog.Record && x.Rexpr.GetSubLink() != nil {
			// ROW(a, b) = (SELECT x, y): a row comparison against a multi-column subquery
			r, err = a.rowSubquery(x.Rexpr.GetSubLink(), sc)
		} else {
			r, err = a.analyzeExpr(x.Rexpr, sc)
		}
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
		re := &expr{typ: ref(elem), nullable: true, src: r.src, lit: isLit(r), coll: r.coll}
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
			if _, _, err := a.unify(all, x.Location, "IN"); err != nil {
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
	coll, cerr := a.collOf(es)
	if cerr != nil {
		return nil, cerr
	}
	if a.isCollSensitiveOp(name, es) {
		a.noteCollConflict(coll, at, "string comparison")
	}
	nullable := r.nullable || (l != nil && l.nullable)
	return &expr{typ: ref(res), nullable: nullable, node: self, lit: isLit(l) && isLit(r), coll: a.resultColl(coll, res)}, nil
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
	isAgg := f.AggStar || f.AggFilter != nil || f.AggWithinGroup || f.AggDistinct || len(f.AggOrder) > 0 || a.isAggregateName(names)
	if isAgg && f.Over == nil {
		if a.inAggArgs > 0 {
			return nil, errAt(codeGroupingError, f.Location, "aggregate function calls cannot be nested")
		}
		a.inAggArgs++
		defer func() { a.inAggArgs-- }()
	}
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
		if err := a.checkWindowDef(f.Over, sc); err != nil {
			return nil, err
		}
		for _, n := range append(append([]*pg_query.Node{}, f.Over.PartitionClause...), f.Over.OrderClause...) {
			if sb := n.GetSortBy(); sb != nil {
				n = sb.Node
			}
			if w := windowIn(n); w != nil {
				return nil, errAt(codeWindowingError, w.Location, "window functions are not allowed in window definitions")
			}
			if _, err := a.analyzeExpr(n, sc); err != nil {
				return nil, err
			}
		}
	}
	for _, n := range f.AggOrder {
		e, err := a.analyzeExpr(n.GetSortBy().GetNode(), sc)
		if err != nil {
			return nil, err
		}
		if f.AggWithinGroup {
			// an ordered-set aggregate's signature is its direct arguments followed by the
			// WITHIN GROUP (ORDER BY ...) expressions
			args = append(args, e)
		}
	}
	// f(x) with x a composite row that has a field f is the field (ParseFuncOrColumn
	// tries the projection before any function)
	if len(args) == 1 && !f.AggStar && f.AggFilter == nil && f.Over == nil && len(f.AggOrder) == 0 && schemaName == "" {
		if t := a.typ(args[0].oid()); t != nil && t.Kind == 'c' {
			if fe := a.fieldOf(args[0], name); fe != nil {
				fe.node = self
				return fe, nil
			}
		}
	}
	actual := a.argOIDs(args)
	named := make([]string, len(f.Args))
	for i, n := range f.Args {
		if na := n.GetNamedArgExpr(); na != nil {
			named[i] = na.Name
		}
	}
	c, ambiguous := a.resolveFunction(schemaName, name, actual, f.AggWithinGroup, named, f.FuncVariadic)
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
		// an ordered-set aggregate called without WITHIN GROUP (the other way round is 42883 in PG)
		for _, fn := range a.s.Catalog.FuncsByName(name) {
			if fn.Kind != 'a' || (schemaName != "" && schemaName != "pg_catalog" && fn.Schema != schemaName) {
				continue
			}
			agg := a.s.Catalog.AggregateByFn(fn.OID)
			ordered := agg != nil && agg.Kind != 'n'
			if ordered && !f.AggWithinGroup {
				return nil, errAt(codeWrongObjectType, f.Location, "WITHIN GROUP is required for ordered-set aggregate %s", name)
			}
		}
		return nil, errAt(codeUndefinedFunction, f.Location, "function %s(%s) does not exist", name, a.typeNames(actual))
	}
	if c.ufn != nil && c.ufn.IsProc && !a.inCall {
		return nil, errAt(codeWrongObjectType, f.Location, "%s(%s) is a procedure", name, a.typeNames(actual))
	}
	if err := a.bindArgs(args, c, f.Location); err != nil {
		return nil, err
	}
	a.lastCallArgs, a.lastCallActual = c.args, a.argOIDs(args)
	a.lastUserFunc = c.ufn
	if c.ufn != nil {
		a.calledFuncs = append(a.calledFuncs, calledFunc{fn: c.ufn, args: f.Args})
	}
	a.lastCatFunc = c.fn
	a.lastFuncRetSet = (c.fn != nil && c.fn.RetSet) || (c.ufn != nil && c.ufn.RetSet)
	switch {
	case c.fn != nil:
		a.funcVolatility[f] = c.fn.Volatile
	case c.ufn != nil:
		a.funcVolatility[f] = c.ufn.Volatile
	}
	if (c.fn != nil && c.fn.Kind == 'a' || c.ufn != nil && c.ufn.IsAgg) && f.Over == nil {
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
	coll, cerr := a.collOf(args)
	if cerr != nil {
		return nil, cerr
	}
	if collSensitiveFunc[name] && len(args) > 0 && a.collatable(args[0].oid()) {
		a.noteCollConflict(coll, f.Location, name+"()")
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
	case c.fn != nil && neverNullFunc[name]:
		nullable = false
	case c.fn != nil && len(c.fn.ArgTypes) == 0 && !c.fn.RetSet && !zeroArgNullable[name]:
		// a nullary function (now(), random(), gen_random_uuid(), ...) has nothing to be
		// NULL about, apart from the few that report an absent value
		nullable = false
	case c.fn != nil && !c.fn.IsStrict && res == catalog.Bool:
	case c.ufn != nil && c.ufn.IsAgg:
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
	return &expr{typ: ref(res), nullable: nullable, node: self, fields: fields, coll: a.resultColl(coll, res)}, nil
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
		var r *expr
		if arg == nil {
			// a searched CASE: the branch runs where its condition held
			r, err = a.underCondition(w.Expr, w.Result, sc)
		} else {
			r, err = a.analyzeExpr(w.Result, sc)
		}
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
	t, coll, err := a.unify(results, c.Location, "CASE")
	if err != nil {
		return nil, err
	}
	return &expr{typ: t, nullable: nullable, node: nodeOf(c), coll: coll}, nil
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
		// a scalar subquery is NULL when it yields no row; an aggregate without GROUP BY
		// always yields one, so its column's own nullability stands (coalesce(max(x), 0))
		nullable := cols[0].nullable || !a.plainAggregate(sel)
		return &expr{typ: cols[0].typ, nullable: nullable, node: self, coll: cols[0].coll.asVar()}, nil
	case pg_query.SubLinkType_ARRAY_SUBLINK:
		if len(cols) != 1 {
			return nil, errAt(codeSyntaxError, s.Location, "subquery must return only one column")
		}
		if ct := a.typ(a.baseType(cols[0].typ.OID)); ct != nil && ct.IsArray() {
			// ARRAY(SELECT int[] ...) is an int[] (array_agg semantics), not int[][]
			return &expr{typ: ref(ct.OID), nullable: false, node: self}, nil
		}
		arr := a.s.Types.ArrayOf(cols[0].typ.OID)
		if arr == 0 {
			return nil, errAt(codeUndefinedObject, s.Location, "could not find array type for data type %s", a.s.Types.Format(cols[0].typ))
		}
		return &expr{typ: ref(arr), nullable: false, node: self}, nil
	case pg_query.SubLinkType_ANY_SUBLINK, pg_query.SubLinkType_ALL_SUBLINK:
		name := "="
		if len(s.OperName) > 0 {
			name = a.opName(s.OperName)
		}
		if row := s.Testexpr.GetRowExpr(); row != nil {
			// (a, b) IN (SELECT x, y ...): compared column by column
			if len(row.Args) != len(cols) {
				if len(row.Args) > len(cols) {
					return nil, errAt(codeSyntaxError, s.Location, "subquery has too few columns")
				}
				return nil, errAt(codeSyntaxError, s.Location, "subquery has too many columns")
			}
			nullable := false
			for i, n := range row.Args {
				l, err := a.analyzeExpr(n, sc)
				if err != nil {
					return nil, err
				}
				r := &expr{typ: cols[i].typ, nullable: cols[i].nullable, coll: cols[i].coll.asVar()}
				if _, err := a.applyOperator(name, l, r, s.Location, nil); err != nil {
					return nil, err
				}
				nullable = nullable || l.nullable || r.nullable
			}
			return &expr{typ: ref(catalog.Bool), nullable: nullable, node: self}, nil
		}
		if len(cols) != 1 {
			return nil, errAt(codeSyntaxError, s.Location, "subquery has too many columns")
		}
		l, err := a.analyzeExpr(s.Testexpr, sc)
		if err != nil {
			return nil, err
		}
		r := &expr{typ: cols[0].typ, nullable: cols[0].nullable, coll: cols[0].coll.asVar()}
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
	for i := 0; i < len(x.Indirection); i++ {
		ind := x.Indirection[i]
		switch v := ind.Node.(type) {
		case *pg_query.Node_AIndices:
			t := a.typ(a.baseType(cur.OID))
			switch {
			case t != nil && t.OID == catalog.JSONB:
				// jsonb subscripting (PG 14): each subscript is a key (text) or an array index
				// (integer) and yields jsonb
				if v.AIndices.IsSlice {
					return nil, errAt(codeDatatypeMismatch, loc(x.Arg), "jsonb subscript does not support slices")
				}
				for _, idx := range []*pg_query.Node{v.AIndices.Lidx, v.AIndices.Uidx} {
					if idx == nil {
						continue
					}
					ie, err := a.analyzeExpr(idx, sc)
					if err != nil {
						return nil, err
					}
					if err := a.bind(ie, catalog.Text, loc(idx)); err != nil {
						return nil, err
					}
					if o := a.baseType(ie.oid()); o != catalog.Text && o != catalog.Int4 && !a.canCoerce(o, catalog.Text, implicitCoercion) {
						return nil, errAt(codeDatatypeMismatch, loc(idx), "subscript type %s is not supported for jsonb", a.s.Types.Format(ie.typ))
					}
				}
				cur = ref(catalog.JSONB)
			case t != nil && t.IsArray():
				// a run of subscripts is one operation: a[1][2] on int[][] (which is _int4) is
				// an int, any slice in the run keeps the array type
				slice := false
				for ; i < len(x.Indirection); i++ {
					ai := x.Indirection[i].GetAIndices()
					if ai == nil {
						break
					}
					slice = slice || ai.IsSlice
					for _, idx := range []*pg_query.Node{ai.Lidx, ai.Uidx} {
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
				}
				i--
				if !slice {
					cur = ref(t.Elem)
				}
			case t != nil && t.Elem != 0 && t.Len > 0:
				// raw_array_subscript_handler: a fixed-length type over elements (point, box,
				// name) subscripts to one element; no slices
				ai := v.AIndices
				for _, idx := range []*pg_query.Node{ai.Lidx, ai.Uidx} {
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
				if !ai.IsSlice {
					cur = ref(t.Elem) // a slice of a point is still a point
				}
			default:
				return nil, errAt(codeDatatypeMismatch, loc(x.Arg), "cannot subscript type %s because it does not support subscripting", a.s.Types.Format(cur))
			}
		case *pg_query.Node_String_:
			t := a.typ(a.baseType(cur.OID))
			if t != nil && t.OID == catalog.Record {
				// (f(x)).name on a function returning record through OUT parameters, or on a
				// row constructor / whole-row value that carries its fields
				fields := e.fields
				if len(fields) == 0 && x.Arg.GetFuncCall() != nil {
					fields = a.outParamCols()
				}
				found := false
				for _, fc := range fields {
					if fc.name == v.String_.Sval {
						cur, found = fc.typ, true
						break
					}
				}
				if !found {
					return nil, errAt(codeUndefinedColumn, loc(x.Arg), "could not identify column %q in record data type", v.String_.Sval)
				}
				break
			}
			if t == nil || t.Kind != 'c' {
				return nil, errAt(codeDatatypeMismatch, loc(x.Arg), "column notation .%s applied to type %s, which is not a composite type", v.String_.Sval, a.s.Types.Format(cur))
			}
			rel := a.relByRowType(t.OID)
			col := rel.Column(v.String_.Sval)
			if col == nil {
				found := false
				for _, fn := range a.s.Functions {
					if fn.Name == v.String_.Sval && len(fn.Args) == 1 && fn.Args[0].Type.OID == t.OID && a.s.OnSearchPath(fn.Schema) {
						cur, found = fn.RetType, true // functional notation: (x).f is f(x)
						break
					}
				}
				if found {
					break
				}
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

// neverNullFunc lists the non-strict built-ins whose result is never NULL: they treat
// NULL inputs as absent or as the string "" instead of propagating them.
var neverNullFunc = map[string]bool{
	"concat": true, "concat_ws": true, "format": true, "num_nonnulls": true, "num_nulls": true,
	"json_build_object": true, "json_build_array": true, "jsonb_build_object": true, "jsonb_build_array": true,
	"json_object": true, "jsonb_object": true, "row_to_json": true,
}

// zeroArgNullable lists the nullary built-ins that do return NULL (no value to report).
var zeroArgNullable = map[string]bool{
	"inet_client_addr": true, "inet_client_port": true, "inet_server_addr": true, "inet_server_port": true,
	"pg_last_wal_receive_lsn": true, "pg_last_wal_replay_lsn": true, "pg_last_xact_replay_timestamp": true,
	"pg_current_xact_id_if_assigned": true, "current_query": true, "pg_current_logfile": true,
}

// plainAggregate reports whether sel is a single aggregate query without GROUP BY / HAVING /
// LIMIT / set operations: it returns exactly one row.
func (a *analyzer) plainAggregate(sel *pg_query.SelectStmt) bool {
	if sel.Op != pg_query.SetOperation_SETOP_NONE || len(sel.GroupClause) > 0 || sel.HavingClause != nil ||
		sel.LimitCount != nil || sel.LimitOffset != nil || len(sel.ValuesLists) > 0 || len(sel.DistinctClause) > 0 {
		return false
	}
	agg := false
	for _, tn := range sel.TargetList {
		schema.WalkNodes(tn, func(n *pg_query.Node) {
			if f := n.GetFuncCall(); f != nil && f.Over == nil && a.isAggregateName(funcNames(f)) {
				agg = true
			}
		})
	}
	return agg
}

func funcNames(f *pg_query.FuncCall) []string {
	var names []string
	for _, n := range f.Funcname {
		names = append(names, n.GetString_().GetSval())
	}
	return names
}

// fieldOf is the field name of composite value e, nil when e's type has no such field.
func (a *analyzer) fieldOf(e *expr, name string) *expr {
	t := a.typ(e.oid())
	if t == nil || t.Kind != 'c' {
		return nil
	}
	rel := a.relByRowType(t.OID)
	if rel == nil {
		return nil
	}
	col := rel.Column(name)
	if col == nil {
		return nil
	}
	return &expr{typ: col.Type, nullable: true}
}

// rowSubquery analyzes a scalar subquery used as one side of a row comparison: its
// columns form an anonymous record (ROWCOMPARE_SUBLINK).
func (a *analyzer) rowSubquery(s *pg_query.SubLink, sc *scope) (*expr, *Error) {
	sel := s.Subselect.GetSelectStmt()
	if s.SubLinkType != pg_query.SubLinkType_EXPR_SUBLINK || sel == nil {
		return a.subLink(s, sc)
	}
	cols, err := a.selectStmt(sel, newScope(sc))
	if err != nil {
		return nil, err
	}
	if len(cols) == 1 {
		return a.subLink(s, sc)
	}
	return &expr{typ: ref(catalog.Record), nullable: true, node: nodeOf(s), fields: cols}, nil
}

// frame option bits (parsenodes.h)
const (
	frameOptionRange                = 0x00002
	frameOptionGroups               = 0x00008
	frameOptionStartOffsetPreceding = 0x00800
	frameOptionEndOffsetPreceding   = 0x01000
	frameOptionStartOffsetFollowing = 0x02000
	frameOptionEndOffsetFollowing   = 0x04000
	frameOptionOffsets              = frameOptionStartOffsetPreceding | frameOptionEndOffsetPreceding | frameOptionStartOffsetFollowing | frameOptionEndOffsetFollowing
)

// checkWindowDef resolves a named window and applies transformWindowDefinitions' frame
// rules: an offset RANGE frame needs exactly one ORDER BY column, GROUPS mode needs an
// ORDER BY at all.
func (a *analyzer) checkWindowDef(def *pg_query.WindowDef, sc *scope) *Error {
	lookup := func(name string) *pg_query.WindowDef {
		for s := sc; s != nil; s = s.parent {
			if w, ok := s.windows[name]; ok {
				return w
			}
		}
		return nil
	}
	order := def.OrderClause
	if def.Name != "" {
		w := lookup(def.Name)
		if w == nil {
			return errAt(codeUndefinedObject, def.Location, "window %q does not exist", def.Name)
		}
		def = w
		order = def.OrderClause
	}
	if def.Refname != "" {
		base := lookup(def.Refname)
		if base == nil {
			return errAt(codeUndefinedObject, def.Location, "window %q does not exist", def.Refname)
		}
		if len(order) == 0 {
			order = base.OrderClause
		}
	}
	fo := def.FrameOptions
	if fo&frameOptionRange != 0 && fo&frameOptionOffsets != 0 && len(order) != 1 {
		return errAt(codeWindowingError, def.Location, "RANGE with offset PRECEDING/FOLLOWING requires exactly one ORDER BY column")
	}
	if fo&frameOptionGroups != 0 && len(order) == 0 {
		return errAt(codeWindowingError, def.Location, "GROUPS mode requires an ORDER BY clause")
	}
	return nil
}
