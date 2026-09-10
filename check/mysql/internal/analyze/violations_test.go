package analyze

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/kr9ly/sqlshape/check/mysql/v2/internal/schema"
	"github.com/kr9ly/sqlshape/v2/x/facts"
)

const violationSchema = `-- sqlshape: mysql 8.4
CREATE TABLE accounts (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  email VARCHAR(255) NOT NULL,
  nick VARCHAR(20),
  balance INT NOT NULL DEFAULT 0,
  UNIQUE KEY accounts_email (email),
  UNIQUE KEY (nick),
  CHECK (balance >= 0),
  CONSTRAINT positive_nick CHECK (nick <> '')
);
CREATE TABLE payments (
  id BIGINT UNSIGNED NOT NULL PRIMARY KEY,
  account_id BIGINT UNSIGNED NOT NULL,
  amount INT NOT NULL,
  CONSTRAINT fk_payments_account FOREIGN KEY (account_id) REFERENCES accounts (id)
);
CREATE TABLE receipts (
  id BIGINT UNSIGNED NOT NULL PRIMARY KEY,
  payment_id BIGINT UNSIGNED NOT NULL,
  FOREIGN KEY (payment_id) REFERENCES payments (id) ON DELETE CASCADE ON UPDATE CASCADE
);
CREATE TABLE audit (
  id BIGINT UNSIGNED NOT NULL PRIMARY KEY,
  receipt_id BIGINT UNSIGNED NOT NULL,
  FOREIGN KEY (receipt_id) REFERENCES receipts (id)
);
`

// TestViolations covers the failure modes: what each write may violate, spelled as the
// expect line spells it (code key[:param]).
func TestViolations(t *testing.T) {
	s, err := schema.Load(violationSchema)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Problems) > 0 {
		t.Fatalf("schema problems: %v", s.Problems)
	}
	cases := []struct {
		sql  string
		want string
	}{
		// an INSERT may hit every unique key but one the server numbers itself, the foreign
		// keys and CHECKs over the inserted columns, and NOT NULL where the value may be NULL
		{"INSERT INTO accounts (email, nick) VALUES ($1, $2)", "1048 accounts.email:1, 1062 accounts_email, 1062 nick, 3819 positive_nick"},
		{"INSERT INTO accounts (id, email) VALUES ($1, $2)", "1048 accounts.email:2, 1062 PRIMARY, 1062 accounts_email"},
		{"INSERT INTO accounts (email, balance) VALUES ($1, $2)", "1048 accounts.balance:2, 1048 accounts.email:1, 1062 accounts_email, 3819 accounts_chk_1"},
		{"INSERT INTO accounts (email) VALUES ('a@x')", "1062 accounts_email"},
		{"INSERT INTO accounts (email, balance) VALUES ('a@x', -1)", "1062 accounts_email, 3819 accounts_chk_1"},
		{"INSERT INTO payments (id, account_id, amount) VALUES ($1, $2, $3)", "1048 payments.account_id:2, 1048 payments.amount:3, 1048 payments.id:1, 1062 PRIMARY, 1452 fk_payments_account"},
		{"INSERT INTO payments (id, account_id, amount) SELECT id, id, 1 FROM accounts WHERE id = $1", "1062 PRIMARY, 1452 fk_payments_account"},
		{"INSERT INTO payments (id, account_id, amount) SELECT id, id, nick FROM accounts", "1048 payments.amount, 1062 PRIMARY, 1452 fk_payments_account"},
		// IGNORE turns every constraint error into a warning
		{"INSERT IGNORE INTO accounts (email) VALUES ($1)", ""},
		{"UPDATE IGNORE accounts SET email = $1 WHERE id = $2", ""},
		{"DELETE IGNORE FROM accounts WHERE id = $1", ""},
		// ON DUPLICATE KEY UPDATE absorbs the insert's unique violations; its update has its own
		{"INSERT INTO accounts (email) VALUES ($1) ON DUPLICATE KEY UPDATE balance = $2", "1048 accounts.balance:2, 1048 accounts.email:1, 3819 accounts_chk_1"},
		{"INSERT INTO accounts (email) VALUES ($1) ON DUPLICATE KEY UPDATE nick = $2", "1048 accounts.email:1, 1062 nick, 3819 positive_nick"},
		// an UPDATE may hit the keys, foreign keys and CHECKs over its SET columns, and the
		// foreign keys of other tables that reference the columns it changes
		{"UPDATE accounts SET balance = balance + 1 WHERE id = $1", "3819 accounts_chk_1"},
		{"UPDATE accounts SET nick = $1 WHERE id = $2", "1062 nick, 3819 positive_nick"},
		{"UPDATE accounts SET email = $1 WHERE id = $2", "1048 accounts.email:1, 1062 accounts_email"},
		{"UPDATE accounts SET id = $1 WHERE id = $2", "1062 PRIMARY, 1451 fk_payments_account"},
		{"UPDATE payments SET account_id = $1 WHERE id = $2", "1048 payments.account_id:1, 1452 fk_payments_account"},
		// a cascade carries the question to the next table: receipts follow payments, audit
		// does not follow receipts
		{"UPDATE payments SET id = $1 WHERE id = $2", "1048 payments.id:1, 1062 PRIMARY"}, // receipts follow by cascade; audit refers to receipts.id, which does not change
		{"DELETE FROM accounts WHERE id = $1", "1451 fk_payments_account"},
		{"DELETE FROM payments WHERE id = $1", "1451 audit_ibfk_1"},
		{"DELETE FROM audit WHERE id = $1", ""},
		{"SELECT id FROM accounts WHERE id = $1", ""},
	}
	for _, c := range cases {
		r, err := Analyze(s, c.sql)
		if err != nil {
			t.Errorf("%s: %v", c.sql, err)
			continue
		}
		var keys []string
		for _, v := range r.Violations {
			k := fmt.Sprintf("%d %s", v.Code, v.Key())
			if v.Param > 0 {
				k += fmt.Sprintf(":%d", v.Param)
			}
			keys = append(keys, k)
		}
		sort.Strings(keys)
		if got := strings.Join(keys, ", "); got != c.want {
			t.Errorf("%s:\n got  %s\n want %s", c.sql, got, c.want)
		}
	}
}

