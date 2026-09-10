package schema

import (
	"strings"
	"testing"
)

// A `-- sqlshape: server` line is read as a server setting, never as a statement's
// directive; PostgreSQL reads no variable yet, so each one is a problem of its own line.
func TestServerSettings(t *testing.T) {
	s, err := Load("-- sqlshape: postgres 17\n-- sqlshape: server search_path = 'app'\nCREATE TABLE t (a int);")
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Problems) != 1 || s.Problems[0].Location != 25 || !strings.Contains(s.Problems[0].Message, "server search_path: not a variable sqlshape reads for PostgreSQL") {
		t.Errorf("problems: %v", s.Problems)
	}
	if r := s.Relation("", "t"); r == nil || len(r.Directives) != 0 {
		t.Errorf("the setting became the table's directive: %+v", r)
	}
	if _, err := Load("-- sqlshape: postgres 17\n-- sqlshape: server search_path\n"); err == nil || !strings.Contains(err.Error(), "want `<variable> = <value>`") {
		t.Errorf("malformed: %v", err)
	}
}
