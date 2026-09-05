package sqlshape

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"reflect"
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

// wrapErr maps integrity-constraint errors to ConstraintError; other errors pass through.
func wrapErr(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && strings.HasPrefix(pgErr.Code, "23") {
		return &ConstraintError{Code: pgErr.Code, Constraint: pgErr.ConstraintName, Table: pgErr.TableName, Column: pgErr.ColumnName, Detail: pgErr.Detail, Err: pgErr}
	}
	return err
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
		rows, err := db.Query(ctx, r.SQL, r.Args...)
		if err != nil {
			yield(zero, wrapErr(err))
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
			if rows, err = db.Query(ctx, r.SQL, r.Args...); err != nil {
				yield(zero, wrapErr(err))
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
			yield(zero, wrapErr(err))
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
	tag, err := db.Exec(ctx, r.SQL, r.Args...)
	return tag, wrapErr(err)
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
func (s Single[R, P]) Exec(ctx context.Context, db DB, p P) (pgconn.CommandTag, error) {
	return s.stmt.Exec(ctx, db, p)
}

// Render renders the template for p.
func (s Single[R, P]) Render(p P) (Rendered, error) { return s.stmt.Render(p) }

// --- row mapping -----------------------------------------------------------

// mapper binds result columns to the fields of R (or to R itself when it is a scalar).
type mapper[R any] struct {
	scalar bool
	fields []int // field index per column
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
	m := &mapper[R]{}
	if rt.Kind() != reflect.Struct || rt.PkgPath() == "time" {
		if len(fds) != 1 {
			return nil, fmt.Errorf("sqlshape: R is %s but the query returns %d columns", rt, len(fds))
		}
		m.scalar = true
	} else {
		byName := map[string]int{}
		lower := map[string]int{}
		for i := 0; i < rt.NumField(); i++ {
			f := rt.Field(i)
			if !f.IsExported() {
				continue
			}
			name := fieldColumn(f)
			if name == "-" {
				continue
			}
			byName[name] = i
			lower[strings.ToLower(name)] = i
		}
		m.fields = make([]int, len(fds))
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
				return nil, fmt.Errorf("sqlshape: result column %q matches %s.%s twice", fd.Name, rt, rt.Field(idx).Name)
			}
			used[idx] = true
			m.fields[i] = idx
		}
		for name, idx := range byName {
			if !used[idx] {
				return nil, fmt.Errorf("sqlshape: field %s.%s has no result column (%s)", rt.Name(), rt.Field(idx).Name, name)
			}
		}
	}
	mapperCache.Store(key, m)
	return m, nil
}

func (m *mapper[R]) scan(rows pgx.Rows) (R, error) {
	var row R
	if m.scalar {
		return row, rows.Scan(&row)
	}
	v := reflect.ValueOf(&row).Elem()
	dests := make([]any, len(m.fields))
	for i, idx := range m.fields {
		fv := v.Field(idx)
		if isNested(fv.Type()) {
			dests[i] = nestedDest(fv)
		} else {
			dests[i] = fv.Addr().Interface()
		}
	}
	return row, rows.Scan(dests...)
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
