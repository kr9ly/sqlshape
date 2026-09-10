package schema

import (
	"strings"
	"testing"

	"github.com/kr9ly/sqlshape/check/mysql/v2/internal/mysqlast"
)

const sample = `-- sqlshape: mysql 8.4
CREATE TABLE customers (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  email VARCHAR(255) NOT NULL,
  name VARCHAR(100) COLLATE utf8mb4_bin,
  status ENUM('active', 'closed') NOT NULL DEFAULT 'active',
  balance DECIMAL(12, 2) NOT NULL DEFAULT 0,
  notes TEXT,
  created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  updated_at TIMESTAMP NULL ON UPDATE CURRENT_TIMESTAMP,
  full_name VARCHAR(200) GENERATED ALWAYS AS (concat(name, '!')) STORED,
  UNIQUE KEY uq_email (email),
  KEY ix_status_created (status, created_at DESC),
  CHECK (balance >= 0)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='people';

CREATE TABLE orders (
  id INT NOT NULL AUTO_INCREMENT,
  customer_id BIGINT UNSIGNED NOT NULL,
  amount DECIMAL(12,2),
  PRIMARY KEY (id),
  CONSTRAINT fk_orders_customer FOREIGN KEY (customer_id) REFERENCES customers (id) ON DELETE CASCADE
);

CREATE INDEX ix_orders_amount ON orders (amount);
CREATE VIEW big_orders (order_id, amt) AS SELECT id, amount FROM orders WHERE amount > 100;
ALTER TABLE orders ADD COLUMN note VARCHAR(20) NULL AFTER customer_id, MODIFY amount DECIMAL(14,2) NOT NULL, DROP INDEX ix_orders_amount;
ALTER TABLE customers RENAME COLUMN notes TO remarks;
RENAME TABLE orders TO purchases;
`

