// Package mysqlstrict reads the -strict advisories of a MySQL schema: an engine that
// enforces no constraints, and a CHECK declared NOT ENFORCED.
package mysqlstrict

import (
	"github.com/kr9ly/sqlshape/v2"
)

var lastLog = sqlshape.One[struct{ ID int32 }, struct{ ID int32 }](`SELECT id FROM logs WHERE id = {{.ID}}`) // want `schema: table logs uses ENGINE=MyISAM: foreign keys are not enforced, and a statement is not atomic under it` `schema: table logs: CHECK c is NOT ENFORCED, so it documents an intent the server does not check` `parameter .ID carries key logs.id as a plain int32`
