package analyze

import (
	pg_query "github.com/pganalyze/pg_query_go/v6"

	"github.com/kr9ly/sqlshape/internal/catalog"
	"github.com/kr9ly/sqlshape/internal/schema"
)

// SQL/JSON (PG 16 / 17) and SQL/XML expressions. They are grammar, not functions, so
// pg_query hands them over as dedicated nodes; the typing rules follow parse_expr.c:
// the constructors return json unless RETURNING says otherwise, JSON_EXISTS is boolean,
// JSON_QUERY is jsonb and JSON_VALUE text unless RETURNING says otherwise, JSON_TABLE
// is a FROM item whose columns are declared. XML expressions return xml except
// xmlserialize (its declared type) and IS DOCUMENT (boolean).

const xmlOID catalog.OID = 142

// jsonOutput resolves a RETURNING clause, def when there is none.
func (a *analyzer) jsonOutput(o *pg_query.JsonOutput, def catalog.OID) (schema.TypeRef, *Error) {
	if o == nil || o.TypeName == nil {
		return ref(def), nil
	}
	t, err := a.s.ResolveType(o.TypeName)
	if err != nil {
		return schema.TypeRef{}, errAt(codeUndefinedObject, o.TypeName.Location, "%v", err)
	}
	if tt := a.typ(t.OID); tt != nil && tt.Kind == 'p' {
		return schema.TypeRef{}, errAt(codeFeatureNotSupported, o.TypeName.Location, "returning pseudo-types is not supported in SQL/JSON functions")
	}
	if o.Returning != nil && o.Returning.Format != nil && o.Returning.Format.Encoding != pg_query.JsonEncoding_JS_ENC_DEFAULT && a.baseType(t.OID) != catalog.Bytea {
		return schema.TypeRef{}, errAt(codeFeatureNotSupported, o.Returning.Format.Location, "cannot set JSON encoding for non-bytea output types")
	}
	return t, nil
}

// containsSubLink reports a subquery anywhere in the expression.
func containsSubLink(n *pg_query.Node) bool {
	if n == nil {
		return false
	}
	if n.GetSubLink() != nil {
		return true
	}
	for _, c := range children(n) {
		if containsSubLink(c) {
			return true
		}
	}
	return false
}

// jsonValue types a JSON value expression (the context item of a query function, an
// element of a constructor): an untyped literal becomes text.
func (a *analyzer) jsonValue(v *pg_query.JsonValueExpr, sc *scope) (*expr, *Error) {
	if v == nil {
		return nil, errAt(codeSyntaxError, -1, "missing JSON value")
	}
	e, err := a.analyzeExpr(v.RawExpr, sc)
	if err != nil {
		return nil, err
	}
	if err := a.bind(e, catalog.Text, loc(v.RawExpr)); err != nil {
		return nil, err
	}
	if v.Format != nil && v.Format.FormatType != pg_query.JsonFormatType_JS_FORMAT_DEFAULT {
		// transformJsonValueExpr: an explicit FORMAT JSON wants a string / bytea / json
		// value, and ENCODING a bytea one
		base := a.baseType(e.oid())
		cat, _ := a.category(base)
		if v.Format.Encoding != pg_query.JsonEncoding_JS_ENC_DEFAULT && base != catalog.Bytea {
			return nil, errAt(codeDatatypeMismatch, v.Format.Location, "JSON ENCODING clause is only allowed for bytea input type")
		}
		if cat != 'S' && base != catalog.Bytea && base != catalog.JSON && base != catalog.JSONB {
			return nil, errAt(codeDatatypeMismatch, loc(v.RawExpr), "cannot use non-string types with explicit FORMAT JSON clause")
		}
	}
	return e, nil
}

