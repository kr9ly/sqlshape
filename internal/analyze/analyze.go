package analyze

import (
	"fmt"
	"regexp"
	"strings"

	pg_query "github.com/pganalyze/pg_query_go/v6"
	"google.golang.org/protobuf/proto"

	"github.com/kr9ly/sqlshape/internal/catalog"
	"github.com/kr9ly/sqlshape/internal/schema"
)

type analyzer struct {
	s        *schema.Schema
	dts      *dtSession // datetime GUC view, built lazily from s
	params   map[int32]catalog.OID
	paramSrc map[int32]*Source
	maxParam int32
	notes    []Note
	assigned []assignment // values stored into columns (violation.go)
	// MERGE: which actions its WHEN clauses take, and the columns its INSERT actions fill
	mergeActions  int
	mergeInserted []string

	viewCache  map[*schema.Relation][]rteCol
	viewScopes map[*schema.Relation]*subquery
	viewBusy   map[*schema.Relation]bool
	// insertSelScope is the scope of INSERT ... SELECT's query (for cardinality)
	insertSelScope *scope
	// lastFuncRetSet is whether the most recent funcCall resolved a set-returning function
	lastFuncRetSet bool
	// inCall is set while analyzing the FuncCall of a CALL statement (procedures allowed)
	inCall bool
	// writes through views (viewdml.go): the relation each view resolves to, the columns a
	// view computes (not writable, keyed by the synthetic column), and the current command
	viewTargets  map[*schema.Relation]*schema.Relation
	viewComputed map[*schema.Column]string
	// viewBase: the base-table column a view target column stands for (two view columns
	// over one base column may not both be assigned)
	viewBase map[*schema.Column]*schema.Column
	writeCmd string
	// inMerge is set while analyzing a MERGE (merge_action() is only valid there)
	inMerge bool
	// inAggArgs is the depth of aggregate calls whose arguments are being analyzed
	// (aggregates do not nest)
	inAggArgs int
	// keepUnknown leaves unknown-typed output columns of the next selectStmt unresolved:
	// INSERT ... SELECT types them by the target columns
	keepUnknown bool
	refs        []RelationRef
	fixed       []Source
	// inView is the depth of view definitions being analyzed: their references are the
	// view's, not the statement's
	inView int
	// lastUserFunc is the user function resolved by the most recent funcCall (for RETURNS TABLE columns)
	lastUserFunc *schema.Function
	// lastCallArgs / lastCallActual: the declared and actual argument types of the most
	// recent funcCall, so polymorphic OUT parameters can be resolved for it
	lastCallArgs, lastCallActual []catalog.OID
	// inFuncArgs / inCase / inFromFunc: nesting a set-returning function is refused there
	inFuncArgs, inCase int
	inFromFunc         bool
	// calledFuncs are the user functions the statement calls (with their arguments): their
	// bodies' failure modes are the statement's too
	calledFuncs []calledFunc
	lastCatFunc *catalog.Func
	// funcParams: when analyzing a SQL function body, its parameters (by name and position)
	funcParams []funcParam
	// funcVolatility remembers the volatility of each resolved function call (card.go)
	funcVolatility map[*pg_query.FuncCall]byte
	// dmlCTEs are the data-modifying statements inside WITH (their failure modes count)
	dmlCTEs []*pg_query.Node
	// unfiltered are tables the template exempts from their visibility policy
	// (`-- sqlshape: unfiltered t1, t2` in the SQL text)
	unfiltered map[string]bool
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
	r, aerr := analyzeStmt(s, tree.Stmts[0].Stmt, nil, sqlDirectives(sql, "unfiltered"))
	return r, aerr
}

var sqlDirective = regexp.MustCompile(`(?m)^[ \t]*--[ \t]*sqlshape:[ \t]*([a-z ]+?)[ \t]+(.+?)[ \t]*$`)

// sqlDirectives reads the comma-separated items of `-- sqlshape: <verb> a, b` lines in a statement.
func sqlDirectives(sql, verb string) map[string]bool {
	out := map[string]bool{}
	for _, m := range sqlDirective.FindAllStringSubmatch(sql, -1) {
		if m[1] != verb {
			continue
		}
		for _, it := range strings.Split(m[2], ",") {
			if it = strings.TrimSpace(it); it != "" {
				out[it] = true
			}
		}
	}
	return out
}

// funcParam is a SQL-function parameter visible in its body by name and as $n.
type funcParam struct {
	name string
	typ  schema.TypeRef
}

