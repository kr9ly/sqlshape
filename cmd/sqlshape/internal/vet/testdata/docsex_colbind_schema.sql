-- sqlshape: postgres 17
-- The "Result columns bind to fields by name" group in docs/checks.md (and its ja
-- translation): a minimal customers/orders pair, just enough for the join the example
-- runs.
CREATE TABLE customers (
    id    bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name  text NOT NULL,
    email text
);

CREATE TABLE orders (
    id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    customer_id bigint NOT NULL REFERENCES customers(id)
);
