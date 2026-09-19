package vet

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
	"unicode/utf8"
)

func TestRuneOfHex(t *testing.T) {
	if r := runeOfHex("0041"); r != 'A' {
		t.Errorf("runeOfHex(0041) = %q, want 'A'", r)
	}
	if r := runeOfHex("00e9"); r != 0xe9 {
		t.Errorf("runeOfHex(00e9) = %U, want U+00E9", r)
	}
	if r := runeOfHex("0001f600"); r != 0x1f600 {
		t.Errorf("runeOfHex(0001f600) = %U, want U+1F600", r)
	}
	// runeOfHex is only ever called on hex digits go/parser has already validated as part of
	// a well-formed \u / \U escape; feed it garbage directly to reach the error path.
	if r := runeOfHex("zzzz"); r != utf8.RuneError {
		t.Errorf("runeOfHex(zzzz) = %v, want utf8.RuneError", r)
	}
}

// TestLitPosEscapes checks litPos's escape-aware walk (vet.go ~354-385) offset by offset
// against a literal containing a \x, a \u (2-byte rune) and a \U (4-byte rune) escape.
// analysistest can only assert the diagnostic's line (the // want comment format), and an
// interpreted string literal never spans more than one source line, so a wrong column
// inside it would silently pass a testdata-based test; this checks the exact token.Pos.
func TestLitPosEscapes(t *testing.T) {
	const src = `package p

const S = "AB\x41\u00e9\U0001F600Z"
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "p.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	var lit *ast.BasicLit
	ast.Inspect(f, func(n ast.Node) bool {
		if bl, ok := n.(*ast.BasicLit); ok && bl.Kind == token.STRING {
			lit = bl
		}
		return true
	})
	if lit == nil {
		t.Fatal("string literal not found")
	}
	if lit.Value != `"AB\x41\u00e9\U0001F600Z"` {
		t.Fatalf("unexpected literal text: %s", lit.Value)
	}
	base := fset.Position(lit.Pos()).Column // column of the opening quote
	// decoded template: "AB" (2) + \x41->'A' (1) + é->'é' (2 UTF-8 bytes) +
	// \U0001F600->emoji (4 UTF-8 bytes) + "Z" (1) = 10 decoded bytes total.
	cases := []struct {
		off  int // offset into the decoded template
		want int // byte offset into the literal's source text (0 = the opening quote)
	}{
		{0, 1},   // 'A'
		{1, 2},   // 'B'
		{2, 3},   // start of \x41
		{3, 7},   // right after \x41 (decoded 'A'): start of é
		{4, 13},  // the 2nd byte of é: no offset lands mid-rune, so this snaps to after é
		{5, 13},  // start of \U0001F600
		{6, 23},  // inside the emoji's 4 bytes: snaps to after \U0001F600
		{8, 23},  // still inside the emoji
		{9, 23},  // 'Z'
		{10, 24}, // past the end: falls back to just before the closing quote
	}
	for _, c := range cases {
		got := fset.Position(litPos(lit, false, c.off)).Column - base
		if got != c.want {
			t.Errorf("litPos(off=%d) column = %d (relative), want %d", c.off, got, c.want)
		}
	}
}

// TestLitPosOctalEscape covers the \NNN (octal) branch, the one escape form the above test
// does not exercise.
func TestLitPosOctalEscape(t *testing.T) {
	const src = `package p

const S = "A\101B"
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "p.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	var lit *ast.BasicLit
	ast.Inspect(f, func(n ast.Node) bool {
		if bl, ok := n.(*ast.BasicLit); ok && bl.Kind == token.STRING {
			lit = bl
		}
		return true
	})
	if lit == nil {
		t.Fatal("string literal not found")
	}
	// decoded template: "A" (1) + \101->'A' (1) + "B" (1) = 3 decoded bytes.
	base := fset.Position(lit.Pos()).Column
	cases := []struct{ off, want int }{
		{0, 1}, // 'A'
		{1, 2}, // start of \101
		{2, 6}, // 'B', right after the 4-byte octal escape
		{3, 7}, // past the end
	}
	for _, c := range cases {
		got := fset.Position(litPos(lit, false, c.off)).Column - base
		if got != c.want {
			t.Errorf("litPos(off=%d) column = %d (relative), want %d", c.off, got, c.want)
		}
	}
}
