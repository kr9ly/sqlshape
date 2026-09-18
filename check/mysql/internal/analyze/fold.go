package analyze

import (
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"

	"github.com/kr9ly/sqlshape/check/mysql/v2/internal/mysqlast"
	"github.com/kr9ly/sqlshape/v2/x/sqlmode"
)

// Constant arithmetic the server cannot compute: ER_DATA_OUT_OF_RANGE (1690).
//
// The server evaluates a constant expression once, before or as the statement runs, and
// an integer result that does not fit the operator's BIGINT / BIGINT UNSIGNED, a DOUBLE
// that is infinite, a function's DOUBLE cast to an integer it does not fit, or a
// RANDOM_BYTES length outside 1 .. 1024 fails the statement with 1690. This file evaluates
// what the corpus probe found the server evaluating (item_func.cc's int_op / real_op and
// check_integer_overflow / check_float_overflow, measured in TestConstantFoldServer):
//
//   - the literals (Item_int / Item_uint / Item_decimal / Item_float, TRUE / FALSE, a quoted
//     string, NULL), unary minus, + - * / DIV %, ABS, EXP, POW / POWER, COT, DEGREES,
//     ROUND with a negative number of places over an integer, RANDOM_BYTES, and CAST to
//     SIGNED / UNSIGNED / FLOAT / DOUBLE. Anything else is not a constant to this file,
//     and an expression it does not model is left alone;
//   - an integer operator's result is unsigned when either operand is (subtraction not
//     under NO_UNSIGNED_SUBTRACTION); the exact result must fit that type. A string
//     operand makes the operator a DOUBLE one (my_strtod: 'abc' is 0), a decimal one a
//     DECIMAL one (never judged: my_decimal's overflow rule is not modeled);
//   - DIV over non-integers divides as decimals and the truncated quotient must fit
//     (either operand unsigned: BIGINT UNSIGNED);
//   - CAST(f AS SIGNED / UNSIGNED) of a DOUBLE a function or operator computed fails when
//     it does not fit (llrint_with_overflow_check), naming the inner expression; a DOUBLE
//     literal is clamped instead (Item_float::val_int), as a decimal or a string is;
//   - the message spells the expression as Item::print does: `(a + b)`, `-(x)`,
//     `pow(2,1024)`, `cast(x as unsigned)`.
//
// Where the failure lands follows how the server evaluates (measured):
//
//   - in a WHERE / HAVING / ON, the optimizer folds the constant before any row is read:
//     the statement fails whatever the data, and so does a constant select item of a
//     query without a FROM (or FROM DUAL), a derived table's or CTE's, an INSERT ...
//     VALUES value, a SET. The checker raises the error;
//   - in a select item of a query with a FROM, an UPDATE's SET, an ON DUPLICATE KEY UPDATE
//     assignment, the expression runs per row: no row, no failure. The checker lists the
//     violation 1690 (keyed "1690" the way 1416 is) instead of failing the statement;
//   - AND / OR stop at the first constant that decides them (a false or, in a condition,
//     a NULL for AND; a true for OR): the constants after it are never evaluated, though
//     a nested AND / OR inside a condition still is. IF / CASE / COALESCE / IFNULL evaluate
//     only the branch a constant condition picks, `c IN (list)` with a constant c stops at
//     the first match, `x IS NULL` over a never-NULL x is folded to false without
//     evaluating x (Item_func_isnull::fix_fields). GROUP BY and ORDER BY drop constant
//     items unevaluated; so does EXISTS with its select list;
//   - a trigger's or routine's body is not judged (a body's run-time failures are its
//     raised violations, and the body walk has no row context).
//
// Ceilings: a comparison whose other side is an aggregate over no rows (NULL) skips the
// constant; `IN (SELECT c FROM t)` folds c into the rewritten condition and fails even over
// an empty t; a hex or bit literal operand is not folded (0x1 is a string to +, b'1' an
// integer); a user variable's value is not known.

type ckind int

const (
	cNull ckind = iota
	cInt
	cUint
	cDec
	cReal
	cStr
)

// cval is a constant's value while folding.
type cval struct {
	kind ckind
	i    *big.Int // cInt / cUint
	d    *big.Rat // cDec
	r    float64  // cReal
	s    string   // cStr
	fn   bool     // computed by a function or operator, not written as a literal
}

