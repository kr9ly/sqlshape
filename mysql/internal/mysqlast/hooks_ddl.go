package mysqlast

import (
	"strconv"
	"strings"

	"github.com/kr9ly/sqlshape/mysql/internal/mysqlparse"
)

// Hand-written alternatives on the DDL side: column types with a charset the server
// resolves at parse time, ALTER TABLE's action-and-modifier lists, index options.
func init() {
	// type: nchar / nvarchar -> PT_char_type in the national charset (utf8mb3), binary collation with BINARY
	national := func(charType string, withLength bool) Hook {
		return func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
			var length Value
			bin := kids[len(kids)-1]
			if withLength {
				length = kids[1]
			}
			cs := Const("national_charset_info")
			if bin == Const("true") {
				cs = Const("national_charset_info.bin")
			}
			return &Node{Class: "PT_char_type", Names: []string{"char_type", "length", "charset", "force_binary"},
				Args: []Value{Const("Char_type::" + charType), length, cs, Const("false")}, Start: n.Start, End: n.End}, nil
		}
	}
	register("type", "nchar field_length opt_bin_mod", national("CHAR", true))
	register("type", "nchar opt_bin_mod", national("CHAR", false))
	register("type", "nvarchar field_length opt_bin_mod", national("VARCHAR", true))
	// type: YEAR_SYM opt_field_length field_options -> PT_year_type (the length is ignored, UNSIGNED deprecated)
	register("type", "YEAR_SYM opt_field_length field_options", build("PT_year_type"))
	// real_type: REAL_SYM -> DOUBLE (FLOAT under MODE_REAL_AS_FLOAT, which the AST does not carry)
	register("real_type", "REAL_SYM", constant("Numeric_type::DOUBLE"))
	// unicode: UNICODE_SYM [BINARY] -> the ucs2 charset / ucs2_bin collation
	register("unicode", "UNICODE_SYM", constant("ucs2"))
	register("unicode", "UNICODE_SYM BINARY_SYM", constant("ucs2_bin"))
	// opt_key_algo: ALGORITHM_SYM EQ real_ulong_num -> 1 / 2 are the only algorithms
	register("opt_key_algo", "ALGORITHM_SYM EQ real_ulong_num", func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
		switch kids[2] {
		case Number(1):
			return Const("enum_key_algorithm::KEY_ALGORITHM_51"), nil
		case Number(2):
			return Const("enum_key_algorithm::KEY_ALGORITHM_55"), nil
		}
		return nil, &Unsupported{Rule: n.Kind.String(), Alt: n.Alt, Start: n.Start, End: n.End, Text: n.Text(b.SQL)}
	})
	// ALTER TABLE's list of actions and modifiers: {flags, actions}
	register("alter_list", "alter_list ',' create_table_options_space_separated", func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
		st, _ := kids[0].(*Struct)
		acts, _ := st.Fields["actions"].(List)
		opts, _ := kids[2].(List)
		st.Fields["actions"] = append(acts, opts...)
		return st, nil
	})
	register("alter_commands_modifier_list", "alter_commands_modifier_list ',' alter_commands_modifier", func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
		return mergeStructs(kids[0], kids[2]), nil
	})
	register("opt_alter_command_list", "alter_commands_modifier_list ',' alter_list", func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
		st, _ := kids[2].(*Struct)
		if st == nil {
			st = &Struct{Fields: map[string]Value{}}
		}
		st.Fields["flags"] = mergeStructs(kids[0], st.Fields["flags"])
		return st, nil
	})
	register("opt_alter_table_actions", "opt_alter_command_list alter_table_partition_options", func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
		st, _ := kids[0].(*Struct)
		if st == nil {
			st = &Struct{Fields: map[string]Value{}, Order: []string{"flags", "actions"}}
		}
		acts, _ := st.Fields["actions"].(List)
		st.Fields["actions"] = append(acts, kids[1])
		return st, nil
	})
	// fulltext_index_option: WITH PARSER_SYM IDENT_sys -> PT_fulltext_index_parser_name
	register("fulltext_index_option", "WITH PARSER_SYM IDENT_sys", build("PT_fulltext_index_parser_name", 3))
	// opt_key_usage_list: %empty -> a list holding the empty hint (USE INDEX ())
	register("opt_key_usage_list", "", func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
		return List{&Node{Class: "Index_hint", Names: []string{"str", "length"}, Args: []Value{nil, Number(0)}}}, nil
	})
	// select_option_list: select_option_list select_option -> the options merged
	register("select_option_list", "select_option_list select_option", func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
		return mergeStructs(kids[0], kids[1]), nil
	})
	// signed_literal: '-' NUM_literal -> the literal negated
	register("signed_literal", "'-' NUM_literal", func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
		return &Node{Class: "Item_func_neg", Names: []string{"a"}, Args: []Value{kids[1]}, Start: n.Start, End: n.End}, nil
	})
	// size_number: IDENT_sys -> a byte count with an optional K/M/G/T/P/E suffix
	register("size_number", "IDENT_sys", func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
		t, _ := kids[0].(Token)
		s := t.Value
		if s == "" {
			s = t.Text
		}
		shift := 0
		if len(s) > 0 {
			switch strings.ToUpper(s[len(s)-1:]) {
			case "K":
				shift = 10
			case "M":
				shift = 20
			case "G":
				shift = 30
			case "T":
				shift = 40
			case "P":
				shift = 50
			case "E":
				shift = 60
			}
			if shift > 0 {
				s = s[:len(s)-1]
			}
		}
		v, err := strconv.ParseUint(s, 10, 64)
		if err != nil {
			return nil, &Unsupported{Rule: n.Kind.String(), Alt: n.Alt, Start: n.Start, End: n.End, Text: n.Text(b.SQL)}
		}
		return Number(int64(v << shift)), nil
	})
	// query_primary: explicit_table (TABLE t) -> SELECT * FROM t
	register("query_primary", "explicit_table", func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
		items := List{&Node{Class: "Item_asterisk", Names: []string{"opt_table_name", "opt_schema_name"}, Args: []Value{nil, nil}}}
		return &Node{Class: "PT_explicit_table", Names: []string{"options", "item_list", "from_clause"}, Args: []Value{nil, items, kids[0]}, Start: n.Start, End: n.End}, nil
	})
	register("explicit_table", "TABLE_SYM table_ident", func(b *Builder, n *mysqlparse.Node, kids []Value) (Value, error) {
		return List{&Node{Class: "PT_table_factor_table_ident", Names: []string{"table_ident", "opt_use_partition", "opt_table_alias", "opt_key_definition", "opt_tablesample"},
			Args: []Value{kids[1], nil, nil, nil, nil}, Start: n.Start, End: n.End}}, nil
	})
	// json_attribute: TEXT_STRING_sys (validated as JSON by the server)
	register("json_attribute", "TEXT_STRING_sys", pass(1))
}

// mergeStructs returns a's fields with b's set on top (nil operands allowed).
func mergeStructs(a, b Value) Value {
	out := &Struct{Fields: map[string]Value{}}
	for _, v := range []Value{a, b} {
		if st, ok := v.(*Struct); ok {
			for _, k := range st.Order {
				if _, dup := out.Fields[k]; !dup {
					out.Order = append(out.Order, k)
				}
				out.Fields[k] = st.Fields[k]
			}
		}
	}
	if len(out.Order) == 0 {
		if a != nil {
			return a
		}
		return b
	}
	return out
}
