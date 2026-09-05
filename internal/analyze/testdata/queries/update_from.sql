UPDATE order_items i SET qty = i.qty + $1, discount = o.total / 10 FROM orders o WHERE o.id = i.order_id AND o.status = $2 RETURNING i.order_id, i.qty, o.status
