package analyze

import (
	"testing"

	"github.com/kr9ly/sqlshape/check/mysql/v2/internal/schema"
)

// loadAndRoutine loads text (failing the test on a load error or a Problem) and returns the
// named routine, failing the test if it is not there.
func loadAndRoutine(t *testing.T, text, name string) (*schema.Schema, *schema.Routine) {
	t.Helper()
	s, err := schema.Load(text)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Problems) > 0 {
		t.Fatalf("schema problems: %v", s.Problems)
	}
	r := s.Routine(name)
	if r == nil {
		t.Fatalf("routine %s not loaded", name)
	}
	return s, r
}

// errPropCases each declare one procedure whose body fails to type for an ordinary reason
// (an unknown identifier, "nope"), reached through a different control-flow path each time:
// walkIf's condition and its THEN body, an ELSEIF chain, WHILE's condition and body, REPEAT's
// body and its UNTIL condition, both CASE forms' WHEN expressions and THEN bodies, and a
// SIGNAL information item's own expression. Each is a distinct "if err != nil { return err }"
// propagation site in body.go that a success-only body never reaches.
var errPropCases = []struct {
	name string
	body string
}{
	{"if_cond", `IF nope THEN SET @x = 1; END IF;`},
	{"if_then", `IF n = 1 THEN SET @x = nope; END IF;`},
	{"if_elseif_cond", `IF n = 1 THEN SET @x = 1; ELSEIF nope THEN SET @x = 2; END IF;`},
	{"if_elseif_then", `IF n = 1 THEN SET @x = 1; ELSEIF n = 2 THEN SET @x = nope; END IF;`},
	{"if_else", `IF n = 1 THEN SET @x = 1; ELSE SET @x = nope; END IF;`},
	{"while_cond", `WHILE nope > 0 DO SET @x = 1; END WHILE;`},
	{"while_body", `WHILE n > 0 DO SET @x = nope; END WHILE;`},
	{"repeat_body", `REPEAT SET @x = nope; UNTIL n > 0 END REPEAT;`},
	{"repeat_cond", `REPEAT SET @x = 1; UNTIL nope END REPEAT;`},
	{"case_simple_selector", `CASE nope WHEN 1 THEN SET @x = 1; END CASE;`},
	{"case_simple_when", `CASE n WHEN nope THEN SET @x = 1; END CASE;`},
	{"case_simple_then", `CASE n WHEN 1 THEN SET @x = nope; END CASE;`},
	{"case_searched_when", `CASE WHEN nope THEN SET @x = 1; END CASE;`},
	{"case_searched_then", `CASE WHEN n = 1 THEN SET @x = nope; END CASE;`},
	{"signal_item", `SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = nope;`},
	{"set_expr", `SET @x = nope;`},
}

func TestErrorPropagation(t *testing.T) {
	for _, c := range errPropCases {
		t.Run(c.name, func(t *testing.T) {
			text := `-- sqlshape: mysql 8.4
CREATE PROCEDURE p(IN n INT)
BEGIN
  ` + c.body + `
END;
`
			s, err := schema.Load(text)
			if err != nil {
				t.Fatal(err)
			}
			if len(s.Problems) > 0 {
				t.Fatalf("schema problems: %v", s.Problems)
			}
			r := s.Routine("p")
			if r == nil {
				t.Fatal("routine p not loaded")
			}
			if _, err := AnalyzeRoutine(s, r); err == nil {
				t.Fatalf("got no error, want one (nope is not a declared variable or column)")
			}
		})
	}
}

