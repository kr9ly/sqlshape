// Package mysql runs checked statements on MySQL through database/sql, with
// go-sql-driver/mysql as the driver (the receive types the checker assumes are that
// driver's: int64 / uint64 for integers, string for DECIMAL, time.Time for temporal columns
// with parseTime=true, []byte for binary strings and JSON).
//
//	var listOrders = sqlshape.Query[Order, ListParams](`SELECT ... WHERE status = {{.Status}}`)
//
//	for o, err := range mysql.Run(ctx, db, listOrders, p) { ... }
//	orders, err := mysql.Collect(ctx, db, listOrders, p)
//
// DB is what a statement runs against: *sql.DB, *sql.Tx and *sql.Conn all satisfy it. The
// template's `$n` placeholders become MySQL's positional `?`, the arguments reordered to
// match (a `$n` written twice is sent twice).
package mysql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"iter"
	"reflect"
	"strings"
	"sync"

	"github.com/kr9ly/sqlshape/v2"
)

// DB is what a Stmt runs against: *sql.DB, *sql.Tx and *sql.Conn all satisfy it.
type DB interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// ErrNoRows is returned by First and Get when the query produced no row (it is sql.ErrNoRows).
var ErrNoRows = sql.ErrNoRows

// IsNoRows reports whether err is ErrNoRows.
func IsNoRows(err error) bool { return errors.Is(err, ErrNoRows) }

// ErrManyRows is returned by Get / Find / ExecOne when a statement declared with One
// touched more than one row: the schema no longer backs the proof the checker made.
var ErrManyRows = errors.New("sqlshape: statement declared with One returned more than one row")

