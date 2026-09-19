package marker

import "mydb"

// -query=mydb.Query,mydb.One:one: the program's own markers are checked like sqlshape's

type Order struct {
	ID    int64
	Total string
}

var ok = mydb.Query[Order, struct{}](`SELECT id, total FROM orders`)

var badColumn = mydb.Query[Order, struct{}](`SELECT id, totl FROM orders`) // want `column "totl" does not exist`

var notOne = mydb.One[Order, struct{}](`SELECT id, total FROM orders`) // want `cannot prove at most one row`
