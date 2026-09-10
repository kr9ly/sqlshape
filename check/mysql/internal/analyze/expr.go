package analyze

import (
	"fmt"
	"strings"

	"github.com/kr9ly/sqlshape/check/mysql/v2/internal/catalog"
	"github.com/kr9ly/sqlshape/check/mysql/v2/internal/mysqlast"
	"github.com/kr9ly/sqlshape/check/mysql/v2/internal/schema"
	"github.com/kr9ly/sqlshape/v2/x/sqlmode"
)

func isParam(v mysqlast.Value) bool {
	n, ok := v.(*mysqlast.Node)
	return ok && n.Class == "Item_param"
}

// setParam records the type context gives a placeholder.
func (a *analyzer) setParam(v mysqlast.Value, t schema.Type) {
	n, ok := v.(*mysqlast.Node)
	if !ok || n.Class != "Item_param" {
		return
	}
	i := a.ph.Number(n.Start) - 1
	if i < 0 || i >= len(a.params) {
		return
	}
	// the first context wins: the same $n in two places keeps its first type
	if !a.params[i].Known {
		a.params[i] = Param{Type: t, Known: true}
	}
}

// noteParamSource records the table column a placeholder met (the first one wins).
func (a *analyzer) noteParamSource(v mysqlast.Value, table *schema.Table, col *schema.Column, assigned bool) {
	n, ok := v.(*mysqlast.Node)
	if !ok || n.Class != "Item_param" || table == nil || col == nil {
		return
	}
	num := a.ph.Number(n.Start)
	if a.paramSrc == nil {
		a.paramSrc = map[int]*ParamSource{}
	}
	if _, done := a.paramSrc[num]; !done {
		a.paramSrc[num] = &ParamSource{Table: table.Name, Column: col.Name, NotNull: col.NotNull, Assigned: assigned}
	}
}

// paramSources records, for the placeholders among the operands of a comparison (=, IN,
// BETWEEN, LIKE ...), the one table column among the other operands: a parameter compared
// with a column stands for that column.
func (a *analyzer) paramSources(sc scope, args []mysqlast.Value) {
	var table *schema.Table
	var col *schema.Column
	for _, v := range args {
		if isParam(v) {
			continue
		}
		if ref, ok := a.plainColumn(sc, v); ok && ref.c.base != nil && ref.c.baseTable != nil {
			if col != nil && (col != ref.c.base || table != ref.c.baseTable) {
				return // two columns: the parameter stands for neither
			}
			table, col = ref.c.baseTable, ref.c.base
		}
	}
	if col == nil {
		return
	}
	for _, v := range args {
		a.noteParamSource(v, table, col, false)
	}
}

// setParamField types a placeholder by an enum_field_types name.
func (a *analyzer) setParamField(v mysqlast.Value, ft string) {
	if t, ok := fromFieldType(ft, false); ok {
		a.setParam(v, t)
	}
}

// expr types an expression: column references, literals and placeholders directly; the
// operators by the server's rules (types.go); function calls by their Item class in the
// catalog. Every construct is walked for its errors and its placeholders; what has no
// rule yet comes back untyped.
func (a *analyzer) expr(sc scope, v mysqlast.Value, where string) (typed, error) {
	switch x := v.(type) {
	case nil:
		return unknown, nil
	case mysqlast.List:
		for _, e := range x {
			if _, err := a.expr(sc, e, where); err != nil {
				return unknown, err
			}
		}
		return unknown, nil
	case *mysqlast.Struct:
		for _, k := range x.Order {
			if _, err := a.expr(sc, x.Fields[k], where); err != nil {
				return unknown, err
			}
		}
		return unknown, nil
	case *mysqlast.Node:
		return a.node(sc, x, where)
	}
	return unknown, nil
}

// exprs types a list of expressions.
func (a *analyzer) exprs(sc scope, vs []mysqlast.Value, where string) ([]typed, error) {
	out := make([]typed, len(vs))
	for i, v := range vs {
		t, err := a.expr(sc, v, where)
		if err != nil {
			return nil, err
		}
		out[i] = t
	}
	return out, nil
}

