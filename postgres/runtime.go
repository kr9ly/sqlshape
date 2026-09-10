package postgres

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

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/kr9ly/sqlshape"
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
func wrapErr[R, P any](s sqlshape.Stmt[R, P], err error) error {
	return wrapPgErr(err, expects(s.Template))
}

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

// isStaleResultTypeErr reports whether err is PostgreSQL's "cached plan must not change
// result type" (SQLSTATE 0A000): formatCache asked for a column's format by an OID from a
// composite/domain type DDL has since recreated (same name, new OID), and requesting an
// explicit QueryResultFormatsByOID bypasses pgx's own describe-and-reprepare path that
// would otherwise absorb the change.
func isStaleResultTypeErr(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "0A000" && strings.Contains(pgErr.Message, "cached plan must not change result type")
}

var expectRe = regexp.MustCompile(`(?m)^[ \t]*--[ \t]*sqlshape:[ \t]*expect[ \t]+(.+?)[ \t]*$`)

// expects reports, for a template, whether its `-- sqlshape: expect` line names a SQLSTATE.
func expects(template string) func(code string) bool {
	return func(code string) bool {
		for _, m := range expectRe.FindAllStringSubmatch(template, -1) {
			for _, item := range strings.Split(m[1], ",") {
				if strings.TrimSpace(item) == code {
					return true
				}
			}
		}
		return false
	}
}

// ErrManyRows is returned by Get / Find when a statement declared with One produced
// more than one row: the schema no longer backs the proof the checker made.
var ErrManyRows = errors.New("sqlshape: statement declared with One returned more than one row")

// Run executes the statement s and yields each row mapped into R. Iteration stops at
// the first error, which is yielded with a zero R. Breaking out early closes the rows.
func Run[R, P any](ctx context.Context, db DB, s sqlshape.Stmt[R, P], p P) iter.Seq2[R, error] {
	return func(yield func(R, error) bool) {
		var zero R
		r, err := s.Render(p)
		if err != nil {
			yield(zero, err)
			return
		}
		fkey := formatKey{typ: reflect.TypeOf(zero), sql: r.SQL}
		var formats pgx.QueryResultFormatsByOID
		if f, ok := formatCache.Load(fkey); ok {
			formats = f.(pgx.QueryResultFormatsByOID)
		}
		if runAttempt(ctx, db, s, r, formats, yield) == staleFormatCache {
			// the cached result-format request named a column OID from a composite /
			// domain type DDL has since recreated (same name, new OID); nothing was
			// yielded yet, so it is safe to drop the entry and retry once from scratch.
			formatCache.Delete(fkey)
			runAttempt(ctx, db, s, r, nil, yield)
		}
	}
}

// runOutcome is what one execution of a statement ended with.
type runOutcome int

const (
	runDone runOutcome = iota
	staleFormatCache
)

