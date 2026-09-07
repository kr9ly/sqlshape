package everything

import (
	"context"
	"net/netip"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/kr9ly/sqlshape"
	"github.com/kr9ly/sqlshape/pgtest"
)

func TestEverything(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	schema, err := pgtest.ReadSchema("schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	pg, err := pgtest.Start(ctx, schema)
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close()
	if err := pg.Verify(ctx, TenantBySlug, Members, DeletedMembers, Rooms, BookingByID, SearchBookings, Schedule,
		UtilizationOf, BookedMinutes, CreateTenant, AddMember, RemoveMember, ImportRooms, Book, TagBooking, CancelBooking, EventCounts); err != nil {
		t.Fatal(err)
	}
	pool, err := Open(ctx, pg.ConnString())
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	team, trial := Team, Trial
	tid, err := CreateTenant.Get(ctx, pool, struct {
		Slug string
		Plan *Plan
	}{"acme", &team})
	if err != nil {
		t.Fatal(err)
	}
	tenant := Tenanted{TenantID: *tid}
	if _, err := CreateTenant.Get(ctx, pool, struct {
		Slug string
		Plan *Plan
	}{"ACME", &trial}); !sqlshape.Violates(err, "tenants_slug_key") {
		t.Fatalf("citext unique: %v", err) // slugs compare case-insensitively
	}
	tn, err := TenantBySlug.Get(ctx, pool, struct{ Slug string }{"Acme"})
	if err != nil || tn.Plan != Team || tn.Settings == nil {
		t.Fatalf("tenant: %v %+v", err, tn)
	}

	ip := netip.MustParseAddr("203.0.113.7")
	ann, err := AddMember.Get(ctx, pool, struct {
		Tenanted
		Email string
		Name  string
		IP    *netip.Addr
	}{tenant, "ann@acme.example", "Ann", &ip})
	if err != nil {
		t.Fatal(err)
	}
	bob, err := AddMember.Get(ctx, pool, struct {
		Tenanted
		Email string
		Name  string
		IP    *netip.Addr
	}{tenant, "bob@acme.example", "Bob", nil})
	if err != nil {
		t.Fatal(err)
	}

	n, err := ImportRooms.Get(ctx, pool, struct {
		Tenanted
		Rooms []RoomIn
	}{tenant, []RoomIn{{"Small", 4, Money{"20.00", "EUR"}}, {"Large", 12, Money{"55.00", "EUR"}}}})
	if err != nil || *n != 2 {
		t.Fatalf("import rooms: %v %v", err, n)
	}
	rooms, err := Rooms.Collect(ctx, pool, tenant)
	if err != nil || len(rooms) != 2 || rooms[0].Name != "Large" || rooms[0].Hourly.String() != "55.00 EUR" || rooms[0].TenantID != *tid {
		t.Fatalf("rooms: %v %+v", err, rooms)
	}
	small, large := rooms[1].ID, rooms[0].ID

	day := time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC)
	at := func(h int) time.Time { return day.Add(time.Duration(h) * time.Hour) }
	b1, err := Reserve(ctx, pool, tenant, small, *ann, at(9), at(10), []string{"ann", "bob"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Reserve(ctx, pool, tenant, small, *bob, at(9), at(11), []string{"bob"}); err == nil || err.Error() != "room 1 is taken between 9:00AM and 11:00AM" {
		t.Fatalf("slot taken: %v", err)
	}
	if _, err := Reserve(ctx, pool, tenant, small, *bob, at(11), at(12), []string{"a", "b", "c", "d", "e"}); err == nil || err.Error() != "room 1 cannot seat 5 people" {
		t.Fatalf("over capacity: %v", err)
	}
	if _, err := Reserve(ctx, pool, tenant, large, *bob, at(9), at(12), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := TagBooking.Exec(ctx, pool, struct {
		Tenanted
		BookingID BookingID
		Tags      map[string]string
	}{tenant, b1, map[string]string{"team": "platform"}}); err != nil {
		t.Fatal(err)
	}

	bk, err := BookingByID.Get(ctx, pool, struct {
		Tenanted
		ID BookingID
	}{tenant, b1})
	if err != nil || bk.Minutes != 60 || len(bk.Attendees) != 2 || bk.Tags["team"] == nil || *bk.Tags["team"] != "platform" || !bk.Slot.Upper.Equal(at(10)) || bk.BookedBy != "ann@acme.example" {
		t.Fatalf("booking: %v %+v", err, bk)
	}

	d, err := LoadDashboard(ctx, pool, tenant)
	if err != nil || len(d.Rooms) != 2 || len(d.Members) != 2 || len(d.Schedule) != 2 {
		t.Fatalf("dashboard: %v %+v", err, d)
	}
	for _, s := range d.Schedule {
		if len(s.Bookings) != 1 || !s.Bookings[0].Slot.Lower.Equal(at(9)) {
			t.Errorf("schedule of %s: %+v", s.Name, s.Bookings)
		}
	}

	min := Minutes(90)
	found, err := Search(ctx, pool, SearchParams{Tenanted: tenant, MinMinutes: &min, From: &day})
	if err != nil || len(found) != 1 || found[0].RoomID != large {
		t.Fatalf("search: %v %+v", err, found)
	}
	tag := "team"
	found, err = Search(ctx, pool, SearchParams{Tenanted: tenant, Tag: &tag, Limit: 5})
	if err != nil || len(found) != 1 || found[0].ID != b1 {
		t.Fatalf("search by tag: %v %+v", err, found)
	}

	if err := UtilizationView.Refresh(ctx, pool); err != nil {
		t.Fatal(err)
	}
	u, err := UtilizationOf.Collect(ctx, pool, struct {
		Tenanted
		Since pgtype.Date
	}{tenant, pgtype.Date{Time: day, Valid: true}})
	if err != nil || len(u) != 2 || u[0].BookedMinutes+u[1].BookedMinutes != 240 {
		t.Fatalf("utilization: %v %+v", err, u)
	}
	if m, err := BookedMinutes.Get(ctx, pool, struct {
		Tenanted
		RoomID RoomID
		Day    pgtype.Date
	}{tenant, large, pgtype.Date{Time: day, Valid: true}}); err != nil || m != 180 {
		t.Fatalf("booked minutes: %v %d", err, m)
	}

	took := 40 * time.Millisecond
	if n, err := Track(ctx, pool, []Event{
		{Tenanted: tenant, At: at(9), Kind: Booked, BookingID: &b1, ClientIP: &ip, Took: &took},
		{Tenanted: tenant, At: at(10), Kind: Viewed},
	}); err != nil || n != 2 {
		t.Fatalf("copy: %v %d", err, n)
	}
	if counts, err := EventCounts.Collect(ctx, pool, tenant); err != nil || len(counts) != 2 || counts[0].Kind != Booked {
		t.Fatalf("event counts: %v %+v", err, counts)
	}

	if id, err := CancelBooking.Get(ctx, pool, struct {
		Tenanted
		BookingID BookingID
	}{tenant, b1}); err != nil || id == nil || *id != b1 {
		t.Fatalf("cancel: %v %v", err, id)
	}
	if id, err := CancelBooking.Get(ctx, pool, struct {
		Tenanted
		BookingID BookingID
	}{tenant, b1}); err != nil || id != nil {
		t.Fatalf("cancel twice: %v %v", err, id)
	}
	if _, err := RemoveMember.Exec(ctx, pool, struct {
		Tenanted
		MemberID MemberID
	}{tenant, *bob}); err != nil {
		t.Fatal(err)
	}
	if ms, err := Members.Collect(ctx, pool, tenant); err != nil || len(ms) != 1 || ms[0].LastIP == nil || *ms[0].LastIP != ip {
		t.Fatalf("members: %v %+v", err, ms)
	}
	if gone, err := DeletedMembers.Collect(ctx, pool, tenant); err != nil || len(gone) != 1 || gone[0].Email != "bob@acme.example" {
		t.Fatalf("deleted members: %v %+v", err, gone)
	}
}
