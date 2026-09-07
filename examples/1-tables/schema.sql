-- Stage 1: plain tables, the ground an ORM also covers. Everything the application
-- does is a statement against these tables; sqlshape checks each statement's columns,
-- parameters and failure modes against this file.

-- The order lifecycle as a lookup table seeded here: the rows are part of the schema (the
-- checker diffs them with the Go constants, the migration keeps them in step), a row can
-- carry a label and an order, a status in use is protected by the foreign key, and a
-- status can be retired by deleting its row — none of which an enum offers.
CREATE TABLE order_statuses (
    code       text PRIMARY KEY,
    label      text NOT NULL,
    sort_order integer NOT NULL
);
INSERT INTO order_statuses (code, label, sort_order) VALUES
    ('pending',   'Awaiting payment', 10),
    ('paid',      'Paid',             20),
    ('shipped',   'Shipped',          30),
    ('cancelled', 'Cancelled',        90);

CREATE TABLE customers (
    id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    email      text NOT NULL UNIQUE,
    name       text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE orders (
    id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    customer_id bigint NOT NULL REFERENCES customers(id),
    status      text NOT NULL DEFAULT 'pending' REFERENCES order_statuses(code),
    total       numeric(12,2) NOT NULL DEFAULT 0 CHECK (total >= 0),
    note        text,
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX orders_customer_idx ON orders (customer_id);

CREATE TABLE order_items (
    order_id bigint NOT NULL REFERENCES orders(id) ON DELETE CASCADE,
    line_no  smallint NOT NULL,
    sku      text NOT NULL,
    qty      integer NOT NULL CHECK (qty > 0),
    price    numeric(12,2) NOT NULL CHECK (price >= 0),
    PRIMARY KEY (order_id, line_no)
);
