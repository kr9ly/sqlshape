package plpgsql

import "github.com/kr9ly/sqlshape"

type AccountID int64 // want AccountID:`bound k accounts.id`

type Move struct {
	ID     AccountID
	Amount int64
}

// the first statement carries the schema problems: the body of broken() does not type-check,
// and -strict adds the dynamic EXECUTE of purge()
var first = sqlshape.Query[int64, struct{}](`SELECT count(*) FROM accounts`) // want `schema .*: function broken: 42703: line 3: column "balanse" of relation "accounts" does not exist` `schema: function purge: line 3: EXECUTE runs SQL built at run time, which is not checked`

// a call into a PL/pgSQL function carries what its body can raise (the annotated AC001 under
// its name), what its UPDATE can violate (the CHECK, and NOT NULL since balance - p_amount is
// not a bare parameter), and the trigger that UPDATE fires (P0001, unannotated), all through
// withdraw(); a function's result may be NULL
var withdrawOK = sqlshape.Query[*int64, Move]("-- sqlshape: expect AC001, P0001, accounts.balance, accounts_balance_check\nSELECT withdraw({{.ID}}, {{.Amount}})")

var withdrawMissing = sqlshape.Query[*int64, Move](`SELECT withdraw({{.ID}}, {{.Amount}})`) // want `may violate AC001 \(raised as Overdrawn, SQLSTATE AC001, through withdraw\(\)\)` `may violate P0001 \(raised by trigger accounts_no_negative on accounts, SQLSTATE P0001, through withdraw\(\)\)` `may violate accounts.balance \(NOT NULL on accounts.balance, SQLSTATE 23502, through withdraw\(\)\)` `may violate accounts_balance_check \(CHECK on accounts \(balance\), SQLSTATE 23514, through withdraw\(\)\)`

// a write on the trigger's table carries the unannotated RAISE by its default code
var deposit = sqlshape.Query[struct{}, Move]("-- sqlshape: expect accounts_balance_check, accounts.balance, P0001\nUPDATE accounts SET balance = balance + {{.Amount}} WHERE id = {{.ID}}")

var depositMissing = sqlshape.Query[struct{}, Move](`UPDATE accounts SET balance = balance + {{.Amount}} WHERE id = {{.ID}}`) // want `may violate P0001 \(raised by trigger accounts_no_negative on accounts, SQLSTATE P0001\)` `may violate accounts.balance` `may violate accounts_balance_check`
