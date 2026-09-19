package analyze

import (
	"fmt"
	"strings"

	"github.com/kr9ly/sqlshape/check/mysql/v2/internal/mysqlast"
	"github.com/kr9ly/sqlshape/check/mysql/v2/internal/schema"
	"github.com/kr9ly/sqlshape/v2/x/facts"
)

// LOCK TABLES, UNLOCK TABLES, LOAD DATA and SELECT ... INTO OUTFILE / DUMPFILE: statements
// a program runs (Exec) that read or write tables without a result set. Inside a trigger
// or routine body the first three are refused by the server's grammar at CREATE time
// (body.go, 1314); OUTFILE is allowed there.

// lockTables types LOCK TABLES: every table must exist (1146, measured; a view may be
// locked), and one table may not be named twice without distinct aliases (1066, measured).
// The statement has no columns and no parameters; its facts are a read of every table it
// names, so the relations it touches are reported.
func (a *analyzer) lockTables(n *mysqlast.Node) error {
	a.facts = &facts.Facts{Kind: facts.Select, Top: &facts.Scope{At: -1}}
	seen := map[string]bool{}
	items, _ := n.Arg("tables").(mysqlast.List)
	for _, item := range items {
		tl, ok := item.(*mysqlast.Node)
		if !ok || tl.Class != "table_lock" {
			return fmt.Errorf("analyze: LOCK TABLES item not understood: %s", mysqlast.Sprint(item))
		}
		rel, err := a.target(tl.Arg("table_ident"), tl.Arg("opt_table_alias"), nil)
		if err != nil {
			return err
		}
		rel.pos = nodeStart(tl)
		key := strings.ToLower(rel.alias)
		if seen[key] {
			return &Error{Message: fmt.Sprintf("Not unique table/alias: '%s'", rel.alias), Code: 1066, Position: a.ph.Back(rel.pos)}
		}
		seen[key] = true
		a.facts.Top.Leaves = append(a.facts.Top.Leaves, a.leafFacts(*rel))
	}
	return nil
}

// unlockTables types UNLOCK TABLES: nothing to resolve, no columns, no parameters.
func (a *analyzer) unlockTables(n *mysqlast.Node) error {
	a.facts = &facts.Facts{Kind: facts.Select, Top: &facts.Scope{At: -1}}
	return nil
}

// loadData types LOAD DATA [LOCAL] INFILE ... INTO TABLE t: an INSERT of the file's rows.
// The target must be a base table (a view is 1288 "The target table v of the LOAD is not
// updatable", measured), the column list resolves against it (1054, a `@var` in the list
// takes the field and assigns nothing), and each SET assignment types its expression
// against the column. The file's values are unknown, so every listed column may receive a
// NULL: for a NOT NULL column that is 1263 ("NULL supplied to NOT NULL column", measured;
// a SET of NULL is the INSERT's own 1048), the keys, foreign keys and checks the columns
// take part in are the INSERT's (1062 / 1452 / 3819, measured), IGNORE turns them into
// warnings and REPLACE deletes the colliding row first (measured: two rows affected), the
// same rules insertViolations applies to an INSERT -- except that a NOT NULL column the
// column list leaves out is not the INSERT's 1364: it takes its type's implicit default
// (measured: `(id)` alone loads, a = 0). The row count is the file's.
func (a *analyzer) loadData(n *mysqlast.Node) error {
	x, _ := mysqlast.AsPTLoadTable(n)
	rel, err := a.target(x.Table(), nil, nil)
	if err != nil {
		return err
	}
	if rel.table == nil {
		return &Error{Message: fmt.Sprintf("The target table %s of the LOAD is not updatable", rel.alias), Code: 1288, Position: a.ph.Back(nodeStart(x.Table()))}
	}
	rel.target = true
	rel.pos = nodeStart(x.Table())
	dup := str(x.OnDuplicate())
	w := &write{kind: facts.Insert, table: rel.table, ignore: strings.HasSuffix(dup, "IGNORE_DUP"), replace: strings.HasSuffix(dup, "REPLACE_DUP"), inserted: map[string]bool{}, query: true, load: true}
	a.write = w
	sc := scope{rels: []relation{*rel}}
	var targets []*schema.Column
	var values []facts.Term
	if cols, ok := x.OptFieldsOrVars().(mysqlast.List); ok && len(cols) > 0 {
		for _, c := range cols {
			if cn, ok := c.(*mysqlast.Node); ok && cn.Class == "Item_user_var_as_out_param" {
				continue // `@var`: the field goes to the variable, not a column
			}
			a.assigning = true
			col, err := a.targetColumn(rel, c, "field list")
			a.assigning = false
			if err != nil {
				return err
			}
			targets = append(targets, col)
		}
	} else {
		targets = rel.table.Columns
	}
	for _, c := range targets {
		w.inserted[c.Name] = true
		w.values = append(w.values, assignment{col: c, nullable: true, fromFile: true})
		values = append(values, facts.Term{Kind: facts.Known, Text: "?"})
	}
	setCols, _ := x.OptSetFields().(mysqlast.List)
	setExprs, _ := x.OptSetExprs().(mysqlast.List)
	for i, c := range setCols {
		if i >= len(setExprs) {
			break
		}
		a.assigning = true
		col, err := a.targetColumn(rel, c, "field list")
		a.assigning = false
		if err != nil {
			return err
		}
		as, err := a.assign(sc, rel.table, col, setExprs[i])
		if err != nil {
			return err
		}
		replaced := false
		for j := range w.values {
			if w.values[j].col == col {
				// SET overrides the field the list gave the column, if any
				w.values[j], values[j], replaced = as, a.storedTerm(sc, col, setExprs[i]), true
			}
		}
		if !replaced {
			w.inserted[col.Name] = true
			w.values = append(w.values, as)
			targets = append(targets, col)
			values = append(values, a.storedTerm(sc, col, setExprs[i]))
		}
	}
	a.facts = &facts.Facts{Kind: facts.Insert, Top: &facts.Scope{At: -1, Leaves: []facts.Leaf{a.leafFacts(*rel)}, Many: "LOAD DATA loads every row of the file"}}
	a.facts.Writes = []facts.Write{a.writeFacts(facts.Insert, rel, nil, targets, values)}
	if w.replace {
		a.facts.Writes = append(a.facts.Writes, facts.Write{Table: rel.table.Name, Kind: facts.Delete, Position: int32(a.ph.Back(rel.pos))})
	}
	return nil
}

// selectIntoFile reports whether a SELECT sends its rows to a file (INTO OUTFILE /
// DUMPFILE), trailing (the statement's own "into") or inside the query specification
// (opt_into1): the statement then returns no result set to the client.
func selectIntoFile(n *mysqlast.Node) bool {
	isFile := func(v mysqlast.Value) bool {
		d, ok := v.(*mysqlast.Node)
		return ok && (d.Class == "PT_into_destination_outfile" || d.Class == "PT_into_destination_dumpfile")
	}
	if isFile(n.Arg("into")) {
		return true
	}
	qe, ok := n.Arg("qe").(*mysqlast.Node)
	if !ok {
		return false
	}
	body, ok := qe.Arg("body").(*mysqlast.Node)
	return ok && body.Class == "PT_query_specification" && isFile(body.Arg("opt_into1"))
}
