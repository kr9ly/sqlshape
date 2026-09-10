// Command gen dumps the bootstrap catalog (pg_catalog objects) of the oracle's
// PostgreSQL into internal/catalog/data/<major>/*.tsv, and extension catalogs into
// internal/catalog/data/<major>/ext/<name>/. Run from the repo root:
//
//	go run ./internal/catalog/gen -pg 18                 # bootstrap catalog
//	go run ./internal/catalog/gen -pg 18 -ext citext     # one extension (repeatable)
//	go run ./internal/catalog/gen -pg 18 -all-ext        # every extension 17's data has
//	go run ./internal/catalog/gen -pg 18 -list           # extensions the oracle binary ships
//
// The TSVs are embedded by package catalog; regenerate when the oracle's release for the
// major version changes (internal/oracle binaries).
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/kr9ly/sqlshape/check/postgres/catalog"
	"github.com/kr9ly/sqlshape/check/postgres/oracle"
	"github.com/kr9ly/sqlshape/check/postgres/pgparse"
)

// Each query yields one TSV. Column order is the parsing contract in catalog.go.
// %[1]s is the row filter: bootstrap objects live in pg_catalog, an extension's
// objects are the members collected in ext_members.
var dumps = map[string]string{
	"pg_type": `
		SELECT t.oid, t.typname, t.typtype, t.typcategory, t.typispreferred,
		       t.typlen, t.typbyval, t.typelem, t.typarray, t.typrelid, t.typbasetype, t.typtypmod,
		       n.nspname
		  FROM pg_type t JOIN pg_namespace n ON n.oid = t.typnamespace
		 WHERE %[6]s
		 ORDER BY t.oid`,
	"pg_proc": `
		SELECT p.oid, p.proname, p.prokind, p.prorettype, p.proretset, p.provariadic,
		       p.pronargs, p.pronargdefaults, p.proisstrict, p.provolatile,
		       array_to_string(p.proargtypes::oid[], ' '),
		       coalesce(array_to_string(p.proallargtypes, ' '), ''),
		       coalesce(array_to_string(p.proargmodes, ''), ''),
		       coalesce(array_to_string(p.proargnames, ','), ''),
		       n.nspname
		  FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
		 WHERE %[1]s
		 ORDER BY p.oid`,
	"pg_operator": `
		SELECT o.oid, o.oprname, o.oprkind, o.oprleft, o.oprright, o.oprresult,
		       o.oprcode::oid, o.oprcom, o.oprnegate, o.oprcanmerge, o.oprcanhash,
		       n.nspname
		  FROM pg_operator o JOIN pg_namespace n ON n.oid = o.oprnamespace
		 WHERE %[1]s
		 ORDER BY o.oid`,
	"pg_cast": `
		SELECT c.oid, c.castsource, c.casttarget, c.castfunc::oid, c.castcontext, c.castmethod
		  FROM pg_cast c
		 WHERE %[2]s
		 ORDER BY c.oid`,
	"pg_aggregate": `
		SELECT a.aggfnoid::oid, a.aggkind, a.aggnumdirectargs, a.aggtranstype
		  FROM pg_aggregate a
		 WHERE %[3]s
		 ORDER BY a.aggfnoid::oid`,
	"pg_range": `
		SELECT r.rngtypid, r.rngsubtype, r.rngmultitypid
		  FROM pg_range r
		 WHERE %[4]s
		 ORDER BY r.rngtypid`,
	// system relations: pg_catalog and information_schema tables / views, one row per column
	"pg_class": `
		SELECT c.oid, c.relname, c.relkind, n.nspname,
		       a.attnum, a.attname, a.atttypid, a.atttypmod, a.attnotnull
		  FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		  JOIN pg_attribute a ON a.attrelid = c.oid
		 WHERE %[5]s AND c.relkind IN ('r', 'v') AND a.attnum > 0 AND NOT a.attisdropped
		 ORDER BY c.oid, a.attnum`,
}

// filters are [namespace rows, pg_cast rows, pg_aggregate rows, pg_range rows, pg_class rows, pg_type rows].
var (
	// information_schema's domains type its views' columns, so they ride along in pg_type
	bootstrapFilters = [6]string{"n.nspname = 'pg_catalog'", "true", "true", "true",
		"n.nspname IN ('pg_catalog', 'information_schema')", "n.nspname IN ('pg_catalog', 'information_schema')"}
	extFilters = [6]string{
		"%s.oid IN (SELECT objid FROM ext_members)",
		"c.oid IN (SELECT objid FROM ext_members)",
		"a.aggfnoid::oid IN (SELECT objid FROM ext_members)",
		"r.rngtypid IN (SELECT objid FROM ext_members)",
		"c.oid IN (SELECT objid FROM ext_members)",
		"t.oid IN (SELECT objid FROM ext_members)",
	}
)

