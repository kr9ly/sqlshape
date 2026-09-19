package analyze

// The run-time failures a function decides by a constant argument's value, judged the way
// fold.go judges constant arithmetic (measured in TestFuncValServer):
//
//   - INET_ATON / INET6_ATON / UNHEX / STR_TO_DATE only warn when the value does not
//     parse, so the failure is an error only in a strict write outside IGNORE
//     (strictWriteStmt): 1411 ER_WRONG_VALUE_FOR_TYPE, except a parsed date with a
//     non-space tail, which is the 1292 make_truncated_value_warning raises;
//   - UUID_TO_BIN / BIN_TO_UUID raise 1411 with my_error on every evaluation, whatever
//     the statement or the sql_mode (measured: SELECT UUID_TO_BIN('zz') fails);
//   - PERIOD_ADD / PERIOD_DIFF raise 1210 ER_WRONG_ARGUMENTS the same way for a constant
//     that is no period (Item_func_period_add::val_int).
//
// Where each failure lands follows fold.go's placement: a term the optimizer folds before
// reading rows is the statement's error, one that runs per row is the violation keyed by
// its own number ("1411" / "1210"), and a query over no rows does not fail (measured:
// SELECT UUID_TO_BIN('zz') FROM t WHERE FALSE runs, the same select item over a row
// fails). The messages quote the argument as the server does: inet_aton / inet6_aton /
// unhex print the argument item (a string literal keeps its own quotes: ''122.256''),
// uuid_to_bin / bin_to_uuid / str_to_date print the value itself.

import (
	"fmt"
	"math/big"
	"strings"

	"github.com/kr9ly/sqlshape/check/mysql/v2/internal/mysqlast"
	"github.com/kr9ly/sqlshape/check/mysql/v2/internal/schema"
)

// foldFuncVal is foldCall's arm for the functions above; handled is false when name is
// none of them. A valid UNHEX carries its decoded bytes as the call's value (c, valued),
// so that BIN_TO_UUID(UNHEX(...)) judges the inner result's length.
func (a *analyzer) foldFuncVal(name string, n *mysqlast.Node, args []mysqlast.Value) (handled bool, c cval, valued bool, fail *foldFail) {
	strictGated := false
	switch name {
	case "INET_ATON", "INET6_ATON", "STR_TO_DATE":
		if !a.strictWriteStmt() {
			return true, cval{}, false, nil
		}
	case "UNHEX":
		strictGated = !a.strictWriteStmt() // the value still folds; only its own 1411 is gated
	case "UUID_TO_BIN", "BIN_TO_UUID", "PERIOD_ADD", "PERIOD_DIFF":
	default:
		return false, cval{}, false, nil
	}
	vals := make([]cval, len(args))
	for i, arg := range args {
		v, ok, argFail := a.foldArg(arg)
		if argFail != nil {
			return true, cval{}, false, argFail
		}
		if !ok {
			return true, cval{}, false, nil // an argument this file cannot value: not judged
		}
		vals[i] = v
	}
	switch name {
	case "INET_ATON":
		if len(vals) == 1 && !vals[0].isNull() {
			if s, ok := cvalString(vals[0]); ok && !inetAton(s) {
				return true, cval{}, false, &foldFail{code: 1411, raw: fmt.Sprintf("Incorrect string value: '%s' for function inet_aton", a.printItem(args[0])), at: n.Start}
			}
		}
	case "INET6_ATON":
		if len(vals) == 1 && !vals[0].isNull() {
			if s, ok := cvalString(vals[0]); ok && !strToIPv4(s) && !strToIPv6(s) {
				return true, cval{}, false, &foldFail{code: 1411, raw: fmt.Sprintf("Incorrect string value: '%s' for function inet6_aton", a.printItem(args[0])), at: n.Start}
			}
		}
	case "UNHEX":
		if len(vals) == 1 && !vals[0].isNull() {
			if s, ok := cvalString(vals[0]); ok {
				if !allHexDigits(s) {
					if strictGated {
						return true, cval{}, false, nil
					}
					return true, cval{}, false, &foldFail{code: 1411, raw: fmt.Sprintf("Incorrect string value: '%s' for function unhex", a.printItem(args[0])), at: n.Start}
				}
				if b, ok := hexLiteralBytes(s); ok {
					return true, cval{kind: cStr, s: string(b), fn: true}, true, nil
				}
			}
		}
	case "STR_TO_DATE":
		if len(vals) == 2 && !vals[0].isNull() && !vals[1].isNull() {
			s, ok1 := cvalString(vals[0])
			f, ok2 := cvalString(vals[1])
			if ok1 && ok2 {
				if fail := a.strToDateFail(s, f, n.Start); fail != nil {
					return true, cval{}, false, fail
				}
			}
		}
	case "UUID_TO_BIN":
		if len(vals) >= 1 && !vals[0].isNull() {
			if s, ok := cvalString(vals[0]); ok && !validUUIDText(s) {
				return true, cval{}, false, &foldFail{code: 1411, raw: fmt.Sprintf("Incorrect string value: '%s' for function uuid_to_bin", s), at: n.Start}
			}
		}
	case "BIN_TO_UUID":
		if len(vals) >= 1 && !vals[0].isNull() {
			if s, ok := cvalString(vals[0]); ok && len(s) != 16 {
				return true, cval{}, false, &foldFail{code: 1411, raw: fmt.Sprintf("Incorrect string value: '%s' for function bin_to_uuid", escapeBinary(s)), at: n.Start}
			}
		}
	case "PERIOD_ADD", "PERIOD_DIFF":
		fname := "period_add"
		if name == "PERIOD_DIFF" {
			fname = "period_diff"
		}
		// the first argument of PERIOD_ADD, both of PERIOD_DIFF, must be periods
		check := vals
		if name == "PERIOD_ADD" && len(vals) == 2 {
			check = vals[:1]
		}
		for _, v := range check {
			if v.isNull() {
				continue
			}
			if p, ok := cvalInt(v); ok && !validPeriod(p) {
				return true, cval{}, false, &foldFail{code: 1210, raw: "Incorrect arguments to " + fname, at: n.Start}
			}
		}
	}
	return true, cval{}, false, nil
}

