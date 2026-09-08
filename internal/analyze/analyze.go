package analyze

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"google.golang.org/protobuf/proto"

	"github.com/kr9ly/sqlshape/internal/catalog"
	"github.com/kr9ly/sqlshape/internal/facts"
	"github.com/kr9ly/sqlshape/internal/pgparse"
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
	// viewDefault: view target columns carrying the view's own default (ALTER VIEW ... SET
	// DEFAULT), which makes DEFAULT a non-DEFAULT value for the base column
	viewDefault map[*schema.Column]bool
	// viewBase: the base-table column a view target column stands for (two view columns
	// over one base column may not both be assigned)
	viewBase map[*schema.Column]*schema.Column
	writeCmd string
	// inMerge is set while analyzing a MERGE (merge_action() is only valid there)
	inMerge bool
	// mergeWhen is set while analyzing a MERGE WHEN condition (no system columns there)
	mergeWhen bool
	// mergeScope is the target + source level of a MERGE (its ON), for the One proof
	mergeScope *scope
	// probing is set while a column is resolved only to see whether it resolves (GROUP BY
	// alias rules): no use is recorded
	probing bool
	// opAmbiguous is set by resolveOperator when more than one candidate fits equally
	opAmbiguous bool
	// polyErr is a specific error resolvePolymorphic leaves behind a false return
	polyErr *Error
	// inDMLCTE is set while a data-modifying WITH item is analyzed
	inDMLCTE bool
	// lastResolvedScope is the scope the last resolveColumn found its column in
	lastResolvedScope *scope
	// aggFrames are the aggregate calls whose arguments are being analyzed, innermost
	// last (check_agg_arguments: an aggregate's level is its arguments' nearest level)
	aggFrames []*aggFrame
	// inAggArgs is the depth of aggregate calls whose arguments are being analyzed
	// (aggregates do not nest)
	inAggArgs int
	// keepUnknown leaves unknown-typed output columns of the next selectStmt unresolved:
	// INSERT ... SELECT types them by the target columns
	keepUnknown bool
	refs        []RelationRef
	// inReturning: analyzing a RETURNING list (its rows are the ones just written)
	inReturning bool
	uses        []Use
	useSeen     map[string]int
	readUses    map[string]bool // uses that are reads (not only write targets), by key
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
	// srfBan names the clause being analyzed when set-returning functions are not allowed
	// in it (COALESCE, UPDATE, RETURNING, VALUES, LIMIT, aggregate / window arguments);
	// srfBanNext hands a ban to the next selectStmt (RETURNING is analyzed as one).
	srfBan, srfBanNext string
	// inInsertValues: the VALUES being analyzed is an INSERT's source (SRFs allowed there)
	inInsertValues bool
	// selectDepth counts the SELECTs being analyzed: a data-modifying WITH is legal only at 0
	selectDepth int
	// calledFuncs are the user functions the statement calls (with their arguments): their
	// bodies' failure modes are the statement's too
	calledFuncs []calledFunc
	lastCatFunc *catalog.Func
	// funcParams: when analyzing a SQL function body, its parameters (by name and position)
	funcParams []funcParam
	// funcVolatility remembers the volatility of each resolved function call (card.go)
	funcVolatility map[*pgparse.FuncCall]byte
	// dmlCTEs are the data-modifying statements inside WITH (their failure modes count)
	dmlCTEs []*pgparse.Node
	// waived are the obligations the statement opts out of, by table (`-- sqlshape:
	// unfiltered t1, t2` and `-- sqlshape: waive t1 pinned(x), t2` in the SQL text; see
	// schema.Relation.Waived for the spec strings). Carried on the facts, judged by
	// internal/obligation
	waived map[string][]string
	// facts.go: the levels recorded for internal/obligation, the last DML target, and the
	// view bodies already converted
	factScopes     []factScope
	writeRecs      []writeRec
	writeLeaf      *rte
	viewFactScopes map[*schema.Relation]*facts.Scope
	// scopeSel is the SELECT a query level analyzes (set before its facts are recorded);
	// factBySel / factOutBySel are the facts of that level and the leaf columns its target
	// list projects plainly, so a parent level can attach an EXISTS / IN subquery's body
	// to its own predicate; claimed marks the bodies so attached
	scopeSel     map[*scope]*pgparse.SelectStmt
	factBySel    map[*pgparse.SelectStmt]*facts.Scope
	factOutBySel map[*pgparse.SelectStmt][]*facts.ColRef
	claimed      map[*facts.Scope]bool
}

