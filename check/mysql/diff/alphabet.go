package diff

// Alphabet: the migrate probe (check/mysql/migrate/probe_test.go) needs a coverage unit
// for its random schema pairs. The planner's whole input is a diff, so the unit lives
// here: every (Op, Kind[, Field]) triple Compare's differ can produce. Op is '+' / '-' /
// '~'; Field is only set for '~' (a key of the Props map Compare calls for that Kind).
//
// The triples are mined mechanically from the same exported Props functions Compare
// itself calls (TableProps, ColumnProps, KeyProps, ForeignKeyProps, CheckProps,
// ViewProps, RoutineProps, EventProps, TriggerProps) against alphabetSQL, a schema
// holding one of everything Compare's Kinds cover. Unlike PostgreSQL's diff (see
// check/postgres/diff/alphabet.go), none of MySQL's Props functions here branch on the
// object's shape -- each always returns the same fixed set of keys regardless of what it
// is handed (ColumnProps never has a key only a generated column carries, say) -- so one
// representative object per Kind is enough; there is no need to hunt down a schema that
// exercises every optional branch, because there are none. A field a future change adds
// to one of those functions shows up here automatically.
//
// Every Kind Compare can produce (table, column, key, foreign key, check, view,
// procedure, function, trigger, event) goes through the same Add / Drop / Alter shape
// (differ.tables / parts / views / routines / triggers / events), so unlike PostgreSQL's
// alphabet there is no Kind that is Alter-only or never-Alter to special-case.

import (
	"fmt"
	"sort"
	"sync"

	"github.com/kr9ly/sqlshape/check/mysql/v2/internal/schema"
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
			fields[kind][k] = true
		}
	}
	need := func(cond bool, format string, args ...any) error {
		if !cond {
			return fmt.Errorf("alphabet fixture: "+format, args...)
		}
		return nil
	}

	t := s.Table("t")
	if err := need(t != nil, "table t missing"); err != nil {
		return nil, err
	}
	add("table", TableProps(t))
	if err := need(len(t.Columns) > 0, "table t has no columns"); err != nil {
		return nil, err
	}
	for _, c := range t.Columns {
		add("column", ColumnProps(t, c))
	}
	if err := need(len(t.Keys) > 0, "table t has no keys"); err != nil {
		return nil, err
	}
	for _, k := range t.Keys {
		add("key", KeyProps(k))
	}
	if err := need(len(t.ForeignKeys) > 0, "table t has no foreign keys"); err != nil {
		return nil, err
	}
	for _, fk := range t.ForeignKeys {
		add("foreign key", ForeignKeyProps(fk))
	}
	if err := need(len(t.Checks) > 0, "table t has no checks"); err != nil {
		return nil, err
	}
	for _, c := range t.Checks {
		add("check", CheckProps(t, c))
	}
	if err := need(len(s.Views) > 0, "no view in the fixture"); err != nil {
		return nil, err
	}
	for _, v := range s.Views {
		add("view", ViewProps(v))
	}
	procs, funcs := 0, 0
	for _, r := range s.Routines {
		switch r.Kind {
		case schema.Procedure:
			add("procedure", RoutineProps(r))
			procs++
		case schema.Function:
			add("function", RoutineProps(r))
			funcs++
		}
	}
	if err := need(procs > 0, "no procedure in the fixture"); err != nil {
		return nil, err
	}
	if err := need(funcs > 0, "no function in the fixture"); err != nil {
		return nil, err
	}
	if err := need(len(s.Triggers) > 0, "no trigger in the fixture"); err != nil {
		return nil, err
	}
	for _, trg := range s.Triggers {
		add("trigger", TriggerProps(trg))
	}
	if err := need(len(s.Events) > 0, "no event in the fixture"); err != nil {
		return nil, err
	}
	for _, ev := range s.Events {
		add("event", EventProps(ev))
	}

	var kinds []string
	for k := range fields {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)

	var out []AlphabetEntry
	for _, k := range kinds {
		out = append(out, AlphabetEntry{Op: Add, Kind: k}, AlphabetEntry{Op: Drop, Kind: k})
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

// alphabetSQL: a schema holding one table (with a column, a key, a foreign key and a
// check), a view, a procedure, a function, a trigger and an event -- one of every Kind
// Compare's differ produces a Change for, so every Props function it calls runs at least
// once (see the package doc comment above for why one representative object per Kind is
// enough here).
const alphabetSQL = `-- sqlshape: mysql 8.4
CREATE TABLE parent (id INT PRIMARY KEY);
CREATE TABLE t (
  id INT PRIMARY KEY,
  a INT NOT NULL DEFAULT 1,
  CONSTRAINT ck1 CHECK (a > 0),
  CONSTRAINT fk1 FOREIGN KEY (a) REFERENCES parent (id) ON DELETE CASCADE ON UPDATE CASCADE,
  KEY k1 (a)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin COMMENT='a table';
CREATE VIEW v AS SELECT id, a FROM t;
CREATE FUNCTION f1(x INT) RETURNS INT DETERMINISTIC RETURN x;
CREATE PROCEDURE p1(x INT) BEGIN SELECT x; END;
CREATE TRIGGER tr1 BEFORE INSERT ON t FOR EACH ROW SET NEW.a = NEW.a;
CREATE EVENT ev1 ON SCHEDULE EVERY 1 HOUR DO SET @probe = 1;
`
