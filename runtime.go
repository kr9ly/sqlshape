package sqlshape

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"unicode"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// DB is what a Stmt runs against: *pgx.Conn, *pgxpool.Pool and pgx.Tx all satisfy it.
type DB interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// ErrNoRows is returned by First and Get when the query produced no row.
var ErrNoRows = pgx.ErrNoRows

// ConstraintError is a constraint violation (SQLSTATE class 23) mapped back to the
// schema: Constraint is the PG constraint name (the same one the checker lists and the
// template's `-- sqlshape: expect` line names), Column the NOT NULL column.
type ConstraintError struct {
	Code       string // SQLSTATE
	Constraint string // "" for NOT NULL
	Table      string
	Column     string
	Detail     string
	Err        *pgconn.PgError
}

func (e *ConstraintError) Error() string {
	switch {
	case e.Constraint != "":
		return "sqlshape: constraint " + e.Constraint + " violated: " + e.Err.Message
	case e.Column != "":
		return "sqlshape: " + e.Table + "." + e.Column + " NOT NULL violated: " + e.Err.Message
	}
	return "sqlshape: " + e.Err.Message
}

func (e *ConstraintError) Unwrap() error { return e.Err }

// Key is the violation as the expect line spells it: the constraint name, or table.column for NOT NULL.
func (e *ConstraintError) Key() string {
	if e.Constraint != "" {
		return e.Constraint
	}
	return e.Table + "." + e.Column
}

// Violates reports whether err is a violation of the named constraint (or table.column NOT NULL).
func Violates(err error, key string) bool {
	var ce *ConstraintError
	return errors.As(err, &ce) && ce.Key() == key
}

// wrapErr maps integrity-constraint errors (SQLSTATE class 23) and the custom SQLSTATEs
// the template's expect line names to ConstraintError; other errors pass through.
func (s Stmt[R, P]) wrapErr(err error) error { return wrapPgErr(err, s.expects) }

// wrapPgErr is wrapErr with the statement's expectations passed in (nil: class 23 only).
// An error already mapped is left alone.
func wrapPgErr(err error, expects func(code string) bool) error {
	var ce *ConstraintError
	if err == nil || errors.As(err, &ce) {
		return err
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return err
	}
	if strings.HasPrefix(pgErr.Code, "23") {
		return &ConstraintError{Code: pgErr.Code, Constraint: pgErr.ConstraintName, Table: pgErr.TableName, Column: pgErr.ColumnName, Detail: pgErr.Detail, Err: pgErr}
	}
	if expects != nil && expects(pgErr.Code) {
		return &ConstraintError{Code: pgErr.Code, Constraint: pgErr.Code, Detail: pgErr.Detail, Err: pgErr}
	}
	return err
}

var expectRe = regexp.MustCompile(`(?m)^[ \t]*--[ \t]*sqlshape:[ \t]*expect[ \t]+(.+?)[ \t]*$`)

// expects reports whether the template's `-- sqlshape: expect` line names the SQLSTATE.
func (s Stmt[R, P]) expects(code string) bool {
	for _, m := range expectRe.FindAllStringSubmatch(s.Template, -1) {
		for _, item := range strings.Split(m[1], ",") {
			if strings.TrimSpace(item) == code {
				return true
			}
		}
	}
	return false
}

// ErrManyRows is returned by Get / Find when a statement declared with One produced
// more than one row: the schema no longer backs the proof the checker made.
var ErrManyRows = errors.New("sqlshape: statement declared with One returned more than one row")