// Analyze analyzes exactly one SQL statement against s.
// A statement PG would reject returns an *Error; other failures are ordinary errors.
// Load parses schema DDL the way schema.Load does and freezes every view's output columns
// as the view is created (PG fixes them at CREATE VIEW; see schema.Relation.Frozen). Use
// this rather than schema.Load for a schema the analyzer will read.
func Load(schemaSQL string) (*schema.Schema, error) {
	cat, err := catalog.Load()
	if err != nil {
		return nil, err
	}
	return LoadWith(cat, schemaSQL)
}

// LoadWith is Load with an explicit catalog.
func LoadWith(cat *catalog.Catalog, schemaSQL string) (*schema.Schema, error) {
	return schema.LoadWithHooks(cat, schemaSQL, freezeView, refreezeDependentNullability)
}

// freezeView is the ViewHook: the view body analyzed against the schema as it stands now,
// building rel.Frozen from scratch (this is the one place the column list itself -- name,
// count, order -- is decided; PG fixes those at CREATE VIEW and nothing later reopens
// them). refreezeDependentNullability (the NotNullHook) later revisits Nullable in
// place, but never the shape freezeView established here.
func freezeView(s *schema.Schema, rel *schema.Relation) {
	a := newAnalyzer(s, nil, nil)
	cols, err := a.viewColumns(rel)
	if err != nil {
		return // the reader will report the body's problem
	}
	rel.Frozen = make([]schema.ViewColumn, len(cols))
	for i, c := range cols {
		rel.Frozen[i] = viewColumnOf(a, c)
	}
}

// viewColumnOf builds one schema.ViewColumn from a resolved view output column.
func viewColumnOf(a *analyzer, c rteCol) schema.ViewColumn {
	vc := schema.ViewColumn{Name: c.name, Type: c.typ, Nullable: c.nullable}
	if cv := c.coll.asVar(); cv.strength == collImplicit {
		vc.Collation = cv.name
	}
	if c.src != nil {
		vc.SrcTable, vc.SrcColumn = c.src.Table, c.src.Column
		if br := a.relByFullName(c.src.Table); br != nil {
			vc.SrcRel = br
			vc.Src = br.Column(c.src.Column) // nil when the base is a view
		}
	}
	return vc
}

// refreezeDependentNullability is the NotNullHook: it runs whenever a table's column NOT
// NULL changes (ALTER COLUMN SET/DROP NOT NULL, or a PRIMARY KEY added after the fact),
// passing that table as rel. PG does not freeze a view's nullability at CREATE VIEW the
// way it freezes the column list -- SELECT through a view rechecks attnotnull on the
// referenced column live, on every query -- so every (plain, not materialized) view that
// depends on rel, transitively, is re-analyzed in declaration order and has its existing
// Frozen columns' Nullable refreshed in place. Type stays frozen too: PG refuses to alter
// the type of a column a view depends on (0A000), so a type that differs here is a schema
// PG would not have accepted, not a change to follow. The column list itself (name,
// count, order) is never touched here: PG really does freeze that part, and re-deriving
// it from a fresh analysis risks disagreeing with the frozen shape for unrelated reasons
// (e.g. a later, unrelated schema change the view's SELECT * would now expand
// differently) that have nothing to do with why this hook ran.
//
// Declaration order matters for a view-of-view: DependentViews returns bases before the
// views built on them, so by the time a nested view is refrozen, viewColumns resolving
// its FROM already reads the inner view's just-updated rel.Frozen (relationRTE's `case
// schema.View, schema.MatView` path takes the frozen branch of viewColumns when
// rel.Frozen != nil).
func refreezeDependentNullability(s *schema.Schema, rel *schema.Relation) {
	for _, v := range s.DependentViews(rel) {
		if v.Kind != schema.View || v.Frozen == nil {
			// a materialized view is its own physical snapshot (PG never copies attnotnull
			// into one; confirmed against a real embedded PG), so it keeps whatever was
			// frozen. A view whose body never froze (freezeView bailed on an error) has
			// nothing to refresh.
			continue
		}
		a := newAnalyzer(s, nil, nil)
		cols, err := freshViewColumns(a, v)
		if err != nil || len(cols) != len(v.Frozen) {
			// the body no longer resolves, or the column count somehow disagrees with what
			// was frozen (should not happen: nothing about the view's own query changed) --
			// either way, leave the last-good Frozen alone rather than risk a bad rewrite.
			continue
		}
		for i, c := range cols {
			if v.Frozen[i].Name != c.name {
				continue // defensive: names are supposed to stay fixed too
			}
			v.Frozen[i].Nullable = c.nullable
		}
	}
}