func TestLoad(t *testing.T) {
	s, err := Load(sample)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range s.Problems {
		t.Errorf("problem: %s", p)
	}
	if s.Version != "8.4" {
		t.Errorf("version %q", s.Version)
	}
	c := s.Table("customers")
	if c == nil {
		t.Fatal("no customers")
	}
	cols := map[string]*Column{}
	var names []string
	for _, col := range c.Columns {
		cols[col.Name] = col
		names = append(names, col.Name)
	}
	if got := strings.Join(names, ","); got != "id,email,name,status,balance,remarks,created_at,updated_at,full_name" {
		t.Errorf("columns %s", got)
	}
	if id := cols["id"]; id.Type.String() != "bigint unsigned" || !id.NotNull || !id.AutoIncrement {
		t.Errorf("id: %+v", id)
	}
	if e := cols["email"]; e.Type.String() != "varchar(255)" || !e.NotNull {
		t.Errorf("email: %+v", e)
	}
	if n := cols["name"]; n.Collation != "utf8mb4_bin" || n.NotNull {
		t.Errorf("name: %+v", n)
	}
	if st := cols["status"]; st.Type.String() != "enum('active','closed')" || mysqlast.Sprint(st.Default) != `PTI_text_literal_text_string(is_7bit=YYTHD->m_parser_state->m_lip.text_string_is_7bit(), literal=TEXT_STRING="active")` {
		t.Errorf("status: %s default %s", st.Type, mysqlast.Sprint(st.Default))
	}
	if b := cols["balance"]; b.Type.String() != "decimal(12,2)" {
		t.Errorf("balance: %s", b.Type)
	}
	if r := cols["remarks"]; r.Type.String() != "text" {
		t.Errorf("remarks: %s", r.Type)
	}
	if ca := cols["created_at"]; ca.Type.String() != "datetime(6)" || mysqlast.Sprint(ca.Default) != "Item_func_now_local(dec=6)" {
		t.Errorf("created_at: %s default %s", ca.Type, mysqlast.Sprint(ca.Default))
	}
	if u := cols["updated_at"]; u.Type.String() != "timestamp" || u.NotNull || !u.OnUpdate {
		t.Errorf("updated_at: %+v", u)
	}
	if f := cols["full_name"]; f.Generated == nil || !f.Stored {
		t.Errorf("full_name: %+v", f)
	}
	var keys []string
	for _, k := range c.Keys {
		var parts []string
		for _, p := range k.Parts {
			parts = append(parts, p.Column+map[bool]string{true: " DESC", false: ""}[p.Desc])
		}
		keys = append(keys, k.Kind.String()+" "+k.Name+"("+strings.Join(parts, ",")+")")
	}
	if got := strings.Join(keys, "; "); got != "PRIMARY KEY PRIMARY(id); UNIQUE uq_email(email); INDEX ix_status_created(status,created_at DESC)" {
		t.Errorf("keys: %s", got)
	}
	if len(c.Checks) != 1 || !c.Checks[0].Enforced {
		t.Errorf("checks: %+v", c.Checks)
	}
	if c.Engine != "InnoDB" || c.Charset != "utf8mb4" || c.Comment != "people" {
		t.Errorf("options: %s %s %s", c.Engine, c.Charset, c.Comment)
	}

	p := s.Table("purchases")
	if p == nil || s.Table("orders") != nil {
		t.Fatal("RENAME TABLE did not take")
	}
	names = names[:0]
	for _, col := range p.Columns {
		names = append(names, col.Name)
	}
	if got := strings.Join(names, ","); got != "id,customer_id,note,amount" {
		t.Errorf("purchases columns %s", got)
	}
	if a := p.Column("amount"); a.Type.String() != "decimal(14,2)" || !a.NotNull {
		t.Errorf("amount after MODIFY: %+v", a)
	}
	if len(p.Keys) != 1 || p.Keys[0].Kind != Primary {
		t.Errorf("purchases keys after DROP INDEX: %+v", p.Keys)
	}
	if len(p.ForeignKeys) != 1 || p.ForeignKeys[0].RefTable != "customers" || p.ForeignKeys[0].OnDelete != "CASCADE" || p.ForeignKeys[0].Columns[0] != "customer_id" || p.ForeignKeys[0].RefColumns[0] != "id" {
		t.Errorf("fk: %+v", p.ForeignKeys)
	}
	v := s.View("big_orders")
	if v == nil || strings.Join(v.Columns, ",") != "order_id,amt" || v.Query == nil || v.CheckOption != "NONE" {
		t.Errorf("view: %+v", v)
	}
}

func TestProblems(t *testing.T) {
	s, err := Load("CREATE TABLE t (a INT);\nCREATE TABLE t (b INT);\nALTER TABLE nope ADD COLUMN x INT;\nSELECT 1 +;\nCREATE TABLE u (a INT PRIMARY KEY) PARTITION BY HASH (a) PARTITIONS 2;\nDROP TABLE IF EXISTS gone;\n")
	if err != nil {
		t.Fatal(err)
	}
	var msgs []string
	for _, p := range s.Problems {
		msgs = append(msgs, p.Message)
	}
	want := []string{"CREATE TABLE t: table already exists", "no such table: nope", "syntax error"}
	if len(msgs) != len(want) {
		t.Fatalf("problems: %q", msgs)
	}
	for i := range want {
		if !strings.HasPrefix(msgs[i], want[i]) {
			t.Errorf("problem %d: %q, want %q", i, msgs[i], want[i])
		}
	}
	if u := s.Table("u"); u == nil || !u.Partitioned {
		t.Error("partitioned table")
	}
}

func TestDeclaredVersion(t *testing.T) {
	if v, _ := DeclaredVersion("CREATE TABLE t (a INT)"); v != "8.4" {
		t.Errorf("default %s", v)
	}
	if _, err := DeclaredVersion("-- sqlshape: mysql 9.1\n"); err == nil {
		t.Error("9.1 should be refused for now")
	}
}
