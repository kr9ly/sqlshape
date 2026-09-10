SELECT order_id, line_no, sku, qty, status FROM order_items JOIN (SELECT id AS order_id, status FROM orders) o USING (order_id) WHERE status = $1
