package mysql

import (
	"errors"
	"regexp"
	"strconv"
	"strings"

	driver "github.com/go-sql-driver/mysql"
)

// ConstraintError is a constraint violation MySQL reported, mapped back to the schema:
// Key is the constraint the way the schema names it — a UNIQUE or PRIMARY key's name, a
// foreign key's constraint name, a CHECK constraint's name, or table.column for a NOT NULL
// column — so that Violates(err, "users_email_key") reads like the checker's own naming. A
// trigger or routine body's own SIGNAL is a ConstraintError too: Key is the SQLSTATE (e.g.
// "45000") when the server reports 1644 (ER_SIGNAL_EXCEPTION) or 1643 (ER_SIGNAL_NOT_FOUND),
// or the decimal MYSQL_ERRNO (e.g. "30001") when SIGNAL set its own error number (a '45'
// SQLSTATE), matching a schema's `-- sqlshape: error <key> = <name>` annotation. 1442 (a
// trigger/routine writing a table already in use up the invoking statement's chain) and
// 1172 (SELECT ... INTO with more than one row) are keyed the same way, by their own
// decimal error number, since the checker's own model has no constraint name for either.
// 1369 (ER_VIEW_CHECK_FAILED, a write through a WITH CHECK OPTION view whose new row does
// not satisfy the view's WHERE, or -- CASCADED -- an underlying view's) is keyed by the
// view's own name, the way the message names it ("CHECK OPTION failed 'db.view'"): unlike
// PostgreSQL's 44000, which names nothing (measured; docs/postgres.md keys it by the bare
// SQLSTATE), MySQL's own message always names the view, so the checker's violations.go
// does too (Violation.Table / .Constraint, both the view's name).
type ConstraintError struct {
	Number  uint16 // MySQL's error number: 1062 duplicate key, 1452 / 1451 foreign key, 1048 NOT NULL, 3819 CHECK, 1644 / 1643 SIGNAL, 1442, 1172, 1369 CHECK OPTION
	Key     string
	Message string
	Err     *driver.MySQLError
}

func (e *ConstraintError) Error() string {
	if e.Key != "" {
		return "sqlshape: constraint " + e.Key + " violated: " + e.Message
	}
	return "sqlshape: " + e.Message
}

func (e *ConstraintError) Unwrap() error { return e.Err }

// Violates reports whether err is a violation of the named constraint. A NOT NULL violation
// is named table.column, the way the checker's expect line spells it; the server's message
// names the column alone, so the bare column matches too. key is the constraint's name, a
// table.column, a raw error code, or a sqlshape.Failure a program declared with
// sqlshape.Error(code) for a `-- sqlshape: error` annotation's code — Violates judges by
// the code either way, never by a schema-declared Name (it does not read the schema).
func Violates[K ~string](err error, key K) bool {
	var ce *ConstraintError
	if !errors.As(err, &ce) {
		return false
	}
	k := string(key)
	if ce.Key == k {
		return true
	}
	if ce.Number == 1048 || ce.Number == 1364 {
		return strings.HasSuffix(k, "."+ce.Key)
	}
	return false
}

var (
	reDuplicate   = regexp.MustCompile(`for key '(?:[^'.]+\.)?([^']+)'$`)                // 1062: Duplicate entry 'x' for key 'users.email'
	reForeign     = regexp.MustCompile("CONSTRAINT `([^`]+)`")                           // 1452 / 1451: ... CONSTRAINT `fk_orders_user` FOREIGN KEY ...
	reNotNull     = regexp.MustCompile(`^Column '([^']+)' cannot be null$`)              // 1048
	reCheck       = regexp.MustCompile(`^Check constraint '([^']+)' is violated\.$`)     // 3819
	reBadNull     = regexp.MustCompile(`^Field '([^']+)' doesn't have a default value$`) // 1364
	reCheckOption = regexp.MustCompile(`^CHECK OPTION failed '(?:[^'.]+\.)?([^']+)'$`)   // 1369: "CHECK OPTION failed 'db.view'"
	reViewDefault = regexp.MustCompile(`^Field of view '(?:[^'.]+\.)?([^']+)' underlying table doesn't have a default value$`) // 1423
)

// WrapError is the error Run / Exec would return for err when it came from a statement run
// outside them (database/sql directly, a migration script, a test): a *ConstraintError for
// an integrity error, so that Violates can judge it; err itself otherwise.
func WrapError(err error) error { return wrapErr(err) }

// wrapErr maps MySQL's integrity errors to ConstraintError; other errors pass through.
func wrapErr(err error) error {
	var ce *ConstraintError
	if err == nil || errors.As(err, &ce) {
		return err
	}
	var me *driver.MySQLError
	if !errors.As(err, &me) {
		return err
	}
	c := &ConstraintError{Number: me.Number, Message: me.Message, Err: me}
	switch me.Number {
	case 1062, 1586: // duplicate entry
		if m := reDuplicate.FindStringSubmatch(me.Message); m != nil {
			c.Key = m[1]
		}
	case 1451, 1452, 1216, 1217: // foreign key
		if m := reForeign.FindStringSubmatch(me.Message); m != nil {
			c.Key = m[1]
		}
	case 1048, 1364: // NOT NULL
		if m := reNotNull.FindStringSubmatch(me.Message); m != nil {
			c.Key = m[1]
		} else if m := reBadNull.FindStringSubmatch(me.Message); m != nil {
			c.Key = m[1]
		}
	case 3819: // CHECK
		if m := reCheck.FindStringSubmatch(me.Message); m != nil {
			c.Key = m[1]
		}
	case 1369: // ER_VIEW_CHECK_FAILED
		if m := reCheckOption.FindStringSubmatch(me.Message); m != nil {
			c.Key = m[1]
		}
	case 1423: // ER_NO_DEFAULT_FOR_VIEW_FIELD: 1364 through a view, keyed by the view's name
		if m := reViewDefault.FindStringSubmatch(me.Message); m != nil {
			c.Key = m[1]
		}
	case 1644, 1643: // SIGNAL / RESIGNAL (ER_SIGNAL_EXCEPTION, ER_SIGNAL_NOT_FOUND)
		c.Key = string(me.SQLState[:])
	case 1442, 1172, 1416, 1690, 1292: // a trigger (or a called routine) writing a table already in
		// use up the invoking statement's chain; SELECT ... INTO with more than one row; a
		// geometry of another type than the spatial column's; a constant the server cannot
		// compute, run per row; a constant cast or conversion the value does not survive,
		// in a strict write -- the checker's own model (violations.go, call.go, geom.go,
		// fold.go) has no constraint name for any of them, so it keys them by their own
		// error number (Constraint: strconv.Itoa(code)), and Violates(err, "1442" / "1172" /
		// "1416" / "1690" / "1292") matches that key back here.
		c.Key = strconv.Itoa(int(me.Number))
	default:
		if strings.HasPrefix(string(me.SQLState[:]), "45") {
			// SIGNAL gave its own MYSQL_ERRNO (a builtin number impersonated this way falls
			// back to the regexes above, which is the best effort documented for that case)
			c.Key = strconv.Itoa(int(me.Number))
			break
		}
		return err
	}
	if c.Key == "" {
		// the number is a constraint's but the message is not the server's own for it: a
		// SIGNAL that set MYSQL_ERRNO to a builtin number (measured: SQLSTATE '23000',
		// MYSQL_ERRNO = 1062 from a trigger). A SIGNAL's key is its MYSQL_ERRNO as decimal
		// text, whichever number it chose.
		c.Key = strconv.Itoa(int(me.Number))
	}
	c.Key = strings.TrimSpace(c.Key)
	return c
}