func (a *analyzer) node(sc scope, n *mysqlast.Node, where string) (typed, error) {
	switch n.Class {
	case "PTI_simple_ident_ident", "PTI_simple_ident_nospvar_ident", "PTI_simple_ident_q_2d", "PTI_simple_ident_q_3d":
		ref, err := a.column(sc, n, where)
		if err != nil {
			return unknown, err
		}
		return typeOfColumn(ref.c), nil
	case "Item_param", "PTI_user_variable":
		return unknown, nil

	// literals
	case "Item_int":
		t := known("bigint", false)
		t.typ.Length = len(strings.TrimLeft(str(n.Arg("i")), "-"))
		return t, nil
	case "Item_uint":
		t := known("bigint", false)
		t.typ.Unsigned = true
		return t, nil
	case "Item_decimal":
		t := known("decimal", false)
		s := str(n.Arg("str"))
		if i := strings.IndexByte(s, '.'); i >= 0 {
			t.typ.Dec = len(s) - i - 1
			t.typ.Length = len(strings.TrimLeft(s, "-")) - 1
		} else {
			t.typ.Dec = 0
			t.typ.Length = len(strings.TrimLeft(s, "-"))
		}
		return t, nil
	case "Item_float":
		return known("double", false), nil
	case "PTI_text_literal_text_string", "PTI_text_literal_nchar_string", "PTI_text_literal_underscore_charset", "PTI_text_literal_concat":
		t := known("varchar", false)
		if tok, ok := n.Arg("literal").(mysqlast.Token); ok {
			t.typ.Length = len([]rune(tok.Value))
		}
		return t, nil
	case "Item_hex_string", "Item_bin_string", "PTI_literal_underscore_charset_hex_num", "PTI_literal_underscore_charset_bin_num":
		return known("varbinary", false), nil
	case "PTI_temporal_literal":
		return unknown, nil
	case "Item_null":
		return known("null", true), nil
	case "Item_func_true", "Item_func_false":
		return boolean(false), nil

	// comparisons and logic: a bigint(1), NULL when an operand is
	case "PTI_comp_op":
		ts, err := a.exprs(sc, []mysqlast.Value{n.Arg("left"), n.Arg("right")}, where)
		if err != nil {
			return unknown, err
		}
		a.paramsFromOthers([]mysqlast.Value{n.Arg("left"), n.Arg("right")}, ts, "")
		a.paramSources(sc, []mysqlast.Value{n.Arg("left"), n.Arg("right")})
		return boolean(ts[0].nullable || ts[1].nullable), nil
	case "Item_func_in":
		list, _ := n.Arg("list").(mysqlast.List)
		ts, err := a.exprs(sc, list, where)
		if err != nil {
			return unknown, err
		}
		a.paramsFromOthers(list, ts, "")
		a.paramSources(sc, list)
		return boolean(anyNullable(ts)), nil
	case "Item_func_between", "Item_func_like", "Item_func_strcmp":
		args := exprArgs(n)
		ts, err := a.exprs(sc, args, where)
		if err != nil {
			return unknown, err
		}
		a.paramsFromOthers(args, ts, "")
		a.paramSources(sc, args)
		return boolean(anyNullable(ts)), nil
	case "Item_cond_and", "Item_cond_or", "Item_func_xor":
		ts, err := a.exprs(sc, exprArgs(n), where)
		if err != nil {
			return unknown, err
		}
		return boolean(anyNullable(ts)), nil
	case "Item_func_isnull", "Item_func_isnotnull":
		if _, err := a.expr(sc, n.Arg("a"), where); err != nil {
			return unknown, err
		}
		return boolean(false), nil
	case "PTI_truth_transform":
		t, err := a.expr(sc, n.Arg("expr"), where)
		if err != nil {
			return unknown, err
		}
		if str(n.Arg("truth_test")) == "Item::BOOL_NEGATED" { // NOT x is NULL for NULL x; IS [NOT] TRUE / FALSE never is
			return boolean(t.nullable), nil
		}
		return boolean(false), nil
	// subqueries: entered with this scope as the enclosing one, for correlated references
	case "PTI_exists_subselect":
		if _, err := a.subquery(n.Arg("subselect"), &sc); err != nil {
			return unknown, err
		}
		return boolean(false), nil
	case "Item_in_subselect", "PTI_comp_op_all":
		// x IN (SELECT c ...), x = ANY (SELECT c ...): a placeholder x takes c's type
		left := n.Arg("left_expr")
		if n.Class == "PTI_comp_op_all" {
			left = n.Arg("left")
		}
		sub := n.Arg("pt_subquery")
		if n.Class == "PTI_comp_op_all" {
			sub = n.Arg("subselect")
		}
		lt, err := a.expr(sc, left, where)
		if err != nil {
			return unknown, err
		}
		cols, err := a.subquery(sub, &sc)
		if err != nil {
			return unknown, err
		}
		row, isRow := left.(*mysqlast.Node)
		if isRow && row.Class == "Item_row" {
			if len(cols) != 1+len(exprArgsTail(row)) {
				return unknown, &Error{Message: fmt.Sprintf("Operand should contain %d column(s)", 1+len(exprArgsTail(row))), Code: 1241, Position: a.ph.Back(n.Start)}
			}
		} else if len(cols) != 1 {
			return unknown, &Error{Message: "Operand should contain 1 column(s)", Code: 1241, Position: a.ph.Back(n.Start)}
		}
		_ = lt
		if isParam(left) && len(cols) == 1 && cols[0].Known {
			a.setParam(left, cols[0].Type)
			if cols[0].base != nil && cols[0].baseTable != nil {
				a.noteParamSource(left, cols[0].baseTable, cols[0].base, false)
			}
		}
		return boolean(true), nil // the server marks every IN / ANY / ALL over a subquery nullable
	case "PTI_singlerow_subselect":
		return a.scalarSubquery(n.Arg("subselect"), sc, n.Start)

	// arithmetic
	case "Item_func_plus", "Item_func_minus", "Item_func_mul", "Item_func_mod":
		ts, err := a.exprs(sc, []mysqlast.Value{n.Arg("a"), n.Arg("b")}, where)
		if err != nil {
			return unknown, err
		}
		a.paramsFromOthers([]mysqlast.Value{n.Arg("a"), n.Arg("b")}, ts, "")
		t := numOp(ts[0], ts[1], n.Class == "Item_func_mod")
		if n.Class == "Item_func_minus" && a.s.Settings.SQLMode.Has(sqlmode.NoUnsignedSubtraction) {
			t.typ.Unsigned = false // Item_func_minus::result_precision under NO_UNSIGNED_SUBTRACTION
		}
		if n.Class == "Item_func_mod" {
			t.nullable = true // x % 0 is NULL
		}
		return t, nil
	case "Item_func_div":
		ts, err := a.exprs(sc, []mysqlast.Value{n.Arg("a"), n.Arg("b")}, where)
		if err != nil {
			return unknown, err
		}
		a.paramsFromOthers([]mysqlast.Value{n.Arg("a"), n.Arg("b")}, ts, "")
		t := numOp(ts[0], ts[1], false)
		if t.typ.Name == "bigint" { // an integer division is exact: decimal
			t.typ = schema.Type{Name: "decimal", Length: -1, Dec: -1}
		}
		t.nullable = true // division by zero
		return t, nil
	case "Item_func_div_int":
		ts, err := a.exprs(sc, []mysqlast.Value{n.Arg("a"), n.Arg("b")}, where)
		if err != nil {
			return unknown, err
		}
		a.paramsFromOthers([]mysqlast.Value{n.Arg("a"), n.Arg("b")}, ts, "LONGLONG")
		t := known("bigint", true)
		t.typ.Unsigned = ts[0].typ.Unsigned || ts[1].typ.Unsigned
		return t, nil
	case "Item_func_neg":
		t, err := a.expr(sc, n.Arg("a"), where)
		if err != nil {
			return unknown, err
		}
		out := num1(t, false)
		out.typ.Unsigned = false
		return out, nil
	case "Item_func_bit_and", "Item_func_bit_or", "Item_func_bit_xor", "Item_func_shift_left", "Item_func_shift_right", "Item_func_bit_neg":
		ts, err := a.exprs(sc, exprArgs(n), where)
		if err != nil {
			return unknown, err
		}
		a.paramsFromOthers(exprArgs(n), ts, "LONGLONG")
		t := known("bigint", anyNullable(ts))
		t.typ.Unsigned = true
		return t, nil

	// branches: aggregate_type over the values a branch can produce
	case "Item_func_if":
		args := []mysqlast.Value{n.Arg("a"), n.Arg("b"), n.Arg("c")}
		ts, err := a.exprs(sc, args, where)
		if err != nil {
			return unknown, err
		}
		a.setParamField(args[0], "LONGLONG")
		a.paramsFromOthers(args[1:], ts[1:], "")
		t := aggregate(ts[1:])
		t.nullable = ts[1].nullable || ts[2].nullable
		return t, nil
	case "Item_func_case":
		list, _ := n.Arg("list").(mysqlast.List)
		var whens, thens []mysqlast.Value
		for i, v := range list {
			if i%2 == 0 {
				whens = append(whens, v)
			} else {
				thens = append(thens, v)
			}
		}
		if first := n.Arg("first_expr"); first != nil {
			whens = append(whens, first)
		}
		wt, err := a.exprs(sc, whens, where)
		if err != nil {
			return unknown, err
		}
		a.paramsFromOthers(whens, wt, "")
		var results []mysqlast.Value
		results = append(results, thens...)
		if e := n.Arg("else_expr"); e != nil {
			results = append(results, e)
		}
		rt, err := a.exprs(sc, results, where)
		if err != nil {
			return unknown, err
		}
		a.paramsFromOthers(results, rt, "")
		t := aggregate(rt)
		t.nullable = n.Arg("else_expr") == nil || anyNullable(rt)
		return t, nil
	case "Item_func_coalesce", "Item_func_ifnull", "Item_func_any_value":
		args := exprArgs(n)
		ts, err := a.exprs(sc, args, where)
		if err != nil {
			return unknown, err
		}
		a.paramsFromOthers(args, ts, "")
		t := aggregate(ts)
		t.nullable = true
		for _, x := range ts {
			if !x.nullable {
				t.nullable = false // a non-nullable argument guarantees a value
			}
		}
		return t, nil
	case "Item_func_nullif":
		ts, err := a.exprs(sc, exprArgs(n), where)
		if err != nil {
			return unknown, err
		}
		a.paramsFromOthers(exprArgs(n), ts, "")
		t := ts[0]
		t.nullable = true
		return t, nil

	// casts
	case "create_func_cast":
		return a.cast(sc, n, where)
	case "Item_func_set_collation", "Item_func_conv_charset":
		t, err := a.expr(sc, n.Arg("a"), where)
		if err != nil {
			return unknown, err
		}
		if t.known && kindOf(t.typ) != "STRING_RESULT" {
			t = known("varchar", true)
		}
		t.nullable = true // an Item_str_func: nullable in strict mode (Item_str_func::fix_fields)
		return t, nil

	// the registry: a function named in the statement
	case "PTI_function_call_generic_ident_sys", "PTI_function_call_generic_2d":
		return a.call(sc, n, where)
	}

	// any other Item class the grammar builds directly: judged by its class in the catalog
	if strings.HasPrefix(n.Class, "Item_") || strings.HasPrefix(n.Class, "PTI_function_call_nonkeyword_") || n.Class == "PTI_count_sym" {
		class := n.Class
		switch {
		case strings.HasPrefix(class, "PTI_function_call_nonkeyword_now"):
			class = "Item_func_now"
		case strings.HasPrefix(class, "PTI_function_call_nonkeyword_sysdate"):
			class = "Item_func_now"
		case class == "PTI_count_sym": // COUNT(*)
			class = "Item_sum_count"
		}
		args := exprArgs(n)
		ts, err := a.exprs(sc, args, where)
		if err != nil {
			return unknown, err
		}
		if _, ok := catalog.Items[class]; ok {
			return a.classType(class, args, ts), nil
		}
		return unknown, nil
	}
	// anything else: walk it for its errors and parameters, and leave it untyped
	if _, err := a.expr(sc, mysqlast.List(n.Args), where); err != nil {
		return unknown, err
	}
	return unknown, nil
}

