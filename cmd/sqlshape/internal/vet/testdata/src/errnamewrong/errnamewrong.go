package errnamewrong

import "github.com/kr9ly/sqlshape/v2"

// Wrong: the schema names P0501 TooManyWidgets, not this.
var Wrong = sqlshape.Error("P0501") // want `schema names function widgets_check's error P0501 "TooManyWidgets", not "Wrong"` Wrong:`sqlshape.Error\(P0501\)`

var insert = sqlshape.Query[struct{}, struct{ Qty int32 }]("-- sqlshape: expect P0501\nINSERT INTO widgets (qty) VALUES ({{.Qty}})")
