package analyze

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"

	"github.com/kr9ly/sqlshape/internal/catalog"
	"github.com/kr9ly/sqlshape/internal/pgparse"
	"github.com/kr9ly/sqlshape/internal/schema"
)

// PL/pgSQL bodies. libpg_query's PL/pgSQL parser gives the body's structure (blocks,
// control flow, variable declarations) with every embedded SQL fragment as text; the
// analyzer then checks each fragment as a statement with the PL variables in scope,
// typed from their declarations (or, for record variables, from the query that fills
// them). What comes out is what a LANGUAGE sql body gives: the relations the body
// touches, the constraints its writes may violate, notes, and type errors, plus the
// SQLSTATEs its RAISE statements throw, which join the function's declared errors.
//
// Trigger functions are analyzed once per CREATE TRIGGER that attaches them, with NEW
// and OLD typed as that table's row; an unattached trigger function is not analyzed.
//
// EXECUTE with a constant string is analyzed like the statement it runs; with a string
// built at run time it cannot be, and an advisory note says so.

const notePLDynamicSQL = "dynamic-sql"

// plVar is a PL/pgSQL variable in scope: a declared scalar, a %ROWTYPE row, a record
// (shape known once a query filled it), a function parameter, or an implicit variable
// (FOUND, NEW, OLD, TG_*, SQLSTATE, SQLERRM).
type plVar struct {
	name   string
	typ    schema.TypeRef
	fields []rteCol // record shape from the query that filled it; nil otherwise
	// open: a record whose shape is unknown (a FETCH target, a record never filled by a
	// statement the analyzer saw); its fields type as unknown instead of failing
	open bool
	// cursor: a bound cursor's query (DECLARE c CURSOR FOR ...), and the columns it
	// produced once OPEN / FOR analyzed it
	cursor     string
	cursorCols []Column
	// constVal: the variable's declared initial value, when it is a string literal and
	// the variable is never reassigned anywhere in the function body (see
	// declareDatums / scanAssignedVarnos). Used to resolve `RAISE ... USING ERRCODE =
	// var` statically when var is really acting as a named constant. "" when the
	// variable has no such value or might change.
	constVal string
}

// plBody is one function body under analysis.
type plBody struct {
	s        *schema.Schema
	fn       *schema.Function
	tg       *schema.Trigger
	rel      *schema.Relation // the trigger's table
	visited  map[*schema.Function]bool
	params   []funcParam       // the function's own parameters ($n and by name)
	vars     map[string]*plVar // by lower-cased name
	datums   []*plVar          // by datum number (nil for datums that are not variables)
	out      *FunctionResult
	raises   []schema.RaisedError
	inExcept int
	// assignedVarnos: every datum number that is ever a write target somewhere in the
	// function (RAISE ERRCODE constant-folding needs to know a variable is never
	// reassigned after its DECLARE default). Filled once by run() before the body walk.
	assignedVarnos map[float64]bool
	// handlerCodes: the SQLSTATE(s) the exception handler currently being walked catches
	// (its WHEN conditions' codes, or the protected body's own raised codes for OTHERS),
	// used to resolve a bare `RAISE ... USING ERRCODE = SQLSTATE` (re-raise) statically.
	// Stack-like: saved/restored around each handler by exceptionBlock.
	handlerCodes []string
}

// plBodies caches the analysis of a body per function (schemas are immutable once loaded).
var plBodies sync.Map // map[*schema.Function]*plCached

type plCached struct {
	results    []*FunctionResult
	violations []Violation
	raises     []schema.RaisedError
	err        error
}

func isPLpgSQL(fn *schema.Function) bool {
	return fn.Body != "" && strings.EqualFold(fn.Language, "plpgsql")
}

// analyzePLpgSQL analyzes fn's body (once per trigger attachment for a trigger function)
// and caches the outcome.
func analyzePLpgSQL(s *schema.Schema, fn *schema.Function) *plCached {
	// a body under analysis that reaches itself (a trigger function writing its own table,
	// a recursive call) meets the in-progress marker and contributes nothing more
	if c, loaded := plBodies.LoadOrStore(fn, &plCached{}); loaded {
		return c.(*plCached)
	}
	c := &plCached{}
	visited := map[*schema.Function]bool{fn: true}
	if fn.RetType.OID == catalog.Trigger {
		for _, tg := range s.Triggers {
			if triggerFunction(s, tg) != fn {
				continue
			}
			b := newPLBody(s, fn, tg, visited)
			r, err := b.run()
			if err != nil {
				c.err = fmt.Errorf("trigger %s: %w", tg.Name, err)
				break
			}
			c.results = append(c.results, r)
			c.violations = append(c.violations, b.violations()...)
			c.raises = append(c.raises, b.raises...)
		}
	} else {
		b := newPLBody(s, fn, nil, visited)
		r, err := b.run()
		if err != nil {
			c.err = err
		} else {
			c.results = append(c.results, r)
			c.violations = b.violations()
			c.raises = b.raises
		}
	}
	plBodies.Store(fn, c)
	return c
}

// triggerFunction resolves the function a trigger executes.
func triggerFunction(s *schema.Schema, tg *schema.Trigger) *schema.Function {
	fs, fname := "", tg.Function
	if i := strings.LastIndex(fname, "."); i >= 0 {
		fs, fname = fname[:i], fname[i+1:]
	}
	return s.Function(fs, fname)
}

// raisedErrors lists the SQLSTATEs a function raises: its `-- sqlshape: error`
// annotations plus, for a PL/pgSQL body, the RAISE statements the analyzer found (an
// annotation with the same code supplies the name).
func raisedErrors(s *schema.Schema, fn *schema.Function) []schema.RaisedError {
	out := append([]schema.RaisedError(nil), fn.Raises...)
	if !isPLpgSQL(fn) {
		return out
	}
	seen := map[string]bool{}
	for _, r := range out {
		seen[r.Code] = true
	}
	for _, r := range analyzePLpgSQL(s, fn).raises {
		if !seen[r.Code] {
			seen[r.Code] = true
			out = append(out, r)
		}
	}
	return out
}

func newPLBody(s *schema.Schema, fn *schema.Function, tg *schema.Trigger, visited map[*schema.Function]bool) *plBody {
	b := &plBody{s: s, fn: fn, tg: tg, visited: visited, vars: map[string]*plVar{}, out: &FunctionResult{}}
	b.params = functionParams(fn)
	if tg != nil {
		rs, rn := "", tg.Table
		if i := strings.LastIndex(rn, "."); i >= 0 {
			rs, rn = rn[:i], rn[i+1:]
		}
		b.rel = s.Relation(rs, rn)
	}
	return b
}

// declare adds a variable (later declarations of the same name shadow earlier ones, as
// in a nested block).
func (b *plBody) declare(v *plVar) {
	b.vars[strings.ToLower(v.name)] = v
}

