package errnamedup

import "github.com/kr9ly/sqlshape/v2"

// Two declarations claiming the same code: a duplicate, whichever one is right about the
// schema's own Name for it (this one is).
var TooManyWidgets = sqlshape.Error("P0501") // want TooManyWidgets:`sqlshape.Error\(P0501\)`

// TooManyWidgetsAgain both duplicates TooManyWidgets's code and gets the schema's Name for
// it wrong (the schema calls P0501 TooManyWidgets, not this).
var TooManyWidgetsAgain = sqlshape.Error("P0501") // want `TooManyWidgets and TooManyWidgetsAgain both declare sqlshape.Error\("P0501"\)` `schema names function widgets_check's error P0501 "TooManyWidgets", not "TooManyWidgetsAgain"` TooManyWidgetsAgain:`sqlshape.Error\(P0501\)`

var insert = sqlshape.Query[struct{}, struct{ Qty int32 }]("-- sqlshape: expect TooManyWidgets\nINSERT INTO widgets (qty) VALUES ({{.Qty}})")
