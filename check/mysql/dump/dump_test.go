package dump

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kr9ly/sqlshape/mysqltest/v2"
)

func example(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("../../../examples/5-mysql/schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Both canonicalizers give the same text, the loader reads it clean, and canonicalizing
// the canonical text again is a fixpoint. Load of a live database reads the same thing.
func TestCanonical(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	schemaSQL := example(t)
	s1, text1, err := Local{}.Canonical(ctx, schemaSQL)
	if errors.Is(err, ErrNoServer) {
		t.Skip("no mysqld on PATH (nix-shell -p mysql84)")
	}
	if err != nil {
		t.Fatal(err)
	}
	if len(s1.Problems) > 0 {
		t.Fatalf("problems: %v", s1.Problems)
	}
	if !strings.HasPrefix(text1, "-- sqlshape: mysql 8.4\nSET FOREIGN_KEY_CHECKS=0;\nCREATE TABLE `customers`") {
		t.Errorf("text:\n%s", text1)
	}
	if strings.Contains(text1, "AUTO_INCREMENT=") || strings.Contains(text1, "DEFINER=") {
		t.Errorf("counters or definers left in:\n%s", text1)
	}
	for _, c := range s1.Tables[0].Columns {
		if c.Text == "" {
			t.Errorf("column %s has no text", c.Name)
		}
	}
	// fixpoint
	_, text2, err := Local{}.Canonical(ctx, text1)
	if err != nil {
		t.Fatal(err)
	}
	if text2 != text1 {
		t.Errorf("not a fixpoint:\n%s\n---\n%s", text1, text2)
	}
	// a live server: Load and Scratch on it
	srv, err := mysqltest.Start(ctx, schemaSQL)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	_, text3, err := Load(ctx, srv.DSN(), Header(schemaSQL))
	if err != nil {
		t.Fatal(err)
	}
	if text3 != text1 {
		t.Errorf("Load differs:\n%s\n---\n%s", text1, text3)
	}
	sc, err := NewScratch(srv.DSN())
	if err != nil {
		t.Fatal(err)
	}
	_, text4, err := sc.Canonical(ctx, schemaSQL)
	if err != nil {
		t.Fatal(err)
	}
	if text4 != text1 {
		t.Errorf("Scratch differs:\n%s\n---\n%s", text1, text4)
	}
	var n int
	if err := srv.Conn().QueryRowContext(ctx, "SELECT COUNT(*) FROM information_schema.SCHEMATA WHERE SCHEMA_NAME LIKE 'sqlshape_scratch_%'").Scan(&n); err != nil || n != 0 {
		t.Errorf("scratch databases left: %d %v", n, err)
	}
}

func TestHeader(t *testing.T) {
	got := Header("-- sqlshape: mysql 8.4\n-- sqlshape: server sql_mode = 'ANSI'\n-- sqlshape: server lower_case_table_names = 1\nCREATE TABLE t (a INT);")
	want := "-- sqlshape: mysql 8.4\n-- sqlshape: server sql_mode = 'ANSI'\n-- sqlshape: server lower_case_table_names = '1'\n"
	if got != want {
		t.Errorf("got %q", got)
	}
	if Header("CREATE TABLE t (a INT);") != "-- sqlshape: mysql 8.4\n" {
		t.Error("default header")
	}
}

func TestViewOrder(t *testing.T) {
	defs := map[string]string{"a": "select * from `b`", "b": "select * from `c` join `t`", "c": "select 1"}
	got := viewOrder([]string{"a", "b", "c"}, defs)
	if strings.Join(got, ",") != "c,b,a" {
		t.Errorf("%v", got)
	}
}
