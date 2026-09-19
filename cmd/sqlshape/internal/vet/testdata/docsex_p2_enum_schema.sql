-- sqlshape: postgres 17
-- "Enum and lookup values agree with the named type's constants": order_statuses/orders
-- exactly as the doc's own schema fence shows.
CREATE TABLE order_statuses (code text PRIMARY KEY, label text NOT NULL);
INSERT INTO order_statuses VALUES ('pending', 'Pending'), ('paid', 'Paid'), ('shipped', 'Shipped');

CREATE TABLE orders (
    id     bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    status text NOT NULL REFERENCES order_statuses(code),
    total  numeric(12,2) NOT NULL
);
