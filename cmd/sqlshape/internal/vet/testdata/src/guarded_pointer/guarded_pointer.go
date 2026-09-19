package guarded_pointer

import "github.com/kr9ly/sqlshape/v2"

// (a) A pointer parameter read inside the then branch of a plain {{if .Name}} on itself
// is proven non-nil for the rest of this expansion: no NOT NULL violation, in either
// expansion (the else branch sends nothing, and t.name has a DEFAULT).
type PA struct {
	ID   int64
	Name *string
}

var qa = sqlshape.Query[struct{}, PA]("-- sqlshape: expect t_pkey\nINSERT INTO t (id{{if .Name}}, name{{end}}) VALUES ({{.ID}}{{if .Name}}, {{.Name}}{{end}})")

// (b) Control case: the same pointer used unconditionally (no guard) still reports the
// NOT NULL violation -- the guard only applies to a use inside the condition that
// proved it non-nil.
type PB struct {
	ID   int64
	Name *string
}

var qb = sqlshape.Query[struct{}, PB]("-- sqlshape: expect t_pkey\nINSERT INTO t (id, name) VALUES ({{.ID}}, {{.Name}})") // want `may violate t.name \(NOT NULL on t.name, SQLSTATE 23502\)`

// (c) The else branch of {{if .Name}} does not guard: reading Name.Name there still
// reports the violation, even though .Name is only reached when the condition is false
// (the checker does not reason about "known false", only "known true").
type PC struct {
	ID   int64
	Name *string
}

var qc = sqlshape.Query[struct{}, PC]("-- sqlshape: expect t_pkey\nINSERT INTO t (id, name) VALUES ({{.ID}}, {{if .Name}}'literal'{{else}}{{.Name}}{{end}})") // want `may violate t.name \(NOT NULL on t.name, SQLSTATE 23502\).*\[if@\d+:else\]`

// (d) {{with .Order}} guards .Order itself only, not a path nested under it: .Name here
// resolves (dot-relative) to the absolute path .Order.Name, a *different* path from the
// guarded .Order, so it gets no benefit from the guard.
//
// (d1) When Name is not a pointer, there was never a NOT NULL violation to begin with
// (its own zero value, not NULL, is what would be sent) -- this case is unaffected by
// guarding either way.
type OrderD1 struct {
	Name string
}

type PD1 struct {
	ID    int64
	Order *OrderD1
}

var qd1 = sqlshape.Query[struct{}, PD1]("-- sqlshape: expect t_pkey\nINSERT INTO t (id{{with .Order}}, name{{end}}) VALUES ({{.ID}}{{with .Order}}, {{.Name}}{{end}})")

// (d2) When Name is itself a pointer, .Order being non-nil (proven by the with) says
// nothing about .Order.Name's own nilness: the violation still fires, because only a
// condition on .Order.Name itself (not .Order) would prove it.
type OrderD2 struct {
	Name *string
}

type PD2 struct {
	ID    int64
	Order *OrderD2
}

var qd2 = sqlshape.Query[struct{}, PD2]("-- sqlshape: expect t_pkey\nINSERT INTO t (id{{with .Order}}, name{{end}}) VALUES ({{.ID}}{{with .Order}}, {{.Name}}{{end}})") // want `may violate t.name \(NOT NULL on t.name, SQLSTATE 23502\)`