// analyzeStmt analyzes one parsed statement; fp are the enclosing function's parameters.
func analyzeStmt(s *schema.Schema, stmt *pg_query.Node, fp []funcParam, unfiltered map[string]bool) (*Result, error) {
	return analyzeStmtIn(s, stmt, fp, unfiltered, map[*schema.Function]bool{})
}

// analyzeStmtIn is analyzeStmt inside a function body: visited holds the functions on
// the call chain so a recursive SQL function terminates.
func analyzeStmtIn(s *schema.Schema, stmt *pg_query.Node, fp []funcParam, unfiltered map[string]bool, visited map[*schema.Function]bool) (*Result, error) {
	tree := &pg_query.ParseResult{Stmts: []*pg_query.RawStmt{{Stmt: stmt}}}
	a := &analyzer{
		s:              s,
		params:         map[int32]catalog.OID{},
		paramSrc:       map[int32]*Source{},
		viewCache:      map[*schema.Relation][]rteCol{},
		viewTargets:    map[*schema.Relation]*schema.Relation{},
		viewComputed:   map[*schema.Column]string{},
		viewBase:       map[*schema.Column]*schema.Column{},
		viewScopes:     map[*schema.Relation]*subquery{},
		viewBusy:       map[*schema.Relation]bool{},
		funcParams:     fp,
		funcVolatility: map[*pg_query.FuncCall]byte{},
		unfiltered:     unfiltered,
	}
	for i, p := range fp {
		a.params[int32(i+1)] = p.typ.OID
	}
	sc := newScope(nil)
	var cols []rteCol
	var aerr *Error
	switch st := tree.Stmts[0].Stmt.Node.(type) {
	case *pg_query.Node_SelectStmt:
		cols, aerr = a.selectStmt(st.SelectStmt, sc)
		if st.SelectStmt.IntoClause != nil {
			cols = nil // SELECT INTO creates a table and returns no rows
		}
	case *pg_query.Node_InsertStmt:
		cols, aerr = a.insertStmt(st.InsertStmt, sc)
	case *pg_query.Node_UpdateStmt:
		cols, aerr = a.updateStmt(st.UpdateStmt, sc)
	case *pg_query.Node_DeleteStmt:
		cols, aerr = a.deleteStmt(st.DeleteStmt, sc)
	case *pg_query.Node_CallStmt:
		cols, aerr = a.callStmt(st.CallStmt, sc)
	case *pg_query.Node_MergeStmt:
		cols, aerr = a.mergeStmt(st.MergeStmt, sc)
	case *pg_query.Node_TruncateStmt:
		for _, rv := range st.TruncateStmt.Relations {
			if _, _, err := a.targetRTE(rv.GetRangeVar(), sc); err != nil {
				return nil, err
			}
		}
	case *pg_query.Node_LockStmt:
		for _, rv := range st.LockStmt.Relations {
			if _, _, err := a.targetRTE(rv.GetRangeVar(), sc); err != nil {
				return nil, err
			}
		}
	case *pg_query.Node_RefreshMatViewStmt:
		rel, _, err := a.targetRTE(st.RefreshMatViewStmt.Relation, sc)
		if err != nil {
			return nil, err
		}
		if rel.Kind != schema.MatView {
			return nil, errAt(codeWrongObjectType, st.RefreshMatViewStmt.Relation.Location, "%q is not a materialized view", rel.Name)
		}
	case *pg_query.Node_NotifyStmt, *pg_query.Node_ListenStmt, *pg_query.Node_UnlistenStmt,
		*pg_query.Node_VariableSetStmt, *pg_query.Node_DiscardStmt:
		// no parameters, no result
	case *pg_query.Node_VariableShowStmt:
		cols = []rteCol{{name: st.VariableShowStmt.Name, typ: ref(catalog.Text)}}
	case *pg_query.Node_TransactionStmt, *pg_query.Node_DoStmt, *pg_query.Node_ClosePortalStmt, *pg_query.Node_CheckPointStmt:
		// no parameters, no result
	case *pg_query.Node_VacuumStmt:
		for _, rn := range st.VacuumStmt.Rels {
			vr := rn.GetVacuumRelation()
			rel, _, err := a.targetRTE(vr.Relation, sc)
			if err != nil {
				return nil, err
			}
			for _, cn := range vr.VaCols {
				if rel.Column(cn.GetString_().GetSval()) == nil {
					return nil, errAt(codeUndefinedColumn, vr.Relation.Location, "column %q of relation %q does not exist", cn.GetString_().GetSval(), rel.Name)
				}
			}
		}
	case *pg_query.Node_CopyStmt:
		cp := st.CopyStmt
		if cp.Relation != nil {
			rel, _, err := a.targetRTE(cp.Relation, sc)
			if err != nil {
				return nil, err
			}
			for _, cn := range cp.Attlist {
				if rel.Column(cn.GetString_().GetSval()) == nil {
					return nil, errAt(codeUndefinedColumn, cp.Relation.Location, "column %q of relation %q does not exist", cn.GetString_().GetSval(), rel.Name)
				}
			}
			if cp.WhereClause != nil {
				_, target, err := a.targetRTE(cp.Relation, sc)
				if err != nil {
					return nil, err
				}
				wsc := newScope(sc)
				wsc.items = []*rte{target}
				if err := a.boolClause(cp.WhereClause, wsc, "WHERE"); err != nil {
					return nil, err
				}
			}
		} else if cp.Query != nil {
			if _, err := a.subStatement(cp.Query, sc); err != nil {
				return nil, err
			}
		}
	case *pg_query.Node_DeclareCursorStmt:
		if _, err := a.subStatement(st.DeclareCursorStmt.Query, sc); err != nil {
			return nil, err
		}
	case *pg_query.Node_CreateTableAsStmt:
		if _, err := a.subStatement(st.CreateTableAsStmt.Query, sc); err != nil {
			return nil, err
		}
	case *pg_query.Node_CreateStmt:
		// CREATE [TEMP] TABLE from application code: nothing to type; the checker cannot see
		// the table in later statements (declare it in schema.sql for that)
	case *pg_query.Node_FetchStmt:
		return nil, &Error{Code: codeFeatureNotSupported, Message: "FETCH: a cursor's columns are not known statically; read the DECLARE CURSOR query directly"}
	default:
		return nil, &Error{Code: codeFeatureNotSupported, Message: fmt.Sprintf("unsupported statement %T", tree.Stmts[0].Stmt.Node)}
	}
	if aerr != nil {
		return nil, aerr
	}
	res := &Result{}
	if len(fp) > 0 {
		a.maxParam = 0 // a function body's $n are its parameters, not statement parameters
	}
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
	for _, dml := range a.dmlCTEs {
		res.Violations = dedupe(append(res.Violations, a.violations(dml)...))
	}
	// a call into a user function may fail the way its body can: the writes go through
	// functions when the database is the API, and the caller declares their failure modes
	for _, cf := range a.calledFuncs {
		res.Violations = dedupe(append(res.Violations, functionViolations(s, cf, visited)...))
	}
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

// subStatement analyzes a statement nested in another (COPY (query), DECLARE CURSOR ...,
// CREATE TABLE AS ...) and returns its columns.
func (a *analyzer) subStatement(n *pg_query.Node, sc *scope) ([]rteCol, *Error) {
	switch st := n.Node.(type) {
	case *pg_query.Node_SelectStmt:
		return a.selectStmt(st.SelectStmt, newScope(sc))
	case *pg_query.Node_InsertStmt:
		return a.insertStmt(st.InsertStmt, newScope(sc))
	case *pg_query.Node_UpdateStmt:
		return a.updateStmt(st.UpdateStmt, newScope(sc))
	case *pg_query.Node_DeleteStmt:
		return a.deleteStmt(st.DeleteStmt, newScope(sc))
	}
	return nil, errAt(codeFeatureNotSupported, -1, "unsupported nested statement %T", n.Node)
}

func init() {
	// CREATE TABLE AS / SELECT INTO in schema.sql: the loader asks the analyzer for the
	// query's columns
	schema.QueryColumns = func(s *schema.Schema, query *pg_query.Node) ([]*schema.Column, error) {
		if sel := query.GetSelectStmt(); sel != nil && sel.IntoClause != nil {
			cp := proto.Clone(sel).(*pg_query.SelectStmt)
			cp.IntoClause = nil
			query = &pg_query.Node{Node: &pg_query.Node_SelectStmt{SelectStmt: cp}}
		}
		r, err := analyzeStmt(s, query, nil, nil)
		if err != nil {
			return nil, err
		}
		var cols []*schema.Column
		for _, c := range r.Columns {
			cols = append(cols, &schema.Column{Name: c.Name, Type: c.Type, NotNull: !c.Nullable})
		}
		return cols, nil
	}
}
