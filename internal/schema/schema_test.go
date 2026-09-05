package schema

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kr9ly/sqlshape/internal/catalog"
	"github.com/kr9ly/sqlshape/internal/oracle"
)

func load(t *testing.T) *Schema {
	t.Helper()
	sql, err := os.ReadFile("testdata/schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	s, err := Load(string(sql))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range s.Problems {
		t.Errorf("problem: %s", p)
	}
	return s
}

func TestStructure(t *testing.T) {
	s := load(t)

	st := s.Types.Lookup("", "order_status")
	if st == nil || st.Kind != 'e' || len(s.Types.Enums[st.OID]) != 4 || s.Types.Enums[st.OID][1] != "paid" {
		t.Fatalf("enum: %+v %v", st, s.Types.Enums)
	}
	if arr := s.Types.ByOID(st.Array); arr == nil || !arr.IsArray() || arr.Elem != st.OID {
		t.Errorf("enum array: %+v", arr)
	}
	yen := s.Types.Lookup("", "yen")
	if yen == nil || yen.Kind != 'd' || yen.BaseType != catalog.Int8 || len(s.Types.Domains[yen.OID].Checks) != 1 {
		t.Errorf("domain: %+v", yen)
	}
	if em := s.Types.Lookup("", "email"); em == nil || !s.Types.Domains[em.OID].NotNull {
		t.Errorf("domain email not null")
	}
	ma := s.Relation("", "money_amount")
	if ma == nil || len(ma.Columns) != 2 || ma.Columns[1].Type.Typmod != 3+4 {
		t.Errorf("composite: %+v", ma)
	}

	orders := s.Relation("", "orders")
	if orders == nil || orders.Kind != Table {
		t.Fatal("orders")
	}
	id := orders.Column("id")
	if id.Type.OID != catalog.Int8 || !id.NotNull || id.Identity == 0 {
		t.Errorf("bigserial id: %+v", id)
	}
	if c := orders.Column("uid"); c.Default == nil || c.NotNull {
		t.Errorf("uid: %+v", c)
	}
	var kinds []string
	for _, c := range orders.Constraints {
		kinds = append(kinds, string(c.Kind)+":"+strings.Join(c.Columns, ","))
	}
	want := "p:id f:user_id c:total u:user_id,note u:uid"
	if got := strings.Join(kinds, " "); got != want {
		t.Errorf("orders constraints\n got %s\nwant %s", got, want)
	}
	if orders.Constraints[1].RefTable != "users" || orders.Constraints[1].RefColumns[0] != "id" {
		t.Errorf("fk: %+v", orders.Constraints[1])
	}
	if orders.Constraints[4].Predicate == nil {
		t.Error("partial unique index lost its predicate")
	}

	items := s.Relation("", "order_items")
	if d := items.Column("discount"); d == nil || !d.NotNull || s.Types.Format(d.Type) != "numeric(5,2)" {
		t.Errorf("altered column: %+v", d)
	}
	if items.Constraints[0].Kind != PrimaryKey || len(items.Constraints[0].Columns) != 2 || items.Constraints[1].Kind != ForeignKey {
		t.Errorf("order_items constraints: %+v", items.Constraints)
	}

	if v := s.Relation("", "order_summary"); v == nil || v.Kind != View || v.Query == nil {
		t.Error("view")
	}
	if mv := s.Relation("", "order_stats"); mv == nil || mv.Kind != MatView || mv.Query == nil {
		t.Error("matview")
	}

	fns := map[string]*Function{}
	for _, f := range s.Functions {
		fns[f.Name] = f
	}
	so := fns["save_order"]
	if so == nil || len(so.Args) != 2 || so.Args[0].Type.OID != ma.RowType || so.Volatile != 's' || !so.Strict || so.RetType.OID != catalog.Int8 {
		t.Errorf("save_order: %+v", so)
	}
	if a := s.Types.ByOID(so.Args[1].Type.OID); a == nil || !a.IsArray() || a.Elem != items.RowType {
		t.Errorf("order_items[]: %+v", a)
	}
	lt := fns["list_totals"]
	if lt == nil || !lt.RetSet || lt.RetType.OID != catalog.Record || len(lt.Args) != 3 || lt.Args[1].Mode != 't' {
		t.Errorf("list_totals: %+v", lt)
	}
	if ui := fns["user_ids"]; ui == nil || !ui.RetSet || ui.RetType.OID != catalog.Int8 {
		t.Errorf("user_ids: %+v", ui)
	}
	if s.Comments["orders.status"] == "" || s.Comments["orders"] == "" || s.Comments["type:order_status"] == "" {
		t.Errorf("comments: %v", s.Comments)
	}
}

// TestAgainstOracle loads the same schema into a real PG and compares, for every
// table, the column names / format_type spelling / NOT NULL with what PG reports.
func TestAgainstOracle(t *testing.T) {
	s := load(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	sql, _ := os.ReadFile("testdata/schema.sql")
	o, err := oracle.Start(ctx, string(sql))
	if err != nil {
		t.Fatalf("oracle: %v", err)
	}
	defer o.Close()

	for _, rel := range s.Relations {
		if rel.Kind != Table {
			continue
		}
		d, err := o.Describe(ctx, "SELECT * FROM "+rel.FullName())
		if err != nil {
			t.Fatalf("%s: %v", rel.Name, err)
		}
		var got, want []string
		for _, c := range rel.Columns {
			nn := "null"
			if c.NotNull {
				nn = "not null"
			}
			// PG describes domain columns by their base type on the wire.
			got = append(got, c.Name+" "+s.Types.Format(s.Types.BaseOf(c.Type))+" "+nn)
		}
		for _, c := range d.Columns {
			nn := "null"
			if c.Source.NotNull {
				nn = "not null"
			}
			want = append(want, c.Name+" "+c.Type.Name+" "+nn)
		}
		if g, w := strings.Join(got, "\n"), strings.Join(want, "\n"); g != w {
			t.Errorf("%s\n--- schema\n%s\n--- oracle\n%s", rel.Name, g, w)
		}
	}
}

func TestUnknownExtension(t *testing.T) {
	s, err := Load("CREATE EXTENSION citext; CREATE EXTENSION nope; CREATE TABLE t (h citext);")
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Problems) != 1 || !strings.Contains(s.Problems[0].Message, `extension "nope"`) {
		t.Fatalf("problems: %v", s.Problems)
	}
	if s.Relation("", "t").Columns[0].Type.OID < 1<<28 {
		t.Fatal("citext column should resolve to the extension's renumbered type")
	}
}
