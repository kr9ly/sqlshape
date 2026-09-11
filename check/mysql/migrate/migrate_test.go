package migrate

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kr9ly/sqlshape/check/mysql/v2/diff"
	"github.com/kr9ly/sqlshape/check/mysql/v2/dump"
	"github.com/kr9ly/sqlshape/check/mysql/v2/internal/schema"
)

type canonical struct {
	s       *schema.Schema
	text    string
	intents []Intent
}

func start(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	if _, _, err := (dump.Local{}).Canonical(ctx, "-- sqlshape: mysql 8.4\nCREATE TABLE t (a INT);"); errors.Is(err, dump.ErrNoServer) {
		t.Skip("no mysqld on PATH (nix-shell -p mysql84)")
	} else if err != nil {
		t.Fatal(err)
	}
	return ctx
}

func mustCanonical(t *testing.T, ctx context.Context, sql string) canonical {
	t.Helper()
	s, text, err := (dump.Local{}).Canonical(ctx, sql)
	if err != nil {
		t.Fatalf("canonical: %v\n%s", err, sql)
	}
	if len(s.Problems) > 0 {
		t.Fatalf("problems: %v", s.Problems)
	}
	in, err := ParseIntents(sql)
	if err != nil {
		t.Fatalf("intents: %v", err)
	}
	return canonical{s, text, in}
}

