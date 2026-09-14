package analyze

import (
	"testing"

	"github.com/kr9ly/sqlshape/check/mysql/v2/internal/schema"
)

// TestAnalyzeEvent: an event's body is walked like a routine's -- its statements' facts are
// recorded, a table it names that the schema does not have is an Error (the server accepts
// it at CREATE time and fails at every run, measured), and a RETURN is 1313 (the server's own
// CREATE-time refusal, measured).
func TestAnalyzeEvent(t *testing.T) {
	s, err := schema.Load(`-- sqlshape: mysql 8.4
CREATE TABLE t (id INT NOT NULL PRIMARY KEY, v INT NOT NULL);
CREATE EVENT ok ON SCHEDULE EVERY 1 DAY DO BEGIN DELETE FROM t WHERE id < 0; UPDATE t SET v = v + 1 WHERE id = 1; END;
CREATE EVENT no_table ON SCHEDULE EVERY 1 DAY DO DELETE FROM nope WHERE id < 0;
CREATE EVENT returns ON SCHEDULE EVERY 1 DAY DO BEGIN DECLARE n INT; SELECT COUNT(*) INTO n FROM t; RETURN n; END;
`)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Problems) > 0 {
		t.Fatalf("problems: %v", s.Problems)
	}
	br, err := AnalyzeEvent(s, s.Event("ok"))
	if err != nil {
		t.Fatal(err)
	}
	if len(br.Statements) != 2 || br.Statements[0].Facts == nil || br.Statements[1].Facts == nil {
		t.Errorf("ok: want the facts of both statements, got %+v", br.Statements)
	}
	if _, err := AnalyzeEvent(s, s.Event("no_table")); errCode(err) != 1146 {
		t.Errorf("no_table: got %v, want 1146", err)
	}
	if _, err := AnalyzeEvent(s, s.Event("returns")); errCode(err) != 1313 {
		t.Errorf("returns: got %v, want 1313", err)
	}
}
