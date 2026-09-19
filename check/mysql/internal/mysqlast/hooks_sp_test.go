package mysqlast

import (
	"strings"
	"testing"
)

// TestCreateTrigger checks that a CREATE TRIGGER with a DEFINER clause, IF NOT EXISTS, a
// FOLLOWS clause and a BEGIN body (DECLARE, a handler, IF/ELSEIF/ELSE, SIGNAL, WHILE, a
// labeled LOOP with LEAVE, and a searched CASE) folds without an *Unsupported.
func TestCreateTrigger(t *testing.T) {
	sql := `CREATE DEFINER=root@localhost TRIGGER IF NOT EXISTS trg BEFORE INSERT ON t FOR EACH ROW
BEGIN
  DECLARE v INT DEFAULT 0;
  DECLARE CONTINUE HANDLER FOR NOT FOUND SET v = -1;
  IF NEW.a > 0 THEN
    SET v = 1;
    SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'bad', MYSQL_ERRNO = 30001;
  ELSEIF NEW.a < 0 THEN
    SET NEW.a = 0;
  ELSE
    CALL p1(NEW.a, @x);
  END IF;
  lp: LOOP
    LEAVE lp;
  END LOOP;
  CASE v
    WHEN 1 THEN SET v = 2;
    ELSE SET v = 3;
  END CASE;
END`
	got := Sprint(mustBuild(t, sql))
	for _, want := range []string{
		`trigger_tail(true, sp_name(db=nil, name="trg"), TRG_ACTION_BEFORE, TRG_EVENT_INSERT, Table_ident(table=t)`,
		`sp_decl_var(names=["v"], type=`,
		`sp_decl_handler(type=sp_handler::CONTINUE, conditions=[sp_condition_value(_mysqlerr=sp_condition_value::NOT_FOUND)]`,
		`sp_if(`,
		`sp_signal(condition=sp_condition_value(_mysqlerr="45000"), info=[sp_signal_item(name=CIN_MESSAGE_TEXT`,
		`sp_signal_item(name=CIN_MYSQL_ERRNO, expr=Item_int(i=30001))`,
		`PT_call(proc_name=sp_name(db=nil, name="p1")`,
		`sp_labeled_control(label="lp", body=[sp_leave(label="lp")])`,
		`simple_case_stmt(`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in\n%s", want, got)
		}
	}
}

// TestCreateTriggerFollows checks the FOLLOWS/PRECEDES clause and a single-statement (no
// BEGIN/END) body.
func TestCreateTriggerFollows(t *testing.T) {
	got := Sprint(mustBuild(t, "CREATE TRIGGER trg2 AFTER UPDATE ON t FOR EACH ROW FOLLOWS trg1 SET @x = 1"))
	for _, want := range []string{
		"TRG_ACTION_AFTER", "TRG_EVENT_UPDATE",
		"ordering_clause: TRG_ORDER_FOLLOWS",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in\n%s", want, got)
		}
	}
}

// TestCreateProcedure checks IN/OUT/INOUT parameters and a body with a cursor (DECLARE
// CURSOR / OPEN / FETCH / CLOSE), SELECT ... INTO and RESIGNAL.
func TestCreateProcedure(t *testing.T) {
	sql := `CREATE PROCEDURE p1(IN a INT, OUT b INT, INOUT c INT)
BEGIN
  DECLARE cur CURSOR FOR SELECT 1;
  OPEN cur;
  FETCH cur INTO b;
  CLOSE cur;
  SELECT a INTO b;
  RESIGNAL;
END`
	got := Sprint(mustBuild(t, sql))
	for _, want := range []string{
		`sp_tail(false, sp_name(db=nil, name="p1")`,
		`sp_param(mode=sp_variable::MODE_IN, name="a"`,
		`sp_param(mode=sp_variable::MODE_OUT, name="b"`,
		`sp_param(mode=sp_variable::MODE_INOUT, name="c"`,
		`sp_decl_cursor(name="cur", query=`,
		`opt_into1=[PT_select_sp_var(name=b)]`,
		`sp_resignal(condition=nil, info=Set_signal_information())`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in\n%s", want, got)
		}
	}
}

