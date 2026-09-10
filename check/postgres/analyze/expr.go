package analyze

import (
	"strconv"
	"strings"

	"github.com/kr9ly/sqlshape/check/postgres/catalog"
	"github.com/kr9ly/sqlshape/check/postgres/pgparse"
	"github.com/kr9ly/sqlshape/check/postgres/schema"
)

// expr is the analysis of one expression node.
type expr struct {
	typ      schema.TypeRef
	nullable bool
	src      *Source
	// rowOf is the FROM item this expression is the whole row of (a bare table alias)
	rowOf *rte
	node  *pgparse.Node
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
		if sv, ok := c.Val.(*pgparse.A_Const_Sval); ok {
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

func (a *analyzer) analyzeExpr(n *pgparse.Node, sc *scope) (*expr, *Error) {
	if n == nil {
		return nil, errAt(codeSyntaxError, -1, "missing expression")
	}
	switch v := n.Node.(type) {
	case *pgparse.Node_AConst:
		if bs := v.AConst.GetBsval(); bs != nil {
			if err := validateBitConst(bs.GetBsval(), v.AConst.Location); err != nil {
				return nil, err
			}
		}
		return a.constExpr(v.AConst, n), nil
	case *pgparse.Node_ParamRef:
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
	case *pgparse.Node_ColumnRef:
		return a.columnRef(v.ColumnRef, sc)
	case *pgparse.Node_TypeCast:
		return a.typeCast(v.TypeCast, sc)
	case *pgparse.Node_AExpr:
		return a.aExpr(v.AExpr, sc)
	case *pgparse.Node_FuncCall:
		return a.funcCall(v.FuncCall, sc)
	case *pgparse.Node_BoolExpr:
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
	case *pgparse.Node_NullTest:
		if _, err := a.analyzeExpr(v.NullTest.Arg, sc); err != nil {
			return nil, err
		}
		return &expr{typ: ref(catalog.Bool), node: n}, nil
	case *pgparse.Node_BooleanTest:
		e, err := a.analyzeExpr(v.BooleanTest.Arg, sc)
		if err != nil {
			return nil, err
		}
		if err := a.bind(e, catalog.Bool, v.BooleanTest.Location); err != nil {
			return nil, err
		}
		return &expr{typ: ref(catalog.Bool), node: n}, nil
	case *pgparse.Node_CaseExpr:
		return a.caseExpr(v.CaseExpr, sc)
	case *pgparse.Node_CoalesceExpr:
		savedBan := a.srfBan
		a.srfBan = "COALESCE"
		defer func() { a.srfBan = savedBan }()
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
	case *pgparse.Node_MinMaxExpr:
		es, err := a.analyzeList(v.MinMaxExpr.Args, sc)
		if err != nil {
			return nil, err
		}
		name := "GREATEST"
		if v.MinMaxExpr.Op == pgparse.MinMaxOp_IS_LEAST {
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
	case *pgparse.Node_AArrayExpr:
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
	case *pgparse.Node_RowExpr:
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
	case *pgparse.Node_SubLink:
		return a.subLink(v.SubLink, sc)
	case *pgparse.Node_AIndirection:
		return a.indirection(v.AIndirection, sc)
	case *pgparse.Node_SqlvalueFunction:
		return &expr{typ: ref(sqlValueType(v.SqlvalueFunction.Op)), node: n}, nil
	case *pgparse.Node_CollateClause:
		e, err := a.analyzeExpr(v.CollateClause.Arg, sc)
		if err != nil {
			return nil, err
		}
		if err := a.explicitCollate(e, strs(v.CollateClause.Collname), v.CollateClause.Location); err != nil {
			return nil, err
		}
		e.node = n
		return e, nil
	case *pgparse.Node_NamedArgExpr:
		return a.analyzeExpr(v.NamedArgExpr.Arg, sc)
	case *pgparse.Node_GroupingFunc:
		// the arguments' level (the nearest one their columns resolve to) is the grouped
		// query the GROUPING belongs to
		fr := &aggFrame{sc: sc}
		a.aggFrames = append(a.aggFrames, fr)
		_, err := a.analyzeList(v.GroupingFunc.Args, sc)
		a.aggFrames = a.aggFrames[:len(a.aggFrames)-1]
		if err != nil {
			return nil, err
		}
		owner := fr.owner
		if owner == nil {
			owner = sc.queryScope()
		}
		if !owner.grouped {
			return nil, errAt(codeGroupingError, v.GroupingFunc.Location, "arguments to GROUPING must be grouping expressions of the associated query level")
		}
		return &expr{typ: ref(catalog.Int4), node: n}, nil
	case *pgparse.Node_JsonObjectConstructor:
		return a.jsonConstructorList(v.JsonObjectConstructor.Exprs, v.JsonObjectConstructor.Output, sc, n)
	case *pgparse.Node_JsonArrayConstructor:
		return a.jsonConstructorList(v.JsonArrayConstructor.Exprs, v.JsonArrayConstructor.Output, sc, n)
	case *pgparse.Node_JsonArrayQueryConstructor:
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
	case *pgparse.Node_JsonArrayAgg:
		ag := v.JsonArrayAgg
		return a.jsonAgg(ag.Constructor, sc, n, func() *Error { _, err := a.jsonValue(ag.Arg, sc); return err })
	case *pgparse.Node_JsonObjectAgg:
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
	case *pgparse.Node_JsonFuncExpr:
		return a.jsonFuncExpr(v.JsonFuncExpr, sc, n)
	case *pgparse.Node_JsonParseExpr:
		je, err := a.jsonValue(v.JsonParseExpr.Expr, sc)
		if err != nil {
			return nil, err
		}
		if b := a.baseType(je.oid()); b != catalog.Bytea && b != catalog.JSON && b != catalog.JSONB {
			if c, _ := a.category(b); c != 'S' {
				// transformJsonParseArg: only strings, bytea and json values parse as JSON
				return nil, errAt(codeCannotCoerce, loc(v.JsonParseExpr.Expr.RawExpr), "cannot cast type %s to json", a.s.Types.Format(je.typ))
			}
		}
		if v.JsonParseExpr.UniqueKeys {
			if c, _ := a.category(a.baseType(je.oid())); c != 'S' && a.baseType(je.oid()) != catalog.Bytea {
				return nil, errAt(codeDatatypeMismatch, n.GetJsonParseExpr().Location, "cannot use non-string types with WITH UNIQUE KEYS clause")
			}
		}
		t, err := a.jsonOutput(v.JsonParseExpr.Output, catalog.JSON)
		if err != nil {
			return nil, err
		}
		return &expr{typ: t, nullable: true, node: n}, nil
	case *pgparse.Node_JsonScalarExpr:
		if _, err := a.analyzeExpr(v.JsonScalarExpr.Expr, sc); err != nil {
			return nil, err
		}
		t, err := a.jsonOutput(v.JsonScalarExpr.Output, catalog.JSON)
		if err != nil {
			return nil, err
		}
		return &expr{typ: t, nullable: true, node: n}, nil
	case *pgparse.Node_JsonSerializeExpr:
		if _, err := a.jsonValue(v.JsonSerializeExpr.Expr, sc); err != nil {
			return nil, err
		}
		t, err := a.jsonOutput(v.JsonSerializeExpr.Output, catalog.Text)
		if err != nil {
			return nil, err
		}
		if c, _ := a.category(a.baseType(t.OID)); c != 'S' && a.baseType(t.OID) != catalog.Bytea {
			return nil, errAt(codeDatatypeMismatch, v.JsonSerializeExpr.Location, "cannot use type %s in RETURNING clause of JSON_SERIALIZE()", a.s.Types.Format(t))
		}
		return &expr{typ: t, nullable: true, node: n}, nil
	case *pgparse.Node_XmlExpr:
		return a.xmlExpr(v.XmlExpr, sc, n)
	case *pgparse.Node_XmlSerialize:
		return a.xmlSerialize(v.XmlSerialize, sc, n)
	case *pgparse.Node_CurrentOfExpr:
		return &expr{typ: ref(catalog.Bool), node: n}, nil
	case *pgparse.Node_MergeSupportFunc:
		// merge_action() in MERGE ... RETURNING (PG 17)
		if !a.inMerge {
			return nil, errAt(codeSyntaxError, v.MergeSupportFunc.Location, "MERGE_ACTION() can only be used in the RETURNING list of a MERGE command")
		}
		return &expr{typ: ref(catalog.Text), node: n}, nil
	case *pgparse.Node_JsonIsPredicate:
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
			if c, _ := a.category(a.baseType(e.oid())); c != 'S' {
				return nil, errAt(codeDatatypeMismatch, v.JsonIsPredicate.Location, "cannot use type %s in IS JSON predicate", a.s.Types.Format(e.typ))
			}
		}
		return &expr{typ: ref(catalog.Bool), nullable: e.nullable, node: n}, nil
	case *pgparse.Node_SetToDefault:
		return &expr{typ: unknownRef(), nullable: true, node: n}, nil
	case *pgparse.Node_List:
		return nil, errAt(codeSyntaxError, -1, "unexpected list expression")
	}
	return nil, errAt(codeFeatureNotSupported, loc(n), "unsupported expression %T", n.Node)
}

func (a *analyzer) analyzeList(nodes []*pgparse.Node, sc *scope) ([]*expr, *Error) {
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

func (a *analyzer) constExpr(c *pgparse.A_Const, n *pgparse.Node) *expr {
	if c.Isnull {
		return &expr{typ: unknownRef(), nullable: true, node: n, lit: true}
	}
	switch v := c.Val.(type) {
	case *pgparse.A_Const_Ival:
		return &expr{typ: ref(catalog.Int4), node: n, lit: true}
	case *pgparse.A_Const_Fval:
		s := v.Fval.GetFval()
		if !strings.ContainsAny(s, ".eE") {
			if _, err := strconv.ParseInt(s, 10, 32); err == nil {
				return &expr{typ: ref(catalog.Int4), node: n, lit: true} // -2147483648, folded with its sign
			}
			if _, err := strconv.ParseInt(s, 10, 64); err == nil {
				return &expr{typ: ref(catalog.Int8), node: n, lit: true}
			}
		}
		return &expr{typ: ref(catalog.Numeric), node: n, lit: true}
	case *pgparse.A_Const_Boolval:
		return &expr{typ: ref(catalog.Bool), node: n, lit: true}
	case *pgparse.A_Const_Bsval:
		return &expr{typ: ref(a.s.Types.Lookup("pg_catalog", "bit").OID), node: n, lit: true}
	default: // string
		return &expr{typ: unknownRef(), node: n, lit: true}
	}
}

func (a *analyzer) columnRef(c *pgparse.ColumnRef, sc *scope) (*expr, *Error) {
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
			r, err := sc.wholeRow(names[len(names)-1], c.Location)
			if err != nil {
				return nil, err
			}
			if r != nil {
				if r.rowType != 0 {
					a.useAll(r.cols, c.Location)
					return &expr{typ: ref(r.rowType), nullable: r.rowNullable, node: nodeOf(c), fields: r.cols}, nil
				}
				cols := r.expand()
				a.useAll(cols, c.Location)
				return &expr{typ: ref(catalog.Record), node: nodeOf(c), fields: cols}, nil
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
		// a PL/pgSQL record variable's field: rec.field, NEW.col, row.col
		if tbl != "" {
			if fe, ferr := a.plField(tbl, col, c.Location); fe != nil || ferr != nil {
				return fe, ferr
			}
		}
		if tbl == "" {
			r, rerr := sc.wholeRow(col, c.Location)
			if rerr != nil {
				return nil, rerr
			}
			if r != nil {
				a.noteVarScope(sc.scopeOf(r))
				a.useAll(r.cols, c.Location)
				if r.rowType != 0 {
					return &expr{typ: ref(r.rowType), nullable: r.rowNullable, node: nodeOf(c), fields: r.cols, rowOf: r}, nil
				}
				if r.scalarFn && len(r.cols) == 1 {
					return &expr{typ: r.cols[0].typ, nullable: true, node: nodeOf(c)}, nil
				}
				if r.join == nil {
					return &expr{typ: ref(catalog.Record), node: nodeOf(c), fields: r.cols}, nil
				}
			}
			// a SQL function's parameter (a column of the same name takes precedence), or
			// a PL/pgSQL variable
			for i, p := range a.funcParams {
				if p.name == col {
					if p.plVar {
						if p.fields != nil || a.isComposite(p.typ.OID) {
							return &expr{typ: p.typ, nullable: true, node: nodeOf(c), fields: p.fields}, nil
						}
						return &expr{typ: p.typ, nullable: true, node: nodeOf(c)}, nil
					}
					return &expr{typ: p.typ, nullable: true, node: nodeOf(c), fparam: int32(i + 1)}, nil
				}
			}
		}
		return nil, err
	}
	// a name that is both a column and a PL/pgSQL variable is ambiguous
	for _, p := range a.funcParams {
		if p.plVar && (tbl == "" && p.name == col || tbl != "" && p.name == tbl) {
			return nil, errAt(codeAmbiguousColumn, c.Location, "column reference %q is ambiguous: it could refer to either a PL/pgSQL variable or a table column", strings.Join(names, "."))
		}
	}
	a.noteVarScope(a.lastResolvedScope)
	e := &expr{typ: rc.typ, nullable: rc.nullable, src: rc.src, node: nodeOf(c), fields: rc.fields, coll: rc.coll.asVar()}
	if e.coll.strength == collNone && a.collatable(rc.typ.OID) {
		e.coll = collation{strength: collImplicit, loc: c.Location}
	}
	return e, nil
}

func (a *analyzer) typeCast(tc *pgparse.TypeCast, sc *scope) (*expr, *Error) {
	e, err := a.typeCastValue(tc, sc)
	if err != nil {
		return nil, err
	}
	// a domain declared with a collation gives the cast result that collation
	if d := a.s.Types.Domains[e.oid()]; d != nil && d.Collation != "" && e.coll.strength <= collImplicit {
		e.coll = collation{strength: collImplicit, name: d.Collation, loc: tc.Location}
	}
	return e, nil
}

func (a *analyzer) typeCastValue(tc *pgparse.TypeCast, sc *scope) (*expr, *Error) {
	target, rerr := a.s.ResolveType(tc.TypeName)
	if rerr != nil {
		return nil, errAt(codeUndefinedObject, tc.TypeName.Location, "%v", rerr)
	}
	// ARRAY[]::type[] is allowed
	if arr := tc.Arg.GetAArrayExpr(); arr != nil && len(arr.Elements) == 0 {
		return &expr{typ: target, node: nodeOf(tc)}, nil
	}
	if arr := tc.Arg.GetAArrayExpr(); arr != nil {
		if at := a.typ(a.baseType(target.OID)); at != nil && at.IsArray() {
			// transformArrayExpr with the cast's array type: every element is coerced to
			// the element type explicitly, so ARRAY[1, 'NaN']::float8[] reads the literal
			// as float8 rather than unifying the elements first
			nested := false
			for _, el := range arr.Elements {
				if el.GetAArrayExpr() != nil {
					nested = true
				}
			}
			if !nested {
				es, err := a.analyzeList(arr.Elements, sc)
				if err != nil {
					return nil, err
				}
				nullable := false
				for i, el := range es {
					if err := a.bind(el, at.Elem, loc(arr.Elements[i])); err != nil {
						return nil, err
					}
					if !a.canCoerce(el.oid(), at.Elem, explicitCoercion) {
						return nil, errAt(codeCannotCoerce, loc(arr.Elements[i]), "cannot cast type %s to %s", a.s.Types.Format(el.typ), a.s.Types.Format(ref(at.Elem)))
					}
					nullable = nullable || el.nullable
				}
				return &expr{typ: target, nullable: nullable, node: nodeOf(tc)}, nil
			}
		}
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
	var src *Source
	if e.oid() == target.OID && (target.Typmod < 0 || target.Typmod == e.typ.Typmod) {
		src = e.src // a cast to the column's own type is the column (coerce_type returns the Var)
	}
	return &expr{typ: target, nullable: e.nullable, node: nodeOf(tc), lit: isLit(e) && a.domainType(target.OID) == nil, src: src}, nil
}

func (a *analyzer) opName(nodes []*pgparse.Node) string {
	parts := strs(nodes)
	return parts[len(parts)-1]
}

func (a *analyzer) aExpr(x *pgparse.A_Expr, sc *scope) (*expr, *Error) {
	self := nodeOf(x)
	switch x.Kind {
	case pgparse.A_Expr_Kind_AEXPR_OP, pgparse.A_Expr_Kind_AEXPR_LIKE, pgparse.A_Expr_Kind_AEXPR_ILIKE, pgparse.A_Expr_Kind_AEXPR_SIMILAR:
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
		if lr, rr := x.Lexpr.GetRowExpr(), x.Rexpr.GetRowExpr(); lr != nil && rr != nil && len(lr.Args) == 0 && len(rr.Args) == 0 {
			return nil, errAt(codeFeatureNotSupported, x.Location, "cannot compare rows of zero length")
		}
		if lr, rr := x.Lexpr.GetRowExpr(), x.Rexpr.GetRowExpr(); lr != nil && rr != nil && !rowCompareOps[name] && !rowPatternOps[name] && x.Kind == pgparse.A_Expr_Kind_AEXPR_OP {
			// make_row_comparison_op: the operator must exist per column, and then have a
			// btree interpretation, which ~~ and friends have not
			if len(lr.Args) != len(rr.Args) {
				return nil, errAt(codeSyntaxError, x.Location, "unequal number of entries in row expressions")
			}
			ls, err := a.analyzeList(lr.Args, sc)
			if err != nil {
				return nil, err
			}
			rs, err := a.analyzeList(rr.Args, sc)
			if err != nil {
				return nil, err
			}
			for i := range ls {
				if _, err := a.applyOperator(name, ls[i], rs[i], x.Location, self); err != nil {
					return nil, err
				}
			}
			return nil, errAt(codeFeatureNotSupported, x.Location, "could not determine interpretation of row comparison operator %s", name)
		}
		if lr, rr := x.Lexpr.GetRowExpr(), x.Rexpr.GetRowExpr(); lr != nil && rr != nil && rowPatternOps[name] {
			// make_row_comparison: ROW(..) op ROW(..) is compared column by column, with any
			// btree comparison operator; the text pattern family has no record operator of
			// its own, so it is resolved here per column
			if len(lr.Args) != len(rr.Args) {
				return nil, errAt(codeSyntaxError, x.Location, "unequal number of entries in row expressions")
			}
			ls, err := a.analyzeList(lr.Args, sc)
			if err != nil {
				return nil, err
			}
			rs, err := a.analyzeList(rr.Args, sc)
			if err != nil {
				return nil, err
			}
			for i := range ls {
				if _, err := a.applyOperator(name, ls[i], rs[i], x.Location, self); err != nil {
					return nil, err
				}
			}
			return &expr{typ: ref(catalog.Bool), nullable: l.nullable || r.nullable, node: self}, nil
		}
		if x.Kind == pgparse.A_Expr_Kind_AEXPR_SIMILAR {
			// x SIMILAR TO y is  x ~ similar_to_escape(y)
			if err := a.bind(r, catalog.Text, x.Location); err != nil {
				return nil, err
			}
			r = &expr{typ: ref(catalog.Text), nullable: r.nullable}
			name = "~"
		}
		return a.applyOperator(name, l, r, x.Location, self)
	case pgparse.A_Expr_Kind_AEXPR_OP_ANY, pgparse.A_Expr_Kind_AEXPR_OP_ALL:
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
				return nil, errAt(codeWrongObjectType, x.Location, "op ANY/ALL (array) requires array on right side")
			}
			elem = rt.Elem
		}
		re := &expr{typ: ref(elem), nullable: true, src: r.src, lit: isLit(r), coll: r.coll}
		res, err := a.applyOperator(name, l, re, x.Location, self)
		if err != nil {
			return nil, err
		}
		if res.oid() != catalog.Bool {
			return nil, errAt(codeWrongObjectType, x.Location, "op ANY/ALL (array) requires operator to yield boolean")
		}
		a.noteParamSource(r, l)
		if r.oid() == catalog.Unknown {
			if err := a.bind(r, a.s.Types.ArrayOf(re.oid()), x.Location); err != nil {
				return nil, err
			}
		}
		return &expr{typ: ref(catalog.Bool), nullable: l.nullable || r.nullable, node: self}, nil
	case pgparse.A_Expr_Kind_AEXPR_IN:
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
	case pgparse.A_Expr_Kind_AEXPR_BETWEEN, pgparse.A_Expr_Kind_AEXPR_NOT_BETWEEN,
		pgparse.A_Expr_Kind_AEXPR_BETWEEN_SYM, pgparse.A_Expr_Kind_AEXPR_NOT_BETWEEN_SYM:
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
	case pgparse.A_Expr_Kind_AEXPR_DISTINCT, pgparse.A_Expr_Kind_AEXPR_NOT_DISTINCT:
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
	case pgparse.A_Expr_Kind_AEXPR_NULLIF:
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
func (a *analyzer) applyOperator(name string, l, r *expr, at int32, self *pgparse.Node) (*expr, *Error) {
	var left catalog.OID
	if l != nil {
		left = l.oid()
	}
	a.opAmbiguous = false
	c := a.resolveOperator(name, left, r.oid())
	if c == nil {
		if l != nil && l.oid() == catalog.Unknown && r.oid() == catalog.Unknown {
			return nil, errAt(codeIndeterminateDatatype, at, "could not determine data type of parameter $%d", firstParam(l, r))
		}
		if a.opAmbiguous {
			if l != nil {
				return nil, errAt(codeAmbiguousFunction, at, "operator is not unique: %s %s %s", a.s.Types.Format(l.typ), name, a.s.Types.Format(r.typ))
			}
			return nil, errAt(codeAmbiguousFunction, at, "operator is not unique: %s %s", name, a.s.Types.Format(r.typ))
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
	if !nullable && a.opStrictButNullable(c.op) {
		nullable = true
	}
	return &expr{typ: ref(res), nullable: nullable, node: self, lit: isLit(l) && isLit(r), coll: a.resultColl(coll, res)}, nil
}

// opStrictButNullable reports whether op's implementing function is one of the
// strict-but-nullable built-ins (strictNullableFunc): even with all-non-NULL inputs it
// can still return SQL NULL because there is nothing to report (missing key, no such
// element, ...). Operators like jsonb's `->`/`->>`/`#>`/`#>>` and hstore's `->` share
// their implementation with the function form (json_object_field_text and friends),
// so routing through the operator's pg_operator.oprcode catches them the same way the
// function-call path already does, extensions (hstore, ...) included.
func (a *analyzer) opStrictButNullable(op *catalog.Operator) bool {
	if op == nil || op.Code == 0 {
		return false
	}
	fn := a.s.Catalog.FuncByOID(op.Code)
	if fn == nil {
		return false
	}
	return strictNullableFunc[fn.Name]
}

func firstParam(es ...*expr) int32 {
	for _, e := range es {
		if e != nil && e.param > 0 {
			return e.param
		}
	}
	return 1
}

func (a *analyzer) funcCall(f *pgparse.FuncCall, sc *scope) (*expr, *Error) {
	self := nodeOf(f)
	names := strs(f.Funcname)
	schemaName, name := "", names[len(names)-1]
	if len(names) > 1 {
		schemaName = names[len(names)-2]
	}
	var args []*expr
	isAgg := f.AggStar || f.AggFilter != nil || f.AggWithinGroup || f.AggDistinct || len(f.AggOrder) > 0 || a.isAggregateName(names)
	var frame *aggFrame
	if isAgg && f.Over == nil {
		if a.inAggArgs > 0 {
			return nil, errAt(codeGroupingError, f.Location, "aggregate function calls cannot be nested")
		}
		a.inAggArgs++
		defer func() { a.inAggArgs-- }()
		frame = &aggFrame{sc: sc, inDirect: f.AggWithinGroup}
		a.aggFrames = append(a.aggFrames, frame)
		defer func() { a.aggFrames = a.aggFrames[:len(a.aggFrames)-1] }()
	}
	if f.AggStar {
		// count(*)
	} else {
		var err *Error
		a.inFuncArgs++
		savedBan := a.srfBan
		if isAgg && f.Over == nil {
			a.srfBan = "aggregate"
		} else if f.Over != nil {
			a.srfBan = "window"
		}
		args, err = a.analyzeList(f.Args, sc)
		a.srfBan = savedBan
		a.inFuncArgs--
		if err != nil {
			return nil, err
		}
	}
	if len(args) == 1 && !isAgg && f.Over == nil && !f.FuncVariadic && args[0].oid() == catalog.Unknown && args[0].node.GetAConst() != nil && args[0].node.GetAConst().GetSval() != nil && f.Args[0].GetNamedArgExpr() == nil {
		// func_get_detail: type-name('literal') is a cast when no function takes an
		// unknown argument exactly, so the literal is read by the type's input function
		if t := a.typeAsFunc(schemaName, name); t != nil {
			if err := a.bind(args[0], t.OID, f.Location); err != nil {
				return nil, err
			}
			return &expr{typ: ref(t.OID), nullable: args[0].nullable, node: self}, nil
		}
	}
	if frame != nil {
		frame.inDirect = false
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
		for _, n := range append(append([]*pgparse.Node{}, f.Over.PartitionClause...), f.Over.OrderClause...) {
			if sb := n.GetSortBy(); sb != nil {
				n = sb.Node
			}
			if w := windowIn(n); w != nil {
				return nil, errAt(codeWindowingError, w.Location, "window functions are not allowed in window definitions")
			}
			oe, err := a.analyzeExpr(n, sc)
			if err != nil {
				return nil, err
			}
			if err := collConflictError(oe.coll, loc(n)); err != nil {
				return nil, err
			}
		}
	}
	for _, n := range f.AggOrder {
		if f.AggDistinct {
			found := false
			for _, arg := range f.Args {
				if deparse(arg) == deparse(n.GetSortBy().GetNode()) {
					found = true
				}
			}
			if !found {
				return nil, errAt(codeInvalidColumnRef, loc(n.GetSortBy().GetNode()), "in an aggregate with DISTINCT, ORDER BY expressions must appear in argument list")
			}
		}
		e, err := a.analyzeExpr(n.GetSortBy().GetNode(), sc)
		if err != nil {
			return nil, err
		}
		if err := collConflictError(e.coll, loc(n.GetSortBy().GetNode())); err != nil {
			return nil, err
		}
		if f.AggWithinGroup {
			// an ordered-set aggregate's signature is its direct arguments followed by the
			// WITHIN GROUP (ORDER BY ...) expressions
			args = append(args, e)
		}
	}
	var aggOwner *scope
	if frame != nil {
		var err *Error
		if aggOwner, err = a.settleAggFrame(frame, f); err != nil {
			return nil, err
		}
	}
	// f(x) with x a composite row that has a field f is the field, unless a function f
	// takes the row (ParseFuncOrColumn: the function wins, checked against the oracle)
	var projection *expr
	if len(args) == 1 && !f.AggStar && f.AggFilter == nil && f.Over == nil && len(f.AggOrder) == 0 && schemaName == "" {
		if t := a.typ(args[0].oid()); t != nil && t.Kind == 'c' {
			if fe := a.fieldOf(args[0], name); fe != nil {
				fe.node = self
				projection = fe
			}
		}
	}
	actual := a.argOIDs(args)
	named := make([]string, len(f.Args))
	for i, n := range f.Args {
		if na := n.GetNamedArgExpr(); na != nil {
			for _, prev := range named[:i] {
				if prev == na.Name {
					return nil, errAt(codeSyntaxError, na.Location, "argument name %q used more than once", na.Name)
				}
			}
			named[i] = na.Name
		} else if i > 0 && named[i-1] != "" {
			return nil, errAt(codeSyntaxError, loc(n), "positional argument cannot follow named argument")
		}
	}
	c, ambiguous := a.resolveFunction(schemaName, name, actual, f.AggWithinGroup, named, f.FuncVariadic)
	if c == nil && projection != nil {
		return projection, nil
	}
	if c == nil {
		// type-name(x) is a cast (func_get_detail), but only where the coercion is a
		// relabeling or goes through I/O: a cast function would have been found by name
		// already, so anything else is no function (42883)
		if len(args) == 1 {
			if t := a.typeAsFunc(schemaName, name); t != nil {
				if args[0].oid() == catalog.Unknown {
					if err := a.bind(args[0], t.OID, f.Location); err != nil {
						return nil, err
					}
				} else if !a.castInFuncSyntax(args[0].oid(), t.OID) {
					goto notCast
				}
				return &expr{typ: ref(t.OID), nullable: args[0].nullable, node: self}, nil
			}
		}
	notCast:
		if ambiguous {
			return nil, errAt(codeAmbiguousFunction, f.Location, "function %s(%s) is not unique", name, a.typeNames(actual))
		}
		if f.AggWithinGroup {
			// sum() WITHIN GROUP (ORDER BY x): the direct + ORDER BY arguments name a normal
			// aggregate, which takes no WITHIN GROUP
			if c2, _ := a.resolveFunction(schemaName, name, actual, false, named, f.FuncVariadic); c2 != nil && c2.fn != nil && c2.fn.Kind == 'a' {
				return nil, errAt(codeWrongObjectType, f.Location, "%s is not an ordered-set aggregate, so it cannot have WITHIN GROUP", name)
			}
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
	if f.FuncVariadic && len(args) > 0 {
		if last := args[len(args)-1]; last.oid() != catalog.Unknown {
			if lt := a.typ(a.baseType(last.oid())); lt == nil || !lt.IsArray() {
				return nil, errAt(codeDatatypeMismatch, loc(f.Args[len(f.Args)-1]), "VARIADIC argument must be an array")
			}
		}
	}
	if f.AggWithinGroup && a.isHypothetical(c) {
		// a hypothetical-set aggregate takes one direct argument per ORDER BY expression,
		// each pair unified to a common type (WITHIN GROUP types x and y cannot be matched)
		ndirect := len(f.Args)
		if ndirect != len(f.AggOrder) {
			return nil, errAt(codeUndefinedFunction, f.Location, "function %s(%s) does not exist", name, a.typeNames(actual))
		}
		for i := 0; i < ndirect; i++ {
			d, o := args[i], args[ndirect+i]
			if d.oid() == catalog.Unknown && o.oid() != catalog.Unknown {
				if err := a.bind(d, o.oid(), loc(f.Args[i])); err != nil {
					return nil, err
				}
				continue
			}
			if o.oid() == catalog.Unknown && d.oid() != catalog.Unknown {
				if err := a.bind(o, d.oid(), loc(f.AggOrder[i].GetSortBy().GetNode())); err != nil {
					return nil, err
				}
				continue
			}
			if d.oid() == catalog.Unknown || o.oid() == catalog.Unknown {
				continue
			}
			if _, ok := a.commonType([]catalog.OID{d.oid(), o.oid()}); !ok {
				return nil, errAt(codeDatatypeMismatch, loc(f.AggOrder[i].GetSortBy().GetNode()), "WITHIN GROUP types %s and %s cannot be matched", a.s.Types.Format(o.typ), a.s.Types.Format(d.typ))
			}
		}
	}
	if u := a.polyUnknownInput(c.args, actual); u != "" {
		if u == "anyelement" {
			return nil, errAt(codeDatatypeMismatch, f.Location, "could not determine polymorphic type because input has type unknown")
		}
		return nil, errAt(codeDatatypeMismatch, f.Location, "could not determine polymorphic type %s because input has type unknown", u)
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
	if a.lastFuncRetSet {
		if a.inFromFunc && a.inFuncArgs > 0 {
			return nil, errAt(codeFeatureNotSupported, f.Location, "set-returning functions must appear at top level of FROM")
		}
		if a.inCase > 0 {
			return nil, errAt(codeFeatureNotSupported, f.Location, "set-returning functions are not allowed in CASE")
		}
		switch a.srfBan {
		case "":
		case "aggregate", "window":
			return nil, errAt(codeFeatureNotSupported, f.Location, "%s function calls cannot contain set-returning function calls", a.srfBan)
		default:
			return nil, errAt(codeFeatureNotSupported, f.Location, "set-returning functions are not allowed in %s", a.srfBan)
		}
	}
	switch {
	case c.fn != nil:
		a.funcVolatility[f] = c.fn.Volatile
	case c.ufn != nil:
		a.funcVolatility[f] = c.ufn.Volatile
	}
	isAggFn := c.fn != nil && c.fn.Kind == 'a' || c.ufn != nil && c.ufn.IsAgg
	if isAggFn && len(f.Args) == 0 && !f.AggStar && !f.AggWithinGroup {
		return nil, errAt(codeWrongObjectType, f.Location, "%s(*) must be used to call a parameterless aggregate function", name)
	}
	if f.Over != nil && !isAggFn && !(c.fn != nil && c.fn.Kind == 'w') && !(c.ufn != nil && c.ufn.IsWindow) {
		return nil, errAt(codeWrongObjectType, f.Location, "OVER specified, but %s is not a window function nor an aggregate function", name)
	}
	if isAggFn && f.Over == nil {
		if aggOwner != nil {
			aggOwner.agg = true // an outer-level aggregate makes that query grouped
		} else {
			sc.agg = true
		}
	}
	res := c.result()
	if t := a.typ(res); t != nil && t.IsPolymorphic() {
		a.polyErr = nil
		rr, ok := a.resolvePolymorphic(c.args, a.argOIDs(args), res)
		if !ok {
			if a.polyErr != nil {
				return nil, a.polyErr
			}
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
	case c.fn != nil && c.fn.IsStrict && !a.strictButNullable(name, args):
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

func (a *analyzer) caseExpr(c *pgparse.CaseExpr, sc *scope) (*expr, *Error) {
	a.inCase++
	defer func() { a.inCase-- }()
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

func (a *analyzer) subLink(s *pgparse.SubLink, sc *scope) (*expr, *Error) {
	sel := s.Subselect.GetSelectStmt()
	if sel == nil {
		return nil, errAt(codeFeatureNotSupported, s.Location, "unsupported subquery")
	}
	if sel.IntoClause != nil {
		return nil, errAt(codeSyntaxError, sel.IntoClause.Rel.GetLocation(), "SELECT ... INTO is not allowed here")
	}
	cols, err := a.selectStmt(sel, newScope(sc))
	if err != nil {
		return nil, err
	}
	self := nodeOf(s)
	switch s.SubLinkType {
	case pgparse.SubLinkType_EXISTS_SUBLINK:
		return &expr{typ: ref(catalog.Bool), node: self}, nil
	case pgparse.SubLinkType_EXPR_SUBLINK:
		if len(cols) != 1 {
			return nil, errAt(codeSyntaxError, s.Location, "subquery must return only one column")
		}
		// a scalar subquery is NULL when it yields no row; an aggregate without GROUP BY
		// always yields one, so its column's own nullability stands (coalesce(max(x), 0))
		nullable := cols[0].nullable || !a.plainAggregate(sel)
		return &expr{typ: cols[0].typ, nullable: nullable, node: self, coll: cols[0].coll.asVar()}, nil
	case pgparse.SubLinkType_ARRAY_SUBLINK:
		if len(cols) != 1 {
			return nil, errAt(codeSyntaxError, s.Location, "subquery must return only one column")
		}
		// get_promoted_array_type: the element's array type when it has one (int2vector
		// has int2vector[]), else the element itself when it is already an array
		// (ARRAY(SELECT int[] ...) is an int[], not int[][])
		arr := a.s.Types.ArrayOf(cols[0].typ.OID)
		if arr == 0 {
			if ct := a.typ(a.baseType(cols[0].typ.OID)); ct != nil && ct.IsArray() {
				return &expr{typ: ref(ct.OID), nullable: false, node: self}, nil
			}
			return nil, errAt(codeUndefinedObject, s.Location, "could not find array type for data type %s", a.s.Types.Format(cols[0].typ))
		}
		return &expr{typ: ref(arr), nullable: false, node: self}, nil
	case pgparse.SubLinkType_ANY_SUBLINK, pgparse.SubLinkType_ALL_SUBLINK:
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

func (a *analyzer) indirection(x *pgparse.A_Indirection, sc *scope) (*expr, *Error) {
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
		case *pgparse.Node_AIndices:
			t := a.typ(a.baseType(cur.OID))
			switch {
			case t != nil && t.OID == catalog.JSONB:
				// jsonb subscripting (PG 14): each subscript is a key (text) or an array index
				// (integer) and yields jsonb
				if v.AIndices.IsSlice {
					return nil, errAt(codeDatatypeMismatch, loc(x.Arg), "jsonb subscript does not support slices")
				}
				for _, idx := range []*pgparse.Node{v.AIndices.Lidx, v.AIndices.Uidx} {
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
				dims := 0
				for ; i < len(x.Indirection); i++ {
					ai := x.Indirection[i].GetAIndices()
					if ai == nil {
						break
					}
					dims++
					if dims > maxArrayDim {
						return nil, errAt("54000", -1, "number of array dimensions (%d) exceeds the maximum allowed (%d)", dims, maxArrayDim)
					}
					slice = slice || ai.IsSlice
					for _, idx := range []*pgparse.Node{ai.Lidx, ai.Uidx} {
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
				for _, idx := range []*pgparse.Node{ai.Lidx, ai.Uidx} {
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
		case *pgparse.Node_String_:
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

func sqlValueType(op pgparse.SQLValueFunctionOp) catalog.OID {
	switch op {
	case pgparse.SQLValueFunctionOp_SVFOP_CURRENT_DATE:
		return catalog.Date
	case pgparse.SQLValueFunctionOp_SVFOP_CURRENT_TIME, pgparse.SQLValueFunctionOp_SVFOP_CURRENT_TIME_N:
		return catalog.TimeTZ
	case pgparse.SQLValueFunctionOp_SVFOP_CURRENT_TIMESTAMP, pgparse.SQLValueFunctionOp_SVFOP_CURRENT_TIMESTAMP_N:
		return catalog.TimestampTZ
	case pgparse.SQLValueFunctionOp_SVFOP_LOCALTIME, pgparse.SQLValueFunctionOp_SVFOP_LOCALTIME_N:
		return catalog.Time
	case pgparse.SQLValueFunctionOp_SVFOP_LOCALTIMESTAMP, pgparse.SQLValueFunctionOp_SVFOP_LOCALTIMESTAMP_N:
		return catalog.Timestamp
	}
	return catalog.Name
}

func boolOpName(op pgparse.BoolExprType) string {
	switch op {
	case pgparse.BoolExprType_AND_EXPR:
		return "AND"
	case pgparse.BoolExprType_OR_EXPR:
		return "OR"
	}
	return "NOT"
}

func strs(nodes []*pgparse.Node) []string {
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, n.GetString_().GetSval())
	}
	return out
}

// loc returns the 0-based location of a node, or -1.
func loc(n *pgparse.Node) int32 {
	if n == nil {
		return -1
	}
	switch v := n.Node.(type) {
	case *pgparse.Node_AConst:
		return v.AConst.Location
	case *pgparse.Node_ParamRef:
		return v.ParamRef.Location
	case *pgparse.Node_ColumnRef:
		return v.ColumnRef.Location
	case *pgparse.Node_TypeCast:
		return v.TypeCast.Location
	case *pgparse.Node_AExpr:
		return v.AExpr.Location
	case *pgparse.Node_FuncCall:
		return v.FuncCall.Location
	case *pgparse.Node_BoolExpr:
		return v.BoolExpr.Location
	case *pgparse.Node_CaseExpr:
		return v.CaseExpr.Location
	case *pgparse.Node_SubLink:
		return v.SubLink.Location
	case *pgparse.Node_AArrayExpr:
		return v.AArrayExpr.Location
	case *pgparse.Node_ResTarget:
		return v.ResTarget.Location
	case *pgparse.Node_RangeVar:
		return v.RangeVar.Location
	case *pgparse.Node_CoalesceExpr:
		return v.CoalesceExpr.Location
	case *pgparse.Node_NullTest:
		return v.NullTest.Location
	case *pgparse.Node_BooleanTest:
		return v.BooleanTest.Location
	}
	return -1
}

// nodeOf wraps a concrete AST message back into a Node (for name figuring).
func nodeOf(m any) *pgparse.Node {
	switch v := m.(type) {
	case *pgparse.ColumnRef:
		return &pgparse.Node{Node: &pgparse.Node_ColumnRef{ColumnRef: v}}
	case *pgparse.TypeCast:
		return &pgparse.Node{Node: &pgparse.Node_TypeCast{TypeCast: v}}
	case *pgparse.A_Expr:
		return &pgparse.Node{Node: &pgparse.Node_AExpr{AExpr: v}}
	case *pgparse.FuncCall:
		return &pgparse.Node{Node: &pgparse.Node_FuncCall{FuncCall: v}}
	case *pgparse.CaseExpr:
		return &pgparse.Node{Node: &pgparse.Node_CaseExpr{CaseExpr: v}}
	case *pgparse.SubLink:
		return &pgparse.Node{Node: &pgparse.Node_SubLink{SubLink: v}}
	case *pgparse.A_Indirection:
		return &pgparse.Node{Node: &pgparse.Node_AIndirection{AIndirection: v}}
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

// strictNullableFunc lists the strict built-ins that return NULL for non-NULL inputs
// when there is nothing to report: no match, no such element, no such object.
var strictNullableFunc = map[string]bool{
	"regexp_match": true, "array_length": true, "array_lower": true, "array_upper": true, "array_position": true,
	"array_ndims": true, "array_dims": true, "json_extract_path": true, "json_extract_path_text": true,
	"jsonb_extract_path": true, "jsonb_extract_path_text": true, "json_object_field": true, "json_object_field_text": true,
	"json_array_element": true, "json_array_element_text": true, "jsonb_object_field": true, "jsonb_object_field_text": true,
	"jsonb_array_element": true, "jsonb_array_element_text": true, "jsonb_path_query_first": true, "json_typeof": false,
	"to_regclass": true, "to_regtype": true, "to_regproc": true, "to_regprocedure": true, "to_regoper": true,
	"to_regoperator": true, "to_regnamespace": true, "to_regrole": true, "to_regcollation": true,
	"substring_index": true, "split_part": false, "nullif": true, "pg_get_userbyid": false,
	// hstore's `->` (single key lookup) implements as fetchval: a missing key returns SQL
	// NULL even though the hstore argument is non-NULL. Reached only via applyOperator's
	// oprcode -> pg_proc.Name lookup (opStrictButNullable), since hstore has no function
	// spelling of its own for this operator.
	"fetchval": true,
}

// strictButNullable: a strict function that still yields NULL from non-NULL inputs: the
// listed ones, and lower / upper of a range or multirange (an unbounded side has no value).
func (a *analyzer) strictButNullable(name string, args []*expr) bool {
	if strictNullableFunc[name] {
		return true
	}
	// A strict nullary function that reports an absent value (zeroArgNullable) hits the
	// IsStrict case in funcCall's switch before the zero-arg case ever runs, since strict
	// short-circuits there for lack of any argument to propagate NULL from. Route it
	// through here too so it isn't forced to nullable=false.
	if len(args) == 0 && zeroArgNullable[name] {
		return true
	}
	if (name == "lower" || name == "upper") && len(args) == 1 {
		if t := a.s.Types.ByOID(a.s.Types.BaseOf(args[0].typ).OID); t != nil && (t.Kind == 'r' || t.Kind == 'm') {
			return true
		}
	}
	return false
}

// zeroArgNullable lists the nullary built-ins that do return NULL (no value to report).
var zeroArgNullable = map[string]bool{
	"inet_client_addr": true, "inet_client_port": true, "inet_server_addr": true, "inet_server_port": true,
	"pg_last_wal_receive_lsn": true, "pg_last_wal_replay_lsn": true, "pg_last_xact_replay_timestamp": true,
	"pg_current_xact_id_if_assigned": true, "txid_current_if_assigned": true, "current_query": true, "pg_current_logfile": true,
}

// plainAggregate reports whether sel is a single aggregate query without GROUP BY / HAVING /
// LIMIT / set operations: it returns exactly one row.
func (a *analyzer) plainAggregate(sel *pgparse.SelectStmt) bool {
	if sel.Op != pgparse.SetOperation_SETOP_NONE || len(sel.GroupClause) > 0 || sel.HavingClause != nil ||
		sel.LimitCount != nil || sel.LimitOffset != nil || len(sel.ValuesLists) > 0 || len(sel.DistinctClause) > 0 {
		return false
	}
	agg := false
	for _, tn := range sel.TargetList {
		schema.WalkNodes(tn, func(n *pgparse.Node) {
			if f := n.GetFuncCall(); f != nil && f.Over == nil && a.isAggregateName(funcNames(f)) {
				agg = true
			}
		})
	}
	return agg
}

func funcNames(f *pgparse.FuncCall) []string {
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
	var src *Source
	if e.rowOf != nil && e.rowOf.rel == rel {
		src = &Source{Table: rel.FullName(), Column: col.Name, NotNull: col.NotNull} // f(t) on a FROM item's row is the column
	}
	return &expr{typ: col.Type, nullable: true, src: src}
}

// rowSubquery analyzes a scalar subquery used as one side of a row comparison: its
// columns form an anonymous record (ROWCOMPARE_SUBLINK).
func (a *analyzer) rowSubquery(s *pgparse.SubLink, sc *scope) (*expr, *Error) {
	sel := s.Subselect.GetSelectStmt()
	if s.SubLinkType != pgparse.SubLinkType_EXPR_SUBLINK || sel == nil {
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
func (a *analyzer) checkWindowDef(def *pgparse.WindowDef, sc *scope) *Error {
	lookup := func(name string) *pgparse.WindowDef {
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
	if fo&frameOptionOffsets != 0 {
		mode := "ROWS"
		switch {
		case fo&frameOptionRange != 0:
			mode = "RANGE"
		case fo&frameOptionGroups != 0:
			mode = "GROUPS"
		}
		for _, off := range []*pgparse.Node{def.StartOffset, def.EndOffset} {
			if off == nil {
				continue
			}
			if hasVarClause(off) {
				return errAt(codeInvalidColumnRef, loc(off), "argument of %s must not contain variables", mode)
			}
			oe, err := a.analyzeExpr(off, sc)
			if err != nil {
				return err
			}
			if mode == "RANGE" {
				// transformFrameOffset: the ORDER BY column's btree opfamily must have an
				// in_range support function for the offset's type
				ce, err := a.analyzeExpr(order[0].GetSortBy().GetNode(), sc)
				if err != nil {
					return err
				}
				if err := a.checkInRange(ce, oe, loc(off)); err != nil {
					return err
				}
			} else if err := a.bind(oe, catalog.Int8, loc(off)); err != nil {
				return err
			}
		}
	}
	return nil
}

// inRangeOffsets lists, per ORDER BY column type, the offset types PG has in_range
// support functions for (pg_amproc, btree in_range).
var inRangeOffsets = map[catalog.OID][]catalog.OID{
	catalog.Int2:        {catalog.Int2, catalog.Int4, catalog.Int8},
	catalog.Int4:        {catalog.Int2, catalog.Int4, catalog.Int8},
	catalog.Int8:        {catalog.Int8},
	catalog.Float4:      {catalog.Float8},
	catalog.Float8:      {catalog.Float8},
	catalog.Numeric:     {catalog.Numeric},
	catalog.Date:        {catalog.Interval},
	catalog.Timestamp:   {catalog.Interval},
	catalog.TimestampTZ: {catalog.Interval},
	catalog.Time:        {catalog.Interval},
	catalog.TimeTZ:      {catalog.Interval},
	catalog.Interval:    {catalog.Interval},
}

// checkInRange is whether RANGE offset frames work for the ORDER BY column and offset types.
func (a *analyzer) checkInRange(col, off *expr, at int32) *Error {
	ct := a.baseType(col.oid())
	if ct == catalog.Unknown {
		return nil
	}
	allowed, ok := inRangeOffsets[ct]
	if !ok {
		return errAt(codeFeatureNotSupported, at, "RANGE with offset PRECEDING/FOLLOWING is not supported for column type %s", a.s.Types.Format(col.typ))
	}
	if off.oid() == catalog.Unknown {
		return a.bind(off, allowed[len(allowed)-1], at) // a literal offset takes the widest offset type
	}
	ot := a.baseType(off.oid())
	for _, t := range allowed {
		if ot == t || a.canCoerce(ot, t, implicitCoercion) {
			return nil
		}
	}
	return errAt(codeFeatureNotSupported, at, "RANGE with offset PRECEDING/FOLLOWING is not supported for column type %s and offset type %s", a.s.Types.Format(col.typ), a.s.Types.Format(off.typ))
}

// hasColumnRef reports a column reference anywhere in the expression.
// aggFrame follows one aggregate call while its arguments are analyzed: the query level
// its columns resolve to decides which query the aggregate belongs to
// (check_agg_arguments: the nearest level among the aggregated arguments' columns).
type aggFrame struct {
	sc *scope // where the call is written
	// owner / ownerDepth: the nearest level a column of the aggregated arguments (FILTER,
	// ORDER BY, plain arguments) resolved to; nil until one does (constants only: this level)
	owner      *scope
	ownerDepth int
	// direct / directDepth: the same for an ordered-set aggregate's direct arguments
	direct      *scope
	directDepth int
	inDirect    bool
	// nested: aggregates written inside the arguments, with the level each belongs to
	nested []nestedAgg
	// cteRefs: CTEs referenced inside the arguments, with the scope defining each (an
	// outer-level aggregate may not use a CTE defined below its level, PostgreSQL 18)
	cteRefs []cteRef
}

type cteRef struct {
	def *scope
	loc int32
}

// noteCTERef records, for every aggregate whose arguments are being analyzed, a CTE
// reference and the scope that defines the CTE.
func (a *analyzer) noteCTERef(def *scope, loc int32) {
	for _, fr := range a.aggFrames {
		fr.cteRefs = append(fr.cteRefs, cteRef{def: def, loc: loc})
	}
}

type nestedAgg struct {
	owner *scope
	loc   int32
}

// noteVarScope records that a column resolved in scope s for every aggregate whose
// arguments are being analyzed; a column of a deeper level (a subquery's own table) is
// not that aggregate's business.
func (a *analyzer) noteVarScope(s *scope) {
	if s == nil {
		return
	}
	for _, fr := range a.aggFrames {
		d, ok := fr.sc.levelsUp(s)
		if !ok {
			continue
		}
		if fr.inDirect {
			if fr.direct == nil || d < fr.directDepth {
				fr.direct, fr.directDepth = s.queryScope(), d
			}
		} else if fr.owner == nil || d < fr.ownerDepth {
			fr.owner, fr.ownerDepth = s.queryScope(), d
		}
	}
}

// settleAggFrame applies check_agg_arguments once an aggregate's arguments are analyzed:
// an ordered-set aggregate's direct arguments may not reach below its level, an aggregate
// of the same level inside the arguments is a nested aggregate, and an aggregate may not
// sit in a FROM item of the level it belongs to. It returns the level the aggregate
// belongs to (nil for the level it is written in).
func (a *analyzer) settleAggFrame(fr *aggFrame, f *pgparse.FuncCall) (*scope, *Error) {
	owner, depth := fr.owner, fr.ownerDepth
	if owner == nil {
		owner, depth = fr.sc.queryScope(), 0
	}
	if fr.direct != nil && fr.directDepth < depth {
		return nil, errAt(codeGroupingError, f.Location, "outer-level aggregate cannot contain a lower-level variable in its direct arguments")
	}
	for _, n := range fr.nested {
		if n.owner == owner {
			return nil, errAt(codeGroupingError, n.loc, "aggregate function calls cannot be nested")
		}
	}
	if depth > 0 {
		for s := fr.sc; s != nil && s != owner; s = s.parent {
			if s.fromOf == owner {
				return nil, errAt(codeGroupingError, f.Location, "aggregate functions are not allowed in FROM clause of their own query level")
			}
		}
		if a.s.Version.Or() >= pgparse.PG18 {
			// the aggregate is evaluated at the outer level, where a CTE of the subquery
			// between does not exist (18; 17 went on and failed the GROUP BY check)
			for _, ref := range fr.cteRefs {
				if d, ok := ref.def.levelsUp(owner); ok && d > 0 {
					return nil, errAt(codeFeatureNotSupported, ref.loc, "outer-level aggregate cannot use a nested CTE")
				}
			}
		}
	}
	for _, outer := range a.aggFrames {
		if outer != fr {
			outer.nested = append(outer.nested, nestedAgg{owner: owner, loc: f.Location})
		}
	}
	if depth == 0 {
		return nil, nil
	}
	return owner, nil
}

// maxArrayDim is PG's MAXDIM.
const maxArrayDim = 6

// hasVarClause is contain_var_clause on a raw expression: a column reference outside any
// subquery (a SubLink's own columns are not this level's variables).
func hasVarClause(n *pgparse.Node) bool {
	if n == nil || n.GetSubLink() != nil {
		return false
	}
	if n.GetColumnRef() != nil {
		return true
	}
	for _, c := range children(n) {
		if hasVarClause(c) {
			return true
		}
	}
	return false
}

func hasColumnRef(n *pgparse.Node) bool {
	if n == nil {
		return false
	}
	if n.GetColumnRef() != nil {
		return true
	}
	for _, c := range children(n) {
		if hasColumnRef(c) {
			return true
		}
	}
	return false
}

// typeAsFunc is FuncNameAsType: the type a function-syntax call may be a cast to. An
// unqualified name never reaches a pg_temp type (temp_ok = false, as for functions).
func (a *analyzer) typeAsFunc(schemaName, name string) *catalog.Type {
	t := a.s.Types.Lookup(schemaName, name)
	if t != nil && schemaName == "" && a.s.Types.Schemas[t.OID] == "pg_temp" {
		return nil
	}
	return t
}

// castInFuncSyntax reports whether type-name(x) of a typed x counts as a cast: the same
// type (domains apart), a binary-coercible pair, or an I/O coercion that does not take a
// row type to a string (find_coercion_pathway in func_get_detail).
func (a *analyzer) castInFuncSyntax(from, to catalog.OID) bool {
	fb, tb := a.baseType(from), a.baseType(to)
	if fb == tb {
		return true
	}
	c := a.s.Catalog.CastBetween(fb, tb)
	if c == nil {
		for _, uc := range a.s.Casts {
			if uc.Source == fb && uc.Target == tb {
				c = uc
			}
		}
	}
	if c != nil {
		return c.Func == 0 && c.Method == 'b'
	}
	fc, _ := a.category(fb)
	tc, _ := a.category(tb)
	if fc == 'S' || tc == 'S' {
		ft := a.typ(fb)
		return !((fb == catalog.Record || ft != nil && ft.Kind == 'c') && tc == 'S')
	}
	return false
}

// rowCompareOps are the operators make_row_comparison_op can interpret: the btree
// comparison operators (record_ops and record_image_ops); rowPatternOps are handled apart.
var rowCompareOps = map[string]bool{"=": true, "<>": true, "!=": true, "<": true, "<=": true, ">": true, ">=": true,
	"*=": true, "*<>": true, "*<": true, "*<=": true, "*>": true, "*>=": true}

// rowPatternOps are the btree comparison operators of the text_pattern_ops family, which
// exist for text types but not for record.
var rowPatternOps = map[string]bool{"~<~": true, "~<=~": true, "~>~": true, "~>=~": true}

// isHypothetical is whether the chosen candidate is a hypothetical-set aggregate.
func (a *analyzer) isHypothetical(c *candidate) bool {
	if c.fn != nil {
		agg := a.s.Catalog.AggregateByFn(c.fn.OID)
		return agg != nil && agg.Kind == 'h'
	}
	return c.ufn != nil && c.ufn.AggKind == 'h'
}
