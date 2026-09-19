package schema

import (
	"strings"
	"testing"
)

const routinesSample = `-- sqlshape: mysql 8.4
CREATE TABLE customers (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  balance DECIMAL(12,2) NOT NULL DEFAULT 0
);

CREATE TABLE orders (
  id INT NOT NULL AUTO_INCREMENT PRIMARY KEY,
  customer_id BIGINT UNSIGNED NOT NULL,
  amount DECIMAL(12,2) NOT NULL
);

CREATE TABLE audit (
  id INT NOT NULL AUTO_INCREMENT PRIMARY KEY,
  what VARCHAR(50)
);

-- sqlshape: error 30001 = OrderTooLarge
CREATE DEFINER=root@localhost TRIGGER trg_orders_check BEFORE INSERT ON orders FOR EACH ROW
BEGIN
  IF NEW.amount > 1000 THEN
    SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'too large', MYSQL_ERRNO = 30001;
  END IF;
  INSERT INTO audit (what) VALUES ('order');
END;

CREATE TRIGGER trg_orders_after AFTER INSERT ON orders FOR EACH ROW FOLLOWS trg_orders_check
BEGIN
  INSERT INTO audit (what) VALUES ('after');
END;

CREATE PROCEDURE add_order(IN cust BIGINT UNSIGNED, IN amt DECIMAL(12,2), OUT new_id INT)
COMMENT 'adds an order'
BEGIN
  INSERT INTO orders (customer_id, amount) VALUES (cust, amt);
  SELECT LAST_INSERT_ID() INTO new_id;
END;

CREATE FUNCTION total_for_customer(cust BIGINT UNSIGNED) RETURNS DECIMAL(12,2)
DETERMINISTIC
READS SQL DATA
BEGIN
  DECLARE total DECIMAL(12,2);
  SELECT SUM(amount) INTO total FROM orders WHERE customer_id = cust;
  RETURN total;
END;

ALTER PROCEDURE add_order COMMENT 'adds an order, updated' SQL SECURITY INVOKER;
ALTER FUNCTION total_for_customer COMMENT 'totals';
DROP TRIGGER trg_orders_after;
DROP PROCEDURE IF EXISTS no_such_proc;
DROP FUNCTION IF EXISTS no_such_func;
`

func TestLoadTriggersAndRoutines(t *testing.T) {
	s, err := Load(routinesSample)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range s.Problems {
		t.Errorf("problem: %s", p)
	}

	if len(s.Triggers) != 1 {
		t.Fatalf("triggers: got %d, want 1 (trg_orders_after dropped): %v", len(s.Triggers), s.Triggers)
	}
	trg := s.Trigger("trg_orders_check")
	if trg == nil {
		t.Fatal("no trg_orders_check")
	}
	if trg.Table != "orders" || trg.Timing != "BEFORE" || trg.Event != "INSERT" {
		t.Errorf("trigger fields: %+v", trg)
	}
	if trg.OrderClause != "" || trg.OrderTrigger != "" {
		t.Errorf("trigger order: %+v", trg)
	}
	if len(trg.Directives) != 1 || !strings.Contains(trg.Directives[0], "error 30001") {
		t.Errorf("trigger directives: %v", trg.Directives)
	}
	if trg.Body == nil {
		t.Error("trigger body nil")
	}
	if trg.Definition == "" || !strings.Contains(trg.Definition, "SIGNAL") {
		t.Errorf("trigger definition: %q", trg.Definition)
	}

	if s.Trigger("trg_orders_after") != nil {
		t.Error("trg_orders_after should have been dropped by DROP TRIGGER")
	}

	proc := s.Routine("add_order")
	if proc == nil {
		t.Fatal("no add_order")
	}
	if proc.Kind != Procedure {
		t.Errorf("add_order kind: %v", proc.Kind)
	}
	if len(proc.Params) != 3 {
		t.Fatalf("add_order params: %+v", proc.Params)
	}
	if proc.Params[0].Mode != "IN" || proc.Params[0].Name != "cust" || proc.Params[0].Type.Name != "bigint" || !proc.Params[0].Type.Unsigned {
		t.Errorf("add_order param 0: %+v", proc.Params[0])
	}
	if proc.Params[2].Mode != "OUT" || proc.Params[2].Name != "new_id" {
		t.Errorf("add_order param 2: %+v", proc.Params[2])
	}

	fn := s.Routine("total_for_customer")
	if fn == nil {
		t.Fatal("no total_for_customer")
	}
	if fn.Kind != Function {
		t.Errorf("total_for_customer kind: %v", fn.Kind)
	}
	if !fn.Deterministic {
		t.Error("total_for_customer should be DETERMINISTIC")
	}
	if fn.DataAccess != "READS_SQL_DATA" {
		t.Errorf("total_for_customer data access: %q", fn.DataAccess)
	}
	if fn.Returns.Name != "decimal" {
		t.Errorf("total_for_customer returns: %+v", fn.Returns)
	}
	if fn.Security != "DEFINER" { // ALTER FUNCTION did not change it (no schema effect)
		t.Errorf("total_for_customer security: %q", fn.Security)
	}

	if proc.Security != "DEFINER" {
		// ALTER PROCEDURE has no schema effect on Security either: characteristics are not
		// modeled as changed by ALTER. Kept at the CREATE-time default.
		t.Errorf("add_order security: %q (ALTER has no schema effect, want DEFINER)", proc.Security)
	}
}