// jsonContext types the context item of JSON_EXISTS / JSON_QUERY / JSON_VALUE / JSON_TABLE:
// json, jsonb, or a string / bytea with FORMAT JSON.
func (a *analyzer) jsonContext(v *pg_query.JsonValueExpr, sc *scope, at int32) (*expr, *Error) {
	if v == nil {
		return nil, errAt(codeSyntaxError, -1, "missing JSON value")
	}
	e, err := a.analyzeExpr(v.RawExpr, sc)
	if err != nil {
		return nil, err
	}
	// an untyped literal context item is jsonb (transformJsonValueExpr), and is validated as such
	if err := a.bind(e, catalog.JSONB, loc(v.RawExpr)); err != nil {
		return nil, err
	}
	if v.Format != nil && v.Format.Encoding != pg_query.JsonEncoding_JS_ENC_DEFAULT && a.baseType(e.oid()) != catalog.Bytea {
		return nil, errAt(codeDatatypeMismatch, v.Format.Location, "JSON ENCODING clause is only allowed for bytea input type")
	}
	if v.Format != nil && (v.Format.Encoding == pg_query.JsonEncoding_JS_ENC_UTF16 || v.Format.Encoding == pg_query.JsonEncoding_JS_ENC_UTF32) {
		return nil, errAt(codeFeatureNotSupported, v.Format.Location, "unsupported JSON encoding")
	}
	switch a.baseType(e.oid()) {
	case catalog.JSON, catalog.JSONB:
	default:
		// anything else is parsed only with FORMAT JSON; otherwise PG tries to cast it to jsonb
		formatted := v.Format != nil && v.Format.FormatType != pg_query.JsonFormatType_JS_FORMAT_DEFAULT
		if c, _ := a.category(e.oid()); !(formatted && (c == 'S' || a.baseType(e.oid()) == catalog.Bytea)) {
			return nil, errAt(codeCannotCoerce, loc(v.RawExpr), "cannot cast type %s to jsonb", a.s.Types.Format(e.typ))
		}
	}
	return e, nil
}

func (a *analyzer) jsonPassing(args []*pg_query.Node, sc *scope) *Error {
	for _, n := range args {
		ja := n.GetJsonArgument()
		if ja == nil {
			continue
		}
		if _, err := a.jsonValue(ja.Val, sc); err != nil {
			return err
		}
	}
	return nil
}

func (a *analyzer) jsonBehavior(b *pg_query.JsonBehavior, sc *scope, want catalog.OID) *Error {
	if b == nil || b.Expr == nil {
		return nil
	}
	if b.Btype == pg_query.JsonBehaviorType_JSON_BEHAVIOR_DEFAULT && (containsSubLink(b.Expr) || a.aggregateIn(b.Expr) != nil || windowIn(b.Expr) != nil) {
		return errAt(codeDatatypeMismatch, loc(b.Expr), "can only specify a constant, non-aggregate function, or operator expression for DEFAULT")
	}
	if b.Btype == pg_query.JsonBehaviorType_JSON_BEHAVIOR_DEFAULT && a.srfIn(b.Expr) {
		return errAt(codeDatatypeMismatch, loc(b.Expr), "DEFAULT expression must not return a set")
	}
	e, err := a.analyzeExpr(b.Expr, sc)
	if err != nil {
		return err
	}
	if want != 0 {
		if err := a.bind(e, want, loc(b.Expr)); err != nil {
			return err
		}
		if !a.canCoerce(e.oid(), want, assignmentCoercion) {
			return errAt(codeCannotCoerce, loc(b.Expr), "cannot cast behavior expression of type %s to %s", a.s.Types.Format(e.typ), a.s.Types.Format(ref(want)))
		}
	}
	return nil
}

func (a *analyzer) jsonPathspec(n *pg_query.Node, sc *scope) *Error {
	if n == nil {
		return nil
	}
	e, err := a.analyzeExpr(n, sc)
	if err != nil {
		return err
	}
	return a.bind(e, catalog.Text, loc(n))
}

