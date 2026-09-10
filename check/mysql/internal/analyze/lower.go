package analyze

import (
	"fmt"

	"github.com/kr9ly/sqlshape/check/mysql/v2/internal/mysqlast"
	"github.com/kr9ly/sqlshape/check/mysql/v2/internal/mysqlparse"
	"github.com/kr9ly/sqlshape/check/mysql/v2/internal/schema"
	"github.com/kr9ly/sqlshape/v2/x/facts"
	"github.com/kr9ly/sqlshape/v2/x/placeholder"
)

// Lower reads a declared predicate over t in the facts language: the expression is parsed
// and typed against the table alone (an error is the declaration's, positioned in it), and
// each conjunct becomes a facts.Pred whose ColRef.Leaf is 0, standing for the subject. This
// is x/obligation's Lowerer for MySQL.
func Lower(s *schema.Schema, expr string, t *schema.Table) ([]facts.Pred, error) {
	prefix := "SELECT 1 FROM `" + t.Name + "` WHERE "
	text, ph := placeholder.Rewrite(prefix + expr)
	cst, err := mysqlparse.Parse(text, 0)
	if err != nil {
		if pe, ok := err.(*mysqlparse.Error); ok {
			return nil, &Error{Message: pe.Message, Code: 1064, Position: max(ph.Back(pe.Offset)-len(prefix), 0)}
		}
		return nil, err
	}
	root, err := mysqlast.Build(text, cst)
	if err != nil {
		return nil, err
	}
	where := whereOf(root)
	if where == nil {
		return nil, fmt.Errorf("analyze: one boolean expression expected")
	}
	a := &analyzer{s: s, text: text, ph: ph, params: make([]Param, ph.Count())}
	sc := scope{rels: []relation{{alias: t.Name, table: t}}}
	if err := a.condition(sc, where, "where clause"); err != nil {
		if e, ok := err.(*Error); ok && e.Position >= len(prefix) {
			e.Position -= len(prefix) // positions are the declaration's, not the wrapper's
		}
		return nil, err
	}
	fs := &facts.Scope{}
	for _, c := range conjuncts(where) {
		a.predFacts(&sc, fs, c, nil)
	}
	return fs.Preds, nil
}

// whereOf digs the WHERE expression out of a parsed `SELECT ... WHERE <expr>`.
func whereOf(root mysqlast.Value) mysqlast.Value {
	n, ok := root.(*mysqlast.Node)
	if !ok || n.Class != "PT_select_stmt" {
		return nil
	}
	qe, ok := n.Arg("qe").(*mysqlast.Node)
	if !ok {
		return nil
	}
	body, ok := qe.Arg("body").(*mysqlast.Node)
	if !ok || body.Class != "PT_query_specification" {
		return nil
	}
	w := body.Arg("opt_where_clause")
	if wn, ok := w.(*mysqlast.Node); ok && wn.Class == "PTI_where" {
		return wn.Arg("expr")
	}
	return w
}

// ViewResult is a view's defining query analyzed on its own: its output columns, the
// base column each one passes through unchanged (empty otherwise), and its facts for the
// obligations the schema declares.
type ViewResult struct {
	Columns []Column
	Sources []ViewSource
	Facts   *facts.Facts
}

// ViewSource is the base column a view's output column is a plain reference to.
type ViewSource struct {
	Table, Column string
}

// AnalyzeView types v's query against s, the way the server does at CREATE VIEW.
func AnalyzeView(s *schema.Schema, v *schema.View) (*ViewResult, error) {
	_, ph := placeholder.Rewrite(v.Definition)
	a := &analyzer{s: s, text: v.Definition, ph: ph, facts: &facts.Facts{Kind: facts.Select}, definingView: v.Name}
	cols, body, err := a.view(v)
	if err != nil {
		return nil, err
	}
	res := &ViewResult{Columns: cols, Facts: a.facts}
	res.Facts.Top = body
	res.Facts.Uses = a.uses
	for _, c := range cols {
		src := ViewSource{}
		if c.base != nil && c.baseTable != nil {
			src = ViewSource{Table: c.baseTable.Name, Column: c.base.Name}
		}
		res.Sources = append(res.Sources, src)
	}
	return res, nil
}
