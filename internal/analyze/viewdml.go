package analyze

import (
	"fmt"

	pg_query "github.com/pganalyze/pg_query_go/v6"

	"github.com/kr9ly/sqlshape/internal/catalog"
	"github.com/kr9ly/sqlshape/internal/schema"
)

// Writes through views. An automatically updatable view (view_query_is_auto_updatable:
// one table in FROM, no set operation / DISTINCT / GROUP BY / HAVING / LIMIT / OFFSET /
// WITH / window / aggregate / set-returning target) takes INSERT / UPDATE / DELETE /
// MERGE as if it were its base table; the write is typed and its violations computed
// against the base table's columns, seen under the view's names. A column the view
// computes (an expression) cannot be written. A view with INSTEAD OF triggers takes any
// write; anything else is PG's 55000.

// writeTarget is targetRTE for a statement that writes rv.
func (a *analyzer) writeTarget(rv *pg_query.RangeVar, sc *scope, cmd string) (*schema.Relation, *rte, *Error) {
	rel, r, err := a.targetRTE(rv, sc)
	if err != nil {
		return nil, nil, err
	}
	a.writeRecs = append(a.writeRecs, writeRec{rel: rel, r: r, cmd: cmd})
	switch rel.Kind {
	case schema.MatView:
		if cmd == "merge into" {
			return nil, nil, errAt(codeFeatureNotSupported, rv.Location, "cannot execute MERGE on relation %q", rel.Name)
		}
		return nil, nil, errAt(codeWrongObjectType, rv.Location, "cannot change materialized view %q", rel.Name)
	case schema.View:
		base, err := a.viewWriteTarget(rel, cmd, rv.Location)
		if err != nil {
			return nil, nil, err
		}
		a.writeCmd = cmd
		return base, r, nil
	}
	return rel, r, nil
}

// viewWriteTarget builds the relation a write through view rel lands on.
func (a *analyzer) viewWriteTarget(rel *schema.Relation, cmd string, loc int32) (*schema.Relation, *Error) {
	if syn, ok := a.viewTargets[rel]; ok {
		return syn, nil
	}
	vc, err := a.viewColumns(rel)
	if err != nil {
		return nil, err
	}
	if a.hasInsteadOfTrigger(rel, cmd) {
		syn := &schema.Relation{OID: rel.OID, Schema: rel.Schema, Name: rel.Name, Kind: rel.Kind, RowType: rel.RowType}
		for i, c := range vc {
			syn.Columns = append(syn.Columns, &schema.Column{Num: int16(i + 1), Name: c.name, Type: c.typ})
		}
		a.viewTargets[rel] = syn
		return syn, nil
	}
	// a conditional DO INSTEAD rule does not take the write, and the view can no longer be
	// auto-updated either
	if q := rel.QualifiedRules; q != nil {
		ev := map[string]string{"insert into": "insert", "update": "update", "delete from": "delete"}[cmd]
		if q[ev] || (cmd == "merge into" && (q["insert"] || q["update"] || q["delete"])) {
			return nil, errAt(codeObjectNotInPrerequisiteState, loc, "cannot %s view %q", cmd, rel.Name)
		}
	}
	baseRV := autoUpdatableBase(a, rel.Query.GetSelectStmt())
	if baseRV == nil {
		return nil, errAt(codeObjectNotInPrerequisiteState, loc, "cannot %s view %q", cmd, rel.Name)
	}
	baseRel := a.s.Relation(baseRV.Schemaname, baseRV.Relname)
	if baseRel == nil {
		return nil, errAt(codeUndefinedTable, loc, "relation %q does not exist", qualName(baseRV))
	}
	fromName := baseRel.FullName() // what the view's columns cite as their source
	if baseRel.Kind == schema.View {
		if baseRel, err = a.viewWriteTarget(baseRel, cmd, loc); err != nil {
			return nil, err
		}
	} else if baseRel.Kind != schema.Table {
		return nil, errAt(codeObjectNotInPrerequisiteState, loc, "cannot %s view %q", cmd, rel.Name)
	}
	// the base table with the view's column names; violations and assignment tracking
	// stay with the base table
	syn := *baseRel
	syn.Columns = nil
	for i, c := range vc {
		var col *schema.Column
		if c.src != nil && c.src.Table == fromName {
			if bc := baseRel.Column(c.src.Column); bc != nil {
				cp := *bc
				cp.Name = c.name
				col = &cp
				if inner, ok := a.viewComputed[bc]; ok {
					a.viewComputed[col] = inner // computed further down the view stack
				}
				a.viewBase[col] = bc
				if b, ok := a.viewBase[bc]; ok {
					a.viewBase[col] = b
				}
				if d, ok := rel.ViewDefaults[c.name]; ok {
					cp.Default = d // the view's own default replaces the base column's
					a.viewDefault[col] = true
				}
			}
		}
		if col == nil {
			col = &schema.Column{Name: c.name, Type: c.typ}
			a.viewComputed[col] = rel.Name
			if d, ok := rel.ViewDefaults[c.name]; ok {
				// a default on a computed column: an INSERT leaving the column out still
				// assigns it (rewriteTargetListIU), which the view refuses
				col.Default = d
				a.viewDefault[col] = true
			}
		}
		col.Num = int16(i + 1)
		syn.Columns = append(syn.Columns, col)
	}
	a.viewTargets[rel] = &syn
	return &syn, nil
}

