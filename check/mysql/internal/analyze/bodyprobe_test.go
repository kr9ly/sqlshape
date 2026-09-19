package analyze

// The body probe: the server's CREATE-time verdict on trigger, procedure and function
// bodies against the analyzer's. Generated bodies mix the constructs the server refuses
// when the body is created (a duplicate DECLARE, a bad SQLSTATE literal, a LEAVE with no
// label, a RETURN in a procedure, a result set in a function, COMMIT / DDL / dynamic SQL /
// FLUSH in a function or trigger, LOCK TABLES / LOAD DATA / ALTER VIEW anywhere, NEW / OLD
// misuse, an undeclared variable or cursor) with ordinary ones; each is CREATEd on a
// running mysqld and DROPped again. Where the server refuses, the analyzer must raise the
// same number (AnalyzeRoutine / AnalyzeTrigger) or the loader the mirroring problem; where
// the server accepts, the loader must report no problem and the analyzer no CREATE-time
// code (a run-time certainty -- 1442, 1456 -- is allowed). Skipped without a mysqld on PATH
// (nix-shell -p mysql84).

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	driver "github.com/go-sql-driver/mysql"

	"github.com/kr9ly/sqlshape/check/mysql/v2/internal/schema"
	"github.com/kr9ly/sqlshape/mysqltest/v2"
)

var (
	bodyN      = flag.Int("body-probe-n", 300, "bodies the body probe judges")
	bodySeed   = flag.Int64("body-probe-seed", 1, "seed of the body probe's generator")
	bodyReport = flag.String("body-probe-report", "", "write the body probe's findings here (default: the test log)")
)

const bodyProbeBase = `-- sqlshape: mysql 8.4
CREATE TABLE t (id INT NOT NULL PRIMARY KEY, a INT, b INT NOT NULL);
CREATE TABLE u (id INT NOT NULL PRIMARY KEY, a INT);
CREATE VIEW v AS SELECT id, a FROM u;
CREATE PROCEDURE other() BEGIN INSERT INTO u VALUES (99, 1); END;
`

// createTimeCodes are the numbers the analyzer raises for what the server refuses at CREATE
// time; an analyzer error of another number against a body the server accepts is a
// run-time certainty (1442, 1456, 1424, 1231, ...) and not judged here.
var createTimeCodes = map[int]bool{1193: true, 1308: true, 1313: true, 1314: true, 1320: true, 1324: true, 1327: true, 1331: true, 1332: true, 1333: true, 1336: true, 1362: true, 1363: true, 1407: true, 1413: true, 1415: true, 1422: true}

// bodyAlphabet lists the shapes the generator draws and the CREATE-time refusals the server
// can answer with.
var bodyAlphabet = []string{
	"procedure", "function", "trigger before insert", "trigger after insert", "trigger before update", "trigger after update", "trigger before delete", "trigger after delete",
	"declare", "declare duplicate", "condition", "condition duplicate", "condition bad sqlstate", "cursor", "cursor duplicate", "handler", "handler duplicate",
	"set", "select into", "select into undeclared", "select result set", "insert", "update own table", "commit", "prepare", "flush", "lock tables", "load data", "alter view", "create table",
	"loop leave", "leave bad label", "return", "no return", "signal", "signal errno zero", "open fetch close", "open undeclared cursor", "set new", "set old", "read old", "read new", "set new unknown column", "read new unknown column", "call", "case no else",
	"refused 1054", "refused 1193", "refused 1308", "refused 1313", "refused 1314", "refused 1320", "refused 1324", "refused 1327", "refused 1331", "refused 1332", "refused 1333", "refused 1336", "refused 1362", "refused 1363", "refused 1407", "refused 1413", "refused 1415", "refused 1422", "accepted",
}

// bodyKnownUnreached names the alphabet entries the run does not reach, each with why.
var bodyKnownUnreached = map[string]string{}

