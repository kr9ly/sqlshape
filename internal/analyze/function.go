package analyze

import (
	"fmt"
	"strings"

	pg_query "github.com/pganalyze/pg_query_go/v6"

	"github.com/kr9ly/sqlshape/internal/catalog"
	"github.com/kr9ly/sqlshape/internal/schema"
)

// SQL function bodies. A LANGUAGE sql function is plain SQL over its parameters, so the
// analyzer can check it the way PG does at CREATE time (check_function_bodies): every
// statement must analyze with the parameters in scope (by name and as $n; a column of
// the same name wins, as in PG), and the last statement must produce what RETURNS
// declares. PL/pgSQL bodies stay opaque.

// FunctionResult is the analysis of a function body.
type FunctionResult struct {
	// Relations the body references directly (dependency graph through functions).
	Relations []RelationRef
	// Notes from the body's statements (domain mixing etc.).
	Notes []Note
}

const codeInvalidFunctionDefinition = "42P13"

// AnalyzeFunction checks a LANGUAGE sql function's body against its signature. Functions
// without an analyzable body return an empty result.
func AnalyzeFunction(s *schema.Schema, fn *schema.Function) (*FunctionResult, error) {
	var stmts []*pg_query.Node
	switch {
	case fn.SQLBody != nil:
		stmts = flattenLists(fn.SQLBody)
	case fn.Body != "" && strings.EqualFold(fn.Language, "sql"):
		tree, err := pg_query.Parse(fn.Body)
		if err != nil {
			return nil, &Error{Code: codeSyntaxError, Message: strings.TrimPrefix(err.Error(), "syntax error ")}
		}
		for _, raw := range tree.Stmts {
			stmts = append(stmts, raw.Stmt)
		}
	default:
		return &FunctionResult{}, nil
	}
	var fp []funcParam
	for _, arg := range fn.Args {
		if arg.Mode == 'i' || arg.Mode == 'b' || arg.Mode == 'v' {
			fp = append(fp, funcParam{name: arg.Name, typ: arg.Type})
		}
	}
	out := &FunctionResult{}
	var last *Result
	for i, st := range stmts {
		if rs := st.GetReturnStmt(); rs != nil {
			// BEGIN ATOMIC ... RETURN expr: analyze as SELECT expr
			st = &pg_query.Node{Node: &pg_query.Node_SelectStmt{SelectStmt: &pg_query.SelectStmt{
				TargetList: []*pg_query.Node{{Node: &pg_query.Node_ResTarget{ResTarget: &pg_query.ResTarget{Val: rs.Returnval}}}},
			}}}
		}
		r, err := analyzeStmt(s, st, fp, nil)
		if err != nil {
			return nil, err
		}
		out.Relations = append(out.Relations, r.Relations...)
		out.Notes = append(out.Notes, r.Notes...)
		if i == len(stmts)-1 {
			last = r
		}
	}
	if fn.IsProc || fn.RetType.OID == catalog.Void || fn.RetType.OID == catalog.Trigger {
		return out, nil
	}
	if last == nil || len(last.Columns) == 0 {
		return nil, &Error{Code: codeInvalidFunctionDefinition, Message: fmt.Sprintf("return type mismatch in function declared to return %s: function's final statement must be SELECT or INSERT/UPDATE/DELETE RETURNING", s.Types.Format(fn.RetType))}
	}
	// declared shape: RETURNS TABLE / OUT columns, a composite type, or one scalar
	var want []schema.TypeRef
	for _, arg := range fn.Args {
		if arg.Mode == 't' || arg.Mode == 'o' || arg.Mode == 'b' {
			want = append(want, arg.Type)
		}
	}
	if len(want) == 0 {
		if rel := relByRowType(s, fn.RetType.OID); rel != nil && len(last.Columns) > 1 {
			for _, c := range rel.Columns {
				want = append(want, c.Type)
			}
		} else {
			want = []schema.TypeRef{fn.RetType}
		}
	}
	a := &analyzer{s: s}
	if len(last.Columns) != len(want) {
		return nil, &Error{Code: codeInvalidFunctionDefinition, Message: fmt.Sprintf("return type mismatch in function declared to return %s: final statement returns %d columns, %d expected", s.Types.Format(fn.RetType), len(last.Columns), len(want))}
	}
	for i, c := range last.Columns {
		if !a.canCoerce(c.Type.OID, want[i].OID, assignmentCoercion) {
			return nil, &Error{Code: codeInvalidFunctionDefinition, Message: fmt.Sprintf("return type mismatch in function declared to return %s: final statement returns %s instead of %s at column %d", s.Types.Format(fn.RetType), s.Types.Format(c.Type), s.Types.Format(want[i]), i+1)}
		}
	}
	return out, nil
}

func relByRowType(s *schema.Schema, oid catalog.OID) *schema.Relation {
	for _, r := range s.Relations {
		if r.RowType == oid {
			return r
		}
	}
	return nil
}

// flattenLists unwraps the nested List nodes of a BEGIN ATOMIC body into statements.
func flattenLists(n *pg_query.Node) []*pg_query.Node {
	l := n.GetList()
	if l == nil {
		return []*pg_query.Node{n}
	}
	var out []*pg_query.Node
	for _, it := range l.Items {
		out = append(out, flattenLists(it)...)
	}
	return out
}

// AnalyzeView analyzes a view's defining query as a statement of its own, so its findings
// (policy, domain mixing) are reported once at the view rather than at every reader.
func AnalyzeView(s *schema.Schema, rel *schema.Relation) (*Result, error) {
	if rel.Query == nil {
		return &Result{}, nil
	}
	return analyzeStmt(s, rel.Query, nil, nil)
}
