// Package everything is the third sqlshape example: a multi-tenant room booking service
// that uses the whole surface. Read examples/1-tables and examples/2-database-api first.
//
// Check it with every policy on:
//
//	go run ../../cmd/sqlshape -strict -schemas=app -require-columns=tenant_id -sync-comments .
//
// which prints exactly one line: the advisory that SearchBookings is checked sparsely.
//
// What it adds:
//
//   - A service boundary: the `core` schema is private, the application references `app`
//     only (-schemas=app), and every table statement pins tenant_id (-require-columns).
//   - Extension types: citext (Slug / Email as strings), pgcrypto (gen_random_uuid),
//     hstore (map[string]*string). inet as netip.Addr, tstzrange as pgtype.Range[time.Time],
//     interval as time.Duration, text[] as []string, a GENERATED column.
//   - `// sqlshape: type core.money`: Money owns its wire format through Scan / Value and is
//     accepted exactly where SQL has core.money.
//   - Composite array parameters: ImportRooms passes []RoomIn for core.room_in[].
//   - Nested rows: room_schedule's array_agg(row(...)) lands in []Slot.
//   - Embedded structs: Tenanted is flattened into every row type that has tenant_id.
//   - A soft-delete policy: core.members rows are visible where deleted_at IS NULL; the app.members
//     view carries the predicate, and the one statement that lists deleted members opts out.
//   - Two trigger SQLSTATEs (BK001 SlotTaken, BK002 OverCapacity) on the write function.
//   - Batch: the dashboard loads rooms, schedule and members in one round trip. Copy: analytics
//     events are appended in bulk. Unprepared: a search with a skewed parameter.
//   - Sparse checking: SearchBookings has 9 optional branches (512 combinations > 256), so
//     the checker takes the sparse set and -strict says so.
//   - pgxpool with LoadUserTypes in AfterConnect, so every pooled connection knows the
//     enum, composites and ranges up front.
//   - COMMENT ON in schema.sql becomes doc comments with -sync-comments and `sqlshape -fix`.
package everything
