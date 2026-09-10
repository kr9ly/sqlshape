package pgtest_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kr9ly/sqlshape/pgtest/v2"
	"github.com/kr9ly/sqlshape/v2"
)

type verifyOrderRow struct {
	ID    int64
	Total string
	Note  *string
}

var verifyListOrders = sqlshape.Query[verifyOrderRow, struct {
	Min  *string
	Sort string
}](`
SELECT o.id, o.total, o.note FROM orders o
 WHERE true {{if .Min}} AND o.total >= {{.Min}} {{end}}
 ORDER BY {{if eq .Sort "total"}} o.total {{else}} o.id {{end}}`)

var verifyOrderByID = sqlshape.One[verifyOrderRow, struct{ ID int64 }](`SELECT id, total, note FROM orders WHERE id = {{.ID}}`)

// a statement PG rejects: both sides must say so (same SQLSTATE class)
var badColumn = sqlshape.Query[verifyOrderRow, struct{}](`SELECT id, total, nope FROM orders`)

func TestVerify(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	schema, err := os.ReadFile("../check/postgres/analyze/testdata/schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	db, err := pgtest.Start(ctx, string(schema))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Verify(ctx, verifyListOrders, verifyOrderByID, badColumn); err != nil {
		t.Fatal(err)
	}
	// a template that does not expand is reported as such
	err = db.Verify(ctx, sqlshape.Query[verifyOrderRow, struct{}](`SELECT {{if}} FROM orders`))
	if err == nil || !strings.Contains(err.Error(), "template") {
		t.Errorf("bad template: %v", err)
	}
}

// TestStartAppliesSchema confirms that a schema PostgreSQL itself rejects fails Start
// before any test gets a broken database to run against.
func TestStartAppliesSchema(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if _, err := pgtest.Start(ctx, `CREATE TABLE t (id bigint PRIMARY KEY,,,);`); err == nil {
		t.Fatal("starting with a schema PostgreSQL rejects should fail")
	}
}

// TestVerifySchemaProblem confirms that a schema problem the loader finds (not PostgreSQL:
// a directive comment is invisible to it) is reported by Verify, by name, before any
// statement is checked.
func TestVerifySchemaProblem(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	// PostgreSQL applies this schema fine: the directive comment is just a comment to it.
	// The loader reads it as a table directive and does not recognize this one.
	db, err := pgtest.Start(ctx, `
-- sqlshape: not a real directive
CREATE TABLE widgets (id bigint PRIMARY KEY);`)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	err = db.Verify(ctx)
	if err == nil || !strings.Contains(err.Error(), "unknown directive") {
		t.Errorf("schema problem: %v", err)
	}
}
