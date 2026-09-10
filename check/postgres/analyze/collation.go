package analyze

import (
	"strings"

	"github.com/kr9ly/sqlshape/check/postgres/v2/catalog"
	"github.com/kr9ly/sqlshape/check/postgres/v2/pgparse"
	"github.com/kr9ly/sqlshape/check/postgres/v2/schema"
)

// Collations (PostgreSQL manual §24.2.2, parse_collate.c).
//
// Every expression of a collatable type (the string category: text, varchar, char,
// name, and arrays / domains over them) carries a collation with a derivation strength:
// none, implicit (a column's declared collation, the default one for a literal), or
// explicit (a COLLATE clause). Combining inputs: any explicit collation wins, and two
// different explicit ones are an error PG raises when it parses the statement (42P21).
// Among implicit ones a non-default collation beats the default, and two different
// non-default ones leave the result's collation indeterminate. PG accepts such a
// statement and fails when it runs it, at the first comparison that needs the collation
// ("could not determine which collation to use for string comparison"), so sqlshape
// reports that as a Note at the operator, function, ORDER BY, GROUP BY or DISTINCT that
// would fail. The set operations UNION / INTERSECT / EXCEPT (not ALL) compare rows and
// PG checks their collation at parse time, so a conflict there is an Error like the
// explicit one.

type collStrength byte

const (
	collNone collStrength = iota
	collImplicit
	collConflict
	collExplicit
)

// collation is the collation state of one expression. name "" is the default collation.
type collation struct {
	strength collStrength
	name     string
	// name2 / loc2 are the second collation of a conflict, for the message and position.
	name2 string
	loc   int32
	loc2  int32
}

const (
	codeCollationMismatch = "42P21"
	noteCollation         = "collation"
)

// collatable reports whether a value of type oid carries a collation.
func (a *analyzer) collatable(oid catalog.OID) bool {
	t := a.typ(a.baseType(oid))
	if t == nil {
		return false
	}
	if t.IsArray() {
		t = a.typ(a.baseType(t.Elem))
		if t == nil {
			return false
		}
	}
	return t.Category == 'S' && t.OID != catalog.Unknown
}

// collName normalizes a (possibly schema-qualified) collation name; "default" is "".
func collName(parts []string) string {
	if len(parts) == 0 {
		return ""
	}
	n := parts[len(parts)-1]
	if n == "default" {
		return ""
	}
	return n
}

func (c collation) display() string {
	if c.name == "" {
		return "default"
	}
	return c.name
}

func collDisplay(name string) string {
	if name == "" {
		return "default"
	}
	return name
}

// asVar is the collation a column read from a FROM item has: an explicit derivation does
// not survive a subquery / CTE / view boundary, an indeterminate one does.
func (c collation) asVar() collation {
	if c.strength == collExplicit {
		return collation{strength: collImplicit, name: c.name, loc: c.loc}
	}
	return c
}

// merge folds in into acc following merge_collation_state; a conflict between explicit
// collations is the parse-time error.
func (a *analyzer) mergeColl(acc *collation, in collation) *Error {
	if in.strength == collNone {
		return nil
	}
	if in.strength > acc.strength {
		*acc = in
		return nil
	}
	if in.strength < acc.strength {
		return nil
	}
	switch in.strength {
	case collImplicit:
		if in.name != acc.name {
			if acc.name == "" {
				acc.name, acc.loc = in.name, in.loc
			} else if in.name != "" {
				acc.strength, acc.name2, acc.loc2 = collConflict, in.name, in.loc
			}
		}
	case collExplicit:
		if in.name != acc.name {
			return errAt(codeCollationMismatch, in.loc, "collation mismatch between explicit collations %q and %q", acc.display(), collDisplay(in.name))
		}
	}
	return nil
}

// collOf combines the collations of the inputs of one operator / function / common-type context.
func (a *analyzer) collOf(es []*expr) (collation, *Error) {
	var acc collation
	for _, e := range es {
		if e == nil {
			continue
		}
		if err := a.mergeColl(&acc, e.coll); err != nil {
			return collation{}, err
		}
	}
	return acc, nil
}

