package analyze

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kr9ly/sqlshape/check/mysql/v2/internal/oracle"
	"github.com/kr9ly/sqlshape/check/mysql/v2/internal/schema"
)

// adv_null_test.go: adversarial findings for the "null" lane (result column types and
// nullability). Each test was confirmed to disagree with a real mysqld (nix-shell -p
// mysql84) before being written down; it is written as the behavior sqlshape should have,
// so it currently fails.

const advNullSchema = `-- sqlshape: mysql 8.4
CREATE TABLE a (
  id INT NOT NULL PRIMARY KEY,
  n INT NULL,
  s VARCHAR(20) NULL,
  bt BIT(4) NULL,
  j JSON NULL,
  dt DATETIME NULL
);
`

func advNullLoad(t *testing.T) *schema.Schema {
	t.Helper()
	s, err := schema.Load(advNullSchema)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Problems) > 0 {
		t.Fatalf("schema problems: %v", s.Problems)
	}
	return s
}

// startOracle skips the test when no mysqld is on PATH.
func startAdvNullOracle(t *testing.T) (context.Context, *oracle.Oracle) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	o, err := oracle.Start(ctx, advNullSchema)
	if errors.Is(err, oracle.ErrNoServer) {
		t.Skip("no mysqld on PATH (nix-shell -p mysql84)")
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { o.Close() })
	return ctx, o
}

// TestAdvNullBitArithmeticIsUnsigned: BIT is an unsigned type in arithmetic context
// (Item_num_op::set_numeric_type on a BIT operand). A real server types `bt + 0` as
// UNSIGNED BIGINT (confirmed against mysqld 8.4), but the analyzer types it as a signed
// bigint because schema.Load never sets Type.Unsigned for a BIT column, and numOp/num1
// then compose from an operand that looks signed.
//
//	repro: SELECT bt + 0 FROM a               (bt BIT(4) NULL)
//	server: bt + 0 -> BIGINT UNSIGNED, nullable
//	sqlshape: bt + 0 -> bigint (signed), nullable
func TestAdvNullBitArithmeticIsUnsigned(t *testing.T) {
	ctx, o := startAdvNullOracle(t)
	s := advNullLoad(t)
	const sql = "SELECT bt + 0 FROM a"

	d, err := o.Describe(ctx, sql)
	if err != nil {
		t.Fatalf("server rejects %s: %v", sql, err)
	}
	if d.Columns[0].Type != "UNSIGNED BIGINT" {
		t.Fatalf("test setup: expected the server itself to say UNSIGNED BIGINT, got %s", d.Columns[0].Type)
	}

	r, err := Analyze(s, sql)
	if err != nil {
		t.Fatalf("analyzer rejects %s: %v", sql, err)
	}
	col := r.Columns[0]
	if !col.Known {
		t.Fatalf("%s: analyzer leaves the column untyped", sql)
	}
	if !col.Type.Unsigned {
		t.Errorf("%s: analyzer says %s (signed), the server says BIGINT UNSIGNED", sql, col.Type)
	}
}

// TestAdvNullRollupMakesGroupColumnNullable: WITH ROLLUP adds a super-aggregate row per
// grouping level, where every rolled-up group column reads NULL -- including one declared
// NOT NULL in the base table. A real server therefore reports such a column as nullable in
// the result set metadata, but the analyzer keeps the base column's own nullability
// (confirmed against mysqld 8.4: `id` is the table's PRIMARY KEY, itself NOT NULL).
//
//	repro: SELECT a.id, COUNT(*) FROM a GROUP BY a.id WITH ROLLUP   (a.id INT NOT NULL PRIMARY KEY)
//	server: column "id" is nullable
//	sqlshape: column "id" is not nullable
func TestAdvNullRollupMakesGroupColumnNullable(t *testing.T) {
	ctx, o := startAdvNullOracle(t)
	s := advNullLoad(t)
	const sql = "SELECT a.id, COUNT(*) FROM a GROUP BY a.id WITH ROLLUP"

	d, err := o.Describe(ctx, sql)
	if err != nil {
		t.Fatalf("server rejects %s: %v", sql, err)
	}
	if !d.Columns[0].Nullable {
		t.Fatalf("test setup: expected the server itself to say nullable")
	}

	r, err := Analyze(s, sql)
	if err != nil {
		t.Fatalf("analyzer rejects %s: %v", sql, err)
	}
	if !r.Columns[0].Nullable {
		t.Errorf("%s: analyzer says column %q is not nullable, the server says it is (the ROLLUP super-aggregate row)", sql, r.Columns[0].Name)
	}
}

// TestAdvNullStrToDateFollowsLiteralFormatConstant: STR_TO_DATE's result type depends on
// the format string (Item_func_str_to_date::fix_from_format): a format with no time parts
// makes a DATE, one with no date parts makes a TIME, and only a format with both (or a
// non-constant format) makes a DATETIME. The analyzer's rule (expr.go, the
// Item_temporal_hybrid_func case) always answers DATETIME for STR_TO_DATE, without reading
// a literal format argument. Confirmed against mysqld 8.4: a pure date format yields DATE.
//
//	repro: SELECT STR_TO_DATE(a.s, '%Y-%m-%d') FROM a
//	server: DATE
//	sqlshape: datetime
func TestAdvNullStrToDateFollowsLiteralFormatConstant(t *testing.T) {
	ctx, o := startAdvNullOracle(t)
	s := advNullLoad(t)
	const sql = "SELECT STR_TO_DATE(a.s, '%Y-%m-%d') FROM a"

	d, err := o.Describe(ctx, sql)
	if err != nil {
		t.Fatalf("server rejects %s: %v", sql, err)
	}
	if d.Columns[0].Type != "DATE" {
		t.Fatalf("test setup: expected the server itself to say DATE, got %s", d.Columns[0].Type)
	}

	r, err := Analyze(s, sql)
	if err != nil {
		t.Fatalf("analyzer rejects %s: %v", sql, err)
	}
	col := r.Columns[0]
	if !col.Known {
		t.Fatalf("%s: analyzer leaves the column untyped", sql)
	}
	if col.Type.Name != "date" {
		t.Errorf("%s: analyzer says %s, the server says DATE (a format string with no time parts)", sql, col.Type)
	}
}

// TestAdvNullJsonArrowOperatorParses: `->` and `->>` are MySQL's shorthand for
// JSON_EXTRACT / JSON_UNQUOTE(JSON_EXTRACT(...)) on a column. A real server accepts them
// (confirmed against mysqld 8.4), but mysqlast's grammar does not implement simple_expr's
// `->` / `->>` production, so the analyzer rejects every statement that uses them --
// spuriously flagging valid application SQL.
//
//	repro: SELECT j -> '$.x' FROM a          (j JSON NULL)
//	server: accepts it, types the column JSON, nullable
//	sqlshape: rejects it (mysqlast: unsupported construct simple_expr/34)
func TestAdvNullJsonArrowOperatorParses(t *testing.T) {
	ctx, o := startAdvNullOracle(t)
	s := advNullLoad(t)

	for _, sql := range []string{
		"SELECT j -> '$.x' FROM a",
		"SELECT j ->> '$.x' FROM a",
	} {
		if _, err := o.Describe(ctx, sql); err != nil {
			t.Fatalf("test setup: server rejects %s: %v", sql, err)
		}
		if _, err := Analyze(s, sql); err != nil {
			t.Errorf("%s: analyzer rejects what the server accepts: %v", sql, err)
		}
	}
}