// foldFail is a 1690 the folding met: the type word of the message ("BIGINT", "BIGINT
// UNSIGNED", "DOUBLE"), the expression printed, and where it starts.
type foldFail struct {
	kind  string
	print string
	at    int
	raw   string // a whole message, when kind / print do not apply (RANDOM_BYTES)
}

func (f *foldFail) message() string {
	if f.raw != "" {
		return f.raw
	}
	return fmt.Sprintf("%s value is out of range in '%s'", f.kind, f.print)
}

var (
	int64Min  = new(big.Int).Neg(new(big.Int).Lsh(big.NewInt(1), 63))
	int64Max  = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 63), big.NewInt(1))
	uint64Max = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 64), big.NewInt(1))
	two63     = new(big.Int).Lsh(big.NewInt(1), 63)
	two63f    = math.Ldexp(1, 63)
)

func (c cval) isNull() bool { return c.kind == cNull }

// asReal is the value in a DOUBLE context (my_strtod for a string: the numeric prefix,
// 0 when there is none).
func (c cval) asReal() float64 {
	switch c.kind {
	case cInt, cUint:
		f, _ := new(big.Float).SetInt(c.i).Float64()
		return f
	case cDec:
		f, _ := c.d.Float64()
		return f
	case cReal:
		return c.r
	case cStr:
		r, _, ok := numericString(c.s)
		if !ok {
			return 0
		}
		f, _ := r.Float64()
		return f
	}
	return 0
}

// asDec is the value in a DECIMAL context.
func (c cval) asDec() *big.Rat {
	switch c.kind {
	case cInt, cUint:
		return new(big.Rat).SetInt(c.i)
	case cDec:
		return c.d
	case cReal:
		r := new(big.Rat)
		if r.SetFloat64(c.r) == nil {
			return new(big.Rat)
		}
		return r
	case cStr:
		r, _, ok := numericString(c.s)
		if !ok {
			return new(big.Rat)
		}
		return r
	}
	return new(big.Rat)
}

// truth is the value as a condition: false for NULL too (null says which).
func (c cval) truth() (b, null bool) {
	switch c.kind {
	case cNull:
		return false, true
	case cInt, cUint:
		return c.i.Sign() != 0, false
	case cDec:
		return c.d.Sign() != 0, false
	case cReal:
		return c.r != 0, false
	case cStr:
		return c.asReal() != 0, false
	}
	return false, false
}

func intVal(i *big.Int, unsigned bool) cval {
	if unsigned {
		return cval{kind: cUint, i: i, fn: true}
	}
	return cval{kind: cInt, i: i, fn: true}
}

// fitsInt reports whether i fits the operator's result type.
func fitsInt(i *big.Int, unsigned bool) bool {
	if unsigned {
		return i.Sign() >= 0 && i.Cmp(uint64Max) <= 0
	}
	return i.Cmp(int64Min) >= 0 && i.Cmp(int64Max) <= 0
}

func intKind(unsigned bool) string {
	if unsigned {
		return "BIGINT UNSIGNED"
	}
	return "BIGINT"
}

// foldable says whether n is a node fold judges at its own level (its arguments are judged
// where the walk met them).
func foldable(n *mysqlast.Node) bool {
	switch n.Class {
	case "Item_func_neg", "Item_func_plus", "Item_func_minus", "Item_func_mul", "Item_func_div", "Item_func_div_int", "create_func_cast":
		return true
	case "PTI_function_call_generic_ident_sys":
		switch strings.ToUpper(str(n.Arg("ident"))) {
		case "ABS", "EXP", "POW", "POWER", "COT", "DEGREES", "ROUND", "RANDOM_BYTES":
			return true
		}
	}
	return false
}

