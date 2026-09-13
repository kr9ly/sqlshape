package mysql

import (
	"errors"
	"testing"

	driver "github.com/go-sql-driver/mysql"
)

// TestConstraintError_Error_Unwrap covers ConstraintError's Error() (both the keyed and
// unkeyed forms) and Unwrap(), none of which any server-backed test exercises directly.
func TestConstraintError_Error_Unwrap(t *testing.T) {
	inner := &driver.MySQLError{Number: 1644, Message: "boom"}
	keyed := &ConstraintError{Key: "45000", Message: "boom", Err: inner}
	if got, want := keyed.Error(), "sqlshape: constraint 45000 violated: boom"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
	if u := keyed.Unwrap(); u != inner {
		t.Errorf("Unwrap() = %v, want %v", u, inner)
	}

	unkeyed := &ConstraintError{Message: "boom", Err: inner}
	if got, want := unkeyed.Error(), "sqlshape: boom"; got != want {
		t.Errorf("Error() (no key) = %q, want %q", got, want)
	}
}

// TestViolates_NotConstraintError covers Violates's early return when err is not (or does
// not wrap) a *ConstraintError.
func TestViolates_NotConstraintError(t *testing.T) {
	if Violates(errors.New("plain"), "45000") {
		t.Error("Violates(plain error) = true, want false")
	}
	if Violates[string](nil, "45000") {
		t.Error("Violates(nil) = true, want false")
	}
}

// TestViolates_NoMatch covers the final "no match" fallthrough: a ConstraintError whose Key
// differs from the queried key, and whose Number is not one of the NOT NULL codes that fall
// back to a table.column suffix match.
func TestViolates_NoMatch(t *testing.T) {
	ce := &ConstraintError{Number: 1644, Key: "45000"}
	if Violates(ce, "30001") {
		t.Error("Violates with mismatched key and non-NOT-NULL number = true, want false")
	}
}

// TestWrapErr_PassThrough covers wrapErr's two pass-through branches: an error that is
// already a *ConstraintError (returned as-is), and an error that is neither a
// *ConstraintError nor a *driver.MySQLError (also returned as-is, e.g. context.Canceled).
func TestWrapErr_PassThrough(t *testing.T) {
	already := &ConstraintError{Key: "x"}
	if got := wrapErr(already); got != error(already) {
		t.Errorf("wrapErr(*ConstraintError) = %v, want the same value", got)
	}

	if wrapErr(nil) != nil {
		t.Errorf("wrapErr(nil) = %v, want nil", wrapErr(nil))
	}

	other := errors.New("not a driver error")
	if got := wrapErr(other); got != other {
		t.Errorf("wrapErr(plain error) = %v, want the same value", got)
	}
}

// TestWrapErr_1364_NoDefaultValue covers the 1364 "doesn't have a default value" message
// shape (reBadNull), distinct from 1364/1048's "cannot be null" shape (reNotNull) the
// server-backed tests already exercise via a NOT NULL column with no explicit value.
func TestWrapErr_1364_NoDefaultValue(t *testing.T) {
	me := &driver.MySQLError{Number: 1364, Message: "Field 'name' doesn't have a default value"}
	got := wrapErr(me)
	var ce *ConstraintError
	if !errors.As(got, &ce) {
		t.Fatalf("wrapErr(1364) = %v, want *ConstraintError", got)
	}
	if ce.Key != "name" {
		t.Errorf("Key = %q, want %q", ce.Key, "name")
	}
}

// TestWrapErr_UnknownSQLState covers wrapErr's default case for a MySQLError whose number is
// none of the ones the switch names and whose SQLSTATE does not start with "45" (so it is not
// a routine/trigger SIGNAL that set its own MYSQL_ERRNO either) — an ordinary server error
// passes through unmapped.
func TestWrapErr_UnknownSQLState(t *testing.T) {
	me := &driver.MySQLError{Number: 1146, Message: "Table 'x' doesn't exist", SQLState: [5]byte{'4', '2', 'S', '0', '2'}}
	got := wrapErr(me)
	if got != error(me) {
		t.Errorf("wrapErr(unmapped) = %v, want the original error", got)
	}
}
