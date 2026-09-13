-- sqlshape: postgres 17
-- "Nested rows are received by structs".
CREATE TABLE orders (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY
);
CREATE TABLE order_items (
    order_id bigint NOT NULL REFERENCES orders(id),
    sku      text NOT NULL,
    qty      integer NOT NULL
);
CREATE TYPE order_item AS (sku text, qty integer);
