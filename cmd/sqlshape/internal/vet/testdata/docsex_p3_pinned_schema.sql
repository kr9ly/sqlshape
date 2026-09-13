-- sqlshape: postgres 17
-- "A column is pinned on every statement (`pinned`, `-require-columns`)": orders,
-- completed with the columns the example's SELECT needs.
-- sqlshape: require pinned(tenant_id)
CREATE TABLE orders (
    id        bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    tenant_id bigint NOT NULL,
    total     numeric(12,2) NOT NULL
);