// foldArg folds one argument, additionally reading the hex and binary literals fold.go
// leaves alone (BIN_TO_UUID(0x11)'s bytes are its string value).
func (a *analyzer) foldArg(v mysqlast.Value) (cval, bool, *foldFail) {
	if n, ok := v.(*mysqlast.Node); ok {
		if n.Class == "PTI_udf_expr" {
			return a.foldArg(n.Arg("expr"))
		}
		if n.Class == "Item_hex_string" {
			if b, ok := hexLiteralBytes(str(n.Arg("literal"))); ok {
				return cval{kind: cStr, s: string(b)}, true, nil
			}
			return cval{}, false, nil
		}
	}
	return a.fold(v)
}

// hexLiteralBytes decodes a 0xABCD / x'ABCD' literal's text into its bytes.
func hexLiteralBytes(s string) ([]byte, bool) {
	s = strings.TrimPrefix(strings.TrimPrefix(s, "0x"), "0X")
	if strings.HasPrefix(strings.ToLower(s), "x'") {
		s = strings.TrimSuffix(s[2:], "'")
	}
	if len(s)%2 == 1 {
		s = "0" + s
	}
	out := make([]byte, 0, len(s)/2)
	for i := 0; i+1 < len(s); i += 2 {
		hi, ok1 := hexDigit(s[i])
		lo, ok2 := hexDigit(s[i+1])
		if !ok1 || !ok2 {
			return nil, false
		}
		out = append(out, hi<<4|lo)
	}
	return out, true
}

func hexDigit(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}

// cvalString is the value in a string context; ok is false for a kind whose exact string
// form this file does not spell (a DOUBLE, a DECIMAL).
func cvalString(c cval) (string, bool) {
	switch c.kind {
	case cStr:
		return c.s, true
	case cInt, cUint:
		return c.i.String(), true
	}
	return "", false
}

