package analyze

import (
	"strings"

	"github.com/kr9ly/sqlshape/check/postgres/catalog"
)

// Domains are opaque units.
//
// PostgreSQL resolves operators on a domain's base type, so with
// `CREATE DOMAIN yen AS bigint` the expression `price_yen + weight_g` type-checks and
// yields bigint. sqlshape is stricter: a domain value only meets a value of the same
// domain, or an untyped literal / constant / parameter, which adopts the unit. Mixing a
// domain with another domain or with a plain value of its base type is reported as a
// Note (PG accepts the statement, so this never touches oracle parity), and the fix is
// an explicit cast to the base type, which says "I mean to drop the unit".
//
// So the check can follow a value through an expression, unit-preserving operations
// keep the domain on their result: yen + yen, yen * n, yen / n, -yen, abs(yen),
// max(yen), COALESCE(yen, 0) are all yen. yen / yen is a plain ratio. Functions PG
// resolves on the base type otherwise return the base type (sum(yen) is numeric).

// Note is a finding that PostgreSQL itself would not report.
type Note struct {
	Code     string
	Message  string
	Position int32 // 1-based, 0 if none
}

const (
	noteDomainMismatch = "domain-mismatch"
	noteAlwaysFails    = "always-fails"
	// noteSQLStateDynamic: a RAISE ... USING ERRCODE = <expr> whose expr the analyzer
	// cannot resolve to a fixed SQLSTATE at analysis time (not a literal, not a variable
	// initialized once to a literal and never reassigned, not a caught SQLSTATE re-raise
	// in an EXCEPTION handler). Reported instead of silently defaulting to P0001, which
	// would misreport what the RAISE actually throws.
	noteSQLStateDynamic = "sqlstate-dynamic"
	// advisory notes (the checker reports them with -strict)
	noteUnorderedLimit = "unordered-limit"
	noteEnumOrder      = "enum-order"
	noteNoIndex        = "no-index"
	noteViewPushdown   = "view-pushdown"
)

// Advisory reports whether a note is advice rather than a likely bug.
func (n Note) Advisory() bool {
	switch n.Code {
	case noteUnorderedLimit, noteEnumOrder, noteNoIndex, noteViewPushdown, notePLDynamicSQL:
		return true
	}
	return false
}

func (a *analyzer) note(code string, loc int32, msg string) {
	pos := int32(0)
	if loc >= 0 {
		pos = loc + 1
	}
	a.notes = append(a.notes, Note{Code: code, Message: msg, Position: pos})
}

// isLit reports whether e is a literal, constant or parameter: it carries no unit of
// its own and adopts the domain of whatever it meets.
func isLit(e *expr) bool {
	return e == nil || e.lit || e.param > 0 || e.oid() == catalog.Unknown
}

// domainType returns oid's type when it is a domain.
func (a *analyzer) domainType(oid catalog.OID) *catalog.Type {
	t := a.typ(oid)
	if t != nil && t.Kind == 'd' && t.BaseType != 0 {
		return t
	}
	return nil
}

// domainOf returns the domain e carries, nil for literals and plain values.
func (a *analyzer) domainOf(e *expr) *catalog.Type {
	if isLit(e) {
		return nil
	}
	return a.domainType(e.oid())
}

// plain reports whether e is a typed value that is neither a domain nor a literal.
func (a *analyzer) plain(e *expr) bool {
	return !isLit(e) && a.domainType(e.oid()) == nil
}

