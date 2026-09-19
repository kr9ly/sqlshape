package parsegen

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStripGrammarSmall(t *testing.T) {
	src := `%{
#include "server.h"
%}
%start stmt
%parse-param { class THD *YYTHD }
%define api.pure
%token<lexer.keyword> SELECT_SYM 258
%token NUM 259
%type <node> stmt expr
%left '+'
%%
stmt:
          SELECT_SYM expr ';' { $$= NEW_PTN PT_select($2); }
        | %empty { }
        ;
expr:
          NUM { $$= $1; }
        | expr '+' expr %prec '+' { $$= NEW_PTN Item_plus(@$, $1, $3); }
        | NUM { mid(); } NUM { $$= nullptr; }

trailing:
          NUM
%%
epilogue();
`
	g, err := StripGrammar(src)
	if err != nil {
		t.Fatal(err)
	}
	if g.Rules != 3 || g.Alternatives != 6 || g.MidRuleActions != 1 || g.Start != "stmt" {
		t.Fatalf("counts: %+v", g)
	}
	// kinds: the rules in definition order, then the terminals in order of first use
	if got := strings.Join(g.Kinds, " "); got != "stmt expr trailing SELECT_SYM ';' NUM '+'" {
		t.Errorf("kinds %q", got)
	}
	for _, want := range []string{
		`%token SELECT_SYM 258`,
		"\n%%\n",
		`  SELECT_SYM expr ';' { $$.node = mk(YYTHD, 0 /* stmt */, 0, 3, L(YYTHD, 3, &@1), $2.node, L(YYTHD, 4, &@3)); *out = $$.node; }`,
		`| %empty { $$.node = mk(YYTHD, 0 /* stmt */, 1, 0); *out = $$.node; }`,
		`| expr '+' expr %prec '+' { $$.node = mk(YYTHD, 1 /* expr */, 1, 3, $1.node, L(YYTHD, 6, &@2), $3.node); }`,
		`| NUM {} NUM { $$.node = mk(YYTHD, 1 /* expr */, 2, 2, L(YYTHD, 5, &@1), L(YYTHD, 5, &@3)); }`,
		"trailing:\n  NUM { $$.node = mk(YYTHD, 2 /* trailing */, 0, 1, L(YYTHD, 5, &@1)); }\n;",
	} {
		if !strings.Contains(g.Text, want) {
			t.Errorf("missing %q in\n%s", want, g.Text)
		}
	}
	for _, gone := range []string{"server.h", "%type", "NEW_PTN", "epilogue", "%parse-param { class THD *YYTHD }\n", "api.pure  "} {
		if strings.Contains(strings.TrimPrefix(g.Text, grammarPrologue), gone) {
			t.Errorf("%q should have been stripped", gone)
		}
	}
}

// The alternative index the generated actions record must be the one ReadActions assigns.
func TestAlternativeIndexAgree(t *testing.T) {
	src := "%start s\n%token A B\n%%\ns: A { $$= $1; } | B x { $$= NEW_PTN PT_x(@$, $2); } | %empty ;\nx: A\n | B { $$= nullptr; }\n%%\n"
	g, err := StripGrammar(src)
	if err != nil {
		t.Fatal(err)
	}
	alts, err := ReadActions(src)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"s/0/pass", "s/1/new", "s/2/default", "x/0/default", "x/1/empty"}
	var got []string
	for _, a := range alts {
		got = append(got, fmt.Sprintf("%s/%d/%s", a.Rule, a.Index, a.Kind))
	}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("alts %v", got)
	}
	for _, m := range []string{"mk(YYTHD, 0 /* s */, 0, 1,", "mk(YYTHD, 0 /* s */, 1, 2,", "mk(YYTHD, 0 /* s */, 2, 0)", "mk(YYTHD, 1 /* x */, 0, 1,", "mk(YYTHD, 1 /* x */, 1, 1,"} {
		if !strings.Contains(g.Text, m) {
			t.Errorf("grammar lacks %q", m)
		}
	}
	if g.Alternatives != len(alts) {
		t.Errorf("StripGrammar saw %d alternatives, ReadActions %d", g.Alternatives, len(alts))
	}
}

