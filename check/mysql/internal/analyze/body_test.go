package analyze

import (
	"strings"
	"testing"

	"github.com/kr9ly/sqlshape/check/mysql/v2/internal/schema"
)

// bodySchema declares users/orders (as testSchema does) plus a handful of triggers and
// routines exercising the body walk's name resolution and control flow.
const bodySchema = `-- sqlshape: mysql 8.4
CREATE TABLE users (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  name VARCHAR(100) NOT NULL,
  email VARCHAR(255),
  balance INT NOT NULL DEFAULT 0
);
CREATE TABLE orders (
  id BIGINT UNSIGNED NOT NULL PRIMARY KEY,
  user_id BIGINT UNSIGNED NOT NULL,
  total DECIMAL(10,2) NOT NULL,
  note TEXT,
  FOREIGN KEY (user_id) REFERENCES users (id)
);
CREATE TABLE order_audit (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  order_id BIGINT UNSIGNED NOT NULL,
  action VARCHAR(20) NOT NULL
);

CREATE TRIGGER orders_before_insert BEFORE INSERT ON orders FOR EACH ROW
BEGIN
  IF NEW.total > 1000000 THEN
    SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'too large', MYSQL_ERRNO = 30001;
  END IF;
  INSERT INTO order_audit (order_id, action) VALUES (NEW.id, 'insert');
END;

CREATE FUNCTION order_note_len(oid BIGINT UNSIGNED) RETURNS INT DETERMINISTIC
BEGIN
  DECLARE n INT DEFAULT 0;
  SELECT LENGTH(note) INTO n FROM orders WHERE id = oid;
  RETURN n;
END;

CREATE PROCEDURE bump_balance(IN uid BIGINT UNSIGNED, IN delta INT)
BEGIN
  UPDATE users SET balance = balance + delta WHERE id = uid;
END;
`

func loadBody(t *testing.T) *schema.Schema {
	s, err := schema.Load(bodySchema)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Problems) > 0 {
		t.Fatalf("schema problems: %v", s.Problems)
	}
	return s
}

