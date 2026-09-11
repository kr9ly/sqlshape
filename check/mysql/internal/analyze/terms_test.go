package analyze

import (
	"testing"

	"github.com/kr9ly/sqlshape/check/mysql/v2/internal/schema"
	"github.com/kr9ly/sqlshape/v2/x/facts"
)

// `col = NULL` is never true: no equality fact, so nothing is pinned or proved single by it.
func TestNullEqualityIsNoFact(t *testing.T) {
	s, err := schema.Load("-- sqlshape: mysql 8.4\nCREATE TABLE orders (id INT PRIMARY KEY, tenant_id INT NOT NULL);")
	if err != nil {
		t.Fatal(err)
	}
	for _, sql := range []string{"SELECT id FROM orders WHERE tenant_id = NULL", "SELECT id FROM orders WHERE id = NULL"} {
		r, err := Analyze(s, sql)
		if err != nil {
			t.Fatal(err)
		}
		if n := len(r.Facts.Top.Fixed); n != 0 {
			t.Errorf("%s: %d fixed columns, want none (%+v)", sql, n, r.Facts.Top.Preds)
		}
	}
	r, _ := Analyze(s, "SELECT id FROM orders WHERE tenant_id = 1")
	if len(r.Facts.Top.Fixed) != 1 {
		t.Errorf("= 1: %+v", r.Facts.Top)
	}
}

// SET col = DEFAULT stores the column's default: a literal default is a Const value, any
// other default the expression DEFAULT.
func TestSetDefaultStoresTheLiteralDefault(t *testing.T) {
	s, err := schema.Load("-- sqlshape: mysql 8.4\nCREATE TABLE orders (id INT PRIMARY KEY, status VARCHAR(10) NOT NULL DEFAULT 'draft', n INT DEFAULT 3, ts DATETIME DEFAULT CURRENT_TIMESTAMP, e VARCHAR(10) DEFAULT (LOWER('A')));")
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Problems) > 0 {
		t.Fatal(s.Problems)
	}
	r, err := Analyze(s, "UPDATE orders SET status = DEFAULT, n = DEFAULT, ts = DEFAULT, e = DEFAULT WHERE id = 1")
	if err != nil {
		t.Fatal(err)
	}
	w := r.Facts.Writes[0]
	want := map[string]facts.Term{
		"status": {Kind: facts.Const, Const: "'draft'"},
		"n":      {Kind: facts.Const, Const: "3"},
		"ts":     {Kind: facts.Known, Text: "DEFAULT"},
		"e":      {Kind: facts.Known, Text: "DEFAULT"},
	}
	for i, col := range w.Assigned {
		if got := w.Values[i]; got != want[col] {
			t.Errorf("%s: %+v, want %+v", col, got, want[col])
		}
	}
	r, _ = Analyze(s, "INSERT INTO orders (id, status) VALUES (1, DEFAULT)")
	if v := r.Facts.Writes[0].Values[1]; v != want["status"] {
		t.Errorf("INSERT DEFAULT: %+v", v)
	}
}