// genBody is one generated routine or trigger: its CREATE text, name, kind and shapes.
type genBody struct {
	name   string
	kind   string // "procedure", "function", "trigger"
	create string
	shapes []string
}

func genRoutine(r *rand.Rand, i int) genBody {
	g := genBody{name: fmt.Sprintf("bp%d", i)}
	timing, event := "", ""
	switch r.Intn(4) {
	case 0:
		g.kind = "procedure"
	case 1:
		g.kind = "function"
	default:
		g.kind = "trigger"
		timing = []string{"BEFORE", "AFTER"}[r.Intn(2)]
		event = []string{"INSERT", "UPDATE", "DELETE"}[r.Intn(3)]
		g.shapes = append(g.shapes, "trigger "+strings.ToLower(timing)+" "+strings.ToLower(event))
	}
	if g.kind != "trigger" {
		g.shapes = append(g.shapes, g.kind)
	}
	var lines []string
	add := func(shape, line string) {
		g.shapes = append(g.shapes, shape)
		lines = append(lines, "  "+line)
	}
	// declarations first, as the grammar wants
	vars := map[string]bool{}
	if r.Intn(2) == 0 {
		add("declare", "DECLARE v INT DEFAULT 0;")
		vars["v"] = true
		if r.Intn(5) == 0 {
			add("declare duplicate", "DECLARE v INT;")
		}
	}
	hasCond := false
	if r.Intn(3) == 0 {
		add("condition", "DECLARE c1 CONDITION FOR SQLSTATE '45001';")
		hasCond = true
		switch r.Intn(6) {
		case 0:
			add("condition duplicate", "DECLARE c1 CONDITION FOR SQLSTATE '45002';")
		case 1:
			add("condition bad sqlstate", "DECLARE c2 CONDITION FOR SQLSTATE '4500';")
		}
	}
	hasCursor := false
	if r.Intn(3) == 0 {
		add("cursor", "DECLARE cur CURSOR FOR SELECT id FROM u;")
		hasCursor = true
		if r.Intn(5) == 0 {
			add("cursor duplicate", "DECLARE cur CURSOR FOR SELECT a FROM u;")
		}
	}
	if r.Intn(3) == 0 {
		add("handler", "DECLARE CONTINUE HANDLER FOR SQLEXCEPTION SET @h = 1;")
		switch r.Intn(6) {
		case 0:
			add("handler duplicate", "DECLARE CONTINUE HANDLER FOR SQLEXCEPTION SET @h = 2;")
		case 1:
			if hasCond {
				add("handler", "DECLARE EXIT HANDLER FOR c1 SET @h = 3;")
			}
		}
	}
	// statements: ordinary ones, and at most one the server may refuse (two now and then,
	// so the report also shows which of two refusals the server names first)
	safe := func() {
		switch r.Intn(7) {
		case 0:
			if vars["v"] {
				add("set", "SET v = v + 1;")
			} else {
				add("set", "SET @x = 1;")
			}
		case 1:
			if vars["v"] {
				add("select into", "SELECT id INTO v FROM u LIMIT 1;")
			}
		case 2:
			add("insert", "INSERT INTO u VALUES (5, 1);")
		case 3:
			add("loop leave", "lbl: LOOP LEAVE lbl; END LOOP;")
		case 4:
			add("signal", "SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'x';")
		case 5:
			add("call", "CALL other();")
		default:
			add("case no else", "CASE @x WHEN 1 THEN SET @y = 1; END CASE;")
		}
	}
	risky := func() {
		switch r.Intn(20) {
		case 0:
			add("select into undeclared", "SELECT id INTO w FROM u LIMIT 1;")
		case 1:
			add("select result set", "SELECT id FROM u;")
		case 2:
			add("update own table", "UPDATE t SET a = 1 WHERE id = 1;")
		case 3:
			add("commit", "COMMIT;")
		case 4:
			add("prepare", "PREPARE s FROM 'SELECT 1';")
		case 5:
			add("flush", "FLUSH TABLES;")
		case 6:
			add("lock tables", "LOCK TABLES u WRITE;")
		case 7:
			add("load data", "LOAD DATA INFILE '/tmp/x' INTO TABLE u;")
		case 8:
			add("alter view", "ALTER VIEW v AS SELECT id, a FROM u WHERE a = 1;")
		case 9:
			add("create table", "CREATE TABLE tmp_x (id INT);")
		case 10:
			add("leave bad label", "lbl2: LOOP LEAVE nope; END LOOP;")
		case 11:
			add("return", "RETURN 1;")
		case 12:
			add("signal errno zero", "SIGNAL SQLSTATE '45000' SET MYSQL_ERRNO = 0;")
		case 13:
			if hasCursor && vars["v"] {
				add("open fetch close", "OPEN cur; FETCH cur INTO v; CLOSE cur;")
			} else {
				add("open undeclared cursor", "OPEN nocur;")
			}
		case 14:
			add("set new", "SET NEW.a = 1;")
		case 15:
			add("set old", "SET OLD.a = 1;")
		case 16:
			add("read old", "SET @o = OLD.a;")
		case 17:
			add("read new", "SET @n = NEW.a;")
		case 18:
			add("set new unknown column", "SET NEW.nosuch = 1;")
		default:
			add("read new unknown column", "SET @o = NEW.nosuch;")
		}
	}
	for k := 0; k < r.Intn(3); k++ {
		safe()
	}
	switch r.Intn(10) {
	case 0, 1:
	case 2:
		risky()
		risky()
	default:
		risky()
	}
	for k := 0; k < r.Intn(2); k++ {
		safe()
	}
	body := "BEGIN\n" + strings.Join(lines, "\n") + "\n"
	switch g.kind {
	case "procedure":
		g.create = "CREATE PROCEDURE " + g.name + "(IN n INT)\n" + body + "END"
	case "function":
		if r.Intn(4) == 0 {
			g.shapes = append(g.shapes, "no return")
			g.create = "CREATE FUNCTION " + g.name + "(n INT) RETURNS INT DETERMINISTIC\n" + body + "END"
		} else {
			g.create = "CREATE FUNCTION " + g.name + "(n INT) RETURNS INT DETERMINISTIC\n" + body + "  RETURN 1;\nEND"
		}
	default:
		g.create = "CREATE TRIGGER " + g.name + " " + timing + " " + event + " ON t FOR EACH ROW\n" + body + "END"
	}
	return g
}

