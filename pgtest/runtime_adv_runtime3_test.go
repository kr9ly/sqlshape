package pgtest_test

// Adversarial tests for lane "runtime", round 3 (postgres/v2 runtime over pgx). Each test
// below is reproduced against a real PostgreSQL (embedded-postgres via check/postgres/oracle)
// and is written to expect the documented / promised behavior, so it currently fails.

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/kr9ly/sqlshape/check/postgres/v2/oracle"
	"github.com/kr9ly/sqlshape/postgres/v2"
	"github.com/kr9ly/sqlshape/v2"
)

// B1: for a *domain* NOT NULL violation, PostgreSQL's error carries neither a
// ConstraintName, a TableName nor a ColumnName (verified against a running server:
// Code=23502, ConstraintName="", TableName="", ColumnName="" — contrast a plain column
// NOT NULL on the same server, which does carry TableName/ColumnName): the violation
// happens inside the domain's own type coercion, before the value reaches a column
// PostgreSQL can name, so a `<table>.<column>` key cannot be formed for it. Per the
// arbitrated fix, ConstraintError.Key() falls back to PgError.DataTypeName — the
// domain's own name — for this one class-23 case, matching the failure mode the checker
// predicts (docs/checks.md's constraint-name table names a domain NOT NULL by the
// domain, not by the table/column it decorates).
//
// check/postgres/analyze/testdata/schema.sql declares `CREATE DOMAIN code AS text; ...
// ALTER DOMAIN code SET NOT NULL;` and table `spans (... cd code ...)` with cd left
// nullable at the column level (the domain itself carries the NOT NULL). Inserting a row
// without cd violates the domain's NOT NULL (SQLSTATE 23502), the same SQLSTATE a plain
// column NOT NULL violation raises. postgres.Violates(err, "code") — the domain's name —
// is expected to match.
var advInsertSpanNoCode = sqlshape.Query[struct{}, struct{ ID int64 }](`INSERT INTO spans (id) VALUES ({{.ID}})`)

func TestAdvRuntime3_B1_DomainNotNullConstraintErrorKey(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	schema, err := os.ReadFile("../check/postgres/analyze/testdata/schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	o, err := oracle.Start(ctx, string(schema))
	if err != nil {
		t.Fatalf("oracle: %v", err)
	}
	defer o.Close()
	db := o.Conn()

	_, err = postgres.Exec(ctx, db, advInsertSpanNoCode, struct{ ID int64 }{1})
	if err == nil {
		t.Fatal("insert without the domain-typed NOT NULL column cd: want an error, got nil")
	}
	if !postgres.Violates(err, "code") {
		t.Errorf("domain NOT NULL violation: want Violates(err, %q) (the domain's own name, PgError has no table/column to name), got false (err: %v)", "code", err)
	}
}

// B2: the same case for a domain declared `NOT NULL` directly in its own CREATE DOMAIN
// (rather than added later with ALTER DOMAIN ... SET NOT NULL): `email AS text NOT NULL
// CHECK (...)` on users.email (nullable at the column level: `email email UNIQUE`, no
// column-level NOT NULL). Key() falls back to the domain's own name here too: "email".
var advInsertUserNoEmail = sqlshape.Query[struct{}, struct{ Name string }](`INSERT INTO users (name) VALUES ({{.Name}})`)

func TestAdvRuntime3_B2_DomainNotNullDeclaredInline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	schema, err := os.ReadFile("../check/postgres/analyze/testdata/schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	o, err := oracle.Start(ctx, string(schema))
	if err != nil {
		t.Fatalf("oracle: %v", err)
	}
	defer o.Close()
	db := o.Conn()

	_, err = postgres.Exec(ctx, db, advInsertUserNoEmail, struct{ Name string }{"nobody"})
	if err == nil {
		t.Fatal("insert without the domain-typed NOT NULL column email: want an error, got nil")
	}
	if !postgres.Violates(err, "email") {
		t.Errorf("domain NOT NULL violation: want Violates(err, %q) (the domain's own name), got false (err: %v)", "email", err)
	}
}