func TestTriggerDropWithTable(t *testing.T) {
	sql := `-- sqlshape: mysql 8.4
CREATE TABLE t (id INT PRIMARY KEY);
CREATE TRIGGER trg BEFORE INSERT ON t FOR EACH ROW SET @x = 1;
DROP TABLE t;
`
	s, err := Load(sql)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range s.Problems {
		t.Errorf("problem: %s", p)
	}
	if len(s.Triggers) != 0 {
		t.Errorf("DROP TABLE should drop its triggers, got %v", s.Triggers)
	}
}

func TestTriggerRenameWithTable(t *testing.T) {
	sql := `-- sqlshape: mysql 8.4
CREATE TABLE t (id INT PRIMARY KEY);
CREATE TRIGGER trg BEFORE INSERT ON t FOR EACH ROW SET @x = 1;
RENAME TABLE t TO t2;
`
	s, err := Load(sql)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range s.Problems {
		t.Errorf("problem: %s", p)
	}
	trg := s.Trigger("trg")
	if trg == nil || trg.Table != "t2" {
		t.Errorf("trigger should follow the rename: %+v", trg)
	}
}

func TestCreateTriggerIfNotExistsAndDropIfExists(t *testing.T) {
	sql := `-- sqlshape: mysql 8.4
CREATE TABLE t (id INT PRIMARY KEY);
CREATE TRIGGER IF NOT EXISTS trg BEFORE INSERT ON t FOR EACH ROW SET @x = 1;
CREATE TRIGGER IF NOT EXISTS trg BEFORE INSERT ON t FOR EACH ROW SET @x = 2;
DROP TRIGGER IF EXISTS no_such;
DROP PROCEDURE IF EXISTS no_such_proc;
`
	s, err := Load(sql)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range s.Problems {
		t.Errorf("problem: %s", p)
	}
	if len(s.Triggers) != 1 {
		t.Errorf("triggers: %v", s.Triggers)
	}
}

func TestRoutineNamespacesSeparate(t *testing.T) {
	sql := `-- sqlshape: mysql 8.4
CREATE PROCEDURE p() BEGIN SELECT 1; END;
CREATE FUNCTION p() RETURNS INT BEGIN RETURN 1; END;
DROP PROCEDURE p;
`
	s, err := Load(sql)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range s.Problems {
		t.Errorf("problem: %s", p)
	}
	if len(s.Routines) != 1 {
		t.Fatalf("routines: %v", s.Routines)
	}
	if s.Routines[0].Kind != Function {
		t.Errorf("remaining routine should be the function: %+v", s.Routines[0])
	}
}

// TestFunctionNotNullDirective: `-- sqlshape: not null` above a CREATE FUNCTION is read,
// the same override postgres/schema.go's createFunction accepts, and sets Routine.NotNull
// rather than being rejected as an unknown directive.
func TestFunctionNotNullDirective(t *testing.T) {
	sql := `-- sqlshape: mysql 8.4
-- sqlshape: not null
CREATE FUNCTION f() RETURNS INT BEGIN RETURN 1; END;
`
	s, err := Load(sql)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range s.Problems {
		t.Errorf("problem: %s", p)
	}
	if len(s.Routines) != 1 || !s.Routines[0].NotNull {
		t.Fatalf("routines: %+v", s.Routines)
	}
}

// TestNotNullDirectiveOnProcedureIsProblem: `not null` names a function's own result, so a
// PROCEDURE (which returns nothing) rejects it as an unknown directive, same as any other
// directive it does not recognize.
func TestNotNullDirectiveOnProcedureIsProblem(t *testing.T) {
	sql := `-- sqlshape: mysql 8.4
-- sqlshape: not null
CREATE PROCEDURE p() BEGIN SELECT 1; END;
`
	s, err := Load(sql)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Problems) != 1 {
		t.Errorf("want 1 problem for not null on a procedure, got %v", s.Problems)
	}
}

// TestNotNullDirectiveOnTriggerIsProblem: a trigger has no result at all, so `not null`
// above one is an unknown directive too.
func TestNotNullDirectiveOnTriggerIsProblem(t *testing.T) {
	sql := `-- sqlshape: mysql 8.4
CREATE TABLE t (id INT PRIMARY KEY);
-- sqlshape: not null
CREATE TRIGGER trg BEFORE INSERT ON t FOR EACH ROW SET @x = 1;
`
	s, err := Load(sql)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Problems) != 1 {
		t.Errorf("want 1 problem for not null on a trigger, got %v", s.Problems)
	}
}

func TestUnknownDirectiveOnTriggerIsProblem(t *testing.T) {
	sql := `-- sqlshape: mysql 8.4
CREATE TABLE t (id INT PRIMARY KEY);
-- sqlshape: bogus
CREATE TRIGGER trg BEFORE INSERT ON t FOR EACH ROW SET @x = 1;
`
	s, err := Load(sql)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Problems) != 1 {
		t.Errorf("want 1 problem for the unknown directive, got %v", s.Problems)
	}
}

func TestTriggerFollowsMissingAnchorIsProblem(t *testing.T) {
	sql := `-- sqlshape: mysql 8.4
CREATE TABLE t (id INT PRIMARY KEY);
CREATE TRIGGER trg2 BEFORE INSERT ON t FOR EACH ROW FOLLOWS ghost SET @x = 1;
`
	s, err := Load(sql)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Problems) != 1 {
		t.Errorf("want 1 problem for the missing FOLLOWS anchor, got %v", s.Problems)
	}
	if len(s.Triggers) != 0 {
		t.Errorf("trigger should not be added: %v", s.Triggers)
	}
}
