package factsprobe

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/kr9ly/sqlshape/v2/x/dialect"
	"github.com/kr9ly/sqlshape/v2/x/facts"
)

// Options drive one run.
type Options struct {
	Dialect Dialect
	// Load builds the dialect's analyzer for a schema text (dialect.Lookup's Loader).
	Load func(schemaSQL string) (dialect.Analyzer, error)
	// DB is the server the statements run on; the probe creates and drops its own tables
	// there (fp<pair>_t<n>).
	DB *sql.DB
	// Seed and N: the generator's seed and how many statements to judge.
	Seed int64
	N    int
	// KnownUnreached names the alphabet entries this dialect's run is known not to reach
	// yet, each with why; an entry reached anyway is reported as stale.
	KnownUnreached map[string]string
	// Log receives the generator's own failures (a schema or statement one side rejected,
	// an evaluator disagreeing with the server), the first few of each kind.
	Log func(format string, args ...any)
}

// Report is a run's outcome.
type Report struct {
	Counts     map[string]int
	Findings   []Verdict // distinct by Kind + Detail, minimized
	Hit        map[string]bool
	Unverified map[string]int
	Missed     []string // alphabet entries neither hit nor known unreached
	Stale      []string // known-unreached entries that were hit
}

// Run generates, judges and reports.
func Run(ctx context.Context, o Options) (*Report, error) {
	if o.Log == nil {
		o.Log = func(string, ...any) {}
	}
	r := rand.New(rand.NewSource(o.Seed))
	rep := &Report{Counts: map[string]int{}, Hit: map[string]bool{}, Unverified: map[string]int{}}
	seen := map[string]bool{}
	logged := map[string]int{}
	logf := func(kind, format string, args ...any) {
		rep.Counts[kind]++
		logged[kind]++
		if logged[kind] <= 3 {
			o.Log(kind+": "+format, args...)
		}
	}
	for i := 0; i < o.N; i++ {
		m := genSchema(r, fmt.Sprintf("fp%d_", i))
		q := genQuery(r, m)
		an, err := o.Load(m.DDL(o.Dialect))
		if err != nil {
			logf("generator (schema)", "%v\n%s", err, m.DDL(o.Dialect))
			continue
		}
		if p := an.Problems(); len(p) > 0 {
			logf("generator (schema)", "%s\n%s", strings.Join(p, "\n"), m.DDL(o.Dialect))
			continue
		}
		if err := o.apply(ctx, m); err != nil {
			return nil, fmt.Errorf("pair %d: creating the tables: %w", i, err)
		}
		v, class := o.judgeOne(ctx, q, m, an)
		for _, h := range v.Hit {
			rep.Hit[h] = true
		}
		for _, u := range v.Unverified {
			if rep.Unverified[u] == 0 {
				o.Log("unverified: %s\n%s", u, v.SQL)
			}
			rep.Unverified[u]++
		}
		switch {
		case class != "":
			logf(class, "%s\n%s\n%s", v.Detail, v.SQL, m.DDL(o.Dialect))
		case v.Kind == "":
			rep.Counts["pass"]++
		default:
			rep.Counts["finding"]++
			// minimize: drop conjuncts and LIMIT while the same kind of failure stands
			v = o.shrink(ctx, q, m, an, v)
			v.Schema = m.DDL(o.Dialect) + strings.Join(m.Inserts(), "\n")
			key := v.Kind + "\n" + v.Detail
			if !seen[key] {
				seen[key] = true
				rep.Findings = append(rep.Findings, v)
			}
		}
		if err := o.drop(ctx, m); err != nil {
			return nil, fmt.Errorf("pair %d: dropping the tables: %w", i, err)
		}
	}
	for _, e := range Alphabet {
		switch {
		case rep.Hit[e]:
			if _, known := o.KnownUnreached[e]; known {
				rep.Stale = append(rep.Stale, e)
			}
		default:
			if _, known := o.KnownUnreached[e]; !known {
				rep.Missed = append(rep.Missed, e)
			}
		}
	}
	return rep, nil
}

