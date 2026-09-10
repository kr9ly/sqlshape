package placeholder

import (
	"strings"
	"testing"
)

func TestPlaceholders(t *testing.T) {
	text, pm := Rewrite("SELECT a$1, 'x$2', `c$3`, \"d$4\" FROM t WHERE x = $1 AND y = $12 AND z=$2")
	want := "SELECT a$1, 'x$2', `c$3`, \"d$4\" FROM t WHERE x = ? AND y = ? AND z=?"
	if text != want {
		t.Fatalf("got  %q\nwant %q", text, want)
	}
	if pm.Count() != 12 || len(pm.marks) != 3 {
		t.Fatalf("count %d marks %d", pm.Count(), len(pm.marks))
	}
	// the `?` for $12 is at index 56 in text; the original `$12` starts at 57 (one `$1` before it grew by 1)
	if n := pm.Number(strings.Index(text, "y = ?") + 4); n != 12 {
		t.Errorf("number = %d, want 12", n)
	}
	off := strings.Index(text, "z=?") + 2
	if back := pm.Back(off); back != off+1+2 {
		t.Errorf("back(%d) = %d, want %d", off, back, off+3)
	}
}
