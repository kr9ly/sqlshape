package diff

// Alphabet: the migrate probe (check/postgres/migrate/probe_test.go) needs a coverage
// unit for its random schema pairs. The planner's whole input is a diff, so the unit
// lives here: every (Op, Kind[, Field]) triple Compare's differ can produce. Op is '+' /
// '-' / '~'; Field is only set for '~' (a key of the Props map the differ compared).
//
// The triples are mined mechanically from the same functions Compare itself calls
// (relProps, colProps, conProps, idxProps, ruleProps, polProps, fnProps, trgProps, and
// UserTypes for enum / domain / composite / range) against alphabetSQL, a schema built
// so that one object exercises every optional branch each of them has. A field a future
// change adds unconditionally to one of those functions shows up here automatically; one
// gated by a brand new condition needs alphabetSQL extended to trigger it, same as any
// other missed branch in a hand-picked fixture -- there is no way around that, chosen
// over hardcoding the key lists a second time (which is exactly the copy diff-outgrows
// silently that this alphabet exists to catch).
//
// Two things are not Props-mined, both because they are not a Props map at all: which
// Kinds Compare ever Adds / Drops versus Alter-only (rows) or Alter-never (schema,
// extension) -- that grouping is read off the differ's call sites and hardcoded in
// buildAlphabet; and rows / comment's single Field ("row <key>" / "text"), added by hand
// for the same reason.

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/kr9ly/sqlshape/check/postgres/v2/schema"
)

// AlphabetEntry is one (Op, Kind[, Field]) triple.
type AlphabetEntry struct {
	Op    Op
	Kind  string
	Field string // "" for Add / Drop
}

func (e AlphabetEntry) String() string {
	if e.Field == "" {
		return fmt.Sprintf("%c %s", e.Op, e.Kind)
	}
	return fmt.Sprintf("%c %s %s", e.Op, e.Kind, e.Field)
}

// NormalizeField maps a Field.Name that carries a user-chosen identifier -- a domain's
// CHECK constraint name ("check ck1"), a composite type's attribute name ("attribute
// a"), a seeded row's key ("row 3") -- to the class Alphabet lists for it. A real
// Change's Field.Name needs this before it is looked up against Alphabet's entries: the
// literal name is the generator's (or the user's) to choose, not part of the alphabet.
func NormalizeField(kind, name string) string {
	switch {
	case kind == "domain" && strings.HasPrefix(name, "check "):
		return "check <name>"
	case kind == "composite" && strings.HasPrefix(name, "attribute "):
		return "attribute <name>"
	case kind == "rows" && strings.HasPrefix(name, "row "):
		return "row <key>"
	}
	return name
}

// alphabetOnlyAdds are Kinds Compare only ever Adds / Drops, never Alters (no Props call
// exists for them): schema and extension are name sets (differ.strings), not diffed
// object properties.
var alphabetNoAlter = map[string]bool{"schema": true, "extension": true}

// alphabetAlterOnly are Kinds Compare only ever Alters: d.rows calls d.add(Alter, "rows",
// ...) alone, never Add / Drop (a row's own presence is a Field within the change, not
// the change itself).
var alphabetAlterOnly = map[string]bool{"rows": true}

var (
	alphabetOnce  sync.Once
	alphabetCache []AlphabetEntry
	alphabetErr   error
)

// Alphabet lists every (Op, Kind[, Field]) triple Compare's differ can produce (see the
// package doc comment above for how). The result is cached: the fixture parses once.
func Alphabet() ([]AlphabetEntry, error) {
	alphabetOnce.Do(func() {
		alphabetCache, alphabetErr = buildAlphabet()
	})
	return alphabetCache, alphabetErr
}

func buildAlphabet() ([]AlphabetEntry, error) {
	s, err := schema.Load(alphabetSQL)
	if err != nil {
		return nil, fmt.Errorf("alphabet fixture: %w", err)
	}
	if len(s.Problems) > 0 {
		return nil, fmt.Errorf("alphabet fixture: %v", s.Problems)
	}

	fields := map[string]map[string]bool{}
	add := func(kind string, props map[string]string) {
		if fields[kind] == nil {
			fields[kind] = map[string]bool{}
		}
		for k := range props {
			fields[kind][NormalizeField(kind, k)] = true
		}
	}
	need := func(cond bool, format string, args ...any) error {
		if !cond {
			return fmt.Errorf("alphabet fixture: "+format, args...)
		}
		return nil
	}

	ut := UserTypes(s)
	for _, n := range []string{"dom", "comp", "e", "myrange"} {
		u, ok := ut[n]
		if err := need(ok, "type %s missing", n); err != nil {
			return nil, err
		}
		add(u.Kind, u.Props)
	}

	relKindOf := map[string]string{
		"parent": "table", "child": "table", "base": "table", "kid": "table", "typed": "table",
		"t": "table", "v": "view", "mv": "matview", "seq": "sequence",
	}
	rels := map[string]*schema.Relation{}
	for n, kind := range relKindOf {
		r := s.Relation("", n)
		if err := need(r != nil, "relation %s missing", n); err != nil {
			return nil, err
		}
		rels[n] = r
		add(kind, relProps(s, r))
	}

	t := rels["t"]
	for _, c := range t.Columns {
		add("column", colProps(s, c))
	}
	for _, r := range []*schema.Relation{t, rels["base"]} {
		for _, c := range r.Constraints {
			add("constraint", conProps(c))
		}
	}
	for _, i := range t.Indexes {
		add("index", idxProps(i))
	}
	for _, rd := range t.Rules() {
		add("rule", ruleProps(rd))
	}
	for _, p := range t.Policies {
		add("policy", polProps(p))
	}
	for _, fn := range s.Functions {
		add("function", fnProps(s, fn))
	}
	if err := need(len(s.Triggers) > 0, "no trigger in the fixture"); err != nil {
		return nil, err
	}
	for _, trg := range s.Triggers {
		add("trigger", trgProps(trg))
	}
	// rows / comment: not a Props() map (see the package doc comment).
	add("rows", map[string]string{"row <key>": ""})
	add("comment", map[string]string{"text": ""})

	var kinds []string
	for k := range fields {
		kinds = append(kinds, k)
	}
	for k := range alphabetNoAlter {
		if fields[k] == nil {
			kinds = append(kinds, k)
		}
	}
	sort.Strings(kinds)

	var out []AlphabetEntry
	for _, k := range kinds {
		if !alphabetAlterOnly[k] {
			out = append(out, AlphabetEntry{Op: Add, Kind: k}, AlphabetEntry{Op: Drop, Kind: k})
		}
		if alphabetNoAlter[k] {
			continue
		}
		var fs []string
		for f := range fields[k] {
			fs = append(fs, f)
		}
		sort.Strings(fs)
		for _, f := range fs {
			out = append(out, AlphabetEntry{Op: Alter, Kind: k, Field: f})
		}
	}
	return out, nil
}

