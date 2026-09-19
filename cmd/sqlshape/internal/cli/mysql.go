package cli

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/go-sql-driver/mysql"
	mydiff "github.com/kr9ly/sqlshape/check/mysql/v2/diff"
	mydump "github.com/kr9ly/sqlshape/check/mysql/v2/dump"
	mymigrate "github.com/kr9ly/sqlshape/check/mysql/v2/migrate"
	"github.com/kr9ly/sqlshape/v2/x/dialect"
)

// The MySQL side of diff / apply / verify-schema. The target is canonicalized in a scratch
// database on the -db server (dump.Scratch), or on a mysqld from PATH when there is no
// server (-from); the live side is read back the same way (dump.Load).

// mysqlCanonicalizer is the canonicalizer for a command: the -db server's scratch database
// when there is one, else a local mysqld. Tests replace it.
var mysqlCanonicalizer = func(dsn string) (mydump.Canonicalizer, error) {
	if dsn == "" {
		return mydump.Local{}, nil
	}
	return mydump.NewScratch(dsn)
}

// mysqlTarget canonicalizes the target schema and checks it is clean.
func mysqlTarget(ctx context.Context, c mydump.Canonicalizer, schemaPath, text string) (*mysqlSchema, error) {
	if err := mydump.Problems(text); err != nil {
		return nil, fmt.Errorf("%s: %w", schemaPath, err)
	}
	canon, canonText, err := c.Canonical(ctx, text)
	if err != nil {
		if errors.Is(err, mydump.ErrNoServer) {
			return nil, errors.New("no mysqld on PATH to canonicalize the schema: give -db, or put mysqld on PATH")
		}
		return nil, fmt.Errorf("%s: %w", schemaPath, err)
	}
	return &mysqlSchema{canon, canonText}, nil
}

// mysqlSchema is a canonical schema with the text it was read from.
type mysqlSchema struct {
	s    *mydump.Schema
	text string
}

func runDiffMySQL(ctx context.Context, schemaPath, text, db, from, pkgs string, stdout io.Writer) error {
	c, err := mysqlCanonicalizer(db)
	if err != nil {
		return err
	}
	tgt, err := mysqlTarget(ctx, c, schemaPath, text)
	if err != nil {
		return err
	}
	var current *mysqlSchema
	if db != "" {
		warnMySQLVersion(ctx, stdout, db, text)
		s, currentText, err := mydump.Load(ctx, db, mydump.Header(text))
		if err != nil {
			return err
		}
		current = &mysqlSchema{s, currentText}
	} else {
		fromText, err := dialect.ReadSchema(from)
		if err != nil {
			return err
		}
		s, currentText, err := c.Canonical(ctx, fromText)
		if err != nil {
			return fmt.Errorf("%s: %w", from, err)
		}
		current = &mysqlSchema{s, currentText}
	}
	intents, err := mymigrate.ParseIntents(text)
	if err != nil {
		return err
	}
	plan, planErr := mymigrate.Plan(current.s, tgt.s, intents)
	for _, stmt := range plan {
		fmt.Fprintln(stdout, stmt)
	}
	if pkgs != "" {
		index, err := indexConsumers(strings.Split(pkgs, ","), current.text)
		if err != nil {
			return err
		}
		if list := impacts(mysqlChanges(mydiff.Compare(current.s, tgt.s)), index); len(list) > 0 {
			fmt.Fprint(stdout, "\n-- consumers of what this plan drops or retypes:\n"+impactText(list, "-- "))
		}
	}
	if planErr != nil {
		return found("the plan needs declarations:\n  %s", strings.ReplaceAll(planErr.Error(), "\n", "\n  "))
	}
	return nil
}