func TestAnalyzeTriggerBasic(t *testing.T) {
	s := loadBody(t)
	tg := s.Trigger("orders_before_insert")
	if tg == nil {
		t.Fatal("trigger not loaded")
	}
	br, err := AnalyzeTrigger(s, tg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(br.Statements) != 1 {
		t.Fatalf("want 1 fact (the INSERT into order_audit), got %d", len(br.Statements))
	}
	if br.Statements[0].Facts.Writes[0].Table != "order_audit" {
		t.Errorf("want a write to order_audit, got %+v", br.Statements[0].Facts.Writes)
	}
}

func TestAnalyzeRoutineBasic(t *testing.T) {
	s := loadBody(t)
	fn := s.Routine("order_note_len")
	if fn == nil {
		t.Fatal("function not loaded")
	}
	br, err := AnalyzeRoutine(s, fn)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(br.Statements) != 1 {
		t.Fatalf("want 1 fact (the SELECT INTO), got %d", len(br.Statements))
	}

	pr := s.Routine("bump_balance")
	if pr == nil {
		t.Fatal("procedure not loaded")
	}
	br2, err := AnalyzeRoutine(s, pr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(br2.Statements) != 1 || br2.Statements[0].Facts.Writes[0].Table != "users" {
		t.Errorf("want 1 write to users, got %+v", br2.Statements)
	}
}

// bodyCase is one body that should fail to analyze with a specific MySQL error number.
type bodyCase struct {
	name string
	sql  string
	code int
}

// triggerCases each declare their own trigger against the schema's `orders` (or `users`)
// table, appended one at a time so a failure in one does not stop the others from being
// checked (schema.Load tolerates a bad CREATE TRIGGER as a Problem, not a hard error).
var triggerCases = []bodyCase{
	{"old in insert trigger", `CREATE TRIGGER t_old_ins BEFORE INSERT ON orders FOR EACH ROW SET @x = OLD.total`, 1363},
	{"new in delete trigger", `CREATE TRIGGER t_new_del AFTER DELETE ON orders FOR EACH ROW SET @x = NEW.total`, 1363},
	{"set old", `CREATE TRIGGER t_set_old BEFORE UPDATE ON orders FOR EACH ROW SET OLD.total = 1`, 1362},
	{"set new after", `CREATE TRIGGER t_set_new_after AFTER INSERT ON orders FOR EACH ROW SET NEW.total = 1`, 1362},
	{"unknown new column", `CREATE TRIGGER t_unknown_new BEFORE INSERT ON orders FOR EACH ROW SET @x = NEW.nope`, 1054},
	{"select without into", `CREATE TRIGGER t_select BEFORE INSERT ON orders FOR EACH ROW BEGIN SELECT 1; END`, 1415},
	{"commit in trigger", `CREATE TRIGGER t_commit BEFORE INSERT ON orders FOR EACH ROW BEGIN COMMIT; END`, 1422},
	{"leave no label", `CREATE TRIGGER t_leave BEFORE INSERT ON orders FOR EACH ROW BEGIN LEAVE nowhere; END`, 1308},
}

func TestAnalyzeTriggerErrors(t *testing.T) {
	for _, c := range triggerCases {
		t.Run(c.name, func(t *testing.T) {
			text := strings.Replace(bodySchema, "CREATE TRIGGER orders_before_insert", c.sql+";\nCREATE TRIGGER orders_before_insert", 1)
			s, err := schema.Load(text)
			if err != nil {
				t.Fatal(err)
			}
			var name string
			// the trigger name is the first identifier after CREATE TRIGGER in c.sql
			fields := strings.Fields(c.sql)
			for i, f := range fields {
				if strings.EqualFold(f, "TRIGGER") && i+1 < len(fields) {
					name = fields[i+1]
					break
				}
			}
			tg := s.Trigger(name)
			if tg == nil {
				t.Fatalf("trigger %s did not load (Problems: %v)", name, s.Problems)
			}
			_, err = AnalyzeTrigger(s, tg)
			if err == nil {
				t.Fatalf("want error %d, got none", c.code)
			}
			ae, ok := err.(*Error)
			if !ok {
				t.Fatalf("want *Error, got %T: %v", err, err)
			}
			if ae.Code != c.code {
				t.Errorf("want code %d, got %d (%s)", c.code, ae.Code, ae.Message)
			}
		})
	}
}

var routineCases = []bodyCase{
	{"function no return", `CREATE FUNCTION fn_noret() RETURNS INT BEGIN SET @x = 1; END`, 1320},
	{"procedure return", `CREATE PROCEDURE pr_ret() BEGIN RETURN 1; END`, 1313},
	{"iterate no label", `CREATE PROCEDURE pr_iter() BEGIN lp: LOOP ITERATE nope; END LOOP; END`, 1308},
	{"select without into in function", `CREATE FUNCTION fn_sel() RETURNS INT BEGIN SELECT 1; RETURN 1; END`, 1415},
	{"commit in function", `CREATE FUNCTION fn_commit() RETURNS INT BEGIN COMMIT; RETURN 1; END`, 1422},
	{"undeclared cursor", `CREATE PROCEDURE pr_cur() BEGIN OPEN nosuch; END`, 1324},
	{"fetch count mismatch", `CREATE PROCEDURE pr_fetch() BEGIN DECLARE x INT; DECLARE cur CURSOR FOR SELECT id, name FROM users; OPEN cur; FETCH cur INTO x; CLOSE cur; END`, 1328},
	{"select into count mismatch", `CREATE PROCEDURE pr_selinto() BEGIN DECLARE x INT; SELECT id, name INTO x FROM users; END`, 1222},
	{"fetch undeclared var", `CREATE PROCEDURE pr_fetchvar() BEGIN DECLARE cur CURSOR FOR SELECT id FROM users; OPEN cur; FETCH cur INTO nosuchvar; CLOSE cur; END`, 1327},
}

func TestAnalyzeRoutineErrors(t *testing.T) {
	for _, c := range routineCases {
		t.Run(c.name, func(t *testing.T) {
			text := strings.Replace(bodySchema, "CREATE FUNCTION order_note_len", c.sql+";\nCREATE FUNCTION order_note_len", 1)
			s, err := schema.Load(text)
			if err != nil {
				t.Fatal(err)
			}
			fields := strings.Fields(c.sql)
			var name string
			for i, f := range fields {
				if (strings.EqualFold(f, "FUNCTION") || strings.EqualFold(f, "PROCEDURE")) && i+1 < len(fields) {
					name = strings.SplitN(fields[i+1], "(", 2)[0]
					break
				}
			}
			r := s.Routine(name)
			if r == nil {
				t.Fatalf("routine %s did not load (Problems: %v)", name, s.Problems)
			}
			_, err = AnalyzeRoutine(s, r)
			if err == nil {
				t.Fatalf("want error %d, got none", c.code)
			}
			ae, ok := err.(*Error)
			if !ok {
				t.Fatalf("want *Error, got %T: %v", err, err)
			}
			if ae.Code != c.code {
				t.Errorf("want code %d, got %d (%s)", c.code, ae.Code, ae.Message)
			}
		})
	}
}

// TestVarShadowsColumn checks that a routine's local variable of the same name as a
// column resolves to the variable, not the column (MySQL's own rule).
func TestVarShadowsColumn(t *testing.T) {
	text := bodySchema + `
CREATE PROCEDURE pr_shadow(IN id BIGINT UNSIGNED)
BEGIN
  DECLARE total INT DEFAULT 5;
  SELECT total FROM users WHERE users.id = id LIMIT 1;
END;
`
	s, err := schema.Load(text)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Problems) > 0 {
		t.Fatalf("schema problems: %v", s.Problems)
	}
	r := s.Routine("pr_shadow")
	if r == nil {
		t.Fatal("routine not loaded")
	}
	// `total` is not a column of users: if the local variable did not shadow it (MySQL's
	// own rule -- a variable/parameter resolves before a column), this would be 1054.
	br, err := AnalyzeRoutine(s, r)
	if err != nil {
		t.Fatalf("unexpected error (the local variable should have shadowed the column): %v", err)
	}
	if len(br.Statements) != 1 {
		t.Errorf("want 1 fact (the bodiless SELECT, allowed in a procedure), got %d", len(br.Statements))
	}
}
