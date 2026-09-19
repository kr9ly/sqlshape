-- sqlshape: postgres 17
-- "An aggregate is reached through its root, one per statement (`aggregate`)": orders is
-- the aggregate root of order_items; invoices is a root of its own (no children), so a
-- statement touching both belongs to two aggregates, as the Rejected example says.
-- sqlshape: aggregate orders (order_items)
CREATE TABLE orders (
    id     bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    status text NOT NULL
);

CREATE TABLE order_items (
    id       bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    order_id bigint NOT NULL REFERENCES orders(id)
);

-- sqlshape: aggregate invoices ()
CREATE TABLE invoices (
    id       bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    order_id bigint NOT NULL REFERENCES orders(id)
);
