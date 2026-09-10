package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/kr9ly/sqlshape/check/postgres/diff"
	"github.com/kr9ly/sqlshape/check/postgres/dump"
	"github.com/kr9ly/sqlshape/check/postgres/migrate"
)

// runApply checks a DDL file by its end state — the database's current schema plus the
// DDL, on an embedded PostgreSQL, must read back as the target — and by the consumers of
// what it drops or retypes (-packages: none may remain, unless -force), then runs it on
// the database, in one transaction unless -no-transaction (CREATE INDEX CONCURRENTLY and
// friends cannot run inside one). -dry-run stops before running.
func runApply(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("sqlshape apply", flag.ContinueOnError)
	fs.SetOutput(stderr)
	db := fs.String("db", "", "connection string of the database to migrate")
	pkgs := fs.String("packages", "", "comma-separated Go package patterns whose statements must no longer depend on what the DDL drops or retypes")
	dryRun := fs.Bool("dry-run", false, "verify only; do not run the DDL")
	force := fs.Bool("force", false, "run even when consumers of dropped / retyped columns remain")
	noTx := fs.Bool("no-transaction", false, "run the DDL as is, not inside one transaction")
	getSchema := schemaFlag(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("give the DDL file to apply")
	}
	if *db == "" {
		return errors.New("-db is required")
	}
	ddlBytes, err := os.ReadFile(fs.Arg(0))
	if err != nil {
		return err
	}
	ddl := string(ddlBytes)
	schemaPath, err := getSchema()
	if err != nil {
		return err
	}
	text, version, err := readTarget(schemaPath)
	if err != nil {
		return err
	}
	srv, err := newServer(ctx, version)
	if err != nil {
		return err
	}
	defer srv.Close()
	tgt, err := loadTarget(ctx, srv, schemaPath, text)
	if err != nil {
		return err
	}
	if err := problems(tgt.text); err != nil {
		return fmt.Errorf("%s: %w", schemaPath, err)
	}
	warnServerVersion(ctx, stderr, *db, tgt.canonical.Version)
	current, currentText, err := dump.Load(ctx, *db, tgt.canonical)
	if err != nil {
		return err
	}
	changes, notes, err := migrate.Verify(ctx, srv, currentText, ddl, tgt.canonical)
	if err != nil {
		return found("the DDL does not apply to the database's current schema: %v", err)
	}
	if len(changes) > 0 {
		var b strings.Builder
		printChanges(&b, changes)
		return found("the DDL does not reach %s; after it, the database would still differ:\n%s", schemaPath, b.String())
	}
	for _, n := range notes {
		fmt.Fprintln(stderr, "note: "+strings.ReplaceAll(n.String(), "\n", "\n  "))
	}
	if *pkgs != "" {
		index, err := indexConsumers(strings.Split(*pkgs, ","), tgt.canonical.Version, currentText)
		if err != nil {
			return err
		}
		if list := impacts(diff.Compare(current, tgt.canonical), index); len(list) > 0 {
			if !*force {
				return found("statements still depend on what the DDL drops or retypes (-force runs it anyway):\n%s", impactText(list, "  "))
			}
			fmt.Fprint(stderr, "warning: statements still depend on what the DDL drops or retypes:\n"+impactText(list, "  "))
		}
	}
	if *dryRun {
		fmt.Fprintf(stdout, "ok: the DDL takes the database to %s (dry run, nothing applied)\n", schemaPath)
		return nil
	}
	conn, err := pgx.Connect(ctx, *db)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	if *noTx {
		if _, err := conn.Exec(ctx, ddl); err != nil {
			return fmt.Errorf("apply: %w", err)
		}
	} else {
		tx, err := conn.Begin(ctx)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, ddl); err != nil {
			tx.Rollback(ctx)
			return fmt.Errorf("apply (rolled back): %w", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return err
		}
	}
	fmt.Fprintf(stdout, "applied: the database now matches %s\n", schemaPath)
	return nil
}
