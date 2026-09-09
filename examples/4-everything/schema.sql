-- sqlshape: postgres 17
-- Stage 3: everything. A multi-tenant room booking service. The `core` schema is the
-- database's private side, `app` is the API the application may reference (-schemas=app);
-- every table row belongs to a tenant and every statement pins it (-require-columns).

CREATE EXTENSION citext;
CREATE EXTENSION pgcrypto;
CREATE EXTENSION hstore;

CREATE SCHEMA core;
CREATE SCHEMA app;

-- --- core: types --------------------------------------------------------------------

CREATE TYPE core.plan AS ENUM ('trial', 'team', 'business');
CREATE DOMAIN core.minutes AS integer CHECK (VALUE > 0);
-- a value object the Go side carries as one declared type (`// sqlshape: type core.money`)
CREATE TYPE core.money AS (amount numeric(12,2), currency char(3));
-- what a bulk import of rooms sends: an array of these
CREATE TYPE core.room_in AS (name text, capacity integer, hourly core.money);
-- one booking inside a room's schedule (a view column cannot be an anonymous record[])
CREATE TYPE core.slot AS (id bigint, slot tstzrange, note text);

-- --- core: tables -------------------------------------------------------------------

CREATE TABLE core.tenants (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    slug       citext NOT NULL UNIQUE,
    plan       core.plan NOT NULL DEFAULT 'trial',
    settings   hstore NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now()
);
COMMENT ON TABLE core.tenants IS 'One customer organisation.';
COMMENT ON COLUMN core.tenants.settings IS 'Free-form key/value settings (feature flags, locale).';

-- soft-deleted rows are invisible: every read must carry the predicate, or opt out
-- sqlshape: visible where deleted_at IS NULL
CREATE TABLE core.members (
    tenant_id  uuid NOT NULL REFERENCES core.tenants(id),
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    email      citext NOT NULL,
    name       text NOT NULL,
    last_ip    inet,
    deleted_at timestamptz,
    UNIQUE (tenant_id, email)
);

CREATE TABLE core.rooms (
    tenant_id uuid NOT NULL REFERENCES core.tenants(id),
    id        bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name      text NOT NULL,
    capacity  integer NOT NULL CHECK (capacity > 0),
    hourly    core.money NOT NULL,
    UNIQUE (tenant_id, name)
);

CREATE TABLE core.bookings (
    tenant_id  uuid NOT NULL REFERENCES core.tenants(id),
    id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    room_id    bigint NOT NULL REFERENCES core.rooms(id),
    member_id  uuid NOT NULL REFERENCES core.members(id),
    slot       tstzrange NOT NULL,
    minutes    core.minutes NOT NULL GENERATED ALWAYS AS ((extract(epoch FROM upper(slot) - lower(slot)) / 60)::integer) STORED,
    attendees  text[] NOT NULL DEFAULT '{}',
    tags       hstore,
    note       text,
    created_at timestamptz NOT NULL DEFAULT now()
);
COMMENT ON COLUMN core.bookings.slot IS 'Reserved window, half-open: [start, end).';
CREATE INDEX bookings_room_slot ON core.bookings (room_id, slot);

-- row-level security as the second fence around tenant data: a session sets app.tenant_id
-- (SET LOCAL app.tenant_id = '...') and the database shows it that tenant's bookings only,
-- whatever the statement says. The checker type-checks the predicate, migrates the policy
-- with the table, and points out when policies would not apply (row security off, a
-- SECURITY DEFINER function). current_setting without missing_ok: a session that forgot to
-- set the tenant fails loudly instead of seeing nothing.
ALTER TABLE core.bookings ENABLE ROW LEVEL SECURITY;
CREATE POLICY bookings_tenant ON core.bookings
    USING (tenant_id = current_setting('app.tenant_id')::uuid);

-- two invariants a CHECK cannot express, each with its own SQLSTATE
-- sqlshape: error BK001 = SlotTaken
CREATE FUNCTION core.check_slot_free() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF EXISTS (SELECT 1 FROM core.bookings b WHERE b.room_id = NEW.room_id AND b.id <> NEW.id AND b.slot && NEW.slot) THEN
        RAISE EXCEPTION 'slot taken' USING ERRCODE = 'BK001';
    END IF;
    RETURN NEW;
END $$;
-- sqlshape: error BK002 = OverCapacity
CREATE FUNCTION core.check_capacity() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF cardinality(NEW.attendees) > (SELECT capacity FROM core.rooms WHERE id = NEW.room_id) THEN
        RAISE EXCEPTION 'over capacity' USING ERRCODE = 'BK002';
    END IF;
    RETURN NEW;
END $$;
-- UPDATE OF: tagging a booking does not re-check the slot or the capacity
CREATE TRIGGER bookings_slot BEFORE INSERT OR UPDATE OF room_id, slot ON core.bookings FOR EACH ROW EXECUTE FUNCTION core.check_slot_free();
CREATE TRIGGER bookings_capacity BEFORE INSERT OR UPDATE OF room_id, attendees ON core.bookings FOR EACH ROW EXECUTE FUNCTION core.check_capacity();

-- --- app: the API -------------------------------------------------------------------

CREATE VIEW app.tenants AS
SELECT id, slug, plan, settings, created_at FROM core.tenants;