// fold evaluates a constant expression. ok is false when v is not a constant this file
// models (its value is unknown); fail is the 1690 the evaluation met, ok or not.
func (a *analyzer) fold(v mysqlast.Value) (c cval, ok bool, fail *foldFail) {
	n, isNode := v.(*mysqlast.Node)
	if !isNode {
		return cval{}, false, nil
	}
	switch n.Class {
	case "Item_int":
		i, good := new(big.Int).SetString(str(n.Arg("i")), 10)
		return cval{kind: cInt, i: i}, good, nil
	case "Item_uint":
		i, good := new(big.Int).SetString(str(n.Arg("str")), 10)
		return cval{kind: cUint, i: i}, good, nil
	case "Item_decimal":
		d, good := new(big.Rat).SetString(str(n.Arg("str")))
		return cval{kind: cDec, d: d}, good, nil
	case "Item_float":
		f, err := strconv.ParseFloat(str(n.Arg("str")), 64)
		if err != nil || math.IsInf(f, 0) {
			return cval{}, false, nil
		}
		return cval{kind: cReal, r: f}, true, nil
	case "Item_null":
		return cval{kind: cNull}, true, nil
	case "Item_func_true":
		return cval{kind: cInt, i: big.NewInt(1)}, true, nil
	case "Item_func_false":
		return cval{kind: cInt, i: big.NewInt(0)}, true, nil
	case "PTI_text_literal_text_string", "PTI_text_literal_nchar_string", "PTI_text_literal_underscore_charset":
		s, good := stringLiteral(n)
		return cval{kind: cStr, s: s}, good, nil
	case "PTI_udf_expr":
		return a.fold(n.Arg("expr"))
	case "Item_func_neg":
		x, ok, fail := a.fold(n.Arg("a"))
		if !ok || fail != nil {
			return cval{}, false, fail
		}
		switch x.kind {
		case cNull:
			return x, true, nil
		case cInt:
			return intVal(new(big.Int).Neg(x.i), false), true, nil
		case cUint:
			// Item_func_neg::fix_num_length_and_dec: -9223372036854775808 is the BIGINT
			// minimum; a larger unsigned turns decimal
			if x.i.Cmp(two63) <= 0 {
				return intVal(new(big.Int).Neg(x.i), false), true, nil
			}
			return cval{kind: cDec, d: new(big.Rat).Neg(new(big.Rat).SetInt(x.i)), fn: true}, true, nil
		case cDec:
			return cval{kind: cDec, d: new(big.Rat).Neg(x.d), fn: true}, true, nil
		default:
			return cval{kind: cReal, r: -x.asReal(), fn: true}, true, nil
		}
	case "Item_func_plus", "Item_func_minus", "Item_func_mul", "Item_func_div", "Item_func_div_int", "Item_func_mod":
		return a.foldBinary(n)
	case "create_func_cast":
		return a.foldCast(n)
	case "PTI_function_call_generic_ident_sys":
		return a.foldCall(n)
	case "PTI_comp_op":
		return a.foldCompare(n)
	case "Item_func_not":
		x, ok, fail := a.fold(n.Arg("a"))
		if !ok || fail != nil {
			return cval{}, false, fail
		}
		if x.isNull() {
			return x, true, nil
		}
		b, _ := x.truth()
		if b {
			return cval{kind: cInt, i: big.NewInt(0), fn: true}, true, nil
		}
		return cval{kind: cInt, i: big.NewInt(1), fn: true}, true, nil
	}
	return cval{}, false, nil
}

// foldCompare is a comparison of two constants (for the conditions AND / OR / IF decide
// on): numbers as numbers, two strings as text, a string against a number as a number.
func (a *analyzer) foldCompare(n *mysqlast.Node) (cval, bool, *foldFail) {
	x, okx, fail := a.fold(n.Arg("left"))
	if fail != nil {
		return cval{}, false, fail
	}
	y, oky, fail := a.fold(n.Arg("right"))
	if fail != nil {
		return cval{}, false, fail
	}
	if !okx || !oky {
		return cval{}, false, nil
	}
	op, _ := n.Arg("boolfunc2creator").(mysqlast.Op)
	boolVal := func(b bool) (cval, bool, *foldFail) {
		if b {
			return cval{kind: cInt, i: big.NewInt(1), fn: true}, true, nil
		}
		return cval{kind: cInt, i: big.NewInt(0), fn: true}, true, nil
	}
	if op == "<=>" {
		if x.isNull() || y.isNull() {
			return boolVal(x.isNull() && y.isNull())
		}
	} else if x.isNull() || y.isNull() {
		return cval{kind: cNull}, true, nil
	}
	var cmp int
	switch {
	case x.kind == cStr && y.kind == cStr:
		cmp = strings.Compare(strings.ToLower(x.s), strings.ToLower(y.s))
	case x.kind == cReal || y.kind == cReal || x.kind == cStr || y.kind == cStr:
		xr, yr := x.asReal(), y.asReal()
		switch {
		case xr < yr:
			cmp = -1
		case xr > yr:
			cmp = 1
		}
	default:
		cmp = x.asDec().Cmp(y.asDec())
	}
	switch op {
	case "=", "<=>":
		return boolVal(cmp == 0)
	case "<>", "!=":
		return boolVal(cmp != 0)
	case "<":
		return boolVal(cmp < 0)
	case "<=":
		return boolVal(cmp <= 0)
	case ">":
		return boolVal(cmp > 0)
	case ">=":
		return boolVal(cmp >= 0)
	}
	return cval{}, false, nil
}