func example(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("../../../examples/5-mysql/schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// plan runs Plan from a to b and applies the DDL on a server holding a, expecting the
// result to read back as b.
func plan(t *testing.T, ctx context.Context, name string, from, to canonical) []string {
	t.Helper()
	ddl, err := Plan(from.s, to.s, to.intents)
	if err != nil {
		t.Fatalf("%s: plan: %v\n%s", name, err, strings.Join(ddl, "\n"))
	}
	changes, err := Verify(ctx, dump.Local{}, from.text, strings.Join(ddl, "\n"), to.s)
	if err != nil {
		t.Fatalf("%s: verify: %v\nDDL:\n%s", name, err, strings.Join(ddl, "\n"))
	}
	if len(changes) > 0 {
		var b strings.Builder
		for _, c := range changes {
			b.WriteString(c.String() + "\n")
		}
		t.Errorf("%s: the DDL does not reach the target:\n%sDDL:\n%s", name, b.String(), strings.Join(ddl, "\n"))
	}
	return ddl
}

func TestPlanEmptyForIdentical(t *testing.T) {
	ctx := start(t)
	base := mustCanonical(t, ctx, example(t))
	ddl, err := Plan(base.s, base.s, nil)
	if err != nil || len(ddl) != 0 {
		t.Errorf("ddl %v, err %v", ddl, err)
	}
	if ch := diff.Compare(base.s, base.s); len(ch) != 0 {
		t.Errorf("changes %v", ch)
	}
}

func TestPlan(t *testing.T) {
	ctx := start(t)
	baseSQL := example(t)
	base := mustCanonical(t, ctx, baseSQL)
	cases := []struct {
		name, edit string
		back       string // declarations for the way back
		oneWay     bool
		want       []string // fragments the DDL must contain
	}{
		{name: "columns", edit: `
-- @migrate drop orders.note
ALTER TABLE customers ADD COLUMN nickname VARCHAR(50) DEFAULT 'anon';
ALTER TABLE customers ADD COLUMN score INT NOT NULL DEFAULT 0 AFTER email;
ALTER TABLE orders MODIFY COLUMN total DECIMAL(14,2) NOT NULL DEFAULT 1;
ALTER TABLE orders DROP COLUMN note;
`, back: "-- @migrate drop customers.nickname\n-- @migrate drop customers.score\n",
			want: []string{"ADD COLUMN `score` int NOT NULL DEFAULT '0' AFTER `email`", "MODIFY COLUMN `total` decimal(14,2) NOT NULL DEFAULT '1.00'", "DROP COLUMN `note`"}},
		{name: "keys and checks", edit: `
ALTER TABLE orders ADD INDEX orders_status_idx (status, customer_id);
ALTER TABLE orders DROP CHECK orders_total_check;
ALTER TABLE orders ADD CONSTRAINT orders_total_check CHECK (total > 0);
ALTER TABLE customers DROP INDEX customers_email_key;
ALTER TABLE customers ADD UNIQUE KEY customers_email_key (email, name);
`, want: []string{"ADD KEY `orders_status_idx` (`status`,`customer_id`)", "DROP CHECK `orders_total_check`", "ADD CONSTRAINT `orders_total_check` CHECK ((`total` > 0))", "DROP INDEX `customers_email_key`", "ADD UNIQUE KEY `customers_email_key` (`email`,`name`)"}},
		{name: "foreign key and new table", edit: `
CREATE TABLE regions (id INT PRIMARY KEY, name VARCHAR(50) NOT NULL);
ALTER TABLE customers ADD COLUMN region_id INT NULL;
ALTER TABLE customers ADD CONSTRAINT fk_customers_region FOREIGN KEY (region_id) REFERENCES regions (id) ON DELETE SET NULL;
ALTER TABLE orders DROP FOREIGN KEY fk_orders_customer;
ALTER TABLE orders ADD CONSTRAINT fk_orders_customer FOREIGN KEY (customer_id) REFERENCES customers (id) ON DELETE CASCADE;
`, back: "-- @migrate drop regions\n-- @migrate drop customers.region_id\n",
			want: []string{"CREATE TABLE `regions`", "ADD CONSTRAINT `fk_customers_region` FOREIGN KEY (`region_id`) REFERENCES `regions` (`id`) ON DELETE SET NULL", "DROP FOREIGN KEY `fk_orders_customer`", "ON DELETE CASCADE"}},
		{name: "renames", edit: `
-- @migrate rename orders.note -> orders.memo
-- @migrate rename customers -> clients
ALTER TABLE orders RENAME COLUMN note TO memo;
RENAME TABLE customers TO clients;
`, back: "-- @migrate rename orders.memo -> orders.note\n-- @migrate rename clients -> customers\n",
			want: []string{"RENAME TABLE `customers` TO `clients`", "ALTER TABLE `orders` RENAME COLUMN `note` TO `memo`"}},
		{name: "table options and position", edit: `
ALTER TABLE customers COMMENT='people who buy';
ALTER TABLE orders MODIFY COLUMN note TEXT AFTER id;
`, want: []string{"ALTER TABLE `customers` COMMENT='people who buy'", "MODIFY COLUMN `note` text AFTER `id`"}},
		{name: "enum label and backfill", edit: `
-- @migrate enum orders.status: drop 'cancelled' using 'pending'
-- @migrate backfill customers.tier = 'basic'
ALTER TABLE orders MODIFY COLUMN status ENUM('pending', 'paid') NOT NULL DEFAULT 'pending';
ALTER TABLE customers ADD COLUMN tier VARCHAR(10) NOT NULL DEFAULT '';
`, oneWay: true,
			want: []string{"UPDATE `orders` SET `status` = 'pending' WHERE `status` = 'cancelled'", "MODIFY COLUMN `status` enum('pending','paid') NOT NULL DEFAULT 'pending'", "UPDATE `customers` SET `tier` = 'basic'"}},
		{name: "views", edit: `
CREATE VIEW paid_orders AS SELECT id, customer_id, total FROM orders WHERE status = 'paid';
`, want: []string{"CREATE ALGORITHM=UNDEFINED SQL SECURITY DEFINER VIEW `paid_orders`"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			to := mustCanonical(t, ctx, baseSQL+"\n"+c.edit)
			// the declarations are in the edit, which the canonical text lacks: parse them
			// from the edited source
			ddl := plan(t, ctx, c.name, base, to)
			joined := strings.Join(ddl, "\n")
			for _, w := range c.want {
				if !strings.Contains(joined, w) {
					t.Errorf("%s: DDL lacks %q:\n%s", c.name, w, joined)
				}
			}
			if c.oneWay {
				return
			}
			backSQL := c.back + baseSQL
			back := mustCanonical(t, ctx, backSQL)
			back.intents, _ = ParseIntents(backSQL)
			// the way back starts from the edited state
			from := canonical{s: to.s, text: to.text}
			plan(t, ctx, c.name+" (back)", from, back)
		})
	}
}

// A view whose definition changes is replaced in place; one the target lacks is dropped.
func TestPlanViews(t *testing.T) {
	ctx := start(t)
	baseSQL := example(t)
	from := mustCanonical(t, ctx, baseSQL+"\nCREATE VIEW paid_orders AS SELECT id FROM orders WHERE status = 'paid';\nCREATE VIEW gone AS SELECT 1 AS x;\n")
	to := mustCanonical(t, ctx, baseSQL+"\nCREATE VIEW paid_orders AS SELECT id, total FROM orders WHERE status = 'paid';\n")
	ddl := plan(t, ctx, "views", from, to)
	joined := strings.Join(ddl, "\n")
	for _, w := range []string{"DROP VIEW `gone`", "CREATE OR REPLACE ALGORITHM=UNDEFINED SQL SECURITY DEFINER VIEW `paid_orders`"} {
		if !strings.Contains(joined, w) {
			t.Errorf("DDL lacks %q:\n%s", w, joined)
		}
	}
}

func TestPlanProblems(t *testing.T) {
	ctx := start(t)
	baseSQL := example(t)
	base := mustCanonical(t, ctx, baseSQL)
	cases := []struct{ name, edit, want string }{
		{"undeclared column drop", "ALTER TABLE orders DROP COLUMN note;", "column orders.note is dropped, which no @migrate declares"},
		{"undeclared table drop", "DROP TABLE orders;", "table orders is dropped, which no @migrate declares"},
		{"stale drop", "-- @migrate drop orders.note", "orders.note still exists in the target schema"},
		{"stale rename", "-- @migrate rename customers.name -> customers.full_name", "customers.full_name is not in the target schema"},
		{"enum label kept", "-- @migrate enum orders.status: drop 'paid' using 'pending'", "'paid' is still a label in the target schema"},
		{"backfill error", "-- @migrate backfill orders.total = nope\nALTER TABLE orders MODIFY COLUMN note TEXT COMMENT 'x';", "backfill orders.total: Unknown column 'nope'"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			text := baseSQL + "\n" + c.edit
			s, _, err := (dump.Local{}).Canonical(ctx, text)
			if err != nil {
				t.Fatal(err)
			}
			in, _ := ParseIntents(text)
			_, err = Plan(base.s, s, in)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("%s: err = %v, want %q", c.name, err, c.want)
			}
		})
	}
}

func TestParseIntents(t *testing.T) {
	in, err := ParseIntents("-- @migrate rename a.b -> a.c\n-- @migrate drop t\n-- @migrate enum o.s: drop 'x' using 'y'\n-- @migrate backfill o.c = 1 where c is null\n")
	if err != nil || len(in) != 4 {
		t.Fatalf("%v %v", in, err)
	}
	want := []string{"rename a.b -> a.c", "drop t", "enum o.s: drop 'x' using 'y'", "backfill o.c = 1 where c is null"}
	for i, w := range want {
		if in[i].String() != w {
			t.Errorf("%d: %q", i, in[i].String())
		}
	}
	if _, err := ParseIntents("-- @migrate enum s: drop 'x' using 'y'\n-- @migrate frobnicate x"); err == nil || !strings.Contains(err.Error(), "enum needs table.column") || !strings.Contains(err.Error(), "unknown @migrate") {
		t.Errorf("%v", err)
	}
}
