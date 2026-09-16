package stmtprobe

import (
	"context"
	"fmt"
	"math/rand"
	"sort"
	"strings"

	"github.com/kr9ly/sqlshape/v2/x/dialect"
)

// ---- the failure probe: every constraint error the server raises was predicted -------
//
// The analyzer lists the constraints a write may violate (dialect.Result.Violations, the
// expect line's keys); the runtime's Violates matches a raised error to one of those keys.
// The probe generates writes against a schema carrying every constraint kind, runs them,
// and requires that a constraint error the server raised is matched by Violates under a
// predicted key. A predicted violation the server did not raise is a "may" and fine.

// FailureOptions extends Options with the runtime's error handling.
type FailureOptions struct {
	Options
	// Wrap is the runtime's WrapError: the error Run / Exec would have returned.
	Wrap func(err error) error
	// Violates is the runtime's Violates over a wrapped error and an expect-line key.
	Violates func(err error, key string) bool
}

// FailureAlphabet lists the write shapes the generator draws and the constraint kinds a
// server error can be, as the probe names them.
var FailureAlphabet = []string{
	"insert", "insert omitted column", "insert ignore", "replace", "upsert nothing", "upsert update",
	"update", "update referenced key", "delete parent restrict", "delete parent cascade", "delete parent set null", "delete child",
	"raised unique", "raised primary key", "raised foreign key", "raised not null", "raised check",
	"predicted unique", "predicted primary key", "predicted foreign key", "predicted not null", "predicted check",
	"predicted always fails", "constraint named", "constraint unnamed",
}

// RunFailures generates, judges and reports the failure probe.
func RunFailures(ctx context.Context, o FailureOptions) (*Report, error) {
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
		m := genFailSchema(r, fmt.Sprintf("fq%d_", i))
		w := genWrite(r, m, o.Dialect)
		an, err := o.Load(m.DDL(o.Dialect))
		if err != nil {
			logf("generator (schema)", "%v\n%s", err, m.DDL(o.Dialect))
			continue
		}
		if p := an.Problems(); len(p) > 0 {
			logf("generator (schema)", "%s\n%s", strings.Join(p, "\n"), m.DDL(o.Dialect))
			continue
		}
		v, class := o.judgeFailure(ctx, w, m, an)
		for _, h := range v.Hit {
			rep.Hit[h] = true
		}
		switch {
		case class != "":
			logf(class, "%s\n%s\n%s", v.Detail, v.SQL, m.DDL(o.Dialect))
		case v.Kind == "":
			rep.Counts["pass"]++
		default:
			rep.Counts["finding"]++
			v.Schema = m.DDL(o.Dialect) + strings.Join(m.Inserts(), "\n")
			key := v.Kind + "\n" + v.Detail
			if !seen[key] {
				seen[key] = true
				rep.Findings = append(rep.Findings, v)
			}
		}
		if err := o.drop(ctx, m); err != nil {
			return nil, fmt.Errorf("statement %d: dropping the tables: %w", i, err)
		}
	}
	for _, e := range FailureAlphabet {
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
	rep.alphabet = FailureAlphabet
	return rep, nil
}

// writeStmt is one generated write and the shapes it exercises.
type writeStmt struct {
	sql    string
	params []Value
	shapes []string
}

// judgeFailure runs w against fresh tables and compares the server's error with the
// predicted violations.
func (o FailureOptions) judgeFailure(ctx context.Context, w *writeStmt, m *Schema, an dialect.Analyzer) (Verdict, string) {
	v := Verdict{SQL: w.sql, Params: w.params, Hit: append([]string(nil), w.shapes...)}
	for _, t := range m.Tables {
		if t.uniqueName != "" || (t.check != nil && t.check.name != "") || (t.fk != nil && t.fk.name != "") {
			v.Hit = append(v.Hit, "constraint named")
		}
		if (len(t.Uniques) > 0 && t.uniqueName == "") || (t.check != nil && t.check.name == "") || (t.fk != nil && t.fk.name == "") {
			v.Hit = append(v.Hit, "constraint unnamed")
		}
	}
	if err := o.apply(ctx, m); err != nil {
		v.Detail = err.Error()
		return v, "generator (schema)"
	}
	res, aerr := an.Analyze(w.sql)
	serr := o.exec(ctx, w.sql, w.params)
	switch {
	case aerr != nil && serr != nil && !constraintError(serr):
		v.Detail = fmt.Sprintf("analyzer: %v; server: %v", aerr, serr)
		return v, "generator (statement)"
	case aerr != nil:
		v.Kind, v.Detail = "analyzer rejects what the server accepts", fmt.Sprintf("%v (the server: %v)", aerr, serr)
		return v, ""
	}
	var predicted []string
	for _, p := range res.Violations {
		predicted = append(predicted, p.Key)
		v.Hit = append(v.Hit, "predicted "+violationKind(p.Code, p.Key))
	}
	sort.Strings(predicted)
	if serr == nil {
		return v, ""
	}
	if !constraintError(serr) {
		v.Kind, v.Detail = "server rejects what the analyzer accepts", serr.Error()
		return v, ""
	}
	wrapped := o.Wrap(serr)
	for _, p := range res.Violations {
		if o.Violates(wrapped, p.Key) {
			v.Hit = append(v.Hit, "raised "+violationKind(p.Code, p.Key))
			return v, ""
		}
	}
	// a failure the analyzer reports as certain rather than possible (PostgreSQL's "INSERT
	// omits t.s, which is NOT NULL without a default: every execution fails" note) is
	// predicted too, and more strongly than an expect line would
	for _, n := range res.Notes {
		if !n.Advisory && strings.Contains(n.Message, "every execution fails") {
			v.Hit = append(v.Hit, "predicted always fails")
			return v, ""
		}
	}
	v.Kind = "unpredicted failure"
	v.Detail = fmt.Sprintf("the server raised %v (wrapped: %v); the analyzer predicted %v", serr, wrapped, predicted)
	return v, ""
}