// foldBinary is + - * / DIV %.
func (a *analyzer) foldBinary(n *mysqlast.Node) (cval, bool, *foldFail) {
	x, okx, fail := a.fold(n.Arg("a"))
	if fail != nil {
		return cval{}, false, fail
	}
	y, oky, fail := a.fold(n.Arg("b"))
	if fail != nil {
		return cval{}, false, fail
	}
	if !okx || !oky {
		return cval{}, false, nil
	}
	if x.isNull() || y.isNull() {
		return cval{kind: cNull}, true, nil
	}
	real := x.kind == cReal || x.kind == cStr || y.kind == cReal || y.kind == cStr
	dec := x.kind == cDec || y.kind == cDec
	unsigned := x.kind == cUint || y.kind == cUint
	failWith := func(kind string) (cval, bool, *foldFail) {
		return cval{}, false, &foldFail{kind: kind, print: a.printItem(n), at: n.Start}
	}
	switch n.Class {
	case "Item_func_plus", "Item_func_minus", "Item_func_mul":
		if real {
			var r float64
			switch n.Class {
			case "Item_func_plus":
				r = x.asReal() + y.asReal()
			case "Item_func_minus":
				r = x.asReal() - y.asReal()
			default:
				r = x.asReal() * y.asReal()
			}
			if math.IsInf(r, 0) {
				return failWith("DOUBLE")
			}
			return cval{kind: cReal, r: r, fn: true}, true, nil
		}
		if dec {
			d := new(big.Rat)
			switch n.Class {
			case "Item_func_plus":
				d.Add(x.asDec(), y.asDec())
			case "Item_func_minus":
				d.Sub(x.asDec(), y.asDec())
			default:
				d.Mul(x.asDec(), y.asDec())
			}
			return cval{kind: cDec, d: d, fn: true}, true, nil
		}
		i := new(big.Int)
		switch n.Class {
		case "Item_func_plus":
			i.Add(x.i, y.i)
		case "Item_func_minus":
			i.Sub(x.i, y.i)
			if a.s.Settings.SQLMode.Has(sqlmode.NoUnsignedSubtraction) {
				unsigned = false
			}
		default:
			i.Mul(x.i, y.i)
		}
		if !fitsInt(i, unsigned) {
			return failWith(intKind(unsigned))
		}
		return intVal(i, unsigned), true, nil
	case "Item_func_div":
		if real {
			if y.asReal() == 0 {
				return cval{kind: cNull}, true, nil
			}
			r := x.asReal() / y.asReal()
			if math.IsInf(r, 0) {
				return failWith("DOUBLE")
			}
			return cval{kind: cReal, r: r, fn: true}, true, nil
		}
		if y.asDec().Sign() == 0 {
			return cval{kind: cNull}, true, nil
		}
		return cval{kind: cDec, d: new(big.Rat).Quo(x.asDec(), y.asDec()), fn: true}, true, nil
	case "Item_func_div_int":
		var q *big.Int
		if !real && !dec {
			if y.i.Sign() == 0 {
				return cval{kind: cNull}, true, nil
			}
			q = new(big.Int).Quo(x.i, y.i)
			if !unsigned && q.Cmp(int64Min) == 0 {
				return failWith("BIGINT") // Item_func_int_div::val_int divides magnitudes: 2^63 does not fit
			}
		} else {
			// Item_func_int_div::val_int over non-integers: a decimal division, truncated
			d := y.asDec()
			if d.Sign() == 0 {
				return cval{kind: cNull}, true, nil
			}
			r := new(big.Rat).Quo(x.asDec(), d)
			q = new(big.Int).Quo(r.Num(), r.Denom())
		}
		if !fitsInt(q, unsigned) {
			return failWith(intKind(unsigned))
		}
		return intVal(q, unsigned), true, nil
	case "Item_func_mod":
		if real || dec {
			return cval{}, false, nil
		}
		if y.i.Sign() == 0 {
			return cval{kind: cNull}, true, nil
		}
		return intVal(new(big.Int).Rem(x.i, y.i), x.kind == cUint), true, nil
	}
	return cval{}, false, nil
}

