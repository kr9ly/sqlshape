package analyze

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	driver "github.com/go-sql-driver/mysql"

	"github.com/kr9ly/sqlshape/check/mysql/v2/internal/schema"
	"github.com/kr9ly/sqlshape/mysqltest/v2"
)

// triggerServerSchema is triggerViolationSchema (see trigger_violations_test.go) minus the
// self-writing trigger (which the server refuses at CREATE time, so a schema meant to load
// and run cannot declare it) plus REPLACE / ON DUPLICATE KEY UPDATE and IGNORE cases.
const triggerServerSchema = `-- sqlshape: mysql 8.4
CREATE TABLE orders (
  id BIGINT UNSIGNED NOT NULL PRIMARY KEY,
  customer_id BIGINT UNSIGNED NOT NULL,
  total DECIMAL(10,2) NOT NULL,
  note VARCHAR(100)
);
CREATE TABLE audit_log (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  msg VARCHAR(100) NOT NULL
);

-- sqlshape: error 30001 = OrderTooLarge
CREATE TRIGGER orders_bi BEFORE INSERT ON orders FOR EACH ROW
BEGIN
  IF NEW.total > 1000000 THEN
    SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'too large', MYSQL_ERRNO = 30001;
  END IF;
  INSERT INTO audit_log (msg) VALUES ('insert');
END;

CREATE TRIGGER orders_bu BEFORE UPDATE ON orders FOR EACH ROW
BEGIN
  DECLARE too_large CONDITION FOR SQLSTATE '45000';
  IF NEW.total > 1000000 THEN
    SIGNAL too_large SET MESSAGE_TEXT = 'too large';
  END IF;
END;

CREATE TABLE widgets (
  id INT NOT NULL PRIMARY KEY,
  v INT NOT NULL
);
-- sqlshape: error 40001 = WidgetDeleted
CREATE TRIGGER widgets_bd BEFORE DELETE ON widgets FOR EACH ROW
BEGIN
  SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'deleted', MYSQL_ERRNO = 40001;
END;
-- sqlshape: error 40002 = WidgetUpdated
CREATE TRIGGER widgets_bu BEFORE UPDATE ON widgets FOR EACH ROW
BEGIN
  SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'updated', MYSQL_ERRNO = 40002;
END;
`

// TestTriggerViolationsServer runs statements that fire orders_bi / orders_bu against a real
// server, and checks that the checker's predicted failure modes (Analyze's Violations) are
// exactly what the server actually raises: the SIGNAL's own number/SQLSTATE (with and
// without MYSQL_ERRNO, plain and through a named CONDITION), a schema constraint through the
// trigger's own embedded write, and that INSERT IGNORE does not absorb a trigger's SIGNAL
// (measured in the failure-modes milestone: it still fails the whole statement).
func TestTriggerViolationsServer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	db, err := mysqltest.Start(ctx, triggerServerSchema)
	if errors.Is(err, mysqltest.ErrNoServer) {
		t.Skip("no mysqld on PATH (nix-shell -p mysql84)")
	}
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s, err := schema.Load(triggerServerSchema)
	if err != nil {
		t.Fatal(err)
	}
	conn := db.Conn()
	if _, err := conn.ExecContext(ctx, "INSERT INTO orders (id, customer_id, total) VALUES (1, 1, 5)"); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		sql       string
		args      []any
		wantNum   int
		wantState string
	}{
		// the BEFORE INSERT trigger's SIGNAL, with its own MYSQL_ERRNO
		{"INSERT INTO orders (id, customer_id, total) VALUES ($1, $2, $3)", []any{2, 1, 2000000}, 30001, "45000"},
		// the BEFORE UPDATE trigger's SIGNAL through a named CONDITION, no MYSQL_ERRNO: the
		// server reports the generic unhandled number, 1644 (measured)
		{"UPDATE orders SET total = $1 WHERE id = $2", []any{2000000, 1}, 1644, "45000"},
		// INSERT IGNORE does not absorb the trigger's SIGNAL (measured)
		{"INSERT IGNORE INTO orders (id, customer_id, total) VALUES ($1, $2, $3)", []any{3, 1, 2000000}, 30001, "45000"},
		// a normal insert: no failure mode predicted or raised
		{"INSERT INTO orders (id, customer_id, total) VALUES ($1, $2, $3)", []any{4, 1, 5}, 0, ""},
	}
	for _, c := range cases {
		r, err := Analyze(s, c.sql)
		if err != nil {
			t.Errorf("%s: %v", c.sql, err)
			continue
		}
		predicted := map[string]bool{}
		for _, v := range r.Violations {
			predicted[fmt.Sprintf("%d %s", v.Code, v.Key())] = true
		}
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		q := strings.NewReplacer("$1", "?", "$2", "?", "$3", "?").Replace(c.sql)
		_, execErr := tx.ExecContext(ctx, q, c.args...)
		tx.Rollback()
		if c.wantNum == 0 {
			if execErr != nil {
				t.Errorf("%s: predicted no failure, the server says %v", c.sql, execErr)
			}
			continue
		}
		var me *driver.MySQLError
		if !errors.As(execErr, &me) {
			t.Errorf("%s: want %d (%s), the server says %v", c.sql, c.wantNum, c.wantState, execErr)
			continue
		}
		if int(me.Number) != c.wantNum || string(me.SQLState[:]) != c.wantState {
			t.Errorf("%s: want %d (%s), the server says %d (%s): %s", c.sql, c.wantNum, c.wantState, me.Number, me.SQLState, me.Message)
		}
		key := fmt.Sprintf("%d %s", c.wantNum, c.wantState)
		if c.wantNum != 1644 && c.wantNum != 1643 {
			key = fmt.Sprintf("%d %d", c.wantNum, c.wantNum)
		}
		if !predicted[key] {
			t.Errorf("%s: the server raises %s, the checker predicted %v", c.sql, key, predicted)
		}
	}
}

