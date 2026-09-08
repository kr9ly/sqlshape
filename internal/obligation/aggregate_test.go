package obligation_test

import (
	"strings"
	"testing"

	"github.com/kr9ly/sqlshape/internal/analyze"
	"github.com/kr9ly/sqlshape/internal/obligation"
)

const aggregateSchema = `
CREATE TABLE currencies (code text PRIMARY KEY);
-- sqlshape: aggregate orders (order_items, order_notes, order_item_tags)
CREATE TABLE orders (id bigint PRIMARY KEY, status text NOT NULL);
CREATE TABLE order_items (id bigint PRIMARY KEY, order_id bigint NOT NULL REFERENCES orders(id), currency text NOT NULL REFERENCES currencies(code), qty int NOT NULL);
CREATE TABLE order_notes (order_id bigint NOT NULL REFERENCES orders(id), seq int NOT NULL, body text, PRIMARY KEY (order_id, seq));
CREATE TABLE order_item_tags (item_id bigint NOT NULL REFERENCES order_items(id), tag text NOT NULL, PRIMARY KEY (item_id, tag));
-- sqlshape: aggregate invoices (invoice_lines)
CREATE TABLE invoices (id bigint PRIMARY KEY, order_id bigint NOT NULL REFERENCES orders(id));
CREATE TABLE invoice_lines (id bigint PRIMARY KEY, invoice_id bigint NOT NULL REFERENCES invoices(id), amount int NOT NULL);
CREATE VIEW order_billing AS SELECT o.id, l.amount FROM orders o JOIN invoices v ON v.order_id = o.id JOIN invoice_lines l ON l.invoice_id = v.id;
`

func TestAggregate(t *testing.T) {
	s, err := analyze.Load(aggregateSchema)
	if err != nil {
		t.Fatal(err)
	}
	decls, problems := obligation.Declarations(s)
	if len(problems) > 0 {
		t.Fatalf("problems: %+v", problems)
	}
	var specs []string
	for _, o := range decls {
		specs = append(specs, o.Subject+": "+o.Body.Spec())
	}
	// a grandchild pins its foreign key to the member it hangs off, not to the root
	wantSpecs := "orders: alone | order_items: pinned(order_id) | order_items: alone | order_notes: pinned(order_id) | order_notes: alone | order_item_tags: pinned(item_id) | order_item_tags: alone | invoices: alone | invoice_lines: pinned(invoice_id) | invoice_lines: alone"
	if got := strings.Join(specs, " | "); got != wantSpecs {
		t.Errorf("expansion:\n got %s\nwant %s", got, wantSpecs)
	}
	cases := []struct{ sql, want string }{
		// a child reached through its root: the join on the root's key pins order_id
		{`SELECT i.qty FROM orders o JOIN order_items i ON i.order_id = o.id WHERE o.id = $1`, `
aggregate orders statement
aggregate orders statement
aggregate orders statement`},
		// a child touched on its own, unpinned
		{`SELECT qty FROM order_items WHERE currency = 'JPY'`, `
aggregate orders FAIL order_items is a child of aggregate orders: reach it through orders (fix order_items.order_id by equality, or join on orders's key)
aggregate orders statement`},
		// a lookup table belongs to no aggregate and is free to join
		{`SELECT i.qty FROM order_items i JOIN currencies c ON c.code = i.currency WHERE i.order_id = $1`, `
aggregate orders statement
aggregate orders statement`},
		// two aggregates in one statement
		{`SELECT o.status, v.id FROM orders o JOIN invoices v ON v.order_id = o.id WHERE o.id = $1`, `
aggregate orders FAIL orders belongs to aggregate orders and this statement also touches invoices of aggregate invoices: one statement, one aggregate (read across aggregates through a view)
aggregate invoices FAIL invoices belongs to aggregate invoices and this statement also touches orders of aggregate orders: one statement, one aggregate (read across aggregates through a view)`},
		// ... which is what a view is for
		{`SELECT amount FROM order_billing WHERE id = $1`, ``},
		// a grandchild reached through its parent, itself reached through the root
		{`SELECT t.tag FROM orders o JOIN order_items i ON i.order_id = o.id JOIN order_item_tags t ON t.item_id = i.id WHERE o.id = $1`, `
aggregate orders statement
aggregate orders statement
aggregate orders statement
aggregate orders fk
aggregate orders statement`},
		// ... but a grandchild joined to a parent that is not itself reached through the root fails at the parent
		{`SELECT t.tag FROM order_items i JOIN order_item_tags t ON t.item_id = i.id WHERE i.qty > 1`, `
aggregate orders FAIL order_items is a child of aggregate orders: reach it through orders (fix order_items.order_id by equality, or join on orders's key)
aggregate orders statement
aggregate orders fk
aggregate orders statement`},
		// a write into a child assigns the root key
		{`INSERT INTO order_notes (order_id, seq, body) VALUES ($1, $2, $3)`, `
aggregate orders statement
aggregate orders statement`},
	}
	for _, c := range cases {
		r, err := analyze.Analyze(s, c.sql)
		if err != nil {
			t.Errorf("%s: %v", c.sql, err)
			continue
		}
		var lines []string
		for _, d := range obligation.Check(s, decls, r.Facts, lowerer{s}) {
			line := d.Obligation.Source + " " + pathName(d.Path)
			if d.Message != "" {
				line += " " + d.Message
			}
			lines = append(lines, line)
		}
		if got, want := strings.Join(lines, "\n"), strings.TrimPrefix(c.want, "\n"); got != want {
			t.Errorf("%s\n--- got ---\n%s\n--- want ---\n%s", c.sql, got, want)
		}
	}

	// a child without a foreign key to the root is a declaration problem
	bad, err := analyze.Load(aggregateSchema + "\n-- sqlshape: aggregate currencies (orders)\nCREATE TABLE dummy (id int);")
	if err != nil {
		t.Fatal(err)
	}
	_, problems = obligation.Declarations(bad)
	if len(problems) != 1 || !strings.Contains(problems[0].Message, "sits above dummy, not currencies") {
		t.Errorf("misplaced aggregate: %+v", problems)
	}
	bad, _ = analyze.Load(strings.Replace(aggregateSchema, "aggregate orders (order_items, order_notes, order_item_tags)", "aggregate orders (order_items, currencies)", 1))
	_, problems = obligation.Declarations(bad)
	if len(problems) != 1 || !strings.Contains(problems[0].Message, "currencies has no foreign key to orders or to another member") {
		t.Errorf("child without fk: %+v", problems)
	}
}
