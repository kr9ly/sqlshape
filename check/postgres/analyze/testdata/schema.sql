-- sqlshape: postgres 17
CREATE TYPE order_status AS ENUM ('pending', 'paid', 'shipped', 'cancelled');
CREATE DOMAIN yen AS bigint CHECK (VALUE >= 0);
CREATE DOMAIN email AS text NOT NULL CHECK (VALUE ~ '@');
CREATE TYPE money_amount AS (amount numeric(12,2), currency char(3));

CREATE EXTENSION citext;
CREATE EXTENSION pgcrypto;
CREATE EXTENSION hstore;
CREATE EXTENSION "uuid-ossp";

CREATE TABLE users (
    id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    email      email UNIQUE,
    name       varchar(100),
    alias      varchar(40) COLLATE "C",
    handle     citext,
    attrs      hstore,
    nick       character(8),
    tags       text[] NOT NULL DEFAULT '{}',
    balance    yen NOT NULL DEFAULT 0,
    role       text NOT NULL DEFAULT 'member' CHECK (role IN ('member', 'admin', 'owner')),
    score      real,
    ratio      double precision,
    flags      bit(4),
    vflags     bit varying(8),
    born       date,
    wake       time(3),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamp(0) without time zone
);

CREATE TABLE orders (
    id         bigserial PRIMARY KEY,
    user_id    bigint NOT NULL REFERENCES users(id),
    status     order_status NOT NULL DEFAULT 'pending',
    total      numeric(12,2) NOT NULL CHECK (total >= 0),
    price      money_amount,
    note       text,
    meta       jsonb,
    uid        uuid DEFAULT gen_random_uuid(),
    matrix     integer[][],
    created_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT orders_user_note_key UNIQUE (user_id, note)
);

CREATE TABLE order_items (
    order_id   bigint NOT NULL,
    line_no    smallint NOT NULL,
    sku        varchar(32) NOT NULL,
    qty        integer NOT NULL DEFAULT 1,
    PRIMARY KEY (order_id, line_no)
);
ALTER TABLE order_items ADD CONSTRAINT order_items_order_fk FOREIGN KEY (order_id) REFERENCES orders(id);
ALTER TABLE order_items ADD COLUMN discount numeric(5,2);
ALTER TABLE order_items ALTER COLUMN discount SET NOT NULL;

CREATE UNIQUE INDEX orders_uid_active ON orders (uid) WHERE status <> 'cancelled';
CREATE INDEX orders_user_idx ON orders (user_id);

CREATE VIEW order_summary AS
SELECT o.id, o.status, o.total, u.email
  FROM orders o JOIN users u ON u.id = o.user_id;

CREATE MATERIALIZED VIEW order_stats AS
SELECT user_id, count(*) AS n, sum(total) AS total FROM orders GROUP BY user_id;

CREATE TABLE order_stats_copy (
    user_id bigint PRIMARY KEY,
    n       bigint NOT NULL,
    total   numeric
);

CREATE FUNCTION save_order(p money_amount, items order_items[]) RETURNS bigint
LANGUAGE sql STABLE STRICT AS $$ SELECT 1::bigint $$;

CREATE FUNCTION list_totals(min_total numeric) RETURNS TABLE (user_id bigint, total numeric)
LANGUAGE sql AS $$ SELECT user_id, sum(total) FROM orders GROUP BY user_id $$;

CREATE FUNCTION user_ids() RETURNS SETOF bigint LANGUAGE sql AS $$ SELECT id FROM users $$;

-- sqlshape: not null
CREATE FUNCTION order_count(p_user bigint) RETURNS bigint
LANGUAGE sql STABLE AS $$ SELECT count(*) FROM orders WHERE user_id = p_user $$;

CREATE FUNCTION nick_of(p_user bigint) RETURNS text
LANGUAGE sql STABLE STRICT AS $$ SELECT name FROM users WHERE id = p_user $$;

CREATE PROCEDURE mark_paid(p_order bigint)
LANGUAGE sql AS $$ UPDATE orders SET status = 'paid' WHERE id = p_order $$;

CREATE PROCEDURE settle(p_order bigint, INOUT p_total numeric)
LANGUAGE sql AS $$ UPDATE orders SET status = 'paid' WHERE id = p_order; SELECT total FROM orders WHERE id = p_order $$;

COMMENT ON TABLE orders IS 'One purchase.';
COMMENT ON COLUMN orders.status IS 'Lifecycle state; see order_status.';
COMMENT ON TYPE order_status IS 'Order lifecycle.';

