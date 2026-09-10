package analyze

import (
	"slices"
	"strings"

	"github.com/kr9ly/sqlshape/mysql/internal/catalog"
	"github.com/kr9ly/sqlshape/mysql/internal/schema"
)

// The type rules, as the server states them. A value has a data type (enum_field_types,
// which schema.Type names the SQL way) and a result kind (Item_result: INT, DECIMAL, REAL
// or STRING), and the rules compose on those: Item_num_op::set_numeric_type for
// arithmetic, Item_func_num1 for one-argument numeric functions, Item::aggregate_type over
// Field::field_type_merge for CASE / IF / COALESCE / GREATEST, and the Item class
// families of the catalog for everything the registry names.

// typed is what the analyzer knows of an expression's value.
type typed struct {
	typ      schema.Type
	known    bool
	nullable bool
}

var unknown = typed{nullable: true}

// known makes a typed of a plain type name.
func known(name string, nullable bool) typed {
	return typed{typ: schema.Type{Name: name, Length: -1, Dec: -1}, known: true, nullable: nullable}
}

// boolean is what a comparison or a logical operator returns: MySQL spells it as a
// bigint of display width 1.
func boolean(nullable bool) typed {
	return typed{typ: schema.Type{Name: "bigint", Length: 1, Dec: -1}, known: true, nullable: nullable}
}

// fieldType is the enum_field_types name (without MYSQL_TYPE_) of a schema type.
func fieldType(t schema.Type) string {
	switch t.Name {
	case "tinyint":
		return "TINY"
	case "smallint":
		return "SHORT"
	case "mediumint":
		return "INT24"
	case "int":
		return "LONG"
	case "bigint":
		return "LONGLONG"
	case "decimal":
		return "NEWDECIMAL"
	case "float":
		return "FLOAT"
	case "double":
		return "DOUBLE"
	case "bit":
		return "BIT"
	case "year":
		return "YEAR"
	case "date":
		return "DATE"
	case "time":
		return "TIME"
	case "datetime":
		return "DATETIME"
	case "timestamp":
		return "TIMESTAMP"
	case "char", "binary":
		return "STRING"
	case "varchar", "varbinary":
		return "VARCHAR"
	case "tinytext", "tinyblob":
		return "TINY_BLOB"
	case "text", "blob":
		return "BLOB"
	case "mediumtext", "mediumblob":
		return "MEDIUM_BLOB"
	case "longtext", "longblob":
		return "LONG_BLOB"
	case "enum":
		return "ENUM"
	case "set":
		return "SET"
	case "json":
		return "JSON"
	case "null":
		return "NULL"
	case "geometry", "point", "linestring", "polygon", "multipoint", "multilinestring", "multipolygon", "geometrycollection", "geomcollection":
		return "GEOMETRY"
	}
	return ""
}

// fromFieldType is the schema type of an enum_field_types name. binary says whether a
// string result is a binary string (BLOB rather than TEXT, VARBINARY rather than VARCHAR).
func fromFieldType(ft string, binary bool) (schema.Type, bool) {
	t := schema.Type{Length: -1, Dec: -1}
	switch ft {
	case "TINY":
		t.Name = "tinyint"
	case "SHORT":
		t.Name = "smallint"
	case "INT24":
		t.Name = "mediumint"
	case "LONG":
		t.Name = "int"
	case "LONGLONG":
		t.Name = "bigint"
	case "DECIMAL", "NEWDECIMAL":
		t.Name = "decimal"
	case "FLOAT":
		t.Name = "float"
	case "DOUBLE":
		t.Name = "double"
	case "BIT":
		t.Name = "bit"
	case "YEAR":
		t.Name = "year"
	case "DATE", "NEWDATE":
		t.Name = "date"
	case "TIME", "TIME2":
		t.Name = "time"
	case "DATETIME", "DATETIME2":
		t.Name = "datetime"
	case "TIMESTAMP", "TIMESTAMP2":
		t.Name = "timestamp"
	case "STRING":
		t.Name = "char"
		if binary {
			t.Name = "binary"
		}
	case "VARCHAR", "VAR_STRING":
		t.Name = "varchar"
		if binary {
			t.Name = "varbinary"
		}
	case "TINY_BLOB":
		t.Name = "tinytext"
		if binary {
			t.Name = "tinyblob"
		}
	case "BLOB":
		t.Name = "text"
		if binary {
			t.Name = "blob"
		}
	case "MEDIUM_BLOB":
		t.Name = "mediumtext"
		if binary {
			t.Name = "mediumblob"
		}
	case "LONG_BLOB":
		t.Name = "longtext"
		if binary {
			t.Name = "longblob"
		}
	case "ENUM":
		t.Name = "enum"
	case "SET":
		t.Name = "set"
	case "JSON":
		t.Name = "json"
	case "NULL":
		t.Name = "null"
	case "GEOMETRY":
		t.Name = "geometry"
	default:
		return t, false
	}
	return t, true
}