// call types a function of the native registry.
func (a *analyzer) call(sc scope, n *mysqlast.Node, where string) (typed, error) {
	name := str(n.Arg("ident"))
	var args []mysqlast.Value
	list, _ := n.Arg("opt_udf_expr_list").(mysqlast.List)
	for _, e := range list {
		if u, ok := e.(*mysqlast.Node); ok && u.Class == "PTI_udf_expr" {
			args = append(args, u.Arg("expr"))
		} else {
			args = append(args, e)
		}
	}
	f := catalog.Lookup(name)
	if f == nil {
		return unknown, &Error{Message: fmt.Sprintf("FUNCTION %s does not exist", name), Code: 1305, Position: a.ph.Back(n.Start)}
	}
	if f.Min >= 0 && !f.Accepts(len(args)) {
		return unknown, &Error{Message: fmt.Sprintf("Incorrect parameter count in the call to native function '%s'", strings.ToUpper(name)), Code: 1582, Position: a.ph.Back(n.Start)}
	}
	ts, err := a.exprs(sc, args, where)
	if err != nil {
		return unknown, err
	}
	switch f.Factory {
	case "Datediff_instantiator": // TO_DAYS(a) - TO_DAYS(b): a bigint, NULL for an invalid date
		a.setParamField(args[0], "DATETIME")
		a.setParamField(args[1], "DATETIME")
		return known("bigint", true), nil
	case "X_instantiator", "Y_instantiator", "Latitude_instantiator", "Longitude_instantiator":
		if len(args) == 1 { // the observer reads a coordinate; with two arguments the mutator returns the geometry
			a.setParamField(args[0], "GEOMETRY")
			return known("double", anyNullable(ts)), nil
		}
	case "Srid_instantiator":
		if len(args) == 1 {
			a.setParamField(args[0], "GEOMETRY")
			return known("bigint", anyNullable(ts)), nil
		}
	case "From_unixtime_instantiator":
		if len(args) == 1 { // Item_func_from_unixtime; with a format it is DATE_FORMAT
			a.setParamField(args[0], "NEWDECIMAL")
			return known("datetime", true), nil
		}
	}
	return a.classType(f.Class, args, ts), nil
}

