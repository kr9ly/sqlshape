-- sqlshape: postgres 17
-- "Do not pass another table's ID": users/orders, just enough for UserID to bind to
-- users.id and for the example's Params.ID to meet orders.id instead.
CREATE TABLE users (
    id    bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    email text NOT NULL
);

CREATE TABLE orders (
    id    bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    total numeric(12,2) NOT NULL
);
