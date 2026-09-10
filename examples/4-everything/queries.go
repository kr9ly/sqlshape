package everything

import (
	"database/sql/driver"
	"fmt"
	"net/netip"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/kr9ly/sqlshape"
	"github.com/kr9ly/sqlshape/postgres"
)

// --- Go types for the schema's types -------------------------------------------------------

type Plan string

const (
	Trial    Plan = "trial"
	Team     Plan = "team"
	Business Plan = "business"
)

func (p Plan) Known() bool { return p == Trial || p == Team || p == Business }

// EventKind is the CHECK value set on app.events.kind.
type EventKind string

const (
	Booked    EventKind = "booked"
	Cancelled EventKind = "cancelled"
	Viewed    EventKind = "viewed"
)

// Minutes meets the core.minutes domain.
type Minutes int32

// Identities. A Go type that meets a key column (a PK, or an FK pointing at one) is bound
// to that identity: a MemberID passed where a room id is expected is reported, though both
// would be plain values otherwise.
type (
	TenantID  string
	MemberID  string
	RoomID    int64
	BookingID int64
)

// Money carries core.money as one value and owns its text form "(amount,currency)".
//
// sqlshape: type core.money
type Money struct {
	Amount   string
	Currency string
}

func (m *Money) Scan(src any) error {
	var s string
	switch v := src.(type) {
	case nil:
		*m = Money{}
		return nil
	case string:
		s = v
	case []byte:
		s = string(v)
	default:
		return fmt.Errorf("Money: cannot scan %T", src)
	}
	s = strings.TrimSuffix(strings.TrimPrefix(s, "("), ")")
	amount, currency, ok := strings.Cut(s, ",")
	if !ok {
		return fmt.Errorf("Money: malformed %q", src)
	}
	*m = Money{Amount: amount, Currency: strings.Trim(currency, `" `)}
	return nil
}

func (m Money) Value() (driver.Value, error) { return "(" + m.Amount + "," + m.Currency + ")", nil }

func (m Money) String() string { return m.Amount + " " + m.Currency }

// Tenanted is embedded into every tenant-scoped row: its field is the row's tenant_id column.
type Tenanted struct {
	TenantID TenantID
}

// --- reads ---------------------------------------------------------------------------------

type Tenant struct {
	ID        TenantID
	Slug      string
	Plan      Plan
	Settings  map[string]*string
	CreatedAt time.Time
}

var TenantBySlug = sqlshape.One[Tenant, struct{ Slug string }](`
SELECT id, slug, plan, settings, created_at FROM app.tenants WHERE slug = {{.Slug}}`)

type Member struct {
	Tenanted
	ID     MemberID
	Email  string
	Name   string
	LastIP *netip.Addr
}

var Members = sqlshape.Query[Member, Tenanted](`
SELECT tenant_id, id, email, name, last_ip FROM app.members WHERE tenant_id = {{.TenantID}} ORDER BY email`)

// app.members already filters deleted members; app.removed_members is the view that
// declared, in schema.sql, that it reads core.members unfiltered.
type DeletedMember struct {
	Tenanted
	ID        MemberID
	Email     string
	DeletedAt time.Time
}

var DeletedMembers = sqlshape.Query[DeletedMember, Tenanted](`
-- sqlshape: not null deleted_at
SELECT tenant_id, id, email, deleted_at FROM app.removed_members WHERE tenant_id = {{.TenantID}} ORDER BY deleted_at`)

type Room struct {
	Tenanted
	ID       RoomID
	Name     string
	Capacity int32
	Hourly   Money
}

var Rooms = sqlshape.Query[Room, Tenanted](`
SELECT tenant_id, id, name, capacity, hourly FROM app.rooms WHERE tenant_id = {{.TenantID}} ORDER BY name`)

type Booking struct {
	Tenanted
	ID        BookingID
	RoomID    RoomID
	Room      string
	MemberID  MemberID
	BookedBy  string
	Slot      pgtype.Range[time.Time]
	Minutes   Minutes
	Attendees []string
	Tags      map[string]*string
	Note      *string
	CreatedAt time.Time
}

const bookingColumns = `tenant_id, id, room_id, room, member_id, booked_by, slot, minutes, attendees, tags, note, created_at`

var BookingByID = sqlshape.One[Booking, struct {
	Tenanted
	ID BookingID
}](`SELECT ` + bookingColumns + ` FROM app.bookings WHERE tenant_id = {{.TenantID}} AND id = {{.ID}}`)

// SearchParams has nine optional filters: 2^9 = 512 branch combinations, more than the
// checker expands in full, so it checks the sparse set (all off, all on, each alone) and
// -strict reports that the runtime cannot compare renderings with the checked set.
type SearchParams struct {
	Tenanted
	RoomID     *RoomID
	MemberID   *MemberID
	From       *time.Time
	Until      *time.Time
	MinMinutes *Minutes
	Attendee   *string
	Tag        *string
	NoteLike   *string
	Room       *string
	Limit      int32
}

var SearchBookings = sqlshape.Query[Booking, SearchParams](`
SELECT ` + bookingColumns + `
  FROM app.bookings
 WHERE tenant_id = {{.TenantID}}
   {{if .RoomID}}     AND room_id = {{.RoomID}}                 {{end}}
   {{if .MemberID}}   AND member_id = {{.MemberID}}             {{end}}
   {{if .From}}       AND upper(slot) > {{.From}}               {{end}}
   {{if .Until}}      AND lower(slot) < {{.Until}}              {{end}}
   {{if .MinMinutes}} AND minutes >= {{.MinMinutes}}            {{end}}
   {{if .Attendee}}   AND {{.Attendee}} = ANY(attendees)        {{end}}
   {{if .Tag}}        AND tags ? {{.Tag}}                       {{end}}
   {{if .NoteLike}}   AND note ILIKE '%' || {{.NoteLike}} || '%' {{end}}
   {{if .Room}}       AND room = {{.Room}}                      {{end}}
 ORDER BY lower(slot), id
 {{with .Limit}} LIMIT {{.}} {{end}}`)

