// Package placeholder rewrites sqlshape's numbered placeholders (`$n`, what the template
// expander emits for every dialect) into MySQL's positional `?`, and maps positions back.
package placeholder

import "strings"

// Map is the rewrite of `$n` placeholders into `?`, kept so that a byte offset in the
// MySQL text maps back to the text the caller gave, and a `?` maps to its n.
type Map struct {
	// marks are the `?` positions in the MySQL text, in order, with their n
	marks []mark
	n     int // the highest n
}

type mark struct {
	at int // offset of `?` in the MySQL text
	n  int
	w  int // width of the original `$n`
}

// Rewrite turns `$n` into `?`. A `$` inside a quoted string, a quoted identifier or an
// identifier (`a$1` is a legal MySQL name) is left alone.
func Rewrite(sql string) (string, Map) {
	var b strings.Builder
	var pm Map
	var quote byte
	ident := false
	for i := 0; i < len(sql); i++ {
		c := sql[i]
		switch {
		case quote != 0:
			b.WriteByte(c)
			if c == '\\' && quote != '`' && i+1 < len(sql) {
				i++
				b.WriteByte(sql[i])
			} else if c == quote {
				quote = 0
			}
			continue
		case c == '\'' || c == '"' || c == '`':
			quote = c
			ident = false
			b.WriteByte(c)
			continue
		case c == '$' && !ident:
			j := i + 1
			for j < len(sql) && sql[j] >= '0' && sql[j] <= '9' {
				j++
			}
			if j > i+1 {
				n := 0
				for _, d := range sql[i+1 : j] {
					n = n*10 + int(d-'0')
				}
				pm.marks = append(pm.marks, mark{at: b.Len(), n: n, w: j - i})
				if n > pm.n {
					pm.n = n
				}
				b.WriteByte('?')
				i = j - 1
				continue
			}
		}
		ident = isIdentByte(c)
		b.WriteByte(c)
	}
	return b.String(), pm
}

func isIdentByte(c byte) bool {
	return c == '_' || c == '$' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= 0x80
}

// Count is the highest n: how many parameters the statement takes.
func (pm Map) Count() int { return pm.n }

// Marks is how many `?` the rewrite produced: the same n written twice is two marks.
func (pm Map) Marks() int { return len(pm.marks) }

// Back maps an offset in the MySQL text to the caller's text.
func (pm Map) Back(off int) int {
	if off < 0 {
		return -1
	}
	shift := 0
	for _, m := range pm.marks {
		if m.at >= off {
			break
		}
		shift += m.w - 1
	}
	return off + shift
}

// Number is the n of the `?` at off, 0 when there is none.
func (pm Map) Number(off int) int {
	for _, m := range pm.marks {
		if m.at == off {
			return m.n
		}
	}
	return 0
}
