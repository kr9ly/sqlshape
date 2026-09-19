package adv_template3

import "github.com/kr9ly/sqlshape/v2"

// x/expand's control() (x/expand/expand.go) records the field paths an {{if}} / {{with}}
// / {{range}} condition reads, so vet's "control paths must exist on P" check
// (cmd/sqlshape/internal/vet/vet.go, `for _, ctl := range res.Controls`) can catch a typo
// in a condition even when the field is never used as a `$n` parameter. control() only
// recognizes a *parse.FieldNode or *parse.DotNode as a direct argument of the condition's
// pipeline; a condition built from a nested function call -- `{{if gt (len .Xz) 0}}` --
// has `(len .Xz)` as a nested *parse.PipeNode argument, which control()'s switch has no
// case for, so `.Xz` is never added to Controls and the typo (Xz instead of Xs, the real
// field on CondParams below) is never checked against P.
//
// The same typo written as the bare condition `{{if .Xz}}` (CondParams2/d.go) is correctly
// flagged as "has no field Xz" -- proving the gap is specifically about a condition wrapped
// in a function call, not about typo detection on conditions in general.
type CondParams struct {
	Xs []int64
}

var withNestedCond = sqlshape.Query[int64, CondParams](`SELECT id FROM widgets {{if gt (len .Xz) 0}}WHERE id = ANY({{.Xs}}){{end}}`) // want `sqlshape: adv_template3\.CondParams has no field Xz`
