-- sqlshape: require pinned(tenant_id)
-- sqlshape: context ops: waive pinned(tenant_id)
CREATE TABLE orders (id bigint PRIMARY KEY, tenant_id bigint NOT NULL);