// classType is what an Item class returns over typed arguments, from the catalog: the
// family fixes the result kind, the facts refine the type, the nullability and the
// placeholders' types; the hybrid families compute from the arguments.
func (a *analyzer) classType(class string, args []mysqlast.Value, ts []typed) typed {
	fs := classFacts(class)
	// placeholders: param_type_is_default gives a type by position, param_type_uses_non_param the others' type
	for _, f := range fs {
		if from, to, ft, ok := paramDefault(f); ok {
			if to < 0 || to > len(args) {
				to = len(args)
			}
			for i := from; i < to && i < len(args); i++ {
				a.setParamField(args[i], ft)
			}
		}
	}
	if ft, ok := paramNonParam(fs); ok {
		a.paramsFromOthers(args, ts, ft)
	}

	nullable := anyNullable(ts)
	if nb, ok := factNullable(fs); ok {
		nullable = nb
	}
	fam := catalog.FamilyOf(class)
	switch {
	case fixNullable(class):
		nullable = anyNullable(ts) // its fix_fields decides, last
	case strictNullable(class) && a.s.Settings.Strict():
		nullable = true // Item_str_func::fix_fields: nullable in strict mode (the default)
	case fam == "Item_json_func":
		nullable = true // Item_json_func's constructor
	case strings.HasPrefix(class, "Item_typecast_"):
		nullable = true // a value the cast cannot convert becomes NULL
	}
	var t typed
	switch {
	// the hybrids: the arguments decide
	case isA(class, "Item_func_int_val"):
		if len(ts) > 0 {
			t = num1(ts[0], true)
		}
	case isA(class, "Item_func_num1"):
		if len(ts) > 0 {
			t = num1(ts[0], false)
			if class == "Item_func_round" && len(ts) == 1 && t.known && t.typ.Name == "decimal" {
				t.typ.Dec = 0 // ROUND(x) rounds to an integer
				t.typ.Length = ts[0].typ.Length
			}
		}
	case isA(class, "Item_num_op"):
		if len(ts) == 2 {
			t = numOp(ts[0], ts[1], false)
		}
	case isA(class, "Item_func_min_max"):
		t = aggregate(ts)
		if t.known && t.typ.Name == "json" {
			t = known("varchar", t.nullable) // GREATEST / LEAST compare JSON as strings
		}
	case fam == "Item_temporal_hybrid_func":
		// ADDTIME / SUBTIME / TIMESTAMP(): a TIME stays TIME, any other temporal first argument
		// makes a DATETIME, a string stays a string. STR_TO_DATE: a DATETIME unless the
		// format is a constant (not read). DATE_ADD is built by the grammar, not here.
		name := "VARCHAR"
		switch {
		case class == "Item_func_str_to_date":
			name = strToDateType(args)
		case len(ts) > 0 && ts[0].known && ts[0].typ.Name == "time":
			name = "TIME"
		case len(ts) > 0 && ts[0].known && isTemporal(ts[0].typ):
			name = "DATETIME"
		}
		if typ, ok := fromFieldType(name, false); ok {
			t = typed{typ: typ, known: true}
		}
	case isA(class, "Item_func_coalesce"):
		t = aggregate(ts)
		nullable = true
		for _, x := range ts {
			if !x.nullable {
				nullable = false // a non-nullable argument guarantees a value
			}
		}
	case isA(class, "Item_func_nullif"):
		if len(ts) > 0 {
			t = ts[0]
			if t.known && kindOf(t.typ) == "STRING_RESULT" && t.typ.Name != "json" { // set_data_type_string: temporal values included
				typ, _ := fromFieldType("VARCHAR", isBinary(t.typ))
				typ.Length = t.typ.Length
				t.typ = typ
			}
			nullable = true
		}
	case isA(class, "Item_sum_hybrid"): // MIN / MAX
		if len(ts) > 0 {
			t = ts[0]
		}
	case isA(class, "Item_sum_sum"): // SUM, AVG
		if len(ts) > 0 && ts[0].known {
			switch numericContext(ts[0]) {
			case "REAL_RESULT":
				t = known("double", true)
			default:
				t = known("decimal", true)
			}
		}
	case class == "Item_func_unix_timestamp":
		// a bigint, or a decimal with the fractional seconds of the argument (a string or a
		// number converts to DATETIME(6) first)
		t = known("bigint", false)
		if len(ts) > 0 && ts[0].known {
			switch {
			case isTemporal(ts[0].typ) && ts[0].typ.Dec <= 0, kindOf(ts[0].typ) == "INT_RESULT":
			default:
				t = known("decimal", false)
			}
		} else if len(ts) > 0 {
			t = known("decimal", false)
		}
	case isA(class, "Item_sum_count"):
		t = known("bigint", false)
	case isA(class, "Item_sum_bit"):
		t = known("bigint", false)
		t.typ.Unsigned = true
	case isA(class, "Item_func_group_concat"):
		t = known("text", true)
	case isA(class, "Item_sum_json_array") || isA(class, "Item_sum_json_object"):
		t = known("json", true)
	case class == "Item_row_number" || class == "Item_rank" || class == "Item_dense_rank" || class == "Item_ntile":
		t = known("bigint", false)
	case class == "Item_cume_dist" || class == "Item_percent_rank":
		t = known("double", false)
	case fam == "Item_bool_func", class == "Item_func_regexp_like":
		t = boolean(nullable) // set_data_type_bool: a bigint(1)
	default:
		name := factType(fs)
		if name == "" {
			switch fam {
			case "Item_int_func", "Item_sum_int":
				name = "LONGLONG"
			case "Item_str_func", "Item_str_ascii_func", "Item_static_string_func":
				name = "VARCHAR"
			case "Item_real_func", "Item_dec_func", "Item_sum_num":
				name = "DOUBLE"
			case "Item_json_func":
				name = "JSON"
			case "Item_geometry_func":
				name = "GEOMETRY"
			case "Item_datetime_func":
				name = "DATETIME"
			case "Item_date_func":
				name = "DATE"
			case "Item_time_func":
				name = "TIME"
			}
		}
		if name != "" {
			// the result collation: binary when the facts say so, or when an Item_str_func
			// sets none (Item's default collation is binary); the arguments' when the facts
			// aggregate them, which a binary argument makes binary
			binary := factBinary(fs)
			switch {
			case binary:
			case class == "Item_func_quote":
				binary = false // a binary argument's quotes take the connection's collation
			case strings.Contains(strings.Join(fs, ";"), "args[0]->collation"):
				binary = len(ts) > 0 && ts[0].known && isBinary(ts[0].typ)
			case aggregatesCharset(fs):
				for _, x := range ts {
					if x.known && isBinary(x.typ) {
						binary = true
					}
				}
			case fam == "Item_str_func" && !explicitCharset(fs):
				binary = true
			}
			if typ, ok := fromFieldType(name, binary); ok {
				t = typed{typ: typ, known: true}
			}
		}
	}
	if !t.known {
		return unknown
	}
	if factUnsigned(fs) {
		t.typ.Unsigned = true
	}
	switch {
	case isA(class, "Item_sum_count"), isA(class, "Item_sum_bit"), isA(class, "Item_non_framing_wf"):
		// Item_sum::resolve_type says nullable for every aggregate; these never are
		t.nullable = false
	case isA(class, "Item_sum"):
		t.nullable = true // an aggregate over no rows is NULL
	case class == "Item_func_regexp_replace":
		// set_data_type_string(MAX_BLOB_WIDTH) in the arguments' character set: over a
		// character string the 16M characters exceed max_allowed_packet (64M bytes at
		// utf8mb4), which Item_str_func::fix_fields turns into nullable; binary and
		// numeric arguments stay within it
		t.nullable = nullable
		for _, x := range ts {
			if x.known && kindOf(x.typ) == "STRING_RESULT" && !isBinary(x.typ) && !isTemporal(x.typ) && x.typ.Name != "json" {
				t.nullable = true
			}
		}
	default:
		t.nullable = nullable
	}
	return t
}

