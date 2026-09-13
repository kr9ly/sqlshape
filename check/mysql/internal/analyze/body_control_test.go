package analyze

import (
	"testing"

	"github.com/kr9ly/sqlshape/check/mysql/v2/internal/schema"
)

// controlSchema exercises body.go's control-flow and DECLARE constructs that bodySchema and
// triggerViolationSchema do not: WHILE / REPEAT / a labeled LOOP with LEAVE and ITERATE, a
// labeled block, simple and searched CASE (with and without ELSE), a nested CALL inside a
// routine body, cursors driven through FETCH (with a NOT FOUND handler), and SELECT/FETCH
// INTO both a `@user` variable and a declared local one.
const controlSchema = `-- sqlshape: mysql 8.4
CREATE TABLE t (
  id INT NOT NULL PRIMARY KEY,
  v INT NOT NULL,
  note VARCHAR(50)
);

CREATE PROCEDURE loop_while(IN n INT)
BEGIN
  DECLARE i INT DEFAULT 0;
  WHILE i < n DO
    SET i = i + 1;
  END WHILE;
END;

CREATE PROCEDURE loop_repeat(IN n INT)
BEGIN
  DECLARE i INT DEFAULT 0;
  REPEAT
    SET i = i + 1;
  UNTIL i >= n END REPEAT;
END;

CREATE PROCEDURE loop_labeled(IN n INT)
BEGIN
  DECLARE i INT DEFAULT 0;
  myloop: LOOP
    SET i = i + 1;
    IF i >= n THEN
      LEAVE myloop;
    END IF;
    ITERATE myloop;
  END LOOP myloop;
END;

CREATE PROCEDURE labeled_block_test(IN n INT)
BEGIN
  blk: BEGIN
    IF n > 0 THEN
      LEAVE blk;
    END IF;
    SET @x = 1;
  END;
END;

CREATE PROCEDURE case_simple(IN n INT)
BEGIN
  DECLARE r INT DEFAULT 0;
  CASE n
    WHEN 1 THEN SET r = 10;
    WHEN 2 THEN SET r = 20;
    ELSE SET r = 0;
  END CASE;
END;

CREATE PROCEDURE case_searched_no_else(IN n INT)
BEGIN
  DECLARE r INT DEFAULT 0;
  CASE
    WHEN n > 10 THEN SET r = 1;
    WHEN n > 5 THEN SET r = 2;
  END CASE;
END;

CREATE PROCEDURE inner_proc(IN a INT)
BEGIN
  SET @y = a;
END;

CREATE PROCEDURE outer_caller(IN a INT)
BEGIN
  CALL inner_proc(a + 1);
END;

CREATE PROCEDURE cursor_fetch(IN cid INT)
BEGIN
  DECLARE done INT DEFAULT 0;
  DECLARE v_id INT;
  DECLARE v_note VARCHAR(50);
  DECLARE cur CURSOR FOR SELECT id, note FROM t WHERE id = cid;
  DECLARE CONTINUE HANDLER FOR NOT FOUND SET done = 1;
  OPEN cur;
  FETCH cur INTO v_id, v_note;
  CLOSE cur;
END;

CREATE PROCEDURE select_into_uservar(IN cid INT)
BEGIN
  SELECT v INTO @out FROM t WHERE id = cid LIMIT 1;
END;

CREATE PROCEDURE select_into_local(IN cid INT)
BEGIN
  DECLARE v_out INT;
  SELECT v INTO v_out FROM t WHERE id = cid LIMIT 1;
END;

CREATE PROCEDURE warn_handler()
BEGIN
  DECLARE CONTINUE HANDLER FOR SQLWARNING SET @w = 1;
  SIGNAL SQLSTATE '01000';
END;

-- sqlshape: error 30001 = NotFoundSignal
CREATE PROCEDURE not_found_handler()
BEGIN
  DECLARE CONTINUE HANDLER FOR NOT FOUND SET @nf = 1;
  SIGNAL SQLSTATE '02000';
END;

-- sqlshape: error 30002 = GeneralSignal
CREATE PROCEDURE sqlexception_handler()
BEGIN
  DECLARE EXIT HANDLER FOR SQLEXCEPTION SET @ex = 1;
  SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'boom', MYSQL_ERRNO = 30002;
END;
`

func loadControl(t *testing.T) *schema.Schema {
	t.Helper()
	s, err := schema.Load(controlSchema)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Problems) > 0 {
		t.Fatalf("schema problems: %v", s.Problems)
	}
	return s
}

func analyzeProc(t *testing.T, s *schema.Schema, name string) *BodyResult {
	t.Helper()
	r := s.Routine(name)
	if r == nil {
		t.Fatalf("procedure %s not loaded", name)
	}
	br, err := AnalyzeRoutine(s, r)
	if err != nil {
		t.Fatalf("%s: unexpected error: %v", name, err)
	}
	return br
}

func TestWalkNode_While(t *testing.T) {
	s := loadControl(t)
	analyzeProc(t, s, "loop_while")
}

func TestWalkNode_Repeat(t *testing.T) {
	s := loadControl(t)
	analyzeProc(t, s, "loop_repeat")
}

func TestWalkNode_LabeledLoopLeaveIterate(t *testing.T) {
	s := loadControl(t)
	analyzeProc(t, s, "loop_labeled")
}

func TestWalkNode_LabeledBlock(t *testing.T) {
	s := loadControl(t)
	analyzeProc(t, s, "labeled_block_test")
}

func TestWalkNode_SimpleCase(t *testing.T) {
	s := loadControl(t)
	analyzeProc(t, s, "case_simple")
}

