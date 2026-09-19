-- sqlshape: mysql 8.4
-- "A statement that calls a function declares the function's failure modes too"
-- (MySQL half): the doc's own place_order() function fence, verbatim, plus a minimal
-- orders table it writes to. customer_id/total are left nullable and orders carries no
-- foreign key here (neither is shown in the doc's fence, and the point of the example is
-- the SIGNAL alone, not a second, unrelated violation this harness would have to invent a
-- schema reason for). Like PostgreSQL's schema.sql / docsex_p2_call_pg_schema.sql, the
-- function is annotated "-- sqlshape: not null" (mysql.md documents the same directive
-- for a MySQL CREATE FUNCTION), so the test package receives place_order's result as
-- plain int64 rather than *int64.
CREATE TABLE orders (
    id          BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
    customer_id BIGINT UNSIGNED,
    total       DECIMAL(12,2)
);

-- sqlshape: error 30001 = OrderTooLarge
-- sqlshape: not null
CREATE FUNCTION place_order(cust_id BIGINT UNSIGNED, amount DECIMAL(10,2)) RETURNS BIGINT
BEGIN
  IF amount > 1000000 THEN
    SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'order too large', MYSQL_ERRNO = 30001;
  END IF;
  INSERT INTO orders (customer_id, total) VALUES (cust_id, amount);
  RETURN LAST_INSERT_ID();
END;