func TestBodyProbe(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	db, err := mysqltest.Start(ctx, bodyProbeBase)
	if errors.Is(err, mysqltest.ErrNoServer) {
		t.Skip("no mysqld on PATH (nix-shell -p mysql84)")
	}
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	r := rand.New(rand.NewSource(*bodySeed))
	counts := map[string]int{}
	hit := map[string]bool{}
	type finding struct{ kind, detail, create string }
	var findings []finding
	seen := map[string]bool{}
	for i := 0; i < *bodyN; i++ {
		g := genRoutine(r, i)
		for _, s := range g.shapes {
			hit[s] = true
		}
		// the server
		_, serr := db.Conn().ExecContext(ctx, g.create)
		code := 0
		var me *driver.MySQLError
		if errors.As(serr, &me) {
			code = int(me.Number)
		} else if serr != nil {
			t.Fatalf("%s: %v", g.create, serr)
		}
		if code == 0 {
			drop := map[string]string{"procedure": "DROP PROCEDURE ", "function": "DROP FUNCTION ", "trigger": "DROP TRIGGER "}[g.kind]
			if _, err := db.Conn().ExecContext(ctx, drop+g.name); err != nil {
				t.Fatalf("drop %s: %v", g.name, err)
			}
			hit["accepted"] = true
		} else {
			hit[fmt.Sprintf("refused %d", code)] = true
		}
		// the analyzer
		s, lerr := schema.Load(bodyProbeBase + g.create + ";\n")
		if lerr != nil {
			t.Fatalf("%s: schema.Load: %v", g.create, lerr)
		}
		var problems []string
		for _, p := range s.Problems {
			if strings.Contains(p.Message, g.name) {
				problems = append(problems, p.Message)
			}
		}
		var aerr error
		switch g.kind {
		case "procedure":
			if rt := s.RoutineOf(schema.Procedure, g.name); rt != nil {
				_, aerr = AnalyzeRoutine(s, rt)
			}
		case "function":
			if rt := s.RoutineOf(schema.Function, g.name); rt != nil {
				_, aerr = AnalyzeRoutine(s, rt)
			}
		default:
			if tg := s.Trigger(g.name); tg != nil {
				_, aerr = AnalyzeTrigger(s, tg)
			}
		}
		acode := errCode(aerr)
		var kind, detail string
		switch {
		case code != 0 && acode == code:
			counts["pass (refused, same code)"]++
		case code != 0 && problemMentions(problems, me.Message):
			counts["pass (refused, loader problem)"]++
		case code != 0 && (createTimeCodes[acode] || len(problems) > 0):
			// both refuse, naming different constructs: the server checks in its own
			// order (grammar-time refusals first), the analyzer in walk order
			counts["pass (refused, other code)"]++
			if counts["pass (refused, other code)"] <= 5 {
				t.Logf("refused with another code: server %v, analyzer %v\n%s", serr, aerr, g.create)
			}
		case code != 0:
			kind, detail = "server refuses, analyzer does not", fmt.Sprintf("server: %v\nanalyzer: %v\nloader problems: %v", serr, aerr, problems)
		case len(problems) > 0:
			kind, detail = "loader problem for a body the server accepts", fmt.Sprintf("loader problems: %v\nanalyzer: %v", problems, aerr)
		case acode != 0 && createTimeCodes[acode]:
			kind, detail = "analyzer refuses what the server accepts", fmt.Sprintf("analyzer: %v", aerr)
		default:
			counts["pass (accepted)"]++
		}
		if kind != "" {
			counts["finding"]++
			key := kind + "\n" + detail
			if !seen[key] {
				seen[key] = true
				findings = append(findings, finding{kind, detail, g.create})
			}
		}
	}
	var report strings.Builder
	fmt.Fprintf(&report, "# body probe (mysql), seed %d, %d bodies\n\n", *bodySeed, *bodyN)
	var keys []string
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&report, "%s: %d\n", k, counts[k])
	}
	for i, f := range findings {
		fmt.Fprintf(&report, "\n## %d. %s\n\n%s\n\n```sql\n%s\n```\n", i+1, f.kind, f.detail, f.create)
	}
	var missed, stale []string
	fmt.Fprintf(&report, "\n## alphabet coverage\n\n")
	for _, e := range bodyAlphabet {
		mark := " "
		if hit[e] {
			mark = "x"
			if _, known := bodyKnownUnreached[e]; known {
				stale = append(stale, e)
			}
		} else if _, known := bodyKnownUnreached[e]; !known {
			missed = append(missed, e)
		}
		fmt.Fprintf(&report, "- [%s] %s\n", mark, e)
	}
	if *bodyReport != "" {
		if err := os.WriteFile(*bodyReport, []byte(report.String()), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("report: %s", *bodyReport)
	} else {
		t.Log(report.String())
	}
	if len(findings) > 0 {
		t.Errorf("%d finding(s), see the report", len(findings))
	}
	if len(missed) > 0 {
		t.Errorf("alphabet entries never reached: %s", strings.Join(missed, ", "))
	}
	if len(stale) > 0 {
		t.Errorf("known-unreached entries that were reached: %s", strings.Join(stale, ", "))
	}
}

// problemMentions: a loader problem carries the server's own message (bodyRefusalProblem).
func problemMentions(problems []string, msg string) bool {
	for _, p := range problems {
		if strings.Contains(p, msg) {
			return true
		}
	}
	return false
}