// Run executes the statement and yields each row mapped into R. Iteration stops at
// the first error, which is yielded with a zero R. Breaking out early closes the rows.
func (s Stmt[R, P]) Run(ctx context.Context, db DB, p P) iter.Seq2[R, error] {
	return func(yield func(R, error) bool) {
		var zero R
		r, err := s.Render(p)
		if err != nil {
			yield(zero, err)
			return
		}
		var rows pgx.Rows
		err = withParamTypes(ctx, db, len(r.Args), func() error {
			rows, err = db.Query(ctx, r.SQL, s.args(r.Args)...)
			return err
		})
		if err != nil {
			yield(zero, s.wrapErr(err))
			return
		}
		// result types the connection cannot decode yet (user enums, composites): load them and re-run
		if oids := unknownTypes(rows.Conn(), rows.FieldDescriptions()); len(oids) > 0 {
			conn := rows.Conn()
			rows.Close()
			if err := loadTypes(ctx, conn, oids); err != nil {
				yield(zero, err)
				return
			}
			if rows, err = db.Query(ctx, r.SQL, s.args(r.Args)...); err != nil {
				yield(zero, s.wrapErr(err))
				return
			}
		}
		defer rows.Close()
		var m *mapper[R]
		for rows.Next() {
			if m == nil {
				m, err = newMapper[R](rows.FieldDescriptions())
				if err != nil {
					yield(zero, err)
					return
				}
			}
			row, err := m.scan(rows)
			if err != nil {
				yield(zero, err)
				return
			}
			if !yield(row, nil) {
				return
			}
		}
		if err := rows.Err(); err != nil {
			yield(zero, s.wrapErr(err))
		}
	}
}