// isBinary reports a binary string type.
func isBinary(t schema.Type) bool {
	switch t.Name {
	case "binary", "varbinary", "tinyblob", "blob", "mediumblob", "longblob":
		return true
	}
	return t.Charset == "binary" || t.Binary
}

// kindOf is the Item_result of a type: "INT_RESULT", "DECIMAL_RESULT", "REAL_RESULT" or
// "STRING_RESULT" (Field::result_merge_type).
func kindOf(t schema.Type) string {
	return catalog.ResultKind(fieldType(t))
}

// isTemporal reports the temporal types (is_temporal_type).
func isTemporal(t schema.Type) bool {
	switch t.Name {
	case "date", "time", "datetime", "timestamp":
		return true
	}
	return false
}

// numericContext is Item::numeric_context_result_type: in arithmetic a temporal value
// counts as an integer (a decimal when it has fractional seconds) and a string as a real.
func numericContext(t typed) string {
	if !t.known {
		return "REAL_RESULT"
	}
	if isTemporal(t.typ) {
		if t.typ.Dec > 0 {
			return "DECIMAL_RESULT"
		}
		return "INT_RESULT"
	}
	k := kindOf(t.typ)
	if k == "STRING_RESULT" || k == "" {
		return "REAL_RESULT"
	}
	return k
}

// numOp is Item_num_op::set_numeric_type: a real operand makes a double, else a decimal
// operand makes a decimal, else the result is a bigint. Integer results keep
// unsigned_flag when either operand is unsigned (result_precision); decimals keep it when
// both are. mod is Item_func_mod: unsigned follows the first operand.
func numOp(a, b typed, mod bool) typed {
	ra, rb := numericContext(a), numericContext(b)
	var out typed
	switch {
	case ra == "REAL_RESULT" || rb == "REAL_RESULT":
		out = known("double", false)
	case ra == "DECIMAL_RESULT" || rb == "DECIMAL_RESULT":
		out = known("decimal", false)
		out.typ.Unsigned = a.typ.Unsigned && b.typ.Unsigned
	default:
		out = known("bigint", false)
		out.typ.Unsigned = a.typ.Unsigned || b.typ.Unsigned
	}
	if mod {
		out.typ.Unsigned = a.typ.Unsigned
	}
	out.known = a.known && b.known
	out.nullable = a.nullable || b.nullable
	return out
}

// num1 is Item_func_num1::set_numeric_type: the numeric type of a one-argument numeric
// function follows its argument's result kind. intVal is Item_func_int_val (FLOOR,
// CEILING), which turns a decimal into a bigint when its integer digits fit one, else a
// decimal of scale 0.
func num1(a typed, intVal bool) typed {
	if !a.known {
		return unknown
	}
	var out typed
	switch kindOf(a.typ) {
	case "INT_RESULT":
		out = known("bigint", a.nullable)
		out.typ.Unsigned = a.typ.Unsigned
	case "DECIMAL_RESULT":
		out = known("decimal", a.nullable)
		out.typ.Unsigned = a.typ.Unsigned
		if intVal {
			precision := a.typ.Length - max(a.typ.Dec, 0)
			if a.typ.Dec != 0 {
				precision++
			}
			if a.typ.Length < 0 || precision+1 < 20 { // max_length (with the sign) < DECIMAL_LONGLONG_DIGITS - 2
				out = known("bigint", a.nullable)
			} else {
				out.typ.Dec = 0
				out.typ.Length = precision
			}
		}
	default: // STRING_RESULT, REAL_RESULT
		out = known("double", a.nullable)
	}
	return out
}

