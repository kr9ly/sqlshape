package mysql

import (
	"github.com/kr9ly/sqlshape/v2"
	"github.com/kr9ly/sqlshape/v2/x/placeholder"
)

// positional turns a rendering's `$n` placeholders into MySQL's `?` and lines the arguments
// up in the order the `?` appear (a `$n` written twice is sent twice). The arguments go
// through sqlshape.Normalize (named strings to string, nil pointers to nil).
func positional(r sqlshape.Rendered) (string, []any) {
	text, pm := placeholder.Rewrite(r.SQL)
	order := pm.Order()
	args := make([]any, len(order))
	for i, n := range order {
		if n >= 1 && n <= len(r.Args) {
			args[i] = sqlshape.Normalize(r.Args[n-1])
		}
	}
	return text, args
}