// jsonFuncExpr is JSON_EXISTS / JSON_QUERY / JSON_VALUE.
func (a *analyzer) jsonFuncExpr(f *pg_query.JsonFuncExpr, sc *scope, n *pg_query.Node) (*expr, *Error) {
	if _, err := a.jsonContext(f.ContextItem, sc, f.Location); err != nil {
		return nil, err
	}
	if err := a.jsonPathspec(f.Pathspec, sc); err != nil {
		return nil, err
	}
	if err := a.jsonPassing(f.Passing, sc); err != nil {
		return nil, err
	}
	def := catalog.OID(catalog.JSONB)
	switch f.Op {
	case pg_query.JsonExprOp_JSON_EXISTS_OP:
		def = catalog.Bool
	case pg_query.JsonExprOp_JSON_VALUE_OP:
		def = catalog.Text
		if r := f.Output.GetReturning(); r != nil && r.Format != nil && r.Format.FormatType != pg_query.JsonFormatType_JS_FORMAT_DEFAULT {
			return nil, errAt(codeSyntaxError, r.Format.Location, "cannot specify FORMAT JSON in RETURNING clause of JSON_VALUE()")
		}
	}
	t, err := a.jsonOutput(f.Output, def)
	if err != nil {
		return nil, err
	}
	if err := jsonBehaviorAllowed(f.Op, f.OnEmpty, f.OnError, "", f.Location); err != nil {
		return nil, err
	}
	if (f.Wrapper == pg_query.JsonWrapper_JSW_CONDITIONAL || f.Wrapper == pg_query.JsonWrapper_JSW_UNCONDITIONAL) &&
		f.Quotes == pg_query.JsonQuotes_JS_QUOTES_OMIT {
		return nil, errAt(codeSyntaxError, f.Location, "SQL/JSON QUOTES behavior must not be specified when WITH WRAPPER is used")
	}
	for _, b := range []*pg_query.JsonBehavior{f.OnEmpty, f.OnError} {
		if err := a.jsonBehavior(b, sc, t.OID); err != nil {
			return nil, err
		}
	}
	return &expr{typ: t, nullable: f.Op != pg_query.JsonExprOp_JSON_EXISTS_OP, node: n}, nil
}

// jsonConstructor is JSON_OBJECT / JSON_ARRAY (with a list, or a subquery).
func (a *analyzer) jsonConstructorList(exprs []*pg_query.Node, out *pg_query.JsonOutput, sc *scope, n *pg_query.Node) (*expr, *Error) {
	def := catalog.OID(catalog.JSON) // jsonb once any input is jsonb (transformJsonConstructorOutput)
	note := func(e *expr) {
		if a.baseType(e.oid()) == catalog.JSONB {
			def = catalog.JSONB
		}
	}
	for _, x := range exprs {
		if kv := x.GetJsonKeyValue(); kv != nil {
			k, err := a.analyzeExpr(kv.Key, sc)
			if err != nil {
				return nil, err
			}
			if err := a.bind(k, catalog.Text, loc(kv.Key)); err != nil {
				return nil, err
			}
			ve, err := a.jsonValue(kv.Value, sc)
			if err != nil {
				return nil, err
			}
			note(ve)
			continue
		}
		if v := x.GetJsonValueExpr(); v != nil {
			ve, err := a.jsonValue(v, sc)
			if err != nil {
				return nil, err
			}
			note(ve)
			continue
		}
		e, err := a.analyzeExpr(x, sc)
		if err != nil {
			return nil, err
		}
		note(e)
	}
	t, err := a.jsonOutput(out, def)
	if err != nil {
		return nil, err
	}
	return &expr{typ: t, node: n}, nil
}

// jsonAgg is JSON_ARRAYAGG / JSON_OBJECTAGG: an aggregate with the constructor's clauses.
func (a *analyzer) jsonAgg(c *pg_query.JsonAggConstructor, sc *scope, n *pg_query.Node, analyzeArg func() *Error) (*expr, *Error) {
	if err := analyzeArg(); err != nil {
		return nil, err
	}
	if c.AggFilter != nil {
		fe, err := a.analyzeExpr(c.AggFilter, sc)
		if err != nil {
			return nil, err
		}
		if err := a.bind(fe, catalog.Bool, c.Location); err != nil {
			return nil, err
		}
	}
	for _, s := range c.AggOrder {
		if _, err := a.analyzeExpr(s.GetSortBy().GetNode(), sc); err != nil {
			return nil, err
		}
	}
	if c.Over != nil {
		for _, x := range append(append([]*pg_query.Node{}, c.Over.PartitionClause...), c.Over.OrderClause...) {
			if sb := x.GetSortBy(); sb != nil {
				x = sb.Node
			}
			if _, err := a.analyzeExpr(x, sc); err != nil {
				return nil, err
			}
		}
	} else {
		sc.agg = true
	}
	t, err := a.jsonOutput(c.Output, catalog.JSON)
	if err != nil {
		return nil, err
	}
	return &expr{typ: t, nullable: true, node: n}, nil
}

