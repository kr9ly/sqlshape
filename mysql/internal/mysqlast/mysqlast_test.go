package mysqlast

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/kr9ly/sqlshape/mysql/internal/mysqlparse"
	"github.com/kr9ly/sqlshape/mysql/internal/parsegen"
)

func mustBuild(t *testing.T, sql string) Value {
	t.Helper()
	root, err := mysqlparse.Parse(sql, 0)
	if err != nil {
		t.Fatal(err)
	}
	v, err := Build(sql, root)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestSelect(t *testing.T) {
	v := mustBuild(t, "SELECT a, `b` AS c FROM t WHERE d = 1 ORDER BY a LIMIT 10")
	got := Sprint(v)
	for _, want := range []string{
		"PT_select_stmt(qe=PT_query_expression(body=PT_query_specification(",
		"item_list=[PTI_expr_with_alias(expr=PTI_simple_ident_ident(ident=a), alias=nil)",
		`PTI_expr_with_alias(expr=PTI_simple_ident_ident(ident=IDENT_QUOTED="b"), alias=c)`,
		"from_clause=[PT_table_factor_table_ident(table_ident=Table_ident(table=t)",
		"opt_where_clause=PTI_where(expr=PTI_comp_op(left=PTI_simple_ident_ident(ident=d), boolfunc2creator==, right=Item_int(",
		"order=PT_order(order_list=[PT_order_expr(item=PTI_simple_ident_ident(ident=a), dir=ORDER_NOT_RELEVANT)])",
		"limit=PT_limit_clause(limit_options={limit: Item_uint(",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in\n%s", want, got)
		}
	}
}

func TestInsertUpdateDelete(t *testing.T) {
	for sql, wants := range map[string][]string{
		"INSERT INTO t (a, b) VALUES (1, 'x'), (2, 'y')": {"PT_insert(is_replace=false, ", "table_ident=Table_ident(table=t)", "column_list=[PTI_simple_ident_nospvar_ident(ident=a), PTI_simple_ident_nospvar_ident(ident=b)]", "row_value_list=[[Item_int(", "PTI_text_literal_text_string("},
		"UPDATE t SET a = a + 1, b = ? WHERE id = 3":     {"PT_update(", "value_list=[Item_func_plus(a=PTI_simple_ident_ident(ident=a), b=Item_int(i=1)), Item_param(28)]", "opt_where_clause=PTI_where(expr=PTI_comp_op(left=PTI_simple_ident_ident(ident=id), boolfunc2creator==, right=Item_int(i=3)))"},
		"DELETE FROM t WHERE id IN (SELECT id FROM u)":   {"PT_delete(", "Item_in_subselect(left_expr=PTI_simple_ident_ident(ident=id), pt_subquery=PT_subquery(query_expression=PT_query_expression("},
	} {
		got := Sprint(mustBuild(t, sql))
		for _, want := range wants {
			if !strings.Contains(got, want) {
				t.Errorf("%s: missing %q in\n%s", sql, want, got)
			}
		}
	}
}