// foldCast is CAST(x AS SIGNED / UNSIGNED / FLOAT / DOUBLE).
func (a *analyzer) foldCast(n *mysqlast.Node) (cval, bool, *foldFail) {
	target := castTarget(a, n)
	switch target {
	case "SIGNED_INT", "UNSIGNED_INT", "FLOAT", "DOUBLE":
	default:
		return cval{}, false, nil
	}
	x, ok, fail := a.fold(n.Arg("arg"))
	if !ok || fail != nil {
		return cval{}, false, fail
	}
	if x.isNull() {
		return x, true, nil
	}
	switch target {
	case "FLOAT":
		r := x.asReal()
		if math.Abs(r) > math.MaxFloat32 {
			return cval{}, false, &foldFail{kind: "DOUBLE", print: a.printItem(n), at: n.Start}
		}
		return cval{kind: cReal, r: float64(float32(r)), fn: true}, true, nil
	case "DOUBLE":
		return cval{kind: cReal, r: x.asReal(), fn: true}, true, nil
	}
	unsigned := target == "UNSIGNED_INT"
	var i *big.Int
	switch x.kind {
	case cInt, cUint:
		i = new(big.Int).Set(x.i)
	case cReal:
		if x.fn {
			// llrint_with_overflow_check: a function's DOUBLE must fit; the message names it
			if x.r < -two63f || x.r >= two63f { // the longlong range, for UNSIGNED too (measured)
				return cval{}, false, &foldFail{kind: "BIGINT", print: a.printItem(n.Arg("arg")), at: nodeStart(n.Arg("arg"))}
			}
		}
		i = roundHalfAway(x.asDec()).Num()
	default:
		i = roundHalfAway(x.asDec()).Num()
	}
	// a value the type cannot hold is clamped (a literal's val_int, my_decimal2int) or
	// wraps (a signed integer read as unsigned)
	switch {
	case unsigned && i.Sign() < 0 && (x.kind == cInt || x.kind == cUint):
		i.Add(i, new(big.Int).Add(uint64Max, big.NewInt(1)))
	case unsigned && i.Sign() < 0:
		i.SetInt64(0)
	case unsigned && i.Cmp(uint64Max) > 0:
		i.Set(uint64Max)
	case !unsigned && i.Cmp(int64Max) > 0:
		if x.kind == cUint {
			i.Sub(i, new(big.Int).Add(uint64Max, big.NewInt(1)))
		} else {
			i.Set(int64Max)
		}
	case !unsigned && i.Cmp(int64Min) < 0:
		i.Set(int64Min)
	}
	return intVal(i, unsigned), true, nil
}

// castTarget is the cast's type word ("SIGNED_INT", "FLOAT", ...), as cast() reads it.
func castTarget(a *analyzer, n *mysqlast.Node) string {
	target := ""
	switch x := n.Arg("type").(type) {
	case *mysqlast.Struct:
		target = str(x.Fields["target"])
		if strings.Contains(target, "?ITEM_CAST_DOUBLE:ITEM_CAST_FLOAT") {
			target = "ITEM_CAST_FLOAT"
			if up := strings.ToUpper(a.text[n.Start:n.End]); strings.Contains(up, "DOUBLE") || strings.Contains(up, "REAL") {
				target = "ITEM_CAST_DOUBLE"
			}
		}
	default:
		target = str(x)
	}
	return strings.TrimPrefix(target, "ITEM_CAST_")
}

