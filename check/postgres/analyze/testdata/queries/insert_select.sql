INSERT INTO order_items (order_id, line_no, sku, discount) SELECT id, 1, note, 0 FROM orders WHERE id = $1 RETURNING order_id, line_no, sku, qty
