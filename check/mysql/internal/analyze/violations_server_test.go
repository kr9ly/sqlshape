package analyze

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	driver "github.com/go-sql-driver/mysql"

	"github.com/kr9ly/sqlshape/check/mysql/v2/internal/schema"
	"github.com/kr9ly/sqlshape/mysqltest/v2"
)

// The runtime's reading of the server's messages (mysql/errors.go), repeated here so that
// the checker's keys are compared with what mysql.ConstraintError.Key will carry.
var (
	reDuplicate = regexp.MustCompile(`for key '(?:[^'.]+\.)?([^']+)'$`)
	reForeign   = regexp.MustCompile("CONSTRAINT `([^`]+)`")
	reNotNull   = regexp.MustCompile(`^Column '([^']+)' cannot be null$`)
	reCheck     = regexp.MustCompile(`^Check constraint '([^']+)' is violated\.$`)
)

// TestViolationsServer runs statements built to violate each predicted constraint against a
// real server and checks that the error number and the constraint's name are the ones the
// checker predicted; a statement predicted to violate nothing must go through.
func TestViolationsServer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	db, err := mysqltest.Start(ctx, violationSchema)
	if errors.Is(err, mysqltest.ErrNoServer) {
		t.Skip("no mysqld on PATH (nix-shell -p mysql84)")
	}
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s, err := schema.Load(violationSchema)
	if err != nil {
		t.Fatal(err)
	}
	conn := db.Conn()
	for _, seed := range []string{
		"INSERT INTO accounts (id, email, nick, balance) VALUES (1, 'a@x', 'a', 10), (2, 'b@x', NULL, 0), (3, 'c@x', NULL, 0)",
		"INSERT INTO payments (id, account_id, amount) VALUES (10, 1, 5)",
		"INSERT INTO receipts (id, payment_id) VALUES (100, 10)",
		"INSERT INTO audit (id, receipt_id) VALUES (1000, 100)",
	} {
		if _, err := conn.ExecContext(ctx, seed); err != nil {
			t.Fatalf("%s: %v", seed, err)
		}
	}
	cases := []struct {
		sql  string // with $n placeholders, as the checker reads it
		args []any
		want string // "<number> <key>" the server raises, "" when it must succeed
	}{
		{"INSERT INTO accounts (id, email) VALUES ($1, $2)", []any{1, "z@x"}, "1062 PRIMARY"},
		{"INSERT INTO accounts (email) VALUES ($1)", []any{"a@x"}, "1062 accounts_email"},
		{"INSERT INTO accounts (email, nick) VALUES ($1, $2)", []any{"z@x", "a"}, "1062 nick"},
		{"INSERT INTO accounts (email) VALUES ($1)", []any{nil}, "1048 email"},
		{"INSERT INTO accounts (email, balance) VALUES ($1, $2)", []any{"z@x", -1}, "3819 accounts_chk_1"},
		{"INSERT INTO accounts (email, nick) VALUES ($1, $2)", []any{"z@x", ""}, "3819 positive_nick"},
		{"INSERT INTO payments (id, account_id, amount) VALUES ($1, $2, $3)", []any{11, 999, 1}, "1452 fk_payments_account"},
		{"UPDATE accounts SET id = $1 WHERE id = $2", []any{50, 1}, "1451 fk_payments_account"},
		{"UPDATE accounts SET email = $1 WHERE id = $2", []any{"b@x", 1}, "1062 accounts_email"},
		{"UPDATE accounts SET balance = $1 WHERE id = $2", []any{-5, 1}, "3819 accounts_chk_1"},
		{"UPDATE payments SET account_id = $1 WHERE id = $2", []any{999, 10}, "1452 fk_payments_account"},
		{"DELETE FROM accounts WHERE id = $1", []any{1}, "1451 fk_payments_account"},
		{"DELETE FROM payments WHERE id = $1", []any{10}, "1451 audit_ibfk_1"},
		// two rows with a NULL nick do not collide on the unique key: nothing to expect
		{"INSERT INTO accounts (email) VALUES ($1)", []any{"d@x"}, ""},
		{"INSERT IGNORE INTO accounts (id, email) VALUES ($1, $2)", []any{1, "z@x"}, ""},
		{"UPDATE IGNORE accounts SET email = $1 WHERE id = $2", []any{"b@x", 1}, ""},
		{"DELETE IGNORE FROM accounts WHERE id = $1", []any{1}, ""},
		{"DELETE FROM audit WHERE id = $1", []any{1000}, ""},
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
		_, execErr := tx.ExecContext(ctx, strings.NewReplacer("$1", "?", "$2", "?", "$3", "?").Replace(c.sql), c.args...)
		tx.Rollback()
		if c.want == "" {
			if execErr != nil {
				t.Errorf("%s: predicted no violation, the server says %v", c.sql, execErr)
			}
			continue
		}
		var me *driver.MySQLError
		if !errors.As(execErr, &me) {
			t.Errorf("%s: want %s, the server says %v", c.sql, c.want, execErr)
			continue
		}
		got := fmt.Sprintf("%d %s", me.Number, serverKey(me))
		if got != c.want {
			t.Errorf("%s: want %s, the server says %s (%s)", c.sql, c.want, got, me.Message)
		}
		// the checker spells NOT NULL as table.column; the server names the column alone
		key := c.want
		if me.Number == 1048 {
			key = fmt.Sprintf("1048 %s.%s", tableOf(c.sql), serverKey(me))
		}
		if !predicted[key] {
			t.Errorf("%s: the server raises %s, the checker predicted %v", c.sql, key, keys(predicted))
		}
	}
}

func serverKey(me *driver.MySQLError) string {
	var re *regexp.Regexp
	switch me.Number {
	case 1062:
		re = reDuplicate
	case 1451, 1452:
		re = reForeign
	case 1048:
		re = reNotNull
	case 3819:
		re = reCheck
	default:
		return ""
	}
	if m := re.FindStringSubmatch(me.Message); m != nil {
		return m[1]
	}
	return ""
}

func tableOf(sql string) string {
	f := strings.Fields(sql)
	for i, w := range f {
		if (strings.EqualFold(w, "INTO") || strings.EqualFold(w, "UPDATE") || strings.EqualFold(w, "FROM")) && i+1 < len(f) {
			return f[i+1]
		}
	}
	return ""
}

func keys(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
