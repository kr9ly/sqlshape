package analyze

import (
	"github.com/kr9ly/sqlshape/internal/catalog"
	"github.com/kr9ly/sqlshape/internal/schema"
)

// coercionContext is pg's CoercionContext.
type coercionContext byte

const (
	implicitCoercion   coercionContext = 'i'
	assignmentCoercion coercionContext = 'a'
	explicitCoercion   coercionContext = 'e'
)

func (a *analyzer) typ(oid catalog.OID) *catalog.Type { return a.s.Types.ByOID(oid) }

// baseType strips domains (getBaseType).
func (a *analyzer) baseType(oid catalog.OID) catalog.OID {
	return a.s.Types.BaseOf(schema.TypeRef{OID: oid, Typmod: -1}).OID
}

func (a *analyzer) category(oid catalog.OID) (byte, bool) {
	t := a.typ(a.baseType(oid))
	if t == nil {
		return 'X', false
	}
	return t.Category, t.IsPreferred
}

// canCoerce mirrors can_coerce_type for one input (§10 + parse_coerce.c).
func (a *analyzer) canCoerce(from, to catalog.OID, ctx coercionContext) bool {
	if from == to || to == catalog.Any {
		return true
	}
	if from == catalog.Unknown {
		return true // unknown literals coerce to anything
	}
	tt := a.typ(to)
	if tt != nil && tt.IsPolymorphic() {
		// per-argument polymorphic check is done in resolvePolymorphic; here only shape
		switch to {
		case catalog.AnyArray, catalog.AnyCompatibleArray:
			ft := a.typ(a.baseType(from))
			return ft != nil && ft.IsArray()
		case catalog.AnyNonArray, catalog.AnyCompatibleNonArray:
			ft := a.typ(a.baseType(from))
			return ft != nil && !ft.IsArray()
		case catalog.AnyEnum:
			ft := a.typ(a.baseType(from))
			return ft != nil && ft.Kind == 'e'
		case catalog.AnyRange, catalog.AnyCompatibleRange:
			ft := a.typ(a.baseType(from))
			return ft != nil && ft.Kind == 'r'
		case catalog.AnyMultirange, catalog.AnyCompatibleMultirange:
			ft := a.typ(a.baseType(from))
			return ft != nil && ft.Kind == 'm'
		}
		return true
	}
	// domain → its base type is always allowed; base → domain is allowed at any context (CoerceToDomain)
	fb, tb := a.baseType(from), a.baseType(to)
	if fb == tb {
		return true
	}
	if fb != from || tb != to {
		return a.canCoerce(fb, tb, ctx)
	}
	if c := a.s.Catalog.CastBetween(from, to); c != nil {
		switch ctx {
		case explicitCoercion:
			return true
		case assignmentCoercion:
			return c.Context == 'a' || c.Context == 'i'
		default:
			return c.Context == 'i'
		}
	}
	// array → array via element cast
	ft, tt2 := a.typ(from), a.typ(to)
	if ft != nil && tt2 != nil && ft.IsArray() && tt2.IsArray() {
		return a.canCoerce(ft.Elem, tt2.Elem, ctx)
	}
	// composite / record: any row type to record
	if to == catalog.Record && ft != nil && ft.Kind == 'c' {
		return true
	}
	// I/O coercion: to/from string types, explicit only (plus assignment to string)
	if ctx == explicitCoercion {
		fc, _ := a.category(from)
		tc, _ := a.category(to)
		if fc == 'S' || tc == 'S' {
			return true
		}
	}
	if ctx == assignmentCoercion {
		if tc, _ := a.category(to); tc == 'S' {
			return true
		}
	}
	return false
}

// candidate is a function or operator being considered.
type candidate struct {
	fn           *catalog.Func
	op           *catalog.Operator
	ufn          *schema.Function
	args         []catalog.OID // declared input types, expanded for variadic / defaults to len(actual)
	nargs        int           // declared arg count before expansion
	variadicElem catalog.OID
}

func (c candidate) result() catalog.OID {
	switch {
	case c.op != nil:
		return c.op.Result
	case c.fn != nil:
		return c.fn.RetType
	default:
		return c.ufn.RetType.OID
	}
}

// canCoerceAll is can_coerce_type over a whole argument list: per-argument coercibility
// plus polymorphic consistency across arguments (check_generic_type_consistency).
func (a *analyzer) canCoerceAll(actual, declared []catalog.OID, ctx coercionContext) bool {
	poly := false
	for i, t := range actual {
		if !a.canCoerce(t, declared[i], ctx) {
			return false
		}
		if dt := a.typ(declared[i]); dt != nil && dt.IsPolymorphic() {
			poly = true
		}
	}
	if poly {
		if _, ok := a.resolvePolymorphic(declared, actual, catalog.Void); !ok {
			return false
		}
	}
	return true
}

