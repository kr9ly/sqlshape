-- sqlshape: postgres 17
-- "Different callers, different rules (`context`)": orders pinned by tenant_id in the
-- base context; the ops context waives the pin and requires id = $1 on delete instead.
-- sqlshape: require pinned(tenant_id)
-- sqlshape: context ops: waive pinned(tenant_id); require id = $1 on delete
CREATE TABLE orders (
    id        bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    tenant_id bigint NOT NULL,
    status    text NOT NULL
);
