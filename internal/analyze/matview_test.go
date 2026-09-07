package analyze

import "testing"

// A materialized view's columns are as nullable as its query's: a snapshot of the query,
// not a table whose columns lost their NOT NULL.
func TestMatViewNullability(t *testing.T) {
	s, err := Load(`
CREATE TABLE b (tenant_id uuid NOT NULL, room_id bigint NOT NULL, slot tstzrange NOT NULL, minutes int NOT NULL, note text);
CREATE MATERIALIZED VIEW mv AS
SELECT b.tenant_id, b.room_id, lower(b.slot)::date AS day, sum(b.minutes) AS booked_minutes, count(*) AS bookings, max(note) AS note
  FROM b GROUP BY b.tenant_id, b.room_id, lower(b.slot)::date;
`)
	if err != nil {
		t.Fatal(err)
	}
	r, err := Analyze(s, "SELECT room_id, day, booked_minutes, bookings, note FROM mv WHERE tenant_id = $1")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"room_id": false, "day": false, "booked_minutes": true, "bookings": false, "note": true}
	for _, c := range r.Columns {
		if c.Nullable != want[c.Name] {
			t.Errorf("%s: nullable=%v, want %v", c.Name, c.Nullable, want[c.Name])
		}
	}
}
