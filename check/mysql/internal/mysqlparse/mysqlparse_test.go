package mysqlparse

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kr9ly/sqlshape/check/mysql/v2/internal/parsegen"
)

func TestParseTree(t *testing.T) {
	sql := "SELECT a, `b` FROM t WHERE c = ? AND d IN (SELECT e FROM u) ORDER BY a LIMIT 10"
	root, err := Parse(sql, 0)
	if err != nil {
		t.Fatal(err)
	}
	if root.Kind.String() != "start_entry" {
		t.Errorf("root kind %s", root.Kind)
	}
	if root.Text(sql) != sql {
		t.Errorf("root span %q", root.Text(sql))
	}
	var leaves []string
	var kinds []string
	var walk func(n *Node)
	walk = func(n *Node) {
		if n.IsLeaf() {
			leaves = append(leaves, n.Kind.String()+"="+n.Text(sql))
			return
		}
		kinds = append(kinds, n.Kind.String())
		for _, c := range n.Children {
			walk(c)
		}
	}
	walk(root)
	got := strings.Join(leaves, " ")
	// unquoted identifiers come back as IDENT_QUOTED: the lexer marks every identifier so under a
	// multi-byte connection charset (utf8mb4), as the server does
	for _, want := range []string{"SELECT_SYM=SELECT", "IDENT_QUOTED=a", "','=,", "IDENT_QUOTED=`b`", "PARAM_MARKER=?", "IN_SYM=IN", "'('=(", "NUM=10", "END_OF_INPUT="} {
		if !strings.Contains(got, want) {
			t.Errorf("leaves lack %q:\n%s", want, got)
		}
	}
	joined := strings.Join(kinds, " ")
	for _, want := range []string{"select_stmt", "query_expression", "where_clause", "order_clause", "limit_clause", "table_subquery"} {
		if !strings.Contains(joined, want) {
			t.Errorf("rules lack %q", want)
		}
	}
}

func TestLeafValues(t *testing.T) {
	sql := "SELECT `a``b`, 'it''s\\n', _utf8mb4'x', 0x1F, 12 FROM t"
	root, err := Parse(sql, 0)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	var walk func(n *Node)
	walk = func(n *Node) {
		if n.IsLeaf() && n.HasValue {
			got[n.Kind.String()+":"+n.Text(sql)] = n.Value
		}
		for _, c := range n.Children {
			walk(c)
		}
	}
	walk(root)
	for k, want := range map[string]string{
		"IDENT_QUOTED:`a``b`":         "a`b",
		"TEXT_STRING:'it''s\\n'":      "it's\n",
		"UNDERSCORE_CHARSET:_utf8mb4": "utf8mb4",
		"HEX_NUM:0x1F":                "1F",
		"NUM:12":                      "12",
	} {
		if got[k] != want {
			t.Errorf("%s: value %q, want %q (all: %v)", k, got[k], want, got)
		}
	}
}

func TestParseError(t *testing.T) {
	_, err := Parse("SELECT 1 +", 0)
	e, ok := err.(*Error)
	if !ok {
		t.Fatalf("want *Error, got %v", err)
	}
	if e.Message != "syntax error" || e.Offset != 10 {
		t.Errorf("got %+v", *e)
	}
}

