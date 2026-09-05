package analyze

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/kr9ly/sqlshape/internal/catalog"
)

var uuidRe = regexp.MustCompile(`^\{?[0-9a-fA-F]{8}-?[0-9a-fA-F]{4}-?[0-9a-fA-F]{4}-?[0-9a-fA-F]{4}-?[0-9a-fA-F]{12}\}?$`)

// validateLiteral mirrors the input-function errors PG raises at parse time when an
// untyped string literal is coerced to a type (SQLSTATE 22P02). Only types whose
// syntax is simple and stable are checked; others are accepted.
func (a *analyzer) validateLiteral(s string, to catalog.OID, loc int32) *Error {
	base := a.baseType(to)
	bad := func(what string) *Error {
		return errAt("22P02", loc, "invalid input syntax for type %s: %q", what, s)
	}
	v := strings.TrimSpace(s)
	switch base {
	case catalog.Int2, catalog.Int4, catalog.Int8:
		bits := map[catalog.OID]int{catalog.Int2: 16, catalog.Int4: 32, catalog.Int8: 64}[base]
		if _, err := strconv.ParseInt(v, 10, bits); err != nil {
			name := map[catalog.OID]string{catalog.Int2: "smallint", catalog.Int4: "integer", catalog.Int8: "bigint"}[base]
			if _, e2 := strconv.ParseInt(v, 10, 64); e2 == nil || isNumberish(v) {
				return errAt("22003", loc, "value %q is out of range for type %s", s, name)
			}
			return bad(name)
		}
	case catalog.Numeric:
		if !isNumberish(v) || strings.Count(v, ".") > 1 {
			if !strings.EqualFold(v, "nan") && !strings.EqualFold(v, "infinity") && !strings.EqualFold(v, "-infinity") {
				return bad("numeric")
			}
		}
	case catalog.Float4, catalog.Float8:
		if _, err := strconv.ParseFloat(v, 64); err != nil && !strings.EqualFold(v, "nan") && !strings.Contains(strings.ToLower(v), "inf") {
			if base == catalog.Float4 {
				return bad("real")
			}
			return bad("double precision")
		}
	case catalog.Bool:
		switch strings.ToLower(v) {
		case "t", "true", "f", "false", "y", "yes", "n", "no", "on", "off", "1", "0", "tr", "tru", "fa", "fal", "fals", "ye", "of":
		default:
			return bad("boolean")
		}
	case catalog.UUID:
		if !uuidRe.MatchString(v) {
			return bad("uuid")
		}
	default:
		if labels, ok := a.s.Types.Enums[base]; ok {
			for _, l := range labels {
				if l == s {
					return nil
				}
			}
			return errAt("22P02", loc, "invalid input value for enum %s: %q", a.typ(base).Name, s)
		}
	}
	return nil
}

func isNumberish(v string) bool {
	if v == "" {
		return false
	}
	digits := 0
	for i, r := range v {
		switch {
		case r >= '0' && r <= '9':
			digits++
		case r == '.', r == 'e', r == 'E':
		case (r == '-' || r == '+') && (i == 0 || v[i-1] == 'e' || v[i-1] == 'E'):
		default:
			return false
		}
	}
	return digits > 0
}
