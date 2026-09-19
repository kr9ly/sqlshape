-- sqlshape: postgres 17
-- "A parameter that may be NULL is a pointer" (-strict): the status column is indexed so
-- the example's one diagnostic is the non-pointer-enum note this section is about, not
-- also an unrelated "no index" advisory.
CREATE TYPE order_status AS ENUM ('pending', 'paid', 'shipped', 'cancelled');
CREATE TABLE orders (
    id     bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    status order_status NOT NULL DEFAULT 'pending'
);
CREATE INDEX orders_status_idx ON orders (status);
