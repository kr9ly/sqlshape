-- sqlshape: postgres 17
-- "A statement that calls a function declares the function's failure modes too"
-- (PostgreSQL half): a place_order() that inserts into orders, whose customer_id foreign
-- key the caller can violate through the function. Annotated "not null" (like
-- order_count in schema.sql) so the RETURNING id value does not add an unrelated NULL
-- advisory of its own.
CREATE TABLE customers (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY
);

CREATE TABLE orders (
    id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    customer_id bigint NOT NULL REFERENCES customers(id),
    note        text
);

-- sqlshape: not null
CREATE FUNCTION place_order(p_customer_id bigint, p_note text) RETURNS bigint
LANGUAGE sql AS $$ INSERT INTO orders (customer_id, note) VALUES (p_customer_id, p_note) RETURNING id $$;