// aggregate is Item::aggregate_type: the type a CASE / IF / COALESCE / GREATEST returns
// over its branches. NULL-typed items do not count; the rest merge pairwise through
// field_type_merge, and integers of mixed signedness widen one step. The result is
// nullable when any item is; callers with their own rule (COALESCE, CASE) override it.
func aggregate(items []typed) typed {
	var first *typed
	for i := range items {
		if !items[i].known {
			return unknown
		}
		if items[i].typ.Name != "null" {
			first = &items[i]
			break
		}
	}
	if first == nil {
		if len(items) == 0 {
			return unknown
		}
		return known("null", true)
	}
	ft := fieldType(first.typ)
	if ft == "" {
		return unknown
	}
	unsigned := first.typ.Unsigned
	mixed := false
	binary := isBinary(first.typ)
	nullable := false
	for _, it := range items {
		nullable = nullable || it.nullable
		if it.typ.Name == "null" {
			continue
		}
		other := fieldType(it.typ)
		if other == "" {
			return unknown
		}
		ft = catalog.Merge(ft, other)
		if ft == "" || ft == "INVALID" {
			return unknown
		}
		mixed = mixed || unsigned != it.typ.Unsigned
		binary = binary && isBinary(it.typ)
	}
	if mixed && isIntegerFieldType(ft) {
		bump := false
		for _, it := range items {
			bump = bump || it.typ.Unsigned && (fieldType(it.typ) == ft || fieldType(it.typ) == "BIT")
		}
		unsigned = false
		if bump {
			switch ft {
			case "TINY":
				ft = "SHORT"
			case "SHORT":
				ft = "INT24"
			case "INT24":
				ft = "LONG"
			case "LONG":
				ft = "LONGLONG"
			case "LONGLONG":
				ft = "NEWDECIMAL"
			}
		}
	}
	t, ok := fromFieldType(ft, binary)
	if !ok {
		return unknown
	}
	t.Unsigned = unsigned && isIntegerFieldType(ft)
	// an ENUM / SET aggregates to itself only against NULL; MySQL keeps the typelib then
	if (ft == "ENUM" || ft == "SET") && len(first.typ.Values) > 0 {
		t.Values = first.typ.Values
	}
	return typed{typ: t, known: true, nullable: nullable}
}

func isIntegerFieldType(ft string) bool {
	switch ft {
	case "TINY", "SHORT", "INT24", "LONG", "LONGLONG":
		return true
	}
	return false
}

// facts gathers the resolve_type facts that apply to class: the class's own, after those
// of the base whose resolve_type it calls first (InheritsResolve); a class without a
// resolve_type of its own is judged by the nearest base that has one.
func facts(class string) []string {
	seen := map[string]bool{}
	cur := class
	for cur != "" && !seen[cur] {
		seen[cur] = true
		it, ok := catalog.Items[cur]
		if !ok {
			return nil
		}
		if len(it.Facts) > 0 || it.InheritsResolve != "" {
			var out []string
			if it.InheritsResolve != "" {
				out = append(out, facts(it.InheritsResolve)...)
			}
			return append(out, it.Facts...)
		}
		cur = it.Base
	}
	return nil
}

// isA reports whether class is, or derives from, base.
func isA(class, base string) bool {
	seen := map[string]bool{}
	for cur := class; cur != "" && !seen[cur]; {
		if cur == base {
			return true
		}
		seen[cur] = true
		it, ok := catalog.Items[cur]
		if !ok {
			return false
		}
		cur = it.Base
	}
	return false
}