// cvalInt is the value in an integer context, rounding a decimal half away from zero as
// val_int does; ok is false for a string holding no integer prefix or a DOUBLE.
func cvalInt(c cval) (int64, bool) {
	switch c.kind {
	case cInt, cUint:
		if c.i.IsInt64() {
			return c.i.Int64(), true
		}
	case cDec:
		half := big.NewRat(1, 2)
		d := new(big.Rat).Set(c.d)
		if d.Sign() >= 0 {
			d.Add(d, half)
		} else {
			d.Sub(d, half)
		}
		i := new(big.Int).Quo(d.Num(), d.Denom())
		if i.IsInt64() {
			return i.Int64(), true
		}
	case cStr:
		// my_strtoll10 through val_int: the numeric prefix; not needed precisely here,
		// digits alone are enough for the periods the corpus writes
		s := strings.TrimSpace(c.s)
		neg := strings.HasPrefix(s, "-")
		s = strings.TrimPrefix(s, "-")
		end := 0
		for end < len(s) && s[end] >= '0' && s[end] <= '9' {
			end++
		}
		if end == 0 {
			return 0, true // no digits: 0, as my_strtoll10 reads it
		}
		i, ok := new(big.Int).SetString(s[:end], 10)
		if !ok || !i.IsInt64() {
			return 0, false
		}
		if neg {
			return -i.Int64(), true
		}
		return i.Int64(), true
	}
	return 0, false
}

// inetAton is Item_func_inet_aton::val_int's parse: digit groups of at most 255 separated
// by dots, at most four groups, not ending on a dot, nothing else.
func inetAton(s string) bool {
	if s == "" {
		return false
	}
	byteVal, dots := 0, 0
	last := byte('.')
	for i := 0; i < len(s); i++ {
		last = s[i]
		switch {
		case last >= '0' && last <= '9':
			byteVal = byteVal*10 + int(last-'0')
			if byteVal > 255 {
				return false
			}
		case last == '.':
			dots++
			byteVal = 0
		default:
			return false
		}
	}
	return last != '.' && dots <= 3
}

// strToIPv4 is item_inetfunc.cc's str_to_ipv4: exactly four groups of one to three
// digits, each at most 255, 7 to 15 characters.
func strToIPv4(s string) bool {
	if len(s) < 7 || len(s) > 15 {
		return false
	}
	byteVal, chars, dots := 0, 0, 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
			chars++
			if chars > 3 {
				return false
			}
			byteVal = byteVal*10 + int(c-'0')
			if byteVal > 255 {
				return false
			}
		case c == '.':
			if chars == 0 {
				return false
			}
			dots++
			if dots > 3 {
				return false
			}
			byteVal, chars = 0, 0
		default:
			return false
		}
	}
	return chars > 0 && dots == 3
}

// strToIPv6 is item_inetfunc.cc's str_to_ipv6: hex groups separated by ':', one '::' gap,
// an optional trailing IPv4 part, filling exactly 16 bytes.
func strToIPv6(s string) bool {
	if len(s) < 2 || len(s) > 8*4+7 {
		return false
	}
	p := 0
	if s[0] == ':' {
		p++
		if p >= len(s) || s[p] != ':' {
			return false
		}
	}
	const size = 16
	dst := 0
	gap := -1
	groupStart := p
	chars, groupVal := 0, 0
	for ; p < len(s); p++ {
		c := s[p]
		switch {
		case c == ':':
			groupStart = p + 1
			if chars == 0 {
				if gap >= 0 {
					return false
				}
				gap = dst
				continue
			}
			if p+1 >= len(s) {
				return false // ending at ':'
			}
			if dst+2 > size {
				return false
			}
			dst += 2
			chars, groupVal = 0, 0
		case c == '.':
			if dst+4 > size {
				return false
			}
			if !strToIPv4(s[groupStart:]) {
				return false
			}
			dst += 4
			chars = 0
			p = len(s) // break
		default:
			d, ok := hexDigit(c)
			if !ok {
				return false
			}
			if chars >= 4 {
				return false
			}
			groupVal = groupVal<<4 | int(d)
			_ = groupVal
			chars++
		}
	}
	if chars > 0 {
		if dst+2 > size {
			return false
		}
		dst += 2
	}
	if gap >= 0 {
		if dst == size {
			return false // no room for the gap
		}
		dst = size
	}
	return dst == size
}

func allHexDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if _, ok := hexDigit(s[i]); !ok {
			return false
		}
	}
	return true
}

