-- sqlshape: postgres 17
-- "Name the errors a trigger raises" (PostgreSQL half): customers/orders with the
-- doc's own trigger, so its default constraint name (orders_customer_id_fkey) and
-- trigger/function names (order_size / check_order_size) match the prose exactly.
CREATE TABLE customers (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY
);

CREATE TABLE orders (
    id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    customer_id bigint NOT NULL REFERENCES customers(id),
    total       numeric(12,2) NOT NULL
);

-- sqlshape: error P0401 = OrderTooLarge
CREATE FUNCTION check_order_size() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.total > 1000000 THEN RAISE EXCEPTION 'order too large' USING ERRCODE = 'P0401'; END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER order_size BEFORE INSERT OR UPDATE ON orders FOR EACH ROW EXECUTE FUNCTION check_order_size();
