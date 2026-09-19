-- sqlshape: postgres 17
-- "A predicate across tables has a witness (`EXISTS`)": orders/shipments exactly as the
-- example's three Passes and two Rejected statements reference.
CREATE TABLE orders (
    id        bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    tenant_id bigint NOT NULL
);

-- sqlshape: require EXISTS (SELECT 1 FROM orders o WHERE o.id = order_id AND o.tenant_id = $1)
CREATE TABLE shipments (
    id       bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    order_id bigint NOT NULL,
    carrier  text NOT NULL
);
