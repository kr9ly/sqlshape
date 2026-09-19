package errnamemissing

import "github.com/kr9ly/sqlshape/v2"

// No var declares P0501 anywhere in this package: the expect line covers the violation
// for checkExpectations' own purposes, but vet still requires a Go binding for it.
var insert = sqlshape.Query[struct{}, struct{ Qty int32 }]("-- sqlshape: expect P0501\nINSERT INTO widgets (qty) VALUES ({{.Qty}})") // want `P0501 \(raised by trigger widgets_check_trigger on widgets as TooManyWidgets, SQLSTATE P0501\) has no .var TooManyWidgets = sqlshape.Error\("P0501"\). declared in this program`