// factType is the type a `set_data_type_*` fact fixes, when exactly one such fact is
// unconditional; "" when the facts leave it to the arguments.
func factType(fs []string) string {
	found := ""
	for _, f := range fs {
		name := ""
		switch {
		case strings.HasPrefix(f, "set_data_type_string("), strings.HasPrefix(f, "set_data_type_char("):
			name = "VARCHAR"
		case strings.HasPrefix(f, "set_data_type_blob("):
			name = "BLOB"
		case strings.HasPrefix(f, "set_data_type_longlong("):
			name = "LONGLONG"
		case strings.HasPrefix(f, "set_data_type_double("):
			name = "DOUBLE"
		case strings.HasPrefix(f, "set_data_type_float("):
			name = "FLOAT"
		case strings.HasPrefix(f, "set_data_type_decimal("):
			name = "NEWDECIMAL"
		case strings.HasPrefix(f, "set_data_type_datetime("):
			name = "DATETIME"
		case strings.HasPrefix(f, "set_data_type_date("):
			name = "DATE"
		case strings.HasPrefix(f, "set_data_type_time("):
			name = "TIME"
		case strings.HasPrefix(f, "set_data_type_timestamp("):
			name = "TIMESTAMP"
		case strings.HasPrefix(f, "set_data_type_year("):
			name = "YEAR"
		case strings.HasPrefix(f, "set_data_type_json("):
			name = "JSON"
		case strings.HasPrefix(f, "set_data_type_geometry("):
			name = "GEOMETRY"
		case strings.HasPrefix(f, "set_data_type_bit("):
			name = "BIT"
		case strings.HasPrefix(f, "set_data_type(MYSQL_TYPE_"):
			name = strings.TrimSuffix(strings.TrimPrefix(f, "set_data_type(MYSQL_TYPE_"), ")")
		case strings.HasPrefix(f, "set_data_type_from_item("), strings.HasPrefix(f, "set_data_type("):
			return "" // follows an argument or a computed type
		default:
			continue
		}
		if found != "" && found != name {
			return "" // two branches set different types: the arguments decide
		}
		found = name
	}
	return found
}

// factNullable reads set_nullable(true) / set_nullable(false); ok is false when the facts
// say nothing unconditional (the default is then: nullable when any argument is).
func factNullable(fs []string) (nullable, ok bool) {
	for _, f := range fs {
		switch f {
		case "set_nullable(true)":
			nullable, ok = true, true
		case "set_nullable(false)":
			nullable, ok = false, true
		}
	}
	return
}

// factUnsigned reads unsigned_flag=true.
func factUnsigned(fs []string) bool {
	return slices.Contains(fs, "unsigned_flag=true")
}

// paramDefault is one param_type_is_default(thd, from, to[, MYSQL_TYPE_X]) fact: the
// placeholders among args[from:to) take type X (VARCHAR when none is given); to = -1 is
// the end. ok is false for a fact that is not of that form.
func paramDefault(f string) (from, to int, ft string, ok bool) {
	if !strings.HasPrefix(f, "param_type_is_default(") {
		return
	}
	inner := strings.TrimSuffix(strings.TrimPrefix(f, "param_type_is_default("), ")")
	parts := strings.Split(inner, ",")
	if len(parts) < 3 {
		return
	}
	from, ok1 := atoi(strings.TrimSpace(parts[1]))
	to, ok2 := atoi(strings.TrimSpace(parts[2]))
	if !ok1 || !ok2 {
		return
	}
	ft = "VARCHAR"
	if len(parts) > 3 {
		ft = strings.TrimPrefix(strings.TrimSpace(parts[3]), "MYSQL_TYPE_")
		if _, known := fromFieldType(ft, false); !known {
			return 0, 0, "", false // a computed type (assumed_type, m_datetime ? ... : ...)
		}
	}
	return from, to, ft, true
}

// paramNonParam reads param_type_uses_non_param(thd[, MYSQL_TYPE_X]): placeholders take
// the type of the other arguments (or X). ok is false when the fact is absent.
func paramNonParam(fs []string) (ft string, ok bool) {
	for _, f := range fs {
		if !strings.HasPrefix(f, "param_type_uses_non_param(") {
			continue
		}
		inner := strings.TrimSuffix(strings.TrimPrefix(f, "param_type_uses_non_param("), ")")
		parts := strings.Split(inner, ",")
		if len(parts) > 1 {
			return strings.TrimPrefix(strings.TrimSpace(parts[1]), "MYSQL_TYPE_"), true
		}
		return "", true
	}
	return "", false
}

func atoi(s string) (int, bool) {
	n, neg := 0, false
	if strings.HasPrefix(s, "-") {
		neg, s = true, s[1:]
	}
	if s == "" {
		return 0, false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int(c-'0')
	}
	if neg {
		n = -n
	}
	return n, true
}
