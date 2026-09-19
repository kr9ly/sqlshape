package migrate

import (
	"strings"
	"testing"
)

// TestPlanEvents: an added, a changed and a dropped event plan as CREATE EVENT / DROP EVENT
// (a changed one both), and an event whose STARTS the schema leaves to the server plans
// nothing even though the two canonical forms were created at different times; the DDL
// reaches the target (plan's own Verify).
func TestPlanEvents(t *testing.T) {
	ctx := start(t)
	baseSQL := example(t)
	from := mustCanonical(t, ctx, baseSQL+`
CREATE EVENT floating ON SCHEDULE EVERY 1 DAY DO DELETE FROM order_audit WHERE created_at < NOW() - INTERVAL 90 DAY;
CREATE EVENT changed ON SCHEDULE EVERY 1 DAY DO DELETE FROM order_audit WHERE created_at < NOW() - INTERVAL 30 DAY;
CREATE EVENT gone ON SCHEDULE EVERY 1 HOUR DO DELETE FROM order_audit WHERE id < 0;
`)
	to := mustCanonical(t, ctx, baseSQL+`
CREATE EVENT floating ON SCHEDULE EVERY 1 DAY DO DELETE FROM order_audit WHERE created_at < NOW() - INTERVAL 90 DAY;
CREATE EVENT changed ON SCHEDULE EVERY 1 DAY DO DELETE FROM order_audit WHERE created_at < NOW() - INTERVAL 60 DAY;
CREATE EVENT added ON SCHEDULE EVERY 1 HOUR STARTS '2030-01-01 00:00:00' ON COMPLETION PRESERVE DISABLE DO DELETE FROM order_audit WHERE id < 0;
`)
	if from.s.Event("floating").Starts == to.s.Event("floating").Starts {
		t.Log("the two canonical forms were created within the same second; the floating STARTS is not exercised this run")
	}
	ddl := plan(t, ctx, "events", from, to)
	joined := strings.Join(ddl, "\n")
	for _, w := range []string{"DROP EVENT `gone`", "DROP EVENT `changed`", "INTERVAL 60 DAY", "CREATE EVENT `added` ON SCHEDULE EVERY 1 HOUR STARTS '2030-01-01 00:00:00' ON COMPLETION PRESERVE DISABLE"} {
		if !strings.Contains(joined, w) {
			t.Errorf("DDL lacks %q:\n%s", w, joined)
		}
	}
	if strings.Contains(joined, "`floating`") {
		t.Errorf("an event whose STARTS the schema leaves to the server must not be replanned:\n%s", joined)
	}
	if strings.Count(joined, "CREATE EVENT") != 2 {
		t.Errorf("want exactly the changed and the added event created:\n%s", joined)
	}
	back := plan(t, ctx, "events back", to, from)
	if j := strings.Join(back, "\n"); !strings.Contains(j, "DROP EVENT `added`") || !strings.Contains(j, "INTERVAL 30 DAY") {
		t.Errorf("way back:\n%s", j)
	}
}
