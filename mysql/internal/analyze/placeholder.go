package analyze

import "strings"

// placeholderMap is the rewrite of sqlshape's `$n` placeholders into MySQL's `?`, kept so
// that a byte offset in the MySQL text maps back to the text the caller gave, and a `?`
// maps to its n.
type placeholderMap struct {
	// marks are the `?` positions in the MySQL text, in order, with their n
	marks []mark
	n     int // the highest n
}

type mark struct {
	at int // offset of `?` in the MySQL text
	n  int
	w  int // width of the original `$n`
}

// placeholders rewrites `$n` to `?`. A `$` inside a quoted string, a quoted identifier or
// an identifier (`a$1` is a legal MySQL name) is left alone.
func placeholders(sql string) (string, placeholderMap) {
	var b strings.Builder
	var pm placeholderMap
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

func (pm placeholderMap) count() int { return pm.n }

// back maps an offset in the MySQL text to the caller's text.
func (pm placeholderMap) back(off int) int {
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

// number is the n of the `?` at off, 0 when there is none.
func (pm placeholderMap) number(off int) int {
	for _, m := range pm.marks {
		if m.at == off {
			return m.n
		}
	}
	return 0
}