// scope is the parameter list for analyzing a fragment: the function's parameters first
// (they are $1..), then every PL variable.
func (b *plBody) scope() []funcParam {
	fp := append([]funcParam(nil), b.params...)
	for _, v := range b.vars {
		if v.open {
			fp = append(fp, funcParam{name: v.name, typ: ref(catalog.Record), plVar: true, fields: []rteCol{}})
			continue
		}
		fp = append(fp, funcParam{name: v.name, typ: v.typ, plVar: true, fields: v.fields})
	}
	return fp
}

// run parses and walks the body.
func (b *plBody) run() (*FunctionResult, error) {
	kind := "FUNCTION"
	if b.fn.IsProc {
		kind = "PROCEDURE"
	}
	text := "CREATE " + kind + " " + plSignature(b.fn) + " LANGUAGE plpgsql AS $sqlshape$" + b.fn.Body + "$sqlshape$"
	js, err := pgparse.ParsePlPgSqlToJSON(text)
	if err != nil {
		return nil, &Error{Code: codeSyntaxError, Message: strings.TrimPrefix(err.Error(), "syntax error ")}
	}
	var doc []map[string]any
	if err := json.Unmarshal([]byte(js), &doc); err != nil || len(doc) == 0 {
		return nil, &Error{Code: codeSyntaxError, Message: "cannot read the PL/pgSQL parse"}
	}
	fnNode, _ := doc[0]["PLpgSQL_function"].(map[string]any)
	b.assignedVarnos = map[float64]bool{}
	scanAssignedVarnos(fnNode["action"], b.assignedVarnos)
	if err := b.declareDatums(fnNode); err != nil {
		return nil, err
	}
	b.declareImplicit()
	if err := b.walkList(fnNode["action"]); err != nil {
		return nil, err
	}
	return b.out, nil
}

// scanAssignedVarnos walks a PL/pgSQL parse fragment (as decoded from
// ParsePlPgSqlToJSON) collecting every datum number that is ever a write target: a plain
// `var := expr` (PLpgSQL_stmt_assign's own "varno"), or one of an INTO / FOR / FETCH
// target's fields (a PLpgSQL_row / PLpgSQL_var, also keyed "varno" there). Expression
// text itself is opaque to the PL parser at this level (kept as a string, parsed only
// later by the SQL parser), so "varno" never appears in a read-only position -- this is
// safe to key on unconditionally, wherever it turns up in the tree.
func scanAssignedVarnos(node any, out map[float64]bool) {
	switch x := node.(type) {
	case []any:
		for _, it := range x {
			scanAssignedVarnos(it, out)
		}
	case map[string]any:
		if vn, ok := x["varno"].(float64); ok {
			out[vn] = true
		}
		for _, v := range x {
			switch v.(type) {
			case []any, map[string]any:
				scanAssignedVarnos(v, out)
			}
		}
	}
}

// plSignature re-renders the function's parameters for the PL/pgSQL parser, which needs
// them to know the parameter names and types (the body text alone has neither).
func plSignature(fn *schema.Function) string {
	var args []string
	for i, a := range fn.Args {
		name := a.Name
		if name == "" {
			name = fmt.Sprintf("sqlshape_arg%d", i+1)
		}
		mode := ""
		switch a.Mode {
		case 'o', 't':
			mode = "OUT "
		case 'b':
			mode = "INOUT "
		case 'v':
			mode = "VARIADIC "
		}
		args = append(args, mode+name+" text")
	}
	// the PL/pgSQL parser validates RETURN / RETURN NEXT / RETURN QUERY against the
	// return kind and creates NEW / OLD only for triggers, so render that faithfully
	ret := " RETURNS text"
	switch {
	case fn.IsProc:
		return "sqlshape_body(" + strings.Join(args, ", ") + ")"
	case fn.RetType.OID == catalog.Trigger:
		ret = " RETURNS trigger"
	case fn.RetType.OID == catalog.Void:
		ret = " RETURNS void"
	case fn.RetSet && fn.RetType.OID == catalog.Record:
		ret = " RETURNS SETOF record"
	case fn.RetSet:
		ret = " RETURNS SETOF text"
	case fn.RetType.OID == catalog.Record:
		ret = " RETURNS record"
	}
	return "sqlshape_body(" + strings.Join(args, ", ") + ")" + ret
}

// declareDatums records the declared variables: PLpgSQL_var (typed), PLpgSQL_rec
// (records: NEW / OLD, DECLARE r record, FOR r IN ...), PLpgSQL_row (INTO targets, no
// variable of their own).
func (b *plBody) declareDatums(fnNode map[string]any) error {
	datums, _ := fnNode["datums"].([]any)
	b.datums = make([]*plVar, len(datums))
	for i, d := range datums {
		dm, _ := d.(map[string]any)
		if v, ok := dm["PLpgSQL_var"].(map[string]any); ok {
			name, _ := v["refname"].(string)
			tn := ""
			if dt, ok := v["datatype"].(map[string]any); ok {
				if t, ok := dt["PLpgSQL_type"].(map[string]any); ok {
					tn, _ = t["typname"].(string)
				}
			}
			pv := &plVar{name: name, cursor: plQuery(v["cursor_explicit_expr"])}
			if tn != "" {
				typ, fields, err := b.resolveTypeName(tn, plLine(v))
				if err != nil {
					return err
				}
				pv.typ, pv.fields = typ, fields
			}
			// a variable declared `x text := 'literal';` and never reassigned anywhere in
			// the function acts as a named constant; RAISE ... USING ERRCODE = x can then
			// be resolved statically the same as a literal (see A1 in adv_plpgsql_test.go).
			if dv, ok := v["default_val"].(map[string]any); ok && !b.assignedVarnos[float64(i)] {
				pv.constVal = plStringLiteral(plQuery(dv))
			}
			// function parameters arrive as datums too (named by the signature above,
			// typed text there): input parameters are already in scope as $n / by name;
			// OUT parameters are variables of their declared type
			if b.isParamName(name) {
				continue
			}
			if arg := b.outArg(name); arg != nil {
				pv.typ, pv.fields = arg.Type, nil
			}
			b.datums[i] = pv
			b.declare(pv)
		}
		if r, ok := dm["PLpgSQL_rec"].(map[string]any); ok {
			name, _ := r["refname"].(string)
			pv := &plVar{name: name, typ: ref(catalog.Record), open: true}
			switch strings.ToLower(name) {
			case "new", "old":
				if b.rel != nil {
					pv.open = false
					pv.typ, pv.fields = b.rowOf(b.rel)
				}
			}
			b.datums[i] = pv
			b.declare(pv)
		}
	}
	return nil
}

// outArg is the OUT parameter named name, nil otherwise.
func (b *plBody) outArg(name string) *schema.FuncArg {
	for i := range b.fn.Args {
		a := &b.fn.Args[i]
		if (a.Mode == 'o' || a.Mode == 't') && strings.EqualFold(a.Name, name) {
			return a
		}
	}
	return nil
}

func (b *plBody) isParamName(name string) bool {
	for _, p := range b.params {
		if strings.EqualFold(p.name, name) {
			return true
		}
	}
	for i, a := range b.fn.Args {
		if a.Name == "" && name == fmt.Sprintf("sqlshape_arg%d", i+1) {
			return true
		}
	}
	return false
}