// Slot is one nested row of room_schedule.bookings: array_agg(row(b.id, b.slot, b.note)::core.slot).
// The LEFT JOIN makes b.id nullable to the analyzer; the FILTER (WHERE b.id IS NOT NULL)
// in the view guarantees otherwise, and the tag says so.
type Slot struct {
	ID   BookingID `col:"id,notnull"`
	Slot pgtype.Range[time.Time]
	Note *string
}

type RoomSchedule struct {
	Tenanted
	RoomID   RoomID
	Name     string
	Capacity int32
	Bookings []Slot // NULL for a room without bookings: a nil slice
}

var Schedule = sqlshape.Query[RoomSchedule, Tenanted](`
SELECT tenant_id, room_id, name, capacity, bookings FROM app.room_schedule WHERE tenant_id = {{.TenantID}} ORDER BY name`)

// Day is a date: pgtype.Date keeps it a civil date instead of a time.Time in some zone.
type Utilization struct {
	RoomID        RoomID
	Day           pgtype.Date
	BookedMinutes int64
	Bookings      int64
}

var UtilizationOf = sqlshape.Query[Utilization, struct {
	Tenanted
	Since pgtype.Date
}](`
-- sqlshape: not null booked_minutes
SELECT room_id, day, booked_minutes, bookings FROM app.utilization WHERE tenant_id = {{.TenantID}} AND day >= {{.Since}} ORDER BY day, room_id`)

var UtilizationView = postgres.MatView("app.utilization")

var BookedMinutes = sqlshape.One[int64, struct {
	Tenanted
	RoomID RoomID
	Day    pgtype.Date
}](`SELECT app.booked_minutes({{.TenantID}}, {{.RoomID}}, {{.Day}})`)

// --- writes ----------------------------------------------------------------------------------

// Plan is a pointer: a non-pointer enum's zero value "" is no label (an advisory under -strict).
var CreateTenant = sqlshape.One[*TenantID, struct {
	Slug string
	Plan *Plan
}](`
-- sqlshape: expect tenants_slug_key
SELECT app.create_tenant({{.Slug}}, {{.Plan}})`)

var AddMember = sqlshape.One[*MemberID, struct {
	Tenanted
	Email string
	Name  string
	IP    *netip.Addr
}](`
-- sqlshape: expect members_tenant_id_email_key, members_tenant_id_fkey
SELECT app.add_member({{.TenantID}}, {{.Email}}, {{.Name}}, {{.IP}})`)

var RemoveMember = sqlshape.One[struct{}, struct {
	Tenanted
	MemberID MemberID
}](`SELECT app.remove_member({{.TenantID}}, {{.MemberID}})`)

// RoomIn is one element of the core.room_in[] parameter: fields line up with the composite.
type RoomIn struct {
	Name     string
	Capacity int32
	Hourly   Money
}

// The NOT NULL modes come from the array's elements, which the checker cannot see into
// (a RoomIn with an empty Name is a "" in Go, never a NULL — but the SQL could be fed one).
var ImportRooms = sqlshape.One[*int64, struct {
	Tenanted
	Rooms []RoomIn
}](`
-- sqlshape: expect rooms_tenant_id_name_key, rooms_tenant_id_fkey, rooms_capacity_check, rooms.tenant_id, rooms.name, rooms.capacity, rooms.hourly
SELECT app.import_rooms({{.TenantID}}, {{.Rooms}})`)

type NewBooking struct {
	Tenanted
	RoomID    RoomID
	MemberID  MemberID
	Slot      pgtype.Range[time.Time]
	Attendees []string
	Note      *string
}

// Slot is a pgtype.Range (Valid: false is NULL) and Attendees a slice (nil is NULL), so
// both NOT NULL columns stay possible failure modes; the Go side keeps them impossible.
var Book = sqlshape.One[*BookingID, NewBooking](`
-- sqlshape: expect bookings_tenant_id_fkey, bookings_room_id_fkey, bookings_member_id_fkey, bookings.slot, bookings.attendees, BK001, BK002
SELECT app.book({{.TenantID}}, {{.RoomID}}, {{.MemberID}}, {{.Slot}}, {{.Attendees}}, {{.Note}})`)

var TagBooking = sqlshape.One[struct{}, struct {
	Tenanted
	BookingID BookingID
	Tags      map[string]string
}](`SELECT app.tag_booking({{.TenantID}}, {{.BookingID}}, {{.Tags}})`)

// CancelBooking returns the cancelled id, or nil when there was nothing to cancel.
var CancelBooking = sqlshape.One[*BookingID, struct {
	Tenanted
	BookingID BookingID
}](`SELECT app.cancel({{.TenantID}}, {{.BookingID}})`)

// Event rows are appended with COPY; the checker matches the columns to the fields.
type Event struct {
	Tenanted
	At        time.Time
	Kind      EventKind
	BookingID *BookingID
	ClientIP  *netip.Addr
	Took      *time.Duration
}

var AppendEvents = postgres.Copy[Event]("app.events", "tenant_id", "at", "kind", "booking_id", "client_ip", "took")

type EventCount struct {
	Kind EventKind
	N    int64
}

var EventCounts = sqlshape.Query[EventCount, Tenanted](`
SELECT kind, count(*) AS n FROM app.events WHERE tenant_id = {{.TenantID}} GROUP BY kind ORDER BY kind`)