// foldCall is a native function call this file models.
func (a *analyzer) foldCall(n *mysqlast.Node) (cval, bool, *foldFail) {
	name, args := funcCallParts(n)
	name = strings.ToUpper(name)
	vals := make([]cval, len(args))
	for i, arg := range args {
		v, ok, fail := a.fold(arg)
		if fail != nil {
			return cval{}, false, fail
		}
		if !ok {
			return cval{}, false, nil
		}
		vals[i] = v
	}
	realFail := func() (cval, bool, *foldFail) {
		return cval{}, false, &foldFail{kind: "DOUBLE", print: a.printItem(n), at: n.Start}
	}
	switch name {
	case "ABS":
		if len(vals) != 1 {
			return cval{}, false, nil
		}
		x := vals[0]
		switch x.kind {
		case cNull:
			return x, true, nil
		case cInt:
			if x.i.Cmp(int64Min) == 0 { // Item_func_abs::int_op
				return cval{}, false, &foldFail{kind: "BIGINT", print: a.printItem(n), at: n.Start}
			}
			return intVal(new(big.Int).Abs(x.i), false), true, nil
		case cUint:
			return intVal(x.i, true), true, nil
		case cDec:
			return cval{kind: cDec, d: new(big.Rat).Abs(x.d), fn: true}, true, nil
		default:
			return cval{kind: cReal, r: math.Abs(x.asReal()), fn: true}, true, nil
		}
	case "EXP", "COT", "DEGREES":
		if len(vals) != 1 {
			return cval{}, false, nil
		}
		x := vals[0]
		if x.isNull() {
			return x, true, nil
		}
		var r float64
		switch name {
		case "EXP":
			r = math.Exp(x.asReal())
		case "COT":
			t := math.Tan(x.asReal())
			if t == 0 { // Item_func_cot::val_real
				return realFail()
			}
			r = 1 / t
		default:
			r = x.asReal() * (180 / math.Pi) // Item_func_units: value * mul
		}
		if math.IsInf(r, 0) {
			return realFail()
		}
		return cval{kind: cReal, r: r, fn: true}, true, nil
	case "POW", "POWER":
		if len(vals) != 2 {
			return cval{}, false, nil
		}
		if vals[0].isNull() || vals[1].isNull() {
			return cval{kind: cNull}, true, nil
		}
		r := math.Pow(vals[0].asReal(), vals[1].asReal())
		if math.IsInf(r, 0) || math.IsNaN(r) {
			return realFail()
		}
		return cval{kind: cReal, r: r, fn: true}, true, nil
	case "ROUND":
		if len(vals) == 0 || len(vals) > 2 {
			return cval{}, false, nil
		}
		x := vals[0]
		if x.isNull() {
			return x, true, nil
		}
		if x.kind != cInt && x.kind != cUint {
			return cval{}, false, nil // a decimal or a double rounds without an integer's range
		}
		places := int64(0)
		if len(vals) == 2 {
			if vals[1].isNull() {
				return cval{kind: cNull}, true, nil
			}
			places = roundHalfAway(vals[1].asDec()).Num().Int64()
		}
		if places >= 0 {
			return intVal(x.i, x.kind == cUint), true, nil
		}
		if places < -40 {
			return intVal(big.NewInt(0), x.kind == cUint), true, nil
		}
		scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(-places), nil)
		q := roundHalfAway(new(big.Rat).SetFrac(x.i, scale)).Num()
		q.Mul(q, scale)
		if !fitsInt(q, x.kind == cUint) {
			return cval{}, false, &foldFail{kind: intKind(x.kind == cUint), print: a.printItem(n), at: n.Start}
		}
		return intVal(q, x.kind == cUint), true, nil
	case "RANDOM_BYTES":
		if len(vals) != 1 {
			return cval{}, false, nil
		}
		x := vals[0]
		if x.isNull() {
			return x, true, nil
		}
		length := roundHalfAway(x.asDec()).Num()
		if length.Sign() < 1 || length.Cmp(big.NewInt(1024)) > 0 {
			return cval{}, false, &foldFail{raw: "length value is out of range in 'random_bytes'", at: n.Start}
		}
		return cval{}, false, nil
	}
	return cval{}, false, nil
}

// printItem spells a folded expression the way Item::print does in the server's message.
func (a *analyzer) printItem(v mysqlast.Value) string {
	n, ok := v.(*mysqlast.Node)
	if !ok {
		return ""
	}
	switch n.Class {
	case "Item_int":
		return str(n.Arg("i"))
	case "Item_uint", "Item_decimal", "Item_float":
		return str(n.Arg("str"))
	case "Item_null":
		return "NULL"
	case "Item_func_true":
		return "true"
	case "Item_func_false":
		return "false"
	case "PTI_text_literal_text_string", "PTI_text_literal_nchar_string", "PTI_text_literal_underscore_charset":
		s, _ := stringLiteral(n)
		return "'" + strings.ReplaceAll(s, "'", "\\'") + "'"
	case "PTI_udf_expr":
		return a.printItem(n.Arg("expr"))
	case "Item_func_neg":
		return "-(" + a.printItem(n.Arg("a")) + ")"
	case "Item_func_plus", "Item_func_minus", "Item_func_mul", "Item_func_div", "Item_func_mod":
		return "(" + a.printItem(n.Arg("a")) + " " + operatorName(n.Class) + " " + a.printItem(n.Arg("b")) + ")"
	case "Item_func_div_int":
		return "(" + a.printItem(n.Arg("a")) + " DIV " + a.printItem(n.Arg("b")) + ")"
	case "create_func_cast":
		word := strings.ToLower(strings.TrimSuffix(castTarget(a, n), "_INT"))
		return "cast(" + a.printItem(n.Arg("arg")) + " as " + word + ")"
	case "PTI_function_call_generic_ident_sys":
		name, args := funcCallParts(n)
		name = strings.ToLower(name)
		if name == "power" {
			name = "pow"
		}
		parts := make([]string, len(args))
		for i, arg := range args {
			parts[i] = a.printItem(arg)
		}
		return name + "(" + strings.Join(parts, ",") + ")"
	}
	return a.textOf(n)
}

