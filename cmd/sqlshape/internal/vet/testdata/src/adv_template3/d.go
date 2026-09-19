package adv_template3

import "github.com/kr9ly/sqlshape/v2"

// Control case for c.go: the same typo'd field name, used as a bare condition instead of
// wrapped in a function call, is correctly flagged.
type CondParams2 struct {
	Xs []int64
}

var directCond = sqlshape.Query[int64, CondParams2](`SELECT id FROM widgets {{if .Xz}}WHERE id = ANY({{.Xs}}){{end}}`) // want `sqlshape: adv_template3\.CondParams2 has no field Xz`