func Analyze(s *schema.Schema, sql string) (*Result, error) {
	tree, err := s.Version.Parse(sql)
	if err != nil {
		return nil, &Error{Code: codeSyntaxError, Message: strings.TrimPrefix(err.Error(), "syntax error ")}
	}
	if len(tree.Stmts) != 1 {
		return nil, fmt.Errorf("expected exactly one statement, got %d", len(tree.Stmts))
	}
	r, aerr := analyzeStmt(s, tree.Stmts[0].Stmt, nil, waivers(sql))
	return r, aerr
}

var sqlDirective = regexp.MustCompile(`(?m)^[ \t]*--[ \t]*sqlshape:[ \t]*([a-z ]+?)[ \t]+(.+?)[ \t]*$`)

// waivers reads the statement's opt-outs: `-- sqlshape: unfiltered a, b` (the predicate
// obligations of a and b) and `-- sqlshape: waive a pinned(x), b` (one named obligation,
// or every obligation, of a table).
func waivers(sql string) map[string][]string {
	out := map[string][]string{}
	for _, m := range sqlDirective.FindAllStringSubmatch(sql, -1) {
		switch m[1] {
		case "unfiltered":
			for _, it := range strings.Split(m[2], ",") {
				if it = strings.TrimSpace(it); it != "" {
					out = schema.AddWaiver(out, it, "unfiltered")
				}
			}
		case "waive":
			for table, spec := range schema.Waivers(m[2]) {
				out = schema.AddWaiver(out, table, spec...)
			}
		}
	}
	return out
}

// funcParam is a SQL-function parameter visible in its body by name and as $n.
type funcParam struct {
	name string
	typ  schema.TypeRef
	// plVar marks a PL/pgSQL variable: not addressable as $n, and an unqualified name
	// that is both a variable and a column is ambiguous (plpgsql.variable_conflict =
	// error, the default), where a SQL function's parameter loses to the column.
	plVar bool
	// fields is a record variable's shape when its type is not a named composite: the
	// columns of the query that fills it. Nil for scalars and %ROWTYPE variables.
	fields []rteCol
}

// newAnalyzer is a fresh analyzer over s; fp are the enclosing function's parameters.
func newAnalyzer(s *schema.Schema, fp []funcParam, waived map[string][]string) *analyzer {
	a := &analyzer{
		s:              s,
		params:         map[int32]catalog.OID{},
		paramSrc:       map[int32]*Source{},
		viewCache:      map[*schema.Relation][]rteCol{},
		viewTargets:    map[*schema.Relation]*schema.Relation{},
		viewComputed:   map[*schema.Column]string{},
		viewDefault:    map[*schema.Column]bool{},
		viewBase:       map[*schema.Column]*schema.Column{},
		viewScopes:     map[*schema.Relation]*subquery{},
		viewBusy:       map[*schema.Relation]bool{},
		viewFactScopes: map[*schema.Relation]*facts.Scope{},
		scopeSel:       map[*scope]*pgparse.SelectStmt{},
		factBySel:      map[*pgparse.SelectStmt]*facts.Scope{},
		factOutBySel:   map[*pgparse.SelectStmt][]*facts.ColRef{},
		claimed:        map[*facts.Scope]bool{},
		funcParams:     fp,
		funcVolatility: map[*pgparse.FuncCall]byte{},
		waived:         waived,
	}
	for i, p := range fp {
		if !p.plVar {
			a.params[int32(i+1)] = p.typ.OID
		}
	}
	return a
}

