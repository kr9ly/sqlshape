// Command gen dumps the bootstrap catalog (pg_catalog objects) of the oracle's
// PostgreSQL into internal/catalog/data/*.tsv. Run from the repo root:
//
//	go run ./internal/catalog/gen
//
// The TSVs are embedded by package catalog; regenerate when oracle.Version changes.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/kr9ly/sqlshape/internal/oracle"
)

// Each query yields one TSV. Column order is the parsing contract in catalog.go.
var dumps = map[string]string{
	"pg_type": `
		SELECT t.oid, t.typname, t.typtype, t.typcategory, t.typispreferred,
		       t.typlen, t.typbyval, t.typelem, t.typarray, t.typrelid, t.typbasetype, t.typtypmod
		  FROM pg_type t JOIN pg_namespace n ON n.oid = t.typnamespace
		 WHERE n.nspname = 'pg_catalog'
		 ORDER BY t.oid`,
	"pg_proc": `
		SELECT p.oid, p.proname, p.prokind, p.prorettype, p.proretset, p.provariadic,
		       p.pronargs, p.pronargdefaults, p.proisstrict, p.provolatile,
		       array_to_string(p.proargtypes::oid[], ' '),
		       coalesce(array_to_string(p.proallargtypes, ' '), ''),
		       coalesce(array_to_string(p.proargmodes, ''), ''),
		       coalesce(array_to_string(p.proargnames, ','), '')
		  FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
		 WHERE n.nspname = 'pg_catalog'
		 ORDER BY p.oid`,
	"pg_operator": `
		SELECT o.oid, o.oprname, o.oprkind, o.oprleft, o.oprright, o.oprresult,
		       o.oprcode::oid, o.oprcom, o.oprnegate, o.oprcanmerge, o.oprcanhash
		  FROM pg_operator o JOIN pg_namespace n ON n.oid = o.oprnamespace
		 WHERE n.nspname = 'pg_catalog'
		 ORDER BY o.oid`,
	"pg_cast": `
		SELECT c.oid, c.castsource, c.casttarget, c.castfunc::oid, c.castcontext, c.castmethod
		  FROM pg_cast c
		 ORDER BY c.oid`,
	"pg_aggregate": `
		SELECT a.aggfnoid::oid, a.aggkind, a.aggnumdirectargs, a.aggtranstype
		  FROM pg_aggregate a
		 ORDER BY a.aggfnoid::oid`,
}

func main() {
	ctx := context.Background()
	o, err := oracle.Start(ctx, "")
	if err != nil {
		log.Fatal(err)
	}
	defer o.Close()

	var version string
	if err := o.Conn().QueryRow(ctx, "SHOW server_version").Scan(&version); err != nil {
		log.Fatal(err)
	}
	outDir := filepath.Join("internal", "catalog", "data")
	if err := os.WriteFile(filepath.Join(outDir, "VERSION"), []byte(version+"\n"), 0o644); err != nil {
		log.Fatal(err)
	}
	for name, q := range dumps {
		f, err := os.Create(filepath.Join(outDir, name+".tsv"))
		if err != nil {
			log.Fatal(err)
		}
		copySQL := fmt.Sprintf("COPY (%s) TO STDOUT", strings.TrimSpace(q))
		tag, err := o.Conn().PgConn().CopyTo(ctx, f, copySQL)
		f.Close()
		if err != nil {
			log.Fatalf("%s: %v", name, err)
		}
		fmt.Printf("%-13s %6d rows\n", name, tag.RowsAffected())
	}
	fmt.Println("server_version", version)
}
