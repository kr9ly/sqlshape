package analyze

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kr9ly/sqlshape/check/mysql/v2/internal/oracle"
	"github.com/kr9ly/sqlshape/check/mysql/v2/internal/schema"
	"github.com/kr9ly/sqlshape/mysqltest/v2"
)

// adv2_null_test.go: adversarial findings for the "nulltype" lane (result column
// nullability/types, second round): each test was confirmed to disagree with a real
// mysqld (nix-shell -p mysql84) before being written down, and is written as the behavior
// sqlshape should have, so it currently fails. Round 1's adv_null_test.go findings are not
// retested here.

const adv2NullSchema = `-- sqlshape: mysql 8.4
CREATE TABLE a (
  id INT NOT NULL PRIMARY KEY,
  n INT NULL
);
`

func adv2NullLoad(t *testing.T) *schema.Schema {
	t.Helper()
	s, err := schema.Load(adv2NullSchema)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Problems) > 0 {
		t.Fatalf("schema problems: %v", s.Problems)
	}
	return s
}

func startAdv2NullOracle(t *testing.T) (context.Context, *oracle.Oracle) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	o, err := oracle.Start(ctx, adv2NullSchema)
	if errors.Is(err, oracle.ErrNoServer) {
		t.Skip("no mysqld on PATH (nix-shell -p mysql84)")
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { o.Close() })
	return ctx, o
}

// TestAdv2NthValueWindowFunctionParses: NTH_VALUE(expr, n) OVER (...) is a real MySQL 8.4
// window function (added in 8.0), grammatically registered in the reader
// (mysqlparse/shapes.go: "NTH_VALUE_SYM '(' expr ',' simple_expr ')' ..." with Kind:
// ActUnknown, meaning the reader treats it as an unmodelled construct rather than building
// a node the analyzer can walk). A real server accepts and types it (confirmed against
// mysqld 8.4: BIGINT, nullable), but sqlshape rejects the whole statement with "unsupported
// construct" -- spuriously flagging valid application SQL, the same failure mode as round
// 1's JSON `->` finding (mysqlparse/shapes.go:2479).
//
//	repro: SELECT NTH_VALUE(id, 2) OVER (ORDER BY id) FROM a
//	server: accepts it, types the column BIGINT, nullable
//	sqlshape: rejects it (mysqlast: unsupported construct window_func_call/10)
func TestAdv2NthValueWindowFunctionParses(t *testing.T) {
	ctx, o := startAdv2NullOracle(t)
	s := adv2NullLoad(t)
	const sql = "SELECT NTH_VALUE(id, 2) OVER (ORDER BY id) FROM a"

	if _, err := o.Describe(ctx, sql); err != nil {
		t.Fatalf("test setup: server rejects %s: %v", sql, err)
	}
	if _, err := Analyze(s, sql); err != nil {
		t.Errorf("%s: analyzer rejects what the server accepts: %v", sql, err)
	}
}

// TestAdv2LeadLagFirstLastValueUntyped: LAG / LEAD / FIRST_VALUE / LAST_VALUE are ordinary
// MySQL 8.4 window functions the reader does build a node for (Item_lead_lag,
// Item_first_last_value: hooks_dml.go / views.go), and a real server always types them
// (confirmed against mysqld 8.4: BIGINT for LAG/LEAD over an INT argument, nullable unless
// a non-NULL default value is given). Item_lead_lag has no entry in
// catalog.Items (check/mysql/internal/catalog/functions.go), so node()'s generic dispatch
// (expr.go's "any other Item class" branch) finds no catalog entry and leaves the column
// untyped; Item_first_last_value does have an entry (Facts: set_nullable(true),
// set_data_type_from_item(args[0])) but classType (expr.go) has no case that reads
// set_data_type_from_item to type it from its own argument, so it too stays untyped. A
// column following the previous/next/first/last row of an ORDER BY -- a common pattern for
// deltas and running comparisons -- is one of the few remaining window-function families
// sqlshape cannot type at all, unlike ROW_NUMBER/RANK/NTILE/SUM/AVG OVER, which it already
// gets right.
//
//	repro: SELECT LAG(id) OVER (ORDER BY id), LEAD(id) OVER (ORDER BY id),
//	       FIRST_VALUE(id) OVER (ORDER BY id), LAST_VALUE(id) OVER (ORDER BY id) FROM a
//	server: every column BIGINT (LAG/LEAD) or BIGINT (FIRST_VALUE/LAST_VALUE over an INT), nullable
//	sqlshape: every column left untyped (Known=false)
func TestAdv2LeadLagFirstLastValueUntyped(t *testing.T) {
	ctx, o := startAdv2NullOracle(t)
	s := adv2NullLoad(t)
	sqls := []string{
		"SELECT LAG(id) OVER (ORDER BY id) FROM a",
		"SELECT LEAD(id) OVER (ORDER BY id) FROM a",
		"SELECT FIRST_VALUE(id) OVER (ORDER BY id) FROM a",
		"SELECT LAST_VALUE(id) OVER (ORDER BY id) FROM a",
	}
	for _, sql := range sqls {
		d, err := o.Describe(ctx, sql)
		if err != nil {
			t.Fatalf("test setup: server rejects %s: %v", sql, err)
		}
		if d.Columns[0].Type == "" {
			t.Fatalf("test setup: expected the server itself to type the column")
		}
		r, err := Analyze(s, sql)
		if err != nil {
			t.Fatalf("analyzer rejects %s: %v", sql, err)
		}
		if !r.Columns[0].Known {
			t.Errorf("%s: analyzer leaves the column untyped, the server says %s", sql, d.Columns[0].Type)
		}
	}
}

