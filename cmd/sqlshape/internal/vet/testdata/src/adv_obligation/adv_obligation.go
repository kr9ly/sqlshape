package adv_obligation

import "github.com/kr9ly/sqlshape/v2"

type Row struct{ ID int64 }

type P struct {
	ID  int64
	ID2 int64
	X   bool
}

// Both branches of {{if .X}} omit orders.tenant_id from their WHERE, so
// `require pinned(tenant_id)` fails identically in every expansion -- the same
// message, word for word, regardless of which branch ran. docs/templates.md's
// "Many branches" section says: "A problem shared by every expansion is reported
// once, without the suffix" (the [if@N:branch] tag that marks a branch-specific
// failure): one diagnostic, with no branch suffix, since the failure does not
// depend on .X at all.
var q = sqlshape.Query[Row, P](`SELECT id FROM orders {{if .X}} WHERE id = {{.ID}} {{else}} WHERE id = {{.ID2}} {{end}}`) // want `orders\.tenant_id is not pinned: every statement on orders must fix tenant_id by equality \(or assign it\)$`
