-- sqlshape: postgres 17
-- "Composite parameters are structs".
CREATE TYPE order_item AS (sku text, qty integer);

-- sqlshape: not null
CREATE FUNCTION place_order(customer_id bigint, items order_item[]) RETURNS bigint
LANGUAGE sql AS $$ SELECT 1::bigint $$;
