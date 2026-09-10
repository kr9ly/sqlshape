package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/kr9ly/sqlshape/check/postgres/v2/diff"
	"github.com/kr9ly/sqlshape/check/postgres/v2/dump"
	"github.com/kr9ly/sqlshape/check/postgres/v2/migrate"
	"github.com/kr9ly/sqlshape/check/postgres/v2/schema"
)

// runDiff prints the DDL from the current state (-db, or the schema text at -from) to
// the target schema. The intents are the target's `-- @migrate` declarations; what they
// do not explain is reported and the exit code is 1, but the DDL is still printed. With
// -packages, the consumers of every drop / type change come after the DDL as comments.
func runDiff(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("sqlshape diff", flag.ContinueOnError)
	fs.SetOutput(stderr)
	db := fs.String("db", "", "connection string of the database holding the current schema")
	from := fs.String("from", "", "schema text holding the current schema (instead of -db)")
	pkgs := fs.String("packages", "", "comma-separated Go package patterns whose statements are indexed as consumers")
	getSchema := schemaFlag(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if (*db == "") == (*from == "") {
		return errors.New("give exactly one of -db and -from")
	}
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
	var current *schema.Schema
	var currentText string
	if *db != "" {
		warnServerVersion(ctx, stderr, *db, tgt.canonical.Version)
		current, currentText, err = dump.Load(ctx, *db, tgt.canonical)
		if err != nil {
			return err
		}
	} else {
		text, err := schema.ReadSource(*from)
		if err != nil {
			return err
		}
		current, currentText, err = srv.Canonical(ctx, text, tgt.canonical)
		if err != nil {
			return fmt.Errorf("%s: %w", *from, err)
		}
	}
	intents, err := migrate.ParseIntents(tgt.text)
	if err != nil {
		return err
	}
	plan, planErr := migrate.Plan(current, tgt.canonical, intents)
	for _, stmt := range plan {
		fmt.Fprintln(stdout, stmt)
	}
	if *pkgs != "" {
		index, err := indexConsumers(strings.Split(*pkgs, ","), tgt.canonical.Version, currentText)
		if err != nil {
			return err
		}
		if list := impacts(diff.Compare(current, tgt.canonical), index); len(list) > 0 {
			fmt.Fprint(stdout, "\n-- consumers of what this plan drops or retypes:\n"+impactText(list, "-- "))
		}
	}
	if planErr != nil {
		return found("the plan needs declarations:\n  %s", strings.ReplaceAll(planErr.Error(), "\n", "\n  "))
	}
	return nil
}
