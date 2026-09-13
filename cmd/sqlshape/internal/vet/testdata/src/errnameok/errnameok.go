package errnameok

import "github.com/kr9ly/sqlshape/v2"

// TooManyWidgets names the trigger's own P0501 -- the schema's Name for it, so this
// declaration is exactly what vet requires.
var TooManyWidgets = sqlshape.Error("P0501") // want TooManyWidgets:`sqlshape.Error\(P0501\)`

// byCode: the expect line spells the raw code.
var insertByCode = sqlshape.Query[struct{}, struct{ Qty int32 }]("-- sqlshape: expect P0501\nINSERT INTO widgets (qty) VALUES ({{.Qty}})")

// byName: the expect line spells the schema's Name instead -- interchangeable with the code.
var insertByName = sqlshape.Query[struct{}, struct{ Qty int32 }]("-- sqlshape: expect TooManyWidgets\nINSERT INTO widgets (qty) VALUES ({{.Qty}})")

// byBoth: writing both leaves neither unmatched.
var insertByBoth = sqlshape.Query[struct{}, struct{ Qty int32 }]("-- sqlshape: expect P0501, TooManyWidgets\nINSERT INTO widgets (qty) VALUES ({{.Qty}})")
