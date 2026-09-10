package dialect

import (
	"time"

	"github.com/kr9ly/sqlshape/v2"
	"github.com/kr9ly/sqlshape/postgres/v2"
)

// The stub dialect (vet/dialect_test.go) knows one table: things(id bigint not null,
// name varchar not null, price decimal null, at datetime not null). A parameter compared
// with a column takes its type; the statement "SELECT boom" is a database error at byte 7.
// Diagnostics about R land on the call, like the PostgreSQL path's.

type Thing struct {
	ID    int64
	Name  string
	Price *string
	At    time.Time
}

type ByID struct{ ID int64 }

var ok = sqlshape.Query[Thing, ByID](`SELECT id, name, price, at FROM things WHERE id = {{.ID}}`) // want `schema .*dialect_schema.sql: 3: a problem with the schema`

type Wrong struct {
	ID    string
	Name  string
	Price string
	At    time.Time
	Extra int
}

var wrong = sqlshape.Query[Wrong, struct{ ID string }](`SELECT id, name, price, at FROM things WHERE id = {{.ID}}`) // want `field ID is string but column "id" is bigint` `field Price is string but column "price" may be NULL` `field Wrong.Extra has no result column` `parameter .ID is string but SQL expects bigint`

var scalar = sqlshape.Query[int64, struct{}](`SELECT id FROM things`)

var twoCols = sqlshape.Query[int64, struct{}](`SELECT id, name FROM things`) // want `R is int64 but the query returns 2 columns`

var boom = sqlshape.Query[int64, struct{}](`SELECT boom`) // want `no such thing as boom \(TD001\)`

type Opt struct {
	ID   int64
	Name string
}

var branches = sqlshape.Query[Opt, struct{ Named bool }](`SELECT id{{if .Named}}, name{{end}} FROM things`) // want `field Opt.Name is not selected in every branch \[if@14:else\]: make it a pointer`

type Status string

func (*Status) Scan(any) error { return nil }

type Typed struct {
	ID   Status // a Scanner receives any column
	Name Status // a named string carries a varchar through its underlying type
}

var typed = sqlshape.Query[Typed, struct{}](`SELECT id, name FROM things`)

var one = sqlshape.One[int64, struct{}](`SELECT id FROM things`) // want `One: testdb cannot prove at most one row yet`

var mv = postgres.MatView("x") // want `MatView is not supported for testdb`