// judgeOne analyzes and runs q. class names a generator-side failure (both sides reject,
// or the evaluator disagrees with the server), "" when the verdict stands. A write runs
// against freshly created tables: its witness (the rows it will touch) is read first, the
// write executed, and the rows read back for the values the facts say were stored.
func (o Options) judgeOne(ctx context.Context, q *query, m *Schema, an dialect.Analyzer) (Verdict, string) {
	sqlText := q.sql()
	v := Verdict{SQL: sqlText, Params: q.params}
	if q.kind != "select" {
		if err := o.apply(ctx, m); err != nil {
			v.Detail = err.Error()
			return v, "generator (schema)"
		}
	}
	res, aerr := an.Analyze(sqlText)
	var rows []row
	var serr error
	if q.kind == "select" {
		rows, serr = o.query(ctx, sqlText, q.params)
	} else {
		if q.kind != "insert" {
			rows, serr = o.query(ctx, q.witness(), q.params)
		}
		if serr == nil {
			serr = o.exec(ctx, sqlText, q.params)
		}
	}
	switch {
	case aerr != nil && serr != nil:
		v.Detail = fmt.Sprintf("analyzer: %v; server: %v", aerr, serr)
		return v, "generator (statement)"
	case aerr != nil:
		v.Kind, v.Detail = "analyzer rejects what the server accepts", aerr.Error()
		return v, ""
	case serr != nil && constraintError(serr):
		// a write the schema's own constraints refuse (a duplicate key, a NULL in a NOT
		// NULL column): the failure modes' concern, not the facts'
		v.Detail = serr.Error()
		return v, "generator (constraint)"
	case serr != nil:
		v.Kind, v.Detail = "server rejects what the analyzer accepts", serr.Error()
		return v, ""
	}
	if q.kind != "insert" {
		want := q.rows(m)
		if !sameRows(want, rows, q.limit1) {
			v.Detail = fmt.Sprintf("the generator's evaluation gives %d rows, the server %d:\n  model:  %v\n  server: %v", len(want), len(rows), want, rows)
			return v, "generator (evaluator)"
		}
	}
	v = judge(q, res.Facts, rows, m)
	if v.Kind != "" || q.kind == "select" || res.Facts == nil {
		return v, ""
	}
	// the write's own claims: the rows it touched now hold the values the facts record
	after, err := o.readBack(ctx, q, rows)
	if err != nil {
		v.Detail = err.Error()
		return v, "generator (read back)"
	}
	switch q.kind {
	case "delete":
		if len(after) > 0 {
			v.Kind, v.Detail = "write refuted", fmt.Sprintf("DELETE left %d of the %d rows the WHERE matched", len(after), len(rows))
		}
	case "insert":
		if len(after) != 1 {
			v.Kind, v.Detail = "write refuted", fmt.Sprintf("INSERT of one row left %d rows with its id", len(after))
			return v, ""
		}
		fallthrough
	case "update":
		for _, w := range res.Facts.Writes {
			if w.Kind != facts.Insert && w.Kind != facts.Update {
				continue
			}
			for i, name := range w.Assigned {
				if i >= len(w.Values) {
					break
				}
				var want Value
				switch w.Values[i].Kind {
				case facts.Const:
					want = constValue(w.Values[i].Const)
				case facts.Param:
					if int(w.Values[i].Param) < 1 || int(w.Values[i].Param) > len(q.params) {
						continue
					}
					want = q.params[w.Values[i].Param-1]
				default:
					continue
				}
				for _, r := range after {
					got := r.get("t0", name)
					if got.String() != want.String() {
						v.Kind, v.Detail = "write refuted", fmt.Sprintf("the facts say the write stores %s into %s; the row read back holds %s: %s", want, name, got, r)
						return v, ""
					}
				}
			}
		}
	}
	return v, ""
}

// constraintError: the server refused a write for a constraint (PostgreSQL's class 23,
// MySQL's 1062 / 1048 / 1452 / 3819 / 1263).
func constraintError(err error) bool {
	msg := err.Error()
	for _, mark := range []string{"SQLSTATE 23", "Error 1062", "Error 1048", "Error 1452", "Error 3819", "Error 1263"} {
		if strings.Contains(msg, mark) {
			return true
		}
	}
	return false
}

// readBack reads the target's rows by the ids of the rows the witness matched (a
// write), or by the inserted id.
func (o Options) readBack(ctx context.Context, q *query, before []row) ([]row, error) {
	var ids []string
	if q.kind == "insert" {
		for _, a := range q.set {
			if a.col.Name == "id" {
				ids = append(ids, a.value.S)
			}
		}
	} else {
		for _, r := range before {
			ids = append(ids, r.get("t0", "id").S)
		}
	}
	if len(ids) == 0 {
		return nil, nil
	}
	sel := &query{kind: "select", leaves: q.leaves[:1]}
	sel.where = []conj{{sql: "t0.id IN (" + strings.Join(ids, ", ") + ")"}}
	return o.query(ctx, sel.sql(), nil)
}

// exec runs a statement with params bound.
func (o Options) exec(ctx context.Context, sqlText string, params []Value) error {
	text, args := o.bind(sqlText, params)
	_, err := o.DB.ExecContext(ctx, text, args...)
	return err
}

// bind lists the arguments a statement text references, in order of first appearance,
// and rewrites its placeholders to match: `?` for a positional dialect, $1..$k renumbered
// otherwise (a witness SELECT carries only some of the statement's parameters, and
// PostgreSQL wants the ones it sees numbered without gaps).
func (o Options) bind(sqlText string, params []Value) (string, []any) {
	var args []any
	index := map[int]int{}
	text := paramRef.ReplaceAllStringFunc(sqlText, func(s string) string {
		n, _ := strconv.Atoi(s[1:])
		k, seen := index[n]
		if !seen {
			args = append(args, params[n-1].arg())
			k = len(args)
			index[n] = k
		}
		if o.Dialect.Positional {
			if seen {
				args = append(args, params[n-1].arg()) // `?` takes one argument per occurrence
			}
			return "?"
		}
		return fmt.Sprintf("$%d", k)
	})
	return text, args
}