func TestWalkNode_SearchedCaseNoElse(t *testing.T) {
	s := loadControl(t)
	analyzeProc(t, s, "case_searched_no_else")
}

func TestWalkCall_NestedInBody(t *testing.T) {
	s := loadControl(t)
	br := analyzeProc(t, s, "outer_caller")
	if len(br.Statements) != 0 {
		t.Errorf("got %d statements, want 0 (CALL alone writes no facts of its own in m4's body walk)", len(br.Statements))
	}
}

func TestCursorFetch_NotFoundHandler(t *testing.T) {
	s := loadControl(t)
	br := analyzeProc(t, s, "cursor_fetch")
	if len(br.Statements) != 1 {
		t.Fatalf("got %d statements, want 1 (the cursor's own DECLARE ... FOR; FETCH itself records no Facts of its own)", len(br.Statements))
	}
}

func TestSelectInto_UserVar(t *testing.T) {
	s := loadControl(t)
	analyzeProc(t, s, "select_into_uservar")
}

func TestSelectInto_LocalVar(t *testing.T) {
	s := loadControl(t)
	analyzeProc(t, s, "select_into_local")
}

func TestHandler_SQLWarning(t *testing.T) {
	s := loadControl(t)
	br := analyzeProc(t, s, "warn_handler")
	// SQLSTATE '01000' is class 01 (a warning): no failure mode, so even though the handler
	// is declared to catch nothing of it, no Violation is raised.
	if len(br.Violations) != 0 {
		t.Errorf("got %+v, want no Violations (SQLWARNING raises nothing to catch)", br.Violations)
	}
}

// TestHandler_NotFoundAbsorbs is a regression test for classifyMysqlerr's own Const case
// (once missing): a HANDLER FOR NOT FOUND must resolve to condNotFound, not the zero condRef
// (kind condSQLState, sqlstate ""), which would catch nothing (no real Violation ever has an
// empty SQLState) -- SIGNAL SQLSTATE '02000' (class "02", NOT FOUND) is absorbed.
func TestHandler_NotFoundAbsorbs(t *testing.T) {
	s := loadControl(t)
	br := analyzeProc(t, s, "not_found_handler")
	if len(br.Violations) != 0 {
		t.Errorf("got %+v, want no Violations (the NOT FOUND handler absorbs the '02000' SIGNAL)", br.Violations)
	}
}

// TestHandler_SQLExceptionAbsorbs is classifyMysqlerr's Const case for SQLEXCEPTION: it
// catches any class but "00"/"01"/"02", so a plain SIGNAL SQLSTATE '45000' is absorbed too.
func TestHandler_SQLExceptionAbsorbs(t *testing.T) {
	s := loadControl(t)
	br := analyzeProc(t, s, "sqlexception_handler")
	if len(br.Violations) != 0 {
		t.Errorf("got %+v, want no Violations (SQLEXCEPTION absorbs the '45000' SIGNAL)", br.Violations)
	}
}

// TestFetch_ColumnCountMismatch is 1328: FETCH's own target count must match the cursor's
// query column count.
func TestFetch_ColumnCountMismatch(t *testing.T) {
	text := `-- sqlshape: mysql 8.4
CREATE TABLE t (id INT NOT NULL PRIMARY KEY, v INT NOT NULL);
CREATE PROCEDURE bad_fetch()
BEGIN
  DECLARE v_id INT;
  DECLARE cur CURSOR FOR SELECT id, v FROM t;
  OPEN cur;
  FETCH cur INTO v_id;
  CLOSE cur;
END;
`
	s, err := schema.Load(text)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Problems) > 0 {
		t.Fatalf("schema problems: %v", s.Problems)
	}
	r := s.Routine("bad_fetch")
	_, err = AnalyzeRoutine(s, r)
	ae, ok := err.(*Error)
	if !ok || ae.Code != 1328 {
		t.Fatalf("got %v, want a 1328 Error", err)
	}
}

// TestFetch_UndeclaredVariable is 1327.
func TestFetch_UndeclaredVariable(t *testing.T) {
	text := `-- sqlshape: mysql 8.4
CREATE TABLE t (id INT NOT NULL PRIMARY KEY);
CREATE PROCEDURE bad_fetch2()
BEGIN
  DECLARE cur CURSOR FOR SELECT id FROM t;
  OPEN cur;
  FETCH cur INTO v_missing;
  CLOSE cur;
END;
`
	s, err := schema.Load(text)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Problems) > 0 {
		t.Fatalf("schema problems: %v", s.Problems)
	}
	r := s.Routine("bad_fetch2")
	_, err = AnalyzeRoutine(s, r)
	ae, ok := err.(*Error)
	if !ok || ae.Code != 1327 {
		t.Fatalf("got %v, want a 1327 Error", err)
	}
}

// TestSelectInto_UndeclaredLocalVariable is 1327 through selectIntoTarget's
// PT_select_sp_var case (as opposed to FETCH's, which selectIntoTarget also serves).
func TestSelectInto_UndeclaredLocalVariable(t *testing.T) {
	text := `-- sqlshape: mysql 8.4
CREATE TABLE t (id INT NOT NULL PRIMARY KEY, v INT NOT NULL);
CREATE PROCEDURE bad_select_into(IN cid INT)
BEGIN
  SELECT v INTO v_missing FROM t WHERE id = cid LIMIT 1;
END;
`
	s, err := schema.Load(text)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Problems) > 0 {
		t.Fatalf("schema problems: %v", s.Problems)
	}
	r := s.Routine("bad_select_into")
	_, err = AnalyzeRoutine(s, r)
	ae, ok := err.(*Error)
	if !ok || ae.Code != 1327 {
		t.Fatalf("got %v, want a 1327 Error", err)
	}
}
