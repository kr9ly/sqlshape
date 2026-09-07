package sqlshape_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/kr9ly/sqlshape"
)

// EvalP exercises every kind the runtime evaluator (render.go) has to handle:
// the boolean / comparison builtins (not, and, or, eq, ne, lt, le, gt, ge, len,
// index), constants, field refs and range variables.
type evalSub struct{ X int }

type EvalP struct {
	I, I2      int
	U, U2      uint
	F32        float32
	F64        float64
	S          string
	B, Bf, Bf2 bool
	PI         *int
	PIN        *int
	Sl         []int
	SlE        []int
	M          map[string]int
	Arr, Arr2  [3]int
	Sub        evalSub
	C          complex128
}

var someEvalInt = 7

// TestRenderConditions renders "{{if COND}}THEN{{else}}ELSE{{end}}" for a battery of
// conditions and checks the branch taken; a successful Render already proves the
// evaluator agrees with the static expander (Stmt.Render compares them internally), so
// checking the literal output is enough to know both the branch AND the agreement held.
func TestRenderConditions(t *testing.T) {
	cases := []struct {
		name string
		cond string
		p    EvalP
		then bool
	}{
		{"not_true", "not .Bf", EvalP{Bf: false}, true},
		{"not_false", "not .B", EvalP{B: true}, false},
		{"not_zero_args", "not", EvalP{}, true},
		{"and_zero_args", "and", EvalP{}, false},
		{"or_zero_args", "or", EvalP{}, false},
		{"and_both_true", "and .B .Bf2", EvalP{B: true, Bf2: true}, true},
		{"and_short_circuit", "and .Bf .B", EvalP{Bf: false, B: true}, false},
		{"or_first_true", "or .B .Bf", EvalP{B: true, Bf: false}, true},
		{"or_all_false", "or .Bf .Bf2", EvalP{Bf: false, Bf2: false}, false},
		{"eq_int_true", "eq .I 5", EvalP{I: 5}, true},
		{"eq_int_false", "eq .I 5", EvalP{I: 6}, false},
		{"eq_multi_arg", "eq .I 1 2 3", EvalP{I: 2}, true},
		{"eq_int_uint", "eq .I .U", EvalP{I: 3, U: 3}, true},
		{"eq_uint_int", "eq .U .I", EvalP{U: 3, I: 3}, true},
		{"eq_int_uint_neg", "eq .I .U", EvalP{I: -1, U: 1}, false},
		{"eq_float_true", "eq .F32 .F64", EvalP{F32: 1.5, F64: 1.5}, true},
		{"eq_float_false", "eq .F32 .F64", EvalP{F32: 1.5, F64: 2.5}, false},
		{"eq_float_literal", "eq .F64 1.5", EvalP{F64: 1.5}, true},
		{"eq_string_true", `eq .S "abc"`, EvalP{S: "abc"}, true},
		{"eq_string_false", `eq .S "abc"`, EvalP{S: "xyz"}, false},
		{"eq_bool_true", "eq .B true", EvalP{B: true}, true},
		{"eq_nil_both_nil", "eq .PIN nil", EvalP{}, true},
		{"eq_nil_one_set", "eq .PI nil", EvalP{PI: &someEvalInt}, false},
		{"eq_array_true", "eq .Arr .Arr2", EvalP{Arr: [3]int{1, 2, 3}, Arr2: [3]int{1, 2, 3}}, true},
		{"eq_array_false", "eq .Arr .Arr2", EvalP{Arr: [3]int{1, 2, 3}, Arr2: [3]int{1, 2, 4}}, false},
		{"eq_nested_pipe", "eq (eq .I 5) true", EvalP{I: 5}, true},
		{"ne_true", "ne .I 5", EvalP{I: 6}, true},
		{"ne_false", "ne .I 5", EvalP{I: 5}, false},
		{"lt_int_true", "lt .I 5", EvalP{I: 3}, true},
		{"lt_int_false", "lt .I 5", EvalP{I: 9}, false},
		{"le_int_eq", "le .I 5", EvalP{I: 5}, true},
		{"gt_uint_true", "gt .U .U2", EvalP{U: 9, U2: 2}, true},
		{"ge_uint_eq", "ge .U .U2", EvalP{U: 2, U2: 2}, true},
		{"ge_string_true", `ge .S "abc"`, EvalP{S: "abd"}, true},
		{"lt_string_true", `lt .S "abc"`, EvalP{S: "aaa"}, true},
		{"len_slice_true", "len .Sl", EvalP{Sl: []int{1, 2}}, true},
		{"len_slice_nil", "len .SlE", EvalP{SlE: nil}, false},
		{"len_map_true", "len .M", EvalP{M: map[string]int{"a": 1}}, true},
		{"len_nil_ptr", "len .PIN", EvalP{}, false},
		{"len_string_true", "len .S", EvalP{S: "x"}, true},
		{"index_slice_true", "index .Sl 0", EvalP{Sl: []int{5, 0}}, true},
		{"index_slice_false", "index .Sl 1", EvalP{Sl: []int{5, 0}}, false},
		{"index_map_true", `index .M "k"`, EvalP{M: map[string]int{"k": 1}}, true},
		{"index_map_missing", `index .M "missing"`, EvalP{M: map[string]int{"k": 1}}, false},
		{"index_array_true", "index .Arr 0", EvalP{Arr: [3]int{7, 0, 0}}, true},
		{"index_string_true", "index .S 0", EvalP{S: "a"}, true},
		{"struct_always_true", ".Sub", EvalP{}, true},
		{"float_truthy", ".F64", EvalP{F64: 1.5}, true},
		{"float_falsy", ".F64", EvalP{F64: 0}, false},
		{"complex_truthy", ".C", EvalP{C: complex(1, 0)}, true},
		{"complex_falsy", ".C", EvalP{C: 0}, false},
		{"pipe_multi_stage", ".Bf | not", EvalP{Bf: false}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tmpl := fmt.Sprintf("{{if %s}}THEN{{else}}ELSE{{end}}", tc.cond)
			stmt := sqlshape.Query[struct{}, EvalP](tmpl)
			r, err := stmt.Render(tc.p)
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			want := "ELSE"
			if tc.then {
				want = "THEN"
			}
			if r.SQL != want {
				t.Errorf("sql = %q, want %q", r.SQL, want)
			}
		})
	}
}

