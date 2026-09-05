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
	stmts, err := functionBody(fn)
	if err != nil {
		return nil, err
	}
	if stmts == nil {
		return &FunctionResult{}, nil
	}
	fp := functionParams(fn)
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

// functionBody parses a SQL function's statements (nil for other languages).
func functionBody(fn *schema.Function) ([]*pg_query.Node, error) {
	switch {
	case fn.SQLBody != nil:
		return flattenLists(fn.SQLBody), nil
	case fn.Body != "" && strings.EqualFold(fn.Language, "sql"):
		tree, err := pg_query.Parse(fn.Body)
		if err != nil {
			return nil, &Error{Code: codeSyntaxError, Message: strings.TrimPrefix(err.Error(), "syntax error ")}
		}
		var stmts []*pg_query.Node
		for _, raw := range tree.Stmts {
			stmts = append(stmts, raw.Stmt)
		}
		return stmts, nil
	}
	return nil, nil
}

func functionParams(fn *schema.Function) []funcParam {
	var fp []funcParam
	for _, arg := range fn.Args {
		if arg.Mode == 'i' || arg.Mode == 'b' || arg.Mode == 'v' {
			fp = append(fp, funcParam{name: arg.Name, typ: arg.Type})
		}
	}
	return fp
}

// calledFunc is a user function a statement calls, with the call's argument expressions.
type calledFunc struct {
	fn   *schema.Function
	args []*pg_query.Node
}

// functionViolations lists what a call to fn may violate: the SQLSTATEs it declares with
// `-- sqlshape: error` and, for a SQL function, whatever its body's statements may violate
// (functions they call included; visited guards recursion). A NOT NULL violation the body
// blames on a parameter is translated to the call: a STRICT function is not even called
// with a NULL, a non-null literal cannot violate, a $n parameter of the statement keeps
// the blame (so the Go type decides), any other argument leaves it possible.
func functionViolations(s *schema.Schema, cf calledFunc, visited map[*schema.Function]bool) []Violation {
	fn := cf.fn
	if visited[fn] {
		return nil
	}
	visited[fn] = true
	var out []Violation
	for _, r := range fn.Raises {
		out = append(out, Violation{Code: r.Code, Constraint: r.Code, Name: r.Name, Function: fn.Name})
	}
	stmts, err := functionBody(fn)
	if err != nil {
		return out
	}
	positional := true
	for _, a := range cf.args {
		if a.GetNamedArgExpr() != nil {
			positional = false
		}
	}
	fp := functionParams(fn)
	for _, st := range stmts {
		if st.GetReturnStmt() != nil {
			continue
		}
		r, err := analyzeStmt(s, st, fp, nil)
		if err != nil {
			continue
		}
		for _, v := range r.Violations {
			if v.Function == "" {
				v.Function = fn.Name
			}
			if v.Param > 0 {
				if fn.Strict {
					continue
				}
				pos := int(v.Param)
				v.Param = 0
				if positional && pos <= len(cf.args) {
					switch arg := cf.args[pos-1].Node.(type) {
					case *pg_query.Node_ParamRef:
						v.Param = arg.ParamRef.Number
					case *pg_query.Node_AConst:
						if !arg.AConst.Isnull {
							continue
						}
					}
				}
			}
			out = append(out, v)
		}
	}
	return dedupe(out)
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
