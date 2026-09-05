-- Stage 1: plain tables, the ground an ORM also covers. Everything the application
-- does is a statement against these tables; sqlshape checks each statement's columns,
-- parameters and failure modes against this file.

CREATE TYPE order_status AS ENUM ('pending', 'paid', 'shipped', 'cancelled');

CREATE TABLE customers (
    id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    email      text NOT NULL UNIQUE,
    name       text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE orders (
    id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    customer_id bigint NOT NULL REFERENCES customers(id),
    status      order_status NOT NULL DEFAULT 'pending',
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