// selectCandidate implements the shared part of §10.2 / §10.3 (func_select_candidate)
// over candidates whose arity already matches. Returns the unique survivor (or nil)
// and how many candidates were still in play when it gave up.
func (a *analyzer) selectCandidate(actual []catalog.OID, cands []candidate) (*candidate, int) {
	// exact match
	var exact []candidate
	for _, c := range cands {
		ok := true
		for i, t := range actual {
			if t != c.args[i] {
				ok = false
				break
			}
		}
		if ok {
			exact = append(exact, c)
		}
	}
	if len(exact) == 1 {
		return &exact[0], 1
	}
	if len(exact) > 1 {
		return nil, len(exact)
	}
	// discard candidates that need a non-implicit coercion
	var ok []candidate
	for _, c := range cands {
		if a.canCoerceAll(actual, c.args, implicitCoercion) {
			ok = append(ok, c)
		}
	}
	cands = ok
	if len(cands) == 0 {
		return nil, 0
	}
	if len(cands) == 1 {
		return &cands[0], 1
	}
	// keep candidates with the most exact matches on input types (domains treated as base)
	best, bestN := []candidate{}, -1
	for _, c := range cands {
		n := 0
		for i, t := range actual {
			if t != catalog.Unknown && a.baseType(t) == c.args[i] {
				n++
			}
		}
		if n > bestN {
			best, bestN = []candidate{c}, n
		} else if n == bestN {
			best = append(best, c)
		}
	}
	cands = best
	if len(cands) == 1 {
		return &cands[0], 1
	}
	// keep candidates that accept preferred types (of the input's category) at the most positions
	best, bestN = []candidate{}, -1
	for _, c := range cands {
		n := 0
		for i, t := range actual {
			if t == catalog.Unknown || a.baseType(t) == c.args[i] {
				continue
			}
			ic, _ := a.category(t)
			cc, pref := a.category(c.args[i])
			if pref && cc == ic {
				n++
			}
		}
		if n > bestN {
			best, bestN = []candidate{c}, n
		} else if n == bestN {
			best = append(best, c)
		}
	}
	cands = best
	if len(cands) == 1 {
		return &cands[0], 1
	}
	// unknown inputs: resolve their category from the candidates
	hasUnknown := false
	for _, t := range actual {
		if t == catalog.Unknown {
			hasUnknown = true
		}
	}
	if hasUnknown {
		resolvable := true
		cats := make([]byte, len(actual))
		prefer := make([]bool, len(actual))
		for i, t := range actual {
			if t != catalog.Unknown {
				continue
			}
			var chosen byte
			sawString := false
			for _, c := range cands {
				cc, _ := a.category(c.args[i])
				if cc == 'S' {
					sawString = true
				}
				if chosen == 0 {
					chosen = cc
				} else if chosen != cc {
					chosen = 0xff // conflict marker
				}
			}
			if chosen == 0xff {
				if sawString {
					chosen = 'S'
				} else {
					resolvable = false
					break
				}
			}
			cats[i] = chosen
			// prefer a preferred type of that category if any candidate offers one
			for _, c := range cands {
				cc, pref := a.category(c.args[i])
				if cc == chosen && pref {
					prefer[i] = true
				}
			}
		}
		if resolvable {
			var keep []candidate
			for _, c := range cands {
				good := true
				for i, t := range actual {
					if t != catalog.Unknown {
						continue
					}
					cc, pref := a.category(c.args[i])
					if cc != cats[i] || (prefer[i] && !pref) {
						good = false
						break
					}
				}
				if good {
					keep = append(keep, c)
				}
			}
			if len(keep) > 0 {
				cands = keep
			}
			if len(cands) == 1 {
				return &cands[0], 1
			}
		}
		// if all known inputs share one type, assume unknowns are that type too
		var known catalog.OID
		same := true
		for _, t := range actual {
			if t == catalog.Unknown {
				continue
			}
			if known == 0 {
				known = t
			} else if known != t {
				same = false
			}
		}
		if same && known != 0 {
			assumed := make([]catalog.OID, len(actual))
			for i, t := range actual {
				if t == catalog.Unknown {
					assumed[i] = known
				} else {
					assumed[i] = t
				}
			}
			var keep []candidate
			for _, c := range cands {
				if a.canCoerceAll(assumed, c.args, implicitCoercion) {
					keep = append(keep, c)
				}
			}
			if len(keep) == 1 {
				return &keep[0], 1
			}
		}
	}
	return nil, len(cands)
}