// jsonTable is JSON_TABLE(context, path COLUMNS (...)) in FROM.
func (a *analyzer) jsonTable(jt *pg_query.JsonTable, sc *scope) (*rte, *Error) {
	if _, err := a.jsonContext(jt.ContextItem, sc, jt.Location); err != nil {
		return nil, err
	}
	if jt.Pathspec != nil {
		if err := a.jsonPathspec(jt.Pathspec.String_, sc); err != nil {
			return nil, err
		}
	}
	if err := a.jsonPassing(jt.Passing, sc); err != nil {
		return nil, err
	}
	if err := jsonBehaviorAllowed(pg_query.JsonExprOp_JSON_TABLE_OP, nil, jt.OnError, "", jt.Location); err != nil {
		return nil, err
	}
	if err := a.jsonBehavior(jt.OnError, sc, 0); err != nil {
		return nil, err
	}
	r := &rte{alias: "json_table"}
	seen := map[string]bool{}
	if jt.Pathspec != nil && jt.Pathspec.Name != "" {
		seen[jt.Pathspec.Name] = true
	}
	if err := a.checkJsonTableNames(jt.Columns, seen); err != nil {
		return nil, err
	}
	cols, err := a.jsonTableColumns(jt.Columns, sc)
	if err != nil {
		return nil, err
	}
	r.cols = cols
	if jt.Alias != nil {
		if jt.Alias.Aliasname != "" {
			r.alias = jt.Alias.Aliasname
		}
		if len(jt.Alias.Colnames) > len(r.cols) {
			return nil, errAt(codeInvalidColumnRef, jt.Location, "JSON_TABLE function has %d columns available but %d columns specified", len(r.cols), len(jt.Alias.Colnames))
		}
		applyColnames(r.cols, jt.Alias)
	}
	return r, nil
}

// checkJsonTableNames is the column / path name uniqueness rule (registerAllJsonTableColumns):
// every column name and every NESTED PATH name across the whole tree must be distinct, and
// only one FOR ORDINALITY column is allowed per COLUMNS list.
func (a *analyzer) checkJsonTableNames(nodes []*pg_query.Node, seen map[string]bool) *Error {
	ordinality := false
	for _, n := range nodes {
		c := n.GetJsonTableColumn()
		if c == nil {
			continue
		}
		if c.Coltype == pg_query.JsonTableColumnType_JTC_FOR_ORDINALITY {
			if ordinality {
				return errAt(codeSyntaxError, c.Location, "only one FOR ORDINALITY column is allowed")
			}
			ordinality = true
		}
		if path := c.Pathspec.GetName(); path != "" {
			if seen[path] {
				return errAt(codeDuplicateAlias, c.Location, "duplicate JSON_TABLE column or path name: %s", path)
			}
			seen[path] = true
		}
		if c.Name != "" {
			if seen[c.Name] {
				return errAt(codeDuplicateAlias, c.Location, "duplicate JSON_TABLE column or path name: %s", c.Name)
			}
			seen[c.Name] = true
		}
		if c.Coltype == pg_query.JsonTableColumnType_JTC_NESTED {
			if err := a.checkJsonTableNames(c.Columns, seen); err != nil {
				return err
			}
		}
	}
	return nil
}

