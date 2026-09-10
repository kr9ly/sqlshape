package mysql

import (
	"context"
	"fmt"
	"strconv"

	"github.com/kr9ly/sqlshape/v2/x/dialect"
	"github.com/kr9ly/sqlshape/v2/x/sqlmode"
)

// Verify checks that the connection runs with the server settings schemaSQL declares
// (`-- sqlshape: server sql_mode = '...'`, `lower_case_table_names = N`; the server's
// defaults where it declares none): the checker judged the statements under those, and a
// server or a DSN that sets sql_mode otherwise (a DSN's `sql_mode=...`, an init command, a
// pool's session setup) would run them under different rules. It reads the session's
// @@sql_mode, so it sees what the pool's connections were given, and the global
// lower_case_table_names, which is fixed at the server's initialization. An application
// calls it once at start-up, after opening the pool.
func Verify(ctx context.Context, db DB, schemaSQL string) error {
	settings, err := dialect.Settings(schemaSQL)
	if err != nil {
		return err
	}
	want, wantLCTN := sqlmode.Default, 0
	for _, s := range settings {
		switch s.Name {
		case "sql_mode":
			if want, err = sqlmode.Parse(s.Value); err != nil {
				return fmt.Errorf("sqlshape: %w", err)
			}
		case "lower_case_table_names":
			if wantLCTN, err = strconv.Atoi(s.Value); err != nil {
				return fmt.Errorf("sqlshape: lower_case_table_names = %s: want a number", s.Value)
			}
		}
	}
	rows, err := db.QueryContext(ctx, "SELECT @@SESSION.sql_mode, @@GLOBAL.lower_case_table_names")
	if err != nil {
		return err
	}
	defer rows.Close()
	var mode string
	var lctn int
	if !rows.Next() {
		return fmt.Errorf("sqlshape: the server answered no row for its settings")
	}
	if err := rows.Scan(&mode, &lctn); err != nil {
		return err
	}
	got, err := sqlmode.Parse(mode)
	if err != nil {
		return fmt.Errorf("sqlshape: the server's sql_mode %q: %w", mode, err)
	}
	if got != want {
		return fmt.Errorf("sqlshape: the connection runs with sql_mode '%s' but schema.sql declares '%s' (the default when it declares none): the statements were checked under the declared mode; declare `-- sqlshape: server sql_mode = '%s'` if the connection's is the one meant", got, want, got)
	}
	if lctn != wantLCTN {
		return fmt.Errorf("sqlshape: the server runs with lower_case_table_names = %d but schema.sql declares %d: declare `-- sqlshape: server lower_case_table_names = %d` if the server is the one meant", lctn, wantLCTN, lctn)
	}
	return nil
}