// validUUIDText is mysql::gtid::Uuid::parse's formats: 32 hex digits, the dashed
// 8-4-4-4-12 form, or the dashed form in braces; nothing else (a braced 32-digit form
// or a stray space is refused, measured).
func validUUIDText(s string) bool {
	if len(s) == 38 && s[0] == '{' && s[37] == '}' {
		s = s[1:37]
	}
	switch len(s) {
	case 32:
		return allHexDigits(s)
	case 36:
		for i := 0; i < 36; i++ {
			if i == 8 || i == 13 || i == 18 || i == 23 {
				if s[i] != '-' {
					return false
				}
				continue
			}
			if _, ok := hexDigit(s[i]); !ok {
				return false
			}
		}
		return true
	}
	return false
}

// escapeBinary spells a value in an error message as ErrConvString does: printable ASCII
// kept, anything else as \xNN.
func escapeBinary(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 0x20 && c < 0x7f {
			b.WriteByte(c)
			continue
		}
		fmt.Fprintf(&b, "\\x%02X", c)
	}
	return b.String()
}

// validPeriod is Item_func_period_add's test: a positive YYMM / YYYYMM whose month is
// 1 to 12 (period.h's valid_period).
func validPeriod(p int64) bool {
	return p > 0 && p%100 >= 1 && p%100 <= 12
}

// wrongArguments is the resolve-time 1210 family (measured in TestFuncValServer, each
// firing wherever the expression sits, dead branches and empty tables included):
//
//   - NTILE's argument must be a positive literal (the grammar already refuses a negative
//     or non-integer one; a literal 0 is 1210);
//   - NTH_VALUE's position must be a positive integer literal (0, a negative, a decimal
//     or a string is 1210);
//   - LIKE's ESCAPE must be a constant of at most one character (a column is 1210, a
//     constant's string form of two characters too);
//   - MATCH's columns must be columns of one relation (a select alias, or columns of two
//     tables, is 1210 "Incorrect arguments to MATCH"; a derived table's columns pass,
//     measured), and AGAINST's argument must hold no column (1210 "... to AGAINST").
func (a *analyzer) wrongArguments(sc scope, n *mysqlast.Node, where string) *Error {
	wrong := func(name string) *Error {
		return &Error{Message: "Incorrect arguments to " + name, Code: 1210, Position: a.ph.Back(n.Start)}
	}
	switch n.Class {
	case "Item_ntile":
		if arg, ok := n.Arg("a").(*mysqlast.Node); ok && arg.Class == "Item_int" && str(arg.Arg("i")) == "0" {
			return wrong("ntile")
		}
	case "Item_nth_value":
		pos, ok := n.Arg("n").(*mysqlast.Node)
		if !ok {
			return nil
		}
		switch pos.Class {
		case "Item_int":
			if str(pos.Arg("i")) == "0" {
				return wrong("nth_value")
			}
		case "Item_func_neg", "Item_decimal", "Item_float",
			"PTI_text_literal_text_string", "PTI_text_literal_nchar_string", "PTI_text_literal_underscore_charset":
			return wrong("nth_value")
		}
	case "Item_func_like":
		esc, ok := n.Arg("escape").(*mysqlast.Node)
		if !ok {
			return nil
		}
		switch esc.Class {
		case "Item_null", "Item_param":
			return nil
		case "PTI_text_literal_underscore_charset", "PTI_literal_underscore_charset_hex_num", "PTI_literal_underscore_charset_bin_num":
			return nil // a introducer'd literal: its length is the charset's business (and 1270 is not this rule)
		}
		// the server wants const_for_execution(): a column is refused, while a scalar
		// subquery or an expression over constants is evaluated (its own failures --
		// more than one row, 1242 -- are the execution's); this file only judges what
		// it can be sure of: a column outside any subquery is 1210, a constant this
		// file folds is measured
		if a.nonConstOutsideSubquery(sc, esc) {
			return wrong("ESCAPE")
		}
		if v, ok, fail := a.fold(esc); ok && fail == nil && !v.isNull() {
			if s, ok := cvalString(v); ok && len([]rune(s)) > 1 {
				return wrong("ESCAPE")
			}
		}
	case "Item_func_match":
		if a.nonConstOutsideSubquery(sc, n.Arg("against")) {
			return wrong("AGAINST")
		}
		args, _ := n.Arg("a").(mysqlast.List)
		var rel *relation
		var base *schema.Table
		for _, arg := range args {
			an, ok := arg.(*mysqlast.Node)
			if !ok || !strings.HasPrefix(an.Class, "PTI_simple_ident") {
				return wrong("MATCH")
			}
			ref, err := a.column(sc, an, where)
			if err != nil {
				continue // its own resolution error is reported by the argument walk
			}
			if ref.rel == nil && ref.c.base == nil {
				// a select alias that is no table column (HAVING st over GROUP_CONCAT);
				// an alias that names a plain column (HAVING inhalt under SELECT *,
				// ORDER BY match(betreff) over the b.betreff item) resolves as the
				// column it is and passes (measured)
				return wrong("MATCH")
			}
			if (rel != nil || base != nil) && rel != ref.rel && (ref.c.baseTable == nil || base != ref.c.baseTable) {
				return wrong("MATCH") // columns of two relations
			}
			rel, base = ref.rel, ref.c.baseTable
		}
	}
	return nil
}