func (a *analyzer) jsonTableColumns(nodes []*pg_query.Node, sc *scope) ([]rteCol, *Error) {
	var cols []rteCol
	for _, n := range nodes {
		c := n.GetJsonTableColumn()
		if c == nil {
			continue
		}
		switch c.Coltype {
		case pg_query.JsonTableColumnType_JTC_FOR_ORDINALITY:
			cols = append(cols, rteCol{name: c.Name, typ: ref(catalog.Int4)})
		case pg_query.JsonTableColumnType_JTC_NESTED:
			if c.Pathspec != nil {
				if err := a.jsonPathspec(c.Pathspec.String_, sc); err != nil {
					return nil, err
				}
			}
			nested, err := a.jsonTableColumns(c.Columns, sc)
			if err != nil {
				return nil, err
			}
			cols = append(cols, nested...)
		default:
			def := catalog.Text
			if c.Coltype == pg_query.JsonTableColumnType_JTC_EXISTS {
				def = catalog.Bool
			}
			t := ref(def)
			if c.TypeName != nil {
				var err error
				if t, err = a.s.ResolveType(c.TypeName); err != nil {
					return nil, errAt(codeUndefinedObject, c.TypeName.Location, "%v", err)
				}
			}
			if c.Pathspec != nil {
				if err := a.jsonPathspec(c.Pathspec.String_, sc); err != nil {
					return nil, err
				}
			}
			op := pg_query.JsonExprOp_JSON_VALUE_OP
			switch {
			case c.Coltype == pg_query.JsonTableColumnType_JTC_EXISTS:
				op = pg_query.JsonExprOp_JSON_EXISTS_OP
			case c.Coltype == pg_query.JsonTableColumnType_JTC_FORMATTED, c.Format != nil && c.Format.FormatType != pg_query.JsonFormatType_JS_FORMAT_DEFAULT:
				op = pg_query.JsonExprOp_JSON_QUERY_OP
			}
			if err := jsonBehaviorAllowed(op, c.OnEmpty, c.OnError, c.Name, c.Location); err != nil {
				return nil, err
			}
			if (c.Wrapper == pg_query.JsonWrapper_JSW_CONDITIONAL || c.Wrapper == pg_query.JsonWrapper_JSW_UNCONDITIONAL) &&
				c.Quotes == pg_query.JsonQuotes_JS_QUOTES_OMIT {
				return nil, errAt(codeSyntaxError, c.Location, "SQL/JSON QUOTES behavior must not be specified when WITH WRAPPER is used")
			}
			for _, b := range []*pg_query.JsonBehavior{c.OnEmpty, c.OnError} {
				if err := a.jsonBehavior(b, sc, t.OID); err != nil {
					return nil, err
				}
			}
			cols = append(cols, rteCol{name: c.Name, typ: t, nullable: true})
		}
	}
	return cols, nil
}

// xmlExpr types the SQL/XML constructors and predicates.
func (a *analyzer) xmlExpr(x *pg_query.XmlExpr, sc *scope, n *pg_query.Node) (*expr, *Error) {
	if x.Op == pg_query.XmlExprOp_IS_XMLELEMENT {
		// xmlattributes: an unnamed value must be a column reference (it names the
		// attribute), and no name twice
		seen := map[string]bool{}
		for _, na := range x.NamedArgs {
			rt := na.GetResTarget()
			name := rt.GetName()
			if name == "" {
				cr := rt.GetVal().GetColumnRef()
				if cr == nil {
					return nil, errAt(codeSyntaxError, rt.GetLocation(), "unnamed XML attribute value must be a column reference")
				}
				fields := cr.Fields
				name = fields[len(fields)-1].GetString_().GetSval()
			}
			if seen[name] {
				return nil, errAt(codeSyntaxError, rt.GetLocation(), "XML attribute name %q appears more than once", name)
			}
			seen[name] = true
		}
	}
	for _, arg := range append(append([]*pg_query.Node{}, x.NamedArgs...), x.Args...) {
		if ra := arg.GetResTarget(); ra != nil {
			arg = ra.Val // xmlattributes(expr AS name), xmlforest(expr AS name)
		}
		e, err := a.analyzeExpr(arg, sc)
		if err != nil {
			return nil, err
		}
		switch x.Op {
		case pg_query.XmlExprOp_IS_XMLPARSE:
			if err := a.bind(e, catalog.Text, loc(arg)); err != nil {
				return nil, err
			}
		case pg_query.XmlExprOp_IS_DOCUMENT:
			if err := a.bind(e, xmlOID, loc(arg)); err != nil {
				return nil, err
			}
			if a.baseType(e.oid()) != xmlOID {
				return nil, errAt(codeDatatypeMismatch, loc(arg), "argument of IS DOCUMENT must be type xml, not type %s", a.s.Types.Format(e.typ))
			}
		case pg_query.XmlExprOp_IS_XMLCONCAT:
			if err := a.bind(e, xmlOID, loc(arg)); err != nil {
				return nil, err
			}
			if a.baseType(e.oid()) != xmlOID {
				return nil, errAt(codeDatatypeMismatch, loc(arg), "argument of XMLCONCAT must be type xml, not type %s", a.s.Types.Format(e.typ))
			}
		case pg_query.XmlExprOp_IS_XMLROOT:
			if err := a.bind(e, xmlOID, loc(arg)); err != nil {
				return nil, err
			}
		default:
			if err := a.bind(e, catalog.Text, loc(arg)); err != nil {
				return nil, err
			}
		}
	}
	switch x.Op {
	case pg_query.XmlExprOp_IS_DOCUMENT:
		return &expr{typ: ref(catalog.Bool), nullable: true, node: n}, nil
	}
	return &expr{typ: ref(xmlOID), nullable: true, node: n}, nil
}

