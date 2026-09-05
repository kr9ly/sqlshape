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

// ErrNoRows is returned by First when the query produced no row.
var ErrNoRows = pgx.ErrNoRows

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
			yield(zero, err)
			return
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
			yield(zero, err)
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
	return db.Exec(ctx, r.SQL, r.Args...)
}

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
		dests[i] = v.Field(idx).Addr().Interface()
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
