package sqlshape

// Failure is a statement's failure mode as the program names it: a schema.sql `--
// sqlshape: error <code> = <Name>` annotation's Name, brought into Go by Error. A
// runtime's Violates(err, key) takes a Failure or a plain string (both spell the same
// thing to it: the code the database itself reports), so a program can hand its own
// named value to the same call that used to take the raw code.
//
//	// schema.sql, above the trigger / routine that raises it:
//	// -- sqlshape: error 30001 = OrderTooLarge
//	var OrderTooLarge = sqlshape.Error("30001")
//	// ...
//	if mysql.Violates(err, OrderTooLarge) { ... }
//
// vet checks both directions: every `sqlshape.Error(code)` declaration names a code the
// schema actually declares, under the Name the schema gave it, and every code the schema
// declares has a Go declaration for it somewhere in the program.
type Failure string

// Error declares a program's Go name for a schema's `-- sqlshape: error <code> = <Name>`
// annotation. code is the annotation's key exactly as schema.sql spells it: a SQLSTATE
// (PostgreSQL, "P0401") or a MYSQL_ERRNO in decimal or a SQLSTATE (MySQL, "30001" or
// "45000"). vet requires the declaration's own name (the identifier `var X = ...` binds)
// to be the schema's Name for that code.
func Error(code string) Failure { return Failure(code) }