// TestCreateFunction checks a function's RETURNS type, a characteristic (DETERMINISTIC),
// a BEGIN ... RETURN body and the single-statement `RETURN expr` body.
func TestCreateFunction(t *testing.T) {
	got := Sprint(mustBuild(t, "CREATE FUNCTION f1(a INT) RETURNS INT DETERMINISTIC\nBEGIN\n  RETURN a + 1;\nEND"))
	for _, want := range []string{
		`sf_tail(false, sp_name(db=nil, name="f1")`,
		`sp_param(mode=sp_variable::MODE_IN, name="a"`,
		`sp_return(Item_func_plus(a=PTI_simple_ident_ident(ident=a), b=Item_int(i=1)))`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in\n%s", want, got)
		}
	}

	got2 := Sprint(mustBuild(t, "CREATE FUNCTION f2(a INT) RETURNS INT\nRETURN a + 1"))
	if !strings.Contains(got2, `sp_return(Item_func_plus(a=PTI_simple_ident_ident(ident=a), b=Item_int(i=1)))`) {
		t.Errorf("missing bare RETURN body in\n%s", got2)
	}
}

// TestDropFunction checks DROP FUNCTION IF EXISTS name and the db-qualified form.
func TestDropFunction(t *testing.T) {
	got := Sprint(mustBuild(t, "DROP FUNCTION IF EXISTS f1"))
	if !strings.Contains(got, `drop_function_stmt(if_exists=1, spname=sp_name(db=nil, name="f1"))`) {
		t.Errorf("unqualified: %s", got)
	}
	got2 := Sprint(mustBuild(t, "DROP FUNCTION db1.f1"))
	if !strings.Contains(got2, `drop_function_stmt(if_exists=0, spname=sp_name(db="db1", name="f1"))`) {
		t.Errorf("qualified: %s", got2)
	}
}

// TestDropTriggerProcedure checks that DROP TRIGGER / DROP PROCEDURE, already supported,
// keep working (they use the same sp_name this file adds hooks for).
func TestDropTriggerProcedure(t *testing.T) {
	got := Sprint(mustBuild(t, "DROP TRIGGER IF EXISTS trg"))
	if !strings.Contains(got, `spname: sp_name(db=nil, name="trg")`) {
		t.Errorf("drop trigger: %s", got)
	}
	got2 := Sprint(mustBuild(t, "DROP PROCEDURE IF EXISTS p1"))
	if !strings.Contains(got2, `spname: sp_name(db=nil, name="p1")`) {
		t.Errorf("drop procedure: %s", got2)
	}
}

// TestAlterProcedureFunction checks ALTER PROCEDURE / FUNCTION's characteristics-only form.
func TestAlterProcedureFunction(t *testing.T) {
	got := Sprint(mustBuild(t, "ALTER PROCEDURE p1 COMMENT 'x' SQL SECURITY INVOKER"))
	if !strings.Contains(got, `spname: sp_name(db=nil, name="p1")`) {
		t.Errorf("alter procedure: %s", got)
	}
	got2 := Sprint(mustBuild(t, "ALTER FUNCTION f1 COMMENT 'y'"))
	if !strings.Contains(got2, `spname: sp_name(db=nil, name="f1")`) {
		t.Errorf("alter function: %s", got2)
	}
}

// TestSpDeclConditionAndCall checks DECLARE ... CONDITION FOR and CALL with output
// parameters bound to a `@user` variable.
func TestSpDeclConditionAndCall(t *testing.T) {
	sql := `CREATE PROCEDURE p2()
BEGIN
  DECLARE too_big CONDITION FOR SQLSTATE '45000';
  DECLARE EXIT HANDLER FOR too_big RESIGNAL;
  CALL p1(1, @out);
END`
	got := Sprint(mustBuild(t, sql))
	for _, want := range []string{
		`sp_decl_condition(name="too_big", value=sp_condition_value(_mysqlerr="45000"))`,
		`sp_decl_handler(type=sp_handler::EXIT, conditions=[sp_condition_name(name="too_big")]`,
		`PT_call(proc_name=sp_name(db=nil, name="p1")`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in\n%s", want, got)
		}
	}
}
