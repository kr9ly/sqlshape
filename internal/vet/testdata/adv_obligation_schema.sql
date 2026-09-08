-- sqlshape: require pinned(tenant_id)
CREATE TABLE orders (
    id        bigint PRIMARY KEY,
    tenant_id bigint NOT NULL,
    user_id   bigint NOT NULL,
    status    text NOT NULL
);
