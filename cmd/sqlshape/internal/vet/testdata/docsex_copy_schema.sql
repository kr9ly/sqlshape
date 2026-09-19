-- sqlshape: postgres 17
-- "Bulk loading with COPY (PostgreSQL)": order_items exactly as the example implies --
-- line_no is NOT NULL with no default, so a Copy that omits it is rejected.
CREATE TABLE orders (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY
);

CREATE TABLE order_items (
    order_id bigint NOT NULL REFERENCES orders(id),
    line_no  smallint NOT NULL,
    sku      varchar(32) NOT NULL
);