// constCheck judges a typed node for a constant 1690: the statement's error where the
// server evaluates the constant before reading rows, the violation 1690 where it runs per
// row (a.foldPerRow), nothing while folding is off (a.noFold: a branch a constant decides
// against, an ORDER BY / GROUP BY item) or inside a body.
func (a *analyzer) constCheck(sc scope, n *mysqlast.Node, where string) *Error {
	if a.noFold > 0 || a.routine != nil || a.trig != nil || !foldable(n) {
		return nil
	}
	_, _, fail := a.fold(n)
	if fail == nil {
		return nil
	}
	perRow := a.foldPerRow
	if condContext(where) {
		perRow = !a.isConstant(n) || !a.optimizeTime(sc, where)
	}
	if perRow {
		table := ""
		if a.write != nil && a.write.table != nil {
			table = a.write.table.Name
		} else {
			for _, rel := range sc.rels {
				if rel.table != nil {
					table = rel.table.Name
					break
				}
			}
		}
		a.storeRaised = append(a.storeRaised, Violation{Code: code1690, Constraint: itoa(code1690), Table: table, SQLState: constraintSQLState(code1690)})
		return nil
	}
	return &Error{Message: fail.message(), Code: code1690, Position: a.ph.Back(fail.at)}
}

// constBool is v as a constant condition: ok when v folds, decided when it is not NULL.
func (a *analyzer) constBool(v mysqlast.Value) (b, null, ok bool) {
	c, ok, fail := a.fold(v)
	if !ok || fail != nil {
		return false, false, false
	}
	b, null = c.truth()
	return b, null, true
}

// constEqual reports whether two constants compare equal (a CASE's operand against a WHEN):
// numerics as numbers, strings as text; false and !ok when they are not comparable here.
func (a *analyzer) constEqual(x, y mysqlast.Value) (eq, ok bool) {
	cx, okx, fx := a.fold(x)
	cy, oky, fy := a.fold(y)
	if !okx || !oky || fx != nil || fy != nil || cx.isNull() || cy.isNull() {
		return false, false
	}
	if cx.kind == cStr || cy.kind == cStr {
		if cx.kind == cStr && cy.kind == cStr {
			return strings.EqualFold(cx.s, cy.s), true
		}
		return false, false
	}
	if cx.kind == cReal || cy.kind == cReal {
		return cx.asReal() == cy.asReal(), true
	}
	return cx.asDec().Cmp(cy.asDec()) == 0, true
}

// isCondNode: an AND / OR, which a condition's optimizer simplifies on its own even when
// an enclosing AND / OR is already decided.
func isCondNode(v mysqlast.Value) bool {
	n, ok := v.(*mysqlast.Node)
	return ok && (n.Class == "Item_cond_and" || n.Class == "Item_cond_or")
}

// condContext says the clause is one the optimizer folds before reading rows.
func condContext(where string) bool {
	return where == "where clause" || where == "having clause" || where == "on clause"
}