// runAttempt runs one execution of the statement (formats nil for the connection's
// normal decoding, or a formatCache entry requesting text format for Scanner columns)
// and yields its rows to yield. It returns staleFormatCache, without yielding anything,
// only when the very first thing the server reported was PostgreSQL's "cached plan must
// not change result type" (0A000) for a request that came from formatCache: no row had
// been produced yet, so the caller may retry with the entry dropped.
func runAttempt[R, P any](ctx context.Context, db DB, s sqlshape.Stmt[R, P], r sqlshape.Rendered, formats pgx.QueryResultFormatsByOID, yield func(R, error) bool) runOutcome {
	var zero R
	var rows pgx.Rows
	err := withParamTypes(ctx, db, len(r.Args), func() error {
		var qerr error
		rows, qerr = db.Query(ctx, r.SQL, args(s.IsUnprepared(), pgArgs(r.Args), formats)...)
		return qerr
	})
	if err != nil {
		if formats != nil && isStaleResultTypeErr(err) {
			return staleFormatCache
		}
		yield(zero, wrapErr(s, err))
		return runDone
	}
	// result types the connection cannot decode yet (user enums, composites): load them and re-run
	if oids := unknownTypes(rows.Conn(), rows.FieldDescriptions()); len(oids) > 0 {
		conn := rows.Conn()
		rows.Close()
		if err := loadTypes(ctx, conn, oids); err != nil {
			yield(zero, err)
			return runDone
		}
		if rows, err = db.Query(ctx, r.SQL, args(s.IsUnprepared(), pgArgs(r.Args), formats)...); err != nil {
			if formats != nil && isStaleResultTypeErr(err) {
				return staleFormatCache
			}
			yield(zero, wrapErr(s, err))
			return runDone
		}
	}
	var m *mapper[R]
	fkey := formatKey{typ: reflect.TypeOf(zero), sql: r.SQL}
	if fds := rows.FieldDescriptions(); len(fds) > 0 { // empty when the statement failed: rows.Err tells
		if m, err = newMapper[R](fds); err != nil {
			rows.Close()
			yield(zero, err)
			return runDone
		}
		// columns a Scanner receives must come as text: remember that for this statement and re-run once
		if len(m.textOIDs) > 0 && formats == nil {
			formatCache.Store(fkey, m.textOIDs)
			rows.Close()
			if rows, err = db.Query(ctx, r.SQL, args(s.IsUnprepared(), pgArgs(r.Args), m.textOIDs)...); err != nil {
				yield(zero, wrapErr(s, err))
				return runDone
			}
		}
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		n++
		if m == nil {
			if m, err = newMapper[R](rows.FieldDescriptions()); err != nil {
				yield(zero, err)
				return runDone
			}
		}
		row, err := m.scan(rows)
		if err != nil {
			yield(zero, err)
			return runDone
		}
		if !yield(row, nil) {
			return runDone
		}
	}
	if err := rows.Err(); err != nil {
		if n == 0 && formats != nil && isStaleResultTypeErr(err) {
			return staleFormatCache
		}
		yield(zero, wrapErr(s, err))
	}
	return runDone
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

// Exec runs a statement that returns no rows (INSERT / UPDATE / DELETE without RETURNING).
func Exec[R, P any](ctx context.Context, db DB, s sqlshape.Stmt[R, P], p P) (pgconn.CommandTag, error) {
	r, err := s.Render(p)
	if err != nil {
		return pgconn.CommandTag{}, err
	}
	var tag pgconn.CommandTag
	err = withParamTypes(ctx, db, len(r.Args), func() error {
		tag, err = db.Exec(ctx, r.SQL, args(s.IsUnprepared(), pgArgs(r.Args), nil)...)
		return err
	})
	return tag, wrapErr(s, err)
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

// args prefixes the execution options pgx reads from the first arguments: the exec mode
// for unprepared statements and the result formats Scanner-receiving columns need.
func args(unprepared bool, a []any, formats pgx.QueryResultFormatsByOID) []any {
	var opts []any
	if unprepared {
		opts = append(opts, pgx.QueryExecModeExec)
	}
	if len(formats) > 0 {
		opts = append(opts, formats)
	}
	if len(opts) == 0 {
		return a
	}
	return append(opts, a...)
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
	var found R
	var zero R
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

// ExecOne runs a single-row statement that returns no rows (e.g. UPDATE ... WHERE id = $1).
// An INSERT / UPDATE / DELETE / MERGE that touched no row returns ErrNoRows (the key did
// not exist), one that touched more than one returns ErrManyRows (the checker's proof
// that the statement affects at most one row did not hold).
func ExecOne[R, P any](ctx context.Context, db DB, s sqlshape.Single[R, P], p P) (pgconn.CommandTag, error) {
	tag, err := Exec(ctx, db, s.Stmt(), p)
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

// --- row mapping -----------------------------------------------------------

// mapper binds result columns to the fields of R (or to R itself when it is a scalar).
type mapper[R any] struct {
	scalar bool
	fields [][]int // field index path per column (embedded structs flattened)
	// labelled is whether R contains a type implementing Known (enum labels the
	// application knows): rows are validated after scanning
	labelled bool
	// textOIDs are the result column types received by a sql.Scanner (a declared type
	// doing its own decoding): pgx hands a Scanner the raw binary of a registered type,
	// so these columns are requested in text format (QueryResultFormatsByOID).
	textOIDs pgx.QueryResultFormatsByOID
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
	m := &mapper[R]{labelled: sqlshape.HasLabelled(rt, map[reflect.Type]bool{})}
	if rt.Kind() != reflect.Struct || rt.PkgPath() == "time" {
		if len(fds) != 1 {
			return nil, fmt.Errorf("sqlshape: R is %s but the query returns %d columns", rt, len(fds))
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
				return nil, fmt.Errorf("sqlshape: result column %q matches %s.%s twice", fd.Name, rt, flat[idx].Name)
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
	if m.scalar {
		if userScanner(rt) {
			m.textOIDs = pgx.QueryResultFormatsByOID{fds[0].DataTypeOID: pgx.TextFormatCode}
		}
	} else {
		for i, idx := range m.fields {
			if userScanner(fieldByIndexType(rt, idx)) {
				if m.textOIDs == nil {
					m.textOIDs = pgx.QueryResultFormatsByOID{}
				}
				m.textOIDs[fds[i].DataTypeOID] = pgx.TextFormatCode
			}
		}
	}
	mapperCache.Store(key, m)
	return m, nil
}

// userScanner reports whether t decodes itself through sql.Scanner (pointer receiver
// included), leaving out the types pgx knows better (pgtype, time, netip).
func userScanner(t reflect.Type) bool {
	base := t
	if base.Kind() == reflect.Pointer {
		base = base.Elem()
	}
	if base.Kind() == reflect.Slice || base.Kind() == reflect.Array {
		base = base.Elem()
		if base.Kind() == reflect.Pointer {
			base = base.Elem()
		}
	}
	if sqlshape.ScalarStruct(base) || base.PkgPath() == "" {
		return false
	}
	return reflect.PointerTo(base).Implements(scannerType)
}

var scannerType = reflect.TypeOf((*interface{ Scan(any) error })(nil)).Elem()

func fieldByIndexType(t reflect.Type, index []int) reflect.Type {
	for _, i := range index {
		if t.Kind() == reflect.Pointer {
			t = t.Elem()
		}
		t = t.Field(i).Type
	}
	return t
}

// formatCache remembers, per (R, SQL), the result formats a statement needs, so the
// text-format request rides along from the second execution on.
var formatCache sync.Map // formatKey → pgx.QueryResultFormatsByOID

type formatKey struct {
	typ reflect.Type
	sql string
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
			fv := sqlshape.FieldByIndex(v, idx)
			if sqlshape.IsRow(fv.Type()) {
				dests[i] = nestedDest(fv)
			} else {
				dests[i] = fv.Addr().Interface()
			}
		}
		err = rows.Scan(dests...)
	}
	if err == nil && m.labelled {
		err = sqlshape.CheckLabels(reflect.ValueOf(row))
	}
	return row, err
}

// IsNoRows reports whether err is ErrNoRows.
func IsNoRows(err error) bool { return errors.Is(err, ErrNoRows) }

// optionalKind reports whether a field type can stay unset when a branch does not select
// it: pointer, slice, map, interface, sql.Null*, pgtype.*, or a struct that scans NULL
// itself through sql.Scanner. Kept in sync with the checker's nullable rule
// (internal/vet/gotypes.go unwrapNullable / matchDir) rather than a fixed type list, so a
// user's own sql.Scanner type is recognized the same way the checker recognizes it.
func optionalKind(t reflect.Type) bool {
	switch t.Kind() {
	case reflect.Pointer, reflect.Slice, reflect.Map, reflect.Interface:
		return true
	}
	pkg := t.PkgPath()
	if pkg == "database/sql" && strings.HasPrefix(t.Name(), "Null") {
		return true // sql.NullString etc.: type checked loosely
	}
	if strings.HasSuffix(pkg, "jackc/pgx/v5/pgtype") {
		return true // pgtype.* all carry Valid
	}
	// a Scanner sees NULL as Scan(nil) and represents it itself
	if pkg != "" && !sqlshape.ScalarStruct(t) && reflect.PointerTo(t).Implements(scannerType) {
		return true
	}
	return false
}
