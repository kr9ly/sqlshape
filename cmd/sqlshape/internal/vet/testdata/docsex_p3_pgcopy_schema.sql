-- sqlshape: postgres 17
-- postgres.md "The runtime: pgx", the Copy example: order_items with every column the
-- fence's postgres.Copy call lists.
CREATE TABLE orders (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY
);

CREATE TABLE order_items (
    order_id bigint NOT NULL REFERENCES orders(id),
    line_no  smallint NOT NULL,
    sku      varchar(32) NOT NULL,
    qty      integer NOT NULL,
    discount numeric(5,2) NOT NULL DEFAULT 0
);