// violationKind names a predicted violation's kind from its dialect code (and its key,
// which tells the primary key from another unique key).
func violationKind(code, key string) string {
	switch {
	case strings.HasSuffix(code, "23505"), strings.HasSuffix(code, "1062"):
		if key == "PRIMARY" || strings.HasSuffix(key, "_pkey") {
			return "primary key"
		}
		return "unique"
	case strings.HasSuffix(code, "23503"), strings.HasSuffix(code, "1452"), strings.HasSuffix(code, "1451"):
		return "foreign key"
	case strings.HasSuffix(code, "23502"), strings.HasSuffix(code, "1048"), strings.HasSuffix(code, "1364"), strings.HasSuffix(code, "1263"):
		return "not null"
	case strings.HasSuffix(code, "23514"), strings.HasSuffix(code, "3819"):
		return "check"
	}
	return code
}

// ---- generation -----------------------------------------------------------------------

// genFailSchema draws a parent p (id, b NOT NULL UNIQUE) and a child t (id, a, b NOT NULL
// UNIQUE, s NOT NULL, c CHECK (c > 0), pid REFERENCES p), each constraint named or left
// to the server, the foreign key's ON DELETE drawn, with rows.
func genFailSchema(r *rand.Rand, prefix string) *Schema {
	p := &Table{Name: prefix + "p", Cols: []Col{{Name: "id", Kind: Int, NotNull: true}, {Name: "b", Kind: Int, NotNull: true}}, PK: []string{"id"}, Uniques: [][]string{{"b"}}}
	if r.Intn(2) == 0 {
		p.uniqueName = "uq_" + prefix + "p_b"
	}
	n := 2 + r.Intn(2)
	for i := 0; i < n; i++ {
		p.Rows = append(p.Rows, []Value{intValue(i + 1), intValue(10 + i)})
	}
	t := &Table{Name: prefix + "t", Cols: []Col{{Name: "id", Kind: Int, NotNull: true}, {Name: "a", Kind: Int}, {Name: "b", Kind: Int, NotNull: true}, {Name: "s", Kind: Str, NotNull: true}, {Name: "c", Kind: Int}, {Name: "pid", Kind: Int}}, PK: []string{"id"}}
	if r.Intn(3) > 0 {
		t.Uniques = [][]string{{"b"}}
		if r.Intn(2) == 0 {
			t.uniqueName = "uq_" + prefix + "t_b"
		}
	}
	if r.Intn(3) > 0 {
		t.check = &checkDef{col: "c"}
		if r.Intn(2) == 0 {
			t.check.name = "chk_" + prefix + "c"
		}
	}
	if r.Intn(4) > 0 {
		t.fk = &fkDef{col: "pid", ref: p}
		if r.Intn(2) == 0 {
			t.fk.name = "fk_" + prefix + "t_pid"
		}
		switch r.Intn(3) {
		case 1:
			t.fk.onDelete = "CASCADE"
		case 2:
			t.fk.onDelete = "SET NULL"
		}
	}
	rows := 2 + r.Intn(3)
	for i := 0; i < rows; i++ {
		pid := null
		if r.Intn(3) > 0 {
			pid = p.Rows[r.Intn(len(p.Rows))][0]
		}
		t.Rows = append(t.Rows, []Value{intValue(i + 1), genInt(r, true), intValue(20 + i), strValue("x"), intValue(1 + r.Intn(3)), pid})
	}
	return &Schema{Tables: []*Table{p, t}}
}