// optimizeTime says the constant at the top of a.parents is one the optimizer evaluates
// before any row is read: the largest constant it is part of is a condition's own term,
// reached from the condition's root through AND / OR / NOT and the predicates the
// optimizer evaluates constant operands of -- IN and LIKE always (Item_func_in's constant
// array, the pattern's analysis), a comparison or BETWEEN when the other operand is a
// column some key of its table covers (the range optimizer builds the range; measured:
// `WHERE id = 9223372036854775807 + 1` fails over no rows, `WHERE name = 1e308 * 10` over
// an unindexed name runs). A constant nested under anything else (an IF's branch, a
// function's argument beside a column) runs per row. In a HAVING, a predicate over an
// aggregate runs per group and is not such a path (measured: `HAVING MAX(id) >
// <constant>` over no rows never evaluates the constant).
func (a *analyzer) optimizeTime(sc scope, where string) bool {
	base := 0
	if len(a.condBases) > 0 {
		base = a.condBases[len(a.condBases)-1]
	}
	for i := len(a.parents) - 2; i >= base; i-- {
		p := a.parents[i]
		if a.isConstant(p) {
			continue // still inside the constant
		}
		switch p.Class {
		case "Item_cond_and", "Item_cond_or", "Item_func_xor", "Item_func_not", "PTI_truth_transform", "PTI_udf_expr":
			continue
		case "Item_func_in", "Item_func_like", "Item_func_isnull", "Item_func_isnotnull":
			if where == "having clause" && containsAggregate(p) {
				return false
			}
			continue
		case "PTI_comp_op", "PTI_handle_sql2003_note184_exception", "Item_func_between":
			if where == "having clause" && containsAggregate(p) {
				return false
			}
			if !a.keyedOperand(sc, p) {
				return false
			}
			continue
		}
		return false
	}
	return true
}

// isConstant says v is an expression over constants alone -- a const_item() to the server,
// evaluated once wherever the optimizer meets it -- whatever its value (it may be the very
// overflow being placed) and whether fold models it (a CONCAT of constants is one too).
func (a *analyzer) isConstant(v mysqlast.Value) bool {
	switch x := v.(type) {
	case mysqlast.List:
		for _, e := range x {
			if !a.isConstant(e) {
				return false
			}
		}
		return true
	case *mysqlast.Node:
		switch x.Class {
		case "Item_int", "Item_uint", "Item_decimal", "Item_float", "Item_null", "Item_func_true", "Item_func_false",
			"PTI_text_literal_text_string", "PTI_text_literal_nchar_string", "PTI_text_literal_underscore_charset":
			return true
		case "PTI_udf_expr":
			return a.isConstant(x.Arg("expr"))
		case "Item_func_neg", "Item_func_not":
			return a.isConstant(x.Arg("a"))
		case "Item_func_plus", "Item_func_minus", "Item_func_mul", "Item_func_div", "Item_func_div_int", "Item_func_mod":
			return a.isConstant(x.Arg("a")) && a.isConstant(x.Arg("b"))
		case "PTI_comp_op":
			return a.isConstant(x.Arg("left")) && a.isConstant(x.Arg("right"))
		case "create_func_cast":
			return a.isConstant(x.Arg("arg"))
		case "PTI_function_call_generic_ident_sys":
			name, args := funcCallParts(x)
			if strings.EqualFold(name, "RANDOM_BYTES") || strings.EqualFold(name, "RAND") || strings.EqualFold(name, "UUID") {
				return false // not a const_item(): evaluated per row, never by the optimizer
			}
			return a.isConstant(mysqlast.List(args))
		case "Item_func_if", "Item_func_case", "Item_func_coalesce", "Item_func_in", "PTI_handle_sql2003_note184_exception",
			"Item_func_between", "Item_func_like", "Item_cond_and", "Item_cond_or", "Item_func_xor", "Item_func_isnull", "Item_func_isnotnull", "Item_row":
			for _, arg := range x.Args {
				if arg != nil && !a.isConstant(arg) {
					return false
				}
			}
			return true
		}
	}
	return false
}

// keyedOperand reports a predicate whose non-constant operand is a column some key of its
// table covers: the range optimizer then evaluates the predicate's constants.
func (a *analyzer) keyedOperand(sc scope, p *mysqlast.Node) bool {
	for _, arg := range p.Args {
		if a.isConstant(arg) {
			continue
		}
		if list, ok := arg.(mysqlast.List); ok { // IN's list, BETWEEN's bounds
			for _, e := range list {
				if !a.isConstant(e) && a.keyedColumn(sc, e) {
					return true
				}
			}
			continue
		}
		if a.keyedColumn(sc, arg) {
			return true
		}
	}
	return false
}

func (a *analyzer) keyedColumn(sc scope, v mysqlast.Value) bool {
	n, ok := v.(*mysqlast.Node)
	if !ok || !isColumnRef(n) {
		return false
	}
	ref, ok := a.plainColumn(sc, n)
	if !ok || ref.col == nil || ref.c.baseTable == nil {
		return false
	}
	for _, k := range ref.c.baseTable.Keys {
		for _, part := range k.Parts {
			if strings.EqualFold(part.Column, ref.col.Name) {
				return true
			}
		}
	}
	return false
}