// alphabetSQL: a schema built to have one object triggering every optional branch of
// every Props-computing function (see Alphabet's doc comment). "-- sqlshape: postgres
// 18" opts into the PG18-only forms it uses (NOT ENFORCED, WITHOUT OVERLAPS / PERIOD,
// VIRTUAL generated columns).
const alphabetSQL = `-- sqlshape: postgres 18
CREATE DOMAIN dom AS integer NOT NULL CHECK (VALUE > 0);
CREATE TYPE comp AS (a integer, b text);
CREATE TYPE e AS ENUM ('x', 'y');
CREATE TYPE myrange AS RANGE (subtype = integer);

CREATE TABLE parent (id integer PRIMARY KEY) PARTITION BY RANGE (id);
CREATE TABLE child PARTITION OF parent FOR VALUES FROM (1) TO (100);

CREATE TABLE base (id integer PRIMARY KEY, n integer, valid_at daterange, CONSTRAINT base_pk PRIMARY KEY (id, valid_at WITHOUT OVERLAPS));
CREATE TABLE kid (n2 integer) INHERITS (base);

CREATE TABLE typed OF comp;

CREATE TABLE t (
  id integer GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  a integer NOT NULL DEFAULT 1,
  g integer GENERATED ALWAYS AS (a + 1) STORED,
  gv integer GENERATED ALWAYS AS (a + 2) VIRTUAL,
  u integer,
  w integer,
  valid_at daterange,
  CONSTRAINT t_u_key UNIQUE (u) DEFERRABLE,
  CONSTRAINT t_w_key UNIQUE NULLS NOT DISTINCT (w),
  CONSTRAINT t_a_check CHECK (a > 0) NOT ENFORCED,
  CONSTRAINT t_fk FOREIGN KEY (u) REFERENCES base (id) ON DELETE CASCADE ON UPDATE CASCADE,
  CONSTRAINT t_temporal_fk FOREIGN KEY (u, PERIOD valid_at) REFERENCES base (id, PERIOD valid_at),
  CONSTRAINT t_excl EXCLUDE USING btree (a WITH =) WHERE (a > 0)
);
CREATE INDEX t_a_idx ON t (a) WHERE (a > 0);
ALTER TABLE t ENABLE ROW LEVEL SECURITY;
ALTER TABLE t FORCE ROW LEVEL SECURITY;
CREATE POLICY t_pol ON t AS RESTRICTIVE FOR SELECT TO some_role USING (a > 0) WITH CHECK (a > 0);
COMMENT ON TABLE t IS 'a table';
COMMENT ON COLUMN t.a IS 'a column';

CREATE VIEW v AS SELECT id, a FROM t WITH CASCADED CHECK OPTION;
CREATE MATERIALIZED VIEW mv AS SELECT id, a FROM t;

CREATE SEQUENCE seq OWNED BY t.a;

CREATE FUNCTION f(x integer DEFAULT 1, OUT y integer) LANGUAGE sql AS $$ SELECT x $$;
CREATE FUNCTION agg_f(x integer) RETURNS integer LANGUAGE sql IMMUTABLE STRICT AS $$ SELECT x $$;
CREATE PROCEDURE p1(x integer) LANGUAGE sql AS $$ SELECT x $$;
CREATE AGGREGATE myagg(integer) (sfunc = int4pl, stype = integer);
CREATE FUNCTION winfn(x integer) RETURNS integer LANGUAGE internal WINDOW AS 'row_number';

CREATE FUNCTION tr_fn() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RETURN NEW; END $$;
CREATE TRIGGER tr AFTER INSERT OR UPDATE OF a OR DELETE ON t FOR EACH ROW EXECUTE FUNCTION tr_fn();

CREATE RULE r AS ON INSERT TO t DO INSTEAD NOTHING;
`