func runApplyMySQL(ctx context.Context, schemaPath, text, db, ddl, pkgs string, dryRun, force bool, stdout, stderr io.Writer) error {
	if db == "" {
		return errors.New("-db is required")
	}
	c, err := mysqlCanonicalizer(db)
	if err != nil {
		return err
	}
	tgt, err := mysqlTarget(ctx, c, schemaPath, text)
	if err != nil {
		return err
	}
	warnMySQLVersion(ctx, stderr, db, text)
	current, currentText, err := mydump.Load(ctx, db, mydump.Header(text))
	if err != nil {
		return err
	}
	changes, err := mymigrate.Verify(ctx, c, currentText, ddl, tgt.s)
	if err != nil {
		return found("the DDL does not apply to the database's current schema: %v", err)
	}
	if len(changes) > 0 {
		var b strings.Builder
		for _, ch := range changes {
			fmt.Fprintln(&b, "  "+strings.ReplaceAll(ch.String(), "\n", "\n  "))
		}
		return found("the DDL does not reach %s; after it, the database would still differ:\n%s", schemaPath, b.String())
	}
	if pkgs != "" {
		index, err := indexConsumers(strings.Split(pkgs, ","), currentText)
		if err != nil {
			return err
		}
		if list := impacts(mysqlChanges(mydiff.Compare(current, tgt.s)), index); len(list) > 0 {
			if !force {
				return found("statements still depend on what the DDL drops or retypes (-force runs it anyway):\n%s", impactText(list, "  "))
			}
			fmt.Fprint(stderr, "warning: statements still depend on what the DDL drops or retypes:\n"+impactText(list, "  "))
		}
	}
	if dryRun {
		fmt.Fprintf(stdout, "ok: the DDL takes the database to %s (dry run, nothing applied)\n", schemaPath)
		return nil
	}
	conn, err := openMySQL(db)
	if err != nil {
		return err
	}
	defer conn.Close()
	stmts := mymigrate.SplitFor(ddl, tgt.s)
	for i, stmt := range stmts {
		if _, err := conn.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("apply: statement %d of %d failed, the %d before it are applied (MySQL DDL commits implicitly; `sqlshape diff` from here gives the rest):\n  %s\n%v", i+1, len(stmts), i, stmt, err)
		}
	}
	fmt.Fprintf(stdout, "applied: the database now matches %s\n", schemaPath)
	return nil
}

func runVerifyMySQL(ctx context.Context, schemaPath, text, db string, stdout, stderr io.Writer) error {
	c, err := mysqlCanonicalizer(db)
	if err != nil {
		return err
	}
	tgt, err := mysqlTarget(ctx, c, schemaPath, text)
	if err != nil {
		return err
	}
	warnMySQLVersion(ctx, stderr, db, text)
	current, _, err := mydump.Load(ctx, db, mydump.Header(text))
	if err != nil {
		return err
	}
	changes := mydiff.Compare(current, tgt.s)
	if len(changes) > 0 {
		var b strings.Builder
		for _, ch := range changes {
			fmt.Fprintln(&b, "  "+strings.ReplaceAll(ch.String(), "\n", "\n  "))
		}
		return found("the database differs from %s (database -> schema):\n%s", schemaPath, strings.TrimRight(b.String(), "\n"))
	}
	fmt.Fprintf(stdout, "ok: the database matches %s\n", schemaPath)
	return nil
}

// warnMySQLVersion tells the user when the server runs another MySQL major.minor than the
// schema declares.
func warnMySQLVersion(ctx context.Context, w io.Writer, dsn, schemaSQL string) {
	d, err := dialect.Declared(schemaSQL)
	if err != nil || d.Version == "" {
		return
	}
	server, err := mydump.ServerVersion(ctx, dsn)
	if err != nil {
		return
	}
	parts := strings.SplitN(server, ".", 3)
	if len(parts) < 2 {
		return
	}
	if mm := parts[0] + "." + parts[1]; mm != d.Version {
		fmt.Fprintf(w, "sqlshape: warning: the database runs MySQL %s but schema.sql declares %s: the DDL is judged and generated by %s's rules\n", server, d.Version, d.Version)
	}
}

func openMySQL(dsn string) (*sql.DB, error) {
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		return nil, err
	}
	c, err := mysql.NewConnector(cfg)
	if err != nil {
		return nil, err
	}
	return sql.OpenDB(c), nil
}
