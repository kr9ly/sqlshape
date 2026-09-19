package analyze

// A comparison of a DATE / DATETIME / TIMESTAMP value with a constant string converts the
// string when the comparator is set up (Arg_comparator::set_cmp_func caching the constant,
// get_date_from_string): a string that does not parse as a datetime under the session's
// sql_mode fails the statement wherever the comparison sits -- a WHERE, a select item, a
// JOIN's ON, a HAVING, an ORDER BY or GROUP BY item, a derived table's or CTE's query, a
// branch a constant decides against -- whatever the statement kind and however the tables
// look (measured: SELECT 1 FROM tt WHERE dt = 'abc' fails over an empty, unindexed table).
// The error is 1525 "Incorrect DATETIME value", or, in a strict write against a plain
// column, the store-shaped 1292 "Incorrect datetime value: ... for column ... at row 1".
// A string that parses but carries a time zone displacement over a zero month or day fails
// the displacement conversion instead, whatever the mode: 1292 "Truncated incorrect
// temporal value", the datetime respelt without its fraction and the displacement's hour
// unpadded (measured: '2020-00-01 00:00:00.123456+00:00' -> '2020-00-01 00:00:00+0:00').
// BETWEEN and IN do not convert eagerly (measured: both run over 'abc').
//
// A comparison, BETWEEN or IN of a numeric column with a constant string that holds no
// number ('' aside, which is 0 silently) is the warning 1292 "Truncated incorrect DOUBLE
// value", an error only in a strict write, eagerly too (measured: UPDATE ... WHERE i =
// '1invalid' fails over an empty table; the SELECT runs; '1e1', '1.5', ' 1' all pass).
//
// The same escalation reads a constant string operand of a bit operator as a whole
// integer: junk or '' is 1292 "Truncated incorrect INTEGER value" (measured: UPDATE ...
// WHERE (c IS NULL) >> ('') fails over an empty table).
//
// Not read: a scalar subquery's constant (SELECT 'abc') as the compared string (the server
// folds it; this file does not), a TIME or YEAR column's own per-row string conversion
// (TIME parses per row; YEAR compares as a number and falls under the numeric rule), a
// numeric expression that is not a plain column, and anything inside a trigger's or
// routine's body.

import (
	"fmt"
	"strings"

	"github.com/kr9ly/sqlshape/check/mysql/v2/internal/mysqlast"
)

// compareConstString judges the operands of one comparison (kind "cmp"), a BETWEEN or an
// IN: the numeric rule alone applies to the latter two, and a BETWEEN's conversion runs
// per row (measured: over an empty table it passes; over a row it fails), so its hit is
// the violation 1292, where a comparison's and an IN's are the statement's error.
func (a *analyzer) compareConstString(sc scope, args []mysqlast.Value, ts []typed, kind string) *Error {
	if a.routine != nil || a.trig != nil {
		return nil
	}
	for i, t := range ts {
		if !t.known {
			continue
		}
		switch t.typ.Name {
		case "date", "datetime", "timestamp":
			if kind != "cmp" {
				continue
			}
			for j, arg := range args {
				if j == i {
					continue
				}
				if e := a.temporalCompared(sc, args[i], t.typ.Name, arg); e != nil {
					return e
				}
			}
		case "tinyint", "smallint", "mediumint", "int", "bigint", "decimal", "double", "float", "year":
			if !a.strictWriteStmt() {
				continue
			}
			if _, ok := a.comparedColumn(sc, args[i]); !ok {
				continue
			}
			for j, arg := range args {
				if j == i {
					continue
				}
				s, at, ok := constString(arg)
				if !ok || s == "" {
					continue
				}
				if _, tail, good := numericString(s); !good || tail {
					if kind == "between" {
						table := ""
						if a.write != nil && a.write.table != nil {
							table = a.write.table.Name
						}
						a.storeRaised = append(a.storeRaised, Violation{Code: code1292, Constraint: itoa(code1292), Table: table, SQLState: constraintSQLState(code1292)})
						return nil
					}
					return &Error{Message: fmt.Sprintf("Truncated incorrect DOUBLE value: '%s'", s), Code: code1292, Position: a.ph.Back(at)}
				}
			}
		}
	}
	return nil
}

