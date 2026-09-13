-- sqlshape: postgres 17
-- "A status column moves along its declared transitions (`transitions`)".
-- sqlshape: transitions status: draft -> submitted, submitted -> paid | cancelled, paid -> refunded
CREATE TABLE orders (
    id     bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    status text NOT NULL
);