// xmlSerialize is XMLSERIALIZE (CONTENT | DOCUMENT expr AS type).
func (a *analyzer) xmlSerialize(x *pg_query.XmlSerialize, sc *scope, n *pg_query.Node) (*expr, *Error) {
	e, err := a.analyzeExpr(x.Expr, sc)
	if err != nil {
		return nil, err
	}
	if err := a.bind(e, xmlOID, loc(x.Expr)); err != nil {
		return nil, err
	}
	t, rerr := a.s.ResolveType(x.TypeName)
	if rerr != nil {
		return nil, errAt(codeUndefinedObject, x.TypeName.Location, "%v", rerr)
	}
	return &expr{typ: t, nullable: e.nullable, node: n}, nil
}

// jsonBehaviorAllowed is transformJsonBehavior's table of which ON EMPTY / ON ERROR
// behaviors each SQL/JSON function accepts (42601 otherwise).
func jsonBehaviorAllowed(op pg_query.JsonExprOp, onEmpty, onError *pg_query.JsonBehavior, column string, loc int32) *Error {
	allowed := func(b *pg_query.JsonBehavior, ok ...pg_query.JsonBehaviorType) bool {
		if b == nil {
			return true
		}
		for _, k := range ok {
			if b.Btype == k {
				return true
			}
		}
		return false
	}
	B := func(names ...string) []pg_query.JsonBehaviorType {
		var out []pg_query.JsonBehaviorType
		for _, n := range names {
			out = append(out, pg_query.JsonBehaviorType(pg_query.JsonBehaviorType_value["JSON_BEHAVIOR_"+n]))
		}
		return out
	}
	var emptyOK, errorOK []pg_query.JsonBehaviorType
	switch op {
	case pg_query.JsonExprOp_JSON_EXISTS_OP:
		emptyOK = nil
		errorOK = B("ERROR", "TRUE", "FALSE", "UNKNOWN")
	case pg_query.JsonExprOp_JSON_QUERY_OP:
		emptyOK = B("ERROR", "NULL", "EMPTY", "EMPTY_ARRAY", "EMPTY_OBJECT", "DEFAULT")
		errorOK = emptyOK
	case pg_query.JsonExprOp_JSON_VALUE_OP:
		emptyOK = B("ERROR", "NULL", "DEFAULT")
		errorOK = emptyOK
	case pg_query.JsonExprOp_JSON_TABLE_OP:
		errorOK = B("ERROR", "EMPTY", "EMPTY_ARRAY")
	}
	suffix := ""
	if column != "" {
		suffix = " for column \"" + column + "\""
	}
	if onEmpty != nil && !allowed(onEmpty, emptyOK...) {
		return errAt(codeSyntaxError, loc, "invalid ON EMPTY behavior%s", suffix)
	}
	if onError != nil && !allowed(onError, errorOK...) {
		return errAt(codeSyntaxError, loc, "invalid ON ERROR behavior%s", suffix)
	}
	return nil
}