// domainOp applies the unit rules to name(l, r) (l nil for prefix operators) whose PG
// result type is res, and returns the result type: the domain when the unit survives.
func (a *analyzer) domainOp(name string, l, r *expr, res catalog.OID, at int32) catalog.OID {
	ld, rd := a.domainOf(l), a.domainOf(r)
	if ld == nil && rd == nil {
		return res
	}
	keep := func(d *catalog.Type) catalog.OID {
		if res == d.BaseType {
			return d.OID
		}
		return res
	}
	if l == nil {
		return keep(rd) // -yen is yen
	}
	mismatch := func(why string) catalog.OID {
		a.note(noteDomainMismatch, at, "domain mismatch: "+a.s.Types.Format(l.typ)+" "+name+" "+a.s.Types.Format(r.typ)+": "+why)
		return res
	}
	same := ld != nil && rd != nil && ld.OID == rd.OID
	mixed := (ld != nil && rd != nil && !same) || (ld != nil && a.plain(r)) || (rd != nil && a.plain(l))
	switch name {
	case "=", "<>", "!=", "<", ">", "<=", ">=", "+", "-", "%":
		// both sides must carry the same unit
		if mixed {
			return mismatch("operands must share the domain (cast to the base type to drop it)")
		}
		if ld != nil {
			return keep(ld)
		}
		return keep(rd)
	case "*":
		// scaling by a unitless factor keeps the unit; the product of two units has no domain
		if ld != nil && rd != nil {
			return mismatch("product of two domain values has no domain (cast to the base type to drop it)")
		}
		if ld != nil {
			return keep(ld)
		}
		return keep(rd)
	case "/":
		// yen / yen is a plain ratio; yen / n stays yen; n / yen is nothing PG can name
		if same {
			return res
		}
		if ld != nil && rd == nil {
			return keep(ld)
		}
		return mismatch("operands must share the domain, or divide the domain value by a plain one (cast to the base type to drop it)")
	}
	return res
}

// domainUnify applies the unit rules to a select_common_type context (CASE, COALESCE,
// UNION, ...) whose PG result is t: all typed inputs must share one domain, which the
// result then keeps.
func (a *analyzer) domainUnify(es []*expr, t catalog.OID, at int32, context string) catalog.OID {
	var d *catalog.Type
	var others []string
	for _, e := range es {
		if isLit(e) {
			continue
		}
		if dd := a.domainOf(e); dd != nil {
			if d == nil {
				d = dd
				continue
			}
			if dd.OID == d.OID {
				continue
			}
		}
		others = append(others, a.s.Types.Format(e.typ))
	}
	if d == nil {
		return t
	}
	if len(others) > 0 {
		a.note(noteDomainMismatch, at, "domain mismatch: "+context+" mixes "+d.Name+" with "+strings.Join(others, ", ")+" (cast to the base type to drop the domain)")
		return t
	}
	if t == d.BaseType {
		return d.OID
	}
	return t
}

// unitPreserving lists the functions whose result carries the domain of their argument.
var unitPreserving = map[string]bool{
	"abs": true, "max": true, "min": true,
	"round": true, "floor": true, "ceil": true, "ceiling": true, "trunc": true, "mod": true,
	"lower": true, "upper": true, "btrim": true, "ltrim": true, "rtrim": true, "trim": true,
}

// domainFunc keeps the domain on the result of a unit-preserving function whose
// non-literal arguments all share it.
func (a *analyzer) domainFunc(name string, args []*expr, res catalog.OID) catalog.OID {
	if !unitPreserving[name] {
		return res
	}
	var d *catalog.Type
	for _, e := range args {
		if isLit(e) {
			continue
		}
		dd := a.domainOf(e)
		if dd == nil || (d != nil && dd.OID != d.OID) {
			return res
		}
		d = dd
	}
	if d != nil && res == d.BaseType {
		return d.OID
	}
	return res
}

// domainAssign reports a typed value of the wrong unit stored into a domain column.
// A plain column accepts anything: it declares no unit to defend.
func (a *analyzer) domainAssign(e *expr, colType catalog.OID, relName, colName string, at int32) {
	d := a.domainType(colType)
	if d == nil || isLit(e) {
		return
	}
	if ed := a.domainOf(e); ed != nil && ed.OID == d.OID {
		return
	}
	a.note(noteDomainMismatch, at, "domain mismatch: "+relName+"."+colName+" is "+d.Name+" but the value is "+a.s.Types.Format(e.typ)+" (cast to "+d.Name+" to assert the unit)")
}