// declareImplicit adds FOUND and, in a trigger, the TG_* variables.
func (b *plBody) declareImplicit() {
	b.declare(&plVar{name: "found", typ: ref(catalog.Bool)})
	if b.tg != nil {
		for name, oid := range map[string]catalog.OID{
			"tg_name": catalog.Name, "tg_when": catalog.Text, "tg_level": catalog.Text, "tg_op": catalog.Text,
			"tg_relid": catalog.OIDType, "tg_table_name": catalog.Name, "tg_table_schema": catalog.Name,
			"tg_nargs": catalog.Int4, "tg_argv": catalog.OID(1009), // text[]
		} {
			b.declare(&plVar{name: name, typ: ref(oid)})
		}
	}
}

// rowOf is a relation's row type, or its columns when it has no named row type.
func (b *plBody) rowOf(rel *schema.Relation) (schema.TypeRef, []rteCol) {
	if rel.RowType != 0 {
		return ref(rel.RowType), nil
	}
	var cols []rteCol
	for _, c := range rel.Columns {
		cols = append(cols, rteCol{name: c.Name, typ: c.Type, nullable: !c.NotNull})
	}
	return ref(catalog.Record), cols
}

var typeSuffix = regexp.MustCompile(`(?i)^(.*)%(TYPE|ROWTYPE)$`)

// resolveTypeName types a declaration: a SQL type name, table%ROWTYPE, table.column%TYPE
// or variable%TYPE.
func (b *plBody) resolveTypeName(tn string, line int) (schema.TypeRef, []rteCol, error) {
	if m := typeSuffix.FindStringSubmatch(strings.TrimSpace(tn)); m != nil {
		parts := strings.Split(m[1], ".")
		for i := range parts {
			parts[i] = strings.Trim(strings.TrimSpace(parts[i]), `"`)
		}
		if strings.EqualFold(m[2], "ROWTYPE") {
			rel := b.relByParts(parts)
			if rel == nil {
				return schema.TypeRef{}, nil, b.errf(line, codeUndefinedTable, "relation %q does not exist", m[1])
			}
			t, f := b.rowOf(rel)
			return t, f, nil
		}
		if len(parts) == 1 {
			if v := b.vars[strings.ToLower(parts[0])]; v != nil {
				return v.typ, v.fields, nil
			}
			for _, p := range b.params {
				if strings.EqualFold(p.name, parts[0]) {
					return p.typ, nil, nil
				}
			}
			return schema.TypeRef{}, nil, b.errf(line, codeUndefinedColumn, "variable %q does not exist", parts[0])
		}
		rel := b.relByParts(parts[:len(parts)-1])
		if rel == nil {
			return schema.TypeRef{}, nil, b.errf(line, codeUndefinedTable, "relation %q does not exist", strings.Join(parts[:len(parts)-1], "."))
		}
		col := rel.Column(parts[len(parts)-1])
		if col == nil {
			return schema.TypeRef{}, nil, b.errf(line, codeUndefinedColumn, "column %q of relation %q does not exist", parts[len(parts)-1], rel.Name)
		}
		return col.Type, nil, nil
	}
	if strings.EqualFold(tn, "record") {
		return ref(catalog.Record), nil, nil
	}
	// the PL parser spells built-in types pg_catalog."integer": a quoted keyword is not a
	// type name to the SQL parser, so unquote it
	if rest, ok := strings.CutPrefix(tn, "pg_catalog."); ok && strings.HasPrefix(rest, `"`) && strings.HasSuffix(rest, `"`) && strings.Count(rest, `"`) == 2 {
		tn = rest[1 : len(rest)-1]
	}
	tree, err := pgparse.Parse("SELECT NULL::" + tn)
	if err != nil {
		return schema.TypeRef{}, nil, b.errf(line, codeUndefinedObject, "type %q does not exist", tn)
	}
	tc := tree.Stmts[0].Stmt.GetSelectStmt().TargetList[0].GetResTarget().Val.GetTypeCast()
	typ, rerr := b.s.ResolveType(tc.TypeName)
	if rerr != nil {
		return schema.TypeRef{}, nil, b.errf(line, codeUndefinedObject, "%v", rerr)
	}
	return typ, nil, nil
}

func (b *plBody) relByParts(parts []string) *schema.Relation {
	switch len(parts) {
	case 1:
		return b.s.Relation("", parts[0])
	case 2:
		return b.s.Relation(parts[0], parts[1])
	}
	return nil
}

func (b *plBody) errf(line int, code, format string, args ...any) *Error {
	msg := fmt.Sprintf(format, args...)
	if line > 0 {
		msg = fmt.Sprintf("line %d: %s", line, msg)
	}
	return &Error{Code: code, Message: msg}
}

func plLine(m map[string]any) int {
	if l, ok := m["lineno"].(float64); ok {
		return int(l)
	}
	return 0
}

// --- statements ----------------------------------------------------------------

