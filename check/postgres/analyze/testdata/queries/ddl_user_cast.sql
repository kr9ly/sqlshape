UPDATE orders SET note = price WHERE id = $1 RETURNING price::text AS p