const adv2TriggerAutoIncSchema = `-- sqlshape: mysql 8.4
CREATE TABLE orders (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  total DECIMAL(10,2) NOT NULL
);
CREATE TABLE audit_log (
  order_id BIGINT UNSIGNED NOT NULL
);
CREATE TRIGGER orders_bi BEFORE INSERT ON orders FOR EACH ROW
BEGIN
  INSERT INTO audit_log (order_id) VALUES (NEW.id);
END;
`

// TestAdv2TriggerAutoIncrementNewColNotActuallyNullable: body.go's trigRowColumn makes
// every NEW.col read nullable in a BEFORE INSERT/UPDATE trigger, unconditionally
// (body.go:282-284: "nullable := !col.NotNull; if row == "NEW" && timing == "BEFORE" {
// nullable = true }"), citing that a NOT NULL column's NEW.col may still be NULL there
// (true, and correctly measured, when the column has no default and no AUTO_INCREMENT).
// But when the column is AUTO_INCREMENT, MySQL has already substituted the placeholder 0
// before the BEFORE INSERT trigger body runs (omitted or written as NULL alike), so NEW.id
// is never actually NULL there. (A non-NULL DEFAULT is different: an explicit NULL reaches
// the body as NULL and the body's own write of it into a NOT NULL column is 1048, measured
// -- so a defaulted column's NEW.col stays nullable.) Confirmed against mysqld 8.4: `INSERT INTO orders (total) VALUES (5.00)`, which
// omits the AUTO_INCREMENT id and therefore fires the trigger with NEW.id = 0, succeeds
// without error, including the trigger's own `INSERT INTO audit_log (order_id) VALUES
// (NEW.id)` into a NOT NULL column with no default. sqlshape nonetheless predicts a 1048
// (NOT NULL) violation on audit_log.order_id for the outer INSERT INTO orders statement --
// a false positive that would make a passing statement look unsafe to a user relying on
// sqlshape's violation predictions.
//
//	repro: INSERT INTO orders (total) VALUES (5.00)   (id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
//	       BEFORE INSERT trigger does INSERT INTO audit_log (order_id) VALUES (NEW.id), order_id NOT NULL)
//	server: the statement succeeds (NEW.id is 0, never NULL, for an omitted AUTO_INCREMENT column)
//	sqlshape: predicts a 1048 NOT NULL violation on audit_log.order_id
func TestAdv2TriggerAutoIncrementNewColNotActuallyNullable(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	db, err := mysqltest.Start(ctx, adv2TriggerAutoIncSchema)
	if errors.Is(err, mysqltest.ErrNoServer) {
		t.Skip("no mysqld on PATH (nix-shell -p mysql84)")
	}
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	const sql = "INSERT INTO orders (total) VALUES (5.00)"
	if _, err := db.Conn().ExecContext(ctx, sql); err != nil {
		t.Fatalf("test setup: server rejects %s: %v", sql, err)
	}

	s, err := schema.Load(adv2TriggerAutoIncSchema)
	if err != nil {
		t.Fatal(err)
	}
	r, err := Analyze(s, sql)
	if err != nil {
		t.Fatalf("analyzer rejects %s: %v", sql, err)
	}
	for _, v := range r.Violations {
		if v.Code == 1048 && v.Table == "audit_log" {
			t.Errorf("%s: analyzer predicts a NOT NULL violation on audit_log.%v, but the server runs the statement without error (NEW.id was the AUTO_INCREMENT placeholder, never NULL): %+v", sql, v.Columns, v)
		}
	}
}