func (b *plBody) walkList(v any) error {
	switch x := v.(type) {
	case []any:
		for _, it := range x {
			if err := b.walkList(it); err != nil {
				return err
			}
		}
	case map[string]any:
		for k, inner := range x {
			im, _ := inner.(map[string]any)
			if strings.HasPrefix(k, "PLpgSQL_stmt_") && im != nil {
				return b.stmt(k, im)
			}
		}
		// a block or a wrapper (PLpgSQL_exception, PLpgSQL_if_elsif, PLpgSQL_case_when)
		for _, inner := range x {
			switch inner.(type) {
			case []any, map[string]any:
				if err := b.walkList(inner); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (b *plBody) stmt(kind string, m map[string]any) error {
	line := plLine(m)
	switch kind {
	case "PLpgSQL_stmt_block":
		return b.block(m)
	case "PLpgSQL_stmt_execsql":
		r, err := b.sql(plQuery(m["sqlstmt"]), line)
		if err != nil {
			return err
		}
		if into, _ := m["into"].(bool); into && r != nil {
			if strict, _ := m["strict"].(bool); strict {
				b.checkIntoStrict(r)
			}
			return b.assignInto(m["target"], r, line)
		}
		return nil
	case "PLpgSQL_stmt_perform":
		_, err := b.sql(plQuery(m["expr"]), line)
		return err
	case "PLpgSQL_stmt_dynexecute":
		return b.dynexecute(m, line)
	case "PLpgSQL_stmt_fors":
		r, err := b.sql(plQuery(m["query"]), line)
		if err != nil {
			return err
		}
		if r != nil {
			b.fillRecord(m["var"], r.Columns)
		}
		return b.walkList(m["body"])
	case "PLpgSQL_stmt_forc":
		// FOR r IN cursor: the row takes the shape of the cursor's bound query
		var cols []Column
		if cv := b.cursorVar(m); cv != nil {
			if err := b.openBound(cv, line); err != nil {
				return err
			}
			cols = cv.cursorCols
		}
		b.fillRecord(m["var"], cols)
		return b.walkList(m["body"])
	case "PLpgSQL_stmt_dynfors":
		if err := b.dynexecute(m, line); err != nil {
			return err
		}
		b.fillRecord(m["var"], nil)
		return b.walkList(m["body"])
	case "PLpgSQL_stmt_fori":
		for _, k := range []string{"lower", "upper", "step"} {
			if q := plQuery(m[k]); q != "" {
				if _, err := b.expr(q, line, ref(catalog.Int4)); err != nil {
					return err
				}
			}
		}
		return b.walkList(m["body"])
	case "PLpgSQL_stmt_foreach_a":
		if _, err := b.expr(plQuery(m["expr"]), line, schema.TypeRef{}); err != nil {
			return err
		}
		return b.walkList(m["body"])
	case "PLpgSQL_stmt_if":
		if _, err := b.expr(plQuery(m["cond"]), line, ref(catalog.Bool)); err != nil {
			return err
		}
		if err := b.walkList(m["then_body"]); err != nil {
			return err
		}
		if elsifs, ok := m["elsif_list"].([]any); ok {
			for _, e := range elsifs {
				em, _ := e.(map[string]any)
				if in, ok := em["PLpgSQL_if_elsif"].(map[string]any); ok {
					if _, err := b.expr(plQuery(in["cond"]), plLine(in), ref(catalog.Bool)); err != nil {
						return err
					}
					if err := b.walkList(in["stmts"]); err != nil {
						return err
					}
				}
			}
		}
		return b.walkList(m["else_body"])
	case "PLpgSQL_stmt_case":
		if q := plQuery(m["t_expr"]); q != "" {
			if _, err := b.expr(q, line, schema.TypeRef{}); err != nil {
				return err
			}
		}
		if whens, ok := m["case_when_list"].([]any); ok {
			for _, w := range whens {
				wm, _ := w.(map[string]any)
				if in, ok := wm["PLpgSQL_case_when"].(map[string]any); ok {
					want := ref(catalog.Bool)
					if plQuery(m["t_expr"]) != "" {
						want = schema.TypeRef{} // CASE x WHEN value: compared, not boolean
					}
					if _, err := b.expr(plQuery(in["expr"]), plLine(in), want); err != nil {
						return err
					}
					if err := b.walkList(in["stmts"]); err != nil {
						return err
					}
				}
			}
		}
		if _, hasElse := m["else_stmts"]; !hasElse {
			// PG's CASE (the PL/pgSQL statement, not the SQL expression) raises
			// CASE_NOT_FOUND (20000) at run time when no WHEN matches and there is no
			// ELSE to fall back to.
			b.addRaise("20000")
		}
		return b.walkList(m["else_stmts"])
	case "PLpgSQL_stmt_loop":
		return b.walkList(m["body"])
	case "PLpgSQL_stmt_while":
		if _, err := b.expr(plQuery(m["cond"]), line, ref(catalog.Bool)); err != nil {
			return err
		}
		return b.walkList(m["body"])
	case "PLpgSQL_stmt_exit":
		if q := plQuery(m["cond"]); q != "" {
			_, err := b.expr(q, line, ref(catalog.Bool))
			return err
		}
		return nil
	case "PLpgSQL_stmt_assign":
		return b.assign(plQuery(m["expr"]), line)
	case "PLpgSQL_stmt_return":
		if v := b.retVar(m); v != nil {
			// RETURN var: the PL parser records the variable instead of an expression
			return b.retType(v.typ, line)
		}
		return b.ret(plQuery(m["expr"]), line)
	case "PLpgSQL_stmt_return_next":
		want := b.fn.RetType
		if rel := relByRowType(b.s, want.OID); rel != nil || b.fn.RetType.OID == catalog.Record {
			want = schema.TypeRef{}
		}
		if v := b.retVar(m); v != nil {
			return b.assignable(v.typ, want, line, "RETURN NEXT "+v.name)
		}
		if q := plQuery(m["expr"]); q != "" {
			_, err := b.expr(q, line, want)
			return err
		}
		// RETURN NEXT var: the PL parser keeps only the variable's number, which the JSON
		// does not carry; the source line names it
		if name := b.returnNextVar(line); name != "" {
			if v := b.vars[strings.ToLower(name)]; v != nil {
				return b.assignable(v.typ, want, line, "RETURN NEXT "+name)
			}
		}
		return nil
	case "PLpgSQL_stmt_return_query":
		if q := plQuery(m["query"]); q != "" {
			r, err := b.sql(q, line)
			if err != nil {
				return err
			}
			if r != nil {
				if err := checkReturnShape(b.s, b.fn, r.Columns); err != nil {
					return b.errf(line, err.Code, "%s", err.Message)
				}
			}
			return nil
		}
		return b.dynexecuteText(plQuery(m["dynquery"]), m["params"], line)
	case "PLpgSQL_stmt_raise":
		return b.raise(m, line)
	case "PLpgSQL_stmt_assert":
		if q := plQuery(m["cond"]); q != "" {
			if _, err := b.expr(q, line, ref(catalog.Bool)); err != nil {
				return err
			}
		}
		// ASSERT raises P0004 (assert_failure) when its condition is false, same as
		// `RAISE assert_failure` (plpgsql.check_asserts defaults to on; that setting is
		// not modeled, per the brief).
		b.addRaise("P0004")
		return nil
	case "PLpgSQL_stmt_open":
		if q := plQuery(m["query"]); q != "" {
			r, err := b.sql(q, line)
			if err != nil {
				return err
			}
			if cv := b.cursorVar(m); cv != nil && r != nil {
				cv.cursorCols = r.Columns
			}
			return nil
		}
		if q := plQuery(m["dynquery"]); q != "" {
			return b.dynexecuteText(q, m["params"], line)
		}
		return b.openBound(b.cursorVar(m), line)
	case "PLpgSQL_stmt_fetch":
		if into, ok := m["target"]; ok {
			var cols []Column
			if cv := b.cursorVar(m); cv != nil {
				if err := b.openBound(cv, line); err != nil {
					return err
				}
				cols = cv.cursorCols
			}
			b.fillRecord(into, cols)
		}
		return nil
	case "PLpgSQL_stmt_call":
		_, err := b.sql(plQuery(m["expr"]), line)
		return err
	case "PLpgSQL_stmt_getdiag", "PLpgSQL_stmt_close", "PLpgSQL_stmt_commit", "PLpgSQL_stmt_rollback":
		return nil
	}
	// an unknown statement kind: look for nested statements
	for _, inner := range m {
		switch inner.(type) {
		case []any, map[string]any:
			if err := b.walkList(inner); err != nil {
				return err
			}
		}
	}
	return nil
}

// block is PLpgSQL_stmt_block: a BEGIN ... END, optionally with an EXCEPTION clause. A
// protected body's own violations and RAISEs are, by themselves, real ways the block can
// fail -- unless a WHEN clause in the block's own EXCEPTION list catches that SQLSTATE,
// in which case PostgreSQL's per-block subtransaction swallows it before it ever reaches
// the caller (see B1 in adv_plpgsql_test.go). So the body is walked first as usual, then
// whatever it newly added to b.out.Violations / b.raises is filtered against the union of
// every handler's WHEN conditions: caught codes are removed (they cannot propagate),
// anything left over stays. The handler bodies are then walked as ordinary statements, so
// whatever they raise themselves is added back on top -- catching an error and raising a
// different one still fails the call.
func (b *plBody) block(m map[string]any) error {
	violAt, raiseAt := len(b.out.Violations), len(b.raises)
	if err := b.walkList(m["body"]); err != nil {
		return err
	}
	ex, ok := m["exceptions"].(map[string]any)
	if !ok {
		return nil
	}
	protectedViol := append([]Violation(nil), b.out.Violations[violAt:]...)
	protectedRaise := append([]schema.RaisedError(nil), b.raises[raiseAt:]...)
	b.out.Violations = b.out.Violations[:violAt]
	b.raises = b.raises[:raiseAt]

	excBlock, _ := ex["PLpgSQL_exception_block"].(map[string]any)
	excList, _ := excBlock["exc_list"].([]any)

	caught := func(code string) bool {
		for _, e := range excList {
			for _, cond := range exceptionConditionNames(e) {
				if sqlstateMatchesCondition(cond, code) {
					return true
				}
			}
		}
		return false
	}
	for _, v := range protectedViol {
		if !caught(v.Code) {
			b.out.Violations = append(b.out.Violations, v)
		}
	}
	for _, r := range protectedRaise {
		if !caught(r.Code) {
			b.raises = append(b.raises, r)
		}
	}

	b.inExcept++
	b.declare(&plVar{name: "sqlstate", typ: ref(catalog.Text)})
	b.declare(&plVar{name: "sqlerrm", typ: ref(catalog.Text)})
	defer func() { b.inExcept-- }()
	for _, e := range excList {
		names := exceptionConditionNames(e)
		var codes []string
		others := false
		for _, n := range names {
			if strings.EqualFold(n, "others") {
				others = true
				continue
			}
			if c := conditionCode(n); c != "" {
				codes = append(codes, c)
			}
		}
		if others {
			// OTHERS catches (and RAISE ... USING ERRCODE = SQLSTATE can only be) whatever
			// the protected body actually raised.
			codes = nil
			seen := map[string]bool{}
			add := func(c string) {
				if c != "" && !seen[c] {
					seen[c] = true
					codes = append(codes, c)
				}
			}
			for _, v := range protectedViol {
				add(v.Code)
			}
			for _, r := range protectedRaise {
				add(r.Code)
			}
		}
		savedHandlerCodes := b.handlerCodes
		b.handlerCodes = codes
		em, _ := e.(map[string]any)
		exc, _ := em["PLpgSQL_exception"].(map[string]any)
		err := b.walkList(exc["action"])
		b.handlerCodes = savedHandlerCodes
		if err != nil {
			return err
		}
	}
	return nil
}

// exceptionConditionNames is the WHEN clause condition names of one exc_list entry
// (PLpgSQL_condition.condname): a built-in condition name, "others", or a bare 5-char
// SQLSTATE from `WHEN SQLSTATE '...'`.
func exceptionConditionNames(e any) []string {
	em, _ := e.(map[string]any)
	exc, _ := em["PLpgSQL_exception"].(map[string]any)
	conds, _ := exc["conditions"].([]any)
	var out []string
	for _, c := range conds {
		cm, _ := c.(map[string]any)
		cond, _ := cm["PLpgSQL_condition"].(map[string]any)
		if name, ok := cond["condname"].(string); ok && name != "" {
			out = append(out, name)
		}
	}
	return out
}

// conditionCode resolves a WHEN clause condition name to its SQLSTATE: a built-in name
// via sqlstateOf, or the bare code itself for `WHEN SQLSTATE 'xxxxx'` (which the PL
// parser also records as a condname, already 5 characters). "" if neither.
func conditionCode(name string) string {
	if c := sqlstateOf(name); c != "" {
		return c
	}
	if len(name) == 5 {
		return strings.ToUpper(name)
	}
	return ""
}

// sqlstateMatchesCondition reports whether a WHEN clause condition (a name sqlstateOf
// knows, "others", or a bare SQLSTATE) catches an error with the given SQLSTATE. PG
// treats a condition name that denotes an entire error class (its code ends in "000",
// e.g. integrity_constraint_violation = 23000) as matching every code in that class by
// its first two characters; a specific condition or a literal SQLSTATE matches exactly.
func sqlstateMatchesCondition(cond, code string) bool {
	if strings.EqualFold(cond, "others") {
		return true
	}
	c := conditionCode(cond)
	if c == "" {
		return false
	}
	if strings.HasSuffix(c, "000") && sqlstateOf(cond) != "" {
		return len(code) >= 2 && code[:2] == c[:2]
	}
	return code == c
}

// cursorVar is the cursor variable a statement's curvar names.
func (b *plBody) cursorVar(m map[string]any) *plVar {
	if n, ok := m["curvar"].(float64); ok && int(n) < len(b.datums) && b.datums[int(n)] != nil {
		return b.datums[int(n)]
	}
	return nil
}

// openBound analyzes a bound cursor's query once and keeps its columns.
func (b *plBody) openBound(cv *plVar, line int) error {
	if cv == nil || cv.cursor == "" || cv.cursorCols != nil {
		return nil
	}
	r, err := b.sql(cv.cursor, line)
	if err != nil {
		return err
	}
	if r != nil {
		cv.cursorCols = r.Columns
	}
	return nil
}

var returnNextRe = regexp.MustCompile(`(?i)\bRETURN\s+NEXT\s+("[^"]+"|[A-Za-z_][A-Za-z0-9_$]*)\s*;`)

// returnNextVar is the bare variable a `RETURN NEXT var;` on the given body line names.
func (b *plBody) returnNextVar(line int) string {
	lines := strings.Split(b.fn.Body, "\n")
	if line < 1 || line > len(lines) {
		return ""
	}
	m := returnNextRe.FindStringSubmatch(lines[line-1])
	if m == nil {
		return ""
	}
	return strings.Trim(m[1], `"`)
}

// retVar is the variable a RETURN / RETURN NEXT of a bare variable names (retvarno).
func (b *plBody) retVar(m map[string]any) *plVar {
	if n, ok := m["retvarno"].(float64); ok && int(n) < len(b.datums) {
		return b.datums[int(n)]
	}
	return nil
}

// retType checks a RETURN of a value of type t against RETURNS.
func (b *plBody) retType(t schema.TypeRef, line int) error {
	switch {
	case b.fn.RetType.OID == catalog.Trigger:
		if t.OID == catalog.Record || (b.rel != nil && t.OID == b.rel.RowType) {
			return nil
		}
		return b.errf(line, codeDatatypeMismatch, "a trigger function returns NEW, OLD or NULL")
	case b.fn.IsProc || b.fn.RetType.OID == catalog.Void:
		return b.errf(line, codeDatatypeMismatch, "RETURN cannot have a parameter in function returning void")
	case b.fn.RetSet:
		return b.errf(line, codeDatatypeMismatch, "RETURN cannot have a parameter in function returning set; use RETURN NEXT or RETURN QUERY")
	}
	want := b.fn.RetType
	if want.OID == catalog.Record || relByRowType(b.s, want.OID) != nil {
		return nil
	}
	return b.assignable(t, want, line, "RETURN")
}

// isUntypedLiteral reports whether an expression is NULL or a string literal: PostgreSQL
// types those from the assignment target, so they assign to anything.
func isUntypedLiteral(q string) bool {
	q = strings.TrimSpace(q)
	return strings.EqualFold(q, "null") || plStringLiteral(q) != ""
}

// plQuery is the SQL text of a PLpgSQL_expr node.
func plQuery(v any) string {
	m, _ := v.(map[string]any)
	e, _ := m["PLpgSQL_expr"].(map[string]any)
	q, _ := e["query"].(string)
	return q
}

// sql analyzes a whole statement of the body.
func (b *plBody) sql(query string, line int) (*Result, error) {
	if strings.TrimSpace(query) == "" {
		return nil, nil
	}
	tree, err := pgparse.Parse(query)
	if err != nil {
		return nil, b.errf(line, codeSyntaxError, "%s", strings.TrimPrefix(err.Error(), "syntax error "))
	}
	if len(tree.Stmts) == 0 {
		return nil, nil
	}
	r, aerr := analyzeStmtIn(b.s, tree.Stmts[0].Stmt, b.scope(), nil, b.visited)
	if aerr != nil {
		if e, ok := aerr.(*Error); ok {
			return nil, b.errf(line, e.Code, "%s", e.Message)
		}
		return nil, b.errf(line, codeSyntaxError, "%v", aerr)
	}
	b.out.Relations = append(b.out.Relations, r.Relations...)
	if r.Facts != nil {
		b.out.Statements = append(b.out.Statements, FunctionStatement{Line: line, Facts: r.Facts})
	}
	for _, n := range r.Notes {
		n.Message = fmt.Sprintf("line %d: %s", line, n.Message)
		n.Position = 0
		b.out.Notes = append(b.out.Notes, n)
	}
	for _, v := range r.Violations {
		if v.Function == "" {
			v.Function = b.fn.Name
		}
		b.out.Violations = append(b.out.Violations, v)
	}
	return r, nil
}

// expr analyzes an expression (`SELECT expr`) and, when want is set, checks that its type
// assigns to want.
func (b *plBody) expr(query string, line int, want schema.TypeRef) (*Result, error) {
	if strings.TrimSpace(query) == "" {
		return nil, nil
	}
	r, err := b.sql("SELECT "+query, line)
	if err != nil || r == nil {
		return r, err
	}
	if want.OID != 0 && len(r.Columns) == 1 && !isUntypedLiteral(query) {
		if err := b.assignable(r.Columns[0].Type, want, line, query); err != nil {
			return nil, err
		}
	}
	return r, nil
}

func (b *plBody) assignable(from, to schema.TypeRef, line int, what string) error {
	if from.OID == catalog.Unknown || to.OID == 0 || to.OID == catalog.Record {
		return nil
	}
	a := &analyzer{s: b.s}
	if !a.canCoerce(from.OID, to.OID, assignmentCoercion) {
		return b.errf(line, codeDatatypeMismatch, "%s is %s, which cannot be assigned to %s", strings.TrimSpace(what), b.s.Types.Format(from), b.s.Types.Format(to))
	}
	return nil
}

var assignSplit = regexp.MustCompile(`^\s*((?:"[^"]+"|[A-Za-z_][A-Za-z0-9_$]*)(?:\s*\.\s*(?:"[^"]+"|[A-Za-z_][A-Za-z0-9_$]*))*)\s*(?:\[[^\]]*\]\s*)*:?=\s*`)

// assign handles `target := expr` (parseMode RAW_PLPGSQL_ASSIGN: the text has both).
func (b *plBody) assign(query string, line int) error {
	m := assignSplit.FindStringSubmatch(query)
	if m == nil {
		_, err := b.expr(query, line, schema.TypeRef{})
		return err
	}
	target := strings.ToLower(strings.NewReplacer(" ", "", "\t", "", `"`, "").Replace(m[1]))
	rhs := query[len(m[0]):]
	var want schema.TypeRef
	parts := strings.Split(target, ".")
	if v := b.vars[parts[0]]; v != nil {
		switch {
		case len(parts) == 1 && strings.Contains(m[0], "["):
			// arr[i] := x: the element type
			if t := b.s.Types.ByOID(v.typ.OID); t != nil && t.Elem != 0 {
				want = ref(t.Elem)
			}
		case len(parts) == 1:
			want = v.typ
		case len(parts) == 2:
			want = b.fieldType(v, parts[1])
		}
		// a record variable assigned a row takes its shape
		if len(parts) == 1 && v.typ.OID == catalog.Record && v.fields == nil {
			r, err := b.expr(rhs, line, schema.TypeRef{})
			if err != nil {
				return err
			}
			if r != nil && len(r.Columns) == 1 && r.Columns[0].Fields != nil {
				v.fields, v.open = plFields(r.Columns[0].Fields), false
			}
			return nil
		}
	}
	_, err := b.expr(rhs, line, want)
	return err
}

// fieldType is the type of v.field, or zero when unknown.
func (b *plBody) fieldType(v *plVar, field string) schema.TypeRef {
	for _, f := range v.fields {
		if f.name == field {
			return f.typ
		}
	}
	if rel := relByRowType(b.s, v.typ.OID); rel != nil {
		if c := rel.Column(field); c != nil {
			return c.Type
		}
	}
	return schema.TypeRef{}
}

// assignInto checks SELECT ... INTO targets: one record / row variable takes the row's
// shape; scalar targets take the columns one to one.
func (b *plBody) assignInto(target any, r *Result, line int) error {
	tm, _ := target.(map[string]any)
	if rec, ok := tm["PLpgSQL_rec"].(map[string]any); ok {
		b.fillRecord(map[string]any{"PLpgSQL_rec": rec}, r.Columns)
		return nil
	}
	if row, ok := tm["PLpgSQL_row"].(map[string]any); ok {
		fields, _ := row["fields"].([]any)
		if len(fields) == 1 {
			fm, _ := fields[0].(map[string]any)
			name, _ := fm["name"].(string)
			if v := b.vars[strings.ToLower(name)]; v != nil && (v.typ.OID == catalog.Record || relByRowType(b.s, v.typ.OID) != nil) {
				// INTO rowvar: the whole row
				if v.typ.OID == catalog.Record {
					v.fields, v.open = plFields(r.Columns), false
				}
				return nil
			}
		}
		if len(fields) != len(r.Columns) {
			return b.errf(line, codeDatatypeMismatch, "INTO has %d target(s) but the query returns %d column(s)", len(fields), len(r.Columns))
		}
		for i, f := range fields {
			fm, _ := f.(map[string]any)
			name, _ := fm["name"].(string)
			v := b.vars[strings.ToLower(name)]
			if v == nil || v.typ.OID == 0 {
				continue
			}
			if v.typ.OID == catalog.Record {
				v.fields, v.open = plFields(r.Columns[i].Fields), r.Columns[i].Fields == nil
				continue
			}
			if err := b.assignable(r.Columns[i].Type, v.typ, line, "INTO "+name+": column "+fmt.Sprint(i+1)); err != nil {
				return err
			}
		}
		return nil
	}
	if v, ok := tm["PLpgSQL_var"].(map[string]any); ok {
		name, _ := v["refname"].(string)
		if pv := b.vars[strings.ToLower(name)]; pv != nil && len(r.Columns) == 1 {
			return b.assignable(r.Columns[0].Type, pv.typ, line, "INTO "+name)
		}
	}
	return nil
}

// fillRecord gives a record variable the shape of cols (nil: unknown, stays open).
func (b *plBody) fillRecord(target any, cols []Column) {
	tm, _ := target.(map[string]any)
	rec, ok := tm["PLpgSQL_rec"].(map[string]any)
	if !ok {
		if row, ok := tm["PLpgSQL_row"].(map[string]any); ok && cols != nil {
			// FOR a, b IN query: scalar loop variables
			fields, _ := row["fields"].([]any)
			for i, f := range fields {
				fm, _ := f.(map[string]any)
				name, _ := fm["name"].(string)
				if v := b.vars[strings.ToLower(name)]; v != nil && v.typ.OID == catalog.Record && i < len(cols) {
					v.fields, v.open = plFields(cols[i].Fields), cols[i].Fields == nil
				}
			}
		}
		return
	}
	name, _ := rec["refname"].(string)
	v := b.vars[strings.ToLower(name)]
	if v == nil {
		v = &plVar{name: name, typ: ref(catalog.Record)}
		b.declare(v)
	}
	if cols == nil {
		v.fields, v.open = nil, true
		return
	}
	v.fields, v.open = plFields(cols), false
}

func plFields(cols []Column) []rteCol {
	out := make([]rteCol, 0, len(cols))
	for _, c := range cols {
		rc := rteCol{name: c.Name, typ: c.Type, nullable: c.Nullable}
		if c.Fields != nil {
			rc.fields = plFields(c.Fields)
		}
		out = append(out, rc)
	}
	return out
}

// ret checks a RETURN against RETURNS.
func (b *plBody) ret(query string, line int) error {
	q := strings.TrimSpace(query)
	if q == "" {
		return nil // RETURN; (the PL parser checks it against the return kind)
	}
	switch {
	case b.fn.RetType.OID == catalog.Trigger:
		switch strings.ToLower(q) {
		case "new", "old", "null":
			return nil
		}
		r, err := b.expr(q, line, schema.TypeRef{})
		if err != nil {
			return err
		}
		if r != nil && len(r.Columns) == 1 && b.rel != nil && r.Columns[0].Type.OID == b.rel.RowType {
			return nil
		}
		return b.errf(line, codeDatatypeMismatch, "a trigger function returns NEW, OLD or NULL")
	case b.fn.IsProc || b.fn.RetType.OID == catalog.Void:
		if q != "" && !strings.EqualFold(q, "null") {
			return b.errf(line, codeDatatypeMismatch, "RETURN cannot have a parameter in function returning void")
		}
		return nil
	case b.fn.RetSet:
		return b.errf(line, codeDatatypeMismatch, "RETURN cannot have a parameter in function returning set; use RETURN NEXT or RETURN QUERY")
	}
	want := b.fn.RetType
	if want.OID == catalog.Record || relByRowType(b.s, want.OID) != nil {
		want = schema.TypeRef{} // a row: the shape check would need the RETURN's record fields
	}
	_, err := b.expr(q, line, want)
	return err
}

// raise records the SQLSTATE a RAISE EXCEPTION throws and checks its expressions.
func (b *plBody) raise(m map[string]any, line int) error {
	if params, ok := m["params"].([]any); ok {
		for _, p := range params {
			if _, err := b.expr(plQuery(p), line, schema.TypeRef{}); err != nil {
				return err
			}
		}
	}
	level, _ := m["elog_level"].(float64)
	if int(level) < 21 { // below ERROR (NOTICE, WARNING, ...): nothing is thrown
		return nil
	}
	codes := []string{"P0001"} // raise_exception
	if cn, ok := m["condname"].(string); ok && cn != "" {
		if c := sqlstateOf(cn); c != "" {
			codes = []string{c}
		}
	}
	unresolved := false
	if opts, ok := m["options"].([]any); ok {
		for _, o := range opts {
			om, _ := o.(map[string]any)
			ro, _ := om["PLpgSQL_raise_option"].(map[string]any)
			t, _ := ro["opt_type"].(float64)
			q := plQuery(ro["expr"])
			if int(t) == 0 { // ERRCODE
				if resolved, ok := b.resolveErrcode(q); ok {
					codes = resolved
				} else {
					unresolved = true
				}
				continue
			}
			if q != "" {
				if _, err := b.expr(q, line, schema.TypeRef{}); err != nil {
					return err
				}
			}
		}
	}
	if unresolved {
		// don't silently default to P0001: that misreports what the RAISE actually
		// throws (a variable / expression ERRCODE, a re-raised SQLSTATE this analyzer
		// could not pin down).
		b.out.Notes = append(b.out.Notes, Note{Code: noteSQLStateDynamic,
			Message: fmt.Sprintf("line %d: RAISE ... USING ERRCODE is not a literal, a variable set once to a literal, or a caught SQLSTATE re-raise, so its SQLSTATE cannot be determined statically", line)})
		return nil
	}
	for _, code := range codes {
		b.addRaise(code)
	}
	return nil
}

// addRaise records that the body can throw code (deduplicated), naming it from the
// function's own `-- sqlshape: error` annotations when one exists for that code -- the
// same bookkeeping RAISE itself does, reused by the implicit SQLSTATEs PL/pgSQL statements
// throw on their own (CASE_NOT_FOUND, no_data_found / too_many_rows, ASSERT, ...).
func (b *plBody) addRaise(code string) {
	name := ""
	for _, r := range b.fn.Raises {
		if r.Code == code {
			name = r.Name
		}
	}
	for _, r := range b.raises {
		if r.Code == code {
			return
		}
	}
	b.raises = append(b.raises, schema.RaisedError{Code: code, Name: name})
}

// checkIntoStrict models the run-time cardinality check an INTO STRICT target does
// (SELECT ... INTO STRICT, static or dynamic): PostgreSQL raises no_data_found (P0002) on
// zero rows and too_many_rows (P0003) on more than one. r.AtMostOne (card.go's proof, via
// analyzeStmtIn) rules out the "too many" half when the query is provably at most one
// row; whether it returns at least one row is not proved here, so P0002 always stays
// possible (see A2 in adv_plpgsql_test.go).
func (b *plBody) checkIntoStrict(r *Result) {
	b.addRaise("P0002")
	if !r.AtMostOne {
		b.addRaise("P0003")
	}
}

// identRe matches a bare PL/pgSQL identifier (unquoted): resolveErrcode uses it to tell
// "USING ERRCODE = my_var" from an arbitrary expression it should not try to constant-fold.
var identRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_$]*$`)

// resolveErrcode statically resolves a RAISE ... USING ERRCODE = <q> expression to the
// SQLSTATE(s) it can throw, when possible: a literal ('55000'), a bare reference to a
// variable that was declared with a literal default and is never reassigned (see
// plVar.constVal), or, inside an EXCEPTION handler, the bare SQLSTATE pseudo-variable
// re-raising whatever this handler caught (b.handlerCodes; see block). ok is false when q
// is some other expression -- the caller reports that rather than guessing.
func (b *plBody) resolveErrcode(q string) ([]string, bool) {
	q = strings.TrimSpace(q)
	if lit := plStringLiteral(q); lit != "" {
		if c := sqlstateOf(lit); c != "" {
			return []string{c}, true
		}
		if len(lit) == 5 {
			return []string{strings.ToUpper(lit)}, true
		}
		return nil, false
	}
	if !identRe.MatchString(q) {
		return nil, false
	}
	if strings.EqualFold(q, "sqlstate") {
		if b.inExcept > 0 && len(b.handlerCodes) > 0 {
			return append([]string(nil), b.handlerCodes...), true
		}
		return nil, false
	}
	if v, ok := b.vars[strings.ToLower(q)]; ok && v.constVal != "" {
		if c := sqlstateOf(v.constVal); c != "" {
			return []string{c}, true
		}
		if len(v.constVal) == 5 {
			return []string{strings.ToUpper(v.constVal)}, true
		}
	}
	return nil, false
}

// plStringLiteral unquotes a single-quoted SQL string literal, "" if q is not one.
func plStringLiteral(q string) string {
	q = strings.TrimSpace(q)
	if len(q) < 2 || q[0] != '\'' || q[len(q)-1] != '\'' {
		return ""
	}
	inner := q[1 : len(q)-1]
	if strings.Contains(strings.ReplaceAll(inner, "''", ""), "'") {
		return ""
	}
	return strings.ReplaceAll(inner, "''", "'")
}

// sqlstateOf maps a condition name to its SQLSTATE (the names RAISE accepts).
func sqlstateOf(name string) string {
	switch strings.ToLower(name) {
	case "raise_exception":
		return "P0001"
	case "no_data_found":
		return "P0002"
	case "too_many_rows":
		return "P0003"
	case "assert_failure":
		return "P0004"
	case "unique_violation":
		return "23505"
	case "foreign_key_violation":
		return "23503"
	case "check_violation":
		return "23514"
	case "not_null_violation":
		return "23502"
	case "restrict_violation":
		return "23001"
	case "integrity_constraint_violation":
		return "23000"
	case "invalid_parameter_value":
		return "22023"
	case "division_by_zero":
		return "22012"
	case "numeric_value_out_of_range":
		return "22003"
	case "insufficient_privilege":
		return "42501"
	case "invalid_text_representation":
		return "22P02"
	case "serialization_failure":
		return "40001"
	case "deadlock_detected":
		return "40P01"
	case "lock_not_available":
		return "55P03"
	case "data_exception":
		return "22000"
	case "invalid_transaction_state":
		return "25000"
	case "feature_not_supported":
		return "0A000"
	case "internal_error":
		return "XX000"
	}
	return ""
}

// dynexecute handles EXECUTE: a constant string is analyzed as the statement it is; a
// string built at run time cannot be checked and gets an advisory note. INTO STRICT
// (into && strict) additionally means P0002 / P0003, the same as a static
// PLpgSQL_stmt_execsql (see A2 in adv_plpgsql_test.go) -- but for a dynamic (non-literal)
// query there is no query to cardinality-prove single, so both stay possible.
func (b *plBody) dynexecute(m map[string]any, line int) error {
	into, _ := m["into"].(bool)
	strict, _ := m["strict"].(bool)
	q := plQuery(m["query"])
	if lit := plStringLiteral(q); lit != "" {
		r, err := b.sql(lit, line)
		if err != nil {
			return err
		}
		if ps, ok := m["params"].([]any); ok {
			for _, p := range ps {
				if _, err := b.expr(plQuery(p), line, schema.TypeRef{}); err != nil {
					return err
				}
			}
		}
		if into && strict && r != nil {
			b.checkIntoStrict(r)
		}
		return nil
	}
	if into && strict {
		b.addRaise("P0002")
		b.addRaise("P0003")
	}
	return b.dynexecuteText(q, m["params"], line)
}

func (b *plBody) dynexecuteText(q string, params any, line int) error {
	if lit := plStringLiteral(q); lit != "" {
		r, err := b.sql(lit, line)
		if err != nil {
			return err
		}
		_ = r
		if ps, ok := params.([]any); ok {
			for _, p := range ps {
				if _, err := b.expr(plQuery(p), line, schema.TypeRef{}); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if _, err := b.expr(q, line, ref(catalog.Text)); err != nil {
		return err
	}
	if ps, ok := params.([]any); ok {
		for _, p := range ps {
			if _, err := b.expr(plQuery(p), line, schema.TypeRef{}); err != nil {
				return err
			}
		}
	}
	b.out.Notes = append(b.out.Notes, Note{Code: notePLDynamicSQL, Message: fmt.Sprintf("line %d: EXECUTE runs SQL built at run time, which is not checked (a constant string would be)", line)})
	return nil
}

func (b *plBody) violations() []Violation {
	return b.out.Violations
}

// --- analyzer hooks -------------------------------------------------------------

// isComposite reports whether oid is a composite (row) type.
func (a *analyzer) isComposite(oid catalog.OID) bool {
	t := a.typ(oid)
	return t != nil && t.Kind == 'c'
}

// plField resolves var.field for a PL/pgSQL variable: a record's field by name, a row
// variable's column, or unknown for an open record. (nil, nil) when tbl is not a variable.
func (a *analyzer) plField(tbl, field string, loc int32) (*expr, *Error) {
	for _, p := range a.funcParams {
		if !p.plVar || !strings.EqualFold(p.name, tbl) {
			continue
		}
		if p.fields != nil {
			for _, f := range p.fields {
				if f.name == field {
					return &expr{typ: f.typ, nullable: true, fields: f.fields}, nil
				}
			}
			if len(p.fields) == 0 {
				return &expr{typ: ref(catalog.Unknown), nullable: true}, nil // open record
			}
			return nil, errAt(codeUndefinedColumn, loc, "record %q has no field %q", tbl, field)
		}
		if rel := a.relByRowType(p.typ.OID); rel != nil {
			if col := rel.Column(field); col != nil {
				return &expr{typ: col.Type, nullable: true}, nil
			}
			return nil, errAt(codeUndefinedColumn, loc, "column %q of row variable %q does not exist", field, tbl)
		}
		if p.typ.OID == catalog.Record {
			return &expr{typ: ref(catalog.Unknown), nullable: true}, nil
		}
		return nil, errAt(codeUndefinedColumn, loc, "%q is not a row or record variable", tbl)
	}
	return nil, nil
}
