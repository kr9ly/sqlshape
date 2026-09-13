-- sqlshape: postgres 17
-- "Do not always send a value into a column with a DEFAULT" (-strict).
CREATE TYPE order_status AS ENUM ('pending', 'paid', 'shipped', 'cancelled');
CREATE TABLE orders (
    id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    customer_id bigint NOT NULL,
    status      order_status NOT NULL DEFAULT 'pending'
);
