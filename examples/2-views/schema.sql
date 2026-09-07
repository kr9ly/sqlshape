-- Stage 2: views as the read model. The tables are stage 1's; the application still
-- writes them with plain INSERT / UPDATE, but everything it reads is a view. A view
-- decides once what a thing is called (customer_email, status_label), which joins make
-- it, how it aggregates, and which rows exist for the application at all — the
-- soft-delete predicate is absorbed here instead of being repeated in every query.
-- `-no-table-reads` holds the line: a SELECT from a table is reported.

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

-- archived orders are invisible: every statement on orders carries the predicate (the
-- views below do), or opts out (archived_orders does)
-- sqlshape: visible where archived_at IS NULL
CREATE TABLE orders (
    id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    customer_id bigint NOT NULL REFERENCES customers(id),
    status      text NOT NULL DEFAULT 'pending' REFERENCES order_statuses(code),
    total       numeric(12,2) NOT NULL DEFAULT 0 CHECK (total >= 0),
    note        text,
    created_at  timestamptz NOT NULL DEFAULT now(),
    archived_at timestamptz
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

-- the read model: one view per thing the application talks about

CREATE VIEW statuses AS
SELECT code, label, sort_order FROM order_statuses;

CREATE VIEW customer_view AS
SELECT id, email, name, created_at FROM customers;

-- an order as the application sees it: the customer's email and the status label are
-- resolved here, and archived orders are not in it
CREATE VIEW order_view AS
SELECT o.id, o.customer_id, c.email AS customer_email, o.status, s.label AS status_label,
       o.total, o.note, o.created_at
  FROM orders o
  JOIN customers c ON c.id = o.customer_id
  JOIN order_statuses s ON s.code = o.status
 WHERE o.archived_at IS NULL;

CREATE VIEW order_line_view AS
SELECT i.order_id, i.line_no, i.sku, i.qty, i.price, i.qty * i.price AS amount
  FROM order_items i;

CREATE VIEW customer_totals AS
SELECT c.id AS customer_id, c.email, count(o.id) AS orders,
       coalesce(sum(o.total), 0) AS spent, max(o.created_at) AS last_order
  FROM customers c
  LEFT JOIN orders o ON o.customer_id = c.id AND o.status <> 'cancelled' AND o.archived_at IS NULL
 GROUP BY c.id, c.email;

-- the one place that looks at archived orders says so
-- sqlshape: unfiltered orders
CREATE VIEW archived_orders AS
SELECT o.id, o.customer_id, o.status, o.total, o.archived_at
  FROM orders o
 WHERE o.archived_at IS NOT NULL;