// genWrite draws a write over the failure schema, biased to collide with its constraints:
// an INSERT into t (a duplicate id or b, a NULL s, a non-positive c, an unknown pid, an
// omitted NOT NULL column; IGNORE / REPLACE / ON DUPLICATE KEY UPDATE on MySQL, ON
// CONFLICT on PostgreSQL), an UPDATE of t's columns or of p's referenced id, a DELETE of a
// parent row or a child row.
func genWrite(r *rand.Rand, m *Schema, d Dialect) *writeStmt {
	p, t := m.Tables[0], m.Tables[1]
	w := &writeStmt{}
	value := func(c Col, v Value) string {
		if r.Intn(4) == 0 {
			w.params = append(w.params, v)
			return fmt.Sprintf("$%d", len(w.params))
		}
		return literal(c.Kind, v)
	}
	pick := func(rows [][]Value, i int) Value { return rows[r.Intn(len(rows))][i] }
	switch r.Intn(10) {
	case 0, 1, 2, 3: // INSERT
		vals := map[string]Value{}
		vals["id"] = intValue(len(t.Rows) + 1 + r.Intn(2))
		if r.Intn(3) == 0 {
			vals["id"] = pick(t.Rows, 0)
		}
		vals["a"] = genInt(r, true)
		vals["b"] = intValue(30 + r.Intn(3))
		if r.Intn(3) == 0 {
			vals["b"] = pick(t.Rows, 2)
		}
		vals["s"] = strValue("y")
		if r.Intn(4) == 0 {
			vals["s"] = null
		}
		vals["c"] = intValue(1 + r.Intn(2))
		if r.Intn(3) == 0 {
			vals["c"] = intValue(-r.Intn(2))
		}
		vals["pid"] = null
		switch r.Intn(3) {
		case 0:
			vals["pid"] = pick(p.Rows, 0)
		case 1:
			vals["pid"] = intValue(90 + r.Intn(3))
		}
		var cols, rendered []string
		omitted := r.Intn(4) == 0
		for _, c := range t.Cols {
			if omitted && c.Name == "s" {
				continue
			}
			cols = append(cols, c.Name)
			rendered = append(rendered, value(c, vals[c.Name]))
		}
		w.shapes = append(w.shapes, "insert")
		if omitted {
			w.shapes = append(w.shapes, "insert omitted column")
		}
		head := "INSERT INTO "
		tail := ""
		switch r.Intn(5) {
		case 0:
			if d.Positional {
				head, tail = "INSERT IGNORE INTO ", ""
			} else {
				tail = " ON CONFLICT DO NOTHING"
			}
			w.shapes = append(w.shapes, ifElse(d.Positional, "insert ignore", "upsert nothing"))
		case 1:
			if d.Positional {
				head = "REPLACE INTO "
				w.shapes = append(w.shapes, "replace")
			} else {
				tail = " ON CONFLICT (id) DO UPDATE SET a = " + value(t.Cols[1], genInt(r, true))
				w.shapes = append(w.shapes, "upsert update")
			}
		case 2:
			if d.Positional {
				tail = " ON DUPLICATE KEY UPDATE a = " + value(t.Cols[1], genInt(r, true))
				w.shapes = append(w.shapes, "upsert update")
			}
		}
		w.sql = head + t.Name + " (" + strings.Join(cols, ", ") + ") VALUES (" + strings.Join(rendered, ", ") + ")" + tail
	case 4, 5, 6: // UPDATE t
		var sets []string
		n := 1 + r.Intn(2)
		used := map[string]bool{}
		for i := 0; i < n; i++ {
			c := t.Cols[1+r.Intn(len(t.Cols)-1)]
			if used[c.Name] {
				continue
			}
			used[c.Name] = true
			var v Value
			switch c.Name {
			case "a":
				v = genInt(r, true)
			case "b":
				v = intValue(30 + r.Intn(2))
				if r.Intn(2) == 0 {
					v = pick(t.Rows, 2)
				}
			case "s":
				v = strValue("z")
				if r.Intn(3) == 0 {
					v = null
				}
			case "c":
				v = intValue(2 - r.Intn(4))
			case "pid":
				v = pick(p.Rows, 0)
				if r.Intn(2) == 0 {
					v = intValue(90)
				}
			}
			sets = append(sets, c.Name+" = "+value(c, v))
		}
		w.shapes = append(w.shapes, "update")
		w.sql = "UPDATE " + t.Name + " SET " + strings.Join(sets, ", ") + " WHERE id = " + value(t.Cols[0], pick(t.Rows, 0))
	case 7: // UPDATE p SET id: a referenced key
		w.shapes = append(w.shapes, "update referenced key")
		w.sql = "UPDATE " + p.Name + " SET id = " + value(p.Cols[0], intValue(50+r.Intn(2))) + " WHERE id = " + value(p.Cols[0], pick(p.Rows, 0))
	case 8: // DELETE a parent row
		shape := "delete parent restrict"
		if t.fk != nil {
			switch t.fk.onDelete {
			case "CASCADE":
				shape = "delete parent cascade"
			case "SET NULL":
				shape = "delete parent set null"
			}
		}
		w.shapes = append(w.shapes, shape)
		w.sql = "DELETE FROM " + p.Name + " WHERE id = " + value(p.Cols[0], pick(p.Rows, 0))
	default: // DELETE a child row
		w.shapes = append(w.shapes, "delete child")
		w.sql = "DELETE FROM " + t.Name + " WHERE b = " + value(t.Cols[2], pick(t.Rows, 2))
	}
	return w
}

func ifElse(c bool, a, b string) string {
	if c {
		return a
	}
	return b
}