// resultColl is the collation the result of type res takes from its inputs' combination.
func (a *analyzer) resultColl(c collation, res catalog.OID) collation {
	if !a.collatable(res) {
		return collation{}
	}
	return c
}

// noteCollConflict reports an indeterminate collation where PG would need one at run time.
func (a *analyzer) noteCollConflict(c collation, at int32, what string) {
	if c.strength != collConflict {
		return
	}
	a.note(noteCollation, at, "could not determine which collation to use for "+what+": implicit collations "+
		quoteColl(c.name)+" and "+quoteColl(c.name2)+" conflict, PostgreSQL fails at run time (apply COLLATE to one side)")
}

// collConflictError is the parse-time refusal of sorting or grouping on an expression whose
// implicit collations conflict (ORDER BY, GROUP BY, DISTINCT, aggregate / window ORDER BY);
// elsewhere a conflict only fails at run time and is a note.
func collConflictError(c collation, at int32) *Error {
	if c.strength != collConflict {
		return nil
	}
	return errAt(codeCollationMismatch, at, "collation mismatch between implicit collations %s and %s", quoteColl(c.name), quoteColl(c.name2))
}

func quoteColl(name string) string { return `"` + collDisplay(name) + `"` }

// collSensitiveOp lists the operators that compare strings under a collation.
var collSensitiveOp = map[string]bool{
	"=": true, "<>": true, "!=": true, "<": true, ">": true, "<=": true, ">=": true,
	"~~": true, "!~~": true, "~~*": true, "!~~*": true, "~": true, "!~": true, "~*": true, "!~*": true,
}

// collSensitiveFunc lists the functions whose result depends on their arguments' collation.
var collSensitiveFunc = map[string]bool{
	"lower": true, "upper": true, "initcap": true, "min": true, "max": true,
	"regexp_match": true, "regexp_matches": true, "regexp_replace": true, "regexp_split_to_array": true,
	"regexp_split_to_table": true, "regexp_like": true, "regexp_count": true, "regexp_instr": true, "regexp_substr": true,
}

// isCollSensitiveOp reports whether name compares collatable operands.
func (a *analyzer) isCollSensitiveOp(name string, es []*expr) bool {
	if !collSensitiveOp[name] {
		return false
	}
	for _, e := range es {
		if e != nil && a.collatable(e.oid()) {
			return true
		}
	}
	return false
}

// explicitCollate types a COLLATE clause: the argument must be collatable.
func (a *analyzer) explicitCollate(e *expr, name []string, at int32) *Error {
	if e.oid() == catalog.Unknown {
		if err := a.bind(e, catalog.Text, at); err != nil {
			return err
		}
	}
	if !a.collatable(e.oid()) {
		return errAt(codeDatatypeMismatch, at, "collations are not supported by type %s", a.s.Types.Format(e.typ))
	}
	if n := collName(name); !a.s.KnownCollation(n) {
		return errAt(codeUndefinedObject, at, "collation %q for encoding \"UTF8\" does not exist", n)
	}
	e.coll = collation{strength: collExplicit, name: collName(name), loc: at}
	e.src = nil // Describe reports no source column through a COLLATE clause
	return nil
}

// columnColl is the collation a table column declares (implicit derivation).
func (a *analyzer) columnColl(c *schema.Column) collation {
	if !a.collatable(c.Type.OID) {
		return collation{}
	}
	return collation{strength: collImplicit, name: collName(strings.Split(c.Collation, "."))}
}

// setOpColl combines the collations of one output column of a set operation. PG resolves
// it while parsing (select_common_collation), so unless the operation is UNION ALL, which
// never compares rows, a conflict between implicit collations is an error too.
func (a *analyzer) setOpColl(l, r collation, op pgparse.SetOperation, all bool) (collation, *Error) {
	acc := l
	if err := a.mergeColl(&acc, r); err != nil {
		return collation{}, err
	}
	if acc.strength == collConflict && !(all && op == pgparse.SetOperation_SETOP_UNION) {
		return collation{}, errAt(codeCollationMismatch, acc.loc2, "collation mismatch between implicit collations %q and %q", collDisplay(acc.name), collDisplay(acc.name2))
	}
	return acc, nil
}