// shrink drops WHERE conjuncts, ON conjuncts beyond the join equality, and LIMIT 1 one at
// a time while the failure keeps its kind.
func (o Options) shrink(ctx context.Context, q *query, m *Schema, an dialect.Analyzer, v Verdict) Verdict {
	try := func(cand *query) bool {
		w, class := o.judgeOne(ctx, cand, m, an)
		if class == "" && w.Kind == v.Kind {
			q, v = cand, w
			return true
		}
		return false
	}
	for i := 0; i < len(q.where); {
		cand := *q
		cand.where = append(append([]conj(nil), q.where[:i]...), q.where[i+1:]...)
		if !try(&cand) {
			i++
		}
	}
	for li := range q.leaves {
		for j := 1; j < len(q.leaves[li].on); {
			cand := *q
			cand.leaves = append([]leaf(nil), q.leaves...)
			cand.leaves[li].on = append(append([]conj(nil), q.leaves[li].on[:j]...), q.leaves[li].on[j+1:]...)
			if !try(&cand) {
				j++
			}
		}
	}
	if q.limit1 {
		cand := *q
		cand.limit1 = false
		try(&cand)
	}
	return v
}

func (o Options) apply(ctx context.Context, m *Schema) error {
	for _, s := range m.Drops() {
		if _, err := o.DB.ExecContext(ctx, strings.TrimSuffix(s, ";")); err != nil {
			return err
		}
	}
	for _, t := range m.Tables {
		if _, err := o.DB.ExecContext(ctx, strings.TrimSuffix(t.create(o.Dialect), ";")); err != nil {
			return fmt.Errorf("%s: %w", t.create(o.Dialect), err)
		}
	}
	for _, s := range m.Inserts() {
		if _, err := o.DB.ExecContext(ctx, strings.TrimSuffix(s, ";")); err != nil {
			return fmt.Errorf("%s: %w", s, err)
		}
	}
	return nil
}

func (o Options) drop(ctx context.Context, m *Schema) error {
	for _, s := range m.Drops() {
		if _, err := o.DB.ExecContext(ctx, strings.TrimSuffix(s, ";")); err != nil {
			return err
		}
	}
	return nil
}

var paramRef = regexp.MustCompile(`\$(\d+)`)

// query runs sqlText with params bound, and returns the rows keyed alias.column (the
// select list aliases every column alias__column).
func (o Options) query(ctx context.Context, sqlText string, params []Value) ([]row, error) {
	text, args := o.bind(sqlText, params)
	rs, err := o.DB.QueryContext(ctx, text, args...)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	cols, err := rs.Columns()
	if err != nil {
		return nil, err
	}
	var out []row
	for rs.Next() {
		vals := make([]sql.NullString, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rs.Scan(ptrs...); err != nil {
			return nil, err
		}
		r := row{}
		for i, c := range cols {
			key := strings.Replace(c, "__", ".", 1)
			if vals[i].Valid {
				r[key] = Value{S: vals[i].String}
			} else {
				r[key] = null
			}
		}
		out = append(out, r)
	}
	return out, rs.Err()
}

// arg is the value as database/sql binds it.
func (v Value) arg() any {
	if v.Null {
		return nil
	}
	if n, err := strconv.Atoi(v.S); err == nil {
		return n
	}
	return v.S
}

// sameRows compares two row multisets; under LIMIT 1 only the counts are compared (which
// row the server picks is not the model's to say).
func sameRows(want, got []row, limit1 bool) bool {
	if limit1 {
		return min(len(want), 1) == len(got)
	}
	if len(want) != len(got) {
		return false
	}
	key := func(rs []row) []string {
		out := make([]string, len(rs))
		for i, r := range rs {
			out[i] = r.String()
		}
		sort.Strings(out)
		return out
	}
	a, b := key(want), key(got)
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// String renders the report the way the migrate probe's is written.
func (rep *Report) String(title string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# facts probe (%s)\n\n", title)
	var keys []string
	for k := range rep.Counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&b, "%s: %d\n", k, rep.Counts[k])
	}
	for i, v := range rep.Findings {
		fmt.Fprintf(&b, "\n## %d. %s\n\n%s\n\nparams: %v\n\n```sql\n%s\n```\n\n### schema\n\n```sql\n%s\n```\n", i+1, v.Kind, v.Detail, v.Params, v.SQL, v.Schema)
	}
	if len(rep.Unverified) > 0 {
		fmt.Fprintf(&b, "\n## unverified claims\n\n")
		var us []string
		for u := range rep.Unverified {
			us = append(us, u)
		}
		sort.Strings(us)
		for _, u := range us {
			fmt.Fprintf(&b, "- %d x %s\n", rep.Unverified[u], u)
		}
	}
	fmt.Fprintf(&b, "\n## alphabet coverage\n\n%d entries, %d hit\n\n", len(Alphabet), len(rep.Hit))
	for _, e := range Alphabet {
		switch {
		case rep.Hit[e]:
			fmt.Fprintf(&b, "- [x] %s\n", e)
		default:
			fmt.Fprintf(&b, "- [ ] %s\n", e)
		}
	}
	return b.String()
}
