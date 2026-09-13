package dump

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kr9ly/sqlshape/check/mysql/v2/internal/schema"
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
	if s1.Trigger("orders_before_insert") == nil {
		t.Error("trigger orders_before_insert not read back")
	}
	if s1.RoutineOf(schema.Function, "customer_order_total") == nil {
		t.Error("function customer_order_total not read back")
	}
	if !strings.Contains(text1, "CREATE TRIGGER `orders_before_insert`") {
		t.Errorf("trigger text missing:\n%s", text1)
	}
	if !strings.Contains(text1, "CREATE FUNCTION `customer_order_total`") {
		t.Errorf("function text missing:\n%s", text1)
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

// Two triggers on the same table, action time and event: SHOW CREATE TRIGGER's own text
// never carries FOLLOWS (measured against mysqld: it reads as if each trigger stood alone),
// so Read must add it itself from information_schema.TRIGGERS' ACTION_ORDER for the second
// trigger to run after the first on a reload, and canonicalizing that text again must keep
// the same order (a fixpoint).
func TestTriggerFollowsOrder(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	schemaSQL := `-- sqlshape: mysql 8.4
CREATE TABLE t (id INT PRIMARY KEY, a INT, b INT);
CREATE TRIGGER t_first BEFORE INSERT ON t FOR EACH ROW SET NEW.a = 1;
CREATE TRIGGER t_second BEFORE INSERT ON t FOR EACH ROW FOLLOWS t_first SET NEW.b = 2;
`
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
	second := s1.Trigger("t_second")
	if second == nil {
		t.Fatal("t_second not read back")
	}
	if second.OrderClause != "FOLLOWS" || second.OrderTrigger != "t_first" {
		t.Errorf("t_second: OrderClause=%q OrderTrigger=%q", second.OrderClause, second.OrderTrigger)
	}
	if !strings.Contains(text1, "FOLLOWS `t_first`") {
		t.Errorf("no FOLLOWS clause in dumped text:\n%s", text1)
	}
	_, text2, err := Local{}.Canonical(ctx, text1)
	if err != nil {
		t.Fatal(err)
	}
	if text2 != text1 {
		t.Errorf("not a fixpoint:\n%s\n---\n%s", text1, text2)
	}
}

func TestViewOrder(t *testing.T) {
	defs := map[string]string{"a": "select * from `b`", "b": "select * from `c` join `t`", "c": "select 1"}
	got := viewOrder([]string{"a", "b", "c"}, defs)
	if strings.Join(got, ",") != "c,b,a" {
		t.Errorf("%v", got)
	}
}

// TestReadRoutinesAndTriggers_QueryError: a context already cancelled before the call makes
// the listing query itself fail immediately (readRoutines' and readTriggers' own first
// QueryContext error, as opposed to every other test here, which only exercises the success
// path through Read/Canonical).
func TestReadRoutinesAndTriggers_QueryError(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	srv, err := mysqltest.Start(ctx, "-- sqlshape: mysql 8.4\nCREATE TABLE t (id INT PRIMARY KEY);\n")
	if errors.Is(err, ErrNoServer) {
		t.Skip("no mysqld on PATH (nix-shell -p mysql84)")
	}
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	cancelled, cancelNow := context.WithCancel(ctx)
	cancelNow()

	var b strings.Builder
	if err := readRoutines(cancelled, srv.Conn(), &b); err == nil {
		t.Error("readRoutines(cancelled context) = nil error, want one")
	}
	if err := readTriggers(cancelled, srv.Conn(), &b); err == nil {
		t.Error("readTriggers(cancelled context) = nil error, want one")
	}
}
