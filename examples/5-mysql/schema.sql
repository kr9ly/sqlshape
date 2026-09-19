-- sqlshape: mysql 8.4
CREATE TABLE customers (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  email VARCHAR(255) NOT NULL,
  name VARCHAR(100) NOT NULL,
  created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  UNIQUE KEY customers_email_key (email)
) ENGINE=InnoDB;

CREATE TABLE orders (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  customer_id BIGINT UNSIGNED NOT NULL,
  status ENUM('pending', 'paid', 'cancelled') NOT NULL DEFAULT 'pending',
  total DECIMAL(10,2) NOT NULL DEFAULT 0,
  note TEXT,
  CONSTRAINT orders_total_check CHECK (total >= 0),
  CONSTRAINT fk_orders_customer FOREIGN KEY (customer_id) REFERENCES customers (id)
) ENGINE=InnoDB;

CREATE TABLE order_audit (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  customer_id BIGINT UNSIGNED NOT NULL,
  total DECIMAL(10,2) NOT NULL,
  created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6)
) ENGINE=InnoDB;

-- sqlshape: error 30001 = OrderTotalTooLarge
CREATE TRIGGER orders_before_insert BEFORE INSERT ON orders FOR EACH ROW
BEGIN
  IF NEW.total > 100000 THEN
    SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'order total too large', MYSQL_ERRNO = 30001;
  END IF;
  INSERT INTO order_audit (customer_id, total) VALUES (NEW.customer_id, NEW.total);
END;

CREATE FUNCTION customer_order_total(cust_id BIGINT UNSIGNED) RETURNS DECIMAL(10,2)
READS SQL DATA
BEGIN
  DECLARE result DECIMAL(10,2);
  SELECT SUM(total) INTO result FROM orders WHERE customer_id = cust_id;
  RETURN result;
END;