// cast types CAST / CONVERT: the target type as written. Casts to temporal types are
// nullable (an unparsable value becomes NULL); the others follow their argument.
func (a *analyzer) cast(sc scope, n *mysqlast.Node, where string) (typed, error) {
	arg, err := a.expr(sc, n.Arg("arg"), where)
	if err != nil {
		return unknown, err
	}
	target, length, dec := "", -1, -1
	binary := false
	switch x := n.Arg("type").(type) {
	case *mysqlast.Struct:
		target = str(x.Fields["target"])
		length = intOr(x.Fields["length"], -1)
		dec = intOr(x.Fields["dec"], -1)
		binary = strings.TrimPrefix(str(x.Fields["charset"]), "&") == "my_charset_bin" || isTrue(x.Fields["binary"])
		if strings.Contains(target, "?ITEM_CAST_DOUBLE:ITEM_CAST_FLOAT") { // the action's ($1 == DOUBLE) ? ... : ...
			target = "ITEM_CAST_FLOAT"
			if up := strings.ToUpper(a.text[n.Start:n.End]); strings.Contains(up, "DOUBLE") || strings.Contains(up, "REAL") {
				target = "ITEM_CAST_DOUBLE"
			}
		}
	default:
		target = str(x) // BINARY x: the cast to CHAR with the binary charset in as_array's slot
		binary = str(n.Arg("as_array")) == "my_charset_bin"
	}
	t := typed{typ: schema.Type{Length: length, Dec: dec}, known: true, nullable: arg.nullable}
	switch strings.TrimPrefix(target, "ITEM_CAST_") {
	case "SIGNED_INT":
		t.typ.Name = "bigint"
	case "UNSIGNED_INT":
		t.typ.Name, t.typ.Unsigned = "bigint", true
	case "CHAR":
		t.typ.Name, t.nullable = "varchar", true // an Item_str_func: nullable in strict mode
		if binary {
			t.typ.Name = "varbinary"
		}
		if length < 0 && arg.known && arg.typ.Length >= 0 {
			t.typ.Length = arg.typ.Length
		}
	case "NCHAR":
		t.typ.Name, t.typ.Charset, t.nullable = "varchar", "utf8mb3", true
	case "DECIMAL":
		t.typ.Name = "decimal"
		if length < 0 {
			t.typ.Length, t.typ.Dec = 10, 0
		} else if dec < 0 {
			t.typ.Dec = 0
		}
	case "FLOAT":
		t.typ.Name = "float"
	case "DOUBLE":
		t.typ.Name = "double"
	case "DATE":
		t.typ.Name, t.typ.Length, t.nullable = "date", -1, true
	case "TIME":
		t.typ.Name, t.typ.Length, t.nullable = "time", -1, true
	case "DATETIME":
		t.typ.Name, t.typ.Length, t.nullable = "datetime", -1, true
	case "YEAR":
		t.typ.Name, t.nullable = "year", true
	case "JSON":
		t.typ.Name, t.nullable = "json", true // Item_json_func: nullable by construction
	case "POINT", "LINESTRING", "POLYGON", "MULTIPOINT", "MULTILINESTRING", "MULTIPOLYGON", "GEOMETRYCOLLECTION":
		t.typ.Name = strings.ToLower(strings.TrimPrefix(target, "ITEM_CAST_"))
	default:
		return unknown, nil
	}
	return t, nil
}

