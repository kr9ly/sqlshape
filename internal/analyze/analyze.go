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
	maxParam int32

	viewCache map[*schema.Relation][]rteCol
	viewBusy  map[*schema.Relation]bool
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
		s:         s,
		params:    map[int32]catalog.OID{},
		viewCache: map[*schema.Relation][]rteCol{},
		viewBusy:  map[*schema.Relation]bool{},
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
	}
	for _, c := range cols {
		res.Columns = append(res.Columns, Column{Name: c.name, Type: c.typ, Nullable: c.nullable, Source: c.src})
	}
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
