package dump

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// TestCanonicalEvents: events read back through SHOW CREATE EVENT, DEFINER dropped, a STARTS
// the source omitted or computed filled in by the server as a literal time (measured) and
// marked unfixed by pinEvents, a literal one kept fixed; the canonical text is a fixpoint.
func TestCanonicalEvents(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	src := `-- sqlshape: mysql 8.4
CREATE TABLE t (id INT NOT NULL PRIMARY KEY, v INT NOT NULL);
CREATE EVENT floating ON SCHEDULE EVERY 1 DAY DO DELETE FROM t WHERE id < 0;
CREATE EVENT computed ON SCHEDULE EVERY 1 HOUR STARTS CURRENT_TIMESTAMP + INTERVAL 1 DAY DO DELETE FROM t WHERE id < 0;
CREATE EVENT fixed ON SCHEDULE EVERY 1 HOUR STARTS '2030-01-01 00:00:00' ENDS '2031-01-01 00:00:00' ON COMPLETION PRESERVE DISABLE COMMENT 'c' DO BEGIN DELETE FROM t WHERE id < 0; UPDATE t SET v = v + 1 WHERE id = 1; END;
CREATE EVENT once ON SCHEDULE AT '2030-01-01 00:00:00' DO INSERT INTO t VALUES (1, 1);
`
	s, text, err := Local{}.Canonical(ctx, src)
	if errors.Is(err, ErrNoServer) {
		t.Skip("no mysqld on PATH (nix-shell -p mysql84)")
	}
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Problems) > 0 {
		t.Fatalf("problems: %v", s.Problems)
	}
	if strings.Contains(text, "DEFINER=") {
		t.Errorf("definer left in:\n%s", text)
	}
	if len(s.Events) != 4 {
		t.Fatalf("events read back: %d\n%s", len(s.Events), text)
	}
	fl := s.Event("floating")
	if fl.Starts == "" || fl.StartsLiteral {
		t.Errorf("floating: the server fills STARTS in, the source did not fix it: %+v", fl)
	}
	if co := s.Event("computed"); co.Starts == "" || co.StartsLiteral {
		t.Errorf("computed: %+v", co)
	}
	fx := s.Event("fixed")
	if fx.Starts != "'2030-01-01 00:00:00'" || !fx.StartsLiteral || fx.Ends != "'2031-01-01 00:00:00'" || !fx.EndsLiteral || fx.Completion != "PRESERVE" || fx.Status != "DISABLE" || fx.Comment != "c" {
		t.Errorf("fixed: %+v", fx)
	}
	if on := s.Event("once"); on.At != "'2030-01-01 00:00:00'" || !on.AtLiteral {
		t.Errorf("once: %+v", on)
	}
	_, text2, err := Local{}.Canonical(ctx, text)
	if err != nil {
		t.Fatal(err)
	}
	if text2 != text {
		t.Errorf("not a fixpoint:\n%s\n---\n%s", text, text2)
	}
}