// mergeTargetCheck is the rewriter's view of a MERGE target with the given actions, down
// the stack of automatically updatable views: a relation with a rule for one of the actions
// refuses MERGE (0A000); a view whose INSTEAD OF triggers cover every action takes it; one
// that is not auto-updatable fails for the first action without a trigger (55000); one that
// is auto-updatable but has triggers for some actions only is refused (0A000).
func (a *analyzer) mergeTargetCheck(rel *schema.Relation, actions []string, loc int32) *Error {
	for {
		for _, cmd := range actions {
			ev := map[string]string{"insert into": "insert", "update": "update", "delete from": "delete"}[cmd]
			if rel.RuleEvents[ev] {
				return errAt(codeFeatureNotSupported, loc, "cannot execute MERGE on relation %q", rel.Name)
			}
		}
		if rel.Kind != schema.View {
			return nil
		}
		covered := 0
		first := ""
		for _, cmd := range actions {
			if a.hasInsteadOfTrigger(rel, cmd) {
				covered++
			} else if first == "" {
				first = cmd
			}
		}
		if covered == len(actions) {
			return nil
		}
		baseRV := autoUpdatableBase(a, rel.Query.GetSelectStmt())
		if baseRV == nil {
			return errAt(codeObjectNotInPrerequisiteState, loc, "cannot %s view %q", first, rel.Name)
		}
		if covered > 0 {
			return errAt(codeFeatureNotSupported, loc, "cannot execute MERGE on relation %q", rel.Name)
		}
		base := a.s.Relation(baseRV.Schemaname, baseRV.Relname)
		if base == nil {
			return nil
		}
		rel = base
	}
}

func (a *analyzer) hasInsteadOfTrigger(rel *schema.Relation, cmd string) bool {
	if rules := rel.InsteadRules; rules != nil {
		switch cmd {
		case "insert into":
			return rules["insert"]
		case "update":
			return rules["update"]
		case "delete from":
			return rules["delete"]
		case "merge into":
			return rules["insert"] || rules["update"] || rules["delete"]
		}
	}
	for _, t := range a.s.Triggers {
		if t.Table != rel.FullName() {
			continue
		}
		switch cmd {
		case "insert into":
			if t.Insert {
				return true
			}
		case "update":
			if t.Update {
				return true
			}
		case "delete from":
			if t.Delete {
				return true
			}
		case "merge into":
			if t.Insert || t.Update || t.Delete {
				return true
			}
		}
	}
	return false
}

// autoUpdatableBase returns the one base relation of an automatically updatable view
// query, nil when the query is not auto-updatable.
func autoUpdatableBase(a *analyzer, sel *pg_query.SelectStmt) *pg_query.RangeVar {
	if sel == nil || sel.Op != pg_query.SetOperation_SETOP_NONE || sel.WithClause != nil ||
		len(sel.DistinctClause) > 0 || len(sel.GroupClause) > 0 || sel.HavingClause != nil ||
		len(sel.WindowClause) > 0 || sel.LimitCount != nil || sel.LimitOffset != nil ||
		len(sel.ValuesLists) > 0 || len(sel.FromClause) != 1 {
		return nil
	}
	rv := sel.FromClause[0].GetRangeVar()
	if rv == nil {
		return nil
	}
	for _, tn := range sel.TargetList {
		if a.hasAggOrSRF(tn.GetResTarget().GetVal()) {
			return nil
		}
	}
	return rv
}

// hasAggOrSRF reports whether an expression contains an aggregate, a window function
// or a set-returning function call.
func (a *analyzer) hasAggOrSRF(n *pg_query.Node) bool {
	if n == nil {
		return false
	}
	if fc := n.GetFuncCall(); fc != nil {
		if fc.Over != nil || fc.AggFilter != nil || fc.AggWithinGroup || fc.AggStar || fc.AggDistinct || a.isAggregateName(strs(fc.Funcname)) {
			return true
		}
		name := strs(fc.Funcname)
		for _, f := range a.s.Catalog.Funcs {
			if f.Name == name[len(name)-1] && f.RetSet {
				return true
			}
		}
		for _, f := range a.s.Functions {
			if f.Name == name[len(name)-1] && f.RetSet {
				return true
			}
		}
	}
	if n.GetSubLink() != nil {
		return false
	}
	for _, c := range children(n) {
		if a.hasAggOrSRF(c) {
			return true
		}
	}
	return false
}

// checkViewColumnWritable rejects assigning a column the view computes.
func (a *analyzer) checkViewColumnWritable(col *schema.Column, at int32) *Error {
	if view, ok := a.viewComputed[col]; ok {
		return errAt(codeFeatureNotSupported, at, "cannot %s column %q of view %q", a.writeCmd, col.Name, view)
	}
	return nil
}

var _ = fmt.Sprintf
var _ = catalog.Text
