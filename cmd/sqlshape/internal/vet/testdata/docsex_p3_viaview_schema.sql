-- sqlshape: postgres 17
-- "Tables are read through views (`via view`, `-no-table-reads` / `-no-tables`)":
-- orders/customers as the direct-read Rejected example needs, and order_summary as the
-- Passes example's view. The concrete example demonstrates the -no-table-reads flag on
-- its own (no persistent `require via view` declared here -- that directive's syntax is
-- shown separately, just above this heading's example, as the general form).
CREATE TABLE orders (
    id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    customer_id bigint NOT NULL
);

CREATE TABLE customers (
    id   bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name text NOT NULL
);

CREATE VIEW order_summary AS
SELECT o.id, c.name AS customer_name FROM orders o JOIN customers c ON c.id = o.customer_id;
