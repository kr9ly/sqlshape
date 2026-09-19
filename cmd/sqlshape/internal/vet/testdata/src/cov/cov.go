package cov

import "github.com/kr9ly/sqlshape/v2"

var dynamic string

var checked = sqlshape.Query[int64, struct{}](`SELECT id FROM users`) // want `coverage: 1 of 2 statements checked, 1 unchecked \(non-constant templates\)`

var unchecked = sqlshape.Query[int64, struct{}](dynamic) // want `query template must be a string constant`