// memberSQL collects every object an extension (and the extensions it pulled in with
// CASCADE) owns: direct members, plus what depends on them internally (array types,
// the row type of a composite, ...). Everything a fresh database gained.
const memberSQL = `
CREATE TEMP TABLE ext_members AS
WITH RECURSIVE m(classid, objid) AS (
    SELECT d.classid, d.objid
      FROM pg_depend d
     WHERE d.refclassid = 'pg_extension'::regclass AND d.deptype = 'e'
    UNION
    SELECT d.classid, d.objid
      FROM pg_depend d JOIN m ON d.refclassid = m.classid AND d.refobjid = m.objid
     WHERE d.deptype IN ('i', 'a')
)
SELECT DISTINCT objid FROM m`

func main() {
	var exts multi
	list := flag.Bool("list", false, "list the extensions available in the oracle binary")
	major := flag.Int("pg", int(pgparse.Default), "PostgreSQL major version to dump")
	allExt := flag.Bool("all-ext", false, "dump every extension the default version's data has a dump for")
	flag.Var(&exts, "ext", "dump this extension's catalog (repeatable)")
	flag.Parse()
	version := pgparse.Version(*major)
	if *allExt {
		exts = append(exts, catalog.Available(int(pgparse.Default))...)
	}

	ctx := context.Background()
	if *list {
		o := start(ctx, version)
		defer o.Close()
		rows, err := o.Conn().Query(ctx, "SELECT name, default_version, coalesce(comment, '') FROM pg_available_extensions ORDER BY name")
		if err != nil {
			log.Fatal(err)
		}
		for rows.Next() {
			var name, ver, comment string
			if err := rows.Scan(&name, &ver, &comment); err != nil {
				log.Fatal(err)
			}
			fmt.Printf("%-24s %-8s %s\n", name, ver, comment)
		}
		return
	}
	dataDir := filepath.Join("internal", "catalog", "data", fmt.Sprint(*major))
	if len(exts) == 0 {
		o := start(ctx, version)
		defer o.Close()
		outDir := dataDir
		if err := os.MkdirAll(outDir, 0o755); err != nil {
			log.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(outDir, "VERSION"), []byte(serverVersion(ctx, o)+"\n"), 0o644); err != nil {
			log.Fatal(err)
		}
		dump(ctx, o, outDir, bootstrapFilters)
		return
	}
	for _, ext := range exts {
		// a fresh cluster per extension: what it owns is exactly what the database gained
		o := start(ctx, version)
		if _, err := o.Conn().Exec(ctx, fmt.Sprintf("CREATE EXTENSION %s CASCADE", quoteIdent(ext))); err != nil {
			log.Fatalf("%s: %v", ext, err)
		}
		if _, err := o.Conn().Exec(ctx, memberSQL); err != nil {
			log.Fatal(err)
		}
		var version, schema string
		var requires []string
		err := o.Conn().QueryRow(ctx, `
			SELECT e.extversion, n.nspname, coalesce(v.requires, '{}')
			  FROM pg_extension e
			  JOIN pg_namespace n ON n.oid = e.extnamespace
			  JOIN pg_available_extension_versions v ON v.name = e.extname AND v.version = e.extversion
			 WHERE e.extname = $1`, ext).Scan(&version, &schema, &requires)
		if err != nil {
			log.Fatalf("%s: %v", ext, err)
		}
		outDir := filepath.Join(dataDir, "ext", ext)
		if err := os.MkdirAll(outDir, 0o755); err != nil {
			log.Fatal(err)
		}
		meta := fmt.Sprintf("version\t%s\nschema\t%s\nrequires\t%s\nserver\t%s\n", version, schema, strings.Join(requires, ","), serverVersion(ctx, o))
		if err := os.WriteFile(filepath.Join(outDir, "META"), []byte(meta), 0o644); err != nil {
			log.Fatal(err)
		}
		fmt.Printf("== %s %s (schema %s", ext, version, schema)
		if len(requires) > 0 {
			fmt.Printf(", requires %s", strings.Join(requires, " "))
		}
		fmt.Println(")")
		f := extFilters
		dump(ctx, o, outDir, f)
		o.Close()
	}
}

func start(ctx context.Context, v pgparse.Version) *oracle.Oracle {
	o, err := oracle.StartVersion(ctx, v, "")
	if err != nil {
		log.Fatal(err)
	}
	return o
}

func serverVersion(ctx context.Context, o *oracle.Oracle) string {
	var version string
	if err := o.Conn().QueryRow(ctx, "SHOW server_version").Scan(&version); err != nil {
		log.Fatal(err)
	}
	return version
}

func dump(ctx context.Context, o *oracle.Oracle, outDir string, filters [6]string) {
	for name, q := range dumps {
		alias := map[string]string{"pg_type": "t", "pg_proc": "p", "pg_operator": "o"}[name]
		nsFilter := filters[0]
		if strings.Contains(nsFilter, "%s") {
			nsFilter = fmt.Sprintf(nsFilter, alias)
		}
		q = fmt.Sprintf(q, nsFilter, filters[1], filters[2], filters[3], filters[4], filters[5])
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
}

func quoteIdent(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(s string) error { *m = append(*m, s); return nil }
