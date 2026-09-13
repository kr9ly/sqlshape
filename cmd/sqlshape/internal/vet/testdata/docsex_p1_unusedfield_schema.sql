-- sqlshape: postgres 17
-- "Do not leave unused fields in `P` (-strict)": a plain, unkeyed orders.id column (no
-- PRIMARY KEY), so -strict's "carries key ... as a plain int64" advisory (a different
-- rule) does not also fire for this example's own R{ID int64} / P.ID int64; indexed, so
-- -strict's "no index on orders leads with any of (id)" advisory does not fire either.
CREATE TABLE orders (
    id bigint NOT NULL
);
CREATE INDEX orders_id_idx ON orders (id);
