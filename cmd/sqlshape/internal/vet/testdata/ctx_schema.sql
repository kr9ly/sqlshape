-- sqlshape: postgres 17
-- sqlshape: require pinned(tenant_id)
-- sqlshape: context ops: waive pinned(tenant_id); require id = $1 on delete
CREATE TABLE orders (id bigint PRIMARY KEY, tenant_id bigint NOT NULL, status text NOT NULL);
