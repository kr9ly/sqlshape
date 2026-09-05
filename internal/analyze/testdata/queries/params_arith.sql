SELECT id, total + $1 AS t2, total * 1.1 AS t3, $2 + 1 AS n FROM orders WHERE id > $3
