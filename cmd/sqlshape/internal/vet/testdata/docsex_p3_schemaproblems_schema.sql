-- sqlshape: postgres 17
-- "Problems in the schema itself": a dynamic EXECUTE inside a PL/pgSQL function (a
-- -strict advisory) and a view referencing a misspelled column (a hard schema error),
-- exactly as the heading's two fences show.
CREATE TABLE orders (
    id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    customer_id bigint NOT NULL
);

CREATE TABLE customers (
    id   bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name text NOT NULL
);

CREATE FUNCTION purge(tbl text) RETURNS void LANGUAGE plpgsql AS $$
BEGIN
  EXECUTE 'DELETE FROM ' || quote_ident(tbl);
END $$;

CREATE VIEW order_summary AS
SELECT o.id, c.nmae AS customer_name FROM orders o JOIN customers c ON c.id = o.customer_id;
