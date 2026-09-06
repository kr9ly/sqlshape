package analyze

import (
	"encoding/json"
	"errors"
	"math"
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
		if _, err := parsePGInt(v, bits); err != nil {
			name := map[catalog.OID]string{catalog.Int2: "smallint", catalog.Int4: "integer", catalog.Int8: "bigint"}[base]
			if _, e2 := parsePGInt(v, 64); e2 == nil || errors.Is(e2, strconv.ErrRange) || isNumberish(v) {
				return errAt("22003", loc, "value %q is out of range for type %s", s, name)
			}
			return bad(name)
		}
	case catalog.Numeric:
		if _, err := parsePGInt(v, 64); err == nil {
			return nil // non-decimal integer literals and digit separators are numeric input too
		}
		if !isNumberish(v) || strings.Count(v, ".") > 1 {
			switch strings.ToLower(strings.TrimLeft(v, "+-")) {
			case "nan", "inf", "infinity": // NaN, [+-]inf, [+-]Infinity
			default:
				return bad("numeric")
			}
		}
	case catalog.Float4, catalog.Float8:
		f, err := strconv.ParseFloat(v, 64)
		if err != nil && !strings.EqualFold(v, "nan") && !strings.Contains(strings.ToLower(v), "inf") {
			if errors.Is(err, strconv.ErrRange) {
				return errAt("22003", loc, "%q is out of range for type double precision", s)
			}
			if base == catalog.Float4 {
				return bad("real")
			}
			return bad("double precision")
		}
		if err == nil && base == catalog.Float4 && (f > math.MaxFloat32 || f < -math.MaxFloat32) {
			return errAt("22003", loc, "%q is out of range for type real", s)
		}
	case catalog.JSON, catalog.JSONB:
		if !json.Valid([]byte(s)) {
			return bad(map[catalog.OID]string{catalog.JSON: "json", catalog.JSONB: "jsonb"}[base])
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

// parsePGInt parses an integer the way pg_strtoint does: optional sign, decimal or
// 0x / 0o / 0b prefixed digits, with single underscores allowed between digits.
func parsePGInt(v string, bits int) (int64, error) {
	neg := false
	if strings.HasPrefix(v, "-") || strings.HasPrefix(v, "+") {
		neg = v[0] == '-'
		v = v[1:]
	}
	base := 10
	if len(v) > 2 && v[0] == '0' {
		switch v[1] {
		case 'x', 'X':
			base, v = 16, v[2:]
		case 'o', 'O':
			base, v = 8, v[2:]
		case 'b', 'B':
			base, v = 2, v[2:]
		}
	}
	if base != 10 {
		v = strings.TrimPrefix(v, "_") // one separator may follow the prefix: 0x_ff
	}
	if v == "" || strings.HasPrefix(v, "_") || strings.HasSuffix(v, "_") || strings.Contains(v, "__") {
		return 0, strconv.ErrSyntax
	}
	v = strings.ReplaceAll(v, "_", "")
	if neg {
		v = "-" + v
	}
	return strconv.ParseInt(v, base, bits)
}