// TestUses covers the column references a statement records: every table's (or view's)
// column once, by position, an INSERT / SET target that is never read marked Assigned.
func TestUses(t *testing.T) {
	s := load(t)
	cases := []struct {
		sql  string
		want string
	}{
		{"SELECT u.id, o.total FROM users u JOIN orders o ON o.user_id = u.id WHERE o.id = $1", "users.id@7 orders.total@13 orders.user_id@51 orders.id@74"},
		{"SELECT id, name FROM v_users WHERE id = $1", "v_users.id@7 v_users.name@11"},
		{"INSERT INTO users (name, email) VALUES ($1, $2)", "users.name@19= users.email@25="},
		{"UPDATE users SET name = $1, email = NULL WHERE id = $2", "users.name@17= users.email@28= users.id@47"},
		{"UPDATE users SET id = $1 WHERE id = $2", "users.id@17"},
		{"SELECT d.id FROM (SELECT id FROM users WHERE name = $1) d", "users.id@25 users.name@45"},
	}
	for _, c := range cases {
		r, err := Analyze(s, c.sql)
		if err != nil {
			t.Errorf("%s: %v", c.sql, err)
			continue
		}
		var parts []string
		for _, u := range r.Uses {
			p := fmt.Sprintf("%s.%s@%d", u.Table, u.Column, u.Position)
			if u.Assigned {
				p += "="
			}
			parts = append(parts, p)
		}
		if got := strings.Join(parts, " "); got != c.want {
			t.Errorf("%s:\n got  %s\n want %s", c.sql, got, c.want)
		}
	}
}

