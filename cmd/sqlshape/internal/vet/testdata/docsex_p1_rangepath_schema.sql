-- sqlshape: postgres 17
-- "Nested paths and `range`".
CREATE TABLE products (
    id   bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name text,
    sku  varchar(32) NOT NULL
);
