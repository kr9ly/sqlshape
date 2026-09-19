package adv_param

import "github.com/kr9ly/sqlshape/v2"

// A scalar int64 parameter into a smallint column: the checker warns (control case,
// proves paramFit's overflow note works at the top level).
type ScalarParams struct {
	ID    int64
	Small int64
}

var updateScalar = sqlshape.Query[struct{}, ScalarParams](`UPDATE t SET small = {{.Small}} WHERE id = {{.ID}}`) // want `parameter .Small: int64 into smallint may overflow`

// The same int64 -> smallint overflow risk, but through an array parameter
// (smallint[]): matchValue's array branch recurses through matchValue directly and
// never goes through paramFit, so the "may overflow" note that fires for the scalar
// case above never fires here even though the same runtime risk exists element-wise.
type ArrayParams struct {
	ID   int64
	Tags []int64
}

// This currently vets clean (no diagnostic): a bug per the adv-param lane report. The
// `want` below records the correct behavior (an overflow note, same as ScalarParams.Small
// above) so this test fails until the array branch of matchValue is routed through
// paramFit the way the scalar path is.
var updateArray = sqlshape.Query[struct{}, ArrayParams](`UPDATE t SET tags = {{.Tags}} WHERE id = {{.ID}}`) // want `parameter .Tags: int64 into smallint may overflow`

// Same gap for real[] (Float4): a scalar float64 -> real parameter gets "loses
// precision" from paramFit; a []float64 -> real[] array parameter gets nothing.
type ArrayFloatParams struct {
	ID    int64
	Reals []float64
}

// Same as updateArray above: currently no diagnostic (bug); `want` records the correct
// behavior so this fails until fixed.
var updateArrayFloat = sqlshape.Query[struct{}, ArrayFloatParams](`UPDATE t SET reals = {{.Reals}} WHERE id = {{.ID}}`) // want `parameter .Reals: float64 into real loses precision`