// TestCorpus measures how much of mysql-test/t builds: the statements the parser accepts
// that fold without an Unsupported construct, and which constructs are missing most.
func TestCorpus(t *testing.T) {
	home, _ := os.UserHomeDir()
	dir := filepath.Join(home, ".cache", "sqlshape", "mysql-server", "mysql-test", "t")
	if _, err := os.Stat(dir); err != nil {
		t.Skip("no MySQL source at", dir)
	}
	stmts, _, err := parsegen.SplitTestDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	roots := map[string]bool{"select_stmt": true, "insert_stmt": true, "replace_stmt": true, "update_stmt": true, "delete_stmt": true,
		"create_table_stmt": true, "create_index_stmt": true, "alter_table_stmt": true, "drop_table_stmt": true, "drop_index_stmt": true,
		"drop_view_stmt": true, "call_stmt": true}
	var parsed, built, dml, dmlBuilt int
	missing := map[string]int{}
	example := map[string]string{}
	for _, s := range stmts {
		if s.Expect != "" {
			continue
		}
		cst, err := mysqlparse.Parse(s.SQL, 0)
		if err != nil {
			continue
		}
		parsed++
		isDML := statementRule(cst, roots)
		if isDML {
			dml++
		}
		_, err = Build(s.SQL, cst)
		var u *Unsupported
		switch {
		case err == nil:
			built++
			if isDML {
				dmlBuilt++
			}
		case errors.As(err, &u):
			k := u.Rule + "/" + itoa(u.Alt)
			missing[k]++
			if example[k] == "" {
				example[k] = strings.ReplaceAll(u.Text, "\n", " ")
			}
		default:
			t.Fatalf("%s:%d: %v", s.File, s.Line, err)
		}
	}
	type kv struct {
		k string
		n int
	}
	var top []kv
	for k, n := range missing {
		top = append(top, kv{k, n})
	}
	sort.Slice(top, func(i, j int) bool { return top[i].n > top[j].n })
	t.Logf("built %d of %d parsed statements (%.1f%%); DML/DDL sqlshape reads: %d of %d (%.1f%%)", built, parsed, 100*float64(built)/float64(parsed), dmlBuilt, dml, 100*float64(dmlBuilt)/float64(max(dml, 1)))
	if rate := float64(dmlBuilt) / float64(max(dml, 1)); rate < 0.97 {
		t.Errorf("the statements sqlshape reads build at %.1f%%, below the 97%% held so far", rate*100)
	}
	for i, kv := range top {
		if i >= 40 {
			break
		}
		ex := example[kv.k]
		if len(ex) > 70 {
			ex = ex[:70]
		}
		t.Logf("  %5d  %-40s %s", kv.n, kv.k, ex)
	}
}

// statementRule reports whether the statement's rule is one sqlshape reads.
func statementRule(cst *mysqlparse.Node, roots map[string]bool) bool {
	found := false
	var walk func(n *mysqlparse.Node, depth int)
	walk = func(n *mysqlparse.Node, depth int) {
		if found || n.IsLeaf() || depth > 6 {
			return
		}
		if roots[n.Kind.String()] {
			found = true
			return
		}
		for _, c := range n.Children {
			walk(c, depth+1)
		}
	}
	walk(cst, 0)
	return found
}

func itoa(i int) string { return strconv.Itoa(i) }

func TestJoinAssociativity(t *testing.T) {
	// the server attaches `a JOIN b` at the leftmost table of `b JOIN c ON p`
	got := Sprint(mustBuild(t, "SELECT 1 FROM a JOIN b JOIN c ON b.x = c.x"))
	want := "PT_joined_table_on(tab1_node=PT_cross_join(tab1_node=PT_table_factor_table_ident(table_ident=Table_ident(table=a)"
	if !strings.Contains(got, want) {
		t.Errorf("missing %q in\n%s", want, got)
	}
}

func TestOperatorsAndRoot(t *testing.T) {
	v := mustBuild(t, "SELECT 1 FROM t WHERE a <> 1 AND b <=> 2")
	n, ok := v.(*Node)
	if !ok || n.Class != "PT_select_stmt" {
		t.Fatalf("root %T %v", v, Sprint(v))
	}
	got := Sprint(v)
	for _, want := range []string{"boolfunc2creator=<>", "boolfunc2creator=<=>", "Item_cond_and(i1=PTI_comp_op(", "), i2=PTI_comp_op("} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in\n%s", want, got)
		}
	}
}

func TestCheckEnforcement(t *testing.T) {
	got := Sprint(mustBuild(t, "CREATE TABLE t (a INT CHECK (a > 0) NOT ENFORCED)"))
	if !strings.Contains(got, "PT_check_constraint_column_attr(name=nil, expr=PTI_comp_op(") || !strings.Contains(got, "enforced=false)") {
		t.Errorf("enforcement not folded onto the CHECK:\n%s", got)
	}
}

func TestUnknownAlterAlgorithm(t *testing.T) {
	sql := "ALTER TABLE t ADD COLUMN b INT, ALGORITHM=SIDEWAYS"
	root, err := mysqlparse.Parse(sql, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, err = Build(sql, root)
	var u *Unsupported
	if !errors.As(err, &u) || u.Rule != "alter_algorithm_option_value" {
		t.Errorf("want Unsupported for an unknown ALGORITHM, got %v", err)
	}
}