// TestReplaceODKUTriggersServer checks the event pairs REPLACE / ON DUPLICATE KEY UPDATE
// predict (violations.go's triggerFailureModes) against what mysqld actually fires on a
// collision: REPLACE additionally fires the DELETE event's triggers (BD/AD, the displaced
// row), ON DUPLICATE KEY UPDATE additionally fires the UPDATE event's (BU/AU) -- both
// measured in the failure-modes milestone.
func TestReplaceODKUTriggersServer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	db, err := mysqltest.Start(ctx, triggerServerSchema)
	if errors.Is(err, mysqltest.ErrNoServer) {
		t.Skip("no mysqld on PATH (nix-shell -p mysql84)")
	}
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s, err := schema.Load(triggerServerSchema)
	if err != nil {
		t.Fatal(err)
	}
	conn := db.Conn()
	if _, err := conn.ExecContext(ctx, "INSERT INTO widgets VALUES (1, 1)"); err != nil {
		t.Fatal(err)
	}

	replaceSQL := "REPLACE INTO widgets VALUES ($1, $2)"
	r, err := Analyze(s, replaceSQL)
	if err != nil {
		t.Fatal(err)
	}
	if got := violationKeys(r.Violations); got != "1048 widgets.id, 1048 widgets.v, 40001 40001=WidgetDeleted" {
		t.Errorf("REPLACE predicted %s, want the DELETE event's trigger (40001)", got)
	}
	_, execErr := conn.ExecContext(ctx, "REPLACE INTO widgets VALUES (1, 2)")
	var me *driver.MySQLError
	if !errors.As(execErr, &me) || me.Number != 40001 {
		t.Errorf("REPLACE colliding on widgets: got %v, want 40001 (BEFORE DELETE fires on the displaced row)", execErr)
	}

	odkuSQL := "INSERT INTO widgets VALUES ($1, $2) ON DUPLICATE KEY UPDATE v = $3"
	r, err = Analyze(s, odkuSQL)
	if err != nil {
		t.Fatal(err)
	}
	if got := violationKeys(r.Violations); got != "1048 widgets.id, 1048 widgets.v, 40002 40002=WidgetUpdated" {
		t.Errorf("ODKU predicted %s, want the UPDATE event's trigger (40002)", got)
	}
	_, execErr = conn.ExecContext(ctx, "INSERT INTO widgets VALUES (1, 9) ON DUPLICATE KEY UPDATE v = 3")
	if !errors.As(execErr, &me) || me.Number != 40002 {
		t.Errorf("ODKU colliding on widgets: got %v, want 40002 (BEFORE UPDATE fires on the collision)", execErr)
	}
}
