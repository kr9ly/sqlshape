package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/kr9ly/sqlshape/internal/diff"
	"github.com/kr9ly/sqlshape/internal/dump"
)

// runVerify lists where the database differs from the target schema (drift). Column order
// differences are notes; any other difference is a finding (exit 1).
func runVerify(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("sqlshape verify-schema", flag.ContinueOnError)
	fs.SetOutput(stderr)
	db := fs.String("db", "", "connection string of the database to compare")
	getSchema := schemaFlag(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if *db == "" {
		return errors.New("-db is required")
	}
	schemaPath, err := getSchema()
	if err != nil {
		return err
	}
	srv, err := dump.NewServer(ctx)
	if err != nil {
		return err
	}
	defer srv.Close()
	tgt, err := loadTarget(ctx, srv, schemaPath)
	if err != nil {
		return err
	}
	if err := problems(tgt.text); err != nil {
		return fmt.Errorf("%s: %w", schemaPath, err)
	}
	current, _, err := dump.Load(ctx, *db, tgt.canonical)
	if err != nil {
		return err
	}
	var changes, notes []diff.Change
	for _, c := range diff.Compare(current, tgt.canonical) {
		if c.OrderOnly() {
			notes = append(notes, c)
		} else {
			changes = append(changes, c)
		}
	}
	for _, n := range notes {
		fmt.Fprintln(stderr, "note: "+strings.ReplaceAll(n.String(), "\n", "\n  "))
	}
	if len(changes) > 0 {
		var b strings.Builder
		printChanges(&b, changes)
		return found("the database differs from %s (database -> schema):\n%s", schemaPath, strings.TrimRight(b.String(), "\n"))
	}
	fmt.Fprintf(stdout, "ok: the database matches %s\n", schemaPath)
	return nil
}