// paramsFromOthers types the placeholders among args by the other arguments: the type
// ft when given (param_type_uses_non_param(thd, TYPE)), else the aggregate of the typed
// arguments (a placeholder compared with a column takes the column's type).
func (a *analyzer) paramsFromOthers(args []mysqlast.Value, ts []typed, ft string) {
	var t schema.Type
	ok := false
	if ft != "" {
		t, ok = fromFieldType(ft, false)
	} else {
		var others []typed
		for i, v := range args {
			if !isParam(v) && i < len(ts) && ts[i].known && ts[i].typ.Name != "null" {
				others = append(others, ts[i])
			}
		}
		if len(others) == 1 {
			t, ok = others[0].typ, true
		} else if len(others) > 1 {
			agg := aggregate(others)
			t, ok = agg.typ, agg.known
		}
	}
	if !ok {
		return
	}
	if t.IsInteger() {
		t.Length = -1 // a display width says nothing about a value
	}
	for _, v := range args {
		if isParam(v) {
			a.setParam(v, t)
		}
	}
}

// exprArgs are the expression arguments of a grammar-built Item node: its Node / List
// arguments, through the PTI_in_sum_expr / PTI_udf_expr wrappers, skipping the
// constants (operator kinds, interval units, flags).
// exprArgsTail is an Item_row's tail (its elements after the head).
func exprArgsTail(row *mysqlast.Node) mysqlast.List {
	l, _ := row.Arg("tail").(mysqlast.List)
	return l
}

