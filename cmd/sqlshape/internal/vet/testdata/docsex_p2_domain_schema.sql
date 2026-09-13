-- sqlshape: postgres 17
-- "Do not mix domains of different units (PostgreSQL)": the doc's own schema fence,
-- verbatim, plus an index on price so the -strict param case does not pick up an
-- unrelated "no index ... scans the whole table" advisory alongside the domain one.
CREATE DOMAIN yen AS bigint;
CREATE DOMAIN gram AS integer;
CREATE TABLE products (id bigint PRIMARY KEY, price yen NOT NULL, weight gram NOT NULL);
CREATE INDEX products_price_idx ON products (price);