// nonConstOutsideSubquery reports a reference the statement cannot run as a constant: a
// column or `@@var` read outside any subquery (a scalar subquery is a constant to
// ESCAPE and AGAINST, measured -- its own shape failures are the execution's), unless
// the name resolves to a routine variable or parameter, which is a run-time constant.
func (a *analyzer) nonConstOutsideSubquery(sc scope, v mysqlast.Value) bool {
	switch x := v.(type) {
	case mysqlast.List:
		for _, e := range x {
			if a.nonConstOutsideSubquery(sc, e) {
				return true
			}
		}
	case *mysqlast.Node:
		switch x.Class {
		case "PTI_singlerow_subselect", "PTI_exists_subselect", "Item_in_subselect", "PT_subquery":
			return false
		case "PTI_simple_ident_ident", "PTI_simple_ident_nospvar_ident":
			if _, ok := a.lookupVar(str(x.Arg("ident"))); ok {
				return false // a routine variable or parameter
			}
			return true
		case "PTI_simple_ident_q_2d", "PTI_simple_ident_q_3d":
			return true
		}
		return a.nonConstOutsideSubquery(sc, mysqlast.List(x.Args))
	case *mysqlast.Struct:
		for _, k := range x.Order {
			if a.nonConstOutsideSubquery(sc, x.Fields[k]) {
				return true
			}
		}
	}
	return false
}

// nameConstArgs is NAME_CONST's resolve-time rule (Item_name_const::do_itemize /
// fix_fields, measured): each argument must be a literal, a literal under one unary
// minus, or a COLLATE-wrapped string literal -- never a folded expression (1+1, -(-1)),
// a boolean or temporal literal, or anything with a column; a NULL name is the reserved
// syntax's own 1382.
func (a *analyzer) nameConstArgs(args []mysqlast.Value, at int) *Error {
	if len(args) != 2 {
		return nil
	}
	if n, ok := args[0].(*mysqlast.Node); ok && n.Class == "Item_null" {
		return &Error{Message: "The 'NAME_CONST' syntax is reserved for purposes internal to the MySQL server", Code: 1382, Position: a.ph.Back(at)}
	}
	for _, arg := range args {
		if !nameConstLiteral(arg, true) {
			return &Error{Message: "Incorrect arguments to NAME_CONST", Code: 1210, Position: a.ph.Back(at)}
		}
	}
	return nil
}

func nameConstLiteral(v mysqlast.Value, negOK bool) bool {
	n, ok := v.(*mysqlast.Node)
	if !ok {
		return false
	}
	switch n.Class {
	case "Item_int", "Item_uint", "Item_decimal", "Item_float", "Item_null", "Item_param",
		"PTI_text_literal_text_string", "PTI_text_literal_nchar_string", "PTI_text_literal_underscore_charset",
		"Item_hex_string", "Item_bin_string", "PTI_literal_underscore_charset_hex_num", "PTI_literal_underscore_charset_bin_num":
		return true
	case "Item_func_neg":
		return negOK && nameConstLiteral(n.Arg("a"), false)
	case "Item_func_set_collation":
		inner, ok := n.Arg("a").(*mysqlast.Node)
		return ok && strings.HasPrefix(inner.Class, "PTI_text_literal")
	}
	return false
}