// analyzeStmt analyzes one parsed statement; fp are the enclosing function's parameters.
func analyzeStmt(s *schema.Schema, stmt *pgparse.Node, fp []funcParam, waived map[string][]string) (*Result, error) {
	return analyzeStmtIn(s, stmt, fp, waived, map[*schema.Function]bool{})
}

// analyzeStmtIn is analyzeStmt inside a function body: visited holds the functions on
// the call chain so a recursive SQL function terminates.
func analyzeStmtIn(s *schema.Schema, stmt *pgparse.Node, fp []funcParam, waived map[string][]string, visited map[*schema.Function]bool) (*Result, error) {
	tree := &pgparse.ParseResult{Stmts: []*pgparse.RawStmt{{Stmt: stmt}}}
	a := newAnalyzer(s, fp, waived)
	sc := newScope(nil)
	var cols []rteCol
	var aerr *Error
	switch st := tree.Stmts[0].Stmt.Node.(type) {
	case *pgparse.Node_SelectStmt:
		cols, aerr = a.selectStmt(st.SelectStmt, sc)
		if st.SelectStmt.IntoClause != nil {
			cols = nil // SELECT INTO creates a table and returns no rows
		}
	case *pgparse.Node_InsertStmt:
		cols, aerr = a.insertStmt(st.InsertStmt, sc)
	case *pgparse.Node_UpdateStmt:
		cols, aerr = a.updateStmt(st.UpdateStmt, sc)
	case *pgparse.Node_DeleteStmt:
		cols, aerr = a.deleteStmt(st.DeleteStmt, sc)
	case *pgparse.Node_CallStmt:
		cols, aerr = a.callStmt(st.CallStmt, sc)
	case *pgparse.Node_MergeStmt:
		cols, aerr = a.mergeStmt(st.MergeStmt, sc)
	case *pgparse.Node_TruncateStmt:
		for _, rv := range st.TruncateStmt.Relations {
			rel, r, err := a.targetRTE(rv.GetRangeVar(), sc)
			if err != nil {
				return nil, err
			}
			// every row goes: a delete of the whole table, for the obligations
			a.writeRecs = append(a.writeRecs, writeRec{rel: rel, r: r, cmd: "delete from"})
		}
	case *pgparse.Node_LockStmt:
		for _, rv := range st.LockStmt.Relations {
			if _, _, err := a.targetRTE(rv.GetRangeVar(), sc); err != nil {
				return nil, err
			}
		}
	case *pgparse.Node_RefreshMatViewStmt:
		rel, _, err := a.targetRTE(st.RefreshMatViewStmt.Relation, sc)
		if err != nil {
			return nil, err
		}
		if rel.Kind != schema.MatView {
			return nil, errAt(codeWrongObjectType, st.RefreshMatViewStmt.Relation.Location, "%q is not a materialized view", rel.Name)
		}
	case *pgparse.Node_NotifyStmt, *pgparse.Node_ListenStmt, *pgparse.Node_UnlistenStmt,
		*pgparse.Node_VariableSetStmt, *pgparse.Node_DiscardStmt:
		// no parameters, no result
	case *pgparse.Node_VariableShowStmt:
		cols = []rteCol{{name: st.VariableShowStmt.Name, typ: ref(catalog.Text)}}
	case *pgparse.Node_TransactionStmt, *pgparse.Node_DoStmt, *pgparse.Node_ClosePortalStmt, *pgparse.Node_CheckPointStmt:
		// no parameters, no result
	case *pgparse.Node_VacuumStmt:
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
	case *pgparse.Node_CopyStmt:
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
	case *pgparse.Node_DeclareCursorStmt:
		if _, err := a.subStatement(st.DeclareCursorStmt.Query, sc); err != nil {
			return nil, err
		}
	case *pgparse.Node_CreateTableAsStmt:
		if _, err := a.subStatement(st.CreateTableAsStmt.Query, sc); err != nil {
			return nil, err
		}
	case *pgparse.Node_CreateStmt:
		// CREATE [TEMP] TABLE from application code: nothing to type; the checker cannot see
		// the table in later statements (declare it in schema.sql for that)
	case *pgparse.Node_FetchStmt:
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
	sort.SliceStable(a.uses, func(i, j int) bool { return a.uses[i].Position < a.uses[j].Position })
	res.Uses = a.uses
	for _, as := range a.assigned {
		if as.rel != nil {
			a.fixed = append(a.fixed, Source{Table: as.rel.FullName(), Column: as.col.Name, NotNull: as.col.NotNull, Assigned: true})
		}
	}
	res.Fixed = a.fixed
	res.Facts = a.buildFacts(tree.Stmts[0].Stmt, sc)
	if res.Facts != nil {
		res.Facts.AtMostOne = res.AtMostOne
	}
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
func (a *analyzer) subStatement(n *pgparse.Node, sc *scope) ([]rteCol, *Error) {
	switch st := n.Node.(type) {
	case *pgparse.Node_SelectStmt:
		return a.selectStmt(st.SelectStmt, newScope(sc))
	case *pgparse.Node_InsertStmt:
		return a.insertStmt(st.InsertStmt, newScope(sc))
	case *pgparse.Node_UpdateStmt:
		return a.updateStmt(st.UpdateStmt, newScope(sc))
	case *pgparse.Node_DeleteStmt:
		return a.deleteStmt(st.DeleteStmt, newScope(sc))
	}
	return nil, errAt(codeFeatureNotSupported, -1, "unsupported nested statement %T", n.Node)
}

func init() {
	// INSERT in schema.sql (seed rows): typed like any statement against the schema so far
	// so, and its certain failures (a NOT NULL column left out) and domain / policy
	// findings are problems too
	schema.CheckStatement = func(s *schema.Schema, stmt *pgparse.Node) error {
		r, err := analyzeStmt(s, stmt, nil, nil)
		if err != nil {
			return err
		}
		for _, n := range r.Notes {
			switch n.Code {
			case noteAlwaysFails, noteDomainMismatch:
				return errors.New(n.Message)
			}
		}
		return nil
	}
	// PARTITION BY (expr): ComputePartitionAttrs' rules on the key expression
	schema.PartitionKeyProblem = func(s *schema.Schema, rel *schema.Relation, expr *pgparse.Node) string {
		a := newAnalyzer(s, nil, nil)
		switch {
		case a.aggregateIn(expr) != nil:
			return "aggregate functions are not allowed in partition key expressions"
		case windowIn(expr) != nil:
			return "window functions are not allowed in partition key expressions"
		case containsSubLink(expr):
			return "cannot use subquery in partition key expression"
		case a.srfIn(expr):
			return "set-returning functions are not allowed in partition key expressions"
		}
		r, err := a.relationRTE(rel, nil, -1)
		if err != nil {
			return ""
		}
		sc := newScope(nil)
		sc.items = []*rte{r}
		if _, err := a.analyzeExpr(expr, sc); err != nil {
			return err.Message
		}
		for f, vol := range a.funcVolatility {
			_ = f
			if vol != 'i' {
				return "functions in partition key expression must be marked IMMUTABLE"
			}
		}
		if !hasColumnRef(expr) {
			return "cannot use constant expression as partition key"
		}
		return ""
	}
	// CREATE TABLE AS / SELECT INTO in schema.sql: the loader asks the analyzer for the
	// query's columns
	schema.QueryColumns = func(s *schema.Schema, query *pgparse.Node) ([]*schema.Column, error) {
		if sel := query.GetSelectStmt(); sel != nil && sel.IntoClause != nil {
			cp := proto.Clone(sel).(*pgparse.SelectStmt)
			cp.IntoClause = nil
			query = &pgparse.Node{Node: &pgparse.Node_SelectStmt{SelectStmt: cp}}
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
