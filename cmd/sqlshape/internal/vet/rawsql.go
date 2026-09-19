package vet

import (
	"go/ast"
	"go/constant"
	"go/types"
	"strings"

	"golang.org/x/tools/go/analysis"
)

// Raw driver calls. sqlshape's guarantee is that SQL text is a compile-time constant and
// runtime values only ever travel as parameters; a Query / Exec on pgx or database/sql
// with a string built at runtime is the hole that guarantee does not cover. -raw-sql
// closes it: constant (default) requires the SQL argument of every driver call to be a
// constant, forbid rejects every driver call not made through sqlshape (except in the
// packages -raw-sql-allow names), allow turns the check off.

// rawCall is a driver method that runs SQL: which argument is the SQL text (-1: none,
// e.g. CopyFrom / SendBatch, which forbid still rejects).
type rawCall struct {
	pkg    string // package path suffix of the method's receiver
	method string
	sqlArg int
}

var rawCalls = []rawCall{
	{"jackc/pgx/v5", "Query", 1}, {"jackc/pgx/v5", "QueryRow", 1}, {"jackc/pgx/v5", "Exec", 1},
	{"jackc/pgx/v5", "Prepare", 2}, {"jackc/pgx/v5", "CopyFrom", -1}, {"jackc/pgx/v5", "SendBatch", -1},
	{"jackc/pgx/v5", "Queue", 0}, // pgx.Batch
	{"jackc/pgx/v5/pgxpool", "Query", 1}, {"jackc/pgx/v5/pgxpool", "QueryRow", 1}, {"jackc/pgx/v5/pgxpool", "Exec", 1},
	{"jackc/pgx/v5/pgxpool", "CopyFrom", -1}, {"jackc/pgx/v5/pgxpool", "SendBatch", -1},
	{"database/sql", "Query", 0}, {"database/sql", "QueryContext", 1}, {"database/sql", "QueryRow", 0}, {"database/sql", "QueryRowContext", 1},
	{"database/sql", "Exec", 0}, {"database/sql", "ExecContext", 1}, {"database/sql", "Prepare", 0}, {"database/sql", "PrepareContext", 1},
}

// checkRawSQL applies -raw-sql to every driver call in the package.
func checkRawSQL(pass *analysis.Pass, calls []*ast.CallExpr) {
	mode := rawSQLFlag
	if mode == "allow" {
		return
	}
	if mode != "constant" && mode != "forbid" {
		if len(calls) > 0 {
			pass.Reportf(calls[0].Pos(), "sqlshape: -raw-sql must be constant, forbid or allow (got %q)", mode)
		}
		return
	}
	allowed := false
	for _, p := range strings.Split(rawSQLAllow, ",") {
		if p = strings.TrimSpace(p); p != "" && (pass.Pkg.Path() == strings.TrimSuffix(p, "/...") || strings.HasPrefix(pass.Pkg.Path(), strings.TrimSuffix(p, "...")) && strings.HasSuffix(p, "/...")) {
			allowed = true
		}
	}
	for _, call := range calls {
		rc, fn, ok := rawCallOf(pass, call)
		if !ok {
			continue
		}
		if mode == "forbid" {
			if !allowed {
				pass.Reportf(call.Pos(), "sqlshape: %s.%s executes SQL outside sqlshape; with -raw-sql=forbid every statement goes through sqlshape.Query / One / postgres.Copy (or list the package in -raw-sql-allow)", fn.Pkg().Name(), rc.method)
			}
			continue
		}
		if rc.sqlArg < 0 || rc.sqlArg >= len(call.Args) {
			continue
		}
		arg := call.Args[rc.sqlArg]
		tv, ok := pass.TypesInfo.Types[arg]
		if ok && tv.Value != nil && tv.Value.Kind() == constant.String {
			continue
		}
		pass.Reportf(arg.Pos(), "sqlshape: SQL passed to %s must be a constant: a string built at run time can carry injected SQL; write the dynamic parts as a sqlshape.Query template (or pass -raw-sql=allow)", rc.method)
	}
}

// rawCallOf recognizes a driver call: a method of the listed packages and names.
func rawCallOf(pass *analysis.Pass, call *ast.CallExpr) (rawCall, *types.Func, bool) {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return rawCall{}, nil, false
	}
	fn, ok := pass.TypesInfo.Uses[sel.Sel].(*types.Func)
	if !ok || fn.Pkg() == nil || fn.Type().(*types.Signature).Recv() == nil {
		return rawCall{}, nil, false
	}
	path := fn.Pkg().Path()
	for _, rc := range rawCalls {
		if fn.Name() == rc.method && (path == rc.pkg || strings.HasSuffix(path, "/"+rc.pkg)) {
			return rc, fn, true
		}
	}
	return rawCall{}, nil, false
}
