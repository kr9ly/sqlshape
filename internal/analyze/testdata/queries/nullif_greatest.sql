SELECT nullif(note, '') AS note, greatest(total, $1) AS g, least(qty, 5) AS l FROM orders, order_items
