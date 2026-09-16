package mysqlast

import (
	"fmt"
	"strings"
)

// Mergeable reports whether the server merges a derived table or view of this query
// expression into the outer query (Query_expression::is_mergeable and merge_heuristic):
// a single SELECT over at least one table, without GROUP BY, HAVING, DISTINCT, LIMIT,
// window functions, or a subquery in its select list. What is not merged is
// materialized into a temporary table, whose columns are typed by analyze's materialized.
func Mergeable(v Value) bool {
	qe, ok := v.(*Node)
	if !ok || qe.Class != "PT_query_expression" {
		return false
	}
	if qe.Arg("limit") != nil {
		return false
	}
	body, ok := qe.Arg("body").(*Node)
	if !ok {
		return false
	}
	if body.Class == "PT_query_expression" {
		return Mergeable(body)
	}
	if body.Class != "PT_query_specification" {
		return false
	}
	from, _ := body.Arg("from_clause").(List)
	if len(from) == 0 || body.Arg("opt_group_clause") != nil || body.Arg("opt_having_clause") != nil || body.Arg("opt_window_clause") != nil {
		return false
	}
	if strings.Contains(fmt.Sprint(body.Arg("options")), "SELECT_DISTINCT") {
		return false
	}
	items, _ := body.Arg("item_list").(List)
	for _, item := range items {
		if ContainsClass(item, "PT_window") || ContainsClass(item, "PT_subquery") {
			return false
		}
	}
	return true
}

// ContainsClass reports whether a node of the class occurs in v.
func ContainsClass(v Value, class string) bool {
	switch x := v.(type) {
	case *Node:
		if x.Class == class {
			return true
		}
		for _, a := range x.Args {
			if ContainsClass(a, class) {
				return true
			}
		}
	case List:
		for _, e := range x {
			if ContainsClass(e, class) {
				return true
			}
		}
	case *Struct:
		for _, e := range x.Fields {
			if ContainsClass(e, class) {
				return true
			}
		}
	}
	return false
}