CREATE VIEW app.members AS
SELECT tenant_id, id, email, name, last_ip FROM core.members WHERE deleted_at IS NULL;

-- the removed members, for an audit screen: the one place that reads them
-- sqlshape: unfiltered core.members
CREATE VIEW app.removed_members AS
SELECT tenant_id, id, email, deleted_at FROM core.members WHERE deleted_at IS NOT NULL;

CREATE VIEW app.rooms AS
SELECT tenant_id, id, name, capacity, hourly FROM core.rooms;

-- a booking made by a since-removed member is still a booking: the view reads members unfiltered
-- sqlshape: unfiltered core.members
CREATE VIEW app.bookings AS
SELECT b.tenant_id, b.id, b.room_id, r.name AS room, b.member_id, m.email AS booked_by,
       b.slot, b.minutes, b.attendees, b.tags, b.note, b.created_at
  FROM core.bookings b
  JOIN core.rooms r ON r.id = b.room_id
  JOIN core.members m ON m.id = b.member_id;

-- a room with its bookings as nested rows
CREATE VIEW app.room_schedule AS
SELECT r.tenant_id, r.id AS room_id, r.name, r.capacity,
       array_agg(row(b.id, b.slot, b.note)::core.slot ORDER BY b.slot) FILTER (WHERE b.id IS NOT NULL) AS bookings
  FROM core.rooms r
  LEFT JOIN core.bookings b ON b.room_id = r.id
 GROUP BY r.tenant_id, r.id;

CREATE MATERIALIZED VIEW app.utilization AS
SELECT b.tenant_id, b.room_id, lower(b.slot)::date AS day, sum(b.minutes) AS booked_minutes, count(*) AS bookings
  FROM core.bookings b
 GROUP BY b.tenant_id, b.room_id, lower(b.slot)::date;
CREATE UNIQUE INDEX utilization_key ON app.utilization (tenant_id, room_id, day);

-- analytics events the application appends in bulk (COPY): the one table in app
CREATE TABLE app.events (
    tenant_id   uuid NOT NULL,
    at          timestamptz NOT NULL DEFAULT now(),
    kind        text NOT NULL CHECK (kind IN ('booked', 'cancelled', 'viewed')),
    booking_id  bigint,
    client_ip   inet,
    took        interval
);
CREATE INDEX events_tenant_at ON app.events (tenant_id, at);

-- a NULL plan means the default: coalesce keeps the NOT NULL column out of the failure modes
CREATE FUNCTION app.create_tenant(p_slug citext, p_plan core.plan) RETURNS uuid
LANGUAGE sql AS $$
    INSERT INTO core.tenants (slug, plan) VALUES (p_slug, coalesce(p_plan, 'trial')) RETURNING id
$$;

CREATE FUNCTION app.add_member(p_tenant uuid, p_email citext, p_name text, p_ip inet) RETURNS uuid
LANGUAGE sql AS $$
    INSERT INTO core.members (tenant_id, email, name, last_ip) VALUES (p_tenant, p_email, p_name, p_ip) RETURNING id
$$;

CREATE FUNCTION app.remove_member(p_tenant uuid, p_member uuid) RETURNS void
LANGUAGE sql AS $$
    UPDATE core.members SET deleted_at = now()
    WHERE tenant_id = p_tenant AND id = p_member AND deleted_at IS NULL
$$;

-- a composite array parameter: the application passes []RoomIn
CREATE FUNCTION app.import_rooms(p_tenant uuid, p_rooms core.room_in[]) RETURNS bigint
LANGUAGE sql AS $$
    WITH ins AS (
        INSERT INTO core.rooms (tenant_id, name, capacity, hourly)
        SELECT p_tenant, r.name, r.capacity, r.hourly FROM unnest(p_rooms) AS r
        RETURNING 1
    )
    SELECT count(*) FROM ins
$$;

CREATE FUNCTION app.book(p_tenant uuid, p_room bigint, p_member uuid, p_slot tstzrange, p_attendees text[], p_note text) RETURNS bigint
LANGUAGE sql AS $$
    INSERT INTO core.bookings (tenant_id, room_id, member_id, slot, attendees, note)
    VALUES (p_tenant, p_room, p_member, p_slot, p_attendees, p_note) RETURNING id
$$;

CREATE FUNCTION app.tag_booking(p_tenant uuid, p_booking bigint, p_tags hstore) RETURNS void
LANGUAGE sql AS $$
    UPDATE core.bookings SET tags = coalesce(tags, '') || p_tags WHERE tenant_id = p_tenant AND id = p_booking
$$;

CREATE FUNCTION app.cancel(p_tenant uuid, p_booking bigint) RETURNS bigint
LANGUAGE sql AS $$
    DELETE FROM core.bookings WHERE tenant_id = p_tenant AND id = p_booking RETURNING id
$$;

-- sqlshape: not null
CREATE FUNCTION app.booked_minutes(p_tenant uuid, p_room bigint, p_day date) RETURNS bigint
LANGUAGE sql STABLE AS $$
    SELECT coalesce(sum(minutes), 0) FROM core.bookings WHERE tenant_id = p_tenant AND room_id = p_room AND lower(slot)::date = p_day
$$;