// resolveOperator implements §10.2 for a binary (left != 0) or prefix operator.
func (a *analyzer) resolveOperator(name string, left, right catalog.OID) *candidate {
	ops := a.s.Catalog.OperatorsByName(name)
	binary := left != 0
	// 2a: unknown on one side of a binary operator is assumed to be the other side's type for the exact-match test
	if binary {
		l, r := a.baseType(left), a.baseType(right)
		if l == catalog.Unknown && r != catalog.Unknown {
			l = r
		} else if r == catalog.Unknown && l != catalog.Unknown {
			r = l
		}
		for _, op := range ops {
			if op.Kind == 'b' && op.Left == l && op.Right == r {
				c := candidate{op: op, args: []catalog.OID{op.Left, op.Right}}
				return &c
			}
		}
	}
	var cands []candidate
	for _, op := range ops {
		if binary && op.Kind == 'b' {
			cands = append(cands, candidate{op: op, args: []catalog.OID{op.Left, op.Right}})
		} else if !binary && op.Kind == 'l' {
			cands = append(cands, candidate{op: op, args: []catalog.OID{op.Right}})
		}
	}
	actual := []catalog.OID{right}
	if binary {
		actual = []catalog.OID{left, right}
	}
	c, _ := a.selectCandidate(actual, cands)
	return c
}

// resolveFunction implements §10.3 over catalog + user functions named name.
func (a *analyzer) resolveFunction(schemaName, name string, actual []catalog.OID) (*candidate, bool) {
	var cands []candidate
	ambiguousExact := false
	add := func(c candidate) {
		cands = append(cands, c)
	}
	if schemaName == "" || schemaName == "pg_catalog" {
		for _, fn := range a.s.Catalog.FuncsByName(name) {
			if fn.Kind == 'p' {
				continue
			}
			if c, ok := expandArgs(fn.ArgTypes, int(fn.NArgDefault), fn.Variadic, len(actual)); ok {
				add(candidate{fn: fn, args: c, nargs: len(fn.ArgTypes), variadicElem: fn.Variadic})
			}
		}
	}
	if schemaName != "pg_catalog" {
		for _, fn := range a.s.Functions {
			if fn.Name != name || (schemaName != "" && fn.Schema != schemaName) || (schemaName == "" && fn.Schema != "public") {
				continue
			}
			var in []catalog.OID
			var variadic catalog.OID
			ndef := 0
			for _, arg := range fn.Args {
				switch arg.Mode {
				case 'o':
					if fn.IsProc {
						in = append(in, arg.Type.OID) // CALL passes OUT arguments too
					}
				case 'i', 'b':
					in = append(in, arg.Type.OID)
					if arg.HasDefault {
						ndef++
					}
				case 'v':
					in = append(in, arg.Type.OID)
					if t := a.typ(arg.Type.OID); t != nil {
						variadic = t.Elem
					}
				}
			}
			if c, ok := expandArgs(in, ndef, variadic, len(actual)); ok {
				add(candidate{ufn: fn, args: c, nargs: len(in), variadicElem: variadic})
			}
		}
	}
	if len(cands) == 0 {
		return nil, false
	}
	c, remaining := a.selectCandidate(actual, cands)
	if c == nil && remaining > 1 {
		ambiguousExact = true
	}
	return c, ambiguousExact
}

// expandArgs adapts a declared parameter list to n actual args using defaults and variadic.
func expandArgs(declared []catalog.OID, ndefault int, variadic catalog.OID, n int) ([]catalog.OID, bool) {
	if variadic != 0 {
		// last declared arg is the variadic array; actual args from there on are elements
		fixed := len(declared) - 1
		if n < fixed {
			return nil, false
		}
		out := make([]catalog.OID, n)
		copy(out, declared[:fixed])
		for i := fixed; i < n; i++ {
			if variadic == catalog.Any {
				out[i] = catalog.Any
			} else {
				out[i] = variadic
			}
		}
		return out, true
	}
	if n == len(declared) {
		return declared, true
	}
	if n < len(declared) && n >= len(declared)-ndefault {
		return declared[:n], true
	}
	return nil, false
}

