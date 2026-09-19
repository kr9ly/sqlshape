-- sqlshape: postgres 17
-- "Do not declare a violation that cannot happen": an orders table whose total has a
-- named CHECK the doc's UPDATE (which never touches total) cannot violate.
CREATE TABLE orders (
    id    bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    note  text,
    total numeric(12,2) NOT NULL CONSTRAINT orders_total_check CHECK (total >= 0)
);