// temporalCompared judges one constant string compared with a date / datetime / timestamp
// value of type typName.
func (a *analyzer) temporalCompared(sc scope, side mysqlast.Value, typName string, arg mysqlast.Value) *Error {
	s, at, ok := constString(arg)
	if !ok {
		return nil
	}
	var tm mysqlTime
	var warn int
	if !strToDatetime(s, a.dateFlags("date"), &tm, &warn) || warn&(timeWarnTruncated|timeWarnOutOfRange|timeWarnZeroDate|timeWarnZeroInDate) != 0 {
		if a.strictWriteStmt() {
			if name, ok := a.comparedColumn(sc, side); ok {
				return &Error{Message: fmt.Sprintf("Incorrect %s value: '%s' for column '%s' at row 1", temporalWord(typName), s, name), Code: code1292, Position: a.ph.Back(at)}
			}
		}
		return &Error{Message: fmt.Sprintf("Incorrect %s value: '%s'", strings.ToUpper(typName), s), Code: 1525, Position: a.ph.Back(at)}
	}
	if tm.hasTZ && (tm.month == 0 || tm.day == 0) {
		return &Error{Message: fmt.Sprintf("Truncated incorrect temporal value: '%s'", displacedSpelling(tm)), Code: code1292, Position: a.ph.Back(at)}
	}
	return nil
}

// displacedSpelling respells a displaced datetime the way the conversion's error message
// does: no fraction, the displacement's hour unpadded.
func displacedSpelling(tm mysqlTime) string {
	off := tm.tzOffset
	sign := "+"
	if off < 0 {
		sign = "-"
		off = -off
	}
	return fmt.Sprintf("%04d-%02d-%02d %02d:%02d:%02d%s%d:%02d",
		tm.year, tm.month, tm.day, tm.hour, tm.minute, tm.second, sign, off/3600, off%3600/60)
}

// bitOpStringOperand judges the constant string operands of a bit operator, landing the
// failure the way constCheck does: the statement's error in a condition or an INSERT's
// VALUES, the violation 1292 where the expression runs per row.
func (a *analyzer) bitOpStringOperand(sc scope, n *mysqlast.Node, where string) *Error {
	if !a.strictWriteStmt() || a.noFold > 0 {
		return nil
	}
	for _, arg := range exprArgs(n) {
		s, at, ok := constString(arg)
		if !ok {
			continue
		}
		if _, good := integerString(s); good {
			continue
		}
		perRow := a.foldPerRow && !condContext(where)
		if perRow {
			table := ""
			if a.write != nil && a.write.table != nil {
				table = a.write.table.Name
			}
			a.storeRaised = append(a.storeRaised, Violation{Code: code1292, Constraint: itoa(code1292), Table: table, SQLState: constraintSQLState(code1292)})
			return nil
		}
		return &Error{Message: fmt.Sprintf("Truncated incorrect INTEGER value: '%s'", s), Code: code1292, Position: a.ph.Back(at)}
	}
	return nil
}

// comparedColumn resolves a comparison operand to its plain column's name, unwrapping the
// expression markers.
func (a *analyzer) comparedColumn(sc scope, v mysqlast.Value) (string, bool) {
	n, ok := unwrapExpr(v)
	if !ok || !isColumnRef(n) {
		return "", false
	}
	ref, ok := a.plainColumn(sc, n)
	if !ok || ref.col == nil {
		return "", false
	}
	return ref.col.Name, true
}

// constString is a constant string literal's text, unwrapping PTI_udf_expr and COLLATE.
func constString(v mysqlast.Value) (s string, at int, ok bool) {
	n, good := unwrapExpr(v)
	if !good {
		return "", 0, false
	}
	s, good = stringLiteral(n)
	return s, n.Start, good
}

// unwrapExpr strips PTI_udf_expr and Item_func_set_collation wrappers.
func unwrapExpr(v mysqlast.Value) (*mysqlast.Node, bool) {
	for {
		n, ok := v.(*mysqlast.Node)
		if !ok {
			return nil, false
		}
		switch n.Class {
		case "PTI_udf_expr":
			v = n.Arg("expr")
		case "Item_func_set_collation":
			v = n.Arg("a")
		default:
			return n, true
		}
	}
}
