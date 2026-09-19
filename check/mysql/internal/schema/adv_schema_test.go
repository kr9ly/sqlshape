package schema

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kr9ly/sqlshape/mysqltest/v2"
)

// Adversarial findings against a real mysqld (round 1, schema lane). Each test states the
// behavior the server actually has; today the loader disagrees (see the failure each one
// reports), which is exactly what these tests are here to pin down.

// TestAdvDropUnnamedForeignKeyByGeneratedName: MySQL accepts DROP FOREIGN KEY naming an
// unnamed constraint by the name it generates for it (`<table>_ibfk_<n>`), and the
// constraint is gone afterward. The loader's dropConstraint compares the ALTER's name
// against ForeignKey.Name, which is "" for an unnamed key until ForeignKeyName() renders
// it — so the DROP never matches, is recorded as a Problem, and the foreign key survives
// in the model although the server has removed it.
func TestAdvDropUnnamedForeignKeyByGeneratedName(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	sql := `CREATE TABLE p (id INT PRIMARY KEY);
CREATE TABLE c (id INT PRIMARY KEY, pid INT, FOREIGN KEY (pid) REFERENCES p(id));
ALTER TABLE c DROP FOREIGN KEY c_ibfk_1;`

	db, err := mysqltest.Start(ctx, sql)
	if errors.Is(err, mysqltest.ErrNoServer) {
		t.Skip("no mysqld on PATH (nix-shell -p mysql84)")
	}
	if err != nil {
		t.Fatalf("server: DROP FOREIGN KEY c_ibfk_1 (the name it would itself report for the "+
			"unnamed constraint) must succeed: %v", err)
	}
	defer db.Close()
	var count int
	if err := db.Conn().QueryRowContext(ctx,
		"SELECT COUNT(*) FROM information_schema.table_constraints WHERE table_schema='sqlshape' AND table_name='c' AND constraint_type='FOREIGN KEY'",
	).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("server: expected the foreign key gone after DROP FOREIGN KEY c_ibfk_1, still %d", count)
	}

	s, err := Load(sql)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range s.Problems {
		t.Errorf("loader: unexpected problem: %s", p)
	}
	c := s.Table("c")
	if c == nil {
		t.Fatal("loader: no table c")
	}
	if len(c.ForeignKeys) != 0 {
		t.Errorf("loader: expected the foreign key gone after DROP FOREIGN KEY c_ibfk_1, still have %+v", c.ForeignKeys)
	}
}

// TestAdvDropUnnamedCheckByGeneratedName: same defect on CHECK constraints. MySQL accepts
// DROP CHECK t_chk_1 for an unnamed CHECK, generating a fresh table for what remains where
// the surviving checks keep their own generated numbers (the counter does not reuse the
// freed slot); the loader neither drops the check nor reflects that renumbering.
func TestAdvDropUnnamedCheckByGeneratedName(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	sql := `CREATE TABLE t (a INT, b INT, c INT, CHECK (a > 0), CHECK (b > 0));
ALTER TABLE t DROP CHECK t_chk_1;
ALTER TABLE t ADD CHECK (c > 0);`

	db, err := mysqltest.Start(ctx, sql)
	if errors.Is(err, mysqltest.ErrNoServer) {
		t.Skip("no mysqld on PATH (nix-shell -p mysql84)")
	}
	if err != nil {
		t.Fatalf("server: DROP CHECK t_chk_1 (the name it would itself report for the first "+
			"unnamed check) must succeed: %v", err)
	}
	defer db.Close()
	rows, err := db.Conn().QueryContext(ctx,
		"SELECT constraint_name FROM information_schema.table_constraints WHERE table_schema='sqlshape' AND table_name='t' AND constraint_type='CHECK' ORDER BY constraint_name")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		names = append(names, n)
	}
	rows.Close()
	// the server's generator counter is not rewound by the DROP: b>0 keeps t_chk_2, and the
	// check added afterward becomes t_chk_3, not a reused t_chk_1.
	if len(names) != 2 || names[0] != "t_chk_2" || names[1] != "t_chk_3" {
		t.Fatalf("server: expected [t_chk_2 t_chk_3], got %v", names)
	}

	s, err := Load(sql)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range s.Problems {
		t.Errorf("loader: unexpected problem: %s", p)
	}
	tb := s.Table("t")
	if tb == nil {
		t.Fatal("loader: no table t")
	}
	var got []string
	for _, c := range tb.Checks {
		got = append(got, tb.CheckName(c))
	}
	if len(got) != 2 || got[0] != "t_chk_2" || got[1] != "t_chk_3" {
		t.Errorf("loader: expected [t_chk_2 t_chk_3] surviving, got %v", got)
	}
}

// TestAdvCaseSensitiveTableNames: on the platform default the loader is built and tested
// against (Linux, lower_case_table_names=0), MySQL table names are case-sensitive: `Foo`
// and `foo` are two distinct tables. The loader looks tables up with strings.EqualFold
// throughout (Schema.Table, createTable's duplicate check, needTable, ...), so the second
// CREATE TABLE is rejected as a duplicate of the first and one of the two tables silently
// disappears from the model.
func TestAdvCaseSensitiveTableNames(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	sql := `CREATE TABLE Foo (id INT PRIMARY KEY);
CREATE TABLE foo (id INT PRIMARY KEY);`

	db, err := mysqltest.Start(ctx, sql)
	if errors.Is(err, mysqltest.ErrNoServer) {
		t.Skip("no mysqld on PATH (nix-shell -p mysql84)")
	}
	if err != nil {
		t.Fatalf("server: Foo and foo must both be creatable as distinct tables under the "+
			"default lower_case_table_names=0: %v", err)
	}
	defer db.Close()
	var lower int
	if err := db.Conn().QueryRowContext(ctx, "SELECT @@lower_case_table_names").Scan(&lower); err != nil {
		t.Fatal(err)
	}
	if lower != 0 {
		t.Skipf("server has lower_case_table_names=%d, not the Linux default 0 this test assumes", lower)
	}
	var count int
	if err := db.Conn().QueryRowContext(ctx,
		"SELECT COUNT(*) FROM information_schema.tables WHERE table_schema='sqlshape' AND table_name IN ('Foo','foo')",
	).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("server: expected both Foo and foo to exist, information_schema has %d", count)
	}

	s, err := Load(sql)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range s.Problems {
		t.Errorf("loader: unexpected problem: %s", p)
	}
	if len(s.Tables) != 2 {
		t.Errorf("loader: expected 2 distinct tables (Foo, foo), got %d: %v", len(s.Tables), s.Tables)
	}
}