func exprArgs(n *mysqlast.Node) []mysqlast.Value {
	var out []mysqlast.Value
	var add func(v mysqlast.Value)
	add = func(v mysqlast.Value) {
		switch x := v.(type) {
		case *mysqlast.Node:
			switch x.Class {
			case "PTI_in_sum_expr", "PTI_udf_expr":
				add(x.Arg("expr"))
			case "PT_window", "String":
				// a window specification, a separator: not an argument
			default:
				out = append(out, x)
			}
		case mysqlast.List:
			for _, e := range x {
				add(e)
			}
		}
	}
	for _, v := range n.Args {
		add(v)
	}
	return out
}

func anyNullable(ts []typed) bool {
	for _, t := range ts {
		if t.nullable {
			return true
		}
	}
	return false
}

// intOr reads a number or a numeric token; def when v is neither.
func intOr(v mysqlast.Value, def int) int {
	switch x := v.(type) {
	case mysqlast.Number:
		return int(x)
	case mysqlast.Token, string, mysqlast.Const:
		if n, ok := atoi(strings.Trim(str(x), "\"")); ok {
			return n
		}
	}
	return def
}

func isTrue(v mysqlast.Value) bool {
	return str(v) == "true"
}

// strToDateType is the type STR_TO_DATE gives from a literal format
// (Item_func_str_to_date::fix_from_format): a TIME when the format has only time parts, a
// DATE when only date parts, a DATETIME when both (or when the format is not a literal).
func strToDateType(args []mysqlast.Value) string {
	if len(args) < 2 {
		return "DATETIME"
	}
	n, ok := args[1].(*mysqlast.Node)
	if !ok || n.Class != "PTI_text_literal_text_string" {
		return "DATETIME"
	}
	tok, ok := n.Arg("literal").(mysqlast.Token)
	if !ok {
		return "DATETIME"
	}
	format := tok.Value
	date, time := false, false
	for i := 0; i+1 < len(format); i++ {
		if format[i] != '%' {
			continue
		}
		i++
		switch {
		case format[i] == 'f', strings.IndexByte("HISThiklrs", format[i]) >= 0:
			time = true
		case strings.IndexByte("MVUXYWabcjmvuxyw", format[i]) >= 0:
			date = true
		}
	}
	switch {
	case time && date:
		return "DATETIME"
	case time:
		return "TIME"
	}
	return "DATE"
}
