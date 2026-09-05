package everything

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kr9ly/sqlshape"
)

// Open connects a pool whose every connection knows the schema's user types (the enum,
// the composites, the range types) before the first statement runs.
func Open(ctx context.Context, connString string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(connString)
	if err != nil {
		return nil, err
	}
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		return sqlshape.LoadUserTypes(ctx, conn)
	}
	return pgxpool.NewWithConfig(ctx, cfg)
}

// Reserve books a room and records the event; the two invariants live in triggers and
// come back by name.
func Reserve(ctx context.Context, db sqlshape.DB, t Tenanted, roomID RoomID, memberID MemberID, from, until time.Time, attendees []string) (BookingID, error) {
	slot := pgtype.Range[time.Time]{Lower: from, Upper: until, LowerType: pgtype.Inclusive, UpperType: pgtype.Exclusive, Valid: true}
	if attendees == nil {
		attendees = []string{} // a nil slice would be NULL, and the column is NOT NULL
	}
	id, err := Book.Get(ctx, db, NewBooking{Tenanted: t, RoomID: roomID, MemberID: memberID, Slot: slot, Attendees: attendees})
	switch {
	case sqlshape.Violates(err, "BK001"):
		return 0, fmt.Errorf("room %d is taken between %s and %s", roomID, from.Format(time.Kitchen), until.Format(time.Kitchen))
	case sqlshape.Violates(err, "BK002"):
		return 0, fmt.Errorf("room %d cannot seat %d people", roomID, len(attendees))
	case err != nil:
		return 0, err
	}
	return *id, nil
}

// Dashboard is what the tenant's home page needs, fetched in one round trip.
type Dashboard struct {
	Rooms    []Room
	Schedule []RoomSchedule
	Members  []Member
}

func LoadDashboard(ctx context.Context, db sqlshape.BatchDB, t Tenanted) (Dashboard, error) {
	b := sqlshape.NewBatch()
	rooms := sqlshape.Queue(b, Rooms, t)
	schedule := sqlshape.Queue(b, Schedule, t)
	members := sqlshape.Queue(b, Members, t)
	if err := b.Send(ctx, db); err != nil {
		return Dashboard{}, err
	}
	var d Dashboard
	var err error
	if d.Rooms, err = rooms.Rows(); err != nil {
		return d, err
	}
	if d.Schedule, err = schedule.Rows(); err != nil {
		return d, err
	}
	d.Members, err = members.Rows()
	return d, err
}

// Track appends analytics events in bulk.
func Track(ctx context.Context, db sqlshape.CopyDB, events []Event) (int64, error) {
	return AppendEvents.From(ctx, db, events)
}

// Search runs the booking search without a prepared statement: which filters are on
// changes the plan, and a generic plan for skewed tenants is the wrong plan.
func Search(ctx context.Context, db sqlshape.DB, p SearchParams) ([]Booking, error) {
	return SearchBookings.Unprepared().Collect(ctx, db, p)
}