// TestChildren covers the nested blocks a statement's facts hang under its own: derived
// tables, CTE bodies, the subqueries of its conditions, the arms of a set operation.
func TestChildren(t *testing.T) {
	s := load(t)
	cases := []struct {
		sql  string
		want string // the top level's shape and its children's leaves
	}{
		{"SELECT d.id FROM (SELECT id FROM users WHERE id = 1) d", "derived d; [users]"},
		{"WITH c AS (SELECT id, user_id FROM orders) SELECT id FROM c WHERE user_id = $1", "cte c; [orders]"},
		{"SELECT id FROM users u WHERE EXISTS (SELECT 1 FROM orders o WHERE o.user_id = u.id)", "table users; [orders]"},
		{"SELECT id FROM users GROUP BY id WITH ROLLUP", "table users, grouping sets; "},
		{"SELECT name, count(*) FROM users GROUP BY 1", "table users, group name; "},
		{"SELECT count(*) FROM users", "table users, single; "},
		{"SELECT id FROM users WHERE id = (SELECT MAX(user_id) FROM orders)", "table users; [orders]"},
		{"SELECT id FROM users UNION SELECT user_id FROM orders WHERE id = 1", "UNION may combine rows; [users] [orders]"},
		{"UPDATE users SET name = $1 WHERE id IN (SELECT user_id FROM orders)", "table users; [orders]"},
		{"INSERT INTO orders (id, user_id, total) SELECT id, id, 0 FROM users WHERE id = $1", "table orders; [users]"},
	}
	for _, c := range cases {
		r, err := Analyze(s, c.sql)
		if err != nil {
			t.Errorf("%s: %v", c.sql, err)
			continue
		}
		top := r.Facts.Top
		var head []string
		for _, l := range top.Leaves {
			name := l.Table
			if name == "" {
				name = l.Alias
			}
			head = append(head, l.Kind.String()+" "+name)
		}
		if top.Many != "" {
			head = append(head, top.Many)
		}
		if top.Single {
			head = append(head, "single")
		}
		if top.GroupingSets {
			head = append(head, "grouping sets")
		}
		for _, g := range top.Groups {
			if !top.GroupingSets {
				head = append(head, "group "+g.Col.Column+g.Text)
			}
		}
		var kids []string
		for _, ch := range top.Children {
			var names []string
			for _, l := range ch.Leaves {
				names = append(names, l.Table)
			}
			kids = append(kids, "["+strings.Join(names, " ")+"]")
		}
		if got := strings.Join(head, ", ") + "; " + strings.Join(kids, " "); got != c.want {
			t.Errorf("%s:\n got  %s\n want %s", c.sql, got, c.want)
		}
		if r.Facts.Kind == facts.Insert && strings.Contains(c.sql, "SELECT") && r.Facts.Source == nil {
			t.Errorf("%s: INSERT ... SELECT records no Source", c.sql)
		}
	}
}

// TestLower covers the reading of a declared predicate in the facts language.
func TestLower(t *testing.T) {
	s := load(t)
	orders := s.Table("orders")
	cases := []struct {
		expr string
		want string
		err  string
	}{
		{"user_id = $1", "0.user_id = $1", ""},
		{"note IS NULL AND total > 0", "0.note IS NULL | opaque \"total > 0\" cols 0.total", ""},
		{"user_id IN (1, 2)", "0.user_id IN (const 1, const 2)", ""},
		{"nope = 1", "", "Unknown column 'nope' in 'where clause' (MySQL error 1054)"},
		{"user_id = ", "", "1064"},
	}
	for _, c := range cases {
		preds, err := Lower(s, c.expr, orders)
		if c.err != "" {
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Errorf("%s: error %v, want %q", c.expr, err, c.err)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", c.expr, err)
			continue
		}
		var parts []string
		for _, p := range preds {
			parts = append(parts, p.String())
		}
		if got := strings.Join(parts, " | "); got != c.want {
			t.Errorf("%s:\n got  %s\n want %s", c.expr, got, c.want)
		}
	}
	if _, err := Lower(s, "nope = 1", orders); err != nil {
		if e, ok := err.(*Error); !ok || e.Position != 0 {
			t.Errorf("position of the declaration's error: %v", err)
		}
	}
}

// TestAnalyzeView covers a view analyzed on its own: its columns, the base column each
// passes through, its facts.
func TestAnalyzeView(t *testing.T) {
	s := load(t)
	vr, err := AnalyzeView(s, s.View("v_stats"))
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprintf("%v %v", vr.Sources, vr.Facts.Top.Leaves[0].Table); got != "[{orders user_id} { }] orders" {
		t.Errorf("v_stats: %s", got)
	}
	if len(vr.Facts.Top.Groups) != 1 || len(vr.Facts.Uses) != 1 {
		t.Errorf("v_stats: groups %v, uses %v", vr.Facts.Top.Groups, vr.Facts.Uses)
	}
}
