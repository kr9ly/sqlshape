-- sqlshape: postgres 17
-- "Meaning comes from use, not from a registry": a users/orders pair, just enough for
-- UserID to bind to users.id on its first meeting and then meet orders.id on its second.
-- orders.total is bigint (not numeric) so the second statement's R (int64) does not pick
-- up an unrelated "drops the fraction" advisory alongside the key-mismatch diagnostic
-- the example is about.
CREATE TABLE users (
    id    bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    email text NOT NULL
);

CREATE TABLE orders (
    id    bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    total bigint NOT NULL
);
