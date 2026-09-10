package mysql

import (
	"errors"
	"regexp"
	"strings"

	driver "github.com/go-sql-driver/mysql"
)

// ConstraintError is a constraint violation MySQL reported, mapped back to the schema:
// Key is the constraint the way the schema names it — a UNIQUE or PRIMARY key's name, a
// foreign key's constraint name, a CHECK constraint's name, or table.column for a NOT NULL
// column — so that Violates(err, "users_email_key") reads like the checker's own naming.
type ConstraintError struct {
	Number  uint16 // MySQL's error number: 1062 duplicate key, 1452 / 1451 foreign key, 1048 NOT NULL, 3819 CHECK
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
// names the column alone, so the bare column matches too.
func Violates(err error, key string) bool {
	var ce *ConstraintError
	if !errors.As(err, &ce) {
		return false
	}
	if ce.Key == key {
		return true
	}
	if ce.Number == 1048 || ce.Number == 1364 {
		return strings.HasSuffix(key, "."+ce.Key)
	}
	return false
}

var (
	reDuplicate = regexp.MustCompile(`for key '(?:[^'.]+\.)?([^']+)'$`)                // 1062: Duplicate entry 'x' for key 'users.email'
	reForeign   = regexp.MustCompile("CONSTRAINT `([^`]+)`")                           // 1452 / 1451: ... CONSTRAINT `fk_orders_user` FOREIGN KEY ...
	reNotNull   = regexp.MustCompile(`^Column '([^']+)' cannot be null$`)              // 1048
	reCheck     = regexp.MustCompile(`^Check constraint '([^']+)' is violated\.$`)     // 3819
	reBadNull   = regexp.MustCompile(`^Field '([^']+)' doesn't have a default value$`) // 1364
)

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
	default:
		return err
	}
	c.Key = strings.TrimSpace(c.Key)
	return c
}