// TestRenderErrors checks the ways a template can fail at evaluation time: an
// unsupported function, a bad arg count, incompatible comparison types, a value
// action that is not a plain field reference, an undefined variable, and a range
// over a non-collection.
func TestRenderErrors(t *testing.T) {
	cases := []struct {
		name string
		tmpl string
		p    EvalP
		want string
	}{
		{"unsupported_func", "{{if foo .I}}T{{else}}E{{end}}", EvalP{I: 1}, "foo"},
		{"eq_needs_two", "{{if eq .I}}T{{else}}E{{end}}", EvalP{I: 1}, "eq needs two arguments"},
		{"ne_needs_two", "{{if ne .I 1 2}}T{{else}}E{{end}}", EvalP{I: 1}, "ne needs two arguments"},
		{"lt_needs_two", "{{if lt .I}}T{{else}}E{{end}}", EvalP{I: 1}, "lt needs two arguments"},
		{"incompatible_compare", "{{if lt .I .S}}T{{else}}E{{end}}", EvalP{I: 1, S: "a"}, "incompatible types"},
		{"index_non_indexable", "{{if index .I 0}}T{{else}}E{{end}}", EvalP{I: 1}, "cannot index"},
		{"value_action_not_field", "{{eq .I 1}}", EvalP{I: 1}, "plain field reference"},
		{"undefined_variable", "{{$x}}", EvalP{}, "undefined variable"},
		{"range_non_collection", "{{range .I}}x{{end}}", EvalP{I: 5}, "range over"},
		{"field_chain_non_struct", "{{if .I.Foo}}T{{else}}E{{end}}", EvalP{I: 1}, "non-struct"},
		{"field_chain_no_field", "{{if .Sub.NoSuchField}}T{{else}}E{{end}}", EvalP{}, "has no field"},
		{"bad_pipeline", "{{if .B .Bf}}T{{else}}E{{end}}", EvalP{B: true, Bf: false}, "bad pipeline"},
		{"chain_node_unsupported", "{{if eq (.Sub).X 1}}T{{else}}E{{end}}", EvalP{Sub: evalSub{X: 1}}, "unsupported expression"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stmt := sqlshape.Query[struct{}, EvalP](tc.tmpl)
			_, err := stmt.Render(tc.p)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
}

// TestRenderValueActionsAndRange covers plain field / dot / variable value actions and
// the shapes of range: 0 (else), 1, 2 (checked against the static expander), and more
// than 2 iterations (unchecked, no static twin), plus range over a map and a range whose
// body itself branches (every then/else combination across iterations is one the static
// expander enumerated).
func TestRenderValueActionsAndRange(t *testing.T) {
	cases := []struct {
		name     string
		tmpl     string
		p        EvalP
		wantSQL  string
		wantArgs []any
	}{
		{"field_ref", "{{.I}}", EvalP{I: 42}, "$1", []any{42}},
		{"range_else_empty", "{{range .Sl}}[{{.}}]{{else}}EMPTY{{end}}", EvalP{Sl: nil}, "EMPTY", nil},
		{"range_one", "{{range .Sl}}[{{.}}]{{else}}EMPTY{{end}}", EvalP{Sl: []int{7}}, "[$1]", []any{7}},
		{"range_two", "{{range .Sl}}[{{.}}]{{else}}EMPTY{{end}}", EvalP{Sl: []int{7, 8}}, "[$1][$2]", []any{7, 8}},
		{"range_three_unchecked", "{{range .Sl}}[{{.}}]{{end}}", EvalP{Sl: []int{1, 2, 3}}, "[$1][$2][$3]", []any{1, 2, 3}},
		{"range_variable", "{{range $v := .Sl}}<{{$v}}>{{end}}", EvalP{Sl: []int{4}}, "<$1>", []any{4}},
		{"range_map_kv", "{{range $k, $v := .M}}{{$k}}={{$v}}{{end}}", EvalP{M: map[string]int{"only": 9}}, "$1=$2", []any{"only", 9}},
		{"range_nested_if", "{{range .Sl}}{{if .}}Y{{else}}N{{end}}{{end}}", EvalP{Sl: []int{0, 3}}, "NY", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stmt := sqlshape.Query[struct{}, EvalP](tc.tmpl)
			r, err := stmt.Render(tc.p)
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			if r.SQL != tc.wantSQL {
				t.Errorf("sql = %q, want %q", r.SQL, tc.wantSQL)
			}
			if len(tc.wantArgs) != len(r.Args) {
				t.Fatalf("args = %#v, want %#v", r.Args, tc.wantArgs)
			}
			for i, want := range tc.wantArgs {
				if fmt.Sprint(r.Args[i]) != fmt.Sprint(want) {
					t.Errorf("arg[%d] = %#v, want %#v", i, r.Args[i], want)
				}
			}
		})
	}
}

// TestRenderConditionVariable covers a declared template variable used as a
// condition argument (arg's *parse.VariableNode branch, resolved successfully).
func TestRenderConditionVariable(t *testing.T) {
	stmt := sqlshape.Query[struct{}, EvalP]("{{$v := .I}}{{if eq $v 5}}THEN{{else}}ELSE{{end}}")
	r, err := stmt.Render(EvalP{I: 5})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if r.SQL != "THEN" {
		t.Errorf("sql = %q, want THEN", r.SQL)
	}
}