func TestMode(t *testing.T) {
	// with ANSI_QUOTES a double-quoted string is an identifier
	root, err := Parse(`SELECT "a" FROM t`, ANSIQuotes)
	if err != nil {
		t.Fatal(err)
	}
	if !hasLeaf(root, "IDENT_QUOTED") || hasLeaf(root, "TEXT_STRING") {
		t.Error("ANSI_QUOTES did not turn the string into an identifier")
	}
	root, err = Parse(`SELECT "a" FROM t`, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !hasLeaf(root, "TEXT_STRING") {
		t.Error(`"a" should be a string without ANSI_QUOTES`)
	}
}

func hasLeaf(n *Node, kind string) bool {
	if n.IsLeaf() {
		return n.Kind.String() == kind
	}
	for _, c := range n.Children {
		if hasLeaf(c, kind) {
			return true
		}
	}
	return false
}

func TestKindOf(t *testing.T) {
	k, ok := KindOf("select_stmt")
	if !ok || k.IsTerminal() || k.String() != "select_stmt" {
		t.Error("select_stmt")
	}
	k, ok = KindOf("SELECT_SYM")
	if !ok || !k.IsTerminal() {
		t.Error("SELECT_SYM")
	}
	if _, ok := KindOf("no_such_rule"); ok {
		t.Error("no_such_rule")
	}
}

func TestVersionComment(t *testing.T) {
	root, err := Parse("SELECT /*!80000 SQL_NO_CACHE */ 1 /* c */", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !hasLeaf(root, "SQL_NO_CACHE_SYM") {
		t.Error("the version comment for a supported version should be parsed")
	}
}

// TestCorpus parses mysql-test/t when the MySQL source is checked out, and holds the
// acceptance the spike measured.
func TestCorpus(t *testing.T) {
	home, _ := os.UserHomeDir()
	dir := filepath.Join(home, ".cache", "sqlshape", "mysql-server", "mysql-test", "t")
	if _, err := os.Stat(dir); err != nil {
		t.Skip("no MySQL source at", dir)
	}
	stmts, _, err := parsegen.SplitTestDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var ok, fail, expectedRejected, expectedAccepted int
	for _, s := range stmts {
		_, err := Parse(s.SQL, 0)
		switch {
		case s.Expect == "ER_PARSE_ERROR" || s.Expect == "1064":
			if err != nil {
				expectedRejected++
			} else {
				expectedAccepted++
			}
		case s.Expect != "":
		case err == nil:
			ok++
		default:
			fail++
			if _, isParse := err.(*Error); !isParse {
				t.Fatalf("%s:%d: %v", s.File, s.Line, err)
			}
		}
	}
	rate := float64(ok) / float64(ok+fail)
	t.Logf("accepted %d of %d statements expecting no error (%.2f%%); ER_PARSE_ERROR: %d rejected, %d accepted", ok, ok+fail, rate*100, expectedRejected, expectedAccepted)
	if rate < 0.998 {
		t.Errorf("acceptance fell to %.2f%%", rate*100)
	}
}

func TestShape(t *testing.T) {
	sql := "SELECT a FROM t WHERE b = 1"
	root, err := Parse(sql, 0)
	if err != nil {
		t.Fatal(err)
	}
	var classes []string
	var walk func(n *Node)
	walk = func(n *Node) {
		if n.IsLeaf() {
			return
		}
		if s := n.Shape(); s.Kind == ActNew {
			classes = append(classes, s.Class)
		}
		for _, c := range n.Children {
			walk(c)
		}
	}
	walk(root)
	joined := strings.Join(classes, " ")
	for _, want := range []string{"PT_select_stmt", "PT_query_specification", "PT_table_factor_table_ident", "PTI_comp_op", "PTI_where", "Item_int"} {
		if !strings.Contains(joined, want) {
			t.Errorf("shapes lack %s in %s", want, joined)
		}
	}
	// the alternative index selects the shape: `where_clause` is the second alternative of
	// opt_where_clause and has no action (bison's default $$ = $1)
	k, _ := KindOf("opt_where_clause")
	var where *Node
	var find func(n *Node)
	find = func(n *Node) {
		if n.Kind == k {
			where = n
		}
		for _, c := range n.Children {
			find(c)
		}
	}
	find(root)
	if where == nil || where.Alt != 1 || where.Shape().Kind != ActDefault {
		t.Errorf("opt_where_clause: %+v shape %+v", where, where.Shape())
	}
}

func TestSplit(t *testing.T) {
	script := "-- header\nCREATE TABLE t (a INT); # c\n/* block ; */ INSERT INTO t VALUES ('a;b', \"c;d\", `e;f`);\n\n-- sqlshape: unfiltered t\nSELECT 1\n"
	got := Split(script)
	// leading comments stay with their statement: `-- sqlshape:` directives live there
	want := []string{"-- header\nCREATE TABLE t (a INT)", "# c\n/* block ; */ INSERT INTO t VALUES ('a;b', \"c;d\", `e;f`)", "-- sqlshape: unfiltered t\nSELECT 1"}
	if len(got) != len(want) {
		t.Fatalf("got %d statements: %+v", len(got), got)
	}
	for i := range want {
		if got[i].SQL != want[i] || script[got[i].Offset:got[i].Offset+len(got[i].SQL)] != got[i].SQL {
			t.Errorf("%d: %q at %d", i, got[i].SQL, got[i].Offset)
		}
	}
}

func checkOffsets(t *testing.T, script string, stmts []Statement) {
	t.Helper()
	for i, s := range stmts {
		if s.Offset < 0 || s.Offset+len(s.SQL) > len(script) || script[s.Offset:s.Offset+len(s.SQL)] != s.SQL {
			t.Errorf("%d: offset %d does not point at %q in script", i, s.Offset, s.SQL)
		}
	}
}

// TestSplitCompoundTrigger checks that a CREATE TRIGGER body with internal ';' (BEGIN ...
// END, IF ... END IF) is cut as one statement, without a DELIMITER command.
func TestSplitCompoundTrigger(t *testing.T) {
	script := "CREATE TABLE t (a INT);\n" +
		"CREATE TRIGGER trg BEFORE INSERT ON t FOR EACH ROW\n" +
		"BEGIN\n" +
		"  DECLARE v INT DEFAULT 0;\n" +
		"  IF NEW.a > 0 THEN\n" +
		"    SET v = 1;\n" +
		"  END IF;\n" +
		"END;\n" +
		"SELECT 1;\n"
	got := SplitMode(script, 0)
	checkOffsets(t, script, got)
	if len(got) != 3 {
		t.Fatalf("got %d statements: %+v", len(got), got)
	}
	if !strings.HasPrefix(got[1].SQL, "CREATE TRIGGER") || !strings.HasSuffix(got[1].SQL, "END") {
		t.Errorf("trigger statement: %q", got[1].SQL)
	}
	if got[2].SQL != "SELECT 1" {
		t.Errorf("trailing statement: %q", got[2].SQL)
	}
}

// TestSplitDelimiter checks that a `DELIMITER` command switches the cut point and is itself
// dropped, and that `DELIMITER ;` restores the default.
func TestSplitDelimiter(t *testing.T) {
	script := "DELIMITER $$\n" +
		"CREATE FUNCTION f() RETURNS INT\n" +
		"BEGIN\n" +
		"  RETURN 1;\n" +
		"END$$\n" +
		"DELIMITER ;\n" +
		"SELECT 2;\n"
	got := SplitMode(script, 0)
	checkOffsets(t, script, got)
	if len(got) != 2 {
		t.Fatalf("got %d statements: %+v", len(got), got)
	}
	if !strings.HasPrefix(got[0].SQL, "CREATE FUNCTION") || !strings.HasSuffix(got[0].SQL, "END") {
		t.Errorf("function statement: %q", got[0].SQL)
	}
	if got[1].SQL != "SELECT 2" {
		t.Errorf("trailing statement: %q", got[1].SQL)
	}
}

// TestSplitCompoundNoBeginEnd checks that a simple (non-BEGIN/END) trigger body, which ends
// at its first ';', does not need the retry.
func TestSplitCompoundNoBeginEnd(t *testing.T) {
	script := "CREATE TRIGGER trg BEFORE INSERT ON t FOR EACH ROW SET NEW.a = 1;\nSELECT 1;\n"
	got := SplitMode(script, 0)
	checkOffsets(t, script, got)
	want := []string{"CREATE TRIGGER trg BEFORE INSERT ON t FOR EACH ROW SET NEW.a = 1", "SELECT 1"}
	for i := range want {
		if got[i].SQL != want[i] {
			t.Errorf("%d: got %q want %q", i, got[i].SQL, want[i])
		}
	}
}

// TestSplitCompoundTruncatedNoSemicolon: a compound CREATE whose body never reaches a ';'
// at all (truncated mid-statement) keeps the whole remainder as one statement -- SplitMode's
// own "ran off the end of the script" branch, the found-false side of its retry loop.
func TestSplitCompoundTruncatedNoSemicolon(t *testing.T) {
	script := "CREATE TRIGGER trg BEFORE INSERT ON t FOR EACH ROW\nBEGIN\n  SET v = 1\n"
	got := SplitMode(script, 0)
	checkOffsets(t, script, got)
	if len(got) != 1 {
		t.Fatalf("got %d statements: %+v", len(got), got)
	}
	if !strings.HasPrefix(got[0].SQL, "CREATE TRIGGER") {
		t.Errorf("got %q", got[0].SQL)
	}
}

// TestSplitCompoundOneSemicolonNoEnd: a compound CREATE whose body has exactly one ';' (an
// incomplete BEGIN with no END, and no further ';' to retry with) exercises the "extending
// further also finds nothing" branch (as opposed to
// TestSplitCompoundTruncatedNoSemicolon's own immediate found-false).
func TestSplitCompoundOneSemicolonNoEnd(t *testing.T) {
	script := "CREATE TRIGGER trg BEFORE INSERT ON t FOR EACH ROW\nBEGIN\n  SET v = 1;\n"
	got := SplitMode(script, 0)
	checkOffsets(t, script, got)
	if len(got) != 1 {
		t.Fatalf("got %d statements: %+v", len(got), got)
	}
	if !strings.HasPrefix(got[0].SQL, "CREATE TRIGGER") {
		t.Errorf("got %q", got[0].SQL)
	}
}

// TestFindDelimiter_EscapedQuote covers findDelimiter's own backslash-escape handling
// inside a quoted string (a ';' right after the escaped quote must stay inside the string).
func TestFindDelimiter_EscapedQuote(t *testing.T) {
	script := `SELECT 'a\'; b';SELECT 2;`
	end, next, found := findDelimiter(script, 0, ";")
	if !found {
		t.Fatal("want found")
	}
	if script[:end] != `SELECT 'a\'; b'` {
		t.Errorf("got %q", script[:end])
	}
	if script[next:] != "SELECT 2;" {
		t.Errorf("got %q", script[next:])
	}
}

// TestFindDelimiter_UnterminatedBlockComment covers findDelimiter's own unterminated `/*`
// handling: the comment runs to the end of the script, so no ';' after it is ever found.
func TestFindDelimiter_UnterminatedBlockComment(t *testing.T) {
	script := "SELECT 1; /* unterminated"
	end, next, found := findDelimiter(script, 10, ";")
	if found {
		t.Fatalf("want not found, got end=%d next=%d", end, next)
	}
	if end != len(script) || next != len(script) {
		t.Errorf("got end=%d next=%d, want both %d", end, next, len(script))
	}
}

// TestMatchDelimiterLine_NoSpaceAfterKeyword: "DELIMITER" with nothing (or no space)
// following is not a DELIMITER command.
func TestMatchDelimiterLine_NoSpaceAfterKeyword(t *testing.T) {
	if _, _, ok := matchDelimiterLine("DELIMITER", 0); ok {
		t.Error("want not ok (keyword alone, no token)")
	}
	if _, _, ok := matchDelimiterLine("DELIMITERX $$\n", 0); ok {
		t.Error("want not ok (no space after the keyword)")
	}
}

// TestMatchDelimiterLine_EmptyToken: "DELIMITER" followed only by trailing space (no token)
// is not a DELIMITER command either.
func TestMatchDelimiterLine_EmptyToken(t *testing.T) {
	if _, _, ok := matchDelimiterLine("DELIMITER   \n", 0); ok {
		t.Error("want not ok (no token after the spaces)")
	}
}

// TestStripLeadingComments_LineCommentNoNewline: a `-- ` comment that runs to the end of
// the text with no trailing newline strips to empty.
func TestStripLeadingComments_LineCommentNoNewline(t *testing.T) {
	if got := stripLeadingComments("-- just a comment, no newline"); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

// TestStripLeadingComments_UnterminatedBlockComment: an unterminated `/*` also strips to
// empty (nothing follows it that could be the statement proper).
func TestStripLeadingComments_UnterminatedBlockComment(t *testing.T) {
	if got := stripLeadingComments("/* unterminated"); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}
