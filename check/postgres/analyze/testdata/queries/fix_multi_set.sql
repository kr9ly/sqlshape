UPDATE orders SET (note, total) = ('x', $1), status = 'paid' WHERE id = $2 RETURNING note, total