// Collect runs the statement and returns all rows.
func (s Stmt[R, P]) Collect(ctx context.Context, db DB, p P) ([]R, error) {
	var out []R
	for row, err := range s.Run(ctx, db, p) {
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, nil
}

// First runs the statement and returns the first row, or ErrNoRows.
func (s Stmt[R, P]) First(ctx context.Context, db DB, p P) (R, error) {
	for row, err := range s.Run(ctx, db, p) {
		return row, err
	}
	var zero R
	return zero, ErrNoRows
}

// Exec runs a statement that returns no rows (INSERT / UPDATE / DELETE without RETURNING).
func (s Stmt[R, P]) Exec(ctx context.Context, db DB, p P) (pgconn.CommandTag, error) {
	r, err := s.Render(p)
	if err != nil {
		return pgconn.CommandTag{}, err
	}
	var tag pgconn.CommandTag
	err = withParamTypes(ctx, db, len(r.Args), func() error {
		tag, err = db.Exec(ctx, r.SQL, s.args(r.Args)...)
		return err
	})
	return tag, s.wrapErr(err)
}

// unknownParamRe matches pgx's client-side encode failure for a parameter whose type the
// connection has not registered ("... for unknown type (OID 16847): ...").
var unknownParamRe = regexp.MustCompile(`unknown type \(OID (\d+)\)`)

// withParamTypes runs a statement, and when a parameter has a user type (composite,
// range, enum) the connection has not registered yet, loads that type on the connection
// and runs again. pgx reports one unknown parameter per attempt, so this repeats, at most
// once per argument. With a pool there is no single connection to register on, so the
// error is returned with the advice to LoadUserTypes in AfterConnect.
func withParamTypes(ctx context.Context, db DB, nargs int, run func() error) error {
	err := run()
	for i := 0; err != nil && i < nargs; i++ {
		m := unknownParamRe.FindStringSubmatch(err.Error())
		if m == nil {
			return err
		}
		oid, _ := strconv.ParseUint(m[1], 10, 32)
		conn := connOf(db)
		if conn == nil {
			return fmt.Errorf("%w (sqlshape: a parameter has a user-defined type this connection has not loaded; call sqlshape.LoadUserTypes from the pool's AfterConnect)", err)
		}
		if lerr := loadTypes(ctx, conn, []uint32{uint32(oid)}); lerr != nil {
			return lerr
		}
		err = run()
	}
	return err
}

// connOf is the single connection behind db, or nil (a pool).
func connOf(db DB) *pgx.Conn {
	switch d := db.(type) {
	case *pgx.Conn:
		return d
	case interface{ Conn() *pgx.Conn }: // pgx.Tx, pgxpool.Conn
		return d.Conn()
	}
	return nil
}

// args prefixes the exec mode for unprepared statements (pgx reads a QueryExecMode first argument).
func (s Stmt[R, P]) args(a []any) []any {
	if !s.unprepared {
		return a
	}
	return append([]any{pgx.QueryExecModeExec}, a...)
}

// Get runs the single-row statement and returns its row, or ErrNoRows.
func (s Single[R, P]) Get(ctx context.Context, db DB, p P) (R, error) {
	row, ok, err := s.Find(ctx, db, p)
	if err == nil && !ok {
		return row, ErrNoRows
	}
	return row, err
}

// Find runs the single-row statement and reports whether a row was found.
func (s Single[R, P]) Find(ctx context.Context, db DB, p P) (R, bool, error) {
	var found R
	var zero R
	n := 0
	for row, err := range s.stmt.Run(ctx, db, p) {
		if err != nil {
			return zero, false, err
		}
		n++
		if n > 1 {
			return zero, false, ErrManyRows
		}
		found = row
	}
	return found, n == 1, nil
}

// Exec runs a single-row statement that returns no rows (e.g. UPDATE ... WHERE id = $1).
// An INSERT / UPDATE / DELETE / MERGE that touched no row returns ErrNoRows (the key did
// not exist), one that touched more than one returns ErrManyRows (the checker's proof
// that the statement affects at most one row did not hold).
func (s Single[R, P]) Exec(ctx context.Context, db DB, p P) (pgconn.CommandTag, error) {
	tag, err := s.stmt.Exec(ctx, db, p)
	if err != nil {
		return tag, err
	}
	if tag.Insert() || tag.Update() || tag.Delete() || strings.HasPrefix(tag.String(), "MERGE") {
		switch n := tag.RowsAffected(); {
		case n == 0:
			return tag, ErrNoRows
		case n > 1:
			return tag, ErrManyRows
		}
	}
	return tag, nil
}

// Render renders the template for p.
func (s Single[R, P]) Render(p P) (Rendered, error) { return s.stmt.Render(p) }

// --- row mapping -----------------------------------------------------------

// mapper binds result columns to the fields of R (or to R itself when it is a scalar).
type mapper[R any] struct {
	scalar bool
	fields [][]int // field index path per column (embedded structs flattened)
	// labelled is whether R contains a type implementing Known (enum labels the
	// application knows): rows are validated after scanning
	labelled bool
}

// Labelled is implemented by a Go enum type (a named string type bound to a PG enum) to
// say which labels the application knows. When a scanned value answers false the row is
// rejected with an *UnknownLabelError: the database has a label this build predates.
//
//	func (s OrderStatus) Known() bool { switch s { case Pending, Paid: return true }; return false }
type Labelled interface{ Known() bool }

// UnknownLabelError reports a scanned enum value the application does not know.
type UnknownLabelError struct {
	Type  reflect.Type
	Value string
}

func (e *UnknownLabelError) Error() string {
	return "sqlshape: unknown " + e.Type.String() + " label " + strconv.Quote(e.Value) + " received (the database has a value this build does not know)"
}

var labelledType = reflect.TypeOf((*Labelled)(nil)).Elem()

// hasLabelled reports whether t (or anything it contains) implements Labelled.
func hasLabelled(t reflect.Type, seen map[reflect.Type]bool) bool {
	if seen[t] {
		return false
	}
	seen[t] = true
	if t.Implements(labelledType) {
		return true
	}
	switch t.Kind() {
	case reflect.Pointer, reflect.Slice, reflect.Array:
		return hasLabelled(t.Elem(), seen)
	case reflect.Struct:
		for i := 0; i < t.NumField(); i++ {
			if t.Field(i).IsExported() && hasLabelled(t.Field(i).Type, seen) {
				return true
			}
		}
	}
	return false
}

// checkLabels validates every Labelled value inside v.
func checkLabels(v reflect.Value) error {
	if !v.IsValid() {
		return nil
	}
	if v.Type().Implements(labelledType) && v.Kind() == reflect.String {
		if s := v.String(); s != "" && !v.Interface().(Labelled).Known() {
			return &UnknownLabelError{Type: v.Type(), Value: s}
		}
		return nil
	}
	switch v.Kind() {
	case reflect.Pointer:
		if v.IsNil() {
			return nil
		}
		return checkLabels(v.Elem())
	case reflect.Slice, reflect.Array:
		for i := 0; i < v.Len(); i++ {
			if err := checkLabels(v.Index(i)); err != nil {
				return err
			}
		}
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			if v.Type().Field(i).IsExported() {
				if err := checkLabels(v.Field(i)); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

var mapperCache sync.Map // mapperKey → *mapper

type mapperKey struct {
	typ  reflect.Type
	cols string
}

func newMapper[R any](fds []pgconn.FieldDescription) (*mapper[R], error) {
	var zero R
	rt := reflect.TypeOf(zero)
	if rt == nil {
		return nil, fmt.Errorf("sqlshape: R must not be an interface type")
	}
	names := make([]string, len(fds))
	for i, fd := range fds {
		names[i] = fd.Name
	}
	key := mapperKey{typ: rt, cols: strings.Join(names, "\x00")}
	if m, ok := mapperCache.Load(key); ok {
		return m.(*mapper[R]), nil
	}
	m := &mapper[R]{labelled: hasLabelled(rt, map[reflect.Type]bool{})}
	if rt.Kind() != reflect.Struct || rt.PkgPath() == "time" {
		if len(fds) != 1 {
			return nil, fmt.Errorf("sqlshape: R is %s but the query returns %d columns", rt, len(fds))
		}
		m.scalar = true
	} else {
		flat, err := flatFields(rt)
		if err != nil {
			return nil, err
		}
		byName := map[string]int{}
		lower := map[string]int{}
		for i, f := range flat {
			byName[f.column] = i
			lower[strings.ToLower(f.column)] = i
		}
		m.fields = make([][]int, len(fds))
		used := map[int]bool{}
		for i, fd := range fds {
			idx, ok := byName[fd.Name]
			if !ok {
				idx, ok = lower[strings.ToLower(fd.Name)]
			}
			if !ok {
				return nil, fmt.Errorf("sqlshape: result column %q has no field in %s", fd.Name, rt)
			}
			if used[idx] {
				return nil, fmt.Errorf("sqlshape: result column %q matches %s.%s twice", fd.Name, rt, flat[idx].name)
			}
			used[idx] = true
			m.fields[i] = flat[idx].index
		}
		for i, f := range flat {
			if !used[i] && !optionalKind(f.typ) {
				// a nullable field may be left unset by a branch that does not select it
				return nil, fmt.Errorf("sqlshape: field %s.%s has no result column (%s)", rt.Name(), f.name, f.column)
			}
		}
	}
	mapperCache.Store(key, m)
	return m, nil
}

// flatField is one scan target of a struct: an exported, non-skipped field, with the
// fields of embedded structs promoted (flattened) into their parent.
type flatField struct {
	index  []int  // for reflect's FieldByIndex
	name   string // Go name, dotted through embedded structs (Base.ID)
	column string // result column it binds to
	typ    reflect.Type
}

var flatFieldsCache sync.Map // reflect.Type → []flatField or error

// flatFields lists the scan targets of a struct type in declaration order. An embedded
// struct (anonymous field, no col / db tag) is flattened: its fields are the parent's,
// so `type Row struct { Base; Extra string }` receives base columns and extra. A named
// struct field is a nested row instead. Two fields binding the same column is an error.
func flatFields(t reflect.Type) ([]flatField, error) {
	if v, ok := flatFieldsCache.Load(t); ok {
		if err, isErr := v.(error); isErr {
			return nil, err
		}
		return v.([]flatField), nil
	}
	var out []flatField
	seen := map[string]string{}
	var walk func(st reflect.Type, prefix string, index []int) error
	walk = func(st reflect.Type, prefix string, index []int) error {
		for i := 0; i < st.NumField(); i++ {
			f := st.Field(i)
			if !f.IsExported() && !f.Anonymous {
				continue
			}
			idx := append(append([]int{}, index...), i)
			if f.Anonymous && embeddedStruct(f) != nil {
				if err := walk(embeddedStruct(f), prefix+f.Name+".", idx); err != nil {
					return err
				}
				continue
			}
			if !f.IsExported() {
				continue
			}
			col := fieldColumn(f)
			if col == "-" {
				continue
			}
			if prev, dup := seen[col]; dup {
				return fmt.Errorf("sqlshape: fields %s.%s and %s.%s both bind to column %q", t, prev, t, prefix+f.Name, col)
			}
			seen[col] = prefix + f.Name
			out = append(out, flatField{index: idx, name: prefix + f.Name, column: col, typ: f.Type})
		}
		return nil
	}
	if err := walk(t, "", nil); err != nil {
		flatFieldsCache.Store(t, err)
		return nil, err
	}
	flatFieldsCache.Store(t, out)
	return out, nil
}

// embeddedStruct returns the struct type an anonymous field flattens into, or nil when
// the field is a leaf: not a struct, time.Time, a Scanner, or tagged with a column name.
func embeddedStruct(f reflect.StructField) reflect.Type {
	if !f.Anonymous {
		return nil
	}
	if _, ok := f.Tag.Lookup("col"); ok {
		return nil
	}
	if _, ok := f.Tag.Lookup("db"); ok {
		return nil
	}
	t := f.Type
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct || !isNested(t) {
		return nil
	}
	return t
}

// fieldByIndex is reflect.Value.FieldByIndex that allocates nil embedded pointers on the way.
func fieldByIndex(v reflect.Value, index []int) reflect.Value {
	for i, x := range index {
		if i > 0 && v.Kind() == reflect.Pointer {
			if v.IsNil() {
				v.Set(reflect.New(v.Type().Elem()))
			}
			v = v.Elem()
		}
		v = v.Field(x)
	}
	return v
}

func (m *mapper[R]) scan(rows pgx.Rows) (R, error) {
	var row R
	var err error
	if m.scalar {
		err = rows.Scan(&row)
	} else {
		v := reflect.ValueOf(&row).Elem()
		dests := make([]any, len(m.fields))
		for i, idx := range m.fields {
			fv := fieldByIndex(v, idx)
			if isNested(fv.Type()) {
				dests[i] = nestedDest(fv)
			} else {
				dests[i] = fv.Addr().Interface()
			}
		}
		err = rows.Scan(dests...)
	}
	if err == nil && m.labelled {
		err = checkLabels(reflect.ValueOf(row))
	}
	return row, err
}

// fieldColumn is the column a struct field binds to: `col:"name"` / `db:"name"`, else snake_case.
// (Kept in sync with the analyzer's rule.)
func fieldColumn(f reflect.StructField) string {
	for _, key := range []string{"col", "db"} {
		if v, ok := f.Tag.Lookup(key); ok {
			name := strings.Split(v, ",")[0]
			if name == "" {
				return snake(f.Name)
			}
			return name
		}
	}
	return snake(f.Name)
}

func snake(s string) string {
	var b strings.Builder
	rs := []rune(s)
	for i, r := range rs {
		if unicode.IsUpper(r) {
			if i > 0 && (unicode.IsLower(rs[i-1]) || (i+1 < len(rs) && unicode.IsLower(rs[i+1]) && unicode.IsUpper(rs[i-1]))) {
				b.WriteByte('_')
			}
			b.WriteRune(unicode.ToLower(r))
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// IsNoRows reports whether err is ErrNoRows.
func IsNoRows(err error) bool { return errors.Is(err, ErrNoRows) }

// optionalKind reports whether a field type can stay unset when a branch does not select it.
func optionalKind(t reflect.Type) bool {
	switch t.Kind() {
	case reflect.Pointer, reflect.Slice, reflect.Map, reflect.Interface:
		return true
	}
	return false
}
