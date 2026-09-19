package dialect

import (
	"reflect"
	"testing"
)

func TestSettings(t *testing.T) {
	cases := []struct {
		sql  string
		want []Setting
		err  string
	}{
		{"-- sqlshape: mysql 8.4\n-- sqlshape: server sql_mode = 'ANSI_QUOTES,STRICT_ALL_TABLES'\n-- sqlshape: server lower_case_table_names = 1\nCREATE TABLE t (a int);",
			[]Setting{{"sql_mode", "ANSI_QUOTES,STRICT_ALL_TABLES", 23}, {"lower_case_table_names", "1", 86}}, ""},
		{"  --sqlshape:server   SQL_MODE='' \n", []Setting{{"sql_mode", "", 0}}, ""},
		{"-- sqlshape: server x = 'it''s'\n", []Setting{{"x", "it's", 0}}, ""},
		{"-- sqlshape: server sql_mode\n", nil, "want `<variable> = <value>`"},
		{"-- sqlshape: server sql_mode =\n", nil, "want a value after `=`"},
		{"-- sqlshape: server sql_mode = 'ANSI\n", nil, "not closed"},
		{"-- sqlshape: server sql_mode = 'a'b'\n", nil, "written ''"},
		{"-- sqlshape: server sql_mode = ANSI QUOTES\n", nil, "string literal"},
		{"-- sqlshape: server 1x = 2\n", nil, "not a variable name"},
		{"-- sqlshape: server a = 1\n-- sqlshape: server A = 2\n", nil, "declared twice"},
		{"-- sqlshape: servers = 1\n-- sqlshape: not null a\n", nil, ""},
	}
	for _, c := range cases {
		got, err := Settings(c.sql)
		if c.err != "" {
			if err == nil || !contains(err.Error(), c.err) {
				t.Errorf("%q: err = %v, want %q", c.sql, err, c.err)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: %v", c.sql, err)
			continue
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%q:\n got %+v\nwant %+v", c.sql, got, c.want)
		}
	}
	if !IsSetting("server sql_mode = ''") || IsSetting("servers") || IsSetting("require a") {
		t.Error("IsSetting")
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && (s == sub || indexOf(s, sub) >= 0))
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
