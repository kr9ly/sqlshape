package analyze

import (
	"fmt"
	"strings"

	pg_query "github.com/pganalyze/pg_query_go/v6"

	"github.com/kr9ly/sqlshape/internal/catalog"
	"github.com/kr9ly/sqlshape/internal/schema"
)

type analyzer struct {
	s        *schema.Schema
	params   map[int32]catalog.OID
	paramSrc map[int32]*Source
	maxParam int32
	notes    []Note
	assigned []assignment // values stored into columns (violation.go)

	viewCache  map[*schema.Relation][]rteCol
	viewScopes map[*schema.Relation]*subquery
	viewBusy   map[*schema.Relation]bool
	// insertSelScope is the scope of INSERT ... SELECT's query (for cardinality)
	insertSelScope *scope
	// lastFuncRetSet is whether the most recent funcCall resolved a set-returning function
	lastFuncRetSet bool
	// inCall is set while analyzing the FuncCall of a CALL statement (procedures allowed)
	inCall bool
	refs   []RelationRef
	fixed  []Source
	// inView is the depth of view definitions being analyzed: their references are the
	// view's, not the statement's
	inView int
	// lastUserFunc is the user function resolved by the most recent funcCall (for RETURNS TABLE columns)
	lastUserFunc *schema.Function
}

// Analyze analyzes exactly one SQL statement against s.
// A statement PG would reject returns an *Error; other failures are ordinary errors.
func Analyze(s *schema.Schema, sql string) (*Result, error) {
	tree, err := pg_query.Parse(sql)
	if err != nil {
		return nil, &Error{Code: codeSyntaxError, Message: strings.TrimPrefix(err.Error(), "syntax error ")}
	}
	if len(tree.Stmts) != 1 {
		return nil, fmt.Errorf("expected exactly one statement, got %d", len(tree.Stmts))
	}
	a := &analyzer{
		s:          s,
		params:     map[int32]catalog.OID{},
		paramSrc:   map[int32]*Source{},
		viewCache:  map[*schema.Relation][]rteCol{},
		viewScopes: map[*schema.Relation]*subquery{},
		viewBusy:   map[*schema.Relation]bool{},
	}
	sc := newScope(nil)
	var cols []rteCol
	var aerr *Error
	switch st := tree.Stmts[0].Stmt.Node.(type) {
	case *pg_query.Node_SelectStmt:
		cols, aerr = a.selectStmt(st.SelectStmt, sc)
	case *pg_query.Node_InsertStmt:
		cols, aerr = a.insertStmt(st.InsertStmt, sc)
	case *pg_query.Node_UpdateStmt:
		cols, aerr = a.updateStmt(st.UpdateStmt, sc)
	case *pg_query.Node_DeleteStmt:
		cols, aerr = a.deleteStmt(st.DeleteStmt, sc)
	case *pg_query.Node_CallStmt:
		cols, aerr = a.callStmt(st.CallStmt, sc)
	default:
		return nil, &Error{Code: codeFeatureNotSupported, Message: fmt.Sprintf("unsupported statement %T", tree.Stmts[0].Stmt.Node)}
	}
	if aerr != nil {
		return nil, aerr
	}
	res := &Result{}
	for i := int32(1); i <= a.maxParam; i++ {
		t, ok := a.params[i]
		if !ok {
			return nil, &Error{Code: codeIndeterminateDatatype, Message: fmt.Sprintf("could not determine data type of parameter $%d", i)}
		}
		if t == catalog.Unknown {
			t = catalog.Text // PG's final fallback for still-unknown parameters
		}
		res.Params = append(res.Params, ref(t))
		res.ParamSources = append(res.ParamSources, a.paramSrc[i])
	}
	for _, c := range cols {
		res.Columns = append(res.Columns, a.column(c))
	}
	res.AtMostOne, res.ManyRowsWhy = a.cardinality(tree.Stmts[0].Stmt, sc)
	if sel := tree.Stmts[0].Stmt.GetSelectStmt(); sel != nil && sel.LimitCount != nil && len(sel.SortClause) == 0 && !res.AtMostOne {
		a.note(noteUnorderedLimit, loc(sel.LimitCount), "LIMIT without ORDER BY: which rows are returned is unspecified")
	}
	res.Violations = a.violations(tree.Stmts[0].Stmt)
	res.Relations = a.refs
	for _, as := range a.assigned {
		if as.rel != nil {
			a.fixed = append(a.fixed, Source{Table: as.rel.FullName(), Column: as.col.Name, NotNull: as.col.NotNull, Assigned: true})
		}
	}
	res.Fixed = a.fixed
	res.Notes = a.notes
	return res, nil
}

// String renders the result in the oracle's golden format (domains flattened as on the wire).
func (r *Result) String(types *schema.Types) string {
	var b strings.Builder
	b.WriteString("params:\n")
	for i, p := range r.Params {
		fmt.Fprintf(&b, "  $%d %s\n", i+1, types.Format(p)) // parameters keep their domain type on the wire
	}
	b.WriteString("columns:\n")
	for _, c := range r.Columns {
		fmt.Fprintf(&b, "  %s %s", c.Name, types.Format(types.BaseOf(c.Type)))
		if c.Source != nil {
			fmt.Fprintf(&b, " <- %s.%s", c.Source.Table, c.Source.Column)
			if c.Source.NotNull {
				b.WriteString(" not null")
			} else {
				b.WriteString(" null")
			}
		}
		b.WriteString("\n")
	}
	return b.String()
}

// column converts a range-table column to a result column, describing record shapes.
func (a *analyzer) column(c rteCol) Column {
	col := Column{Name: c.name, Type: c.typ, Nullable: c.nullable, Source: c.src}
	fields := c.fields
	if len(fields) == 0 {
		// a named composite (or an array of one): its declared columns
		oid := c.typ.OID
		if t := a.typ(oid); t != nil && t.IsArray() {
			oid = t.Elem
		}
		if rel := a.relByRowType(oid); rel != nil {
			for _, rc := range rel.Columns {
				fields = append(fields, rteCol{name: rc.Name, typ: rc.Type, nullable: !rc.NotNull})
			}
		}
	}
	for _, f := range fields {
		col.Fields = append(col.Fields, a.column(f))
	}
	return col
}