// resolvePolymorphic binds any*-typed declared args to actual types (enforce_generic_type_consistency)
// and returns the concrete return type. ok=false on inconsistency.
func (a *analyzer) resolvePolymorphic(declared []catalog.OID, actual []catalog.OID, ret catalog.OID) (catalog.OID, bool) {
	var elem, arr, rng, mrng catalog.OID
	var compatElems []catalog.OID
	haveCompatArr := catalog.OID(0)
	for i, d := range declared {
		if i >= len(actual) {
			break
		}
		act := actual[i]
		if act == catalog.Unknown {
			continue
		}
		act = a.baseType(act)
		switch d {
		case catalog.AnyElement, catalog.AnyNonArray, catalog.AnyEnum:
			if elem != 0 && elem != act {
				return 0, false
			}
			elem = act
		case catalog.AnyArray:
			if arr != 0 && arr != act {
				return 0, false
			}
			arr = act
		case catalog.AnyRange, catalog.AnyCompatibleRange:
			rng = act
		case catalog.AnyMultirange, catalog.AnyCompatibleMultirange:
			mrng = act
		case catalog.AnyCompatible, catalog.AnyCompatibleNonArray:
			compatElems = append(compatElems, act)
		case catalog.AnyCompatibleArray:
			haveCompatArr = act
			if t := a.typ(act); t != nil && t.IsArray() {
				compatElems = append(compatElems, t.Elem)
			}
		}
	}
	if arr != 0 {
		at := a.typ(arr)
		if at == nil || !at.IsArray() {
			return 0, false
		}
		if elem != 0 && elem != at.Elem {
			return 0, false
		}
		elem = at.Elem
	}
	if elem != 0 && arr == 0 {
		arr = a.s.Types.ArrayOf(elem)
	}
	// ranges: anyrange ↔ anyelement ↔ anymultirange through pg_range
	if mrng != 0 && rng == 0 {
		if r := a.s.Catalog.RangeOfMulti(a.baseType(mrng)); r != nil {
			rng = r.OID
		}
	}
	if rng != 0 {
		if r := a.s.Catalog.RangeOf(a.baseType(rng)); r != nil {
			if elem == 0 {
				elem = r.Subtype
			} else if elem != r.Subtype {
				return 0, false
			}
			if mrng == 0 {
				mrng = r.Multi
			}
		}
	} else if elem != 0 {
		if r := a.s.Catalog.RangeForSubtype(elem); r != nil {
			rng, mrng = r.OID, r.Multi
		}
	}
	var compat catalog.OID
	if len(compatElems) > 0 {
		c, ok := a.commonType(compatElems)
		if !ok {
			return 0, false
		}
		compat = c
	}
	_ = haveCompatArr
	if ret == catalog.Void {
		return catalog.Void, true
	}
	switch ret {
	case catalog.AnyElement, catalog.AnyNonArray, catalog.AnyEnum:
		if elem == 0 {
			return 0, false
		}
		return elem, true
	case catalog.AnyArray:
		if arr == 0 {
			return 0, false
		}
		return arr, true
	case catalog.AnyRange, catalog.AnyCompatibleRange:
		if rng == 0 {
			return 0, false
		}
		return rng, true
	case catalog.AnyMultirange, catalog.AnyCompatibleMultirange:
		if mrng == 0 {
			return 0, false
		}
		return mrng, true
	case catalog.AnyCompatible, catalog.AnyCompatibleNonArray:
		if compat == 0 {
			return 0, false
		}
		return compat, true
	case catalog.AnyCompatibleArray:
		if compat == 0 {
			return 0, false
		}
		return a.s.Types.ArrayOf(compat), true
	}
	return ret, true
}

// bindPolymorphicArg gives the concrete type an unknown-typed actual argument should take
// at a polymorphic position, once the others are resolved. 0 if not determinable.
func (a *analyzer) polymorphicArgType(declared catalog.OID, resolvedRet catalog.OID, elem catalog.OID) catalog.OID {
	switch declared {
	case catalog.AnyElement, catalog.AnyNonArray, catalog.AnyEnum, catalog.AnyCompatible, catalog.AnyCompatibleNonArray:
		return elem
	case catalog.AnyArray, catalog.AnyCompatibleArray:
		return a.s.Types.ArrayOf(elem)
	}
	return 0
}

// commonType implements select_common_type (§10.5) for UNION / CASE / ARRAY / COALESCE / IN.
// Returns text when every input is unknown.
func (a *analyzer) commonType(types []catalog.OID) (catalog.OID, bool) {
	var ptype catalog.OID
	for _, t := range types {
		if t != catalog.Unknown {
			ptype = a.baseType(t)
			break
		}
	}
	if ptype == 0 {
		return catalog.Text, true
	}
	pcat, ppref := a.category(ptype)
	for _, t := range types {
		if t == catalog.Unknown {
			continue
		}
		nt := a.baseType(t)
		if nt == ptype {
			continue
		}
		ncat, npref := a.category(nt)
		if ncat != pcat {
			return 0, false
		}
		if !ppref && a.canCoerce(ptype, nt, implicitCoercion) && !a.canCoerce(nt, ptype, implicitCoercion) {
			ptype, ppref = nt, npref
		}
	}
	// every input must be coercible to the chosen type
	for _, t := range types {
		if t != catalog.Unknown && !a.canCoerce(a.baseType(t), ptype, implicitCoercion) {
			return 0, false
		}
	}
	return ptype, true
}