func TestSplitTest(t *testing.T) {
	src := strings.Join([]string{
		"# comment",
		"--disable_warnings",
		"DROP TABLE IF EXISTS t1; # trailing comment",
		"--error ER_PARSE_ERROR",
		"SELECT 1 +;",
		"let $x = 3;",
		"eval SELECT $x;",
		"SELECT 'a;b', \"c;d\", `e;f`; SELECT 2;",
		"DELIMITER //;",
		"CREATE PROCEDURE p() BEGIN SELECT 1; END//",
		"DELIMITER ;//",
		"--error 1064",
		"connect (con1,localhost,root,,);",
		"SELECT 3;",
		"perl;",
		"print 'x;';",
		"EOF",
		"SELECT /* c ; d */ 4",
		"  ;",
	}, "\n")
	stmts, total := splitTest("a.test", src)
	if total != 7 {
		t.Fatalf("total=%d", total)
	}
	got := make([]string, len(stmts))
	for i, s := range stmts {
		got[i] = s.Header() + " " + strings.ReplaceAll(s.SQL, "\n", " ")
	}
	want := []string{
		"/* a.test:3 */ DROP TABLE IF EXISTS t1",
		"/* a.test:5 expect=ER_PARSE_ERROR */ SELECT 1 +",
		"/* a.test:8 */ SELECT 'a;b', \"c;d\", `e;f`",
		"/* a.test:8 */ SELECT 2",
		"/* a.test:10 */ CREATE PROCEDURE p() BEGIN SELECT 1; END",
		"/* a.test:14 */ SELECT 3",
		"/* a.test:18 */ SELECT /* c ; d */ 4",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("got\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestCutLexerAnchors(t *testing.T) {
	_, err := CutLexer("nothing here\n", "nothing here\n")
	if err == nil || !strings.Contains(err.Error(), "anchor") {
		t.Fatalf("expected an anchor error, got %v", err)
	}
}

// TestGenerateFromServerSource needs the MySQL source checkout the spike used.
func TestGenerateFromServerSource(t *testing.T) {
	home, _ := os.UserHomeDir()
	src := filepath.Join(home, ".cache", "sqlshape", "mysql-server")
	if _, err := os.Stat(filepath.Join(src, "sql", "sql_yacc.yy")); err != nil {
		t.Skip("no MySQL source at", src)
	}
	b := &Build{Src: src, Out: t.TempDir()}
	g, err := b.Generate()
	if err != nil {
		t.Fatal(err)
	}
	v, _ := ReadVersion(src)
	if v.String() == "8.4.6" && (g.Rules != 945 || g.Alternatives != 3125 || g.MidRuleActions != 67 || len(g.Kinds) != 1721) {
		t.Errorf("8.4.6 counts changed: %+v", g)
	}
	for _, f := range []string{"grammar.y", "kinds.h", "lexer.cc", "shim/sql/sql_lex.h", "shim/sql/thd_shim.h", "main.cc", "parse.cc", "cst.cc"} {
		if _, err := os.Stat(filepath.Join(b.Out, f)); err != nil {
			t.Error(err)
		}
	}
	lex, _ := os.ReadFile(filepath.Join(b.Out, "lexer.cc"))
	for _, want := range []string{"static int find_keyword(", "static int lex_one_token(", "int my_sql_parser_lex(", "void Lex_input_stream::reset("} {
		if !strings.Contains(string(lex), want) {
			t.Errorf("lexer.cc lacks %s", want)
		}
	}
	if strings.Contains(string(lex), "_parser_state::") {
		t.Error("lexer.cc still carries the *_parser_state constructors")
	}
}
