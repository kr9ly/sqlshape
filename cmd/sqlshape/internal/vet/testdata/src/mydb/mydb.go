// Package mydb is a program's own statement marker: a generic function over the template,
// registered with -query=mydb.Query,mydb.One:one. The checker reads it like sqlshape.Query.
package mydb

type Stmt[R, P any] struct{ SQL string }

func Query[R, P any](sql string) Stmt[R, P] { return Stmt[R, P]{SQL: sql} }

type Single[R, P any] struct{ SQL string }

func One[R, P any](sql string) Single[R, P] { return Single[R, P]{SQL: sql} }
