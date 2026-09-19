package analyze

import (
	"errors"
	"testing"
)

// The checker's side of TestSysVarScopeServer / TestSysVarPlacementServer /
// TestFunctionAliasServer: the same rules without a server.
func TestSysVarScope(t *testing.T) {
	s := load(t)
	cases := []struct {
		sql  string
		want int
		msg  string
	}{
		{"SELECT @@session.thread_stack", 1238, "Variable 'thread_stack' is a GLOBAL variable"},
		{"SELECT @@LOCAL.Thread_Stack", 1238, "Variable 'thread_stack' is a GLOBAL variable"},
		{"SELECT @@global.timestamp", 1238, "Variable 'timestamp' is a SESSION variable"},
		{"SELECT COUNT(@@session.port)", 1238, "Variable 'port' is a GLOBAL variable"},
		{"SELECT IF(1, 1, @@session.port)", 1238, "Variable 'port' is a GLOBAL variable"},
		{"SELECT (SELECT @@session.port)", 1238, "Variable 'port' is a GLOBAL variable"},
		{"SELECT 1 FROM users WHERE FALSE AND @@session.port", 1238, "Variable 'port' is a GLOBAL variable"},
		{"SELECT @@thread_stack", 0, ""},                          // unqualified: never a scope error
		{"SELECT @@timestamp", 0, ""},                             // unqualified session-only
		{"SELECT @@session.sql_mode", 0, ""},                      // both scopes
		{"SELECT @@global.sql_mode", 0, ""},                       // both scopes
		{"SELECT @@session.nosuchvar", 0, ""},                     // unknown: a plugin's business (server: 1193)
		{"SELECT @@global.dragnet.log_error_filter_rules", 0, ""}, // a component's variable
	}
	for _, c := range cases {
		_, err := Analyze(s, c.sql)
		if c.want == 0 {
			if err != nil {
				t.Errorf("%s: %v, want ok", c.sql, err)
			}
			continue
		}
		var e *Error
		if !errors.As(err, &e) || e.Code != c.want || e.Message != c.msg {
			t.Errorf("%s: %v, want %d %q", c.sql, err, c.want, c.msg)
		}
	}
}

func TestFunctionAlias(t *testing.T) {
	s := load(t)
	cases := []struct {
		sql  string
		want int
		msg  string
	}{
		{"SELECT ABS(3 AS three)", 1583, "Incorrect parameters in the call to native function 'abs'"},
		{"SELECT instr('foobar', 'bar' p2)", 1583, "Incorrect parameters in the call to native function 'instr'"},
		{"SELECT abs(nosuchcol AS x) FROM users", 1583, "Incorrect parameters in the call to native function 'abs'"},
		{"INSERT INTO metrics (id, small, tiny, y) VALUES (abs(1 AS x), 1, 1, 2024)", 1583, "Incorrect parameters in the call to native function 'abs'"},
		{"SELECT Abs(1 AS x, 2)", 1582, "Incorrect parameter count in the call to native function 'Abs'"},
		{"SELECT Internal_Table_Rows()", 3566, "Access to native function 'Internal_Table_Rows' is rejected."},
		{"SELECT nosuchfn(1 AS x)", 1584, "Incorrect parameters in the call to stored function `nosuchfn`"},
		{"SELECT abs(1)", 0, ""},
	}
	for _, c := range cases {
		_, err := Analyze(s, c.sql)
		if c.want == 0 {
			if err != nil {
				t.Errorf("%s: %v, want ok", c.sql, err)
			}
			continue
		}
		var e *Error
		if !errors.As(err, &e) || e.Code != c.want || e.Message != c.msg {
			t.Errorf("%s: %v, want %d %q", c.sql, err, c.want, c.msg)
		}
	}
}