// TestWalkSet_MultipleAssignments exercises flattenSetList's own tail chain (three
// assignments in one SET, not just one).
func TestWalkSet_MultipleAssignments(t *testing.T) {
	text := `-- sqlshape: mysql 8.4
CREATE PROCEDURE p(IN n INT)
BEGIN
  DECLARE a INT DEFAULT 0;
  DECLARE b INT DEFAULT 0;
  SET a = 1, b = 2, @c = 3;
END;
`
	s, r := loadAndRoutine(t, text, "p")
	if _, err := AnalyzeRoutine(s, r); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestWalkSetTarget_SystemVariable: an unqualified SET target that resolves to neither a
// local variable/parameter nor NEW/OLD is a system variable, silently accepted (m4's item 4).
func TestWalkSetTarget_SystemVariable(t *testing.T) {
	text := `-- sqlshape: mysql 8.4
CREATE PROCEDURE p()
BEGIN
  SET sort_buffer_size = 1000000;
END;
`
	s, r := loadAndRoutine(t, text, "p")
	if _, err := AnalyzeRoutine(s, r); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestWalkSetTarget_UnrecognizedQualifier: inside a trigger, a qualifier that is not NEW or
// OLD is not validated either (m4's item 4) -- unlike a bare name, this exercises
// walkSetTarget's own final fallthrough (as opposed to the qual == "" one above).
func TestWalkSetTarget_UnrecognizedQualifier(t *testing.T) {
	text := `-- sqlshape: mysql 8.4
CREATE TABLE orders (id INT NOT NULL PRIMARY KEY, total INT NOT NULL);
CREATE TRIGGER t_bogus_qual BEFORE INSERT ON orders FOR EACH ROW
BEGIN
  SET foo.bar = 1;
END;
`
	s, err := schema.Load(text)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Problems) > 0 {
		t.Fatalf("schema problems: %v", s.Problems)
	}
	tg := s.Trigger("t_bogus_qual")
	if tg == nil {
		t.Fatal("trigger not loaded")
	}
	if _, err := AnalyzeTrigger(s, tg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestWalkSet_DefaultKeyword: `SET x = DEFAULT` carries no expression at all (PT_set_variable's
// own opt_expr is nil for it, unlike an ordinary assignment) -- exprAt's own nil guard.
func TestWalkSet_DefaultKeyword(t *testing.T) {
	text := `-- sqlshape: mysql 8.4
CREATE PROCEDURE p()
BEGIN
  SET sql_mode = DEFAULT;
END;
`
	s, r := loadAndRoutine(t, text, "p")
	if _, err := AnalyzeRoutine(s, r); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestSignal_UndeclaredNamedCondition: SIGNAL of an undeclared named condition resolves to
// nothing (resolveCondValue's own !ok path) -- not an error at this milestone (the server
// itself refuses it at CREATE time; schema.Load's grammar accepts naming any ident).
func TestSignal_UndeclaredNamedCondition(t *testing.T) {
	text := `-- sqlshape: mysql 8.4
CREATE PROCEDURE p()
BEGIN
  SIGNAL never_declared_cond;
END;
`
	s, r := loadAndRoutine(t, text, "p")
	br, err := AnalyzeRoutine(s, r)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(br.Violations) != 0 {
		t.Errorf("got %+v, want no Violations (nothing to predict for an unresolved condition)", br.Violations)
	}
}

// TestSignal_NamedConditionByNumber is 1646: SIGNAL/RESIGNAL of a named CONDITION that was
// declared FOR a MySQL error number (not a SQLSTATE) is refused at CREATE time (measured).
func TestSignal_NamedConditionByNumber(t *testing.T) {
	text := `-- sqlshape: mysql 8.4
CREATE PROCEDURE p()
BEGIN
  DECLARE my_err CONDITION FOR 1062;
  SIGNAL my_err;
END;
`
	s, r := loadAndRoutine(t, text, "p")
	_, err := AnalyzeRoutine(s, r)
	ae, ok := err.(*Error)
	if !ok || ae.Code != 1646 {
		t.Fatalf("got %v, want a 1646 Error", err)
	}
}

// TestLookupCondition_Undeclared: a HANDLER FOR an undeclared named condition resolves to
// nothing (lookupCondition's own "not found" path); resolveHandlerConditions then simply
// omits it (not an error -- schema.Load's own grammar accepts naming any ident here, the
// server itself refusing an unknown one at CREATE time, which this milestone does not
// re-check).
func TestLookupCondition_Undeclared(t *testing.T) {
	text := `-- sqlshape: mysql 8.4
CREATE PROCEDURE p()
BEGIN
  DECLARE CONTINUE HANDLER FOR never_declared SET @x = 1;
  SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'boom';
END;
`
	s, r := loadAndRoutine(t, text, "p")
	br, err := AnalyzeRoutine(s, r)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// the undeclared condition catches nothing, so the SIGNAL still propagates.
	if len(br.Violations) != 1 {
		t.Errorf("got %+v, want the SIGNAL to propagate uncaught", br.Violations)
	}
}

// TestCommitCheck_AllowedInProcedure: COMMIT (and, by the same rule, DDL) is fine in a
// PROCEDURE -- commitCheck's own early exemption, as opposed to the 1422 tests, which are
// all triggers or functions.
func TestCommitCheck_AllowedInProcedure(t *testing.T) {
	text := `-- sqlshape: mysql 8.4
CREATE PROCEDURE p()
BEGIN
  COMMIT;
END;
`
	s, r := loadAndRoutine(t, text, "p")
	if _, err := AnalyzeRoutine(s, r); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestCommitCheck_OrdinaryStatementInTrigger: a statement neither DDL nor transaction
// control (here XA START, folded to a generic {sql_command: SQLCOM_XA_START} Struct, the
// same shape COMMIT/ROLLBACK take) is fine even inside a trigger -- commitCheck's own final
// fallthrough (as opposed to TestCommitCheck_DDLInFunction's 1422 and the
// "procedure: anything goes" branch other tests already cover).
func TestCommitCheck_OrdinaryStatementInTrigger(t *testing.T) {
	text := `-- sqlshape: mysql 8.4
CREATE TABLE t (id INT NOT NULL PRIMARY KEY);
CREATE TRIGGER tg BEFORE INSERT ON t FOR EACH ROW
BEGIN
  XA START 'x';
END;
`
	s, err := schema.Load(text)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Problems) > 0 {
		t.Fatalf("schema problems: %v", s.Problems)
	}
	tg := s.Trigger("tg")
	if tg == nil {
		t.Fatal("trigger not loaded")
	}
	if _, err := AnalyzeTrigger(s, tg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestCommitCheck_DDLInFunction is 1422 for a DDL statement (not just COMMIT/transaction
// control) reached inside a FUNCTION body.
func TestCommitCheck_DDLInFunction(t *testing.T) {
	text := `-- sqlshape: mysql 8.4
CREATE TABLE t (id INT NOT NULL PRIMARY KEY);
CREATE FUNCTION f() RETURNS INT
BEGIN
  TRUNCATE TABLE t;
  RETURN 1;
END;
`
	s, err := schema.Load(text)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Problems) > 0 {
		t.Fatalf("schema problems: %v", s.Problems)
	}
	r := s.Routine("f")
	if r == nil {
		t.Fatal("function not loaded")
	}
	_, err = AnalyzeRoutine(s, r)
	ae, ok := err.(*Error)
	if !ok || ae.Code != 1422 {
		t.Fatalf("got %v, want a 1422 Error", err)
	}
}

// TestWalkCall_NestedArgError: a nested CALL's own argument expression can fail to type
// (walkCall's own error propagation, as opposed to call.go's callStmt, which m4's body walk
// does not reach: a body's own CALL is walked, not validated against the callee's
// parameters -- see walkCall's doc).
func TestWalkCall_NestedArgError(t *testing.T) {
	text := `-- sqlshape: mysql 8.4
CREATE PROCEDURE inner_p(IN a INT)
BEGIN
  SET @y = a;
END;
CREATE PROCEDURE outer_p()
BEGIN
  CALL inner_p(nope);
END;
`
	s, r := loadAndRoutine(t, text, "outer_p")
	if _, err := AnalyzeRoutine(s, r); err == nil {
		t.Fatal("got no error, want one (nope is not declared)")
	}
}

// TestWalkFetch_UndeclaredCursor is 1324 through FETCH's own direct check (walkFetch),
// distinct from walkCursorToken's OPEN/CLOSE check of the same error.
func TestWalkFetch_UndeclaredCursor(t *testing.T) {
	text := `-- sqlshape: mysql 8.4
CREATE PROCEDURE p()
BEGIN
  DECLARE v INT;
  FETCH never_declared INTO v;
END;
`
	s, r := loadAndRoutine(t, text, "p")
	_, err := AnalyzeRoutine(s, r)
	ae, ok := err.(*Error)
	if !ok || ae.Code != 1324 {
		t.Fatalf("got %v, want a 1324 Error", err)
	}
}

// TestWalkSelect_StatementError: an ordinary error in the SELECT itself (an unknown column,
// not an INTO-related one) propagates through walkSelect's own selectStmt call.
func TestWalkSelect_StatementError(t *testing.T) {
	text := `-- sqlshape: mysql 8.4
CREATE TABLE t (id INT NOT NULL PRIMARY KEY);
CREATE PROCEDURE p()
BEGIN
  SELECT nope FROM t;
END;
`
	s, r := loadAndRoutine(t, text, "p")
	if _, err := AnalyzeRoutine(s, r); err == nil {
		t.Fatal("got no error, want one (nope is not a column of t)")
	}
}

// TestSelectInto_UnionNoTopLevelInto: selectInto's own fallback when the query expression's
// body is not a plain PT_query_specification (here a UNION): no INTO is detected at the top
// level, so the statement is treated as INTO-less (a procedure's own possible result set),
// not as though the union somehow named INTO targets.
func TestSelectInto_UnionNoTopLevelInto(t *testing.T) {
	text := `-- sqlshape: mysql 8.4
CREATE PROCEDURE p()
BEGIN
  SELECT 1 AS x UNION SELECT 2;
END;
`
	s, r := loadAndRoutine(t, text, "p")
	br, err := AnalyzeRoutine(s, r)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(br.Statements) != 1 || br.Statements[0].Columns == nil {
		t.Errorf("got %+v, want one statement with its own result columns (no INTO)", br.Statements)
	}
}

// TestWalkDML_EmbeddedStatementError: an embedded INSERT/UPDATE/DELETE's own error (here an
// unknown column) propagates through walkDML's own run(n) call.
func TestWalkDML_EmbeddedStatementError(t *testing.T) {
	text := `-- sqlshape: mysql 8.4
CREATE TABLE t (id INT NOT NULL PRIMARY KEY);
CREATE PROCEDURE p()
BEGIN
  UPDATE t SET nope = 1 WHERE id = 1;
END;
`
	s, r := loadAndRoutine(t, text, "p")
	if _, err := AnalyzeRoutine(s, r); err == nil {
		t.Fatal("got no error, want one (nope is not a column of t)")
	}
}

// TestAbsorb_HandlerBodyError: a HANDLER's own body can itself fail to type (here an unknown
// identifier) -- absorb's own error propagation out of walking the handler's body.
func TestAbsorb_HandlerBodyError(t *testing.T) {
	text := `-- sqlshape: mysql 8.4
CREATE PROCEDURE p()
BEGIN
  DECLARE EXIT HANDLER FOR SQLEXCEPTION SET @x = nope;
  SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'boom';
END;
`
	s, r := loadAndRoutine(t, text, "p")
	if _, err := AnalyzeRoutine(s, r); err == nil {
		t.Fatal("got no error, want one (the handler body's own nope)")
	}
}

// TestWalkDecl_CursorQueryError: a cursor's own query can fail to type (here an unknown
// column) -- walkDecl's sp_decl_cursor error propagation.
func TestWalkDecl_CursorQueryError(t *testing.T) {
	text := `-- sqlshape: mysql 8.4
CREATE TABLE t (id INT NOT NULL PRIMARY KEY);
CREATE PROCEDURE p()
BEGIN
  DECLARE cur CURSOR FOR SELECT nope FROM t;
  OPEN cur;
  CLOSE cur;
END;
`
	s, r := loadAndRoutine(t, text, "p")
	if _, err := AnalyzeRoutine(s, r); err == nil {
		t.Fatal("got no error, want one (nope is not a column of t)")
	}
}

// TestWalkSet_LocalVariableRHSError: `SET localvar = ...` (PT_set_variable, as opposed to
// set_expr's `@x = ...`, PT_option_value_no_option_type_user_var) with an erroring
// right-hand side.
func TestWalkSet_LocalVariableRHSError(t *testing.T) {
	text := `-- sqlshape: mysql 8.4
CREATE PROCEDURE p()
BEGIN
  DECLARE a INT DEFAULT 0;
  SET a = nope;
END;
`
	s, r := loadAndRoutine(t, text, "p")
	if _, err := AnalyzeRoutine(s, r); err == nil {
		t.Fatal("got no error, want one (nope is not declared)")
	}
}

// TestWalkNode_NotWalked: a body statement this milestone does not model (SHOW WARNINGS) is
// silently skipped, not an error -- walkNode's own final fallback.
func TestWalkNode_NotWalked(t *testing.T) {
	text := `-- sqlshape: mysql 8.4
CREATE PROCEDURE p()
BEGIN
  SHOW WARNINGS;
END;
`
	s, r := loadAndRoutine(t, text, "p")
	if _, err := AnalyzeRoutine(s, r); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestAnalyzeTrigger_Cycle: trigger A (on x) writes y, whose own trigger B writes back to x
// -- analyzing A's own body reaches triggerFailureModes for y's triggers, which analyzes B,
// which in turn reaches back to A while A's own AnalyzeTrigger call is still on the stack
// (the cache's "loaded" placeholder branch, AnalyzeTrigger's own doc comment). Neither
// recurses forever; the inner call sees the placeholder's zero BodyResult/error.
func TestAnalyzeTrigger_Cycle(t *testing.T) {
	text := `-- sqlshape: mysql 8.4
CREATE TABLE x (id INT NOT NULL PRIMARY KEY);
CREATE TABLE y (id INT NOT NULL PRIMARY KEY);
CREATE TRIGGER trg_x AFTER INSERT ON x FOR EACH ROW
BEGIN
  INSERT INTO y (id) VALUES (NEW.id);
END;
CREATE TRIGGER trg_y AFTER INSERT ON y FOR EACH ROW
BEGIN
  INSERT INTO x (id) VALUES (NEW.id);
END;
`
	s, err := schema.Load(text)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Problems) > 0 {
		t.Fatalf("schema problems: %v", s.Problems)
	}
	tg := s.Trigger("trg_x")
	if tg == nil {
		t.Fatal("trigger not loaded")
	}
	if _, err := AnalyzeTrigger(s, tg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestAnalyzeRoutine_Cycle: a FUNCTION whose own body writes a table through an UPDATE
// calling itself recursively in the SET value reaches AnalyzeRoutine on itself while its own
// first call is still on the stack (walkDML's own violations() -> calledRoutineViolations),
// the same cycle-breaking cache AnalyzeTrigger's own test exercises.
func TestAnalyzeRoutine_Cycle(t *testing.T) {
	text := `-- sqlshape: mysql 8.4
CREATE TABLE t (id INT NOT NULL PRIMARY KEY, v INT NOT NULL);
CREATE FUNCTION rec_f(x INT) RETURNS INT DETERMINISTIC
BEGIN
  UPDATE t SET v = rec_f(x - 1) WHERE id = 1;
  RETURN x;
END;
`
	s, r := loadAndRoutine(t, text, "rec_f")
	if _, err := AnalyzeRoutine(s, r); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestWalkSelect_1172InTrigger: a SELECT ... INTO inside a trigger body that cannot be
// proved to return at most one row is 1172, tagged with the trigger's own Table/Trigger (as
// opposed to triggerViolationSchema's own procedure-only 1172 test).
func TestWalkSelect_1172InTrigger(t *testing.T) {
	text := `-- sqlshape: mysql 8.4
CREATE TABLE orders (id INT NOT NULL PRIMARY KEY, customer_id INT NOT NULL, total INT NOT NULL);
CREATE TABLE audit_log (id INT NOT NULL AUTO_INCREMENT PRIMARY KEY, total INT NOT NULL);
CREATE TRIGGER orders_ai AFTER INSERT ON orders FOR EACH ROW
BEGIN
  DECLARE t INT;
  SELECT total INTO t FROM orders WHERE customer_id = NEW.customer_id;
  INSERT INTO audit_log (total) VALUES (t);
END;
`
	s, err := schema.Load(text)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Problems) > 0 {
		t.Fatalf("schema problems: %v", s.Problems)
	}
	tg := s.Trigger("orders_ai")
	if tg == nil {
		t.Fatal("trigger not loaded")
	}
	br, err := AnalyzeTrigger(s, tg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	found := false
	for _, v := range br.Violations {
		if v.Code == 1172 {
			found = true
			if v.Table != "orders" || v.Trigger != "orders_ai" {
				t.Errorf("got Table=%q Trigger=%q, want orders/orders_ai", v.Table, v.Trigger)
			}
		}
	}
	if !found {
		t.Errorf("got %+v, want a 1172 Violation", br.Violations)
	}
}

// TestWalkDML_MultiTableWriteTablesInProcedure: a multi-table DELETE inside a PROCEDURE (not
// a trigger, so ownTableWrite's own check does not apply) records every target table in
// WriteTables, including the "more" ones -- walkDML's own w.more loop, for call.go's
// checkCalledRoutineOverlap to read back.
func TestWalkDML_MultiTableWriteTablesInProcedure(t *testing.T) {
	text := `-- sqlshape: mysql 8.4
CREATE TABLE a (id INT NOT NULL PRIMARY KEY);
CREATE TABLE b (id INT NOT NULL PRIMARY KEY);
CREATE PROCEDURE p()
BEGIN
  DELETE a, b FROM a JOIN b ON a.id = b.id;
END;
`
	s, r := loadAndRoutine(t, text, "p")
	br, err := AnalyzeRoutine(s, r)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(br.WriteTables) != 2 {
		t.Fatalf("got %v, want both a and b", br.WriteTables)
	}
}

// TestOwnTableWrite_MultiTableSecondTarget: a trigger's own table written as a multi-table
// DELETE's second target (not its first) is still 1442 -- ownTableWrite's own w.more loop, as
// opposed to triggerViolationSchema's own single-table self-write test.
func TestOwnTableWrite_MultiTableSecondTarget(t *testing.T) {
	text := `-- sqlshape: mysql 8.4
CREATE TABLE a (id INT NOT NULL PRIMARY KEY);
CREATE TABLE b (id INT NOT NULL PRIMARY KEY);
CREATE TRIGGER t_multi AFTER DELETE ON b FOR EACH ROW
BEGIN
  DELETE a, b FROM a JOIN b ON a.id = b.id WHERE a.id = OLD.id;
END;
`
	s, err := schema.Load(text)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Problems) > 0 {
		t.Fatalf("schema problems: %v", s.Problems)
	}
	tg := s.Trigger("t_multi")
	if tg == nil {
		t.Fatal("trigger not loaded")
	}
	_, err = AnalyzeTrigger(s, tg)
	ae, ok := err.(*Error)
	if !ok || ae.Code != 1442 {
		t.Fatalf("got %v, want a 1442 Error (b, the trigger's own table, is the DELETE's second target)", err)
	}
}