// Run executes the statement s and yields each row mapped into R. Iteration stops at the
// first error, which is yielded with a zero R. Breaking out early closes the rows.
func Run[R, P any](ctx context.Context, db DB, s sqlshape.Stmt[R, P], p P) iter.Seq2[R, error] {
	return func(yield func(R, error) bool) {
		var zero R
		r, err := s.Render(p)
		if err != nil {
			yield(zero, err)
			return
		}
		text, args := positional(r)
		rows, err := db.QueryContext(ctx, text, args...)
		if err != nil {
			yield(zero, wrapErr(err))
			return
		}
		defer rows.Close()
		cols, err := rows.Columns()
		if err != nil {
			yield(zero, err)
			return
		}
		m, err := newMapper[R](cols)
		if err != nil {
			yield(zero, err)
			return
		}
		for rows.Next() {
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
func Collect[R, P any](ctx context.Context, db DB, s sqlshape.Stmt[R, P], p P) ([]R, error) {
	var out []R
	for row, err := range Run(ctx, db, s, p) {
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, nil
}

// First runs the statement and returns the first row, or ErrNoRows.
func First[R, P any](ctx context.Context, db DB, s sqlshape.Stmt[R, P], p P) (R, error) {
	for row, err := range Run(ctx, db, s, p) {
		return row, err
	}
	var zero R
	return zero, ErrNoRows
}

// Exec runs a statement that returns no rows (INSERT / UPDATE / DELETE).
func Exec[R, P any](ctx context.Context, db DB, s sqlshape.Stmt[R, P], p P) (sql.Result, error) {
	r, err := s.Render(p)
	if err != nil {
		return nil, err
	}
	text, args := positional(r)
	res, err := db.ExecContext(ctx, text, args...)
	return res, wrapErr(err)
}

// Get runs the single-row statement s and returns its row, or ErrNoRows.
func Get[R, P any](ctx context.Context, db DB, s sqlshape.Single[R, P], p P) (R, error) {
	row, ok, err := Find(ctx, db, s, p)
	if err == nil && !ok {
		return row, ErrNoRows
	}
	return row, err
}

// Find runs the single-row statement and reports whether a row was found.
func Find[R, P any](ctx context.Context, db DB, s sqlshape.Single[R, P], p P) (R, bool, error) {
	var found, zero R
	n := 0
	for row, err := range Run(ctx, db, s.Stmt(), p) {
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

// ExecOne runs a single-row statement that returns no rows (e.g. UPDATE ... WHERE id = ?).
// A statement that touched no row returns ErrNoRows (the key did not exist), one that
// touched more than one returns ErrManyRows (the checker's proof did not hold). MySQL
// counts an UPDATE that changed nothing as 0 rows unless the connection asks for
// CLIENT_FOUND_ROWS (the DSN's clientFoundRows=true): with the default, an UPDATE to the
// values a row already has returns ErrNoRows.
func ExecOne[R, P any](ctx context.Context, db DB, s sqlshape.Single[R, P], p P) (sql.Result, error) {
	res, err := Exec(ctx, db, s.Stmt(), p)
	if err != nil {
		return res, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return res, nil // a driver that does not count: nothing to judge
	}
	switch {
	case n == 0:
		return res, ErrNoRows
	case n > 1:
		return res, ErrManyRows
	}
	return res, nil
}

// --- row mapping -----------------------------------------------------------

// mapper binds result columns to the fields of R (or to R itself when it is a scalar).
type mapper[R any] struct {
	scalar   bool
	fields   [][]int // field index path per column
	labelled bool
}

var mapperCache sync.Map // mapperKey → *mapper

type mapperKey struct {
	typ  reflect.Type
	cols string
}

func newMapper[R any](cols []string) (*mapper[R], error) {
	var zero R
	rt := reflect.TypeOf(zero)
	if rt == nil {
		return nil, fmt.Errorf("sqlshape: R must not be an interface type")
	}
	key := mapperKey{typ: rt, cols: strings.Join(cols, "\x00")}
	if m, ok := mapperCache.Load(key); ok {
		return m.(*mapper[R]), nil
	}
	m := &mapper[R]{labelled: sqlshape.HasLabelled(rt, map[reflect.Type]bool{})}
	if rt.Kind() != reflect.Struct || sqlshape.ScalarStruct(rt) || reflect.PointerTo(rt).Implements(scannerType) {
		if len(cols) != 1 {
			return nil, fmt.Errorf("sqlshape: R is %s but the query returns %d columns", rt, len(cols))
		}
		m.scalar = true
	} else {
		flat, err := sqlshape.Fields(rt)
		if err != nil {
			return nil, err
		}
		byName := map[string]int{}
		lower := map[string]int{}
		for i, f := range flat {
			byName[f.Column] = i
			lower[strings.ToLower(f.Column)] = i
		}
		m.fields = make([][]int, len(cols))
		used := map[int]bool{}
		for i, name := range cols {
			idx, ok := byName[name]
			if !ok {
				idx, ok = lower[strings.ToLower(name)]
			}
			if !ok {
				return nil, fmt.Errorf("sqlshape: result column %q has no field in %s", name, rt)
			}
			if used[idx] {
				return nil, fmt.Errorf("sqlshape: result column %q matches %s.%s twice", name, rt, flat[idx].Name)
			}
			used[idx] = true
			m.fields[i] = flat[idx].Index
		}
		for i, f := range flat {
			if !used[i] && !optionalKind(f.Type) {
				// a nullable field may be left unset by a branch that does not select it
				return nil, fmt.Errorf("sqlshape: field %s.%s has no result column (%s)", rt.Name(), f.Name, f.Column)
			}
		}
	}
	mapperCache.Store(key, m)
	return m, nil
}

var scannerType = reflect.TypeOf((*sql.Scanner)(nil)).Elem()

// optionalKind reports whether a field type can stay unset when a branch does not select
// it: pointer, slice, map, interface, sql.Null*, or a type that scans NULL itself through
// sql.Scanner. Kept in sync with the checker's nullable rule.
func optionalKind(t reflect.Type) bool {
	switch t.Kind() {
	case reflect.Pointer, reflect.Slice, reflect.Map, reflect.Interface:
		return true
	}
	if t.PkgPath() == "database/sql" && strings.HasPrefix(t.Name(), "Null") {
		return true
	}
	return t.PkgPath() != "" && !sqlshape.ScalarStruct(t) && reflect.PointerTo(t).Implements(scannerType)
}

func (m *mapper[R]) scan(rows *sql.Rows) (R, error) {
	var row R
	var err error
	if m.scalar {
		err = rows.Scan(&row)
	} else {
		v := reflect.ValueOf(&row).Elem()
		dests := make([]any, len(m.fields))
		for i, idx := range m.fields {
			dests[i] = sqlshape.FieldByIndex(v, idx).Addr().Interface()
		}
		err = rows.Scan(dests...)
	}
	if err == nil && m.labelled {
		err = sqlshape.CheckLabels(reflect.ValueOf(row))
	}
	return row, err
}