-- sqlshape: error P0401 = OrderTooLarge
CREATE FUNCTION check_order_size() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.total > 1000000 THEN
    RAISE EXCEPTION 'order too large' USING ERRCODE = 'P0401';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER orders_size BEFORE INSERT OR UPDATE ON orders FOR EACH ROW EXECUTE FUNCTION check_order_size();

-- sqlshape: visible where deleted_at IS NULL
CREATE TABLE memos (
    id         bigserial PRIMARY KEY,
    user_id    bigint NOT NULL REFERENCES users(id),
    body       text NOT NULL,
    deleted_at timestamptz
);
CREATE VIEW live_memos AS SELECT id, user_id, body FROM memos WHERE deleted_at IS NULL;

-- DDL coverage: partitions, inheritance, LIKE, renames / drops, range types, collations,
-- user-defined aggregate / operator / cast, search_path, interval typmods
CREATE TABLE events (
    id   bigint GENERATED ALWAYS AS IDENTITY,
    ts   timestamptz NOT NULL,
    kind text NOT NULL DEFAULT 'click',
    PRIMARY KEY (id, ts)
) PARTITION BY RANGE (ts);
CREATE TABLE events_2024 PARTITION OF events FOR VALUES FROM ('2024-01-01') TO ('2025-01-01');
CREATE TABLE events_default PARTITION OF events DEFAULT;

CREATE TABLE base_doc (id int PRIMARY KEY, title text NOT NULL);
CREATE TABLE note_doc (body text) INHERITS (base_doc);

CREATE TABLE users_archive (LIKE users INCLUDING DEFAULTS INCLUDING CONSTRAINTS, archived_at timestamptz NOT NULL DEFAULT now());
ALTER TABLE users_archive DROP COLUMN attrs;
ALTER TABLE users_archive RENAME COLUMN nick TO nickname;
ALTER TABLE users_archive RENAME TO users_history;
ALTER TABLE users_history ADD CONSTRAINT users_history_email_len CHECK (length(email) < 200);
ALTER TABLE users_history DROP CONSTRAINT users_history_email_len;

CREATE TABLE tmp_drop (id int);
DROP TABLE tmp_drop;

CREATE TYPE priority AS ENUM ('low', 'high');
ALTER TYPE priority ADD VALUE 'mid' BEFORE 'high';
ALTER TYPE priority RENAME VALUE 'low' TO 'lowest';

CREATE DOMAIN code AS text;
ALTER DOMAIN code ADD CONSTRAINT code_len CHECK (length(VALUE) = 3);
ALTER DOMAIN code SET NOT NULL;

CREATE TYPE floatrange AS RANGE (subtype = float8);
CREATE COLLATION mycoll FROM "C";

CREATE TABLE spans (
    id    int PRIMARY KEY,
    fr    floatrange,
    mr    floatmultirange,
    label text COLLATE mycoll,
    prio  priority NOT NULL DEFAULT 'mid',
    cd    code,
    every interval hour to minute,
    lag3  interval(3),
    d2s   interval day to second(2)
);

CREATE FUNCTION yen_sum_step(bigint, yen) RETURNS bigint LANGUAGE sql IMMUTABLE AS $$ SELECT $1 + $2 $$;
CREATE AGGREGATE yen_sum (yen) (sfunc = yen_sum_step, stype = bigint, initcond = '0');

CREATE FUNCTION approx_eq(numeric, numeric) RETURNS boolean LANGUAGE sql IMMUTABLE AS $$ SELECT abs($1 - $2) < 0.01 $$;
CREATE OPERATOR ~= (leftarg = numeric, rightarg = numeric, function = approx_eq);

CREATE FUNCTION money_text(money_amount) RETURNS text LANGUAGE sql IMMUTABLE AS $$ SELECT ($1).currency $$;
CREATE CAST (money_amount AS text) WITH FUNCTION money_text(money_amount) AS ASSIGNMENT;

CREATE SCHEMA app;
SET search_path TO app, public;
CREATE TABLE settings (k text PRIMARY KEY, v text);
SET search_path TO public;

CREATE TABLE hosts (
    id      int PRIMARY KEY,
    addr    inet NOT NULL,
    net     cidr,
    mac     macaddr,
    uptime  interval NOT NULL,
    attrs   hstore,
    span    int4range NOT NULL,
    spans   int4multirange NOT NULL,
    seen    tstzrange,
    fr      floatrange,
    pos     point,
    flags   bit(4),
    doc     tsvector,
    fee     money,
    at_tz   timetz
);
