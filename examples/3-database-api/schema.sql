-- sqlshape: postgres 17
-- Stage 3: the database as an API. Tables are the database's private side; the
-- application reads views and calls functions. Meaning lives in the schema — domains
-- for units, an enum and a CHECK value set for closed sets, a composite for a value
-- object, a trigger for an invariant a CHECK cannot express — and sqlshape carries it
-- into Go: a Go type that meets a domain is bound to it, a switch over the value set
-- must be exhaustive, the trigger's SQLSTATE becomes a failure mode of the write.

-- units and closed sets
CREATE DOMAIN email AS text CHECK (VALUE ~ '^[^@]+@[^@]+$');
CREATE DOMAIN yen AS bigint CHECK (VALUE >= 0);
CREATE TYPE order_status AS ENUM ('pending', 'paid', 'shipped', 'cancelled');
CREATE TYPE address AS (street text, city text, postal_code text);

-- private tables
CREATE TABLE customers (
    id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    email      email NOT NULL UNIQUE,
    name       text NOT NULL,
    tier       text NOT NULL DEFAULT 'free' CHECK (tier IN ('free', 'pro', 'enterprise')),
    ships_to   address,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE orders (
    id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    customer_id bigint NOT NULL REFERENCES customers(id),
    status      order_status NOT NULL DEFAULT 'pending',
    shipping    yen NOT NULL DEFAULT 0,
    note        text,
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE order_items (
    order_id   bigint NOT NULL REFERENCES orders(id) ON DELETE CASCADE,
    line_no    smallint NOT NULL,
    sku        text NOT NULL,
    qty        integer NOT NULL CHECK (qty > 0),
    unit_price yen NOT NULL,
    PRIMARY KEY (order_id, line_no)
);

-- an invariant a CHECK cannot express: a customer on the free tier may not have more
-- than three open orders. The checker reads the body: the RAISE adds OS001 to the failure
-- modes of every write that fires the trigger. The annotation gives the code a name, so
-- Go can read it back as sqlshape.Violates(err, "TooManyOpenOrders") as well as "OS001".
-- sqlshape: error OS001 = TooManyOpenOrders
CREATE FUNCTION check_open_orders() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF (SELECT tier FROM customers WHERE id = NEW.customer_id) = 'free'
       AND (SELECT count(*) FROM orders WHERE customer_id = NEW.customer_id AND status = 'pending') >= 3 THEN
        RAISE EXCEPTION 'too many open orders' USING ERRCODE = 'OS001';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER orders_open_limit BEFORE INSERT ON orders FOR EACH ROW EXECUTE FUNCTION check_open_orders();

-- the read side: views fix the projection, so the row types are decided here once
CREATE VIEW customer_view AS
SELECT c.id, c.email, c.name, c.tier, c.ships_to, c.created_at
  FROM customers c;

-- yen is a unit: sum() over bigint is numeric and drops it, so the sum is cast back to
-- yen before it is added to o.shipping (yen + yen); numeric + yen would be reported
CREATE VIEW order_view AS
SELECT o.id, o.customer_id, c.email AS customer_email, o.status, o.note, o.created_at,
       coalesce(sum(i.qty * i.unit_price), 0)::yen AS subtotal,
       o.shipping,
       coalesce(sum(i.qty * i.unit_price), 0)::yen + o.shipping AS total,
       count(i.line_no)::int AS lines
  FROM orders o
  JOIN customers c ON c.id = o.customer_id
  LEFT JOIN order_items i ON i.order_id = o.id
 GROUP BY o.id, c.email;

CREATE VIEW order_line_view AS
SELECT i.order_id, i.line_no, i.sku, i.qty, i.unit_price, (i.qty * i.unit_price)::yen AS amount
  FROM order_items i;

CREATE MATERIALIZED VIEW sales_by_day AS
SELECT o.created_at::date AS day, count(*) AS orders, coalesce(sum(i.qty * i.unit_price), 0)::yen AS revenue
  FROM orders o JOIN order_items i ON i.order_id = o.id
 WHERE o.status <> 'cancelled'
 GROUP BY o.created_at::date;
CREATE UNIQUE INDEX sales_by_day_day ON sales_by_day (day);

-- the write side: functions. LANGUAGE sql bodies are analyzed when the schema loads,
-- so a typo in here is reported before any test runs.
CREATE FUNCTION create_customer(p_email email, p_name text, p_tier text) RETURNS bigint
LANGUAGE sql AS $$
    INSERT INTO customers (email, name, tier) VALUES (p_email, p_name, p_tier) RETURNING id
$$;

CREATE FUNCTION set_address(p_customer bigint, p_address address) RETURNS void
LANGUAGE sql AS $$
    UPDATE customers SET ships_to = p_address WHERE id = p_customer
$$;

CREATE FUNCTION place_order(p_customer bigint, p_note text, p_shipping yen) RETURNS bigint
LANGUAGE sql AS $$
    INSERT INTO orders (customer_id, note, shipping) VALUES (p_customer, p_note, p_shipping) RETURNING id
$$;

CREATE FUNCTION add_line(p_order bigint, p_sku text, p_qty integer, p_unit_price yen) RETURNS smallint
LANGUAGE sql AS $$
    INSERT INTO order_items (order_id, line_no, sku, qty, unit_price)
    VALUES (p_order, (SELECT coalesce(max(line_no), 0) + 1 FROM order_items WHERE order_id = p_order), p_sku, p_qty, p_unit_price)
    RETURNING line_no
$$;

-- returns the new status, or NULL when the order was not pending
CREATE FUNCTION pay_order(p_order bigint) RETURNS order_status
LANGUAGE sql AS $$
    UPDATE orders SET status = 'paid' WHERE id = p_order AND status = 'pending' RETURNING status
$$;

-- count(*) is never NULL, but PG does not know that about a function result; the
-- annotation tells the checker, so Go receives int64 rather than *int64
-- sqlshape: not null
CREATE FUNCTION open_orders(p_customer bigint) RETURNS bigint
LANGUAGE sql STABLE AS $$
    SELECT count(*) FROM orders WHERE customer_id = p_customer AND status = 'pending'
$$;
