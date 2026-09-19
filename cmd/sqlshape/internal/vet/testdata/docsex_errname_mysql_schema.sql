-- sqlshape: mysql 8.4
-- "Name the errors a trigger raises" (MySQL half): fk_orders_customer is named
-- explicitly, matching the doc's expect line.
CREATE TABLE customers (
    id BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY
);

CREATE TABLE orders (
    id          BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
    customer_id BIGINT UNSIGNED NOT NULL,
    total       DECIMAL(12,2) NOT NULL,
    CONSTRAINT fk_orders_customer FOREIGN KEY (customer_id) REFERENCES customers(id)
);

-- sqlshape: error 30001 = OrderTooLarge
CREATE TRIGGER order_size BEFORE INSERT ON orders FOR EACH ROW
BEGIN
  IF NEW.total > 1000000 THEN
    SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'order too large', MYSQL_ERRNO = 30001;
  END IF;
END;
